package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Round 7 of PR 582's review (audit 1847, c6997).

// 1847.1: the server rolled back to this process's release while a newer
// one is on disk, and the put-back failed: the runner tries again after
// putBackRetryEvery, not 6 hours, so its jobs don't run the newer wp that long.
func TestFailedPutBackIsRetriedSoon(t *testing.T) {
	e := cliServer(t, "v1.1.0", "v1.1.0", false)
	u := newUpdRunner(t, e, "v1.1.0")
	u.noted("v1.2.0", u.now)
	u.p.downloadFn = failingDownload
	u.busy(1)
	u.step()
	if !strings.Contains(u.out.String(), "trying again in 15 minutes") {
		t.Fatalf("log:\n%s", u.out.String())
	}
	if u.p.updateDue() {
		t.Fatal("due again at once after a failed put-back")
	}
	u.now = u.now.Add(putBackRetryEvery)
	if !u.p.updateDue() {
		t.Fatal("not due after putBackRetryEvery")
	}
	var tried string
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		tried = want
		return u.p.c.selfUpdate(ctx, want)
	}
	u.step()
	if tried != "v1.1.0" || u.p.updatedTo != "" {
		t.Fatalf("tried %q updatedTo %q\n%s", tried, u.p.updatedTo, u.out.String())
	}
}

// ... while a failed download of a newer release keeps the 6 hours.
func TestFailedNewerDownloadKeepsTheLongBackoff(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.p.downloadFn = failingDownload
	u.busy(1)
	u.step()
	u.now = u.now.Add(putBackRetryEvery)
	if u.p.updateDue() || !strings.Contains(u.out.String(), "trying again in 6 hours") {
		t.Fatalf("due %v\n%s", u.p.updateDue(), u.out.String())
	}
}

// c6997: a state lock held by another wp stalls the runner's poll loop no
// longer than claimWait.
func TestRunnerClaimWaitsBrieflyForTheStateLock(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	unlock, err := lockFileWithin(filepath.Join(u.p.c.dir, "state.lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	if u.p.beginUpdate("v1.2.0") {
		t.Fatal("claimed under another wp's lock")
	}
	if d := time.Since(start); d > 2*claimWait {
		t.Fatalf("waited %v for the lock", d)
	}
	if !u.p.heldBack("v1.2.0") {
		t.Fatal("not held back after missing the lock")
	}
}

// 1847.3: a "started" claim whose upgrader died holds a command off for
// updateClaimFresh, not the 6-hour backoff.
func TestDeadClaimHoldsCommandsOffOnlyWhileFresh(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")
	c, _ := newCLI(io.Discard, io.Discard)
	c.recordUpdate("v1.2.0", "started", true)
	b.finish("v1.2.0", someCmd)
	if b.count() != 0 {
		t.Fatalf("started beside a live claim: %v", b.started)
	}
	b.ageRecord(updateClaimFresh + 5*time.Minute)
	b.finish("v1.2.0", someCmd)
	if b.count() != 1 {
		t.Fatalf("a dead claim held the upgrade off: %v", b.started)
	}
}
