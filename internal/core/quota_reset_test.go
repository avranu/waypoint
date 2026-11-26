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

// A from at or before the week's start, or at or after resets_at, is no
// reset inside the week: the pace is the whole week's, as with no from.
func TestQuotaPaceIgnoresOutOfWeekFrom(t *testing.T) {
	reset := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	start := reset.Add(-quotaWeek)
	now := start.Add(2 * 24 * time.Hour)
	st0, p0, r0, e0 := quotaPace(now, now, reset, time.Time{}, quotaWeek, 50)
	for name, from := range map[string]time.Time{
		"at the start":     start,
		"before the start": start.Add(-time.Hour),
		"at resets_at":     reset,
		"after resets_at":  reset.Add(time.Hour),
	} {
		st, p, r, e := quotaPace(now, now, reset, from, quotaWeek, 50)
		if st != st0 || p != p0 || e != e0 || (r == nil) != (r0 == nil) || r != nil && !r.Equal(*r0) {
			t.Errorf("from %s: %s %.2f %.2f %v, want %s %.2f %.2f %v", name, st, p, e, r, st0, p0, e0, r0)
		}
	}
}

// usageQuota reads the week's last allowance reset and paces from it.
func TestUsageQuotaPacesFromReset(t *testing.T) {
	e := newAnalyticsEnv(t)
	now := e.clockNow()
	reset := now.Add(3 * 24 * time.Hour).UnixMilli()
	for _, r := range []struct {
		ago time.Duration
		pct float64
	}{{3 * 24 * time.Hour, 60}, {2 * 24 * time.Hour, 62}, {24 * time.Hour, 3}, {12 * time.Hour, 4}, {0, 5}} {
		if _, err := e.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
			e.me.User.ID, now.Add(-r.ago).UnixMilli(), "seven_day", r.pct, reset, "box"); err != nil {
			t.Fatal(err)
		}
	}
	o := e.overview(e.me, UsageQuery{})
	if o.Quota == nil || len(o.Quota.Windows) == 0 || o.Quota.Windows[0].Span != "seven_day" {
		t.Fatalf("quota: %+v", o.Quota)
	}
	w := o.Quota.Windows[0]
	// The reset was a day ago and the week ends in three: 25% of the span
	// has gone, at 5% used, so 20% by the end.
	if math.Abs(w.Even-25) > 0.1 || math.Abs(w.Projected-20) > 0.1 || w.Status != "on_track" {
		t.Errorf("even %.2f projected %.2f status %s, want 25, 20, on_track", w.Even, w.Projected, w.Status)
	}
	_, p0, _, e0 := quotaPace(now, now, time.UnixMilli(reset), time.Time{}, quotaWeek, 5)
	if math.Abs(w.Even-e0) < 1 || math.Abs(w.Projected-p0) < 1 {
		t.Errorf("same as with no reset: even %.2f projected %.2f", e0, p0)
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

// Use between the last reading before a reset and the first one after it is
// counted: the line falls to 0 at the reset and rises to the first reading
// over that gap, and the points are placed by what was spent in it.
func TestQuotaGuardUseBeforeFirstReadingAfterReset(t *testing.T) {
	g := newGuardEnv(t)
	g.reading(-time.Hour, 80)
	g.reading(4*time.Hour, 80)
	g.reading(8*time.Hour, 40)
	g.reading(9*time.Hour, 41)
	g.reading(10*time.Hour, 42)
	g.session("mine", 5*time.Hour, "fix the login page")
	g.used("mine", 5*time.Hour)
	c := g.check()
	if len(c.Resets) != 1 {
		t.Fatalf("resets %+v", c.Resets)
	}
	if c.Points != 42 {
		t.Errorf("points %v, want 42 (0 to 40 over the gap, then 1 and 1)", c.Points)
	}
	if c.Person != 40 || c.Unexplained != 2 {
		t.Errorf("person %v unexplained %v, want 40 and 2", c.Person, c.Unexplained)
	}
	if c.WeekPercent == nil || *c.WeekPercent != 42 {
		t.Errorf("week percent %v, want 42", c.WeekPercent)
	}
}

// With one reading after a drop, stays() takes it alone: it must agree.
func TestQuotaResetsInOneReadingAfterDrop(t *testing.T) {
	at := func(i int, p float64) quotaSample { return quotaSample{at: int64(i) * 3600_000, pct: p} }
	_, rs, _ := quotaResetsIn([]quotaSample{at(1, 80), at(2, 10), at(3, 12)})
	if len(rs) != 1 {
		t.Errorf("a drop and one reading that stays low: resets %+v, want 1", rs)
	}
	_, rs, _ = quotaResetsIn([]quotaSample{at(1, 80), at(2, 10), at(3, 79)})
	if len(rs) != 0 {
		t.Errorf("a drop and one reading back up: resets %+v, want none", rs)
	}
}
