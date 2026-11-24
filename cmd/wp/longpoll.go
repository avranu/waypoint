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
// The backoff never sleeps past the deadline, and a probe is not sent again
// once the deadline has no hold left.
//
// ask makes one request and says whether the poll is over (true, or an error
// it handled itself). longPoll returns nil then, errPollBound when less than
// minHold is left (a server counts whole seconds: ship's wait wants one, pr wait
// holds whatever is left), or the error that was not retried.
func longPoll(chunk, minHold time.Duration, deadline time.Time, ask func(hold time.Duration, probe bool) (done bool, err error)) error {
	failures := 0
	probe := true
	for {
		left := time.Until(deadline)
		hold := min(chunk, left)
		switch {
		case probe && failures > 0 && (left <= 0 || left < minHold):
			return errPollBound
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
			pollSleep(min(waitRetry[failures], max(time.Until(deadline), 0)))
			failures++
			continue
		}
		if done {
			return nil
		}
		failures, probe = 0, false
	}
}
