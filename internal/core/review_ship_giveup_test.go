package core

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// A ship that keeps failing after its run ended is given up on 24 hours after
// its first try (not after the run's end), the person is told so truthfully,
// and nothing is left silently stranded (WP-1370).
func TestEndedRoundShipGivesUpFromItsFirstTry(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.g.createErr = errors.New("forge unavailable")
	e.shipAndCheck(r1, []string{"rejected"})
	e.exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'released' WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, e.clockNow().UnixMilli(), r1)
	for range 52 { // 26 hours in half-hour steps
		e.tickAll(30 * time.Minute)
		e.s.Wait()
	}
	bp := roundBeforePR(e.ctx, e.s.db.R, r1)
	if bp == nil || bp.ShipStop == "" {
		t.Fatalf("a ship that failed for over 24 hours was never given up: %+v", bp)
	}
	if n := e.notified("gave up"); n != 1 {
		t.Fatalf("the person was told %d times of the give-up", n)
	}
}

func TestShipEndsSaysWhatHappened(t *testing.T) {
	t.Run("one 503 and the window closing is not a give-up after 24 hours", func(t *testing.T) {
		e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
		r1 := e.review("Merge loses an update")
		e.g.createErr = errors.New("forge unavailable")
		e.shipAndCheck(r1, []string{"rejected"})
		if !e.s.shipEnds(e.ctx, r1, errors.New("forge unavailable"), true) {
			t.Fatal("shipEnds didn't end the round")
		}
		if n := e.notified("gave up"); n != 0 {
			t.Fatalf("a round whose window closed was said to have been given up on after 24 hours (%d)", n)
		}
		if n := e.notified("stopped trying"); n != 1 {
			t.Fatalf("told %d times", n)
		}
	})
	t.Run("a refusal recorded on the round stays a refusal", func(t *testing.T) {
		e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
		r1 := e.review("Merge loses an update")
		e.g.createErr = errors.New("forge unavailable")
		e.shipAndCheck(r1, []string{"rejected"})
		e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.ship_refused', json('true')) WHERE id = ?`, r1)
		if !e.s.shipEnds(e.ctx, r1, errors.New("not found"), true) {
			t.Fatal("shipEnds didn't end the round")
		}
		var body string
		_ = e.s.db.R.QueryRow(`SELECT body FROM notifications WHERE body LIKE '%refused%' LIMIT 1`).Scan(&body)
		if !strings.Contains(body, "refused") {
			t.Fatal("not said as a refusal")
		}
	})
}

// Two callers of the same round's ship (the sweep and a settled check) tell
// the person once and open one pull request (WP-1370).
func TestShipRoundTwoCallersShipOnce(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.shipAndCheck(r1, []string{"rejected"})
	before, told := len(e.g.created), e.noticeCount("%is in review%")
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.s.shipRound(e.ctx, shipRoundIn{round: r1})
		}()
	}
	wg.Wait()
	if n := len(e.g.created); n != before {
		t.Fatalf("opened %d more", n-before)
	}
	if n := e.noticeCount("%is in review%"); n != told {
		t.Fatalf("told %d more times", n-told)
	}
}
