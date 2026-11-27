package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func clockProc(t *testing.T) *runnerProc {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	old := clockEvery
	clockEvery = 10 * time.Millisecond
	t.Cleanup(func() { clockEvery = old })
	return &runnerProc{jobs: map[int64]*activeJob{}, freed: make(chan struct{}, 1)}
}

func appendWait(t *testing.T, c *jobClock, what string) {
	t.Helper()
	f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %d %d tests\n", what, time.Now().UnixMilli(), os.Getpid())
}

func ended(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func TestJobClockLeavesSlotWaitsOutOfWork(t *testing.T) {
	p := clockProc(t)
	run := &jobRun{id: 5, timeout: time.Minute, out: newLogSink()}
	ctx, cancel, c := p.jobClock(context.Background(), run, 1)
	defer cancel()
	if c.r == nil || !strings.Contains(strings.Join(c.env(), ""), slotWaitsEnv+"="+c.path) {
		t.Fatalf("no waits file: %q %v", c.path, c.env())
	}
	appendWait(t, c, "wait")
	if ended(ctx, 1500*time.Millisecond) {
		t.Fatalf("ended at the work limit while waiting: %v", ctx.Err())
	}
	appendWait(t, c, "got")
	if !ended(ctx, 3*time.Second) {
		t.Fatal("didn't end once it worked past the limit")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want DeadlineExceeded", ctx.Err())
	}
	child, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	if child.Err() == nil || !errors.Is(child.Err(), context.DeadlineExceeded) {
		t.Fatalf("a child's Err() = %v", child.Err())
	}
	if got := c.timedOutAfter(); !strings.HasPrefix(got, "1s of work (and ") || !strings.HasSuffix(got, "waiting for a test slot)") {
		t.Errorf("timedOutAfter = %q", got)
	}
}

func TestJobClockWallLimitEndsAWaitingJob(t *testing.T) {
	p := clockProc(t)
	run := &jobRun{id: 6, timeout: 1500 * time.Millisecond, out: newLogSink()}
	ctx, cancel, c := p.jobClock(context.Background(), run, 3600)
	defer cancel()
	appendWait(t, c, "wait")
	if !ended(ctx, 3*time.Second) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("wall limit: %v", ctx.Err())
	}
	if got := c.timedOutAfter(); !strings.Contains(got, "of it waiting for a test slot") {
		t.Errorf("timedOutAfter = %q", got)
	}
}

func TestJobClockWithoutWorkLimitIsPlainTimeout(t *testing.T) {
	p := clockProc(t)
	run := &jobRun{id: 7, timeout: 200 * time.Millisecond, out: newLogSink()}
	ctx, cancel, c := p.jobClock(context.Background(), run, 0)
	if ended(ctx, 100*time.Millisecond) {
		t.Fatal("ended early")
	}
	if !ended(ctx, 2*time.Second) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err() = %v", ctx.Err())
	}
	if got := c.timedOutAfter(); got != "0s" {
		t.Errorf("timedOutAfter = %q", got)
	}
	cancel()
	if _, err := os.Stat(c.path); err == nil {
		t.Error("waits file left behind")
	}
	parent, stop := context.WithCancel(context.Background())
	ctx2, cancel2, _ := p.jobClock(parent, &jobRun{id: 8, timeout: time.Minute, out: newLogSink()}, 0)
	defer cancel2()
	stop()
	<-ctx2.Done()
	if !errors.Is(ctx2.Err(), context.Canceled) {
		t.Errorf("Err() = %v", ctx2.Err())
	}
}
