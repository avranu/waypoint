package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The runner decides on the wp on disk and on downloads in flight, its own
// or another wp's, not only on what its own downloads did (1642.1, 1650.1,
// 1650.4, c6694, c6695, c6697).

// putWP writes the wp release v that cliServer serves at exe, as another
// wp's upgrade would.
func putWP(t *testing.T, exe, v string) {
	t.Helper()
	if err := os.WriteFile(exe, []byte(cliBin(v)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func failingDownload(context.Context, string) (string, string, error) {
	return "", "", errors.New("no route")
}

// 1650.1: the runner's own download failed, then another wp put the server's
// release on disk: inside the backoff the runner still restarts on it.
func TestRunnerRestartsOnTheServersReleaseAnotherWpInstalled(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.p.downloadFn = failingDownload
	u.busy(1)
	if exe, _ := u.step(); exe != "" {
		t.Fatalf("failed download: restart %q", exe)
	}
	putWP(t, u.exe, "v1.2.0") // a job's wp upgrade --auto got there
	u.p.downloadFn = func(context.Context, string) (string, string, error) {
		t.Error("downloaded again inside the backoff")
		return "", "", errors.New("again")
	}
	u.now = u.now.Add(diskLookEvery)
	if exe, _ := u.step(); exe != "" {
		t.Fatalf("busy: restart %q", exe)
	}
	if u.p.updatedTo != "v1.2.0" || !strings.Contains(u.out.String(), "Another wp put wp v1.2.0 at "+u.exe) {
		t.Fatalf("the disk wasn't looked at: updatedTo %q\n%s", u.p.updatedTo, u.out.String())
	}
	u.busy(0)
	if exe, _ := u.step(); exe != u.exe {
		t.Fatalf("idle: restart %q", exe)
	}
}

// The disk is looked at once a minute at most.
func TestRunnerLooksAtTheDiskOnceAMinute(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.p.downloadFn = failingDownload
	u.busy(1)
	u.step()
	u.step() // looks: the disk is still the old one
	putWP(t, u.exe, "v1.2.0")
	u.now = u.now.Add(diskLookEvery - time.Second)
	u.step()
	if u.p.updatedTo != "" {
		t.Fatalf("looked again within the minute: %q", u.p.updatedTo)
	}
	u.now = u.now.Add(time.Second)
	u.step()
	if u.p.updatedTo != "v1.2.0" {
		t.Fatalf("didn't look after the minute: %q", u.p.updatedTo)
	}
}

// 1650.4: a busy runner put another release on disk, then the server came
// back to the release this process runs: the disk is put back, no restart.
func TestServerBackOnTheRunnersReleasePutsTheDiskBack(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.1.0", false)
	u := newUpdRunner(t, e, "v1.2.0") // this process is v1.1.0
	u.noted("v1.2.0", u.now)
	u.p.serverVersion = "v1.1.0" // the server rolled back
	var wants []string
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		wants = append(wants, want)
		putWP(t, u.exe, want)
		return want, u.exe, nil
	}
	u.busy(1)
	u.now = u.now.Add(runnerDrainAfter + time.Minute)
	if exe, drain := u.step(); exe != "" || drain {
		t.Fatalf("restart %q drain %v", exe, drain)
	}
	if len(wants) != 1 || wants[0] != "v1.1.0" {
		t.Fatalf("downloads %v, want the running release put back", wants)
	}
	if u.p.updatedTo != "" || !strings.Contains(u.out.String(), "Put wp v1.1.0 back at "+u.exe) {
		t.Fatalf("updatedTo %q\n%s", u.p.updatedTo, u.out.String())
	}
	u.busy(0)
	if exe, _ := u.step(); exe != "" {
		t.Fatalf("idle: restarted on %q although it runs the server's release", exe)
	}
	if len(wants) != 1 {
		t.Fatalf("downloaded again: %v", wants)
	}
}

// ... and idle, the same, without a restart.
func TestIdleRunnerPutsItsReleaseBackWithoutRestarting(t *testing.T) {
	e := cliServer(t, "v1.1.0", "v1.1.0", false)
	u := newUpdRunner(t, e, "v1.1.0")
	u.noted("v1.2.0", u.now)
	if exe, _ := u.step(); exe != "" {
		t.Fatalf("restart %q", exe)
	}
	if !strings.Contains(u.disk(), "wp v1.1.0") || u.p.updatedTo != "" {
		t.Fatalf("disk %q updatedTo %q", u.disk(), u.p.updatedTo)
	}
}

// ... but a failed put-back doesn't restart on the other release.
func TestFailedPutBackDoesNotRestartOnTheOtherRelease(t *testing.T) {
	e := cliServer(t, "v1.1.0", "v1.1.0", false)
	u := newUpdRunner(t, e, "v1.1.0")
	u.noted("v1.2.0", u.now)
	u.p.downloadFn = failingDownload
	for range 2 {
		if exe, _ := u.step(); exe != "" {
			t.Fatalf("restart %q", exe)
		}
	}
}

// c6695: while a download runs, the runner doesn't restart, not even on a
// binary it recorded as the server's: the download may replace it first.
func TestNoRestartWhileADownloadRuns(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.noted("v1.2.0", u.now)
	u.p.updating, u.p.updateDone, u.p.updateCancel = true, make(chan updateResult, 1), func() {}
	if exe := u.p.updateStep(context.Background()); exe != "" {
		t.Fatalf("restarted on %q with a download running", exe)
	}
	u.p.updateDone <- updateResult{sv: "v1.2.0", got: "v1.2.0", exe: u.exe}
	if exe := u.p.updateStep(context.Background()); exe != u.exe {
		t.Fatalf("after the download: restart %q", exe)
	}
}

// c6697: a wp command's fresh claim on the server's release holds the runner
// back, and while it runs the runner doesn't restart on an older download.
func TestRunnerWaitsForAnotherWpsClaim(t *testing.T) {
	e := cliServer(t, "v1.3.0", "v1.3.0", false)
	u := newUpdRunner(t, e, "v1.3.0")
	u.noted("v1.2.0", u.now)
	c, _ := newCLI(io.Discard, io.Discard)
	c.recordUpdate("v1.3.0", "started", true) // a wp command's
	u.p.downloadFn = func(context.Context, string) (string, string, error) {
		t.Error("downloaded beside another wp's claim")
		return "", "", errors.New("x")
	}
	if exe, _ := u.step(); exe != "" {
		t.Fatalf("restarted on %q while another wp downloads the server's", exe)
	}
	if strings.Contains(u.out.String(), "updating wp") {
		t.Errorf("log:\n%s", u.out.String())
	}
	if rec := u.record(); rec == nil || rec.Result != "started" {
		t.Fatalf("the claim was overwritten: %+v", rec)
	}
	// The claim goes stale (its process died): the runner tries itself.
	_ = c.updateState(func(s *State) { s.Update.Checked = s.Update.Checked.Add(-updateClaimFresh) })
	u.p.updateHeldUntil = time.Time{}
	var got string
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) { got = want; return want, u.exe, nil }
	if exe, _ := u.step(); exe != u.exe || got != "v1.3.0" {
		t.Fatalf("restart %q download %q", exe, got)
	}
}

// c6694: a runner that restarted keeps the backoff of a failure the shared
// record holds for the server's release.
func TestRestartedRunnerKeepsTheSharedBackoff(t *testing.T) {
	e := cliServer(t, "v1.3.0", "v1.3.0", false)
	u := newUpdRunner(t, e, "v1.3.0")
	c, _ := newCLI(io.Discard, io.Discard)
	c.recordUpdate("v1.3.0", "failed: no route", false) // the runner before the restart
	u.p.downloadFn = func(context.Context, string) (string, string, error) {
		t.Error("downloaded inside the recorded backoff")
		return "", "", errors.New("x")
	}
	u.step()
	if u.p.updateHeldUntil.IsZero() || u.p.updateHeldByClaim {
		t.Fatalf("held until %v by claim %v", u.p.updateHeldUntil, u.p.updateHeldByClaim)
	}
	// Another release isn't held back.
	u.p.serverVersion = "v1.4.0"
	var got string
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) { got = want; return want, u.exe, nil }
	u.step()
	if got != "v1.4.0" {
		t.Fatalf("download %q", got)
	}
}

// 1650.5: a runner that stops (its context not cancelled: a fatal poll
// error) cancels its download and waits for it before the claim goes.
func TestStopUpdateWaitsForTheDownload(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	done := make(chan struct{})
	started := make(chan struct{})
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // finishing its renames
		close(done)
		return "", "", ctx.Err()
	}
	u.busy(1)
	u.p.updateStep(context.Background())
	<-started
	u.p.stopUpdate()
	select {
	case <-done:
	default:
		t.Fatal("stopUpdate returned before the download")
	}
	if rec := u.record(); rec != nil {
		t.Errorf("record = %+v, want the claim released", rec)
	}
}

// 1650.3: the SessionStart hook's daily claim and the same hook's look at
// the server's header start one upgrade, not two.
func TestDailyClaimAndHeaderStartOneUpgrade(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")
	c, _ := newCLI(io.Discard, io.Discard)
	c.autoUpdateInBackground()
	b.finish("v1.2.0", someCmd)
	if b.count() != 1 {
		t.Fatalf("started %v", b.started)
	}
}

// 1650.6: an upgrade that couldn't be started gives its claim back.
func TestUnstartedUpgradeGivesTheClaimBack(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	b := newBehindEnv(t)
	setVersion(t, "v1.1.0")
	backgroundStart = func(string, ...string) bool { return false } // newBehindEnv's cleanup puts the original back
	b.finish("v1.2.0", someCmd)
	if c, _ := newCLI(io.Discard, io.Discard); c.state.Update != nil {
		t.Fatalf("record = %+v, want none", c.state.Update)
	}
}

// c6696: a result for one release doesn't overwrite a fresh claim on another.
func TestOlderResultLeavesANewerClaim(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	c, _ := newCLI(io.Discard, io.Discard)
	c.recordUpdate("v1.3.0", "started", true)
	c.recordUpdate("v1.2.0", "updated from v1.1.0 to v1.2.0", false)
	if u := c.state.Update; u == nil || u.Server != "v1.3.0" || u.Result != "started" {
		t.Fatalf("record = %+v", u)
	}
	// Its own claim's result, and one with no claim live, are written.
	c.recordUpdate("v1.3.0", "failed: x", false)
	c.recordUpdate("v1.2.0", "up to date", false)
	if u := c.state.Update; u == nil || u.Server != "v1.2.0" {
		t.Fatalf("record = %+v", u)
	}
}

// 1650.2: replaceExe waits for another wp's replace of the same binary.
func TestReplaceExeIsOneAtATime(t *testing.T) {
	exe := installedWP(t)
	unlock, err := lockFileWithin(strings.TrimSuffix(exe, "wp")+".wp-update.lock", 0)
	if err != nil {
		t.Fatal(err)
	}
	next := exe + ".next"
	_ = os.WriteFile(next, []byte("new"), 0o755)
	done := make(chan error, 1)
	go func() { done <- replaceExe(exe, next) }()
	select {
	case err := <-done:
		t.Fatalf("replaced while another held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("exe = %q", b)
	}
}

// c6732 (c6697 again): any live claim holds the runner's download back, as it
// holds back every wp command's: the SessionStart hook's daily one, which
// names no release, and one on another release (its wp upgrade --auto
// downloads what the server serves when it runs, the same binary).
func TestRunnerWaitsForAnyLiveClaim(t *testing.T) {
	for name, claimed := range map[string]string{"daily hook": "", "another release": "v1.2.0"} {
		t.Run(name, func(t *testing.T) {
			e := cliServer(t, "v1.3.0", "v1.3.0", false)
			u := newUpdRunner(t, e, "v1.3.0")
			c, _ := newCLI(io.Discard, io.Discard)
			c.recordUpdate(claimed, "started", true)
			u.p.downloadFn = func(context.Context, string) (string, string, error) {
				t.Error("downloaded beside another wp's claim")
				return "", "", errors.New("x")
			}
			if exe, _ := u.step(); exe != "" {
				t.Fatalf("restart %q", exe)
			}
			if rec := u.record(); rec == nil || rec.Server != claimed || rec.Result != "started" {
				t.Fatalf("the claim was overwritten: %+v", rec)
			}
			if !u.p.updateHeldByClaim || !u.p.heldBack("v1.3.0") {
				t.Fatalf("not held: for %q until %v", u.p.updateHeldFor, u.p.updateHeldUntil)
			}
		})
	}
}

// 1788.2: a file on disk that only says it is the server's release (its
// SHA-256 isn't the one SHA256SUMS names) is not run, and not restarted on.
func TestRunnerDoesNotRunAFileThatOnlySaysItIsTheRelease(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.p.downloadFn = failingDownload
	u.busy(1)
	u.step()
	marker := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(u.exe, []byte("#!/bin/sh\ntouch "+marker+"\necho 'wp v1.2.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	u.now = u.now.Add(diskLookEvery)
	u.step()
	u.busy(0)
	if exe, _ := u.step(); exe != "" || u.p.updatedTo != "" {
		t.Fatalf("restart %q updatedTo %q", exe, u.p.updatedTo)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the file on disk was run")
	}
}
