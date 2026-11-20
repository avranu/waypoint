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
		{Check: "goals", Runs: 4, Done: 3, Skipped: 1, Leads: 9, Dropped: 2, TakenUp: 3, Held: 2, CostUSD: 0.125},
		{Check: "verify", Runs: 4, Done: 4, CostUSD: 0.5},
	})
	for _, want := range []string{"Haiku helpers", "CHECK", "goals", "    4     3      0       1      9       2       3     2", "$0.12", "verify", "TAKENUP"} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "checked against the code") || strings.Contains(got, "asked about again") || strings.Contains(got, "size guard, unread") {
		t.Errorf("a verdict, re-ask or guard line with none:\n%s", got)
	}
	got = helperStatsText([]helperStat{{Check: "verify", Runs: 1, Done: 1, Confirmed: 3, ConfirmedRejected: 1, Refuted: 2, RefutedHeld: 1, Unsupported: 4}})
	if !strings.Contains(got, "3 confirmed (1 of them rejected by the session), 2 refuted (1 of them fixed or confirmed anyway), 4 unsupported") {
		t.Errorf("verdict line:\n%s", got)
	}
	got = helperStatsText([]helperStat{{Check: "quality", Runs: 1, Done: 1, Leads: 250, Dropped: 7, Guarded: 5}, {Check: "verify", Runs: 1, Done: 1, Refuted: 3, Reasked: 3, Overturned: 1}})
	for _, want := range []string{"quality: 5 leads cut past the prompt's size guard, unread.", "Refuted findings asked about again: 3; 1 not refuted the second time (so unsupported)."} {
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
		{"index":1,"decided":5,"arms":{"again":3},"signalled":2,"followed":3,"useful":2,"checks":4,"passed":1,"unchecked":1,"sampled":5,"asks":21,"split":2,"unsettled":1},
		{"index":4,"decided":1,"arms":{},"checks":2,"passed":1,"sampled":1,"asks":3}]}}`)
	for _, want := range []string{"SIGNAL", "1            5     3      2", "3+", "Checks of the sessions' marks: 6 asked, 2 passed over (couldn't be asked, failed or too slow), 1 of those sent on",
		"The decision model's asks: 6 decisions scored from 24 answers (4.0 each); in 2 the answers called a question differently, and in 1 the asks' mean still changed its decision with the last asks."} {
		if !strings.Contains(got, want) {
			t.Errorf("text lacks %q:\n%s", want, got)
		}
	}
	got = read(`{"nitpicks":{"review_again":{"enough":1},"review_again_by_round":[{"index":1,"decided":1,"arms":{"enough":1}}]}}`)
	if strings.Contains(got, "Checks of the sessions' marks") || strings.Contains(got, "decision model's asks") {
		t.Errorf("a checks or asks line with none:\n%s", got)
	}
}
