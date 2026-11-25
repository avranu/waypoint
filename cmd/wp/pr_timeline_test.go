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
 "runs":[{"number":123,"workflow":"ci","event":"pull_request","tier":"","head":"7777777","status":"failure","queued_at":"2026-11-25T08:10:00Z","wait_s":192,"run_s":845},
         {"number":124,"workflow":"ci","event":"pull_request","head":"8888888","status":"cancelled","queued_at":"2026-11-25T08:40:00Z"},
         {"number":125,"workflow":"ci","event":"push","stack":true,"head":"aaaaaaa","status":"success","queued_at":"2026-11-25T12:10:00Z","wait_s":0,"run_s":0}],
 "holds":[{"what":"quiet","head":"8888888","from":"2026-11-25T10:05:00Z","until":"2026-11-25T10:17:00Z"}],
 "totals":{"open_to_merged_s":16620,"ci_wait_s":192,"ci_run_s":845,"held_s":720}}`

func TestPRTimelineText(t *testing.T) {
	var tl prTimeline
	if err := json.Unmarshal([]byte(canned), &tl); err != nil {
		t.Fatal(err)
	}
	got := tl.text(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	lines := strings.Split(got, "\n")
	if want := "open to merged 4h37m: 3m12s waiting for a runner, 14m5s running CI in 3 runs, 12m held after green"; lines[0] != want {
		t.Errorf("totals line = %q, want %q", lines[0], want)
	}
	for _, want := range []string{"run #123 ci: waited 3m12s, ran 14m5s, failure", "pushed 8888888", "run #124 ci: never started, cancelled", "run #125 ci on the merge queue's stack: waited 0s, ran 0s, success", "armed by robin", "held 12m: quiet"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	order := []string{"opened", "run #123", "pushed", "run #124", "armed", "held 12m", "run #125", "  merged"}
	last := -1
	for _, w := range order {
		at := strings.Index(got, w)
		if at < last {
			t.Errorf("%q is out of order in\n%s", w, got)
		}
		last = at
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
