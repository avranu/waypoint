package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// GET /risk: the level and reasons as JSON and text, on the issue itself
// and in wp show's text; a token reads a kind's risk only with that kind's
// read permission (WP-554).
func TestRiskAPI(t *testing.T) {
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
	issuesOnly, _, _ := svc.CreateToken(ctx, me, core.TokenSpec{Name: "narrow", Permissions: []core.Permission{core.PermIssuesRead}})
	narrow := &client{t: t, base: ts.URL, http: http.DefaultClient, token: issuesOnly}

	if code, b := human.do("POST", "/api/v1/projects", map[string]any{"name": "Risky", "key": "RSK"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}
	var issue map[string]any
	agent.json("POST", "/api/v1/issues", map[string]any{"project": "RSK", "title": "Filed by an agent"}, &issue)
	key := issue["key"].(string)
	if r, ok := issue["risk"].(map[string]any); !ok || r["level"] != "medium" {
		t.Errorf("issue risk = %v", issue["risk"])
	}

	var risk core.ProvenanceRisk
	if code := narrow.json("GET", "/api/v1/risk/issue/"+key, nil, &risk); code != 200 || risk.Level != core.RiskMedium || risk.Reasons[0].Factor != "agent_text" {
		t.Fatalf("risk: %d %+v", code, risk)
	}
	code, b := human.do("GET", "/api/v1/risk/issue/"+key+"?format=text", nil)
	if code != 200 || !strings.HasPrefix(string(b), "Risk: medium\n  medium (agent_text): agent anthropic/claude-opus-5-5 wrote it") {
		t.Errorf("text: %d %s", code, b)
	}
	code, b = human.do("GET", "/api/v1/issues/"+key+"?format=text", nil)
	if code != 200 || !strings.Contains(string(b), "Risk: medium (agent text)\n") {
		t.Errorf("issue text: %d %s", code, b)
	}

	var page map[string]any
	agent.json("POST", "/api/v1/pages", map[string]any{"title": "Notes", "project": "RSK"}, &page)
	ref := "/api/v1/risk/page/" + itoa(int(page["id"].(float64)))
	if code := human.json("GET", ref, nil, &risk); code != 200 || risk.Level != core.RiskMedium {
		t.Errorf("page risk: %d %+v", code, risk)
	}
	if code, b := narrow.do("GET", ref, nil); code != 403 {
		t.Errorf("a token without pages.read read a page's risk: %d %s", code, b)
	}
	if code, b := human.do("GET", "/api/v1/risk/galaxy/1", nil); code != 400 {
		t.Errorf("unknown kind: %d %s", code, b)
	}
}
