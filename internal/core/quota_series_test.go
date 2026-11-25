package core

import (
	"math"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/quota"
)

// quotaAt builds a week's readings: hours into the week, percent, session.
func quotaAt(reset time.Time, rs ...any) []quota.Reading {
	start := reset.Add(-quotaWeek)
	var out []quota.Reading
	for i := 0; i+2 < len(rs); i += 3 {
		out = append(out, quota.Reading{
			At:    start.Add(time.Duration(rs[i].(float64) * float64(time.Hour))).UnixMilli(),
			Reset: reset.UnixMilli(), Pct: rs[i+1].(float64), Session: rs[i+2].(string),
		})
	}
	return out
}

// A window the period only reaches into is given whole, so a reset before the
// period still counts in what the window used.
func TestQuotaHistoryKeepsWholeWindows(t *testing.T) {
	reset := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	rs := quotaAt(reset, 10.0, 60.0, "a", 20.0, 2.0, "a", 100.0, 30.0, "a")
	lines := quota.LinesFrom(rs, quotaWeek)
	// 60, the reset, then 2 and 30.
	if h := quotaHistory(lines, rs[2].At); len(h) != 4 || h[0].Percent != 60 || h[1].Percent != 0 {
		t.Errorf("history %+v", h)
	}
	if h := quotaHistory(lines, reset.UnixMilli()); len(h) != 0 {
		t.Errorf("an ended window %+v", h)
	}
}

// The week the quota was reset three days in: the projection goes on at the
// rate of recent use, from what is used since the reset.
func TestQuotaPaceAfterReset(t *testing.T) {
	reset := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	start := reset.Add(-quotaWeek)
	var rs []any
	// 1.35 points an hour for 72 hours, a reset, then the same for 15.
	for h := 1.0; h <= 72; h++ {
		rs = append(rs, h, 1.35*h, "a")
	}
	for h := 73.0; h <= 87; h++ {
		rs = append(rs, h, 1.35*(h-72), "a")
	}
	lines := quota.LinesFrom(quotaAt(reset, rs...), quotaWeek)
	now := start.Add(87 * time.Hour)
	status, projected, runsOut, even := quotaPace(now, now, lines, 24*time.Hour)
	used := 1.35 * 15
	want := used + 1.35*(168-87)
	if status != "will_run_out" || math.Abs(projected-want) > 0.5 || runsOut == nil {
		t.Fatalf("%s %.1f %v, want will_run_out %.1f", status, projected, runsOut, want)
	}
	if e := 100 * 15.0 / (168 - 72); math.Abs(even-e) > 0.1 {
		t.Errorf("even %.2f, want %.2f", even, e)
	}
	if out := now.Add(time.Duration((100 - used) / 1.35 * float64(time.Hour))); runsOut.Sub(out).Abs() > time.Minute {
		t.Errorf("runs out %v, want %v", runsOut, out)
	}
}

func TestQuotaPaceInANewWindow(t *testing.T) {
	reset := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	start := reset.Add(-quotaWeek)
	at := func(h float64) int64 { return start.Add(time.Duration(h * float64(time.Hour))).UnixMilli() }
	// A window just begun goes by the one before it.
	prev := quotaAt(reset.Add(-quotaWeek), 24.0, 12.0, "a", 168.0, 84.0, "a")
	lines := quota.LinesFrom(append(prev, quotaAt(reset, 1.0, 0.5, "a")...), quotaWeek)
	got := func(lines []*quota.Line) float64 {
		r, _ := quota.Rate(lines, at(1), 24*time.Hour)
		return r * float64(time.Hour.Milliseconds())
	}
	status, _, _, _ := quotaPace(msTime(at(1)), msTime(at(1)), lines, 24*time.Hour)
	if r := got(lines); status == "too_early" || math.Abs(r-0.5) > 0.01 {
		t.Errorf("new window: %s at %.3f", status, r)
	}
}

func TestQuotaEverywhereAfterReset(t *testing.T) {
	e := newAnalyticsEnv(t)
	now := e.clockNow()
	reset := now.Add(4 * 24 * time.Hour).UnixMilli()
	send := func(ago time.Duration, session string, pct float64) {
		e.t.Helper()
		if _, err := e.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine, session) VALUES (?,?,?,?,?,?,?)`,
			e.me.User.ID, now.Add(-ago).UnixMilli(), "seven_day", pct, reset, "box", session); err != nil {
			t.Fatal(err)
		}
	}
	send(30*time.Hour, "b", 70)
	send(20*time.Hour, "a", 97)
	send(19*time.Hour, "a", 3)
	send(time.Hour, "a", 20)
	send(time.Minute, "a", 23)
	send(0, "b", 70) // what b heard before the reset
	m, err := e.s.autopilotMeter(e.ctx, e.me.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Even pace runs from the reset, 20 hours ago, to the end in 96.
	if valueOf(m.WeekPercent) != 23 || math.Abs(valueOf(m.WeekEven)-17.2) > 0.1 {
		t.Errorf("meter %v even %v", valueOf(m.WeekPercent), valueOf(m.WeekEven))
	}
	o := e.overview(e.me, UsageQuery{})
	w := o.Quota.Windows[0]
	if w.Percent != 23 || len(w.Resets) != 1 || !w.PacedFrom.Equal(w.Resets[0]) || w.Status != "will_run_out" {
		t.Errorf("window %+v", w)
	}
	for _, p := range w.History {
		if p.At.Equal(msTime(now.UnixMilli())) && p.Percent == 70 {
			t.Errorf("an old reading in the history: %+v", p)
		}
	}
	l, err := e.s.currentQuota(e.ctx, e.me.User.ID, "seven_day", now)
	if err != nil || l.Used() != 120 {
		t.Errorf("used %v %v", l.Used(), err)
	}
	// The guard reads the week as the gates do, not at its old high.
	weeks, err := e.s.quotaWeekLines(e.ctx, e.me.User.ID, now.Add(-quotaWeek).UnixMilli(), now.UnixMilli()+1)
	if err != nil || len(weeks) != 1 || weeks[0].At(now.UnixMilli()) != 23 {
		t.Errorf("guard %v %v", weeks, err)
	}
}
