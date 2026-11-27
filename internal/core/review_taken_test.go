package core

import (
	"strings"
	"testing"
	"time"
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

// openRun opens a run on an issue for a session (nil: a run with no session),
// as the agent that holds the test's task.
func (e *loopEnv) openRun(issue int64, session any) {
	e.t.Helper()
	e.openRunAs(issue, session, e.taskAgent())
}

// openRunAs opens a run on an issue for a session as the given agent.
func (e *loopEnv) openRunAs(issue int64, session any, agent string) {
	e.t.Helper()
	e.exec(`INSERT INTO agent_runs(issue_id, agent, user_id, started_at, session_id) VALUES (?, ?, ?, ?, ?)`, issue, agent, e.admin.User.ID, e.clockNow().UnixMilli(), session)
}

// taskAgent is the agent of the test task's first run (the round's).
func (e *loopEnv) taskAgent() (agent string) {
	e.t.Helper()
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT agent FROM agent_runs WHERE issue_id = ? ORDER BY id LIMIT 1`, e.task.ID).Scan(&agent); err != nil {
		e.t.Fatal(err)
	}
	return agent
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
	// The take opened a new run; it carries the round, so the round goes on
	// as the session's (RunOpen) and is not released as one whose run ended.
	if in := e.factsOf(r1); !in.RunOpen || in.Taken {
		t.Fatalf("RunOpen = %v, Taken = %v: want the session's new run to carry the round", in.RunOpen, in.Taken)
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
	// Shipped for the live session it is, not as one that ended: the pull
	// request's babysit carries the session, so its news reaches it.
	var babysits, mine int
	if err := e.s.db.R.QueryRow(`SELECT count(*), count(*) FILTER (WHERE session_id = ?) FROM babysits`, e.session).Scan(&babysits, &mine); err != nil {
		t.Fatal(err)
	}
	if babysits != 1 || mine != 1 {
		t.Fatalf("%d babysits, %d of the session's: want the pull request babysat for the session", babysits, mine)
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

// Another session's paused run (idle, waiting for its person) still holds
// the task while its agent has the claim (holdingRun): the round is left,
// not shipped under it. Once the claim lapses the paused run holds nothing.
func TestOtherSessionPausedRunTakesTheTaskWhileItHoldsTheClaim(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	e.openRun(e.task.ID, e.otherSession())
	now := e.clockNow().UnixMilli()
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'paused' WHERE issue_id = ? AND ended_at IS NULL`, now, e.task.ID)
	e.exec(`UPDATE issues SET claimed_by = ?, claimed_until = ? WHERE id = ?`, e.taskAgent(), now+time.Hour.Milliseconds(), e.task.ID)
	in := e.factsOf(r1)
	if !in.Taken || in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want another session's paused run with the claim to hold the task", in.Taken, in.RunOpen)
	}
	if out := decideRound(in); out.Do != doNothing || out.Settled != takenWant {
		t.Fatalf("decideRound = %+v: want the round left, %q", out, takenWant)
	}
	if got, err := e.s.shipNewsNow(e.ctx, e.session, e.task.ID); err != nil || got == nil || !strings.Contains(got.Text, "/work/other on otherbox") {
		t.Fatalf("shipNewsNow = %+v, %v: want the paused holder named", got, err)
	}
	// The round's session taking the task back is not under the paused run:
	// whoever took the task last holds it.
	e.openRun(e.task.ID, e.session)
	if in := e.factsOf(r1); in.Taken || !in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want the session's later run to carry the round", in.Taken, in.RunOpen)
	}
	e.exec(`DELETE FROM agent_runs WHERE issue_id = ? AND ended_at IS NULL`, e.task.ID)
	e.exec(`UPDATE issues SET claimed_by = '', claimed_until = NULL WHERE id = ?`, e.task.ID)
	if in := e.factsOf(r1); in.Taken {
		t.Fatal("a paused run whose claim lapsed still took the task")
	}
}

// The same agent taking the task again with no session (a caller that named
// none) is the round's holder still, not another (WP-1828): its run carries
// the round. Another agent's run with no session is another holder.
func TestSessionlessRunOfTheRoundsAgentCarriesTheRound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	e.openRun(e.task.ID, nil)
	in := e.factsOf(r1)
	if in.Taken || !in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want the agent's own run with no session to carry the round", in.Taken, in.RunOpen)
	}
	if out := decideRound(in); out.Do != doShip {
		t.Fatalf("decideRound = %+v: want the reviewed round shipped", out)
	}
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'expired' WHERE issue_id = ? AND ended_at IS NULL`, e.clockNow().UnixMilli(), e.task.ID)
	e.openRunAs(e.task.ID, nil, "someone/else")
	in = e.factsOf(r1)
	if !in.Taken || in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want another agent's run with no session to take the task", in.Taken, in.RunOpen)
	}
	got, err := e.s.shipNewsNow(e.ctx, e.session, e.task.ID)
	if err != nil || got == nil || !strings.Contains(got.Text, "a run of someone/else with no session holds it") {
		t.Fatalf("shipNewsNow = %+v, %v: want the holder named", got, err)
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

// A round with no session has no session to compare: a later run of its
// run's agent carries it, and another agent's run is another holder.
func TestRoundWithNoSessionComparesAgents(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.exec(`UPDATE work_log SET session_id = NULL WHERE id = ?`, r1)
	if in := e.factsOf(r1); in.Taken || !in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: a round with no session counted its own open run as another holder", in.Taken, in.RunOpen)
	}
	e.expireRun(r1)
	e.openRun(e.task.ID, e.session)
	if in := e.factsOf(r1); in.Taken || !in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want the same agent's later run to carry a round with no session", in.Taken, in.RunOpen)
	}
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'expired' WHERE issue_id = ? AND ended_at IS NULL`, e.clockNow().UnixMilli(), e.task.ID)
	e.openRunAs(e.task.ID, e.otherSession(), "someone/else")
	if in := e.factsOf(r1); !in.Taken || in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want another agent's run to take a round with no session", in.Taken, in.RunOpen)
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

// The session took the task again and let that claim lapse too: the
// round's window counts from the end of the session's later run, so the
// sweep opens its pull request though the round's own run ended longer ago
// than Waypoint keeps trying. Another holder's run ending doesn't move it.
func TestRoundWindowCountsFromTheSessionsLaterRun(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	e.setClock(e.clockNow().Add(strandedShipWindow))
	e.openRun(e.task.ID, e.session)
	e.setClock(e.clockNow().Add(time.Hour))
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'expired' WHERE issue_id = ? AND ended_at IS NULL`, e.clockNow().UnixMilli(), e.task.ID)
	if out := decideRound(e.factsOf(r1)); out.Do != doReleaseAfterRunEnded {
		t.Fatalf("decideRound = %+v: want the round opened, its window counted from the session's later run", out)
	}
	e.s.shipSettledRounds(e.ctx)
	e.s.Wait()
	if bp := e.bpOf(r1); bp == nil || !bp.ShipDone {
		t.Fatalf("before_pr = %+v: want the sweep to have shipped the round", bp)
	}
}

// Another session's run ending is not the round's: a round its own run left
// longer ago than the window is not opened when that holder leaves.
func TestOtherHoldersRunEndDoesNotReopenTheWindow(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	e.openRun(e.task.ID, e.otherSession())
	e.setClock(e.clockNow().Add(strandedShipWindow + time.Hour))
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'expired' WHERE issue_id = ? AND ended_at IS NULL`, e.clockNow().UnixMilli(), e.task.ID)
	if out := decideRound(e.factsOf(r1)); out.Do != doNothing {
		t.Fatalf("decideRound = %+v: want the round left, its own run ended longer ago than the window", out)
	}
}

// Another session took the task after the round's session took it again:
// whoever took it last holds it, so the round is Taken and not shipped
// behind that session, though the round's session's run is still open.
func TestOtherSessionAfterTheRenewedClaimTakesTheTask(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	e.openRun(e.task.ID, e.session)
	e.openRun(e.task.ID, e.otherSession())
	in := e.factsOf(r1)
	if !in.Taken || in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want the later session's run to hold the task", in.Taken, in.RunOpen)
	}
	if out := decideRound(in); out.Do != doNothing || out.Settled != takenWant {
		t.Fatalf("decideRound = %+v: want the round left for the later holder", out)
	}
	if open, err := roundRunOpen(e.ctx, e.s.db.R, r1); err != nil || open {
		t.Fatalf("roundRunOpen = %v, %v: a ship's gate must not pass behind the later holder", open, err)
	}
	// The round's session taking it back after that carries the round again.
	e.openRun(e.task.ID, e.session)
	if in := e.factsOf(r1); in.Taken || !in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want the session's newest run to carry the round", in.Taken, in.RunOpen)
	}
}

// A run with no session under the round's agent's name, opened by another
// person's token, is another holder: a sessionless caller names its agent
// unchecked, so the name alone doesn't make it the round's holder.
func TestSessionlessRunOfAnotherUserUnderTheAgentsNameTakesTheTask(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	u := e.user("mallory", false)
	e.exec(`INSERT INTO agent_runs(issue_id, agent, user_id, started_at) VALUES (?, ?, ?, ?)`, e.task.ID, e.taskAgent(), u.User.ID, e.clockNow().UnixMilli())
	in := e.factsOf(r1)
	if !in.Taken || in.RunOpen {
		t.Fatalf("Taken = %v, RunOpen = %v: want another user's run under the agent's name to be another holder", in.Taken, in.RunOpen)
	}
	got, err := e.s.shipNewsNow(e.ctx, e.session, e.task.ID)
	if err != nil || got == nil || !strings.Contains(got.Text, "That run's own `wp ship") || strings.Contains(got.Text, "That session's") {
		t.Fatalf("shipNewsNow = %+v, %v: want the sessionless holder's own ship named, not a session's", got, err)
	}
}

// A round left because another session holds the task is recorded on the
// round (before_pr's taken_by and taken_at), once per holder: the ship's
// answer, the sweep and advanceRound all write it, and reading it again
// doesn't rewrite it.
func TestRoundLeftForAnotherHolderIsRecorded(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.fixedRound()
	e.expireRun(r1)
	other := e.otherSession()
	e.openRun(e.task.ID, other)
	var holder int64
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT max(id) FROM agent_runs WHERE issue_id = ? AND ended_at IS NULL`, e.task.ID).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.shipNewsNow(e.ctx, e.session, e.task.ID); err != nil {
		t.Fatal(err)
	}
	bp := e.bpOf(r1)
	if bp == nil || bp.TakenBy != holder || bp.TakenAt == 0 {
		t.Fatalf("before_pr = %+v: want taken_by %d and a taken_at", bp, holder)
	}
	at := bp.TakenAt
	e.setClock(e.clockNow().Add(time.Minute))
	e.s.shipSettledRounds(e.ctx)
	if _, err := e.s.advanceRound(e.ctx, r1); err != nil {
		t.Fatal(err)
	}
	if bp := e.bpOf(r1); bp.TakenBy != holder || bp.TakenAt != at {
		t.Fatalf("before_pr = %+v: want the record of the same holder left as it was (taken_at %d)", bp, at)
	}
	e.exec(`UPDATE agent_runs SET ended_at = ? WHERE id = ?`, e.clockNow().UnixMilli(), holder)
	e.openRun(e.task.ID, other)
	if _, err := e.s.advanceRound(e.ctx, r1); err != nil {
		t.Fatal(err)
	}
	if bp := e.bpOf(r1); bp.TakenBy == holder || bp.TakenAt == at {
		t.Fatalf("before_pr = %+v: want the holder's new run recorded", bp)
	}
}
