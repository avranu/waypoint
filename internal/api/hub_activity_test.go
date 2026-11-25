package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestAgentActivityAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	name := "Agents"
	svc.CreateProject(ctx, &core.Actor{User: u}, core.ProjectInput{Name: &name})
	svc.CreateIssue(ctx, &core.Actor{User: u}, core.IssueInput{Project: "AGEN", Title: "Write the parser", Agent: "claude"})
	raw, _, _ := svc.CreateToken(ctx, &core.Actor{User: u}, core.TokenSpec{Name: "code", Agent: "anthropic/claude-code"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: raw}
	code, b := agent.do("POST", "/api/v1/agent/next", nil)
	if code != 200 {
		t.Fatalf("next: %d %s", code, b)
	}
	key := strings.Fields(string(b))[0]
	if code, b := agent.do("POST", "/api/v1/agent/usage/"+key, map[string]any{"input_tokens": 1500, "output_tokens": 20, "model": "claude-opus-5-5", "report_id": "r1"}); code != 200 {
		t.Fatalf("usage: %d %s", code, b)
	}
	if code, b := agent.do("POST", "/api/v1/agent/done/"+key, nil); code != 200 {
		t.Fatalf("done: %d %s", code, b)
	}

	var act core.Activity
	if code := agent.json("GET", "/api/v1/agents/activity?agent=opus&days=7", nil, &act); code != 200 {
		t.Fatalf("activity: %d", code)
	}
	if act.Totals.TasksDone != 1 || act.Totals.InputTokens != 1500 || len(act.Agents) != 1 || len(act.Daily) != 7 ||
		len(act.Runs) != 1 || act.Runs[0].Model != "claude-opus-5-5" {
		t.Fatalf("activity = %+v", act)
	}
	code, b = agent.do("GET", "/api/v1/agents/activity?format=text&agent=claude", nil)
	if code != 200 || !strings.Contains(string(b), "Agents matching claude, last 30 days: 1 task done in 1 run") ||
		!strings.Contains(string(b), "anthropic/claude-code: 1 done") || !strings.Contains(string(b), "(claude-opus-5-5)") ||
		!strings.Contains(string(b), key) {
		t.Fatalf("text: %d %s", code, b)
	}
	if code, b := agent.do("GET", "/api/v1/agents/activity?project=NOPE", nil); code != 404 {
		t.Errorf("unknown project: %d %s", code, b)
	}
}

// Agents with nothing in the period are left out (WP-271).
func TestActivityTextSkipsIdleAgents(t *testing.T) {
	now := time.Now()
	a := &core.Activity{From: now.AddDate(0, 0, -6), To: now, Agents: []*core.AgentActivity{
		{Identity: "anthropic/claude-code", Done: 2, Runs: 3, Seconds: 600},
		{Identity: "openai/codex"},
	}}
	got := activityText(a, "")
	if !strings.Contains(got, "anthropic/claude-code: 2 done") || strings.Contains(got, "openai/codex") {
		t.Errorf("text:\n%s", got)
	}
}
