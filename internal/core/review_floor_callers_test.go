package core

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// WP-1440: the review-chain callers themselves, not runChain alone.

// gatewayWithout is a gateway env that lacks the extensions named: without
// GatewayFallbacks every chain entry is its own request.
func gatewayWithout(t *testing.T, g *fakeGateway, without ...string) (*env, int64) {
	t.Helper()
	e := newEnv(t)
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	e.s.LLM = gatewayClient()
	var ext []string
	for _, x := range GatewayExtensions {
		if !slices.Contains(without, x) {
			ext = append(ext, x)
		}
	}
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: ext}); err != nil {
		t.Fatal(err)
	}
	return e, e.s.gatewayID(e.ctx)
}

func (g *fakeGateway) weakestOfLocked(i int) any {
	if i >= len(g.bodies) {
		return "no request"
	}
	rt, _ := g.bodies[i]["routing"].(map[string]any)
	return rt["weakest"]
}

// With no model at the bar the gateway's remaining entries end (the same
// request would only be refused again) while an entry on another provider is
// still asked. The gateway lacks fallbacks, so each entry is its own request
// and only the skip loop in runChainAt stops the second and third.
func TestNoModelAtBarSkipsOnlyTheGatewayEntries(t *testing.T) {
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`, noBar: 50, retry: "600"}
	e, pid := gatewayWithout(t, g, GatewayFallbacks)
	other := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`, served: "gpt-named"}
	osrv := httptest.NewServer(other)
	t.Cleanup(osrv.Close)
	kind, name, base, key := "openai", "named provider", osrv.URL+"/v1", "olm_test"
	op, err := e.s.CreateAIProvider(e.ctx, e.admin, AIProviderInput{Name: &name, Kind: &kind, BaseURL: &base, APIKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	chain := []ChainModel{{ProviderID: pid, Model: "opencode/muse-spark-1.3-contributor-free"}, {ProviderID: pid, Model: "free"},
		{ProviderID: pid, Model: "opencode/space-bunny-free"}, {ProviderID: op.ID, Model: "gpt-named"}}
	var out struct {
		Points int `json:"points"`
	}
	ctx := withGatewayRouting(aiContext(e.ctx, "review"), reviewRouting(8))
	used, at, err := e.s.runChain(ctx, chain, estimateSystem, "x", estimateSchema, &out, time.Minute)
	if err != nil || used.ProviderID != op.ID {
		t.Fatalf("the other provider's model was not asked: %+v %v (%v)", used, err, at)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) != 1 {
		t.Errorf("%d requests reached the gateway after no_model_at_bar, want 1", len(g.bodies))
	}
	for i := range g.bodies {
		if w := g.weakestOfLocked(i); w != reviewFloorBar {
			t.Errorf("request %d went without the bar: %v", i, w)
		}
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	if len(other.bodies) != 1 || other.bodies[0]["routing"] != nil {
		t.Errorf("the named model's request = %v", other.bodies)
	}
}

// verifyFindings (the fix check) and replyRun carry the floor and their own
// request ids; a fix check with no model at the bar leaves the finding open.
func TestFixCheckAndReplyCallersCarryTheirRouting(t *testing.T) {
	g := &fakeGateway{answer: `{"verdict":"present","reason":"still there"}`}
	se := newShipEnv(t, "github")
	// The scripted reviewer's finding, before the gateway takes over.
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	fs := se.findings(5)
	if len(fs) == 0 {
		t.Fatal("no finding to reply to")
	}
	se.sg.reply(0, "sam", "find() is checked by the caller.")
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	se.s.LLM = gatewayClient()
	if err := se.s.EnsureLLMGateway(se.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	pid := se.s.gatewayID(se.ctx)
	chain := []ChainModel{{ProviderID: pid, Model: "free"}}
	se.sg.fileAt["1111111aaaa:a.go"] = "before\n"
	se.sg.fileAt["2222222bbbb:a.go"] = "after\n"
	rows := []*findingRow{{BotFinding: BotFinding{ID: 1, Path: "a.go", State: FindingOpen, Severity: "high", Claim: "c"}, headSHA: "1111111aaaa"}}
	reset := func() {
		g.mu.Lock()
		g.bodies, g.ids = nil, nil
		g.mu.Unlock()
	}
	reset()
	run := &BotRun{ID: 41, PR: &PullRequest{Repo: "me/app", ForgeID: se.f.ID}}
	out := se.s.verifyFindings(se.ctx, run, se.f, se.sg, &ForgePR{HeadSHA: "2222222bbbb"}, &syncResult{findings: rows}, chain)
	if out.present != 1 {
		t.Fatalf("verifyFindings: %+v", out)
	}
	g.mu.Lock()
	if len(g.bodies) != 1 || g.weakestOfLocked(0) != reviewFloorBar || !strings.HasPrefix(g.ids[0], "wp-fixcheck-41-") {
		t.Errorf("fix check's request: weakest %v, ids %v", g.weakestOfLocked(0), g.ids)
	}
	g.mu.Unlock()

	// No model at the bar: the finding is left as it was.
	reset()
	g.mu.Lock()
	g.noBar = 50
	g.mu.Unlock()
	out = se.s.verifyFindings(se.ctx, run, se.f, se.sg, &ForgePR{HeadSHA: "2222222bbbb"}, &syncResult{findings: rows}, chain)
	if out.checked != 0 || out.fixed != 0 || len(out.log) == 0 || out.log[0].Outcome != "no_model_at_bar" {
		t.Errorf("fix check with no model at the bar: %+v", out)
	}
	if rows[0].State != FindingOpen {
		t.Errorf("finding state = %q, want it left open", rows[0].State)
	}
	g.mu.Lock()
	g.noBar = 0
	g.mu.Unlock()

	// The thread reply, through replyRun itself.
	g.mu.Lock()
	g.answer = `{"action":"stand_by","reply":"The caller does not check it."}`
	g.mu.Unlock()
	reset()
	opts, _ := json.Marshal(reviewOptions{FindingID: fs[0].ID, Models: chain})
	rrun := &BotRun{ID: 55, ProjectID: se.p.ID, PR: se.pr(5), Kind: "reply", options: string(opts)}
	if _, err := se.s.replyRun(se.ctx, rrun); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	if len(g.bodies) != 1 || g.weakestOfLocked(0) != reviewFloorBar || !strings.HasPrefix(g.ids[0], "wp-reply-55-") {
		t.Errorf("reply's request: %d bodies, weakest %v, ids %v", len(g.bodies), g.weakestOfLocked(0), g.ids)
	}
	g.mu.Unlock()
}

// Work that is not code review sends no minimum, through its real callers.
func TestNonReviewCallersSendNoMinimum(t *testing.T) {
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine", "flags": [], "summary": "ok"}`}
	e, _ := floorEnv(t, g)
	pid := e.s.gatewayID(e.ctx)
	for _, task := range []string{"estimate", "task_safety", "prep"} {
		if err := e.s.SetAITask(e.ctx, e.admin, task, pid, "free", nil); err != nil {
			t.Fatal(task, err)
		}
	}
	p := e.project(e.admin, "Alpha", "kanban")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Do a thing"})
	seen := func(what string, from int) int {
		t.Helper()
		g.mu.Lock()
		defer g.mu.Unlock()
		if len(g.bodies) <= from {
			t.Errorf("%s: no request reached the gateway", what)
			return len(g.bodies)
		}
		for n := from; n < len(g.bodies); n++ {
			if w := g.weakestOfLocked(n); w != nil {
				t.Errorf("%s sent weakest %v", what, w)
			}
		}
		return len(g.bodies)
	}
	if _, err := e.s.EstimateIssue(e.ctx, e.admin, i.Key); err != nil {
		t.Fatal(err)
	}
	n := seen("estimate", 0)
	_, _, _ = e.s.screenByModel(e.ctx, i.Key, &screenInput{title: "Do a thing", desc: "text", textAgent: "bot"})
	n = seen("task safety", n)
	var out struct {
		Points int `json:"points"`
	}
	pctx, chain, err := e.s.prepChain(e.ctx, i.Key, "normal")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.s.runAIWith(pctx, "prep", chain, estimateSystem, "x", estimateSchema, &out)
	seen("prep", n)
}

// A review run whose every reviewer gets no_model_at_bar ends at once, counts
// toward the reviews-down streak, and says why.
func TestReviewRunAtNoModelAtBarCountsTowardReviewsDown(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	g := &fakeGateway{answer: oneFinding, noBar: 1000}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	pe.s.LLM = gatewayClient()
	if err := pe.s.EnsureLLMGateway(pe.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	pid := pe.s.gatewayID(pe.ctx)
	if err := pe.s.SetAITask(pe.ctx, pe.admin, "review", pid, "free", nil); err != nil {
		t.Fatal(err)
	}
	notices := func() int {
		var n int
		_ = pe.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE kind = ? AND title LIKE 'Reviews in%'`, NotifyPR).Scan(&n)
		return n
	}
	for n := 1; n <= reviewsDownAfter; n++ {
		g.mu.Lock()
		g.bodies, g.ids = nil, nil
		g.mu.Unlock()
		pe.pullHook("opened", ForgePR{Number: 20 + n, Title: "Fix", Author: "sam", HeadRef: fmt.Sprintf("fix%d", n), HeadSHA: fmt.Sprintf("aaaaaa%d1111", n), State: "open"})
		done := pe.drain()
		if len(done) != 1 || done[0].Status != "failed" {
			t.Fatalf("run %d: %+v", n, done)
		}
		run := done[0]
		if !strings.HasPrefix(run.Error, providersDownText) || !strings.Contains(run.Error, reviewFloorBar) || !strings.Contains(run.Error, "no model at the review minimum") {
			t.Errorf("run %d error = %q", n, run.Error)
		}
		g.mu.Lock()
		reqs := len(g.bodies)
		g.mu.Unlock()
		if reqs != 1 {
			t.Errorf("run %d sent %d requests, want 1", n, reqs)
		}
		pe.s.Wait()
		if n < reviewsDownAfter && notices() != 0 {
			t.Fatalf("told after %d runs", n)
		}
	}
	var title, body string
	if err := pe.s.db.R.QueryRow(`SELECT title, body FROM notifications WHERE kind = ? AND title LIKE 'Reviews in%' LIMIT 1`, NotifyPR).Scan(&title, &body); err != nil {
		t.Fatalf("no reviews-down notice after %d runs: %v", reviewsDownAfter, err)
	}
	if !strings.Contains(body, reviewFloorBar) {
		t.Errorf("notice reads %q / %q", title, body)
	}
	if runs := pe.runs(); len(runs) != reviewsDownAfter {
		t.Errorf("%d runs, want %d (none requeued)", len(runs), reviewsDownAfter)
	}
}

// The verifier with no model at the bar keeps the findings, unchecked: the
// reviewers' work is posted, not lost.
func TestVerifierAtNoModelAtBarKeepsFindings(t *testing.T) {
	pe := newPREnv(t, "github")
	g := &fakeGateway{answer: `{"verdicts":[]}`, noBar: 50}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	pe.s.LLM = gatewayClient()
	if err := pe.s.EnsureLLMGateway(pe.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	pid := pe.s.gatewayID(pe.ctx)
	pe.fg.mu.Lock()
	pe.fg.files["app/login.go"] = "package app\n\nfunc Login() {}\n"
	pe.fg.mu.Unlock()
	found := []Finding{{File: "app/login.go", Line: 3, Severity: "high", Claim: "First."}}
	ctx := withGatewayRouting(aiContext(pe.ctx, "verify_review"), verifyRouting(4))
	v := pe.s.verifyNew(ctx, []ChainModel{{ProviderID: pid, Model: "free"}}, pe.f, pe.s.Forges.(ForgePRs), "me/app", "abc", found, splitDiff(sampleDiff))
	if len(v.kept) != 1 || v.unchecked != 1 || len(v.dropped) != 0 {
		t.Errorf("verified = %+v", v)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 || g.weakestOfLocked(0) != reviewFloorBar || !strings.HasPrefix(g.ids[0], "wp-verify-4-") {
		t.Errorf("verifier's request: weakest %v ids %v", g.weakestOfLocked(0), g.ids)
	}
}

func TestNoModelAtBarNeedsStatusAndCode(t *testing.T) {
	body := `{"error":{"message":"no model at the bar","type":"server_error","code":"no_model_at_bar"}}`
	for _, c := range []struct {
		e    llmError
		want bool
	}{
		{llmError{status: 503, body: body}, true},
		{llmError{status: 500, body: body}, false},
		{llmError{status: 503, body: `{"error":{"message":"the model said no_model_at_bar","code":"server"}}`}, false},
		{llmError{status: 503, body: "no_model_at_bar"}, false},
	} {
		if got := c.e.noModelAtBar(); got != c.want {
			t.Errorf("%d %s: %v, want %v", c.e.status, c.e.body, got, c.want)
		}
	}
}

// A no_model_at_bar that carries a Retry-After (some models at the bar are
// only busy) is asked again with the same bar, and the answer is used.
func TestNoModelAtBarWithRetryAfterIsAskedAgain(t *testing.T) {
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`, noBar: 1, retry: "0"}
	e, pid := gatewayWithout(t, g, GatewayFallbacks)
	chain := []ChainModel{{ProviderID: pid, Model: "free"}}
	var out struct {
		Points int `json:"points"`
	}
	ctx := withGatewayRouting(aiContext(e.ctx, "review"), reviewRouting(8))
	if _, _, err := e.s.runChain(ctx, chain, estimateSystem, "x", estimateSchema, &out, time.Minute); err != nil || out.Points != 5 {
		t.Fatalf("not answered after the busy refusal: %+v %v", out, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(g.bodies))
	}
	for i := range g.bodies {
		if w := g.weakestOfLocked(i); w != reviewFloorBar {
			t.Errorf("request %d went without the bar: %v", i, w)
		}
	}
}
