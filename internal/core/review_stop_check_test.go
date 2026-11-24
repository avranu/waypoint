package core

import "testing"

// A Stop while the round's check of the marks is in flight is not the round's
// report: the check's end carries the round on (WP-1370).
func TestRoundAtStopWaitsForItsCheck(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	var sid int64
	_ = e.s.db.R.QueryRow(`SELECT session_id FROM work_log WHERE id = ?`, r1).Scan(&sid)
	x, err := e.s.sessionByID(e.ctx, e.s.db.R, e.admin, sid)
	if err != nil {
		t.Fatal(err)
	}
	var issue int64
	_ = e.s.db.R.QueryRow(`SELECT issue_id FROM work_log WHERE id = ?`, r1).Scan(&issue)
	var ended bool
	err = e.s.write(e.ctx, SystemActor(), func(tx *tx) error {
		var err error
		ended, err = e.s.roundAtStop(e.ctx, tx, x, issue, "Waiting for the check.", e.s.nowMS())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if st := e.round(r1).state; ended || st != RoundRunning {
		t.Fatalf("a Stop with the check in flight ended the round: ended=%v state=%s", ended, st)
	}
}
