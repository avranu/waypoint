package core

import (
	"errors"
	"testing"
	"time"
)

// After Waypoint's own ship ended (the forge refused it, or 24 hours of
// trying ran out), a new wp ship opens and arms the pull request: the end
// leaves no run or claim behind that would hold it off (WP-1370).
func TestShipWorksAgainAfterTheServerGaveUp(t *testing.T) {
	for name, end := range map[string]func(e *loopEnv, round int64){
		"forge refusal": func(e *loopEnv, round int64) {
			e.g.createErr = apiErr(403, "forbidden", nil)
			e.shipAndCheck(round, []string{"rejected"})
			e.tickAll(time.Minute)
			e.s.Wait()
		},
		"24 hour give-up": func(e *loopEnv, round int64) {
			e.g.createErr = errors.New("forge unavailable")
			e.shipAndCheck(round, []string{"rejected"})
			e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'released' WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, e.clockNow().UnixMilli(), round)
			for range 52 {
				e.tickAll(30 * time.Minute)
				e.s.Wait()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r1 := e.review("Merge loses an update")
			end(e, r1)
			bp := roundBeforePR(e.ctx, e.s.db.R, r1)
			if bp == nil || bp.ShipStop == "" {
				t.Fatalf("the round did not end: %+v", bp)
			}
			if n := len(e.g.created); n != 0 {
				t.Fatalf("a pull request opened while the forge failed: %d", n)
			}
			e.g.createErr = nil
			res, err := e.ship(e.task.Key, ShipAuto)
			if err != nil || res.Held == "" {
				t.Fatalf("wp ship after the end: %+v, %v", res, err)
			}
			// It starts a new review round, which a clean audit takes to the pull request.
			e.stopWith("Pushed; the review comes first.")
			r2 := e.step().reviewID.Int64
			if r2 == 0 || r2 == r1 {
				t.Fatalf("no new round: %d after %d", r2, r1)
			}
			e.tick(61 * time.Second)
			e.endAudit(e.box, "done")
			e.tick(time.Second)
			e.s.Wait()
			if n := len(e.g.created); n != 1 {
				t.Fatalf("opened %d pull requests after the second wp ship", n)
			}
			if r := e.round(r2); r.state != RoundDone {
				t.Fatalf("round %d = %+v", r2, r)
			}
		})
	}
}
