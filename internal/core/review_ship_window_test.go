package core

import (
	"errors"
	"testing"
	"time"
)

// A ship whose first try came late in the window of a run that ended (the
// window is 24 hours from the end) keeps being tried after that window, until
// its own 24 hours from the first try are up: neither the sweep's prefilter
// nor decideRound's SinceEnd counts it from the end (WP-1370).
func TestShipSweepKeepsAFailingShipPastTheStrandedWindow(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.g.createErr = errors.New("forge unavailable")
	e.shipAndCheck(r1, []string{"rejected"})
	now := e.clockNow().UnixMilli()
	day := (24 * time.Hour).Milliseconds()
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'released' WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, now-day-6*time.Hour.Milliseconds(), r1)
	e.exec(`UPDATE agent_sessions SET ended_at = ? WHERE id = (SELECT session_id FROM work_log WHERE id = ?)`, now-day-6*time.Hour.Milliseconds(), r1)
	e.exec(`UPDATE work_log SET state = 'done', updated_at = ?, before_pr = json_set(before_pr, '$.ship_first', ?, '$.ship_next', ?, '$.ship_tries', 1) WHERE id = ?`,
		now-day-6*time.Hour.Milliseconds(), now-time.Hour.Milliseconds(), now, r1)
	before := roundBeforePR(e.ctx, e.s.db.R, r1).ShipTries
	e.tickAll(time.Minute)
	e.s.Wait()
	bp := roundBeforePR(e.ctx, e.s.db.R, r1)
	if bp == nil || bp.ShipTries <= before {
		t.Fatalf("the sweep dropped a failing ship past the stranded window: %+v (tries before %d)", bp, before)
	}
}

// The autopilot step's ship of a round (shipOwed) keeps the sweep's backoff:
// a ship that just failed isn't tried again before its next try is due, and
// is when it is (WP-1370).
func TestShipOwedKeepsTheBackoff(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.g.createErr = errors.New("forge unavailable")
	e.shipAndCheck(r1, []string{"rejected"})
	tries := func() int { return roundBeforePR(e.ctx, e.s.db.R, r1).ShipTries }
	n := tries()
	if n == 0 {
		t.Fatal("setup: no try recorded")
	}
	if _, err := e.s.shipOwed(e.ctx, r1); err != nil || tries() != n {
		t.Fatalf("shipOwed tried again inside the backoff: tries %d -> %d, %v", n, tries(), err)
	}
	e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.ship_next', 1) WHERE id = ?`, r1)
	_, _ = e.s.shipOwed(e.ctx, r1)
	if tries() <= n {
		t.Fatalf("shipOwed did not try when due: tries %d", tries())
	}
}
