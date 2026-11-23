package core

import (
	"strings"
	"testing"
)

// The other ways a relation is removed, beside RemoveRelation, don't let an
// editor remove or learn of a relation to a task they can't see.
//
// Replace (relate): the actor names both ends and each is opened for them
// first (editor of one, viewer of the other), so a task they can't see
// answers as a missing key does and nothing is replaced or dismissed.
func TestRelateReplacePathNeverTouchesAHiddenTask(t *testing.T) {
	e := newEnv(t)
	ed := e.user("edie", false)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	if err := e.s.SetMember(e.ctx, e.admin, a.Key, "edie", "editor"); err != nil {
		t.Fatal(err)
	}
	x := e.issue(e.admin, IssueInput{Project: a.Key, Title: "in alpha"})
	y := e.issue(e.admin, IssueInput{Project: b.Key, Title: "secret in beta"})
	// Waypoint's inferred relation between them, and its evidence.
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO issue_relations(source_id, target_id, kind, created_at, found, basis) VALUES (?, ?, 'relates', 1, ?, 'inferred')`, x.ID, y.ID, rewriteFound); err != nil {
		t.Fatal(err)
	}
	_, hidden := e.s.Relate(e.ctx, ed, x.Key, RelationInput{Kind: "supersedes", Issue: y.Key})
	_, missing := e.s.Relate(e.ctx, ed, x.Key, RelationInput{Kind: "supersedes", Issue: "BETA-999"})
	if hidden == nil || missing == nil || hidden.Error() == "" {
		t.Fatalf("relating to a hidden task: %v, to a missing one: %v", hidden, missing)
	}
	if strings.ReplaceAll(hidden.Error(), y.Key, "K") != strings.ReplaceAll(missing.Error(), "BETA-999", "K") {
		t.Errorf("hidden answers %q, missing %q: an oracle", hidden, missing)
	}
	var rels, dismissed, events int
	_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT (SELECT count(*) FROM issue_relations), (SELECT count(*) FROM rewrite_dismissals)`).Scan(&rels, &dismissed)
	e.s.Wait()
	_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM events WHERE issue_id = ? AND field = 'relation'`, x.ID).Scan(&events)
	if rels != 1 || dismissed != 0 || events != 0 {
		t.Errorf("%d relations, %d dismissals, %d relation events: the hidden relation was touched", rels, dismissed, events)
	}
}

// Text (dropUnnamedRelations): an editor can't make a link to a task they
// can't see by naming it, and when a link the text made goes because they
// stopped naming the task, its removal records only what its making put in
// the same history already.
func TestNamedRelationPathNeverMakesOrNamesAHiddenTask(t *testing.T) {
	e := newEnv(t)
	ed := e.user("edie", false)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	if err := e.s.SetMember(e.ctx, e.admin, a.Key, "edie", "editor"); err != nil {
		t.Fatal(err)
	}
	x := e.issue(e.admin, IssueInput{Project: a.Key, Title: "in alpha"})
	y := e.issue(e.admin, IssueInput{Project: b.Key, Title: "secret in beta"})
	save := func(who *Actor, text string) {
		t.Helper()
		if _, err := e.s.UpdateIssue(e.ctx, who, x.Key, IssuePatch{Description: &text}); err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
	}
	rows := func() (n int) {
		_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM issue_relations`).Scan(&n)
		return n
	}
	naming := func() (n int) {
		_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM events WHERE issue_id = ? AND field = 'relation' AND (old_value LIKE ? OR new_value LIKE ? OR coalesce(data, '') LIKE ?)`,
			x.ID, "%"+y.Key+"%", "%"+y.Key+"%", "%"+y.Key+"%").Scan(&n)
		return n
	}
	save(ed, "see "+y.Key)
	if rows() != 0 || naming() != 0 {
		t.Fatalf("an editor's text made %d relations, %d events naming %s, to a task they can't see", rows(), naming(), y.Key)
	}
	// Someone who sees both names it (anew: a link is made for a key the
	// save newly names); the link and its event are made.
	save(ed, "nothing named yet")
	save(e.admin, "see "+y.Key)
	if rows() != 1 || naming() != 1 {
		t.Fatalf("setup: %d relations, %d events naming %s", rows(), naming(), y.Key)
	}
	// The editor stops naming it: the link goes with the text, and the
	// history gains the removal, naming what the making already named.
	save(ed, "nothing named")
	if rows() != 0 {
		t.Errorf("the link outlived the text: %d relations", rows())
	}
	if naming() != 2 {
		t.Errorf("%d events name %s, want the making and the removal", naming(), y.Key)
	}
}
