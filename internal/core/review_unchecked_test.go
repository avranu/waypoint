package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A check of the marks that didn't run, then a label-only round 2 that has no
// check of its own: the pull request the server opens still says the marks
// of round 1 went unchecked and why, in its description, the session's
// message and the person's notice, and round 1's findings are in the pull
// request's todo (WP-1370).
func TestUncheckedMarksSurviveALaterRound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || !strings.Contains(res.Held, "checking your marks") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endCheck(e.box, "failed")
	e.s.Wait()
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d pull requests around the unchecked marks", len(e.g.created))
	}
	if _, _, _, _, next := e.decision(r1); next == 0 {
		t.Fatal("no second round was scheduled after the check failed")
	}
	// Round 2 is label-only (no check of its own) and finds nothing: the
	// server opens the pull request, with no session ship.
	e.setClock(e.clockNow().Add(time.Minute))
	var r2 int64
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship for round 2 = %+v, %v", res, err)
	}
	e.stopWith("Pushed; the review comes first.")
	r2 = e.step().reviewID.Int64
	e.tick(61 * time.Second)
	e.endAudit(e.box, "done")
	e.tick(time.Second)
	e.tickAll(10 * time.Minute)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("opened %d pull requests; round 2 %s (%s)", len(e.g.created), e.round(r2).state, e.roundReason(r2))
	}
	const unchecked = "the check of the marks didn't run"
	if b := e.g.created[0].Body; !strings.Contains(b, unchecked) {
		t.Errorf("the pull request's description doesn't say the check didn't run:\n%s", b)
	}
	var msg string
	if err := e.s.db.R.QueryRow(`SELECT body FROM session_messages WHERE session_id = ? AND step_key LIKE 'shipped:%'`, e.ag.Session).Scan(&msg); err != nil {
		t.Fatalf("no queued message: %v", err)
	}
	if !strings.Contains(msg, unchecked) {
		t.Errorf("the session's message doesn't say it: %s", msg)
	}
	if n := e.noticeWith(unchecked); n == "" {
		t.Errorf("the person's notice doesn't say it: %v", e.notices())
	}
	var prID int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM pull_requests ORDER BY id DESC LIMIT 1`).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	todo, err := e.s.auditTodoErr(e.ctx, prID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range todo {
		found = found || strings.Contains(f.Title, "Merge loses an update")
	}
	if !found {
		t.Errorf("round 1's unchecked finding isn't in the pull request's todo: %+v", todo)
	}
}

// openedThenLost is a round whose pull request is open (linked to the task) and
// whose ship the process died in: the round runs and the run is open again, as
// the resume test sets it. The first ship's message and notices are cleared,
// so what the second says is what is read.
func openedThenLost(t *testing.T) (*loopEnv, int64) {
	t.Helper()
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"rejected"})
	if len(e.g.created) != 1 || e.round(r1).state != RoundDone {
		t.Fatalf("setup: %d pull requests, round %+v", len(e.g.created), e.round(r1))
	}
	e.exec(`UPDATE work_log SET state = 'running', ended_at = NULL, read_state = 'pending', before_pr = json_remove(before_pr, '$.ship_done') WHERE id = ?`, r1)
	e.exec(`UPDATE agent_runs SET ended_at = NULL WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, r1)
	e.exec(`DELETE FROM session_messages WHERE step_key LIKE 'shipped:%'`)
	e.exec(`DELETE FROM notifications`)
	return e, r1
}

// The pull request was open already (linked to the task): what the round, the
// session and the person are told says so, never that Waypoint opened it.
func TestServerShipOfAnExistingPRSaysItWasOpen(t *testing.T) {
	e, r1 := openedThenLost(t)
	for range 3 {
		e.tickAll(5 * time.Minute)
	}
	var body, msg string
	_ = e.s.db.R.QueryRow(`SELECT body FROM work_log WHERE id = ?`, r1).Scan(&body)
	_ = e.s.db.R.QueryRow(`SELECT body FROM session_messages WHERE session_id = ? AND step_key LIKE 'shipped:%'`, e.ag.Session).Scan(&msg)
	if !strings.Contains(body, "already open") || strings.Contains(body, "Waypoint opened the pull request") {
		t.Errorf("the round's end says %q", body)
	}
	if !strings.Contains(msg, "already open") || strings.Contains(msg, "Waypoint opened") {
		t.Errorf("the session's message says %q", msg)
	}
	if n := e.noticeWith("already open"); n == "" || strings.Contains(n, "Waypoint opened") {
		t.Errorf("the person's notice: %v", e.notices())
	}
}

// A pull request open from the branch that is linked to no task, or to
// another one, isn't armed or babysat: the person is told and the round ends.
func TestServerShipLeavesAPRAlone(t *testing.T) {
	for _, tc := range []struct {
		name, relink string
		wantOwner    string
	}{
		{"unlinked by a person", `INSERT INTO issue_unlinks(issue_id, url, user_id, agent, created_at) SELECT issue_id, url, NULL, '', 0 FROM external_links WHERE url LIKE '%/pulls/%'; DELETE FROM external_links WHERE url LIKE '%/pulls/%'`, "isn't linked to any task"},
		{"linked to another task", `UPDATE external_links SET issue_id = (SELECT id FROM issues WHERE title = 'Second') WHERE url LIKE '%/pulls/%'`, "is linked to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r1 := openedThenLost(t)
			e.exec(`UPDATE pull_requests SET armed_at = NULL`)
			e.exec(`DELETE FROM babysits`)
			e.exec(tc.relink)
			for range 3 {
				e.tickAll(5 * time.Minute)
			}
			var armed, babysat int
			_ = e.s.db.R.QueryRow(`SELECT count(armed_at) FROM pull_requests`).Scan(&armed)
			_ = e.s.db.R.QueryRow(`SELECT count(*) FROM babysits`).Scan(&babysat)
			if armed != 0 || babysat != 0 {
				t.Errorf("armed %d, babysat %d", armed, babysat)
			}
			var body string
			_ = e.s.db.R.QueryRow(`SELECT body FROM work_log WHERE id = ?`, r1).Scan(&body)
			if !strings.Contains(body, tc.wantOwner) {
				t.Errorf("the round says %q", body)
			}
			if e.noticeCount("%pull request not opened") != 1 {
				t.Errorf("told %d times: %v", e.noticeCount("%pull request not opened"), e.notices())
			}
		})
	}
}

// The run ends while a ship is held: the round reaches an end, whatever state
// it was in, and doesn't stay pending or running for ever (WP-1370).
func TestRunEndedUnderAHeldShipEndsItsRound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	round := e.step().reviewID.Int64
	if st := e.round(round).state; st != RoundPending {
		t.Fatalf("setup: round %s", st)
	}
	e.exec(`UPDATE autopilot_steps SET state = 'due' WHERE review_id = ?`, round) // a step still carries it
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'done' WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, e.clockNow().UnixMilli(), round)
	e.s.sweepRounds(e.ctx)
	if st := e.round(round).state; st != RoundSkipped {
		t.Errorf("a pending round of an ended run is %s", st)
	}
	e.exec(`UPDATE work_log SET state = 'running' WHERE id = ?`, round)
	e.s.sweepRounds(e.ctx)
	if st := e.round(round).state; st != RoundInterrupted {
		t.Errorf("a running round of an ended run is %s", st)
	}
}

// The session ended with the run open: the pull request opens, and the
// babysit and the pull request's claim aren't recorded for a dead session.
func TestServerShipForAnEndedSessionRecordsNoDeadSession(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"rejected"}, "[x.1] Merge loses an update") // the check disagrees: the session decides it
	e.endSession()
	e.tickAll(heldScreenWait + 10*time.Minute)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("opened %d pull requests; round %s", len(e.g.created), e.round(r1).state)
	}
	var babysitSessions, claims int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM babysits WHERE session_id = ?`, e.session).Scan(&babysitSessions)
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM pr_claims WHERE session_id = ?`, e.session).Scan(&claims)
	if babysitSessions != 0 || claims != 0 {
		t.Errorf("recorded for the dead session: %d babysits, %d claims", babysitSessions, claims)
	}
	var babysits int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM babysits`).Scan(&babysits)
	if babysits != 1 {
		t.Errorf("%d babysits: the pull request is still babysat, as no one's", babysits)
	}
}

// anotherShipWent is a round recorded under the ship that is about to write
// its own, as a ship of the task's other branch, or the sweep, would (the
// branch's own ships queue on its lock): the one write after it meets
// ErrConflict.
func (e *loopEnv) anotherShipWent(ctx context.Context) {
	g, err := e.s.gateFor(ctx, e.ag, e.task.Key, false, false)
	if err != nil {
		e.t.Errorf("gate: %v", err)
		return
	}
	err = e.s.write(ctx, e.ag, func(t *tx) error {
		_, err := e.s.newReviewRound(ctx, t, g.x, g.run, g.issueID, g.lim.reviewPrompt(), e.s.nowMS())
		return err
	})
	if err != nil {
		e.t.Errorf("the other ship's round: %v", err)
	}
}

func (e *loopEnv) reviewRounds() int {
	e.t.Helper()
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM work_log WHERE kind = 'review' AND issue_id = ?`, e.task.ID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// A ship that finds the round moved under it asks once more and reads where
// the other left it, so the session is told the truth and isn't sent to run
// the same command again (WP-1370).
func TestFirstShipRetriesOnceWhenAnotherShipWentFirst(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	e.s.beforeRoundWrite = e.anotherShipWent
	res, err := e.ship(e.task.Key, ShipAuto)
	if err != nil {
		t.Fatalf("the ship that lost the race returned %v, not where the round stands", err)
	}
	if e.s.beforeRoundWrite != nil {
		t.Fatal("the ship never reached the write the test races")
	}
	if res.Held == "" || res.Pull != nil {
		t.Fatalf("after the retry: %+v", res)
	}
	if n := e.reviewRounds(); n != 1 {
		t.Fatalf("%d review rounds, want only the one the other ship made", n)
	}
}

// reviewAgain's conflict: another ship recorded a round while the check's
// end was deciding whether to schedule one, so reviewAgain's write meets
// ErrConflict, schedules nothing beside the other's round, and the pull
// request isn't opened around it.
func TestReviewAgainMeetsAConflictWithAnotherShip(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || !strings.Contains(res.Held, "checking your marks") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Waiting for the check of my marks.")
	before := e.reviewRounds()
	e.s.beforeRoundWrite = e.anotherShipWent
	e.endCheck(e.box, "failed")
	e.s.Wait()
	if e.s.beforeRoundWrite != nil {
		t.Fatal("reviewAgain never reached the write the test races")
	}
	if n := e.reviewRounds(); n != before+1 {
		t.Fatalf("%d review rounds, want %d: reviewAgain scheduled one beside the other's", n, before+1)
	}
	if len(e.g.created) != 0 {
		t.Errorf("%d pull requests opened around the lost race", len(e.g.created))
	}
}

// aCheckAboutToEnd is a round whose check of the marks is running on a
// runner: the session has stopped to wait for it.
func aCheckAboutToEnd(t *testing.T) (*loopEnv, int64, *RunnerClaim) {
	t.Helper()
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || !strings.Contains(res.Held, "checking your marks") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.s.Wait()
	w := e.poll(e.admin, e.box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no check job: %+v", w.Job)
	}
	return e, r1, w.Job
}

func (e *loopEnv) checkStatus(round int64) string {
	e.t.Helper()
	var st string
	if err := e.s.db.R.QueryRow(`SELECT status FROM code_audits WHERE request_key = ?`, fmt.Sprintf(checkAuditKey, round)).Scan(&st); err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *loopEnv) endCheckNow(j *RunnerClaim) error {
	_, err := e.s.FinishRunnerJob(e.ctx, e.admin, j.ID, RunnerFinish{State: "done", Result: []byte(mustJSON(auditResult("Read it.")))})
	return err
}

// The check's end and the patience that cancels it meet in either order or at
// once: one of them wins, the audit stays as the winner left it, and the
// round is carried on once (WP-1370).
func TestCheckEndsOnceWhenItsEndAndItsPatienceMeet(t *testing.T) {
	t.Run("the end first", func(t *testing.T) {
		e, r1, j := aCheckAboutToEnd(t)
		if err := e.endCheckNow(j); err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
		e.setClock(e.clockNow().Add(checkPatience + time.Minute))
		e.s.expireChecks(e.ctx)
		if _, err := e.s.CancelAudit(e.ctx, SystemActor(), e.auditOfCheck(r1)); err == nil {
			t.Error("cancelling an audit that already ended didn't say so")
		}
		e.s.Wait()
		if st := e.checkStatus(r1); st != AuditDone {
			t.Errorf("the check is %q, want done", st)
		}
	})
	t.Run("the patience first", func(t *testing.T) {
		e, r1, j := aCheckAboutToEnd(t)
		e.setClock(e.clockNow().Add(checkPatience + time.Minute))
		e.s.expireChecks(e.ctx)
		_ = e.endCheckNow(j) // the runner's late answer: refused or ignored
		e.s.Wait()
		if st := e.checkStatus(r1); st != AuditCancelled {
			t.Errorf("the check is %q, want cancelled: a late answer took the round back", st)
		}
	})
	t.Run("at once", func(t *testing.T) {
		e, r1, j := aCheckAboutToEnd(t)
		e.setClock(e.clockNow().Add(checkPatience + time.Minute))
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(2)
			go func() { defer wg.Done(); e.s.expireChecks(e.ctx) }()
			go func() { defer wg.Done(); _ = e.endCheckNow(j) }()
		}
		wg.Wait()
		e.s.Wait()
		st := e.checkStatus(r1)
		if st != AuditDone && st != AuditCancelled {
			t.Fatalf("the check is %q, neither done nor cancelled", st)
		}
		e.tickAll(10 * time.Minute)
		e.s.Wait()
		if st2 := e.checkStatus(r1); st2 != st {
			t.Errorf("the check went from %q to %q after both had run", st, st2)
		}
		if n := e.decisions(); n > 1 {
			t.Errorf("%d decisions for one check", n)
		}
		if len(e.g.created) > 1 {
			t.Errorf("%d pull requests opened", len(e.g.created))
		}
	})
}

func (e *loopEnv) auditOfCheck(round int64) int64 {
	e.t.Helper()
	var id int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM code_audits WHERE request_key = ?`, fmt.Sprintf(checkAuditKey, round)).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// A round that ships ends the autopilot step carrying it in the same write
// that records the shipping: when the step can't be ended, nothing of the
// shipping stands, so the next sweep ships again rather than leaving a step
// waiting on a round that is over (WP-1370).
func TestShippedRoundEndsItsStepInOneWrite(t *testing.T) {
	t.Run("ends it", func(t *testing.T) {
		e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
		r1 := e.review("Merge loses an update")
		e.shipAndCheck(r1, []string{"rejected"})
		e.tickAll(10 * time.Minute)
		e.s.Wait()
		if len(e.g.created) != 1 || e.round(r1).state != RoundDone {
			t.Fatalf("%d pull requests, round %s (%s)", len(e.g.created), e.round(r1).state, e.roundReason(r1))
		}
		var live int
		if err := e.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE review_id = ? AND state IN (?, ?, ?)`, r1, StepDue, StepWaiting, StepStarting).Scan(&live); err != nil || live != 0 {
			t.Fatalf("%d steps still wait on the shipped round (%v)", live, err)
		}
	})
	t.Run("not apart", func(t *testing.T) {
		e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
		r1 := e.review("Merge loses an update")
		e.exec(`CREATE TRIGGER no_shipped_step BEFORE UPDATE ON autopilot_steps WHEN NEW.reason = 'shipped' BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
		e.shipAndCheck(r1, []string{"rejected"})
		e.s.Wait()
		if e.round(r1).state == RoundDone && len(e.g.created) > 0 {
			var live int
			_ = e.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE review_id = ? AND state IN (?, ?, ?)`, r1, StepDue, StepWaiting, StepStarting).Scan(&live)
			if live != 0 {
				t.Fatalf("the round is done and its step still waits: the two were written apart")
			}
		}
		if e.round(r1).state == RoundDone {
			t.Fatalf("the round is done though ending its step failed")
		}
	})
}

// What the open said of its own accord (res.Notice) reaches the person: a
// task tagged ci-urgent gets its pull request on the lane, and the person's
// notice says that, once.
func TestServerShipShowsTheOpensNotice(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, e.task.Key, IssuePatch{AddLabels: []string{CIUrgentLabel}}); err != nil {
		t.Fatal(err)
	}
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"rejected"})
	e.tickAll(10 * time.Minute)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("%d pull requests opened", len(e.g.created))
	}
	want := "is tagged " + CIUrgentLabel
	if n := e.noticeWith(want); n == "" {
		t.Fatalf("the person's notice doesn't carry the open's notice: %v", e.notices())
	} else if strings.Count(n, want) != 1 {
		t.Errorf("the open's notice is said %d times:\n%s", strings.Count(n, want), n)
	}
}
