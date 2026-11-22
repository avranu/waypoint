package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Audit 1391.1: the clock's watcher, a cancel and the timeout message read
// the waits file from three goroutines.
func TestSlotWaitReaderIsSafeFromSeveralGoroutines(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	var lines []string
	for pid := 1; pid <= 300; pid++ {
		lines = append(lines, fmt.Sprintf("wait %d %d tests", base.UnixMilli(), pid))
	}
	path := filepath.Join(t.TempDir(), "waits")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		r := newSlotWaitReader(path)
		r.alive = func(int) bool { return false } // every read records dead pids
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Every wait ended when its pid was first seen gone,
				// which is some reader's now: 1s to 8s after base.
				if total, since := r.read(base.Add(time.Duration(g+1) * time.Second)); total < time.Second || total > 8*time.Second || !since.IsZero() {
					t.Errorf("read: %v since %v", total, since)
				}
			}()
		}
		wg.Wait()
	}
}

// Audit 1391.3: a wait can't count for more than the job has run, whatever
// its line says: one dated before the job started, or after now.
func TestSlotWaitReaderKeepsWaitsInsideTheJob(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	ms := func(s int) string { return fmt.Sprint(at(s).UnixMilli()) }
	path := filepath.Join(t.TempDir(), "waits")
	write := func(lines ...string) {
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := newSlotWaitReader(path)
	r.alive = func(int) bool { return true }
	r.start = at(50)

	write("wait 0 10 tests") // 1970, still open
	if total, since := r.read(at(100)); total != 50*time.Second || !since.Equal(at(50)) {
		t.Errorf("a wait from before the job: %v since %v, want 50s since the job's start", total, since)
	}
	write("wait "+ms(60)+" 10 tests", "got "+ms(9000)+" 10 tests") // ends after now
	if total, _ := r.read(at(100)); total != 40*time.Second {
		t.Errorf("a wait ending after now: %v, want 40s", total)
	}
	write("wait " + ms(9000) + " 10 tests") // starts after now
	if total, since := r.read(at(100)); total != 0 || !since.IsZero() {
		t.Errorf("a wait starting after now: %v %v", total, since)
	}
	write("wait "+ms(10)+" 10 tests", "got "+ms(40)+" 10 tests") // all before the job
	if total, _ := r.read(at(100)); total != 0 {
		t.Errorf("a wait over before the job: %v", total)
	}
}

// Audits 1391.5 and 1391.8: without a waits file the work limit still holds
// (no wait can be left out of it), and the job's log says so.
func TestJobClockKeepsTheWorkLimitWithoutAWaitsFile(t *testing.T) {
	p := clockProc(t)
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_RUNNER_CACHE", filepath.Join(notADir, "cache"))
	run := &jobRun{id: 9, timeout: time.Minute, out: newLogSink()}
	ctx, cancel, c := p.jobClock(context.Background(), run, 1)
	defer cancel()
	if c.r != nil || c.env() != nil {
		t.Fatalf("a waits file where none can be made: %q", c.path)
	}
	if !ended(ctx, 3*time.Second) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("the work limit didn't end it: %v", ctx.Err())
	}
	if got := c.timedOutAfter(); got != "1s of work" {
		t.Errorf("timedOutAfter = %q", got)
	}
	if log := string(run.out.Tail()); !strings.Contains(log, "count as work") {
		t.Errorf("the log doesn't say waits count as work: %q", log)
	}
}

// Audit 1391.11: the line about a job's waits says they didn't count against
// its work only when it had a work limit.
func TestJobClockWaitedLine(t *testing.T) {
	for _, tc := range []struct {
		work     int
		say, not string
	}{
		{3600, "Waited 2m0s for a test slot; that time didn't count against its 1h0m0s of work.", ""},
		{0, "Waited 2m0s for a test slot, inside its 2h0m0s limit.", "didn't count"},
	} {
		p := clockProc(t)
		clockEvery = time.Hour // the watcher stays out of it
		run := &jobRun{id: 10, timeout: 2 * time.Hour, out: newLogSink()}
		_, cancel, c := p.jobClock(context.Background(), run, tc.work)
		began := time.Now().Add(-10 * time.Minute)
		c.r.mu.Lock()
		c.r.start = began
		c.r.mu.Unlock()
		line := fmt.Sprintf("wait %d %d tests\ngot %d %d tests\n", began.Add(time.Minute).UnixMilli(), os.Getpid(), began.Add(3*time.Minute).UnixMilli(), os.Getpid())
		if err := os.WriteFile(c.path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		cancel()
		log := string(run.out.Tail())
		if !strings.Contains(log, tc.say) || tc.not != "" && strings.Contains(log, tc.not) {
			t.Errorf("work %d: log %q, want %q", tc.work, log, tc.say)
		}
	}
}

// Audit 1391.1 (second part): once cancel returns the watcher has stopped,
// so it can't mark the job as waiting again after cancel cleared that.
func TestJobClockCancelStopsTheWatcherFirst(t *testing.T) {
	p := clockProc(t)
	clockEvery = time.Millisecond
	for i := range 40 {
		aj := &activeJob{lane: laneHeavy}
		run := &jobRun{id: int64(100 + i), timeout: time.Minute, out: newLogSink(), job: aj}
		_, cancel, c := p.jobClock(context.Background(), run, 30)
		appendWait(t, c, "wait")
		for range 200 {
			p.mu.Lock()
			set := !aj.slotWaitSince.IsZero()
			p.mu.Unlock()
			if set {
				break
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		time.Sleep(3 * time.Millisecond)
		p.mu.Lock()
		since := aj.slotWaitSince
		p.mu.Unlock()
		if !since.IsZero() {
			t.Fatalf("run %d: still marked waiting after cancel", i)
		}
	}
}

// Audit 1391.31: the job's log says when it has waited long enough for the
// runner to take another heavy job.
func TestJobClockLogsWhenItGivesUpItsPlace(t *testing.T) {
	p := clockProc(t)
	run := &jobRun{id: 11, timeout: time.Minute, out: newLogSink(), job: &activeJob{lane: laneHeavy}}
	_, cancel, c := p.jobClock(context.Background(), run, 30)
	defer cancel()
	c.r.mu.Lock()
	c.r.start = time.Now().Add(-10 * time.Minute)
	c.r.mu.Unlock()
	line := fmt.Sprintf("wait %d %d tests\n", time.Now().Add(-2*time.Minute).UnixMilli(), os.Getpid())
	if err := os.WriteFile(c.path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(string(run.out.Tail()), "the runner may take another heavy job") {
		if time.Now().After(deadline) {
			t.Fatalf("no line about the place it gave up: %q", run.out.Tail())
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-p.freed:
	case <-time.After(time.Second):
		t.Error("the runner wasn't woken")
	}
}

// Audit 1391.6: every wait for a slot is noted, not only wp slot run's.
func TestAcquireSlotNotesItsWait(t *testing.T) {
	slotEnv(t)
	waits := filepath.Join(t.TempDir(), "waits")
	if err := os.WriteFile(waits, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(slotWaitsEnv, waits)
	hold, err := acquireSlot(t.Context(), "autopush", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	hold.release()
	b, _ := os.ReadFile(waits)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "wait ") || !strings.HasSuffix(lines[0], " autopush") || !strings.HasPrefix(lines[1], "got ") {
		t.Fatalf("waits file: %q", b)
	}
}

// Audit 1391.14: a wait that can't be noted is said, once, where the agent
// sees it.
func TestNoteSlotWaitSaysWhenItCannotWrite(t *testing.T) {
	var warn strings.Builder
	old := slotWaitWarn
	slotWaitWarn = &warn
	t.Cleanup(func() { slotWaitWarn = old })
	t.Setenv(slotWaitsEnv, filepath.Join(t.TempDir(), "gone", "waits"))
	done := noteSlotWait("tests")
	done()
	if n := strings.Count(warn.String(), "counts against the job's time"); n != 1 {
		t.Errorf("warned %d times: %q", n, warn.String())
	}
}

// Audit 1413.3: a wait whose line can't be written (the file opens, the
// write fails) is said too.
func TestNoteSlotWaitSaysWhenTheWriteFails(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("no /dev/full")
	}
	var warn strings.Builder
	old := slotWaitWarn
	slotWaitWarn = &warn
	t.Cleanup(func() { slotWaitWarn = old })
	t.Setenv(slotWaitsEnv, "/dev/full")
	noteSlotWait("tests")()
	if n := strings.Count(warn.String(), "counts against the job's time"); n != 1 {
		t.Errorf("warned %d times: %q", n, warn.String())
	}
}

// Audit 1413.7: a pid seen dead for one wait says nothing about a later
// wait by a process that got the same pid.
func TestSlotWaitReaderDeadPidReused(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	ms := func(s int) string { return fmt.Sprint(at(s).UnixMilli()) }
	path := filepath.Join(t.TempDir(), "waits")
	write := func(lines ...string) {
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := newSlotWaitReader(path)
	alive := false
	r.alive = func(int) bool { return alive }
	write("wait " + ms(0) + " 10 tests")
	if total, _ := r.read(at(10)); total != 10*time.Second {
		t.Fatalf("the dead wait: %v, want 10s", total)
	}
	// The same pid waits again, later, and is alive.
	alive = true
	write("wait "+ms(0)+" 10 tests", "wait "+ms(30)+" 10 tests")
	if total, since := r.read(at(50)); total != 20*time.Second || !since.Equal(at(30)) {
		t.Errorf("a live wait by a reused pid: %v since %v, want 20s since +30s", total, since)
	}
	// And one that died too: it ended when it was first seen gone, not when
	// the earlier holder of the pid was.
	alive = false
	write("wait "+ms(0)+" 10 tests", "wait "+ms(60)+" 10 tests")
	if total, _ := r.read(at(70)); total != 10*time.Second {
		t.Errorf("a dead wait by a reused pid: %v, want 10s", total)
	}
}
