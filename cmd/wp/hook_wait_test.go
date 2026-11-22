package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const waitOutstanding = `{"audit":12,"findings":["12.1","12.2"],"requests":["R-4"],"lines":"- 2 findings without a verdict in your review round's audit #12 (12.1, 12.2): ` + "`wp audit mark ID fixed`" + `.\n- 1 answered request you haven't acted on (R-4): ` + "`wp request ls --answered`" + `."}`

// waitHook runs the hook against a server that answers body with status and
// returns what it printed and how often it was asked.
func waitHook(t *testing.T, status int, body string, input map[string]any) (string, int32) {
	t.Helper()
	var asked atomic.Int32
	var session string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/outstanding" {
			t.Errorf("asked %s", r.URL.Path)
		}
		asked.Add(1)
		session = r.Header.Get("X-Waypoint-Session")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "tok")
	t.Setenv("WAYPOINT_AGENT", "anthropic/claude-opus-5-5")
	t.Setenv("WAYPOINT_SESSION", "")
	var out bytes.Buffer
	c, err := newCLI(&out, &out)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(input)
	var stdout bytes.Buffer
	hookClaudeWait(c, bytes.NewReader(b), &stdout)
	if asked.Load() > 0 && session != "sess-1" {
		t.Errorf("the server was asked for session %q", session)
	}
	return stdout.String(), asked.Load()
}

func bashCall(command string, background bool) map[string]any {
	in := map[string]any{"command": command}
	if background {
		in["run_in_background"] = true
	}
	return map[string]any{"session_id": "sess-1", "hook_event_name": "PostToolUse", "tool_name": "Bash", "cwd": "/tmp", "tool_input": in}
}

func TestWaitHookNamesOutstandingWork(t *testing.T) {
	for _, tc := range []struct {
		name       string
		command    string
		background bool
	}{
		{"background call", "sleep 600", true},
		{"wp testsum", "wp testsum -- make check", false},
		{"wp slot run", "wp slot run tests -- go test ./...", false},
		{"wp pr wait", "wp pr wait 12", false},
		{"make check", "make check", false},
		{"after cd", "cd /repo && make check", false},
		{"piped", "wp pr wait 12 2>&1 | tail -5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, asked := waitHook(t, 200, waitOutstanding, bashCall(tc.command, tc.background))
			if asked != 1 {
				t.Fatalf("the server was asked %d times", asked)
			}
			var got struct {
				Out struct {
					Event   string `json:"hookEventName"`
					Context string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output %q: %v", out, err)
			}
			if got.Out.Event != "PostToolUse" {
				t.Errorf("event = %q", got.Out.Event)
			}
			for _, want := range []string{"12.1, 12.2", "wp request ls --answered", "doesn't need"} {
				if !strings.Contains(got.Out.Context, want) {
					t.Errorf("context lacks %q:\n%s", want, got.Out.Context)
				}
			}
		})
	}
}

func TestWaitHookIsSilentWithNothingToSay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		input  map[string]any
		asked  int32
	}{
		{"no outstanding work", 200, `{}`, bashCall("make check", false), 1},
		{"empty lines", 200, `{"lines":""}`, bashCall("sleep 9", true), 1},
		{"a Bash call that isn't a wait", 200, waitOutstanding, bashCall("ls -la", false), 0},
		{"a name that only contains a wait", 200, waitOutstanding, bashCall("echo make checks", false), 0},
		{"a command that mentions one in a string", 200, waitOutstanding, bashCall(`git commit -m "run wp testsum"`, false), 0},
		{"not Bash", 200, waitOutstanding, map[string]any{"session_id": "sess-1", "hook_event_name": "PostToolUse", "tool_name": "Read", "tool_input": map[string]any{"run_in_background": true}}, 0},
		{"not PostToolUse", 200, waitOutstanding, map[string]any{"session_id": "sess-1", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "make check"}}, 0},
		{"the server errors", 500, `boom`, bashCall("make check", false), 1},
		{"the server says no", 403, `{"error":"no"}`, bashCall("make check", false), 1},
		{"a reply that isn't JSON", 200, `<html>`, bashCall("make check", false), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, asked := waitHook(t, tc.status, tc.body, tc.input)
			if out != "" {
				t.Errorf("printed %q", out)
			}
			if asked != tc.asked {
				t.Errorf("asked the server %d times, want %d", asked, tc.asked)
			}
		})
	}
}

func TestWaitHookWithNoServerIsSilent(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "")
	t.Setenv("WAYPOINT_TOKEN", "")
	var out bytes.Buffer
	c, err := newCLI(&out, &out)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(bashCall("make check", false))
	var stdout bytes.Buffer
	hookClaudeWait(c, bytes.NewReader(b), &stdout)
	if stdout.Len() != 0 {
		t.Errorf("printed %q", stdout.String())
	}
}
