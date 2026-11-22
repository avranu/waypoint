package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const waitOutstanding = `{"audit":12,"findings":["12.1","12.2"],"requests":["R-4"],"lines":"- 2 findings without a verdict in your review round's audit #12 (12.1, 12.2): ` + "`wp audit mark ID fixed`" + `.\n- 1 answered request you haven't acted on (R-4): ` + "`wp request ls --answered`" + `."}`

// waitHook runs the hook against a server that answers body with status and
// returns what it printed and how often it was asked.
func waitHook(t *testing.T, status int, body string, input map[string]any) (string, int32) {
	t.Helper()
	var asked atomic.Int32
	var session string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/outstanding" || r.Method != http.MethodPost {
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
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
	if os.Getenv("WAYPOINT_SLOT_DIR") == "" {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
	}
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

// bashCall is a PostToolUse event for a Bash call; background says Claude
// Code was told to run it in the background.
func bashCall(command string, background bool) map[string]any {
	in := map[string]any{"command": command}
	if background {
		in["run_in_background"] = true
	}
	return map[string]any{"session_id": "sess-1", "hook_event_name": "PostToolUse", "tool_name": "Bash", "cwd": "/tmp", "tool_input": in}
}

// autoBackgrounded is a call Claude Code moved to the background itself.
func autoBackgrounded(command string) map[string]any {
	in := bashCall(command, false)
	in["tool_response"] = map[string]any{"backgroundTaskId": "b1x2y3", "stdout": ""}
	return in
}

func waitContext(t *testing.T, out string) string {
	t.Helper()
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
	return got.Out.Context
}

func TestWaitHookNamesOutstandingWorkOfABackgroundCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{"run_in_background", bashCall("sleep 600", true)},
		{"background testsum", bashCall("wp testsum -- make check", true)},
		{"auto-backgrounded", autoBackgrounded("make check")},
		{"auto-backgrounded wp pr wait", autoBackgrounded("wp pr wait 12")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, asked := waitHook(t, 200, waitOutstanding, tc.input)
			if asked != 1 {
				t.Fatalf("the server was asked %d times", asked)
			}
			ctx := waitContext(t, out)
			for _, want := range []string{"12.1, 12.2", "wp request ls --answered", "doesn't need"} {
				if !strings.Contains(ctx, want) {
					t.Errorf("context lacks %q:\n%s", want, ctx)
				}
			}
		})
	}
}

// A foreground call is over when the hook runs: nothing has started, and the
// hook neither asks the server (which records what it names) nor speaks.
func TestWaitHookIsSilentForForegroundCalls(t *testing.T) {
	for _, command := range []string{"wp testsum -- make check", "make check", "wp pr wait 12", "wp slot run tests -- go test ./...", "cd /repo && make check"} {
		t.Run(command, func(t *testing.T) {
			out, asked := waitHook(t, 200, waitOutstanding, bashCall(command, false))
			if out != "" || asked != 0 {
				t.Errorf("printed %q after asking %d times", out, asked)
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
		{"no outstanding work", 200, `{}`, bashCall("make check", true), 1},
		{"empty lines", 200, `{"lines":""}`, bashCall("sleep 9", true), 1},
		{"a Bash call that isn't a wait", 200, waitOutstanding, bashCall("ls -la", false), 0},
		{"a response without a task id", 200, waitOutstanding, func() map[string]any {
			in := bashCall("ls", false)
			in["tool_response"] = map[string]any{"stdout": "x"}
			return in
		}(), 0},
		{"not Bash", 200, waitOutstanding, map[string]any{"session_id": "sess-1", "hook_event_name": "PostToolUse", "tool_name": "Read", "tool_input": map[string]any{"run_in_background": true}}, 0},
		{"not PostToolUse", 200, waitOutstanding, map[string]any{"session_id": "sess-1", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "make check", "run_in_background": true}}, 0},
		{"the server errors", 500, `boom`, bashCall("make check", true), 1},
		{"the server says no", 403, `{"error":"no"}`, bashCall("make check", true), 1},
		{"a reply that isn't JSON", 200, `<html>`, bashCall("make check", true), 1},
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
	t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
	var out bytes.Buffer
	c, err := newCLI(&out, &out)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(bashCall("make check", true))
	var stdout bytes.Buffer
	hookClaudeWait(c, bytes.NewReader(b), &stdout)
	if stdout.Len() != 0 {
		t.Errorf("printed %q", stdout.String())
	}
}

func TestWaitETACommand(t *testing.T) {
	for _, tc := range []struct {
		command string
		name    string
		cmd     string
		n       int
		ok      bool
	}{
		{"wp testsum -- make check", "tests", "make check", 0, true},
		{"cd /r && wp testsum -- go test ./... | tail", "tests", "go test ./...", 0, true},
		{"wp slot run tests -- go test ./...", "tests", "go test ./...", 0, true},
		{"wp slot run build -n 2 -- make all", "build", "make all", 2, true},
		{"./wp slot run tests -n1 -- sh -c 'echo hi there'", "tests", "sh -c echo hi there", 1, true},
		{"wp testsum go test ./...", "", "", 0, false},
		{"make check", "", "", 0, false},
		{"echo wp testsum -- x", "", "", 0, false},
	} {
		name, cmd, n, ok := waitETACommand(tc.command)
		if name != tc.name || cmd != tc.cmd || n != tc.n || ok != tc.ok {
			t.Errorf("%q = %q %q %d %v, want %q %q %d %v", tc.command, name, cmd, n, ok, tc.name, tc.cmd, tc.n, tc.ok)
		}
	}
}

// seedRuns writes finished runs of cmd in repo, each mins long.
func seedRuns(t *testing.T, name, cmd, repo string, mins ...float64) {
	t.Helper()
	dir, _ := slotDir()
	_ = os.MkdirAll(dir, 0o700)
	var lines []string
	for i, m := range mins {
		b, _ := json.Marshal(slotRunRec{Name: name, Cmd: cmd, Repo: repo, At: int64(1000 + i), Secs: m * 60})
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(filepath.Join(dir, slotRunsFile), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// holdSlot holds the slot as a command that began ago before.
func holdSlot(t *testing.T, name string, ago time.Duration, args string) {
	t.Helper()
	dir, _ := slotDir()
	h, err := acquireSlot(t.Context(), name, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.release)
	f, err := os.OpenFile(filepath.Join(dir, name+".0.lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte("1\t"+strconv.FormatInt(time.Now().Add(-ago).Unix(), 10)+"\t/r\t"+args+"\n"), 0)
	_ = f.Close()
}

// Nobody sees the stderr line while the agent waits: a backgrounded run's
// output sits in a file. The hook says the ETA instead, from the slot
// directory, with or without outstanding work.
func TestWaitHookSaysTheETAOfABackgroundedRun(t *testing.T) {
	t.Run("a free slot: running", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "make check", "/tmp", 30, 30, 30)
		out, _ := waitHook(t, 200, `{}`, bashCall("wp testsum -- make check", true))
		ctx := waitContext(t, out)
		if !strings.Contains(ctx, "The test is running, and is expected to complete in 36 minutes.") || !strings.Contains(ctx, "end your turn") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("the run itself already holds the slot", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", time.Second, "slot run tests -n 1 -- go test ./...")
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "is running, and is expected to complete in 36 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("someone else holds it: queued", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", 10*time.Minute, "slot run tests -n 1 -- go test ./...")
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		// (20 left + 30 run) * 1.2
		if ctx := waitContext(t, out); !strings.Contains(ctx, "The test is queued, and is expected to complete in 60 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("queued behind a waiter", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", 10*time.Minute, "slot run tests -n 1 -- go test ./...")
		dir, _ := slotDir()
		_ = os.WriteFile(filepath.Join(dir, "tests."+strconv.Itoa(os.Getppid())+".wait"), []byte(strconv.FormatInt(time.Now().Add(-time.Minute).UnixNano(), 10)), 0o600)
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		// (20 + 30 for the one ahead + 30) * 1.2
		if ctx := waitContext(t, out); !strings.Contains(ctx, "is expected to complete in 96 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("with outstanding work: the ETA first", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "make check", "/tmp", 30, 30, 30)
		out, asked := waitHook(t, 200, waitOutstanding, bashCall("wp testsum -- make check", true))
		ctx := waitContext(t, out)
		i, j := strings.Index(ctx, "expected to complete"), strings.Index(ctx, "12.1, 12.2")
		if asked != 1 || i < 0 || j < 0 || i > j {
			t.Errorf("asked %d; context:\n%s", asked, ctx)
		}
	})
	t.Run("the server failing leaves the ETA", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "make check", "/tmp", 30, 30, 30)
		out, _ := waitHook(t, 500, `boom`, bashCall("wp testsum -- make check", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "expected to complete in 36 minutes") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("no history: no ETA", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		out, _ := waitHook(t, 200, `{}`, bashCall("wp testsum -- make check", true))
		if out != "" {
			t.Errorf("printed %q", out)
		}
	})
	t.Run("another command's history is not this one's", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "go vet ./...", "/tmp", 30)
		out, _ := waitHook(t, 200, `{}`, bashCall("wp testsum -- make check", true))
		if out != "" {
			t.Errorf("printed %q", out)
		}
	})
}
