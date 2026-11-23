package core

import (
	"errors"
	"testing"
	"time"
)

// No ship of Waypoint's stops being retried in silence: a round whose ship
// was ever tried and failed ends through the one claimed write that tells the
// person, however old it is when the sweep gets to it, and whichever of the
// round's windows (24 hours from the first try, or from the run's end) it is
// past (WP-1370).
func TestShipNeverStopsRetryingInSilence(t *testing.T) {
	hour := time.Hour.Milliseconds()
	for name, c := range map[string]struct {
		first, end    int64 // hours ago
		state         string
		sessionEnded  bool
		runOpen       bool
		clearErr      bool
		wantNoticeMin int
	}{
		"both windows close at the same instant":     {first: 24, end: 24, state: "done"},
		"the server was down past the mark":          {first: 30, end: 30, state: "done"},
		"the server came back after 3 days":          {first: 72, end: 72, state: "done"},
		"the first try came after the run ended":     {first: 25, end: 80, state: "done"},
		"the session ended meanwhile":                {first: 26, end: 26, state: "done", sessionEnded: true, runOpen: true},
		"the error was cleared, the first try stays": {first: 25, end: 25, state: "done", clearErr: true},
		"the round never left running":               {first: 26, end: 26, state: "running"},
		"the round failed":                           {first: 26, end: 26, state: "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r1 := e.review("Merge loses an update")
			e.g.createErr = errors.New("forge unavailable")
			e.shipAndCheck(r1, []string{"rejected"})
			now := e.clockNow().UnixMilli()
			if !c.runOpen {
				e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'released' WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, now-c.end*hour, r1)
			}
			if c.sessionEnded || !c.runOpen {
				e.exec(`UPDATE agent_sessions SET ended_at = ? WHERE id = (SELECT session_id FROM work_log WHERE id = ?)`, now-c.end*hour, r1)
			}
			e.exec(`UPDATE work_log SET state = ?, updated_at = ?, before_pr = json_set(before_pr, '$.ship_first', ?, '$.ship_next', ?, '$.ship_tries', 3) WHERE id = ?`,
				c.state, now-c.end*hour, now-c.first*hour, now, r1)
			if c.clearErr {
				e.exec(`UPDATE work_log SET before_pr = json_remove(before_pr, '$.ship_err') WHERE id = ?`, r1)
			}
			for range 3 {
				e.tickAll(time.Minute)
				e.s.Wait()
			}
			bp := roundBeforePR(e.ctx, e.s.db.R, r1)
			if bp == nil || bp.ShipStop == "" {
				t.Fatalf("the ship stopped being retried without ending the round: %+v", bp)
			}
			if n := e.notified("gave up") + e.notified("stopped trying"); n != 1 {
				t.Fatalf("the person was told %d times", n)
			}
			// And wp ship works again.
			e.g.createErr = nil
			if res, err := e.ship(e.task.Key, ShipAuto); err != nil || (res.Held == "" && res.Pull == nil) {
				t.Fatalf("wp ship after the end: %+v, %v", res, err)
			}
		})
	}
}
