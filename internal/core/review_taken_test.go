package core

import "testing"

// takenBy: the round's run ended (its claim expired) and a run on the task
// opened, of the round's own session or of another one (WP-1828).
func (e *loopEnv) takenBy(round int64, sameSession bool) roundIn {
	e.t.Helper()
	now := e.clockNow().UnixMilli()
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'expired' WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, now, round)
	sid := any(e.session)
	if !sameSession {
		res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at) VALUES (?, 'other', 'c2', ?, ?, ?)`, e.admin.User.ID, e.task.ID, now, now)
		if err != nil {
			e.t.Fatal(err)
		}
		sid, _ = res.LastInsertId()
	}
	e.exec(`INSERT INTO agent_runs(issue_id, agent, user_id, started_at, session_id) VALUES (?, 'claude', ?, ?, ?)`, e.task.ID, e.admin.User.ID, now, sid)
	in, err := e.s.roundInOf(e.ctx, round)
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

// A round's own session that took the task again after its claim expired
// still holds it: the round goes on.
func TestRoundSessionTakingTheTaskAgainIsNotAnotherHolder(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.doneRound(r1)
	e.exec(`UPDATE code_audit_findings SET state = 'fixed' WHERE audit_id = (SELECT audit_id FROM work_log WHERE id = ?)`, r1)
	in := e.takenBy(r1, true)
	if in.Taken {
		t.Fatal("the round's own session, holding the task again, counted as another session")
	}
	if out := decideRound(in); out.Do != doReleaseAfterRunEnded {
		t.Fatalf("decideRound = %+v: want the round opened after its run ended", out)
	}
}

// A different session's open run is still another holder.
func TestOtherSessionOpenRunStillTakesTheTask(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.doneRound(r1)
	e.exec(`UPDATE code_audit_findings SET state = 'fixed' WHERE audit_id = (SELECT audit_id FROM work_log WHERE id = ?)`, r1)
	in := e.takenBy(r1, false)
	if !in.Taken {
		t.Fatal("another session's open run was not Taken")
	}
	if out := decideRound(in); out.Do == doReleaseAfterRunEnded {
		t.Fatalf("decideRound = %+v: a round another session holds must not ship", out)
	}
}
