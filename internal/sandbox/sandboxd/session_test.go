//go:build linux

package sandboxd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/forward"
)

// unitCall is one transient unit the helper asked for.
type unitCall struct {
	Unit       string
	UID, GID   int
	Props      []string
	Argv       []string
	stdio      [3]*os.File
	stopSignal <-chan struct{}
}

func (c unitCall) has(prop string) bool { return slices.Contains(c.Props, prop) }

// fakeUnits stands in for systemd: a broker unit listens on its socket, a
// cleaner empties the directories it was given, and a session unit does
// what the test's session func says.
type fakeUnits struct {
	mu        sync.Mutex
	started   []unitCall
	ran       []unitCall
	stopped   []string
	listeners map[string]net.Listener
	stops     map[string]chan struct{}
	failStart error
	noListen  bool
	session   func(c unitCall) (int, error)
}

func (f *fakeUnits) StartUnit(unit string, uid, gid int, props, argv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, unitCall{Unit: unit, UID: uid, GID: gid, Props: props, Argv: argv})
	if f.failStart != nil {
		return f.failStart
	}
	if i := slices.Index(argv, "--socket"); i >= 0 && !f.noListen {
		l, err := net.Listen("unix", argv[i+1])
		if err != nil {
			return err
		}
		f.listeners[unit] = l
	}
	return nil
}

func (f *fakeUnits) RunUnit(unit string, uid, gid int, props, argv []string, stdio [3]*os.File) (int, error) {
	stop := make(chan struct{})
	c := unitCall{Unit: unit, UID: uid, GID: gid, Props: props, Argv: argv, stdio: stdio, stopSignal: stop}
	f.mu.Lock()
	f.ran = append(f.ran, c)
	if f.stops == nil {
		f.stops = map[string]chan struct{}{}
	}
	f.stops[unit] = stop
	fn := f.session
	f.mu.Unlock()
	if strings.HasSuffix(unit, "-clean.service") {
		for _, p := range props {
			if src, ok := strings.CutPrefix(p, "BindPaths="); ok && strings.HasSuffix(src, ":/s") {
				dir := strings.TrimSuffix(src, ":/s")
				if err := errors.Join(CleanContents(dir+"/ws"), CleanContents(dir+"/home")); err != nil {
					return 1, nil
				}
				return 0, nil
			}
		}
		return 2, nil
	}
	if fn == nil {
		return 0, nil
	}
	return fn(c)
}

func (f *fakeUnits) StopUnits(units ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range units {
		f.stopped = append(f.stopped, u)
		if ch := f.stops[u]; ch != nil {
			close(ch)
			delete(f.stops, u)
		}
		if l := f.listeners[u]; l != nil {
			_ = l.Close()
			delete(f.listeners, u)
		}
	}
	return nil
}

func (f *fakeUnits) calls(unit string) (started, ran []unitCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.started {
		if c.Unit == unit {
			started = append(started, c)
		}
	}
	for _, c := range f.ran {
		if c.Unit == unit {
			ran = append(ran, c)
		}
	}
	return started, ran
}

func systemBundle(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "ca-certificates.crt")
	if err := os.WriteFile(p, []byte("-----BEGIN CERTIFICATE-----\nSYSTEM\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const tunnelPolicy = `{"rules":[{"host":"api.anthropic.com","mode":"tunnel"}]}`

func newSession(id string) sandbox.Session {
	return sandbox.Session{
		ID: id, Class: sandbox.ClassUnattended, Argv: []string{"/usr/bin/make", "check"},
		Env: map[string]string{"FOO": "bar", "GOTOOLCHAIN": "go1.26.0"}, Egress: json.RawMessage(tunnelPolicy),
	}
}

func (r *rig) startSpec(ss sandbox.Session) *sandbox.Signed {
	r.t.Helper()
	return r.sign(&sandbox.Spec{Op: sandbox.OpStart, Runner: 3, Job: 11, Level: sandbox.LevelContained,
		Repos: []sandbox.Repo{*waypointRepo}, Session: &ss})
}

// sessionRig is a helper with the waypoint repo's users made.
func sessionRig(t *testing.T) (*rig, *Helper, sandbox.User) {
	r := newRig(t)
	h := r.open()
	resp := ensure(h, r.spec(3, waypointRepo))
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	return r, h, resp.Users[0]
}

func start(h *Helper, sd *sandbox.Signed) sandbox.Response {
	return h.Start(sandbox.Request{Op: sandbox.OpStart, Spec: sd}, [3]*os.File{}, nil)
}

func TestStartSession(t *testing.T) {
	r, h, u := sessionRig(t)
	run := filepath.Join(r.cfg.RunPath, "s1")
	var inside []string
	r.sys.session = func(c unitCall) (int, error) {
		read := func(name string) string {
			b, err := os.ReadFile(filepath.Join(run, name))
			if err != nil {
				t.Error(err)
			}
			return string(b)
		}
		inside = append(inside, read("etc/passwd"), read("etc/group"), read("etc/ca-certificates.crt"), read("policy.json"))
		var l forward.Launch
		if err := json.Unmarshal([]byte(read("launch.json")), &l); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"FOO=bar", "GOTOOLCHAIN=go1.26.0", "HOME=/home/sandbox", "HTTPS_PROXY=http://127.0.0.1:3128",
			"USER=" + u.Name, "WAYPOINT_SANDBOX=contained", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"} {
			if !slices.Contains(l.Env, want) {
				t.Errorf("the session's environment lacks %s: %v", want, l.Env)
			}
		}
		if !slices.Equal(l.Argv, []string{"/usr/bin/make", "check"}) || l.Dir != "/work" || l.Broker != forward.BrokerSock {
			t.Errorf("launch %+v", l)
		}
		fi, err := os.Stat(filepath.Join(run, "broker"))
		if err != nil || fi.Mode()&os.ModeSetgid == 0 || fi.Mode().Perm() != 0o750 {
			t.Errorf("broker dir %v %v", fi.Mode(), err)
		}
		if got := r.chowns[filepath.Join(run, "broker")]; got != [2]int{998, u.UID} {
			t.Errorf("the broker dir went to %v", got)
		}
		ws := filepath.Join(r.cfg.StorePath, "sessions", "s1", "ws")
		if err := os.WriteFile(filepath.Join(ws, "out.txt"), []byte("x"), 0o600); err != nil {
			t.Error(err)
		}
		if err := os.MkdirAll(filepath.Join(ws, "pkg", "mod"), 0o755); err != nil {
			t.Error(err)
		}
		_ = os.Chmod(filepath.Join(ws, "pkg"), 0o555)
		st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status
		if len(st.Sessions) != 1 || !st.Sessions[0].Running || st.Sessions[0].User != u.Name || st.Sessions[0].Job != 11 {
			t.Errorf("status while running %+v", st.Sessions)
		}
		return 3, nil
	}
	resp := start(h, r.startSpec(newSession("s1")))
	if resp.Error != "" || resp.Exit == nil || *resp.Exit != 3 || resp.Unit != "waypoint-sb-s1.service" {
		t.Fatalf("%+v", resp)
	}
	if len(inside) != 4 {
		t.Fatal("the session didn't run")
	}
	if !strings.Contains(inside[0], u.Name+":x:"+itoa(u.UID)+":"+itoa(u.UID)+":Waypoint sandbox:/home/sandbox:/bin/bash\n") ||
		strings.Contains(inside[0], "devhost") {
		t.Errorf("passwd:\n%s", inside[0])
	}
	if !strings.Contains(inside[1], u.Name+":x:"+itoa(u.UID)+":") {
		t.Errorf("group:\n%s", inside[1])
	}
	if !strings.HasPrefix(inside[2], "-----BEGIN CERTIFICATE-----\nSYSTEM\n") || strings.Count(inside[2], "BEGIN CERTIFICATE") != 2 {
		t.Errorf("the session's CA bundle:\n%s", inside[2])
	}
	if inside[3] != tunnelPolicy {
		t.Errorf("policy %s", inside[3])
	}

	started, _ := r.sys.calls("waypoint-sb-s1-broker.service")
	if len(started) != 1 || started[0].UID != 998 || started[0].GID != 998 ||
		!started[0].has("LoadCredential=ca-key.pem:"+r.cfg.StatePath+"/ca-key.pem") ||
		!started[0].has("ReadWritePaths="+run+"/broker") ||
		!slices.Equal(started[0].Argv, []string{sandbox.Binary, "broker", "--policy", run + "/policy.json", "--socket", run + "/broker/proxy.sock"}) {
		t.Errorf("broker %+v", started)
	}
	_, ran := r.sys.calls("waypoint-sb-s1.service")
	if len(ran) != 1 || ran[0].UID != u.UID || ran[0].GID != u.UID || !ran[0].has("PrivateNetwork=yes") ||
		!ran[0].has("BindPaths="+filepath.Join(r.cfg.StorePath, "sessions/s1/ws")+":/work "+filepath.Join(r.cfg.StorePath, "sessions/s1/home")+":/home/sandbox") ||
		!slices.Equal(ran[0].Argv, []string{sandbox.Binary, "forward", "--launch", "/etc/waypoint-sandbox/launch.json"}) {
		t.Errorf("session %+v", ran)
	}
	_, cleaned := r.sys.calls("waypoint-sb-s1-clean.service")
	if len(cleaned) != 1 || cleaned[0].UID != u.UID || !cleaned[0].has("BindPaths="+filepath.Join(r.cfg.StorePath, "sessions/s1")+":/s") {
		t.Errorf("cleaner %+v", cleaned)
	}

	if !slices.Contains(r.sys.stopped, "waypoint-sb-s1-broker.service") {
		t.Errorf("stopped %v", r.sys.stopped)
	}
	for _, p := range []string{run, filepath.Join(r.cfg.StorePath, "sessions", "s1")} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there: %v", p, err)
		}
	}
	if st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status; len(st.Sessions) != 0 || len(st.Drift) != 0 {
		t.Errorf("after: %+v %v", st.Sessions, st.Drift)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestStartKeep(t *testing.T) {
	r, h, u := sessionRig(t)
	ws := filepath.Join(r.cfg.StorePath, "sessions", "k1", "ws")
	step := 0
	r.sys.session = func(unitCall) (int, error) {
		step++
		if step == 1 {
			return 0, os.WriteFile(filepath.Join(ws, "step1"), nil, 0o600)
		}
		if _, err := os.Stat(filepath.Join(ws, "step1")); step == 2 && err != nil {
			t.Errorf("the kept workspace lost step 1's file: %v", err)
		}
		return 0, nil
	}
	ss := newSession("k1")
	ss.Keep = true
	if resp := start(h, r.startSpec(ss)); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status
	if len(st.Sessions) != 1 || st.Sessions[0].Running || !st.Sessions[0].Keep || st.Sessions[0].UID != u.UID {
		t.Fatalf("kept %+v", st.Sessions)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.RunPath, "k1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run dir outlived the session: %v", err)
	}
	other := ss
	other.Class = sandbox.ClassPerson
	if resp := start(h, r.startSpec(other)); !strings.Contains(resp.Error, "unattended session") {
		t.Errorf("class change: %+v", resp)
	}
	ss.Keep = false
	if resp := start(h, r.startSpec(ss)); resp.Error != "" || step != 2 {
		t.Fatalf("%+v step %d", resp, step)
	}
	if _, err := os.Lstat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the workspace is still there: %v", err)
	}

	ss.ID = "k2"
	ss.Keep = true
	if resp := start(h, r.startSpec(ss)); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if resp := r.remove(h, u); !strings.Contains(resp.Error, "session") {
		t.Errorf("remove with a kept session: %+v", resp)
	}
	if resp := h.Handle(sandbox.Request{Op: sandbox.OpStop, Session: "k2"}); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status; len(st.Sessions) != 0 {
		t.Errorf("after stop %+v", st.Sessions)
	}
	if _, err := os.Lstat(filepath.Join(r.cfg.StorePath, "sessions", "k2")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stop left the workspace: %v", err)
	}
	if resp := h.Handle(sandbox.Request{Op: sandbox.OpStop, Session: "k9"}); resp.Error != "" {
		t.Error(resp.Error)
	}
	if resp := h.Handle(sandbox.Request{Op: sandbox.OpStop, Session: "../x"}); resp.Error == "" {
		t.Error("a bad session id was stopped")
	}
}

func TestStartPerson(t *testing.T) {
	r, h, u := sessionRig(t)
	ss := newSession("p1")
	ss.Class = sandbox.ClassPerson
	r.sys.session = func(c unitCall) (int, error) {
		if !c.has("BindPaths=" + filepath.Join(r.cfg.StorePath, "sessions/p1/ws") + ":/work " + filepath.Join(r.cfg.StorePath, "homes", u.Name) + ":/home/sandbox") {
			t.Errorf("a person's session doesn't get the repo user's home: %v", c.Props)
		}
		if _, err := os.Lstat(filepath.Join(r.cfg.StorePath, "sessions/p1/home")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a person's session got a throwaway home: %v", err)
		}
		return 0, nil
	}
	if resp := start(h, r.startSpec(ss)); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.StorePath, "homes", u.Name)); err != nil {
		t.Errorf("the person's home went with the session: %v", err)
	}
}

func TestStartRefuses(t *testing.T) {
	r, h, _ := sessionRig(t)
	bad := func(name string, mut func(*sandbox.Spec), want string) {
		t.Helper()
		ss := newSession("r1")
		sp := &sandbox.Spec{Op: sandbox.OpStart, Runner: 3, Level: sandbox.LevelContained, Repos: []sandbox.Repo{*waypointRepo}, Session: &ss}
		mut(sp)
		resp := start(h, r.sign(sp))
		if !strings.Contains(resp.Error, want) {
			t.Errorf("%s: got %+v, want an error with %q", name, resp, want)
		}
	}
	bad("level", func(s *sandbox.Spec) { s.Level = "tool" }, "contained")
	bad("no session", func(s *sandbox.Spec) { s.Session = nil }, "no session")
	bad("no repo", func(s *sandbox.Spec) { s.Repos = nil }, "no repository")
	bad("unknown repo", func(s *sandbox.Spec) {
		s.Repos = []sandbox.Repo{{ID: "1/99", Slug: "Robin/other", Owner: "1/robin"}}
	}, "no machine user")
	bad("unknown policy field", func(s *sandbox.Spec) { s.Session.Egress = json.RawMessage(`{"rules":[],"limit":{}}`) }, "unknown field")
	bad("credential", func(s *sandbox.Spec) {
		s.Session.Egress = json.RawMessage(`{"rules":[{"host":"git.example","cred":"forge"}]}`)
	}, "forge")
	bad("reserved env", func(s *sandbox.Spec) { s.Session.Env = map[string]string{"https_proxy": "x"} }, "sets https_proxy itself")
	bad("ld env", func(s *sandbox.Spec) { s.Session.Env = map[string]string{"LD_PRELOAD": "/x.so"} }, "itself")
	bad("relative argv", func(s *sandbox.Spec) { s.Session.Argv = []string{"make"} }, "absolute")
	bad("too much memory", func(s *sandbox.Spec) { s.Session.Resources.MemoryMB = 1 << 20 }, "memory_mb")
	bad("bad id", func(s *sandbox.Spec) { s.Session.ID = "A/../b" }, "session id")
	bad("wrong op", func(s *sandbox.Spec) { s.Op = sandbox.OpEnsureUsers }, "not start")

	sd := r.startSpec(newSession("r2"))
	if resp := start(h, sd); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if resp := start(h, sd); resp.Error == "" {
		t.Error("a start spec ran twice")
	}

	delete(r.sys.users, sandbox.EgressUser)
	bad("egress user", func(*sandbox.Spec) {}, "wp-egress")
	r.sys.users[sandbox.EgressUser] = 998

	r.sys.noListen = true
	h.sessionWait = 100 * time.Millisecond
	bad("no broker", func(*sandbox.Spec) {}, "didn't listen")
	r.sys.noListen = false
	if _, ran := r.sys.calls("waypoint-sb-r1.service"); len(ran) != 0 {
		t.Error("the session ran without its broker")
	}
	if _, err := os.Lstat(filepath.Join(r.cfg.StorePath, "sessions", "r1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed start left its workspace: %v", err)
	}
	if st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status; len(st.Sessions) != 0 || len(st.Drift) != 0 {
		t.Errorf("after refusals: %+v %v", st.Sessions, st.Drift)
	}
	if resp := h.Handle(sandbox.Request{Op: sandbox.OpStart, Spec: r.startSpec(newSession("r3"))}); !strings.Contains(resp.Error, "stdin") {
		t.Errorf("start via Handle: %+v", resp)
	}
}

// blockingSession runs until its unit is stopped, then exits as SIGTERM
// would have it.
func blockingSession(running chan<- struct{}) func(unitCall) (int, error) {
	return func(c unitCall) (int, error) {
		running <- struct{}{}
		<-c.stopSignal
		return 143, nil
	}
}

func TestStartHangupAndStop(t *testing.T) {
	r, h, _ := sessionRig(t)
	running := make(chan struct{}, 1)
	r.sys.session = blockingSession(running)

	hangup := make(chan struct{})
	done := make(chan sandbox.Response, 1)
	go func() {
		done <- h.Start(sandbox.Request{Op: sandbox.OpStart, Spec: r.startSpec(newSession("h1"))}, [3]*os.File{}, hangup)
	}()
	<-running
	if resp := start(h, r.startSpec(newSession("h1"))); !strings.Contains(resp.Error, "running") {
		t.Errorf("second start: %+v", resp)
	}
	close(hangup)
	resp := <-done
	if resp.Exit == nil || *resp.Exit != 143 {
		t.Errorf("after hang-up %+v", resp)
	}
	if _, err := os.Lstat(filepath.Join(r.cfg.StorePath, "sessions", "h1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the hung-up session's workspace is left: %v", err)
	}

	ss := newSession("h2")
	ss.Keep = true
	go func() { done <- start(h, r.startSpec(ss)) }()
	<-running
	if resp := h.Handle(sandbox.Request{Op: sandbox.OpStop, Session: "h2"}); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if resp := <-done; resp.Exit == nil || *resp.Exit != 143 {
		t.Errorf("stopped session %+v", resp)
	}
	if st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status; len(st.Sessions) != 0 {
		t.Errorf("a stopped kept session stayed: %+v", st.Sessions)
	}
}

func TestReconcileSessions(t *testing.T) {
	r, h, u := sessionRig(t)
	ss := newSession("kept")
	ss.Keep = true
	if resp := start(h, r.startSpec(ss)); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if _, err := h.store.sessionDirs("crashed", u.UID, true); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.led.Sessions = append(h.led.Sessions, sessionRecord{ID: "crashed", Class: sandbox.ClassUnattended, User: u.Name, UID: u.UID})
	if err := h.led.save(h.ledgerPath()); err != nil {
		t.Fatal(err)
	}
	h.mu.Unlock()
	for _, d := range []string{"crashed/broker", "stray/broker"} {
		if err := os.MkdirAll(filepath.Join(r.cfg.RunPath, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.dir("sessions/stranger", os.Getuid(), os.Getgid(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.cfg.StorePath, "sessions", "stranger", "ws"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.Close()

	h2 := r.open()
	st := h2.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status
	if len(st.Sessions) != 1 || st.Sessions[0].ID != "kept" {
		t.Errorf("sessions after a restart %+v", st.Sessions)
	}
	if len(st.Drift) != 1 || !strings.Contains(st.Drift[0], "stranger") {
		t.Errorf("drift %v", st.Drift)
	}
	for _, p := range []string{filepath.Join(r.cfg.StorePath, "sessions", "crashed"), filepath.Join(r.cfg.RunPath, "crashed"), filepath.Join(r.cfg.RunPath, "stray")} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the restart: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.cfg.StorePath, "sessions", "kept", "ws")); err != nil {
		t.Errorf("the kept workspace went: %v", err)
	}
	if !slices.Contains(r.sys.stopped, "waypoint-sb-crashed.service") || !slices.Contains(r.sys.stopped, "waypoint-sb-stray-broker.service") {
		t.Errorf("stopped %v", r.sys.stopped)
	}
}

// TestServeStart passes real descriptors over the helper's socket.
func TestServeStart(t *testing.T) {
	r, h, _ := sessionRig(t)
	sock := filepath.Join(t.TempDir(), "h.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = h.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })

	r.sys.session = func(c unitCall) (int, error) {
		in, err := io.ReadAll(bufio.NewReader(c.stdio[0]))
		if err != nil {
			return 1, err
		}
		_, _ = c.stdio[1].WriteString("out:" + string(in))
		_, _ = c.stdio[2].WriteString("err")
		return 7, nil
	}
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	_, _ = inW.WriteString("hello")
	_ = inW.Close()
	resp, err := sandbox.Start(context.Background(), sock, r.startSpec(newSession("v1")), inR, outW, errW)
	if err != nil || *resp.Exit != 7 {
		t.Fatalf("%+v %v", resp, err)
	}
	_ = outW.Close()
	_ = errW.Close()
	if out, _ := io.ReadAll(outR); string(out) != "out:hello" {
		t.Errorf("stdout %q", out)
	}
	if e, _ := io.ReadAll(errR); string(e) != "err" {
		t.Errorf("stderr %q", e)
	}

	dir, _ := os.Open(t.TempDir())
	defer func() { _ = dir.Close() }()
	if resp, err := sandbox.Start(context.Background(), sock, r.startSpec(newSession("v2")), dir, outR, errR); err == nil || !strings.Contains(err.Error(), "descriptor 0") {
		t.Errorf("a directory as stdin: %+v %v", resp, err)
	}
	if _, ran := r.sys.calls("waypoint-sb-v2.service"); len(ran) != 0 {
		t.Error("a session ran with a directory as stdin")
	}

	running := make(chan struct{}, 1)
	r.sys.session = blockingSession(running)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sandbox.Start(ctx, sock, r.startSpec(newSession("v3")), inR, outR, errR)
		done <- err
	}()
	<-running
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := h.Handle(sandbox.Request{Op: sandbox.OpStatus}).Status
		if len(st.Sessions) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the hung-up session is still there: %+v", st.Sessions)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := sandbox.Stop(context.Background(), sock, "v3"); err != nil {
		t.Errorf("stop after: %v", err)
	}
}

func TestCleanContents(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "ws")
	for _, d := range []string{"a/b/c", "ro/x"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "ro/x/f"), nil, 0o400)
	_ = os.Chmod(filepath.Join(dir, "ro/x"), 0o500)
	_ = os.Chmod(filepath.Join(dir, "ro"), 0o000)
	_ = os.Symlink(outside, filepath.Join(dir, "link"))
	_ = os.Symlink(root, filepath.Join(dir, "a/dirlink"))
	if err := CleanContents(dir); err != nil {
		t.Fatal(err)
	}
	if e, _ := os.ReadDir(dir); len(e) != 0 {
		t.Errorf("left %v", e)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "keep" {
		t.Errorf("cleaned through a symlink: %v", err)
	}
	if err := CleanContents(filepath.Join(root, "missing")); err != nil {
		t.Errorf("a missing dir: %v", err)
	}
	if err := CleanContents(outside); err == nil {
		t.Error("cleaned a file")
	}
}

func TestUnitProps(t *testing.T) {
	p := unitPlan{Run: "/run/waypoint-sandbox/s1", Workspace: "/store/sessions/s1/ws", Home: "/store/sessions/s1/home",
		Toolchains: []string{"/opt/go", "/opt/node"}, Resources: sandbox.Resources{CPUs: 2}}
	props := containedProps(p)
	for _, want := range []string{
		"TemporaryFileSystem=/:ro", "PrivateNetwork=yes", "PrivateDevices=yes", "NoNewPrivileges=yes", "CapabilityBoundingSet=",
		"RestrictSUIDSGID=yes", "RestrictNamespaces=yes", "SystemCallErrorNumber=EPERM", "Slice=waypoint-sandbox.slice",
		"TemporaryFileSystem=/tmp:mode=1777,size=2048M /dev/shm:mode=1777,size=512M",
		"BindPaths=/run/waypoint-sandbox/s1/broker:/run/waypoint-broker",
		"BindPaths=/store/sessions/s1/ws:/work /store/sessions/s1/home:/home/sandbox",
		"BindReadOnlyPaths=/opt/go /opt/node",
		"BindReadOnlyPaths=/run/waypoint-sandbox/s1/etc/ca-certificates.crt:/etc/ssl/certs/ca-certificates.crt",
		"CPUQuota=200%", "MemoryMax=4096M", "TasksMax=4096", "RuntimeMaxSec=21600", "WorkingDirectory=/work",
	} {
		if !slices.Contains(props, want) {
			t.Errorf("the session unit lacks %s", want)
		}
	}
	for _, prop := range props {
		if strings.Contains(prop, "/home/claude") || strings.HasPrefix(prop, "MemorySwapMax") || strings.HasPrefix(prop, "SupplementaryGroups") {
			t.Errorf("the session unit has %s", prop)
		}
	}
	b := brokerProps("/run/waypoint-sandbox/s1", "/var/lib/waypoint/sandbox")
	for _, want := range []string{"ProtectSystem=strict", "ReadWritePaths=/run/waypoint-sandbox/s1/broker", "UMask=0007",
		"LoadCredential=ca.pem:/var/lib/waypoint/sandbox/ca.pem", "CapabilityBoundingSet="} {
		if !slices.Contains(b, want) {
			t.Errorf("the broker unit lacks %s", want)
		}
	}
	c := cleanProps("/store/sessions/s1")
	if !slices.Contains(c, "BindPaths=/store/sessions/s1:/s") || !slices.Contains(c, "PrivateNetwork=yes") || !slices.Contains(c, "TemporaryFileSystem=/:ro") {
		t.Errorf("the cleaner %v", c)
	}
	for _, p := range []string{"/var/lib/a b", "rel", "/a:b", "/a/../b", "/a/", "/a$x"} {
		if unitPath(p) {
			t.Errorf("unitPath(%q)", p)
		}
	}
	if !unitPath("/var/lib/waypoint/store") {
		t.Error("the default store isn't a unit path")
	}
}
