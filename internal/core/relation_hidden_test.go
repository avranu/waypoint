package core

import (
	"strings"
	"testing"
)

// An editor of project A who can't see task B can't remove a relation to B:
// the answer is the one for a relation that doesn't exist, the relation
// stays, and nothing recorded in A names B.
func TestRemoveRelationToATaskTheActorCannotSee(t *testing.T) {
	e := newEnv(t)
	ed := e.user("edie", false)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	if err := e.s.SetMember(e.ctx, e.admin, a.Key, "edie", "editor"); err != nil {
		t.Fatal(err)
	}
	x := e.issue(e.admin, IssueInput{Project: a.Key, Title: "in alpha"})
	y := e.issue(e.admin, IssueInput{Project: b.Key, Title: "secret in beta"})
	res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO issue_relations(source_id, target_id, kind, created_at) VALUES (?, ?, 'relates', 1)`, x.ID, y.ID)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := res.LastInsertId()

	if d := e.get(ed, x.Key); len(d.Relations) != 0 {
		t.Fatalf("setup: the editor sees %d relations, want 0", len(d.Relations))
	}
	gone := e.s.RemoveRelation(e.ctx, ed, x.Key, rel+1000)
	err = e.s.RemoveRelation(e.ctx, ed, x.Key, rel)
	if err == nil {
		t.Fatalf("an editor removed a relation to %s, a task they can't see", y.Key)
	}
	if gone == nil || err.Error() != gone.Error() {
		t.Errorf("hidden relation answers %q, a missing one %q: an oracle", err, gone)
	}
	var n int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM issue_relations WHERE id = ?`, rel).Scan(&n); err != nil || n != 1 {
		t.Errorf("relation rows = %d, %v; want it kept", n, err)
	}
	e.s.Wait()
	for _, q := range []string{
		`SELECT COUNT(*) FROM events WHERE issue_id = ? AND (old_value LIKE ? OR COALESCE(data,'') LIKE ?)`,
	} {
		if err := e.s.db.R.QueryRowContext(e.ctx, q, x.ID, "%"+y.Key+"%", "%"+y.Key+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d events on %s name %s", n, x.Key, y.Key)
		}
	}
	hist, err := e.s.History(e.ctx, ed, x.Key, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range hist {
		if strings.Contains(ev.Old, y.Key) {
			t.Errorf("history shows %q", ev.Old)
		}
	}
}
