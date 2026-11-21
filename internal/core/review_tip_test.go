package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Waypoint opens a reviewed round's pull request only for the commit the
// review read: a push after the audit means the review didn't see what would
// open (WP-1325, WP-1370).

const (
	tipAudited = "aaaaaaa1111111"
	tipPushed  = "bbbbbbb2222222"
)

// tipRound runs the round up to the audit's end and holds the ship, with the
// audit having read tipAudited.
func tipRound(t *testing.T) (*loopEnv, int64) {
	t.Helper()
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Pushed; the review comes first.")
	round := e.step().reviewID.Int64
	e.tick(61 * time.Second)
	e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.head_sha', ?) WHERE id = ?`, tipAudited, round)
	e.endAudit(e.box, "done")
	ra, _ := e.roundAuditRow(round)
	e.exec(`UPDATE code_audits SET to_sha = ? WHERE id = ?`, tipAudited, ra.id)
	return e, round
}

func TestShipRoundOpensOnlyForTheCommitReviewed(t *testing.T) {
	t.Run("pushed after the audit: no pull request, the round starts over", func(t *testing.T) {
		e, round := tipRound(t)
		e.g.tips = map[string]string{"wp-1-first": tipPushed}
		e.tick(time.Second)
		e.s.Wait()
		if len(e.g.created) != 0 {
			t.Fatalf("opened %d for a branch that moved after its review", len(e.g.created))
		}
		r := e.round(round)
		if r.state != RoundInterrupted || e.roundReason(round) != roundRestarted {
			t.Fatalf("round = %s (%s)", r.state, e.roundReason(round))
		}
		e.tick(time.Hour)
		if len(e.g.created) != 0 {
			t.Fatalf("a later tick opened %d", len(e.g.created))
		}
	})
	t.Run("the tip is what the audit read: it opens", func(t *testing.T) {
		e, _ := tipRound(t)
		e.g.tips = map[string]string{"wp-1-first": tipAudited}
		e.tick(time.Second)
		e.s.Wait()
		if len(e.g.created) != 1 {
			t.Fatalf("opened %d", len(e.g.created))
		}
	})
	t.Run("the forge can't say: it doesn't open, and tries again", func(t *testing.T) {
		e, round := tipRound(t)
		e.g.tipErr = errors.New("forge unavailable")
		e.tick(time.Second)
		e.s.Wait()
		if len(e.g.created) != 0 {
			t.Fatalf("opened %d with no word of the tip", len(e.g.created))
		}
		if r := e.round(round); r.state != RoundDone {
			t.Fatalf("round = %s (%s)", r.state, e.roundReason(round))
		}
		if bp := roundBeforePR(e.ctx, e.s.db.R, round); bp == nil || bp.ShipTries != 1 || !strings.Contains(bp.ShipErr, "tip") {
			t.Fatalf("no retry recorded: %+v", bp)
		}
		e.g.tipErr = nil
		e.g.tips = map[string]string{"wp-1-first": tipAudited}
		e.tickAll(shipBackoff(1) + time.Minute)
		e.s.Wait()
		if len(e.g.created) != 1 {
			t.Fatalf("after the forge answered, opened %d", len(e.g.created))
		}
	})
}
