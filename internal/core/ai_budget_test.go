package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgetGateway is a gateway whose key has a daily request limit (WP-373):
// GET /v1/key says the limit and the day's usage, and once quota is set
// every request answers the daily-limit 429 the real one gives. Requests
// are counted as admitted, whether or not they succeed.
type budgetGateway struct {
	mu      sync.Mutex
	inner   *fakeJev
	rpd     int
	day     int
	quota   bool
	reads   int
	reqs    int
	retryIn string
	busy    string
	jevBusy bool
}

func (b *budgetGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	switch r.URL.Path {
	case "/v1/key":
		b.reads++
		if b.rpd == 0 {
			b.mu.Unlock()
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "key_x", "policy": map[string]any{"limits": map[string]any{"rpm": 60, "rpd": b.rpd}}, "usage": map[string]any{"day_requests": b.day}})
		b.mu.Unlock()
		return
	case "/v1/chat/completions", "/v1/systemone":
		b.reqs++
		b.day++
		if b.busy != "" && (r.URL.Path == "/v1/chat/completions" || b.jevBusy) {
			b.mu.Unlock()
			w.Header().Set("Retry-After", b.busy)
			w.WriteHeader(503)
			w.Write([]byte(`{"error":{"message":"all 5 candidate models are cooling down","type":"api_error","code":"unavailable"}}`))
			return
		}
		if b.quota {
			b.mu.Unlock()
			w.Header().Set("Retry-After", b.retryIn)
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"key:key_x reached its daily request limit (1000)","type":"insufficient_quota","code":"quota"}}`))
			return
		}
	}
	b.mu.Unlock()
	b.inner.ServeHTTP(w, r)
}

func (b *budgetGateway) set(f func(*budgetGateway)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(b)
}

func (b *budgetGateway) requests() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reqs
}

func newBudgetEnv(t *testing.T) (*env, *budgetGateway) {
	t.Helper()
	e := newEnv(t)
	g := &fakeGateway{answer: `{"flags": [], "reason": ""}`}
	bg := &budgetGateway{inner: &fakeJev{chat: g}, retryIn: "43200", rpd: 1000}
	gw := httptest.NewServer(bg)
	t.Cleanup(gw.Close)
	e.s.LLM = gatewayClient()
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	return e, bg
}

var estimateAnswer = func() (out struct {
	Points     *float64 `json:"points"`
	Confidence string   `json:"confidence"`
	Reason     string   `json:"reason"`
}) {
	return
}

// A daily-limit 429 parks every caller until the reset: no request goes to
// a key known to be spent, and the callers are told when it resets.
func TestDailyQuotaParksTheGateway(t *testing.T) {
	e, g := newBudgetEnv(t)
	g.set(func(g *budgetGateway) { g.quota, g.rpd = true, 0 })
	g.inner.chat.answer = `{"points": 3, "confidence": "high", "reason": "ok"}`
	ask := func() error {
		out := estimateAnswer()
		_, err := e.s.runAI(e.ctx, "estimate", estimateSystem, "x", estimateSchema, &out)
		return err
	}
	err := ask()
	var wait *aiWaitError
	if !errors.As(err, &wait) || wait.why != "quota" || !IsInvalid(err) {
		t.Fatalf("first ask: %v", err)
	}
	// The daily quota resets at 00:00 UTC (nextUTCMidnight).
	if want := e.clockNow().Add(12 * time.Hour); !wait.until.Equal(want) {
		t.Errorf("resets at %v, want %v", wait.until, want)
	}
	if n := g.requests(); n != 1 {
		t.Fatalf("requests to the gateway = %d, want the one that met the limit", n)
	}
	for range 3 {
		if err := ask(); !errors.As(err, &wait) {
			t.Fatalf("parked ask: %v", err)
		}
	}
	if _, _, err := e.s.decide(e.ctx, "estimate", "x", map[string]jevQuestion{"q": {Type: "noul"}}); !errors.As(err, &wait) {
		t.Errorf("parked decision: %v", err)
	}
	chain, _ := e.s.taskChain(e.ctx, "review")
	var ans struct{}
	_, attempts, err := e.s.runChain(e.ctx, chain, "s", "p", map[string]any{"type": "object"}, &ans, time.Minute)
	if !errors.As(err, &wait) || len(attempts) == 0 || attempts[0].Outcome != "quota" {
		t.Errorf("parked chain: %v, %+v", err, attempts)
	}
	if n := g.requests(); n != 1 {
		t.Errorf("requests while parked = %d, want still 1", n)
	}
	g.set(func(g *budgetGateway) { g.quota = false })
	e.setClock(wait.until)
	if err := ask(); err != nil {
		t.Fatalf("after the reset: %v", err)
	}
	if n := g.requests(); n != 2 {
		t.Errorf("requests after the reset = %d", n)
	}
}

// A raised limit ends the wait before the reset.
func TestParkedKeyRecheckedForARaisedLimit(t *testing.T) {
	e, g := newBudgetEnv(t)
	g.inner.chat.answer = `{"points": 3, "confidence": "high", "reason": "ok"}`
	g.set(func(g *budgetGateway) { g.quota, g.rpd, g.day = true, 1000, 1000 })
	out := estimateAnswer()
	run := func() error {
		_, err := e.s.runAI(e.ctx, "estimate", estimateSystem, "x", estimateSchema, &out)
		return err
	}
	var wait *aiWaitError
	if err := run(); !errors.As(err, &wait) {
		t.Fatalf("spent key: %v", err)
	}
	g.set(func(g *budgetGateway) { g.quota, g.rpd = false, 5000 })
	if err := run(); !errors.As(err, &wait) {
		t.Fatalf("rechecked at once: %v", err)
	}
	e.setClock(e.clockNow().Add(aiParkedCheck + time.Second))
	if err := run(); err != nil {
		t.Fatalf("after the recheck: %v", err)
	}
}

// Waypoint keeps no budget of its own: however much of the key's day is
// used, background work asks while the gateway admits it (WP-412).
func TestNoBudgetOfOurOwn(t *testing.T) {
	e, g := newBudgetEnv(t)
	g.set(func(g *budgetGateway) { g.day = 990 })
	var sa screenAnswer
	for range 5 {
		if _, err := e.s.runAI(e.ctx, "task_safety", screenSystem, "x", screenSchema, &sa); err != nil {
			t.Fatalf("background work near the key's limit: %v", err)
		}
	}
	if n := g.requests(); n != 5 {
		t.Errorf("requests = %d", n)
	}
	if n := e.s.screenAllowance(e.ctx); n != screenSweepBatch {
		t.Errorf("sweep allowance = %d", n)
	}
}

// A Retry-After the HTTP client doesn't wait out parks requests for those
// models until then, without a failed try; requests for other models go on.
func TestRetryAfterParksThoseModels(t *testing.T) {
	e, g := newBudgetEnv(t)
	g.set(func(g *budgetGateway) { g.busy = "600" })
	var sa screenAnswer
	_, err := e.s.runAI(e.ctx, "task_safety", screenSystem, "x", screenSchema, &sa)
	var wait *aiWaitError
	if !errors.As(err, &wait) || wait.why != "busy" || !wait.until.Equal(e.clockNow().Add(10*time.Minute)) {
		t.Fatalf("503 with Retry-After: %v", err)
	}
	if n := g.requests(); n != 1 {
		t.Fatalf("requests = %d", n)
	}
	if w := e.s.screenAIWait(e.ctx); w == nil || w.why != "busy" {
		t.Errorf("screen wait = %v", w)
	}
	if n := e.s.screenAllowance(e.ctx); n != 0 {
		t.Errorf("sweep allowance while the screen's models are busy = %d", n)
	}
	if _, err := e.s.runAI(e.ctx, "task_safety", screenSystem, "x", screenSchema, &sa); !errors.As(err, &wait) {
		t.Fatalf("parked ask: %v", err)
	}
	if n := g.requests(); n != 1 {
		t.Fatalf("requests while parked = %d", n)
	}
	g.set(func(g *budgetGateway) { g.busy = "" })
	chain, _ := e.s.taskChain(e.ctx, "review")
	var ans struct{}
	if _, _, err := e.s.runChain(e.ctx, chain, "s", "p", map[string]any{"type": "object"}, &ans, time.Minute); err != nil {
		t.Fatalf("review while the screen's models wait: %v", err)
	}
	if n := g.requests(); n != 2 {
		t.Fatalf("requests = %d", n)
	}
	e.setClock(e.clockNow().Add(10 * time.Minute))
	if _, err := e.s.runAI(e.ctx, "task_safety", screenSystem, "x", screenSchema, &sa); err != nil {
		t.Fatalf("after the wait: %v", err)
	}
	if e.s.screenAIWait(e.ctx) != nil {
		t.Error("the screen still waits")
	}
}

// A screen the gateway asks to wait counts no failed try, and isn't swept
// before the wait ends.
func TestScreensWaitOutRetryAfter(t *testing.T) {
	e, g := newBudgetEnv(t)
	g.set(func(g *budgetGateway) { g.busy, g.jevBusy = "3600", true })
	p := e.project(e.admin, "Safe", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	k := e.issue(writer, IssueInput{Project: p.Key, Title: "One", Description: "Rename a helper.", Agent: "claude"}).Key
	e.s.bg.Wait()
	sent := g.requests()
	if sent != 2 {
		t.Fatalf("the screen sent %d requests, want the decision model's and then the chat model's", sent)
	}
	var attempts int
	if err := e.s.db.R.QueryRow(`SELECT coalesce(sum(attempts), 0) FROM task_safety`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Errorf("the wait counted %d failed attempts", attempts)
	}
	for range 5 {
		e.setClock(e.clockNow().Add(10 * time.Minute))
		e.s.sweepScreens(e.ctx)
		e.s.bg.Wait()
	}
	if n := g.requests(); n != sent {
		t.Fatalf("sweeps spent %d requests during the wait", n-sent)
	}
	g.set(func(g *budgetGateway) { g.busy, g.jevBusy = "", false })
	e.setClock(e.clockNow().Add(15 * time.Minute))
	e.s.sweepScreens(e.ctx)
	e.s.bg.Wait()
	if d := e.get(e.admin, k); d.Safety == nil || d.Safety.Verdict != SafetyClear {
		t.Errorf("after the wait: %+v", d.Safety)
	}
}

// Screens that meet the daily limit wait for the reset without counting a
// failed try, send nothing meanwhile, and run again after it.
func TestScreensWaitOutTheDailyLimit(t *testing.T) {
	e, g := newBudgetEnv(t)
	g.set(func(g *budgetGateway) { g.quota, g.rpd = true, 0 })
	p := e.project(e.admin, "Safe", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	var keys []string
	for _, title := range []string{"One", "Two", "Three"} {
		keys = append(keys, e.issue(writer, IssueInput{Project: p.Key, Title: title, Description: "Rename a helper.", Agent: "claude"}).Key)
		e.s.bg.Wait()
	}
	sent := g.requests()
	if sent != 1 {
		t.Fatalf("the screens sent %d requests; the first met the limit and parked the rest", sent)
	}
	attempts := func() (n int) {
		if err := e.s.db.R.QueryRow(`SELECT coalesce(sum(attempts), 0) FROM task_safety`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := attempts(); n != 0 {
		t.Errorf("waiting for the reset counted %d failed attempts", n)
	}
	for range 5 {
		e.setClock(e.clockNow().Add(time.Hour))
		e.s.sweepScreens(e.ctx)
		e.s.bg.Wait()
	}
	if n := g.requests(); n != sent {
		t.Fatalf("sweeps spent %d requests while the key was spent", n-sent)
	}
	for _, k := range keys {
		if d := e.get(e.admin, k); d.Safety == nil || d.Safety.Verdict != SafetyPending {
			t.Fatalf("%s: %+v", k, d.Safety)
		}
	}
	g.set(func(g *budgetGateway) { g.quota = false })
	e.setClock(nextUTCMidnight(e.clockNow()).Add(time.Minute))
	e.s.sweepScreens(e.ctx)
	e.s.bg.Wait()
	for _, k := range keys {
		if d := e.get(e.admin, k); d.Safety == nil || d.Safety.Verdict != SafetyClear {
			t.Errorf("%s after the reset: %+v", k, d.Safety)
		}
	}
}

// A review run that finds the key spent is queued for the reset, without
// using one of its attempts; it runs once the key has requests again.
func TestReviewRunWaitsForTheDailyLimit(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	gw := &gatewayScript{scriptLLM: pe.llm}
	pe.s.LLM = gw
	var pid int64
	if err := pe.s.db.R.QueryRow(`SELECT id FROM ai_providers LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := declareGateway(pe.s, pid); err != nil {
		t.Fatal(err)
	}
	models, _ := pe.llm.Models(pe.ctx, nil)
	for _, m := range models {
		pe.llm.answers[m.ID] = "!quota"
	}
	pe.pullHook("opened", ForgePR{Number: 3, Title: "x", Author: "sam", HeadRef: "x", HeadSHA: "aaaaaaa1111", State: "open"})
	if done := pe.drain(); len(done) != 1 || done[0].Status != "queued" || done[0].Attempts != 0 {
		t.Fatalf("after the limit: %+v", done)
	}
	calls := len(pe.llm.calls)
	if calls == 0 || calls > 2 {
		t.Errorf("model calls = %d", calls)
	}
	pe.setClock(pe.clockNow().Add(botRetryAfter + 1))
	if done := pe.drain(); len(done) != 0 {
		t.Fatalf("ran before the reset: %+v", done)
	}
	pe.setClock(nextUTCMidnight(pe.clockNow()).Add(20 * time.Minute))
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[]}`
	if done := pe.drain(); len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("after the reset: %+v", done)
	}
}

// gatewayScript is a scriptLLM that is a gateway, and whose "!quota" answer
// is the gateway's daily-limit 429.
type gatewayScript struct{ *scriptLLM }

func (g *gatewayScript) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	g.mu.Lock()
	a := g.answers[model]
	g.mu.Unlock()
	if a == "!quota" {
		g.mu.Lock()
		g.calls = append(g.calls, model)
		g.mu.Unlock()
		return nil, "", &llmError{status: 429, body: `{"error":{"message":"key:key_x reached its daily request limit (1000)","type":"insufficient_quota","code":"quota"}}`, retryAfter: 5 * time.Hour}
	}
	return g.scriptLLM.Complete(ctx, p, model, system, prompt, schema)
}

func (g *gatewayScript) CompleteWithFallbacks(ctx context.Context, p *AIProvider, model string, fallbacks []string, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	return g.Complete(ctx, p, model, system, prompt, schema)
}

func TestDailyQuotaIsToldFromARateLimit(t *testing.T) {
	quota := &llmError{status: 429, body: `{"error":{"message":"reached its daily request limit (1000)","type":"insufficient_quota","code":"quota"}}`}
	rpm := &llmError{status: 429, body: `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit"}}`}
	if !quota.dailyQuota() || rpm.dailyQuota() || (&llmError{status: 500, body: "insufficient_quota"}).dailyQuota() {
		t.Error("dailyQuota")
	}
	if classifyModelError(quota) != "quota" || classifyModelError(rpm) != "rate_limit" {
		t.Error("classify")
	}
	if got := attemptSummary([]ModelAttempt{{Outcome: "quota"}}); !strings.Contains(got, "daily limit") {
		t.Errorf("summary %q", got)
	}
}

// A Retry-After longer than a day is a fault, not a wait: a daily-limit 429
// parks until the reset, a busy answer parks nothing, and a huge one doesn't
// overflow.
func TestRetryAfterBeyondADay(t *testing.T) {
	e, _ := newBudgetEnv(t)
	if d, ok := retryAfter("99999999999999", e.clockNow()); !ok || d <= 0 || d > 366*24*time.Hour {
		t.Errorf("huge Retry-After = %v %v", d, ok)
	}
	pid := e.s.gatewayID(e.ctx)
	w := e.s.noteWait(e.ctx, pid, "free", &llmError{status: 429, body: `{"error":{"code":"insufficient_quota"}}`, retryAfter: 48 * time.Hour})
	if w == nil || w.why != "quota" || !w.until.Equal(nextUTCMidnight(e.clockNow())) {
		t.Errorf("daily limit with a 48h Retry-After: %+v", w)
	}
	if w := e.s.noteWait(e.ctx, pid, "free", &llmError{status: 503, retryAfter: 48 * time.Hour}); w != nil {
		t.Errorf("busy with a 48h Retry-After parked: %+v", w)
	}
}

// The screen waits only on its own chain: another provider's spent day or
// another task's wait doesn't hold it.
func TestScreenWaitsOnlyOnItsChain(t *testing.T) {
	e, _ := newBudgetEnv(t)
	pid := e.s.gatewayID(e.ctx)
	until := e.clockNow().Add(time.Hour)
	e.s.parkAI(pid+100, until)
	e.s.parkAsk(pid, "other", "", "estimate", until)
	if w := e.s.screenAIWait(e.ctx); w != nil {
		t.Fatalf("another provider's or task's wait holds the screen: %v", w)
	}
	e.s.parkAsk(pid, "free", "", "task_safety", until)
	if w := e.s.screenAIWait(e.ctx); w == nil || w.why != "busy" || !w.until.Equal(until) {
		t.Fatalf("the screen's own busy wait: %+v", w)
	}
	e.s.parkAI(pid, until.Add(time.Hour))
	if w := e.s.screenAIWait(e.ctx); w == nil || w.why != "quota" {
		t.Fatalf("the screen's provider out of its day: %+v", w)
	}
}

// A gateway without the key extension can't be asked whether its limit was
// raised, so a quota 429 without Retry-After parks it a while, not until
// midnight (WP-1187).
func TestStockGatewayQuotaParksAWhile(t *testing.T) {
	e, g := newBudgetEnv(t)
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test", Model: "my-model", Extensions: []string{}}); err != nil {
		t.Fatal(err)
	}
	g.set(func(g *budgetGateway) { g.quota, g.retryIn = true, "" })
	g.inner.chat.answer = `{"points": 3, "confidence": "high", "reason": "ok"}`
	out := estimateAnswer()
	_, err := e.s.runAI(e.ctx, "estimate", estimateSystem, "x", estimateSchema, &out)
	var wait *aiWaitError
	if !errors.As(err, &wait) || wait.why != "quota" {
		t.Fatalf("ask: %v", err)
	}
	if want := e.clockNow().Add(stockQuotaWait); !wait.until.Equal(want) {
		t.Errorf("parked until %v, want %v", wait.until, want)
	}
}
