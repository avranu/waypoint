package main

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// pollAsk is one request longPoll made.
type pollAsk struct {
	hold  time.Duration
	probe bool
}

// runPoll runs longPoll against asks that fail with a 503 where fail says so
// (the fourth ends it), with the backoff recorded instead of slept.
func runPoll(t *testing.T, chunk, minHold, bound time.Duration, retry []time.Duration, fail func(n int) bool) ([]pollAsk, []time.Duration, error) {
	t.Helper()
	oldSleep, oldRetry := pollSleep, waitRetry
	var slept []time.Duration
	pollSleep = func(d time.Duration) { slept = append(slept, d) }
	waitRetry = retry
	t.Cleanup(func() { pollSleep, waitRetry = oldSleep, oldRetry })
	var asks []pollAsk
	err := longPoll(chunk, minHold, time.Now().Add(bound), func(hold time.Duration, probe bool, _ time.Duration) (bool, error) {
		asks = append(asks, pollAsk{hold, probe})
		if fail(len(asks)) {
			return false, &apiError{status: http.StatusServiceUnavailable}
		}
		return len(asks) >= 4, nil
	})
	return asks, slept, err
}

// A probe that failed with no time left ends the poll at once, without a
// sleep: out of time, with the request's error for a caller that has no answer
// to fall back on (wp pr wait has no verdict to print).
func TestLongPollProbeFailureAtTheBound(t *testing.T) {
	asks, slept, err := runPoll(t, time.Second, time.Second, 0, []time.Duration{5 * time.Second}, func(int) bool { return true })
	var ae *apiError
	if !errors.Is(err, errPollBound) || !errors.As(err, &ae) || len(asks) != 1 || len(slept) != 0 {
		t.Errorf("err %v after %d requests, slept %v", err, len(asks), slept)
	}
}

// A probe that failed with less time left than the backoff is asked again
// when that time is up, instead of being given up with time unused.
func TestLongPollRetriesTheProbeWithLessThanTheBackoffLeft(t *testing.T) {
	asks, slept, err := runPoll(t, time.Second, time.Second, 3*time.Second, []time.Duration{5 * time.Second, 5 * time.Second}, func(n int) bool { return n == 1 })
	if len(asks) < 2 || !asks[1].probe || len(slept) != 1 || slept[0] > 3*time.Second || slept[0] < 2*time.Second {
		t.Errorf("err %v, asks %+v, slept %v", err, asks, slept)
	}
}

// A probe holds nothing, so minHold doesn't apply to its retry: with less
// than minHold left after the backoff, it is still sent again.
func TestLongPollRetriesTheProbeWithLessThanMinHoldLeft(t *testing.T) {
	asks, slept, err := runPoll(t, time.Second, time.Second, 500*time.Millisecond, []time.Duration{time.Millisecond}, func(n int) bool { return n == 1 })
	if !errors.Is(err, errPollBound) || len(asks) != 2 || !asks[1].probe || asks[1].hold != 0 || len(slept) != 1 || slept[0] != time.Millisecond {
		t.Errorf("err %v, asks %+v, slept %v", err, asks, slept)
	}
}

// A held request that failed with no room for the backoff and another hold
// ends at the bound without sleeping; the caller keeps its last answer, and
// can read why the last request failed.
func TestLongPollHeldFailureAtTheBound(t *testing.T) {
	asks, slept, err := runPoll(t, time.Second, time.Second, 1500*time.Millisecond, []time.Duration{time.Second}, func(n int) bool { return n == 2 })
	var ae *apiError
	if !errors.Is(err, errPollBound) || !errors.As(err, &ae) || len(asks) != 2 || asks[1].probe || asks[1].hold != time.Second || len(slept) != 0 {
		t.Errorf("err %v, asks %+v, slept %v", err, asks, slept)
	}
}

// With room left, a held request that failed is asked again after the
// backoff, holding no longer than waitShortChunk.
func TestLongPollRetriesAHeldRequest(t *testing.T) {
	asks, slept, err := runPoll(t, 2*waitShortChunk, time.Second, time.Hour, []time.Duration{time.Second}, func(n int) bool { return n == 2 })
	if err != nil || len(asks) != 4 || asks[1].hold != 2*waitShortChunk || asks[2].probe || asks[2].hold != waitShortChunk || len(slept) != 1 || slept[0] != time.Second {
		t.Errorf("err %v, asks %+v, slept %v", err, asks, slept)
	}
}

// A request that fails with an error asking again can't fix ends the poll with
// that error, not errPollBound, and without a sleep.
func TestLongPollNonTransientErrorEnds(t *testing.T) {
	oldSleep := pollSleep
	var slept int
	pollSleep = func(time.Duration) { slept++ }
	t.Cleanup(func() { pollSleep = oldSleep })
	bad := &apiError{status: http.StatusBadRequest}
	n := 0
	err := longPoll(time.Second, time.Second, time.Now().Add(time.Hour), func(time.Duration, bool, time.Duration) (bool, error) {
		n++
		return false, bad
	})
	if err != error(bad) || n != 1 || slept != 0 {
		t.Errorf("err %v after %d requests, slept %d", err, n, slept)
	}
}

// After len(waitRetry) failures in a row the poll gives up with the error even
// with time left.
func TestLongPollGivesUpAfterTheRetriesAreUsed(t *testing.T) {
	asks, slept, err := runPoll(t, time.Second, time.Second, time.Hour, []time.Duration{time.Millisecond, time.Millisecond}, func(int) bool { return true })
	var ae *apiError
	if errors.Is(err, errPollBound) || !errors.As(err, &ae) || len(asks) != 3 || len(slept) != 2 {
		t.Errorf("err %v after %d requests, slept %v", err, len(asks), slept)
	}
}

// Only the first request may take pollSlack past its hold. A probe asked again
// after a failure, and every held request, ends by pollGrace after the
// deadline, so a request that stalls can't keep the caller 30s past its bound.
func TestLongPollLaterRequestsEndNearTheDeadline(t *testing.T) {
	oldSleep, oldRetry := pollSleep, waitRetry
	pollSleep, waitRetry = func(time.Duration) {}, []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pollSleep, waitRetry = oldSleep, oldRetry })
	for name, tc := range map[string]struct {
		bound time.Duration
		fail  func(n int) bool
	}{
		"a retried probe":   {300 * time.Millisecond, func(n int) bool { return n == 1 }},
		"held requests":     {3 * time.Second, func(int) bool { return false }},
		"a retried request": {3 * time.Second, func(n int) bool { return n == 2 }},
	} {
		deadline := time.Now().Add(tc.bound)
		var limits []time.Duration
		var whens []time.Time
		_ = longPoll(time.Second, time.Nanosecond, deadline, func(hold time.Duration, _ bool, limit time.Duration) (bool, error) {
			limits, whens = append(limits, limit), append(whens, time.Now())
			if tc.fail(len(limits)) {
				return false, &apiError{status: http.StatusServiceUnavailable}
			}
			return len(limits) >= 3, nil
		})
		if len(limits) < 2 || limits[0] != pollSlack {
			t.Errorf("%s: limits %v", name, limits)
			continue
		}
		for i := 1; i < len(limits); i++ {
			if end := whens[i].Add(limits[i]); end.After(deadline.Add(pollGrace + 100*time.Millisecond)) {
				t.Errorf("%s: request %d may run until %v past the deadline", name, i+1, end.Sub(deadline))
			}
		}
	}
}
