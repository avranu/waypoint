package core

import (
	"strings"
	"testing"
	"time"
)

// standaloneAuditNews is the news key of an audit an agent asked for that
// belongs to no round and no pull request (so to no task), asked by the
// admin.
func standaloneAuditNews(t *testing.T, a *apEnv) string {
	t.Helper()
	res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, requested_by, requested_via, status, title, created_at)
		VALUES (?, (SELECT id FROM forges LIMIT 1), 'acme/app', 'branch', ?, ?, 'cli', 'done', 'the branch', ?)`,
		a.p.ID, a.admin.User.ID, a.admin.User.ID, a.clockNow().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return "audit:" + itoa64(id) + ":done"
}

// An audit nobody tied to a task, asked by a session that ended, goes to the
// requester's next session in the worktree, whatever task it is on.
func TestStandaloneAuditNewsReachesTheNextSession(t *testing.T) {
	a, _, _ := heldEnv(t)
	endedSessionWithNews(t, a, standaloneAuditNews(t, a))
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Another task", Agent: "claude"})
	if got := startSessionWithTask(t, a, other.ID); len(got) != 1 {
		t.Fatalf("the next session was handed %+v", got)
	}
}

// With no next session, the person who asked is told when the news expires,
// and the message is not dropped silently.
func TestStandaloneAuditNewsReachesThePersonWhenItExpires(t *testing.T) {
	a, _, _ := heldEnv(t)
	key := standaloneAuditNews(t, a)
	endedSessionWithNews(t, a, key)
	a.tick(orphanMessageAge + time.Hour)
	a.s.RunWorkLog(a.ctx)
	a.s.Wait()
	var n int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ? AND url LIKE '/audits/%'`, a.admin.User.ID, NotifyAudit).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the requester was told %d times, %v", n, err)
	}
}

// News expires by how long its session has been over, as moving it does,
// not by when it was queued: a message queued long before its session ended
// still goes to the next session for a day after the end.
func TestOrphanNewsExpiresByTheSessionsAge(t *testing.T) {
	a, _, held := heldEnv(t)
	key := newsKey(t, a, "audit:", a.heldRound())
	endedSessionWithNews(t, a, key)
	if _, err := a.s.db.W.Exec(`UPDATE session_messages SET created_at = created_at - ? WHERE step_key = ?`, (30 * time.Hour).Milliseconds(), key); err != nil {
		t.Fatal(err)
	}
	a.s.RunWorkLog(a.ctx)
	a.s.Wait()
	var st string
	_ = a.s.db.R.QueryRow(`SELECT state FROM session_messages WHERE step_key = ?`, key).Scan(&st)
	if st != "queued" {
		t.Fatalf("state = %q: expired while its session had been over for an hour", st)
	}
	if got := startSessionWithTask(t, a, held.ID); len(got) != 1 || !strings.Contains(got[0].Body, "news body") {
		t.Fatalf("delivered %+v", got)
	}
}
