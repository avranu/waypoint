package core

import "testing"

// The collector section of the server status is optional: when it can't be
// read the rest still comes back (WP-999).
func TestServerStatusSurvivesUnreadableCollectors(t *testing.T) {
	c := newCollectorEnv(t)
	if _, err := c.s.db.W.ExecContext(c.ctx, `DROP TABLE collector_health`); err != nil {
		t.Fatal(err)
	}
	st, err := c.s.ServerStatus(c.ctx, c.admin)
	if err != nil {
		t.Fatalf("status failed for want of the collectors: %v", err)
	}
	if len(st.Collectors) != 0 {
		t.Errorf("collectors = %+v", st.Collectors)
	}
}
