package core

import (
	"strings"
	"testing"
)

// A decision an agent writes for a task a person filed counts on that
// person's request; one for its own task stays a proposal (WP-189). Nobody
// approved it, so the person is never recorded as its approver or as the
// reviewer of the agent's changes (WP-409).
func TestDesignRequestedByAPerson(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "")
	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"

	mine := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Build the export"})
	theirs := e.issue(&agent, IssueInput{Project: p.Key, Title: "Tidy the export"})
	write := func(title, forTask string) *DesignNote {
		t.Helper()
		n, err := e.s.CreateDesign(e.ctx, &agent, DesignInput{Title: &title, Kind: str("decision"), Body: str("Because."), Project: &p.Key, For: forTask})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		return n
	}

	n := write("Export format", mine.Key)
	if !n.Approved || n.Requested == nil || n.Requested.Issue != mine.Key || n.Requested.By == nil || n.Requested.By.ID != e.admin.User.ID {
		t.Fatalf("requested note = %+v", n)
	}
	if n.CreatedAgent != agent.Agent || n.ApprovedBy != nil || !strings.Contains(n.Notice, "Saved for "+mine.Key) || strings.Contains(n.Notice, "Accepted") {
		t.Errorf("provenance = %q %v %q", n.CreatedAgent, n.ApprovedBy, n.Notice)
	}
	if h := n.History; len(h) != 1 || h[0].ReviewedBy != nil || h[0].State != RevApplied || !strings.Contains(h[0].ReviewNote, "requested in "+mine.Key) {
		t.Errorf("first revision = %+v", h)
	}
	if !slicesAnyLink(n.Links, mine.Key) {
		t.Errorf("the task isn't linked: %+v", n.Links)
	}
	if q, err := e.s.DesignReview(e.ctx, e.admin, p.Key); err != nil || len(q) != 0 {
		t.Errorf("review queue = %+v %v", q, err)
	}

	for _, c := range []struct{ title, task string }{{"Own task", theirs.Key}, {"No task", ""}} {
		o := write(c.title, c.task)
		if o.Approved || o.Requested != nil || !strings.Contains(o.Notice, "proposal") {
			t.Errorf("%s = approved %v requested %v %q", c.title, o.Approved, o.Requested, o.Notice)
		}
	}
	if q, _ := e.s.DesignReview(e.ctx, e.admin, p.Key); len(q) != 2 {
		t.Errorf("review queue has %d, want the 2 proposals", len(q))
	}

	got, err := e.s.UpdateDesign(e.ctx, &agent, n.ID, DesignInput{Body: str("Because, and more.")})
	if err != nil || got.Body != "Because, and more." || got.Notice != "" {
		t.Fatalf("agent edit = %+v %v", got, err)
	}
	got, err = e.s.AddDesignAddition(e.ctx, &agent, n.ID, "Also this.")
	if err != nil || got.Pending != 0 || got.Notice != "" {
		t.Fatalf("agent addition = %+v %v", got, err)
	}
	if q, _ := e.s.DesignReview(e.ctx, e.admin, p.Key); len(q) != 2 {
		t.Errorf("review queue has %d after the agent's changes, want 2", len(q))
	}
	if h := got.History; len(h) == 0 || !strings.Contains(h[0].ReviewNote, "requested in "+mine.Key) {
		t.Errorf("history = %+v", h)
	}
	for _, r := range got.History {
		if r.Agent != "" && (r.State != RevApplied || r.ReviewedBy != nil || r.ReviewedAt != nil) {
			t.Errorf("agent revision recorded as a person's: %+v", r)
		}
	}
	if len(got.Additions) != 1 || got.Additions[0].State != RevApplied {
		t.Errorf("additions = %+v", got.Additions)
	}

	plain := e.admin
	own, err := e.s.CreateDesign(e.ctx, plain, DesignInput{Title: str("Person's"), Kind: str("decision"), Body: str("x"), Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	if got, err = e.s.UpdateDesign(e.ctx, &agent, own.ID, DesignInput{Body: str("y")}); err != nil || !strings.Contains(got.Notice, "proposed edit") {
		t.Errorf("edit of a person's note = %+v %v", got, err)
	}
}

func slicesAnyLink(links []*Link, key string) bool {
	for _, l := range links {
		if l.Item.Key == key || strings.Contains(l.Item.Title, key) || l.Item.Ref == key {
			return true
		}
	}
	return false
}
