package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestParseWpCommand(t *testing.T) {
	for in, want := range map[string][3]string{
		"/wp":                    {"help", "", ""},
		"/wp next":               {"next", "next", ""},
		" /wp Pause ":            {"stop", "Pause", ""},
		"/wp btw use the v2 API": {"btw", "btw", "use the v2 API"},
		"/wp exe \"make check\"": {"exe", "exe", `"make check"`},
		"/wp btw\nline two":      {"btw", "btw", "line two"},
		"/wp frob":               {"", "frob", ""},
		"/wp review wp-12":       {"review", "review", "wp-12"},
		"/wp audit":              {"review", "audit", ""},
	} {
		cmd, word, rest, ok := parseWpCommand(in)
		if !ok || [3]string{cmd, word, rest} != want {
			t.Errorf("parseWpCommand(%q) = %q %q %q %v, want %q", in, cmd, word, rest, ok, want)
		}
	}
	for _, in := range []string{"/wpx", "please /wp next", "/keep", "wp next"} {
		if _, _, _, ok := parseWpCommand(in); ok {
			t.Errorf("%q read as a /wp command", in)
		}
	}
}

// The CLI's server commands are the server's: each one it sends, the
// server reads as the same command.
func TestWpServerCommandsMatchTheServer(t *testing.T) {
	for word, cmd := range wpCommands {
		server := core.SessionCommand("/wp " + word)
		if wpServerCommand(cmd) != (server != "") || (server != "" && server != cmd) {
			t.Errorf("/wp %s: CLI %q (server's: %v), server %q", word, cmd, wpServerCommand(cmd), server)
		}
	}
}

// The task key /wp review sends is the one the server reads.
func TestWpReviewKey(t *testing.T) {
	for _, rest := range []string{"", "wp-12", "WP-12 please", "please WP-12", "WP-", "12", "x-1-2", "Ab9-7"} {
		if got, want := wpReviewKey(rest), core.SessionReviewKey(rest); got != want {
			t.Errorf("wpReviewKey(%q) = %q, the server reads %q", rest, got, want)
		}
	}
	if wpReviewKey("wp-12") != "WP-12" {
		t.Errorf("wp-12 read as %q", wpReviewKey("wp-12"))
	}
}

func slashHook(t *testing.T, c *cli, session, cwd, prompt string) string {
	t.Helper()
	return slashHookIn(t, c, session, cwd, prompt, "")
}

// slashHookIn is slashHook for a session whose transcript is at transcript.
func slashHookIn(t *testing.T, c *cli, session, cwd, prompt, transcript string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"session_id": session, "hook_event_name": "UserPromptSubmit", "cwd": cwd, "prompt": prompt, "transcript_path": transcript})
	var out bytes.Buffer
	hookClaudeSlash(c, bytes.NewReader(b), &out)
	return out.String()
}

func stopReason(t *testing.T, out string) string {
	t.Helper()
	var r struct {
		Continue   *bool  `json:"continue"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.Continue == nil || *r.Continue {
		t.Fatalf("not a stopped prompt: %q", out)
	}
	return r.StopReason
}

// /wp btw is kept for the session's next real prompt (not a keep-warm
// ping, not another session's) and handed over once; a subagent's tool
// result never takes it, the agent's own does.
func TestWpBtw(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	if got := stopReason(t, slashHook(t, c, "s1", "/x", "/wp btw use the v2 API")); !strings.Contains(got, "noted") {
		t.Fatalf("/wp btw said %q", got)
	}
	slashHook(t, c, "s1", "/x", "/wp btw and keep the tests")
	if out := slashHook(t, c, "s2", "/x", "hello"); out != "" {
		t.Errorf("another session got the note: %q", out)
	}
	if out := slashHook(t, c, "s1", "/x", keepWarmMark+": still here"); out != "" {
		t.Errorf("a keep-warm ping took the note: %q", out)
	}
	out := slashHook(t, c, "s1", "/x", "<task-notification>done</task-notification>")
	if !strings.Contains(out, notesHead) || !strings.Contains(out, "- A note: use the v2 API\\n- A note: and keep the tests") || !strings.Contains(out, "additionalContext") {
		t.Fatalf("next prompt: %q", out)
	}
	if out := slashHook(t, c, "s1", "/x", "hello"); out != "" {
		t.Errorf("the note came twice: %q", out)
	}
	slashHook(t, c, "s1", "/x", "/wp btw one more")
	burn := func(agent string) string {
		b, _ := json.Marshal(map[string]any{"session_id": "s1", "hook_event_name": "PostToolUse", "tool_name": "Bash", "agent_id": agent})
		var out bytes.Buffer
		hookClaudeBurn(c, bytes.NewReader(b), &out)
		return out.String()
	}
	if out := burn("sub-1"); out != "" {
		t.Errorf("a subagent's tool result took the note: %q", out)
	}
	if out := burn(""); !strings.Contains(out, "one more") || !strings.Contains(out, `"PostToolUse"`) {
		t.Errorf("the agent's tool result: %q", out)
	}
	if got := stopReason(t, slashHook(t, c, "s1", "/x", "/wp btw")); !strings.Contains(got, "nothing noted") {
		t.Errorf("an empty /wp btw: %q", got)
	}
	// Where Claude Code kept the stopped /wp line and its answer for the
	// next request, the note isn't handed over a second time.
	slashHook(t, c, "s1", "/x", "/wp btw kept by Claude Code")
	transcript := keptTranscript(t, time.Now())
	if out := slashHookIn(t, c, "s1", "/x", "hello", transcript); out != "" {
		t.Errorf("a kept note came again: %q", out)
	}
	slashHook(t, c, "s1", "/x", "/wp btw before a kept stop's time")
	if out := slashHookIn(t, c, "s1", "/x", "hello", keptTranscript(t, time.Now().Add(-time.Minute))); !strings.Contains(out, "before a kept stop") {
		t.Errorf("an older kept stop hid the note: %q", out)
	}
}

// keptTranscript is a transcript in which Claude Code kept a prompt a hook
// stopped at at, as 2.1.325 writes it.
func keptTranscript(t *testing.T, at time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := `{"type":"user","message":{"role":"user","content":"hi"},"timestamp":"` + at.Add(-time.Hour).UTC().Format(time.RFC3339Nano) + `"}
{"type":"system","subtype":"informational","content":"Operation stopped by hook: Waypoint: noted","level":"warning","preventContinuation":true,"promptSubmitEnding":{"ending":{"kind":"stopped_by_hook","reason":"Waypoint: noted"},"promptKept":true},"timestamp":"` + at.UTC().Format(time.RFC3339Nano) + `"}
`
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// /wp exe runs the command in the session's directory and shows its end;
// a long one goes on running into its log.
func TestWpExe(t *testing.T) {
	// the test runs in agents' sessions too: a person drives this one
	for _, k := range []string{"WP_TASK", "WAYPOINT_FOLLOWUP", "WAYPOINT_UNATTENDED"} {
		t.Setenv(k, "")
	}
	c := &cli{dir: t.TempDir()}
	dir := t.TempDir()
	got := stopReason(t, slashHook(t, c, "s1", dir, `/wp exe "pwd; seq 1 100; exit 3"`))
	if !strings.Contains(got, "(exit 3,") || !strings.Contains(got, "\n100") || strings.Contains(got, "\n50\n") || !strings.HasPrefix(got, "$ pwd; seq 1 100; exit 3") {
		t.Fatalf("/wp exe said %q", got)
	}
	logs, _ := filepath.Glob(filepath.Join(c.dir, "exe", "*-s1.log"))
	if len(logs) != 1 {
		t.Fatalf("logs: %v", logs)
	}
	if b, _ := os.ReadFile(logs[0]); !strings.HasPrefix(string(b), dir+"\n1\n") {
		t.Errorf("log = %q", b)
	}
	// The agent reads the run with its next prompt: the command, how it
	// ended and the end of its output.
	next := slashHook(t, c, "s1", dir, "go on")
	for _, want := range []string{notesHead, "/wp exe in " + dir, "exit 3, took", "$ pwd; seq 1 100; exit 3", "\\n100\\n(whole output: "} {
		if !strings.Contains(next, want) {
			t.Errorf("next prompt lacks %q: %q", want, next)
		}
	}
	// Where Claude Code kept the answer for the agent, a run that ended
	// within the hook isn't told twice.
	stopReason(t, slashHook(t, c, "s1", dir, "/wp exe echo quick"))
	if out := slashHookIn(t, c, "s1", dir, "go on", keptTranscript(t, time.Now())); out != "" {
		t.Errorf("a kept run came again: %q", out)
	}
	old := exeWait
	exeWait = 200 * time.Millisecond
	t.Cleanup(func() { exeWait = old })
	exits := func() int {
		m, _ := filepath.Glob(filepath.Join(c.dir, "exe", "*-s1.log.exit"))
		return len(m)
	}
	ended := exits()
	got = stopReason(t, slashHook(t, c, "s1", dir, "/wp exe echo started; sleep 1; echo ended; exit 4"))
	if !strings.Contains(got, "still running after") || !strings.Contains(got, "started") {
		t.Errorf("a long command: %q", got)
	}
	// A run that outlived its hook is told as it ended, kept answer or not.
	if exits() != ended {
		t.Fatal("an exit file before the run ended")
	}
	for deadline := time.Now().Add(10 * time.Second); exits() == ended && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	next = slashHookIn(t, c, "s1", dir, "go on", keptTranscript(t, time.Now()))
	if !strings.Contains(next, "(exit 4)") || !strings.Contains(next, "started\\nended") {
		t.Errorf("the long run as it ended: %q", next)
	}
	t.Setenv("WP_TASK", "APP-1")
	if got := stopReason(t, slashHook(t, c, "s1", dir, "/wp exe touch x")); !strings.Contains(got, "nothing ran") || fileExists(filepath.Join(dir, "x")) {
		t.Errorf("in a task session: %q", got)
	}
}

// The server's commands and any other prompt are for the event hook: the
// slash hook says nothing.
func TestSlashHookLeavesTheRest(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	for _, p := range []string{"/wp next", "/wp stop", "/keep", "fix the bug"} {
		if out := slashHook(t, c, "s1", "/x", p); out != "" {
			t.Errorf("%q: %q", p, out)
		}
	}
	if got := stopReason(t, slashHook(t, c, "s1", "/x", "/wp frob")); !strings.Contains(got, "there's no /wp frob") || !strings.Contains(got, "/wp next") {
		t.Errorf("unknown: %q", got)
	}
	if got := stopReason(t, slashHook(t, c, "s1", "/x", "/wp")); got != wpHelp {
		t.Errorf("help: %q", got)
	}
}

func TestUnquoteCommand(t *testing.T) {
	for in, want := range map[string]string{
		`"make check"`:         "make check",
		`'ls -la'`:             "ls -la",
		`echo "a" "b"`:         `echo "a" "b"`,
		`"a" && "b"`:           `"a" && "b"`,
		` go test ./... `:      "go test ./...",
		`"unbalanced`:          `"unbalanced`,
		`grep -n "x" file.txt`: `grep -n "x" file.txt`,
	} {
		if got := unquoteCommand(in); got != want {
			t.Errorf("unquoteCommand(%q) = %q, want %q", in, got, want)
		}
	}
}
