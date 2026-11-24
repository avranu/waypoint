package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePrePushClaude makes a change on every run; on a run asked to fix the
// pre-push check it also makes fixed.txt, unless FAKE_NOFIX is set.
const fakePrePushClaude = `#!/bin/sh
prompt=$(cat)
case "$prompt" in
  *"pre-push check"*)
    echo "$prompt" > "$FAKE_OUT/fixprompt"
    [ -n "$FAKE_NOFIX" ] || echo ok > fixed.txt
    ;;
  *) echo work > work.txt ;;
esac
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","usage":{"input_tokens":10,"output_tokens":5},"total_cost_usd":0.01}'
`

// prePushScript passes once fixed.txt exists, and notes each run.
const prePushScript = `echo ran >> "$FAKE_OUT/ran"
test -f fixed.txt || { echo "lint: fixed.txt is missing"; exit 1; }
`

// prePushRepo is a repository, optionally with the check committed in it,
// that takes pushes to its checked-out branch.
func prePushRepo(t *testing.T, script string) string {
	t.Helper()
	remote := newGitRepo(t)
	gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
	if script != "" {
		if err := os.MkdirAll(filepath.Join(remote, ".waypoint"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(remote, prePushFile), []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, remote, "add", "-A")
		gitOut(t, remote, "commit", "-q", "-m", "Add the check")
	}
	return remote
}

func runPrePushJob(t *testing.T, remote string, mod func(*agentJob)) (map[string]any, outcome, string) {
	t.Helper()
	passTestEnv(t, "FAKE_NOFIX")
	job := agentJob{CLI: "claude", Prompt: "Do it.", Repo: remote, Branch: "agent/round", Push: true, Permission: "edit_run", CommitMessage: "Round"}
	if mod != nil {
		mod(&job)
	}
	run := newRun(time.Minute)
	o := newTestProc(t).execAgent(context.Background(), run, "agent", job)
	res, _ := o.Result.(map[string]any)
	return res, o, run.out.text()
}

func timesRan(t *testing.T, rec string) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(rec, "ran"))
	return strings.Count(string(b), "ran")
}

func TestPrePushCheck(t *testing.T) {
	t.Run("no file", func(t *testing.T) {
		rec := fakeCLI(t, "claude", fakePrePushClaude)
		remote := prePushRepo(t, "")
		res, o, log := runPrePushJob(t, remote, nil)
		if o.State != "done" || res["pushed"] != true {
			t.Fatalf("outcome = %+v\n%s", o, log)
		}
		if _, ok := res["pre_push"]; ok || timesRan(t, rec) != 0 {
			t.Errorf("pre_push = %v, ran %d", res["pre_push"], timesRan(t, rec))
		}
	})
	t.Run("passes", func(t *testing.T) {
		rec := fakeCLI(t, "claude", fakePrePushClaude)
		remote := prePushRepo(t, "echo ran >> \"$FAKE_OUT/ran\"\n")
		res, o, log := runPrePushJob(t, remote, nil)
		if o.State != "done" || res["pushed"] != true || res["pre_push"] != "passed" || timesRan(t, rec) != 1 {
			t.Fatalf("result = %v, ran %d\n%s", res, timesRan(t, rec), log)
		}
		if !strings.Contains(log, "Running the repository's pre-push check (.waypoint/pre-push)") {
			t.Errorf("log = %s", log)
		}
	})
	t.Run("fixed by the agent", func(t *testing.T) {
		rec := fakeCLI(t, "claude", fakePrePushClaude)
		remote := prePushRepo(t, prePushScript)
		res, o, log := runPrePushJob(t, remote, nil)
		if o.State != "done" || res["pushed"] != true || res["pre_push"] != "passed after a fix" || timesRan(t, rec) != 2 {
			t.Fatalf("result = %v, ran %d\n%s", res, timesRan(t, rec), log)
		}
		if got := gitOut(t, remote, "show", "agent/round:fixed.txt"); got != "ok" {
			t.Errorf("fix not pushed: %q", got)
		}
		if p, _ := os.ReadFile(filepath.Join(rec, "fixprompt")); !strings.Contains(string(p), "lint: fixed.txt is missing") || !strings.Contains(string(p), "<untrusted>") {
			t.Errorf("fix prompt = %q", p)
		}
		if tok, _ := res["tokens"].(tokenUse); tok.Input != 20 || tok.Output != 10 {
			t.Errorf("tokens = %+v", res["tokens"])
		}
	})
	t.Run("still failing", func(t *testing.T) {
		rec := fakeCLI(t, "claude", fakePrePushClaude)
		t.Setenv("FAKE_NOFIX", "1")
		remote := prePushRepo(t, prePushScript)
		res, o, log := runPrePushJob(t, remote, nil)
		pp, _ := res["pre_push"].(string)
		if o.State != "done" || res["pushed"] != true || !strings.HasPrefix(pp, "failed: ") || !strings.Contains(pp, "fixed.txt is missing") || timesRan(t, rec) != 2 {
			t.Fatalf("result = %v, ran %d\n%s", res, timesRan(t, rec), log)
		}
		if got := gitOut(t, remote, "show", "agent/round:work.txt"); got != "work" {
			t.Errorf("work not pushed: %q", got)
		}
	})
	t.Run("times out", func(t *testing.T) {
		defer func(l time.Duration) { prePushLimit = l }(prePushLimit)
		prePushLimit = 300 * time.Millisecond
		fakeCLI(t, "claude", fakePrePushClaude)
		t.Setenv("FAKE_NOFIX", "1")
		remote := prePushRepo(t, "sleep 30\n")
		res, o, log := runPrePushJob(t, remote, nil)
		if pp, _ := res["pre_push"].(string); o.State != "done" || res["pushed"] != true || !strings.HasPrefix(pp, "failed: timed out after") {
			t.Fatalf("result = %v\n%s", res, log)
		}
	})
	t.Run("read only", func(t *testing.T) {
		rec := fakeCLI(t, "claude", fakePrePushClaude)
		remote := prePushRepo(t, prePushScript)
		res, _, _ := runPrePushJob(t, remote, func(j *agentJob) { j.ReadOnly, j.Push = true, false })
		if _, ok := res["pre_push"]; ok || timesRan(t, rec) != 0 {
			t.Errorf("pre_push = %v, ran %d", res["pre_push"], timesRan(t, rec))
		}
	})
	t.Run("salvage does not run it", func(t *testing.T) {
		rec := fakeCLI(t, "claude", fakeClaudeOutOfTime)
		remote := prePushRepo(t, prePushScript)
		p := newTestProc(t)
		job := agentJob{CLI: "claude", Prompt: "x", Repo: remote, Branch: "agent/round", Push: true, Permission: "edit_run", CommitMessage: "Round", KeepOnTimeout: true}
		o := p.execAgent(context.Background(), newRun(5*time.Second), "agent", job)
		res, _ := o.Result.(map[string]any)
		if res["pushed"] != true || timesRan(t, rec) != 0 {
			t.Fatalf("result = %v, ran %d", res, timesRan(t, rec))
		}
		if _, ok := res["pre_push"]; ok {
			t.Errorf("pre_push = %v", res["pre_push"])
		}
	})
}
