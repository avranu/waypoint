package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Transcript lines as Claude Code writes them (2.1.x).
const (
	tlUser     = `{"type":"user","isSidechain":false,"message":{"role":"user","content":"do the task"}}`
	tlReplyA1  = `{"type":"assistant","isSidechain":false,"message":{"id":"msg_a","role":"assistant","content":[{"type":"text","text":"on it"}]}}`
	tlReplyA2  = `{"type":"assistant","isSidechain":false,"message":{"id":"msg_a","role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`
	tlReplyB   = `{"type":"assistant","isSidechain":false,"message":{"id":"msg_b","role":"assistant","content":[{"type":"text","text":"done"}]}}`
	tlSideB    = `{"type":"assistant","isSidechain":true,"message":{"id":"msg_s","role":"assistant","content":[{"type":"text","text":"subagent"}]}}`
	tlBoundary = `{"type":"system","subtype":"compact_boundary","isSidechain":false,"compactMetadata":{"trigger":"manual","preTokens":150000}}`
	tlSummary  = `{"type":"user","isSidechain":false,"isCompactSummary":true,"message":{"role":"user","content":"This session is being continued from a previous conversation that ran out of context."}}`
	tlCommand  = `{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-name>/compact</command-name>"}}`
	tlTask     = `{"type":"user","isSidechain":false,"message":{"role":"user","content":"<task-notification>\n<task-id>b1</task-id>"}}`
)

func writeCompactTranscript(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, "projects", "-repo", "old-123.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadCompaction(t *testing.T) {
	for name, c := range map[string]struct {
		lines []string
		want  transcriptCompaction
	}{
		"never compacted":  {[]string{tlUser, tlReplyA1, tlReplyA2}, transcriptCompaction{replies: 1}},
		"nothing since":    {[]string{tlUser, tlReplyA1, tlBoundary, tlSummary, tlCommand}, transcriptCompaction{compacted: true}},
		"a report since":   {[]string{tlUser, tlReplyA1, tlBoundary, tlSummary, tlCommand, tlTask, tlReplyB}, transcriptCompaction{compacted: true, replies: 1}},
		"one reply, split": {[]string{tlBoundary, tlSummary, tlUser, tlReplyA1, tlReplyA2, tlReplyB}, transcriptCompaction{compacted: true, replies: 2}},
		"subagent's only":  {[]string{tlBoundary, tlSummary, tlSideB}, transcriptCompaction{compacted: true}},
		"the last counts":  {[]string{tlBoundary, tlUser, tlReplyA1, tlBoundary, tlSummary}, transcriptCompaction{compacted: true}},
	} {
		got, err := readCompaction(writeCompactTranscript(t, t.TempDir(), c.lines...))
		if err != nil || got != c.want {
			t.Errorf("%s: %+v %v, want %+v", name, got, err, c.want)
		}
	}
}

// The transcript decides; the server's word only stands without one.
func TestReviewCompaction(t *testing.T) {
	dir := t.TempDir()
	clean := writeCompactTranscript(t, filepath.Join(dir, "clean"), tlUser, tlReplyA1, tlBoundary, tlSummary)
	worked := writeCompactTranscript(t, filepath.Join(dir, "worked"), tlUser, tlReplyA1, tlBoundary, tlSummary, tlTask, tlReplyB)
	never := writeCompactTranscript(t, filepath.Join(dir, "never"), tlUser, tlReplyA1)
	for _, c := range []struct {
		path   string
		server bool
		want   bool
		why    string
	}{
		{"", true, false, "no transcript"},
		{"", false, true, "wasn't compacted"},
		{filepath.Join(dir, "missing.jsonl"), true, false, "couldn't be read"},
		{clean, true, false, "nothing since"},
		{clean, false, false, "nothing since"},
		{worked, true, true, "has worked since (1 reply)"},
		{worked, false, true, "since its last compaction (1 reply)"},
		{never, true, true, "shows no compaction"},
		{never, false, true, "never been compacted"},
	} {
		got, why := reviewCompaction(c.path, c.server)
		if got != c.want || !strings.Contains(why, c.why) {
			t.Errorf("%s server=%v: %v %q, want %v %q", filepath.Base(c.path), c.server, got, why, c.want, c.why)
		}
	}
}

// reviewJobEnv sets up a session old-123 for a review job and returns the
// fake pane and the session file.
func reviewJobEnv(t *testing.T, status string) (*fakeTmux, string, string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	_ = os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700)
	file := filepath.Join(cfg, "sessions", strconv.Itoa(os.Getpid())+".json")
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "old-123", "status": status, "tmux": "w:@1.%7"})
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeTmux{t: t, file: file, screen: screenEmpty}
	oldTmux, oldPoll, oldSettle, oldStart := runTmux, autopilotPoll, autopilotSettle, autopilotCompactStart
	runTmux, autopilotPoll, autopilotSettle, autopilotCompactStart = f.run, 10*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		runTmux, autopilotPoll, autopilotSettle, autopilotCompactStart = oldTmux, oldPoll, oldSettle, oldStart
	})
	return f, file, cfg
}

// The server says compacted, but the session worked since (a background
// task reported back): the job compacts it again before the review (WP-816).
func TestAutopilotReviewCompactsAgainWhenWorkedSince(t *testing.T) {
	f, _, cfg := reviewJobEnv(t, "idle")
	writeCompactTranscript(t, cfg, tlUser, tlReplyA1, tlBoundary, tlSummary, tlCommand, tlTask, tlReplyB)
	dir := t.TempDir()
	prompt := autopilotReviewMark + "APP-1: Grok's findings."
	o := newTestProc(t, dir).execAutopilot(context.Background(), newRun(time.Minute),
		autopilotJob{CLIID: "old-123", Dir: dir, Done: "APP-1", ReviewOf: "APP-1", Review: prompt, Compacted: true})
	if r, _ := o.Result.(autopilotResult); o.State != "done" || r.Outcome != "reviewing" {
		t.Fatalf("outcome %+v", o)
	}
	if strings.Join(f.typed, " ") != "/compact Enter Enter" || f.pasted != prompt {
		t.Errorf("typed %q, pasted %q", f.typed, f.pasted)
	}
}

// The server lost track, but the transcript shows the compaction with
// nothing since: no second /compact.
func TestAutopilotReviewTrustsCleanTranscript(t *testing.T) {
	f, _, cfg := reviewJobEnv(t, "idle")
	writeCompactTranscript(t, cfg, tlUser, tlReplyA1, tlBoundary, tlSummary, tlCommand)
	dir := t.TempDir()
	prompt := autopilotReviewMark + "APP-1: Grok's findings."
	o := newTestProc(t, dir).execAutopilot(context.Background(), newRun(time.Minute),
		autopilotJob{CLIID: "old-123", Dir: dir, Done: "APP-1", ReviewOf: "APP-1", Review: prompt})
	if r, _ := o.Result.(autopilotResult); o.State != "done" || r.Outcome != "reviewing" {
		t.Fatalf("outcome %+v", o)
	}
	if strings.Join(f.typed, " ") != "Enter" || f.pasted != prompt {
		t.Errorf("typed %q, pasted %q", f.typed, f.pasted)
	}
}

// A busy session holds a review job only until it goes idle, then the job
// compacts and types the review at once (WP-816); one that stays busy is
// held as before.
func TestAutopilotReviewWaitsForIdle(t *testing.T) {
	f, file, _ := reviewJobEnv(t, "busy")
	// A turn in progress on the screen: a busy registry beside an empty
	// prompt is read as idle (WP-1325).
	f.screen = screenWorking
	old := autopilotIdleWait
	t.Cleanup(func() { autopilotIdleWait = old })
	dir := t.TempDir()
	p := newTestProc(t, dir)
	prompt := autopilotReviewMark + "APP-1: Grok's findings."
	job := autopilotJob{CLIID: "old-123", Dir: dir, Done: "APP-1", ReviewOf: "APP-1", Review: prompt}

	autopilotIdleWait = 50 * time.Millisecond
	o := p.execAutopilot(context.Background(), newRun(time.Minute), job)
	if r, _ := o.Result.(autopilotResult); r.Outcome != "held" || r.Reason != "busy" || len(f.typed) != 0 {
		t.Fatalf("still busy: %+v, typed %q", o, f.typed)
	}

	autopilotIdleWait = 5 * time.Second
	go func() {
		time.Sleep(100 * time.Millisecond)
		b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "old-123", "status": "idle", "tmux": "w:@1.%7"})
		_ = os.WriteFile(file, b, 0o600)
	}()
	o = p.execAutopilot(context.Background(), newRun(time.Minute), job)
	if r, _ := o.Result.(autopilotResult); o.State != "done" || r.Outcome != "reviewing" {
		t.Fatalf("went idle: %+v", o)
	}
	if strings.Join(f.typed, " ") != "/compact Enter Enter" || f.pasted != prompt {
		t.Errorf("typed %q, pasted %q", f.typed, f.pasted)
	}
}

// WP-1306: Claude Code says "shell" for a session idle at its prompt with a
// background shell still running (a silent waiter). A review is typed there
// as into an idle one, with no wait; the session going busy after is the
// review starting.
func TestAutopilotReviewTypesIntoAShellSession(t *testing.T) {
	f, _, cfg := reviewJobEnv(t, "shell")
	writeCompactTranscript(t, cfg, tlUser, tlReplyA1, tlBoundary, tlSummary, tlCommand)
	old := autopilotIdleWait
	t.Cleanup(func() { autopilotIdleWait = old })
	autopilotIdleWait = 50 * time.Millisecond
	dir := t.TempDir()
	prompt := autopilotReviewMark + "APP-1: Grok's findings."
	o := newTestProc(t, dir).execAutopilot(context.Background(), newRun(time.Minute),
		autopilotJob{CLIID: "old-123", Dir: dir, Done: "APP-1", ReviewOf: "APP-1", Review: prompt, Compacted: true})
	if r, _ := o.Result.(autopilotResult); o.State != "done" || r.Outcome != "reviewing" || r.Warning != "" {
		t.Fatalf("outcome %+v", o)
	}
	if f.pasted != prompt {
		t.Errorf("typed %q, pasted %q", f.typed, f.pasted)
	}
}

// A task's start isn't typed into a session with a shell still running: the
// start clears or restarts it, which would end that shell.
func TestAutopilotStartHoldsAShellSession(t *testing.T) {
	f, _, _ := reviewJobEnv(t, "shell")
	dir := t.TempDir()
	o := newTestProc(t, dir).execAutopilot(context.Background(), newRun(time.Minute),
		autopilotJob{CLIID: "old-123", Dir: dir, Key: "APP-2"})
	r, _ := o.Result.(autopilotResult)
	if r.Outcome != "held" || r.Reason != "busy" || len(f.typed) != 0 {
		t.Fatalf("outcome %+v, typed %q", o, f.typed)
	}
	// It says why, not "shell, not idle" (audit #1039).
	if !strings.Contains(r.Detail, "background shell") || strings.Contains(r.Detail, "not idle") {
		t.Errorf("detail = %q", r.Detail)
	}
}
