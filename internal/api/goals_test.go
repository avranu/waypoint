package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Goals, gaps and traces over HTTP and MCP (WP-382, WP-ADR50).
func TestGoalsAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: u}
	tok, _, _ := svc.CreateAPIToken(ctx, me, "robin", false, 0)
	human := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	agentTok, _, _ := svc.CreateToken(ctx, me, core.TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: agentTok}
	if code, b := human.do("POST", "/api/v1/projects", map[string]any{"name": "Widgets", "key": "WID"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}
	var goal, suggested, task, stray map[string]any
	human.json("POST", "/api/v1/design", map[string]any{"title": "People find any widget in a second", "kind": "goal", "project": "WID"}, &goal)
	agent.json("POST", "/api/v1/design", map[string]any{"title": "Use Elasticsearch for widgets", "kind": "goal", "project": "WID"}, &suggested)
	agent.json("POST", "/api/v1/issues", map[string]any{"project": "WID", "title": "Index widget names"}, &task)
	agent.json("POST", "/api/v1/issues", map[string]any{"project": "WID", "title": "Recolour the footer"}, &stray)
	goalID := int64(goal["id"].(float64))

	var link map[string]any
	if code := agent.json("POST", "/api/v1/links", map[string]any{"from": map[string]any{"ref": task["key"]}, "to": map[string]any{"ref": fmt.Sprintf("design:%d", goalID)}, "rel": "serves"}, &link); code != 200 {
		t.Fatalf("agent link: %d %v", code, link)
	}
	linkID := int64(link["id"].(float64))

	_, b := agent.do("GET", "/api/v1/goals?project=WID&format=text", nil)
	for _, want := range []string{"People find any widget in a second", "approved", "SUGGESTED, not approved", "wording:", "Gaps:"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("goals text lacks %q:\n%s", want, b)
		}
	}
	_, b = agent.do("GET", "/api/v1/goals/gaps?project=WID&format=text", nil)
	for _, want := range []string{"Recolour the footer", "no goal, no person", fmt.Sprintf("link %d:", linkID), "by agent anthropic/claude-opus-5-5", "Use Elasticsearch for widgets"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("gaps text lacks %q:\n%s", want, b)
		}
	}
	_, b = agent.do("GET", "/api/v1/trace?ref="+task["key"].(string)+"&format=text", nil)
	if s := string(b); !strings.Contains(s, "route to goal") || !strings.Contains(s, "served by") && !strings.Contains(s, "serves") || !strings.Contains(s, "by agent") {
		t.Errorf("trace text:\n%s", s)
	}
	var tr map[string]any
	if code := agent.json("GET", "/api/v1/trace?ref="+task["key"].(string), nil, &tr); code != 200 || tr["grounding"] != core.GroundAgentGoal {
		t.Errorf("trace json: %d %v", code, tr["grounding"])
	}

	if code, _ := agent.do("POST", fmt.Sprintf("/api/v1/goals/links/%d/confirm", linkID), nil); code != 403 {
		t.Errorf("agent confirm: %d, want 403", code)
	}
	if code, b := human.do("POST", fmt.Sprintf("/api/v1/goals/links/%d/confirm", linkID), nil); code != 200 || !strings.Contains(string(b), `"basis":"person"`) {
		t.Fatalf("confirm: %d %s", code, b)
	}
	if human.json("GET", "/api/v1/trace?ref="+task["key"].(string), nil, &tr); tr["grounding"] != core.GroundPersonGoal {
		t.Errorf("after confirm: %v", tr["grounding"])
	}

	if code, b := human.do("POST", "/api/v1/goals/dismiss-match", map[string]any{"item": map[string]any{"ref": stray["key"]}, "goal": goalID}); code != 204 {
		t.Fatalf("dismiss match: %d %s", code, b)
	}
	if code, _ := agent.do("POST", "/api/v1/goals/dismiss-match", map[string]any{"item": map[string]any{"ref": stray["key"]}, "goal": goalID}); code != 403 {
		t.Errorf("agent dismiss: %d, want 403", code)
	}

	if text := callMCPTool(t, ts.URL, agentTok, "goals", map[string]any{"project": "WID", "gaps": true}); !strings.Contains(text, "Recolour the footer") {
		t.Errorf("mcp goals gaps:\n%s", text)
	}
	if text := callMCPTool(t, ts.URL, agentTok, "trace", map[string]any{"ref": task["key"]}); !strings.Contains(text, "People find any widget") {
		t.Errorf("mcp trace:\n%s", text)
	}
	if code, _ := agent.do("GET", "/api/v1/trace?ref=WID-999", nil); code != 404 {
		t.Errorf("unknown ref: %d", code)
	}
}
