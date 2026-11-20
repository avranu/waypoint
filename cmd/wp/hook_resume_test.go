package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The prompt hook resumes the directory's task as the prompt's own session
// and asks the server to resume it only if that session was working on it:
// the claim may be one another session in the directory took (WP-720).
func TestHookResumeAsksOnlyOwn(t *testing.T) {
	t.Setenv("WAYPOINT_SESSION", "")
	type call struct {
		path, session string
		body          map[string]any
	}
	var got []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, call{r.URL.Path, r.Header.Get("X-Waypoint-Session"), body})
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "tok")
	dir := t.TempDir()
	c := &cli{dir: t.TempDir()}
	c.state.Claims = map[string]*claim{dir: {Key: "WP-688", Session: "old-session", Checked: true}}
	c.state.Cwds = map[string]*cwdSession{dir: {Session: "old-session"}}
	in, _ := json.Marshal(map[string]string{"session_id": "new-session", "cwd": dir, "prompt": "Handle the next task"})
	hookClaudeResume(c, strings.NewReader(string(in)))
	if len(got) != 1 || got[0].path != "/api/v1/agent/resume/WP-688" || got[0].session != "new-session" || got[0].body["only_own"] != true {
		t.Fatalf("calls = %+v", got)
	}
}
