package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestUsageCLIAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	mk := func(name string, admin bool) (*core.Actor, *client) {
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct-horse-battery", IsAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		a := &core.Actor{User: u}
		tok, _, _ := svc.CreateAPIToken(ctx, a, name, false, 0)
		return a, &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	admin, adminC := mk("robin", true)
	_, pat := mk("pat", false)
	agentTok, _, _ := svc.CreateToken(ctx, admin, core.TokenSpec{Name: "cc", Agent: "anthropic/claude-code"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: agentTok}

	hour := time.Now().Add(-2 * time.Hour).Truncate(time.Hour).UnixMilli()
	row := func(kind, head, detail string) map[string]any {
		return map[string]any{"h": hour, "s": "s1", "repo": "a/b", "agent": "main", "kind": kind, "head": head, "detail": detail, "n": 2}
	}
	body := map[string]any{"machine": "box", "version": "test", "chunks": []any{map[string]any{"id": "c1", "cli": []any{
		row("file", "", ""), row("wp", "wp next", ""), row("wp", "wp show WP-1 the title", ""),
	}}}}
	var res core.UsageIngestResult
	if code := adminC.json("POST", "/api/v1/usage/ingest", body, &res); code != 200 || res.Accepted != 1 {
		t.Fatalf("ingest: %d %+v", code, res)
	}

	var r core.UsageCLIReport
	if code := adminC.json("GET", "/api/v1/usage/cli?days=30", nil, &r); code != 200 || r.Total.WP != 2 || len(r.Commands) != 1 {
		t.Fatalf("report: %d %+v", code, r)
	}
	r = core.UsageCLIReport{}
	if code := agent.json("GET", "/api/v1/usage/cli?days=30", nil, &r); code != 200 || r.Total.WP != 2 {
		t.Errorf("agent: %d %+v", code, r.Total)
	}
	for _, path := range []string{"/api/v1/usage/overview", "/api/v1/usage/no-task?days=7", "/api/v1/usage/ledger", "/api/v1/usage/trends", "/api/v1/usage/summary", "/api/v1/usage/refusals", "/api/v1/usage/risky", "/api/v1/usage/warm", "/api/v1/usage/guard",
		"/api/v1/usage/breakdown?by=repo", "/api/v1/autopilot", "/api/v1/autopilot/summaries"} {
		if code, b := agent.do("GET", path, nil); code != 200 {
			t.Errorf("agent on %s: %d %s", path, code, b)
		}
	}
	if code, _ := agent.do("GET", "/api/v1/usage/cli?days=30&user=pat", nil); code != 403 {
		t.Errorf("agent reading pat: %d", code)
	}
	if code, _ := pat.do("GET", "/api/v1/usage/cli?days=30&user=robin", nil); code != 403 {
		t.Errorf("pat reading robin: %d", code)
	}
	if code, _ := (&client{t: t, base: ts.URL, http: http.DefaultClient}).do("GET", "/api/v1/usage/cli?days=30", nil); code != 401 {
		t.Errorf("anonymous: %d", code)
	}
}
