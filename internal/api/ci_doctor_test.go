package api

import (
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestCIDoctorText(t *testing.T) {
	now := time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC)
	last := now.Add(-4 * time.Hour)
	d := &core.CIDoctor{GeneratedAt: now, Verdict: "problem",
		Bottleneck: core.CIBottleneck{Kind: "stalled", Repo: "Robin/juno", Summary: "Robin/juno's merge line is stalled on #117", Evidence: []string{"its front has held the line 4h"}},
		Findings: []core.CIFinding{
			{Level: "problem", Kind: "line_stalled", Repo: "Robin/juno", PR: 117, Title: "Robin/juno's merge line is stalled", Detail: "#117: watching for 3h", Next: "wp pr babysit 117 -p Robin/juno --status"},
			{Level: "warn", Kind: "unreliable_job", Repo: "Robin/waypoint", Title: "Robin/waypoint e2e 3 passed 2 of 8 runs"},
		},
		Lines: []core.CILine{{Repo: "Robin/juno", Base: "main", Armed: 40, Moving: 38, Stalled: true, AtFront: 4 * 3600, LastMerge: &last,
			Front:          &core.CILinePR{Number: 117, Title: "Add tags", State: "watching", Wait: "waiting for the forge's new results", WaitFor: 3 * 3600},
			Stacks:         core.CIStackStats{Tried: 6, Passed: 2, Failed: 3, Cancelled: 1, FailedBy: map[string]int{"refused": 2, "failed": 1}, Reran: 3, RerunPassed: 1},
			Steps:          core.CIMergeSteps{N: 3, Update: 120, FullRun: 1500, Total: 2400},
			EndedBy:        []core.CILineJob{{Name: "juno-browser", Merges: 3, P50: 2400}},
			HeldAfterGreen: []core.CIHoldStat{{What: "waiting for forge-checks/codeql", Merges: 2, P50: 1080}},
			HeldOf:         3,
			Merged: []core.CIMergeTiming{{Number: 116, Title: "Fix", MergedAt: last, Known: true, Reruns: 2, Early: true,
				Held: []core.CIHold{{What: "quiet", Seconds: 600}}, CIMergeSteps: core.CIMergeSteps{FullRun: 1500, Total: 2400}}}}},
		Jobs: []core.CIDoctorJob{{Repo: "Robin/juno", Name: "juno-browser", Runs: 9, Success: 5, Failure: 1, Cancelled: 1, StoppedByFailure: 2, StoppedBy: "e2e", SlowRank: 2},
			{Repo: "Robin/juno", Name: "e2e shard 2", Runs: 17, Success: 7, Failure: 4, OnePR: 272}},
		CIWork: []core.CIWorkPR{{Repo: "Robin/juno", Number: 120, Title: "Fix flaky browser job", Position: 3, State: "watching", Issue: "JUNO-313"}},
	}
	out := ciDoctorText(d, "", true)
	for _, want := range []string{"PROBLEM", "Bottleneck (stalled)", "next: wp pr babysit 117", "front #117", "STALLED", "full run 25m", "2 job re-runs", "#3 in line", "stacked places 6h: 6, 2 passed, 3 failed (2 refused), 1 cancelled; 3 re-run, 1 of them passed", "JUNO-313",
		"full run ends on: juno-browser in 3 (takes 40m)", "held by (of 3 merges): waiting for forge-checks/codeql 18m in 2",
		"merged before its full run passed", "after green: quiet 10m", "slowest #2", "2 stopped when e2e failed", "e2e shard 2: 17 runs, 7 passed, 4 failed, 0 cancelled [fails only with #272]"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if out := ciDoctorText(d, "Robin/juno", true); strings.Contains(out, "e2e 3") {
		t.Errorf("--repo kept another repo's finding:\n%s", out)
	}
	if strings.Contains(out, "Urgent lane") {
		t.Errorf("an urgent lane line with none to say:\n%s", out)
	}

	d.UrgentLane = []core.CIUrgentLane{{Repo: "Robin/juno", Text: "Robin/juno is stuck (its merge line is stalled). … `wp pr urgent 120 --repo Robin/juno` …"},
		{Repo: "Robin/waypoint", Text: "Robin/waypoint is stuck (main is red)."}}
	d.CIWork[0].Urgent = true
	out = ciDoctorText(d, "Robin/juno", false)
	if !strings.Contains(out, "\nUrgent lane: Robin/juno is stuck") || strings.Contains(out, "Robin/waypoint is stuck") || !strings.Contains(out, "JUNO-313 [urgent]") {
		t.Errorf("urgent lane:\n%s", out)
	}
}

// The front's line says a round is queued, unless its wait already does.
func TestCIDoctorTextQueuedRound(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	text := func(wait string) string {
		q := &core.CIQueuedRound{Job: 9, Since: now.Add(-10 * time.Minute), For: 600, Ahead: 1}
		d := &core.CIDoctor{GeneratedAt: now, Lines: []core.CILine{{Repo: "acme/app", Base: "main", Armed: 3, Moving: 3,
			Front: &core.CILinePR{Number: 12, Title: "Add tags", State: "working", Wait: wait, WaitFor: 600, Queued: q}}}}
		return ciDoctorText(d, "", true)
	}
	const note = " (round queued, waiting for the runner for 10m)"
	if out := text("round 1: an agent is on it (job #9)"); strings.Count(out, note) != 1 {
		t.Errorf("want one %q in:\n%s", note, out)
	}
	if out := text("round 1: queued, waiting for the runner since 09:50 UTC (1 job ahead; job #9)"); strings.Contains(out, "round queued, waiting for the runner for") {
		t.Errorf("the queued round is said twice in:\n%s", out)
	}
}
