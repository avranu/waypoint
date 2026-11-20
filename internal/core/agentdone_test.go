package core

import (
	"strings"
	"testing"
)

// WP-91: a person told an agent to do a task it never took (not delegated,
// no `wp next KEY`); finishing it is take then done, so work and usage are
// attributed. Closed work, another agent's live claim and a review step's
// "different" rule still refuse.
func TestAgentDoneTakesWhatWasNeverTaken(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Told", "")
	tokOpus, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	tokHaiku, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "haiku", Agent: "anthropic/claude-haiku-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tokOpus)
	haiku, _ := e.s.TokenActor(e.ctx, tokHaiku)

	// Not delegated, never taken: done succeeds and records a run with its usage.
	plain := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Told in chat"})
	res, err := e.s.AgentDone(e.ctx, opus, plain.Key, AgentDoneInput{Comment: "Did it.", Usage: &AgentUsage{InputTokens: 500, OutputTokens: 100, CostUSD: 0.01}})
	if err != nil || !res.Finished {
		t.Fatalf("done on a never-taken issue = %+v, %v", res, err)
	}
	d := e.get(e.admin, plain.Key)
	if d.Status.Category != CatDone || d.ClaimedBy != "" {
		t.Errorf("after done: %s claimed by %q", d.Status.Category, d.ClaimedBy)
	}
	if len(d.AgentRuns) != 1 || d.AgentRuns[0].Outcome != "done" || d.AgentRuns[0].InputTokens != 500 || d.AgentRuns[0].Agent != "anthropic/claude-opus-5-5" {
		t.Errorf("runs = %+v", d.AgentRuns)
	}
	if again, err := e.s.AgentDone(e.ctx, opus, plain.Key, AgentDoneInput{}); err == nil {
		t.Errorf("finished a closed issue again: %+v", again)
	}

	// An unclaimed backlog task works the same.
	back := e.issue(e.admin, IssueInput{Project: p.Key, Title: "From the backlog", Status: "Backlog"})
	if res, err := e.s.AgentDone(e.ctx, opus, back.Key, AgentDoneInput{}); err != nil || !res.Finished {
		t.Errorf("done on a backlog task = %+v, %v", res, err)
	}

	// Another agent's live claim still refuses, and nothing changes.
	held := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Held", Agent: "any"})
	if _, err := e.s.AgentTake(e.ctx, haiku, held.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentDone(e.ctx, opus, held.Key, AgentDoneInput{}); err == nil || !strings.Contains(err.Error(), "claimed by") {
		t.Errorf("finished another agent's task: %v", err)
	}
	if d := e.get(e.admin, held.Key); d.ClaimedBy != "anthropic/claude-haiku-5-5" || d.Status.Category == CatDone {
		t.Errorf("the other agent's claim changed: %q %s", d.ClaimedBy, d.Status.Category)
	}

	// Usage on a task it never took is recorded; on another's it isn't.
	say := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Usage only"})
	if err := e.s.AgentReportUsage(e.ctx, opus, say.Key, "", AgentUsage{InputTokens: 7}); err != nil {
		t.Errorf("usage on a never-taken task: %v", err)
	}
	if d := e.get(e.admin, say.Key); len(d.AgentRuns) != 1 || d.AgentRuns[0].InputTokens != 7 {
		t.Errorf("usage runs = %+v", d.AgentRuns)
	}
	if err := e.s.AgentReportUsage(e.ctx, opus, held.Key, "", AgentUsage{InputTokens: 7}); err == nil {
		t.Error("usage recorded on another agent's claimed task")
	}

	// A review step that needs a different agent still refuses, and says what to do.
	e.s.CreateWorkflow(e.ctx, e.admin, p.Key, WorkflowInput{Name: str("Reviewed"),
		Steps: &[]WorkflowStep{{Name: "implement"}, {Name: "review", Agent: "different"}}})
	w := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Feature", Agent: "any", Workflow: "Reviewed"})
	if res, err := e.s.AgentDone(e.ctx, opus, w.Key, AgentDoneInput{}); err != nil || res.Step != "review" {
		t.Fatalf("implement done = %+v %v", res, err)
	}
	_, err = e.s.AgentDone(e.ctx, opus, w.Key, AgentDoneInput{})
	if err == nil || !strings.Contains(err.Error(), "different agent") || !strings.Contains(err.Error(), "another agent") {
		t.Errorf("implementer approving its own review: %v", err)
	}
	if _, err := e.s.AgentTake(e.ctx, opus, w.Key, ""); err == nil {
		t.Error("the implementer took its own review step")
	}
	if res, err := e.s.AgentDone(e.ctx, haiku, w.Key, AgentDoneInput{}); err != nil || !res.Finished {
		t.Errorf("a different agent finishing the review = %+v %v", res, err)
	}
}
