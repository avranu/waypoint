package core

import (
	"testing"
	"time"
)

// A round held on a usage limit is bounded, whatever the limit, and a person
// is told once about it (WP-1370).
func TestRoundOnAUsagePauseIsBoundedAndTold(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(a *apEnv)
	}{
		{"five hours", func(a *apEnv) {
			a.exec2(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
				a.admin.User.ID, a.clockNow().UnixMilli(), "five_hour", 99.0, a.clockNow().Add(100*time.Hour).UnixMilli(), "box")
		}},
		{"week max", func(a *apEnv) {
			a.exec2(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
				a.admin.User.ID, a.clockNow().UnixMilli(), "seven_day", 99.0, a.clockNow().Add(5*24*time.Hour).UnixMilli(), "box")
		}},
		{"week pace", func(a *apEnv) {
			a.exec2(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
				a.admin.User.ID, a.clockNow().UnixMilli(), "seven_day", 90.0, a.clockNow().Add(6*24*time.Hour).UnixMilli(), "box")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _, _ := heldEnv(t)
			a.stopWith("Waiting for the review.")
			id := a.step().reviewID.Int64
			c.set(a)
			for range 60 {
				a.tickAll(30 * time.Minute)
			}
			a.s.Wait()
			if st := a.round(id).state; st == RoundPending {
				t.Fatalf("a round still pending after 30 hours on a limit; step %+v", a.step())
			}
			var n int
			if err := a.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE kind = ? AND (title LIKE '%usage limit%' OR title LIKE '%limit%')`, NotifyAutopilot).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("told %d times about the limit, want once", n)
			}
		})
	}
}
