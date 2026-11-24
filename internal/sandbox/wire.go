package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// Socket is where the helper listens; only the runner's user may connect.
const Socket = "/run/waypoint-sandboxd.sock"

// Default places, which wp sandbox install sets up.
const (
	StoreDir = "/var/lib/waypoint/store"
	StateDir = "/var/lib/waypoint/sandboxd"
	KeyFile  = "/etc/waypoint/sandboxd.pub"
	Binary   = "/usr/local/libexec/waypoint/waypoint-sandboxd"
	Unit     = "waypoint-sandboxd.service"
	StoreMnt = "var-lib-waypoint-store.mount" // systemd's name for StoreDir's mount
	// Contained sessions (WP-470): their run directories, the slice they
	// share (PL7 §6.7) and the user their brokers run as.
	RunDir     = "/run/waypoint-sandbox"
	Slice      = "waypoint-sandbox.slice"
	EgressUser = "wp-egress"
)

// Ops without a spec.
const (
	OpStatus = "status"
	// OpStop ends a running session, or clears up after one that ended
	// badly. It needs no spec: it can only take a session away.
	OpStop = "stop"
)

// maxMessage bounds one request or answer on the socket.
const maxMessage = 1 << 20

// Request is one call to the helper: an op and, for anything that changes
// the machine, the signed spec that asks for it.
type Request struct {
	Op   string  `json:"op"`
	Spec *Signed `json:"spec,omitempty"`
	// Session is the session stop ends.
	Session string `json:"session,omitempty"`
}

// Response is the helper's answer.
type Response struct {
	Error string `json:"error,omitempty"`
	// Users: what EnsureUsers made sure of, or the one RemoveUser took
	// away.
	Users  []User  `json:"users,omitempty"`
	Status *Status `json:"status,omitempty"`
	// Exit is a started session's exit status, and Unit its systemd unit.
	Exit *int   `json:"exit,omitempty"`
	Unit string `json:"unit,omitempty"`
}

// User is a machine user in the helper's ledger.
type User struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
	Kind string `json:"kind"` // repo, org
	// Key is the forge identity the user belongs to (Repo.ID or
	// Repo.Owner); Slug is its name when last used.
	Key      string `json:"key"`
	Slug     string `json:"slug"`
	Salt     int    `json:"salt,omitempty"`
	Created  int64  `json:"created"`   // unix seconds
	LastUsed int64  `json:"last_used"` // unix seconds
	// Present: the account exists on the machine (Status only).
	Present bool `json:"present,omitempty"`
}

// Removed is a machine user the helper took away: its uid is retired and
// its store directories wait in Archive for a person to delete.
type Removed struct {
	User
	At      int64  `json:"at"` // unix seconds
	Job     int64  `json:"job,omitempty"`
	Archive string `json:"archive,omitempty"`
}

// User kinds.
const (
	KindRepo = "repo"
	KindOrg  = "org"
)

// Status is the helper's view of the machine, which the runner reports to
// Waypoint and wp sandbox doctor shows.
type Status struct {
	Version string `json:"version"`
	// Key is the pinned spec key's fingerprint.
	Key       string `json:"key"`
	RunnerUID int    `json:"runner_uid"`
	// RunnerID, when set, is the only Waypoint runner whose specs count
	// here (--runner-id).
	RunnerID int64       `json:"runner_id,omitempty"`
	Store    StoreStatus `json:"store"`
	KVM      bool        `json:"kvm"`
	Users    []User      `json:"users"`
	Retired  int         `json:"retired"` // uids never handed out again
	// Removed are the users taken away, newest first (at most 20).
	Removed []Removed `json:"removed,omitempty"`
	// Pending counts operations a crash interrupted that the helper
	// couldn't finish; Drift says what differs from the ledger.
	Pending int      `json:"pending"`
	Drift   []string `json:"drift"`
	// Sessions are the Contained sessions running now.
	Sessions []SessionInfo `json:"sessions,omitempty"`
}

// SessionInfo is a running session, as Status shows it.
type SessionInfo struct {
	ID      string `json:"id"`
	Class   string `json:"class"`
	User    string `json:"user"`
	UID     int    `json:"uid"`
	Runner  int64  `json:"runner"`
	Job     int64  `json:"job,omitempty"`
	Unit    string `json:"unit"`
	Started int64  `json:"started"` // unix seconds
	// Running: the session runs now; Keep: its workspace stays when it
	// ends, for the next start of the same session, until a stop.
	Running bool `json:"running,omitempty"`
	Keep    bool `json:"keep,omitempty"`
}

// StoreStatus is the store's health.
type StoreStatus struct {
	Path    string `json:"path"`
	OK      bool   `json:"ok"`
	Problem string `json:"problem,omitempty"`
	Mounted bool   `json:"mounted"`        // a mount of its own (on a dev host, the bind from the store)
	FSType  string `json:"fs,omitempty"`   // xfs gives reflink snapshots
	Free    int64  `json:"free,omitempty"` // bytes
}

// ErrNotInstalled: there's no helper socket on this machine.
var ErrNotInstalled = errors.New("the sandbox helper isn't installed here (wp sandbox install)")

// Call sends one request to the helper at socket and reads its answer. A
// refusal comes back as an error.
func Call(ctx context.Context, socket string, req Request) (*Response, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		switch {
		case errors.Is(err, syscall.ENOENT):
			return nil, ErrNotInstalled
		case errors.Is(err, syscall.ECONNREFUSED):
			return nil, fmt.Errorf("the sandbox helper isn't running (systemctl status %s): %w", Unit, err)
		}
		return nil, fmt.Errorf("can't reach the sandbox helper: %w", err)
	}
	defer func() { _ = c.Close() }()
	dl := time.Now().Add(30 * time.Second)
	if t, ok := ctx.Deadline(); ok && t.Before(dl) {
		dl = t
	}
	_ = c.SetDeadline(dl)
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// A helper that refuses the caller answers and closes without reading,
	// so the write can fail with a broken pipe while the refusal is already
	// waiting: read it either way.
	_, werr := c.Write(append(b, '\n'))
	line, err := ReadMessage(bufio.NewReader(c))
	if err != nil {
		if werr != nil {
			return nil, werr
		}
		return nil, fmt.Errorf("the sandbox helper didn't answer: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("the sandbox helper's answer doesn't parse: %w", err)
	}
	if resp.Error != "" {
		return &resp, errors.New(resp.Error)
	}
	return &resp, nil
}

// ReadMessage reads one newline-ended message of at most maxMessage bytes.
func ReadMessage(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, more, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > maxMessage {
			return nil, errors.New("message too long")
		}
		if !more {
			return out, nil
		}
	}
}

// Report is what a runner tells Waypoint about its machine's helper, when
// it starts, when the answer changes and every few minutes.
type Report struct {
	// Installed: the helper's socket exists.
	Installed bool `json:"installed"`
	// Error: installed, but it didn't answer.
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`
}
