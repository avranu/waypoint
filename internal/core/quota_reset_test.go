package core

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// samplesAt reads "15:04:05 pct" pairs of one day into readings of a week.
func samplesAt(t *testing.T, day string, reset time.Time, rows ...any) []quotaSample {
	t.Helper()
	var out []quotaSample
	for i := 0; i+1 < len(rows); i += 2 {
		at, err := time.Parse("2006-01-02T15:04:05", day+"T"+rows[i].(string))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, quotaSample{at: at.UnixMilli(), pct: float64(rows[i+1].(int)), reset: reset.UnixMilli()})
	}
	return out
}

// The real series (WP-1881): two allowance resets with resets_at unchanged,
// one weekly rollover (resets_at moved, so another week), and readings that
// wobble or flick between the old and new level.
func TestQuotaResetsInRealSeries(t *testing.T) {
	wk1 := time.Date(2026, 11, 21, 17, 0, 0, 0, time.UTC)
	wk2 := wk1.Add(quotaWeek)
	a := samplesAt(t, "2026-11-19", wk1, "22:02:01", 97, "22:30:13", 98, "22:59:57", 99, "23:07:15", 0, "23:13:46", 1, "23:33:30", 2, "23:50:00", 3)
	a = append(a, samplesAt(t, "2026-11-20", wk1, "00:06:36", 5, "00:19:20", 7, "01:53:14", 18)...)
	_, rs, _ := quotaResetsIn(a)
	if len(rs) != 1 || rs[0].At != time.Date(2026, 11, 19, 23, 7, 15, 0, time.UTC).UnixMilli() || rs[0].Was != 99 {
		t.Fatalf("Nov 19 resets %+v, want one at 23:07:15 from 99", rs)
	}
	// The rollover is the next week's first reading, never a drop in this one.
	b := samplesAt(t, "2026-11-21", wk2, "17:15:50", 0, "17:26:19", 1)
	b = append(b, samplesAt(t, "2026-11-22", wk2, "22:20:42", 50, "22:22:43", 38, "22:23:46", 50)...)
	b = append(b, samplesAt(t, "2026-11-24", wk2, "09:30:00", 90, "10:11:14", 37, "10:14:42", 90, "10:18:55", 91, "10:46:59", 92, "11:11:35", 91, "11:19:44", 92,
		"13:49:01", 98, "14:20:41", 97, "14:26:32", 99, "16:00:54", 100, "16:02:01", 0, "16:04:21", 99, "16:11:09", 0, "16:23:25", 1, "16:49:14", 2, "16:51:31", 1, "16:55:03", 2, "17:15:06", 3, "17:48:44", 2, "17:49:58", 3, "17:52:11", 4)...)
	// 09:30 90 is a drop from 50 only by a week's worth of silence: that is
	// the real week's path, not an outlier.
	kept, rs, at := quotaResetsIn(b)
	if len(rs) != 1 || rs[0].At != time.Date(2026, 11, 24, 16, 2, 1, 0, time.UTC).UnixMilli() || rs[0].Was != 100 {
		t.Fatalf("Nov 24 resets %+v, want one at 16:02:01 from 100", rs)
	}
	for _, k := range kept {
		switch k.at {
		case time.Date(2026, 11, 24, 10, 11, 14, 0, time.UTC).UnixMilli():
			t.Error("the lone 37 among the 90s is kept")
		case time.Date(2026, 11, 24, 16, 4, 21, 0, time.UTC).UnixMilli():
			t.Error("the lone 99 after the reset is kept")
		}
	}
	if len(at) != 1 || kept[at[0]].pct != 0 {
		t.Errorf("segment starts at %v of %d kept readings", at, len(kept))
	}
}

// Jitter of 2 to 12 points, down and straight back, is not a reset.
func TestQuotaResetsInJitter(t *testing.T) {
	wk := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	var rows []any
	base := 40
	for i, wob := range []int{2, 5, 12, 3, 8, 12} {
		rows = append(rows, fmt.Sprintf("10:%02d:00", i*10), base, fmt.Sprintf("10:%02d:30", i*10), base-wob)
		base += 3
	}
	kept, rs, _ := quotaResetsIn(samplesAt(t, "2026-11-24", wk, rows...))
	if len(rs) != 0 || len(kept) != len(rows)/2 {
		t.Fatalf("resets %+v, kept %d of %d", rs, len(kept), len(rows)/2)
	}
}

// A reading that falls and is not followed by more of the same is an outlier;
// the last reading alone is not enough to call a reset.
func TestQuotaResetsInUnconfirmed(t *testing.T) {
	wk := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	_, rs, _ := quotaResetsIn(samplesAt(t, "2026-11-24", wk, "10:00:00", 80, "10:10:00", 81, "10:20:00", 0))
	if len(rs) != 0 {
		t.Fatalf("a last reading alone made a reset: %+v", rs)
	}
}

// A day with an allowance reset in it: the week's high-water mark restarts,
// the later rises count as points, and the even pace runs from the reset to
// resets_at.
func TestQuotaGuardAllowanceReset(t *testing.T) {
	g := newGuardEnv(t)
	g.reading(-time.Hour, 90)
	g.reading(2*time.Hour, 90)
	g.reading(3*time.Hour, 99)
	g.reading(4*time.Hour, 0)
	g.reading(5*time.Hour, 99) // flicks back to the old level once
	g.reading(6*time.Hour, 3)
	g.reading(10*time.Hour, 8)
	g.reading(13*time.Hour, 9)
	c := g.check()
	if len(c.Resets) != 1 || !c.Resets[0].At.Equal(g.from.Add(4*time.Hour)) || c.Resets[0].WasPercent != 99 {
		t.Fatalf("resets %+v", c.Resets)
	}
	if c.Points != 18 {
		t.Errorf("points %v, want 18 (90 to 99 before the reset, 0 to 3, 3 to 8, 8 to 9 after)", c.Points)
	}
	reset := time.UnixMilli(g.reset)
	from := g.from.Add(4 * time.Hour)
	want := 100 * float64(g.from.Add(24*time.Hour).Sub(from)) / float64(reset.Sub(from))
	if c.WeekEven == nil || math.Abs(*c.WeekEven-want) > 0.1 {
		t.Errorf("even pace %v, want %.1f from the reset", c.WeekEven, want)
	}
	if c.WeekPercent == nil || *c.WeekPercent != 9 {
		t.Errorf("week percent %v, want 9", c.WeekPercent)
	}
}

// quotaPace after a reset paces what remains, not the whole week.
func TestQuotaPaceAfterReset(t *testing.T) {
	reset := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	from := time.Date(2026, 11, 24, 16, 2, 0, 0, time.UTC)
	now := from.Add(24 * time.Hour)
	status, projected, _, even := quotaPace(now, now, reset, from, quotaWeek, 30)
	remaining := reset.Sub(from)
	if wantEven := 100 * float64(24*time.Hour) / float64(remaining); math.Abs(even-wantEven) > 0.01 {
		t.Errorf("even %.2f, want %.2f", even, wantEven)
	}
	if want := 30 * float64(remaining) / float64(24*time.Hour); math.Abs(projected-want) > 0.01 || status != "will_run_out" {
		t.Errorf("status %s projected %.1f, want will_run_out %.1f", status, projected, want)
	}
	_, p0, _, e0 := quotaPace(now, now, reset, time.Time{}, quotaWeek, 30)
	if e0 == even || p0 == projected {
		t.Errorf("no reset gave the same pace")
	}
}

// A check stored before resets were read keeps its figures; on read it gets
// the resets in its window and a note saying so.
func TestQuotaGuardStoredPreResetCheck(t *testing.T) {
	g := newGuardEnv(t)
	g.reading(-time.Hour, 90)
	g.reading(3*time.Hour, 99)
	g.reading(4*time.Hour, 0)
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
	c := rep.Checks[0]
	if c.Points != 9 || len(c.Resets) != 1 || c.ResetNote == "" {
		t.Errorf("points %v resets %+v note %q: figures kept, reset filled, noted", c.Points, c.Resets, c.ResetNote)
	}
}

// Readings under pace early in the week, then a confirmed drop to 0 later:
// the even pace at each earlier hour is the week's, not the reset's, so the
// automation's hours are not "over" and nothing breaches (audit 1907.2).
func TestQuotaGuardEvenPaceBeforeLaterReset(t *testing.T) {
	g := newGuardEnv(t)
	g.reading(-time.Hour, 20)
	g.reading(2*time.Hour, 24)
	g.reading(4*time.Hour, 34)
	g.reading(8*time.Hour, 0)
	g.reading(10*time.Hour, 1)
	g.reading(12*time.Hour, 2)
	g.reading(13*time.Hour, 3)
	g.session("auto-1", 2*time.Hour, "Waypoint autopilot: work on WP-1")
	g.used("auto-1", 2*time.Hour)
	g.used("auto-1", 3*time.Hour)
	c := g.check()
	if len(c.Resets) != 1 {
		t.Fatalf("resets %+v: the drop to 0 is confirmed", c.Resets)
	}
	if c.ByPhase[GuardPhaseOver] != 0 || c.Breach {
		t.Errorf("by phase %v breach %v %v: under pace before the reset", c.ByPhase, c.Breach, c.Reasons)
	}
	if c.Hours[2].Phase != GuardPhaseUnder {
		t.Errorf("hour 2 phase %q, want under", c.Hours[2].Phase)
	}
	if c.Hours[2].Even == nil || math.Abs(*c.Hours[2].Even-31.2) > 0.2 {
		t.Errorf("hour 2 even %v, want the week's pace near 31.2", valueOf(c.Hours[2].Even))
	}
}

// A window wholly before a later reset gets the week's pace too.
func TestQuotaGuardWindowBeforeLaterReset(t *testing.T) {
	g := newGuardEnv(t)
	g.reading(-time.Hour, 20)
	g.reading(2*time.Hour, 24)
	g.reading(13*time.Hour, 34)
	g.reading(26*time.Hour, 0)
	g.reading(27*time.Hour, 1)
	g.reading(28*time.Hour, 2)
	c := g.check()
	reset := time.UnixMilli(g.reset)
	start := reset.Add(-quotaWeek)
	want := 100 * float64(g.from.Add(24*time.Hour).Sub(start)) / float64(quotaWeek)
	if c.WeekEven != nil && math.Abs(*c.WeekEven-want) > 0.1 {
		t.Errorf("window even %v, want %.1f", *c.WeekEven, want)
	}
	for i, h := range c.Hours {
		if h.Phase == GuardPhaseOver {
			t.Fatalf("hour %d over", i)
		}
	}
}
