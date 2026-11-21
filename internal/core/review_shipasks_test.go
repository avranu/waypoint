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

// The session's plain ship of a pending round whose audit it read asks
// decideRound too (WP-1370; shipAudited): its report only for a pending
// round with a finished, read audit nothing is left to decide in; a tell
// while findings are left; nothing otherwise.
func TestShipAuditedDecideRound(t *testing.T) {
	for _, c := range []struct {
		name string
		in   roundIn
		do   int
	}{
		{"done audit, all decided", roundIn{State: RoundPending, Audit: AuditDone}, doReport},
		{"done audit, findings left", roundIn{State: RoundPending, Audit: AuditDone, Undecided: 2}, doTell},
		{"done audit that read nothing", roundIn{State: RoundPending, Audit: AuditDone, ReadNothing: true}, doNothing},
		{"audit queued", roundIn{State: RoundPending, Audit: AuditQueued}, doNothing},
		{"audit running", roundIn{State: RoundPending, Audit: AuditRunning}, doNothing},
		{"audit failed", roundIn{State: RoundPending, Audit: AuditFailed}, doNothing},
		{"no audit", roundIn{State: RoundPending}, doNothing},
		{"round under way", roundIn{State: RoundRunning, Audit: AuditDone}, doNothing},
		{"round done already", roundIn{State: RoundDone, Audit: AuditDone}, doNothing},
		{"round skipped", roundIn{State: RoundSkipped, Audit: AuditDone}, doNothing},
	} {
		c.in.ShipAudited = true
		out := decideRound(c.in)
		if out.Do != c.do {
			t.Errorf("%s: %+v, want %d", c.name, out, c.do)
		}
		if out.Do == doNothing && out.Settled == "" {
			t.Errorf("%s: a doNothing without its reason", c.name)
		}
		if out.Do == doTell && out.Until <= 0 {
			t.Errorf("%s: a tell with no time", c.name)
		}
	}
}
