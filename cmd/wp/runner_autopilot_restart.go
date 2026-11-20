package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Restarting instead of clearing (WP-ADR9): /exit the session and start
// Claude Code again in its pane, with its flags and the prompt as its
// flags. The new process picks up a new Claude Code version, settings,
// plugins and MCP servers, and starts with nothing held in memory. It is
// started without a prompt and given one the way a cleared session is, once
// it is ready for it: a prompt on the command line would be one plain line,
// sit in the shell's history and the process list, and have to dodge
// --remote-control's optional name. It works when Claude Code
// runs straight under the pane's shell (the shell takes the command line) or
// is the pane's own process (tmux respawns the pane). Anything else, such as
// a wrapper script or Claude Code started through node, is left alone and
// the person is told. The new session runs in its own worktree (WP-ADR57,
// runner_autopilot_worktree.go).

// relaunch is how a session's process is started again.
type relaunch struct {
	respawn bool     // Claude Code is the pane's own process: respawn the pane
	shell   string   // else the shell it runs under, which takes the command
	dir     string   // its working directory
	bin     string   // the claude to run
	args    []string // its flags, as it is started again
	env     []string // variables it is started with (env VAR=…, WP-1265)
	// Where the shell is, and where it must go first (a worktree starts from
	// the repo's root).
	shellDir, cdTo string
	// The repo's main checkout ("" when there is none to sweep), whether the
	// new session gets a worktree, and the worktree the old one leaves.
	root     string
	worktree bool
	ended    string
}

// procInfo is what the restart reads about a process.
type procInfo struct {
	PPID int
	Comm string
	Args []string
	Dir  string
}

// Seams for tests: /proc and whether a process still runs.
var (
	procOf = func(pid int) (procInfo, error) {
		base := filepath.Join("/proc", strconv.Itoa(pid))
		var p procInfo
		stat, err := os.ReadFile(filepath.Join(base, "stat")) // #nosec G304 -- a process of this user's, found through Claude Code's registry or tmux
		if err != nil {
			return p, err
		}
		// "pid (comm) state ppid …": comm may hold spaces and parentheses.
		at := bytes.LastIndexByte(stat, ')')
		if at < 0 {
			return p, fmt.Errorf("can't read /proc/%d/stat", pid)
		}
		if f := strings.Fields(string(stat[at+1:])); len(f) > 1 {
			p.PPID, _ = strconv.Atoi(f[1])
		}
		comm, _ := os.ReadFile(filepath.Join(base, "comm")) // #nosec G304 -- as above
		p.Comm = strings.TrimSpace(string(comm))
		cmdline, _ := os.ReadFile(filepath.Join(base, "cmdline")) // #nosec G304 -- as above
		for _, a := range bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0}) {
			p.Args = append(p.Args, string(a))
		}
		p.Dir, _ = os.Readlink(filepath.Join(base, "cwd"))
		return p, nil
	}
	pidAlive = processAlive
)

// paneShells are the shells that take a typed command line.
var paneShells = map[string]bool{"bash": true, "zsh": true, "fish": true, "sh": true, "dash": true, "ksh": true, "mksh": true}

// planRelaunch says how the session process pid in pane can be started
// again, or why it can't (then it is left alone).
func planRelaunch(ctx context.Context, socket, pane string, pid int) (*relaunch, string) {
	me, err := procOf(pid)
	if err != nil {
		return nil, "its process can't be read"
	}
	if len(me.Args) == 0 || filepath.Base(me.Args[0]) != "claude" {
		return nil, "it wasn't started as claude"
	}
	out, err := runTmux(ctx, socket, nil, "display-message", "-p", "-t", pane, "#{pane_pid}")
	if err != nil {
		return nil, "tmux doesn't name the pane's process"
	}
	panePID, _ := strconv.Atoi(strings.TrimSpace(out))
	rl := &relaunch{dir: me.Dir, bin: me.Args[0], args: withUnattendedSettings(withRemoteControl(relaunchArgs(me.Args[1:])))}
	switch panePID {
	case pid:
		rl.respawn = true
		if !filepath.IsAbs(rl.bin) {
			rl.bin = claudeOnPath(rl.bin)
		}
	case me.PPID:
		sh, err := procOf(panePID)
		if err != nil || !paneShells[sh.Comm] {
			return nil, "it runs under " + cmpStr(sh.Comm, "a program") + ", not a shell"
		}
		rl.shell, rl.shellDir = sh.Comm, sh.Dir
	default:
		return nil, "it doesn't run straight in its pane"
	}
	if rl.dir == "" {
		return nil, "its folder can't be read"
	}
	return rl, ""
}

// claudeOnPath is the claude a respawned pane runs: tmux starts it with its
// own environment, whose PATH may not have it.
func claudeOnPath(name string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".local", "bin", name); fileExists(p) {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

// relaunchFlags are the flags a restarted session keeps, with whether each
// takes a value. Everything else is dropped: a prompt, a session to resume or
// a name belong to the old session, and an unknown flag might take a value
// that can't be told from a prompt.
var relaunchFlags = map[string]bool{
	"--rc": false, "--remote-control": false, "--remote-control-session-name-prefix": true,
	"--dangerously-skip-permissions": false, "--allow-dangerously-skip-permissions": false,
	"--model": true, "--permission-mode": true, "--effort": true, "--agent": true, "--fallback-model": true,
	"--add-dir": true, "--settings": true, "--setting-sources": true, "--mcp-config": true, "--strict-mcp-config": false,
	"--chrome": false, "--no-chrome": false, "--ide": false, "--verbose": false,
}

// relaunchArgs keeps args' flags that a restart passes on (relaunchFlags).
// --remote-control's optional name is dropped: the new session gets its own.
func relaunchArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		name, _, inline := strings.Cut(args[i], "=")
		takes, ok := relaunchFlags[name]
		switch {
		case !ok:
		case name == "--rc" || name == "--remote-control":
			out = append(out, name)
		case !takes || inline:
			out = append(out, args[i])
		case i+1 < len(args):
			out = append(out, args[i], args[i+1])
			i++
		}
	}
	return out
}

// commandLine is the line that starts the session again.
func (rl *relaunch) commandLine() string {
	var parts []string
	if len(rl.env) > 0 {
		parts = append(parts, "env")
		for _, kv := range rl.env {
			parts = append(parts, shellWord(kv))
		}
	}
	parts = append(parts, shellWord(rl.bin))
	for _, a := range rl.args {
		parts = append(parts, shellWord(a))
	}
	line := strings.Join(parts, " ")
	if rl.cdTo != "" {
		line = "cd " + shellWord(rl.cdTo) + " && " + line
	}
	return line
}

// shellWord quotes s for sh, bash, zsh and fish alike, unless it needs none.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@%+,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// restartSession exits session s, starts Claude Code again in its pane and
// gives the new session the next task's prompt. The step's last word and the
// screen checks have passed.
func (p *runnerProc) restartSession(ctx context.Context, run *jobRun, in autopilotJob, dir string, s *ccSession, socket, pane string, rl *relaunch) outcome {
	say := func(format string, a ...any) { fmt.Fprintf(run.out, format+"\n", a...) }
	base := autopilotResult{Via: "restart", Warning: p.planWorktree(ctx, rl)}
	if base.Warning != "" {
		say("%s", base.Warning)
	}
	// After /exit, a failure leaves the pane without a session: say so
	// plainly, so the person starts one.
	var unshipped chan []string // set once the scan has started
	ended := func(format string, a ...any) outcome {
		d := fmt.Sprintf(format, a...)
		say("ended, but %s", d)
		r := base
		r.Outcome, r.Detail = "cleared", d
		if unshipped != nil {
			r.Unshipped = <-unshipped
		}
		return outcome{State: "done", Result: r}
	}
	keepPane := false
	if rl.respawn {
		// The pane would close with its process: keep it open to respawn.
		out, _ := runTmux(ctx, socket, nil, "show-options", "-p", "-v", "-t", pane, "remain-on-exit")
		if keepPane = strings.TrimSpace(out) != "on"; keepPane {
			if _, err := runTmux(ctx, socket, nil, "set-option", "-p", "-t", pane, "remain-on-exit", "on"); err != nil {
				return failed("can't keep its pane open across the restart: %v", err)
			}
			defer func() {
				_, _ = runTmux(context.WithoutCancel(ctx), socket, nil, "set-option", "-p", "-u", "-t", pane, "remain-on-exit")
			}()
		}
	}
	say("restarting session %s in tmux pane %s", in.CLIID, pane)
	if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "-l", "/exit"); err != nil {
		return failed("%v", err)
	}
	if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "Enter"); err != nil {
		return failed("%v", err)
	}
	// A worktree with commits asks what to do with it. Keep it: the sweep
	// below is what decides, and it never loses work.
	var pressed time.Time
	gone := func() bool {
		if !pidAlive(s.PID) {
			return true
		}
		if time.Since(pressed) > 3*time.Second {
			if screen, err := runTmux(ctx, socket, nil, "capture-pane", "-p", "-t", pane); err == nil && keepWorktreeDialog(screen) {
				say("keeping the worktree at Claude Code's question")
				pressed = time.Now()
				_, _ = runTmux(ctx, socket, nil, "send-keys", "-t", pane, "Enter")
			}
		}
		return false
	}
	if !waitUntil(ctx, 30*time.Second, gone) {
		return failed("typed /exit, but Claude Code was still running 30 s later")
	}
	worktree := ""
	if rl.worktree {
		name := worktreeName(ctx, rl.root, in.Key)
		if name == "" {
			// Never start in the person's checkout: stop with the pane free.
			return ended("no worktree for this session: every name for %s is taken", in.Key)
		} else {
			worktree = filepath.Join(rl.root, autopilotWorktrees, name)
			freeStaleLocks(ctx, rl.root, in.Key, say)
			rl.args = append([]string{"--worktree", name}, rl.args...)
		}
	}
	if rl.root != "" {
		spare := worktree
		if !rl.worktree {
			spare = rl.dir // the new session starts where the old one was
		}
		base.Kept = p.sweepWorktrees(ctx, run, in, rl, spare)
		// It can take minutes on a large repository, and the pane is empty
		// now: the new session starts while it runs.
		unshipped = make(chan []string, 1)
		go func() { unshipped <- p.noteUnshipped(ctx, run, in, rl) }()
	}
	line := rl.commandLine()
	if rl.respawn {
		dead := func() bool {
			out, _ := runTmux(ctx, socket, nil, "display-message", "-p", "-t", pane, "#{pane_dead}")
			return strings.TrimSpace(out) == "1"
		}
		if !waitUntil(ctx, 10*time.Second, dead) {
			return ended("its pane didn't come free to start Claude Code again")
		}
		if _, err := runTmux(ctx, socket, nil, "respawn-pane", "-t", pane, "-c", rl.dir, line); err != nil {
			return ended("Claude Code couldn't be started again in its pane: %v", err)
		}
	} else {
		atShell := func() bool {
			out, _ := runTmux(ctx, socket, nil, "display-message", "-p", "-t", pane, "#{pane_current_command}")
			return strings.TrimSpace(out) == rl.shell
		}
		if !waitUntil(ctx, 10*time.Second, atShell) {
			return ended("its pane didn't come back to %s to start Claude Code again", rl.shell)
		}
		// A leading space keeps the line out of the shell's history.
		if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "-l", " "+line); err != nil {
			return ended("Claude Code couldn't be started again: %v", err)
		}
		if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "Enter"); err != nil {
			return ended("its command line was typed and not sent (press Enter there): %v", err)
		}
	}
	ccDir := claudeConfigDir()
	var next *ccSession
	if !waitUntil(ctx, time.Minute, func() bool { next = findPaneSession(ccDir, pane, s.PID); return next != nil }) {
		return ended("Claude Code was started again and no session showed up in its pane within a minute")
	}
	say("new session %s", next.SessionID)
	if rl.worktree {
		dir = rl.root
	}
	if worktree != "" {
		if waitUntil(ctx, 10*time.Second, func() bool { return dirExists(worktree) }) {
			dir = worktree
			if w := runWorktreeSetup(ctx, run, worktree, next.SessionID, in.Agent); w != "" {
				base.Warning = strings.TrimSpace(base.Warning + " " + w)
			}
		} else {
			return ended("the new session's worktree didn't appear at %s, so it isn't prompted from the checkout", worktree)
		}
	}
	done := p.promptSession(ctx, run, in, dir, socket, pane, next.PID, next.SessionID, base)
	if r, ok := done.Result.(autopilotResult); ok && unshipped != nil {
		r.Unshipped = <-unshipped
		done.Result = r
	}
	return done
}

// findPaneSession is the running session in pane other than process not's.
func findPaneSession(dir, pane string, not int) *ccSession {
	files, _ := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- Claude Code's own session registry
		if err != nil {
			continue
		}
		var s ccSession
		if json.Unmarshal(b, &s) != nil || s.SessionID == "" || s.PID == not || !strings.HasSuffix(s.Tmux, "."+pane) || !pidAlive(s.PID) {
			continue
		}
		return &s
	}
	return nil
}

// waitUntil polls ok until it holds or limit passes.
func waitUntil(ctx context.Context, limit time.Duration, ok func() bool) bool {
	end := time.Now().Add(limit)
	for {
		if ok() {
			return true
		}
		if !time.Now().Before(end) || !sleepCtx(ctx, autopilotPoll) {
			return false
		}
	}
}
