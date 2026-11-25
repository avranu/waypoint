//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// stubDetect keeps the tests from probing the CLIs installed on this machine.
func stubDetect(t *testing.T) {
	t.Helper()
	old := detectCLIsFn
	detectCLIsFn = func(context.Context) []runnerCLI {
		return []runnerCLI{{Name: "claude", Version: "9.9.9", SignedIn: boolp(true)}}
	}
	t.Cleanup(func() { detectCLIsFn = old })
}

// runnerUser makes a person with a plain token (an agent's token can't
// be a runner) and points the CLI at it.
func (e *env) runnerUser(name string) *core.Actor {
	e.t.Helper()
	stubDetect(e.t)
	u, err := e.svc.CreateUser(context.Background(), core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct horse battery"})
	if err != nil {
		e.t.Fatal(err)
	}
	me := &core.Actor{User: u}
	tok, _, err := e.svc.CreateToken(context.Background(), me, core.TokenSpec{Name: "runner"})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Setenv("WAYPOINT_TOKEN", tok)
	return me
}

func cmdJobInput(command, dir string) json.RawMessage {
	b, _ := json.Marshal(core.CommandJob{Command: command, Dir: dir})
	return b
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunnerTakesQueuedJobsOnce(t *testing.T) {
	e := newEnv(t)
	me := e.runnerUser("robin")
	dir := t.TempDir()
	ctx := context.Background()
	reg := core.RunnerRegistration{Name: "box", AllowedDirs: []string{dir}}
	if _, err := e.svc.RegisterRunner(ctx, me, reg); err != nil {
		t.Fatal(err)
	}
	j1, err := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("echo first; echo bad >&2; exit 0", dir), Source: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	j2, _ := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("echo second; exit 4", dir)})

	var out syncBuffer
	c := &cli{dir: e.dir, out: &out, err: &out}
	err = runRunner(ctx, c, runnerOpts{Name: "box", Allow: []string{dir}, MaxJobs: 1, Once: true, PollWait: 0})
	if err != nil {
		t.Fatalf("runner: %v\n%s", err, out.String())
	}
	got1, _ := e.svc.GetRunnerJob(ctx, me, j1.ID, 0)
	got2, _ := e.svc.GetRunnerJob(ctx, me, j2.ID, 0)
	if got1.State != "done" || got1.ExitCode == nil || *got1.ExitCode != 0 || !strings.Contains(*got1.Log, "first\n") || !strings.Contains(*got1.Log, "bad\n") {
		t.Errorf("job 1 = %+v log %v", got1, got1.Log)
	}
	if got2.State != "failed" || *got2.ExitCode != 4 || got2.Runner != "box" {
		t.Errorf("job 2 = %+v", got2)
	}
	if !strings.Contains(out.String(), "is connected") {
		t.Errorf("runner said %q", out.String())
	}
	rs, _ := e.svc.ListRunners(ctx, me, false)
	if len(rs) != 1 || rs[0].Name != "box" || rs[0].OS == "" || rs[0].Version == "" || len(rs[0].AllowedDirs) != 1 {
		t.Errorf("runner = %+v", rs)
	}
}

func TestRunnerRefusesAnAgentToken(t *testing.T) {
	e := newEnv(t)
	stubDetect(t)
	e.login()
	var out syncBuffer
	err := runRunner(context.Background(), &cli{dir: e.dir, out: &out, err: &out}, runnerOpts{Name: "box", MaxJobs: 1, Once: true, PollWait: 0})
	if err == nil || !strings.Contains(err.Error(), "not as an agent") {
		t.Errorf("err = %v", err)
	}
}

func TestRunnerCancelAndRemoval(t *testing.T) {
	e := newEnv(t)
	me := e.runnerUser("robin")
	dir := t.TempDir()
	ctx := context.Background()
	rctx, stop := context.WithCancel(ctx)
	var out syncBuffer
	done := make(chan error, 1)
	c := &cli{dir: e.dir, out: &out, err: &out}
	go func() {
		done <- runRunner(rctx, c, runnerOpts{Name: "box", Allow: []string{dir}, MaxJobs: 1, PollWait: -1})
	}()
	var runner *core.Runner
	waitFor(t, "the runner to register", 10*time.Second, func() bool {
		rs, _ := e.svc.ListRunners(ctx, me, false)
		if len(rs) == 1 {
			runner = rs[0]
		}
		return len(rs) == 1
	})
	job, err := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("sleep 60", dir)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to start", 10*time.Second, func() bool {
		j, _ := e.svc.GetRunnerJob(ctx, me, job.ID, -1)
		return j.State == "running"
	})
	second, _ := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("echo after", dir)})
	time.Sleep(300 * time.Millisecond)
	if j, _ := e.svc.GetRunnerJob(ctx, me, second.ID, -1); j.State != "queued" {
		t.Errorf("a full runner took a second job: %s", j.State)
	}
	start := time.Now()
	if _, err := e.svc.CancelRunnerJob(ctx, me, job.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to be cancelled", 15*time.Second, func() bool {
		j, _ := e.svc.GetRunnerJob(ctx, me, job.ID, -1)
		return j.State == "cancelled"
	})
	t.Logf("cancelled in %v", time.Since(start))
	waitFor(t, "the freed slot to take the waiting job", 15*time.Second, func() bool {
		j, _ := e.svc.GetRunnerJob(ctx, me, second.ID, -1)
		return j.State == "done"
	})
	if err := e.svc.DeleteRunner(ctx, me, runner.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runner ended with %v\n%s", err, out.String())
		}
	case <-time.After(40 * time.Second):
		t.Fatal("the runner kept running after it was removed")
	}
	stop()
	if !strings.Contains(out.String(), "was removed") {
		t.Errorf("output = %s", out.String())
	}
}

// A job goes on, and its end is reported, when the server goes away and
// comes back.
func TestRunnerSurvivesTheServerGoingAway(t *testing.T) {
	e := newEnv(t)
	me := e.runnerUser("robin")
	dir := t.TempDir()
	ctx := context.Background()
	target, _ := url.Parse(e.url)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var down atomic.Bool
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()
	t.Setenv("WAYPOINT_SERVER", front.URL)

	rctx, stop := context.WithCancel(ctx)
	defer stop()
	var out syncBuffer
	done := make(chan error, 1)
	c := &cli{dir: e.dir, out: &out, err: &out}
	go func() {
		done <- runRunner(rctx, c, runnerOpts{Name: "box", Allow: []string{dir}, MaxJobs: 1, PollWait: -1})
	}()
	waitFor(t, "the runner to register", 10*time.Second, func() bool {
		rs, _ := e.svc.ListRunners(ctx, me, false)
		return len(rs) == 1
	})
	job, err := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("echo before; sleep 2; echo after", dir)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to start", 10*time.Second, func() bool {
		j, _ := e.svc.GetRunnerJob(ctx, me, job.ID, -1)
		return j.State == "running"
	})
	down.Store(true)
	time.Sleep(3500 * time.Millisecond)
	if j, _ := e.svc.GetRunnerJob(ctx, me, job.ID, -1); j.State != "running" {
		t.Fatalf("the job ended while nobody could hear it: %s", j.State)
	}
	down.Store(false)
	waitFor(t, "the job's end to be reported", 40*time.Second, func() bool {
		j, _ := e.svc.GetRunnerJob(ctx, me, job.ID, -1)
		return j.State == "done"
	})
	j, _ := e.svc.GetRunnerJob(ctx, me, job.ID, 0)
	if !strings.Contains(*j.Log, "before\n") || !strings.Contains(*j.Log, "after\n") {
		t.Errorf("log = %q", *j.Log)
	}
	stop()
	<-done
}

// syncBuffer is a bytes.Buffer the runner's goroutines and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type fakeMeter struct{ s loadSample }

func (f fakeMeter) sample() loadSample { return f.s }

func TestRunnerTakesFewerHeavyJobsWhileTheMachineIsBusy(t *testing.T) {
	e := newEnv(t)
	me := e.runnerUser("robin")
	old := newLoadMeterFn
	newLoadMeterFn = func() loadMeter {
		return fakeMeter{loadSample{CPUs: 8, Load1: 14, CPUSome: 5, MemSome: 0, IOFull: 0, MemFree: 0.6, Busy: 8, Own: 1, measured: true}}
	}
	t.Cleanup(func() { newLoadMeterFn = old })
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := e.svc.RegisterRunner(ctx, me, core.RunnerRegistration{Name: "box", AllowedDirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	j1, _ := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("sleep 1", dir)})
	j2, _ := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("true", dir)})
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- runRunner(rctx, &cli{dir: e.dir, out: &out, err: &out}, runnerOpts{Name: "box", Allow: []string{dir}, MaxJobs: 3, PollWait: -1, NoUsageSync: true, NoPrep: true, NoAutoUpdate: true, CacheDir: t.TempDir()})
	}()
	state := func(id int64) string {
		j, _ := e.svc.GetRunnerJob(ctx, me, id, 0)
		return j.State
	}
	waitFor(t, "job 1 to start", 10*time.Second, func() bool { return state(j1.ID) != "queued" })
	if s := state(j2.ID); s != "queued" {
		t.Fatalf("busy machine started a second heavy job (%s)", s)
	}
	rs, _ := e.svc.ListRunners(ctx, me, false)
	if c := rs[0].Capacity; c == nil || c.Level != "busy" || c.Limit != 1 || c.Max != 3 || !strings.Contains(c.Reason, "load 1.8 per CPU") {
		t.Errorf("capacity = %+v", c)
	}
	waitFor(t, "job 2 to run after job 1", 15*time.Second, func() bool { return state(j2.ID) == "done" })
	if !strings.Contains(out.String(), "Machine busy (load 1.8 per CPU)") {
		t.Errorf("runner said %q", out.String())
	}
	stop()
	<-done
}

// A command a person runs from a request doesn't wait behind an agent's job
// while the machine is overloaded; another agent job does (WP-503).
func TestRunnerRunsARequestsCommandWhileOverloaded(t *testing.T) {
	e := newEnv(t)
	me := e.runnerUser("robin")
	old := newLoadMeterFn
	newLoadMeterFn = func() loadMeter {
		return fakeMeter{loadSample{CPUs: 8, Load1: 4, CPUSome: 58, MemSome: 0, IOFull: 0, MemFree: 0.6, Busy: 8, Own: 1, measured: true}}
	}
	t.Cleanup(func() { newLoadMeterFn = old })
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := e.svc.RegisterRunner(ctx, me, core.RunnerRegistration{Name: "box", AllowedDirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	long, _ := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("sleep 30", dir), Source: "babysit"})
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- runRunner(rctx, &cli{dir: e.dir, out: &out, err: &out}, runnerOpts{Name: "box", Allow: []string{dir}, MaxJobs: 3, PollWait: -1, NoUsageSync: true, NoPrep: true, NoAutoUpdate: true, CacheDir: t.TempDir()})
	}()
	state := func(id int64) string {
		j, _ := e.svc.GetRunnerJob(ctx, me, id, 0)
		return j.State
	}
	waitFor(t, "the long job to start", 10*time.Second, func() bool { return state(long.ID) == "running" })
	agent, _ := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("true", dir), Source: "autopilot"})
	req, _ := e.svc.EnqueueRunnerJob(ctx, me, core.RunnerJobInput{Kind: core.JobCommand, Input: cmdJobInput("true", dir), Source: "request_cell"})
	waitFor(t, "the request's command to run", 15*time.Second, func() bool { return state(req.ID) == "done" })
	if s := state(agent.ID); s != "queued" {
		t.Fatalf("overloaded machine started another agent job (%s)", s)
	}
	j, _ := e.svc.GetRunnerJob(ctx, me, agent.ID, 0)
	if !strings.Contains(j.Waiting, "box is overloaded (tasks waited for a CPU 58% of the time), so it runs one job at a time; it is running a babysit round.") || !strings.Contains(j.Waiting, "This job is next.") {
		t.Errorf("waiting = %q", j.Waiting)
	}
	stop()
	<-done
}
