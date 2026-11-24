package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startFixture: a repo Claude Code is trusted in, tmux on PATH, no session
// yet. launched collects what tmux was asked to start; show says whether a
// new session turns up.
type startFixture struct {
	f        *fakePane
	repo     string
	p        *runnerProc
	launched []string
	killed   []string
	renamed  string
}

func newStartFixture(t *testing.T, show bool) *startFixture {
	t.Helper()
	x := &startFixture{f: &fakePane{}}
	x.repo, x.p = restartFixture(t, x.f)
	// The fixture's old session would look like the new one.
	_ = os.Remove(filepath.Join(x.f.cfg, "sessions", strconv.Itoa(os.Getpid())+".json"))
	bin := t.TempDir()
	_ = os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\n"), 0o700) // #nosec G306 -- it has to be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WAYPOINT_SERVER", "")
	oldLaunch, oldWait, oldTmux := launchTmux, autopilotStartWait, runTmux
	t.Cleanup(func() { launchTmux, autopilotStartWait, runTmux = oldLaunch, oldWait, oldTmux })
	autopilotStartWait = 100 * time.Millisecond
	runTmux = func(ctx context.Context, socket string, stdin []byte, args ...string) (string, error) {
		switch args[0] {
		case "kill-session":
			x.killed = append(x.killed, args[len(args)-1])
			return "", nil
		case "rename-session":
			x.renamed = args[len(args)-1]
			return "", nil
		}
		return x.f.run(ctx, socket, stdin, args...)
	}
	launchTmux = func(_ context.Context, args ...string) (string, error) {
		x.launched = args
		if show {
			_ = os.MkdirAll(x.f.worktree, 0o700)
			b, _ := json.Marshal(map[string]any{"pid": fakeNewPID, "sessionId": "new-789", "status": "idle", "tmux": "w:@1.%7"})
			_ = os.WriteFile(filepath.Join(x.f.cfg, "sessions", strconv.Itoa(fakeNewPID)+".json"), b, 0o600)
			x.f.started = strings.Join(args, " ")
			x.f.typed = []string{"(launched)"}
		}
		return "%7\n", nil
	}
	runWpNext = func(context.Context, string, []string, string, string) (string, error) { return "APP-2  Second", nil }
	return x
}

func TestAutopilotStartLaunchesInTmux(t *testing.T) {
	x := newStartFixture(t, true)
	t.Setenv("WAYPOINT_TOKEN", "wpt_runner_secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("CLAUDE_ENV_FILE", "/parent/env")
	o := x.p.execAutopilotStart(context.Background(), newRun(time.Minute), autopilotJob{Dir: x.repo, Key: "APP-2", Agent: "anthropic/claude-code"})
	r, _ := o.Result.(autopilotResult)
	if o.State != "done" || r.Outcome != "started" || r.Via != "start" || r.NewCLI != "new-789" {
		t.Fatalf("%+v %+v", o, r)
	}
	got := strings.Join(x.launched, " ")
	for _, want := range []string{"new-session -d -P -F #{pane_id} -s wp-start-0-app-2-", "-c " + x.repo, "-x 200 -y 50", "-- /bin/sh -c " + autopilotStartShell + " sh", " WAYPOINT_UNATTENDED=1", " PATH=", " --worktree app-2 --rc --settings " + unattendedSettings} {
		if !strings.Contains(got, want) {
			t.Errorf("launched %q, want %q in it", got, want)
		}
	}
	for _, secret := range []string{"wpt_runner_secret", "sk-secret", "/parent/env"} {
		if strings.Contains(got, secret) {
			t.Errorf("launched %q: the runner's %q went along", got, secret)
		}
	}
	if !strings.HasPrefix(x.renamed, "wp-app-2-") {
		t.Errorf("renamed to %q: a runner restart would end this session", x.renamed)
	}
	if x.f.pasted != "APP-2  Second" {
		t.Errorf("pasted %q", x.f.pasted)
	}
	if len(x.killed) != 0 {
		t.Errorf("killed %v", x.killed)
	}
}

// A session that never shows up leaves no tmux session behind.
func TestAutopilotStartCleansUp(t *testing.T) {
	x := newStartFixture(t, false)
	o := x.p.execAutopilotStart(context.Background(), newRun(time.Minute), autopilotJob{Dir: x.repo, Key: "APP-2"})
	if r, _ := o.Result.(autopilotResult); r.Outcome != "cleared" || !strings.Contains(r.Detail, "no session showed up") {
		t.Fatalf("%+v %+v", o, r)
	}
	if len(x.killed) != 1 || !strings.HasPrefix(x.killed[0], "wp-start-0-app-2-") || x.renamed != "" {
		t.Errorf("killed %v, renamed %q", x.killed, x.renamed)
	}
}

// Never in the person's checkout: without a worktree nothing starts.
func TestAutopilotStartNeedsAWorktree(t *testing.T) {
	x := newStartFixture(t, true)
	dir := t.TempDir()
	x.p.allowed = append(x.p.allowed, dir)
	o := x.p.execAutopilotStart(context.Background(), newRun(time.Minute), autopilotJob{Dir: dir, Key: "APP-2"})
	if r, _ := o.Result.(autopilotResult); r.Outcome != "held" || r.Reason != "no_worktree" || x.launched != nil {
		t.Fatalf("%+v %+v, launched %v", o, r, x.launched)
	}
}

func TestAutopilotStartNoTmux(t *testing.T) {
	x := newStartFixture(t, true)
	t.Setenv("PATH", t.TempDir())
	o := x.p.execAutopilotStart(context.Background(), newRun(time.Minute), autopilotJob{Dir: x.repo, Key: "APP-2"})
	if r, _ := o.Result.(autopilotResult); r.Outcome != "held" || r.Reason != "no_tmux" || x.launched != nil {
		t.Fatalf("%+v %+v", o, r)
	}
	if slices.Contains(runnerFeatureList(), "autopilot_start") {
		t.Error("a machine with no tmux says it can start sessions")
	}
}

// A step stopped while the job was queued starts nothing.
func TestAutopilotStartUnwanted(t *testing.T) {
	x := newStartFixture(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"wanted":false,"pulls":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_runner")
	o := x.p.execAutopilotStart(context.Background(), newRun(time.Minute), autopilotJob{Dir: x.repo, Key: "APP-2", Step: 7})
	if r, _ := o.Result.(autopilotResult); r.Outcome != "unwanted" || x.launched != nil {
		t.Fatalf("%+v %+v, launched %v", o, r, x.launched)
	}
}

// Stopped while the new session was getting ready: it is closed, not given
// the task the stop just released.
func TestAutopilotStartStoppedWhileWaiting(t *testing.T) {
	x := newStartFixture(t, true)
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/wanted") {
			_, _ = w.Write([]byte(`{"pulls":[]}`))
			return
		}
		asked++
		_, _ = w.Write([]byte(`{"wanted":` + strconv.FormatBool(asked <= 2) + `}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_runner")
	o := x.p.execAutopilotStart(context.Background(), newRun(time.Minute), autopilotJob{Dir: x.repo, Key: "APP-2", Step: 7})
	if r, _ := o.Result.(autopilotResult); r.Outcome != "unwanted" || x.f.pasted != "" {
		t.Fatalf("%+v %+v, pasted %q", o, r, x.f.pasted)
	}
	if len(x.killed) != 1 || x.renamed != "" {
		t.Errorf("killed %v, renamed %q", x.killed, x.renamed)
	}
}

// A runner that starts up ends what it was still starting when it stopped,
// and nothing else.
func TestRunnerReapsInterruptedStarts(t *testing.T) {
	_, p := restartFixture(t, &fakePane{})
	p.id = 3
	old := runTmux
	t.Cleanup(func() { runTmux = old })
	var killed []string
	runTmux = func(_ context.Context, _ string, _ []byte, args ...string) (string, error) {
		switch args[0] {
		case "list-sessions":
			return "main\nwp-app-2-41\nwp-start-3-app-3-42\nwp-start-4-app-4-43\n", nil
		case "kill-session":
			killed = append(killed, args[len(args)-1])
		}
		return "", nil
	}
	p.reapInterruptedStarts(context.Background())
	if !slices.Equal(killed, []string{"wp-start-3-app-3-42"}) {
		t.Errorf("killed %v", killed)
	}
}

func TestRunnerFeaturesWithTmux(t *testing.T) {
	bin := t.TempDir()
	_ = os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\n"), 0o700) // #nosec G306 -- it has to be executable
	t.Setenv("PATH", bin)
	if !slices.Contains(runnerFeatureList(), "autopilot_start") {
		t.Errorf("features = %v", runnerFeatureList())
	}
}
