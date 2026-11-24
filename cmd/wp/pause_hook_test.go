package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testPause = `{"id":3,"spec":"any","note":"Quota alert: 41% of the week in an hour","by":{"username":"robin","display_name":"Robin"},"created_at":"2026-11-05T09:00:00Z"}`

// A pause from the server reaches the gate and the nudge through the local
// copy; a stale copy is ignored; null clears it (WP-146).
func TestSessionPauseHooks(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GATE", "off") // the pause refuses agents whatever the gate's mode
	t.Setenv("HOME", t.TempDir())
	c := &cli{dir: t.TempDir()}
	gate := func() string {
		var out bytes.Buffer
		hookClaudeGate(c, strings.NewReader(`{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"haiku-worker","model":"haiku"}}`), &out)
		return out.String()
	}
	nudge := func(session string) string {
		in, _ := json.Marshal(map[string]any{"session_id": session, "tool_name": "Bash", "cwd": t.TempDir(), "tool_input": map[string]string{"command": "ls"}})
		var out bytes.Buffer
		hookClaudeNudge(c, bytes.NewReader(in), &out)
		return out.String()
	}
	if gate() != "" || nudge("s1") != "" {
		t.Fatal("hooks spoke with no pause")
	}

	c.savePause(json.RawMessage(nil))
	c.savePause(json.RawMessage(testPause))
	if g := gate(); !strings.Contains(g, `"permissionDecision":"deny"`) || !strings.Contains(g, "Robin paused agents") || !strings.Contains(g, "41% of the week") {
		t.Errorf("gate while paused: %s", g)
	}
	if n := nudge("s1"); !strings.Contains(n, "note what is left") || strings.Contains(n, "deny") {
		t.Errorf("first nudge: %s", n)
	}
	if n := nudge("s1"); n != "" {
		t.Errorf("told twice at once: %s", n)
	}
	if n := nudge("s2"); n == "" {
		t.Error("another session wasn't told")
	}
	p := c.readPause()
	p.Told["s1"] = time.Now().Add(-time.Hour)
	c.writePause(p)
	if n := nudge("s1"); n != "" {
		t.Errorf("told again later: %s", n)
	}

	c.savePause(json.RawMessage(testPause))
	if n := nudge("s1"); n != "" {
		t.Errorf("the same pause told again: %s", n)
	}
	c.savePause(json.RawMessage(strings.Replace(testPause, `"id":3`, `"id":4`, 1)))
	if n := nudge("s1"); n == "" {
		t.Error("a new pause wasn't told")
	}

	p = c.readPause()
	p.Checked = time.Now().Add(-pauseFresh - time.Minute)
	c.writePause(p)
	if gate() != "" || c.activePause() != nil {
		t.Error("a stale copy still refuses")
	}

	c.savePause(json.RawMessage("null"))
	if gate() != "" || nudge("s3") != "" {
		t.Error("resumed, still refused")
	}
}

func TestPullPause(t *testing.T) {
	var asked atomic.Int32
	body := `{"pause":` + testPause + `}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/pause" || r.Method != "GET" {
			http.NotFound(w, r)
			return
		}
		asked.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "tok")
	c := &cli{dir: t.TempDir()}
	hookClaudePause(c, strings.NewReader(`{}`), false)
	if p := c.activePause(); p == nil || p.ID != 3 {
		t.Fatalf("pulled %+v", p)
	}
	hookClaudePause(c, strings.NewReader(`{}`), false)
	if n := asked.Load(); n != 1 {
		t.Errorf("asked %d times", n)
	}
	body = `{"pause":null}`
	p := c.readPause()
	p.Tried = time.Now().Add(-pausePullEvery - time.Second)
	c.writePause(p)
	hookClaudePause(c, strings.NewReader(`{}`), false)
	if c.activePause() != nil {
		t.Error("resume didn't clear the copy")
	}
}
