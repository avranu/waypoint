package api

import (
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestCILineText(t *testing.T) {
	if out := ciLineText(&core.CILineReport{Days: 7}); !strings.Contains(out, "No merge line") {
		t.Errorf("empty:\n%s", out)
	}
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	queued := now.Add(-90 * time.Minute)
	two, half := 2, 0.5
	used := 0.25
	day := func(date string) core.CILineDay {
		return core.CILineDay{Date: date, States: map[string]int{"merged": 1, "failed": 1, "passed": 1}, Rows: 3, Runs: 2, Merges: 2, MergedOnStack: 1,
			FullRuns: &two, FullPerMerge: &half, RunsUsed: 1}
	}
	tot := day("total")
	tot.UsedShare = &used
	rep := &core.CILineReport{GeneratedAt: now, Days: 2, Lines: []core.CIMergeLine{{
		Repo: "acme/app", Base: "main",
		Armed: []core.CILineEntry{
			{Number: 7, HeadSHA: "7777777", State: "draft", Queued: true, QueuedAt: &queued, Babysit: "watching", Wait: "waits for the draft tier",
				Stack: &core.CILineLive{Branch: "wp-queue/main/7", Heads: "main + #6 + #7", State: "running", AgeSec: 300}},
			{Number: 8, HeadSHA: "8888888", State: "open"},
		},
		Days: []core.CILineDay{day("2026-11-22"), day("2026-11-23")}, Totals: tot,
		Reasons: []core.CILineReason{{Reason: "head moved", Count: 2, Examples: []core.CILineExample{{Note: "#6's head moved", Count: 2}}}},
	}}}
	out := ciLineText(rep)
	for _, want := range []string{"acme/app main", "1. #7 7777777 draft, queued 1h30m ago; watching: waits for the draft tier", "stack main + #6 + #7 (wp-queue/main/7), running, 5m",
		"2. #8 8888888 open, not queued; no babysit", "2026-11-22", "total", "full/merge", "0.5", "1 of 2 runs (25%)", "head moved", "e.g. #6's head moved"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	t.Log("\n" + out)
}
