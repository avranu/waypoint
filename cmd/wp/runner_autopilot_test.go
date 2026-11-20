package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Screens as tmux capture-pane -e shows them (from Claude Code 2.1.312).
const (
	screenEmpty       = "● banana\n\x1b[39m────────\n\x1b[39m❯ \n\x1b[39m────────\n  ⏵⏵ auto mode on\n"
	screenPlaceholder = "\x1b[39m────────\n\x1b[39m❯ \x1b[2mTry \"create a util logging.py that...\"\x1b[0m\n\x1b[39m────────\n"
	screenDraft       = "\x1b[38;5;246m❯\x1b[39m old prompt\n\x1b[39m────────\n\x1b[39m❯ my draft\n  second line\n\x1b[39m────────\n"
)

func TestPromptInput(t *testing.T) {
	for name, c := range map[string]struct {
		screen, want string
		found        bool
	}{
		"empty":           {screenEmpty, "", true},
		"placeholder":     {screenPlaceholder, "", true},
		"draft":           {screenDraft, "my draft second line", true},
		"no box":          {"Do you want to proceed?\n 1. Yes\n", "", false},
		"truecolor draft": {"────\n\u276f\u00a0\x1b[38;2;177;185;249m@src/main.go\x1b[39m\n────\n", "@src/main.go", true},
		"256-color draft": {"────\n\u276f\u00a0\x1b[38;5;2m/deploy now\x1b[39m\n────\n", "/deploy now", true},
		"status line ❯":   {"────\n\u276f\u00a0half typed\n────\n  ~/proj main \u276f\n", "half typed", true},
		"no bottom edge":  {"────\n\u276f\u00a0\n", "", false},
	} {
		got, found := promptInput(c.screen)
		if got != c.want || found != c.found {
			t.Errorf("%s: %q %v, want %q %v", name, got, found, c.want, c.found)
		}
	}
}

// fakeTmux plays a Claude Code session in a pane: a paste and Enter make it
// busy.
type fakeTmux struct {
	t      *testing.T
	file   string
	screen string
	typed  []string
	pasted string
}

func (f *fakeTmux) run(ctx context.Context, socket string, stdin []byte, args ...string) (string, error) {
	set := func(k, v string) {
		var m map[string]any
		b, _ := os.ReadFile(f.file)
		_ = json.Unmarshal(b, &m)
		m[k] = v
		b, _ = json.Marshal(m)
		_ = os.WriteFile(f.file, b, 0o600)
	}
	switch args[0] {
	case "capture-pane":
		return f.screen, nil
	case "send-keys":
		key := args[len(args)-1]
		f.typed = append(f.typed, key)
		if key == "Enter" && f.pasted != "" {
			set("status", "busy")
		}
	case "load-buffer":
		f.pasted = string(stdin)
	case "paste-buffer":
	default:
		return "", errors.New("unexpected tmux " + args[0])
	}
	return "", nil
}

func TestAutopilotJobHoldsWhatItCantRestart(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	_ = os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700)
	file := filepath.Join(cfg, "sessions", strconv.Itoa(os.Getpid())+".json")
	write := func(status, tmux string) {
		b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "old-123", "status": status, "tmux": tmux})
		if err := os.WriteFile(file, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeTmux{t: t, file: file, screen: screenEmpty}
	oldTmux, oldNext := runTmux, runWpNext
	runTmux = f.run
	runWpNext = func(context.Context, string, []string, string, string) (string, error) {
		t.Error("ran wp next for a session it held")
		return "", nil
	}
	t.Cleanup(func() { runTmux, runWpNext = oldTmux, oldNext })
	dir := t.TempDir()
	p := newTestProc(t, dir)
	job := autopilotJob{CLIID: "old-123", Dir: dir, Key: "APP-2", Done: "APP-1", Agent: "anthropic/claude-code"}
	result := func(o outcome) autopilotResult {
		t.Helper()
		if o.State != "done" {
			t.Fatalf("outcome = %+v", o)
		}
		r, _ := o.Result.(autopilotResult)
		return r
	}

	write("busy", "w:@1.%7")
	if r := result(p.execAutopilot(context.Background(), newRun(time.Minute), job)); r.Outcome != "held" || r.Reason != "busy" {
		t.Errorf("busy: %+v", r)
	}
	write("idle", "")
	if r := result(p.execAutopilot(context.Background(), newRun(time.Minute), job)); r.Outcome != "held" || r.Reason != "no_tmux" {
		t.Errorf("outside tmux: %+v", r)
	}
	write("idle", "w:@1.%7")
	f.screen = screenDraft
	if r := result(p.execAutopilot(context.Background(), newRun(time.Minute), job)); r.Outcome != "held" || r.Reason != "draft" || len(f.typed) != 0 {
		t.Errorf("a draft: %+v, typed %q", r, f.typed)
	}
	// Ready, but this test binary isn't a claude that can be started again:
	// no /clear in its place (WP-ADR57), and nothing typed.
	f.screen = screenPlaceholder
	r := result(p.execAutopilot(context.Background(), newRun(time.Minute), job))
	if r.Outcome != "held" || r.Reason != "no_tmux" || !strings.Contains(r.Detail, "can't be restarted") || len(f.typed) != 0 {
		t.Errorf("can't restart: %+v, typed %q", r, f.typed)
	}
	if r := result(p.execAutopilot(context.Background(), newRun(time.Minute), autopilotJob{CLIID: "gone-1", Dir: dir, Key: "APP-2"})); r.Outcome != "gone" {
		t.Errorf("a session that isn't running: %+v", r)
	}
}

// A prompt with a whole audit in it gets longer to arrive than a short one.
func TestReviewPasteSettle(t *testing.T) {
	if got := reviewPasteSettle(500); got != 400*time.Millisecond {
		t.Errorf("short prompt: %s", got)
	}
	if got := reviewPasteSettle(100 << 10); got != 1400*time.Millisecond {
		t.Errorf("100 KB prompt: %s", got)
	}
}

func TestAutopilotPromptWhenWpNextFailed(t *testing.T) {
	got := autopilotPrompt(autopilotJob{Key: "APP-3"}, "APP-3 is claimed by another agent", errors.New("exit status 1"))
	if !strings.Contains(got, "failed (APP-3 is claimed by another agent)") || !strings.Contains(got, "`wp next APP-3` shows the task") || !strings.Contains(got, "rather than working around it") || !strings.HasPrefix(got, "Autopilot assigned APP-3 to you, but") {
		t.Errorf("prompt = %q", got)
	}
}

// The server's template (WP-308) is filled in; without one, the default is.
func TestAutopilotPromptTemplate(t *testing.T) {
	in := autopilotJob{Key: "APP-4", Title: "Fix the thing", Done: "APP-3", Prompt: "Do {key} ({title}) after {done}:\n{next}"}
	if got := autopilotPrompt(in, "APP-4  Fix the thing\n", nil); got != "Do APP-4 (Fix the thing) after APP-3:\nAPP-4  Fix the thing" {
		t.Errorf("prompt = %q", got)
	}
	in.Prompt = ""
	if got := autopilotPrompt(in, "the answer", nil); got != "the answer" {
		t.Errorf("default prompt = %q", got)
	}
}

// Compacted while the review waited for its audit (WP-761): only the prompt
// is typed.
func TestAutopilotReviewAlreadyCompacted(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	_ = os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700)
	file := filepath.Join(cfg, "sessions", strconv.Itoa(os.Getpid())+".json")
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "old-123", "status": "idle", "tmux": "w:@1.%7"})
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeTmux{t: t, file: file, screen: screenEmpty}
	oldTmux, oldPoll, oldSettle := runTmux, autopilotPoll, autopilotSettle
	runTmux, autopilotPoll, autopilotSettle = f.run, 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { runTmux, autopilotPoll, autopilotSettle = oldTmux, oldPoll, oldSettle })
	dir := t.TempDir()
	p := newTestProc(t, dir)
	prompt := autopilotReviewMark + "APP-1: Grok's findings."
	o := p.execAutopilot(context.Background(), newRun(time.Minute), autopilotJob{CLIID: "old-123", Dir: dir, Done: "APP-1", ReviewOf: "APP-1", Review: prompt, Compacted: true})
	if r, _ := o.Result.(autopilotResult); o.State != "done" || r.Outcome != "reviewing" {
		t.Fatalf("outcome %+v", o)
	}
	if strings.Join(f.typed, " ") != "Enter" {
		t.Errorf("typed %q", f.typed)
	}
	if f.pasted != prompt {
		t.Errorf("pasted %q", f.pasted)
	}
}

// A review job (WP-285) compacts the session, waits for it, then pastes the
// review prompt into the same session: no restart, no wp next.
func TestAutopilotReviewCompactsThenPrompts(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	_ = os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700)
	file := filepath.Join(cfg, "sessions", strconv.Itoa(os.Getpid())+".json")
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "old-123", "status": "idle", "tmux": "w:@1.%7"})
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeTmux{t: t, file: file, screen: screenEmpty}
	oldTmux, oldNext, oldPoll, oldSettle, oldStart := runTmux, runWpNext, autopilotPoll, autopilotSettle, autopilotCompactStart
	runTmux, autopilotPoll, autopilotSettle, autopilotCompactStart = f.run, 10*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond
	runWpNext = func(ctx context.Context, dir string, env []string, key, _ string) (string, error) {
		t.Error("a review runs no wp next")
		return "", nil
	}
	t.Cleanup(func() {
		runTmux, runWpNext, autopilotPoll, autopilotSettle, autopilotCompactStart = oldTmux, oldNext, oldPoll, oldSettle, oldStart
	})
	dir := t.TempDir()
	p := newTestProc(t, dir)
	prompt := autopilotReviewMark + "APP-1: Run an adversarial review on this change, and correct any issues that are discovered."
	job := autopilotJob{CLIID: "old-123", Dir: dir, Done: "APP-1", ReviewOf: "APP-1", Review: prompt}
	o := p.execAutopilot(context.Background(), newRun(time.Minute), job)
	r, _ := o.Result.(autopilotResult)
	if o.State != "done" || r.Outcome != "reviewing" || r.NewCLI != "" {
		t.Fatalf("outcome %+v %+v", o, r)
	}
	if strings.Join(f.typed, " ") != "/compact Enter Enter" {
		t.Errorf("typed %q", f.typed)
	}
	if f.pasted != prompt {
		t.Errorf("pasted %q", f.pasted)
	}
	if key, ok := reviewPromptKey(f.pasted); !ok || key != "APP-1" {
		t.Errorf("the hook reads %q %v from the review prompt", key, ok)
	}

	// Still busy compacting when the wait runs out: nothing is pasted.
	f.typed, f.pasted = nil, ""
	_ = os.WriteFile(file, b, 0o600)
	old := autopilotCompactWait
	autopilotCompactWait = 50 * time.Millisecond
	t.Cleanup(func() { autopilotCompactWait = old })
	busy := &compactingTmux{fakeTmux: f}
	runTmux = busy.run
	o = p.execAutopilot(context.Background(), newRun(time.Minute), job)
	if r, _ := o.Result.(autopilotResult); r.Outcome != "held" || r.Reason != "screen" || f.pasted != "" {
		t.Errorf("still compacting: %+v, pasted %q", r, f.pasted)
	}

	// A job without its prompt fails rather than typing anything.
	if o := p.execAutopilot(context.Background(), newRun(time.Minute), autopilotJob{CLIID: "old-123", Dir: dir, ReviewOf: "APP-1"}); o.State != "failed" {
		t.Errorf("no prompt: %+v", o)
	}
}

// compactingTmux: a session that turns busy on /compact and stays so.
type compactingTmux struct{ *fakeTmux }

func (c *compactingTmux) run(ctx context.Context, socket string, stdin []byte, args ...string) (string, error) {
	out, err := c.fakeTmux.run(ctx, socket, stdin, args...)
	if args[0] == "send-keys" && args[len(args)-1] == "Enter" {
		var m map[string]any
		b, _ := os.ReadFile(c.file)
		_ = json.Unmarshal(b, &m)
		m["status"] = "busy"
		b, _ = json.Marshal(m)
		_ = os.WriteFile(c.file, b, 0o600)
	}
	return out, err
}

// The runner's fallback prompt is the server's default (WP-308).
func TestDefaultAutopilotPromptMatchesCore(t *testing.T) {
	if defaultAutopilotPrompt != core.DefaultAutopilotPrompt {
		t.Error("defaultAutopilotPrompt differs from core.DefaultAutopilotPrompt")
	}
}

// WP-770: what to do with the task comes last, after the checkout and brief.
func TestSplitClosing(t *testing.T) {
	const closing = taskClosing + " When done, run `wp ship`."
	body, got := splitClosing("WP-1 \"x\"\n\n" + closing + "\nA warning\n")
	if body != "WP-1 \"x\"\nA warning\n" || got != closing {
		t.Errorf("body %q closing %q", body, got)
	}
	if body, got := splitClosing("WP-1 \"x\"\n"); body != "WP-1 \"x\"\n" || got != "" {
		t.Errorf("an old server's answer: body %q closing %q", body, got)
	}
}
