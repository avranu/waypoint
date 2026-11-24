package main

import (
	"errors"
	"time"
)

// pollSleep waits between retries of a long poll (replaced in tests).
var pollSleep = time.Sleep

// errPollBound: the deadline is too close to hold another request.
var errPollBound = errors.New("long poll: out of time")

// pollBoundError is errPollBound when the last request failed: cause is why,
// for a caller with nothing better to say.
type pollBoundError struct{ cause error }

func (e *pollBoundError) Error() string        { return errPollBound.Error() + ": " + e.cause.Error() }
func (e *pollBoundError) Is(target error) bool { return target == errPollBound }
func (e *pollBoundError) Unwrap() error        { return e.cause }

// longPoll is the loop behind wp pr wait and wp ship's wait: ask the server to
// hold a request until something happens, up to deadline. The first request
// holds nothing (probe), so it answers where things stand now; the rest hold
// min(chunk, time left). A transient failure is retried after waitRetry's
// backoff, up to len(waitRetry) times in a row, and once a held request has
// failed the chunk drops to waitShortChunk (a proxy may have cut it short).
// A failed probe is asked again while any time is left, after the backoff
// or what is left of the time, whichever is shorter. A failed held request
// needs room for the whole backoff and another hold of minHold. A failure
// without that room ends the poll at once, without sleeping up to the deadline
// first.
//
// ask makes one request and says whether the poll is over (true, or an error
// it handled itself). longPoll returns nil then, the error that was not
// retried, or errPollBound when time ran out: less than minHold is left (a
// server counts whole seconds: ship's wait wants one, pr wait holds whatever is
// left), or a failed request had no room to be asked again, which
// pollBoundError says with the request's error. That can be the probe's, so a
// caller may get errPollBound with no answer at all.
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
			backoff, left := waitRetry[failures], time.Until(deadline)
			switch {
			case probe && left <= 0, !probe && left-backoff < minHold:
				return &pollBoundError{err}
			case probe:
				backoff = min(backoff, left)
			}
			pollSleep(backoff)
			failures++
			continue
		}
		if done {
			return nil
		}
		failures, probe = 0, false
	}
}
