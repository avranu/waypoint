package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The helpers' table: nothing without a record; each check's row; the
// verdict line only when the check of findings gave verdicts.
func TestHelperStatsText(t *testing.T) {
	if got := helperStatsText(nil); got != "" {
		t.Errorf("no record = %q", got)
	}
	got := helperStatsText([]helperStat{
		{Check: "goals", Runs: 4, Done: 3, Skipped: 1, Raised: 9, Confirmed: 5, Added: 4, Fixed: 2, Filed: 1, Rejected: 1, CostUSD: 0.125},
		{Check: "verify", Runs: 4, Done: 4, CostUSD: 0.5},
	})
	for _, want := range []string{"Haiku helpers", "CHECK", "goals", "    4     3      0       1      9         5     4     2     1        1", "$0.12", "verify", "CONFIRMED", "ADDED", "FILED", "REJECTED", "2026-11-20"} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "checked against the code") || strings.Contains(got, "asked about again") || strings.Contains(got, "TAKENUP") || strings.Contains(got, "size guard") {
		t.Errorf("a verdict, re-ask or guard line with none:\n%s", got)
	}
	got = helperStatsText([]helperStat{{Check: "verify", Runs: 1, Done: 1, Confirmed: 3, ConfirmedRejected: 1, Refuted: 2, RefutedHeld: 1, Unsupported: 4}})
	if !strings.Contains(got, "3 confirmed (1 of them rejected by the session), 2 refuted (1 of them fixed or confirmed anyway), 4 unsupported") {
		t.Errorf("verdict line:\n%s", got)
	}
	got = helperStatsText([]helperStat{{Check: "verify", Runs: 1, Done: 1, Refuted: 3, RefutedHeld: 1, RefutedFiled: 1}})
	if !strings.Contains(got, "3 refuted (1 of them fixed or confirmed anyway and 1 filed as issues)") {
		t.Errorf("verdict line with a filed refutation:\n%s", got)
	}
	got = helperStatsText([]helperStat{{Check: "quality", Runs: 1, Done: 1, Raised: 250, Stopped: 1}, {Check: "verify", Runs: 1, Done: 1, Refuted: 3, Reasked: 3, Overturned: 1}})
	for _, want := range []string{"quality: 0 runs failed in part, 1 stopped with their job", "Refuted findings asked about again: 3; 1 not refuted the second time (so unsupported)."} {
		if !strings.Contains(got, want) {
			t.Errorf("text lacks %q:\n%s", want, got)
		}
	}
}

// The rounds' table reads the server's JSON: the signal column, and the
// line on the checks of the sessions' marks, summed over indexes, only when
// one was asked.
func TestAgainByRoundText(t *testing.T) {
	read := func(raw string) string {
		var st roundStats
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		return st.nitpickText()
	}
	got := read(`{"nitpicks":{"review_again":{"again":3},"review_again_by_round":[
		{"index":1,"decided":5,"arms":{"again":3},"signalled":2,"followed":3,"useful":2,"checks":4,"passed":1,"unchecked":1,"sampled":5,"asks":21,"split":2,"unsettled":1,"standing":7,"filed":3,"useful_all_filed":1},
		{"index":4,"decided":1,"arms":{},"checks":2,"passed":1,"sampled":1,"asks":3}],
		"review_ship_holds":[{"issue":"WP-1","holds":4,"repeats":0},{"issue":"WP-2","holds":5,"repeats":3,"audits":[12,15]}]}}`)
	for _, want := range []string{"SIGNAL", "1            5     3      2", "3+", "STANDING FILED", "       7     3\n",
		"Of the USEFUL rounds after an again, 1 found only findings the session filed as issues.",
		"Ships held for findings still to decide: 9, of 2 tasks. Held again for the same findings with none decided since, most in a day: WP-2 3 (audits #12, #15).", "Checks of the sessions' marks: 6 asked, 2 passed over (couldn't be asked, failed or too slow), 1 of those sent on",
		"The decision model's asks: 6 decisions scored from 24 answers (4.0 each); in 2 the answers called a question differently, and in 1 the asks' mean still changed its decision with the last asks."} {
		if !strings.Contains(got, want) {
			t.Errorf("text lacks %q:\n%s", want, got)
		}
	}
	got = read(`{"nitpicks":{"review_again":{"enough":1},"review_again_by_round":[{"index":1,"decided":1,"arms":{"enough":1}}]}}`)
	if strings.Contains(got, "Checks of the sessions' marks") || strings.Contains(got, "decision model's asks") || strings.Contains(got, "only findings the session filed") || strings.Contains(got, "Ships held") {
		t.Errorf("a checks or asks line with none:\n%s", got)
	}
}
