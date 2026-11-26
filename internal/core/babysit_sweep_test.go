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

// ended fails the test unless done is closed soon.
func ended(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not end", what)
	}
}

// alsoBabysit opens one more pull request and starts a babysit of it.
func (be *bsEnv) alsoBabysit(n int, head string) *PullRequest {
	be.t.Helper()
	p := bsPR
	p.Number, p.HeadRef, p.HeadSHA = n, fmt.Sprintf("branch-%d", n), head
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(n)
	be.start(pr)
	be.hookAs("Stop")
	be.s.Wait()
	return pr
}

// inOrder makes the sweep step the pull requests in this order.
func inOrder(t *testing.T, be *bsEnv, prs ...*PullRequest) {
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
// ran and which took longest.
func TestCheckBabysitsLogsSlowSteps(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	logs := &syncBuffer{}
	be.s.log = slog.New(slog.NewTextHandler(logs, nil))
	be.s.CheckBabysits(be.ctx)
	if got := logs.String(); strings.Contains(got, "slow") {
		t.Fatalf("a quick sweep logged: %s", got)
	}
	inOrder(t, be, prs[:]...)
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

// A slow sweep names its three slowest steps, slowest first, and no other.
func TestBabysitSweepNamesItsThreeSlowestSteps(t *testing.T) {
	be, two, _ := twoBabysits(t)
	prs := []*PullRequest{two[0], two[1], be.alsoBabysit(9, "9999999cccc"), be.alsoBabysit(10, "aaaaaaadddd")}
	logs := &syncBuffer{}
	be.s.log = slog.New(slog.NewTextHandler(logs, nil))
	inOrder(t, be, prs...)
	be.pass(babysitResyncEvery + time.Minute)
	took := map[int]time.Duration{7: 10 * time.Second, 8: 40 * time.Second, 9: 3 * time.Second, 10: 20 * time.Second}
	var mu sync.Mutex
	seen := map[int]bool{}
	be.sg.notesHold = func(n int) {
		mu.Lock()
		first := !seen[n]
		seen[n] = true
		mu.Unlock()
		if first {
			be.pass(took[n])
		}
	}
	within(t, "the sweep", func() { be.s.CheckBabysits(be.ctx) })
	got := logs.String()
	repo := prs[0].Repo
	for _, want := range []string{"babysit sweep slow", "steps=4", "total=1m13s", fmt.Sprintf(`slowest="%s#8 40s, %s#10 20s, %s#7 10s"`, repo, repo, repo)} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in: %s", want, got)
		}
	}
	if n := strings.Count(got, "babysit step slow"); n != 3 {
		t.Errorf("%d steps logged as slow, want the 3 that took %s or more: %s", n, babysitStepSlow, got)
	}
}

// A sweep another takes over from between steps starts no further step.
func TestBabysitSweepTakenOverBetweenStepsStartsNone(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	logs := &syncBuffer{}
	be.s.log = slog.New(slog.NewTextHandler(logs, nil))
	be.pass(babysitResyncEvery + time.Minute)
	g := holdNotes(be, 0)
	defer g.open()
	// The first time the sweep reads the clock with the mark its own, which
	// is before its first step, the time for it passes and another sweep
	// takes the mark.
	var once sync.Once
	var other int64
	var in string
	var stood time.Duration
	be.s.SetClock(func() time.Time {
		if be.sweepHeld() != 0 {
			once.Do(func() {
				be.pass(babysitSweepAlive)
				other, in, stood = be.s.babysitSweep.begin(be.clockNow(), babysitSweepAlive)
			})
		}
		return be.clockNow()
	})
	within(t, "the sweep taken over from", func() { be.s.CheckBabysits(be.ctx) })
	be.s.SetClock(be.clockNow)
	if other == 0 || be.sweepHeld() != other || in != "" || stood < babysitSweepAlive {
		t.Fatalf("the takeover: run %d, mark %d, in %q, stood %s", other, be.sweepHeld(), in, stood)
	}
	for _, pr := range prs {
		if n := g.readsOf(pr.Number); n != 0 {
			t.Errorf("the sweep taken over from stepped #%d: %d reads", pr.Number, n)
		}
		unlock, free := be.s.babysitLocks.tryLock(pr.ID)
		if !free {
			t.Fatalf("the sweep taken over from kept the lock of #%d", pr.Number)
		}
		unlock()
	}
	got := logs.String()
	if n := strings.Count(got, "babysit sweep stopped: another took over"); n != 1 || !strings.Contains(got, "steps=0") {
		t.Errorf("the sweep taken over from should say once that it stopped, with no step taken: %s", got)
	}
	be.s.babysitSweep.end(other)
}

// A sweep that is slow but moving is not joined by a second one when the
// scheduler counts its run as stuck; one that has stopped moving is taken
// over from, without waiting for the pull request it stands in, and stops
// when that step returns.
func TestBabysitSweepGivesWayToOneStillMoving(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	logs := &syncBuffer{}
	be.s.log = slog.New(slog.NewTextHandler(logs, nil))
	inOrder(t, be, prs[:]...)
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
	if got, want := logs.String(), fmt.Sprintf("in=%s#%d", prs[0].Repo, prs[0].Number); strings.Count(got, "babysit sweep took over from one that finished no step") != 1 || !strings.Contains(got, "for=5m1s") || !strings.Contains(got, want) {
		t.Errorf("the takeover should name the pull request the other sweep stands in (%s) and for how long: %s", want, got)
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
	if want := fmt.Sprintf(`slowest="%s#%d 5m1s"`, prs[0].Repo, prs[0].Number); !strings.Contains(got, "total=5m1s") || !strings.Contains(got, want) {
		t.Errorf("the sweep taken over from should say how long it took and name its step (%s): %s", want, got)
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
	inOrder(t, be, prs[:]...)
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

// A sweep taken over from leaves the mark to the sweep that took over when
// it ends: the next sweep gives way to that one, not to nobody.
func TestBabysitSweepTakenOverFromLeavesTheMark(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	inOrder(t, be, prs[:]...)
	be.pass(babysitResyncEvery + time.Minute)
	entered := map[int]chan struct{}{prs[0].Number: make(chan struct{}, 4), prs[1].Number: make(chan struct{}, 4)}
	release := map[int]chan struct{}{prs[0].Number: make(chan struct{}), prs[1].Number: make(chan struct{})}
	be.sg.notesHold = func(n int) {
		entered[n] <- struct{}{}
		<-release[n]
	}
	open := [2]func(){
		sync.OnceFunc(func() { close(release[prs[0].Number]) }),
		sync.OnceFunc(func() { close(release[prs[1].Number]) }),
	}
	defer open[0]()
	defer open[1]()
	reached := func(i int, what string) {
		t.Helper()
		select {
		case <-entered[prs[i].Number]:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the held step was never reached", what)
		}
	}
	sweep := func() chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			be.s.CheckBabysits(be.ctx)
		}()
		return done
	}
	first := sweep()
	reached(0, "the first sweep")
	be.pass(babysitSweepAlive + time.Second)
	second := sweep()
	reached(1, "the sweep that takes over")
	open[0]()
	ended(t, first, "the sweep taken over from")
	if be.sweepHeld() != 2 {
		t.Fatalf("the mark after the sweep taken over from ended: %d, want the second sweep's", be.sweepHeld())
	}
	within(t, "a sweep beside the one that took over", func() { be.s.CheckBabysits(be.ctx) })
	if be.sweepRuns() != 2 {
		t.Errorf("a sweep started beside the one that took over: %d sweeps", be.sweepRuns())
	}
	open[1]()
	ended(t, second, "the sweep that took over")
	if be.sweepHeld() != 0 {
		t.Errorf("the mark is still held after the sweeps ended: %d", be.sweepHeld())
	}
}

// A step that panics does not keep its pull request's lock: the next step
// for it, a sweep's or an event's, still runs.
func TestBabysitSweepFreesTheLockWhenAStepPanics(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	inOrder(t, be, prs[:]...)
	be.pass(babysitResyncEvery + time.Minute)
	be.sg.notesHold = func(n int) {
		if n == prs[0].Number {
			panic("the forge's client broke")
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the sweep did not panic: the test holds nothing")
			}
		}()
		be.s.CheckBabysits(be.ctx)
	}()
	unlock, free := be.s.babysitLocks.tryLock(prs[0].ID)
	if !free {
		t.Fatal("the pull request's lock is still held after its step panicked")
	}
	unlock()
	if be.sweepHeld() != 0 {
		t.Errorf("the mark is still held after the sweep panicked: %d", be.sweepHeld())
	}
}

// A sweep that gives way to one still moving did none of its work: the job
// counts it as skipped, not as a clean run.
func TestBabysitSweepThatGivesWayIsABusySkip(t *testing.T) {
	be, prs, _ := twoBabysits(t)
	inOrder(t, be, prs[:]...)
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
	ended(t, first, "the held sweep")
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
	inOrder(t, be, prs[:]...)
	g := holdNotes(be, prs[0].Number)
	defer g.open()
	first := make(chan struct{})
	go func() {
		defer close(first)
		be.s.CheckBabysits(be.ctx)
	}()
	g.await(t, "the sweep")
	var news scheduled
	for _, j := range be.s.schedule() {
		if j.name == "batched news" {
			news = j
		}
	}
	if news.run == nil || news.lane != "" {
		t.Fatalf("the batched news job: %+v", news)
	}
	within(t, "the batched news job", func() { news.run(be.ctx) })
	if jobs := be.messageJobs(); len(jobs) != 1 || !strings.Contains(jobs[0], "o/r#7") {
		t.Errorf("a due batch while the sweep is held: %v", jobs)
	}
	g.open()
	ended(t, first, "the held sweep")
}
