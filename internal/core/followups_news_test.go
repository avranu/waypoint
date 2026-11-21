package core

import (
	"strings"
	"testing"
	"time"
)

// endedSessionWithNews is a session that ended in the cwd of the held task's
// session, and the news (stepKey) still queued for it.
func endedSessionWithNews(t *testing.T, a *apEnv, stepKey string) int64 {
	t.Helper()
	now := a.clockNow().UnixMilli()
	res, err := a.s.db.W.Exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, cwd, state, started_at, last_seen_at, ended_at)
		VALUES (?, 'box', 'old-cli', '/work/app', 'ended', ?, ?, ?)`, a.admin.User.ID, now-3600_000, now-3000_000, now-3000_000)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := a.s.db.W.Exec(`INSERT INTO session_messages(session_id, body, step_key, created_at, passive) VALUES (?, 'news body', ?, ?, 0)`, id, stepKey, now-3000_000); err != nil {
		t.Fatal(err)
	}
	return id
}

// startSessionWithTask starts a new session in the same cwd, its open run on
// the issue, and returns the messages its SessionStart hook delivered.
func startSessionWithTask(t *testing.T, a *apEnv, issueID int64) []*SessionMessage {
	t.Helper()
	now := a.clockNow().UnixMilli()
	res, err := a.s.db.W.Exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, cwd, state, started_at, last_seen_at)
		VALUES (?, 'box', 'new-cli', '/work/app', 'idle', ?, ?)`, a.admin.User.ID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := res.LastInsertId()
	if _, err := a.s.db.W.Exec(`INSERT INTO agent_runs(issue_id, agent, user_id, started_at, session_id) VALUES (?, 'claude', ?, ?, ?)`, issueID, a.admin.User.ID, now, sid); err != nil {
		t.Fatal(err)
	}
	// Its start: the hook takes what is queued for it.
	out, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", "new-cli", "SessionStart", func(h *HookEvent) { h.Source = "startup" })})
	if err != nil {
		t.Fatal(err)
	}
	return out.Messages
}

// newsKey is the step key of news about the held task's round: a ship's by
// the round, an audit's or a check's by the audit that carries the round's
// request key (the round's own, or its check's).
func newsKey(t *testing.T, a *apEnv, prefix string, round int64) string {
	t.Helper()
	if prefix == "shipped:" {
		return prefix + itoa64(round)
	}
	key := "review-round:" + itoa64(round)
	if prefix == "check:" {
		key += ":check"
	}
	res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key) VALUES (?, (SELECT id FROM forges LIMIT 1), 'acme/app', 'branch', ?, ?, ?)`,
		a.p.ID, a.admin.User.ID, a.clockNow().UnixMilli(), key)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return prefix + itoa64(id) + ":done"
}

// News of one task's audit, check or ship, queued for a session that ended,
// is not handed to the next session in the cwd when that one is on another
// task (WP-1370).
func TestTaskNewsStaysWithItsTask(t *testing.T) {
	for _, prefix := range []string{"shipped:", "audit:", "check:"} {
		t.Run(prefix, func(t *testing.T) {
			a, _, _ := heldEnv(t)
			round := a.heldRound()
			key := newsKey(t, a, prefix, round)
			old := endedSessionWithNews(t, a, key)
			other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Another task", Agent: "claude"})
			got := startSessionWithTask(t, a, other.ID)
			if len(got) != 0 {
				t.Fatalf("another task's news was delivered to the new session: %+v", got)
			}
			var sid int64
			_ = a.s.db.R.QueryRow(`SELECT session_id FROM session_messages WHERE step_key = ?`, key).Scan(&sid)
			if sid != old {
				t.Errorf("the message moved from session %d to %d", old, sid)
			}
		})
	}
}

// The same news goes on to a new session that is on its task.
func TestTaskNewsMovesToASessionOnItsTask(t *testing.T) {
	for _, prefix := range []string{"shipped:", "audit:", "check:"} {
		t.Run(prefix, func(t *testing.T) {
			a, _, held := heldEnv(t)
			endedSessionWithNews(t, a, newsKey(t, a, prefix, a.heldRound()))
			got := startSessionWithTask(t, a, held.ID)
			if len(got) != 1 || !strings.Contains(got[0].Body, "news body") {
				t.Fatalf("delivered %+v", got)
			}
		})
	}
}

// What isn't news of a task (a person's queued message) moves as before.
func TestPersonMessageMovesToTheNextSession(t *testing.T) {
	a, _, _ := heldEnv(t)
	endedSessionWithNews(t, a, "")
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Another task", Agent: "claude"})
	if got := startSessionWithTask(t, a, other.ID); len(got) != 1 {
		t.Fatalf("delivered %d", len(got))
	}
}

// News of a task nobody takes up expires after a day, with a line in the log.
func TestTaskNewsExpires(t *testing.T) {
	for _, prefix := range []string{"shipped:", "audit:", "check:"} {
		t.Run(prefix, func(t *testing.T) {
			a, _, _ := heldEnv(t)
			key := newsKey(t, a, prefix, a.heldRound())
			endedSessionWithNews(t, a, key)
			a.tick(orphanMessageAge + time.Hour)
			a.s.RunWorkLog(a.ctx)
			a.s.Wait()
			var st string
			_ = a.s.db.R.QueryRow(`SELECT state FROM session_messages WHERE step_key = ?`, key).Scan(&st)
			if st != "cancelled" {
				t.Fatalf("state = %q, want cancelled", st)
			}
		})
	}
}
