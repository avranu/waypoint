package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestUsageAnalyticsAPI(t *testing.T) {
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
	body := map[string]any{"machine": "box", "version": "test", "parsed": 10, "chunks": []any{map[string]any{"id": "c1",
		"messages": []any{map[string]any{"h": hour, "s": "s1", "repo": "a/b", "model": "claude-opus-5-5", "n": 2, "in": 10, "out": 5, "cw5": 1, "cw1": 0, "cr": 100}},
		"tools":    []any{map[string]any{"h": hour, "s": "s1", "kind": "bash", "tool": "Bash", "detail": "git status", "calls": 3}},
	}}}
	var res core.UsageIngestResult
	if code := pat.json("POST", "/api/v1/usage/ingest", body, &res); code != 200 || res.Accepted != 1 {
		t.Fatalf("ingest: %d %+v", code, res)
	}
	if code := pat.json("POST", "/api/v1/usage/ingest", body, &res); code != 200 || res.Duplicate != 1 || res.Accepted != 0 {
		t.Errorf("repeat: %d %+v", code, res)
	}
	body["chunks"].([]any)[0].(map[string]any)["id"] = "c2"
	body["from_the_future"] = true
	if code, b := pat.do("POST", "/api/v1/usage/ingest", body); code != 200 {
		t.Errorf("unknown field: %d %s", code, b)
	}
	if code, _ := (&client{t: t, base: ts.URL, http: http.DefaultClient}).do("POST", "/api/v1/usage/ingest", body); code != 401 {
		t.Errorf("anonymous: %d", code)
	}
	if code, b := pat.do("POST", "/api/v1/usage/ingest", map[string]any{"chunks": []any{}}); code != 400 {
		t.Errorf("no machine: %d %s", code, b)
	}
	if code, b := pat.do("POST", "/api/v1/usage/quota", map[string]any{"machine": "box", "seven_day": map[string]any{"percent": 40, "resets_at": time.Now().Add(72 * time.Hour).UnixMilli()}}); code != 204 {
		t.Errorf("quota: %d %s", code, b)
	}

	var o core.UsageOverview
	if code := pat.json("GET", "/api/v1/usage/overview?days=7", nil, &o); code != 200 || o.Totals.Messages != 4 || o.Totals.Output != 10 || len(o.Bash) != 1 || o.Quota == nil || o.Quota.Windows[0].Percent != 40 {
		t.Fatalf("overview: %d %+v", code, o.Totals)
	}
	if code := pat.json("GET", "/api/v1/usage/overview?machine=elsewhere", nil, &o); code != 200 || o.Totals.Messages != 0 {
		t.Errorf("machine filter: %d %+v", code, o.Totals)
	}
	var sum core.UsageSummaryData
	if code := pat.json("GET", "/api/v1/usage/summary", nil, &sum); code != 200 || !sum.HasData || sum.Totals.Messages != 4 {
		t.Errorf("summary: %d %+v", code, sum)
	}
	if code, _ := agent.do("GET", "/api/v1/usage/overview", nil); code != 200 {
		t.Errorf("agent read: %d", code)
	}
	if code := adminC.json("GET", "/api/v1/usage/overview?user=pat", nil, &o); code != 200 || o.Totals.Messages != 4 {
		t.Errorf("admin read: %d %+v", code, o.Totals)
	}
	if code, _ := pat.do("GET", "/api/v1/usage/overview?user=robin", nil); code != 403 {
		t.Errorf("pat reads robin: %d", code)
	}
	if code, _ := pat.do("GET", "/api/v1/usage/overview?project=NOPE", nil); code != 404 {
		t.Errorf("unknown project: %d", code)
	}
	var rows []core.UsageBreakdown
	if code := pat.json("GET", "/api/v1/usage/breakdown?by=repo", nil, &rows); code != 200 || len(rows) != 1 || rows[0].Key != "a/b" {
		t.Errorf("breakdown: %d %+v", code, rows)
	}
	if code, _ := pat.do("GET", "/api/v1/usage/breakdown?by=nope", nil); code != 400 {
		t.Errorf("bad breakdown: %d", code)
	}

	var prices []core.ModelPrice
	if code := pat.json("GET", "/api/v1/usage/prices", nil, &prices); code != 200 || len(prices) < 10 {
		t.Errorf("prices: %d %d", code, len(prices))
	}
	if code, _ := pat.do("PUT", "/api/v1/usage/prices", map[string]any{"model": "x-1", "input": 1}); code != 403 {
		t.Errorf("pat sets a price: %d", code)
	}
	if code, _ := agent.do("PUT", "/api/v1/usage/prices", map[string]any{"model": "x-1", "input": 1}); code != 403 && code != 400 {
		t.Errorf("agent sets a price: %d", code)
	}
	if code, b := adminC.do("PUT", "/api/v1/usage/prices", map[string]any{"model": "x-1", "input": 1, "output": 2}); code != 200 || !strings.Contains(string(b), `"x-1"`) {
		t.Errorf("admin sets a price: %d %s", code, b)
	}
	if code, _ := adminC.do("DELETE", "/api/v1/usage/prices?model=x-1", nil); code != 200 && code != 204 {
		t.Errorf("delete price: %d", code)
	}
}

// GET /usage/no-task needs usage.read; days and user are accepted, and
// another person's is for an admin only.
func TestUsageNoTaskAPI(t *testing.T) {
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
	var got map[string]any
	if code := pat.json("GET", "/api/v1/usage/no-task", nil, &got); code != 200 || got == nil {
		t.Fatalf("no-task: %d %v", code, got)
	}
	if code, b := pat.do("GET", "/api/v1/usage/no-task?days=7&user=pat", nil); code != 200 {
		t.Errorf("days and own user: %d %s", code, b)
	}
	if code := adminC.json("GET", "/api/v1/usage/no-task?days=30&user=pat", nil, &got); code != 200 {
		t.Errorf("admin reads pat: %d", code)
	}
	if code, _ := pat.do("GET", "/api/v1/usage/no-task?user=robin", nil); code != 403 {
		t.Errorf("pat reads robin: %d", code)
	}
	secret, _, err := svc.CreateToken(ctx, admin, core.TokenSpec{Name: "narrow", Agent: "anthropic/claude-code", Permissions: []core.Permission{core.PermIssuesRead}})
	if err != nil {
		t.Fatal(err)
	}
	narrow := &client{t: t, base: ts.URL, http: http.DefaultClient, token: secret}
	if code, _ := narrow.do("GET", "/api/v1/usage/no-task", nil); code != 403 {
		t.Errorf("token without usage.read: %d", code)
	}
	if code, _ := (&client{t: t, base: ts.URL, http: http.DefaultClient}).do("GET", "/api/v1/usage/no-task", nil); code != 401 {
		t.Errorf("anonymous: %d", code)
	}
}
