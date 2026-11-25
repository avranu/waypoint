package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGuardReportText(t *testing.T) {
	raw := `{"who":"pat","days":14,"checks":[{"from":"2026-11-09T11:00:00Z","to":"2026-11-10T11:00:00Z","resets_at":"2026-11-14T00:36:00Z",
		"week_percent":61,"week_even":44.3,"points":11,"unexplained":0,"automation":7,"person":4,"by_phase":{"over":7,"under":0},
		"cost_usd":5.5,"automation_usd":5.5,"limits":{"week_ahead":15,"week_max":90,"week_end_hours":12,"spend_hours":12,"over_max":3},
		"sources":[{"kind":"session","id":4,"title":"WP-1 · session auto-1","points":6,"over":6,"cost_usd":4}],
		"hours":[{"hour":"2026-11-09T13:00:00Z","phase":"over","percent":50,"even":31.2,"points":3,"automation":3,"cost_usd":2,"automation_usd":2},
		{"hour":"2026-11-09T14:00:00Z","phase":"over","points":0,"automation":0,"cost_usd":0,"automation_usd":0}],
		"breach":true,"reasons":["automation used 7.0 points of the week in 3 hours while your autopilot limits held it back (3 or more is a breach)"]}]}`
	var r guardReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	out := guardReportText(&r, true, "chiron")
	for _, want := range []string{
		"Quota guard (pat), last 14 days: 1 check, 1 breach.",
		"Nov 9 11:00 to Nov 10 11:00 UTC: week at 61% (even 44%), resets Nov 14 00:36 UTC",
		"points 11.0: automation 7.0, person 4.0, nobody's 0.0",
		"automation by phase: over 7.0\n",
		"BREACH: automation used 7.0 points",
		"session 4 WP-1 · session auto-1: 6.0 points (6.0 over), $4.00",
		"Nov 9 13:00  over   50%   31%   3.0",
		"week_ahead +15, week_max 90%, week_end_hours 12h; use what's left in the last 12h; a breach from 3 points.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Nov 9 14:00") {
		t.Errorf("an hour that spent nothing is listed:\n%s", out)
	}
}
