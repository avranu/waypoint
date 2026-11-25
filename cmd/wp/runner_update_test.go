package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
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

// step is one pass of the loop's update logic; with jobs running a download
// is started in the background and waited for here, so a test sees its result.
func (u *updRunner) step() (string, bool) {
	u.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	exe := u.p.updateStep(ctx)
	u.settle()
	return exe, u.p.draining
}

// settle waits for a background download to finish and collects its result.
func (u *updRunner) settle() {
	u.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for u.p.updating && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		u.p.collectUpdate()
	}
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
	if !strings.Contains(u.out.String(), "Updated "+u.exe+" to wp v1.2.0.") || !strings.Contains(u.out.String(), "Restarting on wp v1.2.0 at "+u.exe) {
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

// The loop must not stop polling (and applying cancels) while a busy runner
// downloads: updateStep returns at once and a later pass collects the result.
func TestBusyDownloadDoesNotBlockTheLoop(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	release := make(chan struct{})
	var calls atomic.Int32
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		calls.Add(1)
		<-release
		return want, u.exe, nil
	}
	u.busy(1)
	done := make(chan string, 1)
	go func() { done <- u.p.updateStep(context.Background()) }()
	select {
	case exe := <-done:
		if exe != "" {
			t.Fatalf("restart %q", exe)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("updateStep waited for the download")
	}
	for range 3 { // more passes while it downloads: no second download
		if exe := u.p.updateStep(context.Background()); exe != "" {
			t.Fatalf("restart %q while downloading", exe)
		}
	}
	if u.p.updatedTo != "" {
		t.Fatal("recorded the update before it finished")
	}
	close(release)
	waitFor(t, "the result to be collected", 5*time.Second, func() bool {
		u.p.updateStep(context.Background())
		return u.p.updatedTo == "v1.2.0"
	})
	if calls.Load() != 1 || u.p.updatedExe != u.exe || !strings.Contains(u.out.String(), "this runner restarts on it when its 1 job finishes.") {
		t.Fatalf("downloads %d, exe %q, log:\n%s", calls.Load(), u.p.updatedExe, u.out.String())
	}
}

// wp follows the server down too (a rollback).
func TestRunnerFollowsAnOlderServer(t *testing.T) {
	e := cliServer(t, "v1.0.0", "v1.0.0", false)
	u := newUpdRunner(t, e, "v1.0.0") // the runner's wp is v1.1.0
	if exe, _ := u.step(); exe != u.exe || !strings.Contains(u.disk(), "wp v1.0.0") {
		t.Fatalf("restart %q disk %q", exe, u.disk())
	}
}

// Through the real loop: a runner that always has a job still gets the new
// binary at once, drains after the wait (polling with no lanes), and
// restarts once the job ends.
func TestRunLoopUpdatesDrainsAndRestarts(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	me := e.runnerUser("robin")
	exe := installedWP(t)
	setVersion(t, "v1.1.0")
	dir := t.TempDir()
	flag := filepath.Join(dir, "release")
	ctx := context.Background()

	var offset atomic.Int64
	oldClock := runnerClock
	runnerClock = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	t.Cleanup(func() { runnerClock = oldClock })

	var mu sync.Mutex
	var polls []map[string]any
	target, _ := url.Parse(e.url)
	proxy := httputil.NewSingleHostReverseProxy(target)
	// The runner hears of the new release only once its job is running.
	started := filepath.Join(dir, "started")
	proxy.ModifyResponse = func(res *http.Response) error {
		if p := res.Request.URL.Path; p != "/api/v1/runner/poll" && p != "/api/v1/runner/register" {
			return nil
		}
		if _, err := os.Stat(started); err == nil {
			return nil
		}
		b, _ := io.ReadAll(res.Body)
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			m["server_version"] = ""
			b, _ = json.Marshal(m)
		}
		res.Body = io.NopCloser(bytes.NewReader(b))
		res.ContentLength, res.Header["Content-Length"] = int64(len(b)), []string{strconv.Itoa(len(b))}
		return nil
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/runner/poll" {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			mu.Lock()
			polls = append(polls, m)
			mu.Unlock()
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()
	t.Setenv("WAYPOINT_SERVER", front.URL)

	if _, err := e.svc.RegisterRunner(ctx, me, core.RunnerRegistration{Name: "box", AllowedDirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("touch "+started+"; while [ ! -f "+flag+" ]; do sleep 0.1; done", dir)}); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	c := &cli{dir: e.dir, out: &out, err: &out}
	done := make(chan error, 1)
	go func() {
		done <- runRunner(ctx, c, runnerOpts{Name: "box", Allow: []string{dir}, MaxJobs: 3, PollWait: -1, NoUsageSync: true})
	}()
	waitFor(t, "the binary to be replaced with the job running", 30*time.Second, func() bool {
		b, _ := os.ReadFile(exe)
		return strings.Contains(string(b), "wp v1.2.0")
	})
	select {
	case err := <-done:
		t.Fatalf("restarted with a job running: %v\n%s", err, out.String())
	default:
	}
	waitFor(t, "the loop to note the update", 30*time.Second, func() bool {
		return strings.Contains(out.String(), "this runner restarts on it when its 1 job finishes.")
	})
	offset.Store(int64(runnerDrainAfter + time.Minute))
	defer func() {
		if t.Failed() {
			mu.Lock()
			t.Logf("last polls: %v\nlog:\n%s", polls[max(len(polls)-3, 0):], out.String())
			mu.Unlock()
		}
	}()
	waitFor(t, "a heartbeat poll with no lanes while a job runs", 30*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range polls[len(polls)-1:] {
			running, _ := m["running"].([]any)
			lanes, _ := m["lanes"].([]any)
			return len(running) > 0 && len(lanes) == 0 && m["free"] == float64(0)
		}
		return false
	})
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var re *restartError
		if !errors.As(err, &re) || re.exe != exe {
			t.Fatalf("ended with %v\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("no restart after the job ended\n%s", out.String())
	}
}

// record is the shared state file's update record, as another wp would read it.
func (u *updRunner) record() *updateCheck {
	u.t.Helper()
	c, err := newCLI(io.Discard, io.Discard)
	if err != nil {
		u.t.Fatal(err)
	}
	return c.state.Update
}

// The newer binary on disk is used even when the download for a still newer
// release fails (1642.1): the runner restarts on what it has.
func TestFailedFollowUpDownloadStillRestartsOnTheBinaryOnDisk(t *testing.T) {
	e := cliServer(t, "v1.3.0", "v1.3.0", false)
	u := newUpdRunner(t, e, "v1.3.0")
	u.p.updatedTo, u.p.updatedExe, u.p.updatedAt = "v1.2.0", u.exe, u.now // installed earlier
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		return "", "", errors.New("no route")
	}
	u.busy(1)
	if exe, drain := u.step(); exe != "" || drain {
		t.Fatalf("busy: restart %q drain %v", exe, drain)
	}
	if !strings.Contains(u.out.String(), "Couldn't update wp") {
		t.Fatalf("log:\n%s", u.out.String())
	}
	// Failed try inside the backoff: no new download, but v1.2.0 is on disk.
	u.p.downloadFn = func(context.Context, string) (string, string, error) {
		t.Error("downloaded again inside the backoff")
		return "", "", errors.New("again")
	}
	u.now = u.now.Add(runnerDrainAfter + time.Minute)
	if exe, drain := u.step(); exe != "" || !drain {
		t.Fatalf("after the limit: restart %q drain %v", exe, drain)
	}
	if !strings.Contains(u.out.String(), "restarting on wp v1.2.0") || strings.Contains(u.out.String(), "wp v1.3.0 since") {
		t.Errorf("the drain line must name the version on disk:\n%s", u.out.String())
	}
	u.busy(0)
	if exe, _ := u.step(); exe != u.exe {
		t.Fatalf("idle: restart %q", exe)
	}
	if !strings.Contains(u.out.String(), "Restarting on wp v1.2.0 at "+u.exe) {
		t.Errorf("log:\n%s", u.out.String())
	}
}

// Idle, the failed follow-up download is followed at once, in the same pass,
// by the restart: no poll in between may take a job onto this process
// (1788.1).
func TestIdleFailedFollowUpDownloadThenRestartsOnDisk(t *testing.T) {
	e := cliServer(t, "v1.3.0", "v1.3.0", false)
	u := newUpdRunner(t, e, "v1.3.0")
	u.p.updatedTo, u.p.updatedExe, u.p.updatedAt = "v1.2.0", u.exe, u.now
	u.p.downloadFn = func(context.Context, string) (string, string, error) { return "", "", errors.New("no route") }
	if exe, drain := u.step(); exe != u.exe || drain {
		t.Fatalf("the failed pass: restart %q drain %v", exe, drain)
	}
	if !strings.Contains(u.out.String(), "Couldn't update wp") {
		t.Errorf("log:\n%s", u.out.String())
	}
}

// A download that finishes after the server moved on is followed by another.
func TestDownloadThatFinishesAfterTheServerMovedOnIsFollowedByAnother(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	release := make(chan struct{})
	var wants []string
	var mu sync.Mutex
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		mu.Lock()
		wants = append(wants, want)
		first := len(wants) == 1
		mu.Unlock()
		if first {
			<-release
		}
		return want, u.exe, nil
	}
	u.busy(1)
	u.p.updateStep(context.Background())
	u.p.serverVersion = "v1.3.0" // the server moves on mid-download
	close(release)
	waitFor(t, "the second download", 5*time.Second, func() bool {
		u.p.updateStep(context.Background())
		mu.Lock()
		defer mu.Unlock()
		return len(wants) == 2
	})
	if wants[1] != "v1.3.0" {
		t.Fatalf("downloads %v", wants)
	}
}

func TestInterruptedIdleDownloadIsNoFailureAndReleasesTheClaim(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	ctx, cancel := context.WithCancel(context.Background())
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		if rec := u.record(); rec == nil || rec.Result != "started" {
			t.Errorf("no claim during the download: %+v", rec)
		}
		cancel() // the runner is stopping
		return "", "", context.Canceled
	}
	if exe := u.p.updateStep(ctx); exe != "" {
		t.Fatalf("restart %q", exe)
	}
	if strings.Contains(u.out.String(), "Couldn't update") || strings.Contains(u.out.String(), "6 hours") {
		t.Errorf("an interrupted download was reported as a failure:\n%s", u.out.String())
	}
	if rec := u.record(); rec != nil {
		t.Errorf("the record = %+v, want the claim released", rec)
	}
}

func TestIdleDownloadFailureIsLoggedAndRecorded(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.p.downloadFn = func(context.Context, string) (string, string, error) { return "", "", errors.New("bad checksum") }
	if exe := u.p.updateStep(context.Background()); exe != "" {
		t.Fatalf("restart %q", exe)
	}
	if !strings.Contains(u.out.String(), "Couldn't update wp: bad checksum. Carrying on with v1.1.0; trying again in 6 hours.") {
		t.Errorf("log:\n%s", u.out.String())
	}
	if rec := u.record(); rec == nil || rec.Server != "v1.2.0" || rec.Result != "failed: bad checksum" {
		t.Errorf("record = %+v", rec)
	}
}

func TestIdleDownloadSuccessIsRecorded(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	if exe := u.p.updateStep(context.Background()); exe != u.exe {
		t.Fatalf("restart %q", exe)
	}
	if rec := u.record(); rec == nil || rec.Result != "updated from v1.1.0 to v1.2.0" {
		t.Errorf("record = %+v", rec)
	}
}

// A runner that stops while a busy download runs leaves no claim that would
// keep every wp command from updating for 6 hours.
func TestStoppingDuringABusyDownloadReleasesTheClaim(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	u.p.downloadFn = func(ctx context.Context, want string) (string, string, error) {
		close(started)
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	u.busy(1)
	u.p.updateStep(ctx)
	<-started
	if rec := u.record(); rec == nil || rec.Result != "started" {
		t.Fatalf("no claim: %+v", rec)
	}
	cancel()
	u.p.stopUpdate()
	if rec := u.record(); rec != nil {
		t.Errorf("record = %+v, want the claim released", rec)
	}
	if strings.Contains(u.out.String(), "Couldn't update") {
		t.Errorf("log:\n%s", u.out.String())
	}
}

func TestReleaseUpdateClaimLeavesOtherRecordsAlone(t *testing.T) {
	cliServer(t, "v1.2.0", "v1.2.0", false)
	c, _ := newCLI(io.Discard, io.Discard)
	for name, rec := range map[string]*updateCheck{
		"another version": {Server: "v1.3.0", Result: "started"},
		"a result":        {Server: "v1.2.0", Result: "failed: x"},
	} {
		c.recordUpdate(rec.Server, rec.Result, true)
		c.releaseUpdateClaim("v1.2.0")
		if c.state.Update == nil {
			t.Errorf("%s: released", name)
		}
	}
	c.recordUpdate("v1.2.0", "started", true)
	c.releaseUpdateClaim("v1.2.0")
	if c.state.Update != nil {
		t.Errorf("claim kept: %+v", c.state.Update)
	}
}

func TestCollectUpdateWithNoJobsNamesNoJobCount(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	u := newUpdRunner(t, e, "v1.2.0")
	u.p.updating = true
	u.p.updateDone = make(chan updateResult, 1)
	u.p.updateDone <- updateResult{sv: "v1.2.0", got: "v1.2.0", exe: u.exe}
	u.p.collectUpdate()
	out := u.out.String()
	if strings.Contains(out, "0 jobs") || !strings.Contains(out, "Updated "+u.exe+" to wp v1.2.0.") {
		t.Errorf("log:\n%s", out)
	}
}

func TestUpdateAllowed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		local, server string
		opts          runnerOpts
		env           string
		want          bool
	}{
		{"a newer release", "v1.1.0", "v1.2.0", runnerOpts{}, "", true},
		{"an older release", "v1.1.0", "v1.0.0", runnerOpts{}, "", true},
		{"equal versions", "v1.1.0", "v1.1.0", runnerOpts{}, "", false},
		{"dev build", "dev", "v1.2.0", runnerOpts{}, "", false},
		{"dirty build", "v1.1.0-dirty", "v1.2.0", runnerOpts{}, "", false},
		{"between tags", "v1.1.0-3-gabc1234", "v1.2.0", runnerOpts{}, "", false},
		{"no server version", "v1.1.0", "", runnerOpts{}, "", false},
		{"a dev server", "v1.1.0", "dev", runnerOpts{}, "", false},
		{"a non-release server", "v1.1.0", "v1.2.0-1-gabc1234", runnerOpts{}, "", false},
		{"--no-auto-update", "v1.1.0", "v1.2.0", runnerOpts{NoAutoUpdate: true}, "", false},
		{"--once", "v1.1.0", "v1.2.0", runnerOpts{Once: true}, "", false},
		{"WAYPOINT_NO_AUTO_UPDATE", "v1.1.0", "v1.2.0", runnerOpts{}, "1", false},
		{"WAYPOINT_NO_AUTO_UPDATE=0", "v1.1.0", "v1.2.0", runnerOpts{}, "0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setVersion(t, tc.local)
			t.Setenv("WAYPOINT_NO_AUTO_UPDATE", tc.env)
			p := &runnerProc{opts: tc.opts, serverVersion: tc.server}
			if got := p.updateAllowed(); got != tc.want {
				t.Fatalf("updateAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}
