package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// The runner's reading of an objective's round (WP-927, WP-ADR111, WP-ADR142): a job of
// kind "objective", Waypoint's own, queued onto the runner of the person who
// put the objective in effect. It reads in fresh checkouts the runner makes
// itself, so the agent that worked the round has no hand in the reading.
//
// What isolation it has: the checkouts are scratch worktrees of the runner's
// own clone, never a person's working tree; the commands run in them with the
// environment a runner gives any job (jobEnv: an allowlist, so neither the
// runner's token nor Waypoint's variables nor forge, cloud or ssh-agent
// credentials are in it), each in its own process group that is killed on
// timeout, cancel and when it exits, with its output capped; the checkouts
// are removed afterwards. What it lacks: the commands run as the runner's
// own user, so HOME (and the files in it: saved Waypoint config, ssh keys,
// caches) and the network are as open to them as to any command job: there
// is no kernel sandbox, container or separate user for them, only the
// helper's machine users where a person has set that up for jobs that ask.

// objectiveFeature is advertised in runnerFeatureList when this machine can
// read an objective: it needs git and a shell.
func objectiveFeature() bool {
	if _, err := exec.LookPath("git"); err != nil {
		return false
	}
	if _, err := exec.LookPath(shellProgram); err != nil {
		return false
	}
	return true
}

// shellProgram is what runs a command line (shellCommand).
var shellProgram = map[bool]string{true: "cmd", false: "sh"}[runtime.GOOS == "windows"]

var refNameBad = lazyre.New(`[\x00-\x20\x7f~^:?*\[\\]|\.\.|@\{`)

// objectiveRef refuses a branch or commit name git could take for an
// option or that isn't a name at all.
func objectiveRef(what, v string) error {
	if v == "" {
		return nil
	}
	if strings.HasPrefix(v, "-") || len(v) > 250 || refNameBad.MatchString(v) {
		return fmt.Errorf("the %s %q isn't a branch or commit name", what, excerpt(v, 60))
	}
	return nil
}

// objectiveDir is a folder within a checkout: relative, going nowhere else.
func objectiveDir(checkout, dir string) (string, error) {
	dir = strings.Trim(filepath.ToSlash(strings.TrimSpace(dir)), "/")
	if dir == "" || dir == "." {
		return checkout, nil
	}
	if filepath.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") || strings.Contains(dir, "/../") || strings.HasSuffix(dir, "/..") {
		return "", fmt.Errorf("the folder %q isn't within the repository", dir)
	}
	full := filepath.Join(checkout, filepath.FromSlash(dir))
	st, err := os.Stat(full)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("the folder %s isn't in the repository at that commit", dir)
	}
	// A link in the repository can lead anywhere on this machine.
	real, err := filepath.EvalSymlinks(full)
	root, err2 := filepath.EvalSymlinks(checkout)
	if err != nil || err2 != nil || (real != root && !strings.HasPrefix(real, root+string(filepath.Separator))) {
		return "", fmt.Errorf("the folder %s leaves the repository", dir)
	}
	return full, nil
}

// objectiveMinTimed is the fewest timed runs a time is read from: a median
// and a p-value of fewer say nothing.
const objectiveMinTimed = 3

// execObjective reads one objective round and answers with a
// core.ObjectiveJobResult. A check that fails is an answer; the job fails
// only when nothing could be read, with a message for a person to act on.
func (p *runnerProc) execObjective(ctx context.Context, run *jobRun, in core.ObjectiveJob) (o outcome) {
	say := func(format string, a ...any) { fmt.Fprintf(run.out, format+"\n", a...) }
	if err := p.cloneAllowed(in.Repo); err != nil {
		return failed("the repository %q can't be cloned: %v", excerpt(in.Repo, 80), err)
	}
	if err := errors.Join(objectiveRef("branch", in.Ref), objectiveRef("base", in.Base)); err != nil {
		return failed("%v", err)
	}
	if !objectiveFeature() {
		return failed("this machine can't read an objective: it needs git and a shell on its PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, run.timeout)
	defer cancel()
	budget := time.Duration(in.Budget) * time.Second
	if budget <= 0 || budget > run.timeout {
		budget = run.timeout / 2
	}
	// One command may use the whole budget; a read of a time plans its runs
	// within it, and the job's own timeout is the backstop.
	spec := measureSpec{Check: in.Check, Setup: in.Setup, Measure: in.Measure, Unit: in.Unit, Timeout: budget, Budget: budget, MinRuns: objectiveMinTimed,
		Env: append(jobEnv(os.Environ(), nil, false), "GIT_TERMINAL_PROMPT=0")}

	bare, err := p.ensureClone(ctx, run, in.Repo)
	if err != nil {
		return p.objectiveError(ctx, run, "can't get the repository: %v", err)
	}
	genv := p.gitProtocolEnv()
	head, err := p.resolveBase(ctx, bare, in.Ref, genv)
	if err != nil {
		if in.Ref != "" {
			return p.objectiveError(ctx, run, "the branch %s isn't in the repository any more (%v): was it deleted or renamed? Set the objective's branch again", in.Ref, err)
		}
		return p.objectiveError(ctx, run, "%v", err)
	}
	base := ""
	if in.Base != "" {
		if base, err = p.resolveBase(ctx, bare, in.Base, genv); err != nil {
			return p.objectiveError(ctx, run, "the commit the round started from (%s) isn't in the repository any more (%v): wp objective next skips this round's reading", excerpt(in.Base, 12), err)
		}
	}
	both := base != "" && base != head && in.Check != "" && (in.Measure == core.MeasureTime || in.Measure == core.MeasureValue)

	if err := os.MkdirAll(filepath.Join(p.cacheDir(), "work"), 0o750); err != nil {
		return failed("%v", err)
	}
	root, err := os.MkdirTemp(filepath.Join(p.cacheDir(), "work"), fmt.Sprintf("job%d-obj-", run.id))
	if err != nil {
		return failed("%v", err)
	}
	defer func() {
		// Even after a cancel or a timeout: the checkouts go.
		bg, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer stop()
		for _, d := range []string{"head", "base"} {
			_, _ = p.git(bg, bare, "worktree", "remove", "--force", filepath.Join(root, d))
		}
		_ = os.RemoveAll(root)
		_, _ = p.git(bg, bare, "worktree", "prune")
	}()
	checkout := func(name, sha string) (string, error) {
		dir := filepath.Join(root, name)
		if _, err := p.git(ctx, bare, "worktree", "add", "--detach", "--quiet", dir, sha); err != nil {
			return "", err
		}
		return dir, nil
	}
	headRoot, err := checkout("head", head)
	if err != nil {
		return p.objectiveError(ctx, run, "can't check out %s: %v", head[:12], err)
	}
	say("Checked out %s at %s", orStr(in.Ref, "the default branch"), head[:12])
	res := &core.ObjectiveJobResult{Head: head}

	headDir, err := objectiveDir(headRoot, in.Dir)
	// Every command runs in the folder: keep-passing ones too, which run in
	// it without a check.
	if err != nil && (in.Check != "" || in.Setup != "" || len(in.KeepPassing) > 0) {
		return p.objectiveError(ctx, run, "%v (at %s)", err, head[:12])
	}
	if err != nil {
		headDir = headRoot
	}
	sides := []measureSide{{"the branch (" + head[:12] + ")", headDir}}
	var baseRoot string
	var baseGone string // why the base can't be read, when it can't
	if both {
		if baseRoot, err = checkout("base", base); err != nil {
			return p.objectiveError(ctx, run, "can't check out %s: %v", base[:12], err)
		}
		say("Checked out the commit the round started from, %s", base[:12])
		if d, err := objectiveDir(baseRoot, in.Dir); err != nil {
			baseGone = err.Error()
		} else {
			sides = append(sides, measureSide{"the round's start (" + base[:12] + ")", d})
		}
	}

	if in.Check != "" {
		reads, pv, exact, err := measureCheck(ctx, spec, sides, func(f string, a ...any) { say(f, a...) })
		if err != nil {
			return p.objectiveStopped(ctx, run, err)
		}
		res.Reading = readingOf(reads[0])
		if both {
			res.Base = base
			if baseGone != "" {
				res.BaseReading = &core.ObjectiveReading{Runs: 1, Exit: -1, Output: baseGone}
			} else {
				res.BaseReading = readingOf(reads[1])
				if reads[0].OK && reads[1].OK {
					res.P, res.Exact = pv, exact
				}
			}
		} else if reads[0].OK {
			res.Exact = exact
		}
		say("Read: %s", describeReading(res.Reading))
	}
	// With no check, nothing above ran the setup, and the keep-passing
	// commands need what it builds as much as a check does.
	if in.Check == "" && in.Setup != "" && len(in.KeepPassing) > 0 {
		say("Setting up: %s", excerpt(in.Setup, 100))
		r, err := runCheck(ctx, headDir, spec.Env, in.Setup, budget)
		switch {
		case err != nil:
			return p.objectiveStopped(ctx, run, stopped(ctx, "setup in "+sides[0].Label, err))
		case r.TimedOut:
			return p.objectiveStopped(ctx, run, stopf("the setup (%s) ran longer than %s and was stopped", excerpt(in.Setup, 80), budget.Round(time.Second)))
		case r.Code != 0:
			return p.objectiveStopped(ctx, run, stopf("the setup (%s) failed with exit %d: %s", excerpt(in.Setup, 80), r.Code, excerpt(strings.TrimSpace(trimOutput(r.All, 1<<30)), 400)))
		}
	}
	// The keep-passing commands: each at the head, whatever the check read.
	for _, h := range in.KeepPassing {
		say("Keeping passing: %s", excerpt(h, 100))
		r, err := runCheck(ctx, headDir, spec.Env, h, budget)
		switch {
		case err != nil:
			return p.objectiveStopped(ctx, run, stopped(ctx, "the command that must keep passing, "+excerpt(h, 80), err))
		case r.TimedOut:
			return p.objectiveStopped(ctx, run, stopf("the command that must keep passing, %s, ran longer than %s and was stopped", excerpt(h, 80), budget.Round(time.Second)))
		}
		res.Kept = append(res.Kept, core.ObjectiveKept{Command: h, OK: r.Code == 0, Output: trimOutput(r.All, 2000)})
	}
	if base != "" && base != head {
		// A list that can't be read fails the reading: an empty one would
		// say the round left its own check alone.
		touched, err := p.objectiveTouched(ctx, bare, base, head, in)
		if err != nil {
			return p.objectiveError(ctx, run, "can't list what changed between %s and %s: %v", base[:12], head[:12], err)
		}
		res.Touched = touched
	}
	if res.Reading == nil && len(res.Kept) == 0 {
		say("Nothing to read but the commit.")
	}
	return outcome{State: "done", Result: res}
}

// objectiveError fails the job, or says it was stopped.
func (p *runnerProc) objectiveError(ctx context.Context, run *jobRun, format string, a ...any) outcome {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failed("the reading ran out of its %s before it was done (%s)", run.timeout.Round(time.Second), fmt.Sprintf(format, a...))
	case ctx.Err() != nil:
		return outcome{State: "cancelled", Error: "Stopped."}
	}
	return failed(format, a...)
}

// objectiveStopped is how a read that couldn't be taken ends.
func (p *runnerProc) objectiveStopped(ctx context.Context, run *jobRun, err error) outcome {
	var stop *measureStop
	if errors.As(err, &stop) {
		return failed("%s", stop.msg)
	}
	return p.objectiveError(ctx, run, "%v", err)
}

func readingOf(r *sideRead) *core.ObjectiveReading {
	out := &core.ObjectiveReading{OK: r.OK, Value: r.Value, Low: r.Low, High: r.High, Runs: max(r.Runs, 1), Exit: r.Exit, Output: trimOutput(r.Output, 2000)}
	if !r.OK {
		out.Value, out.Low, out.High = nil, nil, nil
	}
	return out
}

func describeReading(r *core.ObjectiveReading) string {
	switch {
	case r == nil:
		return "nothing"
	case !r.OK:
		return fmt.Sprintf("failed (exit %d): %s", r.Exit, excerpt(lastText(r.Output), 120))
	case r.Value != nil:
		return fmt.Sprintf("%g over %d run(s)", *r.Value, r.Runs)
	}
	return "passed"
}

// objectiveTouched are the files changed between base and head that the
// job's own commands name: a round that edits the check it is read by can't
// be taken at its word.
func (p *runnerProc) objectiveTouched(ctx context.Context, bare, base, head string, in core.ObjectiveJob) ([]string, error) {
	files, err := p.gitPaths(ctx, bare, "diff", "--name-only", base, head)
	if err != nil {
		return nil, err
	}
	cmds := strings.Join(append([]string{in.Check, in.Setup}, in.KeepPassing...), "\n")
	dir := strings.Trim(filepath.ToSlash(in.Dir), "/")
	var out []string
	for _, f := range files {
		if commandNames(cmds, f) || dir != "" && strings.HasPrefix(f, dir+"/") && commandNames(cmds, strings.TrimPrefix(f, dir+"/")) {
			out = append(out, f)
		}
	}
	return out, nil
}

// commandNames says whether the command text names the file path: as a
// word of its own, optionally with a leading ./ or quotes.
func commandNames(cmds, path string) bool {
	if path == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(cmds[from:], path)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(path)
		before, after := byte(' '), byte(' ')
		if i > 0 {
			before = cmds[i-1]
		}
		if end < len(cmds) {
			after = cmds[end]
		}
		if i >= 2 && cmds[i-2:i] == "./" {
			before = ' '
			if i >= 3 {
				before = cmds[i-3]
			}
		}
		if isPathEdge(before, true) && isPathEdge(after, false) {
			return true
		}
		from = i + 1
	}
}

// isPathEdge: b can sit next to a file name in a command without being part
// of a longer name.
func isPathEdge(b byte, before bool) bool {
	switch b {
	case ' ', '\t', '\n', '"', '\'', ';', '|', '&', '(', ')', '<', '>', '=', ':', '`', '$':
		return true
	}
	return false
}
