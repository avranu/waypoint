package core

import (
	"context"
	"encoding/json"
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
	re.llm.answers[museModel] = loginFinding
	l := &stallRoute{scriptLLM: re.llm, order: order}
	re.s.LLM = l
	chain, err := re.s.taskChain(re.ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	re.s.SetClock(func() time.Time { return now })
	chain = slices.Clone(chain[:1])
	chain[0].Model = museModel // the name the fake serves, as a route's would be unknown to it
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

// Every model of a route left out: the ask goes out as it would without
// the cool-downs.
type emptyRoute struct {
	*stallRoute
}

func (l *emptyRoute) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	ex := slices.Clone(gatewayExcludeOf(ctx))
	l.rmu.Lock()
	l.reqs = append(l.reqs, ex)
	l.rmu.Unlock()
	if slices.Contains(ex, stallA) {
		return nil, "", &llmError{status: 503, body: "no model left"}
	}
	return l.scriptLLM.Complete(ctx, p, model, system, prompt, schema)
}

func (l *emptyRoute) CompleteWithFallbacks(ctx context.Context, p *AIProvider, model string, _ []string, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	return l.Complete(ctx, p, model, system, prompt, schema)
}

func TestReviewCooldownNeverExcludesAll(t *testing.T) {
	re, l, chain, _ := newStallEnv(t)
	re.s.coolModel(re.ctx, re.s.gatewayWith(re.ctx, GatewayExclude), gatewayReviewRoute, stallA)
	e := &emptyRoute{l}
	re.s.LLM = e
	a := re.s.reviewOnce(re.ctx, chain, "the diff")
	r := e.requests()
	if a.err != nil || len(r) != 2 || !slices.Contains(r[0], stallA) || slices.Contains(r[1], stallA) {
		t.Fatalf("answer %+v requests %v", a, r)
	}
}

// Only a stall that identifies a model arms a cool-down; other failures,
// other routes and other providers are untouched.
func TestReviewCooldownOnlyForStalls(t *testing.T) {
	re, _, chain, _ := newStallEnv(t)
	pid := re.s.gatewayWith(re.ctx, GatewayExclude)
	for name, answers := range map[string]string{"rate limit": "!429", "plain timeout": "!timeout"} {
		re.llm.answers[museModel] = answers
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

// Parallel reviewers share the cool-downs.
func TestReviewCooldownConcurrent(t *testing.T) {
	re, l, chain, _ := newStallEnv(t, stallA)
	re.s.reviewOnce(re.ctx, chain, "the diff")
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a := re.s.reviewOnce(re.ctx, chain, "the diff"); a.err != nil {
				t.Error(a.err)
			}
		}()
	}
	wg.Wait()
	for _, r := range l.requests()[2:] {
		if !slices.Contains(r, stallA) {
			t.Errorf("request without the cooling model: %v", r)
		}
	}
}
