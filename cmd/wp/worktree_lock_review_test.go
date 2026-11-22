package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// An old-format lock with a pid and no start time has nothing tying the pid
// to this host or boot, so it is never gone by pid, whether or not a process of
// that number runs here (WP-1370).
func TestLockOwnerNoStartTime(t *testing.T) {
	fakeClock(t)
	old := lockProcLookup
	t.Cleanup(func() { lockProcLookup = old })
	lockProcLookup = func(pid int) (lockProc, bool, error) { return lockProc{}, pid == 10, nil }
	for _, reason := range []string{"claude agent a (pid 12)", "claude session a (pid 10)"} {
		if gone, why := lockOwner(reason, time.Time{}); gone {
			t.Errorf("%s: judged gone with no start and no host: %s", reason, why)
		}
	}
	lockProcLookup = func(int) (lockProc, bool, error) { return lockProc{}, false, errors.New("no /proc") }
	if gone, why := lockOwner("claude agent a (pid 12)", time.Time{}); gone {
		t.Errorf("no view: gone: %s", why)
	}
}

// A lock that names another host is never judged by this host's /proc, and one
// that names this host is judged as any other (WP-1370).
func TestLockOwnerHost(t *testing.T) {
	now := fakeClock(t)
	oldProc, oldHost := lockProcLookup, lockHost
	t.Cleanup(func() { lockProcLookup, lockHost = oldProc, oldHost })
	lockHost = func() string { return "here" }
	lockProcLookup = func(int) (lockProc, bool, error) { return lockProc{}, false, nil } // no such pid
	at := now
	for name, c := range map[string]struct {
		reason string
		gone   bool
	}{
		"another host, pid absent here":      {"claude agent a (pid 12 start 90000 host elsewhere)", false},
		"another host, no start":             {"claude agent a (pid 12 host elsewhere)", false},
		"this host, pid absent":              {"claude agent a (pid 12 start 90000 host here)", true},
		"this host, no start, pid absent":    {"claude agent a (pid 12 host here)", true},
		"no host, no start, pid absent":      {"claude agent a (pid 12)", false},
		"no host, start, pid absent (known)": {"claude agent a (pid 12 start 90000)", true},
	} {
		if gone, why := lockOwner(c.reason, at); gone != c.gone {
			t.Errorf("%s: gone=%v (%s), want %v", name, gone, why, c.gone)
		}
	}
}

// A real child process: not gone while it runs, gone once killed, and gone
// while it is a zombie (exited, not reaped), which the session side agrees
// with (WP-1370).
func TestLockOwnerKilledChildAndZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep to run")
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	pid := cmd.Process.Pid
	start := procStart(pid)
	if start == "" {
		t.Fatal("a running child has no start time")
	}
	reason := fmt.Sprintf("claude agent k (pid %d start %s)", pid, start)
	cs := &cwdSession{Session: "k", Pid: pid, PidStart: start, At: time.Now()}
	if gone, why := lockOwner(reason, time.Now()); gone {
		t.Fatalf("running child judged gone: %s", why)
	}
	if !cs.provablyAlive() || !cs.alive() {
		t.Fatal("a running child is not alive on the session side")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Killed and not yet waited for: a zombie. Wait for the kernel to show it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e, found, _ := readLockProc(pid); found && e.zombie {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e, found, _ := readLockProc(pid); !found || !e.zombie {
		t.Fatalf("the killed child is not a zombie: %+v %v", e, found)
	}
	if gone, why := lockOwner(reason, time.Now()); !gone {
		t.Errorf("lock side: a zombie is not gone: %s", why)
	}
	if cs.provablyAlive() || cs.alive() {
		t.Errorf("session side: a zombie is alive (provablyAlive %v, alive %v)", cs.provablyAlive(), cs.alive())
	}
	_ = cmd.Wait()
	reaped = true
	if gone, why := lockOwner(reason, time.Now()); !gone {
		t.Errorf("lock side: a reaped child is not gone: %s", why)
	}
	if cs.provablyAlive() || cs.alive() {
		t.Errorf("session side: a reaped child is alive")
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

// A host with no /proc of its own has no view: a pid it can't find there is
// "can't tell", never an owner that is gone (WP-1370).
func TestReadLockProcWithoutProcIsCantTell(t *testing.T) {
	oldRoot, oldSig := lockProcRoot, lockPidSignal
	t.Cleanup(func() { lockProcRoot, lockPidSignal = oldRoot, oldSig })
	lockProcRoot = t.TempDir() // no self/stat in it
	lockPidSignal = func(int) bool { return false }
	if _, found, err := readLockProc(4242); err == nil || found {
		t.Fatalf("no /proc: found %v, err %v; want an error", found, err)
	}
}
