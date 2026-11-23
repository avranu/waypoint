package core

import (
	"fmt"
	"reflect"
	"testing"
)

// e makes a line entry for pull request n (id n*10), head "h<n>", settled
// and behind the tip.
func e(n int) lineEntry {
	return lineEntry{PRID: int64(n) * 10, Number: n, Head: fmt.Sprintf("h%d", n)}
}

func onTip(en lineEntry, passed, running bool) lineEntry {
	en.OnTip, en.FullPassed, en.FullRunning = true, passed, running
	return en
}

func branch(en lineEntry) lineEntry { en.Branch = true; return en }

// cand makes a candidate id over the pull requests ns in order.
func cand(id int64, state string, ns ...int) lineCand {
	c := lineCand{ID: id, State: state}
	for _, n := range ns {
		c.Heads = append(c.Heads, stackHead{PRID: int64(n) * 10, Number: n, SHA: fmt.Sprintf("h%d", n)})
	}
	return c
}

func heads(ns ...int) []stackHead { return cand(0, "", ns...).Heads }

func tried(ns ...[]int) map[string]bool {
	m := map[string]bool{}
	for _, n := range ns {
		m[candKey(heads(n...))] = true
	}
	return m
}

func TestPlanLine(t *testing.T) {
	start := func(ns ...int) lineAction { return lineAction{Kind: lineStart, Heads: heads(ns...)} }
	ends := func(id int64) lineAction { return lineAction{Kind: lineEnd, Cand: id} }
	merge := func(n int, c int64) lineAction { return lineAction{Kind: lineMerge, PR: int64(n) * 10, Cand: c} }
	eject := func(n int, c int64, conflict bool) lineAction {
		return lineAction{Kind: lineEject, PR: int64(n) * 10, Cand: c, Conflict: conflict}
	}
	withMerged := func(c lineCand, m int) lineCand { c.Merged = m; return c }
	rerun := func(c lineCand) lineCand { c.Reran = true; return c }
	baseOnly := func(c lineCand) lineCand { c.BaseOnly = true; return c }

	tests := []struct {
		name string
		snap lineSnap
		want []lineAction
	}{
		{name: "stale tip decides nothing", snap: lineSnap{TipStale: true, Entries: []lineEntry{e(1)}}},
		{name: "empty line ends its candidates", snap: lineSnap{Cands: []lineCand{cand(5, candRunning, 1)}}, want: []lineAction{ends(5)}},
		{name: "empty line and nothing live", snap: lineSnap{}},
		{name: "front on the tip with a full pass merges without a candidate", snap: lineSnap{Entries: []lineEntry{onTip(e(1), true, false), e(2)}}, want: []lineAction{merge(1, 0)}},
		{name: "a passed candidate of place 1 merges", snap: lineSnap{Entries: []lineEntry{e(1), e(2)}, Cands: []lineCand{cand(5, candPassed, 1)}}, want: []lineAction{merge(1, 5)}},
		{name: "a passed candidate of place 2 merges while place 1's failed", snap: lineSnap{
			Entries: []lineEntry{e(1), e(2)},
			Cands:   []lineCand{rerun(cand(5, candFailed, 1)), cand(6, candPassed, 1, 2)}}, want: []lineAction{merge(1, 6)}},
		{name: "failed, not rerun: rerun", snap: lineSnap{Entries: []lineEntry{e(1)}, Cands: []lineCand{cand(5, candFailed, 1)}}, want: []lineAction{{Kind: lineRerun, Cand: 5}}},
		{name: "failed, rerun, at place 1: eject the front", snap: lineSnap{Entries: []lineEntry{e(1), e(2)}, Cands: []lineCand{rerun(cand(5, candFailed, 1))}}, want: []lineAction{eject(1, 5, false)}},
		{name: "failed at place 2 with place 1 passed: the front merges on place 1's candidate", snap: lineSnap{
			Entries: []lineEntry{e(1), e(2)},
			Cands:   []lineCand{cand(5, candPassed, 1), rerun(cand(6, candFailed, 1, 2))}}, want: []lineAction{merge(1, 5)}},
		{name: "failed at place 2, place 1 running: nothing", snap: lineSnap{
			Entries: []lineEntry{e(1), e(2)},
			Cands:   []lineCand{cand(5, candRunning, 1), rerun(cand(6, candFailed, 1, 2))}}},
		{name: "failed batch ends and places start one by one", snap: lineSnap{
			Entries: []lineEntry{e(1), e(2)}, BatchDepth: 2,
			Cands: []lineCand{rerun(cand(6, candFailed, 1, 2))}}, want: []lineAction{ends(6), start(1), start(1, 2)}},
		{name: "a tried batch isn't started again", snap: lineSnap{
			Entries: []lineEntry{e(1), e(2)}, Saturated: true, BatchDepth: 2, Tried: tried([]int{1, 2})}, want: []lineAction{start(1)}},
		{name: "conflict ejects the pull request that didn't merge", snap: lineSnap{
			Entries: []lineEntry{e(1), e(2)},
			Cands:   []lineCand{func() lineCand { c := cand(6, candConflict, 1, 2); c.ConflictPR = 20; return c }()}},
			want: []lineAction{eject(2, 6, true)}},
		{name: "lost ends and starts afresh", snap: lineSnap{Entries: []lineEntry{e(1)}, Cands: []lineCand{cand(5, candLost, 1)}}, want: []lineAction{ends(5), start(1)}},
		{name: "head moved: candidate ends", snap: lineSnap{
			Entries: []lineEntry{{PRID: 10, Number: 1, Head: "other"}},
			Cands:   []lineCand{cand(5, candRunning, 1)}}, want: []lineAction{ends(5), {Kind: lineStart, Heads: []stackHead{{PRID: 10, Number: 1, SHA: "other"}}}}},
		{name: "order changed: candidate ends", snap: lineSnap{
			Entries: []lineEntry{e(2), e(1)},
			Cands:   []lineCand{cand(5, candRunning, 1, 2)}}, want: []lineAction{ends(5), start(2), start(2, 1)}},
		{name: "tip is something else (merged -1): candidate ends", snap: lineSnap{
			Entries: []lineEntry{e(1)}, Cands: []lineCand{withMerged(cand(5, candRunning, 1), -1)}}, want: []lineAction{ends(5), start(1)}},
		{name: "merged 1 keeps a 3-head candidate valid for the remaining two", snap: lineSnap{
			Entries: []lineEntry{e(2), e(3)},
			Cands:   []lineCand{withMerged(cand(5, candPassed, 1, 2, 3), 1)}}, want: []lineAction{merge(2, 5)}},
		{name: "duplicates: the second ends", snap: lineSnap{
			Entries: []lineEntry{e(1)}, Cands: []lineCand{cand(5, candRunning, 1), cand(6, candRunning, 1)}}, want: []lineAction{ends(6)}},
		{name: "saturated starts one batch of BatchDepth", snap: lineSnap{
			Entries: []lineEntry{e(1), e(2), e(3)}, Saturated: true, BatchDepth: 2}, want: []lineAction{start(1, 2)}},
		{name: "saturated with one live starts none", snap: lineSnap{
			Entries: []lineEntry{e(1), e(2), e(3)}, Saturated: true, BatchDepth: 2, Cands: []lineCand{cand(5, candRunning, 1, 2)}}},
		{name: "branch front not on the tip is updated", snap: lineSnap{Entries: []lineEntry{branch(e(1)), e(2)}}, want: []lineAction{{Kind: lineUpdate, PR: 10}}},
		{name: "branch front on the tip and running: nothing, no candidates behind", snap: lineSnap{
			Entries: []lineEntry{branch(onTip(e(1), false, true)), e(2)}}},
		{name: "branch front on the tip with no run is updated: a held draft is marked ready, which runs the full tier", snap: lineSnap{
			Entries: []lineEntry{branch(onTip(e(1), false, false)), e(2)}}, want: []lineAction{{Kind: lineUpdate, PR: 10}}},
		{name: "branch entry at place 2 limits candidates to place 1", snap: lineSnap{Entries: []lineEntry{e(1), branch(e(2)), e(3)}}, want: []lineAction{start(1)}},
		{name: "depth 3 cap", snap: lineSnap{Entries: []lineEntry{e(1), e(2), e(3), e(4)}}, want: []lineAction{start(1), start(1, 2), start(1, 2, 3)}},
		{name: "front on the tip with its full run going gets no place-1 candidate", snap: lineSnap{
			Entries: []lineEntry{onTip(e(1), false, true), e(2), e(3)}}, want: []lineAction{start(1, 2), start(1, 2, 3)}},
		// A red base (WP-1659): one place at a time; the candidate's own run says
		// whether the base's failure matters.
		{name: "red base: the front alone is started", snap: lineSnap{BaseRed: true, Entries: []lineEntry{e(1), e(2), e(3)}}, want: []lineAction{start(1)}},
		{name: "red base saturated: the front alone", snap: lineSnap{BaseRed: true, Saturated: true, BatchDepth: 3, Entries: []lineEntry{e(1), e(2), e(3)}}, want: []lineAction{start(1)}},
		{name: "red base, a candidate failed only what the base fails: it ends base_red and nothing else", snap: lineSnap{BaseRed: true,
			Entries: []lineEntry{e(1), e(2)}, Cands: []lineCand{baseOnly(cand(5, candFailed, 1))}}, want: []lineAction{ends(5)}},
		{name: "red base, a candidate failed its own: rerun once", snap: lineSnap{BaseRed: true,
			Entries: []lineEntry{e(1), e(2)}, Cands: []lineCand{cand(5, candFailed, 1)}}, want: []lineAction{{Kind: lineRerun, Cand: 5}}},
		{name: "red base, a candidate failed its own again: ejected", snap: lineSnap{BaseRed: true,
			Entries: []lineEntry{e(1), e(2)}, Cands: []lineCand{rerun(cand(5, candFailed, 1))}}, want: []lineAction{eject(1, 5, false)}},
		{name: "red base, a pass merges", snap: lineSnap{BaseRed: true,
			Entries: []lineEntry{e(1), e(2)}, Cands: []lineCand{cand(5, candPassed, 1)}}, want: []lineAction{merge(1, 5)}},
		{name: "red base, the front's heads tried: nothing starts", snap: lineSnap{BaseRed: true, Tried: tried([]int{1}),
			Entries: []lineEntry{e(1), e(2)}}},
		{name: "red base the front fixes: its base-only failure is its own (rerun)", snap: lineSnap{BaseRed: true, FrontFixes: true,
			Entries: []lineEntry{e(1)}, Cands: []lineCand{baseOnly(cand(5, candFailed, 1))}}, want: []lineAction{{Kind: lineRerun, Cand: 5}}},
		{name: "red base the front fixes: its base-only failure ejects after the rerun", snap: lineSnap{BaseRed: true, FrontFixes: true,
			Entries: []lineEntry{e(1)}, Cands: []lineCand{rerun(baseOnly(cand(5, candFailed, 1)))}}, want: []lineAction{eject(1, 5, false)}},
		{name: "red base: a head that doesn't merge is ejected", snap: lineSnap{BaseRed: true,
			Entries: []lineEntry{e(1), e(2)},
			Cands:   []lineCand{func() lineCand { c := cand(6, candConflict, 1, 2); c.ConflictPR = 20; return c }()}}, want: []lineAction{eject(2, 6, true)}},
		{name: "red base: a branch front is updated", snap: lineSnap{BaseRed: true, Entries: []lineEntry{branch(e(1)), e(2)}}, want: []lineAction{{Kind: lineUpdate, PR: 10}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planLine(tt.snap)
			for i := range got {
				got[i].Why, got[i].End = "", "" // the wording is not a rule
			}
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("planLine:\n got  %+v\n want %+v", got, tt.want)
			}
		})
	}
}

// A candidate that fails only what fails on the base ends as base_red, which
// isn't counted against its heads.
func TestPlanLineBaseRedEndReason(t *testing.T) {
	c := cand(5, candFailed, 1)
	c.BaseOnly = true
	got := planLine(lineSnap{BaseRed: true, Entries: []lineEntry{e(1)}, Cands: []lineCand{c}})
	if len(got) != 1 || got[0].Kind != lineEnd || got[0].End != endBaseRed {
		t.Fatalf("got %+v", got)
	}
}
