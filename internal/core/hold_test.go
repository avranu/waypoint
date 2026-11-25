package core

import (
	"strings"
	"testing"
	"time"
)

// A task on hold (WP-269) is no one's to take until the hold ends: wp next
// skips it and says why on the queue, taking it by name is refused, and
// once its date passes it is handed out again on its own.
func TestHoldKeepsATaskFromAgentsUntilItEnds(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	later := e.issue(e.admin, IssueInput{Project: p.Key, Title: "After the quota refreshes", Agent: "claude", Priority: "urgent"})
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Other work", Agent: "claude", Priority: "low"})

	d, err := e.s.SetHold(e.ctx, e.admin, later.Key, HoldInput{Until: "2026-10-08", Note: "The gateway's quota refreshes"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Hold == nil || d.Hold.Until == nil || !d.Hold.Until.Equal(time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)) || d.Hold.By != "robin" {
		t.Fatalf("hold = %+v", d.Hold)
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{Peek: true}); err != nil || task == nil || task.Issue.Key != other.Key {
		t.Fatalf("next while held = %v, %v", task, err)
	}
	q, err := e.s.AgentQueueFor(e.ctx, e.admin, "anthropic/claude-opus-5-5", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Waiting) != 1 || q.Waiting[0].Reason != WaitHold || !strings.Contains(q.Waiting[0].Detail, "until 2026-10-08 (The gateway's quota refreshes)") {
		t.Errorf("waiting = %+v", q.Waiting)
	}
	if _, err := e.s.AgentTake(e.ctx, opus, later.Key, ""); err == nil || !strings.Contains(err.Error(), "on hold until 2026-10-08") {
		t.Errorf("take while held: %v", err)
	}
	if _, err := e.s.AgentDone(e.ctx, opus, later.Key, AgentDoneInput{}); err == nil || !strings.Contains(err.Error(), "on hold") {
		t.Errorf("done while held: %v", err)
	}
	res, err := e.s.Search(e.ctx, e.admin, SearchParams{Query: "is:on-hold"})
	if err != nil || len(res.Issues) != 1 || res.Issues[0].Key != later.Key {
		t.Errorf("is:on-hold = %+v, %v", res, err)
	}

	e.setClock(time.Date(2026, 10, 8, 0, 0, 1, 0, time.UTC))
	if e.get(e.admin, later.Key).Hold != nil {
		t.Error("an ended hold still shows")
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != later.Key {
		t.Fatalf("next after the hold = %v, %v", task, err)
	}
	if res, _ := e.s.Search(e.ctx, e.admin, SearchParams{Query: "is:on-hold"}); len(res.Issues) != 0 {
		t.Errorf("is:on-hold after it ended = %d issues", len(res.Issues))
	}
}

// Holding a task an agent works on ends its claim; a hold without a date
// lasts until lifted; one a person placed is theirs, while an agent's own
// may be lifted by anyone.
func TestHoldEndsTheClaimAndOnlyAPersonLiftsTheirs(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Wait for the schema call", Agent: "claude"})

	if got, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || got == nil || got.Issue.Key != task.Key {
		t.Fatalf("next = %v, %v", got, err)
	}
	if _, err := e.s.SetHold(e.ctx, opus, task.Key, HoldInput{Note: "waiting on Robin"}); err != nil {
		t.Fatal(err)
	}
	d := e.get(e.admin, task.Key)
	if d.ClaimedBy != "" || len(d.AgentRuns) != 1 || d.AgentRuns[0].Outcome != "released" {
		t.Errorf("after hold: claimed by %q, runs %+v", d.ClaimedBy, d.AgentRuns)
	}
	if d.Hold == nil || d.Hold.Until != nil || d.Hold.Agent != "anthropic/claude-opus-5-5" {
		t.Fatalf("hold = %+v", d.Hold)
	}
	e.setClock(e.clockNow().AddDate(1, 0, 0))
	if got, _ := e.s.AgentNext(e.ctx, opus, AgentQuery{Peek: true}); got != nil {
		t.Errorf("a hold without a date ended by itself: next = %s", got.Issue.Key)
	}
	if _, err := e.s.SetHold(e.ctx, e.admin, task.Key, HoldInput{Note: "I'll decide"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.LiftHold(e.ctx, opus, task.Key); err == nil || !strings.Contains(err.Error(), "only a person") {
		t.Errorf("agent lifting a person's hold: %v", err)
	}
	if _, err := e.s.SetHold(e.ctx, opus, task.Key, HoldInput{Until: "+1d"}); err == nil {
		t.Error("an agent changed a person's hold")
	}
	if d, err := e.s.LiftHold(e.ctx, e.admin, task.Key); err != nil || d.Hold != nil {
		t.Fatalf("lift = %+v, %v", d, err)
	}
	if _, err := e.s.LiftHold(e.ctx, e.admin, task.Key); err == nil {
		t.Error("lifted a hold twice")
	}
	if got, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || got == nil || got.Issue.Key != task.Key {
		t.Errorf("next after lifting = %v, %v", got, err)
	}
}

// A task can start on hold, and a hold's end must be in the future.
func TestNewTaskOnHold(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	d := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Rerun the eval", Hold: &HoldInput{Until: "2026-10-09T15:00:00Z"}})
	if d.Hold == nil || d.Hold.Until == nil || !d.Hold.Until.Equal(time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("hold = %+v", d.Hold)
	}
	for _, bad := range []string{"yesterday", "2026-10-05T11:00:00Z", "someday"} {
		if _, err := e.s.SetHold(e.ctx, e.admin, d.Key, HoldInput{Until: bad}); err == nil {
			t.Errorf("hold until %q was accepted", bad)
		}
	}
	if _, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: p.Key, Title: "x", Hold: &HoldInput{Until: "yesterday"}}); err == nil {
		t.Error("created a task on a hold that already ended")
	}
}
