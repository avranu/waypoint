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
	err := longPoll(chunk, minHold, time.Now().Add(bound), func(hold time.Duration, probe bool) (bool, error) {
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
