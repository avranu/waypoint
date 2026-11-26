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
// section 1 measured, grouped under the two stages of WP-ADR163 that exist:
// converging, everything before the pull request holds a place in the merge
// line, and integrating, in the line.
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
	flowLine        = "draft with nothing else holding it (in the merge line)"
	flowUnattribute = "nothing recorded"
)

// flowStages lists the stages in the order the report prints ties, with
// their group.
var flowStages = []struct{ Name, Group string }{
	{flowLine, FlowIntegrating},
	{flowCIRunning, FlowConverging},
	{flowRoundWait, FlowConverging},
	{flowRoundRun, FlowConverging},
	{flowRoundJob, FlowConverging},
	{flowWork, FlowConverging},
	{flowCIFailed, FlowConverging},
	{flowFootprint, FlowConverging},
	{flowReview, FlowConverging},
	{flowQuiet, FlowConverging},
	{flowAudit, FlowConverging},
	{flowMergeable, FlowConverging},
	{flowStacked, FlowIntegrating},
	{flowParked, FlowConverging},
	{flowUnattribute, FlowConverging},
}

func flowGroup(stage string) string {
	for _, s := range flowStages {
		if s.Name == stage {
			return s.Group
		}
	}
	return FlowConverging
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
)

// holdStage reads a merge hold's text (the strings merge.go gives) as WP-X28
// did: the first that matches decides. Texts it doesn't know count for nothing.
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
	switch {
	case has("round is still working"):
		return kindRound, flowRoundJob
	case has("checks failed"):
		return kindGate, flowCIFailed
	case has("still running", "haven't reported", "haven't been read", "full run", "behind main"):
		return kindGate, flowCIRunning
	case has("codeql", "waiting for Waypoint"):
		return kindGate, flowReview
	case has("footprint check due", "a footprint check) to finish"):
		return kindGate, flowFootprint
	case has("waiting for audit", "adversarial review of"):
		return kindGate, flowAudit
	case strings.HasPrefix(w, "quiet"):
		return kindGate, flowQuiet
	case has("still checking whether it merges", "conflicts"):
		return kindGate, flowMergeable
	case has("merges after"):
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
}

// flowSpan is a stretch of a pull request's life in one stage. Why is what
// the babysit said it waited for, kept for the stages that have no gate.
type flowSpan struct {
	Stage    string
	From, To int64
	Why      string
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
//  5. a draft with nothing else holding it: in the merge line;
//  6. nothing recorded.
//
// Holds are read as they were stored, uncapped and however short. A pull
// request that left the line and came back has its time in between counted
// as the holds then say; a hold left open runs to End.
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
	for _, x := range h.Holds {
		kind, stage := holdStage(x.What)
		if kind == kindNone {
			continue
		}
		until := x.Until
		if until == 0 {
			until = h.End
		}
		add(stage, kind, x.From, until)
	}

	points := []int64{h.Opened, h.End}
	for _, iv := range ivs {
		points = append(points, iv.from, iv.to)
	}
	slices.Sort(points)
	points = slices.Compact(points)

	var out []flowSpan
	emit := func(stage string, from, to int64, why string) {
		if n := len(out); n > 0 && out[n-1].Stage == stage && out[n-1].To == from && out[n-1].Why == why {
			out[n-1].To = to
			return
		}
		out = append(out, flowSpan{stage, from, to, why})
	}
	for i := 0; i+1 < len(points); i++ {
		from, to := points[i], points[i+1]
		var round, gates, other []string
		for _, iv := range ivs {
			if iv.from > from || iv.to < to {
				continue
			}
			switch iv.kind {
			case kindRound:
				round = append(round, iv.stage)
			case kindGate:
				gates = append(gates, iv.stage)
			default:
				other = append(other, iv.stage)
			}
		}
		switch {
		case len(round) > 0:
			emit(pickStage(round, flowRoundRun, flowRoundWait, flowRoundJob), from, to, "")
		case len(gates) > 0:
			gates = uniqueStages(gates)
			for j, g := range gates {
				emit(g, from+(to-from)*int64(j)/int64(len(gates)), from+(to-from)*int64(j+1)/int64(len(gates)), "")
			}
		case slices.Contains(other, flowWork):
			emit(flowWork, from, to, waitWhy(h.Waits, from))
		case slices.Contains(other, flowParked):
			emit(flowParked, from, to, "")
		case slices.Contains(other, flowLine):
			emit(flowLine, from, to, "")
		default:
			emit(flowUnattribute, from, to, waitWhy(h.Waits, from))
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
		if slices.Contains(stages, s.Name) {
			out = append(out, s.Name)
		}
	}
	return out
}

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
