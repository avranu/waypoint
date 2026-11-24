package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

const waitOutstanding = `{"audit":12,"findings":["12.1","12.2"],"requests":["R-4"],"request_ids":[4],"lines":"- 2 findings still to do in your review round's audit #12, below.\n### 12.1 [high] Merge reads a nil map\n### 12.2 [low] Dead code\n\nWaypoint: a request you filed was decided.\n\nWP:R-4 (question) \"Squash?\": Sam answered:\nNo, keep them apart."}`

// waitHook runs the hook against a server that answers body with status and
// returns what it printed and how often it was asked.
func waitHook(t *testing.T, status int, body string, input map[string]any) (string, int32) {
	t.Helper()
	out, asked, _ := waitHookTold(t, status, body, input)
	return out, asked
}

// waitHookTold is waitHook, also returning the requests the hook said it
// told (nil: it didn't say).
func waitHookTold(t *testing.T, status int, body string, input map[string]any) (string, int32, []int64) {
	t.Helper()
	var asked atomic.Int32
	var session string
	var told []int64
	var stdout lockedBuffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/outstanding/told" {
			var in struct {
				Requests []int64 `json:"requests"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("told: %v", err)
			}
			told = in.Requests
			// An answer is told only once the hook has printed it.
			if stdout.String() == "" {
				t.Errorf("told %v before the hook printed anything", in.Requests)
			}
			if r.Header.Get("X-Waypoint-Session") != "sess-1" {
				t.Errorf("told for session %q", r.Header.Get("X-Waypoint-Session"))
			}
			return
		}
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
	hookClaudeWait(c, bytes.NewReader(b), &stdout)
	if asked.Load() > 0 && session != "sess-1" {
		t.Errorf("the server was asked for session %q", session)
	}
	return stdout.String(), asked.Load(), told
}

// lockedBuffer is the hook's stdout, which the test's server reads while the
// hook is still running.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
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
			for _, want := range []string{"12.1 [high] Merge reads a nil map", "Sam answered:\nNo, keep them apart.", "Work of yours to do while it runs (leave anything that needs its result):"} {
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
	seedRuns(t, "tests", "make check", "/tmp", 30, 30, 30)
	b, _ = json.Marshal(bashCall("wp testsum -- make check", true))
	stdout.Reset()
	hookClaudeWait(c, bytes.NewReader(b), &stdout)
	if ctx := waitContext(t, stdout.String()); !strings.Contains(ctx, "is expected to complete in 36 minutes.") {
		t.Errorf("context:\n%s", ctx)
	}
}

// The hook reads the server's own type, so a renamed field can't leave it
// printing nothing (audit #1372.21).
func TestWaitHookReadsTheServersOutstanding(t *testing.T) {
	b, _ := json.Marshal(core.SessionOutstanding{Audit: 3, Findings: []string{"3.1"}, Lines: "- 1 finding still to do in audit #3"})
	out, _ := waitHook(t, 200, string(b), bashCall("sleep 600", true))
	if ctx := waitContext(t, out); !strings.Contains(ctx, "- 1 finding still to do in audit #3") {
		t.Errorf("context:\n%s", ctx)
	}
}

// A slow ETA (git, the slot directory) doesn't cost the outstanding work:
// the hook says what it has by its budget, inside Claude Code's timeout
// (audit #1372.13).
func TestWaitHookAnswersWithinItsBudget(t *testing.T) {
	oldETA, oldBudget := waitETAFor, waitBudget
	release := make(chan struct{})
	waitETAFor = func(string, string, string) string { <-release; return "late ETA" }
	waitBudget = 300 * time.Millisecond
	t.Cleanup(func() { close(release); waitETAFor, waitBudget = oldETA, oldBudget })
	start := time.Now()
	out, _ := waitHook(t, 200, waitOutstanding, bashCall("wp testsum -- make check", true))
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v", took)
	}
	if ctx := waitContext(t, out); !strings.Contains(ctx, "12.1 [high]") || strings.Contains(ctx, "late ETA") {
		t.Errorf("context:\n%s", ctx)
	}
}

func TestWaitHookNeedsASession(t *testing.T) {
	t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
	seedRuns(t, "tests", "make check", "/tmp", 30, 30, 30)
	in := bashCall("wp testsum -- make check", true)
	in["session_id"] = ""
	out, asked := waitHook(t, 200, waitOutstanding, in)
	if out != "" || asked != 0 {
		t.Errorf("printed %q after asking %d times", out, asked)
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
		{"wp slot run build -n=2 -- make all", "build", "make all", 2, true},
		{"wp slot run build -n 3 -- make all", "build", "make all", 3, true},
		{"wp testsum -- make check 2>&1 | tail -5", "tests", "make check", 0, true},
		{"wp testsum -- make check > /tmp/log 2>&1", "tests", "make check", 0, true},
		{"wp testsum -- make check >/tmp/log", "tests", "make check", 0, true},
		{"wp testsum -- make check >>/tmp/log 2>/dev/null", "tests", "make check", 0, true},
		{"wp testsum -- make check &>/tmp/log", "tests", "make check", 0, true},
		{"wp testsum -- make check <in", "tests", "make check", 0, true},
		{"timeout 900 wp testsum -- make check", "tests", "make check", 0, true},
		{"timeout -s KILL 900 wp testsum -- make check", "tests", "make check", 0, true},
		{"timeout --signal TERM -k 10 900 wp testsum -- make check", "tests", "make check", 0, true},
		{"env -u NAME wp testsum -- make check", "tests", "make check", 0, true},
		{"env -C /r X=1 wp testsum -- make check", "tests", "make check", 0, true},
		{"stdbuf -o L wp testsum -- make check", "tests", "make check", 0, true},
		{"nice --adjustment 5 wp testsum -- make check", "tests", "make check", 0, true},
		{"nice -n 5 wp slot run tests -- go test ./...", "tests", "go test ./...", 0, true},
		{"FOO=1 nohup env X=2 wp testsum -- make check", "tests", "make check", 0, true},
		{"bash -c 'wp testsum -- make check'", "tests", "make check", 0, true},
		{"timeout 5 bash -c \"cd /r && wp testsum -- make check\"", "tests", "make check", 0, true},
		{"wp testsum --repeat 3 -- make check", "", "", 0, false},
		{"wp testsum --repeat=3 -- make check", "", "", 0, false},
		{"wp testsum --base main -- make check", "", "", 0, false},
		{"wp testsum --base=main -- make check", "", "", 0, false},
		{"wp slot run 'a b' -- x", "", "", 0, false},
		{"wp testsum --", "", "", 0, false},
		{"wp slot run tests --", "", "", 0, false},
		{"wp testsum go test ./...", "tests", "go test ./...", 0, true},
		{"wp testsum go test -json ./...", "tests", "go test ./...", 0, true},
		{"wp testsum --keep-env GH_TOKEN --format=go make check", "tests", "make check", 0, true},
		{"wp testsum --format go -- make check", "tests", "make check", 0, true},
		{"wp testsum --until-fail make check", "", "", 0, false},
		{"wp testsum --repeat 3 make check", "", "", 0, false},
		{"wp testsum --bogus make check", "", "", 0, false},
		{"wp testsum -", "", "", 0, false},
		{"wp testsum", "", "", 0, false},
		{"wp >/tmp/log", "", "", 0, false},
		{`wp testsum -- make check >"/tmp/o.log"`, "tests", "make check", 0, true},
		{`wp testsum -- make check 2>"$LOG" >'f'`, "tests", "make check", 0, true},
		{`wp testsum -- grep \> f`, "tests", "grep > f", 0, true},
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
	holdSlotPid(t, name, 1, ago, args)
}

// holdSlotPid is holdSlot as the process pid.
func holdSlotPid(t *testing.T, name string, pid int, ago time.Duration, args string) {
	t.Helper()
	holdSlotAs(t, name, pid, ago, args, "")
}

// holdSlotAs is holdSlotPid recording the holder's session (none: the
// four-field line of a wp from before it was recorded).
func holdSlotAs(t *testing.T, name string, pid int, ago time.Duration, args, session string) {
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
	line := strconv.Itoa(pid) + "\t" + strconv.FormatInt(time.Now().Add(-ago).Unix(), 10) + "\t/r\t" + args
	if session != "" {
		line += "\t" + session
	}
	_, _ = f.WriteAt([]byte(line+"\n"), 0)
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
		if ctx := waitContext(t, out); !strings.Contains(ctx, "The test is queued, and is expected to complete in 60 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("queued behind a waiter", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", 10*time.Minute, "slot run tests -n 1 -- go test ./...")
		dir, _ := slotDir()
		writeWaiter(t, dir, os.Getppid(), time.Now().Add(-time.Minute).UnixNano())
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "is expected to complete in 96 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("another session's identical run < 15s old holds it: queued", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		stubPidSessions(t, map[int]string{1: "sess-2"})
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", time.Second, "slot run tests -n 1 -- go test ./...")
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "The test is queued") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("its own run, past 15s on the slot: running", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		stubPidSessions(t, map[int]string{1: "sess-1"})
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", 20*time.Second, "slot run tests -n 1 -- go test ./...")
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "The test is running") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("its own waiter file is not ahead of it", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		stubPidSessions(t, map[int]string{1: "sess-2", os.Getppid(): "sess-1"})
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", 10*time.Minute, "slot run tests -n 1 -- go test ./...")
		dir, _ := slotDir()
		writeWaiter(t, dir, os.Getppid(), time.Now().Add(-time.Minute).UnixNano())
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "is expected to complete in 60 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("only waiters older than its own are ahead", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		stubPidSessions(t, map[int]string{1: "sess-2", os.Getppid(): "sess-1", os.Getpid(): "sess-3"})
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlotPid(t, "tests", 4194000, 10*time.Minute, "slot run tests -n 1 -- go test ./...")
		dir, _ := slotDir()
		now := time.Now()
		writeWaiter(t, dir, os.Getppid(), now.Add(-time.Minute).UnixNano())
		writeWaiter(t, dir, 1, now.Add(-5*time.Minute).UnixNano())
		writeWaiter(t, dir, os.Getpid(), now.Add(-10*time.Second).UnixNano())
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "is expected to complete in 96 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("a young holder of another command is not the run itself: queued", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", time.Second, "slot run tests -n 1 -- make lint")
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "The test is queued") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("two slots, one held by someone else: running", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", 10*time.Minute, "slot run tests -n 1 -- go test ./...")
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 2 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "The test is running") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("no session to read (darwin): its own new waiter is not ahead of it", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		stubPidSessions(t, map[int]string{})
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", 10*time.Minute, "slot run tests -n 1 -- go test ./...")
		dir, _ := slotDir()
		writeWaiter(t, dir, os.Getppid(), time.Now().Add(-2*time.Second).UnixNano())
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		// (20 left + 30 run) * 1.2 is the expected wait, in minutes.
		if ctx := waitContext(t, out); !strings.Contains(ctx, "is expected to complete in 60 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	// Where no process's session can be read (darwin), the session each run recorded says whose it is.
	t.Run("no session to read: another session's identical run 1s old holds it: queued", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		stubPidSessions(t, map[int]string{})
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlotAs(t, "tests", 1, time.Second, "slot run tests -n 1 -- go test ./...", "sess-2")
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "The test is queued") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("no session to read: its own waiter minutes old (auto-backgrounded) is not ahead of it", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		stubPidSessions(t, map[int]string{})
		seedRuns(t, "tests", "go test ./...", "/tmp", 30, 30, 30)
		holdSlot(t, "tests", 10*time.Minute, "slot run tests -n 1 -- go test ./...")
		dir, _ := slotDir()
		writeWaiterAs(t, dir, os.Getppid(), time.Now().Add(-2*time.Minute).UnixNano(), "sess-1")
		out, _ := waitHook(t, 200, `{}`, bashCall("wp slot run tests -n 1 -- go test ./...", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "is expected to complete in 60 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	// A quoted '>' is an argument, not a redirection: the key is the one the run records.
	t.Run("a quoted > is an argument", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "grep > file", "/tmp", 30, 30, 30)
		out, _ := waitHook(t, 200, `{}`, bashCall("wp testsum -- grep '>' file", true))
		if ctx := waitContext(t, out); !strings.Contains(ctx, "is expected to complete in 36 minutes.") {
			t.Errorf("context:\n%s", ctx)
		}
	})
	t.Run("with outstanding work: the ETA first", func(t *testing.T) {
		t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
		seedRuns(t, "tests", "make check", "/tmp", 30, 30, 30)
		out, asked := waitHook(t, 200, waitOutstanding, bashCall("wp testsum -- make check", true))
		ctx := waitContext(t, out)
		i, j := strings.Index(ctx, "expected to complete"), strings.Index(ctx, "12.1 [high]")
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

func TestShellCommandsKeepRedirectionsInTheCommand(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"a 2>&1 | b", "[[a 2>&1] [b]]"},
		{"a >&2; b", "[[a >&2] [b]]"},
		{"a <&3 && b", "[[a <&3] [b]]"},
		{"a &>f; b", "[[a &>f] [b]]"},
		{"a &>>f & b", "[[a &>>f] [b]]"},
		{"a & b", "[[a] [b]]"},
		{"a && b", "[[a] [b]]"},
		{"a |& b", "[[a] [b]]"},
	} {
		if got := fmt.Sprint(shellCommands(tc.in)); got != tc.want {
			t.Errorf("%q = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// Answers are marked told only once the hook has printed them, and only those
// the server sent; with none, or no output, it says nothing.
func TestWaitHookSaysItToldAfterPrinting(t *testing.T) {
	out, _, told := waitHookTold(t, 200, waitOutstanding, bashCall("make check", true))
	if !strings.Contains(out, "No, keep them apart.") || !slices.Equal(told, []int64{4}) {
		t.Errorf("told %v after printing:\n%s", told, out)
	}
	_, _, told = waitHookTold(t, 200, `{"audit":12,"findings":["12.1"],"lines":"- 1 finding"}`, bashCall("make check", true))
	if told != nil {
		t.Errorf("told %v with no answers", told)
	}
	_, _, told = waitHookTold(t, 200, waitOutstanding, bashCall("make check", false))
	if told != nil {
		t.Errorf("told %v for a foreground call", told)
	}
}
