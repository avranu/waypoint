package core

import (
	"strings"
	"testing"
	"time"
)

func humanThread() []ReviewThread {
	return []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
		{ID: "501", Author: "alice", Body: "Handle empty rows.", SHA: "7777777aaaa", At: time.Now()}}}}
}

func TestResolveAPersonsCommentAfterReplying(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.noResolv = true
	be.sg.threads = humanThread()
	pr := be.open()
	list, err := be.s.ListReviewComments(be.ctx, be.agent, pr.ID)
	if err != nil || len(list) != 1 || list[0].Dealt {
		t.Fatalf("list = %+v, %v", list, err)
	}
	ref := list[0].Ref
	if _, err := be.s.ResolveReviewComments(be.ctx, be.agent, ResolveInput{IDs: []string{ref}}); !IsInvalid(err) || !strings.Contains(err.Error(), "say what you did") {
		t.Fatalf("resolve without a reply: %v", err)
	}
	if _, err := be.s.ReplyToReviewComment(be.ctx, be.agent, ref, "Handled in 3f2a9c1."); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(be.sg.replies, "\n"), "Handled in 3f2a9c1.") {
		t.Errorf("replies = %v", be.sg.replies)
	}
	if got, err := be.s.ResolveReviewComments(be.ctx, be.agent, ResolveInput{IDs: []string{ref}}); err != nil || got[0].Verdict != VerdictResolved {
		t.Fatalf("resolve after replying: %+v, %v", got, err)
	}
	c := be.comment("501")
	if !c.Dealt || c.Resolved || !c.Replied {
		t.Errorf("comment = %+v (dealt, not resolved on the forge, replied)", c)
	}
	list, _ = be.s.ListReviewComments(be.ctx, be.agent, pr.ID)
	if !strings.Contains(list[0].ResolvedBy, "(agent)") || !list[0].Dealt {
		t.Errorf("resolved by %q, dealt %v", list[0].ResolvedBy, list[0].Dealt)
	}
	if _, err := be.s.syncReviewComments(be.ctx, pr); err != nil {
		t.Fatal(err)
	}
	if c := be.comment("501"); c.Verdict != VerdictResolved || !c.Dealt {
		t.Errorf("after a read: %+v", c)
	}
	if _, err := be.s.UnresolveReviewComments(be.ctx, be.agent, []string{ref}); err != nil {
		t.Fatal(err)
	}
	if c := be.comment("501"); c.Dealt || c.Verdict != VerdictNew {
		t.Errorf("after unresolve: %+v", c)
	}
}

func TestResolveWithANoteReachesTheForge(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.threads = humanThread()
	pr := be.open()
	list, _ := be.s.ListReviewComments(be.ctx, be.agent, pr.ID)
	if _, err := be.s.ResolveReviewComments(be.ctx, be.agent, ResolveInput{IDs: []string{list[0].Ref}, Note: "Not needed: rows is never empty here."}); err != nil {
		t.Fatal(err)
	}
	if len(be.sg.resolved) != 1 || !strings.Contains(strings.Join(be.sg.replies, "\n"), "never empty") {
		t.Errorf("resolved %v replies %v", be.sg.resolved, be.sg.replies)
	}
	if c := be.comment("501"); !c.Resolved || c.Verdict != VerdictResolved {
		t.Errorf("comment = %+v", c)
	}
	if _, err := be.s.UnresolveReviewComments(be.ctx, be.agent, []string{list[0].Ref}); err != nil {
		t.Fatal(err)
	}
	if be.sg.threads[0].Resolved {
		t.Error("thread still resolved on the forge")
	}
}

func TestAgentCantResolveAPersonsOwnCall(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.noResolv = true
	be.sg.threads = humanThread()
	pr := be.open()
	list, _ := be.s.ListReviewComments(be.ctx, be.agent, pr.ID)
	ref := list[0].Ref
	if _, err := be.s.ResolveReviewComments(be.ctx, be.admin, ResolveInput{IDs: []string{ref}}); err != nil {
		t.Fatalf("a person resolves with no reply: %v", err)
	}
	if _, err := be.s.UnresolveReviewComments(be.ctx, be.agent, []string{ref}); !IsInvalid(err) || !strings.Contains(err.Error(), "waits for a person") {
		t.Errorf("an agent reopening a person's resolution: %v", err)
	}
	if _, err := be.s.ReactToReviewComment(be.ctx, be.agent, ref, "sparkles"); !IsInvalid(err) {
		t.Errorf("odd emoji: %v", err)
	}
	if _, err := be.s.ReactToReviewComment(be.ctx, be.agent, ref, "👀"); err != nil {
		t.Fatal(err)
	}
	if got := be.sg.reacted(); len(got) != 1 || got[0] != "inline:501:eyes" {
		t.Errorf("reacted = %v", got)
	}
}

func TestResolvedFindingStopsBlockingTheGateOnForgejo(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.sg.noResolv = true
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	pr := se.pr(5)
	agent := *se.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	list, err := se.s.ListReviewComments(se.ctx, &agent, pr.ID)
	if err != nil || len(list) != 1 || list[0].FindingID == 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := se.s.ResolveReviewComments(se.ctx, &agent, ResolveInput{IDs: []string{list[0].Ref}}); !IsInvalid(err) {
		t.Fatalf("a finding needs a note: %v", err)
	}
	if _, err := se.s.ResolveReviewComments(se.ctx, &agent, ResolveInput{IDs: []string{list[0].Ref}, Note: "The caller checks."}); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if fs := se.findings(5); fs[0].State != FindingResolved {
		t.Fatalf("finding = %+v", fs[0])
	}
	se.s.checkPR(se.ctx, pr.ID, "test")
	if fs := se.findings(5); fs[0].State != FindingResolved {
		t.Fatalf("finding reopened by a read: %+v", fs[0])
	}
	if _, err := se.s.UnresolveReviewComments(se.ctx, &agent, []string{list[0].Ref}); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if fs := se.findings(5); fs[0].State != FindingOpen {
		t.Fatalf("finding after unresolve = %+v", fs[0])
	}
}

// A packet drops the review note Waypoint signs its own comments with,
// however an admin worded it, and keeps everyone else's text whole.
func TestPacketBody(t *testing.T) {
	cases := map[string]string{
		"Off by one.\n\n<sub>**Automated review** by haiku · not checked by a person; verify before acting on it.</sub>\n<!-- waypoint-finding -->": "Off by one.",
		"Off by one.\n\n<sub>Bot notes, read with care</sub>\n<!-- waypoint-finding run=3 n=1 -->":                                                  "Off by one.",
		"Fixed in abc.\n\n<sub>**Automated review** by sonnet</sub>\n<!-- waypoint-reply -->":                                                       "Fixed in abc.",
		"Looks wrong.\n\n<sub>my footnote</sub>":   "Looks wrong.\n\n<sub>my footnote</sub>",
		"<!-- waypoint-finding -->\nNo note here.": "No note here.",
	}
	for in, want := range cases {
		if got := packetBody(in); got != want {
			t.Errorf("packetBody(%q) = %q, want %q", in, got, want)
		}
	}
}
