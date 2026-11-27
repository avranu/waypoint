package core

import (
	"slices"
	"strings"
	"testing"
)

// Who made each step of a trace is what was stored when it was made, not
// read off its ends later: an agent's comment, an agent's parent, an
// agent acting through a person's shell all stay an agent's, and only a
// person acting directly confirms, approves, dismisses or stops asking
// (WP-382, WP-ADR50).
func TestGoalProvenance(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Delta", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	shell := *e.admin
	shell.AgentEnv = "claude-code"

	goal := e.goal(e.admin, p.Key, "Readers trust every number")
	typo := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix the login page typo"})
	e.goals(e.admin, p.Key)
	if got := e.grounding(typo.Key); got != GroundPersonRequest {
		t.Fatalf("person's task before links = %s", got)
	}

	if _, err := e.s.AddComment(e.ctx, &agent, typo.Key, "This serves [["+goal.Title+"]]."); err != nil {
		t.Fatal(err)
	}
	e.goals(e.admin, p.Key)
	if got := e.grounding(typo.Key); got != GroundAgentGoal {
		t.Errorf("after an agent's comment link = %s, want %s", got, GroundAgentGoal)
	}
	if _, err := e.s.AddComment(e.ctx, e.admin, typo.Key, "Yes: [["+goal.Title+"]]."); err != nil {
		t.Fatal(err)
	}
	e.goals(e.admin, p.Key)
	if got := e.grounding(typo.Key); got != GroundPersonGoal {
		t.Errorf("after a person's comment link = %s, want %s", got, GroundPersonGoal)
	}

	child := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Tidy the footer spacing"})
	parent := typo.Key
	if _, err := e.s.UpdateIssue(e.ctx, &agent, child.Key, IssuePatch{Parent: Opt[string]{Set: true, Value: &parent}}); err != nil {
		t.Fatal(err)
	}
	e.goals(e.admin, p.Key)
	if got := e.grounding(child.Key); got != GroundAgentGoal {
		t.Errorf("under a parent an agent set = %s, want %s", got, GroundAgentGoal)
	}
	none := ""
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, child.Key, IssuePatch{Parent: Opt[string]{Set: true, Value: &none}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, child.Key, IssuePatch{Parent: Opt[string]{Set: true, Value: &parent}}); err != nil {
		t.Fatal(err)
	}
	e.goals(e.admin, p.Key)
	if got := e.grounding(child.Key); got != GroundPersonGoal {
		t.Errorf("under a parent a person set = %s, want %s", got, GroundPersonGoal)
	}

	title := "Owners see the week at a glance"
	g, err := e.s.CreateDesign(e.ctx, &agent, DesignInput{Title: &title, Kind: str("goal"), Project: &p.Key, For: typo.Key})
	if err != nil {
		t.Fatal(err)
	}
	if g.Approved || g.Requested != nil {
		t.Errorf("an agent's goal --for a person's task = approved %v, requested %+v", g.Approved, g.Requested)
	}

	stray := e.issue(&agent, IssueInput{Project: p.Key, Title: "Rename the sidebar colours"})
	l := e.serve(&agent, stray.Key, goal.ID)
	o := e.goals(e.admin, p.Key)
	if !slices.ContainsFunc(o.Links, func(x GoalLink) bool { return x.LinkID == l.ID }) {
		t.Fatalf("an agent's link isn't waiting: %+v", o.Links)
	}
	if _, err := e.s.ConfirmGoalLink(e.ctx, &shell, l.ID); err == nil {
		t.Error("an agent's shell confirmed a link")
	}
	if err := e.s.DismissGoalLink(e.ctx, &shell, l.ID); err == nil {
		t.Error("an agent's shell dismissed a link")
	}
	if _, err := e.s.AcknowledgeGoalLinks(e.ctx, &shell, []int64{l.ID}); err == nil {
		t.Error("an agent's shell stopped the asking")
	}
	if _, err := e.s.ApproveDesign(e.ctx, &shell, g.ID, ""); err == nil {
		t.Error("an agent's shell approved a goal")
	}

	n, err := e.s.AcknowledgeGoalLinks(e.ctx, e.admin, []int64{l.ID})
	if err != nil || n != 1 {
		t.Fatalf("acknowledge = %d, %v", n, err)
	}
	o = e.goals(e.admin, p.Key)
	if slices.ContainsFunc(o.Links, func(x GoalLink) bool { return x.LinkID == l.ID }) {
		t.Error("an acknowledged link still waits")
	}
	if got := e.grounding(stray.Key); got != GroundAgentGoal {
		t.Errorf("after stop asking = %s, want %s (not a confirm)", got, GroundAgentGoal)
	}

	d := e.get(e.admin, stray.Key)
	if d.Goal == nil || d.Goal.Grounding != GroundAgentGoal || d.Goal.Goal == nil || d.Goal.Goal.ID != goal.ID {
		t.Errorf("issue goal = %+v", d.Goal)
	}
	if !hasRiskFactor(d.Risk, riskLooseGoal) {
		t.Errorf("an agent's task on an agent's link: risk = %+v", d.Risk)
	}
	if intent := e.s.reviewIntent(e.ctx, 0); intent != "" {
		t.Errorf("a pull request with no tasks has intent %q", intent)
	}

	if err := e.s.DismissGoalLink(e.ctx, e.admin, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddItemLink(e.ctx, &agent, LinkInput{From: LinkEnd{Type: KIssue, Key: stray.Key}, To: LinkEnd{Type: KDesign, ID: goal.ID}, Rel: "serves"}); err == nil {
		t.Error("an agent relinked what a person turned down")
	}
	e.serve(e.admin, stray.Key, goal.ID)
	e.goals(e.admin, p.Key)
	if got := e.grounding(stray.Key); got != GroundPersonGoal {
		t.Errorf("a person's own link after their no = %s, want %s", got, GroundPersonGoal)
	}

	fresh := e.issue(&agent, IssueInput{Project: p.Key, Title: "Add a footer badge"})
	e.s.goalsSettle(e.ctx)
	if _, err := e.s.db.W.Exec(`DELETE FROM goal_traces WHERE item_type = 'issue' AND item_id = ?`, fresh.ID); err != nil {
		t.Fatal(err)
	}
	d = e.get(e.admin, fresh.Key)
	if d.Goal == nil || !d.Goal.Pending {
		t.Errorf("untraced issue goal = %+v", d.Goal)
	}
	if !slices.ContainsFunc(d.Risk.Reasons, func(r RiskReason) bool { return strings.Contains(r.Text, "hasn't traced") }) {
		t.Errorf("untraced risk = %+v", d.Risk)
	}
	if o := e.goalsNoRecompute(e.admin, p.Key); o.Tracing != 1 {
		t.Errorf("tracing = %d, want 1", o.Tracing)
	}
}

// Drift counts each finished task as it traced when it was finished, keeps
// Waypoint's guesses apart from links people or agents made, and shows
// each person only the work they can see.
func TestGoalDriftSnapshot(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Epsilon", "")
	goal := e.goal(e.admin, p.Key, "Travellers find a route in one tap")
	linked := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Speed up the planner"})
	l := e.serve(e.admin, linked.Key, goal.ID)
	guessed := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Find a route for travellers in one tap"})
	e.goals(e.admin, p.Key)
	if got := e.grounding(guessed.Key); got != GroundInferredGoal {
		t.Fatalf("guessed task = %s, want %s", got, GroundInferredGoal)
	}
	done := "Done"
	for _, k := range []string{linked.Key, guessed.Key} {
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, k, IssuePatch{Status: &done}); err != nil {
			t.Fatal(err)
		}
	}
	e.goals(e.admin, p.Key)
	if err := e.s.RemoveItemLink(e.ctx, e.admin, l.ID); err != nil {
		t.Fatal(err)
	}
	o := e.goals(e.admin, p.Key)
	if o.Drift == nil {
		t.Fatal("no drift")
	}
	if o.Drift.Week[GroundPersonGoal] != 1 || o.Drift.Week[GroundInferredGoal] != 1 {
		t.Errorf("week = %+v", o.Drift.Week)
	}
	if o.Drift.Grounded != 0.5 || o.Drift.Guessed != 0.5 || !strings.Contains(o.Drift.Summary, "guess") {
		t.Errorf("grounded %v guessed %v summary %q", o.Drift.Grounded, o.Drift.Guessed, o.Drift.Summary)
	}
}

func (e *env) goalsNoRecompute(a *Actor, project string) *GoalsOverview {
	e.t.Helper()
	o, err := e.s.ListGoals(e.ctx, a, project)
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func hasRiskFactor(r *ProvenanceRisk, factor string) bool {
	return r != nil && slices.ContainsFunc(r.Reasons, func(x RiskReason) bool { return x.Factor == factor })
}

// An agent stating a link Waypoint already guessed makes it the agent's
// claim, whichever came first.
func TestGoalGuessThenAgentLink(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Zeta", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	goal := e.goal(e.admin, p.Key, "Travellers find a route in one tap")
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Speed up the planner"})
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Find a route for travellers in one tap"})
	e.goals(e.admin, p.Key)
	if got := e.grounding(task.Key); got != GroundInferredGoal {
		t.Fatalf("before the agent's link = %s, want %s", got, GroundInferredGoal)
	}
	l := e.serve(&agent, task.Key, goal.ID)
	o := e.goals(e.admin, p.Key)
	if got := e.grounding(task.Key); got != GroundAgentGoal {
		t.Errorf("after the agent's link = %s, want %s", got, GroundAgentGoal)
	}
	for _, x := range o.Links {
		if x.LinkID == l.ID && (x.Basis != linkBasisAgent || strings.Contains(x.Note, "guess")) {
			t.Errorf("the agent's link still reads as a guess: %+v", x)
		}
	}
}
