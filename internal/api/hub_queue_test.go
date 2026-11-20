package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestAgentQueueAPI(t *testing.T) {
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
	for _, title := range []string{"First", "Second"} {
		if code, b := human.do("POST", "/api/v1/issues", map[string]any{"project": "WID", "title": title, "agent": "claude"}); code != 200 {
			t.Fatalf("issue: %d %s", code, b)
		}
	}
	if code, b := human.do("POST", "/api/v1/agents/queue/order", map[string]any{"key": "WID-2", "top": true}); code >= 300 {
		t.Fatalf("pin: %d %s", code, b)
	}
	if code, _ := agent.do("POST", "/api/v1/agents/queue/order", map[string]any{"key": "WID-1", "top": true}); code != 400 && code != 403 {
		t.Errorf("agent pinned: %d", code)
	}
	var q core.AgentQueue
	if code := human.json("GET", "/api/v1/agents/queue?agent=anthropic/claude-opus-5-5", nil, &q); code != 200 || len(q.Next) != 2 || q.Next[0].Issue.Key != "WID-2" || !q.Next[0].Pinned {
		t.Fatalf("queue: %d %+v", code, q)
	}
	code, b := human.do("GET", "/api/v1/agents/queue?agent=anthropic/claude-opus-5-5&format=text", nil)
	if code != 200 || !strings.Contains(string(b), " 1. WID-2 Second  [Pinned at #1 by robin]") {
		t.Errorf("queue text: %d %s", code, b)
	}

	// Paused: next answers in words, not with an error.
	if code, b := human.do("POST", "/api/v1/agents/pauses", map[string]any{"spec": "claude", "note": "lunch"}); code != 200 {
		t.Fatalf("pause: %d %s", code, b)
	}
	code, b = agent.do("POST", "/api/v1/agent/next", map[string]any{})
	if code != 200 || !strings.HasPrefix(string(b), "Paused: robin stopped agents matching claude") || !strings.Contains(string(b), "lunch") {
		t.Errorf("paused next: %d %s", code, b)
	}
	var resumed map[string]int
	if code := human.json("DELETE", "/api/v1/agents/pauses?spec=claude", nil, &resumed); code != 200 || resumed["resumed"] != 1 {
		t.Errorf("resume: %d %v", code, resumed)
	}
	code, b = agent.do("POST", "/api/v1/agent/next", map[string]any{})
	if code != 200 || !strings.HasPrefix(string(b), `WID-2 "Second" by `) || !strings.Contains(string(b), "\nPinned at #1 by robin") {
		t.Errorf("next: %d %s", code, b)
	}
	// A person takes it back.
	if code, b := human.do("POST", "/api/v1/agents/release/WID-2", nil); code >= 300 {
		t.Errorf("release: %d %s", code, b)
	}
	if code, _ := human.do("POST", "/api/v1/agents/release/WID-2", nil); code != 400 {
		t.Errorf("release twice: %d", code)
	}
}
