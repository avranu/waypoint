package api

import (
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestCIHistoryText(t *testing.T) {
	empty := ciHistoryText(&core.CIHistory{Days: 30, Bucket: "day"}, false)
	if !strings.Contains(empty, "No CI runs kept yet") {
		t.Errorf("empty:\n%s", empty)
	}
	p := func(n int64) *int64 { return &n }
	since := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	h := &core.CIHistory{Days: 30, Bucket: "day", Since: &since,
		Now:    core.CIHistoryStats{Jobs: 10, Failed: 1, Busy: 3600, WaitP50: p(60), Runs: 3, RunP50: p(1800), Merges: 2, MergeP50: p(2400)},
		Before: core.CIHistoryStats{Jobs: 20, Failed: 4, Busy: 7200},
		Points: []core.CIHistoryPoint{
			{At: since.AddDate(0, 0, -1).Truncate(24 * time.Hour)},
			{At: since.Truncate(24 * time.Hour)},
			{At: since.AddDate(0, 0, 1), CIHistoryStats: core.CIHistoryStats{Jobs: 4, Failed: 1, Busy: 1800, RunP50: p(1500), RunP90: p(1700), Runs: 1}},
		},
		Jobs: []core.CIHistoryJobDiff{{Repo: "acme/app", Name: "backend", Runs: 3, P50: p(600), BeforeRuns: 5, BeforeP50: p(900)}},
	}
	out := ciHistoryText(h, false)
	for _, want := range []string{"kept since 2026-10-01", "1 (10%)", "4 (20%)", "1.0h", "Oct 01", "Oct 02", "1 (25%)", "25m", "acme/app backend", "was 15m"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Sep 30") {
		t.Errorf("a bucket before the history began was listed:\n%s", out)
	}
}
