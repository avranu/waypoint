package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaudeOutOfTime commits one piece of work, starts another and never
// ends: the runner's time limit stops it.
const fakeClaudeOutOfTime = `#!/bin/sh
cat > /dev/null
echo done > finished.txt
git add finished.txt
git -c user.name=agent -c user.email=agent@example.invalid commit -q -m "Merge the base"
echo half > half-done.txt
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"wp testsum -- make check"}}]}}'
exec sleep 60
`

func TestTimedOutAgentKeepsItsCommits(t *testing.T) {
	fakeCLI(t, "claude", fakeClaudeOutOfTime)
	remote := newGitRepo(t)
	gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
	p := newTestProc(t)
	job := agentJob{CLI: "claude", Prompt: "x", Repo: remote, Branch: "agent/round", Push: true, Permission: "edit_run", CommitMessage: "Round"}

	// Asked to keep them (a babysitting round, WP-1597): the commit it had
	// made is pushed, what it left uncommitted is not, and the job still fails.
	job.KeepOnTimeout = true
	run := newRun(10 * time.Second)
	o := p.execAgent(context.Background(), run, "agent", job)
	if o.State != "failed" || !strings.Contains(o.Error, "timed out") {
		t.Fatalf("outcome = %+v\n%s", o, run.out.text())
	}
	res, _ := o.Result.(map[string]any)
	if res["pushed"] != true || res["timed_out"] != true || len(res["commits"].([]string)) != 1 {
		t.Fatalf("result = %v\n%s", res, run.out.text())
	}
	if left, _ := res["uncommitted"].([]string); len(left) != 1 || left[0] != "half-done.txt" {
		t.Errorf("uncommitted = %v", res["uncommitted"])
	}
	if got := gitOut(t, remote, "log", "-1", "--format=%s", "agent/round"); got != "Merge the base" {
		t.Errorf("pushed commit = %q", got)
	}
	if got := gitOut(t, remote, "ls-tree", "-r", "--name-only", "agent/round"); !strings.Contains(got, "finished.txt") || strings.Contains(got, "half-done.txt") {
		t.Errorf("pushed files = %q", got)
	}
	if left, _ := os.ReadDir(filepath.Join(p.cacheDir(), "work")); len(left) != 0 {
		t.Errorf("work trees left: %v", left)
	}

	// Not asked: a timeout pushes nothing, as before.
	job.KeepOnTimeout, job.Branch = false, "agent/other"
	if o := p.execAgent(context.Background(), newRun(10*time.Second), "agent", job); o.State != "failed" || o.Result != nil {
		t.Errorf("outcome = %+v", o)
	}
	if got := gitOut(t, remote, "branch", "--list", "agent/other"); got != "" {
		t.Errorf("branch pushed: %q", got)
	}
}

func TestCallClockSaysWhereTheTimeWent(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	c := &callClock{now: func() time.Time { return now }}
	if got := c.spent(); got != "" {
		t.Errorf("nothing ran: %q", got)
	}
	c.begin("a", "Bash uv sync")
	now = now.Add(12 * time.Minute)
	c.end("a")
	c.begin("b", "Read main.go")
	now = now.Add(time.Second)
	c.end("b") // quick: not worth naming
	c.end("unknown")
	c.begin("c", "Bash wp testsum -- make check")
	now = now.Add(37 * time.Minute)
	want := "When stopped it had been 37m0s in `Bash wp testsum -- make check`. Its slowest calls that ended: 12m0s in `Bash uv sync`."
	if got := c.spent(); got != want {
		t.Errorf("spent = %q\nwant    %q", got, want)
	}
	var none *callClock // a CLI whose output doesn't tell its calls apart
	none.begin("x", "y")
	none.end("x")
	if got := none.spent(); got != "" {
		t.Errorf("nil clock: %q", got)
	}
}
