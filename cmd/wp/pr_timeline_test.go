package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const canned = `{"opened_at":"2026-11-25T08:00:00Z","merged_at":"2026-11-25T12:37:00Z",
 "pushes":[{"at":"2026-11-25T08:30:00Z","head":"8888888"}],
 "armed":[{"at":"2026-11-25T10:00:00Z","what":"arm","by":"robin"}],
 "runs":[{"number":123,"workflow":"ci","event":"pull_request","tier":"","head":"7777777","status":"failure","queued_at":"2026-11-25T08:10:00Z","started_at":"2026-11-25T08:13:12Z","stopped_at":"2026-11-25T08:27:17Z","wait_s":192,"run_s":845},
         {"number":124,"workflow":"ci","event":"pull_request","head":"8888888","status":"cancelled","queued_at":"2026-11-25T08:40:00Z"},
         {"number":125,"workflow":"ci","event":"push","stack":true,"head":"aaaaaaa","status":"success","queued_at":"2026-11-25T12:10:00Z","started_at":"2026-11-25T12:10:00Z","stopped_at":"2026-11-25T12:10:00Z","wait_s":0,"run_s":0},
         {"number":126,"workflow":"ci","event":"pull_request","head":"8888888","status":"cancelled","queued_at":"2026-11-25T08:50:00Z","stopped_at":"2026-11-25T08:55:00Z","wait_s":300}],
 "holds":[{"what":"quiet","head":"8888888","from":"2026-11-25T10:00:00Z","until":"2026-11-25T10:17:00Z","after_green_s":720}],
 "totals":{"open_to_merged_s":16620,"ci_wait_s":192,"ci_run_s":845,"held_s":720}}`

func TestPRTimelineText(t *testing.T) {
	var tl prTimeline
	if err := json.Unmarshal([]byte(canned), &tl); err != nil {
		t.Fatal(err)
	}
	got := tl.text(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	lines := strings.Split(got, "\n")
	if want := "open to merged 4h37m; over its 4 runs 3m12s waiting for a runner and 14m5s running CI; 12m held after green"; lines[0] != want {
		t.Errorf("totals line = %q, want %q", lines[0], want)
	}
	for _, want := range []string{
		"run #123 ci: started " + clock(time.Date(2026, 11, 25, 8, 13, 12, 0, time.UTC)) + " after waiting 3m12s, finished " + clock(time.Date(2026, 11, 25, 8, 27, 17, 0, time.UTC)) + " after running 14m5s, failure",
		"pushed 8888888", "run #124 ci: never started, cancelled",
		"run #126 ci: never started, waited 5m, finished " + clock(time.Date(2026, 11, 25, 8, 55, 0, 0, time.UTC)) + ", cancelled",
		"run #125 ci on the merge queue's stack: started " + clock(time.Date(2026, 11, 25, 12, 10, 0, 0, time.UTC)) + " after waiting 0s", "armed by robin", "held 17m (12m after green): quiet"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	order := []string{"opened", "run #123", "pushed", "run #124", "run #126", "armed", "held 17m", "run #125", "  merged"}
	last := -1
	for _, w := range order {
		at := strings.Index(got, w)
		if at < 0 || at < last {
			t.Errorf("%q is out of order in\n%s", w, got)
		}
		last = at
	}
}

// A merge Waypoint did is audited once the forge took it, and one done on
// the forge is audited from its report: each line says who and where, and
// the merged line is the pull request's own end.
func TestPRTimelineTextMergeAuditAndMerged(t *testing.T) {
	var tl prTimeline
	if err := json.Unmarshal([]byte(`{"state":"merged","opened_at":"2026-11-25T08:00:00Z","merged_at":"2026-11-25T10:05:00Z",
	 "pushes":[],"armed":[{"at":"2026-11-25T10:00:00Z","what":"merge","by":"robin"},{"at":"2026-11-25T11:00:00Z","what":"merged","by":"sam"}],
	 "runs":[],"holds":[],"totals":{"ci_wait_s":0,"ci_run_s":0,"held_s":0}}`), &tl); err != nil {
		t.Fatal(err)
	}
	got := tl.text(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{"  merged from Waypoint by robin\n", "  merged on the forge by sam\n", "  merged\n"} {
		if !strings.Contains(got+"\n", want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "merge started") || strings.Contains(got, "merge by") {
		t.Errorf("a merge is said to start in\n%s", got)
	}
}

// An open pull request's time so far runs to the server's clock, not this one's.
func TestPRTimelineTextOpenUsesServerClock(t *testing.T) {
	var tl prTimeline
	if err := json.Unmarshal([]byte(`{"state":"open","as_of":"2026-11-25T10:00:00Z","opened_at":"2026-11-25T08:00:00Z",
	 "pushes":[],"armed":[{"at":"2026-11-25T08:30:00Z","what":"disarm","by":"robin"}],"runs":[],
	 "holds":[{"what":"behind main","head":"8888888","from":"2026-11-25T09:30:00Z","after_green_s":0}],
	 "totals":{"ci_wait_s":0,"ci_run_s":0,"held_s":0}}`), &tl); err != nil {
		t.Fatal(err)
	}
	got := tl.text(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{"open for 2h00m; ", "  disarmed by robin\n", "  held 30m so far: behind main"} {
		if !strings.Contains(got+"\n", want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
}

// A pull request closed without merging is not "open for" a time that grows.
func TestPRTimelineTextClosedUnmerged(t *testing.T) {
	var tl prTimeline
	if err := json.Unmarshal([]byte(`{"state":"closed","opened_at":"2026-11-01T08:00:00Z","closed_at":"2026-11-01T09:30:00Z",
	 "pushes":[],"armed":[],"runs":[],"holds":[],"totals":{"ci_wait_s":0,"ci_run_s":0,"held_s":0}}`), &tl); err != nil {
		t.Fatal(err)
	}
	got := tl.text(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(got, "closed unmerged after 1h30m; ") || !strings.HasSuffix(got, "  closed") {
		t.Errorf("got\n%s", got)
	}
}

func TestPRTimelineJSON(t *testing.T) {
	var out bytes.Buffer
	c := &cli{out: &out, asJSON: true}
	if err := c.emit([]byte(canned), func() string { return "text" }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"ci_wait_s": 192`) || strings.Contains(out.String(), "text") {
		t.Errorf("--json printed %s", out.String())
	}
}
