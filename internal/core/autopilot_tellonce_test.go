package core

import "testing"

// A step held up by one reason, then another, then the first again tells the
// person once per reason, not each time the reason changes back (WP-1370).
func TestTellOnceIsPerReason(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Pushed; the review comes first.")
	for _, reason := range []string{"reason-a", "reason-b", "reason-a", "reason-b", "reason-a"} {
		e.s.tellOnce(e.ctx, e.step(), reason, "Told: "+reason, "body")
	}
	for _, reason := range []string{"reason-a", "reason-b"} {
		if n := e.noticeCount("Told: " + reason); n != 1 {
			t.Errorf("%s told %d times, want once", reason, n)
		}
	}
}
