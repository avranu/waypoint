package hermes

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveChat drives a real Hermes through a door: set HERMES_DOOR,
// HERMES_DOOR_CA (a file), HERMES_DOOR_KEY and HERMES_PASSWORD (the dev
// door in hermestest/door and a hermes-agent container will do).
func TestLiveChat(t *testing.T) {
	door := os.Getenv("HERMES_DOOR")
	if door == "" {
		t.Skip("HERMES_DOOR not set")
	}
	ca, err := os.ReadFile(os.Getenv("HERMES_DOOR_CA"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{URL: door, CAPEM: string(ca), Key: os.Getenv("HERMES_DOOR_KEY"), Username: "waypoint", Password: os.Getenv("HERMES_PASSWORD")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := c.Check(ctx); err != nil {
		t.Fatal("check:", err)
	}
	st, err := c.GateStatus(ctx)
	if err != nil {
		t.Fatal("gate:", err)
	}
	t.Logf("gate: %+v", st)
	gw := c.Gateway()
	sub, hello, _ := gw.Subscribe(0)
	defer gw.Unsubscribe(sub)
	t.Logf("hello: %+v", hello)
	res, err := gw.Call(ctx, "session.create", map[string]any{"source": "waypoint", "close_on_disconnect": false})
	if err != nil {
		t.Fatal("create:", err)
	}
	var created struct {
		SessionID       string `json:"session_id"`
		StoredSessionID string `json:"stored_session_id"`
	}
	_ = json.Unmarshal(res, &created)
	t.Logf("session %s (stored %s)", created.SessionID, created.StoredSessionID)
	if _, err := gw.Call(ctx, "prompt.submit", map[string]any{"session_id": created.SessionID, "text": "Say hello in five words."}); err != nil {
		t.Fatal("submit:", err)
	}
	var text strings.Builder
	for {
		select {
		case <-ctx.Done():
			t.Fatal("no message.complete")
		case f := <-sub.C:
			var m struct {
				Method string `json:"method"`
				Params struct {
					Type    string          `json:"type"`
					Payload json.RawMessage `json:"payload"`
				} `json:"params"`
			}
			_ = json.Unmarshal(f.Msg, &m)
			t.Logf("%d %s %s %.200s", f.Seq, m.Method, m.Params.Type, m.Params.Payload)
			if m.Params.Type == "message.delta" {
				var d struct{ Text string }
				_ = json.Unmarshal(m.Params.Payload, &d)
				text.WriteString(d.Text)
			}
			if m.Params.Type == "message.complete" {
				t.Logf("reply: %q", text.String())
				return
			}
		}
	}
}
