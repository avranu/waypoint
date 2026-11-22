package core

import (
	"strings"
	"testing"
)

// WP-1470: session metrics that were guesses are labelled, and every drop is
// counted.

func dropCount(t *testing.T, e *env, reason string) int64 {
	t.Helper()
	ds, err := e.s.HookDrops(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, d := range ds {
		if d.Reason == reason {
			n += d.Dropped
		}
	}
	return n
}

func startedSession(t *testing.T, e *env, ag *Actor, cli string) *Actor {
	t.Helper()
	return sessionAgent(t, e, ag, cli, "/work/app", nil)
}

// A Stop that trims the timeline says how many events it dropped.
func TestSessionStopTrimIsCounted(t *testing.T) {
	e := newEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	sess := startedSession(t, e, ag, "trim-1")
	now := e.clockNow().UnixMilli()
	extra := 7
	for i := 0; i < sessionEventsKept+extra; i++ {
		if _, err := e.s.db.W.Exec(`INSERT INTO session_events(session_id, kind, created_at) VALUES (?, 'update', ?)`, sess.Session, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.SessionHook(e.ctx, sess, []HookEvent{hook("m1", "trim-1", "Stop", nil)}); err != nil {
		t.Fatal(err)
	}
	if n := dropCount(t, e, DropSessionEventsTrim); n < int64(extra) {
		t.Errorf("trim counted %d, want at least %d", n, extra)
	}
	var kept int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM session_events WHERE session_id = ?`, sess.Session).Scan(&kept)
	if kept > sessionEventsKept+2 {
		t.Errorf("timeline kept %d events", kept)
	}
}

// --no-prompts: the prompt is on the timeline as a row with no text, and
// counted; a prompt with its text is not.
func TestWithheldPromptIsARowAndCounted(t *testing.T) {
	e := newEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	sess := startedSession(t, e, ag, "wh-1")
	if _, err := e.s.SessionHook(e.ctx, sess, []HookEvent{hook("m1", "wh-1", "UserPromptSubmit", func(h *HookEvent) { h.PromptWithheld = true })}); err != nil {
		t.Fatal(err)
	}
	var label string
	if err := e.s.db.R.QueryRow(`SELECT label || detail FROM session_events WHERE session_id = ? AND kind = 'prompt'`, sess.Session).Scan(&label); err != nil || !strings.Contains(label, PromptWithheldLine) {
		t.Errorf("timeline row = %q (%v)", label, err)
	}
	if n := dropCount(t, e, DropPromptTextWithheld); n != 1 {
		t.Errorf("withheld prompts counted: %d", n)
	}
	if _, err := e.s.SessionHook(e.ctx, sess, []HookEvent{hook("m1", "wh-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "hello" })}); err != nil {
		t.Fatal(err)
	}
	if n := dropCount(t, e, DropPromptTextWithheld); n != 1 {
		t.Errorf("a prompt with text counted as withheld: %d", n)
	}
}

// A write naming a session the server never registered is counted.
func TestUnregisteredSessionWriteIsCounted(t *testing.T) {
	e := newEnv(t)
	e.s.CountUnregisteredWrite(e.ctx, e.admin.User.ID, "box")
	e.s.CountUnregisteredWrite(e.ctx, e.admin.User.ID, "box")
	e.s.CountUnregisteredWrite(e.ctx, 0, "box") // no person: nothing to count against
	if n := dropCount(t, e, DropUnregisteredWrite); n != 2 {
		t.Errorf("counted %d, want 2", n)
	}
}

// Uploaded rows the server cannot keep are counted by why.
func TestUsageIngestSkipsAreCounted(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-1)
	c := UsageChunk{
		Messages: []UsageMessageRow{
			msg(h, "s1", "a/b", "claude-opus-5-5", "main", 1, 1, 1, 0, 0, 0), // kept
			msg(h, "", "a/b", "claude-opus-5-5", "main", 1, 1, 1, 0, 0, 0),   // no session
			msg(h, "s1", "a/b", "", "main", 1, 1, 1, 0, 0, 0),                // no model
			msg(e.hour(24*400), "s1", "a/b", "claude-opus-5-5", "main", 1, 1, 1, 0, 0, 0),
		},
		Tools: []UsageToolRow{{Hour: h, Session: "s1", Kind: "", Tool: "Bash", Calls: 1}},
	}
	e.ingest(e.me, "skip-1", "box", c)
	for reason, want := range map[string]int64{"ingest_no_session": 1, "ingest_no_model": 1, "ingest_hour": 1, "ingest_no_kind": 1} {
		if got := dropCount(t, e.env, reason); got != want {
			t.Errorf("%s counted %d, want %d", reason, got, want)
		}
	}
}

// A test report with more runs than the cap says how many it left out.
func TestEvidenceCapIsCounted(t *testing.T) {
	a := newAPEnv(t)
	iss := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Evidence", Agent: "claude"})
	runs := make([]TestRun, maxEvidenceRuns+3)
	for i := range runs {
		runs[i] = TestRun{Outcome: EvidencePass}
	}
	a.evidence(iss.Key, "sha1", "ran a lot", runs...)
	if n := dropCount(t, a.env, DropEvidenceRunsCap); n != 3 {
		t.Errorf("evidence cap counted %d, want 3", n)
	}
}

// One source decides the agent label: the request's identity, else the CLI
// the hook names; never a blanket Claude Code default.
func TestAgentLabelHasOneSource(t *testing.T) {
	for _, c := range []struct {
		agent, cli, want string
	}{
		{"", "", "anthropic/claude-code"},
		{"", "claude", "anthropic/claude-code"},
		{"", "gemini", "google/gemini-cli"},
		{"", "Codex", "openai/codex"},
		{"", "unheard-of", "unheard-of"},
		{"acme/own-agent", "gemini", "acme/own-agent"}, // identity beats the cli
	} {
		if got := agentLabel(&Actor{Agent: c.agent}, c.cli); got != c.want {
			t.Errorf("agentLabel(%q, %q) = %q, want %q", c.agent, c.cli, got, c.want)
		}
	}
	e := newEnv(t)
	plain := *e.admin
	plain.Agent = ""
	res, err := e.s.SessionHook(e.ctx, &plain, []HookEvent{hook("m1", "gem-1", "SessionStart", func(h *HookEvent) { h.CLI = "gemini" })})
	if err != nil {
		t.Fatal(err)
	}
	var agent string
	_ = e.s.db.R.QueryRow(`SELECT agent FROM agent_sessions WHERE id = ?`, res.Session).Scan(&agent)
	if agent != "google/gemini-cli" {
		t.Errorf("a gemini session was recorded as %q", agent)
	}
}

// The "session data" collector totals what was dropped, so a drop reaches
// collector health.
func TestSessionDataCollectorTotalsDrops(t *testing.T) {
	c := newCollectorEnv(t)
	c.s.CheckCollectorHealth(c.ctx)
	h := c.health(t, "session data")
	if h.Dropped == nil || *h.Dropped != 0 {
		t.Errorf("before any drop: %+v", h)
	}
	c.s.CountUnregisteredWrite(c.ctx, c.admin.User.ID, "box")
	c.s.CheckCollectorHealth(c.ctx)
	if h = c.health(t, "session data"); h.Dropped == nil || *h.Dropped != 1 {
		t.Errorf("after a drop: %+v", h)
	}
}

// Of the agent-written events, how many carry the session.
func TestServerStatusCountsAgentEventsWithASession(t *testing.T) {
	e := newEnv(t)
	now := e.clockNow().UnixMilli()
	for _, data := range []string{
		`{"via":"anthropic/claude-code","session":5}`,
		`{"via":"anthropic/claude-code"}`,
		`{"via":"mcp","session":0}`,
		`{"session":9}`, // not agent-written: no via
	} {
		if _, err := e.s.db.W.Exec(`INSERT INTO events(actor_id, kind, data, created_at) VALUES (?, 'issue.updated', ?, ?)`, e.admin.User.ID, data, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.s.agentEventSessions(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != 3 || got.WithSession != 1 || got.Sampled < 4 || got.Sampled > agentEventsSampled {
		t.Errorf("agent events = %+v, want 3 agent-written, 1 with a session, of the events read", got)
	}
}
