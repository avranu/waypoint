package core

import (
	"strings"
	"testing"
	"time"
)

// The cards phase 2 adds (WP-D18): each is a valid rule on its own, the one
// that deploys is added switched off, and the agent digest runs for real.
func TestAgentCardsAreValid(t *testing.T) {
	want := map[string]string{
		"Weekly code review into a PR":          "schedule",
		"Ship merged PRs":                       "repo_pushed",
		"Merge when ready":                      "pr_updated",
		"Retry flaky CI once":                   "pr_ci_failed",
		"Tell me when an environment goes down": "env_down",
		"Tell me when an agent's question waits more than 30 minutes": "request_waiting",
		"Daily digest of agent work":                                  "schedule",
		"Fix failing CI on agents' PRs":                               "pr_ci_failed",
	}
	e := newEnv(t)
	p := e.project(e.admin, "Cards", "")
	seen := 0
	for _, tpl := range AutomationTemplates {
		event, ok := want[tpl.Name]
		if !ok {
			continue
		}
		seen++
		if tpl.Rule.Trigger.Event != event {
			t.Errorf("%s: trigger %s, want %s", tpl.Name, tpl.Rule.Trigger.Event, event)
		}
		if tpl.Off != (tpl.Name == "Ship merged PRs") {
			t.Errorf("%s: off = %v", tpl.Name, tpl.Off)
		}
		if _, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: &tpl.Name, Rule: &tpl.Rule}); err != nil {
			t.Errorf("%s doesn't validate: %v", tpl.Name, err)
		}
	}
	if seen != len(want) {
		t.Errorf("%d of %d cards are in the gallery", seen, len(want))
	}
	weekly := templateRule(t, "Weekly code review into a PR")
	if weekly.Trigger.Weekday != 2 || weekly.Trigger.At != "12:00" || weekly.Trigger.Every != "week" {
		t.Errorf("schedule = %+v", weekly.Trigger)
	}
	review := weekly.Actions[0]
	if review.Type != "agent_task" || review.Extra["permission"] != AgentEdit {
		t.Errorf("first step = %+v", review)
	}
}

func TestAgentActivityAndTheDigestCard(t *testing.T) {
	e := newEnv(t)
	llm := &autoLLM{answers: map[string]string{"Write a short digest": `{"text":"One task done."}`}}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Digest", "")
	digest := e.addAuto(p, "Digest", templateRule(t, "Daily digest of agent work"))

	e.s.RunTimedAutomations(e.ctx)
	e.s.DrainAutomationRuns(e.ctx)
	if runs := e.runs(digest); len(runs) != 1 || runs[0].Status != RunSucceeded || len(llm.prompts) != 0 {
		t.Fatalf("quiet day: %+v, %d prompts", runs, len(llm.prompts))
	}
	it := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Urgent parser bug", Agent: "claude"})
	tok, _, err := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "code", Agent: "anthropic/claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	code, _ := e.s.TokenActor(e.ctx, tok)
	if _, err := e.s.AgentTake(e.ctx, code, it.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentDone(e.ctx, code, it.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	one := e.addAuto(p, "Activity", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "agent_activity", ID: "a", Extra: map[string]any{"days": 1}},
		{Type: "notify", User: "admins", Title: "Activity", Message: "{{.Steps.a.Done}} done: {{.Steps.a.Text}}"},
	}})
	run := e.manual(one, nil)
	e.s.DrainAutomationRuns(e.ctx)
	got := e.run(run.ID)
	if got.Status != RunSucceeded || stepByID(got, "a").Outputs["Done"] != 1.0 || !strings.Contains(stepByID(got, "a").Outputs["Text"].(string), it.Key) {
		t.Fatalf("activity: %s %q %+v", got.Status, got.Error, stepByID(got, "a").Outputs)
	}
	e.setClock(e.clockNow().Add(24 * time.Hour))
	e.s.RunTimedAutomations(e.ctx)
	e.s.DrainAutomationRuns(e.ctx)
	if len(llm.prompts) != 1 || !strings.Contains(llm.prompts[0], it.Key) || !strings.Contains(llm.prompts[0], "<untrusted>") {
		t.Fatalf("prompts = %q", llm.prompts)
	}
	ns, _, _ := e.s.ListNotifications(e.ctx, e.admin, false, 10)
	var told bool
	for _, n := range ns {
		told = told || (strings.HasPrefix(n.Title, "Agents: yesterday in Digest") && n.Body == "One task done.")
	}
	if !told {
		t.Errorf("notifications = %+v", ns)
	}
}
