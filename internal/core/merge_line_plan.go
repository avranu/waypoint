package core

import "fmt"

// The merge line's planner (WP-1659, WP-ADR152). A line is the armed pull
// requests of one base of one repo whose heads are settled, in order. The
// line never pushes to a pull request's branch: it tests candidates, each a
// branch whose run merges the base's tip and the heads of the line's first
// places in order and runs the full tier on the result, and merges the
// front on a candidate that passed.
//
// planLine is the only place that decides what a line does. It reads one
// snapshot and returns actions; lineStep gathers the snapshot and carries
// them out. It reads no clock, database or forge, so every rule has a table
// test.

// lineDepth: the places that get a candidate of their own.
const lineDepth = 3

// A candidate's state as the planner sees it.
const (
	candRunning  = "running"
	candPassed   = "passed"
	candFailed   = "failed"   // its checks failed
	candConflict = "conflict" // one of its heads didn't merge
	candLost     = "lost"     // no run came, or it never ended: not the heads' fault
)

// lineEntry is a settled pull request in the line.
type lineEntry struct {
	PRID   int64
	Number int
	Head   string
	// Branch: this pull request can only be tested on its own branch (a
	// fork's, one that changes the CI workflow, a line that can't build
	// candidates), so at the front its branch is brought up to date and the
	// run on that head is its test.
	Branch bool
	// OnTip: the head holds the base's tip. FullPassed, FullRunning: the
	// full tier on the head.
	OnTip       bool
	FullPassed  bool
	FullRunning bool
}

// lineCand is a live candidate: Heads merged in order onto Base.
type lineCand struct {
	ID    int64
	Base  string
	Heads []stackHead
	State string
	// Merged: how many of Heads, from the first, the base's tip already
	// holds as merges in order (0 while the tip is Base); -1 when the tip
	// is anything else.
	Merged int
	// Reran: its failed jobs were run again once.
	Reran bool
	// ConflictPR: the pull request whose head didn't merge (candConflict).
	ConflictPR int64
}

// lineSnap is a line as read once.
type lineSnap struct {
	// TipStale: the forge's read of the base doesn't show a merge Waypoint
	// just made. Nothing is decided on it.
	TipStale bool
	Entries  []lineEntry
	Cands    []lineCand
	// Saturated: jobs wait for runners, so the line starts one candidate
	// for its first BatchDepth places instead of one per place.
	Saturated  bool
	BatchDepth int
	// Tried: candKey of every (heads) set whose candidate ended on the
	// heads' account (failed, conflict). The same heads aren't run again.
	Tried map[string]bool
}

// end_reason words of a candidate's row (migration 0187).
const (
	endMerged      = "merged"
	endFailed      = "failed"
	endConflict    = "conflict"
	endLost        = "lost"
	endNoRun       = "no_run"
	endNoMerge     = "no_merge"
	endLineChanged = "line_changed"
	endBaseMoved   = "base_moved"
	endHeadMoved   = "head_moved"
	endRefused     = "refused"
	endDuplicate   = "duplicate"
	endBatchSplit  = "batch_split"
	endLeft        = "left"
)

type lineActionKind string

const (
	lineStart  lineActionKind = "start"  // a candidate for Heads
	lineEnd    lineActionKind = "end"    // cancel candidate Cand
	lineRerun  lineActionKind = "rerun"  // run candidate Cand's failed jobs again
	lineMerge  lineActionKind = "merge"  // merge PR, on Cand when it has one
	lineEject  lineActionKind = "eject"  // PR leaves the line: Cand failed or conflicted on it
	lineUpdate lineActionKind = "update" // bring PR's own branch up to date (lineEntry.Branch)
)

type lineAction struct {
	Kind     lineActionKind
	PR       int64
	Cand     int64
	Heads    []stackHead
	Conflict bool // lineEject: its head didn't merge
	Why      string
	// End: lineEnd's end_reason word.
	End string
}

// candKey names a set of heads in order.
func candKey(heads []stackHead) string {
	k := ""
	for _, h := range heads {
		k += fmt.Sprintf("%d@%s ", h.PRID, h.SHA)
	}
	return k
}

// entryHeads is the heads of the line's first n places.
func entryHeads(es []lineEntry, n int) []stackHead {
	hs := make([]stackHead, 0, n)
	for _, e := range es[:n] {
		hs = append(hs, stackHead{PRID: e.PRID, Number: e.Number, SHA: e.Head})
	}
	return hs
}

// covers: how many of the line's first places c tests, 0 when it no longer
// matches the line: the base moved under it other than by its own heads
// merging, a head moved, or the order changed.
func (c lineCand) covers(es []lineEntry) int {
	if c.Merged < 0 || c.Merged >= len(c.Heads) {
		return 0
	}
	rest := c.Heads[c.Merged:]
	if len(rest) > len(es) {
		return 0
	}
	for i, h := range rest {
		if es[i].PRID != h.PRID || !sameSHA(es[i].Head, h.SHA) {
			return 0
		}
	}
	return len(rest)
}

// planLine decides what the line does now.
func planLine(s lineSnap) []lineAction {
	if s.TipStale {
		return nil
	}
	var acts []lineAction
	// by[n]: the live candidate that tests exactly the first n places.
	by := map[int]lineCand{}
	for _, c := range s.Cands {
		n := c.covers(s.Entries)
		switch {
		case n == 0:
			acts = append(acts, lineAction{Kind: lineEnd, Cand: c.ID, Why: "the line or the base changed under it", End: endLineChanged})
		case c.State == candLost:
			acts = append(acts, lineAction{Kind: lineEnd, Cand: c.ID, Why: "its run never came or never ended", End: endLost})
		default:
			if _, dup := by[n]; dup {
				acts = append(acts, lineAction{Kind: lineEnd, Cand: c.ID, Why: "another candidate tests the same places", End: endDuplicate})
				continue
			}
			by[n] = c
		}
	}
	if len(s.Entries) == 0 {
		return acts
	}
	front := s.Entries[0]

	// The front merges: on its own head when that holds the tip and passed
	// the full tier, else on any passed candidate that starts with it (a
	// candidate vouches for all its heads merged in order).
	if front.OnTip && front.FullPassed {
		return append(acts, lineAction{Kind: lineMerge, PR: front.PRID})
	}
	for n := 1; n <= len(s.Entries); n++ {
		if c, ok := by[n]; ok && c.State == candPassed {
			return append(acts, lineAction{Kind: lineMerge, PR: front.PRID, Cand: c.ID})
		}
	}

	// A candidate that failed or didn't merge says who leaves. Only the
	// lowest one is read: those above it tested the same heads and more.
	ejected := false
	for n := 1; n <= len(s.Entries) && !ejected; n++ {
		c, ok := by[n]
		if !ok || c.State == candRunning {
			continue
		}
		switch {
		case c.State == candConflict:
			acts = append(acts, lineAction{Kind: lineEject, PR: c.ConflictPR, Cand: c.ID, Conflict: true, Why: "its head doesn't merge onto the pull requests ahead of it or the base"})
			ejected = true
		case c.State == candFailed && !c.Reran:
			acts = append(acts, lineAction{Kind: lineRerun, Cand: c.ID})
			by[n] = lineCand{ID: c.ID, State: candRunning}
		case c.State == candFailed:
			// A passed candidate below would have merged the front above, so
			// one below is still going or failed itself.
			_, has := by[n-1]
			switch {
			case n == 1:
				acts = append(acts, lineAction{Kind: lineEject, PR: s.Entries[n-1].PRID, Cand: c.ID, Why: "the line's candidate failed on the base's tip with only its head merged"})
				ejected = true
			case has:
				// The one below is still going: it says whether this failure
				// is the last head's or an earlier one's.
			default:
				// A batch: nothing says which head failed it. It ends, and
				// each place gets a candidate of its own (Tried keeps the
				// same batch from starting again).
				acts = append(acts, lineAction{Kind: lineEnd, Cand: c.ID, Why: "the batch failed; its places are tested one by one", End: endBatchSplit})
				delete(by, n)
			}
		}
	}
	if ejected {
		return acts // the line is read again without it
	}

	// The front on its own branch: brought up to date, then its run.
	if front.Branch {
		if !front.OnTip {
			acts = append(acts, lineAction{Kind: lineUpdate, PR: front.PRID})
		}
		return acts
	}

	// Candidates for the first places that have none. Places end at the
	// first pull request that can't ride one.
	places := min(len(s.Entries), lineDepth)
	for i, e := range s.Entries[:places] {
		if e.Branch {
			places = i
			break
		}
	}
	start := func(n int) bool {
		heads := entryHeads(s.Entries, n)
		if _, live := by[n]; live || s.Tried[candKey(heads)] {
			return false
		}
		acts = append(acts, lineAction{Kind: lineStart, Heads: heads})
		return true
	}
	if s.Saturated {
		// One run for the first places together, while none is going.
		if len(by) == 0 {
			n := min(places, max(s.BatchDepth, 1))
			if n < 1 || !start(n) {
				start(1) // that batch failed before: the front alone
			}
		}
		return acts
	}
	for n := 1; n <= places; n++ {
		if n == 1 && front.OnTip && front.FullRunning {
			continue // the front's own full run is its test
		}
		start(n)
	}
	return acts
}
