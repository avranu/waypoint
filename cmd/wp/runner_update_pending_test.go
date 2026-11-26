package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Round 5 of PR 582's review: a pending restart keeps job admission closed
// on every pass, the restart is on the file the runner recorded, and the
// shared record gates every download (c6971, c6972, c6980-c6983).

// c6972: a runner draining for v1.2.0 when the server moves on to v1.3.0
// keeps taking no jobs while it downloads v1.3.0, and afterwards: its
// restart is still pending, and the wait for it doesn't start again.
func TestDrainingRunnerStaysDrainedWhileItDownloadsANewerRelease(t *testing.T) {
	e := cliServer(t, "v1.3.0", "v1.3.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		putWP(t, u.exe, want)
		return want, u.exe, nil
	}
	u.busy(1)
	u.step() // v1.2.0 on disk
	u.now = u.now.Add(runnerDrainAfter)
	if _, drain := u.step(); !drain {
		t.Fatalf("not draining after the wait\n%s", u.out.String())
	}
	u.p.serverVersion = "v1.3.0"
	release := make(chan struct{})
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		<-release
		putWP(t, u.exe, want)
		return want, u.exe, nil
	}
	for i := range 3 { // the pass that starts it, then passes while it runs
		if exe := u.p.updateStep(context.Background()); exe != "" || !u.p.draining {
			t.Fatalf("pass %d while downloading v1.3.0: restart %q drain %v", i, exe, u.p.draining)
		}
		if lanes, free := u.p.lanesNow(); free != 0 || len(lanes) != 0 {
			t.Fatalf("pass %d: took jobs while a restart is pending: %v %d", i, lanes, free)
		}
	}
	close(release)
	u.settle()
	if u.p.updatedTo != "v1.3.0" {
		t.Fatalf("updatedTo %q", u.p.updatedTo)
	}
	if _, drain := u.step(); !drain {
		t.Fatal("the drain started over for v1.3.0")
	}
	u.busy(0)
	if exe, _ := u.step(); exe != u.exe || !strings.Contains(u.disk(), "wp v1.3.0") {
		t.Fatalf("idle: restart %q disk %q", exe, u.disk())
	}
}

// ... and while another wp's claim holds the newer download back.
func TestDrainingRunnerStaysDrainedWhileAnotherWpDownloads(t *testing.T) {
	e := cliServer(t, "v1.3.0", "v1.3.0", false)
	u := newUpdRunner(t, e, "v1.3.0")
	u.noted("v1.2.0", u.now.Add(-runnerDrainAfter))
	c, _ := newCLI(io.Discard, io.Discard)
	c.recordUpdate("v1.3.0", "started", true)
	u.busy(1)
	if exe, drain := u.step(); exe != "" || !drain {
		t.Fatalf("restart %q drain %v", exe, drain)
	}
}

// ... but a runner putting its own release back has no restart pending.
func TestPuttingItsOwnReleaseBackEndsTheDrain(t *testing.T) {
	e := cliServer(t, "v1.1.0", "v1.1.0", false)
	u := newUpdRunner(t, e, "v1.1.0") // the server is back on this process's release
	u.noted("v1.2.0", u.now.Add(-runnerDrainAfter))
	release := make(chan struct{})
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		<-release
		return u.p.c.selfUpdate(ctx, want)
	}
	u.busy(1)
	if exe := u.p.updateStep(context.Background()); exe != "" || u.p.draining {
		t.Fatalf("restart %q drain %v", exe, u.p.draining)
	}
	close(release)
	u.settle()
	if exe, drain := u.step(); exe != "" || drain || u.p.updatedTo != "" {
		t.Fatalf("restart %q drain %v updatedTo %q", exe, drain, u.p.updatedTo)
	}
}

// c6980: the runner noted v1.2.0 on disk, then another wp put this
// process's release back there (the server rolled back for a while, unseen
// by the runner): it doesn't restart on that file as v1.2.0, but downloads
// v1.2.0 again and restarts on it, in the same pass.
func TestRunnerDoesNotRestartOnAFileThatChangedSinceItNotedIt(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	var downloads atomic.Int32
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		downloads.Add(1)
		return u.p.c.selfUpdate(ctx, want)
	}
	u.busy(1)
	u.step()
	if u.p.updatedTo != "v1.2.0" {
		t.Fatalf("updatedTo %q", u.p.updatedTo)
	}
	putWP(t, u.exe, "v1.1.0") // another wp put the old release back
	u.busy(0)
	exe, _ := u.step()
	if exe != u.exe || !strings.Contains(u.disk(), "wp v1.2.0") || downloads.Load() != 2 {
		t.Fatalf("restart %q on %q after %d downloads\n%s", exe, u.disk(), downloads.Load(), u.out.String())
	}
}

// c6971: a claim another wp gives back (its download was cut short) holds
// the runner back no longer than until its next look at the record.
func TestReleasedClaimIsSeenBeforeItWouldHaveGoneStale(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	c, _ := newCLI(io.Discard, io.Discard)
	c.recordUpdate("v1.2.0", "started", true)
	var downloads atomic.Int32
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		downloads.Add(1)
		return u.p.c.selfUpdate(ctx, want)
	}
	u.busy(1)
	u.step()
	if downloads.Load() != 0 || !u.p.heldBack("v1.2.0") {
		t.Fatalf("not held back by the claim: %d downloads", downloads.Load())
	}
	lookedBy(t, u.p)
	c.releaseUpdateClaim("v1.2.0")
	u.p.updateHeldUntil = time.Now().Add(-time.Second) // the look is due
	u.step()
	if downloads.Load() != 1 || u.p.updatedTo != "v1.2.0" {
		t.Fatalf("downloads %d updatedTo %q", downloads.Load(), u.p.updatedTo)
	}
}

// ... while a claim still live holds it back after the look too.
func TestLiveClaimStillHoldsAfterTheLook(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	c, _ := newCLI(io.Discard, io.Discard)
	c.recordUpdate("v1.2.0", "started", true)
	u.p.downloadFn = func(context.Context, string) (string, string, error) {
		t.Error("downloaded beside a live claim")
		return "", "", errors.New("x")
	}
	u.busy(1)
	u.step()
	lookedBy(t, u.p)
	u.p.updateHeldUntil = time.Now().Add(-time.Second) // the look is due
	u.step()
	if !u.p.heldBack("v1.2.0") || !u.p.updateHeldByClaim {
		t.Fatal("not held back")
	}
	lookedBy(t, u.p)
}

// lookedBy: a claim holds the runner only until its next look at the
// record, updateHoldRecheck away, not as long as the claim stays fresh.
func lookedBy(t *testing.T, p *runnerProc) {
	t.Helper()
	if !p.updateHeldByClaim || p.updateHeldUntil.After(time.Now().Add(updateHoldRecheck)) {
		t.Fatalf("held by claim %v until %v, past the next look", p.updateHeldByClaim, p.updateHeldUntil)
	}
}

// breakState makes the shared state file unwritable: state.json is a
// directory, so it reads as empty and every write fails.
func breakState(t *testing.T) {
	t.Helper()
	c, err := newCLI(io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(c.dir, "state.json")
	_ = os.Remove(p)
	if err := os.MkdirAll(filepath.Join(p, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// c6981: a runner that couldn't read or save its claim doesn't download:
// another wp's claim may be there unseen, and its own isn't there to be
// seen. It tries again a little later, and says why once.
func TestRunnerDoesNotDownloadWithoutASavedClaim(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	breakState(t)
	u.p.downloadFn = func(context.Context, string) (string, string, error) {
		t.Error("downloaded without a saved claim")
		return "", "", errors.New("x")
	}
	u.busy(1)
	u.step()
	u.step()
	if !u.p.heldBack("v1.2.0") || u.p.updateHeldByClaim {
		t.Fatalf("held %v by claim %v", u.p.heldBack("v1.2.0"), u.p.updateHeldByClaim)
	}
	if n := strings.Count(u.out.String(), "Couldn't claim the update"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, u.out.String())
	}
}

// c7017: a state file that isn't valid JSON may hold another wp's claim:
// the runner doesn't claim over it (the file stays as it is) and doesn't
// download.
func TestRunnerDoesNotClaimOverAStateItCannotRead(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	c, _ := newCLI(io.Discard, io.Discard)
	p := filepath.Join(c.dir, "state.json")
	if err := os.WriteFile(p, []byte("{\"update\": "), 0o600); err != nil {
		t.Fatal(err)
	}
	u.p.downloadFn = func(context.Context, string) (string, string, error) {
		t.Error("downloaded over a state it couldn't read")
		return "", "", errors.New("x")
	}
	u.busy(1)
	u.step()
	if !u.p.heldBack("v1.2.0") || u.p.updateHeldByClaim {
		t.Fatalf("held %v by claim %v", u.p.heldBack("v1.2.0"), u.p.updateHeldByClaim)
	}
	if b, _ := os.ReadFile(p); string(b) != "{\"update\": " {
		t.Errorf("state.json was rewritten: %q", b)
	}
}

// ... and a wp command doesn't start the background upgrade either.
func TestCommandDoesNotStartTheUpgradeWithoutASavedClaim(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")
	breakState(t)
	b.finish("v1.2.0", someCmd)
	if b.count() != 0 {
		t.Fatalf("started without a saved claim: %v", b.started)
	}
}

// c6982: this runner's own try of v1.3.0 is more than 6 hours old, and
// another wp failed v1.3.0 a minute ago: that newer failure holds it back.
func TestNewerSharedFailureHoldsBackAfterTheRunnersOwnBackoff(t *testing.T) {
	e := cliServer(t, "v1.3.0", "v1.3.0", false)
	u := newUpdRunner(t, e, "v1.3.0")
	u.p.downloadFn = failingDownload
	u.busy(1)
	u.step() // the runner's own failure
	u.now = u.now.Add(updateRetryEvery + time.Minute)
	c, _ := newCLI(io.Discard, io.Discard)
	c.recordUpdate("v1.3.0", "failed: no route", false) // another wp's, later
	u.p.downloadFn = func(context.Context, string) (string, string, error) {
		t.Error("downloaded inside another wp's newer backoff")
		return "", "", errors.New("x")
	}
	u.step()
	if !u.p.heldBack("v1.3.0") || u.p.updateHeldByClaim {
		t.Fatalf("held %v by claim %v", u.p.heldBack("v1.3.0"), u.p.updateHeldByClaim)
	}
}
