package core

import (
	"math"
	"strings"
	"testing"
	"time"
)

// meterAfter lays a week's readings (offset from the env's window start,
// percent) and reads the autopilot meter at the env's clock.
func meterAfter(t *testing.T, g *guardEnv, rows ...float64) *AutopilotMeter {
	t.Helper()
	for i := 0; i+1 < len(rows); i += 2 {
		g.reading(time.Duration(rows[i]*float64(time.Hour)), rows[i+1])
	}
	m, err := g.s.autopilotMeter(g.ctx, g.me.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// After an allowance reset the gate paces from the reset: 40% a day after
// the reset is ahead of that pace, though not ahead of one from the week's
// start. The meter reads the latest reading, not a maximum.
func TestAutopilotMeterAfterAllowanceReset(t *testing.T) {
	g := newGuardEnv(t)
	m := meterAfter(t, g, 0, 60, 1, 61, 1.2, 0, 2, 1, 3, 2, 25, 40)
	if m.WeekReset == nil || !m.WeekReset.Equal(g.from.Add(72*time.Minute)) || m.WeekResetWas == nil || *m.WeekResetWas != 61 {
		t.Fatalf("reset %v was %v", m.WeekReset, m.WeekResetWas)
	}
	if m.WeekPercent == nil || *m.WeekPercent != 40 {
		t.Fatalf("week percent %v, the latest reading is 40", m.WeekPercent)
	}
	from := g.from.Add(72 * time.Minute)
	at := g.from.Add(25 * time.Hour)
	want := math.Round(1000*float64(at.Sub(from))/float64(time.UnixMilli(g.reset).Sub(from))) / 10
	if m.WeekEven == nil || *m.WeekEven != want {
		t.Fatalf("even %v, want %v from the reset", m.WeekEven, want)
	}
	lim := AutopilotLimits{WeekAhead: ptrOf(15.0)}
	why, _ := quotaPaused(m, lim, g.s.now())
	if pauseReason(why) != PauseWeek || !strings.Contains(why, "since the allowance reset at Nov 9 12:12 UTC") {
		t.Errorf("paused %q, want ahead of the from-reset pace and saying since when", why)
	}
	weekStart := quotaEven(0, g.reset, at.UnixMilli())
	if 40-weekStart > 15 {
		t.Fatalf("test setup: the week-start pace %.1f is behind too, so the test shows nothing", weekStart)
	}
}

// Without a reset the meter is the week-start pace, as before.
func TestAutopilotMeterNoReset(t *testing.T) {
	g := newGuardEnv(t)
	m := meterAfter(t, g, 0, 30, 1, 32, 2, 35, 25, 40)
	at := g.from.Add(25 * time.Hour)
	old := 100 * (1 - float64(time.UnixMilli(g.reset).Sub(at))/float64(7*24*time.Hour))
	if m.WeekReset != nil || m.WeekEven == nil || *m.WeekEven != math.Round(old*10)/10 {
		t.Errorf("reset %v even %v, want the week-start pace %.1f", m.WeekReset, m.WeekEven, old)
	}
}

// One low reading among high ones is not a reset: the gate's pace does not move.
func TestAutopilotMeterLoneLowReading(t *testing.T) {
	g := newGuardEnv(t)
	m := meterAfter(t, g, 0, 60, 1, 61, 2, 10, 3, 62, 4, 63, 5, 63, 25, 64)
	if m.WeekReset != nil {
		t.Fatalf("a lone low reading made a reset at %v", m.WeekReset)
	}
	at := g.from.Add(25 * time.Hour)
	old := 100 * (1 - float64(time.UnixMilli(g.reset).Sub(at))/float64(7*24*time.Hour))
	if *m.WeekEven != math.Round(old*10)/10 {
		t.Errorf("even %v, want %.1f", *m.WeekEven, old)
	}
}
