package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateway mocks the gateway: an OpenAI API under /v1 that streams, names
// the model that answered in servedModelHeader, and can be told to rate
// limit, fail or answer badly.
type fakeGateway struct {
	mu       sync.Mutex
	answer   string
	served   string // the model that answers; space-bunny if unset
	bad      int    // answer this many estimates short of fields
	limited  int    // answer this many requests 429
	retry    string // their Retry-After
	status   int    // answer every request with this status instead
	midError bool   // fail after the stream has started
	bodies   []map[string]any
	at       []time.Time
	ids      []string         // each chat request's X-Request-ID
	feedback []map[string]any // POST /v1/feedback bodies
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.URL.Path {
	case "/v1/models":
		w.Write([]byte(`{"object":"list","data":[{"id":"free","free":true},{"id":"opencode/muse-spark-1.2-contributor-free","free":true},` +
			`{"id":"opencode/muse-spark-1.3-contributor-free","free":true},{"id":"opencode/nemotron-3-ultra-free","free":true},` +
			`{"id":"opencode/space-bunny-free","free":true},{"id":"opencode/mimo-v2.6-flash-free","free":true}]}`))
		return
	case "/v1/feedback":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		g.feedback = append(g.feedback, body)
		w.Write([]byte(`{"request_id":"x"}`))
		return
	case "/v1/chat/completions":
	default:
		w.WriteHeader(404)
		return
	}
	if r.Header.Get("Authorization") != "Bearer olm_test" {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"bad key","type":"authentication_error","code":"invalid_api_key"}}`))
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	g.bodies, g.at = append(g.bodies, body), append(g.at, time.Now())
	g.ids = append(g.ids, r.Header.Get("X-Request-ID"))
	if g.limited > 0 {
		g.limited--
		w.Header().Set("Retry-After", g.retry)
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit"}}`))
		return
	}
	if g.status != 0 {
		w.WriteHeader(g.status)
		w.Write([]byte(`{"error":{"message":"all 3 candidate models failed; last: upstream 500","type":"server_error","code":"server"}}`))
		return
	}
	served, answer := g.served, g.answer
	if served == "" {
		served = "opencode/space-bunny-free"
	}
	if g.bad > 0 && isEstimate(body) {
		g.bad--
		answer = `{"points": 3}`
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set(servedModelHeader, served)
	fmt.Fprint(w, ": keep-alive\n\n")
	if g.midError {
		fmt.Fprint(w, `data: {"error":{"message":"upstream went away","type":"server_error"}}`+"\n\n")
		return
	}
	// The answer in two pieces, as a stream delivers it.
	half := len(answer) / 2
	for _, part := range []string{answer[:half], answer[half:]} {
		c, _ := json.Marshal(map[string]any{"model": served, "choices": []any{map[string]any{"delta": map[string]string{"content": part}}}})
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// estimates are the requests that asked for an estimate: an issue's other
// AI tasks run alongside.
func (g *fakeGateway) estimates() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []map[string]any
	for _, b := range g.bodies {
		if isEstimate(b) {
			out = append(out, b)
		}
	}
	return out
}

func isEstimate(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	return strings.HasPrefix(fmt.Sprint(m["content"]), estimateSystem)
}

// namedGatewayModels is a chain of models a gateway serves by name, the
// free route last.
var namedGatewayModels = []string{"opencode/muse-spark-*", "opencode/nemotron-*-ultra-free", "opencode/space-bunny-free", "opencode/mimo-*-flash-free", "free"}

func gatewayClient() *httpLLM {
	return &httpLLM{http: EgressPolicy{AllowLoopback: true}.client(10 * time.Second)}
}

func TestGatewayClient(t *testing.T) {
	g := &fakeGateway{answer: `{"points": 3, "confidence": "high", "reason": "ok"}`}
	srv := httptest.NewServer(g)
	defer srv.Close()
	c := gatewayClient()
	p := &AIProvider{Kind: "openai", BaseURL: srv.URL + "/v1", apiKey: "olm_test"}
	ctx := context.Background()

	// A streamed answer, and the model behind the route.
	raw, served, err := c.Complete(ctx, p, "free", "sys", "prompt", estimateSchema)
	if err != nil || served != "opencode/space-bunny-free" || !strings.Contains(string(raw), `"points": 3`) {
		t.Fatalf("answer %s by %q: %v", raw, served, err)
	}
	if g.bodies[0]["stream"] != true || g.bodies[0]["model"] != "free" {
		t.Errorf("request = %v", g.bodies[0])
	}

	// A 429 waits as long as Retry-After says, then tries again.
	g.limited, g.retry = 1, "1"
	if _, _, err := c.Complete(ctx, p, "free", "sys", "prompt", estimateSchema); err != nil {
		t.Fatal(err)
	}
	n := len(g.at)
	if wait := g.at[n-1].Sub(g.at[n-2]); wait < time.Second {
		t.Errorf("retried after %v, Retry-After said 1s", wait)
	}

	// Not when the wait outlasts the call: the chain's next model gets it.
	g.limited, g.retry = 1, "30"
	short, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err = c.Complete(short, p, "free", "sys", "prompt", estimateSchema)
	if classifyModelError(err) != "rate_limit" || time.Since(start) > 2*time.Second {
		t.Errorf("long Retry-After: %v after %v", err, time.Since(start))
	}
	g.limited = 0

	// 502: every model behind the gateway failed. No retry, a server error.
	g.status = 502
	before := len(g.bodies)
	_, _, err = c.Complete(ctx, p, "free", "sys", "prompt", estimateSchema)
	if classifyModelError(err) != "server" || len(g.bodies) != before+1 {
		t.Errorf("502: %v after %d requests", err, len(g.bodies)-before)
	}
	g.status = 0

	// An error after the stream started is the model's.
	g.midError = true
	_, _, err = c.Complete(ctx, p, "free", "sys", "prompt", estimateSchema)
	var me *modelError
	if !errors.As(err, &me) || !strings.Contains(me.msg, "went away") {
		t.Errorf("mid-stream error: %v", err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for h, want := range map[string]time.Duration{
		"3":                             3 * time.Second,
		"Wed, 21 Oct 2026 12:00:10 GMT": 10 * time.Second,
		"Wed, 21 Oct 2026 11:00:00 GMT": 0,
	} {
		if got, ok := retryAfter(h, now); !ok || got != want {
			t.Errorf("retryAfter(%q) = %v %v, want %v", h, got, ok, want)
		}
	}
	for _, h := range []string{"", "soon"} {
		if _, ok := retryAfter(h, now); ok {
			t.Errorf("retryAfter(%q) ok", h)
		}
	}
}

func TestCheckAnswer(t *testing.T) {
	for raw, want := range map[string]string{
		`{"points": 3, "confidence": "high", "reason": "ok"}`: "",
		`{"points": 4, "confidence": "huge", "reason": "ok"}`: "", // enums are the caller's to map
		`{"points": 3, "confidence": "high"}`:                 `lacks "reason"`,
		`{"points": "3", "confidence": "high", "reason": ""}`: `"points" should be a number`,
		`[]`: "should be an object",
	} {
		err := checkAnswer(estimateSchema, json.RawMessage(raw))
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
	if err := checkAnswer(breakdownSchema, json.RawMessage(`{"subtasks":[{"title":"a","points":1},{"points":2}]}`)); err == nil || !strings.Contains(err.Error(), `item 2 lacks "title"`) {
		t.Errorf("array items: %v", err)
	}
}

func TestEnsureLLMGateway(t *testing.T) {
	e := newEnv(t)
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`}
	gw := httptest.NewServer(g)
	defer gw.Close()
	// What was set up before: another OpenAI-compatible provider.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"old-model","choices":[{"message":{"content":"{\"points\": 2, \"confidence\": \"low\", \"reason\": \"old\"}"}}]}`))
	}))
	defer old.Close()
	e.s.LLM = gatewayClient()
	// The first setup needs a URL: with only a key there is nothing to keep.
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: "", APIKey: "olm_test", Extensions: GatewayExtensions}); err == nil || e.s.gatewayID(e.ctx) != 0 {
		t.Fatalf("a key-only first setup = %v, provider %d; want an error and none", err, e.s.gatewayID(e.ctx))
	}
	prev, err := e.s.CreateAIProvider(e.ctx, e.admin, AIProviderInput{Name: str("Old"), Kind: str("openai"), BaseURL: str(old.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetAITask(e.ctx, e.admin, "estimate", prev.ID, "old-model", nil); err != nil {
		t.Fatal(err)
	}

	// The gateway's root is given, as for Anthropic clients; its OpenAI API is /v1.
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	tasks, _ := e.s.AITasks(e.ctx, e.admin)
	var gwID int64
	for _, tk := range tasks {
		want := gatewayModels(tk.Task)
		if tk.Task == "estimate" { // what it had after
			want = append(want, "old-model")
			gwID = *tk.ProviderID
		}
		if got := chainModels(tk); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", tk.Task, got, want)
		}
	}
	if m := gatewayModels("verify_review"); m[0] == gatewayModels("review")[0] {
		t.Errorf("the verifier starts with the reviewer's model: %v", m) // WP-D14
	}
	// Reviews ask for the gateway's review routes, the free route last.
	if r, v := gatewayModels("review"), gatewayModels("verify_review"); !slices.Equal(r, []string{"review", "free"}) || !slices.Equal(v, []string{"review-verify", "free"}) {
		t.Errorf("review = %v, verify_review = %v", r, v)
	}
	gp, err := e.s.aiProvider(e.ctx, gwID)
	if err != nil || gp.Name != "LLM gateway" || gp.BaseURL != gw.URL+"/v1" || gp.apiKey != "olm_test" {
		t.Fatalf("provider = %+v %v", gp, err)
	}
	// A start with only the key keeps the saved URL and takes the new key.
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: "", APIKey: "olm_rotated", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	if gp, err := e.s.aiProvider(e.ctx, gwID); err != nil || gp.BaseURL != gw.URL+"/v1" || gp.apiKey != "olm_rotated" {
		t.Fatalf("after a key-only start: %+v %v", gp, err)
	}
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: "", APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}

	// Estimates ask the gateway for its free route, and name the model
	// that answered.
	p := e.project(e.admin, "OL", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Add dark mode"})
	est, err := e.s.EstimateIssue(e.ctx, e.admin, i.Key)
	if err != nil || est.Points != 5 || est.Model != "LLM gateway · opencode/space-bunny-free" {
		t.Fatalf("estimate = %+v %v", est, err)
	}
	asked := g.estimates()
	if body := asked[len(asked)-1]; body["model"] != "free" || body["fallbacks"] != nil {
		t.Errorf("asked for %v then %v", body["model"], body["fallbacks"])
	}

	// A chain that names models on the gateway asks for them in one
	// request, the newest of each family.
	var named []ChainModel
	for _, m := range append(namedGatewayModels[1:], "old-model") {
		named = append(named, ChainModel{ProviderID: gwID, Model: m})
	}
	named[len(named)-1].ProviderID = prev.ID
	if err := e.s.SetAITask(e.ctx, e.admin, "estimate", gwID, namedGatewayModels[0], named); err != nil {
		t.Fatal(err)
	}
	if est, err = e.s.EstimateIssue(e.ctx, e.admin, i.Key); err != nil || est.Points != 5 {
		t.Fatalf("estimate = %+v %v", est, err)
	}
	asked = g.estimates()
	if body := asked[len(asked)-1]; body["model"] != "opencode/muse-spark-1.3-contributor-free" ||
		fmt.Sprint(body["fallbacks"]) != "[opencode/nemotron-3-ultra-free opencode/space-bunny-free opencode/mimo-v2.6-flash-free free]" {
		t.Errorf("asked for %v then %v", body["model"], body["fallbacks"])
	}
	// A 502 (every model behind it failed) falls back to what was there
	// before, without asking the gateway again.
	g.mu.Lock()
	g.status = 502
	g.mu.Unlock()
	before := len(g.estimates())
	est, err = e.s.EstimateIssue(e.ctx, e.admin, i.Key)
	if err != nil || est.Points != 2 || est.Model != "Old · old-model" {
		t.Fatalf("estimate after 502 = %+v %v", est, err)
	}
	for _, b := range g.estimates()[before:] {
		if b["model"] != "opencode/muse-spark-1.3-contributor-free" {
			t.Errorf("asked the gateway again, for %v", b["model"])
		}
	}
	// Muse Spark answers badly: the models after it get their turn, in a
	// request of their own.
	g.mu.Lock()
	g.status, g.served, g.bad = 0, "opencode/muse-spark-1.3-contributor-free", 1
	g.mu.Unlock()
	if est, err = e.s.EstimateIssue(e.ctx, e.admin, i.Key); err != nil || est.Points != 5 {
		t.Fatalf("estimate after a bad answer = %+v %v", est, err)
	}
	asked = g.estimates()
	if body := asked[len(asked)-1]; body["model"] != "opencode/nemotron-3-ultra-free" ||
		fmt.Sprint(body["fallbacks"]) != "[opencode/space-bunny-free opencode/mimo-v2.6-flash-free free]" {
		t.Errorf("after the bad answer asked for %v then %v", body["model"], body["fallbacks"])
	}

	// Later starts bring the key up to date and leave the tasks alone.
	if err := e.s.SetAITask(e.ctx, e.admin, "breakdown", prev.ID, "old-model", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL + "/v1/", APIKey: "olm_rotated", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	if gp, _ := e.s.aiProvider(e.ctx, gwID); gp.apiKey != "olm_rotated" {
		t.Errorf("key not updated: %+v", gp)
	}
	tasks, _ = e.s.AITasks(e.ctx, e.admin)
	for _, tk := range tasks {
		if tk.Task == "breakdown" && tk.Model != "old-model" {
			t.Errorf("breakdown reassigned: %+v", tk)
		}
	}
	// Removed by an admin, it stays removed.
	if err := e.s.DeleteAIProvider(e.ctx, e.admin, gwID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	if ps, _ := e.s.ListAIProviders(e.ctx, e.admin); len(ps) != 1 {
		t.Errorf("providers = %+v", ps)
	}
}

// chainModels is a task's models in order.
func chainModels(tk AITask) []string {
	ms := []string{tk.Model}
	for _, f := range tk.Fallbacks {
		ms = append(ms, f.Model)
	}
	return ms
}

// A stock OpenAI-compatible gateway, with no extensions: every task asks
// for the one model setup names, and requests carry nothing but the
// standard fields (WP-1187).
func TestStockGateway(t *testing.T) {
	e := newEnv(t)
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`, served: "my-model"}
	gw := httptest.NewServer(g)
	defer gw.Close()
	e.s.LLM = gatewayClient()
	// Without routes it needs the model to ask for.
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test"}); err == nil || e.s.gatewayID(e.ctx) != 0 {
		t.Fatalf("no model = %v, provider %d; want an error and none", err, e.s.gatewayID(e.ctx))
	}
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Model: "my-model"}); err != nil {
		t.Fatal(err)
	}
	pid := e.s.gatewayID(e.ctx)
	gp, err := e.s.aiProvider(e.ctx, pid)
	if err != nil || gp.Name != "LLM gateway" {
		t.Fatalf("provider = %+v %v", gp, err)
	}
	tasks, _ := e.s.AITasks(e.ctx, e.admin)
	for _, tk := range tasks {
		if got := chainModels(tk); len(got) == 0 || got[0] != "my-model" {
			t.Errorf("%s = %v, want my-model first", tk.Task, got)
		}
	}
	for _, x := range GatewayExtensions {
		if e.s.gatewayWith(e.ctx, x) != 0 {
			t.Errorf("declares %s", x)
		}
	}
	if e.s.surplusAllowed(e.ctx) {
		t.Error("surplus work allowed without routing")
	}
	if _, _, why := e.s.gatewayTasksLLM(e.ctx); why == "" {
		t.Error("audits through a gateway without tasks")
	}
	if c := e.s.defaultAuditChain(e.ctx); c[0].CLI == GatewayCLI {
		t.Errorf("default audit chain = %v", c)
	}
	if _, _, err := e.s.decide(e.ctx, "test", "{}", nil); !errors.Is(err, errNoDecider) {
		t.Errorf("decide = %v, want errNoDecider", err)
	}

	// Two models on it: one request each, no fallbacks, routing or exclude.
	chain := []ChainModel{{ProviderID: pid, Model: "my-model"}, {ProviderID: pid, Model: "other-model"}}
	ctx := withGatewayExclude(withGatewayRouting(e.ctx, &gatewayRouting{prefix: "wp-test-", paid: "fallback"}), []string{"anthropic/claude-sonnet-4"})
	var out map[string]any
	if _, _, _, err := e.s.runChainAt(ctx, chain, estimateSystem, "prompt", estimateSchema, &out, time.Minute); err != nil {
		t.Fatal(err)
	}
	asked := g.estimates()
	body := asked[len(asked)-1]
	if body["model"] != "my-model" || body["fallbacks"] != nil || body["routing"] != nil || body["exclude"] != nil {
		t.Errorf("asked %v", body)
	}
	if id := g.ids[len(g.ids)-1]; id != "" {
		t.Errorf("X-Request-ID %q", id)
	}

	// An admin's own chain for one task, after a fallback of theirs on
	// another.
	other, err := e.s.CreateAIProvider(e.ctx, e.admin, AIProviderInput{Name: str("Other"), Kind: str("openai"), BaseURL: str(gw.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetAITask(e.ctx, e.admin, "breakdown", pid, "admin-pick", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetAITask(e.ctx, e.admin, "estimate", pid, "my-model", []ChainModel{{ProviderID: other.ID, Model: "other-model"}}); err != nil {
		t.Fatal(err)
	}
	leads := func(when string, want func(task string) []string) {
		t.Helper()
		tasks, _ := e.s.AITasks(e.ctx, e.admin)
		for _, tk := range tasks {
			w := want(tk.Task)
			switch tk.Task {
			case "breakdown":
				w = []string{"admin-pick"}
			case "estimate":
				w = append(w, "other-model")
			}
			if got := chainModels(tk); !slices.Equal(got, w) || *tk.ProviderID != pid {
				t.Errorf("%s: %s = %v, want %v", when, tk.Task, got, w)
			}
		}
	}

	// A later start declares them all: the extensions apply, and the tasks
	// setup gave the one model ask for the routes instead.
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	if e.s.gatewayWith(e.ctx, GatewayExtensions...) != pid {
		t.Error("extensions not taken up")
	}
	leads("routes declared", gatewayModels)
	// A start that doesn't say keeps them; one that says none drops them,
	// and the tasks go back to the model, which a start that doesn't name
	// one keeps (WP-1187: a gateway without routes doesn't answer them).
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test"}); err != nil || e.s.gatewayWith(e.ctx, GatewayExtensions...) != pid {
		t.Errorf("unset extensions = %v, provider %d", err, e.s.gatewayWith(e.ctx, GatewayExtensions...))
	}
	leads("unset extensions", gatewayModels)
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test", Extensions: []string{}}); err != nil || e.s.gatewayWith(e.ctx, GatewayFallbacks) != 0 {
		t.Errorf("none = %v", err)
	}
	leads("routes dropped", func(string) []string { return []string{"my-model"} })
	// Another model: the tasks still asking for the old one ask for it.
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test", Model: "new-model"}); err != nil {
		t.Fatal(err)
	}
	leads("model changed", func(string) []string { return []string{"new-model"} })
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test", Model: "my-model", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	leads("routes again", gatewayModels)
	if _, _, _, err := e.s.runChainAt(ctx, chain, estimateSystem, "prompt", estimateSchema, &out, time.Minute); err != nil {
		t.Fatal(err)
	}
	asked = g.estimates()
	body = asked[len(asked)-1]
	if fmt.Sprint(body["fallbacks"]) != "[other-model]" || body["routing"] == nil || body["exclude"] == nil {
		t.Errorf("with extensions asked %v", body)
	}
}

func TestParseGatewayExtensions(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"all":                    strings.Join(GatewayExtensions, ","),
		"Fallbacks, exclude key": "fallbacks,exclude,key",
		"routes,routes":          "routes",
		"none":                   "",
	} {
		got, err := ParseGatewayExtensions(in)
		if err != nil || strings.Join(got, ",") != want {
			t.Errorf("%q = %v %v, want %s", in, got, err, want)
		}
	}
	if got, _ := ParseGatewayExtensions(""); got != nil {
		t.Errorf("unset = %v, want nil (keep)", got)
	}
	if got, _ := ParseGatewayExtensions("none"); got == nil {
		t.Error("none = nil, want empty (declare none)")
	}
	if _, err := ParseGatewayExtensions("routes,warp"); err == nil || !strings.Contains(err.Error(), `"warp"`) {
		t.Errorf("unknown = %v", err)
	}
	// all and none stand alone: anything beside them is a mistake.
	for _, in := range []string{"all,warp", "warp,all", "none,routes", "all,none", "routes all"} {
		if got, err := ParseGatewayExtensions(in); err == nil {
			t.Errorf("%q = %v, want an error", in, got)
		}
	}
}

// A first setup that stops part way (a busy database) is finished by the
// next start: every task asks the gateway first, none twice.
func TestGatewaySetupResumes(t *testing.T) {
	e := newEnv(t)
	gw := httptest.NewServer(&fakeGateway{})
	defer gw.Close()
	e.s.LLM = gatewayClient()
	for _, q := range []string{
		`CREATE TRIGGER busy_ins BEFORE INSERT ON ai_tasks WHEN NEW.task = 'review' BEGIN SELECT RAISE(ABORT, 'database is busy'); END`,
		`CREATE TRIGGER busy_upd BEFORE UPDATE ON ai_tasks WHEN NEW.task = 'review' BEGIN SELECT RAISE(ABORT, 'database is busy'); END`,
	} {
		if _, err := e.s.db.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	g := GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}
	if err := e.s.EnsureLLMGateway(e.ctx, g); err == nil {
		t.Fatal("the busy setup didn't fail")
	}
	if _, err := e.s.db.W.Exec(`DROP TRIGGER busy_ins; DROP TRIGGER busy_upd`); err != nil {
		t.Fatal(err)
	}
	if e.s.gatewayWith(e.ctx, GatewayRoutes) != 0 {
		t.Error("a half-done setup declares its extensions")
	}
	if err := e.s.EnsureLLMGateway(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	pid := e.s.gatewayWith(e.ctx, GatewayExtensions...)
	if pid == 0 {
		t.Fatal("the finished setup declares no extensions")
	}
	tasks, _ := e.s.AITasks(e.ctx, e.admin)
	for _, tk := range tasks {
		if got := chainModels(tk); !slices.Equal(got, gatewayModels(tk.Task)) || tk.ProviderID == nil || *tk.ProviderID != pid {
			t.Errorf("%s = %v", tk.Task, got)
		}
	}
	// Once finished, a start leaves the chains as an admin set them.
	if err := e.s.SetAITask(e.ctx, e.admin, "review", pid, "my-pick", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.s.EnsureLLMGateway(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	tasks, _ = e.s.AITasks(e.ctx, e.admin)
	for _, tk := range tasks {
		if tk.Task == "review" && !slices.Equal(chainModels(tk), []string{"my-pick"}) {
			t.Errorf("review = %v after a later start", chainModels(tk))
		}
	}
}

// A first setup from the old settings' names declares every extension, as
// those deploys had; with the new names it declares what they say.
func TestGatewaySetupLegacy(t *testing.T) {
	e := newEnv(t)
	gw := httptest.NewServer(&fakeGateway{})
	defer gw.Close()
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Legacy: true}); err != nil {
		t.Fatal(err)
	}
	pid := e.s.gatewayWith(e.ctx, GatewayExtensions...)
	if pid == 0 {
		t.Fatal("legacy first setup without all extensions")
	}
	// Later starts on the old names keep what was declared since.
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test", Model: "my-model", Extensions: []string{GatewayKey}}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test", Legacy: true}); err != nil {
		t.Fatal(err)
	}
	if e.s.gatewayWith(e.ctx, GatewayKey) != pid || e.s.gatewayWith(e.ctx, GatewayRoutes) != 0 {
		t.Error("a legacy start re-declared every extension")
	}
}

// A site that ran OpenLLMs before WP-1187 goes on as it was with nothing
// for a person to do: 0164 declares every extension there is, and a start
// on the old names keeps them and the chains.
func TestOpenLLMsSiteCarriesOver(t *testing.T) {
	body, err := os.ReadFile("../db/migrations/0164_llm_gateway.sql")
	if err != nil {
		t.Fatal(err)
	}
	if all := "'" + strings.Join(GatewayExtensions, ",") + "'"; !strings.Contains(string(body), all) {
		t.Fatalf("0164 doesn't declare every extension (%s)", all)
	}
	e := newEnv(t)
	gw := httptest.NewServer(&fakeGateway{})
	defer gw.Close()
	p, err := e.s.CreateAIProvider(e.ctx, e.admin, AIProviderInput{Name: str("OpenLLMs"), Kind: str("openai"), BaseURL: str(gw.URL + "/v1")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range aiTasks {
		if err := e.s.SetAITask(e.ctx, e.admin, tk.Task, p.ID, gatewayModels(tk.Task)[0], []ChainModel{{ProviderID: p.ID, Model: "free"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := declareGateway(e.s, p.ID); err != nil { // what 0164 leaves
		t.Fatal(err)
	}
	before, _ := e.s.AITasks(e.ctx, e.admin)
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Legacy: true}); err != nil {
		t.Fatal(err)
	}
	if e.s.gatewayWith(e.ctx, GatewayExtensions...) != p.ID {
		t.Error("an extension was lost")
	}
	same := func(when string) {
		t.Helper()
		after, _ := e.s.AITasks(e.ctx, e.admin)
		for i := range before {
			if !slices.Equal(chainModels(before[i]), chainModels(after[i])) {
				t.Errorf("%s: %s: %v became %v", when, before[i].Task, chainModels(before[i]), chainModels(after[i]))
			}
		}
	}
	same("legacy start")
	// Dropping routes there needs the model the tasks ask for instead: a
	// start without one changes nothing, rather than leave chains of routes
	// the gateway is said not to answer.
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test", Extensions: []string{GatewayKey}}); err == nil {
		t.Error("routes dropped without a model")
	}
	if e.s.gatewayWith(e.ctx, GatewayExtensions...) != p.ID {
		t.Error("extensions changed by a start that failed")
	}
	same("routes dropped without a model")
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{APIKey: "olm_test", Model: "my-model", Extensions: []string{GatewayKey}}); err != nil {
		t.Fatal(err)
	}
	after, _ := e.s.AITasks(e.ctx, e.admin)
	for _, tk := range after {
		// Each asked for its first route then "free": only the first was
		// setup's lead for most; review and prep had both.
		if got := chainModels(tk); got[0] != "my-model" || slices.Contains(got[:len(got)-1], gatewayModels(tk.Task)[0]) {
			t.Errorf("routes dropped: %s = %v", tk.Task, got)
		}
	}
}

// A removed gateway provider is no gateway: what needs one doesn't find it,
// and a later start doesn't bring it back.
func TestRemovedGateway(t *testing.T) {
	e := newEnv(t)
	gw := httptest.NewServer(&fakeGateway{})
	defer gw.Close()
	g := GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}
	if err := e.s.EnsureLLMGateway(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	pid := e.s.gatewayID(e.ctx)
	if err := e.s.DeleteAIProvider(e.ctx, e.admin, pid); err != nil {
		t.Fatal(err)
	}
	if id, with := e.s.gatewayID(e.ctx), e.s.gatewayWith(e.ctx); id != 0 || with != 0 {
		t.Errorf("after removal gatewayID = %d, gatewayWith = %d", id, with)
	}
	if err := e.s.EnsureLLMGateway(e.ctx, g); err != nil || e.s.gatewayID(e.ctx) != 0 {
		t.Errorf("a later start = %v, gateway %d", err, e.s.gatewayID(e.ctx))
	}
}

// TestGatewayLive makes one real call through a gateway with routes and
// fallbacks, when LLM_GATEWAY_API_KEY and LLM_GATEWAY_URL are set:
// go test ./internal/core -run TestGatewayLive -v
func TestGatewayLive(t *testing.T) {
	key, base := os.Getenv("LLM_GATEWAY_API_KEY"), os.Getenv("LLM_GATEWAY_URL")
	if key == "" || base == "" {
		t.Skip("LLM_GATEWAY_API_KEY or LLM_GATEWAY_URL not set")
	}
	c := &httpLLM{http: EgressPolicy{}.client(3 * time.Minute)}
	p := &AIProvider{Kind: "openai", BaseURL: gatewayBase(base), apiKey: key}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	raw, served, err := c.CompleteWithFallbacks(ctx, p, "opencode/muse-spark-1.3-contributor-free",
		[]string{"opencode/nemotron-3-ultra-free", gatewayFreeRoute}, estimateSystem,
		"Work item to estimate (task):\nTitle: Fix a typo in the README\n", estimateSchema)
	t.Logf("served by %s; answer: %s", served, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAnswer(estimateSchema, raw); err != nil {
		t.Fatalf("the answer doesn't fit: %v", err) // runChain would try the next model
	}
}

// A paid entry is its own request, after the free ones failed, and the
// gateway being rate limited doesn't skip it (WP-243).
func TestRunChainAsksAPaidModelAlone(t *testing.T) {
	e := newEnv(t)
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`, served: "anthropic/claude-haiku-5-5", limited: 1}
	gw := httptest.NewServer(g)
	defer gw.Close()
	e.s.LLM = gatewayClient()
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	pid := e.s.gatewayID(e.ctx)
	chain := []ChainModel{{ProviderID: pid, Model: "opencode/muse-spark-*"}, {ProviderID: pid, Model: "free"},
		{ProviderID: pid, Model: "anthropic/claude-haiku-5-5", Paid: true}, {ProviderID: pid, Model: "opencode/space-bunny-free"}}
	g.mu.Lock()
	g.bodies = nil
	g.mu.Unlock()
	var out struct {
		Points int `json:"points"`
	}
	used, attempts, err := e.s.runChain(e.ctx, chain, estimateSystem, "estimate this", estimateSchema, &out, time.Minute)
	if err != nil || used.Model != "anthropic/claude-haiku-5-5" || !used.Paid || out.Points != 5 {
		t.Fatalf("used %+v, %v, %+v", used, err, attempts)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) != 2 || g.bodies[0]["fallbacks"] == nil || g.bodies[1]["model"] != "anthropic/claude-haiku-5-5" || g.bodies[1]["fallbacks"] != nil {
		t.Errorf("asked %v", g.bodies)
	}
	if fb := fmt.Sprint(g.bodies[0]["fallbacks"]); strings.Contains(fb, "haiku") {
		t.Errorf("the free request's fallbacks name the paid model: %s", fb)
	}
}

// declareGateway makes provider pid this Waypoint's LLM gateway, with every
// extension, as a deploy with the old settings has it.
func declareGateway(s *Service, pid int64) error {
	if _, err := s.db.W.Exec(`INSERT OR REPLACE INTO server_settings(key, value) VALUES (?, ?)`, settingGatewayProvider, itoa64(pid)); err != nil {
		return err
	}
	return s.setGatewayExtensions(context.Background(), GatewayExtensions)
}
