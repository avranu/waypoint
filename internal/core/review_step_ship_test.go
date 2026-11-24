package core

import (
	"encoding/json"
	"testing"
	"time"
)

// shippedWithBox is a task shipped and held for its review, with the
// audit runner there (firstEnv): the round's audit is Grok's.
func shippedWithBox(t *testing.T) (*apEnv, *Runner, *shipForge, *IssueDetail) {
	t.Helper()
	a, box, g := firstEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	if res, err := a.ship(first.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	return a, box, g, first
}

func (a *apEnv) stepCount() int {
	a.t.Helper()
	var n int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE session_id = ?`, a.session).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

// The round's step is made by the ship that asked for the round, due at
// once, so its audit starts while the session's turn is still ending
// (WP-1370). The Stop that follows keeps the one step and the review's
// typing a minute after it.
func TestRoundStepMadeAtShip(t *testing.T) {
	a, _, _, _ := shippedWithBox(t)
	round := a.heldRound()
	st := a.step()
	if st.reviewID.Int64 != round || st.state != StepDue || st.dueAt > a.clockNow().UnixMilli() {
		t.Fatalf("step at the ship = %+v", st)
	}
	a.stopWith("Pushed; the review comes first.")
	if a.stepCount() != 1 {
		t.Fatalf("%d steps after the Stop", a.stepCount())
	}
	st = a.step()
	if st.reviewID.Int64 != round || st.state != StepDue || st.dueAt != a.clockNow().UnixMilli()+reviewStartDelay {
		t.Fatalf("step after the Stop = %+v", st)
	}
}

func TestRoundAuditStartsBeforeTheStop(t *testing.T) {
	a, _, _, _ := shippedWithBox(t)
	round := a.heldRound()
	a.tick(time.Second)
	if _, au := a.roundAuditRow(round); au == nil {
		t.Fatalf("no audit was asked for while the turn was ending; step = %+v", a.step())
	}
	// The Stop doesn't put an audit under way back to due: it only
	// records when the session stopped.
	st := a.step()
	if st.state != StepWaiting || st.reason != WaitAudit {
		t.Fatalf("step = %+v", st)
	}
	stop := a.clockNow().UnixMilli()
	a.stopWith("Pushed; the review comes first.")
	st = a.step()
	if st.state != StepWaiting || st.reason != WaitAudit || st.stopAt <= stop-1 || a.stepCount() != 1 {
		t.Fatalf("step after the Stop = %+v", st)
	}
}

// A ship that never gets a Stop still has its step, so the watchdog has
// nothing to revive (reviveHeldRounds is only a backup).
func TestRoundStepSurvivesRevive(t *testing.T) {
	a, _, _, _ := shippedWithBox(t)
	a.setClock(a.clockNow().Add(heldStopLost + time.Minute))
	// The session reads idle (its turn has gone quiet), so the revive does
	// look at the round; only its step keeps it from picking the round up.
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET state = ? WHERE id = ?`, SessionIdle, a.session); err != nil {
		t.Fatal(err)
	}
	before := *a.step()
	a.s.reviveHeldRounds(a.ctx)
	if a.stepCount() != 1 {
		t.Fatalf("%d steps", a.stepCount())
	}
	// One step by session is upserted: a revive that picked the round up
	// anyway would leave the one step, put back to due, so compare the step.
	if after := *a.step(); after != before {
		t.Fatalf("the revive touched the round's step:\n before %+v\n after  %+v", before, after)
	}
}

// The round interrupted for the last time, whose session then just
// stops, is released like a round that failed: Waypoint opens the pull
// request and says why.
func TestRoundInterruptedForTheLastTimeIsReleased(t *testing.T) {
	a, g, _ := heldEnv(t)
	for range reviewInterruptsMax {
		before := len(a.jobs)
		a.tick(61 * time.Second)
		if len(a.jobs) != before+1 {
			t.Fatalf("jobs = %d, want %d", len(a.jobs), before+1)
		}
		var in AutopilotJob
		_ = json.Unmarshal(a.jobs[before].Input, &in)
		a.prompt(in.Review)
		// The session leaves the review for another task: a person's prompt
		// alone doesn't end it any more (WP-1320).
		if err := a.s.write(a.ctx, SystemActor(), func(t *tx) error {
			return a.s.stopStep(a.ctx, t, a.session, "working", "it took another task itself")
		}); err != nil {
			t.Fatal(err)
		}
		a.stopWith("Looked at it.")
	}
	if len(g.created) != 0 {
		t.Fatalf("opened %d before the sweep", len(g.created))
	}
	a.tick(time.Minute)
	a.s.Wait()
	if len(g.created) != 1 {
		t.Fatalf("opened %d pull requests; step = %+v; rounds = %v", len(g.created), a.step(), a.rounds())
	}
	var n int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ? AND body LIKE '%interrupted%' AND body LIKE '%3%'`, a.admin.User.ID, NotifyAutopilot).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Errorf("no notice says the round was interrupted 3 times; last = %q", a.lastNotice())
	}
}

// Interrupted fewer times: the Stop asks for the round again, and nothing
// is released.
func TestRoundInterruptedOnceIsNotReleased(t *testing.T) {
	a, g, _ := heldEnv(t)
	a.tick(61 * time.Second)
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	a.prompt(in.Review)
	a.prompt("wait")
	a.stopWith("Looked at it.")
	a.tick(time.Minute)
	if len(g.created) != 0 {
		t.Fatalf("opened %d", len(g.created))
	}
}

// roundReason is why the round ended as it did.
func (a *apEnv) roundReason(id int64) string {
	a.t.Helper()
	var s string
	if err := a.s.db.R.QueryRow(`SELECT coalesce(reason, '') FROM work_log WHERE id = ?`, id).Scan(&s); err != nil {
		a.t.Fatal(err)
	}
	return s
}
