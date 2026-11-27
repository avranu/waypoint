package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wrapPaste is a prompt as Claude Code hands the hook a bracketed paste
// (WP-1562).
func wrapPaste(p string) string {
	return "\n\n<pasted_content id=\"d29a\">\n" + p + "\n</pasted_content id=\"d29a\">\n"
}

func TestAutopilotMarksMatchCore(t *testing.T) {
	for _, m := range []struct{ name, here, core string }{
		{"autopilotPromptMark", autopilotPromptMark, core.AutopilotPromptMark},
		{"autopilotReviewMark", autopilotReviewMark, core.AutopilotReviewMark},
		{"cmpReviewPrompt", cmpReviewPrompt, core.AutopilotReviewMark},
	} {
		if m.here != m.core {
			t.Errorf("%s: cmd/wp says %q, core says %q", m.name, m.here, m.core)
		}
	}
}

// The quota guard reads a paste's text: autopilot's prompt pasted into a
// session the guard stopped is refused and the stop stands; a person's
// paste takes the session back (WP-1562).
func TestGuardReadsAPastesText(t *testing.T) {
	t.Setenv("WAYPOINT_SESSION", "s1")
	t.Setenv("WAYPOINT_SERVER", "")
	t.Setenv("WAYPOINT_TOKEN", "")
	c := &cli{dir: t.TempDir()}
	old := burnNow
	burnNow = func() time.Time { return guardT0 }
	defer func() { burnNow = old }()
	c.withBurnState("s1", func(st *burnState) bool {
		st.Guard = &guardLocal{Stopped: "hours", AutoAt: guardT0.Add(-20 * time.Hour), Told: map[string]float64{"hours": 0}}
		return true
	})
	prompt := func(p string) string {
		b, _ := json.Marshal(map[string]any{"session_id": "s1", "hook_event_name": "UserPromptSubmit", "cwd": "/x", "prompt": p})
		var out bytes.Buffer
		hookClaudeEvent(c, bytes.NewReader(b), &out, false)
		return out.String()
	}
	if out := prompt(wrapPaste(autopilotReviewMark + "APP-1: findings")); !strings.Contains(out, "takes no prompts from automation") || c.guardStopped("s1") != "hours" {
		t.Fatalf("autopilot's wrapped review: %q, stopped %q", out, c.guardStopped("s1"))
	}
	c.notePastedPrompt("s1", "APP-2", "Handle APP-2.")
	if out := prompt(wrapPaste("Handle APP-2.")); !strings.Contains(out, "takes no prompts from automation") || c.guardStopped("s1") != "hours" {
		t.Fatalf("autopilot's wrapped task prompt: %q, stopped %q", out, c.guardStopped("s1"))
	}
	if out := prompt(wrapPaste("my own notes")); out != "" || c.guardStopped("s1") != "" {
		t.Fatalf("a person's paste: %q, stopped %q", out, c.guardStopped("s1"))
	}
}

// A wrapped keep-warm ping is no work on the task: the resume hook doesn't
// restart its clock, and the slash hook keeps /wp btw's note for a real
// prompt; a wrapped /wp command is still the command (WP-1562).
func TestPastedKeepWarmIsNoWork(t *testing.T) {
	t.Setenv("WAYPOINT_SESSION", "")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "tok")
	dir := t.TempDir()
	c := &cli{dir: t.TempDir()}
	c.state.Claims = map[string]*claim{dir: {Key: "WP-688", Session: "s1", Checked: true}}
	in, _ := json.Marshal(map[string]string{"session_id": "s1", "cwd": dir, "prompt": wrapPaste(core.KeepWarmPing())})
	hookClaudeResume(c, strings.NewReader(string(in)))
	if calls != 0 {
		t.Fatalf("a wrapped keep-warm ping restarted the task's clock: %d calls", calls)
	}

	if got := stopReason(t, slashHook(t, c, "s1", "/x", wrapPaste("/wp btw use the v2 API"))); !strings.Contains(got, "noted") {
		t.Fatalf("a wrapped /wp btw said %q", got)
	}
	if out := slashHook(t, c, "s1", "/x", wrapPaste(core.KeepWarmPing())); out != "" {
		t.Fatalf("a wrapped keep-warm ping took the note: %q", out)
	}
	if out := slashHook(t, c, "s1", "/x", "hello"); !strings.Contains(out, "use the v2 API") {
		t.Fatalf("the note didn't reach the next prompt: %q", out)
	}
}

// The usage scanner reads a wrapped report or ping as it reads one typed.
func TestCmpPromptKindReadsAPaste(t *testing.T) {
	for _, p := range []string{"<task-notification>done</task-notification>", keepWarmMark + ": reply ok", "[Request interrupted by user]", "Another Claude session sent a message: hi"} {
		if got, want := cmpPromptKind(wrapPaste(p)), cmpPromptKind(p); got != want || want == 0 {
			t.Errorf("cmpPromptKind(wrapped %q) = %q, unwrapped %q", p, got, want)
		}
	}
}
