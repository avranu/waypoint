package core

import (
	"strings"
	"testing"
)

// The reverse of a relation is judged where the row is written, per kind
// (WP-1370): a symmetric kind says the same from both ends, so the pair the
// other way round is the relation already there; an asymmetric kind the other
// way round (A follows B while B follows A) contradicts itself and is refused.
// A different kind the other way round is a different statement and stands.
func TestInsertRelationJudgesTheReverseForEveryKind(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Rev", "")
	for _, k := range relationKindList {
		t.Run(k.Kind, func(t *testing.T) {
			a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "first of " + k.Kind})
			b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "second of " + k.Kind})
			ins := func(src, dst int64, kind string) (ok bool, err error) {
				err = e.s.write(e.ctx, e.admin, func(tx *tx) (err error) {
					ok, err = e.s.insertRelation(e.ctx, tx, newRelation{Src: src, Dst: dst, Kind: kind, Basis: "person"})
					return err
				})
				return ok, err
			}
			if ok, err := ins(a.ID, b.ID, k.Kind); !ok || err != nil {
				t.Fatalf("first: %v %v", ok, err)
			}
			if ok, err := ins(a.ID, b.ID, k.Kind); ok || err != nil {
				t.Errorf("the same way again: inserted %v, err %v; want already there", ok, err)
			}
			ok, err := ins(b.ID, a.ID, k.Kind)
			if k.Inverse == "" {
				if ok || err != nil {
					t.Errorf("symmetric %s the other way: inserted %v, err %v; want already there", k.Kind, ok, err)
				}
			} else if ok || err == nil || !strings.Contains(err.Error(), "other way") {
				t.Errorf("asymmetric %s the other way: inserted %v, err %v; want a refusal that says so", k.Kind, ok, err)
			}
			var n int
			if err := e.s.db.R.QueryRow(`SELECT count(*) FROM issue_relations WHERE kind = ? AND source_id IN (?,?) AND target_id IN (?,?)`, k.Kind, a.ID, b.ID, a.ID, b.ID).Scan(&n); err != nil || n != 1 {
				t.Errorf("%d %s rows for the pair, %v; want 1", n, k.Kind, err)
			}
			other := "relates"
			if k.Kind == "relates" {
				other = "blocks"
			}
			if ok, err := ins(b.ID, a.ID, other); !ok || err != nil {
				t.Errorf("%s the other way beside %s: %v %v", other, k.Kind, ok, err)
			}
		})
	}
}

// Saying it through Relate, from either end's name, is refused the same way.
func TestRelateRefusesAContradiction(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Rev2", "")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "blocker"})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "blocked"})
	if _, err := e.s.Relate(e.ctx, e.admin, a.Key, RelationInput{Kind: "blocks", Issue: b.Key}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ from, kind, to string }{{b.Key, "blocks", a.Key}, {a.Key, "blocked_by", b.Key}} {
		if _, err := e.s.Relate(e.ctx, e.admin, c.from, RelationInput{Kind: c.kind, Issue: c.to}); err == nil || !strings.Contains(err.Error(), "other way") {
			t.Errorf("%s %s %s: err = %v, want a contradiction", c.from, c.kind, c.to, err)
		}
	}
	if got := e.get(e.admin, a.Key).Relations; len(got) != 1 || got[0].Kind != "blocks" {
		t.Errorf("relations = %+v", got)
	}
}
