package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusalCases are one triggering Bash call per refusal kind (WP-1411).
func refusalCases(t *testing.T) []struct {
	kind, cmd, want string
	input           map[string]any
	cwd             string
} {
	t.Helper()
	f := newTidyFixture(t)
	dir := t.TempDir()
	tasks := filepath.Join(dir, "s", "tasks")
	if err := os.MkdirAll(tasks, 0o700); err != nil {
		t.Fatal(err)
	}
	running := filepath.Join(tasks, "brun1.output")
	if err := os.WriteFile(running, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held := heldOpen
	t.Cleanup(func() { heldOpen = held })
	heldOpen = func(file string) bool { return file == running }
	return []struct {
		kind, cmd, want string
		input           map[string]any
		cwd             string
	}{
		{"grok-cut-short", "", "wp audit", map[string]any{"command": "timeout 580 agent -p --model grok-4.7-high x", "timeout": 600000}, dir},
		{"pgrep-self", "", "[g]olangci-lint run", map[string]any{"command": pgrepLoop}, dir},
		{"worktree-place", "", ".claude/worktrees/x", map[string]any{"command": "git worktree add ../repo-x -b x"}, f.repo},
		{"task-wait", "", "brun1 is still running", map[string]any{"command": "sleep 60; cat " + running}, dir},
	}
}

func runHook(h func(*cli, io.Reader, io.Writer), c *cli, cwd string, input map[string]any) string {
	in, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": cwd, "tool_input": input})
	var out bytes.Buffer
	h(c, bytes.NewReader(in), &out)
	return out.String()
}

// The guard hook emits every refusal, in an attended session and with the
// guard off; the nudge hook emits none and the counter still counts.
func TestRefusalsComeFromGuard(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WAYPOINT_FOLLOWUP", "")
	t.Setenv("WAYPOINT_UNATTENDED", "")
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	for _, tc := range refusalCases(t) {
		for _, mode := range []string{"", "off"} {
			t.Run(tc.kind+"/guard="+mode, func(t *testing.T) {
				t.Setenv("WP_GUARD", mode)
				c := &cli{dir: t.TempDir()}
				out := runHook(hookClaudeGuard, c, tc.cwd, tc.input)
				if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, tc.want) {
					t.Errorf("guard output %q, want a deny with %q", out, tc.want)
				}
				if st := c.loadState().Nudges[tc.kind]; st == nil || st.Shown != 1 {
					t.Errorf("Nudges[%s] = %+v, want Shown 1", tc.kind, st)
				}
			})
		}
		t.Run(tc.kind+"/nudge", func(t *testing.T) {
			out := runHook(hookClaudeNudge, &cli{dir: t.TempDir()}, tc.cwd, tc.input)
			if out != "" {
				t.Errorf("nudge output %q, want nothing on a refused call", out)
			}
		})
	}
}

// A live untold pause no longer hides a refusal, and the refused call does
// not use up the pause's tell: the next call still hears it.
func TestPauseDoesNotHideRefusal(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GUARD", "")
	t.Setenv("HOME", t.TempDir())
	c := &cli{dir: t.TempDir()}
	c.savePause(json.RawMessage(testPause))
	input := map[string]any{"command": pgrepLoop}
	if out := runHook(hookClaudeGuard, c, t.TempDir(), input); !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("guard with a pause: %q", out)
	}
	if out := runHook(hookClaudeNudge, c, t.TempDir(), input); out != "" {
		t.Errorf("nudge on a refused call: %q", out)
	}
	if out := runHook(hookClaudeNudge, c, t.TempDir(), map[string]any{"command": "ls"}); !strings.Contains(out, "note what is left") {
		t.Errorf("the call after a refusal did not hear the pause: %q", out)
	}
}

// Each hook scans /proc for a task's output at most once per Bash call
// (WP-1398): the nudge hook once whatever the outcome, the guard hook only for
// a command it could refuse (a sleep before the read). The two hooks are
// separate processes, so a sleep before the read is scanned once in each; any
// other command is scanned once in all.
func TestTaskOutputScannedOnce(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GUARD", "")
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tasks := filepath.Join(dir, "s", "tasks")
	if err := os.MkdirAll(tasks, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tasks, "brun1.output")
	if err := os.WriteFile(out, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := heldOpen
	t.Cleanup(func() { heldOpen = old })
	for _, held := range []bool{true, false} {
		scans := 0
		heldOpen = func(string) bool { scans++; return held }
		input := map[string]any{"command": "tail -5 " + out}
		got := runHook(hookClaudeNudge, &cli{dir: t.TempDir()}, dir, input)
		if scans != 1 {
			t.Errorf("held=%v: nudge scanned %d times, want 1", held, scans)
		}
		if held != strings.Contains(got, "is still running") {
			t.Errorf("held=%v: nudge output %q", held, got)
		}
		scans = 0
		if got := runHook(hookClaudeGuard, &cli{dir: t.TempDir()}, dir, input); scans != 0 || got != "" {
			t.Errorf("held=%v: guard scanned %d times, want 0; output %q", held, scans, got)
		}
		scans = 0
		got = runHook(hookClaudeGuard, &cli{dir: t.TempDir()}, dir, map[string]any{"command": "sleep 30; tail -5 " + out})
		if scans != 1 || held != strings.Contains(got, `"permissionDecision":"deny"`) {
			t.Errorf("held=%v: guard on a sleep scanned %d times, want 1; output %q", held, scans, got)
		}
		scans = 0
		got = runHook(hookClaudeNudge, &cli{dir: t.TempDir()}, dir, map[string]any{"command": "sleep 30; tail -5 " + out})
		if scans != 1 || (held && got != "") {
			t.Errorf("held=%v: nudge on a sleep scanned %d times, want 1; output %q", held, scans, got)
		}
	}
}

// A call that is both a nudge refusal and a guard hit in an unattended
// session is refused once with both reasons, and guard.log keeps its line
// (WP-1398): the refusal must not hide the guard's finding until a retry.
func TestRefusalKeepsGuardHit(t *testing.T) {
	_, repo := guardTestSite(t)
	t.Setenv("WP_TASK", "WP-1")
	cmd := pgrepLoop + "; sudo rm -rf /x"
	for _, mode := range []string{"", "warn", "off"} {
		t.Run("guard="+mode, func(t *testing.T) {
			t.Setenv("WP_GUARD", mode)
			cfg := t.TempDir()
			c := &cli{dir: cfg}
			out := runHook(hookClaudeGuard, c, repo, map[string]any{"command": cmd})
			if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "[g]olangci-lint run") {
				t.Fatalf("output %q, want the pgrep-self refusal", out)
			}
			b, _ := os.ReadFile(filepath.Join(cfg, "guard.log"))
			if mode == "off" {
				if len(b) != 0 || strings.Contains(out, "running as root") {
					t.Errorf("guard off: log %q, output %q", b, out)
				}
				return
			}
			var o struct {
				H struct {
					Reason string `json:"permissionDecisionReason"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &o); err != nil {
				t.Fatal(err)
			}
			h := c.guardCheck(hookEventInput{ToolName: "Bash", Cwd: repo, ToolInput: json.RawMessage(`{"command":"sudo rm -rf /x"}`)})
			if h == nil {
				t.Fatal("the guard does not hit sudo")
			}
			if !strings.Contains(o.H.Reason, h.reason) {
				t.Errorf("reason %q lacks the guard's %q", o.H.Reason, h.reason)
			}
			var e map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(b), &e); err != nil || e["rule"] != ruleRoot || e["what"] != cmd {
				t.Errorf("guard.log %q (%v), want one line for the call", b, err)
			}
			if st := c.loadState().Nudges["pgrep-self"]; st == nil || st.Shown != 1 {
				t.Errorf("pgrep-self not counted: %+v", st)
			}
		})
	}
}
