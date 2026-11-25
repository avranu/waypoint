package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp next asks for the task's arms, keeps them on the claim and does the
// arm: the next_brief experiment's "none" prints no brief.
func TestNextDoesTheBriefArm(t *testing.T) {
	s := newSessionEnv(t)
	t.Setenv("WP_TASK", "")
	t.Setenv("CLAUDECODE", "")
	commitFiles(t, s.repo, map[string]string{"core/merge.go": briefMergeGo, "README.md": "# App\n"})
	ctx := context.Background()

	s.asAgent()
	i, err := s.svc.CreateIssue(ctx, s.me, core.IssueInput{Project: s.project.Key, Title: "Make MergeReady wait", Description: "Change `core/merge.go`.", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if out := s.wp("next", i.Key); !strings.Contains(out, "Where to start") {
		t.Fatalf("no run:\n%s", out)
	}

	s.asHuman()
	if out := s.wp("experiment", "start", "next_brief", "--days", "7", "-m", "does the brief pay?"); !strings.Contains(out, "Started next_brief") {
		t.Fatalf("start:\n%s", out)
	}
	if out, code := s.run("experiment", "start", "next_brief", "--weights", "brief"); code == 0 || !strings.Contains(out, "ARM=N") {
		t.Errorf("bad weights: %d\n%s", code, out)
	}
	if out := s.wp("experiment", "ls"); !strings.Contains(out, "next_brief") || !strings.Contains(out, "live: run") {
		t.Errorf("ls:\n%s", out)
	}

	s.asAgent()
	agent := &core.Actor{User: s.me.User, Agent: "anthropic/claude-opus-5-5"}
	seen := map[string]bool{}
	for n := 0; n < 30 && len(seen) < 2; n++ {
		x, err := s.svc.CreateIssue(ctx, s.me, core.IssueInput{Project: s.project.Key, Title: fmt.Sprint("Task ", n), Description: "Change `core/merge.go`.", Agent: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		out := s.wp("next", x.Key)
		got, err := s.svc.IssueArms(ctx, agent, x.Key, core.IssueArmsInput{Experiments: []string{"next_brief"}})
		if err != nil {
			t.Fatal(err)
		}
		arm := got.Arms["next_brief"].Arm
		seen[arm] = true
		if brief := strings.Contains(out, "Where to start"); brief != (arm == "brief") {
			t.Fatalf("%s in arm %s; brief printed %v:\n%s", x.Key, arm, brief, out)
		}
		c := &cli{dir: s.dir}
		c.state = c.loadState()
		if a := c.armHere(x.Key, expNextBrief); a != arm {
			t.Fatalf("the claim keeps %q; the server says %q", a, arm)
		}
	}
	if len(seen) != 2 {
		t.Errorf("arms seen: %v", seen)
	}

	s.asHuman()
	if out := s.wp("experiment", "show", "next_brief", "--scope", "all"); !strings.Contains(out, "Caveats:") || !strings.Contains(out, "Too few") {
		t.Errorf("show:\n%s", out)
	}
	if out := s.wp("experiment", "stop", "next_brief", "-m", "test over"); !strings.Contains(out, "Stopped next_brief") {
		t.Errorf("stop:\n%s", out)
	}
	s.asAgent()
	if out, code := s.run("experiment", "start", "next_brief"); code == 0 {
		t.Errorf("an agent started an experiment:\n%s", out)
	}
}

// A hook finds its session's arm on the claim, from another directory of
// the session too; no claim is the control.
func TestSessionArm(t *testing.T) {
	c := &cli{}
	c.state.Claims = map[string]*claim{"/w/one": {Key: "WP-1", Session: "s9", Arms: map[string]string{expSearchNudge: "off"}}}
	if a := c.sessionArm("/w/one/sub", "s9", expSearchNudge); a != "off" {
		t.Errorf("by session: %q", a)
	}
	if a := c.sessionArm("/elsewhere", "s8", expSearchNudge); a != "" {
		t.Errorf("another session: %q", a)
	}
}
