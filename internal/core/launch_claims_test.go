package core

import (
	"strings"
	"testing"
)

// runHolder is the session on an issue's latest run, and that session's task.
func runHolder(t *testing.T, e *env, issueID int64) (session, sessionIssue int64) {
	t.Helper()
	if err := e.s.db.R.QueryRow(`SELECT coalesce(r.session_id, 0), coalesce((SELECT issue_id FROM agent_sessions x WHERE x.id = r.session_id), 0)
		FROM agent_runs r WHERE r.issue_id = ? ORDER BY r.id DESC LIMIT 1`, issueID).Scan(&session, &sessionIssue); err != nil {
		t.Fatal(err)
	}
	return session, sessionIssue
}

func launcher(t *testing.T, e *env) *Actor {
	t.Helper()
	tok, _, err := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "cc", Agent: "anthropic/claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.s.TokenActor(e.ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// WP-1037: what a launcher takes for the session it starts, the session
// holds once it registers, in each way a take can land.
func TestTaskSessionAdoptsItsLaunchersTake(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Launch", "")
	taker := launcher(t, e)
	prestart := *taker
	prestart.Prestart = true
	start := func(id, key string) int64 {
		t.Helper()
		r, err := e.s.SessionHook(e.ctx, taker, []HookEvent{hook("box", id, "SessionStart", func(ev *HookEvent) { ev.Task = key })})
		if err != nil {
			t.Fatal(err)
		}
		return r.Session
	}

	t.Run("plain", func(t *testing.T) {
		is := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Plain", Agent: "anthropic/claude-code"})
		if _, err := e.s.AgentTake(e.ctx, &prestart, is.Key, ""); err != nil {
			t.Fatal(err)
		}
		s := start("plain", is.Key)
		if got, issue := runHolder(t, e, is.ID); got != s || issue != is.ID {
			t.Fatalf("run's session %d (task %d), want %d (%d)", got, issue, s, is.ID)
		}
	})

	// Starting a fresh session on a task an older session still holds: the
	// new one takes the run over, as its own wp next KEY would (WP-89).
	t.Run("held by an older session", func(t *testing.T) {
		is := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Held", Agent: "anthropic/claude-code"})
		old, err := e.s.SessionHook(e.ctx, taker, []HookEvent{hook("box", "old", "SessionStart", nil)})
		if err != nil {
			t.Fatal(err)
		}
		inOld := *taker
		inOld.Session = old.Session
		if _, err := e.s.AgentTake(e.ctx, &inOld, is.Key, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.AgentTake(e.ctx, &prestart, is.Key, ""); err != nil {
			t.Fatal(err)
		}
		s := start("fresh", is.Key)
		if got, _ := runHolder(t, e, is.ID); got != s {
			t.Fatalf("run's session %d, want the new session %d (old %d)", got, s, old.Session)
		}
	})

	// Taking an epic hands out a sub-task: the session bound to the epic
	// holds the sub-task's run.
	t.Run("epic", func(t *testing.T) {
		epic := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Epic", Agent: "anthropic/claude-code"})
		sub := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Sub", Agent: "anthropic/claude-code", Parent: epic.Key})
		task, err := e.s.AgentTake(e.ctx, &prestart, epic.Key, "")
		if err != nil {
			t.Fatal(err)
		}
		if task.Issue.Key != sub.Key {
			t.Fatalf("took %s, want the sub-task %s", task.Issue.Key, sub.Key)
		}
		s := start("epic", epic.Key)
		if got, issue := runHolder(t, e, sub.ID); got != s || issue != sub.ID {
			t.Fatalf("sub-task run's session %d (task %d), want %d (%d)", got, issue, s, sub.ID)
		}
	})
}

// The launcher's take is unattended: a task the safety screen holds isn't
// handed to the session it starts, as the session's own take wouldn't be.
func TestPrestartTakeIsScreened(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Safe", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	taker := launcher(t, e)
	risky := e.issue(writer, IssueInput{Project: p.Key, Title: "Install gVisor", Agent: "claude", Priority: "urgent",
		Description: "Run `sudo apt-get install runsc` on farhost."})
	e.s.bg.Wait()
	prestart := *taker
	prestart.Prestart = true
	if _, err := e.s.AgentTake(e.ctx, &prestart, risky.Key, ""); err == nil || !strings.Contains(err.Error(), "cleared for agents") {
		t.Fatalf("prestart take of a held task: %v, want the screen's refusal", err)
	}
}
