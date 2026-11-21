package core

import (
	"strings"
	"testing"
	"time"
)

// A review round's audit outlives the round (WP-1059): a round that ends
// without it hands it to the task's pull request, rather than cancelling it
// or leaving it with nobody.

// handoffStart gives the task a pull request (#7, head wp-1), has the session
// ship it and asks for the round's audit. It returns the round and its audit.
func handoffStart(t *testing.T, a *apEnv) (round, audit, prID int64) {
	t.Helper()
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	if _, err := a.s.db.W.Exec(`UPDATE pull_requests SET head_ref = 'wp-1' WHERE number = 7`); err != nil {
		t.Fatal(err)
	}
	if err := a.s.db.R.QueryRow(`SELECT id FROM pull_requests WHERE number = 7`).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Shipped as #7.")
	round = a.step().reviewID.Int64
	a.tick(61 * time.Second)
	ra, au := a.roundAuditRow(round)
	if au == nil || au.Purpose != AuditReview {
		t.Fatalf("round audit = %+v %+v", ra, au)
	}
	return round, ra.id, prID
}

type auditState struct {
	purpose, status string
	prID            int64
	fixIssue        int64
}

func handoffAudit(t *testing.T, a *apEnv, id int64) auditState {
	t.Helper()
	a.s.Wait()
	var st auditState
	if err := a.s.db.R.QueryRow(`SELECT purpose, status, coalesce(pr_id, 0), coalesce(fix_issue_id, 0) FROM code_audits WHERE id = ?`, id).
		Scan(&st.purpose, &st.status, &st.prID, &st.fixIssue); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRoundAuditOutlivesTheRound(t *testing.T) {
	// The agent skipped the review while the audit ran: the audit goes on as
	// the pull request's, holds the merge while it runs, and its findings
	// hold the merge and are a babysitting round's work after.
	t.Run("skipped while it runs", func(t *testing.T) {
		a, box := auditAPEnv(t)
		round, audit, prID := handoffStart(t, a)
		a.s.setRoundNow(a.ctx, round, []string{RoundPending}, RoundSkipped, "the agent skipped it: in-session review")
		a.s.sweepRounds(a.ctx)
		st := handoffAudit(t, a, audit)
		if st.status == AuditCancelled || st.purpose != "" || st.prID != prID {
			t.Fatalf("after the skip: %+v", st)
		}
		if going, ok := a.s.auditGoing(a.ctx, prID); !ok || going != audit {
			t.Errorf("the handed audit doesn't hold the merge: %d %v", going, ok)
		}
		if why, _, _ := a.s.auditHolds(a.ctx, prID, audit); len(why) == 0 || !strings.Contains(why[0], "from an adversarial review") {
			t.Errorf("hold = %q", why)
		}
		a.endAudit(box, "done", "Merge reads a nil map")
		if len(a.jobs) != 0 {
			t.Errorf("the skipped round was typed: %+v", a.jobs)
		}
		why, _, _ := a.s.auditHolds(a.ctx, prID, 0)
		if len(why) == 0 || !strings.Contains(why[0], "audit #"+itoa64(audit)) {
			t.Errorf("the findings don't hold the merge: %q", why)
		}
		b := &Babysit{}
		if got := a.s.auditRoundWork(a.ctx, b, prID); len(got) != 1 {
			t.Errorf("babysitting work = %v", got)
		}
	})
	// The round audited the branch before its pull request opened: the
	// audit waits for one on that branch.
	t.Run("a branch's audit waits for its pull request", func(t *testing.T) {
		a, _ := auditAPEnv(t)
		round, audit, prID := handoffStart(t, a)
		if _, err := a.s.db.W.Exec(`UPDATE code_audits SET target = 'branch', ref = 'wp-1', pr_id = NULL, pr_number = 0 WHERE id = ?`, audit); err != nil {
			t.Fatal(err)
		}
		if _, err := a.s.db.W.Exec(`UPDATE pull_requests SET head_ref = 'other' WHERE id = ?`, prID); err != nil {
			t.Fatal(err)
		}
		a.s.setRoundNow(a.ctx, round, []string{RoundPending}, RoundFailed, "the runner couldn't start it")
		a.s.sweepRounds(a.ctx)
		if st := handoffAudit(t, a, audit); st.status == AuditCancelled || st.purpose != AuditReview || st.prID != 0 {
			t.Fatalf("with no pull request: %+v", st)
		}
		if _, err := a.s.db.W.Exec(`UPDATE pull_requests SET head_ref = 'wp-1' WHERE id = ?`, prID); err != nil {
			t.Fatal(err)
		}
		a.s.sweepRounds(a.ctx)
		if st := handoffAudit(t, a, audit); st.purpose != "" || st.prID != prID {
			t.Fatalf("with its pull request open: %+v", st)
		}
	})
	// The audit finished, the round failed before the session read it, and
	// the pull request merged: a task to check its findings.
	t.Run("merged with findings to do", func(t *testing.T) {
		a, box := auditAPEnv(t)
		round, audit, prID := handoffStart(t, a)
		a.endAudit(box, "done", "Merge reads a nil map")
		a.s.setRoundNow(a.ctx, round, []string{RoundPending, RoundRunning}, RoundFailed, "no report came back within 2 hours")
		if _, err := a.s.db.W.Exec(`UPDATE pull_requests SET state = 'merged' WHERE id = ?`, prID); err != nil {
			t.Fatal(err)
		}
		a.s.sweepRounds(a.ctx)
		st := handoffAudit(t, a, audit)
		if st.purpose != "" || st.fixIssue == 0 {
			t.Fatalf("after the merge: %+v", st)
		}
		var title string
		_ = a.s.db.R.QueryRow(`SELECT title FROM issues WHERE id = ?`, st.fixIssue).Scan(&title)
		if !strings.Contains(title, "Check audit #"+itoa64(audit)) {
			t.Errorf("fix task = %q", title)
		}
		a.s.sweepRounds(a.ctx)
		var n int
		_ = a.s.db.R.QueryRow(`SELECT count(*) FROM issues WHERE title LIKE ?`, "Check audit #"+itoa64(audit)+"%").Scan(&n)
		if n != 1 {
			t.Errorf("%d fix tasks", n)
		}
	})
	// A round that read its audit keeps it.
	t.Run("a done round keeps it", func(t *testing.T) {
		a, box := auditAPEnv(t)
		round, audit, _ := handoffStart(t, a)
		a.endAudit(box, "done", "Merge reads a nil map")
		a.s.setRoundNow(a.ctx, round, []string{RoundPending, RoundRunning}, RoundDone, "")
		a.s.sweepRounds(a.ctx)
		if st := handoffAudit(t, a, audit); st.purpose != AuditReview {
			t.Fatalf("after the round: %+v", st)
		}
	})
	// A person called the review off: the audit is cancelled, as before.
	t.Run("a person cancels it", func(t *testing.T) {
		a, _ := auditAPEnv(t)
		round, audit, _ := handoffStart(t, a)
		a.s.setRoundNow(a.ctx, round, []string{RoundPending}, RoundSkipped, roundCancelledBy+"a-person")
		a.s.sweepRounds(a.ctx)
		if st := handoffAudit(t, a, audit); st.status != AuditCancelled || st.purpose != AuditReview {
			t.Fatalf("after the cancel: %+v", st)
		}
	})
	// An audit older than the hand-over looks back is nobody's: the cancel
	// sweep still cancels it rather than leave it running.
	t.Run("older than the hand-over window", func(t *testing.T) {
		a, _ := auditAPEnv(t)
		round, audit, _ := handoffStart(t, a)
		old := a.s.nowMS() - handOverWithin.Milliseconds() - 60_000
		if _, err := a.s.db.W.Exec(`UPDATE code_audits SET created_at = ? WHERE id = ?`, old, audit); err != nil {
			t.Fatal(err)
		}
		a.s.setRoundNow(a.ctx, round, []string{RoundPending}, RoundSkipped, "the agent skipped it: in-session review")
		a.s.sweepRounds(a.ctx)
		if st := handoffAudit(t, a, audit); st.status != AuditCancelled || st.purpose != AuditReview {
			t.Fatalf("an old audit: %+v", st)
		}
	})
	// A later round reviews the task: the earlier one's audit is moot.
	t.Run("a later round", func(t *testing.T) {
		a, _ := auditAPEnv(t)
		round, audit, _ := handoffStart(t, a)
		a.s.setRoundNow(a.ctx, round, []string{RoundPending}, RoundInterrupted, "the session ended during the review")
		if _, err := a.s.db.W.Exec(`INSERT INTO work_log(kind, session_id, run_id, issue_id, state, prompt, created_at, updated_at)
			SELECT kind, session_id, run_id, issue_id, 'done', prompt, created_at, updated_at FROM work_log WHERE id = ?`, round); err != nil {
			t.Fatal(err)
		}
		a.s.sweepRounds(a.ctx)
		if st := handoffAudit(t, a, audit); st.status != AuditCancelled || st.purpose != AuditReview {
			t.Fatalf("with a later round: %+v", st)
		}
	})
}

// The round's audit runs past the round's patience: Sonnet reviews it in a
// new session beside it, and when that ends first its findings are the
// round's; the audit isn't cancelled, and it goes on as the pull request's
// once the round is past pending, so its findings still come to the change.
func TestRoundOutwaitsItsAudit(t *testing.T) {
	a, box := auditAPEnv(t)
	round, audit, prID := handoffStart(t, a)
	a.s.Wait()
	job := a.poll(a.admin, box, RunnerPoll{}).Job
	if job == nil || job.Kind != JobAudit {
		t.Fatalf("no audit job: %+v", job)
	}
	a.tick(30 * time.Minute)
	st := a.step()
	if st.reason != WaitAudit || len(a.jobs) != 0 {
		t.Fatalf("gave up within its patience: %+v %+v", st, a.jobs)
	}
	if !strings.Contains(st.detail, "a Sonnet review in a new session starts beside it in ") {
		t.Errorf("the wait doesn't say when the Sonnet review starts: %q", st.detail)
	}
	a.tick(reviewAuditPatience)
	// The Sonnet reviewer ends first: its findings are the round's.
	a.endReviewer(box, "done", "The retry loops")
	a.tick(time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	ra, _ := a.roundAuditRow(round)
	if !strings.HasPrefix(ra.note, roundAuditOutwaited) || !strings.Contains(ra.note, "audit #"+itoa64(audit)) {
		t.Errorf("note = %q", ra.note)
	}
	if st := handoffAudit(t, a, audit); st.status == AuditCancelled {
		t.Fatalf("the slow audit was cancelled: %+v", st)
	}
	// Pending, the round still has it; once its prompt is typed, the pull
	// request does.
	a.s.sweepRounds(a.ctx)
	if st := handoffAudit(t, a, audit); st.purpose != AuditReview {
		t.Fatalf("handed over before the round started: %+v", st)
	}
	a.s.setRoundNow(a.ctx, round, []string{RoundPending}, RoundRunning, "")
	a.s.sweepRounds(a.ctx)
	if st := handoffAudit(t, a, audit); st.status == AuditCancelled || st.purpose != "" || st.prID != prID {
		t.Fatalf("after the round started: %+v", st)
	}
	if going, ok := a.s.auditGoing(a.ctx, prID); !ok || going != audit {
		t.Errorf("the slow audit doesn't hold the merge: %d %v", going, ok)
	}
	a.s.setRoundNow(a.ctx, round, []string{RoundRunning}, RoundDone, "")
	res := auditResult("Read it.", map[string]any{"title": "Merge reads a nil map", "path": "internal/merge.go", "line": 10, "severity": "major",
		"problem": "A problem", "confirm": "Merge twice"})
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, job.ID, RunnerFinish{State: "done", Result: []byte(mustJSON(res))}); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.s.Wait()
	if len(a.jobs) != 1 {
		t.Errorf("the round was typed its audit's findings too: %+v", a.jobs)
	}
	b := &Babysit{}
	if got := a.s.auditRoundWork(a.ctx, b, prID); len(got) != 1 {
		t.Errorf("babysitting work = %v", got)
	}
}
