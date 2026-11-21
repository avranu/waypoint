package core

import (
	"math"
	"strings"
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

func (f *rewriteFixture) evidenceRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.R.QueryRowContext(f.ctx, `SELECT count(*) FROM rewrite_evidence`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A call that is malformed or partial deletes nothing; a well-formed one
// that names the repo and says the change replaces nothing replaces that
// repo's evidence with nothing.
func TestNoteRewritesEmptyAndMalformedCalls(t *testing.T) {
	f := newRewriteFixture(t)
	if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "abc1234", f.prs); err != nil {
		t.Fatal(err)
	}
	if f.evidenceRows(t) != 1 {
		t.Fatal("setup: no evidence")
	}
	for _, c := range []struct {
		name, repo, head string
		prs              []RewrittenPR
		invalid          bool
	}{
		{"blank repo, empty list", "", "abc1234", []RewrittenPR{}, true},
		{"blank repo, spaces", "  ", "abc1234", []RewrittenPR{}, true},
		{"no list at all", "o/r", "abc1234", nil, true},
		{"empty list, no commit", "o/r", "", []RewrittenPR{}, false},
		{"empty list, another repo", "o/other", "abc1234", []RewrittenPR{}, false},
	} {
		_, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, c.repo, c.head, c.prs)
		if c.invalid != IsInvalid(err) || (!c.invalid && err != nil) {
			t.Errorf("%s: err = %v, want invalid = %v", c.name, err, c.invalid)
		}
		if n := f.evidenceRows(t); n != 1 {
			t.Errorf("%s: %d evidence rows, want the one kept", c.name, n)
		}
	}
	// Well-formed, same repo, nothing replaced: the evidence goes, the relation stays.
	if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, " o/r ", "def5678", []RewrittenPR{}); err != nil {
		t.Fatal(err)
	}
	if n := f.evidenceRows(t); n != 0 {
		t.Errorf("%d evidence rows after a well-formed empty call", n)
	}
	if rels := f.relations(t, f.b.Key); len(rels) != 1 {
		t.Errorf("relations = %+v", rels)
	}
}

// A ship by someone who can't see a task doesn't drop the evidence another
// ship recorded for it.
func TestNoteRewritesKeepsEvidenceForTasksTheActorCannotSee(t *testing.T) {
	f := newRewriteFixture(t)
	if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "abc1234", f.prs); err != nil {
		t.Fatal(err)
	}
	// The task moves to a project the second shipper can't see.
	sam := f.user("sam", false)
	sp := f.project(sam, "Sam", "kanban")
	if _, err := f.s.db.W.ExecContext(f.ctx, `UPDATE issues SET project_id = ? WHERE id = ?`, sp.ID, f.a.ID); err != nil {
		t.Fatal(err)
	}
	lee := f.user("lee", false)
	if err := f.s.SetMember(f.ctx, f.admin, f.proj.Key, "lee", "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.NoteRewrites(f.ctx, lee, f.b.Key, "o/r", "def5678", []RewrittenPR{}); err != nil {
		t.Fatal(err)
	}
	if n := f.evidenceRows(t); n != 1 {
		t.Errorf("%d evidence rows: a shipper who can't see the task dropped its evidence", n)
	}
}

// Every string and count a call stores or reads is bounded at the boundary,
// and a call over a bound is refused whole: nothing is written.
func TestNoteRewritesBoundsItsInput(t *testing.T) {
	f := newRewriteFixture(t)
	long := strings.Repeat("a", 1<<20)
	many := make([]RewrittenPR, rewriteMaxPRs+1)
	for i := range many {
		many[i] = f.prs[0]
	}
	bad := func(p RewrittenPR, edit func(*RewrittenPR)) []RewrittenPR { edit(&p); return []RewrittenPR{p} }
	for _, c := range []struct {
		name, repo, head string
		prs              []RewrittenPR
		field            string
	}{
		{"repo 1 MB", long, "abc1234", f.prs, "repo"},
		{"repo with a newline", "o/r\nx", "abc1234", f.prs, "repo"},
		{"repo with spaces", "o/ r", "abc1234", f.prs, "repo"},
		{"repo is not owner/name", "justaname", "abc1234", f.prs, "repo"},
		{"repo dot segments", "o/..", "abc1234", f.prs, "repo"},
		{"head 1 MB", "o/r", long, f.prs, "head"},
		{"head with a flag", "o/r", "--output=/tmp/x", f.prs, "head"},
		{"head with a newline", "o/r", "abc1234\nx", f.prs, "head"},
		{"too many pull requests", "o/r", "abc1234", many, "prs"},
		{"pull request 0", "o/r", "abc1234", bad(f.prs[0], func(p *RewrittenPR) { p.Number = 0 }), "prs"},
		{"negative lines", "o/r", "abc1234", bad(f.prs[0], func(p *RewrittenPR) { p.Lines = -1 }), "prs"},
		{"huge lines", "o/r", "abc1234", bad(f.prs[0], func(p *RewrittenPR) { p.Lines = 1 << 40 }), "prs"},
		{"share over 1", "o/r", "abc1234", bad(f.prs[0], func(p *RewrittenPR) { p.Share = 7 }), "prs"},
		{"share NaN", "o/r", "abc1234", bad(f.prs[0], func(p *RewrittenPR) { p.Share = math.NaN() }), "prs"},
		{"negative time", "o/r", "abc1234", bad(f.prs[0], func(p *RewrittenPR) { p.At = -5 }), "prs"},
	} {
		_, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, c.repo, c.head, c.prs)
		if !IsInvalid(err) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: err = %v, want an invalid error naming %q", c.name, err, c.field)
		}
	}
	if f.evidenceRows(t) != 0 || f.relationRows(t) != 0 {
		t.Error("a refused call wrote something")
	}
	// The bounds themselves are allowed, as are a commit hash of either
	// length and HEAD (what wp sends when it can't read the commit).
	for _, head := range []string{"", "HEAD", strings.Repeat("f", 40), strings.Repeat("F", 64), "refs/heads/x"} {
		if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", head, f.prs); err != nil {
			t.Errorf("head %q: %v", head, err)
		}
	}
	if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "group/sub/r", "abc1234", many[:rewriteMaxPRs]); err != nil {
		t.Errorf("a nested repo and %d pull requests: %v", rewriteMaxPRs, err)
	}
}

// A task the read can't load is not "a task the actor can't see": only a
// missing one is passed over, and a read that fails fails the call, so a
// database error never reads as "nothing replaced" (which would then drop
// the evidence held for it).
func TestRewrittenWorkReturnsAReadErrorOnTheOtherTask(t *testing.T) {
	f := newRewriteFixture(t)
	if _, err := f.s.db.W.ExecContext(f.ctx, `UPDATE issues SET created_at = 'not a time' WHERE id = ?`, f.a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.RewrittenWork(f.ctx, f.admin, f.b.Key, "o/r", f.prs)
	if err == nil {
		t.Fatalf("a task that can't be read was passed over: %+v", got)
	}
}

// An editor demoted between NoteRewrites' role check and its write records
// nothing and drops nothing: the role is read again in the write.
func TestRecordRewritesRechecksTheRoleInTheWrite(t *testing.T) {
	f := newRewriteFixture(t)
	lee := f.user("lee", false)
	if err := f.s.SetMember(f.ctx, f.admin, f.proj.Key, "lee", "editor"); err != nil {
		t.Fatal(err)
	}
	read, err := f.s.RewrittenWork(f.ctx, lee, f.b.Key, "o/r", f.prs)
	if err != nil || len(read) != 1 {
		t.Fatalf("lee read %+v, %v", read, err)
	}
	d := f.get(f.admin, f.b.Key)
	if err := f.s.SetMember(f.ctx, f.admin, f.proj.Key, "lee", "viewer"); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.recordRewrites(f.ctx, lee, d, "o/r", "abc1234", read)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || f.relationRows(t) != 0 || f.evidenceRows(t) != 0 {
		t.Errorf("a viewer recorded %+v, %d relations, %d evidence rows", got, f.relationRows(t), f.evidenceRows(t))
	}
}

// insertRelation is the one way a row enters issue_relations: a pair that
// already has the relation (the same way round, or for a symmetric kind the
// other way) is "already there", not an error; Unrelated holds off for a
// relation of any kind either way; a directional kind the other way round is
// a different relation and goes in.
func TestInsertRelationIsIdempotent(t *testing.T) {
	f := newRewriteFixture(t)
	put := func(src, dst int64, kind string, unrelated bool) (bool, error) {
		var ins bool
		err := f.s.write(f.ctx, f.admin, func(tx *tx) error {
			var err error
			ins, err = f.s.insertRelation(f.ctx, tx, newRelation{Src: src, Dst: dst, Kind: kind, Unrelated: unrelated})
			return err
		})
		return ins, err
	}
	steps := []struct {
		name      string
		src, dst  int64
		kind      string
		unrelated bool
		want      bool
	}{
		{"first relates", f.a.ID, f.b.ID, "relates", false, true},
		{"same again", f.a.ID, f.b.ID, "relates", false, false},
		{"relates from the other end", f.b.ID, f.a.ID, "relates", false, false},
		{"blocks beside relates", f.a.ID, f.b.ID, "blocks", false, true},
		{"blocks the other way is its own relation", f.b.ID, f.a.ID, "blocks", false, true},
		{"unrelated holds off for any relation", f.b.ID, f.a.ID, "duplicates", true, false},
	}
	for _, st := range steps {
		got, err := put(st.src, st.dst, st.kind, st.unrelated)
		if err != nil || got != st.want {
			t.Errorf("%s: inserted = %v, %v; want %v, nil", st.name, got, err, st.want)
		}
	}
	if n := f.relationRows(t); n != 3 {
		t.Errorf("%d relation rows, want 3", n)
	}
	if _, err := put(f.a.ID, f.a.ID, "relates", false); err == nil {
		t.Error("an issue related to itself")
	}
}

// A dismissal made after RewrittenWork read, from either task's side, is
// read again in the write: the pair is left out and nothing is recorded.
func TestRecordRewritesRechecksTheDismissalInTheWrite(t *testing.T) {
	f := newRewriteFixture(t)
	read, err := f.s.RewrittenWork(f.ctx, f.admin, f.b.Key, "o/r", f.prs)
	if err != nil || len(read) != 1 {
		t.Fatalf("read %+v, %v", read, err)
	}
	lo, hi := f.a.ID, f.b.ID
	if lo > hi {
		lo, hi = hi, lo
	}
	if _, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO rewrite_dismissals(low_id, high_id, created_at) VALUES (?,?,1)`, lo, hi); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.recordRewrites(f.ctx, f.admin, f.get(f.admin, f.b.Key), "o/r", "abc1234", read)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || f.relationRows(t) != 0 || f.evidenceRows(t) != 0 {
		t.Errorf("recorded %+v, %d relations, %d evidence rows for a dismissed pair", got, f.relationRows(t), f.evidenceRows(t))
	}
}
