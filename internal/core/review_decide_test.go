package core

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
												for _, carried := range []bool{false, true} {
													for _, ended := range []bool{false, true} {
														in := roundIn{Carried: carried, SessionEnded: ended, State: state, Read: read, Audit: audit, Undecided: undecided, Check: check, CheckMarks: 1,
															Session: session, RunOpen: runOpen, PROpen: prOpen, Unshipped: unshipped, SinceStart: since, SinceChange: change}
														if state == RoundFailed || state == RoundSkipped || state == RoundInterrupted {
															in.Release = "the review " + state
														}
														out := decideRound(in)
														n++
														name := fmt.Sprintf("%+v -> %+v", in, out)
														over := state == RoundFailed || state == RoundSkipped || state == RoundInterrupted
														waypoints := runOpen && !prOpen // an open run with no pull request: Waypoint's to carry on
														if out.Do == doEnd && (out.To == "" || out.Why == "" || (state != RoundPending && state != RoundRunning && out.Told == "")) {
															t.Fatalf("an end says nothing or ends a round that isn't going: %s", name)
														}
														if out.Do == doEnd && state == RoundRunning && runOpen && check != checkAbsent {
															t.Fatalf("ended a round Waypoint's check is on: %s", name)
														}
														if state == RoundRunning && ended && (!runOpen || (check == checkAbsent && !prOpen)) && out.Do != doEnd {
															t.Fatalf("a running round of an ended session isn't ended: %s", name)
														}
														if state == RoundPending && !carried && (!runOpen || !prOpen) && out.Do != doEnd {
															t.Fatalf("a pending round nobody carries isn't ended: %s", name)
														}
														if waypoints && (out.Do == doNothing || out.Do == doFinish) {
															t.Fatalf("an open run with no pull request is left alone or finished: %s", name)
														}
														if unshipped && !runOpen && (state == RoundPending || state == RoundRunning) && out.Do != doEnd {
															t.Fatalf("a round of a held ship whose run ended stays %s: %s", state, name)
														}
														if !runOpen && (over || (state == RoundDone && in.reviewed())) && unshipped && !prOpen && out.Do != doReleaseAfterRunEnded {
															t.Fatalf("a round the ended run left unshipped isn't named for release: %s", name)
														}
														if out.Do == doReleaseAfterRunEnded && (runOpen || prOpen || !unshipped || (!over && state != RoundDone)) {
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
	// The session's ship of an audited round (shipAudited) reports only a
	// pending round whose finished audit read something and left nothing.
	for _, state := range []string{RoundPending, RoundRunning, RoundDone, RoundSkipped, RoundInterrupted, RoundFailed} {
		for _, audit := range []string{"", AuditQueued, AuditRunning, AuditDone, AuditFailed} {
			for _, undecided := range []int{0, 2} {
				for _, nothing := range []bool{false, true} {
					in := roundIn{ShipAudited: true, State: state, Audit: audit, Undecided: undecided, ReadNothing: nothing, RunOpen: true}
					out := decideRound(in)
					may := state == RoundPending && audit == AuditDone && !nothing && undecided == 0
					if (out.Do == doReport) != may {
						t.Fatalf("%+v: report=%v: %+v", in, out.Do == doReport, out)
					}
					if out.Do != doReport && out.Do != doNothing && out.Do != doTell {
						t.Fatalf("%+v: ship of an audited round answers %d", in, out.Do)
					}
					if out.Do == doNothing && out.Settled == "" {
						t.Fatalf("%+v: a doNothing without its reason", in)
					}
				}
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
		{"no check, no report after the limit: the round fails (the release is of a failed round)",
			roundIn{State: RoundRunning, RunOpen: true, SinceStart: day}, doEnd},
		{"check didn't run: ships with the marks carried",
			roundIn{State: RoundRunning, Check: checkDidnt, RunOpen: true}, doShipCarried},
		{"the pull request is open: finish",
			roundIn{State: RoundRunning, Check: checkAgreed, RunOpen: true, PROpen: true}, doFinish},
		{"a stop with the review's agents still going: waits", roundIn{State: RoundRunning, Stopped: true, Subagents: true, RunOpen: true}, doWait},
		{"a stop while the check is going: waits", roundIn{State: RoundRunning, Stopped: true, Check: checkRunning, RunOpen: true}, doWait},
		{"a stop: its message is the report", roundIn{State: RoundRunning, Stopped: true, RunOpen: true}, doReport},
		{"session idle, no check: the round fails", roundIn{State: RoundRunning, Idle: true, RunOpen: true}, doEnd},
		{"session idle, check going: Waypoint's", roundIn{State: RoundRunning, Idle: true, Check: checkRunning, RunOpen: true}, doWait},
		{"session ended in a running round, no check: interrupted", roundIn{State: RoundRunning, SessionEnded: true, RunOpen: true}, doEnd},
		{"session ended, run ended too, check there: interrupted", roundIn{State: RoundRunning, SessionEnded: true, Check: checkRunning}, doEnd},
		{"pending round nobody carries: skipped", roundIn{State: RoundPending, RunOpen: true}, doEnd},
		{"pending round a step carries: waits", roundIn{State: RoundPending, RunOpen: true, Carried: true}, doWait},
		{"running past the limit, run closed: failed", roundIn{State: RoundRunning, SinceStart: day}, doEnd},
		{"a held ship's round pending when its run ended: skipped", roundIn{State: RoundPending, Unshipped: true, Carried: true}, doEnd},
		{"a held ship's round running when its run ended: interrupted", roundIn{State: RoundRunning, Unshipped: true, Session: true}, doEnd},
		{"superseded, still within the wait: waits", roundIn{State: RoundDone, Superseded: true, RunOpen: true, Unshipped: true, SinceChange: time.Minute}, doWait},
		{"superseded and the wait is over: shipped, not a wait with no time left", roundIn{State: RoundDone, Superseded: true, RunOpen: true, Unshipped: true, SinceChange: heldScreenWait + time.Minute}, doShipCarried},
		{"a ship that has failed for the give-up window, run ended: ended, not left alone", roundIn{State: RoundDone, Unshipped: true, ShipSpent: true, SinceEnd: shipGiveUp + time.Minute}, doEnd},
	}
	for _, c := range cases {
		if out := decideRound(c.in); out.Do != c.do {
			t.Errorf("%s: %+v", c.name, out)
		}
	}
}

// The words of the old, separate decisions don't come back: what a round
// needs is decideRound's alone (WP-1370).
func TestNoSecondDecisionOutsideDecideRound(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	gone := regexp.MustCompile(`\b(reviewedToShip|roundNeeds|needCheck|needTell|needShip|needNone)\b`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := gone.Find(raw); m != nil {
			t.Errorf("%s names %s: ask decideRound instead", f, m)
		}
	}
}

// A round that didn't run its review is released with its own reason, unless
// a person's cancel or a new ask stands (WP-1370).
func TestDecideRoundReleaseOrHold(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   roundIn
		do   int
		why  string
	}{
		{"failed with a reason", roundIn{State: RoundFailed, RunOpen: true, Unshipped: true, Release: "tmux went away"}, doRelease, "tmux went away"},
		{"cancelled by a person", roundIn{State: RoundFailed, RunOpen: true, Unshipped: true}, doHold, ""},
		{"replaced or asked again", roundIn{State: RoundInterrupted, RunOpen: true, Unshipped: true}, doHold, ""},
		{"interrupted for the last time", roundIn{State: RoundInterrupted, RunOpen: true, Unshipped: true, Release: "interrupted 3 times"}, doRelease, "interrupted 3 times"},
	} {
		out := decideRound(tc.in)
		if out.Do != tc.do || (tc.why != "" && out.Why != tc.why) {
			t.Errorf("%s: %+v", tc.name, out)
		}
	}
}

// A round whose run has ended is never left with nothing to say: every
// combination of what is recorded of it either has Waypoint act (end it,
// open its pull request) or says, in Settled, why it is left alone for good.
// Nothing waits or tells without a named thing and a time left (WP-1370,
// WP-1325).
func TestDecideEndedRoundNeverStrands(t *testing.T) {
	bools := []bool{false, true}
	n := 0
	for _, state := range []string{RoundPending, RoundRunning, RoundDone, RoundSkipped, RoundInterrupted, RoundFailed} {
		for _, read := range []string{"", readPending, readDone, readFailed, "rules"} {
			for _, audit := range []string{"", AuditQueued, AuditRunning, AuditDone, AuditFailed} {
				for _, check := range []string{checkAbsent, checkQueued, checkRunning, checkAgreed, checkDisagreed, checkDidnt} {
					for _, since := range []time.Duration{0, 48 * time.Hour} {
						for _, sinceEnd := range []time.Duration{0, strandedShipWindow + time.Hour} {
							for mask := 0; mask < 1<<11; mask++ {
								flag := func(i int) bool { return mask&(1<<i) != 0 }
								in := roundIn{State: state, Read: read, Audit: audit, Check: check, CheckMarks: 1, SinceStart: since, SinceChange: since, SinceEnd: sinceEnd,
									Undecided: 1, RunOpen: false,
									Session: flag(0), Superseded: flag(1), Carried: flag(2), PROpen: flag(3), PRGone: flag(4), Resolved: flag(5), Linked: flag(6),
									Newer: flag(7), Taken: flag(8), Unshipped: flag(9), SessionEnded: flag(10)}
								if state == RoundSkipped || state == RoundFailed || state == RoundInterrupted {
									in.Release = "the review " + state
									if flag(0) && flag(1) { // (a person's decision stands: no release)
										in.Release = ""
									}
								}
								out := decideRound(in)
								n++
								name := fmt.Sprintf("%+v -> %+v", in, out)
								switch out.Do {
								case doNothing:
									if out.Settled == "" {
										t.Fatalf("left alone with no reason: %s", name)
									}
								case doWait, doTell:
									if out.For == "" || out.Until <= 0 {
										t.Fatalf("waits on nothing or with no time left: %s", name)
									}
								case doEnd:
									if out.To == "" || out.Why == "" {
										t.Fatalf("an end that says nothing: %s", name)
									}
								case doReleaseAfterRunEnded:
									if !in.Unshipped || in.PROpen || in.PRGone || in.Linked || in.Resolved || in.Superseded || in.Newer || in.Taken || in.SinceEnd > strandedShipWindow || out.Why == "" {
										t.Fatalf("released a round that is left alone: %s", name)
									}
								}
								// Nothing the exclusions leave a round for may be released, and
								// a round none leaves alone is: opened (done, failed, skipped,
								// interrupted with a reason to release) or ended in its own state.
								free := in.Unshipped && !in.PROpen && !in.PRGone && !in.Linked && !in.Resolved && !in.Superseded && !in.Newer && !in.Taken && in.SinceEnd <= strandedShipWindow
								over := state == RoundSkipped || state == RoundFailed || state == RoundInterrupted
								unreviewed := state == RoundDone && !in.reviewed() // asked for its report, or ended and told (WP-1370), never opened
								if free && out.Do != doEnd && (!unreviewed || (out.Do != doTell && out.Do != doWait)) && (state == RoundDone || (over && in.Release != "")) && out.Do != doReleaseAfterRunEnded {
									t.Fatalf("an unshipped round of an ended run isn't released: %s", name)
								}
								if free && (state == RoundPending || state == RoundRunning) && out.Do != doEnd {
									t.Fatalf("an unshipped round of an ended run keeps going: %s", name)
								}
								_ = bools
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d combinations", n)
}
