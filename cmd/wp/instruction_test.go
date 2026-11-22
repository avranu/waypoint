package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// subagentStartContext is the additionalContext a SubagentStart hook printed,
// "" when it printed nothing.
func subagentStartContext(t *testing.T, out string) string {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		return ""
	}
	var o struct {
		Specific struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.Specific.Event != "SubagentStart" {
		t.Fatalf("SubagentStart output isn't hookSpecificOutput JSON: %q (%v)", out, err)
	}
	return o.Specific.Context
}

// Agent instructions end to end (WP-1559, ADR146): a person adds them with
// the CLI, the hooks hand each session and sub-agent the ones that fit it,
// an agent's change waits for a person, and a project can turn them off.
func TestCLIInstructionsReachTheSessionsTheyAreFor(t *testing.T) {
	s := newSessionEnv(t)
	key := s.project.Key
	t.Setenv("HOME", t.TempDir()) // no agent definitions or Claude Code settings of this machine
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")

	s.asHuman()
	out := s.wp("instruction", "add", "Fan out reading", "-p", key, "--scope", "main", "--model", "fable", "--model", "opus", "-m", "MAIN-TIP: hand reading to haiku workers.")
	if !strings.Contains(out, "Added #1 Fan out reading ("+key+")") || !strings.Contains(out, "main thread on fable, opus") || strings.Contains(out, "approve") {
		t.Fatalf("add as a person: %s", out)
	}
	s.wp("instruction", "add", "Worker report", "-p", key, "--scope", "subagent", "--agent-type", "*-worker", "--model", "haiku", "-m", "WORKER-TIP: report in five lines.")
	s.wp("instruction", "add", "Everyone", "-p", key, "-m", "ALL-TIP: say what you skipped.")
	if out, code := s.run("instruction", "add", "No text", "-p", key); code == 0 || !strings.Contains(out, "-m TEXT or -f FILE") {
		t.Errorf("add without text: %d %s", code, out)
	}

	// A Fable main thread gets the main-thread and everyone instructions,
	// ahead of everything else in the SessionStart context.
	s.asAgent()
	ctx := sessionStartContext(t, s.hook(in("sess-1", "SessionStart", s.repo, map[string]any{"source": "startup", "model": "claude-fable-5-2"})))
	if !strings.HasPrefix(ctx, "MAIN-TIP: hand reading to haiku workers.\n\nALL-TIP: say what you skipped.") || strings.Contains(ctx, "WORKER-TIP") {
		t.Fatalf("a Fable session's context: %q", ctx)
	}
	// A Sonnet main thread isn't told to fan out.
	ctx = sessionStartContext(t, s.hook(in("sess-2", "SessionStart", s.repo, map[string]any{"source": "startup", "model": "claude-sonnet-5-5"})))
	if strings.Contains(ctx, "MAIN-TIP") || !strings.HasPrefix(ctx, "ALL-TIP") {
		t.Errorf("a Sonnet session's context: %q", ctx)
	}
	// After a compaction the main thread is sent them again.
	ctx = sessionStartContext(t, s.hook(in("sess-1", "SessionStart", s.repo, map[string]any{"source": "compact", "model": "claude-fable-5-2"})))
	if !strings.HasPrefix(ctx, "MAIN-TIP") {
		t.Errorf("after a compaction: %q", ctx)
	}

	// A sub-agent whose model nobody can tell gets only what names no model.
	sub := map[string]any{"agent_id": "a1", "agent_type": "haiku-worker"}
	if got := subagentStartContext(t, s.hook(in("sess-1", "SubagentStart", s.repo, sub))); got != "ALL-TIP: say what you skipped." {
		t.Errorf("a sub-agent of unknown model: %q", got)
	}
	// One the Agent gate resolved a model for gets its own too.
	var sink bytes.Buffer
	c, err := newCLI(&sink, &sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.updateState(func(st *State) {
		st.Reserved = map[string][]reservation{"sess-1": {{ID: "toolu_1", Type: "haiku-worker", Model: "claude-haiku-5-5", At: time.Now()}}}
	}); err != nil {
		t.Fatal(err)
	}
	sub["agent_id"] = "a2"
	if got := subagentStartContext(t, s.hook(in("sess-1", "SubagentStart", s.repo, sub))); got != "WORKER-TIP: report in five lines.\n\nALL-TIP: say what you skipped." {
		t.Errorf("a Haiku worker: %q", got)
	}
	// Another type of sub-agent on the same model doesn't.
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "claude-haiku-5-5")
	if got := subagentStartContext(t, s.hook(in("sess-1", "SubagentStart", s.repo, map[string]any{"agent_id": "a3", "agent_type": "Explore"}))); got != "ALL-TIP: say what you skipped." {
		t.Errorf("an Explore sub-agent: %q", got)
	}

	// What was sent, and what wasn't and why, is on the instruction.
	if out = s.wp("instruction", "show", "2"); !strings.Contains(out, "Sent 1 time: 0 sessions, 1 sub-agent") || !strings.Contains(out, "Not sent 1 time: the agent's model wasn't known.") {
		t.Errorf("show the worker instruction: %s", out)
	}
	if out = s.wp("instruction", "show", "1"); !strings.Contains(out, "Sent 2 times: 1 session, 0 sub-agents, 1 after a compaction.") {
		t.Errorf("show the main instruction: %s", out)
	}
	out = s.wp("instruction", "for", "-p", key, "--agent-type", "sonnet-worker", "--model", "claude-haiku-5-5")
	if !strings.HasPrefix(out, "WORKER-TIP") || !strings.Contains(out, "ALL-TIP") || strings.Contains(out, "MAIN-TIP") {
		t.Errorf("for a worker: %s", out)
	}

	// An agent's change waits: the approved text is still what is sent.
	out = s.wp("instruction", "edit", "3", "-m", "ALL-TIP v2")
	if !strings.Contains(out, "rev 2 waits for a person") || !strings.Contains(out, "wp instruction approve 3") {
		t.Errorf("an agent's edit: %s", out)
	}
	if _, code := s.run("instruction", "approve", "3"); code == 0 {
		t.Error("an agent approved its own change")
	}
	if _, code := s.run("instruction", "off", "1"); code == 0 {
		t.Error("an agent turned an instruction off")
	}
	if ctx = sessionStartContext(t, s.hook(in("sess-3", "SessionStart", s.repo, map[string]any{"source": "startup", "model": "claude-sonnet-5-5"}))); !strings.HasPrefix(ctx, "ALL-TIP: say what you skipped.") {
		t.Errorf("before approval: %q", ctx)
	}
	s.asHuman()
	if out = s.wp("instruction", "approve", "3"); !strings.Contains(out, "Approved: #3 Everyone") {
		t.Errorf("approve: %s", out)
	}
	if out = s.wp("instruction", "ls", "-p", key); strings.Contains(out, "waits") || strings.Count(strings.TrimSpace(out), "\n") != 2 {
		t.Errorf("ls: %s", out)
	}
	s.asAgent()
	if ctx = sessionStartContext(t, s.hook(in("sess-4", "SessionStart", s.repo, map[string]any{"source": "startup", "model": "claude-sonnet-5-5"}))); !strings.HasPrefix(ctx, "ALL-TIP v2") {
		t.Errorf("after approval: %q", ctx)
	}

	// The project turns delivery off: nothing is sent, the instructions stay.
	s.asHuman()
	s.wp("instruction", "delivery", "off", "-p", key)
	if out = s.wp("instruction", "for", "-p", key, "--model", "claude-fable-5-2"); !strings.Contains(out, "turned off") {
		t.Errorf("for, with delivery off: %s", out)
	}
	s.asAgent()
	if ctx = sessionStartContext(t, s.hook(in("sess-5", "SessionStart", s.repo, map[string]any{"source": "startup", "model": "claude-fable-5-2"}))); strings.Contains(ctx, "TIP") {
		t.Errorf("with delivery off: %q", ctx)
	}
	if got := subagentStartContext(t, s.hook(in("sess-5", "SubagentStart", s.repo, map[string]any{"agent_id": "b1", "agent_type": "Explore"}))); got != "" {
		t.Errorf("a sub-agent with delivery off: %q", got)
	}
	s.asHuman()
	s.wp("instruction", "delivery", "on", "-p", key)
	s.wp("instruction", "rm", "1", "--yes")
	if out = s.wp("instruction", "ls", "-p", key); strings.Contains(out, "Fan out") {
		t.Errorf("after rm: %s", out)
	}
}
