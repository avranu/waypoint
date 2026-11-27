package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClassifyFlow(t *testing.T) {
	m := func(x int64) int64 { return x * 60000 }
	hold := func(what string, from, until int64) flowHold { return flowHold{what, m(from), m(until)} }
	type want = []string // "stage from-to" in minutes
	cases := []struct {
		name string
		h    flowHistory
		want want
	}{
		{"nothing recorded", flowHistory{Opened: 0, End: m(30)}, want{flowUnattribute + " 0-30"}},
		{"a round beats the gate holding at the same time", flowHistory{
			End:    m(100),
			Holds:  []flowHold{hold("checks failed: build", 0, 100)},
			Rounds: []flowRound{{Sent: m(20), Started: m(30), Ended: m(50)}},
		}, want{flowCIFailed + " 0-20", flowRoundWait + " 20-30", flowRoundRun + " 30-50", flowCIFailed + " 50-100"}},
		{"a round with no job times is one stage", flowHistory{
			End: m(60), Rounds: []flowRound{{Sent: m(10), Ended: m(40)}},
		}, want{flowUnattribute + " 0-10", flowRoundJob + " 10-40", flowUnattribute + " 40-60"}},
		{"a round with no end is the hold's to say", flowHistory{
			End:    m(60),
			Rounds: []flowRound{{Sent: m(10), Started: m(12)}},
			Holds:  []flowHold{hold("a round is still working on it", 10, 40)},
		}, want{flowUnattribute + " 0-10", flowRoundJob + " 10-40", flowUnattribute + " 40-60"}},
		{"gates at once share the time", flowHistory{
			End:   m(60),
			Holds: []flowHold{hold("checks still running", 0, 60), hold("quiet period: 5m left", 0, 60)},
		}, want{flowCIRunning + " 0-30", flowQuiet + " 30-60"}},
		{"overlapping holds of one stage count once", flowHistory{
			End:   m(60),
			Holds: []flowHold{hold("checks still running", 0, 40), hold("full run not green yet", 20, 60)},
		}, want{flowCIRunning + " 0-60"}},
		{"a gate beats work, work beats parked, parked beats the line", flowHistory{
			End: m(100),
			Holds: []flowHold{
				hold("still a draft", 0, 100), hold("it didn't settle", 10, 100),
				hold("comments not dealt with", 20, 100), hold("quiet period", 40, 60),
			},
		}, want{flowLine + " 0-10", flowParked + " 10-20", flowWork + " 20-40", flowQuiet + " 40-60", flowWork + " 60-100"}},
		{"work says what the babysit waited for", flowHistory{
			End:   m(60),
			Holds: []flowHold{hold("comments not dealt with", 0, 60)},
			Waits: []flowWait{{At: m(-5), State: "watching", Wait: "waiting for round 3 on abcdef0123"}},
		}, want{flowWork + " 0-60"}},
		{"still open: an open hold runs to the end", flowHistory{
			End:   m(45),
			Holds: []flowHold{{What: "still a draft", From: m(15)}},
		}, want{flowUnattribute + " 0-15", flowLine + " 15-45"}},
		{"left the line and rejoined", flowHistory{
			End:   m(100),
			Holds: []flowHold{hold("still a draft", 0, 30), hold("still a draft", 50, 100)},
		}, want{flowLine + " 0-30", flowUnattribute + " 30-50", flowLine + " 50-100"}},
		{"recorded stays beat the draft hold, and the time between is converging", flowHistory{
			End:   m(100),
			Holds: []flowHold{hold("still a draft", 0, 100)},
			Lines: []flowLineSpan{{From: m(10), To: m(30)}, {From: m(50), To: m(80)}},
		}, want{flowUnattribute + " 0-10", flowLine + " 10-30", flowWork + " 30-50", flowLine + " 50-80", flowWork + " 80-100"}},
		{"a stay still open runs to the end; a gate still beats it", flowHistory{
			End:   m(60),
			Holds: []flowHold{hold("quiet period", 20, 40)},
			Lines: []flowLineSpan{{From: m(10)}},
		}, want{flowUnattribute + " 0-10", flowLine + " 10-20", flowQuiet + " 20-40", flowLine + " 40-60"}},
		{"an unrecognised hold names the time nothing else claims", flowHistory{
			End:   m(60),
			Holds: []flowHold{hold("held by thing 42", 0, 30), hold("quiet period", 40, 50)},
		}, want{flowUnattribute + " 0-30", flowUnattribute + " 30-40", flowQuiet + " 40-50", flowUnattribute + " 50-60"}},
		{"holds outside its life are cut", flowHistory{
			Opened: m(10), End: m(20),
			Holds: []flowHold{hold("still a draft", 0, 50)},
		}, want{flowLine + " 10-20"}},
		{"texts it can't read count for nothing", flowHistory{
			End: m(10), Holds: []flowHold{hold("some new reason", 0, 10)},
		}, want{flowUnattribute + " 0-10"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spans := classifyFlow(tc.h)
			var got []string
			var total int64
			for _, s := range spans {
				got = append(got, fmt.Sprintf("%s %d-%d", s.Stage, (s.From-tc.h.Opened)/60000+tc.h.Opened/60000, (s.To-tc.h.Opened)/60000+tc.h.Opened/60000))
				total += s.To - s.From
			}
			if total != tc.h.End-tc.h.Opened {
				t.Errorf("spans add to %d ms, want %d: every moment is in exactly one stage", total, tc.h.End-tc.h.Opened)
			}
			if strings.Join(got, "; ") != strings.Join(tc.want, "; ") {
				t.Errorf("stages\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestClassifyFlowWhy(t *testing.T) {
	spans := classifyFlow(flowHistory{
		End:   60000 * 60,
		Holds: []flowHold{{What: "comments not dealt with", From: 0, Until: 60000 * 60}},
		Waits: []flowWait{{At: 0, State: "watching", Wait: "waiting for round 3 on abcdef0123"}},
	})
	if len(spans) != 1 || spans[0].Why != "waiting for round N on N" {
		t.Fatalf("spans = %+v, want the wait with head and count blanked", spans)
	}
}

// A recorded stay makes a moment integrating whatever it waits for; out of a
// stay it is converging, even for a stage only the line is likely to see.
func TestClassifyFlowGroupsByLineStay(t *testing.T) {
	m := func(x int64) int64 { return x * 60000 }
	spans := classifyFlow(flowHistory{
		End: m(60),
		Holds: []flowHold{
			{What: "checks still running", From: 0, Until: m(30)},
			{What: stackMergedPrefix + " #N", From: m(40), Until: m(60)},
		},
		Lines: []flowLineSpan{{From: m(10), To: m(20)}},
	})
	var got []string
	for _, s := range spans {
		got = append(got, fmt.Sprintf("%s %d-%d %s", s.Stage, s.From/60000, s.To/60000, s.group()))
	}
	want := []string{
		flowCIRunning + " 0-10 " + FlowConverging,
		flowCIRunning + " 10-20 " + FlowIntegrating,
		flowCIRunning + " 20-30 " + FlowConverging,
		flowWork + " 30-40 " + FlowConverging,
		flowStacked + " 40-60 " + FlowConverging,
	}
	if !slices.Equal(got, want) {
		t.Errorf("spans\n got %v\nwant %v", got, want)
	}
}

// Before stays were recorded the draft hold says the line, whatever stays came
// later; after that only the stays do.
func TestClassifyFlowDraftHoldBeforeRecording(t *testing.T) {
	m := func(x int64) int64 { return x * 60000 }
	spans := classifyFlow(flowHistory{
		End: m(100), LinesFrom: m(50),
		Holds: []flowHold{{What: "still a draft", From: 0, Until: m(100)}},
		Lines: []flowLineSpan{{From: m(70), To: m(80)}},
	})
	var got []string
	for _, s := range spans {
		got = append(got, fmt.Sprintf("%s %d-%d", s.Stage, s.From/60000, s.To/60000))
	}
	want := []string{flowLine + " 0-50", flowUnattribute + " 50-70", flowLine + " 70-80", flowWork + " 80-100"}
	if !slices.Equal(got, want) {
		t.Errorf("spans\n got %v\nwant %v", got, want)
	}
}

// A stretch is cut where what the babysit waited for changed, and an unread
// hold names the time after a stay rather than it counting as work.
func TestClassifyFlowWaitsAndUnknownAfterStay(t *testing.T) {
	m := func(x int64) int64 { return x * 60000 }
	spans := classifyFlow(flowHistory{
		End:   m(60),
		Holds: []flowHold{{What: "held by thing 42", From: m(40), Until: m(60)}},
		Lines: []flowLineSpan{{From: 0, To: m(10)}},
		Waits: []flowWait{{At: m(10), Wait: "first"}, {At: m(25), Wait: "second"}},
	})
	var got []string
	for _, s := range spans {
		got = append(got, fmt.Sprintf("%s %d-%d %s", s.Stage, s.From/60000, s.To/60000, s.Why))
	}
	want := []string{
		flowLine + " 0-10 ",
		flowWork + " 10-25 first",
		flowWork + " 25-40 second",
		flowUnattribute + " 40-60 held by thing N",
	}
	if !slices.Equal(got, want) {
		t.Errorf("spans\n got %q\nwant %q", got, want)
	}
}

// The readiness texts, as merge_holds keeps them, and the stage each is.
func TestHoldStage(t *testing.T) {
	for what, want := range map[string]string{
		"required check build is failed":                               flowCIFailed,
		"required check build is pending":                              flowCIRunning,
		"required check build hasn't reported":                         flowCIRunning,
		"behind main; updating the branch, then waiting for checks":    flowCIRunning,
		"behind release; updating the branch, then waiting for checks": flowCIRunning,
		stackMergedPrefix + " #N":                                      flowStacked,
		stackClosedPrefix + " #N":                                      flowStacked,
		"footprint check queued (audit #N)":                            flowFootprint,
		"audit #N, a footprint check, named N change beyond the task":  flowWork,
		"whether a round is still working couldn't be read":            "",
		"the audits it merges after couldn't be read":                  "",
	} {
		if _, got := holdStage(what); got != want {
			t.Errorf("%q: stage %q, want %q", what, got, want)
		}
	}
}

// A span across UTC midnight counts on each day for the time spent there.
func TestFlowAggSplitsDays(t *testing.T) {
	a := newFlowAgg()
	a.add(1, classifyFlow(flowHistory{
		Opened: dayMS - 3600_000, End: dayMS + 2*3600_000,
		Holds: []flowHold{{What: "checks still running", From: 1}},
	}), true)
	for day, want := range map[string]float64{"1970-01-01": 1, "1970-01-02": 2} {
		if h, _, _ := a.dayReport(day); h != want {
			t.Errorf("%s: %v h, want %v", day, h, want)
		}
	}
}

// The holds `wp pr show` printed for two merged pull requests of this repo
// (WP-1910: it shows the newest 8, so the time before them is "nothing
// recorded" here, though merge_holds has all of it), against stages worked
// out by hand from the same lines.
func TestClassifyFlowRealPulls(t *testing.T) {
	at := func(s string) int64 {
		v, err := time.Parse("2006-01-02 15:04:05", "2026-"+s)
		if err != nil {
			t.Fatal(err)
		}
		return v.UnixMilli()
	}
	h := func(what, from, to string) flowHold { return flowHold{what, at(from), at(to)} }
	secs := func(h flowHistory) map[string]int64 {
		got := map[string]int64{}
		for _, s := range classifyFlow(h) {
			got[s.Stage] += (s.To - s.From) / 1000
		}
		return got
	}
	check := func(name string, got, want map[string]int64) {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %d s, want %d (all: %v)", name, k, got[k], v, got)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: stages %v, want %v", name, got, want)
		}
	}
	// #599: opened 11-24 17:29:01, merged 11-25 02:57:23, four rounds (their
	// ends were not kept).
	check("599", secs(flowHistory{
		Opened: at("11-24 17:29:01"), End: at("11-25 02:57:23"),
		Holds: []flowHold{
			h("checks failed: ci / backend (pull_request)", "11-24 21:52:00", "11-24 23:12:00"),
			h("audit #N, a footprint check, named N changes beyond the task (N.N): undo or split out", "11-24 23:29:00", "11-24 23:44:00"),
			h("a babysitting round is still working on it", "11-24 23:29:00", "11-25 00:01:00"),
		},
		Rounds: []flowRound{{Sent: at("11-24 18:48:30")}, {Sent: at("11-24 20:02:54")}, {Sent: at("11-24 21:52:37")}, {Sent: at("11-24 23:29:24")}},
	}), map[string]int64{flowCIFailed: 80 * 60, flowRoundJob: 32 * 60, flowUnattribute: 34102 - 80*60 - 32*60})
	// #563: from 12:00 to its merge at 14:45:07. The review wait and the
	// checks running at once share 14:12 to 14:38.
	check("563", secs(flowHistory{
		Opened: at("11-23 12:00:00"), End: at("11-23 14:45:07"),
		Holds: []flowHold{
			h("waiting for Waypoint's “Review pull requests”", "11-23 12:07:00", "11-23 12:10:00"),
			h("audit #N, a footprint check, named N change beyond the task (N.N)", "11-23 13:49:00", "11-23 13:53:00"),
			h("a babysitting round is still working on it", "11-23 13:53:00", "11-23 14:11:00"),
			h("N review comment not dealt with", "11-23 14:11:00", "11-23 14:44:00"),
			h("waiting for Waypoint's “Review pull requests” and forge-checks/codeql", "11-23 14:11:00", "11-23 14:28:00"),
			h("checks haven't reported yet", "11-23 14:12:00", "11-23 14:28:00"),
			h("N checks still running", "11-23 14:28:00", "11-23 14:44:00"),
			h("waiting for Waypoint's “Review pull requests”", "11-23 14:28:00", "11-23 14:38:00"),
		},
	}), map[string]int64{flowReview: 1020, flowCIRunning: 1140, flowWork: 240, flowRoundJob: 1080, flowUnattribute: 6427})
}

// A hold text no rule reads shows up by name (digits blanked) under "nothing
// recorded".
func TestClassifyFlowUnknownHoldIsNamed(t *testing.T) {
	spans := classifyFlow(flowHistory{
		End:   60000 * 60,
		Holds: []flowHold{{What: "held by thing 42", From: 0, Until: 60000 * 60}},
	})
	if len(spans) != 1 || spans[0].Stage != flowUnattribute || spans[0].Why != "held by thing N" {
		t.Fatalf("spans = %+v, want nothing recorded, why \"held by thing N\"", spans)
	}
}
