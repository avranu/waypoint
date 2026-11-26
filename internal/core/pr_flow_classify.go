package core

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// Where a pull request's open time went (WP-1910, WP-ADR163). Every moment
// from opening to merge (or to now, if it is open) is counted against one
// stage, by the priority classifyFlow states. The stages are those WP-X28
// section 1 measured. Apart from its stage, each moment falls in one of the
// two stages of WP-ADR163 that exist: integrating while the pull request holds
// a place in the merge line, whatever it waits for there, and converging while
// it doesn't.
const (
	FlowConverging  = "converging"
	FlowIntegrating = "integrating"

	flowRoundWait   = "round: waiting for a runner"
	flowRoundRun    = "round: running"
	flowRoundJob    = "round job queued or running" // a round with no job times kept
	flowCIFailed    = "CI failed (re-run or round pending)"
	flowCIRunning   = "CI running on the head"
	flowReview      = "review wait (an expected reviewer not in yet)"
	flowFootprint   = "footprint check due or running"
	flowAudit       = "audit running"
	flowQuiet       = "quiet period"
	flowMergeable   = "mergeability or conflict"
	flowStacked     = "stacked behind another pull request"
	flowWork        = "work to do, no gate holding it, no round sent"
	flowParked      = "parked for a person"
	flowLine        = "in the merge line"
	flowUnattribute = "nothing recorded"
)

// flowStages lists the stages in the order the report prints ties.
var flowStages = []string{
	flowLine,
	flowCIRunning,
	flowRoundWait,
	flowRoundRun,
	flowRoundJob,
	flowWork,
	flowCIFailed,
	flowFootprint,
	flowReview,
	flowQuiet,
	flowAudit,
	flowMergeable,
	flowStacked,
	flowParked,
	flowUnattribute,
}

// flowKind says what a merge hold's text is: a gate (a thing waited for,
// which shares the time with other gates), or a state saying what is left.
type flowKind int

const (
	kindNone flowKind = iota
	kindGate
	kindRound
	kindWork
	kindParked
	kindDraft
	kindUnknown // a hold text no rule reads
	kindGap     // out of the line between two stays in it
)

// holdStage reads a merge hold's text (the readiness reasons, as holdKind
// keeps them) as WP-X28 did: the first that matches decides. Texts it doesn't
// know, and those that only say something couldn't be read, count for nothing.
func holdStage(what string) (flowKind, string) {
	w := what
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(w, x) {
				return true
			}
		}
		return false
	}
	required := strings.HasPrefix(w, "required check ")
	switch {
	case has("couldn't be read", "couldn't read"):
		// Not evidence of what the text names: nothing is known.
	case has("round is still working"):
		return kindRound, flowRoundJob
	case has("checks failed"), required && has(" is "+CheckFailed):
		return kindGate, flowCIFailed
	case required, strings.HasPrefix(w, "behind "), has("still running", "haven't reported", "haven't been read", "full run"):
		return kindGate, flowCIRunning
	case has("codeql", "waiting for Waypoint"):
		return kindGate, flowReview
	case has("footprint check due", "footprint check queued", "a footprint check) to finish"):
		return kindGate, flowFootprint
	case has("waiting for audit", "adversarial review of"):
		return kindGate, flowAudit
	case strings.HasPrefix(w, "quiet"):
		return kindGate, flowQuiet
	case has("still checking whether it merges", "conflicts"):
		return kindGate, flowMergeable
	case has("merges after"), strings.HasPrefix(w, stackMergedPrefix), strings.HasPrefix(w, stackClosedPrefix):
		return kindGate, flowStacked
	case has("not dealt with", "still to do", "beyond the task", "carried from the review"):
		return kindWork, flowWork
	case has("didn't settle"):
		return kindParked, flowParked
	case has("still a draft"):
		return kindDraft, flowLine
	}
	return kindNone, ""
}

// flowHold is a merge hold, in ms; Until 0 is one not cleared by the end.
type flowHold struct {
	What        string
	From, Until int64
}

// flowRound is a babysit round in ms: sent, its job claimed by a runner (0:
// not known) and finished (0: not recorded, as before WP-1910).
type flowRound struct{ Sent, Started, Ended int64 }

// flowWait is a change of what a babysit waits for.
type flowWait struct {
	At          int64
	State, Wait string
}

// flowHistory is what classifyFlow reads of a pull request.
type flowHistory struct {
	Opened, End int64 // ms; End is the merge, or now for one still open
	Holds       []flowHold
	Rounds      []flowRound
	Waits       []flowWait
	Lines       []flowLineSpan // stays in the merge line, in order
	// LinesFrom is when stays began to be recorded, 0 if not known: before
	// it, only the "still a draft" hold says the pull request was in line.
	LinesFrom int64
}

// flowLineSpan is one stay in the merge line; To 0 is one not left by the end.
type flowLineSpan struct{ From, To int64 }

// flowSpan is a stretch of a pull request's life in one stage. InLine: it
// held a place in the merge line. Why is what the babysit said it waited for,
// kept for the stages that have no gate.
type flowSpan struct {
	Stage    string
	From, To int64
	InLine   bool
	Why      string
}

func (s flowSpan) group() string {
	if s.InLine {
		return FlowIntegrating
	}
	return FlowConverging
}

type flowInterval struct {
	stage    string
	kind     flowKind
	from, to int64
}

// classifyFlow gives each moment between Opened and End to one stage. In
// priority order:
//  1. a round's job: waiting for a runner (sent to claimed) then running
//     (claimed to finished); a round with no job times, or none recorded for
//     its end, counts as the "round is still working" hold says;
//  2. the gates holding the merge: CI failed, CI running, review, footprint
//     check, audit, quiet period, mergeability, stacked. Gates at once share
//     the moment equally, so each gets an equal part of it;
//  3. work left, with no gate and no round: the work stage;
//  4. parked for a person;
//  5. in the merge line: from the recorded stays in it, and before stays were
//     recorded (LinesFrom) from the "still a draft" hold. When LinesFrom isn't
//     known, that hold counts only for a pull request with no stay;
//  6. nothing recorded, named by a hold no rule reads if there is one;
//  7. out of the line after a stay in it, with nothing else known, it has
//     work to do: the work stage;
//  8. nothing recorded.
//
// A moment is in the line (integrating) when a recorded stay, or a "still a
// draft" hold that counts, covers it, whatever its stage. Holds are read as they
// were stored, uncapped and however short. A pull request that left the line
// and came back has its time in between counted as the holds then say; a hold
// left open runs to End.
func classifyFlow(h flowHistory) []flowSpan {
	if h.End <= h.Opened {
		return nil
	}
	clamp := func(v int64) int64 { return min(max(v, h.Opened), h.End) }
	var ivs []flowInterval
	add := func(stage string, kind flowKind, from, to int64) {
		if from, to = clamp(from), clamp(to); to > from {
			ivs = append(ivs, flowInterval{stage, kind, from, to})
		}
	}
	for _, r := range h.Rounds {
		switch {
		case r.Ended <= r.Sent || r.Sent == 0:
		case r.Started > r.Sent && r.Started < r.Ended:
			add(flowRoundWait, kindRound, r.Sent, r.Started)
			add(flowRoundRun, kindRound, r.Started, r.Ended)
		case r.Started != 0 && r.Started <= r.Sent:
			add(flowRoundRun, kindRound, r.Sent, r.Ended)
		default:
			add(flowRoundJob, kindRound, r.Sent, r.Ended)
		}
	}
	for i, l := range h.Lines {
		to := l.To
		if to == 0 {
			to = h.End
		}
		add(flowLine, kindDraft, l.From, to)
		if i > 0 && h.Lines[i-1].To != 0 {
			add(flowWork, kindGap, h.Lines[i-1].To, l.From)
		}
	}
	if n := len(h.Lines); n > 0 && h.Lines[n-1].To != 0 {
		add(flowWork, kindGap, h.Lines[n-1].To, h.End)
	}
	for _, x := range h.Holds {
		kind, stage := holdStage(x.What)
		if kind == kindNone {
			// Counts for nothing, but the text is kept to say what the
			// time was, if nothing else claims it.
			until := x.Until
			if until == 0 {
				until = h.End
			}
			add(flowVolatile.ReplaceAllString(x.What, "N"), kindUnknown, x.From, until)
			continue
		}
		until := x.Until
		if until == 0 {
			until = h.End
		}
		if kind == kindDraft {
			switch {
			case h.LinesFrom != 0:
				until = min(until, h.LinesFrom)
			case len(h.Lines) > 0:
				continue
			}
		}
		add(stage, kind, x.From, until)
	}

	points := []int64{h.Opened, h.End}
	// Cut at each UTC midnight, so gates at once share each day's own time.
	for d := (h.Opened/dayMS + 1) * dayMS; d < h.End; d += dayMS {
		points = append(points, d)
	}
	for _, iv := range ivs {
		points = append(points, iv.from, iv.to)
	}
	slices.Sort(points)
	points = slices.Compact(points)

	var out []flowSpan
	inLine := false // the moment at hand holds a place in the merge line
	emit := func(stage string, from, to int64, why string) {
		if n := len(out); n > 0 && out[n-1].Stage == stage && out[n-1].To == from && out[n-1].InLine == inLine && out[n-1].Why == why {
			out[n-1].To = to
			return
		}
		out = append(out, flowSpan{stage, from, to, inLine, why})
	}
	// emitWaits cuts a stretch where what the babysit waited for changed.
	emitWaits := func(stage string, from, to int64) {
		for _, w := range h.Waits {
			if w.At > from && w.At < to {
				emit(stage, from, w.At, waitWhy(h.Waits, from))
				from = w.At
			}
		}
		emit(stage, from, to, waitWhy(h.Waits, from))
	}
	for i := 0; i+1 < len(points); i++ {
		from, to := points[i], points[i+1]
		var round, gates, other []string
		var unknown []string
		gap := false
		for _, iv := range ivs {
			if iv.from > from || iv.to < to {
				continue
			}
			switch iv.kind {
			case kindRound:
				round = append(round, iv.stage)
			case kindGate:
				gates = append(gates, iv.stage)
			case kindGap:
				gap = true
			case kindUnknown:
				unknown = append(unknown, iv.stage)
			default:
				other = append(other, iv.stage)
			}
		}
		inLine = slices.Contains(other, flowLine)
		switch {
		case len(round) > 0:
			emit(pickStage(round, flowRoundRun, flowRoundWait, flowRoundJob), from, to, "")
		case len(gates) > 0:
			gates = uniqueStages(gates)
			for j, g := range gates {
				emit(g, from+(to-from)*int64(j)/int64(len(gates)), from+(to-from)*int64(j+1)/int64(len(gates)), "")
			}
		case slices.Contains(other, flowWork):
			emitWaits(flowWork, from, to)
		case slices.Contains(other, flowParked):
			emit(flowParked, from, to, "")
		case slices.Contains(other, flowLine):
			emit(flowLine, from, to, "")
		case len(unknown) > 0:
			slices.Sort(unknown)
			emit(flowUnattribute, from, to, unknown[0])
		case gap:
			emitWaits(flowWork, from, to)
		default:
			emitWaits(flowUnattribute, from, to)
		}
	}
	return out
}

// pickStage is the first of order found in stages.
func pickStage(stages []string, order ...string) string {
	for _, o := range order {
		if slices.Contains(stages, o) {
			return o
		}
	}
	return stages[0]
}

// uniqueStages is the stages once each, in the order flowStages lists them.
func uniqueStages(stages []string) []string {
	var out []string
	for _, s := range flowStages {
		if slices.Contains(stages, s) {
			out = append(out, s)
		}
	}
	return out
}

const dayMS = int64(24 * time.Hour / time.Millisecond)

var flowVolatile = regexp.MustCompile(`[0-9a-f]{7,40}|\d+`)

// waitWhy is what the babysit waited for at a time: its latest wait text
// until then, with head ids and counts blanked so the same wait adds up.
func waitWhy(waits []flowWait, at int64) string {
	why := ""
	for _, w := range waits {
		if w.At > at {
			break
		}
		why = w.Wait
		if w.Wait == "" {
			why = "(" + w.State + ")"
		}
	}
	return flowVolatile.ReplaceAllString(why, "N")
}

// jobEnd is when a round's job finished, else the fallback.
func jobEnd(job *RunnerJob, fallback time.Time) time.Time {
	if job != nil && job.FinishedAt != nil {
		return *job.FinishedAt
	}
	return fallback
}
