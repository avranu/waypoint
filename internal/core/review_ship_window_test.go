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
	// The run ended 30 hours ago, the round was last touched then, and the
	// first try was an hour ago; the next is due now.
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
