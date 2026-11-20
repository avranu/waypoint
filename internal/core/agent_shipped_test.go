package core

import (
	"strings"
	"testing"
)

// A task out on a pull request is the merge's or a person's, not an
// agent's: shipping ends the claim, wp next neither hands it back nor out,
// and wp done leaves it where it is until the pull request merges (WP-246).
func TestShippedTaskLeavesTheAgentsQueue(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	shipped := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Shipped work", Agent: "claude", Priority: "urgent"})
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Other work", Agent: "claude", Priority: "low"})
	link := func(key, url string) int64 {
		t.Helper()
		l, err := e.s.AddLink(e.ctx, e.admin, key, url, "")
		if err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
		e.setLinkState(l.ID, "open")
		return l.ID
	}
	inReview := "In Review"

	// wp ship: the agent that holds it moves it to In Review by its link.
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != shipped.Key {
		t.Fatalf("next = %v, %v", task, err)
	}
	const pr = "https://github.com/me/web/pull/7"
	l := link(shipped.Key, pr)
	if _, err := e.s.UpdateIssue(e.ctx, opus, shipped.Key, IssuePatch{Status: &inReview, ViaLink: pr}); err != nil {
		t.Fatal(err)
	}
	d := e.get(e.admin, shipped.Key)
	if d.ClaimedBy != "" || len(d.AgentRuns) != 1 || d.AgentRuns[0].Outcome != "done" {
		t.Errorf("after ship: claimed by %q, runs %+v", d.ClaimedBy, d.AgentRuns)
	}
	// Asking again hands out other work, not the shipped task.
	task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{})
	if err != nil || task == nil || task.Issue.Key != other.Key {
		t.Fatalf("next after ship = %v, %v", task, err)
	}
	q, err := e.s.AgentQueueFor(e.ctx, e.admin, "anthropic/claude-opus-5-5", "")
	if err != nil {
		t.Fatal(err)
	}
	var waits []string
	for _, it := range q.Waiting {
		waits = append(waits, it.Issue.Key+" "+it.Reason+": "+it.Detail)
	}
	if len(waits) != 1 || !strings.HasPrefix(waits[0], shipped.Key+" review: ") || !strings.Contains(waits[0], "me/web#7 is open") {
		t.Errorf("waiting = %q", waits)
	}

	// wp done before the merge: the agent's part ends, the task stays.
	res, err := e.s.AgentDone(e.ctx, opus, shipped.Key, AgentDoneInput{Comment: "PR #7"})
	if err != nil || res.Finished || res.Waiting != "me/web#7" || res.Status != "In Review" {
		t.Fatalf("done before merge = %+v, %v", res, err)
	}
	// After it merges, wp done finishes it as before.
	e.setLinkState(l, "merged")
	if res, err := e.s.AgentDone(e.ctx, opus, shipped.Key, AgentDoneInput{}); err != nil || !res.Finished {
		t.Fatalf("done after merge = %+v, %v", res, err)
	}

	// Moved to In Review by its link while the agent held it (not by the
	// agent: an automation): asking again lets the claim go.
	held := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Held work", Agent: "claude", Priority: "urgent"})
	if _, err := e.s.AgentDone(e.ctx, opus, other.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != held.Key {
		t.Fatalf("next = %v, %v", task, err)
	}
	const pr2 = "https://github.com/me/web/pull/8"
	l2 := link(held.Key, pr2)
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, held.Key, IssuePatch{Status: &inReview, ViaLink: pr2}); err != nil {
		t.Fatal(err)
	}
	e.setLinkState(l2, "merged")
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task != nil {
		t.Errorf("next with only shipped work = %+v, %v", task, err)
	}
	if d := e.get(e.admin, held.Key); d.ClaimedBy != "" {
		t.Errorf("shipped claim kept by %q", d.ClaimedBy)
	}
	// A person moving it back to To Do hands it out again.
	todo := "todo"
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, held.Key, IssuePatch{Status: &todo}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != held.Key {
		t.Errorf("next after To Do = %+v, %v", task, err)
	}
}

// wp ship finishes the agent's part even when the project's "In Review
// when a PR opens" rule moved the task a moment before ship asked to, and
// when a long turn let the claim lapse: only a run that ended done tells
// autopilot the session finished its task (WP-537).
func TestShipFinishesTheRunWhateverMovedItFirst(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	inReview := "In Review"
	ship := func(key, pr string, before func()) *IssueDetail {
		t.Helper()
		if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != key {
			t.Fatalf("next = %v, %v", task, err)
		}
		if _, err := e.s.AddLink(e.ctx, e.admin, key, pr, ""); err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
		before()
		if _, err := e.s.UpdateIssue(e.ctx, opus, key, IssuePatch{Status: &inReview, ViaLink: pr}); err != nil {
			t.Fatal(err)
		}
		return e.get(e.admin, key)
	}

	// The rule moved it first: ship's own move finds it there.
	ruled := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Moved by the rule", Agent: "claude", Priority: "urgent"})
	const pr1 = "https://github.com/me/web/pull/7"
	d := ship(ruled.Key, pr1, func() {
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, ruled.Key, IssuePatch{Status: &inReview, ViaLink: pr1}); err != nil {
			t.Fatal(err)
		}
	})
	if d.Status.Name != "In Review" || d.ClaimedBy != "" || len(d.AgentRuns) != 1 || d.AgentRuns[0].Outcome != "done" {
		t.Errorf("after the rule and ship: %s, claimed by %q, runs %+v", d.Status.Name, d.ClaimedBy, d.AgentRuns)
	}

	// The claim lapsed during a long turn: the run expired, ship still
	// records the finish.
	long := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Long turn", Agent: "claude", Priority: "urgent"})
	d = ship(long.Key, "https://github.com/me/web/pull/8", func() {
		if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE agent_runs SET ended_at = started_at + 1, outcome = 'expired' WHERE issue_id = ?`, long.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ?`, long.ID); err != nil {
			t.Fatal(err)
		}
	})
	if d.ClaimedBy != "" || len(d.AgentRuns) != 2 || d.AgentRuns[0].Outcome != "done" || d.AgentRuns[1].Outcome != "expired" {
		t.Errorf("after a lapsed claim and ship: claimed by %q, runs %+v", d.ClaimedBy, d.AgentRuns)
	}

	// Another agent's claim stays theirs.
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Taken by another", Agent: "claude", Priority: "urgent"})
	d = ship(other.Key, "https://github.com/me/web/pull/9", func() {
		if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE issues SET claimed_by = 'openai/codex' WHERE id = ?`, other.ID); err != nil {
			t.Fatal(err)
		}
	})
	if d.ClaimedBy != "openai/codex" || len(d.AgentRuns) != 1 || d.AgentRuns[0].Outcome == "done" {
		t.Errorf("another's claim: claimed by %q, runs %+v", d.ClaimedBy, d.AgentRuns)
	}
}

func (e *env) setLinkState(id int64, state string) {
	e.t.Helper()
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE external_links SET state = ? WHERE id = ?`, state, id); err != nil {
		e.t.Fatal(err)
	}
}

// A task moved on to In Review waits for a person even with no open or
// merged pull request linked: one closed after its commits merged through
// another, or work that went in without one. A claim held on it is let go
// (WP-267).
func TestHandedOnTaskLeavesTheAgentsQueue(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	closed := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Merged through another", Agent: "claude", Priority: "urgent"})
	bare := e.issue(e.admin, IssueInput{Project: p.Key, Title: "No pull request", Agent: "claude", Priority: "high"})
	held := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Held work", Agent: "claude", Priority: "medium"})
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Other work", Agent: "claude", Priority: "low"})
	inReview := "In Review"
	l, err := e.s.AddLink(e.ctx, e.admin, closed.Key, "https://github.com/me/web/pull/7", "")
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	e.setLinkState(l.ID, "closed")
	for _, key := range []string{closed.Key, bare.Key} {
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, key, IssuePatch{Status: &inReview}); err != nil {
			t.Fatal(err)
		}
	}

	// The agent takes the held task, then it moves on to In Review.
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != held.Key {
		t.Fatalf("next = %v, %v", task, err)
	}
	if _, err := e.s.UpdateIssue(e.ctx, opus, held.Key, IssuePatch{Status: &inReview}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != other.Key {
		t.Fatalf("next after In Review = %v, %v", task, err)
	}
	if d := e.get(e.admin, held.Key); d.ClaimedBy != "" {
		t.Errorf("handed-on claim kept by %q", d.ClaimedBy)
	}
	q, err := e.s.AgentQueueFor(e.ctx, e.admin, "anthropic/claude-opus-5-5", "")
	if err != nil {
		t.Fatal(err)
	}
	waits := map[string]string{}
	for _, it := range q.Waiting {
		waits[it.Issue.Key] = it.Reason + ": " + it.Detail
	}
	for _, key := range []string{closed.Key, bare.Key, held.Key} {
		if !strings.HasPrefix(waits[key], "review: It moved on to In Review") {
			t.Errorf("%s waits %q", key, waits[key])
		}
	}
	// A person moving one back to To Do hands it out again.
	if _, err := e.s.AgentDone(e.ctx, opus, other.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	todo := "todo"
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, bare.Key, IssuePatch{Status: &todo}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != bare.Key {
		t.Errorf("next after To Do = %+v, %v", task, err)
	}
}
