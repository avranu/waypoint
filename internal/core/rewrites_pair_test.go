package core

import (
	"testing"
	"time"
)

// bigPR is a pull request whose lines clear the bar, merged a day ago.
func (f *rewriteFixture) bigPR(n int) []RewrittenPR {
	return []RewrittenPR{{Number: n, Lines: 80, Share: 0.5, At: f.s.now().Add(-24 * time.Hour).UnixMilli()}}
}

// A person removes the inferred relation from one task's side: the other
// task's next ship doesn't make it again (the dismissal is of the pair).
func TestDismissalHoldsFromTheOtherSide(t *testing.T) {
	f := newRewriteFixture(t)
	f.link(t, f.b.ID, 500)
	if got, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", f.prs); err != nil || len(got) != 1 {
		t.Fatalf("b's ship: %+v, %v", got, err)
	}
	// Removed from a's side.
	rels := f.relations(t, f.a.Key)
	if len(rels) != 1 {
		t.Fatalf("a's relations = %+v", rels)
	}
	if err := f.s.RemoveRelation(f.ctx, f.admin, f.a.Key, rels[0].ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.NoteRewrites(f.ctx, f.admin, f.a.Key, "o/r", "h", f.bigPR(500))
	if err != nil || len(got) != 0 {
		t.Errorf("a's ship named %+v, %v", got, err)
	}
	if rels := f.relations(t, f.a.Key); len(rels) != 0 {
		t.Errorf("the removed relation came back from the other side: %+v", rels)
	}
	// And the read (RewrittenWork) leaves it out too.
	if read, err := f.s.RewrittenWork(f.ctx, f.admin, f.a.Key, "o/r", f.bigPR(500)); err != nil || len(read) != 0 {
		t.Errorf("RewrittenWork read %+v, %v", read, err)
	}
}

func (f *rewriteFixture) relationRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.R.QueryRowContext(f.ctx, `SELECT count(*) FROM issue_relations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A person relates the pair after the ship read the work and before it
// recorded it, from either end: that one relation is the only one, and
// Waypoint's inferred one isn't made beside it, nor evidence kept for it.
func TestRecordRewritesDefersToARelationMadeSince(t *testing.T) {
	for _, from := range []string{"the task shipped", "the other task"} {
		f := newRewriteFixture(t)
		d := f.get(f.admin, f.b.Key)
		read, err := f.s.RewrittenWork(f.ctx, f.admin, f.b.Key, "o/r", f.prs)
		if err != nil || len(read) != 1 {
			t.Fatalf("read %+v, %v", read, err)
		}
		a, b := f.a.Key, f.b.Key
		if from == "the task shipped" {
			a, b = b, a
		}
		if err := f.s.AddRelation(f.ctx, f.admin, a, "relates", b); err != nil {
			t.Fatal(err)
		}
		got, err := f.s.recordRewrites(f.ctx, f.admin, d, "o/r", "h", read)
		if err != nil {
			t.Fatalf("%s: %v", from, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: recorded %+v beside a person's relation", from, got)
		}
		if n := f.relationRows(t); n != 1 {
			t.Errorf("%s: %d relations, want the person's alone", from, n)
		}
		var found string
		if err := f.s.db.R.QueryRowContext(f.ctx, `SELECT coalesce(found, '') FROM issue_relations`).Scan(&found); err != nil || found != "" {
			t.Errorf("%s: surviving relation found = %q (%v), want the person's", from, found, err)
		}
		var ev int
		_ = f.s.db.R.QueryRowContext(f.ctx, `SELECT count(*) FROM rewrite_evidence`).Scan(&ev)
		if ev != 0 {
			t.Errorf("%s: %d evidence rows for a pair a person related", from, ev)
		}
	}
}
