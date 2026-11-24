package core

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"
)

func (e *env) goals(a *Actor, project string) *GoalsOverview {
	e.t.Helper()
	if _, err := e.s.RecomputeGoalTraces(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	o, err := e.s.ListGoals(e.ctx, a, project)
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func (e *env) grounding(key string) string {
	e.t.Helper()
	tr, err := e.s.Trace(e.ctx, e.admin, TraceQuery{Ref: key})
	if err != nil {
		e.t.Fatal(err)
	}
	return tr.Grounding
}

func (e *env) goal(a *Actor, project, title string) *DesignNote {
	e.t.Helper()
	n, err := e.s.CreateDesign(e.ctx, a, DesignInput{Title: &title, Kind: str("goal"), Project: &project})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) serve(a *Actor, key string, goal int64) *Link {
	e.t.Helper()
	l, err := e.s.AddItemLink(e.ctx, a, LinkInput{From: LinkEnd{Type: KIssue, Key: key}, To: LinkEnd{Type: KDesign, ID: goal}, Rel: "serves"})
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// Goals and the trace from work to them (WP-382, WP-ADR50).
func TestGoalTraces(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"

	// No goals yet: everything reads as it is, and the view asks for one.
	o := e.goals(e.admin, p.Key)
	if o.Scoped || len(o.Goals) != 0 || len(o.Ask) == 0 || !strings.Contains(o.Ask[0], "What should Alpha do") {
		t.Fatalf("empty view = %+v", o)
	}

	search := e.goal(e.admin, p.Key, "People find any task in a second")
	if !search.Approved {
		t.Fatal("a person's goal is approved as it is saved")
	}
	export := e.goal(e.admin, p.Key, "People take their data with them")
	mine := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Make task search instant"})
	sub := e.issue(&agent, IssueInput{Project: p.Key, Title: "Index titles for search", Parent: mine.Key})
	stray := e.issue(&agent, IssueInput{Project: p.Key, Title: "Rename the sidebar colours"})
	own := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix the login page typo"})

	// Waypoint guesses the search work serves the search goal; the rest it leaves.
	e.goals(e.admin, p.Key)
	for key, want := range map[string]string{mine.Key: GroundInferredGoal, sub.Key: GroundInferredGoal, stray.Key: GroundNone, own.Key: GroundPersonRequest} {
		if got := e.grounding(key); got != want {
			t.Errorf("%s before links = %s, want %s", key, got, want)
		}
	}

	// A person's link (here, the same as the guess) grounds the task and, through the parent, the agent's subtask.
	e.serve(e.admin, mine.Key, search.ID)
	o = e.goals(e.admin, p.Key)
	if got := e.grounding(mine.Key); got != GroundPersonGoal {
		t.Errorf("linked task = %s", got)
	}
	if got := e.grounding(sub.Key); got != GroundAgentGoal {
		t.Errorf("agent's subtask = %s, want agent_goal (an agent put it there)", got)
	}
	tr, err := e.s.Trace(e.ctx, e.admin, TraceQuery{Ref: sub.Key})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Routes) == 0 || tr.Routes[0].Goal.ID != search.ID || len(tr.Routes[0].Hops) != 2 || tr.Routes[0].Hops[0].Basis != "agent" || tr.Routes[0].Hops[1].Basis != "person" {
		t.Fatalf("subtask routes = %+v", tr.Routes)
	}
	if tr.Person || tr.PersonHops == nil || *tr.PersonHops != 1 || len(tr.PersonRoute) != 1 || tr.PersonRoute[0].To.Key != mine.Key {
		t.Errorf("person distance = %v %v %+v", tr.Person, tr.PersonHops, tr.PersonRoute)
	}
	var cov *GoalView
	for _, g := range o.Goals {
		if g.Note.ID == search.ID {
			cov = g
		}
	}
	if cov == nil || cov.Coverage.Open != 2 || cov.Coverage.ByGrounding[GroundPersonGoal] != 1 || cov.Coverage.ByGrounding[GroundAgentGoal] != 1 {
		t.Errorf("coverage = %+v", cov)
	}

	// The agent's untraced task is a medium risk; the person's own isn't.
	r, err := e.s.RiskOf(e.ctx, e.admin, "issue", stray.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(r.Reasons, func(x RiskReason) bool { return x.Factor == riskNoGoal }) {
		t.Errorf("stray risk = %+v", r)
	}
	if r, _ := e.s.RiskOf(e.ctx, e.admin, "issue", own.Key, ""); r != nil && slices.ContainsFunc(r.Reasons, func(x RiskReason) bool { return x.Factor == riskNoGoal }) {
		t.Errorf("a person's own task gets no_goal: %+v", r)
	}
	if got := e.search(e.admin, "risk:medium"); !slices.Contains(got, stray.Key) {
		t.Errorf("risk:medium = %v, want %s", got, stray.Key)
	}
	if got := e.search(e.admin, "goal:none"); !slices.Contains(got, stray.Key) || slices.Contains(got, mine.Key) {
		t.Errorf("goal:none = %v", got)
	}
	if got := e.search(e.admin, "goal:people"); !slices.Equal(got, []string{mine.Key}) {
		t.Errorf("goal:people = %v", got)
	}
	if got := e.search(e.admin, "goal:"+itoa64(search.ID)); len(got) != 2 {
		t.Errorf("goal:N = %v", got)
	}
	if _, err := e.s.Search(e.ctx, e.admin, SearchParams{Query: "goal:sideways"}); err == nil {
		t.Error("goal:sideways was accepted")
	}
	found := false
	for _, u := range o.Untraced {
		found = found || u.Item.Key == stray.Key && u.Grounding == GroundNone && u.Agent == agent.Agent
	}
	if !found || o.UntracedAll < 2 {
		t.Errorf("untraced = %d %+v", o.UntracedAll, o.Untraced)
	}

	// An agent's link counts as an agent's until a person confirms it.
	l := e.serve(&agent, own.Key, export.ID)
	e.goals(e.admin, p.Key)
	if got := e.grounding(own.Key); got != GroundAgentGoal {
		t.Errorf("agent-linked = %s", got)
	}
	if _, err := e.s.ConfirmGoalLink(e.ctx, &agent, l.ID); err == nil {
		t.Error("an agent confirmed a link")
	}
	cl, err := e.s.ConfirmGoalLink(e.ctx, e.admin, l.ID)
	if err != nil || cl.Basis != "person" || cl.ConfirmedBy == nil {
		t.Fatalf("confirm = %+v %v", cl, err)
	}
	o = e.goals(e.admin, p.Key)
	if got := e.grounding(own.Key); got != GroundPersonGoal {
		t.Errorf("confirmed = %s", got)
	}
	if slices.ContainsFunc(o.Links, func(x GoalLink) bool { return x.LinkID == l.ID }) {
		t.Error("a confirmed link still waits for a person")
	}
	// An agent can't take back a link a person confirmed.
	if err := e.s.RemoveItemLink(e.ctx, &agent, l.ID); err == nil {
		t.Error("an agent removed a confirmed link")
	}
}

// Waypoint guesses the goal of plainly matching work, a person confirms or
// dismisses the guess, and a dismissed guess is never made again.
func TestGoalMatcher(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Beta", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	invoices := e.goal(e.admin, p.Key, "Customers get correct invoices every month")
	e.goal(e.admin, p.Key, "The team sees deploys and outages at a glance")
	for _, title := range []string{"Write the onboarding guide", "Tidy the settings page", "Plan the quarterly offsite", "Upgrade the mail templates"} {
		e.issue(e.admin, IssueInput{Project: p.Key, Title: title})
	}
	guess := e.issue(&agent, IssueInput{Project: p.Key, Title: "Invoices show the wrong month for customers", Description: "Monthly invoices for some customers carry last month's date."})
	vague := e.issue(&agent, IssueInput{Project: p.Key, Title: "Rename the sidebar colours"})

	o := e.goals(e.admin, p.Key)
	if got := e.grounding(guess.Key); got != GroundInferredGoal {
		t.Fatalf("matching work = %s, want inferred_goal", got)
	}
	if got := e.grounding(vague.Key); got != GroundNone {
		t.Errorf("unrelated work = %s, want none", got)
	}
	var gl *GoalLink
	for i := range o.Links {
		if o.Links[i].Item.Key == guess.Key {
			gl = &o.Links[i]
		}
	}
	if gl == nil || gl.Basis != "inferred" || gl.Goal.ID != invoices.ID || gl.Score == nil || !strings.Contains(gl.Note, "invoice") {
		t.Fatalf("guess = %+v (links %+v)", gl, o.Links)
	}
	// An agent can't drop Waypoint's guess; a person dismisses it.
	if err := e.s.RemoveItemLink(e.ctx, &agent, gl.LinkID); err == nil {
		t.Error("an agent removed a guess")
	}
	if err := e.s.DismissGoalLink(e.ctx, e.admin, gl.LinkID); err != nil {
		t.Fatal(err)
	}
	// The no keeps the guess as it was: its score, basis and age.
	var score sql.NullFloat64
	var basis string
	var madeAt sql.NullInt64
	if err := e.s.db.R.QueryRow(`SELECT link_score, link_basis, link_created_at FROM goal_dismissals WHERE src_id = ? AND goal_id = ?`, guess.ID, invoices.ID).Scan(&score, &basis, &madeAt); err != nil {
		t.Fatal(err)
	}
	if !score.Valid || *gl.Score != score.Float64 || basis != "inferred" || !madeAt.Valid || madeAt.Int64 <= 0 {
		t.Errorf("dismissal kept score=%v basis=%q created_at=%v; the guess had score %v", score, basis, madeAt, *gl.Score)
	}
	e.goals(e.admin, p.Key)
	if got := e.grounding(guess.Key); got != GroundNone {
		t.Errorf("after dismissal = %s; the guess came back", got)
	}
	tr, _ := e.s.Trace(e.ctx, e.admin, TraceQuery{Ref: guess.Key})
	if tr.Candidate != nil && tr.Candidate.Goal.ID == invoices.ID {
		t.Errorf("a dismissed goal is still offered: %+v", tr.Candidate)
	}
}

func TestGoalWording(t *testing.T) {
	for title, want := range map[string]string{
		"A performant task tracker":                   "",
		"Use Django, Postgres and Redis":              "Postgres",
		"Switch to Kubernetes for deploys":            "says how",
		"Move internal/core/goals.go into a package":  "names a file",
		"Speed up ListGoals":                          "names code",
		"Ship v2.1 before the launch":                 "names a version",
		"People can go anywhere in the app in a step": "",
	} {
		got := strings.Join(goalWording(title), " | ")
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%q = %q, want %q", title, got, want)
		}
	}
}

// The weekly check tells the owners how the week traced, once a week.
func TestGoalDrift(t *testing.T) {
	e := newEnv(t)
	e.setClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	p := e.project(e.admin, "Gamma", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	e.goal(e.admin, p.Key, "Readers find the answer on the first page")
	e.issue(&agent, IssueInput{Project: p.Key, Title: "Rename the sidebar colours"})
	e.goals(e.admin, p.Key)
	count := func() int {
		var n int
		if err := e.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE kind = ?`, NotifyGoals).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.s.maybeGoalDrift(e.ctx)
	if count() != 1 {
		t.Fatalf("notices = %d, want 1", count())
	}
	var body string
	_ = e.s.db.R.QueryRow(`SELECT body FROM notifications WHERE kind = ?`, NotifyGoals).Scan(&body)
	if !strings.Contains(body, "1 open task traces to no approved goal") || !strings.Contains(body, "no person asked for") {
		t.Errorf("body = %q", body)
	}
	e.s.maybeGoalDrift(e.ctx)
	if count() != 1 {
		t.Error("the check ran twice in a week")
	}
	e.setClock(e.clockNow().Add(8 * 24 * time.Hour))
	e.s.maybeGoalDrift(e.ctx)
	if count() != 2 {
		t.Error("the check didn't run a week later")
	}
}

// WP-1790: a request-written note stays editable by agents, so who put it
// under its parent is whoever last did, not the person who filed the request.
func TestGoalTraceNoteParentBasis(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	a := e.goal(e.admin, p.Key, "People find any task in a second")
	b := e.goal(e.admin, p.Key, "People take their data with them")
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Pick the export format"})

	note, err := e.s.CreateDesign(e.ctx, &agent, DesignInput{Title: str("Export as CSV"), Kind: str("decision"), Body: str("CSV."),
		Project: &p.Key, Parent: Opt[int64]{Set: true, Value: &a.ID}, For: task.Key})
	if err != nil || note.Requested == nil {
		t.Fatalf("create = %+v, %v", note, err)
	}
	underBasis := func(parent int64) string {
		t.Helper()
		e.goals(e.admin, p.Key)
		tr, err := e.s.Trace(e.ctx, e.admin, TraceQuery{Ref: itoa64(note.ID)})
		if err != nil {
			t.Fatal(err)
		}
		// The relation graph's under edge says the same as the trace: the
		// graph page shows its basis too, "under (by a person)". Read afresh:
		// the test clock stands still, so the cached graph's signature
		// wouldn't see the move.
		g, err := loadRelGraph(e.ctx, e.s.db.R)
		if err != nil {
			t.Fatal(err)
		}
		graphBasis := ""
		from, _ := g.node(KDesign, note.ID)
		to, _ := g.node(KDesign, parent)
		for _, ed := range g.edges {
			if ed.kind == "under" && ed.a == from && ed.b == to {
				graphBasis = basisNames[ed.basis]
			}
		}
		for _, r := range tr.Routes {
			for _, h := range r.Hops {
				if h.Kind == "under" && h.To.ID == parent {
					if graphBasis != h.Basis {
						t.Errorf("under %d: trace basis %s, relation graph %q", parent, h.Basis, graphBasis)
					}
					return h.Basis
				}
			}
		}
		t.Fatalf("no route under %d: %+v", parent, tr.Routes)
		return ""
	}
	if got := underBasis(a.ID); got != "agent" {
		t.Errorf("placed by an agent at creation: basis = %s, want agent", got)
	}
	if _, err := e.s.UpdateDesign(e.ctx, &agent, note.ID, DesignInput{Parent: Opt[int64]{Set: true, Value: &b.ID}}); err != nil {
		t.Fatal(err)
	}
	if got := underBasis(b.ID); got != "agent" {
		t.Errorf("moved by an agent: basis = %s, want agent", got)
	}
	if _, err := e.s.UpdateDesign(e.ctx, e.admin, note.ID, DesignInput{Parent: Opt[int64]{Set: true, Value: &a.ID}}); err != nil {
		t.Fatal(err)
	}
	if got := underBasis(a.ID); got != "person" {
		t.Errorf("moved by a person: basis = %s, want person", got)
	}

	// A proposal's place counts as a person's once they approve the text.
	prop, err := e.s.CreateDesign(e.ctx, &agent, DesignInput{Title: str("Export as XLSX"), Kind: str("decision"), Body: str("XLSX."),
		Project: &p.Key, Parent: Opt[int64]{Set: true, Value: &b.ID}})
	if err != nil {
		t.Fatal(err)
	}
	note = prop
	if got := underBasis(b.ID); got != "agent" {
		t.Errorf("proposal before approval: basis = %s, want agent", got)
	}
	if _, err := e.s.ApproveDesign(e.ctx, e.admin, prop.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := underBasis(b.ID); got != "person" {
		t.Errorf("proposal after approval: basis = %s, want person", got)
	}
}
