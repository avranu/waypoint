package core

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestMergeReviewers(t *testing.T) {
	a := []Finding{
		{File: "app/login.go", Line: 11, Severity: "medium", Claim: "The nil check is inverted so every login fails", Model: "m1", Requests: []string{"r1"}},
		{File: "app/login.go", Line: 40, Severity: "low", Claim: "The error from save is ignored", Model: "m1"},
	}
	b := []Finding{
		{File: "login.go", Line: 12, Severity: "high", Claim: "Inverted nil check: every login fails with errNoUser", Model: "m2", Suggestion: "\tif user == nil {", Requests: []string{"r2"}},
		{File: "app/login.go", Line: 41, Severity: "low", Claim: "Timeout is never applied to the request", Model: "m2"},
	}
	c := []Finding{{File: "app/login.go", Line: 10, Severity: "low", Claim: "Every login fails: the nil check is inverted", Model: "m3"}}
	got := mergeReviewers([][]Finding{a, b, c})
	if len(got) != 3 {
		t.Fatalf("merged = %+v", got)
	}
	m := got[0]
	if m.Agreed != 3 || m.Severity != "high" || !slices.Equal(m.Models, []string{"m1", "m2", "m3"}) || m.Suggestion == "" || m.File != "app/login.go" {
		t.Errorf("the agreed finding = %+v", m)
	}
	if !slices.Equal(m.Requests, []string{"r1", "r2"}) || !slices.Equal(a[0].Requests, []string{"r1"}) {
		t.Errorf("the agreed finding's requests = %v (the first reviewer's now %v)", m.Requests, a[0].Requests)
	}
	if got[1].Agreed != 1 || got[2].Agreed != 1 || got[2].Model != "m2" {
		t.Errorf("different problems on near lines stay apart: %+v", got[1:])
	}
	same := mergeReviewers([][]Finding{{a[0], a[0]}})
	if len(same) != 2 || same[0].Agreed != 1 {
		t.Errorf("one reviewer twice = %+v", same)
	}
	if got := reviewerChains([]ChainModel{{ProviderID: 1, Model: "x"}}, [][]ChainModel{nil, {{ProviderID: 1, Model: "x"}}, {{ProviderID: 1, Model: "y"}}, {{ProviderID: 1, Model: "z"}}}, 0); len(got) != 3 || got[1][0].Model != "y" {
		t.Errorf("chains = %+v", got)
	}
	gw := []ChainModel{{ProviderID: 9, Model: "review"}}
	if got := reviewerChains(gw, [][]ChainModel{gw, {{ProviderID: 1, Model: "x"}}, {{ProviderID: 1, Model: "x"}}}, 9); len(got) != 3 || got[1][0].ProviderID != 9 || got[2][0].Model != "x" {
		t.Errorf("gateway chains = %+v", got)
	}
	if extraChain([]any{map[string]any{"provider_id": float64(2), "model": "m"}, map[string]any{"model": ""}}) == nil {
		t.Error("extraChain dropped a good chain")
	}
}

func TestReviewPipeline(t *testing.T) {
	pe := newPREnv(t, "github")
	const muse, nemo, mimo = "opencode/muse-spark-1.3-contributor-free", "opencode/nemotron-3-ultra-free", "opencode/mimo-v2.6-flash-free"
	chain, err := pe.s.taskChain(pe.ctx, "review")
	if err != nil || len(chain) == 0 {
		t.Fatal(chain, err)
	}
	pid := chain[0].ProviderID
	list, err := pe.s.ListAutomations(pe.ctx, pe.admin, pe.p.Key)
	if err != nil {
		t.Fatal(err)
	}
	var rule *Automation
	for _, a := range list {
		if a.Name == "Review pull requests" {
			rule = a
		}
	}
	if rule == nil {
		t.Fatal("no review rule")
	}
	r := rule.Rule
	r.Actions[0].Extra = map[string]any{
		"second_models": []any{map[string]any{"provider_id": float64(pid), "model": nemo}},
		"verify_models": []any{map[string]any{"provider_id": float64(pid), "model": mimo}},
	}
	if _, err := pe.s.UpdateAutomation(pe.ctx, pe.admin, rule.ID, AutomationInput{Rule: &r}); err != nil {
		t.Fatal(err)
	}
	pe.llm.answers[muse] = `{"findings":[
		{"file":"app/login.go","line":11,"severity":"medium","claim":"The nil check is inverted, so every login fails.","suggestion":"\tif user == nil {"},
		{"file":"app/login.go","line":12,"severity":"high","claim":"errNoUser is returned for a user that exists, which leaks whether accounts exist.","suggestion":""}]}`
	pe.llm.answers[nemo] = `{"findings":[
		{"file":"app/login.go","line":11,"severity":"high","claim":"Inverted nil check: every login fails.","suggestion":""},
		{"file":"app/login.go","line":13,"severity":"low","claim":"Login should log the failed attempt.","suggestion":""}]}`
	pe.llm.answers[mimo] = `{"verdicts":[
		{"n":1,"verdict":"keep","reason":"Line 11 tests user != nil.","line":0,"severity":"","claim":""},
		{"n":2,"verdict":"drop","reason":"errNoUser is the same for every failure; nothing leaks.","line":0,"severity":"","claim":""},
		{"n":3,"verdict":"correct","reason":"Not a defect worth more than low.","line":0,"severity":"low","claim":"Failed logins aren't logged."}]}`
	pe.fg.mu.Lock()
	pe.fg.files["app/login.go"] = "package app\n\nfunc Login() error {\n\tuser := find()\n\tif user != nil {\n\t\treturn errNoUser\n\t}\n\treturn nil\n}\n"
	pe.fg.mu.Unlock()

	pr := ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"}
	pe.pullHook("opened", pr)
	done := pe.drain()
	if len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("runs = %+v", done)
	}
	run := done[0]
	var verifyPrompt string
	for n, m := range pe.llm.calls {
		if m == mimo {
			verifyPrompt = pe.llm.prompts[n]
		}
	}
	if !slices.Contains(pe.llm.calls, muse) || !slices.Contains(pe.llm.calls, nemo) || verifyPrompt == "" {
		t.Fatalf("calls = %v", pe.llm.calls)
	}
	if !strings.Contains(verifyPrompt, "5| \tif user != nil {") || !strings.Contains(verifyPrompt, "The change to app/login.go") ||
		!strings.Contains(verifyPrompt, "Finding 1 (high)") {
		t.Errorf("verifier prompt:\n%s", verifyPrompt)
	}
	reviews := pe.fg.reviewsOn(pe.f, "me/app", 5)
	if len(reviews) != 1 {
		t.Fatalf("reviews = %q", reviews)
	}
	body := reviews[0]
	for _, want := range []string{
		"`" + muse + "`, `" + nemo + "`, checked by `" + mimo + "`",
		"2 findings inline (1 raised by more than one reviewer).",
		"<details><summary>1 finding dropped by the verifier</summary>",
		"leaks whether accounts exist",
		"_Dropped: errNoUser is the same for every failure; nothing leaks._",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review body lacks %q:\n%s", want, body)
		}
	}
	lc := pe.fg.lines[0]
	if len(lc) != 2 || lc[0].Line != 11 || !strings.Contains(lc[0].Body, "**P1**") || !strings.Contains(lc[0].Body, "_(raised by 2 reviewers)_") ||
		!strings.Contains(lc[0].Body, "checked by `"+mimo+"`") || !strings.Contains(lc[1].Body, "Failed logins aren't logged.") || !strings.Contains(lc[1].Body, "**P3**") {
		t.Errorf("line comments = %+v", lc)
	}
	if len(run.Findings) != 3 || run.Findings[0].Agreed != 2 || run.Findings[0].VerifiedBy != mimo || run.Findings[1].Agreed != 1 ||
		run.Findings[2].State != FindingDropped || run.Findings[2].Verify != VerifyDropped || run.Findings[0].Verify != VerifyKept ||
		run.Findings[1].Verify != VerifyCorrected || run.Findings[0].Strategy == "" || run.Findings[0].PromptVersion == "" {
		t.Errorf("findings = %+v", run.Findings)
	}
	if !strings.Contains(run.Summary, "checked by "+mimo) || !strings.Contains(run.Summary, "1 dropped by the verifier") {
		t.Errorf("summary = %q", run.Summary)
	}

	pe.llm.answers[mimo] = "!429"
	pr.HeadSHA = "2222222bbbb"
	pe.fg.setPR(pe.f, "me/app", pr, sampleDiff)
	pe.llm.answers[muse] = `{"findings":[{"file":"app/login.go","line":12,"severity":"medium","claim":"A new problem on the second head.","suggestion":""}]}`
	pe.llm.answers[nemo] = `{"findings":[]}`
	pe.pullHook("synchronize", pr)
	if done = pe.drain(); len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("second run = %+v", done)
	}
	reviews = pe.fg.reviewsOn(pe.f, "me/app", 5)
	if last := reviews[len(reviews)-1]; !strings.Contains(last, "1 finding not checked against the code") || strings.Contains(last, "checked by `") {
		t.Errorf("unverified review = %s", last)
	}
	if !strings.Contains(done[0].Summary, "1 not verified") {
		t.Errorf("summary = %q", done[0].Summary)
	}
}

func TestVerifyNewUncheckable(t *testing.T) {
	pe := newPREnv(t, "github")
	const mimo = "opencode/mimo-v2.6-flash-free"
	chain, _ := pe.s.taskChain(pe.ctx, "review")
	vchain := []ChainModel{{ProviderID: chain[0].ProviderID, Model: mimo}}
	pe.fg.mu.Lock()
	pe.fg.files["app/login.go"] = "package app\n\nfunc Login() {}\n"
	pe.fg.mu.Unlock()
	found := []Finding{
		{File: "app/login.go", Line: 3, Severity: "high", Claim: "First."},
		{File: "app/gone.go", Line: 1, Severity: "high", Claim: "On a file the forge can't serve."},
		{File: "app/login.go", Line: 3, Severity: "low", Claim: "Third, never judged."},
	}
	pe.llm.answers[mimo] = `{"verdicts":[{"n":1,"verdict":"keep","reason":"ok","line":0,"severity":"","claim":""}]}`
	cl := pe.s.Forges.(ForgePRs)
	v := pe.s.verifyNew(pe.ctx, vchain, pe.f, cl, "me/app", "abc", found, splitDiff(sampleDiff))
	if len(v.kept) != 3 || v.kept[0].VerifiedBy != mimo || v.kept[1].VerifiedBy != "" || v.kept[2].VerifiedBy != "" || v.unchecked != 2 || !slices.Equal(v.models, []string{mimo}) {
		t.Errorf("verified = %+v", v)
	}
	for _, p := range pe.llm.prompts {
		if strings.Contains(p, "gone.go") {
			t.Errorf("an unreadable file went to the verifier:\n%s", p)
		}
	}
	if v := pe.s.verifyNew(pe.ctx, nil, pe.f, cl, "me/app", "abc", found, nil); len(v.kept) != 3 || v.unchecked != 3 {
		t.Errorf("no verifier = %+v", v)
	}
}

// An answer that judges none of the findings sends the verifier on past
// the models the gateway covered, not one model per attempt: a request to
// the gateway names several.
func TestVerifyNewContinuesPastTheGateway(t *testing.T) {
	pe := newPREnv(t, "github")
	g := &fakeGateway{answer: `{"verdicts":[{"n":9,"verdict":"keep","reason":"not one of them","line":0,"severity":"","claim":""}]}`}
	gw := httptest.NewServer(g)
	defer gw.Close()
	pe.s.LLM = gatewayClient()
	if err := pe.s.EnsureLLMGateway(pe.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	pid := pe.s.gatewayID(pe.ctx)
	pe.fg.mu.Lock()
	pe.fg.files["app/login.go"] = "package app\n\nfunc Login() {}\n"
	pe.fg.mu.Unlock()
	found := []Finding{{File: "app/login.go", Line: 3, Severity: "high", Claim: "First."}}
	cl := pe.s.Forges.(ForgePRs)
	asked := func(models ...string) []map[string]any {
		t.Helper()
		var chain []ChainModel
		for _, m := range models {
			chain = append(chain, ChainModel{ProviderID: pid, Model: m})
		}
		g.mu.Lock()
		g.bodies = nil
		g.mu.Unlock()
		if v := pe.s.verifyNew(pe.ctx, chain, pe.f, cl, "me/app", "abc", found, splitDiff(sampleDiff)); v.unchecked != 1 {
			t.Errorf("verified = %+v", v)
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.bodies
	}
	b := asked("opencode/muse-spark-1.3-contributor-free", "opencode/nemotron-3-ultra-free", "opencode/space-bunny-free", "free")
	if len(b) != 2 || b[1]["model"] != "free" || b[1]["fallbacks"] != nil {
		t.Errorf("asked %d times, last %v then %v", len(b), b[len(b)-1]["model"], b[len(b)-1]["fallbacks"])
	}
	// A route answers as one of its own models: which of the request's
	// models that was is unknown, so all of them count as asked.
	g.mu.Lock()
	g.served = "groq/openai/gpt-oss-120b"
	g.mu.Unlock()
	if b := asked("review-verify", "free"); len(b) != 1 {
		t.Errorf("asked %d times", len(b))
	}
}

// slowRoute is a gateway whose route serves a model that never answers in
// time, until the request leaves that model out.
type slowRoute struct {
	*scriptLLM
	slow string
}

func (l *slowRoute) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	if !slices.Contains(gatewayExcludeOf(ctx), l.slow) {
		l.mu.Lock()
		l.calls = append(l.calls, model+" → "+l.slow)
		l.mu.Unlock()
		return nil, l.slow, context.DeadlineExceeded
	}
	return l.scriptLLM.Complete(ctx, p, model, system, prompt, schema)
}

func (l *slowRoute) CompleteWithFallbacks(ctx context.Context, p *AIProvider, model string, _ []string, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	return l.Complete(ctx, p, model, system, prompt, schema)
}

// A route's model that runs out the call's time is left out of one more
// request, so the part is still reviewed, and the failure is recorded as
// that model's (WP-1709).
func TestReviewAsksAgainWithoutTheModelThatTimedOut(t *testing.T) {
	re := newReviewEnv(t)
	re.asGateway(t)
	const grok = "cursor/grok-4.7-medium"
	re.llm.answers[museModel] = loginFinding
	re.s.LLM = &slowRoute{scriptLLM: re.llm, slow: grok}
	chain, err := re.s.taskChain(re.ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	a := re.s.reviewOnce(re.ctx, chain[:1], "the diff")
	if a.err != nil || len(a.findings) != 1 || a.model != museModel {
		t.Fatalf("answer = %+v", a)
	}
	if len(a.log) != 2 || a.log[0].Model != grok || a.log[0].Outcome != "timeout" || a.log[1].Outcome != "ok" {
		t.Errorf("attempts = %+v", a.log)
	}
	if got := failedMidStream(chain[:1], []ModelAttempt{{Model: chain[0].Model, Outcome: "timeout"}}); got != "" {
		t.Errorf("the chain's own model = %q", got)
	}
	if got := failedMidStream(chain[:1], []ModelAttempt{{Model: grok, Outcome: "rate_limit"}}); got != "" {
		t.Errorf("a rate limit = %q", got)
	}
}
