package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A write that names a session the server has not registered is counted; a
// read, and the hook's own call (which registers sessions), are not (WP-1470).
func TestUnregisteredSessionWriteIsCountedByTheAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	robin := &core.Actor{User: u}
	tok, _, _ := svc.CreateAPIToken(ctx, robin, "robin", false, 0)
	c := &client{t: t, base: ts.URL, http: &http.Client{Transport: headerTransport{"X-Waypoint-Machine": "box", "X-Waypoint-Session": "ghost"}}, token: tok}
	if code, b := c.do("POST", "/api/v1/projects", map[string]any{"name": "Widgets", "key": "WID"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}
	counted := func() int64 {
		ds, err := svc.HookDrops(ctx, robin)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, d := range ds {
			if d.Reason == core.DropUnregisteredWrite {
				n += d.Dropped
			}
		}
		return n
	}
	if n := counted(); n != 1 {
		t.Fatalf("after one write naming an unknown session: counted %d", n)
	}
	if code, b := c.do("GET", "/api/v1/projects", nil); code != 200 {
		t.Fatalf("list: %d %s", code, b)
	}
	// a write that is refused stored nothing, so dropped nothing
	if code, _ := c.do("POST", "/api/v1/projects", map[string]any{"name": "Widgets", "key": "WID"}); code < 400 {
		t.Fatalf("a duplicate project was taken: %d", code)
	}
	// the wait hook's ask for outstanding work writes nothing without a
	// session (WP-1571, audit #1344.15)
	if code, b := c.do("POST", "/api/v1/sessions/outstanding", map[string]any{}); code != 200 {
		t.Fatalf("outstanding: %d %s", code, b)
	}
	// nor does its word that it gave answers
	if code, b := c.do("POST", "/api/v1/sessions/outstanding/told", map[string]any{"requests": []int64{1}}); code != 204 {
		t.Fatalf("told: %d %s", code, b)
	}
	many := make([]int64, 51)
	if code, _ := c.do("POST", "/api/v1/sessions/outstanding/told", map[string]any{"requests": many}); code != 400 {
		t.Errorf("told with 51 requests: %d, want 400", code)
	}
	if code := c.json("POST", "/api/v1/sessions/hook", map[string]any{"events": []map[string]any{
		{"event": "SessionStart", "session_id": "ghost", "machine": "box", "cwd": "/work/app"},
	}}, nil); code != 200 {
		t.Fatalf("hook: %d", code)
	}
	if n := counted(); n != 1 {
		t.Errorf("a read, a refused write, the wait hook's ask or word, or the hook's own call was counted: %d", n)
	}
}

func TestServerStatusTextSaysHowManyAgentEventsCarryASession(t *testing.T) {
	out := serverStatusText(&core.ServerStatus{Version: "x", Started: time.Now(), AgentEvents: core.AgentEventSessions{Sampled: 20000, Agent: 40, WithSession: 10}}, time.Now())
	if !strings.Contains(out, "10 of 40 (25%)") {
		t.Errorf("status text:\n%s", out)
	}
}
