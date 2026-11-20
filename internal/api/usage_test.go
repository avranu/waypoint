package api

import (
	"context"
	"net/http"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestKnowledgeUsageAPI(t *testing.T) {
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
	var g map[string]any
	if code := human.json("POST", "/api/v1/glossary", map[string]any{"term": "Board", "definition": "Columns.", "avoid": []string{"kanban"}, "project": "WID"}, &g); code != 200 {
		t.Fatalf("term: %d %v", code, g)
	}
	id := itoa(int(g["id"].(float64)))
	// The agent looks it up (by the word to avoid, over HTTP and MCP) and
	// writes the word to avoid in a task.
	if code, _ := agent.do("GET", "/api/v1/glossary/lookup?term=kanban&project=WID", nil); code != 200 {
		t.Fatalf("lookup: %d", code)
	}
	callMCPTool(t, ts.URL, tok, "glossary_lookup", map[string]any{"term": "Board", "project": "WID"})
	if code, b := agent.do("POST", "/api/v1/issues", map[string]any{"project": "WID", "title": "Add a kanban view"}); code != 200 {
		t.Fatalf("issue: %d %s", code, b)
	}
	var us struct {
		Counts struct{ Lookups, Avoided, Agents int }
		Uses   []struct {
			Kind, Agent, Word string
			Source            *struct{ Key string }
		}
	}
	if code := human.json("GET", "/api/v1/knowledge/usage?type=glossary&id="+id, nil, &us); code != 200 {
		t.Fatalf("usage: %d", code)
	}
	if us.Counts.Lookups != 2 || us.Counts.Avoided != 1 || us.Counts.Agents != 2 {
		t.Errorf("counts = %+v", us.Counts)
	}
	if len(us.Uses) != 3 || us.Uses[0].Kind != "avoided" || us.Uses[0].Source == nil || us.Uses[0].Source.Key != "WID-1" {
		t.Errorf("uses = %+v", us.Uses)
	}
	var ov struct {
		Totals  struct{ Lookups, Avoided int }
		Avoided []struct{ Word string }
	}
	if code := human.json("GET", "/api/v1/agents/knowledge?agent=opus&days=7", nil, &ov); code != 200 || ov.Totals.Lookups != 1 || len(ov.Avoided) != 1 || ov.Avoided[0].Word != "kanban" {
		t.Errorf("overview: %d %+v", code, ov)
	}
	if code, _ := human.do("GET", "/api/v1/knowledge/usage?type=issue&id=1", nil); code != 400 {
		t.Errorf("issue usage: %d", code)
	}
}
