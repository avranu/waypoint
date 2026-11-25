package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// stallRoute is a gateway whose review route serves its models in order,
// each stalling (after its stream started) unless the request leaves it out.
type stallRoute struct {
	*scriptLLM
	order []string
	reqs  [][]string // the exclude list of each request, in order
	rmu   sync.Mutex
}

func (l *stallRoute) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	ex := slices.Clone(gatewayExcludeOf(ctx))
	l.rmu.Lock()
	l.reqs = append(l.reqs, ex)
	l.rmu.Unlock()
	for _, m := range l.order {
		if !slices.Contains(ex, m) {
			return nil, m, context.DeadlineExceeded
		}
	}
	return l.scriptLLM.Complete(ctx, p, model, system, prompt, schema)
}

func (l *stallRoute) CompleteWithFallbacks(ctx context.Context, p *AIProvider, model string, _ []string, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	return l.Complete(ctx, p, model, system, prompt, schema)
}

func (l *stallRoute) requests() [][]string {
	l.rmu.Lock()
	defer l.rmu.Unlock()
	return slices.Clone(l.reqs)
}

const (
	stallA = "cursor/grok-4.7-medium"
	stallB = "moonshot/kimi-k3.1-pro"
)

func newStallEnv(t *testing.T, order ...string) (*reviewEnv, *stallRoute, []ChainModel, *time.Time) {
	t.Helper()
	re := newReviewEnv(t)
	re.asGateway(t)
	re.llm.answers[gatewayReviewRoute] = loginFinding
	l := &stallRoute{scriptLLM: re.llm, order: order}
	re.s.LLM = l
	chain, err := re.s.taskChain(re.ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	re.s.SetClock(func() time.Time { return now })
	chain = slices.Clone(chain[:1])
	chain[0].Model = gatewayReviewRoute // the chain asks the route, as production's does
	return re, l, chain, &now
}

// A stall is remembered across parts and runs, ends with the cool-down, and
// a replacement that also stalls is left out as well (WP-1768).
func TestReviewCooldownAfterStall(t *testing.T) {
	re, l, chain, now := newStallEnv(t, stallA, stallB)
	ask := func() reviewerAnswer {
		a := re.s.reviewOnce(re.ctx, chain, "the diff")
		if a.err != nil || len(a.findings) != 1 {
			t.Fatalf("answer = %+v", a)
		}
		return a
	}
	has := func(ex []string, m string) bool { return slices.Contains(ex, m) }

	// Chunk 1: A stalls, and the one re-ask without A stalls on B, which
	// ends that part as today but is remembered too.
	if a := re.s.reviewOnce(re.ctx, chain, "the diff"); a.err == nil {
		t.Fatalf("answer = %+v", a)
	}
	r := l.requests()
	if len(r) != 2 || has(r[0], stallA) || !has(r[1], stallA) {
		t.Fatalf("first part's requests = %v", r)
	}
	// Chunk 2 (and a later run, which is the same call): both start without A and B.
	for range 2 {
		before := len(l.requests())
		a := ask()
		r = l.requests()[before:]
		if len(r) != 1 || !has(r[0], stallA) || !has(r[0], stallB) || len(a.log) != 1 {
			t.Fatalf("later requests = %v, attempts %+v", r, a.log)
		}
	}
	// The cool-down passes: A is asked again.
	*now = now.Add(modelCooldown + time.Minute)
	before := len(l.requests())
	re.s.reviewOnce(re.ctx, chain, "the diff")
	r = l.requests()[before:]
	if len(r) != 2 || has(r[0], stallA) || has(r[0], stallB) {
		t.Fatalf("after the cool-down = %v", r)
	}
}

// Reviewer 0's exclusion and the cool-down's are both in the request.
func TestReviewCooldownKeepsOtherExclusions(t *testing.T) {
	re, l, chain, _ := newStallEnv(t, stallA)
	re.s.reviewOnce(re.ctx, chain, "the diff") // A stalls once
	before := len(l.requests())
	re.s.reviewOnce(withGatewayExclude(re.ctx, []string{museModel + "-other"}), chain, "the diff")
	r := l.requests()[before:]
	if len(r) != 1 || !slices.Contains(r[0], stallA) || !slices.Contains(r[0], museModel+"-other") {
		t.Fatalf("requests = %v", r)
	}
}

// hookRoute is a gateway whose answer to each request is up to fn, which gets
// the request's exclude list and bar and answers (served, err); a nil err
// serves the scripted answer.
type hookRoute struct {
	*scriptLLM
	fn   func(ex []string, bar string, n int) (string, error)
	reqs [][]string
	bars []string
	rmu  sync.Mutex
}

func (l *hookRoute) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	ex := slices.Clone(gatewayExcludeOf(ctx))
	bar := ""
	if g := gatewayAskOf(ctx); g != nil {
		bar = g.weakest
	}
	l.rmu.Lock()
	l.reqs = append(l.reqs, ex)
	l.bars = append(l.bars, bar)
	n := len(l.reqs)
	l.rmu.Unlock()
	if served, err := l.fn(ex, bar, n); err != nil {
		return nil, served, err
	}
	return l.scriptLLM.Complete(ctx, p, model, system, prompt, schema)
}

func (l *hookRoute) CompleteWithFallbacks(ctx context.Context, p *AIProvider, model string, _ []string, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	return l.Complete(ctx, p, model, system, prompt, schema)
}

func (l *hookRoute) requests() [][]string {
	l.rmu.Lock()
	defer l.rmu.Unlock()
	return slices.Clone(l.reqs)
}

// noModelAtBar is what the gateway answers when no model is at the bar.
func noModelAtBar() error {
	return &llmError{status: 503, body: `{"error":{"message":"no model at the bar","type":"api_error","code":"no_model_at_bar"}}`}
}

// newHookEnv is newStallEnv with a gateway answering as fn says, and a bar on
// the review's requests as a round's carry.
func newHookEnv(t *testing.T, fn func(ex []string, bar string, n int) (string, error)) (*reviewEnv, *hookRoute, []ChainModel, context.Context) {
	t.Helper()
	re, l, chain, _ := newStallEnv(t)
	h := &hookRoute{scriptLLM: l.scriptLLM, fn: fn}
	re.s.LLM = h
	ctx := withGatewayRouting(re.ctx, &gatewayRouting{prefix: "t", paid: "allowed", weakest: reviewFloorBar})
	return re, h, chain, ctx
}

func coolA(re *reviewEnv) {
	re.s.coolModel(re.ctx, re.s.gatewayWith(re.ctx, GatewayExclude), gatewayReviewRoute, stallA)
}

// Every model of a route left out: the ask goes out as it would without
// the cool-downs, with its bar, and the slot is answered. no_model_at_bar
// ends the chain's gateway entries, so the re-ask is a new request.
func TestReviewCooldownNeverExcludesAll(t *testing.T) {
	re, h, chain, ctx := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		if slices.Contains(ex, stallA) {
			return "", noModelAtBar()
		}
		return "", nil
	})
	coolA(re)
	a := re.s.reviewOnce(ctx, chain, "the diff")
	r := h.requests()
	if a.err != nil || len(a.findings) != 1 || len(r) != 2 || !slices.Contains(r[0], stallA) || slices.Contains(r[1], stallA) {
		t.Fatalf("answer %+v requests %v", a, r)
	}
	if h.bars[0] != reviewFloorBar || h.bars[1] != reviewFloorBar {
		t.Errorf("bars = %v, want the floor on both", h.bars)
	}
	if len(a.log) != 2 || a.log[0].Outcome != "no_model_at_bar" || a.log[1].Outcome != "ok" {
		t.Errorf("attempts = %+v", a.log)
	}
}

// excludeRemovesAll is the gateway's answer when the exclusions removed every
// model of the route: a 400, no Retry-After.
func excludeRemovesAll() error {
	return &llmError{status: 400, body: `{"error":{"message":"exclude removes every model \"review\" and its fallbacks could use","type":"invalid_request_error","param":null,"code":"invalid_request"}}`}
}

// The cooling models are all the route has: the gateway answers 400, and the
// slot is asked again without them, keeping the first reviewer's exclusion.
func TestReviewCooldownAllCoolingAnswers400(t *testing.T) {
	const zero = "zero/model-0"
	re, h, chain, ctx := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		if slices.Contains(ex, stallA) {
			return "", excludeRemovesAll()
		}
		return "", nil
	})
	coolA(re)
	a := re.s.reviewOnce(withGatewayExclude(ctx, []string{zero}), chain, "the diff")
	r := h.requests()
	if a.err != nil || len(a.findings) != 1 || len(r) != 2 || slices.Contains(r[1], stallA) || !slices.Contains(r[1], zero) {
		t.Fatalf("answer %+v requests %v", a, r)
	}
	if len(a.log) != 2 || a.log[0].Outcome != "error" || a.log[1].Outcome != "ok" {
		t.Errorf("attempts = %+v", a.log)
	}
}

// A no_model_at_bar that carries Retry-After parks the names it asked for, but
// the plain re-ask includes a model it did not try, so it is still sent.
func TestReviewCooldownRetryAfterDoesNotStopReask(t *testing.T) {
	re, h, chain, ctx := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		if slices.Contains(ex, stallA) {
			e := noModelAtBar().(*llmError)
			e.retryAfter = 2 * time.Minute
			return "", e
		}
		return "", nil
	})
	coolA(re)
	a := re.s.reviewOnce(ctx, chain, "the diff")
	if r := h.requests(); a.err != nil || len(a.findings) != 1 || len(r) != 2 {
		t.Fatalf("answer %+v requests %v", a, r)
	}
}

// A bare 503 is not what the gateway answers to exhaustion: not asked again.
func TestReviewCooldownBare503NotReasked(t *testing.T) {
	re, h, chain, _ := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		if slices.Contains(ex, stallA) {
			return "", &llmError{status: 503, body: "no model left"}
		}
		return "", nil
	})
	coolA(re)
	if a := re.s.reviewOnce(re.ctx, chain, "the diff"); a.err == nil || len(h.requests()) != 1 {
		t.Fatalf("answer %+v requests %v", a, h.requests())
	}
}

// The key's daily limit, parked while the cooled ask ran, still stops the
// re-ask.
func TestReviewCooldownQuotaParkStopsReask(t *testing.T) {
	var re *reviewEnv
	re, h, chain, _ := newHookEnv(t, func([]string, string, int) (string, error) {
		re.s.parkAI(re.s.gatewayWith(re.ctx, GatewayExclude), re.s.now().Add(time.Hour))
		return "", excludeRemovesAll()
	})
	coolA(re)
	a := re.s.reviewOnce(re.ctx, chain, "the diff")
	if a.err == nil || len(h.requests()) != 1 {
		t.Fatalf("answer %+v requests %v", a, h.requests())
	}
}

// The WP-1709 block's last ask, without slow alone, also follows a 400.
func TestReviewCooldownFinalReaskOn400(t *testing.T) {
	re, h, chain, _ := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		switch {
		case slices.Contains(ex, stallA) && slices.Contains(ex, stallB):
			return "", excludeRemovesAll()
		case slices.Contains(ex, stallA):
			return stallB, context.DeadlineExceeded
		}
		return "", nil
	})
	coolA(re)
	a := re.s.reviewOnce(re.ctx, chain, "the diff")
	if r := h.requests(); a.err != nil || len(a.findings) != 1 || len(r) != 3 {
		t.Fatalf("answer %+v requests %v", a, r)
	}
}

// An ordinary failure of the cooled ask (a 502, 504, 404) is not asked
// again as if no model were left.
func TestReviewCooldownOrdinaryFailureNotReasked(t *testing.T) {
	for _, status := range []int{502, 504, 404, 500} {
		re, h, chain, _ := newHookEnv(t, func([]string, string, int) (string, error) {
			return "", &llmError{status: status, body: "nope"}
		})
		coolA(re)
		a := re.s.reviewOnce(re.ctx, chain, "the diff")
		if a.err == nil || len(h.requests()) != 1 {
			t.Errorf("status %d: err %v, requests %v", status, a.err, h.requests())
		}
	}
}

// A mid-stream failure that is an answer's fault (an empty or unreadable
// answer, outcome error) is asked again without that model as before, but
// nothing is remembered.
func TestReviewCooldownIgnoresErrorOutcome(t *testing.T) {
	re, h, chain, _ := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		if !slices.Contains(ex, stallA) {
			return stallA, errors.New("empty answer")
		}
		return "", nil
	})
	a := re.s.reviewOnce(re.ctx, chain, "the diff")
	r := h.requests()
	if a.err != nil || len(r) != 2 || !slices.Contains(r[1], stallA) {
		t.Fatalf("answer %+v requests %v", a, r)
	}
	if a.log[0].Outcome != "error" {
		t.Fatalf("attempts = %+v", a.log)
	}
	if m, _ := re.s.coolingModels(re.s.gatewayWith(re.ctx, GatewayExclude), gatewayReviewRoute); len(m) != 0 {
		t.Errorf("cooling = %v", m)
	}
}

// A is cooling, B stalls on the ask without A, the one without A and B
// finds no model, and the last ask without B alone is served by A: WP-1709
// as it was before the cool-down. Every attempt is kept.
func TestReviewCooldownFinalReaskWithoutCooled(t *testing.T) {
	re, h, chain, _ := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		switch {
		case slices.Contains(ex, stallA) && slices.Contains(ex, stallB):
			return "", noModelAtBar()
		case slices.Contains(ex, stallA):
			return stallB, context.DeadlineExceeded
		}
		return "", nil // B left out, A (cooling) serves
	})
	coolA(re)
	a := re.s.reviewOnce(re.ctx, chain, "the diff")
	r := h.requests()
	if a.err != nil || len(a.findings) != 1 || len(r) != 3 {
		t.Fatalf("answer %+v requests %v", a, r)
	}
	if !slices.Contains(r[0], stallA) || slices.Contains(r[0], stallB) ||
		!slices.Contains(r[1], stallA) || !slices.Contains(r[1], stallB) ||
		slices.Contains(r[2], stallA) || !slices.Contains(r[2], stallB) {
		t.Errorf("requests = %v", r)
	}
	if len(a.log) != 3 || a.log[0].Outcome != "timeout" || a.log[1].Outcome != "no_model_at_bar" || a.log[2].Outcome != "ok" {
		t.Errorf("attempts = %+v", a.log)
	}
}

// The re-asks without a model keep what the first reviewer's model excluded,
// the plain one too.
func TestReviewCooldownPlainReaskKeepsReviewerZeroExclusion(t *testing.T) {
	const zero = "zero/model-0"
	re, h, chain, _ := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		if slices.Contains(ex, stallA) {
			return "", noModelAtBar()
		}
		return "", nil
	})
	coolA(re)
	a := re.s.reviewOnce(withGatewayExclude(re.ctx, []string{zero}), chain, "the diff")
	r := h.requests()
	if a.err != nil || len(r) != 2 {
		t.Fatalf("answer %+v requests %v", a, r)
	}
	for n, ex := range r {
		if !slices.Contains(ex, zero) {
			t.Errorf("request %d lost the first reviewer's exclusion: %v", n, ex)
		}
	}
	if slices.Contains(r[1], stallA) {
		t.Errorf("the plain re-ask still excludes A: %v", r[1])
	}
}

// With too many patterns for one request, the first reviewer's and the
// stalled model's stay, and the cool-downs are what is cut.
func TestReviewCooldownTruncationDropsCooledFirst(t *testing.T) {
	re, h, chain, _ := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		if !slices.Contains(ex, stallA) {
			return stallA, context.DeadlineExceeded
		}
		return "", nil
	})
	var zero []string
	for n := range 14 { // two patterns each
		zero = append(zero, "zero/model-"+itoa64(int64(n)))
	}
	pid := re.s.gatewayWith(re.ctx, GatewayExclude)
	re.s.coolModel(re.ctx, pid, gatewayReviewRoute, "cool/one")
	re.s.coolModel(re.ctx, pid, gatewayReviewRoute, "cool/two")
	re.s.reviewOnce(withGatewayExclude(re.ctx, zero), chain, "the diff")
	r := h.requests()
	if len(r) < 2 {
		t.Fatalf("requests = %v", r)
	}
	last := r[1]
	if !slices.Contains(last, stallA) || !slices.Contains(last, zero[0]) || !slices.Contains(last, zero[len(zero)-1]) || len(last) > maxExcludes {
		t.Errorf("the re-ask = %v", last)
	}
}

// A run cancelled mid-ask arms no cool-down and asks nothing again.
func TestReviewCooldownCancelledArmsNothing(t *testing.T) {
	var cancel context.CancelFunc
	re, h, chain, _ := newHookEnv(t, func([]string, string, int) (string, error) {
		cancel()
		return stallA, context.DeadlineExceeded
	})
	ctx, c := context.WithCancel(re.ctx)
	cancel = c
	defer c()
	a := re.s.reviewOnce(ctx, chain, "the diff")
	if a.err == nil || len(h.requests()) != 1 {
		t.Fatalf("err %v requests %v", a.err, h.requests())
	}
	if m, _ := re.s.coolingModels(re.s.gatewayWith(re.ctx, GatewayExclude), gatewayReviewRoute); len(m) != 0 {
		t.Errorf("cooling = %v", m)
	}
}

// A chain that names a gateway model is not held off cooling models, and
// its stall arms none.
func TestReviewCooldownSkipsNamedModelChain(t *testing.T) {
	re, h, chain, _ := newHookEnv(t, func(ex []string, _ string, _ int) (string, error) {
		if !slices.Contains(ex, stallA) {
			return stallA, context.DeadlineExceeded
		}
		return "", nil
	})
	re.llm.answers["named/model"] = loginFinding
	chain[0].Model = "named/model"
	pid := re.s.gatewayWith(re.ctx, GatewayExclude)
	re.s.coolModel(re.ctx, pid, gatewayReviewRoute, stallB)
	re.s.reviewOnce(re.ctx, chain, "the diff")
	r := h.requests()
	if len(r) == 0 || slices.Contains(r[0], stallB) {
		t.Fatalf("a named model's request left out a cooling one: %v", r)
	}
	if m, _ := re.s.coolingModels(pid, gatewayReviewRoute); !slices.Equal(m, []string{stallB}) {
		t.Errorf("cooling = %v, want only what was armed by hand", m)
	}
}

// Only a stall that identifies a model arms a cool-down; other failures,
// other routes and other providers are untouched.
func TestReviewCooldownOnlyForStalls(t *testing.T) {
	re, _, chain, _ := newStallEnv(t)
	pid := re.s.gatewayWith(re.ctx, GatewayExclude)
	for name, answers := range map[string]string{"rate limit": "!429", "plain timeout": "!timeout"} {
		re.llm.answers[gatewayReviewRoute] = answers
		re.s.reviewOnce(re.ctx, chain, "the diff")
		if m, _ := re.s.coolingModels(pid, gatewayReviewRoute); len(m) != 0 {
			t.Errorf("%s armed %v", name, m)
		}
	}
	re.s.coolModel(re.ctx, pid, gatewayReviewRoute, stallA)
	if m, _ := re.s.coolingModels(pid, gatewayVerifyRoute); len(m) != 0 {
		t.Errorf("the verifier's route cools %v", m)
	}
	if m, _ := re.s.coolingModels(pid+1, gatewayReviewRoute); len(m) != 0 {
		t.Errorf("another provider cools %v", m)
	}
	if m, _ := re.s.coolingModels(pid, gatewayReviewRoute); !slices.Equal(m, []string{stallA}) {
		t.Errorf("cooling = %v", m)
	}
}

// Parallel reviewers share the cool-downs: writes, expiry and reads at once.
func TestReviewCooldownConcurrent(t *testing.T) {
	re, l, chain, now := newStallEnv(t, stallA)
	re.s.reviewOnce(re.ctx, chain, "the diff")
	pid := re.s.gatewayWith(re.ctx, GatewayExclude)
	var clock sync.Mutex
	re.s.SetClock(func() time.Time { clock.Lock(); defer clock.Unlock(); return *now })
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				re.s.reviewOnce(re.ctx, chain, "the diff") // may or may not meet A cooling
				re.s.coolingModels(pid, gatewayReviewRoute)
			}
		}()
	}
	for n := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range 50 {
				re.s.coolModel(re.ctx, pid, gatewayReviewRoute, "cool/"+itoa64(int64(n*100+k%5)))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			clock.Lock()
			*now = now.Add(modelCooldown / 4)
			clock.Unlock()
			re.s.coolingModels(pid, gatewayReviewRoute)
		}
	}()
	wg.Wait()
	_ = l
	// Settled, a fresh stall is remembered exactly once per model.
	re.s.coolModel(re.ctx, pid, gatewayReviewRoute, stallB)
	if m, _ := re.s.coolingModels(pid, gatewayReviewRoute); !slices.Contains(m, stallB) {
		t.Errorf("cooling = %v", m)
	}
}

// A stall is remembered when a later entry went on to answer or time out, and
// only for the gateway's own choice (WP-1768).
func TestCoolStallsEarlierAttemptsOnly(t *testing.T) {
	re, _, chain, _ := newStallEnv(t)
	const gw, other = 7, 8
	attempts := []ModelAttempt{
		{Model: stallA, Outcome: "timeout", providerID: gw},
		{Model: "opencode/muse-spark-1", Outcome: "timeout", providerID: other}, // a pattern resolved elsewhere
		{Model: stallB, Outcome: "bad_output", providerID: gw},                  // not a stall
		{Model: gatewayReviewRoute, Outcome: "timeout", providerID: gw},         // the chain's own name
		{Model: "x/ok", Outcome: "ok", providerID: other},
	}
	re.s.coolStalls(re.ctx, gw, gatewayReviewRoute, chain, attempts)
	if got, _ := re.s.coolingModels(gw, gatewayReviewRoute); !slices.Equal(got, []string{stallA}) {
		t.Fatalf("cooling = %v", got)
	}
}

// A no-model answer anywhere in the attempts is seen, not only the last.
func TestLeftNoModelAnyAttempt(t *testing.T) {
	_, _, chain, _ := newStallEnv(t)
	const gw = 7
	a := []ModelAttempt{{Outcome: "no_model_at_bar", providerID: gw}, {Model: "x/y", Outcome: "ok", providerID: 8}}
	if !leftNoModel(gw, chain, a) {
		t.Fatal("not seen")
	}
	if leftNoModel(gw, chain, []ModelAttempt{{Outcome: "server", Error: "HTTP 504", providerID: gw}}) {
		t.Fatal("a 504 is not no model")
	}
}

// The gateway entry answers 400 and a later chain entry answers in its place:
// the route is still asked plain, and the answer already had is kept if that
// fails (WP-1768).
func TestReviewCooldownReasksAfterLaterEntryAnswered(t *testing.T) {
	re, h, _, _ := newHookEnv(t, func(ex []string, _ string, n int) (string, error) {
		if n == 1 {
			return "", excludeRemovesAll()
		}
		return "", nil
	})
	full, err := re.s.taskChain(re.ctx, "review")
	if err != nil || len(full) < 2 {
		t.Fatalf("chain %v, err %v", full, err)
	}
	full = slices.Clone(full)
	full[0].Model = gatewayReviewRoute
	coolA(re)
	a := re.s.reviewOnce(re.ctx, full, "the diff")
	r := h.requests()
	if a.err != nil || len(a.findings) != 1 || len(r) < 3 || slices.Contains(r[len(r)-1], stallA) {
		t.Fatalf("answer %+v requests %v", a, r)
	}
}

// A 400 from a provider other than the gateway is that provider's own, not
// the gateway's no-model answer: the route is not asked plain again.
func TestReviewCooldownOtherProvider400NotReasked(t *testing.T) {
	re, h, chain, _ := newHookEnv(t, func(_ []string, _ string, n int) (string, error) {
		if n == 1 {
			return "", &llmError{status: 502, body: "bad gateway"}
		}
		return "", &llmError{status: 400, body: "bad request"}
	})
	other, err := re.s.CreateAIProvider(re.ctx, re.admin, AIProviderInput{Name: str("Other"), Kind: str("openai"), BaseURL: str("http://other.invalid")})
	if err != nil {
		t.Fatal(err)
	}
	chain = append(slices.Clone(chain), ChainModel{ProviderID: other.ID, Model: "other/model"})
	coolA(re)
	a := re.s.reviewOnce(re.ctx, chain, "the diff")
	if r := h.requests(); a.err == nil || len(r) != 2 {
		t.Fatalf("answer %+v requests %v", a, r)
	}
}

func TestLeftNoModelProviders(t *testing.T) {
	_, _, chain, _ := newStallEnv(t)
	const gw, other = 7, 8
	for _, c := range []struct {
		name string
		a    ModelAttempt
		want bool
	}{
		{"400 from the gateway", ModelAttempt{Model: gatewayReviewRoute, Outcome: "error", status: 400, providerID: gw}, true},
		{"400 from another provider", ModelAttempt{Model: gatewayReviewRoute, Outcome: "error", status: 400, providerID: other}, false},
		{"no_model_at_bar from another provider", ModelAttempt{Outcome: "no_model_at_bar", providerID: other}, false},
		{"no_model_at_bar from the gateway", ModelAttempt{Outcome: "no_model_at_bar", providerID: gw}, true},
	} {
		if got := leftNoModel(gw, chain, []ModelAttempt{c.a}); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	mixed := []ModelAttempt{{Outcome: "no_model_at_bar", providerID: gw}, {Model: "x/y", Outcome: "ok", providerID: other}}
	if !leftNoModel(gw, chain, mixed) {
		t.Error("the gateway's no-model answer among other attempts is not seen")
	}
}
