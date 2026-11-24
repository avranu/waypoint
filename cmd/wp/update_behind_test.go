package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// behindEnv: an installed wp, a recorder in place of the detached upgrade,
// and a fresh cli per call (each wp command is its own process).
type behindEnv struct {
	t       *testing.T
	exe     string
	mu      sync.Mutex
	started []string
}

func newBehindEnv(t *testing.T) *behindEnv {
	t.Helper()
	b := &behindEnv{t: t}
	b.exe = installedWP(t)
	old := backgroundStart
	backgroundStart = func(bin string, args ...string) {
		b.mu.Lock()
		b.started = append(b.started, bin+" "+strings.Join(args, " "))
		b.mu.Unlock()
	}
	t.Cleanup(func() { backgroundStart = old })
	return b
}

func (b *behindEnv) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.started)
}

// finish is what run does when a command ends having seen server version seen.
func (b *behindEnv) finish(seen string, cmd *command) {
	b.t.Helper()
	c, err := newCLI(io.Discard, io.Discard)
	if err != nil {
		b.t.Fatal(err)
	}
	c.cur = cmd
	if seen != "" {
		c.seeServerVersion(seen)
	}
	c.autoUpdateIfBehind()
}

func (b *behindEnv) ageRecord(d time.Duration) {
	b.t.Helper()
	c, _ := newCLI(io.Discard, io.Discard)
	_ = c.updateState(func(s *State) { s.Update.Checked = s.Update.Checked.Add(-d) })
}

func TestAutoUpdateIfBehind(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")

	b.finish("v1.2.0", someCmd)
	if b.count() != 1 || b.started[0] != b.exe+" upgrade --auto" {
		t.Fatalf("a differing release: started %v", b.started)
	}
	// Claimed up front: the download is still running ("started"), so other
	// wp processes don't begin a second one.
	if c, _ := newCLI(io.Discard, io.Discard); c.state.Update == nil || c.state.Update.Result != "started" || c.state.Update.Server != "v1.2.0" {
		t.Fatalf("claim = %+v", c.state.Update)
	}
	b.finish("v1.2.0", someCmd)
	b.ageRecord(5*time.Hour + 50*time.Minute)
	b.finish("v1.2.0", someCmd)
	if b.count() != 1 {
		t.Fatalf("the same version within 6h started again: %v", b.started)
	}
	b.finish("v1.3.0", someCmd) // a newer release at once
	if b.count() != 2 {
		t.Fatalf("a newer version: %v", b.started)
	}
	b.ageRecord(6 * time.Hour)
	b.finish("v1.3.0", someCmd)
	if b.count() != 3 {
		t.Fatalf("after 6h: %v", b.started)
	}
}

func TestAutoUpdateIfBehindStaysQuiet(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	for name, tc := range map[string]struct {
		mine, seen string
		cmd        *command
		off        bool
	}{
		"nothing seen":        {mine: "v1.1.0", seen: "", cmd: someCmd},
		"equal":               {mine: "v1.2.0", seen: "v1.2.0", cmd: someCmd},
		"my build is dev":     {mine: "dev", seen: "v1.2.0", cmd: someCmd},
		"my build is dirty":   {mine: "v1.1.0-dirty", seen: "v1.2.0", cmd: someCmd},
		"server not release":  {mine: "v1.1.0", seen: "v1.2.0-3-gabc1234", cmd: someCmd},
		"opted out":           {mine: "v1.1.0", seen: "v1.2.0", cmd: someCmd, off: true},
		"the foreground loop": {mine: "v1.1.0", seen: "v1.2.0", cmd: runnerCmd},
		"upgrade itself":      {mine: "v1.1.0", seen: "v1.2.0", cmd: updateCmd},
	} {
		t.Run(name, func(t *testing.T) {
			setVersion(t, tc.mine)
			if tc.off {
				t.Setenv("WAYPOINT_NO_AUTO_UPDATE", "1")
			}
			b.finish(tc.seen, tc.cmd)
			if b.count() != 0 {
				t.Fatalf("started %v", b.started)
			}
		})
	}
	// A temporary build (go run, go test) is left alone too.
	setVersion(t, "v1.1.0")
	old := selfExe
	selfExe = func() (string, error) { return os.TempDir() + "/go-build123/b001/exe/wp", nil }
	t.Cleanup(func() { selfExe = old })
	b.finish("v1.2.0", someCmd)
	if b.count() != 0 {
		t.Fatalf("a throwaway build started %v", b.started)
	}
}

// Through the real dispatch: a normal command and a hook event each show the
// server's release and end by starting the upgrade, whatever the answer.
func TestAutoUpdateIfBehindEndToEnd(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	e.runnerUser("robin")
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")

	e.run("auth", "status") // the answer may be an error: its header still counts
	if b.count() != 1 || b.started[0] != b.exe+" upgrade --auto" {
		t.Fatalf("after a command: %v", b.started)
	}

	b.ageRecord(7 * time.Hour)
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"hello","cwd":"` + t.TempDir() + `"}`)
	_, _ = f.Seek(0, 0)
	oldIn := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = oldIn })
	out, code := e.run("hook", "claude-event")
	if code != 0 {
		t.Fatalf("hook exit %d: %s", code, out)
	}
	if b.count() != 2 {
		t.Fatalf("after a hook event: %v", b.started)
	}
}

// someCmd stands for any ordinary command.
var someCmd = &command{name: "ls"}

// A job's wp must not upgrade on a runner that opted out (WP-1773): the
// runner's own switches reach the job as WAYPOINT_NO_AUTO_UPDATE.
func TestJobEnvCarriesTheRunnersUpdateOptOut(t *testing.T) {
	has := func(env []string) bool { return slices.Contains(env, "WAYPOINT_NO_AUTO_UPDATE=1") }
	for name, tc := range map[string]struct {
		opts runnerOpts
		env  string
		want bool
	}{
		"updating":         {runnerOpts{}, "", false},
		"--no-auto-update": {runnerOpts{NoAutoUpdate: true}, "", true},
		"--once":           {runnerOpts{Once: true}, "", true},
		"the runner's env": {runnerOpts{}, "1", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("WAYPOINT_NO_AUTO_UPDATE", tc.env)
			p := &runnerProc{opts: tc.opts}
			if got := has(p.updateOptOutEnv()); got != tc.want {
				t.Fatalf("opt-out env = %v, want %v", p.updateOptOutEnv(), tc.want)
			}
			if got := has(runnerSelfEnv(nil, p.updateOptOutEnv()...)); got != tc.want {
				t.Fatalf("command job env lacks it: %v", runnerSelfEnv(nil, p.updateOptOutEnv()...))
			}
		})
	}
	// With it set, the job's wp stays quiet.
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")
	t.Setenv("WAYPOINT_NO_AUTO_UPDATE", "1")
	b.finish("v1.2.0", someCmd)
	if b.count() != 0 {
		t.Fatalf("started %v", b.started)
	}
}

// Runner and wp commands see each other's work: a failed server check keeps
// the claimed version's backoff, and the on-disk binary counts.
func TestAutoUpdateKeepsTheClaimedVersionWhenTheServerCheckFails(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")
	b.finish("v1.2.0", someCmd)
	c, _ := newCLI(io.Discard, io.Discard)
	c.autoUpdate(context.Background(), "", errors.New("down"))
	if c.state.Update == nil || c.state.Update.Server != "v1.2.0" || !strings.Contains(c.state.Update.Result, "couldn't ask the server") {
		t.Fatalf("record = %+v", c.state.Update)
	}
	b.finish("v1.2.0", someCmd)
	if b.count() != 1 {
		t.Fatalf("started again: %v", b.started)
	}
}

func TestSelfUpdateSkipsTheDownloadWhenTheDiskHasIt(t *testing.T) {
	setVersion(t, "v1.1.0")
	exe := installedWP(t)
	// Another process already put v1.2.0 here; this cli has no server to download from.
	_ = os.WriteFile(exe, []byte("#!/bin/sh\necho 'wp v1.2.0'\n"), 0o755)
	c := &cli{dir: t.TempDir(), out: io.Discard, err: io.Discard}
	got, path, err := c.selfUpdate(context.Background(), "v1.2.0")
	if err != nil || got != "v1.2.0" || path != exe {
		t.Fatalf("got %q %q %v", got, path, err)
	}
	// --force with the running version still downloads (here: fails for want of a server).
	setVersion(t, "v1.2.0")
	if _, _, err := c.selfUpdate(context.Background(), "v1.2.0"); err == nil {
		t.Fatal("a binary equal to the running one was taken as already updated")
	}
}

func TestRunnerRecordsItsUpdateInTheSharedState(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	b := &behindEnv{t: t, exe: u.exe}
	old := backgroundStart
	backgroundStart = func(bin string, args ...string) { b.started = append(b.started, bin) }
	t.Cleanup(func() { backgroundStart = old })
	started := make(chan struct{})
	release := make(chan struct{})
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		close(started)
		<-release
		return want, u.exe, nil
	}
	u.busy(1)
	u.step()
	<-started
	c, _ := newCLI(io.Discard, io.Discard)
	if c.state.Update == nil || c.state.Update.Server != "v1.2.0" || c.state.Update.Result != "started" {
		t.Fatalf("claim = %+v", c.state.Update)
	}
	c.seeServerVersion("v1.2.0")
	c.autoUpdateIfBehind() // a wp command during the runner's download
	if len(b.started) != 0 {
		t.Fatalf("a second download started: %v", b.started)
	}
	close(release)
	waitFor(t, "the result", 5*time.Second, func() bool { u.step(); return u.p.updatedTo == "v1.2.0" })
	c, _ = newCLI(io.Discard, io.Discard)
	if !strings.Contains(c.state.Update.Result, "updated from v1.1.0 to v1.2.0") {
		t.Fatalf("result = %+v", c.state.Update)
	}
}

func TestFetchPublicAndStreamGetRecordTheServerVersion(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	_ = e
	c, _ := newCLI(io.Discard, io.Discard)
	if _, err := c.serverVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.sawVersion.Load().(string); v != "v1.2.0" {
		t.Errorf("fetchPublic saw %q", v)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Waypoint-Version", " v1.4.0 ")
		_, _ = w.Write([]byte("body"))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	c2, _ := newCLI(io.Discard, io.Discard)
	var sink bytes.Buffer
	if err := c2.streamGet("/x", &sink); err != nil {
		t.Fatal(err)
	}
	if v, _ := c2.sawVersion.Load().(string); v != "v1.4.0" {
		t.Errorf("streamGet saw %q", v)
	}
}

func TestSeeServerVersionIgnoresBlankAndTrims(t *testing.T) {
	c := &cli{}
	c.seeServerVersion("   ")
	c.seeServerVersion("")
	if v, _ := c.sawVersion.Load().(string); v != "" {
		t.Fatalf("saw %q", v)
	}
	c.seeServerVersion("  v1.2.0\n")
	c.seeServerVersion("") // a later answer without it keeps what was seen
	if v, _ := c.sawVersion.Load().(string); v != "v1.2.0" {
		t.Fatalf("saw %q", v)
	}
}

// A cli that loaded its state before another one claimed must not start a
// second upgrade: the claim re-checks under the lock.
func TestClaimRechecksUnderTheLock(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")
	a, _ := newCLI(io.Discard, io.Discard)
	stale, _ := newCLI(io.Discard, io.Discard) // loaded before a claims
	a.seeServerVersion("v1.2.0")
	stale.seeServerVersion("v1.2.0")
	a.cur, stale.cur = someCmd, someCmd
	a.autoUpdateIfBehind()
	stale.autoUpdateIfBehind()
	if b.count() != 1 {
		t.Fatalf("started %v", b.started)
	}
}

// wp follows the server, down as well as up (a rollback).
func TestAutoUpdateFollowsAnOlderServer(t *testing.T) {
	cliServer(t, "v1.0.0", "v1.0.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")
	b.finish("v1.0.0", someCmd)
	if b.count() != 1 {
		t.Fatalf("an older server release: %v", b.started)
	}
}

func TestRunnerStatusSaysWhenAnotherReleaseRuns(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	me := e.runnerUser("robin")
	newBehindEnv(t)
	setVersion(t, "v1.1.0")
	for name, v := range map[string]string{"old": "v0.149.0", "same": "v1.2.0", "dev": "dev"} {
		if _, err := e.svc.RegisterRunner(context.Background(), me, core.RunnerRegistration{Name: name, Version: v}); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := e.run("runner", "status")
	if !strings.Contains(out, "wp v0.149.0, server is v1.2.0)") {
		t.Errorf("no mark on the old runner:\n%s", out)
	}
	if strings.Contains(out, "wp v1.2.0, server") || strings.Contains(out, "wp dev, server") {
		t.Errorf("marked a runner that doesn't differ:\n%s", out)
	}
}
