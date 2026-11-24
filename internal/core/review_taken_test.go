package core

import (
	"strings"
	"testing"
)

const takenWant = "another session holds the task"

// fixedRound is a finished round with every finding marked fixed.
func (e *loopEnv) fixedRound() int64 {
	e.t.Helper()
	r := e.review("Merge loses an update")
	e.doneRound(r)
	e.exec(`UPDATE code_audit_findings SET state = 'fixed' WHERE audit_id = (SELECT audit_id FROM work_log WHERE id = ?)`, r)
	return r
}

// expireRun ends the round's run the way an expired claim does.
func (e *loopEnv) expireRun(round int64) {
	e.t.Helper()
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'expired' WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, e.clockNow().UnixMilli(), round)
}

// otherSession is a second live session of the same user.
func (e *loopEnv) otherSession() int64 {
	e.t.Helper()
	now := e.clockNow().UnixMilli()
	res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, cwd, started_at, last_seen_at) VALUES (?, 'otherbox', 'c2', ?, '/work/other', ?, ?)`, e.admin.User.ID, e.task.ID, now, now)
	if err != nil {
		e.t.Fatal(err)
	}
	sid, err := res.LastInsertId()
	if err != nil {
		e.t.Fatal(err)
	}
	return sid
}

// openRun opens a run on an issue for a session (nil: a run with no session).
func (e *loopEnv) openRun(issue int64, session any) {
	e.t.Helper()
	e.exec(`INSERT INTO agent_runs(issue_id, agent, user_id, started_at, session_id) VALUES (?, 'claude', ?, ?, ?)`, issue, e.admin.User.ID, e.clockNow().UnixMilli(), session)
}

func (e *loopEnv) factsOf(round int64) roundIn {
	e.t.Helper()
	in, err := e.s.roundInOf(e.ctx, round)
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

func (e *loopEnv) openRuns() (n int) {
	e.t.Helper()
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM agent_runs WHERE issue_id = ? AND ended_at IS NULL`, e.task.ID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// The round's own session took the task again after its claim expired
// (WP-1828): its new run carries the round, as if the first had never ended.
// With every finding decided, the session's ship opens the pull request and
// ends that run.
func TestRoundSessionTakingTheTaskAgainCarriesTheRound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	e.openRun(e.task.ID, e.session)
	in := e.factsOf(r1)
	if in.Taken || !in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want the session's new run to count as the round's open run", in.Taken, in.RunOpen)
	}
	if out := decideRound(in); out.Do != doShip {
		t.Fatalf("decideRound = %+v: want the reviewed round shipped", out)
	}
}

// The whole of it, as WP-1707 met it: the claim expires during the review,
// the session takes the task again, marks its findings and ships. The check
// of the marks agrees and Waypoint opens the pull request and ends the run.
func TestShipAfterTheClaimExpiredOpensThePullRequest(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.expireRun(r1)
	if _, err := e.s.AgentTake(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	if n := e.openRuns(); n != 1 {
		t.Fatalf("%d runs open after the session took the task again: want 1", n)
	}
	e.shipChecked1(r1)
	e.finishShip()
	if bp := e.bpOf(r1); bp == nil || !bp.ShipDone {
		t.Fatalf("before_pr = %+v: want the round shipped", bp)
	}
	if len(e.g.created) != 1 {
		t.Fatalf("%d pull requests opened: want 1", len(e.g.created))
	}
	if n := e.openRuns(); n != 0 {
		t.Fatalf("%d runs still open on the task after its ship", n)
	}
}

// While that session still has a finding to decide, nothing is opened behind
// its back and its run stays open.
func TestRoundOfASessionStillWorkingIsNotReleased(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.doneRound(r1)
	e.expireRun(r1)
	e.openRun(e.task.ID, e.session)
	in := e.factsOf(r1)
	if in.Undecided == 0 {
		t.Fatal("the test's finding should be undecided")
	}
	out := decideRound(in)
	if out.Do == doReleaseAfterRunEnded || out.Do == doRelease || out.Do == doShip || out.Do == doShipCarried {
		t.Fatalf("decideRound = %+v: a session still deciding its findings keeps the round", out)
	}
	if _, err := e.s.advanceRound(e.ctx, r1); err != nil {
		t.Fatal(err)
	}
	if bp := e.bpOf(r1); bp == nil || bp.ShipDone {
		t.Fatalf("before_pr = %+v: nothing should have shipped", bp)
	}
	if n := e.openRuns(); n != 1 {
		t.Fatalf("%d runs open on the task: want the session's run left open", n)
	}
}

// A session that went to another task and came back in a new run holds the
// round again: the earlier move doesn't end it.
func TestSessionBackOnTheTaskAfterAnotherHasNotMovedOn(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	other := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Another task"})
	e.openRun(other.ID, e.session)
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'done' WHERE issue_id = ?`, e.clockNow().UnixMilli(), other.ID)
	e.openRun(e.task.ID, e.session)
	if in := e.factsOf(r1); !in.RunOpen || in.Taken {
		t.Fatalf("RunOpen = %v, Taken = %v: want the session's new run on the task to carry the round", in.RunOpen, in.Taken)
	}
}

// A different session's open run is another holder: the round is left, and
// the ship's answer says who holds the task and what the caller can do.
func TestOtherSessionOpenRunTakesTheTask(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	e.openRun(e.task.ID, e.otherSession())
	in := e.factsOf(r1)
	if !in.Taken || in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want another session's run to take the task", in.Taken, in.RunOpen)
	}
	if out := decideRound(in); out.Do != doNothing || out.Settled != takenWant {
		t.Fatalf("decideRound = %+v: want the round left, %q", out, takenWant)
	}
	got, err := e.s.shipNewsNow(e.ctx, e.session, e.task.ID)
	if err != nil || got == nil {
		t.Fatalf("shipNewsNow = %+v, %v", got, err)
	}
	for _, want := range []string{"/work/other on otherbox", "wp ship " + e.task.Key + " --review"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("ship news %q: want %q in it", got.Text, want)
		}
	}
	if strings.Contains(got.Text, "Nothing more is waiting on you") {
		t.Errorf("ship news %q: the caller has something it can do", got.Text)
	}
}

// An open run with no session is somebody's work too: for a round with a
// session it is another holder, never the round's own.
func TestSessionlessOpenRunTakesTheTask(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	e.openRun(e.task.ID, nil)
	if in := e.factsOf(r1); !in.Taken || in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want a run with no session to take the task", in.Taken, in.RunOpen)
	}
}

// The round's own run, open with no session recorded, is not another holder.
func TestRoundsOwnSessionlessRunIsNotAnotherHolder(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.exec(`UPDATE agent_runs SET session_id = NULL WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, r1)
	if in := e.factsOf(r1); in.Taken || !in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: the round's own open run is its own", in.Taken, in.RunOpen)
	}
}

// A round with no session compares runs: any other open run on the task is
// another holder, and its own run is not.
func TestRoundWithNoSessionComparesRuns(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.exec(`UPDATE work_log SET session_id = NULL WHERE id = ?`, r1)
	if in := e.factsOf(r1); in.Taken {
		t.Fatal("a round with no session counted its own open run as another holder")
	}
	e.expireRun(r1)
	e.openRun(e.task.ID, e.session)
	if in := e.factsOf(r1); !in.Taken || in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want another open run to take a round with no session", in.Taken, in.RunOpen)
	}
}

// An open run on another task says nothing of who holds this one.
func TestOpenRunOnAnotherTaskDoesNotTakeThisOne(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	other := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Another task"})
	e.openRun(other.ID, e.otherSession())
	if in := e.factsOf(r1); in.Taken || in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: a run on another task is not a holder of this one", in.Taken, in.RunOpen)
	}
}
