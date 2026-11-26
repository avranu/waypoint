package core

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeJev mocks the gateway's /v1/systemone in front of a fakeGateway: it
// answers each question with yes, as given in yes (0 when unset), or
// answers every request with status.
type fakeJev struct {
	mu     sync.Mutex
	yes    map[string]float64
	status int
	bodies []map[string]any
	chat   *fakeGateway
}

func (j *fakeJev) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/systemone" {
		j.chat.ServeHTTP(w, r)
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	j.bodies = append(j.bodies, body)
	if j.status != 0 {
		w.WriteHeader(j.status)
		w.Write([]byte(`{"error":{"message":"this API key may not use \"jev-latest\"","type":"permission_error","code":"permission"}}`))
		return
	}
	answers := map[string]any{}
	qs, _ := body["questions"].(map[string]any)
	for name := range qs {
		answers[name] = map[string]any{"type": "noul", "noul": j.yes[name]}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.14.0", "answers": answers, "usage": map[string]int{"input_tokens": 400, "output_tokens": 11}})
}

func (j *fakeJev) asked() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.bodies)
}

// screens are the chat requests that screened a task.
func (g *fakeGateway) screens() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, b := range g.bodies {
		msgs, _ := b["messages"].([]any)
		if m, _ := msgs[0].(map[string]any); strings.HasPrefix(fmt.Sprint(m["content"]), screenSystem) {
			n++
		}
	}
	return n
}

func TestDecideThroughTheGateway(t *testing.T) {
	j := &fakeJev{yes: map[string]float64{"refund": 0.98}, chat: &fakeGateway{}}
	gw := httptest.NewServer(j)
	defer gw.Close()
	c := gatewayClient()
	p := &AIProvider{Kind: "openai", BaseURL: gw.URL + "/v1", apiKey: "olm_test"}
	qs := map[string]jevQuestion{"refund": {Type: "noul", Instructions: "Does the customer ask for a refund?"}}
	ans, model, err := c.Decide(t.Context(), p, jevModel, "I was charged twice.", qs)
	if err != nil || model != "jev-1.14.0" || ans["refund"].Noul != 0.98 {
		t.Fatalf("answers %+v, model %q, %v", ans, model, err)
	}
	b := j.bodies[0]
	if b["model"] != "jev-latest" || b["state"] != "I was charged twice." || fmt.Sprint(b["questions"]) != "map[refund:map[instructions:Does the customer ask for a refund? type:noul]]" {
		t.Errorf("sent %v", b)
	}
	j.status = 403
	if _, _, err := c.Decide(t.Context(), p, jevModel, "x", qs); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("refused key: %v", err)
	}
}

// The decision model clears a task it is sure is plain; any doubt, or the
// decision model failing, leaves the task to the chat model (WP-298).
func TestSafetyScreenAsksTheDecisionModelFirst(t *testing.T) {
	e := newEnv(t)
	g := &fakeGateway{answer: `{"flags": ["other_hosts"], "reason": "acts on farhost"}`}
	j := &fakeJev{chat: g}
	gw := httptest.NewServer(j)
	defer gw.Close()
	e.s.LLM = gatewayClient()
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Safe", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	screen := func(title, desc string) *TaskSafety {
		t.Helper()
		i := e.issue(writer, IssueInput{Project: p.Key, Title: title, Description: desc, Agent: "claude"})
		e.s.bg.Wait()
		return e.get(e.admin, i.Key).Safety
	}

	ts := screen("Tidy the queue code", "Split queue.go's long function.")
	if ts == nil || ts.Verdict != SafetyClear || ts.Model != "LLM gateway · jev-1.14.0" || g.screens() != 0 || j.asked() != 1 {
		t.Fatalf("plain task: %+v, chat screens %d, decisions %d", ts, g.screens(), j.asked())
	}
	b := j.bodies[0]
	if st := fmt.Sprint(b["state"]); !strings.Contains(st, "an AI agent, not reviewed: screen this") || !strings.Contains(st, "Split queue.go") {
		t.Errorf("state:\n%s", st)
	}
	if qs, _ := b["questions"].(map[string]any); len(qs) != len(screenFlags) || !strings.Contains(fmt.Sprint(qs["other_hosts"]), "act on another machine") {
		t.Errorf("questions %v", qs)
	}

	j.yes = map[string]float64{"other_hosts": 0.3}
	ts = screen("Restart the runner", "Restart the runner on farhost.")
	if ts == nil || ts.Verdict != SafetyHold || ts.Reason != "acts on farhost" || g.screens() != 1 {
		t.Fatalf("unsure: %+v, chat screens %d", ts, g.screens())
	}

	j.yes = map[string]float64{"install": 0.4}
	g.answer = `{"flags": ["install"], "reason": "adds a dev dependency"}`
	ts = screen("Add a linter", "Add eslint-plugin-react as a dev dependency.")
	if ts == nil || ts.Verdict != SafetyClear || !slices.Contains(ts.Flags, "install") || g.screens() != 2 {
		t.Fatalf("guarded doubt: %+v, chat screens %d", ts, g.screens())
	}

	j.yes, j.status = nil, 403
	g.answer = `{"flags": [], "reason": ""}`
	ts = screen("Rename a helper", "Rename fooBar to foo.")
	if ts == nil || ts.Verdict != SafetyClear || ts.Model == "LLM gateway · jev-1.14.0" || g.screens() != 3 {
		t.Fatalf("refused: %+v, chat screens %d", ts, g.screens())
	}
}

// A probability the decision model gives out of range fails the ask: every
// caller weighs by them, so a 5 where 0.5 was meant would outvote the
// other asks of a sample (WP-1320's audit).
func TestCheckProbabilities(t *testing.T) {
	for _, c := range []struct {
		name string
		ans  jevAnswer
		ok   bool
	}{
		{"a yes", jevAnswer{Type: "noul", Noul: 0.98, Confidence: 1}, true},
		{"a choice", jevAnswer{Type: "choice", Choice: "high", Confidence: 0.6, Probabilities: map[string]float64{"high": 0.6, "low": 0.4}}, true},
		{"rounding at one", jevAnswer{Probabilities: map[string]float64{"high": 1.0000000001}}, true},
		{"nothing given", jevAnswer{}, true},
		{"a percentage", jevAnswer{Type: "noul", Noul: 98}, false},
		{"a negative confidence", jevAnswer{Confidence: -0.1}, false},
		{"a choice over one", jevAnswer{Probabilities: map[string]float64{"high": 5, "low": 0.4}}, false},
		{"not a number", jevAnswer{Probabilities: map[string]float64{"high": math.NaN()}}, false},
		{"infinite", jevAnswer{Noul: math.Inf(1)}, false},
	} {
		if err := checkProbabilities(map[string]jevAnswer{"q": c.ans}); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// decide fails the ask when the decision model's probability isn't one: a
// percentage (98) or a number past 1 would outvote the other asks of a
// sample, so no caller sees it (WP-1320).
func TestDecideRefusesOutOfRangeProbability(t *testing.T) {
	e := newEnv(t)
	j := &fakeJev{chat: &fakeGateway{}}
	gw := httptest.NewServer(j)
	defer gw.Close()
	e.s.LLM = gatewayClient()
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	qs := map[string]jevQuestion{"refund": {Type: "noul", Instructions: "Does the customer ask for a refund?"}}
	for _, c := range []struct {
		yes float64
		ok  bool
	}{{0.98, true}, {98, false}, {1.5, false}, {-0.2, false}} {
		j.yes = map[string]float64{"refund": c.yes}
		ans, _, err := e.s.decide(e.ctx, "test", "I was charged twice.", qs)
		if (err == nil) != c.ok || (err == nil && ans["refund"].Noul != c.yes) {
			t.Errorf("a probability of %v: answers %+v, err %v; ok want %v", c.yes, ans, err, c.ok)
		}
		if c.ok != (err == nil) || (!c.ok && ans != nil) {
			t.Errorf("a probability of %v still returned answers: %+v", c.yes, ans)
		}
	}
}
