package core

import (
	"strings"
	"testing"
)

// A pause with running sessions included (WP-146).

func TestSessionPauseWindsDownSessions(t *testing.T) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	if st, err := be.s.SessionPause(be.ctx, be.agent); err != nil || st.Pause != nil {
		t.Fatalf("no pause yet: %+v %v", st, err)
	}
	// A queue-only pause leaves sessions alone.
	if _, err := be.s.PauseAgents(be.ctx, be.admin, "any", "queue only", false); err != nil {
		t.Fatal(err)
	}
	if res := be.hookAs("PostToolUse"); res.Pause != nil {
		t.Fatalf("a queue pause reached the session: %+v", res.Pause)
	}
	if _, err := be.s.PauseAgents(be.ctx, be.admin, "any", "quota jumped", true); err != nil {
		t.Fatal(err)
	}
	st, err := be.s.SessionPause(be.ctx, be.agent)
	if err != nil || st.Pause == nil || !st.Pause.Sessions {
		t.Fatalf("session pause = %+v %v", st, err)
	}
	if res := be.hookAs("PostToolUse"); res.Pause == nil {
		t.Fatal("the hook reply doesn't carry the pause")
	}

	// A Stop takes nothing that would carry the session on.
	be.s.queueMessage(be.ctx, be.agent.Session, "do the next thing", "test:next", messageFrom{}, false)
	res := be.hookAs("Stop")
	if len(res.Messages) != 0 || res.Block != "" {
		t.Fatalf("a paused stop took %d messages (block %q): %q", len(res.Messages), res.Block, res.Text)
	}
	if !strings.Contains(res.Note, "Paused (quota jumped)") {
		t.Errorf("note = %q", res.Note)
	}
	be.s.Wait()

	// Idle: nothing is posted to it while paused.
	be.runner(be.admin, "m1", nil)
	be.s.postToIdleSession(be.ctx, be.agent.Session)
	if jobs := be.messageJobs(); len(jobs) != 0 {
		t.Fatalf("posted to a paused session: %v", jobs)
	}

	// Resume posts what waited.
	if _, err := be.s.ResumeAgents(be.ctx, be.admin, "any"); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	if jobs := be.messageJobs(); len(jobs) != 1 || !strings.Contains(jobs[0], "do the next thing") {
		t.Fatalf("resume didn't post what waited: %v", jobs)
	}
}

func TestSessionPauseCoversNamedAgents(t *testing.T) {
	be := newBabysitEnv(t)
	if _, err := be.s.PauseAgents(be.ctx, be.admin, "anthropic/claude-sonnet-5-5", "", true); err != nil {
		t.Fatal(err)
	}
	if st, _ := be.s.SessionPause(be.ctx, be.agent); st.Pause != nil {
		t.Fatalf("a Sonnet pause covered an Opus session: %+v", st.Pause)
	}
	if _, err := be.s.PauseAgents(be.ctx, be.admin, "anthropic/claude-opus-5-5", "", true); err != nil {
		t.Fatal(err)
	}
	if st, _ := be.s.SessionPause(be.ctx, be.agent); st.Pause == nil {
		t.Fatal("an Opus pause missed an Opus session")
	}
}

// A paused stop leaves what was queued; when that session then ends, the
// next session in the same worktree takes it at its start (WP-579). Another
// worktree's session doesn't, and babysit news stays with its babysit.
func TestEndedSessionsMessagesGoToTheNext(t *testing.T) {
	e := newEnv(t)
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	ag, _ := e.s.TokenActor(e.ctx, tok)
	old := sessionAgent(t, e, ag, "cli-old", "/work/app", nil)
	if _, err := e.s.PauseAgents(e.ctx, e.admin, "any", "quota", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.queueMessage(e.ctx, old.Session, "the answer you waited for", "test:answer", messageFrom{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.storeMessage(e.ctx, old.Session, "babysit news", "babysit:1:x", messageFrom{}, true); err != nil {
		t.Fatal(err)
	}
	res, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("m1", "cli-old", "Stop", func(h *HookEvent) { h.Cwd = "/work/app" })})
	if err != nil || len(res.Messages) != 0 {
		t.Fatalf("paused stop took %+v (%v)", res.Messages, err)
	}
	endSession(t, e, ag, "cli-old", "/work/app", "other")
	if _, err := e.s.ResumeAgents(e.ctx, e.admin, "any"); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()

	res, err = e.s.SessionHook(e.ctx, ag, []HookEvent{hook("m1", "cli-elsewhere", "SessionStart", func(h *HookEvent) { h.Cwd = "/work/other" })})
	if err != nil || len(res.Messages) != 0 {
		t.Fatalf("another worktree took %+v (%v)", res.Messages, err)
	}
	res, err = e.s.SessionHook(e.ctx, ag, []HookEvent{hook("m1", "cli-new", "SessionStart", func(h *HookEvent) { h.Cwd = "/work/app" })})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Body != "the answer you waited for" {
		t.Fatalf("next session got %+v", res.Messages)
	}
	if m, _ := e.s.messageByKey(e.ctx, "babysit:1:x"); m == nil || m.sessionID != old.Session || m.State != MessageQueued {
		t.Errorf("babysit news moved: %+v", m)
	}
}
