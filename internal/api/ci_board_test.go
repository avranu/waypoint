package api

import (
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestCIBoardText(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	at := func(m int) *time.Time { x := now.Add(time.Duration(m) * time.Minute); return &x }
	b := &core.CIBoard{GeneratedAt: now,
		Summary: core.CISummary{Running: 1, Waiting: 1, Capacity: 2, AllDoneAt: at(20), AllDoneP90: at(35)},
		Alerts:  []core.CIAlert{{Level: "warn", Title: "Runner runner2 is offline"}, {Level: "info", Title: "ETAs guess"}},
		Running: []core.CIJob{{CIRef: core.CIRef{Repo: "me/app", Workflow: "ci.yml", RunNumber: 7, PR: &core.CIPRRef{Number: 3}},
			Name: "backend", Duration: 300, Typical: 720, EstFinish: at(7)}},
		Queued: []core.CIJob{
			{CIRef: core.CIRef{Repo: "me/web", Workflow: "ci.yml", RunNumber: 2, Ref: "main"}, Name: "unit", Status: "waiting", Position: 1, EstStart: at(7)},
			{CIRef: core.CIRef{Hidden: true}, Name: "x", Status: "waiting", Position: 2, RunsOn: []string{"gpu"}},
		},
	}
	out := ciBoardText(b, "", "", true)
	for _, want := range []string{"1/2 slots busy", "All done ~", "(in 20m)", "WARN: Runner runner2 is offline",
		"me/app ci.yml #7 PR 3 / backend  5m of usually 12m", "1. me/web ci.yml #2 main / unit  waiting  starts ~", "2. (another project) / x  waiting  never (no runner offers gpu)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ETAs guess") {
		t.Error("info alerts belong on the page, not in wp ci status")
	}
	if out := ciBoardText(b, "me/web", "", true); strings.Contains(out, "backend") || !strings.Contains(out, "unit") {
		t.Errorf("--repo me/web:\n%s", out)
	}
}

// The short wp ci status is the pool line, alerts and one line per run of the
// scope, capped; -v lists everything (WP-271).
func TestCIBoardTextShort(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	at := func(m int) *time.Time { x := now.Add(time.Duration(m) * time.Minute); return &x }
	b := &core.CIBoard{GeneratedAt: now,
		Summary: core.CISummary{Running: 2, Waiting: 5, Capacity: 4, AllDoneAt: at(60), AllDoneP90: at(90)},
		Hosts:   []core.CIHost{{Name: "runner2", Running: 1, Capacity: 2, Health: "ok"}, {Name: "runner3", Running: 1, Capacity: 2, Health: "disk_full"}},
	}
	for i := 1; i <= 8; i++ {
		b.Runs = append(b.Runs, core.CIRun{CIRef: core.CIRef{Repo: "me/app", Project: "APP", Workflow: "ci.yml", RunNumber: int64(i)}, Jobs: 4, Done: 1, EstFinish: at(100 - i)})
	}
	b.Runs = append(b.Runs, core.CIRun{CIRef: core.CIRef{Repo: "me/other", Project: "OTH", Workflow: "ci.yml", RunNumber: 99}, Jobs: 2, EstFinish: at(5)})
	b.Runs = append(b.Runs, core.CIRun{CIRef: core.CIRef{Hidden: true}, Jobs: 2, EstFinish: at(5)})
	b.Running = []core.CIJob{{CIRef: b.Runs[0].CIRef, Name: "backend"}}

	out := ciBoardText(b, "", "APP", false)
	for _, want := range []string{"Pool: 2/4 slots busy, 5 queued", "8 runs", "me/app ci.yml #8", "+2 more (-v)", "runner3 1/2 DISK FULL"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, not := range []string{"me/other", "another project", "backend", "runner2", "All done", "Running:"} {
		if strings.Contains(out, not) {
			t.Errorf("short output has %q:\n%s", not, out)
		}
	}
	if n := strings.Count(out, "\n"); n > 12 {
		t.Errorf("%d lines:\n%s", n, out)
	}
	if out := ciBoardText(b, "", "NONE", false); !strings.Contains(out, "Nothing of yours is running or queued.") {
		t.Errorf("no runs in scope:\n%s", out)
	}
	all := ciBoardText(b, "", "", false)
	if !strings.Contains(all, "9 runs") || !strings.Contains(all, "All done ~") || strings.Contains(all, "another project") {
		t.Errorf("every project:\n%s", all)
	}
	if v := ciBoardText(b, "", "APP", true); !strings.Contains(v, "Running:") || !strings.Contains(v, "runner2 1/2") || strings.Contains(v, "me/other") {
		t.Errorf("verbose:\n%s", v)
	}
}
