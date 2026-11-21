package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func fakeClock(t *testing.T) time.Time {
	t.Helper()
	now := time.Now()
	old := lockClock
	lockClock = func() (time.Time, time.Duration, error) { return now, 1000 * time.Second, nil }
	t.Cleanup(func() { lockClock = old })
	return now
}

// A start value that cannot be a real start time makes "can't tell", however
// the arithmetic would wrap.
func TestLockOwnerStartOverflow(t *testing.T) {
	now := fakeClock(t)
	old := lockProcLookup
	lockProcLookup = func(int) (lockProc, bool, error) { return lockProc{}, false, nil } // would read as gone
	t.Cleanup(func() { lockProcLookup = old })
	for _, start := range []uint64{144115188075855872, 1152921504606846976, 1844674407370955162, 18446744073709551615} {
		gone, why := lockOwner(fmt.Sprintf("claude agent a (pid 12 start %d)", start), now)
		if gone {
			t.Errorf("start %d: judged gone: %s", start, why)
		}
	}
}

// An old-format lock with a pid and no start time: a pid that does not exist
// is gone; one that exists can't be compared.
func TestLockOwnerNoStartTime(t *testing.T) {
	fakeClock(t)
	old := lockProcLookup
	t.Cleanup(func() { lockProcLookup = old })
	lockProcLookup = func(pid int) (lockProc, bool, error) { return lockProc{}, pid == 10, nil }
	if gone, why := lockOwner("claude agent a (pid 12)", time.Time{}); !gone {
		t.Errorf("absent pid, no start: not gone: %s", why)
	}
	if gone, why := lockOwner("claude session a (pid 10)", time.Time{}); gone {
		t.Errorf("running pid, no start: gone: %s", why)
	}
	lockProcLookup = func(int) (lockProc, bool, error) { return lockProc{}, false, errors.New("no /proc") }
	if gone, why := lockOwner("claude agent a (pid 12)", time.Time{}); gone {
		t.Errorf("no view: gone: %s", why)
	}
}

// A pid /proc does not show but the kernel says exists (hidepid, or a view
// that hides it) is not "no such process".
func TestReadLockProcHidden(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "self", "stat"), []byte("1 (x) S"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldRoot, oldSig := lockProcRoot, lockPidSignal
	lockProcRoot = root
	t.Cleanup(func() { lockProcRoot, lockPidSignal = oldRoot, oldSig })
	lockPidSignal = func(int) bool { return true }
	if _, found, err := readLockProc(4242); found || err == nil {
		t.Errorf("hidden pid: found=%v err=%v, want an error", found, err)
	}
	lockPidSignal = func(int) bool { return false }
	if _, found, err := readLockProc(4242); found || err != nil {
		t.Errorf("absent pid: found=%v err=%v, want none", found, err)
	}
}

// Two reclaimers of one dead lock: exactly one wins, the other is told it was
// taken; and a lock that is no longer the one judged is left alone.
func TestUnlockJudgedOneWinner(t *testing.T) {
	f := newTidyFixture(t)
	ctx := context.Background()
	p := lockedWorktree(t, f, "wp-1", "worktree-wp-1", deadReason(t))
	judged := mustRepo(t, f).find(p).lockReason
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = unlockJudged(ctx, f.repo, p, judged)
		}()
	}
	wg.Wait()
	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, errLockTaken):
			t.Errorf("racer %d: %v", i, err)
		}
	}
	if wins != 1 {
		t.Errorf("%d reclaimers succeeded, want 1", wins)
	}
}

func TestUnlockJudgedChangedLock(t *testing.T) {
	f := newTidyFixture(t)
	p := lockedWorktree(t, f, "wp-1", "worktree-wp-1", deadReason(t))
	// Judged as another dead lock; the one there now is a different dead one.
	if _, err := unlockJudged(context.Background(), f.repo, p, "claude agent other (pid 999999 start 1)"); err == nil || !isLocked(t, f, p) {
		t.Errorf("unlocked a lock that is not the one judged: %v", err)
	}
}
