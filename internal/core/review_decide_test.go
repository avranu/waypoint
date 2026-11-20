package core

import (
	"fmt"
	"testing"
	"time"
)

// Every combination of a round's recorded facts has an answer, and none
// strands: an open run whose pull request isn't open always has something
// that ends it, and nothing waits or tells for longer than its limit
// (WP-1370).
func TestDecideRoundNeverStrands(t *testing.T) {
	ages := []time.Duration{0, checkPatience + time.Minute, heldScreenWait + time.Minute, 48 * time.Hour}
	n := 0
	for _, state := range []string{RoundPending, RoundRunning, RoundDone, RoundSkipped, RoundInterrupted, RoundFailed} {
		for _, read := range []string{readPending, readDone, readFailed} {
			for _, audit := range []string{"", AuditQueued, AuditRunning, AuditDone, AuditFailed} {
				for _, undecided := range []int{0, 2} {
					for _, check := range []string{checkAbsent, checkQueued, checkRunning, checkAgreed, checkDisagreed, checkDidnt} {
						for _, session := range []bool{false, true} {
							for _, runOpen := range []bool{false, true} {
								for _, prOpen := range []bool{false, true} {
									for _, since := range ages {
										for _, change := range ages {
											for _, unshipped := range []bool{false, true} {
												in := roundIn{State: state, Read: read, Audit: audit, Undecided: undecided, Check: check, CheckMarks: 1,
													Session: session, RunOpen: runOpen, PROpen: prOpen, Unshipped: unshipped, SinceStart: since, SinceChange: change}
												out := decideRound(in)
												n++
												name := fmt.Sprintf("%+v -> %+v", in, out)
												ended := state == RoundFailed || state == RoundSkipped || state == RoundInterrupted
												if !runOpen && ended && unshipped && !prOpen && out.Do != doReleaseAfterRunEnded {
													t.Fatalf("a round the ended run left unshipped isn't named for release: %s", name)
												}
												if out.Do == doReleaseAfterRunEnded && (runOpen || prOpen || !unshipped || !ended) {
													t.Fatalf("released after the run ended when it hadn't, or shipped: %s", name)
												}
												if runOpen && out.Do == doNothing {
													t.Fatalf("an open run is left alone: %s", name)
												}
												if runOpen && prOpen && out.Do != doFinish {
													t.Fatalf("an open pull request with an open run isn't finished: %s", name)
												}
												if runOpen && !prOpen && out.Do == doFinish {
													t.Fatalf("finished with no pull request: %s", name)
												}
												if out.Do == doTell && !session {
													t.Fatalf("told a session that isn't there: %s", name)
												}
												if (out.Do == doTell || out.Do == doWait) && out.Until <= 0 {
													t.Fatalf("a wait or a tell with no time left that doesn't end: %s", name)
												}
												if (out.Do == doRelease || out.Do == doShipCarried) && out.Why == "" {
													t.Fatalf("a release says nothing: %s", name)
												}
												if check == checkDisagreed && (out.Do == doShip) {
													t.Fatalf("shipped clean around a disagreeing check: %s", name)
												}
												if (check == checkQueued || check == checkRunning) && (out.Do == doShip || out.Do == doRelease) && since < 48*time.Hour {
													t.Fatalf("ended a round whose check is going: %s", name)
												}
												if state == RoundDone && undecided > 0 && audit == AuditDone && read == readDone && (check == checkAbsent || check == checkAgreed) && out.Do == doShip {
													t.Fatalf("shipped clean with findings undecided: %s", name)
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	// Past every limit, with nobody to tell, a round that could still wait
	// or tell has to end.
	for _, state := range []string{RoundPending, RoundRunning, RoundDone} {
		for _, check := range []string{checkAbsent, checkQueued, checkRunning, checkAgreed, checkDisagreed, checkDidnt} {
			in := roundIn{State: state, Read: readDone, Audit: AuditDone, Undecided: 1, Check: check, RunOpen: true,
				SinceStart: 48 * time.Hour, SinceChange: 48 * time.Hour}
			if out := decideRound(in); out.Do == doWait || out.Do == doTell {
				t.Errorf("%+v still waits past every limit: %+v", in, out)
			}
		}
	}
	t.Logf("%d combinations", n)
}

// The cells the review found, by name.
func TestDecideRoundCells(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name string
		in   roundIn
		do   int
	}{
		{"done round, check disagreed, session gone: released, not shipped clean",
			roundIn{State: RoundDone, Read: readDone, Audit: AuditDone, Check: checkDisagreed, CheckMarks: 1, RunOpen: true}, doRelease},
		{"done round, check disagreed, session live and in time: the session has it",
			roundIn{State: RoundDone, Read: readDone, Audit: AuditDone, Check: checkDisagreed, CheckMarks: 1, Session: true, RunOpen: true}, doTell},
		{"running round, check agreed after the session ended: ships",
			roundIn{State: RoundRunning, Check: checkAgreed, RunOpen: true}, doShip},
		{"read gave up: released",
			roundIn{State: RoundDone, Read: readFailed, Audit: AuditDone, RunOpen: true}, doRelease},
		{"a check going for hours is not a report that never came",
			roundIn{State: RoundRunning, Check: checkRunning, RunOpen: true, SinceStart: 5 * time.Hour, SinceChange: time.Minute}, doWait},
		{"no check, no report after the limit: released",
			roundIn{State: RoundRunning, RunOpen: true, SinceStart: day}, doRelease},
		{"check didn't run: ships with the marks carried",
			roundIn{State: RoundRunning, Check: checkDidnt, RunOpen: true}, doShipCarried},
		{"the pull request is open: finish",
			roundIn{State: RoundRunning, Check: checkAgreed, RunOpen: true, PROpen: true}, doFinish},
	}
	for _, c := range cases {
		if out := decideRound(c.in); out.Do != c.do {
			t.Errorf("%s: %+v", c.name, out)
		}
	}
}
