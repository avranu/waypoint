//go:build linux

package sandboxd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/broker"
	"git.jmann.me/claude/waypoint/internal/sandbox/forward"
)

// systemCAs is where Debian and Ubuntu keep the machine's CA bundle.
const systemCAs = "/etc/ssl/certs/ca-certificates.crt"

// Inside a Contained unit.
const (
	sandboxHome = "/home/sandbox"
	sandboxWork = "/work"
	sandboxEtc  = "/etc/waypoint-sandbox"
	sandboxPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	unitPrefix  = "waypoint-sb-"
)

// sessionRecord is a session the ledger knows of: its workspace is in the
// store until a session that doesn't keep it ends, or a stop.
type sessionRecord struct {
	ID      string `json:"id"`
	Class   string `json:"class"`
	User    string `json:"user"`
	UID     int    `json:"uid"`
	Runner  int64  `json:"runner"`
	Job     int64  `json:"job,omitempty"`
	Keep    bool   `json:"keep,omitempty"`
	Created int64  `json:"created"`
	Started int64  `json:"started"`
}

// session is a session the helper is running or clearing now.
type session struct {
	rec     sessionRecord
	run     string
	running bool
	discard bool
	done    chan struct{}
}

func sessionUnit(id string) string { return unitPrefix + id + ".service" }
func brokerUnit(id string) string  { return unitPrefix + id + "-broker.service" }
func cleanUnit(id string) string   { return unitPrefix + id + "-clean.service" }

func (l *ledger) session(id string) *sessionRecord {
	for i := range l.Sessions {
		if l.Sessions[i].ID == id {
			return &l.Sessions[i]
		}
	}
	return nil
}

func (l *ledger) dropSession(id string) {
	l.Sessions = slices.DeleteFunc(l.Sessions, func(r sessionRecord) bool { return r.ID == id })
}

// openSessions readies what sessions need: the run directory, the
// sandbox CA, the toolchains.
func (h *Helper) openSessions() error {
	if !filepath.IsAbs(h.cfg.RunPath) {
		return fmt.Errorf("the sessions' run directory %s isn't an absolute path", h.cfg.RunPath)
	}
	if err := os.Mkdir(h.cfg.RunPath, 0o755); err != nil && !errors.Is(err, os.ErrExist) { // #nosec G301 -- root's; wp-egress and the sessions' users pass through it, nothing in it is secret
		return err
	}
	var st unix.Stat_t
	if err := unix.Lstat(h.cfg.RunPath, &st); err != nil {
		return err
	}
	if err := checkDir(h.cfg.RunPath, &st, h.cfg.StoreOwner); err != nil {
		return err
	}
	if st.Mode&0o777 != 0o755 {
		if err := os.Chmod(h.cfg.RunPath, 0o755); err != nil { // #nosec G302 -- as above
			return err
		}
	}
	for _, t := range h.cfg.Toolchains {
		if err := h.checkToolchain(t); err != nil {
			return err
		}
	}
	ca, err := loadOrMakeCA(h.cfg.StatePath)
	if err != nil {
		return err
	}
	h.ca = ca
	return nil
}

func (h *Helper) checkToolchain(dir string) error {
	if !unitPath(dir) {
		return fmt.Errorf("the toolchain %q must be a clean absolute path with no spaces, colons, quotes, backslashes, $, ; or %%", dir)
	}
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		return fmt.Errorf("the toolchain %s: %w", dir, err)
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return fmt.Errorf("the toolchain %s isn't a directory", dir)
	case int(st.Uid) != h.cfg.StoreOwner && int(st.Uid) != h.cfg.RunnerUID:
		return fmt.Errorf("the toolchain %s is owned by uid %d: only root's or the runner's can be shared with sandboxes", dir, st.Uid)
	case st.Mode&0o022 != 0:
		return fmt.Errorf("the toolchain %s can be written by others (mode %o)", dir, st.Mode&0o7777)
	}
	return nil
}

// loadOrMakeCA reads the machine's sandbox CA from the state directory,
// making it the first time.
func loadOrMakeCA(state string) (*broker.CA, error) {
	certPath, keyPath := filepath.Join(state, "ca.pem"), filepath.Join(state, "ca-key.pem")
	certPEM, cerr := os.ReadFile(certPath) // #nosec G304 -- the helper's state directory
	keyPEM, kerr := os.ReadFile(keyPath)   // #nosec G304 -- the helper's state directory
	if errors.Is(cerr, os.ErrNotExist) && errors.Is(kerr, os.ErrNotExist) {
		host, _ := os.Hostname()
		ca, err := broker.NewCA("Waypoint sandbox CA " + host)
		if err != nil {
			return nil, err
		}
		if certPEM, keyPEM, err = ca.MarshalPEM(); err != nil {
			return nil, err
		}
		if err := writeNew(keyPath, keyPEM, 0o600); err != nil {
			return nil, err
		}
		if err := writeNew(certPath, certPEM, 0o644); err != nil {
			return nil, err
		}
		return ca, nil
	}
	if cerr != nil || kerr != nil {
		return nil, fmt.Errorf("the sandbox CA in %s is incomplete: %w", state, errors.Join(cerr, kerr))
	}
	return broker.LoadCA(certPEM, keyPEM)
}

// writeNew writes a file that mustn't exist yet, with exactly mode.
func writeNew(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600) // #nosec G304 -- the helper's own directories
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// startPlan is what prepare decided for one run of a session.
type startPlan struct {
	ses       *session
	uid       int
	egressUID int
	egressGID int
	workspace string
	home      string
	resources sandbox.Resources
}

// Start runs the session a start request asks for, with stdio as its
// stdin, stdout and stderr, and answers when it has ended and been
// cleared. Closing hangup (the runner went away) stops it.
func (h *Helper) Start(req sandbox.Request, stdio [3]*os.File, hangup <-chan struct{}) sandbox.Response {
	p, err := h.prepare(req.Spec)
	if err != nil {
		h.log.Warn("refused", "op", sandbox.OpStart, "err", err)
		if p != nil {
			h.finish(p.ses)
		}
		return sandbox.Response{Error: err.Error()}
	}
	id := p.ses.rec.ID
	unit := sessionUnit(id)
	code, err := h.run(p, stdio, hangup)
	h.finish(p.ses)
	if err != nil {
		h.log.Warn("session failed", "session", id, "err", err)
		return sandbox.Response{Error: err.Error(), Unit: unit}
	}
	h.log.Info("session ended", "session", id, "exit", code)
	return sandbox.Response{Exit: &code, Unit: unit}
}

// prepare checks a start spec and readies the session: the ledger record,
// the store directories and the run directory. The session is marked
// running before it returns, so no other start or stop takes it.
func (h *Helper) prepare(sd *sandbox.Signed) (*startPlan, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, err := h.spec(sandbox.OpStart, sd)
	if err != nil {
		return nil, err
	}
	saved := false
	defer func() {
		if !saved {
			_ = h.led.save(h.ledgerPath())
		}
	}()
	for _, p := range []string{h.cfg.RunPath, h.cfg.StorePath, h.cfg.StatePath, h.cfg.Binary} {
		if !unitPath(p) {
			return nil, fmt.Errorf("%q must be a clean absolute path with no spaces, colons, quotes, backslashes, $, ; or %% for sessions to run", p)
		}
	}
	if s.Level != sandbox.LevelContained {
		return nil, fmt.Errorf("this helper runs sessions at the %s level, not %q", sandbox.LevelContained, s.Level)
	}
	ss := s.Session
	if err := ss.Validate(); err != nil {
		return nil, err
	}
	if len(s.Repos) == 0 {
		return nil, errors.New("the spec names no repository")
	}
	u := h.led.byKey(sandbox.KindRepo, s.Repos[0].ID)
	if u == nil {
		return nil, fmt.Errorf("%s has no machine user here yet (ensure_users first)", s.Repos[0].Slug)
	}
	if uid, found, err := h.sys.UserByName(u.Name); err != nil {
		return nil, err
	} else if !found || uid != u.UID {
		return nil, fmt.Errorf("the machine user %s is missing (ensure_users makes it again)", u.Name)
	}
	if err := h.checkPolicy(ss.Egress); err != nil {
		return nil, err
	}
	egressUID, found, err := h.sys.UserByName(h.cfg.EgressUser)
	if err != nil {
		return nil, err
	}
	egressGID, gfound, err := h.sys.GroupByName(h.cfg.EgressUser)
	if err != nil {
		return nil, err
	}
	if !found || !gfound {
		return nil, fmt.Errorf("there is no %s user for session brokers (wp sandbox install makes it)", h.cfg.EgressUser)
	}
	if h.sessions[ss.ID] != nil {
		return nil, fmt.Errorf("the session %s is running or being cleared", ss.ID)
	}
	now := h.now().Unix()
	rec := h.led.session(ss.ID)
	first := rec == nil
	switch {
	case first:
		h.led.Sessions = append(h.led.Sessions, sessionRecord{ID: ss.ID, Class: ss.Class, User: u.Name, UID: u.UID, Created: now})
		rec = h.led.session(ss.ID)
	case !rec.Keep:
		return nil, fmt.Errorf("the session %s ended without being cleared: stop it first", ss.ID)
	case rec.UID != u.UID || rec.Class != ss.Class:
		return nil, fmt.Errorf("the session %s is %s's %s session, not %s's %s one", ss.ID, rec.User, rec.Class, u.Name, ss.Class)
	}
	rec.Runner, rec.Job, rec.Keep, rec.Started = s.Runner, s.Job, ss.Keep, now
	u.LastUsed = now
	saved = true
	if err := h.led.save(h.ledgerPath()); err != nil {
		return nil, err
	}
	ses := &session{rec: *rec, run: filepath.Join(h.cfg.RunPath, ss.ID), running: true, done: make(chan struct{})}
	h.sessions[ss.ID] = ses
	p := &startPlan{ses: ses, uid: u.UID, egressUID: egressUID, egressGID: egressGID, resources: ss.Resources.WithDefaults()}
	unattended := ss.Class == sandbox.ClassUnattended
	dir, err := h.store.sessionDirs(ss.ID, u.UID, unattended)
	if err == nil && !unattended {
		err = h.store.userDirs(u.Name, u.UID, u.UID)
	}
	if err != nil {
		return p, err
	}
	p.workspace = filepath.Join(dir, "ws")
	p.home = h.store.home(u.Name)
	if unattended {
		p.home = filepath.Join(dir, "home")
	}
	if err := h.writeRunDir(p, ss, u.Name); err != nil {
		return p, err
	}
	h.log.Info("starting a session", "session", ss.ID, "class", ss.Class, "user", u.Name, "uid", u.UID, "runner", s.Runner, "job", s.Job, "keep", ss.Keep)
	return p, nil
}

// checkPolicy refuses an egress policy the broker would refuse: the same
// checks, before anything starts. Credentials (WP-472) and the push
// gateway (WP-476) aren't wired to session brokers yet, so rules that
// need them are refused here.
func (h *Helper) checkPolicy(raw json.RawMessage) error {
	p, err := broker.ParsePolicy(raw)
	if err != nil {
		return fmt.Errorf("the session's egress policy: %w", err)
	}
	if _, err := broker.New(broker.Config{Policy: p, CA: h.ca}); err != nil {
		return fmt.Errorf("the session's egress policy: %w", err)
	}
	return nil
}

// writeRunDir writes the session's run directory: root's, and read-only
// in the session but for the broker's socket directory, which is
// wp-egress's and the repo user's group's.
func (h *Helper) writeRunDir(p *startPlan, ss *sandbox.Session, user string) error {
	run := p.ses.run
	h.removeRunDir(run)
	if err := os.Mkdir(run, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(run, 0o755); err != nil { // #nosec G302 -- the broker (wp-egress) reaches its socket and policy through it
		return err
	}
	if err := os.Mkdir(filepath.Join(run, "etc"), 0o755); err != nil { // #nosec G301 -- the session's passwd, hosts and CAs: public
		return err
	}
	if err := os.Chmod(filepath.Join(run, "etc"), 0o755); err != nil { // #nosec G302 -- as above
		return err
	}
	system, err := os.ReadFile(h.cfg.SystemCAs)
	if err != nil {
		return fmt.Errorf("the machine's CA bundle: %w", err)
	}
	caPEM := h.ca.CertPEM()
	bundle := append(append(bytes.TrimRight(system, "\n"), '\n'), caPEM...)
	launch, err := json.MarshalIndent(forward.Launch{
		Argv: ss.Argv, Env: h.sessionEnv(user, ss.Env), Dir: sandboxWork,
		Listen: forward.Listen, Broker: forward.BrokerSock,
	}, "", "  ")
	if err != nil {
		return err
	}
	uid := strconv.Itoa(p.uid)
	files := []struct {
		name string
		data []byte
	}{
		{"etc/passwd", []byte("root:x:0:0:root:/root:/usr/sbin/nologin\n" +
			user + ":x:" + uid + ":" + uid + ":Waypoint sandbox:" + sandboxHome + ":/bin/bash\n" +
			"nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n")},
		{"etc/group", []byte("root:x:0:\n" + user + ":x:" + uid + ":\nnogroup:x:65534:\n")},
		{"etc/hosts", []byte("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")},
		{"etc/nsswitch.conf", []byte("passwd: files\ngroup: files\nshadow: files\nhosts: files\n")},
		{"etc/ca-certificates.crt", bundle},
		{"etc/ca.pem", caPEM},
		{"launch.json", launch},
		{"policy.json", ss.Egress},
	}
	for _, f := range files {
		if err := writeNew(filepath.Join(run, f.name), f.data, 0o644); err != nil {
			return err
		}
	}
	bdir := filepath.Join(run, "broker")
	if err := os.Mkdir(bdir, 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(bdir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fchmod(fd, 0o2750); err != nil {
		return err
	}
	return h.store.chown(fd, p.egressUID, p.uid)
}

// removeRunDir takes a session's run directory away. The broker's
// directory is first handed back to root, which can't otherwise empty it.
func (h *Helper) removeRunDir(run string) {
	if fd, err := unix.Open(filepath.Join(run, "broker"), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); err == nil {
		if err := h.store.chown(fd, h.cfg.StoreOwner, h.cfg.StoreOwner); err != nil {
			h.log.Warn("taking back a broker directory", "dir", run, "err", err)
		}
		_ = unix.Fchmod(fd, 0o700)
		_ = unix.Close(fd)
	}
	if err := os.RemoveAll(run); err != nil {
		h.log.Warn("removing a session's run directory", "dir", run, "err", err)
	}
}

// sessionEnv is the session's whole environment: the sandbox's own
// settings, then the spec's (which can't name the sandbox's).
func (h *Helper) sessionEnv(user string, extra map[string]string) []string {
	path := sandboxPath
	for _, t := range slices.Backward(h.cfg.Toolchains) {
		if fi, err := os.Stat(filepath.Join(t, "bin")); err == nil && fi.IsDir() {
			path = filepath.Join(t, "bin") + ":" + path
		}
	}
	proxy := "http://" + forward.Listen
	bundle := "/etc/ssl/certs/ca-certificates.crt"
	env := map[string]string{
		"GOTOOLCHAIN": "local",
		"LANG":        "C.UTF-8",
	}
	for k, v := range extra {
		env[k] = v
	}
	for k, v := range map[string]string{
		"HOME": sandboxHome, "USER": user, "LOGNAME": user, "SHELL": "/bin/bash", "PATH": path, "TMPDIR": "/tmp",
		"HTTP_PROXY": proxy, "HTTPS_PROXY": proxy, "http_proxy": proxy, "https_proxy": proxy,
		"NO_PROXY": "localhost,127.0.0.1,::1", "no_proxy": "localhost,127.0.0.1,::1",
		"SSL_CERT_FILE": bundle, "GIT_SSL_CAINFO": bundle, "REQUESTS_CA_BUNDLE": bundle, "CURL_CA_BUNDLE": bundle,
		"NODE_EXTRA_CA_CERTS": sandboxEtc + "/ca.pem",
		sandbox.EnvLevel:      sandbox.LevelContained,
	} {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// run starts the broker, waits for its socket, and runs the session's
// unit until it ends; a hang-up stops it.
func (h *Helper) run(p *startPlan, stdio [3]*os.File, hangup <-chan struct{}) (int, error) {
	id, run := p.ses.rec.ID, p.ses.run
	if err := h.sys.StartUnit(brokerUnit(id), p.egressUID, p.egressGID,
		brokerProps(run, h.cfg.StatePath), []string{h.cfg.Binary, "broker",
			"--policy", filepath.Join(run, "policy.json"), "--socket", filepath.Join(run, "broker", "proxy.sock")}); err != nil {
		return 0, fmt.Errorf("the session's broker didn't start: %w", err)
	}
	if err := h.waitSocket(filepath.Join(run, "broker", "proxy.sock")); err != nil {
		return 0, fmt.Errorf("the session's broker didn't listen (journalctl -u %s): %w", brokerUnit(id), err)
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-hangup:
			h.log.Info("the runner hung up: stopping the session", "session", id)
			if err := h.sys.StopUnits(sessionUnit(id)); err != nil {
				h.log.Warn("stopping a session", "session", id, "err", err)
			}
		case <-stopped:
		}
	}()
	props := containedProps(unitPlan{
		Run: run, Workspace: p.workspace, Home: p.home, Toolchains: h.cfg.Toolchains, Resources: p.resources,
	})
	return h.sys.RunUnit(sessionUnit(id), p.uid, p.uid, props, []string{h.cfg.Binary, "forward", "--launch", sandboxEtc + "/launch.json"}, stdio)
}

func (h *Helper) waitSocket(path string) error {
	deadline := time.Now().Add(h.sessionWait)
	for {
		fi, err := os.Lstat(path)
		if err == nil && fi.Mode().Type() == os.ModeSocket {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = errors.New("not a socket")
			}
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// finish stops a session's units and clears it: the run directory
// always, the workspace unless it's kept.
func (h *Helper) finish(ses *session) {
	id := ses.rec.ID
	if err := h.sys.StopUnits(sessionUnit(id), brokerUnit(id)); err != nil {
		h.noteDrift("couldn't stop the session %s: %v", id, err)
	}
	h.removeRunDir(ses.run)
	h.mu.Lock()
	ses.running = false
	discard := ses.discard || !ses.rec.Keep
	h.mu.Unlock()
	if discard {
		h.clearWorkspace(id, ses.rec.UID)
	}
	h.mu.Lock()
	delete(h.sessions, id)
	h.mu.Unlock()
	close(ses.done)
}

// clearWorkspace removes a session's workspace and home: their contents
// as the repo user (root never deletes a sandbox's files), then the
// emptied directories, and the ledger record. A failure is kept, and
// reported as drift.
func (h *Helper) clearWorkspace(id string, uid int) {
	ws, home := h.store.sessionLeft(id)
	if ws || home {
		code, err := h.sys.RunUnit(cleanUnit(id), uid, uid, cleanProps(h.store.sessionPath(id)),
			[]string{h.cfg.Binary, "clean", "/s/ws", "/s/home"}, [3]*os.File{})
		if err == nil && code != 0 {
			err = fmt.Errorf("the cleaner exited %d (journalctl -u %s)", code, cleanUnit(id))
		}
		if err != nil {
			h.noteDrift("couldn't clear the session %s: %v", id, err)
			return
		}
	}
	if err := h.store.removeSession(id); err != nil {
		h.noteDrift("couldn't clear the session %s: %v", id, err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.led.dropSession(id)
	if err := h.led.save(h.ledgerPath()); err != nil {
		h.log.Warn("saving the ledger", "err", err)
	}
}

func (h *Helper) noteDrift(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	h.log.Warn(msg)
	h.mu.Lock()
	h.reconcileDrift = append(h.reconcileDrift, msg)
	h.mu.Unlock()
}

// stop ends a running session and clears it, kept or not; for one that
// isn't running it clears what's left. A session nobody knows is done.
func (h *Helper) stop(id string) error {
	if !sandbox.ValidSessionID(id) {
		return fmt.Errorf("%q isn't a session id", id)
	}
	h.mu.Lock()
	if ses := h.sessions[id]; ses != nil {
		ses.discard = true
		running := ses.running
		h.mu.Unlock()
		if running {
			if err := h.sys.StopUnits(sessionUnit(id), brokerUnit(id)); err != nil {
				return err
			}
		}
		select {
		case <-ses.done:
			return nil
		case <-time.After(5 * time.Minute):
			return fmt.Errorf("the session %s is taking long to clear", id)
		}
	}
	rec := h.led.session(id)
	uid, found := 0, false
	if rec != nil {
		uid, found = rec.UID, true
	} else {
		var err error
		if uid, found, err = h.store.sessionOwner(id); err != nil {
			h.mu.Unlock()
			return err
		}
		if found && !h.led.holds(uid) {
			h.mu.Unlock()
			return fmt.Errorf("the session %s's workspace belongs to uid %d, which isn't a machine user: left alone", id, uid)
		}
	}
	ses := &session{rec: sessionRecord{ID: id, UID: uid}, run: filepath.Join(h.cfg.RunPath, id), done: make(chan struct{})}
	h.sessions[id] = ses
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.sessions, id)
		h.mu.Unlock()
		close(ses.done)
	}()
	if err := h.sys.StopUnits(sessionUnit(id), brokerUnit(id)); err != nil {
		return err
	}
	h.removeRunDir(ses.run)
	if found {
		h.clearWorkspace(id, uid)
	} else if err := h.store.removeSession(id); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.led.session(id) != nil {
		return fmt.Errorf("the session %s couldn't be cleared: see the helper's drift", id)
	}
	return nil
}

// reconcileSessions clears up after a helper that stopped with sessions
// running: their units are stopped and their run directories go; their
// workspaces go too unless kept. A workspace the ledger doesn't know is
// cleared if a machine user owns it.
func (h *Helper) reconcileSessions() {
	known := map[string]bool{}
	for _, rec := range slices.Clone(h.led.Sessions) {
		known[rec.ID] = true
		if err := h.sys.StopUnits(sessionUnit(rec.ID), brokerUnit(rec.ID)); err != nil {
			h.reconcileDrift = append(h.reconcileDrift, fmt.Sprintf("couldn't stop the session %s: %v", rec.ID, err))
			continue
		}
		h.removeRunDir(filepath.Join(h.cfg.RunPath, rec.ID))
		if !rec.Keep {
			h.clearWorkspaceLocked(rec.ID, rec.UID)
		}
	}
	if ids, err := h.store.sessionIDs(); err != nil {
		h.reconcileDrift = append(h.reconcileDrift, "can't list the store's sessions: "+err.Error())
	} else {
		for _, id := range ids {
			if known[id] {
				continue
			}
			if !sandbox.ValidSessionID(id) {
				h.reconcileDrift = append(h.reconcileDrift, fmt.Sprintf("the store has a session directory %q that no session could have made: left alone", id))
				continue
			}
			uid, found, err := h.store.sessionOwner(id)
			switch {
			case err != nil:
				h.reconcileDrift = append(h.reconcileDrift, fmt.Sprintf("the session %s: %v", id, err))
			case found && !h.led.holds(uid):
				h.reconcileDrift = append(h.reconcileDrift, fmt.Sprintf("the session %s's workspace belongs to uid %d, which isn't a machine user: left alone", id, uid))
			case found:
				h.led.Sessions = append(h.led.Sessions, sessionRecord{ID: id, UID: uid})
				h.clearWorkspaceLocked(id, uid)
			default:
				if err := h.store.removeSession(id); err != nil {
					h.reconcileDrift = append(h.reconcileDrift, err.Error())
				}
			}
		}
	}
	if names, err := os.ReadDir(h.cfg.RunPath); err == nil {
		for _, e := range names {
			if !known[e.Name()] && sandbox.ValidSessionID(e.Name()) {
				_ = h.sys.StopUnits(sessionUnit(e.Name()), brokerUnit(e.Name()))
				h.removeRunDir(filepath.Join(h.cfg.RunPath, e.Name()))
			}
		}
	}
}

// clearWorkspaceLocked is clearWorkspace for Open, which holds no lock
// and runs before anything else can.
func (h *Helper) clearWorkspaceLocked(id string, uid int) {
	ws, home := h.store.sessionLeft(id)
	if ws || home {
		code, err := h.sys.RunUnit(cleanUnit(id), uid, uid, cleanProps(h.store.sessionPath(id)),
			[]string{h.cfg.Binary, "clean", "/s/ws", "/s/home"}, [3]*os.File{})
		if err == nil && code != 0 {
			err = fmt.Errorf("the cleaner exited %d", code)
		}
		if err != nil {
			h.reconcileDrift = append(h.reconcileDrift, fmt.Sprintf("couldn't clear the session %s: %v", id, err))
			return
		}
	}
	if err := h.store.removeSession(id); err != nil {
		h.reconcileDrift = append(h.reconcileDrift, fmt.Sprintf("couldn't clear the session %s: %v", id, err))
		return
	}
	h.led.dropSession(id)
}

// sessionInfos lists the sessions for Status (h.mu held).
func (h *Helper) sessionInfos() []sandbox.SessionInfo {
	var out []sandbox.SessionInfo
	for _, r := range h.led.Sessions {
		ses := h.sessions[r.ID]
		out = append(out, sandbox.SessionInfo{
			ID: r.ID, Class: r.Class, User: r.User, UID: r.UID, Runner: r.Runner, Job: r.Job,
			Unit: sessionUnit(r.ID), Started: r.Started, Running: ses != nil && ses.running, Keep: r.Keep,
		})
	}
	return out
}
