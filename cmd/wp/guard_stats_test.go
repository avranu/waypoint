package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuardStatsReport(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	lines := strings.Join([]string{
		`{"time":"2026-11-26T10:00:00Z","mode":"deny","rule":"install","reason":"installing software","matched":"npx tsc"}`,
		`{"time":"2026-11-26T11:00:00Z","mode":"deny","rule":"install","reason":"installing software","matched":"npx tsc"}`,
		`{"time":"2026-11-25T09:00:00Z","mode":"deny","rule":"repo","reason":"writing outside repo","matched":"/tmp/foo"}`,
		`{"time":"2026-10-01T09:00:00Z","mode":"deny","rule":"download","reason":"old entry","matched":"curl example.com"}`,
		`not json at all`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "guard.log"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := readGuardStats(dir, 7, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 3 || rep.Malformed != 1 {
		t.Fatalf("7d total=%d malformed=%d, want 3 and 1", rep.Total, rep.Malformed)
	}
	if len(rep.ByRule) != 2 || rep.ByRule[0].Key != "install" || rep.ByRule[0].Count != 2 {
		t.Fatalf("by_rule = %+v", rep.ByRule)
	}
	text := guardStatsText(rep)
	for _, want := range []string{"last 7 days", "3 entries (deny 3)", "install", "installing software", "npx tsc"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}

	allRep, err := readGuardStats(dir, 0, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if allRep.Total != 4 {
		t.Fatalf("all entries total=%d, want 4", allRep.Total)
	}
}

func TestAnalyzeMemoryMDAndHelperBreakdown(t *testing.T) {
	text := "# Memory\n\n- Never run full tests locally on this machine\n- Never run full tests locally on this machine\n- Never run full tests locally on this machine\n- Never run full tests locally on this machine\n"
	lines, dupes := analyzeMemoryMD(text)
	if lines != 6 || dupes != 3 {
		t.Fatalf("analyzeMemoryMD = (%d, %d), want (6, 3)", lines, dupes)
	}

	out := helperStatsText([]helperStat{{
		Check:               "assumptions",
		Runs:                10,
		Done:                10,
		Answered:            4,
		UnansweredFailed:    4,
		UnansweredCancelled: 1,
		Raised:              5,
		Confirmed:           3,
		CostUSD:             1.25,
	}})
	for _, want := range []string{"6 done runs went with a reviewer that didn't answer", "4 reviewer/provider failed", "1 cancelled", "1 retry/other"} {
		if !strings.Contains(out, want) {
			t.Errorf("helperStatsText missing %q:\n%s", want, out)
		}
	}
}
