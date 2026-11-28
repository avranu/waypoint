package core

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A wait or a tell the decision returns always has time left, whatever the
// ages are, and at the very edge of each limit too: a wait with no time left
// that nothing acts on would hold a round for ever (WP-1370). The ages are
// every limit decideRound compares with, a nanosecond either side of it and
// on it. The session's own ship (ShipAsks, ShipAudited) and a Stop (Stopped)
// are asked as well as the plain decision, and each doNothing says why.
func TestDecideRoundEveryWaitHasTimeLeft(t *testing.T) {
	limits := []time.Duration{checkPatience, heldScreenWait, readWait, reviewRunMax + autopilotNothingRetry, checkPatience + heldScreenWait, strandedShipWindow, shipGiveUp}
	ages := []time.Duration{0, 48 * time.Hour}
	for _, l := range limits {
		ages = append(ages, l-1, l, l+1)
	}
	asks := []struct {
		name string
		set  func(*roundIn)
	}{
		{"plain", func(*roundIn) {}},
		{"superseded", func(in *roundIn) { in.Superseded = true }},
		{"shipAsks", func(in *roundIn) { in.ShipAsks, in.Started = true, true }},
		{"shipAudited", func(in *roundIn) { in.ShipAudited = true }},
		{"stopped", func(in *roundIn) { in.Stopped = true }},
		{"stoppedWithAgents", func(in *roundIn) { in.Stopped, in.Subagents = true, true }},
	}
	var total atomic.Int64
	var wg sync.WaitGroup
	for _, ask := range asks {
		wg.Go(func() {
			var local int64
			for _, state := range []string{RoundPending, RoundRunning, RoundDone, RoundSkipped, RoundInterrupted, RoundFailed} {
				for _, read := range []string{readPending, readDone, readFailed} {
					for _, audit := range []string{"", AuditQueued, AuditRunning, AuditDone, AuditFailed} {
						for _, check := range []string{checkAbsent, checkQueued, checkRunning, checkAgreed, checkDisagreed, checkDidnt} {
							for _, flags := range []int{0, 1, 2, 3, 4, 5, 6, 7} {
								for _, since := range ages {
									for _, change := range ages {
										in := roundIn{State: state, Read: read, Audit: audit, Undecided: flags & 1, Check: check, CheckMarks: 1,
											Session: flags&2 != 0, RunOpen: flags&4 != 0, SinceStart: since, SinceChange: change}
										if state == RoundFailed || state == RoundSkipped || state == RoundInterrupted {
											in.Release = "the review " + state
										}
										ask.set(&in)
										out := decideRound(in)
										local++
										if (out.Do == doWait || out.Do == doTell) && out.Until <= 0 {
											t.Errorf("%s: a %d with no time left and no end: %+v -> %+v", ask.name, out.Do, in, out)
											return
										}
										if out.Do == doNothing && out.Settled == "" {
											t.Errorf("%s: a doNothing with no reason: %+v", ask.name, in)
											return
										}
									}
								}
							}
						}
					}
				}
			}
			total.Add(local)
		})
	}
	wg.Wait()
	for _, since := range ages {
		for _, spent := range []bool{false, true} {
			in := roundIn{State: RoundDone, Unshipped: true, SinceEnd: since, ShipSpent: spent, Release: "x"}
			if out := decideRound(in); out.Do == doWait || out.Do == doTell {
				t.Fatalf("an ended round waits: %s -> %+v", fmt.Sprintf("%+v", in), out)
			}
		}
	}
	t.Logf("%d combinations", total.Load())
}
