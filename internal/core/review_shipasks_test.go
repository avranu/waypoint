package core

import "testing"

// The session's own ship asks decideRound what it means for the round it
// reaches (WP-1370): its report when the round is under way, ended, or
// given up after its prompt went in; nothing when the round never started.
func TestShipAsksDecideRound(t *testing.T) {
	for _, c := range []struct {
		state   string
		started bool
		report  bool
	}{
		{RoundRunning, true, true},
		{RoundDone, true, true},
		{RoundFailed, true, true},
		{RoundFailed, false, false},
		{RoundPending, false, false},
		{RoundSkipped, true, false},
		{RoundInterrupted, true, false},
	} {
		out := decideRound(roundIn{State: c.state, Started: c.started, ShipAsks: true})
		if got := out.Do == doReport; got != c.report {
			t.Errorf("%s started=%v: report=%v, want %v (%+v)", c.state, c.started, got, c.report, out)
		}
		if out.Do == doNothing && out.Settled == "" {
			t.Errorf("%s started=%v: a doNothing without its reason", c.state, c.started)
		}
		if roundDecidable(c.state, c.started) != c.report {
			t.Errorf("roundDecidable(%s, %v) disagrees with decideRound", c.state, c.started)
		}
	}
}
