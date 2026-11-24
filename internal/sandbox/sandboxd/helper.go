//go:build linux

// Package sandboxd is the root sandbox helper (WP-418, WP-ADR28, PL7
// §5.3): the only part of Waypoint that runs as root. It answers the
// runner's user only, over a unix socket, and acts only on specs the
// Waypoint server signed. It makes the machine users and their store
// directories and takes them away again (WP-448), and runs Contained
// sessions as those users (WP-470).
package sandboxd

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/broker"
)

// Config is how the helper was installed.
type Config struct {
	Version string
	// Key is the server's pinned spec key.
	Key ed25519.PublicKey
	// RunnerUID is the only user (besides root) the socket answers.
	RunnerUID int
	// RunnerID, when set, is the only runner whose specs count here.
	RunnerID   int64
	StorePath  string
	StatePath  string // the directory holding the ledger
	StoreOwner int    // 0; the tests' own uid
	// RepinStore accepts a store directory other than the one pinned,
	// after a person moved it on purpose.
	RepinStore bool

	// Contained sessions (WP-470). Empty takes the default.
	RunPath    string // the sessions' run directories (sandbox.RunDir)
	Binary     string // this binary, as sessions see it (sandbox.Binary)
	SystemCAs  string // the machine's CA bundle
	EgressUser string // the brokers' user (sandbox.EgressUser)
	// Toolchains are directories every session sees read-only at the same
	// path (a dir/bin is put on PATH): root's or the runner's, and nobody
	// else's to write.
	Toolchains []string
}

// Helper serves requests. Open it, then Serve.
type Helper struct {
	cfg            Config
	sys            System
	now            func() time.Time
	log            *slog.Logger
	mu             sync.Mutex
	led            *ledger
	store          *store
	reconcileDrift []string
	chown          func(fd, uid, gid int) error
	ca             *broker.CA
	sessions       map[string]*session
	sessionWait    time.Duration
}

// New makes a helper; Open loads its ledger and store.
func New(cfg Config, sys System, log *slog.Logger) *Helper {
	if log == nil {
		log = slog.Default()
	}
	if cfg.RunPath == "" {
		cfg.RunPath = sandbox.RunDir
	}
	if cfg.Binary == "" {
		cfg.Binary = sandbox.Binary
	}
	if cfg.SystemCAs == "" {
		cfg.SystemCAs = systemCAs
	}
	if cfg.EgressUser == "" {
		cfg.EgressUser = sandbox.EgressUser
	}
	return &Helper{cfg: cfg, sys: sys, now: time.Now, log: log, sessions: map[string]*session{}, sessionWait: 10 * time.Second}
}

func (h *Helper) ledgerPath() string { return filepath.Join(h.cfg.StatePath, "ledger.json") }

// Open checks the state directory and the store, pins the store the first
// time, and finishes what a crash interrupted.
func (h *Helper) Open() error {
	fi, err := os.Lstat(h.cfg.StatePath)
	if err != nil {
		return fmt.Errorf("the helper's state directory: %w", err)
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("the helper's state directory %s must be a directory only its owner can use (0700)", h.cfg.StatePath)
	}
	if h.led, err = loadLedger(h.ledgerPath()); err != nil {
		return err
	}
	if h.store, err = openStore(h.cfg.StorePath, h.cfg.StoreOwner); err != nil {
		return err
	}
	if h.chown != nil {
		h.store.chown = h.chown
	}
	switch {
	case h.led.Store == nil || h.cfg.RepinStore:
		p := h.store.pin
		h.led.Store = &p
	case *h.led.Store != h.store.pin:
		h.store.close()
		return fmt.Errorf("the store %s isn't the directory this helper pinned (device %d inode %d): if it was moved on purpose, start the helper once with --repin-store",
			h.cfg.StorePath, h.led.Store.Dev, h.led.Store.Ino)
	}
	if err := h.openSessions(); err != nil {
		h.store.close()
		return err
	}
	h.reconcile()
	h.reconcileSessions()
	return h.led.save(h.ledgerPath())
}

// Close lets go of the store.
func (h *Helper) Close() {
	if h.store != nil {
		h.store.close()
	}
}

// reconcile finishes every operation a crash (or a failure) interrupted:
// each step is safe to repeat. One it can't finish stays, and Status shows
// it.
func (h *Helper) reconcile() {
	h.reconcileDrift = nil
	for _, in := range append([]intent(nil), h.led.Intents...) {
		var err error
		switch in.Op {
		case intentCreateUser:
			if err = h.create(in.User, in.Runner); err == nil && h.led.byName(in.User.Name) == nil {
				h.led.Users = append(h.led.Users, in.User)
			}
		case intentRemoveUser:
			err = h.finishRemove(in)
		default:
			h.reconcileDrift = append(h.reconcileDrift, fmt.Sprintf("an unknown operation %q was left unfinished", in.Op))
			continue
		}
		if err != nil {
			h.log.Warn("finishing an interrupted operation", "op", in.Op, "user", in.User.Name, "err", err)
			h.reconcileDrift = append(h.reconcileDrift, fmt.Sprintf("couldn't finish %s %s: %v", map[string]string{intentCreateUser: "creating", intentRemoveUser: "removing"}[in.Op], in.User.Name, err))
			continue
		}
		h.led.dropIntent(in.ID)
		h.log.Info("finished an interrupted operation", "op", in.Op, "user", in.User.Name, "uid", in.User.UID)
	}
}

// Handle answers one request.
func (h *Helper) Handle(req sandbox.Request) sandbox.Response {
	switch req.Op {
	case sandbox.OpStart:
		return sandbox.Response{Error: "start needs the session's stdin, stdout and stderr (sandbox.Start)"}
	case sandbox.OpStop:
		if err := h.stop(req.Session); err != nil {
			h.log.Warn("stop", "session", req.Session, "err", err)
			return sandbox.Response{Error: err.Error()}
		}
		return sandbox.Response{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch req.Op {
	case sandbox.OpStatus:
		st := h.status()
		return sandbox.Response{Status: &st}
	case sandbox.OpEnsureUsers:
		users, err := h.ensureUsers(req.Spec)
		if err != nil {
			h.log.Warn("refused", "op", req.Op, "err", err)
			return sandbox.Response{Error: err.Error()}
		}
		return sandbox.Response{Users: users}
	case sandbox.OpRemoveUser:
		u, err := h.removeUser(req.Spec)
		if err != nil {
			h.log.Warn("refused or failed", "op", req.Op, "err", err)
			return sandbox.Response{Error: err.Error()}
		}
		return sandbox.Response{Users: []sandbox.User{u}}
	case sandbox.OpReconcile:
		if _, err := h.spec(sandbox.OpReconcile, req.Spec); err != nil {
			h.log.Warn("refused", "op", req.Op, "err", err)
			return sandbox.Response{Error: err.Error()}
		}
		h.reconcile()
		if err := h.led.save(h.ledgerPath()); err != nil {
			return sandbox.Response{Error: err.Error()}
		}
		st := h.status()
		return sandbox.Response{Status: &st}
	}
	return sandbox.Response{Error: fmt.Sprintf("unknown operation %q", req.Op)}
}

// spec checks a request's spec: signed by the pinned key, current, for
// this op and runner, and not seen before. Its nonce is recorded (and
// saved with whatever the operation writes next).
func (h *Helper) spec(op string, sd *sandbox.Signed) (*sandbox.Spec, error) {
	s, err := sandbox.Verify(h.cfg.Key, sd, h.now())
	if err != nil {
		if errors.Is(err, sandbox.ErrBadSignature) && sd != nil && sd.Key != "" {
			return nil, fmt.Errorf("%w (signed with %s; this helper pins %s)", err, sd.Key, sandbox.Fingerprint(h.cfg.Key))
		}
		return nil, err
	}
	if s.Op != op {
		return nil, fmt.Errorf("the spec is for %s, not %s", s.Op, op)
	}
	if h.cfg.RunnerID != 0 && s.Runner != h.cfg.RunnerID {
		return nil, fmt.Errorf("the spec is for runner %d; this machine's is %d", s.Runner, h.cfg.RunnerID)
	}
	if err := h.led.useNonce(s.Nonce, s.Expiry, h.now().Unix()); err != nil {
		return nil, err
	}
	return s, nil
}

// keyText is what a forge identity may look like: it goes into GECOS and
// the ledger, so nothing that could split a passwd line.
var keyText = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// ensureUsers makes sure each of the spec's repos has its repo user and
// each of their owners its org user, all with store directories.
func (h *Helper) ensureUsers(sd *sandbox.Signed) ([]sandbox.User, error) {
	s, err := h.spec(sandbox.OpEnsureUsers, sd)
	if err != nil {
		return nil, err
	}
	if len(s.Repos) == 0 {
		return nil, errors.New("the spec names no repository")
	}
	if len(s.Repos) > sandbox.MaxSpecRepos {
		return nil, fmt.Errorf("the spec names %d repositories; at most %d", len(s.Repos), sandbox.MaxSpecRepos)
	}
	for _, r := range s.Repos {
		if !keyText.MatchString(r.ID) || !keyText.MatchString(r.Owner) || !strings.Contains(r.Slug, "/") || len(r.Slug) > 200 {
			return nil, fmt.Errorf("the spec names a repository wrongly (%q)", r.Slug)
		}
	}
	var out []sandbox.User
	orgs := map[string]bool{}
	for _, r := range s.Repos {
		repoUser, err := h.ensureUser(sandbox.KindRepo, r.ID, r.Slug, s.Runner)
		if err != nil {
			_ = h.led.save(h.ledgerPath())
			return nil, err
		}
		out = append(out, repoUser)
		if orgs[r.Owner] {
			continue
		}
		orgs[r.Owner] = true
		owner, _, _ := strings.Cut(r.Slug, "/")
		orgUser, err := h.ensureUser(sandbox.KindOrg, r.Owner, owner, s.Runner)
		if err != nil {
			_ = h.led.save(h.ledgerPath())
			return nil, err
		}
		out = append(out, orgUser)
	}
	return out, h.led.save(h.ledgerPath())
}

// removeUser takes a machine user away: out of the ledger with its uid
// retired at once, so no spec reaches it again; then its account and group
// go and its store directories are archived. What fails stays as an
// intent that reconcile finishes. Removing one already removed is done.
func (h *Helper) removeUser(sd *sandbox.Signed) (sandbox.User, error) {
	s, err := h.spec(sandbox.OpRemoveUser, sd)
	if err != nil {
		return sandbox.User{}, err
	}
	t := s.User
	if t == nil || t.Name == "" || (t.Kind != sandbox.KindRepo && t.Kind != sandbox.KindOrg) {
		return sandbox.User{}, errors.New("the spec names no machine user")
	}
	u := h.led.byKey(t.Kind, t.Key)
	if u != nil && u.Name == t.Name {
		for _, r := range h.led.Sessions {
			if r.UID == u.UID {
				_ = h.led.save(h.ledgerPath())
				return sandbox.User{}, fmt.Errorf("%s still has the session %s: stop it first", u.Name, r.ID)
			}
		}
	}
	if u == nil || u.Name != t.Name {
		if in := h.led.intending(t.Name); in != nil && in.Op == intentRemoveUser && in.User.Key == t.Key {
			err := h.finishRemove(*in)
			if err == nil {
				h.led.dropIntent(in.ID)
			}
			return in.User, errors.Join(err, h.led.save(h.ledgerPath()))
		}
		if r := h.led.removed(t.Name); r != nil && r.Key == t.Key {
			return r.User, h.led.save(h.ledgerPath())
		}
		_ = h.led.save(h.ledgerPath())
		return sandbox.User{}, fmt.Errorf("%s isn't a machine user of %s here", t.Name, t.Key)
	}
	now := h.now().Unix()
	in := intent{
		ID: sandbox.NewNonce(), Op: intentRemoveUser, User: *u, Runner: s.Runner, Job: s.Job, Started: now,
		Archive: fmt.Sprintf("%s-%d", u.Name, now),
	}
	h.led.dropUser(u.Name)
	h.led.Retired = append(h.led.Retired, in.User.UID)
	h.led.Intents = append(h.led.Intents, in)
	if err := h.led.save(h.ledgerPath()); err != nil {
		return sandbox.User{}, err
	}
	if err := h.finishRemove(in); err != nil {
		h.reconcileDrift = append(h.reconcileDrift, fmt.Sprintf("couldn't finish removing %s: %v", in.User.Name, err))
		return sandbox.User{}, errors.Join(err, h.led.save(h.ledgerPath()))
	}
	h.led.dropIntent(in.ID)
	h.log.Info("removed a machine user", "user", in.User.Name, "uid", in.User.UID, "archive", in.Archive)
	return in.User, h.led.save(h.ledgerPath())
}

// finishRemove does a removal's steps, each only if it's still needed:
// the account, the group, the store directories, and the history entry.
// An account or group with another id than the ledger's is left alone.
func (h *Helper) finishRemove(in intent) error {
	u := in.User
	uid, found, err := h.sys.UserByName(u.Name)
	if err != nil {
		return err
	}
	if found {
		if uid != u.UID {
			return fmt.Errorf("the user %s has uid %d, not the ledger's %d: left alone", u.Name, uid, u.UID)
		}
		if err := h.sys.DelUser(u.Name); err != nil {
			return err
		}
	}
	gid, found, err := h.sys.GroupByName(u.Name)
	if err != nil {
		return err
	}
	if found {
		if gid != u.UID {
			return fmt.Errorf("the group %s has gid %d, not the ledger's %d: left alone", u.Name, gid, u.UID)
		}
		if err := h.sys.DelGroup(u.Name); err != nil {
			return err
		}
	}
	archive, err := h.store.archive(u.Name, in.Archive)
	if err != nil {
		return err
	}
	if r := h.led.removed(u.Name); r == nil || r.UID != u.UID {
		h.led.Removed = append([]sandbox.Removed{{User: u, At: h.now().Unix(), Job: in.Job, Archive: archive}}, h.led.Removed...)
		if len(h.led.Removed) > maxRemoved {
			h.led.Removed = h.led.Removed[:maxRemoved]
		}
	}
	return nil
}

func userName(kind, slug, key string, salt int) string {
	if kind == sandbox.KindOrg {
		return sandbox.OrgUser(slug, key, salt)
	}
	return sandbox.RepoUser(slug, key, salt)
}

// ensureUser returns the ledger's user for a forge identity, creating it
// the first time.
func (h *Helper) ensureUser(kind, key, slug string, runner int64) (sandbox.User, error) {
	now := h.now().Unix()
	if u := h.led.byKey(kind, key); u != nil {
		u.Slug, u.LastUsed = slug, now
		if err := h.create(*u, runner); err != nil {
			return sandbox.User{}, err
		}
		return *u, nil
	}
	if in := h.led.creating(kind, key); in != nil {
		u, id := in.User, in.ID
		u.Slug, u.LastUsed = slug, now
		if err := h.create(u, in.Runner); err != nil {
			return sandbox.User{}, err
		}
		if h.led.byName(u.Name) == nil {
			h.led.Users = append(h.led.Users, u)
		}
		h.led.dropIntent(id)
		return u, nil
	}
	var name string
	for salt := 0; ; salt++ {
		if salt > 20 {
			return sandbox.User{}, errors.New("no free user name for this repository")
		}
		name = userName(kind, slug, key, salt)
		if h.led.byName(name) != nil || h.led.intending(name) != nil {
			continue
		}
		if _, found, err := h.sys.UserByName(name); err != nil {
			return sandbox.User{}, err
		} else if found {
			return sandbox.User{}, fmt.Errorf("a user %s exists on this machine but not in the sandbox ledger: Waypoint won't take it over (remove or rename it)", name)
		}
		if _, found, err := h.sys.GroupByName(name); err != nil {
			return sandbox.User{}, err
		} else if found {
			return sandbox.User{}, fmt.Errorf("a group %s exists on this machine but not in the sandbox ledger: Waypoint won't take it over (remove or rename it)", name)
		}
		u := sandbox.User{Name: name, Kind: kind, Key: key, Slug: slug, Salt: salt, Created: now, LastUsed: now}
		uid, err := h.freeUID()
		if err != nil {
			return sandbox.User{}, err
		}
		u.UID = uid
		in := intent{ID: sandbox.NewNonce(), Op: intentCreateUser, User: u, Runner: runner, Started: now}
		h.led.Intents = append(h.led.Intents, in)
		if err := h.led.save(h.ledgerPath()); err != nil {
			h.led.dropIntent(in.ID)
			return sandbox.User{}, err
		}
		if err := h.create(u, runner); err != nil {
			return sandbox.User{}, err
		}
		h.led.Users = append(h.led.Users, u)
		h.led.dropIntent(in.ID)
		h.log.Info("created a machine user", "user", name, "uid", u.UID, "for", key, "slug", slug)
		return u, nil
	}
}

// freeUID is the lowest uid in the range that the ledger never used and
// the machine doesn't have.
func (h *Helper) freeUID() (int, error) {
	for uid := firstUID; uid <= lastUID; uid++ {
		if h.led.holds(uid) {
			continue
		}
		if _, found, err := h.sys.UserByUID(uid); err != nil {
			return 0, err
		} else if found {
			continue
		}
		if _, found, err := h.sys.GroupByGID(uid); err != nil {
			return 0, err
		} else if found {
			continue
		}
		return uid, nil
	}
	return 0, errors.New("the sandbox uid range 40000-49999 is used up")
}

// create makes u's group, account and store directories, each step only
// if it's missing, and refuses one that exists with another id.
func (h *Helper) create(u sandbox.User, runner int64) error {
	gid, found, err := h.sys.GroupByName(u.Name)
	if err != nil {
		return err
	}
	if found && gid != u.UID {
		return fmt.Errorf("the group %s has gid %d, not the ledger's %d", u.Name, gid, u.UID)
	}
	if !found {
		if other, taken, err := h.sys.GroupByGID(u.UID); err != nil {
			return err
		} else if taken {
			return fmt.Errorf("gid %d belongs to %s, not %s", u.UID, other, u.Name)
		}
		if err := h.sys.AddGroup(u.Name, u.UID); err != nil {
			return err
		}
	}
	uid, found, err := h.sys.UserByName(u.Name)
	if err != nil {
		return err
	}
	if found && uid != u.UID {
		return fmt.Errorf("the user %s has uid %d, not the ledger's %d", u.Name, uid, u.UID)
	}
	if !found {
		if other, taken, err := h.sys.UserByUID(u.UID); err != nil {
			return err
		} else if taken {
			return fmt.Errorf("uid %d belongs to %s, not %s", u.UID, other, u.Name)
		}
		gecos := fmt.Sprintf("waypoint-sandbox %d/%s", runner, u.Key)
		if err := h.sys.AddUser(u.Name, u.UID, u.UID, h.store.home(u.Name), gecos); err != nil {
			return err
		}
	}
	return h.store.userDirs(u.Name, u.UID, u.UID)
}

// status reports the machine as the ledger sees it.
func (h *Helper) status() sandbox.Status {
	st := sandbox.Status{
		Version:   h.cfg.Version,
		Key:       sandbox.Fingerprint(h.cfg.Key),
		RunnerUID: h.cfg.RunnerUID,
		RunnerID:  h.cfg.RunnerID,
		Store:     h.store.status(h.led.Store),
		KVM:       kvm(),
		Users:     []sandbox.User{},
		Retired:   len(h.led.Retired),
		Removed:   h.led.Removed[:min(len(h.led.Removed), 20)],
		Pending:   len(h.led.Intents),
		Drift:     append([]string{}, h.reconcileDrift...),
	}
	sub, err := h.sys.SubIDs()
	if err != nil {
		st.Drift = append(st.Drift, "can't read /etc/subuid: "+err.Error())
	}
	for _, u := range h.led.Users {
		uid, found, err := h.sys.UserByName(u.Name)
		u.Present = err == nil && found && uid == u.UID
		if !u.Present {
			st.Drift = append(st.Drift, fmt.Sprintf("%s is in the ledger but not on this machine (it's made again on its next job)", u.Name))
		}
		if sub[u.Name] {
			st.Drift = append(st.Drift, fmt.Sprintf("%s has subordinate ids in /etc/subuid or /etc/subgid: remove them", u.Name))
		}
		st.Users = append(st.Users, u)
	}
	st.Sessions = h.sessionInfos()
	return st
}
