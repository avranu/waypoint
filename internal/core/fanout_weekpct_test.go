package core

import (
	"math"
	"testing"
	"time"
)

// usdPerWeekPct divides the week's spend by the points the week used: a
// mid-week allowance reset (seven_day falling with resets_at unchanged)
// starts a new segment, so 99 then reset then 80 is 179 points, not 99.
func TestUsdPerWeekPctAcrossAReset(t *testing.T) {
	for _, tc := range []struct {
		name string
		pcts []float64
		want float64 // points used
	}{
		{"no reset", []float64{10, 30, 50, 65, 80}, 80},
		{"reset", []float64{99, 0, 10, 25, 40, 60, 80}, 179},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := newFanoutEnv(t)
			uid := fe.admin.User.ID
			now := time.Now().UnixMilli()
			reset := now + 2*day
			for i, p := range tc.pcts {
				at := now - int64(len(tc.pcts)-i)*3600_000
				if _, err := fe.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
					uid, at, "seven_day", p, reset, "box"); err != nil {
					t.Fatal(err)
				}
			}
			hour := now - now%3600_000 - 3600_000
			if _, err := fe.s.db.W.Exec(`INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, output) VALUES (?,?,?,?,?,?,?)`,
				uid, hour, "box", "s", "", "claude-opus-5-5", 5_000_000); err != nil {
				t.Fatal(err)
			}
			book, _, err := fe.s.loadPrices(fe.ctx)
			if err != nil {
				t.Fatal(err)
			}
			byFam, err := fe.s.usageCost(fe.ctx, book, `user_id = ? AND hour >= ?`, uid, reset-7*day)
			if err != nil {
				t.Fatal(err)
			}
			var spend float64
			for _, v := range byFam {
				spend += v[0]
			}
			if spend <= 0 {
				t.Fatal("no priced spend")
			}
			got, ok := fe.s.usdPerWeekPct(fe.ctx, book, uid, now)
			if !ok || math.Abs(got-spend/tc.want) > 1e-9 {
				t.Errorf("usdPerWeekPct = %v, %v; want %v (spend %v / %v points)", got, ok, spend/tc.want, spend, tc.want)
			}
		})
	}
}
