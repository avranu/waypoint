package core

import (
	"strings"
	"testing"
	"time"
)

// sessionAgent starts a Claude Code session for the agent behind tok and
// returns the agent acting in it.
func sessionAgent(t *testing.T, e *env, ag *Actor, cli, cwd string, mod func(*HookEvent)) *Actor {
	t.Helper()
	res, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("m1", cli, "SessionStart", func(h *HookEvent) {
		h.Cwd = cwd
		if mod != nil {
			mod(h)
		}
	})})
	if err != nil {
		t.Fatal(err)
	}
	in := *ag
	in.Session = res.Session
	return &in
}

func endSession(t *testing.T, e *env, ag *Actor, cli, cwd, reason string) {
	t.Helper()
	if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("m1", cli, "SessionEnd", func(h *HookEvent) { h.Cwd, h.EndReason = cwd, reason })}); err != nil {
		t.Fatal(err)
	}
}

// Every session on a machine can be the same agent identity: a task one
// live session holds isn't handed back to another, which takes the next
// one and hears where the first is held; once that session ends, the task
// is anyone's of the identity to resume (WP-89).
func TestNextLeavesAnotherSessionsTask(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	ag, _ := e.s.TokenActor(e.ctx, tok)
	x := e.issue(e.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	y := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Second", Agent: "claude", Priority: "low"})
	a := sessionAgent(t, e, ag, "cli-a", "/work/a", nil)
	b := sessionAgent(t, e, ag, "cli-b", "/work/b", nil)

	if task, err := e.s.AgentNext(e.ctx, a, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("a next = %v, %v", task, err)
	}
	task, err := e.s.AgentNext(e.ctx, b, AgentQuery{})
	if err != nil || task == nil || task.Issue.Key != y.Key || task.Resumed {
		t.Fatalf("b next = %+v, %v", task, err)
	}
	if len(task.Skipped) != 1 || !strings.HasPrefix(task.Skipped[0], x.Key+" (another session of yours in /work/a on m1") {
		t.Errorf("skipped = %q", task.Skipped)
	}
	if again, _ := e.s.AgentNext(e.ctx, a, AgentQuery{}); again == nil || again.Issue.Key != x.Key || !again.Resumed {
		t.Errorf("a resume = %+v", again)
	}

	endSession(t, e, ag, "cli-a", "/work/a", "logout")
	c := sessionAgent(t, e, ag, "cli-c", "/work/c", nil)
	if task, err := e.s.AgentNext(e.ctx, c, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key || !task.Resumed {
		t.Fatalf("c next = %+v, %v", task, err)
	}
	if task, _ := e.s.AgentNext(e.ctx, b, AgentQuery{}); task == nil || task.Issue.Key != y.Key {
		t.Errorf("b next after c took x = %+v", task)
	}
}

// A session deep in its work (subagents, long commands) or waiting on a
// permission prompt reports nothing for well over half an hour, yet still
// holds its task: another session of the identity passes it over and hears
// how long it has been quiet. Only the claim's lease running out lets the
// task go to another session (WP-580).
func TestNextLeavesAQuietSessionsTask(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	ag, _ := e.s.TokenActor(e.ctx, tok)
	x := e.issue(e.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	y := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Second", Agent: "claude", Priority: "high"})
	a := sessionAgent(t, e, ag, "cli-a", "/work/a", nil)
	if task, err := e.s.AgentNext(e.ctx, a, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("a next = %v, %v", task, err)
	}
	if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("m1", "cli-a", "Notification", func(h *HookEvent) {
		h.Cwd, h.NotificationType = "/work/a", "permission_prompt"
	})}); err != nil {
		t.Fatal(err)
	}

	e.setClock(e.clockNow().Add(35 * time.Minute))
	b := sessionAgent(t, e, ag, "cli-b", "/work/b", nil)
	task, err := e.s.AgentNext(e.ctx, b, AgentQuery{})
	if err != nil || task == nil || task.Issue.Key != y.Key || task.Resumed {
		t.Fatalf("b next while a is quiet = %+v, %v", task, err)
	}
	if len(task.Skipped) != 1 || task.Skipped[0] != x.Key+" (another session of yours in /work/a on m1 holds it, last heard from 35m ago)" {
		t.Errorf("skipped = %q", task.Skipped)
	}
	if _, err := e.s.AgentResume(e.ctx, b, x.Key, ""); err == nil || !strings.Contains(err.Error(), "/work/a") {
		t.Errorf("b resuming a's task = %v", err)
	}
	if again, _ := e.s.AgentNext(e.ctx, a, AgentQuery{}); again == nil || again.Issue.Key != x.Key || !again.Resumed {
		t.Errorf("a resume = %+v", again)
	}

	e.setClock(e.clockNow().Add(ClaimLease + time.Minute))
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentDone(e.ctx, b, y.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, b, AgentQuery{}); err != nil || task != nil {
		t.Fatalf("b next after a's lease ran out = %+v, %v", task, err)
	}
	ready, waiting, err := e.s.agentWork(e.ctx, b, []string{b.Agent}, AgentQuery{Agent: b.Agent, Peek: true})
	if err != nil || len(ready) != 0 || len(waiting) != 1 || waiting[0].reason != WaitClaimed ||
		!strings.HasPrefix(waiting[0].detail, "Another session of yours in /work/a on m1 holds it, last heard from 2h36m ago; its claim lapsed") {
		t.Fatalf("b's queue after a's lease ran out: ready %d, waiting %d %q, %v", len(ready), len(waiting), waiting[0].detail, err)
	}

	e.setClock(e.clockNow().Add(sessionEndedAfter))
	if task, err := e.s.AgentNext(e.ctx, b, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("b next after a ended = %+v, %v", task, err)
	}
}

// The claim of a session stuck on a permission prompt lapses after the
// lease, since only the session's own reports renew it. Autopilot then
// handed its task to another session, and once the person answered both
// prompts two sessions did the same work (WP-439 on 2026-11-07). The task
// stays the quiet session's: it gets it back from wp next, and others pass
// it over until it moves on, ends, or (not waiting on its person) has been
// quiet for strandedIdle (WP-672).
func TestLapsedClaimStaysWithItsQuietSession(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	ag, _ := e.s.TokenActor(e.ctx, tok)
	x := e.issue(e.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	y := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Second", Agent: "claude", Priority: "low"})
	a := sessionAgent(t, e, ag, "cli-a", "/work/a", nil)
	if task, err := e.s.AgentNext(e.ctx, a, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("a next = %v, %v", task, err)
	}
	e.setClock(e.clockNow().Add(ClaimLease + time.Minute))
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}

	b := sessionAgent(t, e, ag, "cli-b", "/work/b", nil)
	ready, _, err := e.s.agentWork(e.ctx, b, []string{b.Agent}, AgentQuery{Agent: b.Agent, Peek: true})
	if err != nil || len(ready) != 1 || ready[0].issue.Key != y.Key {
		t.Fatalf("b's ready while a is quiet = %v, %v", ready, err)
	}
	if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("m1", "cli-a", "UserPromptSubmit", func(h *HookEvent) { h.Cwd = "/work/a" })}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, a, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("a next after its lease ran out = %+v, %v", task, err)
	}

	e.setClock(e.clockNow().Add(ClaimLease + time.Minute))
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if ready, _, _ := e.s.agentWork(e.ctx, b, []string{b.Agent}, AgentQuery{Agent: b.Agent, Peek: true}); len(ready) != 1 || ready[0].issue.Key != y.Key {
		t.Fatalf("b's ready 2h after a went quiet = %d", len(ready))
	}
	e.setClock(e.clockNow().Add(strandedIdle))
	if ready, _, _ := e.s.agentWork(e.ctx, b, []string{b.Agent}, AgentQuery{Agent: b.Agent, Peek: true}); len(ready) != 2 || ready[0].issue.Key != x.Key {
		t.Fatalf("b's ready once a is gone = %d", len(ready))
	}

	c := sessionAgent(t, e, ag, "cli-c", "/work/c", nil)
	if task, err := e.s.AgentNext(e.ctx, c, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("c next = %+v, %v", task, err)
	}
	e.setClock(e.clockNow().Add(ClaimLease + time.Minute))
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, c, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("c next again = %+v, %v", task, err)
	}
	if _, err := e.s.AgentDone(e.ctx, c, x.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, c, AgentQuery{}); err != nil || task == nil || task.Issue.Key != y.Key {
		t.Fatalf("c next after x = %+v, %v", task, err)
	}
}

// /clear ends a session and starts another in the same terminal: its
// babysit news and the task it was on go to the new one, whichever hook
// arrives first (WP-253).
func TestClearCarriesOverToTheNewSession(t *testing.T) {
	for _, startFirst := range []bool{false, true} {
		e := newEnv(t)
		p := e.project(e.admin, "App", "")
		tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
		ag, _ := e.s.TokenActor(e.ctx, tok)
		x := e.issue(e.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude"})
		old := sessionAgent(t, e, ag, "cli-old", "/work/app", nil)
		other := sessionAgent(t, e, ag, "cli-other", "/work/elsewhere", nil)
		if task, err := e.s.AgentNext(e.ctx, old, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
			t.Fatalf("next = %v, %v", task, err)
		}
		if _, err := e.s.storeMessage(e.ctx, old.Session, "Waypoint gave up babysitting me/app#7", "babysit:1:gave_up", messageFrom{}, false); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.storeMessage(e.ctx, other.Session, "news for another terminal", "babysit:2:gave_up", messageFrom{}, false); err != nil {
			t.Fatal(err)
		}
		var fresh *Actor
		if startFirst {
			fresh = sessionAgent(t, e, ag, "cli-new", "/work/app", func(h *HookEvent) { h.Source = "clear" })
			endSession(t, e, ag, "cli-old", "/work/app", "clear")
		} else {
			endSession(t, e, ag, "cli-old", "/work/app", "clear")
			fresh = sessionAgent(t, e, ag, "cli-new", "/work/app", func(h *HookEvent) { h.Source = "clear" })
		}
		for key, want := range map[string]int64{"babysit:1:gave_up": fresh.Session, "babysit:2:gave_up": other.Session} {
			m, err := e.s.messageByKey(e.ctx, key)
			if err != nil || m.sessionID != want {
				t.Errorf("start first %v: %s is session %d, want %d (%v)", startFirst, key, m.sessionID, want, err)
			}
		}
		var sess int64
		if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT session_id FROM agent_runs WHERE ended_at IS NULL`).Scan(&sess); err != nil || sess != fresh.Session {
			t.Errorf("start first %v: the open run is session %d, want %d (%v)", startFirst, sess, fresh.Session, err)
		}
	}
}

// A session that goes idle (waiting for its person, or compacting its
// context) pauses its clock but still holds the task: another session of
// the same identity takes the next one instead (WP-566). /clear while idle
// carries the paused task to the new session.
func TestNextLeavesAnotherSessionsPausedTask(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	ag, _ := e.s.TokenActor(e.ctx, tok)
	x := e.issue(e.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	y := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Second", Agent: "claude", Priority: "high"})
	z := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Third", Agent: "claude", Priority: "low"})
	a := sessionAgent(t, e, ag, "cli-a", "/work/a", nil)
	b := sessionAgent(t, e, ag, "cli-b", "/work/b", nil)
	if task, err := e.s.AgentNext(e.ctx, a, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("a next = %v, %v", task, err)
	}
	if _, err := e.s.AgentPause(e.ctx, a, x.Key, ""); err != nil {
		t.Fatal(err)
	}
	task, err := e.s.AgentNext(e.ctx, b, AgentQuery{})
	if err != nil || task == nil || task.Issue.Key != y.Key || task.Resumed {
		t.Fatalf("b next while a is idle = %+v, %v", task, err)
	}
	if len(task.Skipped) != 1 || !strings.HasPrefix(task.Skipped[0], x.Key+" (another session of yours in /work/a") {
		t.Errorf("skipped = %q", task.Skipped)
	}

	if _, err := e.s.AgentResume(e.ctx, a, x.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentPause(e.ctx, b, y.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentResume(e.ctx, b, y.Key, ""); err != nil {
		t.Fatal(err)
	}
	if task, _ := e.s.AgentNext(e.ctx, b, AgentQuery{}); task == nil || task.Issue.Key != y.Key || !task.Resumed {
		t.Errorf("b resumes its own = %+v", task)
	}

	if _, err := e.s.AgentPause(e.ctx, a, x.Key, ""); err != nil {
		t.Fatal(err)
	}
	endSession(t, e, ag, "cli-a", "/work/a", "clear")
	fresh := sessionAgent(t, e, ag, "cli-a2", "/work/a", func(h *HookEvent) { h.Source = "clear" })
	if _, err := e.s.AgentDone(e.ctx, b, y.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, b, AgentQuery{}); err != nil || task == nil || task.Issue.Key != z.Key {
		t.Fatalf("b next after a's /clear = %+v, %v", task, err)
	}
	if task, _ := e.s.AgentNext(e.ctx, fresh, AgentQuery{}); task == nil || task.Issue.Key != x.Key || !task.Resumed {
		t.Errorf("the cleared session resumes x = %+v", task)
	}
}

// The hook resumes a directory's task at each prompt, and that task may be
// one another session there took and left: the hook's resume (only own)
// leaves it alone, so the new session holds no run it never took. Its own
// paused task, or one /clear carried over, resumes as before (WP-720).
func TestHookResumeLeavesAnotherSessionsTask(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	ag, _ := e.s.TokenActor(e.ctx, tok)
	x := e.issue(e.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	y := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Second", Agent: "claude", Priority: "high"})
	open := func(a *Actor) int {
		var n int
		if err := e.s.db.R.QueryRow(`SELECT count(*) FROM agent_runs WHERE session_id = ? AND ended_at IS NULL`, a.Session).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	a := sessionAgent(t, e, ag, "cli-a", "/work/a", nil)
	if task, err := e.s.AgentNext(e.ctx, a, AgentQuery{}); err != nil || task == nil || task.Issue.Key != x.Key {
		t.Fatalf("a next = %v, %v", task, err)
	}
	if _, err := e.s.AgentPause(e.ctx, a, x.Key, ""); err != nil {
		t.Fatal(err)
	}
	endSession(t, e, ag, "cli-a", "/work/a", "prompt_input_exit")
	b := sessionAgent(t, e, ag, "cli-b", "/work/a", nil)
	if _, err := e.s.AgentResumeOwn(e.ctx, b, x.Key, ""); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Errorf("the hook resuming a's task in b = %v", err)
	}
	if n := open(b); n != 0 {
		t.Errorf("b holds %d runs it never took", n)
	}
	if _, err := e.s.AgentResume(e.ctx, b, x.Key, ""); err != nil || open(b) != 1 {
		t.Errorf("wp resume in b = %v, open %d", err, open(b))
	}

	c := sessionAgent(t, e, ag, "cli-c", "/work/c", nil)
	if task, err := e.s.AgentNext(e.ctx, c, AgentQuery{}); err != nil || task == nil || task.Issue.Key != y.Key {
		t.Fatalf("c next = %v, %v", task, err)
	}
	if _, err := e.s.AgentPause(e.ctx, c, y.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentResumeOwn(e.ctx, c, y.Key, ""); err != nil || open(c) != 1 {
		t.Errorf("c resuming its own = %v, open %d", err, open(c))
	}
	if _, err := e.s.AgentPause(e.ctx, c, y.Key, ""); err != nil {
		t.Fatal(err)
	}
	endSession(t, e, ag, "cli-c", "/work/c", "clear")
	fresh := sessionAgent(t, e, ag, "cli-c2", "/work/c", func(h *HookEvent) { h.Source = "clear" })
	if _, err := e.s.AgentResumeOwn(e.ctx, fresh, y.Key, ""); err != nil || open(fresh) != 1 {
		t.Errorf("resume after /clear = %v, open %d", err, open(fresh))
	}
}
