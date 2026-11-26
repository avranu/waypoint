package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// notified counts the person's autopilot notifications whose text holds s.
func (a *apEnv) notified(s string) int {
	a.t.Helper()
	var n int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ? AND (title LIKE ? OR body LIKE ?)`,
		a.admin.User.ID, NotifyAutopilot, "%"+s+"%", "%"+s+"%").Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

// A round that is due Waypoint's own ship is never given up on: the forge
// down for hours is retried on a backoff, the person is told once, with the
// error, after heldShipRetry, and the pull request opens when it comes back.
func TestShipRetriedPastTheWindowAndToldOnce(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.g.createErr = errors.New("forge unavailable")
	e.shipAndCheck(r1, []string{"rejected"})
	for range 8 {
		e.tickAll(30 * time.Minute)
		e.s.Wait()
		if r := e.round(r1); r.state == RoundFailed || r.state == RoundInterrupted || r.state == RoundSkipped {
			t.Fatalf("the sweep ended a round that only waits on Waypoint's ship: %s (%s)", r.state, e.roundReason(r1))
		}
	}
	if n := e.notified("forge unavailable"); n != 1 {
		t.Fatalf("the person was told %d times of the failure, want once, with the error", n)
	}
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d with the forge down", len(e.g.created))
	}
	e.g.createErr = nil
	e.tickAll(heldShipRetry + time.Minute)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("after the forge came back, opened %d", len(e.g.created))
	}
}

// The failure is retried quickly first: not left a half hour for the first
// second try.
func TestShipRetriedQuicklyAtFirst(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.g.createErr = errors.New("forge unavailable")
	e.shipAndCheck(r1, []string{"rejected"})
	e.g.createErr = nil
	e.tickAll(2 * time.Minute)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("two minutes after the failure, opened %d", len(e.g.created))
	}
}

// A row that can't be read is an error, not a skipped round (WP-1407).
func TestScanIDsFailsOnABadRow(t *testing.T) {
	a := newAPEnv(t)
	rows, err := a.s.db.R.Query(`SELECT 1 UNION ALL SELECT NULL UNION ALL SELECT 3`)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := scanIDs(rows)
	if err == nil {
		t.Fatalf("scanIDs = %v, nil: a row that couldn't be read was dropped", ids)
	}
	if !strings.Contains(err.Error(), "NULL") {
		t.Errorf("err = %v", err)
	}
	rows, err = a.s.db.R.Query(`SELECT 1 UNION ALL SELECT 2`)
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := scanIDs(rows); err != nil || len(ids) != 2 {
		t.Errorf("scanIDs = %v, %v", ids, err)
	}
}

// tickAll is a tick of the autopilot and of the work log's sweeps, the two
// minute ticks of the server.
func (a *apEnv) tickAll(d time.Duration) {
	a.t.Helper()
	a.tick(d)
	a.s.RunWorkLog(a.ctx)
	a.s.Wait()
}

// The retry state is on the round, not in memory: it survives a restart (a
// new run of the sweep reads the same record), so a restart neither retries
// at once nor tells the person a second time (WP-1370).
func TestShipRetryStateIsOnTheRound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.g.createErr = errors.New("forge unavailable")
	e.shipAndCheck(r1, []string{"rejected"})
	bp := roundBeforePR(e.ctx, e.s.db.R, r1)
	if bp == nil || bp.ShipTries == 0 || bp.ShipNext <= e.s.nowMS() || bp.ShipErr == "" {
		t.Fatalf("a failed try left no record on the round: %+v", bp)
	}
	if e.s.shipDue(e.ctx, r1, e.s.nowMS()) {
		t.Fatal("a try that just failed is due at once")
	}
	for range 4 {
		e.tickAll(30 * time.Minute)
		e.s.Wait()
	}
	if n := e.notified("forge unavailable"); n != 1 {
		t.Fatalf("told %d times", n)
	}
	if bp := roundBeforePR(e.ctx, e.s.db.R, r1); bp == nil || !bp.ShipTold {
		t.Fatalf("the notice isn't recorded: %+v", bp)
	}
}
