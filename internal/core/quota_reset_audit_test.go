package core

import (
	"fmt"
	"testing"
	"time"
)

// A reset from a modest level is still a reset when use climbs straight
// after it: the readings after it are low when they are nearer where it
// landed than where it fell from, not only when they are 30 points under the
// old level, which a fall of just 30 can never stay (audit 1931.8). The
// boundaries: a fall of 29 is not a reset, a fall of 30 is.
func TestQuotaResetsInShallowDropThenClimb(t *testing.T) {
	wk := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		pcts   []int
		resets int
	}{
		{"a fall of 30, then climbing", []int{40, 10, 11, 12, 13}, 1},
		{"a fall of 31, then climbing", []int{40, 9, 10, 11, 12}, 1},
		{"a fall of 29", []int{40, 11, 12, 13, 14}, 0},
		{"a fall of exactly 30, staying", []int{40, 10, 10, 10}, 1},
		{"a deep fall, then a fast climb", []int{80, 5, 20, 45, 50}, 1},
		{"one low reading among high ones", []int{60, 10, 60, 61, 62}, 0},
		{"one low reading, then half way back", []int{60, 30, 50, 52, 55}, 0},
		{"a small fall to the floor is jitter", []int{20, 3, 4, 5}, 0},
	} {
		var rows []any
		for i, p := range c.pcts {
			rows = append(rows, fmt.Sprintf("10:%02d:00", i*5), p)
		}
		_, rs, _ := quotaResetsIn(samplesAt(t, "2026-11-24", wk, rows...))
		if len(rs) != c.resets {
			t.Errorf("%s %v: %d resets, want %d", c.name, c.pcts, len(rs), c.resets)
		}
	}
}

// A stored check is given only the resets inside its own window: one earlier
// in the same week is not its (audit 1931.24).
func TestQuotaGuardStoredCheckResetsAreItsWindows(t *testing.T) {
	g := newGuardEnv(t)
	g.reading(-20*time.Hour, 80)
	g.reading(-18*time.Hour, 10) // a reset before the window
	g.reading(-17*time.Hour, 12)
	g.reading(-16*time.Hour, 14)
	g.reading(-12*time.Hour, 40)
	g.reading(-8*time.Hour, 60)
	g.reading(-5*time.Hour, 80)
	g.reading(-2*time.Hour, 90)
	g.reading(3*time.Hour, 99)
	g.reading(4*time.Hour, 0) // the reset inside it
	g.reading(6*time.Hour, 3)
	g.reading(7*time.Hour, 5)
	g.exec(`INSERT INTO quota_guard_checks(user_id, at, from_at, to_at, breach, report) VALUES (?,?,?,?,?,?)`,
		g.me.User.ID, g.h(25*time.Hour), g.h(0), g.h(24*time.Hour), 0,
		fmt.Sprintf(`{"at":%q,"from":%q,"to":%q,"points":9,"unexplained":9,"automation":0,"person":0,"by_phase":{},"cost_usd":0,"automation_usd":0}`,
			g.from.Add(25*time.Hour).Format(time.RFC3339), g.from.Format(time.RFC3339), g.from.Add(24*time.Hour).Format(time.RFC3339)))
	rep, err := g.s.QuotaGuard(g.ctx, g.me, UsageQuery{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) != 1 {
		t.Fatalf("%d checks", len(rep.Checks))
	}
	rs := rep.Checks[0].Resets
	if len(rs) != 1 || !rs[0].At.Equal(g.from.Add(4*time.Hour)) || rs[0].WasPercent != 99 {
		t.Errorf("resets %+v: want only the one inside the window, at +4h from 99", rs)
	}
}
