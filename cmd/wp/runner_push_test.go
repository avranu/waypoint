//go:build !windows

package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// A stand-in for claude that edits the checkout, and, as a person's session
// would, pushes another commit to the branch while it works.
const movingClaude = `#!/bin/sh
cat > /dev/null
echo "made by claude" > made-by-claude.txt
R="$MOVE_REMOTE"
blob=$(echo "$MOVE_CONTENT" | git -C "$R" hash-object -w --stdin)
tree=$( (git -C "$R" ls-tree feat | grep -v "	$MOVE_FILE$"; printf '100644 blob %s\t%s\n' "$blob" "$MOVE_FILE") | git -C "$R" mktree)
c=$(git -C "$R" -c user.name=t -c user.email=t@t commit-tree "$tree" -p feat -m "someone else pushed")
git -C "$R" update-ref refs/heads/feat "$c"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","usage":{"input_tokens":1,"output_tokens":1}}'
`

func movedBranchJob(t *testing.T, file, content string) (*runnerProc, string, agentJob) {
	t.Helper()
	fakeCLI(t, "claude", movingClaude)
	remote := newGitRepo(t)
	gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
	gitOut(t, remote, "branch", "feat")
	t.Setenv("MOVE_REMOTE", remote)
	t.Setenv("MOVE_FILE", file)
	t.Setenv("MOVE_CONTENT", content)
	passTestEnv(t, "MOVE_REMOTE", "MOVE_FILE", "MOVE_CONTENT")
	return newTestProc(t), remote, agentJob{CLI: "claude", Prompt: "Add a file.", Repo: remote, Base: "feat", Branch: "feat", Push: true, CommitMessage: "Add the file"}
}

// A push refused because the branch moved during the round isn't thrown
// away: the round's commits are rebased onto the new tip and pushed (WP-1914).
func TestRoundPushRebasesOntoAMovedBranch(t *testing.T) {
	p, remote, job := movedBranchJob(t, "other.txt", "theirs")
	run := newRun(time.Minute)
	o := p.execAgent(context.Background(), run, "agent", job)
	if o.State != "done" {
		t.Fatalf("outcome = %+v\n%s", o, run.out.text())
	}
	res := o.Result.(map[string]any)
	if res["pushed"] != true || res["rebased"] != true {
		t.Errorf("result = %v", res)
	}
	if got := gitOut(t, remote, "log", "--format=%s", "-2", "feat"); got != "Add the file\nsomeone else pushed" {
		t.Errorf("branch = %q", got)
	}
	if got := gitOut(t, remote, "rev-parse", "feat"); res["commit"] != got {
		t.Errorf("reported commit %v, branch at %s", res["commit"], got)
	}
	for _, f := range []string{"made-by-claude.txt", "other.txt"} {
		if gitOut(t, remote, "cat-file", "-t", "feat:"+f) != "blob" {
			t.Errorf("%s isn't on the branch", f)
		}
	}
}

func TestRoundPushFailsWhenTheRebaseConflicts(t *testing.T) {
	p, remote, job := movedBranchJob(t, "made-by-claude.txt", "theirs")
	run := newRun(time.Minute)
	o := p.execAgent(context.Background(), run, "agent", job)
	if o.State != "failed" || !strings.HasPrefix(o.Error, "pushing feat failed: ") || !strings.Contains(o.Error, "don't rebase") {
		t.Fatalf("outcome = %+v", o)
	}
	if got := gitOut(t, remote, "log", "-1", "--format=%s", "feat"); got != "someone else pushed" {
		t.Errorf("branch = %q", got)
	}
	if left, _ := os.ReadDir(p.cacheDir() + "/work"); len(left) != 0 {
		t.Errorf("work trees left: %v", left)
	}
}

// A job that ends before its agent starts says so in its result, so the
// round isn't counted.
func TestAgentJobFaultsBeforeTheAgentStarts(t *testing.T) {
	fakeCLI(t, "claude", fakeClaude)
	p := newTestProc(t)
	o := p.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "claude", Prompt: "x", Repo: "/no/such/repository"})
	if res, _ := o.Result.(map[string]any); o.State != "failed" || res["fault"] != "checkout" || !strings.Contains(o.Error, "can't get the repository") {
		t.Fatalf("clone failure = %+v", o)
	}
	no := false
	p.clis = []runnerCLI{{Name: "claude", SignedIn: &no}}
	if res, _ := p.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "claude", Prompt: "x", Repo: "/x"}).Result.(map[string]any); res["fault"] != "cli" {
		t.Errorf("signed out: %v", res)
	}
}
