package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// goalPassBound is how long quiesceGoals waits before it fails the test,
// naming the pass that still runs, so a pass that never ends fails here
// and not at go test's timeout (WP-1372).
const goalPassBound = 2 * time.Minute

// quiesceGoals waits out the trace, judgement and weekly drift passes
// running now, so a test can swap the model or its answer without a data
// race. A recompute that ends by starting a judgement pass is waited out
// too. goalsSettle gives up after its cap for views; a test can't
// (WP-1372). A back-off's later pass (meaningRetryLater, a minute or more
// away) is cancelled: nothing would order its read of the model after the
// test's swap, and no test waits a minute for it. Safety screens of
// agents' tasks are not waited out: a test sets s.LLM before it makes any
// task and swaps a fakeLLM's answer with set (audit 1117.1).
func (s *Service) quiesceGoals(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), goalPassBound)
	defer cancel()
	if still := s.waitGoalPasses(ctx); still != "" {
		t.Fatalf("%s still runs after %s", still, goalPassBound)
	}
}

// waitGoalPasses waits until no trace, judgement or weekly drift pass
// runs, cancelling a back-off's pending pass each time it looks. If ctx
// ends first it names the pass still running ("": none).
func (s *Service) waitGoalPasses(ctx context.Context) string {
	for {
		var wait chan struct{}
		var what string
		s.goals.mu.Lock()
		s.cancelJudgeTimerLocked()
		switch g := &s.goals; {
		case g.running && g.idle != nil:
			wait, what = g.idle, "a goal trace recompute"
		case g.judging && g.judgeIdle != nil:
			wait, what = g.judgeIdle, "a goal judgement pass"
		case g.drifting && g.driftIdle != nil:
			wait, what = g.driftIdle, "a weekly goal drift run"
		}
		s.goals.mu.Unlock()
		if wait == nil {
			return ""
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return what
		}
	}
}

// goalSettled waits out passes another test may have left, then gives the
// env's Service a short goalsSettle cap.
func goalSettled(t *testing.T, e *env, limit time.Duration) *goalState {
	t.Helper()
	e.s.quiesceGoals(t)
	g := &e.s.goals
	g.mu.Lock()
	g.settleWait = limit
	g.mu.Unlock()
	return g
}

// goalsSettle gives up at its cap while a recompute still runs; a recompute
// that then starts a judgement pass is waited out to the end by
// quiesceGoals (WP-1372).
func TestQuiesceGoalsOutwaitsSettleCap(t *testing.T) {
	e := newEnv(t)
	g := goalSettled(t, e, 20*time.Millisecond)
	recompute, judge, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	g.mu.Lock()
	g.running, g.idle = true, recompute
	g.mu.Unlock()
	// Free the release on every exit, so a failed check does not leave a goroutine blocked on it.
	freed := sync.OnceFunc(func() { close(release) })
	defer freed()
	var over atomic.Bool
	go func() {
		<-release
		g.mu.Lock()
		g.judging, g.judgeIdle = true, judge
		g.running = false
		g.mu.Unlock()
		close(recompute)
		time.Sleep(50 * time.Millisecond)
		over.Store(true) // before the flag clears, so a re-check never sees it idle first
		g.mu.Lock()
		g.judging = false
		g.mu.Unlock()
		close(judge)
	}()

	start := time.Now()
	e.s.goalsSettle(e.ctx)
	// The 1s bound sits far above the 20ms cap, so a loaded race run does not flake.
	if took := time.Since(start); took > time.Second {
		t.Fatalf("goalsSettle took %s, not its 20ms cap", took)
	}
	g.mu.Lock()
	running := g.running
	g.mu.Unlock()
	if !running {
		t.Fatal("goalsSettle returned after the recompute ended, not at its cap")
	}
	freed()
	e.s.quiesceGoals(t)
	if !over.Load() {
		t.Fatal("quiesceGoals returned while a pass still ran")
	}
}

// A pass that never ends is named when the wait gives up, so a stuck
// test fails saying which pass held it (WP-1372).
func TestWaitGoalPassesNamesAPassThatNeverEnds(t *testing.T) {
	e := newEnv(t)
	g := goalSettled(t, e, 0)
	for _, want := range []string{"a goal trace recompute", "a goal judgement pass", "a weekly goal drift run"} {
		never := make(chan struct{})
		g.mu.Lock()
		switch want {
		case "a goal trace recompute":
			g.running, g.idle = true, never
		case "a goal judgement pass":
			g.judging, g.judgeIdle = true, never
		default:
			g.drifting, g.driftIdle = true, never
		}
		g.mu.Unlock()
		ctx, cancel := context.WithTimeout(e.ctx, 20*time.Millisecond)
		got := e.s.waitGoalPasses(ctx)
		cancel()
		g.mu.Lock()
		g.running, g.judging, g.drifting = false, false, false
		g.mu.Unlock()
		close(never)
		if got != want {
			t.Errorf("waitGoalPasses = %q, want %q", got, want)
		}
	}
}

// heldLLM answers at once, except a prompt containing hold, which waits
// until release closes.
type heldLLM struct {
	fakeLLM
	hold    string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	if strings.Contains(prompt, h.hold) {
		h.once.Do(func() { close(h.entered) })
		<-h.release
	}
	return h.fakeLLM.Complete(ctx, p, model, system, prompt, schema)
}

// The weekly drift run asks the model in the background (its goal
// suggestions); quiesceGoals waits until that call has answered and the
// run has ended (WP-1372, audit 1115.2).
func TestQuiesceGoalsWaitsOutTheDriftRun(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Atlas", "")
	e.goal(e.admin, p.Key, "Travellers find a route in one tap")
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Export my trips as a spreadsheet"})
	g := goalSettled(t, e, 0)
	llm := &heldLLM{fakeLLM: fakeLLM{answer: `{"goals":[],"rewordings":[]}`}, hold: "Tasks people wrote that no goal covers yet",
		entered: make(chan struct{}), release: make(chan struct{})}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	freed := sync.OnceFunc(func() { close(llm.release) })
	defer freed()

	e.s.startGoalDrift(e.ctx)
	select {
	case <-llm.entered:
	case <-time.After(goalPassBound):
		t.Fatal("the drift run never asked the model")
	}
	var released atomic.Bool
	go func() {
		time.Sleep(50 * time.Millisecond)
		released.Store(true)
		freed()
	}()
	e.s.quiesceGoals(t)
	if !released.Load() {
		t.Fatal("quiesceGoals returned while the drift run waited on the model")
	}
	g.mu.Lock()
	drifting, idle := g.drifting, g.driftIdle
	g.mu.Unlock()
	select {
	case <-idle:
	default:
		t.Fatalf("quiesceGoals returned with the drift run's channel open (drifting %v)", drifting)
	}
}

// Each pass closes its idle channel in the same hold of the lock that
// clears its flag, so a wait that finds no pass running never finds its
// channel still open (WP-1372, audit 1115.1).
func TestGoalPassClosesItsIdleWithItsFlag(t *testing.T) {
	e := newEnv(t)
	g := goalSettled(t, e, 0)
	passes := []struct {
		name  string
		start func()
		state func() (bool, chan struct{})
	}{
		{"trace recompute", e.s.goalsChanged, func() (bool, chan struct{}) { return g.running, g.idle }},
		{"judgement pass", e.s.judgementsChanged, func() (bool, chan struct{}) { return g.judging, g.judgeIdle }},
		{"weekly drift run", func() { e.s.startGoalDrift(e.ctx) }, func() (bool, chan struct{}) { return g.drifting, g.driftIdle }},
	}
	for _, p := range passes {
		for i := 0; i < 100; i++ {
			p.start()
			for {
				g.mu.Lock()
				on, idle := p.state()
				open := false
				if !on && idle != nil {
					select {
					case <-idle:
					default:
						open = true
					}
				}
				g.mu.Unlock()
				if open {
					t.Fatalf("%s: flag clear while its idle channel is open (run %d)", p.name, i)
				}
				if !on {
					break
				}
			}
			e.s.quiesceGoals(t)
		}
	}
}

// A back-off's later pass is cancelled by quiesceGoals, so it can't read a
// model the test swaps in after; a timer that already fired starts
// nothing once it is cancelled (WP-1372).
func TestQuiesceGoalsCancelsABackOffPass(t *testing.T) {
	e := newEnv(t)
	g := goalSettled(t, e, 0)
	e.s.meaningRetryLater(e.s.nowMS())
	g.mu.Lock()
	gen, armed := g.judgeGen, g.judgeTimer != nil
	g.mu.Unlock()
	if !armed {
		t.Fatal("no back-off pass armed")
	}
	e.s.quiesceGoals(t)
	e.s.judgeTimerFired(gen) // Fires the timer's callback after quiesce with a stale generation, which must start nothing.
	g.mu.Lock()
	timer, judging := g.judgeTimer, g.judging
	g.mu.Unlock()
	if timer != nil || judging {
		t.Fatalf("after quiesceGoals: timer armed %v, judging %v", timer != nil, judging)
	}

	e.s.meaningRetryLater(e.s.nowMS())
	g.mu.Lock()
	gen, before := g.judgeGen, g.judgeIdle
	g.mu.Unlock()
	e.s.judgeTimerFired(gen)
	g.mu.Lock()
	after := g.judgeIdle
	g.mu.Unlock()
	if after == nil || after == before {
		t.Error("a current back-off timer started no pass")
	}
	e.s.quiesceGoals(t)
}
