package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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

func TestShipRoundHeldToTheReviewedHead(t *testing.T) {
	t.Run("pushed between the check and the forge opening it: recorded, not armed, round starts over", func(t *testing.T) {
		e, round := tipRound(t)
		e.g.tips = map[string]string{"wp-1-first": tipAudited}
		e.g.heads = map[string]string{"wp-1-first": tipPushed}
		e.tick(time.Second)
		e.s.Wait()
		if len(e.g.created) != 1 {
			t.Fatalf("opened %d", len(e.g.created))
		}
		var armed int
		_ = e.s.db.R.QueryRow(`SELECT count(*) FROM pull_requests WHERE armed_at IS NOT NULL`).Scan(&armed)
		if armed != 0 {
			t.Fatal("a pull request at a commit the review didn't read was armed")
		}
		if r := e.round(round); r.state != RoundInterrupted || e.roundReason(round) != roundRestarted {
			t.Fatalf("round = %s (%s)", r.state, e.roundReason(round))
		}
	})
	t.Run("nothing says what the review read: nothing opens", func(t *testing.T) {
		e, round := tipRound(t)
		e.g.tips = map[string]string{"wp-1-first": tipAudited}
		ra, _ := e.roundAuditRow(round)
		e.exec(`UPDATE code_audits SET to_sha = '' WHERE id = ?`, ra.id)
		e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.head_sha', '') WHERE id = ?`, round)
		e.tick(time.Second)
		e.s.Wait()
		if len(e.g.created) != 0 {
			t.Fatalf("opened %d with no commit the review read", len(e.g.created))
		}
	})
	t.Run("the pin keeps an arm to the reviewed head", func(t *testing.T) {
		e, _ := tipRound(t)
		e.g.tips = map[string]string{"wp-1-first": tipAudited}
		e.g.heads = map[string]string{"wp-1-first": tipAudited}
		e.tick(time.Second)
		e.s.Wait()
		var id int64
		_ = e.s.db.R.QueryRow(`SELECT id FROM pull_requests ORDER BY id DESC LIMIT 1`).Scan(&id)
		if _, err := e.s.MergePR(e.ctx, e.admin, id, MergeInput{WhenReady: true, Head: tipPushed}); err == nil || !strings.Contains(err.Error(), "not armed") {
			t.Fatalf("an arm pinned to another head = %v", err)
		}
	})
}

// A review session that stops with a background shell running (a waiter on
// CI) has reported: the same Stop reads the same background work whether it
// is the Stop path or the idle one (WP-1370).
func TestReviewStopWithABackgroundShellIsTheReport(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	round := e.review("Merge loses an update")
	e.stopWith("Review: nothing found.", PendingTask{Type: "shell", Status: "running", Description: "wp pr wait 7"})
	if r := e.round(round); r.state != RoundDone {
		t.Fatalf("a background shell held the review's report: %+v", r)
	}
}

// A background agent the review session started and is waiting on holds the
// report: its Stop is not yet the review's end, on the Stop path the review
// round reads as well as the idle one (WP-1370).
func TestReviewStopWithABackgroundAgentIsNotTheReport(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	round := e.review("Merge loses an update")
	e.stopWith("Review: a helper is still reading.", PendingTask{Type: "agent", Status: "running", Description: "read the diff"})
	if r := e.round(round); r.state != RoundRunning {
		t.Fatalf("a background agent still running, and the round is %s", r.state)
	}
}
