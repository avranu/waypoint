package core

import (
	"testing"
	"time"
)

// A round running when its runner goes away is still bounded: nothing in
// what Waypoint does for it needs a runner (WP-1370).
func TestRunningRoundWithNoRunnerIsBounded(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.exec(`DELETE FROM runners`)
	e.exec(`UPDATE agent_sessions SET last_seen_at = last_seen_at - 3600000 WHERE id = (SELECT session_id FROM work_log WHERE id = ?)`, r1)
	for range 10 {
		e.tickAll(30 * time.Minute)
	}
	e.s.Wait()
	if st := e.round(r1).state; st == RoundRunning {
		t.Fatalf("round still running after 5 hours with no runner; step %+v", e.step())
	}
}
