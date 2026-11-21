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
