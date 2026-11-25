package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fcfsWaiter makes a live waiter of name that is not this process: a locked
// waiter file for pid, that came at (unix ns), for the slots in order. The
// returned func ends the wait.
func fcfsWaiter(t *testing.T, dir, name string, pid int, at int64, order []int) (gone func()) {
	t.Helper()
	p := filepath.Join(dir, fmt.Sprintf("%s.%d.wait", name, pid))
	line := strconv.FormatInt(at, 10) + "\t\t" + slotOrderField(order)
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0o600) // #nosec G304 -- the test's own directory
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := slotTryLock(f); !ok || err != nil {
		t.Fatalf("lock %s: %v %v", p, ok, err)
	}
	var once bool
	gone = func() {
		if !once {
			once = true
			_ = os.Remove(p)
			_ = f.Close()
		}
	}
	t.Cleanup(gone)
	return gone
}

type fcfsGet struct {
	h   *slotHold
	err error
}

func fcfsAcquire(ctx context.Context, name string, order []int) chan fcfsGet {
	ch := make(chan fcfsGet, 1)
	go func() {
		h, err := acquireSlotIn(ctx, name, order, nil, nil)
		ch <- fcfsGet{h, err}
	}()
	return ch
}

func fcfsNotYet(t *testing.T, ch chan fcfsGet, what string) {
	t.Helper()
	select {
	case r := <-ch:
		if r.h != nil {
			r.h.release()
		}
		t.Fatalf("%s got a slot an earlier waiter may take", what)
	case <-time.After(400 * time.Millisecond):
	}
}

func fcfsGot(t *testing.T, ch chan fcfsGet, within time.Duration) *slotHold {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.h
	case <-time.After(within):
		t.Fatal("no slot")
	}
	return nil
}

func fcfsLock(t *testing.T, dir, name string, i int) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%s.%d.lock", name, i)), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := slotTryLock(f); !ok {
		t.Fatalf("slot %d is held", i)
	}
	t.Cleanup(func() { _ = f.Close() })
}

func fcfsDir(t *testing.T) string {
	t.Helper()
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	dir, _ := slotDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A free slot is not taken by a run that came later than a waiter that may
// take it, not even on its first try (the lottery WP-1876 ends).
func TestSlotFCFSLaterArrivalLeavesAFreeSlotToAnEarlierWaiter(t *testing.T) {
	dir := fcfsDir(t)
	gone := fcfsWaiter(t, dir, "tests", 4194001, time.Now().Add(-time.Second).UnixNano(), []int{0, 1})
	ch := fcfsAcquire(context.Background(), "tests", []int{1, 0})
	fcfsNotYet(t, ch, "a later arrival")
	if h := sqHeld(t, "tests", 2); len(h) != 0 {
		t.Errorf("slots %v taken while the earlier waiter had not", h)
	}
	gone()
	fcfsGot(t, ch, 5*time.Second).release()
}

// A long waiter at the head may only use the lower slots, so it doesn't hold
// short ones off the upper slots; a later long waiter is held off the lower.
func TestSlotFCFSLongWaiterAtTheHeadLeavesTheUpperSlotsToShortRuns(t *testing.T) {
	dir := fcfsDir(t)
	fcfsWaiter(t, dir, "tests", 4194001, time.Now().Add(-time.Second).UnixNano(), slotOrder(slotClassLong, 4))
	short := fcfsAcquire(context.Background(), "tests", slotOrder(slotClassShort, 4))
	h := fcfsGot(t, short, 3*time.Second)
	if h.idx != 3 {
		t.Errorf("short run took slot %d, want 3", h.idx)
	}
	h.release()
	long := fcfsAcquire(context.Background(), "tests", slotOrder(slotClassLong, 4))
	fcfsNotYet(t, long, "a later long run")
}

// With the lower slots held, the short run behind a long waiter takes an upper one at once.
func TestSlotFCFSShortRunsPassALongWaiterWhenTheLowerSlotsAreHeld(t *testing.T) {
	dir := fcfsDir(t)
	fcfsLock(t, dir, "tests", 0)
	fcfsLock(t, dir, "tests", 1)
	fcfsWaiter(t, dir, "tests", 4194001, time.Now().Add(-time.Second).UnixNano(), slotOrder(slotClassLong, 4))
	start := time.Now()
	h := fcfsGot(t, fcfsAcquire(context.Background(), "tests", slotOrder(slotClassShort, 4)), 3*time.Second)
	if h.idx != 3 || time.Since(start) > 2*time.Second {
		t.Errorf("slot %d after %v", h.idx, time.Since(start))
	}
	h.release()
}

func TestSlotFCFSEarlierArrivalIsNotHeldByALaterWaiter(t *testing.T) {
	dir := fcfsDir(t)
	fcfsWaiter(t, dir, "tests", 4194001, time.Now().Add(time.Minute).UnixNano(), []int{0, 1})
	fcfsGot(t, fcfsAcquire(context.Background(), "tests", []int{1, 0}), 3*time.Second).release()
}

// A run that was held behind its worktree's long run came before that wait:
// it queues from then, not from when it began to try the slots.
func TestSlotFCFSArrivalComesFromTheContext(t *testing.T) {
	dir := fcfsDir(t)
	fcfsWaiter(t, dir, "tests", 4194001, time.Now().Add(-time.Second).UnixNano(), []int{0})
	ctx := context.WithValue(context.Background(), slotArrivalKeyT{}, time.Now().Add(-time.Minute))
	fcfsGot(t, fcfsAcquire(ctx, "tests", []int{0}), 3*time.Second).release()
}

func TestSlotFCFSDeadWaiterBlocksNobody(t *testing.T) {
	dir := fcfsDir(t)
	stale := filepath.Join(dir, "tests.4194002.wait")
	line := strconv.FormatInt(time.Now().Add(-time.Hour).UnixNano(), 10) + "\t\t0,1"
	if err := os.WriteFile(stale, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	fcfsGot(t, fcfsAcquire(context.Background(), "tests", []int{0, 1}), 3*time.Second).release()
	if time.Since(start) > 2*time.Second {
		t.Errorf("waited %v behind a dead waiter", time.Since(start))
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the dead waiter's file is still there: %v", err)
	}
}

func TestSlotBlockedSlots(t *testing.T) {
	dir := fcfsDir(t)
	now := time.Now().UnixNano()
	s := int64(time.Second)
	fcfsWaiter(t, dir, "tests", 4194001, now-4*s, slotOrder(slotClassLong, 4))  // a, long: 0 1
	fcfsWaiter(t, dir, "tests", 4194002, now-3*s, slotOrder(slotClassShort, 4)) // b, short: all
	fcfsWaiter(t, dir, "tests", 4194003, now-2*s, slotOrder(slotClassLong, 4))  // c, long
	fcfsWaiter(t, dir, "other", 4194004, now-5*s, []int{0, 1, 2, 3})            // another name
	blocked := func(arrival int64) (out []int) {
		b := slotBlockedSlots(dir, "tests", arrival)
		for i := 0; i < 4; i++ {
			if b(i) {
				out = append(out, i)
			}
		}
		return out
	}
	for _, c := range []struct {
		name    string
		arrival int64
		want    []int
	}{
		{"before everyone", now - 10*s, nil},
		{"after the long head only", now - 3500*int64(time.Millisecond), []int{0, 1}},
		{"after the short one", now - 2500*int64(time.Millisecond), []int{0, 1, 2, 3}},
		{"after all", now, []int{0, 1, 2, 3}},
	} {
		if got := blocked(c.arrival); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: blocked %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSlotBlockedSlotsTieBreaksOnPid(t *testing.T) {
	dir := fcfsDir(t)
	at := time.Now().UnixNano()
	fcfsWaiter(t, dir, "tests", os.Getpid()-1, at, []int{0})
	fcfsWaiter(t, dir, "tests", os.Getpid()+1, at, []int{1})
	b := slotBlockedSlots(dir, "tests", at)
	if !b(0) || b(1) {
		t.Errorf("a lower pid at the same time is first, a higher one is not: blocked 0=%v 1=%v", b(0), b(1))
	}
}

// A waiter file from a wp that didn't record its slots blocks none: that wp
// doesn't honour the order either, and a stall behind a long run is worse.
func TestSlotBlockedSlotsLegacyWaiterBlocksNothing(t *testing.T) {
	dir := fcfsDir(t)
	writeWaiter(t, dir, 4194001, time.Now().Add(-time.Second).UnixNano())
	b := slotBlockedSlots(dir, "tests", time.Now().UnixNano())
	if b(0) || b(7) {
		t.Error("a waiter with no slots recorded blocks nothing")
	}
}

// A live waiter whose file was not refreshed within slotWaiterStale blocks
// nothing; a fresh one still does. Times are set on the files, no sleeps.
func TestSlotBlockedSlotsStaleLiveWaiterBlocksNothing(t *testing.T) {
	dir := fcfsDir(t)
	now := time.Now()
	at := now.Add(-time.Minute).UnixNano()
	fcfsWaiter(t, dir, "tests", 4194001, at, []int{0})
	fcfsWaiter(t, dir, "tests", 4194002, at, []int{1})
	touch := func(pid int, mt time.Time) {
		p := filepath.Join(dir, fmt.Sprintf("tests.%d.wait", pid))
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	touch(4194001, now.Add(-slotWaiterStale-time.Second)) // stopped
	touch(4194002, now.Add(-slotWaiterStale/2))           // polling
	b := slotBlockedSlotsAt(dir, "tests", now.UnixNano(), now)
	if b(0) {
		t.Error("a live waiter that stopped polling still blocks")
	}
	if !b(1) {
		t.Error("a waiter that polled recently no longer blocks")
	}
}

// The waiter's own poll refreshes its file.
func TestSlotTouchWaiterRefreshesTheFile(t *testing.T) {
	dir := fcfsDir(t)
	remove := slotAddWaiterFor(dir, "tests", time.Now(), []int{0})
	defer remove()
	p := slotWaiterPath(dir, "tests")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	slotTouchWaiter(dir, "tests")
	st, err := os.Stat(p)
	if err != nil || time.Since(st.ModTime()) > time.Minute {
		t.Errorf("mtime %v err %v, want refreshed", st.ModTime(), err)
	}
}

// A run held behind its own worktree's long run is not a waiter for any
// slot, so it blocks nobody; once its turn comes it queues from its start.
func TestSlotFCFSRunBehindItsOwnLongRunIsNoWaiter(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqRepo(t)
	sqArgs(t)
	sqSeed(t, "tests", sqCmd, 1800, 3)
	c1, _, _ := sqCLI()
	_, rel1, err := c1.takeSlot(context.Background(), "tests", 4)
	if err != nil {
		t.Fatal(err)
	}
	c2, _, _ := sqCLI()
	ch := sqGo(c2, "tests", 4)
	sqNotYet(t, ch, "second run of the same worktree")
	dir, _ := slotDir()
	if files, _ := filepath.Glob(filepath.Join(dir, "tests.*.wait")); len(files) != 0 {
		t.Errorf("a run behind its own worktree's run registered as a waiter: %v", files)
	}
	rel1()
	sqGot(t, ch)()
}
