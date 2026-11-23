package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A session of autopilot's own (WP-799, WP-ADR72): no one's session is
// involved. The runner starts Claude Code in a new tmux session, in a worktree
// of the task's, and gives it its task as a restart does. The session then
// goes on like any other: at its Stop autopilot can move it on.

// autopilotStartWait is how long a new session has to show up.
var autopilotStartWait = time.Minute

// launchTmux runs tmux to start a session. Under systemd the first tmux of a
// machine would be a child of the runner's service and die with it, so it
// starts in a scope of its own. tmux runs with a job's environment, not the
// runner's: the first tmux of a machine keeps what it is started with.
var launchTmux = func(ctx context.Context, args ...string) (string, error) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return "", errNoTmux
	}
	env := withRunnerVars(autopilotStartEnv(), "TMUX_TMPDIR")
	if os.Getenv("INVOCATION_ID") != "" {
		if sr, err := exec.LookPath("systemd-run"); err == nil {
			argv := append([]string{"--user", "--scope", "--collect", "--quiet", "--", bin}, args...)
			cmd := exec.CommandContext(ctx, sr, argv...)                                 // #nosec G204 -- tmux with a fixed subcommand; the task key is checked against keyRE
			cmd.Env = withRunnerVars(env, "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS") // what systemd-run --user needs to reach the bus
			if out, err := cmd.CombinedOutput(); err == nil {
				return string(out), nil
			}
			// The scope may have started tmux before failing: end that session
			// so the direct start below doesn't leave a second one.
			for i, a := range args {
				if a == "-s" && i+1 < len(args) {
					kill := exec.CommandContext(ctx, bin, "kill-session", "-t", args[i+1]) // #nosec G204 -- the session name this start chose
					kill.Env = env
					_ = kill.Run()
					break
				}
			}
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- tmux with a fixed subcommand; the task key is checked against keyRE
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// withRunnerVars is env and the runner's value of each of keys that is set.
func withRunnerVars(env []string, keys ...string) []string {
	out := append([]string{}, env...)
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// autopilotStartEnv is the new session's whole environment, beside what tmux
// gives a pane: a job's (WP-418), so the runner's token, keys and SSH agent
// stay behind and it is unattended (WP-270). Waypoint's config directory
// comes along: it is where the session's hooks find their sign-in. TERM is
// tmux's, not the runner's.
func autopilotStartEnv() []string {
	extra := map[string]string{}
	if v := os.Getenv("WAYPOINT_CONFIG_DIR"); v != "" {
		extra["WAYPOINT_CONFIG_DIR"] = v
	}
	var out []string
	for _, kv := range jobEnv(os.Environ(), extra, false) {
		if k, _, _ := strings.Cut(kv, "="); k != "TERM" && k != "COLORTERM" {
			out = append(out, kv)
		}
	}
	return out
}

// autopilotStartShell replaces the environment of a pane with the arguments
// after it: a tmux server that is already running hands every new session its
// own, so setting variables couldn't remove any. tmux's own stay: they say
// which pane this is.
const autopilotStartShell = `exec env -i TMUX="$TMUX" TMUX_PANE="$TMUX_PANE" TERM="$TERM" "$@"`

// autopilotStartingPrefix names a tmux session while a runner is still
// starting it. Such a session outlives a runner that dies, and Waypoint then
// counts the start as failed and tries another, so a runner that starts up
// ends those it left behind (reapInterruptedStarts). Once the session has its
// task it is renamed. The runner's id keeps a second runner's starts out of it.
func autopilotStartingPrefix(runner int64) string {
	return fmt.Sprintf("wp-start-%d-", runner)
}

// reapInterruptedStarts ends the sessions an earlier run of this runner was
// still starting when it stopped.
func (p *runnerProc) reapInterruptedStarts(ctx context.Context) {
	out, err := runTmux(ctx, "", nil, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return // no tmux, or no server: nothing to end
	}
	for _, name := range strings.Fields(out) {
		if strings.HasPrefix(name, autopilotStartingPrefix(p.id)) {
			p.logf("Ending %s: this runner stopped while it was starting it", name)
			_, _ = runTmux(ctx, "", nil, "kill-session", "-t", name)
		}
	}
}

// runnerFeatureList is what this wp can do. "autopilot_start" needs tmux on
// the machine, so autopilot picks a runner that can start a session.
func runnerFeatureList() []string {
	out := []string{core.FeatureWorkClock, "confine", "autopilot", "audit", "audit-scope", "autopilot_review", "request_lane", "message_lane", "gateway_agent", "gateway_lane", "task_kickoff"}
	if _, err := exec.LookPath("tmux"); err == nil {
		out = append(out, "autopilot_start")
	}
	if objectiveFeature() {
		out = append(out, core.FeatureObjective) // reads an objective's round (WP-927)
	}
	if runtime.GOOS == "linux" {
		out = append(out, "sandbox_drill") // the escape drill (WP-473)
	}
	return out
}

func (p *runnerProc) execAutopilotStart(ctx context.Context, run *jobRun, in autopilotJob) outcome {
	say := func(format string, a ...any) { fmt.Fprintf(run.out, format+"\n", a...) }
	held := func(reason, format string, a ...any) outcome {
		d := fmt.Sprintf(format, a...)
		say("held: %s", d)
		return outcome{State: "done", Result: autopilotResult{Outcome: "held", Reason: reason, Detail: d}}
	}
	dir, err := resolveDir(in.Dir, p.allowed)
	if err != nil {
		return failed("%v", err)
	}
	if !keyRE.MatchString(in.Key) {
		return failed("bad task key %q", in.Key)
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return held("no_tmux", "%v", errNoTmux)
	}
	// Never in the person's checkout: a worktree, or not at all.
	rl := &relaunch{dir: dir, respawn: true}
	if w := p.planWorktree(ctx, rl); w != "" {
		return held("no_worktree", "%s", w)
	} else if !rl.worktree {
		return held("no_worktree", "no worktree can be made in %s", dir)
	}
	rl.ended = "" // the checkout isn't a session that ended
	name := worktreeName(ctx, rl.root, in.Key)
	if name == "" {
		return held("no_worktree", "no worktree for this session: every name for %s is taken", in.Key)
	}
	worktree := filepath.Join(rl.root, autopilotWorktrees, name)
	freeStaleLocks(ctx, rl.root, in.Key, say)
	base := autopilotResult{Via: "start", Kept: p.sweepWorktrees(ctx, run, in, rl, worktree)}
	if !p.stepWanted(ctx, in.Step, run.id) {
		say("the step no longer wants this job: nothing started")
		return outcome{State: "done", Result: autopilotResult{Outcome: "unwanted"}}
	}
	tmuxName := fmt.Sprintf("%s%s-%d", autopilotStartingPrefix(p.id), name, run.id)
	args := []string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", tmuxName, "-c", rl.root, "-x", "200", "-y", "50"}
	args = append(args, "--", "/bin/sh", "-c", autopilotStartShell, "sh")
	set := run.launch.forDir(rl.root) // the person's session settings (WP-1265)
	args = append(args, autopilotStartEnv()...)
	args = append(args, set.env()...)
	args = append(args, claudeOnPath("claude"))
	args = append(args, set.applyTo([]string{"--worktree", name})...)
	args = append(args, "--settings", unattendedSettings)
	out, err := launchTmux(ctx, args...)
	if err != nil {
		return failed("couldn't start tmux: %v", err)
	}
	pane := strings.TrimSpace(out)
	if !strings.HasPrefix(pane, "%") {
		_, _ = runTmux(context.WithoutCancel(ctx), "", nil, "kill-session", "-t", tmuxName)
		return failed("tmux didn't name the new pane (%q)", pane)
	}
	say("started Claude Code in tmux session %s, pane %s", tmuxName, pane)
	// A start that doesn't get as far as its prompt leaves nothing behind.
	giveUp := func(res autopilotResult) outcome {
		bg := context.WithoutCancel(ctx)
		if screen, err := runTmux(bg, "", nil, "capture-pane", "-p", "-t", pane); err == nil {
			say("its screen:\n%s", strings.TrimRight(screen, "\n "))
		}
		_, _ = runTmux(bg, "", nil, "kill-session", "-t", tmuxName)
		return outcome{State: "done", Result: res}
	}
	ended := func(format string, a ...any) outcome {
		d := fmt.Sprintf(format, a...)
		say("gave up: %s", d)
		r := base
		r.Outcome, r.Detail = "cleared", d
		return giveUp(r)
	}
	ccDir := claudeConfigDir()
	var next *ccSession
	if !waitUntil(ctx, autopilotStartWait, func() bool { next = findPaneSession(ccDir, pane, 0); return next != nil }) {
		return ended("Claude Code was started in tmux and no session showed up within a minute")
	}
	say("new session %s", next.SessionID)
	if !waitUntil(ctx, 10*time.Second, func() bool { return dirExists(worktree) }) {
		return ended("the new session's worktree didn't appear at %s", worktree)
	}
	if w := runWorktreeSetup(ctx, run, worktree, next.SessionID, in.Agent); w != "" {
		base.Warning = strings.TrimSpace(base.Warning + " " + w)
	}
	if !p.stepWanted(ctx, in.Step, run.id) {
		say("the step no longer wants this job: closing the new session")
		return giveUp(autopilotResult{Outcome: "unwanted"})
	}
	o := p.promptSession(ctx, run, in, worktree, "", pane, next.PID, next.SessionID, base)
	r, ok := o.Result.(autopilotResult)
	if ok && (r.Outcome == "cleared" || r.Outcome == "unwanted") {
		return giveUp(r) // no one would ever give it the task
	}
	if _, err := runTmux(context.WithoutCancel(ctx), "", nil, "rename-session", "-t", tmuxName, fmt.Sprintf("wp-%s-%d", name, run.id)); err != nil && ok {
		r.Warning = strings.TrimSpace(r.Warning + fmt.Sprintf(" couldn't rename tmux session %s, so the runner's next restart ends it: %v", tmuxName, err))
		o.Result = r
	}
	return o
}
