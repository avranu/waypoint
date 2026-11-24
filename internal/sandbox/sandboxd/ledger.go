//go:build linux

package sandboxd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// Paths and users come from the ledger, never from a tree a sandbox could write.

// UIDs come from sandbox.FirstUID..LastUID and are never handed out twice.
const (
	firstUID = sandbox.FirstUID
	lastUID  = sandbox.LastUID
)

type ledger struct {
	V       int            `json:"v"`
	Store   *pin           `json:"store,omitempty"`
	Users   []sandbox.User `json:"users"`
	Retired []int          `json:"retired"`
	// Removed are the users taken away, newest first (the last
	// maxRemoved).
	Removed []sandbox.Removed `json:"removed,omitempty"`
	Intents []intent          `json:"intents"`
	Nonces  map[string]int64  `json:"nonces"` // nonce → its spec's expiry
	// Sessions are the Contained sessions whose workspaces are in the
	// store: running, kept for a next step, or left by a crash.
	Sessions []sessionRecord `json:"sessions,omitempty"`
}

// pin is the store's identity when it was first opened.
type pin struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// intent is written before an operation acts, so a crash halfway leaves a
// record the next start finishes.
type intent struct {
	ID      string       `json:"id"`
	Op      string       `json:"op"` // create_user, remove_user
	User    sandbox.User `json:"user"`
	Runner  int64        `json:"runner"`
	Job     int64        `json:"job,omitempty"`
	Started int64        `json:"started"`
	// Archive is where a removal moves the user's store directories,
	// below the store's trash.
	Archive string `json:"archive,omitempty"`
}

const (
	intentCreateUser = "create_user"
	intentRemoveUser = "remove_user"
)

// maxRemoved bounds the ledger's history of removed users; their uids
// stay retired regardless.
const maxRemoved = 200

func loadLedger(path string) (*ledger, error) {
	l := &ledger{V: 1, Nonces: map[string]int64{}}
	b, err := os.ReadFile(path) // #nosec G304 -- the helper's own state file
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, fmt.Errorf("the ledger %s is damaged: %w", path, err)
	}
	if l.Nonces == nil {
		l.Nonces = map[string]int64{}
	}
	return l, nil
}

// save writes the ledger whole: a temporary file, synced, renamed over the
// old one, and the directory synced.
func (l *ledger) save(path string) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".ledger-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir) // #nosec G304 -- the state directory
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func (l *ledger) byKey(kind, key string) *sandbox.User {
	for i := range l.Users {
		if l.Users[i].Kind == kind && l.Users[i].Key == key {
			return &l.Users[i]
		}
	}
	return nil
}

func (l *ledger) byName(name string) *sandbox.User {
	for i := range l.Users {
		if l.Users[i].Name == name {
			return &l.Users[i]
		}
	}
	return nil
}

// holds says whether the ledger has a use for the uid: a user, a retired
// uid or one an unfinished operation is creating.
func (l *ledger) holds(uid int) bool {
	if slices.Contains(l.Retired, uid) {
		return true
	}
	for _, u := range l.Users {
		if u.UID == uid {
			return true
		}
	}
	for _, in := range l.Intents {
		if in.User.UID == uid {
			return true
		}
	}
	return false
}

// removed is the latest removal of name, if the history still has it.
func (l *ledger) removed(name string) *sandbox.Removed {
	for i := range l.Removed {
		if l.Removed[i].Name == name {
			return &l.Removed[i]
		}
	}
	return nil
}

// dropUser takes name out of the ledger's users.
func (l *ledger) dropUser(name string) {
	l.Users = slices.DeleteFunc(l.Users, func(u sandbox.User) bool { return u.Name == name })
}

// intending says whether an unfinished operation is creating or removing
// name.
func (l *ledger) intending(name string) *intent {
	for i := range l.Intents {
		if l.Intents[i].User.Name == name {
			return &l.Intents[i]
		}
	}
	return nil
}

// creating is an unfinished create of the user for kind and key.
func (l *ledger) creating(kind, key string) *intent {
	for i := range l.Intents {
		if in := &l.Intents[i]; in.Op == intentCreateUser && in.User.Kind == kind && in.User.Key == key {
			return in
		}
	}
	return nil
}

func (l *ledger) dropIntent(id string) {
	l.Intents = slices.DeleteFunc(l.Intents, func(in intent) bool { return in.ID == id })
}

// useNonce records a spec's nonce, refusing one seen before, and forgets
// nonces whose specs have expired.
func (l *ledger) useNonce(nonce string, exp, now int64) error {
	for n, e := range l.Nonces {
		if e < now {
			delete(l.Nonces, n)
		}
	}
	if _, seen := l.Nonces[nonce]; seen {
		return errors.New("this spec was used already")
	}
	l.Nonces[nonce] = exp
	return nil
}
