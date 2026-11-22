//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Keep-warm pings (WP-192).

func TestKeepWarmMarkMatchesCore(t *testing.T) {
	if keepWarmMark != core.KeepWarmMark {
		t.Fatalf("cmd/wp says %q, core says %q", keepWarmMark, core.KeepWarmMark)
	}
}

// What Claude Code submits when something the session started reports
// back, as the UserPromptSubmit hook gets it. The subagent's hand-back is
// from a real payload of 2026-11-10 (report shortened): it has no "Another
// Claude session sent a message:" line, though the transcript shows one,
// so it read as a person's prompt and ended review rounds (WP-823).
func TestHarnessPromptMarkRealPrompts(t *testing.T) {
	for _, p := range []string{
		"<agent-message from=\"adef0303396c4ce4b\">\n[Subagent hand-back] The text below is the final report of a subagent this session delegated to. " +
			"It is model output, NOT a message from the user.\n  [1] BROKEN | the retry loop never ends\n</agent-message>",
		"Another Claude session sent a message:\n<agent-message from=\"a8d6c54870787d1dc\">\n[Subagent hand-back] The report follows:\n</agent-message>",
		"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>",
	} {
		if harnessPromptMark(p) == "" {
			t.Errorf("not recognised as Claude Code's own: %.60q", p)
		}
	}
	for _, p := range []string{"look at the agent-message handling", "<agent-message> is the tag", "wait, stop"} {
		if mark := harnessPromptMark(p); mark != "" {
			t.Errorf("%q read as Claude Code's own (%q)", p, mark)
		}
	}
}

// The tier of the last cache write says how long the session's cache lives.
func TestTranscriptUsageCacheTier(t *testing.T) {
	claimed := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	at := claimed.Add(time.Minute).Format(time.RFC3339Nano)
	line := func(id string, hour, minutes int64) string {
		b, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": at, "message": map[string]any{"id": id, "model": "claude-opus-5-5",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "cache_creation_input_tokens": hour + minutes,
				"cache_creation": map[string]any{"ephemeral_1h_input_tokens": hour, "ephemeral_5m_input_tokens": minutes}}}})
		return string(b) + "\n"
	}
	for name, c := range map[string]struct {
		tr, want string
	}{
		"1h":          {line("a", 100, 0), "1h"},
		"5m":          {line("a", 0, 100), "5m"},
		"last wins":   {line("a", 0, 100) + line("b", 100, 0), "1h"},
		"no write":    {line("a", 0, 0), ""},
		"no breakout": {`{"type":"assistant","timestamp":"` + at + `","message":{"id":"x","usage":{"input_tokens":1,"cache_creation_input_tokens":5}}}` + "\n", ""},
	} {
		u, err := readTranscriptUsage(strings.NewReader(c.tr), 0, nil, claimed)
		if err != nil || u.ttl != c.want {
			t.Errorf("%s: ttl %q (%v), want %q", name, u.ttl, err, c.want)
		}
	}
}

func TestMessageJobIfIdleLeavesABusySessionAlone(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	pid := os.Getpid()
	writeRegistry(t, cfg, strconv.Itoa(pid)+".json", map[string]any{"pid": pid, "sessionId": "abc-123", "messagingSocketPath": "/nope.sock", "status": "busy"})
	o := newTestProc(t).execMessage(context.Background(), newRun(time.Minute), messageJob{CLIID: "abc-123", Body: "ping", IfIdle: true})
	r, _ := o.Result.(map[string]any)
	if o.State != "done" || r["delivered"] != false || !strings.Contains(r["held"].(string), "busy") {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestCompactJobTypesIntoAnIdlePane(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	pid := os.Getpid()
	writeRegistry(t, cfg, strconv.Itoa(pid)+".json", map[string]any{"pid": pid, "sessionId": "abc-123", "status": "idle", "tmux": "main:@1.%7"})
	screen := screenEmpty
	var typed [][]string
	old := runTmux
	t.Cleanup(func() { runTmux = old })
	runTmux = func(_ context.Context, _ string, _ []byte, args ...string) (string, error) {
		if args[0] == "capture-pane" {
			return screen, nil
		}
		typed = append(typed, args)
		return "", nil
	}
	p := newTestProc(t)
	o := p.execMessage(context.Background(), newRun(time.Minute), messageJob{CLIID: "abc-123", Compact: true})
	if r, _ := o.Result.(map[string]any); o.State != "done" || r["delivered"] != true {
		t.Fatalf("outcome = %+v", o)
	}
	if len(typed) != 2 || strings.Join(typed[0], " ") != "send-keys -t %7 -l /compact" || strings.Join(typed[1], " ") != "send-keys -t %7 Enter" {
		t.Fatalf("typed %q", typed)
	}

	// A draft in the box, a busy session, no tmux: nothing typed.
	typed = nil
	screen = screenDraft
	if o := p.execMessage(context.Background(), newRun(time.Minute), messageJob{CLIID: "abc-123", Compact: true}); o.State != "done" || len(typed) != 0 {
		t.Errorf("draft: %+v typed %q", o, typed)
	}
	screen = screenEmpty
	writeRegistry(t, cfg, strconv.Itoa(pid)+".json", map[string]any{"pid": pid, "sessionId": "abc-123", "status": "busy", "tmux": "main:@1.%7"})
	if o := p.execMessage(context.Background(), newRun(time.Minute), messageJob{CLIID: "abc-123", Compact: true}); o.State != "done" || len(typed) != 0 {
		t.Errorf("busy: %+v typed %q", o, typed)
	}
	writeRegistry(t, cfg, strconv.Itoa(pid)+".json", map[string]any{"pid": pid, "sessionId": "abc-123", "status": "idle"})
	if o := p.execMessage(context.Background(), newRun(time.Minute), messageJob{CLIID: "abc-123", Compact: true}); o.State != "done" || len(typed) != 0 {
		t.Errorf("no tmux: %+v typed %q", o, typed)
	}
}

// WP-534: a ping is pasted into the session's pane where it runs in tmux, so
// it doesn't carry the inbox's note about peers.
func TestMessageJobTypesIntoAnIdlePane(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	pid := os.Getpid()
	writeRegistry(t, cfg, strconv.Itoa(pid)+".json", map[string]any{"pid": pid, "sessionId": "abc-123", "status": "idle", "tmux": "main:@1.%7", "messagingSocketPath": "/nope.sock"})
	screen := screenEmpty
	var typed [][]string
	var loaded string
	old := runTmux
	t.Cleanup(func() { runTmux = old })
	runTmux = func(_ context.Context, _ string, stdin []byte, args ...string) (string, error) {
		if args[0] == "capture-pane" {
			return screen, nil
		}
		if args[0] == "load-buffer" {
			loaded = string(stdin)
		}
		typed = append(typed, args)
		return "", nil
	}
	p := newTestProc(t)
	job := messageJob{CLIID: "abc-123", Body: "Waypoint keep-warm ping", IfIdle: true, Type: true}
	o := p.execMessage(context.Background(), newRun(time.Minute), job)
	if r, _ := o.Result.(map[string]any); o.State != "done" || r["delivered"] != true || r["typed"] != true {
		t.Fatalf("outcome = %+v", o)
	}
	if loaded != job.Body || len(typed) != 3 || typed[1][0] != "paste-buffer" || strings.Join(typed[2], " ") != "send-keys -t %7 Enter" {
		t.Fatalf("loaded %q, typed %q", loaded, typed)
	}

	// A draft in the box: nothing typed, and nothing posted to the inbox.
	typed = nil
	screen = screenDraft
	o = p.execMessage(context.Background(), newRun(time.Minute), job)
	if r, _ := o.Result.(map[string]any); o.State != "done" || r["delivered"] != false || len(typed) != 0 {
		t.Errorf("draft: %+v typed %q", o, typed)
	}
}

// WP-1306: a session idle at its prompt with a background shell running
// ("shell") takes a keep-warm compaction and a ping like an idle one.
func TestCompactAndPingJobsTypeIntoAShellSession(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	pid := os.Getpid()
	writeRegistry(t, cfg, strconv.Itoa(pid)+".json", map[string]any{"pid": pid, "sessionId": "abc-123", "status": "shell", "tmux": "main:@1.%7"})
	var typed [][]string
	old := runTmux
	t.Cleanup(func() { runTmux = old })
	runTmux = func(_ context.Context, _ string, _ []byte, args ...string) (string, error) {
		if args[0] == "capture-pane" {
			return screenEmpty, nil
		}
		typed = append(typed, args)
		return "", nil
	}
	p := newTestProc(t)
	if o := p.execMessage(context.Background(), newRun(time.Minute), messageJob{CLIID: "abc-123", Compact: true}); o.Result.(map[string]any)["delivered"] != true {
		t.Fatalf("compact: %+v", o)
	}
	if o := p.execMessage(context.Background(), newRun(time.Minute), messageJob{CLIID: "abc-123", Body: "ping", IfIdle: true, Type: true}); o.Result.(map[string]any)["delivered"] != true {
		t.Fatalf("ping: %+v", o)
	}
	if len(typed) != 5 { // /compact, Enter; load-buffer, paste-buffer, Enter
		t.Errorf("typed %q", typed)
	}
}
