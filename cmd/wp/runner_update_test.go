package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// A runner that never goes idle still gets the new wp on disk at once, keeps
// taking jobs for a while, then drains and restarts when idle (WP-1773).
type updRunner struct {
	t   *testing.T
	p   *runnerProc
	out syncBuffer
	exe string
	now time.Time
}

func newUpdRunner(t *testing.T, e *env, serverVersion string) *updRunner {
	t.Helper()
	u := &updRunner{t: t, exe: installedWP(t), now: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)}
	setVersion(t, "v1.1.0")
	c := &cli{dir: e.dir, out: &u.out, err: &u.out}
	u.p = &runnerProc{c: c, opts: runnerOpts{Name: "box", MaxJobs: 3}, jobs: map[int64]*activeJob{}, freed: make(chan struct{}, 1),
		serverVersion: serverVersion, now: func() time.Time { return u.now }}
	return u
}

func (u *updRunner) busy(n int) {
	u.p.mu.Lock()
	defer u.p.mu.Unlock()
	u.p.jobs = map[int64]*activeJob{}
	for i := range n {
		u.p.jobs[int64(i+1)] = &activeJob{id: int64(i + 1), lane: laneHeavy}
	}
}

func (u *updRunner) step() (string, bool) {
	u.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return u.p.updateStep(ctx)
}

func (u *updRunner) disk() string {
	b, _ := os.ReadFile(u.exe)
	return string(b)
}

func TestBusyRunnerUpdatesTheDiskOnceThenDrainsThenRestarts(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.busy(2)

	if exe, drain := u.step(); exe != "" || drain {
		t.Fatalf("first step: restart %q drain %v", exe, drain)
	}
	if !strings.Contains(u.disk(), "wp v1.2.0") {
		t.Fatalf("the binary on disk wasn't replaced: %q", u.disk())
	}
	if !strings.Contains(u.out.String(), "Updated "+u.exe+" to wp v1.2.0; this runner restarts on it when its 2 jobs finish.") {
		t.Errorf("log:\n%s", u.out.String())
	}
	// Not downloaded again: put a marker where the binary is.
	_ = os.WriteFile(u.exe, []byte("marker"), 0o755)
	u.now = u.now.Add(runnerDrainAfter - time.Minute)
	if exe, drain := u.step(); exe != "" || drain {
		t.Fatalf("just before the limit: restart %q drain %v", exe, drain)
	}
	if lanes, free := u.p.lanesNow(); free == 0 || len(lanes) == 0 {
		t.Fatalf("before the limit it must still take jobs: %v %d", lanes, free)
	}
	u.now = u.now.Add(2 * time.Minute)
	for range 2 { // logs once
		if exe, drain := u.step(); exe != "" || !drain {
			t.Fatalf("after the limit: restart %q drain %v", exe, drain)
		}
	}
	if n := strings.Count(u.out.String(), "taking no new jobs until the 2 running finish, then restarting on wp v1.2.0"); n != 1 {
		t.Errorf("drain logged %d times:\n%s", n, u.out.String())
	}
	if lanes, free := u.p.lanesNow(); free != 0 || len(lanes) != 0 {
		t.Fatalf("draining must ask for no jobs (a heartbeat poll): %v %d", lanes, free)
	}
	u.busy(0)
	if exe, drain := u.step(); exe != u.exe || drain {
		t.Fatalf("idle: restart %q drain %v", exe, drain)
	}
	if u.disk() != "marker" {
		t.Errorf("the restart downloaded again: %q", u.disk())
	}
}

func TestIdleRunnerAfterDiskUpdateRestartsWithoutDownloading(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.busy(1)
	u.step()
	_ = os.WriteFile(u.exe, []byte("marker"), 0o755)
	u.busy(0)
	if exe, _ := u.step(); exe != u.exe || u.disk() != "marker" {
		t.Fatalf("restart %q, disk %q", exe, u.disk())
	}
}

func TestIdleRunnerUpdatesThenRestarts(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	if exe, drain := u.step(); exe != u.exe || drain || !strings.Contains(u.disk(), "wp v1.2.0") {
		t.Fatalf("restart %q drain %v disk %q", exe, drain, u.disk())
	}
	if !strings.Contains(u.out.String(), "Updated "+u.exe+" to wp v1.2.0; restarting") {
		t.Errorf("log:\n%s", u.out.String())
	}
}

func TestServerMovesOnAgainAfterTheDiskUpdate(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.p.updatedTo, u.p.updatedExe, u.p.updatedAt = "v1.1.9", u.exe, u.now // an earlier release
	u.now = u.now.Add(runnerDrainAfter + time.Hour)
	u.busy(1)
	if _, drain := u.step(); drain {
		t.Fatal("the old release's wait must not drain for the new one")
	}
	if !strings.Contains(u.disk(), "wp v1.2.0") || u.p.updatedTo != "v1.2.0" {
		t.Fatalf("no fresh update: disk %q updatedTo %q", u.disk(), u.p.updatedTo)
	}
}

func TestFailedUpdateWhileBusyDoesNotDrain(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", true) // bad checksum
	u := newUpdRunner(t, e, "v1.2.0")
	u.busy(1)
	u.step()
	if u.disk() != "old" || !strings.Contains(u.out.String(), "Couldn't update wp") {
		t.Fatalf("disk %q log:\n%s", u.disk(), u.out.String())
	}
	u.now = u.now.Add(runnerDrainAfter + time.Hour)
	if exe, drain := u.step(); exe != "" || drain {
		t.Fatalf("after a failure: restart %q drain %v", exe, drain)
	}
	if n := strings.Count(u.out.String(), "Couldn't update wp"); n != 1 {
		t.Fatalf("retried within 6h (%d tries):\n%s", n, u.out.String())
	}
	if lanes, free := u.p.lanesNow(); free == 0 || len(lanes) == 0 {
		t.Fatal("a runner whose update failed must keep taking jobs")
	}
	u.now = u.now.Add(6 * time.Hour)
	u.step()
	if n := strings.Count(u.out.String(), "Couldn't update wp"); n != 2 {
		t.Fatalf("no retry after 6h (%d tries):\n%s", n, u.out.String())
	}
}

func TestNoUpdateOptionsKeepTheRunnerAsIs(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	for name, opts := range map[string]runnerOpts{"--no-auto-update": {NoAutoUpdate: true}, "--once": {Once: true}} {
		u := newUpdRunner(t, e, "v1.2.0")
		u.p.opts = opts
		u.busy(1)
		u.now = u.now.Add(time.Hour)
		if exe, drain := u.step(); exe != "" || drain || u.disk() != "old" {
			t.Errorf("%s: restart %q drain %v disk %q", name, exe, drain, u.disk())
		}
	}
}
