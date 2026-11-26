package core

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// notesGate holds the notes read of one pull request, which a sweep's step
// makes when it syncs, and counts the reads of each.
type notesGate struct {
	mu      sync.Mutex
	reads   map[int]int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// holdNotes makes the forge's notes read of pull request hold wait until
// open is called. entered gets a value each time the held read is reached.
func holdNotes(be *bsEnv, hold int) *notesGate {
	g := &notesGate{reads: map[int]int{}, entered: make(chan struct{}, 16), release: make(chan struct{})}
	be.sg.notesHold = func(n int) {
		g.mu.Lock()
		g.reads[n]++
		g.mu.Unlock()
		if n == hold {
			g.entered <- struct{}{}
			<-g.release
		}
	}
	return g
}

func (g *notesGate) open() { g.once.Do(func() { close(g.release) }) }

func (g *notesGate) readsOf(n int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads[n]
}

// await fails the test unless the held read is reached soon.
func (g *notesGate) await(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the held step was never reached", what)
	}
}

// within runs f in a goroutine and fails the test unless it returns soon,
// so a call that blocks cannot hang the test binary.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

// inOrder makes the sweep step the first pull request first.
func inOrder(t *testing.T, be *bsEnv, prs [2]*PullRequest) {
	t.Helper()
	for i, pr := range prs {
		if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET updated_at = ? WHERE pr_id = ?`, 1000+i, pr.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func (be *bsEnv) sweepRuns() int64 {
	be.s.babysitSweep.mu.Lock()
	defer be.s.babysitSweep.mu.Unlock()
	return be.s.babysitSweep.n
}

func (be *bsEnv) sweepHeld() int64 {
	be.s.babysitSweep.mu.Lock()
	defer be.s.babysitSweep.mu.Unlock()
	return be.s.babysitSweep.run
}

// A sweep says which step was slow and, when the sweep was, how many steps
// ran and which three took longest (WP-1944).
func TestCheckBabysitsLogsSlowSteps(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	logs := &syncBuffer{}
	be.s.log = slog.New(slog.NewTextHandler(logs, nil))
	be.s.CheckBabysits(be.ctx)
	if got := logs.String(); strings.Contains(got, "slow") {
		t.Fatalf("a quick sweep logged: %s", got)
	}
	inOrder(t, be, prs)
	be.pass(babysitResyncEvery + time.Minute)
	g := holdNotes(be, prs[0].Number)
	defer g.open()
	done := make(chan struct{})
	go func() {
		defer close(done)
		be.s.CheckBabysits(be.ctx)
	}()
	g.await(t, "the sweep")
	be.pass(48 * time.Second)
	g.open()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep did not end")
	}
	got := logs.String()
	for _, want := range []string{"babysit step slow", "took=48s", "babysit sweep slow", "steps=2", "total=48s", "slowest=", fmt.Sprintf("%s#%d 48s", prs[0].Repo, prs[0].Number)} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in: %s", want, got)
		}
	}
	if other := fmt.Sprintf("%s#%d 48s", prs[1].Repo, prs[1].Number); strings.Contains(got, other) {
		t.Errorf("the quick step is named as slow: %s", got)
	}
	if g.readsOf(prs[1].Number) != 1 {
		t.Errorf("the second pull request's notes were read %d times", g.readsOf(prs[1].Number))
	}
}

// A sweep that is slow but moving is not joined by a second one when the
// scheduler counts its run as stuck; one that has stopped moving is taken
// over from, without waiting for the pull request it stands in, and stops
// when that step returns.
func TestBabysitSweepGivesWayToOneStillMoving(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	logs := &syncBuffer{}
	be.s.log = slog.New(slog.NewTextHandler(logs, nil))
	inOrder(t, be, prs)
	be.pass(babysitResyncEvery + time.Minute)
	g := holdNotes(be, prs[0].Number)
	defer g.open()
	first := make(chan struct{})
	go func() {
		defer close(first)
		be.s.CheckBabysits(be.ctx)
	}()
	g.await(t, "the first sweep")

	be.pass(babysitSweepAlive - time.Second)
	within(t, "a sweep beside one still moving", func() { be.s.CheckBabysits(be.ctx) })
	if g.readsOf(prs[1].Number) != 0 || be.sweepRuns() != 1 {
		t.Fatalf("a second sweep beside one still moving stepped: reads %d, sweeps %d", g.readsOf(prs[1].Number), be.sweepRuns())
	}

	be.pass(2 * time.Second)
	within(t, "the sweep that takes over", func() { be.s.CheckBabysits(be.ctx) })
	if be.sweepRuns() != 2 || be.sweepHeld() != 0 {
		t.Fatalf("the takeover: sweeps %d, mark %d", be.sweepRuns(), be.sweepHeld())
	}
	if g.readsOf(prs[1].Number) != 1 {
		t.Fatalf("the sweep that took over did not step the second pull request: %d", g.readsOf(prs[1].Number))
	}

	within(t, "a sweep after the takeover ended", func() { be.s.CheckBabysits(be.ctx) })
	if be.sweepRuns() != 3 {
		t.Errorf("no sweep started after the takeover ended: %d", be.sweepRuns())
	}

	g.open()
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("the first sweep did not end")
	}
	got := logs.String()
	if n := strings.Count(got, "babysit sweep stopped: another took over"); n != 1 || !strings.Contains(got, "steps=1") {
		t.Errorf("the sweep taken over from should stop after the step it stood in, once, having taken one step: %s", got)
	}
	if n := g.readsOf(prs[1].Number); n != 1 {
		t.Errorf("the sweep taken over from stepped the second pull request: %d reads", n)
	}
	if run := be.sweepHeld(); run != 0 {
		t.Errorf("the mark is still held after the sweeps ended: %d", run)
	}
}

// A pull request whose lock something else holds is passed over by a sweep.
func TestBabysitSweepPassesOverALockedPullRequest(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	inOrder(t, be, prs)
	be.pass(babysitResyncEvery + time.Minute)
	g := holdNotes(be, 0)
	unlock := be.s.babysitLocks.lock(prs[0].ID)
	defer unlock()
	within(t, "a sweep past a locked pull request", func() { be.s.CheckBabysits(be.ctx) })
	if g.readsOf(prs[0].Number) != 0 || g.readsOf(prs[1].Number) != 1 {
		t.Errorf("reads: locked %d, other %d", g.readsOf(prs[0].Number), g.readsOf(prs[1].Number))
	}
	if be.sweepHeld() != 0 {
		t.Errorf("the mark is held after the sweep: %d", be.sweepHeld())
	}
}

// A sweep that gives way to one still moving did none of its work: the job
// counts it as skipped, not as a clean run.
func TestBabysitSweepThatGivesWayIsABusySkip(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	inOrder(t, be, prs)
	be.pass(babysitResyncEvery + time.Minute)
	g := holdNotes(be, prs[0].Number)
	defer g.open()
	first := make(chan struct{})
	go func() {
		defer close(first)
		be.s.CheckBabysits(be.ctx)
	}()
	g.await(t, "the sweep")
	const calls = 3
	for range calls {
		within(t, "a sweep that gives way", func() { be.s.untracedJob(be.ctx, "babysits", time.Minute, be.s.CheckBabysits) })
	}
	j := be.jobOf("babysits")
	if j.Runs != 0 || j.Skips != calls || j.LastResult != jobSkipped {
		t.Errorf("babysits after giving way %d times: runs %d, skips %d, last %q", calls, j.Runs, j.Skips, j.LastResult)
	}
	if _, ok := be.s.jobs.busySinces()["babysits"]; !ok {
		t.Errorf("babysits is not busy: %v", be.s.jobs.busySinces())
	}
	g.open()
	<-first
}

// Batched news that is due goes out by its own job while a babysit sweep is
// held in a step.
func TestBatchedNewsIsReleasedWhileASweepIsHeld(t *testing.T) {
	be, prs, bs := twoBabysits(t)
	bs[0].Blocking = true
	be.pass(10 * time.Minute)
	be.s.babysitTell(be.ctx, bs[0], prs[0], "gave_up", "Waypoint gave up babysitting o/r#7.")
	be.pass(batchBlockingWait + babysitResyncEvery + time.Second)
	be.runner(be.admin, "m1", nil)
	inOrder(t, be, prs)
	g := holdNotes(be, prs[0].Number)
	defer g.open()
	first := make(chan struct{})
	go func() {
		defer close(first)
		be.s.CheckBabysits(be.ctx)
	}()
	g.await(t, "the sweep")
	within(t, "the release of batched news", func() { be.s.releaseDueBatches(be.ctx) })
	if jobs := be.messageJobs(); len(jobs) != 1 || !strings.Contains(jobs[0], "o/r#7") {
		t.Errorf("a due batch while the sweep is held: %v", jobs)
	}
	g.open()
	<-first
}
