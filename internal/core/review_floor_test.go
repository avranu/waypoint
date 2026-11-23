package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// WP-1440: Waypoint sends code-review work a minimum (reviewFloorBar) and
// every other task none. These tests read the request bodies the gateway got.

func floorEnv(t *testing.T, g *fakeGateway) (*env, []ChainModel) {
	t.Helper()
	e := newEnv(t)
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	e.s.LLM = gatewayClient()
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	pid := e.s.gatewayID(e.ctx)
	g.mu.Lock()
	g.bodies, g.ids = nil, nil
	g.mu.Unlock()
	return e, []ChainModel{{ProviderID: pid, Model: "opencode/muse-spark-*"}, {ProviderID: pid, Model: "free"}, {ProviderID: pid, Model: "opencode/space-bunny-free"}}
}

func (g *fakeGateway) weakestOf(i int) any {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt, _ := g.bodies[i]["routing"].(map[string]any)
	return rt["weakest"]
}

func TestFloorGoesOnReviewVerifyFixCheckReplyAndHeldRequests(t *testing.T) {
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`, bar: "weakest=deepseek-v4.1-flash;model=deepseek-v4.1-flash;quality=70;purpose=review;from=request"}
	e, chain := floorEnv(t, g)
	var out struct {
		Points int `json:"points"`
	}
	// A reviewer's request and its folded fallbacks carry the floor.
	rctx := withGatewayRouting(aiContext(e.ctx, "review"), reviewRouting(7))
	if _, at, err := e.s.runChain(rctx, chain, estimateSystem, "x", estimateSchema, &out, time.Minute); err != nil {
		t.Fatal(err, at)
	} else if len(at) != 1 || at[0].Bar != g.bar {
		t.Errorf("attempt's bar = %+v", at)
	}
	// The verifier, fix check, thread reply and held-edit check too, each
	// under its own request id.
	withFloor := []struct{ task, prefix string }{{"verify_review", "wp-verify-7-"}, {"fix_check", "wp-fixcheck-7-"}, {"reply", "wp-reply-7-"}, {"held_edit", "wp-held-7-"}}
	routings := []*gatewayRouting{verifyRouting(7), fixCheckRouting(7), replyRouting(7), heldRouting(7)}
	for i, c := range withFloor {
		if _, _, err := e.s.runChain(withGatewayRouting(aiContext(e.ctx, c.task), routings[i]), chain, estimateSystem, "x", estimateSchema, &out, time.Minute); err != nil {
			t.Fatal(c.task, err)
		}
	}
	// Other work sends none: no routing at all, or no weakest in it.
	none := []string{"task_safety", "prep", "title", "pr_body", "estimate", "work_log", "automation", "goal"}
	for _, task := range none {
		if _, _, err := e.s.runChain(aiContext(e.ctx, task), chain, estimateSystem, "x", estimateSchema, &out, time.Minute); err != nil {
			t.Fatal(task, err)
		}
	}
	g.mu.Lock()
	n := len(g.bodies)
	ids := append([]string(nil), g.ids...)
	g.mu.Unlock()
	floored := 1 + len(withFloor)
	if n != floored+len(none) {
		t.Fatalf("%d requests", n)
	}
	for i := range floored {
		if got := g.weakestOf(i); got != reviewFloorBar {
			t.Errorf("request %d weakest = %v", i, got)
		}
	}
	if !strings.HasPrefix(ids[0], "wp-review-7-") {
		t.Errorf("reviewer request id %q", ids[0])
	}
	for i, c := range withFloor {
		if !strings.HasPrefix(ids[i+1], c.prefix) {
			t.Errorf("%s request id %q, want %s…", c.task, ids[i+1], c.prefix)
		}
	}
	for i := floored; i < n; i++ {
		if got := g.weakestOf(i); got != nil {
			t.Errorf("request %d (no minimum) sent weakest %v", i, got)
		}
	}
	// The bar the gateway applied is kept on the record, and groups by model.
	var bar string
	if err := e.s.db.R.QueryRow(`SELECT bar FROM ai_calls WHERE task = 'review'`).Scan(&bar); err != nil || bar != g.bar {
		t.Errorf("ai_calls.bar = %q (%v)", bar, err)
	}
	u, err := e.s.AIUsage(e.ctx, e.admin, AIUsageQuery{By: "bar"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range u.Rows {
		got[r.Key] = r.Requests
	}
	// The fake gateway puts the header on every answer, so every request groups under it.
	if len(got) != 1 || got[reviewFloorBar] != floored+len(none) {
		t.Errorf("--by bar = %v", got)
	}
}

func TestOnlyTheGatewayGetsRouting(t *testing.T) {
	// A provider that is not the declared gateway is never handed a
	// routing object, whatever the context holds.
	e := newEnv(t)
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`}
	srv := httptest.NewServer(g)
	defer srv.Close()
	e.s.LLM = gatewayClient()
	p := &AIProvider{Kind: "openai", BaseURL: srv.URL + "/v1", apiKey: "olm_test"}
	if _, _, err := e.s.LLM.Complete(e.ctx, p, "free", "sys", "prompt", estimateSchema); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.bodies[0]["routing"] != nil {
		t.Errorf("routing sent without an ask: %v", g.bodies[0]["routing"])
	}
}

// The verifier's, fix check's, reply's and held-edit check's request ids reach
// no feedback report: the sweep reads review_requests, which only the
// reviewers' answers fill (saveFindings).
func TestOtherReviewChainRequestsReachNoFeedback(t *testing.T) {
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`}
	e, chain := floorEnv(t, g)
	var out struct {
		Points int `json:"points"`
	}
	for i, r := range []*gatewayRouting{verifyRouting(3), fixCheckRouting(3), replyRouting(3), heldRouting(3)} {
		if _, _, err := e.s.runChain(withGatewayRouting(aiContext(e.ctx, "fix_check"), r), chain, estimateSystem, "x", estimateSchema, &out, time.Minute); err != nil {
			t.Fatal(i, err)
		}
	}
	e.s.ReportReviewFeedback(e.ctx)
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM review_requests`).Scan(&n); err != nil || n != 0 {
		t.Errorf("review_requests has %d rows (%v)", n, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.feedback) != 0 {
		t.Errorf("reported on %d requests: %v", len(g.feedback), g.feedback)
	}
}

// A held edit fails closed when the gateway has no model at the bar: the
// check says it could weaken CI, so a person decides. Every request carried
// the bar, and none was sent without it.
func TestHeldEditFailsClosedAtNoModelAtBar(t *testing.T) {
	g := &fakeGateway{answer: `{"weakens": false, "reason": "fine"}`, noBar: 50, retry: "600"}
	e, _ := floorEnv(t, g)
	pr := &PullRequest{ID: 9, Repo: "acme/app", Number: 9}
	h := &HeldEdit{Files: []string{".forgejo/workflows/ci.yml"}, Patch: "-run: make\n+run: make check\n"}
	weakens, why := e.s.weakens(e.ctx, pr, h)
	if !weakens {
		t.Fatalf("a held edit passed with no model at the bar: %q", why)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 {
		t.Fatalf("no request was sent (%q)", why)
	}
	for i, b := range g.bodies {
		if rt, _ := b["routing"].(map[string]any); rt["weakest"] != reviewFloorBar {
			t.Errorf("request %d had no bar: %v", i, b["routing"])
		}
		if !strings.HasPrefix(g.ids[i], "wp-held-9-") {
			t.Errorf("request id %q", g.ids[i])
		}
	}
}

// The bar the gateway reports for an agent task is read off its response.
func TestTaskBarIsRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(barHeader, "weakest=deepseek-v4.1-flash;model=x;quality=70;purpose=audit;from=request")
		w.Write([]byte(`{"id":"t1","status":"running"}`))
	}))
	defer srv.Close()
	c := gatewayClient()
	p := &AIProvider{Kind: "openai", BaseURL: srv.URL + "/v1", apiKey: "olm_test"}
	tk, err := c.taskCall(context.Background(), p, http.MethodGet, "/tasks/t1", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tk.Bar, "weakest=deepseek-v4.1-flash;") {
		t.Errorf("task bar = %q", tk.Bar)
	}
}

// The floor is recorded on an audit only when its chain asks the gateway to
// choose; high and elevated bars are kept; models a person named are not held.
func TestRequestAuditFloor(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	ae.gatewayBox()
	ask := func(in AuditInput) *CodeAudit {
		t.Helper()
		in.Project = ae.p.Key
		a, err := ae.s.RequestAudit(ae.ctx, ae.admin, in)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	for _, c := range []struct {
		name string
		in   AuditInput
		want string
	}{
		{"default chain", AuditInput{}, reviewFloorBar},
		{"gateway named by a person", AuditInput{Models: []string{"gateway"}}, reviewFloorBar},
		{"a footprint audit on the default chain", AuditInput{purpose: AuditFootprint, prContext: "x"}, reviewFloorBar},
		{"high bar kept", AuditInput{weakest: trustedAuditBar}, trustedAuditBar},
		{"elevated bar kept", AuditInput{weakest: elevatedAuditBar}, elevatedAuditBar},
		{"named models: none", AuditInput{Models: []string{"cursor/grok-4.7-high"}}, ""},
		{"named models drop a floor passed in", AuditInput{Models: []string{"cursor/grok-4.7-high"}, weakest: reviewFloorBar}, ""},
		{"named models keep a higher bar", AuditInput{Models: []string{"claude/opus"}, weakest: trustedAuditBar}, trustedAuditBar},
	} {
		if a := ask(c.in); a.Weakest != c.want {
			t.Errorf("%s: weakest = %q, want %q", c.name, a.Weakest, c.want)
		}
	}
	// A rerun keeps what the audit was held to.
	a := ask(AuditInput{})
	if again, err := ae.s.RerunAudit(ae.ctx, ae.admin, a.ID, "cli"); err != nil || again.Weakest != reviewFloorBar {
		t.Errorf("rerun = %+v %v", again, err)
	}
}

// A gateway model other than the audit purpose, named for the audit, is not
// sent the floor.
func TestNamedGatewayModelGetsNoFloor(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	box := ae.gatewayBox()
	b, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway/some-model", "gateway"}})
	if err != nil || b.Weakest != reviewFloorBar {
		t.Fatalf("audit = %+v %v", b, err)
	}
	job := agentJobOf(t, ae.takeOn(box))
	if job.Model != "some-model" || job.GatewayWeakest != "" {
		t.Errorf("the named model's job = %s weakest %q, want no floor", job.Model, job.GatewayWeakest)
	}
}

// A floor round reuses the audits that asked the gateway to choose at the
// floor or above, and one whose models a person named; never one from before
// the floor that asked the gateway to choose with no bar. Another bar takes
// only an audit held to the same.
func TestPRAuditAtHeadByBar(t *testing.T) {
	a, _ := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	var prID, forge int64
	if err := a.s.db.R.QueryRow(`SELECT id, forge_id FROM pull_requests WHERE number = 7`).Scan(&prID, &forge); err != nil {
		t.Fatal(err)
	}
	pr := &PullRequest{ID: prID, HeadSHA: "aaa"}
	const gateway, named = `[{"cli":"gateway","model":"audit"}]`, `[{"cli":"cursor","model":"grok-4.7-high"}]`
	for _, c := range []struct {
		name, round, weakest, models string
		reused                       bool
	}{
		{"floor round, audit at the floor", reviewFloorBar, reviewFloorBar, gateway, true},
		{"floor round, audit at elevated", reviewFloorBar, elevatedAuditBar, gateway, true},
		{"floor round, audit at trusted", reviewFloorBar, trustedAuditBar, gateway, true},
		{"floor round, legacy audit with a gateway entry", reviewFloorBar, "", gateway, false},
		{"floor round, audit of named models", reviewFloorBar, "", named, true},
		{"elevated round, audit at the floor", elevatedAuditBar, reviewFloorBar, gateway, false},
		{"elevated round, audit at elevated", elevatedAuditBar, elevatedAuditBar, gateway, true},
		{"trusted round, audit at elevated", trustedAuditBar, elevatedAuditBar, gateway, false},
		{"no gateway: no bar takes any", "", "", gateway, true},
	} {
		res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, pr_number, ref, status, model, models, author_model, title, owner_id, created_at, weakest)
			VALUES (?, ?, 'acme/app', 'pr', ?, 7, 'aaa', 'done', 'gateway/deepseek/deepseek-v4.1-flash', ?, '', 'acme/app#7', ?, 0, ?)`, a.p.ID, forge, prID, c.models, a.admin.User.ID, c.weakest)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		got, err := a.s.prAuditAtHead(a.ctx, pr, "", c.round, nil)
		if err != nil {
			t.Fatal(err)
		}
		if (got == id) != c.reused {
			t.Errorf("%s: reused = %v, want %v", c.name, got == id, c.reused)
		}
		if _, err := a.s.db.W.Exec(`DELETE FROM code_audits WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
}

// A round's stakes are one thing whether or not a gateway was set when they
// were decided: the floor is stored for a normal change and read back as the
// same bar; a row from before the floor reads as the floor.
func TestRoundStakesBarAgreesWithDecision(t *testing.T) {
	a, _ := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.finish(first.Key)
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	decided, err := a.s.decideStakes(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	read, err := a.s.roundStakes(a.ctx, id)
	if err != nil || read.bar != decided.bar || read.bar != tierBar(decided.tier) {
		t.Errorf("decided %q, read %q (%v)", decided.bar, read.bar, err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE review_stakes SET bar = '' WHERE round_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if read, err := a.s.roundStakes(a.ctx, id); err != nil || read.bar != reviewFloorBar {
		t.Errorf("a legacy row reads %q (%v)", read.bar, err)
	}
}

// The usage grouping keeps the weakest= part of the header, and a call with
// none is grouped under "none".
func TestUsageByBarGroupsWeakest(t *testing.T) {
	e := newEnv(t)
	for _, bar := range []string{"weakest=claude haiku;model=m;quality=80;purpose=audit;from=request", "weakest=claude haiku", ""} {
		e.s.recordAICall(e.ctx, aiCall{task: "review", model: "m", outcome: "ok", bar: bar})
	}
	u, err := e.s.AIUsage(e.ctx, e.admin, AIUsageQuery{By: "bar"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range u.Rows {
		got[r.Key] = r.Requests
	}
	if got["claude haiku"] != 2 || got["none"] != 1 || len(got) != 2 {
		t.Errorf("--by bar = %v", got)
	}
}

// The bar a task was held to is kept on the task, next to its id.
func TestGatewayTaskKeepsItsBar(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	box := ae.gatewayBox()
	ae.runnerSeen(ae.box, false) // no Cursor online: the default audit is the gateway's
	if _, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key}); err != nil {
		t.Fatal(err)
	}
	claim := ae.takeOn(box)
	if job := agentJobOf(t, claim); job.GatewayWeakest != reviewFloorBar {
		t.Fatalf("the job's weakest = %q", job.GatewayWeakest)
	}
	v := ae.startTask(claim.ID)
	var bar string
	if err := ae.s.db.R.QueryRow(`SELECT bar FROM gateway_tasks WHERE task_id = ?`, v.ID).Scan(&bar); err != nil || bar != "weakest="+reviewFloorBar+";from=request" {
		t.Errorf("gateway_tasks.bar = %q (%v)", bar, err)
	}
}
