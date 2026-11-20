package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bp(b bool) *bool { return &b }

// WP-1265: autopilot's sessions get the person's settings in place of the
// flags they set; Remote Control stays on by default unless Code Mode is.
func TestSessionSettingsApplyToAutopilotFlags(t *testing.T) {
	old := []string{"--rc", "--model", "sonnet", "--effort=low", "--chrome", "--settings", unattendedSettings}
	for _, c := range []struct {
		name string
		set  sessionSettings
		want string
	}{
		{"nothing set", sessionSettings{}, "--rc --model sonnet --effort=low --chrome --settings " + unattendedSettings},
		{"settings win", sessionSettings{Model: "opus", Effort: "max", PermissionMode: "auto", Chrome: bp(false)},
			"--rc --settings " + unattendedSettings + " --model opus --effort max --permission-mode auto --no-chrome"},
		{"remote control off", sessionSettings{RemoteControl: bp(false)}, "--model sonnet --effort=low --chrome --settings " + unattendedSettings},
		{"code mode turns the default off", sessionSettings{CodeMode: bp(true)}, "--model sonnet --effort=low --chrome --settings " + unattendedSettings},
		{"both asked for", sessionSettings{CodeMode: bp(true), RemoteControl: bp(true)}, "--rc --model sonnet --effort=low --chrome --settings " + unattendedSettings},
	} {
		if got := strings.Join(c.set.applyTo(old), " "); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	if got := strings.Join(sessionSettings{RemoteControl: bp(true)}.applyTo([]string{"--worktree", "x"}), " "); got != "--worktree x --rc" {
		t.Errorf("start: %s", got)
	}
	if got := (sessionSettings{CodeMode: bp(false)}).env(); len(got) != 1 || got[0] != codeModeVar+"=0" {
		t.Errorf("env %v", got)
	}
}

// A launch's own choices win; the settings fill the rest, and Code Mode
// reaches the environment even though CLAUDE_CODE_* from the parent doesn't.
func TestSessionSettingsFillALaunch(t *testing.T) {
	prefs := &sessionPrefs{
		All:   sessionSettings{Model: "sonnet", Effort: "high", CodeMode: bp(true)},
		Repos: map[string]sessionSettings{"robin/waypoint": {RemoteControl: bp(true), Chrome: bp(true), Effort: "max"}},
	}
	l := taskLaunch{model: "opus", prompt: "go"}
	prefs.forRepo("Robin/Waypoint").fill(&l)
	if got := strings.Join(l.claudeArgs(), " "); got != "--model opus --effort max --rc --chrome -- go" {
		t.Errorf("args %s", got)
	}
	env := strings.Join(l.sessionEnv([]string{"PATH=/bin", codeModeVar + "=0"}), " ")
	if env != "PATH=/bin "+codeModeVar+"=1" {
		t.Errorf("env %s", env)
	}
	l = taskLaunch{prompt: "go"}
	prefs.forRepo("robin/other").fill(&l)
	if got := strings.Join(l.claudeArgs(), " "); got != "--model sonnet --effort high -- go" {
		t.Errorf("elsewhere %s", got)
	}
}

func TestRelaunchLineCarriesTheSettingsVariables(t *testing.T) {
	rl := &relaunch{bin: "claude", args: []string{"--rc"}, env: []string{codeModeVar + "=1"}, cdTo: "/w"}
	if got := rl.commandLine(); got != "cd /w && env "+codeModeVar+"=1 claude --rc" {
		t.Errorf("line %q", got)
	}
}

func TestAutopilotStartUsesTheSessionSettings(t *testing.T) {
	x := newStartFixture(t, true)
	run := newRun(time.Minute)
	run.launch = &sessionPrefs{All: sessionSettings{CodeMode: bp(true), Model: "opus"}}
	o := x.p.execAutopilotStart(context.Background(), run, autopilotJob{Dir: x.repo, Key: "APP-2"})
	if r, _ := o.Result.(autopilotResult); r.Outcome != "started" {
		t.Fatalf("%+v", o)
	}
	got := strings.Join(x.launched, " ")
	if !strings.Contains(got, " "+codeModeVar+"=1 ") || !strings.Contains(got, "claude --worktree app-2 --model opus --settings "+unattendedSettings) || strings.Contains(got, "--rc") {
		t.Errorf("launched %q", got)
	}
}

func TestAutopilotRestartUsesTheSessionSettings(t *testing.T) {
	f := &fakePane{respawn: true}
	repo, p := restartFixture(t, f)
	runWpNext = func(context.Context, string, []string, string, string) (string, error) { return "APP-2  Second", nil }
	run := newRun(time.Minute)
	run.launch = &sessionPrefs{All: sessionSettings{RemoteControl: bp(false), Effort: "max"}}
	o := p.execAutopilot(context.Background(), run, autopilotJob{CLIID: "old-123", Dir: repo, Key: "APP-2"})
	if r, _ := o.Result.(autopilotResult); r.Outcome != "started" {
		t.Fatalf("%+v", o)
	}
	if !strings.HasSuffix(f.started, "claude --worktree app-2 --settings "+shellWord(unattendedSettings)+" --effort max") {
		t.Errorf("started %q", f.started)
	}
}

func TestSessionJobUsesTheSessionSettings(t *testing.T) {
	rec := fakeCLI(t, "claude", fakeClaudeBG)
	dir := t.TempDir()
	p := newTestProc(t, dir)
	p.clis = []runnerCLI{{Name: "claude"}}
	run := newRun(time.Minute)
	run.launch = &sessionPrefs{All: sessionSettings{Model: "sonnet", Effort: "low", RemoteControl: bp(true), CodeMode: bp(false)}}
	if o := p.execSession(context.Background(), run, sessionJob{Dir: dir, Prompt: "go", Model: "opus"}); o.State != "done" {
		t.Fatalf("outcome = %+v (%s)", o, run.out.text())
	}
	args, _ := os.ReadFile(filepath.Join(rec, "args"))
	if want := "--bg\n--settings\n" + unattendedSettings + "\n--model\nopus\n--effort\nlow\n--rc\n--\ngo\n"; string(args) != want {
		t.Errorf("args = %q", args)
	}
	if env, _ := os.ReadFile(filepath.Join(rec, "env")); !strings.Contains(string(env), codeModeVar+"=0\n") {
		t.Errorf("env = %s", env)
	}
}

// A permission mode set in the settings takes the place of the old
// session's skip-permissions flag too: the person's mode is what runs
// (WP-1265 review 1003.4).
func TestSessionSettingsPermissionModeReplacesSkipPermissions(t *testing.T) {
	old := []string{"--dangerously-skip-permissions", "--allow-dangerously-skip-permissions", "--permission-mode=auto", "--rc"}
	if got := strings.Join(sessionSettings{PermissionMode: "plan"}.applyTo(old), " "); got != "--allow-dangerously-skip-permissions --rc --permission-mode plan" {
		t.Errorf("mode set: %s", got)
	}
	if got := strings.Join(sessionSettings{}.applyTo(old), " "); got != strings.Join(old, " ") {
		t.Errorf("mode unset: %s", got)
	}
}

// wp start and wp fanout say when they can't read the settings, rather
// than starting without them in silence (WP-1265 review 1003.2).
func TestMySessionPrefsSaysWhenItCantRead(t *testing.T) {
	status := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "down", status)
			return
		}
		_, _ = w.Write([]byte(`{"prefs":{"week_start":1}}`))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "x")
	var errb strings.Builder
	c := &cli{out: &strings.Builder{}, err: &errb}
	if p := c.mySessionPrefs(); p != nil || !strings.Contains(errb.String(), "Claude Code settings") {
		t.Errorf("server error: %v, said %q", p, errb.String())
	}
	errb.Reset()
	status = http.StatusOK
	if p := c.mySessionPrefs(); p != nil || errb.Len() != 0 {
		t.Errorf("none set: %v, said %q", p, errb.String())
	}
}
