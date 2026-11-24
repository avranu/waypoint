package core

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// roundForShipTries is a reviewed round whose server ship hasn't gone, with
// its first failed try an hour old.
func (e *loopEnv) roundForShipTries() (round int64, now int64) {
	e.t.Helper()
	e.reviewedUntilShip()
	e.g.createErr = errors.New("forge unavailable")
	if err := e.s.db.R.QueryRow(`SELECT id FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1`).Scan(&round); err != nil {
		e.t.Fatal(err)
	}
	now = e.s.nowMS()
	e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.ship_first', ?, '$.ship_tries', 0, '$.ship_told', json('false')) WHERE id = ?`, now-time.Hour.Milliseconds(), round)
	return round, now
}

// Concurrent failed tries count exactly and tell the person once (WP-1370).
func TestShipTriedConcurrentCountsAndTellsOnce(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	round, now := e.roundForShipTries()
	e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.ship_tries', 0) WHERE id = ?`, round)
	before := e.noticeCount("Waypoint can't open%")
	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.s.shipTried(e.ctx, round, e.task.Key, now, errors.New("forge unavailable"))
		}()
	}
	wg.Wait()
	bp := roundBeforePR(e.ctx, e.s.db.R, round)
	if bp == nil || bp.ShipTries != n {
		t.Fatalf("tries = %+v, want %d", bp, n)
	}
	if got := e.noticeCount("Waypoint can't open%") - before; got != 1 {
		t.Fatalf("told %d times, want once", got)
	}
}

// A write that fails tells no one that tick (the notice is part of the
// write), and the person is told at most once overall.
func TestShipTriedWriteFailureTellsNoOne(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	round, now := e.roundForShipTries()
	before := e.noticeCount("Waypoint can't open%")
	e.exec(`CREATE TRIGGER no_try BEFORE UPDATE OF before_pr ON work_log BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	e.s.shipTried(e.ctx, round, e.task.Key, now, errors.New("forge unavailable"))
	if got := e.noticeCount("Waypoint can't open%") - before; got != 0 {
		t.Fatalf("told %d times with the write failing", got)
	}
	e.exec(`DROP TRIGGER no_try`)
	e.s.shipTried(e.ctx, round, e.task.Key, now, errors.New("forge unavailable"))
	e.s.shipTried(e.ctx, round, e.task.Key, now, errors.New("forge unavailable"))
	if got := e.noticeCount("Waypoint can't open%") - before; got != 1 {
		t.Fatalf("told %d times overall, want once", got)
	}
}
