package quota

import (
	"math"
	"testing"
	"time"
)

// quotaAt builds a week's readings: hours into the week, percent, session.
func quotaAt(reset time.Time, rs ...any) []Reading {
	start := reset.Add(-Week)
	var out []Reading
	for i := 0; i+2 < len(rs); i += 3 {
		out = append(out, Reading{
			At:    start.Add(time.Duration(rs[i].(float64) * float64(time.Hour))).UnixMilli(),
			Reset: reset.UnixMilli(), Pct: rs[i+1].(float64), Session: rs[i+2].(string),
		})
	}
	return out
}

func TestQuotaLineResets(t *testing.T) {
	reset := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		readings  []any
		pct, used float64
		resets    int
	}{
		{"rising", []any{1.0, 5.0, "a", 2.0, 9.0, "a"}, 9, 9, 0},
		{"a session sees its own reading fall", []any{70.0, 97.0, "a", 72.0, 4.0, "a"}, 4, 101, 1},
		{"a session that was waiting repeats an old reading", []any{
			10.0, 20.0, "b", 70.0, 60.0, "a", 70.1, 20.0, "b", 71.0, 20.0, "b",
		}, 60, 60, 0},
		{"a lower reading among higher ones", []any{
			70.0, 60.0, "a", 70.1, 30.0, "b", 70.2, 61.0, "a", 71.0, 31.0, "c",
		}, 61, 61, 0},
		{"new sessions only, going on lower", []any{70.0, 97.0, "a", 72.0, 3.0, "b", 72.1, 4.0, "c"}, 97, 97, 0},
		{"new sessions only, still lower later", []any{70.0, 97.0, "a", 72.0, 3.0, "b", 72.5, 4.0, "c"}, 4, 101, 1},
		{"no session ids", []any{70.0, 97.0, "", 72.0, 3.0, "", 72.5, 3.0, ""}, 3, 100, 1},
		{"old readings after a reset", []any{
			69.0, 90.0, "b", 70.0, 97.0, "a", 72.0, 4.0, "a", 72.1, 97.0, "a2", 72.2, 90.0, "b", 72.3, 5.0, "a",
		}, 5, 102, 1},
		{"a session from after the reset repeats one use", []any{
			70.0, 97.0, "a", 72.0, 4.0, "b", 72.2, 4.0, "b", 72.5, 4.0, "b",
		}, 4, 101, 1},
		{"the old level repeated once after a reset", []any{
			70.0, 97.0, "a", 72.0, 4.0, "b", 72.2, 97.0, "a", 72.5, 4.0, "b", 73.0, 4.0, "b",
		}, 4, 101, 1},
		{"two resets", []any{20.0, 50.0, "a", 21.0, 2.0, "a", 60.0, 40.0, "a", 61.0, 1.0, "a"}, 1, 91, 2},
	} {
		lines := LinesFrom(quotaAt(reset, tc.readings...), Week)
		if len(lines) != 1 {
			t.Fatalf("%s: %d lines", tc.name, len(lines))
		}
		l := lines[0]
		if l.Last().Pct != tc.pct || l.Used() != tc.used || len(l.Resets()) != tc.resets {
			t.Errorf("%s: at %.0f used %.0f resets %d, want %.0f %.0f %d", tc.name, l.Last().Pct, l.Used(), len(l.Resets()), tc.pct, tc.used, tc.resets)
		}
	}
}

func TestQuotaLinesWindows(t *testing.T) {
	first := time.Date(2026, 11, 21, 17, 0, 0, 0, time.UTC)
	next := first.Add(Week)
	rs := quotaAt(first, 100.0, 80.0, "a")
	rs = append(rs, quotaAt(next, 1.0, 3.0, "a")...)
	// A reading still naming the window that has ended.
	rs = append(rs, Reading{At: first.Add(2 * time.Hour).UnixMilli(), Reset: first.UnixMilli(), Pct: 80, Session: "b"})
	lines := LinesFrom(rs, Week)
	if len(lines) != 2 || lines[1].Last().Pct != 3 || len(lines[1].Resets()) != 0 {
		t.Fatalf("lines %+v", lines)
	}
}

// A reading timed just before its window began still counts.
func TestQuotaReadingBeforeItsWindow(t *testing.T) {
	reset := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	l := LinesFrom(quotaAt(reset, -0.1, 12.0, "a"), Week)[0]
	if l.Last().Pct != 12 || l.Start != l.Last().At {
		t.Errorf("%+v", l)
	}
}

// A session repeating its use from before a reset doesn't make the reading
// since any newer.
func TestQuotaSeenIgnoresRepeats(t *testing.T) {
	reset := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	rs := quotaAt(reset, 70.0, 97.0, "a", 70.5, 97.0, "c", 72.0, 4.0, "a", 80.0, 97.0, "c")
	if l := LinesFrom(rs, Week)[0]; l.Last().Pct != 4 || l.SeenAt != rs[2].At {
		t.Errorf("at %.0f seen %v, want 4 at %v", l.Last().Pct, time.UnixMilli(l.SeenAt), time.UnixMilli(rs[2].At))
	}
}

func TestQuotaRateWeighsRecentUse(t *testing.T) {
	reset := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	start := reset.Add(-Week)
	at := func(h float64) int64 { return start.Add(time.Duration(h * float64(time.Hour))).UnixMilli() }
	perHour := func(lines []*Line, h float64) float64 {
		r, _ := Rate(lines, at(h), 24*time.Hour)
		return r * float64(time.Hour.Milliseconds())
	}
	// 40 points in the first day, then 4 in the second: the second counts twice.
	got := perHour(LinesFrom(quotaAt(reset, 24.0, 40.0, "a", 48.0, 44.0, "a"), Week), 48)
	if want := (40*0.5 + 4.0) / (24*0.5 + 24); math.Abs(got-want) > 0.001 {
		t.Errorf("rate %.3f, want %.3f", got, want)
	}
	// Time at the limit says nothing about the rate.
	got = perHour(LinesFrom(quotaAt(reset, 50.0, 100.0, "a", 100.0, 100.0, "a"), Week), 100)
	if math.Abs(got-2) > 0.001 {
		t.Errorf("rate at the limit %.3f, want 2", got)
	}
}
