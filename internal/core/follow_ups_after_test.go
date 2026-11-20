package core

import (
	"testing"
	"time"
)

// WP-1003: relating a task to one that was done before it was filed makes
// it a follow-up, found "after", whichever end someone relates from. Not
// when the other wasn't done first, for a sub-task, or for a link nobody
// chose (Waypoint's own matcher).
func TestFollowUpAfterDone(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "After", "")
	done := "Done"
	finish := func(key string) {
		t.Helper()
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, key, IssuePatch{Status: &done}); err != nil {
			t.Fatal(err)
		}
	}
	tick := func() { e.setClock(e.clockNow().Add(time.Minute)) }
	find := func(key, other string) *Relation {
		t.Helper()
		var found *Relation
		for _, r := range e.get(e.admin, key).Relations {
			if r.Issue.Key == other {
				if found != nil {
					t.Errorf("%s has two relations to %s", key, other)
				}
				found = &r
			}
		}
		return found
	}
	relate := func(a *Actor, key, kind, other string) *RelationMade {
		t.Helper()
		made, err := e.s.Relate(e.ctx, a, key, RelationInput{Kind: kind, Issue: other})
		if err != nil {
			t.Fatal(err)
		}
		return made
	}

	first := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Export to CSV"})
	open := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Export to JSON"})
	tick()
	finish(first.Key)
	tick()
	later := e.issue(e.admin, IssueInput{Project: p.Key, Title: "CSV export drops quotes"})

	// From the later task: it follows the earlier one.
	if made := relate(e.admin, later.Key, "relates", first.Key); made.Kind != "follows" || made.Found != followFoundAfter {
		t.Errorf("made = %+v, want follows/after", made)
	}
	if r := find(later.Key, first.Key); r == nil || r.Kind != "follows" || r.Found != followFoundAfter || r.Basis != "person" {
		t.Errorf("later's relation = %+v, want follows, found after, basis person", r)
	}
	if r := find(first.Key, later.Key); r == nil || r.Kind != "followed_by" {
		t.Errorf("first's relation = %+v, want followed_by", r)
	}
	// Saying it again, from either end, keeps the one follow-up and says so.
	if made := relate(e.admin, later.Key, "relates", first.Key); made.Kind != "follows" || made.Found != followFoundAfter {
		t.Errorf("again from later: made = %+v, want follows/after", made)
	}
	if made := relate(e.admin, first.Key, "relates", later.Key); made.Kind != "followed_by" || made.Found != followFoundAfter {
		t.Errorf("again from first: made = %+v, want followed_by/after", made)
	}
	var rows int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM issue_relations WHERE (source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?)`,
		first.ID, later.ID, later.ID, first.ID).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("relations between the pair = %d (%v), want 1", rows, err)
	}

	// From the earlier task, by an agent: the later one still follows it.
	again := e.issue(e.admin, IssueInput{Project: p.Key, Title: "CSV export: add a header row"})
	if made := relate(asAgent(e.admin, "anthropic/claude-opus-5-5"), first.Key, "relates", again.Key); made.Kind != "followed_by" {
		t.Errorf("made = %+v, want followed_by", made)
	}
	if r := find(again.Key, first.Key); r == nil || r.Kind != "follows" || r.Found != followFoundAfter || r.Basis != "agent" {
		t.Errorf("again's relation = %+v, want follows, found after, basis agent", r)
	}

	// The earlier task wasn't done when this was filed (it was open, and
	// done only after): a plain relation.
	tick()
	finish(open.Key)
	if made := relate(e.admin, later.Key, "relates", open.Key); made.Kind != "relates" || made.Found != "" {
		t.Errorf("not done first: made = %+v", made)
	}
	// Other kinds stay what was said.
	if made := relate(e.admin, later.Key, "blocked_by", first.Key); made.Kind != "blocked_by" {
		t.Errorf("blocked_by: made = %+v", made)
	}

	// A sub-task filed under a done task is a slice of it.
	tick()
	slice := e.issue(e.admin, IssueInput{Project: p.Key, Title: "CSV export: escape commas", Parent: first.Key})
	if made := relate(e.admin, slice.Key, "relates", first.Key); made.Kind != "relates" {
		t.Errorf("sub-task: made = %+v", made)
	}

	// Waypoint's own matcher chose nothing: a plain relation.
	guess := e.issue(e.admin, IssueInput{Project: p.Key, Title: "CSV import"})
	if made := relate(aiActorNamed("Auto relate", p.ID, ""), guess.Key, "relates", first.Key); made.Kind != "relates" {
		t.Errorf("auto-relate: made = %+v", made)
	}

	// A pair that already has a follow-up, either way, keeps it: no second
	// follow-up pointing back.
	back := e.issue(e.admin, IssueInput{Project: p.Key, Title: "CSV export docs"})
	relate(e.admin, first.Key, "follows", back.Key) // against the order
	if made := relate(e.admin, back.Key, "relates", first.Key); made.Found != "" {
		t.Errorf("already following: made = %+v", made)
	}
	var follows int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM issue_relations WHERE kind = 'follows' AND ((source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?))`,
		first.ID, back.ID, back.ID, first.ID).Scan(&follows); err != nil || follows != 1 {
		t.Errorf("follows between the pair = %d (%v), want 1", follows, err)
	}

	// A plain relation made earlier gives way to the follow-up.
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Reports page"})
	relate(e.admin, other.Key, "relates", later.Key) // neither done: plain
	tick()
	finish(later.Key)
	tick()
	next := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Quote every CSV field"})
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO issue_relations(source_id, target_id, kind, created_at) VALUES (?,?,'relates',0)`, later.ID, next.ID); err != nil {
		t.Fatal(err)
	}
	relate(e.admin, next.Key, "relates", later.Key)
	if r := find(next.Key, later.Key); r == nil || r.Kind != "follows" {
		t.Errorf("next's relation = %+v, want follows alone", r)
	}
}
