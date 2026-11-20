package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRelaunchArgs(t *testing.T) {
	for in, want := range map[string]string{
		"--rc": "--rc",
		"--rc --allow-dangerously-skip-permissions":     "--rc --allow-dangerously-skip-permissions",
		"--remote-control":                              "--remote-control",
		"--model opus --rc -n waypoint":                 "--model opus --rc",
		"--resume abc --rc":                             "--rc",
		"--permission-mode=auto Handle WP-1":            "--permission-mode=auto",
		"--add-dir /x --mystery value --verbose":        "--add-dir /x --verbose",
		"--effort high --session-id 1-2-3 -c --chrome":  "--effort high --chrome",
		"--remote-control-session-name-prefix box --rc": "--remote-control-session-name-prefix box --rc",
	} {
		if got := strings.Join(relaunchArgs(strings.Fields(in)), " "); got != want {
			t.Errorf("relaunchArgs(%q) = %q, want %q", in, got, want)
		}
	}
	rl := &relaunch{bin: "claude", args: []string{"--rc", "--add-dir", "/a b"}}
	if got := rl.commandLine(); got != `claude --rc --add-dir '/a b'` {
		t.Errorf("commandLine = %s", got)
	}
	rl.cdTo = "/a b"
	if got := rl.commandLine(); got != `cd '/a b' && claude --rc --add-dir '/a b'` {
		t.Errorf("commandLine with cd = %s", got)
	}
	if got := strings.Join(relaunchArgs(strings.Fields("--worktree wp-1 --rc")), " "); got != "--rc" {
		t.Errorf("the old worktree stayed in the flags: %s", got)
	}
	if got := shellWord("it's done; next: WP-2"); got != `'it'\''s done; next: WP-2'` {
		t.Errorf("shellWord = %s", got)
	}
}

// fakePane plays a pane where Claude Code runs under bash (or is the pane's
// own process, respawn): /exit ends it, the command line (or respawn) starts
// a new session, and a paste and Enter there set it to work.
type fakePane struct {
	cfg     string
	respawn bool
	exited  bool
	typed   []string
	started string // the command line Claude Code was started with
	opts    []string
	pasted  string
	dialog  string // what the new session shows instead of its input box
	// exitDialog is what /exit asks in a worktree with commits; Enter answers.
	exitDialog string
	asked      bool
	worktree   string // the directory --worktree makes
	respawnIn  string // respawn-pane's -c
}

const fakeNewPID = 4242

func (f *fakePane) run(ctx context.Context, socket string, stdin []byte, args ...string) (string, error) {
	status := func(s string) {
		b, _ := json.Marshal(map[string]any{"pid": fakeNewPID, "sessionId": "new-789", "status": s, "tmux": "w:@1.%7"})
		_ = os.WriteFile(filepath.Join(f.cfg, "sessions", strconv.Itoa(fakeNewPID)+".json"), b, 0o600)
	}
	start := func(line string) {
		f.started = line
		if f.worktree != "" && strings.Contains(line, "--worktree") {
			_ = os.MkdirAll(f.worktree, 0o700)
		}
		status("idle")
	}
	switch args[0] {
	case "capture-pane":
		if f.asked && !f.exited {
			return f.exitDialog, nil
		}
		if f.started != "" && f.dialog != "" {
			return f.dialog, nil
		}
		return screenEmpty, nil
	case "display-message":
		switch args[len(args)-1] {
		case "#{pane_pid}":
			if f.respawn {
				return strconv.Itoa(os.Getpid()), nil
			}
			return "100", nil
		case "#{pane_current_command}":
			if f.exited {
				return "bash", nil
			}
			return "claude", nil
		case "#{pane_dead}":
			if f.exited {
				return "1", nil
			}
			return "0", nil
		}
	case "send-keys":
		key := args[len(args)-1]
		f.typed = append(f.typed, key)
		if key == "Enter" && len(f.typed) >= 2 {
			switch prev := f.typed[len(f.typed)-2]; {
			case prev == "/exit" && f.exitDialog != "":
				f.asked = true
			case prev == "/exit", f.asked && !f.exited:
				f.exited = true
			case f.pasted != "":
				status("busy")
			case f.exited && !f.respawn:
				start(strings.TrimPrefix(prev, " "))
			}
		}
		return "", nil
	case "load-buffer":
		f.pasted = string(stdin)
		return "", nil
	case "paste-buffer":
		return "", nil
	case "show-options":
		return "", nil
	case "set-option":
		f.opts = append(f.opts, strings.Join(args[1:], " "))
		return "", nil
	case "respawn-pane":
		if !f.exited {
			return "", errors.New("pane still active")
		}
		f.respawnIn = args[len(args)-2]
		start(args[len(args)-1])
		return "", nil
	}
	return "", errors.New("unexpected tmux " + strings.Join(args, " "))
}

// restartFixture is a session (this process, as far as the runner can tell)
// idle in a pane, in a git repo with an origin.
func restartFixture(t *testing.T, f *fakePane) (repo string, p *runnerProc) {
	t.Helper()
	f.cfg = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", f.cfg)
	_ = os.MkdirAll(filepath.Join(f.cfg, "sessions"), 0o700)
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "old-123", "status": "idle", "tmux": "w:@1.%7"})
	_ = os.WriteFile(filepath.Join(f.cfg, "sessions", strconv.Itoa(os.Getpid())+".json"), b, 0o600)
	repo = newTidyFixture(t).repo
	f.worktree = filepath.Join(repo, ".claude", "worktrees", "app-2")
	oldTmux, oldNext, oldPoll, oldSettle, oldProc, oldAlive := runTmux, runWpNext, autopilotPoll, autopilotSettle, procOf, pidAlive
	runTmux, autopilotPoll, autopilotSettle = f.run, 5*time.Millisecond, 5*time.Millisecond
	procOf = func(pid int) (procInfo, error) {
		switch pid {
		case os.Getpid():
			return procInfo{PPID: 100, Comm: "claude", Args: []string{"claude", "--rc", "-n", "old"}, Dir: repo}, nil
		case 100:
			return procInfo{PPID: 1, Comm: "bash", Dir: repo}, nil
		}
		return procInfo{}, errors.New("no such process")
	}
	pidAlive = func(pid int) bool { return pid != os.Getpid() || !f.exited }
	t.Cleanup(func() {
		runTmux, runWpNext, autopilotPoll, autopilotSettle, procOf, pidAlive = oldTmux, oldNext, oldPoll, oldSettle, oldProc, oldAlive
	})
	return repo, newTestProc(t, repo)
}

func TestAutopilotJobRestartsTheSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"pulls":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_runner")
	for _, respawn := range []bool{false, true} {
		f := &fakePane{respawn: respawn}
		repo, p := restartFixture(t, f)
		var nextEnv []string
		var nextDir string
		runWpNext = func(_ context.Context, dir string, env []string, _, _ string) (string, error) {
			if f.started == "" {
				t.Error("wp next ran before the new session started")
			}
			nextDir, nextEnv = dir, env
			return "APP-2  Second", nil
		}
		run := newRun(time.Minute)
		run.token = "wpt_job"
		o := p.execAutopilot(context.Background(), run, autopilotJob{CLIID: "old-123", Dir: repo, Key: "APP-2", Done: "APP-1", Agent: "anthropic/claude-code"})
		r, _ := o.Result.(autopilotResult)
		if o.State != "done" || r.Outcome != "started" || r.Via != "restart" || r.NewCLI != "new-789" || r.Warning != "" {
			t.Fatalf("respawn %v: %+v %+v", respawn, o, r)
		}
		if !strings.HasSuffix(f.started, "claude --worktree app-2 --rc --settings "+shellWord(unattendedSettings)) {
			t.Errorf("respawn %v: started %q", respawn, f.started)
		}
		if nextDir != f.worktree {
			t.Errorf("respawn %v: wp next ran in %s, not the worktree", respawn, nextDir)
		}
		if !strings.HasPrefix(f.pasted, "APP-2") ||
			!strings.Contains(f.pasted, "APP-2  Second") {
			t.Errorf("respawn %v: pasted %q", respawn, f.pasted)
		}
		// The session's hook will know it for autopilot's own (WP-308).
		if key, ok := p.c.pastedPromptKey("new-789", "  "+f.pasted+"\n"); !ok || key != "APP-2" {
			t.Errorf("respawn %v: pasted prompt not noted: %q %v", respawn, key, ok)
		}
		env := strings.Join(nextEnv, "\n")
		if !strings.Contains(env, "WAYPOINT_SESSION=new-789") || !strings.Contains(env, "WAYPOINT_AGENT=anthropic/claude-code") {
			t.Errorf("respawn %v: wp next ran for another session:\n%s", respawn, env)
		}
		// wp next runs as the job's own token, never the runner's (WP-418).
		if !strings.Contains(env, "WAYPOINT_TOKEN=wpt_job") || strings.Contains(env, "wpt_runner") {
			t.Errorf("respawn %v: wp next env has the wrong token:\n%s", respawn, env)
		}
		if respawn {
			if strings.Join(f.opts, "; ") != "-p -t %7 remain-on-exit on; -p -u -t %7 remain-on-exit" {
				t.Errorf("respawn: options %q", f.opts)
			}
			if f.respawnIn != repo {
				t.Errorf("respawned in %q, not the repo's root", f.respawnIn)
			}
		} else if strings.Join(f.typed, "|") != "/exit|Enter| claude --worktree app-2 --rc --settings "+shellWord(unattendedSettings)+"|Enter|Enter" {
			t.Errorf("typed %q", f.typed)
		}
	}
}

// Leaving a worktree with commits asks a question: Keep is the answer, and
// the session starts once it has gone.
func TestAutopilotRestartAnswersKeepAtTheExitDialog(t *testing.T) {
	f := &fakePane{exitDialog: exitDialogKeep}
	repo, p := restartFixture(t, f)
	runWpNext = func(context.Context, string, []string, string, string) (string, error) { return "APP-2", nil }
	o := p.execAutopilot(context.Background(), newRun(time.Minute), autopilotJob{CLIID: "old-123", Dir: repo, Key: "APP-2"})
	if r, _ := o.Result.(autopilotResult); r.Outcome != "started" {
		t.Fatalf("%+v %+v", o, r)
	}
	if got := strings.Join(f.typed, "|"); got != "/exit|Enter|Enter| claude --worktree app-2 --rc --settings "+shellWord(unattendedSettings)+"|Enter|Enter" {
		t.Errorf("typed %q", got)
	}
}

// A restarted session that never shows an empty input box (a dialog, or
// someone typing there) gets no prompt, and the person hears it has none.
func TestAutopilotRestartWaitsForTheInputBox(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	_ = os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700)
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "old-123", "status": "idle", "tmux": "w:@1.%7"})
	_ = os.WriteFile(filepath.Join(cfg, "sessions", strconv.Itoa(os.Getpid())+".json"), b, 0o600)
	dir := t.TempDir()
	f := &fakePane{cfg: cfg, dialog: "Do you trust the files in this folder?\n❯ 1. Yes, proceed\n  2. No, exit\n"}
	oldTmux, oldNext, oldPoll, oldWait, oldProc, oldAlive := runTmux, runWpNext, autopilotPoll, autopilotReadyWait, procOf, pidAlive
	runTmux, autopilotPoll, autopilotReadyWait = f.run, 5*time.Millisecond, 100*time.Millisecond
	runWpNext = func(context.Context, string, []string, string, string) (string, error) {
		t.Error("ran wp next for a session that wasn't ready")
		return "", nil
	}
	procOf = func(pid int) (procInfo, error) {
		if pid == 100 {
			return procInfo{PPID: 1, Comm: "bash"}, nil
		}
		return procInfo{PPID: 100, Comm: "claude", Args: []string{"claude", "--rc"}, Dir: dir}, nil
	}
	pidAlive = func(pid int) bool { return pid != os.Getpid() || !f.exited }
	t.Cleanup(func() {
		runTmux, runWpNext, autopilotPoll, autopilotReadyWait, procOf, pidAlive = oldTmux, oldNext, oldPoll, oldWait, oldProc, oldAlive
	})
	o := newTestProc(t, dir).execAutopilot(context.Background(), newRun(time.Minute), autopilotJob{CLIID: "old-123", Dir: dir, Key: "APP-2"})
	r, _ := o.Result.(autopilotResult)
	if r.Outcome != "cleared" || r.Via != "restart" || r.NewCLI != "new-789" || !strings.Contains(r.Detail, "wasn't ready for its prompt") || f.pasted != "" {
		t.Errorf("%+v, pasted %q", r, f.pasted)
	}
}

func TestPlanRelaunchSaysWhyNot(t *testing.T) {
	oldProc := procOf
	t.Cleanup(func() { procOf = oldProc })
	procOf = func(pid int) (procInfo, error) {
		return procInfo{PPID: 100, Args: []string{"node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, Dir: "/x"}, nil
	}
	if rl, why := planRelaunch(context.Background(), "", "%7", 5); rl != nil || why != "it wasn't started as claude" {
		t.Errorf("node: %+v %q", rl, why)
	}
	procOf = func(pid int) (procInfo, error) {
		if pid == 100 {
			return procInfo{Comm: "make"}, nil
		}
		return procInfo{PPID: 100, Args: []string{"claude"}, Dir: "/x"}, nil
	}
	oldTmux := runTmux
	t.Cleanup(func() { runTmux = oldTmux })
	runTmux = func(context.Context, string, []byte, ...string) (string, error) { return "100\n", nil }
	if rl, why := planRelaunch(context.Background(), "", "%7", 5); rl != nil || why != "it runs under make, not a shell" {
		t.Errorf("under make: %+v %q", rl, why)
	}
}

// A session started without Remote Control gets it when autopilot restarts
// it, and one that has it keeps its own flag (WP-1044).
func TestPlanRelaunchTurnsOnRemoteControl(t *testing.T) {
	oldProc, oldTmux := procOf, runTmux
	t.Cleanup(func() { procOf, runTmux = oldProc, oldTmux })
	runTmux = func(context.Context, string, []byte, ...string) (string, error) { return "100\n", nil }
	for in, want := range map[string]string{
		"claude --model opus":            "--model opus --rc --settings " + unattendedSettings,
		"claude --remote-control box":    "--remote-control --settings " + unattendedSettings,
		"claude --rc --settings /x.json": "--rc --settings /x.json",
	} {
		procOf = func(pid int) (procInfo, error) {
			if pid == 100 {
				return procInfo{PPID: 1, Comm: "bash", Dir: "/x"}, nil
			}
			return procInfo{PPID: 100, Comm: "claude", Args: strings.Fields(in), Dir: "/x"}, nil
		}
		rl, why := planRelaunch(context.Background(), "", "%7", 5)
		if rl == nil {
			t.Fatalf("%s: %s", in, why)
		}
		if got := strings.Join(rl.args, " "); got != want {
			t.Errorf("%s: args %q, want %q", in, got, want)
		}
	}
	if got := strings.Join(withRemoteControl([]string{"--remote-control=box"}), " "); got != "--remote-control=box" {
		t.Errorf("named: %s", got)
	}
}

func TestKeepCommandInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commands", "keep.md")
	if ch, err := syncSlashCommand(path, keepCommand, false, false); err != nil || !ch {
		t.Fatalf("install: %v %v", ch, err)
	}
	if b, _ := os.ReadFile(path); string(b) != keepCommand {
		t.Errorf("keep.md = %q", b)
	}
	if ch, _ := syncSlashCommand(path, keepCommand, false, false); ch {
		t.Error("installing again changed it")
	}
	if ch, err := syncSlashCommand(path, keepCommand, true, false); err != nil || !ch || fileExists(path) {
		t.Errorf("uninstall: %v %v, still there %v", ch, err, fileExists(path))
	}
	_ = os.WriteFile(path, []byte("my own keep\n"), 0o600)
	if ch, _ := syncSlashCommand(path, keepCommand, false, false); ch {
		t.Error("overwrote the person's own keep.md")
	}
	if ch, _ := syncSlashCommand(path, keepCommand, true, false); ch || !fileExists(path) {
		t.Error("removed the person's own keep.md")
	}
}
