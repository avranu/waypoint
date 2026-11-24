package main

import (
	"errors"
	"time"
)

// pollSleep waits between retries of a long poll (replaced in tests).
var pollSleep = time.Sleep

// errPollBound: the deadline is too close to hold another request.
var errPollBound = errors.New("long poll: out of time")

// longPoll is the loop behind wp pr wait and wp ship's wait: ask the server to
// hold a request until something happens, up to deadline. The first request
// holds nothing (probe), so it answers where things stand now; the rest hold
// min(chunk, time left). A transient failure is retried after waitRetry's
// backoff, up to len(waitRetry) times in a row, and once a held request has
// failed the chunk drops to waitShortChunk (a proxy may have cut it short).
// A failure with no room before the deadline for the backoff and another
// request (a probe needs any time at all, a held one minHold) ends the poll at
// once, without sleeping up to the deadline first.
//
// ask makes one request and says whether the poll is over (true, or an error
// it handled itself). longPoll returns nil then, errPollBound when less than
// minHold is left (a server counts whole seconds: ship's wait wants one, pr wait
// holds whatever is left), or the error that was not retried. errPollBound
// only comes after a request was answered: while none has been (the probe
// keeps failing), the caller has no answer to fall back on and gets the error.
func longPoll(chunk, minHold time.Duration, deadline time.Time, ask func(hold time.Duration, probe bool) (done bool, err error)) error {
	failures := 0
	probe := true
	for {
		hold := min(chunk, time.Until(deadline))
		switch {
		case probe:
			hold = 0
		case hold < minHold:
			return errPollBound
		}
		done, err := ask(hold, probe)
		if err != nil {
			if !transientErr(err) || failures >= len(waitRetry) {
				return err
			}
			if hold > 0 {
				chunk = min(chunk, waitShortChunk)
			}
			switch left := time.Until(deadline) - waitRetry[failures]; {
			case probe && left <= 0:
				return err
			case !probe && left < minHold:
				return errPollBound
			}
			pollSleep(waitRetry[failures])
			failures++
			continue
		}
		if done {
			return nil
		}
		failures, probe = 0, false
	}
}
