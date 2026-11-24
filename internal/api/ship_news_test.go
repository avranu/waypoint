package api

import (
	"context"
	"net/http"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The ship-news route answers "none" for a task with no review round, and an
// ack call answers "acked" (WP-1669).
func TestShipNewsRoute(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: u}, "robin", false, 0)
	c := &client{t: t, base: ts.URL, http: &http.Client{Transport: headerTransport{"X-Waypoint-Machine": "box"}}, token: tok}
	if code, b := c.do("POST", "/api/v1/projects", map[string]any{"name": "Widgets", "key": "WID"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}
	if code, b := c.do("POST", "/api/v1/issues", map[string]any{"project": "WID", "title": "Task"}); code != 200 {
		t.Fatalf("issue: %d %s", code, b)
	}
	if code := c.json("POST", "/api/v1/sessions/hook", map[string]any{"events": []map[string]any{
		{"event": "SessionStart", "session_id": "sess1", "machine": "box", "cwd": "/work/app"},
	}}, nil); code != 200 {
		t.Fatalf("hook: %d", code)
	}
	var got struct {
		Outcome string `json:"outcome"`
	}
	body := map[string]any{"session_id": "sess1", "machine": "box", "task": "WID-1", "wait": 0}
	if code := c.json("POST", "/api/v1/sessions/hook/ship-news", body, &got); code != 200 || got.Outcome != "none" {
		t.Fatalf("wait: %d %+v", code, got)
	}
	body["ack"] = []int64{1}
	if code := c.json("POST", "/api/v1/sessions/hook/ship-news", body, &got); code != 200 || got.Outcome != "acked" {
		t.Fatalf("ack: %d %+v", code, got)
	}
}
