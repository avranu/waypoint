package main

import (
	"context"
	"os"
	"os/exec"
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
    [ -z "$FAKE_LOCK" ] || touch "$(git rev-parse --git-dir)/index.lock"
    ;;
  *) echo work > work.txt; [ -z "$FAKE_PROTECT" ] || echo keep > keep.txt ;;
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
	passTestEnv(t, "FAKE_NOFIX", "FAKE_LOCK", "FAKE_PROTECT")
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
		rec := fakeCLI(t, "claude", fakePrePushClaude)
		t.Setenv("FAKE_NOFIX", "1")
		remote := prePushRepo(t, "sleep 30\n")
		res, o, log := runPrePushJob(t, remote, nil)
		if pp, _ := res["pre_push"].(string); o.State != "done" || res["pushed"] != true || !strings.HasPrefix(pp, "failed: timed out after") {
			t.Fatalf("result = %v\n%s", res, log)
		}
		// A timeout is nothing for the agent to fix (audit #1748.4).
		if _, err := os.Stat(filepath.Join(rec, "fixprompt")); err == nil || strings.Contains(log, "Asking the agent to fix") {
			t.Errorf("the agent was run to fix a timeout\n%s", log)
		}
	})
	// The script is the agent's copy: only an agent that may run commands
	// anyway, outside a confined CLI, gets it run (audit #1748.1).
	for name, mod := range map[string]func(*agentJob){
		"edit only": func(j *agentJob) { j.Permission = "edit" },
		"confined":  func(j *agentJob) { j.Confine = true },
	} {
		t.Run(name, func(t *testing.T) {
			rec := fakeCLI(t, "claude", fakePrePushClaude)
			remote := prePushRepo(t, prePushScript)
			res, o, log := runPrePushJob(t, remote, mod)
			if o.State == "done" && res["pushed"] != true {
				t.Fatalf("outcome = %+v\n%s", o, log)
			}
			if _, ok := res["pre_push"]; ok || timesRan(t, rec) != 0 {
				t.Errorf("pre_push = %v, ran %d", res["pre_push"], timesRan(t, rec))
			}
		})
	}
	t.Run("read only", func(t *testing.T) {
		rec := fakeCLI(t, "claude", fakePrePushClaude)
		remote := prePushRepo(t, prePushScript)
		res, _, _ := runPrePushJob(t, remote, func(j *agentJob) { j.ReadOnly, j.Push = true, false })
		if _, ok := res["pre_push"]; ok || timesRan(t, rec) != 0 {
			t.Errorf("pre_push = %v, ran %d", res["pre_push"], timesRan(t, rec))
		}
	})
	// A fix that can't be committed isn't pushed, so the check's pass on
	// the work tree is no verdict on what is (c6954).
	t.Run("fix not committed", func(t *testing.T) {
		rec := fakeCLI(t, "claude", fakePrePushClaude)
		t.Setenv("FAKE_LOCK", "1")
		remote := prePushRepo(t, prePushScript)
		res, o, log := runPrePushJob(t, remote, nil)
		pp, _ := res["pre_push"].(string)
		if o.State != "done" || res["pushed"] != true || !strings.HasPrefix(pp, "failed: the agent's fix couldn't be committed") || timesRan(t, rec) != 1 {
			t.Fatalf("result = %v, ran %d\n%s", res, timesRan(t, rec), log)
		}
	})
	// The check ran on a protected change that is then taken out: its
	// verdict isn't on the pushed commits (c6953).
	t.Run("protected files restored", func(t *testing.T) {
		fakeCLI(t, "claude", fakePrePushClaude)
		t.Setenv("FAKE_PROTECT", "1")
		remote := prePushRepo(t, "true\n")
		res, o, log := runPrePushJob(t, remote, func(j *agentJob) { j.ProtectPaths = []string{"work.txt"} })
		pp, _ := res["pre_push"].(string)
		if o.State != "done" || res["pushed"] != true || !strings.HasPrefix(pp, "not run on the pushed commits") {
			t.Fatalf("result = %v\n%s", res, log)
		}
		if got := gitOut(t, remote, "show", "agent/round:keep.txt"); got != "keep" {
			t.Errorf("keep.txt not pushed: %q", got)
		}
	})
	// A job a person stops during the check pushes nothing (c6948).
	t.Run("stopped during the check", func(t *testing.T) {
		rec := fakeCLI(t, "claude", fakePrePushClaude)
		remote := prePushRepo(t, "touch \"$FAKE_OUT/checking\"; sleep 30\n")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			for ctx.Err() == nil {
				if _, err := os.Stat(filepath.Join(rec, "checking")); err == nil {
					cancel()
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		job := agentJob{CLI: "claude", Prompt: "Do it.", Repo: remote, Branch: "agent/round", Push: true, Permission: "edit_run", CommitMessage: "Round"}
		run := newRun(time.Minute)
		o := newTestProc(t).execAgent(ctx, run, "agent", job)
		if o.State != "cancelled" {
			t.Fatalf("outcome = %+v\n%s", o, run.out.text())
		}
		if err := exec.Command("git", "-C", remote, "rev-parse", "-q", "--verify", "refs/heads/agent/round").Run(); err == nil {
			t.Errorf("the branch was pushed after the job was stopped")
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
