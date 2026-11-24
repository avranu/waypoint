package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A repository can carry a check its agents' commits must pass before they
// are pushed, a sibling of .waypoint/worktree-setup. The agent gets one
// chance to fix what it reports. The branch is pushed either way: a broken or
// flaky script must not lose the agent's work, and CI is still the gate.

// prePushFile is the repository's check, run with sh in the checkout's root.
var prePushFile = filepath.Join(".waypoint", "pre-push")

// prePushLimit is how long one run of the check has; a var for tests.
var prePushLimit = 15 * time.Minute

// prePushCheck is what deliver needs to check the commits before pushing:
// the environment the agent CLI ran with, and a way to run it once more with
// the check's output.
type prePushCheck struct {
	env []string
	fix func(ctx context.Context, output string) (tokenUse, error)
}

// prePushFixPrompt is what the agent is told on its second run.
func prePushFixPrompt(output string) string {
	return "The repository's pre-push check (" + filepath.ToSlash(prePushFile) + ") failed on the commits made on this branch. " +
		"Fix what it reports and commit the fix; change nothing else. Run `sh " + filepath.ToSlash(prePushFile) + "` to confirm before you end.\n\n" +
		"The end of its output:\n<untrusted>\n" + output + "\n</untrusted>\n"
}

// prePushRun is how one run of the script ended.
type prePushRun struct {
	ok     bool
	ran    bool // false: sh couldn't be started (Windows has none), which is no verdict on the commits
	output string
	why    string // set when it was stopped rather than failed, such as a timeout
}

// runPrePush runs the script in work, streams its output to the job log and
// keeps the end of it.
func runPrePush(ctx context.Context, run *jobRun, work string, env []string) prePushRun {
	fmt.Fprintf(run.out, "\nRunning the repository's pre-push check (%s)\n", filepath.ToSlash(prePushFile))
	cctx, cancel := context.WithTimeout(ctx, prePushLimit)
	defer cancel()
	cmd := procCommand(cctx, "sh", filepath.ToSlash(prePushFile))
	cmd.Dir = work
	cmd.Env = env
	var tail tailBuffer
	w := &teeWriter{a: run.out, b: &tail}
	cmd.Stdout, cmd.Stderr = w, w
	err := runProc(cmd)
	r := prePushRun{ran: true, output: tail.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
		r.ok = true
		fmt.Fprintf(run.out, "The pre-push check passed\n")
		return r
	case ctx.Err() != nil:
		r.why = "stopped when the job's time ran out"
	case cctx.Err() != nil:
		r.why = "timed out after " + prePushLimit.String()
	case !errors.As(err, &exit):
		// sh itself wouldn't start: nothing was checked.
		r.ran = false
		fmt.Fprintf(run.out, "The pre-push check couldn't be run, so the commits go unchecked: %v\n", err)
		return r
	}
	fmt.Fprintf(run.out, "\nThe pre-push check failed: %s\n", orStr(r.why, err.Error()))
	return r
}

// commitLeft commits what the agent left uncommitted, if anything.
func (p *runnerProc) commitLeft(ctx context.Context, run *jobRun, in agentJob, work string) error {
	if status, _ := p.git(ctx, work, "status", "--porcelain"); status == "" {
		return nil
	}
	msg := strings.TrimSpace(in.CommitMessage)
	if msg == "" {
		msg = "Agent changes"
	}
	fmt.Fprintf(run.out, "\nCommitting the changes the agent left\n")
	if _, err := p.git(ctx, work, "add", "-A"); err != nil {
		return fmt.Errorf("git add failed: %v", err)
	}
	if _, err := p.commit(ctx, work, msg, true); err != nil {
		return fmt.Errorf("committing the agent's changes failed: %v", err)
	}
	return nil
}

// pushable is ctx while the job's time lasts, and a fresh bounded one for the
// push once it has run out, as salvage has.
func pushable(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), salvageTimeout)
}

// prePush runs the repository's check on the commits about to be pushed, if
// it has one, and lets the agent fix what it reports once. It records how the
// check ended in result and never stops the push. The context it returns is
// one the push can still use when the job's own ran out meanwhile; the cancel
// func is to be deferred.
func (p *runnerProc) prePush(ctx context.Context, run *jobRun, in agentJob, work, baseSHA string, chk *prePushCheck, result map[string]any) (context.Context, context.CancelFunc) {
	if chk == nil || !in.Push || in.Branch == "" {
		return ctx, func() {}
	}
	if fi, err := os.Lstat(filepath.Join(work, prePushFile)); err != nil || !fi.Mode().IsRegular() {
		return ctx, func() {}
	}
	if head, err := p.git(ctx, work, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(head) == baseSHA {
		return ctx, func() {} // nothing to push
	}
	last := runPrePush(ctx, run, work, chk.env)
	switch {
	case !last.ran:
		return pushable(ctx)
	case last.ok:
		result["pre_push"] = "passed"
		return pushable(ctx)
	}
	if ctx.Err() == nil && chk.fix != nil {
		fmt.Fprintf(run.out, "\nAsking the agent to fix what the check reported\n")
		tokens, err := chk.fix(ctx, last.output)
		if err != nil {
			fmt.Fprintf(run.out, "\nThe agent's fix run failed: %v\n", err)
		}
		if t, ok := result["tokens"].(tokenUse); ok {
			t.add(tokens)
			result["tokens"] = t
		}
		cctx, cancel := pushable(ctx)
		if err := p.commitLeft(cctx, run, in, work); err != nil {
			fmt.Fprintf(run.out, "\n%v\n", err)
		}
		cancel()
		if ctx.Err() == nil {
			if again := runPrePush(ctx, run, work, chk.env); again.ran {
				if again.ok {
					result["pre_push"] = "passed after a fix"
					return pushable(ctx)
				}
				last = again
			}
		}
	}
	result["pre_push"] = "failed: " + orStr(last.why, excerpt(last.output, 300))
	return pushable(ctx)
}

// prePushFix is how the agent CLI is run a second time on the check's output:
// the same checkout and environment as its first run, a new prompt, and no
// answer schema, which the first run's prompt asked for and this one doesn't.
func (p *runnerProc) prePushFix(run *jobRun, in agentJob, path, work string, env []string) func(context.Context, string) (tokenUse, error) {
	return func(ctx context.Context, output string) (tokenUse, error) {
		in2 := in
		in2.JSONSchema = ""
		in2.Prompt = prePushFixPrompt(output)
		drv, err := p.buildDriver(ctx, in2, path, work)
		if err != nil {
			return tokenUse{}, err
		}
		cmd := procCommand(ctx, path, drv.args...)
		cmd.Dir = work
		cmd.Env = env
		if drv.stdin != "" {
			cmd.Stdin = strings.NewReader(drv.stdin)
		}
		var stdout bytes.Buffer
		if drv.isStream {
			lw := &lineWriter{fn: func(l []byte) { drv.line(l, run.out) }}
			cmd.Stdout = lw
		} else {
			cmd.Stdout = &stdout
		}
		var stderrTail tailBuffer
		cmd.Stderr = &teeWriter{a: run.out, b: &stderrTail}
		runErr := runProc(cmd)
		if lw, ok := cmd.Stdout.(*lineWriter); ok {
			lw.flush() // the driver reads its answer from the lines
		}
		_, _, tokens, ferr := drv.finish(stdout.Bytes())
		if ferr != nil {
			return tokens, ferr
		}
		if runErr != nil {
			return tokens, fmt.Errorf("exited with an error: %v. %s", runErr, excerpt(stderrTail.String(), 300))
		}
		return tokens, nil
	}
}
