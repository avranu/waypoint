package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func intp(n int) *int { return &n }

// taskReportOf is what the server sends, read as the CLI reads it.
func taskReportOf(t *testing.T, o *core.TaskUsage) *taskUsageReport {
	t.Helper()
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var r taskUsageReport
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestTaskUsageTextAfterRunAndReview(t *testing.T) {
	at := time.Date(2026, 11, 25, 14, 30, 0, 0, time.UTC)
	o := &core.TaskUsage{Key: "WP-1", Title: "Ship", Who: "pat", Used: []core.TaskUsageTool{}}
	o.CostUSD = 5.71
	o.AfterRun = &core.TaskCarryOver{CostUSD: 36.49, Sessions: 1}
	o.Rounds = []core.TaskUsageRound{{At: at, State: "done", Found: intp(15), Fixed: intp(12), Rejected: intp(3)}, {At: at.Add(time.Hour), State: "done"}}
	o.Reviewer = &core.TaskUsageReviewer{Audits: 2, Input: 2000, Output: 400}
	merged := at.Add(2 * time.Hour)
	o.PullRequests = []core.TaskUsagePR{{Repo: "acme/web", Number: 7, State: "merged", MergedAt: &merged}, {Repo: "acme/web", Number: 8, State: "open"}}
	got := taskUsageText(taskReportOf(t, o))
	for _, want := range []string{
		"$5.71,",
		"After its run ended, same session, no other task claimed: $36.49 in 1 session.",
		"Review round 1 2026-11-25 14:30 done: found 15, fixed 12, rejected 3",
		"Review round 2 2026-11-25 15:30 done\n",
		"Reviewers: 2 audits, 2,000 tokens in, 400 out.",
		"Pull request acme/web#7: merged 2026-11-25 16:30",
		"Pull request acme/web#8: open\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	raw, _ := json.Marshal(o)
	if !strings.Contains(string(raw), `"after_run":{"cost_usd":36.49,"sessions":1}`) {
		t.Errorf("json: %s", raw)
	}
}

// Nothing is printed for what is absent, and the JSON leaves it out.
func TestTaskUsageTextPrintsNothingAbsent(t *testing.T) {
	o := &core.TaskUsage{Key: "WP-1", Title: "Ship", Who: "pat", Used: []core.TaskUsageTool{}}
	got := taskUsageText(taskReportOf(t, o))
	for _, bad := range []string{"After its run", "Review round", "Reviewers", "Pull request"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q printed with none:\n%s", bad, got)
		}
	}
	raw, _ := json.Marshal(o)
	for _, bad := range []string{"after_run", "rounds", "reviewer", "pull_requests"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("%s in json %s", bad, raw)
		}
	}
}

func TestNoTaskText(t *testing.T) {
	o := &core.NoTaskUsage{Who: "pat", Days: 7, From: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
		TotalUSD: 100, TaskUSD: 70, NoTaskUSD: 30, CarryUSD: 20, CarryTasks: 1, NeverUSD: 10, NeverSessions: 1,
		CarryOver: []core.NoTaskCarry{{Key: "JOBH-578", Title: "Ship it", CostUSD: 20, Sessions: 2}},
		Never:     []core.NoTaskSession{{Machine: "m1", Session: "0123456789abcdef", Repo: "acme/web", FirstHour: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC), CostUSD: 10}}}
	raw, _ := json.Marshal(o)
	var r noTaskReport
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	got := noTaskText(&r)
	for _, want := range []string{"total      $100.00", "on tasks   $70.00 (70%)", "on no task $30.00 (30%)", "Carry-over $20.00", "JOBH-578", "2 sessions",
		"Sessions that never had a run: $10.00 in 1", "m1 01234567  acme/web  from 2026-11-24 09:00"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Other") {
		t.Errorf("an empty part printed:\n%s", got)
	}
}

func TestCLIUsageTaskNone(t *testing.T) {
	e := newEnv(t)
	e.login()
	out := e.wp("usage", "task", "--none", "--days", "7")
	if !strings.Contains(out, "Spend, last 7 days") || !strings.Contains(out, "on no task $0.00") {
		t.Errorf("text: %s", out)
	}
	var r noTaskReport
	if err := json.Unmarshal([]byte(e.wp("usage", "task", "--none", "--json")), &r); err != nil || r.Days != 30 {
		t.Errorf("json: %+v, %v", r, err)
	}
	if out, code := e.run("usage", "task", "--none", "WP-1"); code == 0 || !strings.Contains(out, "leave out the task WP-1") {
		t.Errorf("a task with --none: %d %s", code, out)
	}
	if out, code := e.run("usage", "task", "--days", "7"); code == 0 || !strings.Contains(out, "wp usage task --none --days 7") {
		t.Errorf("--days alone: %d %s", code, out)
	}
	if out, code := e.run("usage", "task", "--none", "--days", "-7"); code == 0 || !strings.Contains(out, "wp usage task --none --days 7") {
		t.Errorf("a negative --days: %d %s", code, out)
	}
}
