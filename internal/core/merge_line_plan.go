package core

import "fmt"

// lineDepth: the places that get a candidate of their own.
const lineDepth = 3

// A candidate's state as the planner sees it.
const (
	candRunning  = "running"
	candPassed   = "passed"
	candFailed   = "failed"
	candConflict = "conflict"
	candLost     = "lost"
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
	// BaseOnly: its run failed and every job that failed in it also fails on
	// the base's tip.
	BaseOnly bool
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
	// BaseRed: jobs fail on the base's tip; the line tests one place at a
	// time. FrontFixes: the front is the pull request that fixes them
	// (lineFixer's), so its failures are its own.
	BaseRed    bool
	FrontFixes bool
	// Tried: candKey of every (heads) set whose candidate ended on the
	// heads' account (failed, conflict), and, while BaseRed, of those that
	// ended base_red on this tip. The same heads aren't run again.
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
	endUnreadable  = "unreadable"
	endBaseRed     = "base_red"
)

type lineActionKind string

const (
	lineStart  lineActionKind = "start"
	lineEnd    lineActionKind = "end"
	lineRerun  lineActionKind = "rerun"
	lineMerge  lineActionKind = "merge"
	lineEject  lineActionKind = "eject"
	lineUpdate lineActionKind = "update"
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

	if front.OnTip && front.FullPassed {
		return append(acts, lineAction{Kind: lineMerge, PR: front.PRID})
	}
	for n := 1; n <= len(s.Entries); n++ {
		if c, ok := by[n]; ok && c.State == candPassed {
			return append(acts, lineAction{Kind: lineMerge, PR: front.PRID, Cand: c.ID})
		}
	}

	ejected := false
	redEnded := map[string]bool{}
	for n := 1; n <= len(s.Entries) && !ejected; n++ {
		c, ok := by[n]
		if !ok || c.State == candRunning {
			continue
		}
		switch {
		case c.State == candConflict:
			acts = append(acts, lineAction{Kind: lineEject, PR: c.ConflictPR, Cand: c.ID, Conflict: true, Why: "its head doesn't merge onto the pull requests ahead of it or the base"})
			ejected = true
		case c.State == candFailed && c.BaseOnly && s.BaseRed && !s.FrontFixes:
			acts = append(acts, lineAction{Kind: lineEnd, Cand: c.ID, Why: "it failed only what fails on the base's tip", End: endBaseRed})
			delete(by, n)
			redEnded[candKey(c.Heads[c.Merged:])] = true
		case c.State == candFailed && !c.Reran:
			acts = append(acts, lineAction{Kind: lineRerun, Cand: c.ID})
			by[n] = lineCand{ID: c.ID, State: candRunning}
		case c.State == candFailed:
			_, has := by[n-1]
			switch {
			case n == 1:
				acts = append(acts, lineAction{Kind: lineEject, PR: s.Entries[n-1].PRID, Cand: c.ID, Why: "the line's candidate failed on the base's tip with only its head merged"})
				ejected = true
			case has:
			default:
				acts = append(acts, lineAction{Kind: lineEnd, Cand: c.ID, Why: "the batch failed; its places are tested one by one", End: endBatchSplit})
				delete(by, n)
			}
		}
	}
	if ejected {
		return acts
	}

	if front.Branch {
		if !front.OnTip || !front.FullRunning {
			acts = append(acts, lineAction{Kind: lineUpdate, PR: front.PRID})
		}
		return acts
	}

	places := min(len(s.Entries), lineDepth)
	for i, e := range s.Entries[:places] {
		if e.Branch {
			places = i
			break
		}
	}
	if s.BaseRed {
		places = min(places, 1)
	}
	start := func(n int) bool {
		heads := entryHeads(s.Entries, n)
		if _, live := by[n]; live || s.Tried[candKey(heads)] || redEnded[candKey(heads)] {
			return false
		}
		acts = append(acts, lineAction{Kind: lineStart, Heads: heads})
		return true
	}
	if s.Saturated {
		if len(by) == 0 {
			n := min(places, max(s.BatchDepth, 1))
			if n < 1 || !start(n) {
				start(1)
			}
		}
		return acts
	}
	for n := 1; n <= places; n++ {
		if n == 1 && front.OnTip && front.FullRunning {
			continue
		}
		start(n)
	}
	return acts
}
