package main

import (
	"bytes"
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

func TestSessionStartResumeSkipsDuplicateHint(t *testing.T) {
	t.Setenv("WAYPOINT_SESSION", "")
	var reply string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "tok")
	dir := t.TempDir()
	c := &cli{dir: t.TempDir()}

	call := func(source string) string {
		in, _ := json.Marshal(map[string]string{
			"session_id":      "sess-resume",
			"hook_event_name": "SessionStart",
			"source":          source,
			"cwd":             dir,
		})
		var out bytes.Buffer
		hookClaudeEvent(c, bytes.NewReader(in), &out, false)
		return out.String()
	}

	reply = `{}`
	if got := call("startup"); !strings.Contains(got, "wp syntax") {
		t.Fatalf("startup should include sessionHint, got %q", got)
	}
	if got := call("resume"); got != "" {
		t.Fatalf("resume with no dynamic text should not duplicate sessionHint, got %q", got)
	}
	reply = `{"text":"Decision arrived"}`
	got := call("resume")
	if !strings.Contains(got, "Decision arrived") || strings.Contains(got, "wp syntax") {
		t.Fatalf("resume should emit dynamic text only without duplicating sessionHint, got %q", got)
	}
}
