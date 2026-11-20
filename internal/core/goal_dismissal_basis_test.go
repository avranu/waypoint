package core

import (
	"database/sql"
	"testing"
	"time"
)

type keptDismissal struct {
	score     sql.NullFloat64
	basis     string
	by        sql.NullInt64
	agent     string
	createdAt sql.NullInt64
}

func (e *env) keptDismissal(src, goal int64) keptDismissal {
	e.t.Helper()
	var k keptDismissal
	if err := e.s.db.R.QueryRow(`SELECT link_score, link_basis, link_by, link_agent, link_created_at FROM goal_dismissals WHERE src_id = ? AND goal_id = ?`, src, goal).
		Scan(&k.score, &k.basis, &k.by, &k.agent, &k.createdAt); err != nil {
		e.t.Fatal(err)
	}
	return k
}

// A person's no keeps the link it was about: who made it and when (WP-999).
func TestDismissedAgentLinkKeepsItsAuthor(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Delta", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	goal := e.goal(e.admin, p.Key, "Readers trust every number")
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix the login page typo"})
	l := e.serve(&agent, task.Key, goal.ID)
	if err := e.s.DismissGoalLink(e.ctx, e.admin, l.ID); err != nil {
		t.Fatal(err)
	}
	k := e.keptDismissal(task.ID, goal.ID)
	if k.agent == "" || !k.createdAt.Valid || k.createdAt.Int64 <= 0 {
		t.Errorf("kept %+v: want the agent and when it linked", k)
	}
}

// An agent's [[wikilink]] stays in its text: the no keeps the link row's data.
func TestDismissedWikilinkKeepsItsAuthor(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Delta", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	goal := e.goal(e.admin, p.Key, "Readers trust every number")
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix the login page typo"})
	if _, err := e.s.AddComment(e.ctx, &agent, task.Key, "This serves [["+goal.Title+"]]."); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM knowledge_links WHERE auto = 1 AND dst_id = ?`, goal.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DismissGoalLink(e.ctx, e.admin, id); err != nil {
		t.Fatal(err)
	}
	k := e.keptDismissal(task.ID, goal.ID)
	if k.agent == "" || !k.createdAt.Valid {
		t.Errorf("kept %+v: want the agent that wrote the wikilink", k)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM knowledge_links WHERE id = ?`, id).Scan(&n); err != nil || n != 1 {
		t.Errorf("the wikilink row should stay (n=%d, err=%v)", n, err)
	}
}

// A suggestion dismissed has no link to keep.
func TestDismissedSuggestionKeepsNoLink(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Delta", "")
	goal := e.goal(e.admin, p.Key, "Readers trust every number")
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix the login page typo"})
	if err := e.s.DismissGoalMatch(e.ctx, e.admin, LinkEnd{Type: KIssue, Key: task.Key}, goal.ID); err != nil {
		t.Fatal(err)
	}
	k := e.keptDismissal(task.ID, goal.ID)
	if k.score.Valid || k.createdAt.Valid || k.basis != "" {
		t.Errorf("kept %+v for a suggestion", k)
	}
}

// An agent stating a link Waypoint only guessed takes it over, but the
// matcher's score stays on the row.
func TestAgentRestatingAGuessKeepsItsScore(t *testing.T) {
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
	e.goals(e.admin, p.Key)
	var score sql.NullFloat64
	if err := e.s.db.R.QueryRow(`SELECT score FROM knowledge_links WHERE src_id = ? AND dst_id = ? AND basis = 'inferred'`, guess.ID, invoices.ID).Scan(&score); err != nil || !score.Valid {
		t.Fatalf("no scored guess (score=%v, err=%v)", score, err)
	}
	e.serve(&agent, guess.Key, invoices.ID)
	var after sql.NullFloat64
	var basis, by string
	if err := e.s.db.R.QueryRow(`SELECT score, basis, created_agent FROM knowledge_links WHERE src_id = ? AND dst_id = ?`, guess.ID, invoices.ID).Scan(&after, &basis, &by); err != nil {
		t.Fatal(err)
	}
	if basis == "inferred" || by == "" {
		t.Fatalf("the agent didn't take the link over: basis=%q agent=%q", basis, by)
	}
	if !after.Valid || after.Float64 != score.Float64 {
		t.Errorf("score after the agent restated it = %v, was %v", after, score)
	}
}

// A no said to a suggestion before the link itself is dismissed takes on the
// link's data, and keeps its first person and time.
func TestDismissingTheLinkFillsAnEarlierSuggestionNo(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Delta", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	goal := e.goal(e.admin, p.Key, "Readers trust every number")
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix the login page typo"})
	l := e.serve(&agent, task.Key, goal.ID)
	if err := e.s.DismissGoalMatch(e.ctx, e.admin, LinkEnd{Type: KIssue, Key: task.Key}, goal.ID); err != nil {
		t.Fatal(err)
	}
	if k := e.keptDismissal(task.ID, goal.ID); k.createdAt.Valid {
		t.Fatalf("a suggestion's no holds a link: %+v", k)
	}
	var first int64
	if err := e.s.db.R.QueryRow(`SELECT created_at FROM goal_dismissals WHERE src_id = ? AND goal_id = ?`, task.ID, goal.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(time.Hour))
	if err := e.s.DismissGoalLink(e.ctx, e.admin, l.ID); err != nil {
		t.Fatal(err)
	}
	k := e.keptDismissal(task.ID, goal.ID)
	if k.agent == "" || !k.createdAt.Valid {
		t.Errorf("the link's data wasn't filled in: %+v", k)
	}
	var again int64
	if err := e.s.db.R.QueryRow(`SELECT created_at FROM goal_dismissals WHERE src_id = ? AND goal_id = ?`, task.ID, goal.ID).Scan(&again); err != nil || again != first {
		t.Errorf("the first no's time changed: %d, was %d (%v)", again, first, err)
	}
}

// The score a restated guess keeps is verification data: the link list and the
// goal overview show it only on a guess, not on an agent's claim (WP-999).
func TestRestatedGuessShowsNoScore(t *testing.T) {
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
	o := e.goals(e.admin, p.Key)
	scored := func(o *GoalsOverview) bool {
		for _, l := range o.Links {
			if l.Item.Key == guess.Key && l.Score != nil {
				return true
			}
		}
		return false
	}
	if !scored(o) {
		t.Fatalf("a guess should show its score in the overview: %+v", o.Links)
	}
	e.serve(&agent, guess.Key, invoices.ID)
	o = e.goals(e.admin, p.Key)
	found := false
	for _, l := range o.Links {
		found = found || l.Item.Key == guess.Key
	}
	if !found || scored(o) {
		t.Errorf("overview after the agent restated it (listed %v): score still shown: %+v", found, o.Links)
	}
	links, err := e.s.ItemLinks(e.ctx, e.admin, LinkEnd{Type: KIssue, Key: guess.Key})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, l := range links {
		if l.Rel == "serves" {
			seen = true
			if l.Score != nil {
				t.Errorf("link list shows score %v on an agent's claim", *l.Score)
			}
		}
	}
	if !seen {
		t.Fatalf("no serves link listed: %+v", links)
	}
	var kept sql.NullFloat64
	if err := e.s.db.R.QueryRow(`SELECT score FROM knowledge_links WHERE src_id = ? AND dst_id = ?`, guess.ID, invoices.ID).Scan(&kept); err != nil || !kept.Valid {
		t.Errorf("the row lost its score (score=%v, err=%v)", kept, err)
	}
}
