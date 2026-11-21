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

// Screens of Claude Code sessions (WP-1325). The first four are real
// captures (ANSI stripped, trailing spaces trimmed) of sessions on a machine
// running them: the turn in progress with a shell and agents, the same with
// an agents list under the footer, the main thread at its prompt while the
// registry said busy for over an hour (background tasks and agents), and a
// session at rest. They were taken on 2026-11-20 from Claude Code 2.1.330,
// the version installed then. All the others are synthetic, for cases the
// captures don't show.
const (
	screenRule = "──────────────────────────────────────────────────\n"
	// Real: a turn running; the footer offers to interrupt it.
	screenWorking = "* Embellishing… (5m 37s · ↓ 15.9k tokens)\n  ⎿  Tip: Use /btw to ask a side question without interrupting Claude\n\n" +
		screenRule + "❯ \n" + screenRule +
		"  Fable 5.2 · ctx 90k 9% · 7d 68%\n  ⏵⏵ auto mode on · 1 shell · esc to interrupt · ← 2 agents · ↓ to manage\n"
	// Real: a turn running, with the agents list under the footer.
	screenWorkingAgents = "· Sketching… (55s · ↓ 4.0k tokens)\n  ⎿  Tip: Use /btw to ask a side question without interrupting Claude\n✔ Update installed · Restart to update\n" +
		screenRule + "❯ \n" + screenRule +
		"  Fable 5.2 · ctx 63k 6% · 7d 41%\n  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt · ← 2 agents · ↓ to manage\n\n" +
		"  ● main\n  ◯ adversarial-review  Checking untrusted helper in review_helpers.go\n"
	// Real: the main thread at its prompt, background tasks and agents running.
	screenAgentsIdle = screenRule + "❯ \n" + screenRule +
		"  Opus 5.5 · OPEN-569 · ctx 143k 14% · 7d 56%\n  ⏵⏵ auto mode on · 5 background tasks · ← 2 agents · ↓ to manage\n" +
		"  ● main\n  ◯ sonnet-worker  Waiting for internal/tasks tests                    53m 50s · ↓ 161.1k tokens\n"
	// Real: at rest, nothing in the background.
	screenRest = "✻ Baked for 10h 21m 53s · done 10:06 AM\n\n※ recap: reviewing and repairing recent work. (turn off recaps in /config)\n\n" +
		screenRule + "❯ \n" + screenRule +
		"  Fable 5.2 · ctx 107k 11% · 7d 40%\n  ⏵⏵ auto mode on (shift+tab to cycle) · ← 2 agents\n"
	// Synthetic: the footer cut short before the hint by a narrow pane; the
	// spinner above the box still shows the turn.
	screenNarrow = "* Embellishing… (5m 37s · ↓ 15.9k tokens)\n\n" + screenRule + "❯ \n" + screenRule + "  ⏵⏵ auto mode on · 1 shell\n"
	// Synthetic: at rest, an agent whose status line says "interrupt".
	screenAgentInterrupt = screenRule + "❯ \n" + screenRule +
		"  ⏵⏵ auto mode on · ← 2 agents · ↓ to manage\n  ● main\n  ◯ reviewer  Deciding whether to interrupt the run\n"
	// Synthetic: at rest, with a transcript that quotes the hint and a
	// spinner line far above the box, and indented lines that look like one.
	screenQuotesHint = "· Sketching… (55s)\n● The idle test reads \"esc to interrupt\" on the screen.\n  * Embellishing… (5m 37s)\n● It does.\n● Anything else?\n● No.\n● Yes.\n  ⎿  Done\n\n" +
		screenRule + "❯ \n" + screenRule + "  ⏵⏵ auto mode on\n"
	// Synthetic: nothing asked, a dialog with no input box.
	screenDialog = "Do you want to proceed?\n 1. Yes\n 2. No\n"
)

func TestTurnShown(t *testing.T) {
	for name, c := range map[string]struct {
		screen string
		want   bool
	}{
		"idle with agents":        {screenAgentsIdle, false},
		"at rest":                 {screenRest, false},
		"agent says interrupt":    {screenAgentInterrupt, false},
		"narrow pane, spinner":    {screenNarrow, true},
		"empty":                   {screenEmpty, false},
		"working":                 {screenWorking, true},
		"working with agents":     {screenWorkingAgents, true},
		"transcript quotes hint":  {screenQuotesHint, false},
		"no box":                  {screenDialog, true},
		"footer hint only":        {"\n────────\n❯ \n────────\n  esc to interrupt\n", true},
		"footer hint, no spinner": {screenRule + "❯ \n" + screenRule + "  ⏵⏵ auto mode on · esc to interrupt\n", true},
		// Synthetic, at rest: other people's words quote the hint.
		"last message quotes hint": {"● The footer says esc to interrupt while a turn runs.\n\n" + screenRule + "❯ \n" + screenRule + "  ⏵⏵ auto mode on\n", false},
		"draft quotes hint":        {screenRule + "❯ what does esc to interrupt mean\n" + screenRule + "  ⏵⏵ auto mode on\n", false},
		"agent quotes hint":        {screenRule + "❯ \n" + screenRule + "  ⏵⏵ auto mode on · ← 1 agent\n  ● main\n  ◯ worker  Reading the esc to interrupt hint\n", false},
		// Synthetic, a turn running in a narrow pane: a task list between
		// the spinner and the box, and the older spinner that carried the
		// hint itself.
		"spinner over a task list": {"✶ Wiring… (2m 4s · ↓ 3.1k tokens)\n  ⎿  ☒ Read the diff\n     ☐ Fix the test\n     ☐ Run the suite\n\n" + screenRule + "❯ \n" + screenRule + "  ⏵⏵ auto mode on\n", true},
		// A task list longer than any fixed look-back, in a narrow pane.
		"spinner over a long task list": {"✶ Wiring… (2m 4s · ↓ 3.1k tokens)\n  ⎿  ☒ Read the diff\n" + strings.Repeat("     ☐ Fix a test\n", 12) + "\n" + screenRule + "❯ \n" + screenRule + "  ⏵⏵ auto mode on\n", true},
		// At rest, a person's last prompt that looks like a spinner line.
		"person's prompt like a spinner": {"> Fixing… (it broke)\n" + screenRule + "❯ \n" + screenRule + "  ⏵⏵ auto mode on\n", false},
		"spinner with the hint":          {"✻ Thinking… (12s · esc to interrupt)\n\n" + screenRule + "❯ \n" + screenRule + "  ? for shortcuts\n", true},
	} {
		if got := turnShown(c.screen); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// screenTmux is fakeTmux with a screen that changes when the prompt is sent
// (to afterEnter, as a session taking it would) and a registry that never
// does, as with a session whose background agents keep it "busy".
type screenTmux struct {
	*fakeTmux
	afterEnter string
}

func (s *screenTmux) run(ctx context.Context, socket string, stdin []byte, args ...string) (string, error) {
	if args[0] == "send-keys" && args[len(args)-1] == "Enter" && s.pasted != "" {
		s.typed = append(s.typed, "Enter")
		s.screen = s.afterEnter
		return "", nil
	}
	return s.fakeTmux.run(ctx, socket, stdin, args...)
}

func busyReviewEnv(t *testing.T, status string, screen, after string) (*runnerProc, *screenTmux, autopilotJob) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	_ = os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700)
	file := filepath.Join(cfg, "sessions", strconv.Itoa(os.Getpid())+".json")
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": "old-123", "status": status, "tmux": "w:@1.%7"})
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	st := &screenTmux{fakeTmux: &fakeTmux{t: t, file: file, screen: screen}, afterEnter: after}
	oldTmux, oldPoll, oldSettle, oldIdle := runTmux, autopilotPoll, autopilotSettle, autopilotIdleWait
	runTmux, autopilotPoll, autopilotSettle, autopilotIdleWait = st.run, 10*time.Millisecond, 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		runTmux, autopilotPoll, autopilotSettle, autopilotIdleWait = oldTmux, oldPoll, oldSettle, oldIdle
	})
	dir := t.TempDir()
	prompt := autopilotReviewMark + "APP-1: Grok's findings."
	return newTestProc(t, dir), st, autopilotJob{CLIID: "old-123", Dir: dir, Done: "APP-1", ReviewOf: "APP-1", Review: prompt, Compacted: true}
}

// A session "busy" in the registry for its background agents, its main
// thread at the prompt, gets the review typed, and it is confirmed from the
// screen (the registry never changes).
func TestReviewTypedBesideBackgroundAgents(t *testing.T) {
	p, st, job := busyReviewEnv(t, "busy", screenAgentsIdle, screenWorking)
	o := p.execAutopilot(context.Background(), newRun(time.Minute), job)
	r, _ := o.Result.(autopilotResult)
	if o.State != "done" || r.Outcome != "reviewing" || r.Warning != "" {
		t.Fatalf("outcome %+v %+v", o, r)
	}
	if st.pasted != job.Review || strings.Join(st.typed, " ") != "Enter" {
		t.Errorf("pasted %q typed %q", st.pasted, st.typed)
	}
}

// The same session taking the prompt without a sign on its screen is said
// so, rather than the stale registry passing for a confirmation.
func TestReviewNotTakenWarnsFromScreen(t *testing.T) {
	p, _, job := busyReviewEnv(t, "busy", screenAgentsIdle, screenAgentsIdle)
	old := reviewTookWait
	reviewTookWait = 200 * time.Millisecond
	t.Cleanup(func() { reviewTookWait = old })
	o := p.execAutopilot(context.Background(), newRun(time.Minute), job)
	if r, _ := o.Result.(autopilotResult); r.Outcome != "reviewing" || !strings.Contains(r.Warning, "didn't start") {
		t.Fatalf("outcome %+v", o)
	}
}

// A working main thread is waited for and then held, as before.
func TestReviewHeldWhileMainThreadWorks(t *testing.T) {
	for name, screen := range map[string]string{"working": screenWorking, "working with agents": screenWorkingAgents, "dialog": screenDialog} {
		p, st, job := busyReviewEnv(t, "busy", screen, screen)
		o := p.execAutopilot(context.Background(), newRun(time.Minute), job)
		if r, _ := o.Result.(autopilotResult); r.Outcome != "held" || r.Reason != "busy" || st.pasted != "" {
			t.Errorf("%s: %+v, pasted %q", name, o, st.pasted)
		}
	}
}

// A draft in the box of such a session holds the review as ever.
func TestReviewHeldForDraftBesideAgents(t *testing.T) {
	draft := strings.Replace(screenAgentsIdle, "❯ \n", "❯ my draft\n", 1)
	p, st, job := busyReviewEnv(t, "busy", draft, draft)
	o := p.execAutopilot(context.Background(), newRun(time.Minute), job)
	if r, _ := o.Result.(autopilotResult); r.Outcome != "held" || r.Reason != "draft" || st.pasted != "" {
		t.Errorf("%+v, pasted %q", o, st.pasted)
	}
}

// A task's start keeps the strict test: busy in the registry is busy.
func TestTaskStartStillNeedsIdleRegistry(t *testing.T) {
	p, st, job := busyReviewEnv(t, "busy", screenAgentsIdle, screenAgentsIdle)
	job.ReviewOf, job.Review, job.Key = "", "", "APP-2"
	o := p.execAutopilot(context.Background(), newRun(time.Minute), job)
	if r, _ := o.Result.(autopilotResult); r.Outcome != "held" || r.Reason != "busy" || len(st.typed) != 0 {
		t.Errorf("%+v typed %q", o, st.typed)
	}
}

// A session busy in the registry for its background agents, asked to
// compact first: /compact shows as a turn on the screen, then the empty
// prompt returns, and the review is pasted and taken, all by the screen
// while the registry stays "busy".
type compactScreenTmux struct {
	*fakeTmux
	compacted bool
	captures  int
}

func (c *compactScreenTmux) run(ctx context.Context, socket string, stdin []byte, args ...string) (string, error) {
	switch {
	case args[0] == "send-keys" && args[len(args)-1] == "Enter" && c.pasted == "":
		c.typed = append(c.typed, "Enter")
		c.compacted, c.captures, c.screen = true, 0, screenWorking
		return "", nil
	case args[0] == "send-keys" && args[len(args)-1] == "Enter":
		c.typed = append(c.typed, "Enter")
		c.screen = screenWorking
		return "", nil
	case args[0] == "capture-pane" && c.compacted && c.pasted == "":
		// The compaction runs for a few looks, then the prompt is back.
		if c.captures++; c.captures > 4 {
			c.screen = screenAgentsIdle
		}
	}
	return c.fakeTmux.run(ctx, socket, stdin, args...)
}

func TestReviewCompactsBesideBackgroundAgents(t *testing.T) {
	p, st, job := busyReviewEnv(t, "busy", screenAgentsIdle, screenWorking)
	cs := &compactScreenTmux{fakeTmux: st.fakeTmux}
	runTmux = cs.run
	job.Compacted = false
	o := p.execAutopilot(context.Background(), newRun(time.Minute), job)
	r, _ := o.Result.(autopilotResult)
	if o.State != "done" || r.Outcome != "reviewing" || r.Warning != "" || !strings.Contains(r.Detail, "registry said busy") {
		t.Fatalf("outcome %+v %+v", o, r)
	}
	if strings.Join(cs.typed, " ") != "/compact Enter Enter" || cs.pasted != job.Review {
		t.Errorf("typed %q, pasted %q", cs.typed, cs.pasted)
	}
}
