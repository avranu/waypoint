package main

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestCLIAutomations(t *testing.T) {
	e := newEnv(t)
	t.Chdir(t.TempDir())
	me := e.login()
	ctx := context.Background()
	name := "Reports"
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	add := func(title string, rule core.Rule) *core.Automation {
		a, err := e.svc.CreateAutomation(ctx, me, p.Key, core.AutomationInput{Name: &title, Rule: &rule})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	open := add("Weekly report", core.Rule{
		Trigger: core.Trigger{Event: "manual"}, AllowAgents: true,
		Inputs: []core.RuleInput{{Name: "days", Type: "number", Default: "7"}, {Name: "focus", Type: "text", Required: true}},
		Actions: []core.Action{
			{Type: "create_issue", ID: "mk", Title: "Report on {{.Inputs.focus}}, {{.Inputs.days}} days"},
		},
	})
	closed := add("Locked down", core.Rule{Trigger: core.Trigger{Event: "manual"}, Actions: []core.Action{{Type: "create_issue", Title: "no"}}})
	add("On create", core.Rule{Trigger: core.Trigger{Event: "created"}, Actions: []core.Action{{Type: "add_label", Value: "x"}}})

	out := e.wp("automation", "ls", "-p", p.Key)
	for _, want := range []string{"Weekly report", "manual (agents ok)", "Locked down", "On create", "created"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}
	if out := e.wp("automation", "show", "weekly", "-p", p.Key); !strings.Contains(out, "Input days (number, default 7)") ||
		!strings.Contains(out, "Input focus (text, required)") || !strings.Contains(out, "agents may run it") || !strings.Contains(out, "Step mk: create_issue") {
		t.Errorf("show:\n%s", out)
	}
	if out, code := e.run("automation", "show", "o", "-p", p.Key); code == 0 || !strings.Contains(out, "could be") {
		t.Errorf("ambiguous name: %d %s", code, out)
	}
	if out, code := e.run("automation", "show", "nope", "-p", p.Key); code == 0 || !strings.Contains(out, "no automation called") {
		t.Errorf("unknown name: %d %s", code, out)
	}

	out = e.wp("automation", "run", "Weekly report", "--input", "focus=bugs, mostly", "--input", "days=14", "--wait", "-p", p.Key)
	if !strings.Contains(out, "succeeded") || !strings.Contains(out, "mk") || !strings.Contains(out, "create_issue") {
		t.Errorf("run --wait:\n%s", out)
	}
	res, err := e.svc.Search(ctx, me, core.SearchParams{Query: "title:Report", Limit: 5})
	if err != nil || len(res.Issues) != 1 || res.Issues[0].Title != "Report on bugs, mostly, 14 days" {
		t.Fatalf("issues = %+v %v", res, err)
	}
	if out, code := e.run("automation", "run", "Weekly report", "-p", p.Key); code == 0 || !strings.Contains(out, "required") {
		t.Errorf("missing input: %d %s", code, out)
	}
	if out, code := e.run("automation", "run", "Locked down", "-p", p.Key); code == 0 || !strings.Contains(out, "agents") {
		t.Errorf("agent ran a locked automation: %d %s", code, out)
	}
	_ = closed
	if out, code := e.run("automation", "run", "On create", "-p", p.Key); code == 0 || !strings.Contains(out, "manual") {
		t.Errorf("ran an automation with no manual trigger: %d %s", code, out)
	}
	out = e.wp("automation", "runs", "Weekly", "-p", p.Key)
	if !strings.Contains(out, "succeeded") || !strings.Contains(out, "cli") {
		t.Errorf("runs:\n%s", out)
	}
	if js := e.wp("automation", "runs", "1", "--json"); !strings.HasPrefix(strings.TrimSpace(js), "[") && open.ID == 1 {
		t.Errorf("runs --json:\n%s", js)
	}
	weeklyRuns, _ := e.svc.ListAutomationRuns(ctx, me, open.ID, 5, 0)
	if len(weeklyRuns) == 0 {
		t.Fatal("the weekly report has no runs")
	}
	weeklyRun := strconv.FormatInt(weeklyRuns[0].ID, 10)
	if out := e.wp("automation", "show", "Weekly report", weeklyRun, "-p", p.Key); !strings.Contains(out, "Run #"+weeklyRun) {
		t.Errorf("show run:\n%s", out)
	}
	if out, code := e.run("automation", "show", "Locked down", weeklyRun, "-p", p.Key); code == 0 || !strings.Contains(out, "belongs to another automation") {
		t.Errorf("show of another automation's run: %d %s", code, out)
	}
	if out, code := e.run("automation", "runs", "Weekly", "-n", "0", "-p", p.Key); code == 0 || !strings.Contains(out, "1 to 200") {
		t.Errorf("-n 0: %d %s", code, out)
	}
	slow := add("Slow", core.Rule{Trigger: core.Trigger{Event: "manual"}, AllowAgents: true, Actions: []core.Action{{Type: "delay", Extra: map[string]any{"duration": "1h"}}}})
	out = e.wp("automation", "run", "Slow", "-p", p.Key)
	if !strings.Contains(out, "queued") {
		t.Errorf("slow run:\n%s", out)
	}
	e.svc.DrainAutomationRuns(ctx)
	runs, _ := e.svc.ListAutomationRuns(ctx, me, slow.ID, 5, 0)
	if len(runs) != 1 || runs[0].Status != core.RunWaiting {
		t.Fatalf("slow runs = %+v", runs)
	}
	if out := e.wp("automation", "cancel", "#"+strconv.FormatInt(runs[0].ID, 10)); !strings.Contains(out, "cancelled") {
		t.Errorf("cancel:\n%s", out)
	}
}
