package core

import (
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// surplusEnv is a project whose agent queue runner "box" prepares, with
// an LLM gateway with every extension.
func surplusEnv(t *testing.T) (*env, *fakeGateway, *Project) {
	t.Helper()
	e := newEnv(t)
	g := &fakeGateway{answer: fmt.Sprintf(prepModelAnswer, "APP-1")}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	e.s.LLM = gatewayClient()
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "App", "")
	p.Settings.AutoPrep = true
	if _, err := e.s.UpdateProject(e.ctx, e.admin, p.Key, ProjectInput{Settings: &p.Settings}); err != nil {
		t.Fatal(err)
	}
	ag := agentActor(e.admin, "anthropic/claude-code")
	if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "s1", "SessionStart", nil)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ? WHERE cli_id = 's1'`, p.ID); err != nil {
		t.Fatal(err)
	}
	e.runner(e.admin, "box", nil)
	return e, g, p
}

func (g *fakeGateway) last() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 {
		return nil
	}
	return g.bodies[len(g.bodies)-1]
}

func (g *fakeGateway) set(f func(*fakeGateway)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g)
}

func routingOf(body map[string]any) map[string]any {
	r, _ := body["routing"].(map[string]any)
	return r
}

func (e *env) one(q string, args ...any) string {
	e.t.Helper()
	var s string
	if err := e.s.db.R.QueryRowContext(e.ctx, q, args...).Scan(&s); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return s
}

// The tasks behind a queue's front are prepared at surplus while the gateway
// has spare capacity; one an agent gets is counted used; a refusal is no
// failure, holds surplus work only, and leaves the task for a normal prep.
func TestPrepAheadAtSurplus(t *testing.T) {
	e, g, p := surplusEnv(t)
	for n := range 5 {
		e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprintf("Task %d", n)})
	}
	e.setClock(e.clockNow().Add(15 * time.Minute))
	due, err := e.s.PrepDue(e.ctx, e.admin, "box", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 5 {
		t.Fatalf("due = %d tasks; want the 3 at the front and the 2 behind", len(due))
	}
	front, ahead := due[0].Key, due[3].Key

	if _, err := e.s.UploadPrep(e.ctx, e.admin, front, prepTestBundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if r := routingOf(g.last()); r["priority"] != nil {
		t.Errorf("a front prep asked for priority %v", r["priority"])
	}
	if _, err := e.s.UploadPrep(e.ctx, e.admin, ahead, prepTestBundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	body := g.last()
	if r := routingOf(body); r["priority"] != "surplus" || r["paid"] != "never" || body["model"] != gatewayPrepPurpose {
		t.Fatalf("prep ahead asked %v for %v; want surplus, never paid, the prep purpose", r, body["model"])
	}
	id := e.issueID(ahead)
	if got := e.one(`SELECT priority || ' ' || state FROM task_preps WHERE issue_id = ?`, id); got != "surplus ready" {
		t.Fatalf("prep ahead = %s", got)
	}
	if got := e.one(`SELECT position || ':' || outcome FROM prep_ahead WHERE issue_id = ?`, id); got != "1:" {
		t.Fatalf("prep_ahead = %q", got)
	}
	ag := agentActor(e.admin, "anthropic/claude-code")
	if _, err := e.s.GetPrep(e.ctx, ag, ahead, true); err != nil {
		t.Fatal(err)
	}
	if got := e.one(`SELECT outcome FROM prep_ahead WHERE issue_id = ?`, id); got != "used" {
		t.Fatalf("taken while current: %q", got)
	}

	g.set(func(g *fakeGateway) { g.status = 503 })
	refused := due[4].Key
	if _, err := e.s.UploadPrep(e.ctx, e.admin, refused, prepTestBundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	rid := e.issueID(refused)
	if got := e.one(`SELECT COUNT(*) FROM task_preps WHERE issue_id = ?`, rid); got != "0" {
		t.Errorf("a refused prep left %s rows: it is no failed prep", got)
	}
	if got := e.one(`SELECT outcome FROM prep_ahead WHERE issue_id = ?`, rid); got != "refused" {
		t.Errorf("prep_ahead = %q", got)
	}
	if got := e.one(`SELECT outcome FROM ai_calls WHERE priority = 'surplus' ORDER BY id DESC LIMIT 1`); got != "refused" {
		t.Errorf("ai_calls outcome = %q", got)
	}
	if e.s.surplusAllowed(e.ctx) {
		t.Error("surplus work is still sent after a refusal")
	}
	g.set(func(g *fakeGateway) { g.status = 0 })
	for _, d := range due[1:3] {
		if _, err := e.s.UploadPrep(e.ctx, e.admin, d.Key, prepTestBundle()); err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
		if got := e.one(`SELECT priority || ' ' || state FROM task_preps WHERE issue_id = ?`, e.issueID(d.Key)); got != "normal ready" {
			t.Fatalf("a front prep while surplus waits: %s", got)
		}
	}
	again, err := e.s.PrepDue(e.ctx, e.admin, "box", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("while surplus waits: %+v; want none (the refused task is behind the front)", again)
	}
	rep, err := e.s.SurplusReport(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if txt := rep.String(); !strings.Contains(txt, "held now") || !strings.Contains(txt, "1 ok, 1 refused") {
		t.Errorf("report:\n%s", txt)
	}
	e.setClock(e.clockNow().Add(surplusRetry + time.Second))
	if !e.s.surplusAllowed(e.ctx) {
		t.Error("surplus work still held after the wait")
	}
	again, err = e.s.PrepDue(e.ctx, e.admin, "box", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].Key != refused {
		t.Errorf("after the wait: %+v; want the refused task again", again)
	}
}

func (e *env) issueID(key string) int64 {
	e.t.Helper()
	i, err := e.s.GetIssue(e.ctx, e.admin, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return i.ID
}

// A position behind the front is dropped once enough of its preps are
// decided and their use is surely under the bar; refusals don't count.
func TestPrepAheadDepth(t *testing.T) {
	e, _, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "T"})
	add := func(pos int, outcome string, n int) {
		for range n {
			if _, err := e.s.db.W.Exec(`INSERT INTO prep_ahead(issue_id, position, outcome, created_at, decided_at) VALUES (?,?,?,?,?)`,
				i.ID, pos, outcome, e.s.nowMS(), e.s.nowMS()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if d := e.s.prepAheadDepth(e.ctx); d != prepAheadMax {
		t.Fatalf("no evidence: depth %d", d)
	}
	add(1, "used", 6)
	add(1, "redone", 6)
	add(2, "unused", 11)
	add(2, "refused", 30)
	if d := e.s.prepAheadDepth(e.ctx); d != prepAheadMax {
		t.Fatalf("0 of 11 used (upper bound %.2f): depth %d", wilsonUpper(0, 11), d)
	}
	add(2, "redone", 1)
	if d := e.s.prepAheadDepth(e.ctx); d != 1 {
		t.Fatalf("0 of 12 used (upper bound %.2f): depth %d, want 1", wilsonUpper(0, 12), d)
	}
	e.setClock(e.clockNow().Add(prepAheadWindow + time.Hour))
	if d := e.s.prepAheadDepth(e.ctx); d != prepAheadMax {
		t.Fatalf("after the window: depth %d", d)
	}
}

func TestWilson(t *testing.T) {
	for _, c := range []struct {
		k, n     int
		lo, hi   float64
		toleance float64
	}{{0, 0, 0, 1, 0}, {0, 12, 0, 0.243, 0.005}, {5, 10, 0.237, 0.763, 0.005}, {10, 10, 0.722, 1, 0.005}} {
		if lo, hi := wilsonLower(c.k, c.n), wilsonUpper(c.k, c.n); math.Abs(lo-c.lo) > c.toleance || math.Abs(hi-c.hi) > c.toleance {
			t.Errorf("%d of %d: %.3f-%.3f, want %.3f-%.3f", c.k, c.n, lo, hi, c.lo, c.hi)
		}
	}
}

// Preps and trials ask for the gateway's prep purpose under a request id;
// trials go at surplus to models other than the prep's and the tested ones;
// when the work merges each is scored against the files it changed, by the
// model that answered, and reported to the gateway once.
func TestPrepModels(t *testing.T) {
	e, g, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "T"})

	ctx, chain, err := e.s.prepChain(e.ctx, i.Key, "normal")
	if err != nil {
		t.Fatal(err)
	}
	names := func(c []ChainModel) []string {
		var out []string
		for _, m := range c {
			out = append(out, m.Model)
		}
		return out
	}
	if r := gatewayRoutingOf(ctx); !slices.Equal(names(chain), []string{gatewayPrepPurpose, gatewayFreeRoute}) || r == nil || r.prefix != "wp-prep-"+i.Key || r.priority != "" {
		t.Fatalf("normal prep: chain %v, routing %+v", names(chain), r)
	}
	ctx, chain, err = e.s.prepChain(e.ctx, i.Key, prioritySurplus)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(chain), []string{gatewayPrepPurpose, gatewayFreeRoute}) || priorityOf(ctx) != prioritySurplus {
		t.Fatalf("surplus prep: chain %v, priority %q", names(chain), priorityOf(ctx))
	}

	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Other"})
	score := func(model string, n, picked, hits int) {
		for k := range n {
			if _, err := e.s.db.W.Exec(`INSERT INTO prep_scores(issue_id, source, priority, model, picked, truth, hits, scored_at, request_id, asked_at, reported_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`, other.ID, fmt.Sprintf("trial%d", k), prioritySurplus, model, picked, 2, hits, e.s.nowMS(),
				fmt.Sprintf("old-%s-%d", model, k), e.s.nowMS(), e.s.nowMS()); err != nil {
				t.Fatal(err)
			}
		}
	}
	score("tested/a", 10, 2, 1)
	score("new/c", 3, 2, 1)
	if _, err := e.s.db.W.Exec(`INSERT INTO prep_scores(issue_id, source, priority, model, picked, truth, hits, scored_at, request_id, asked_at)
		VALUES (?, 'prep', 'normal', 'new/c', 2, 2, 1, ?, 'gone', ?)`, other.ID, e.s.nowMS(), e.clockNow().Add(-prepFeedbackAge-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	e.setClock(e.clockNow().Add(15 * time.Minute))
	g.set(func(g *fakeGateway) { g.served = "opencode/space-bunny-free" })
	if _, err := e.s.UploadPrep(e.ctx, e.admin, i.Key, prepTestBundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if body := g.last(); body["model"] != gatewayPrepPurpose || fmt.Sprint(body["fallbacks"]) != "["+gatewayFreeRoute+"]" || routingOf(body)["priority"] != nil {
		t.Fatalf("prep asked %v (fallbacks %v, routing %v)", body["model"], body["fallbacks"], routingOf(body))
	}
	prepID := e.one(`SELECT request_id FROM task_preps WHERE issue_id = ?`, i.ID)
	if !strings.HasPrefix(prepID, "wp-prep-"+i.Key+"-") {
		t.Fatalf("prep request id %q", prepID)
	}
	g.set(func(g *fakeGateway) { g.served = "new/d" })
	e.s.RunPrepTrials(e.ctx)
	body := g.last()
	ex := fmt.Sprint(body["exclude"])
	if body["model"] != gatewayPrepPurpose || routingOf(body)["priority"] != "surplus" || !strings.Contains(ex, "opencode/space-bunny-free") ||
		!strings.Contains(ex, "tested/a") || strings.Contains(ex, "new/c") {
		t.Fatalf("trial asked %v at %v, excluding %s", body["model"], routingOf(body), ex)
	}
	trialID := e.one(`SELECT request_id FROM prep_trials WHERE issue_id = ? AND kind = 'chat'`, i.ID)
	if got := e.one(`SELECT model FROM prep_trials WHERE issue_id = ? AND kind = 'chat'`, i.ID); got != "new/d" || !strings.HasPrefix(trialID, "wp-trial-"+i.Key+"-") {
		t.Fatalf("trial by %q as %q", got, trialID)
	}
	g.mu.Lock()
	sent := slices.Clone(g.ids)
	g.mu.Unlock()
	if !slices.Contains(sent, prepID) || !slices.Contains(sent, trialID) {
		t.Fatalf("requests sent as %q; kept %q, %q", sent, prepID, trialID)
	}

	f := e.forge("forgejo", "https://git.example.com", false)
	url := "https://git.example.com/acme/app/pulls/3"
	for _, q := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, []any{p.ID, f.ID}},
		{`INSERT INTO pull_requests(forge_id, repo, number, url, state, head_sha, files_sha, files, merged_at, created_at, updated_at) VALUES (?, 'acme/app', 3, ?, 'merged', 'h1', 'h1', ?, ?, 0, 0)`,
			[]any{f.ID, url, `{"paths":["internal/merge.go","internal/gate.go","internal/merge_test.go","docs/x.md"]}`, e.clockNow().Add(time.Hour).UnixMilli()}},
		{`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/app', 3, 'merged', 0)`, []any{i.ID, url}},
	} {
		if _, err := e.s.db.W.Exec(q.q, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	done := "done"
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	url4 := "https://git.example.com/acme/app/pulls/4"
	if _, err := e.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, head_sha, created_at, updated_at) VALUES (?, 'acme/app', 4, ?, 'open', 'h4', 0, 0)`, f.ID, url4); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/app', 4, 'open', 0)`, i.ID, url4); err != nil {
		t.Fatal(err)
	}
	e.s.ScorePreps(e.ctx)
	if n := e.one(`SELECT COUNT(*) FROM prep_scores WHERE issue_id = ?`, i.ID); n != "0" {
		t.Fatalf("scored %s with a pull request still open", n)
	}
	if _, err := e.s.db.W.Exec(`UPDATE pull_requests SET state = 'closed' WHERE number = 4`); err != nil {
		t.Fatal(err)
	}
	e.s.ScorePreps(e.ctx)
	rows, err := e.s.db.R.Query(`SELECT source || ' ' || priority || ' ' || model || ' ' || picked || '/' || truth || '/' || hits || ' ' || request_id
		FROM prep_scores WHERE issue_id = ? ORDER BY source`, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		got = append(got, s)
	}
	rows.Close()
	want := []string{"prep normal opencode/space-bunny-free 1/2/1 " + prepID, "trial surplus new/d 1/2/1 " + trialID}
	if !slices.Equal(got, want) {
		t.Fatalf("scores = %q, want %q", got, want)
	}
	e.s.ScorePreps(e.ctx)
	if n := e.one(`SELECT COUNT(*) FROM prep_scores WHERE issue_id = ?`, i.ID); n != "2" {
		t.Errorf("after a second sweep: %s scores", n)
	}

	e.s.ReportReviewFeedback(e.ctx)
	e.s.ReportReviewFeedback(e.ctx)
	g.mu.Lock()
	reports := slices.Clone(g.feedback)
	g.mu.Unlock()
	var sentReports []string
	for _, r := range reports {
		sentReports = append(sentReports, fmt.Sprintf("%v %v/%v", r["request_id"], r["verified"], r["false_positives"]))
	}
	slices.Sort(sentReports)
	wantReports := []string{prepID + " 1/0", trialID + " 1/0"}
	slices.Sort(wantReports)
	if !slices.Equal(sentReports, wantReports) {
		t.Fatalf("reports = %q, want %q", sentReports, wantReports)
	}
	if n := e.one(`SELECT COUNT(*) FROM prep_scores WHERE issue_id = ? AND reported_at IS NOT NULL`, i.ID); n != "2" {
		t.Errorf("%s reports remembered", n)
	}

	later := e.clockNow().Add(2 * time.Hour).UnixMilli()
	if _, err := e.s.db.W.Exec(`UPDATE prep_scores SET scored_at = ? WHERE issue_id = ?`, later-1, i.ID); err != nil {
		t.Fatal(err)
	}
	e.s.ScorePreps(e.ctx)
	if n := e.one(`SELECT COUNT(*) FROM prep_scores WHERE issue_id = ? AND scored_at = ?`, i.ID, later-1); n != "2" {
		t.Fatalf("scored again with nothing merged since: %s left as they were", n)
	}
	url5 := "https://git.example.com/acme/app/pulls/5"
	if _, err := e.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, head_sha, files_sha, files, merged_at, created_at, updated_at) VALUES (?, 'acme/app', 5, ?, 'merged', 'h5', 'h5', ?, ?, 0, 0)`,
		f.ID, url5, `{"paths":["internal/merge.go"]}`, later); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/app', 5, 'merged', 0)`, i.ID, url5); err != nil {
		t.Fatal(err)
	}
	e.s.ScorePreps(e.ctx)
	if n := e.one(`SELECT COUNT(*) FROM prep_scores WHERE issue_id = ? AND scored_at != ? AND reported_at IS NOT NULL`, i.ID, later-1); n != "2" {
		t.Fatalf("after a later merge: %s scored again with their reports standing", n)
	}
	e.s.ReportReviewFeedback(e.ctx)
	g.mu.Lock()
	if len(g.feedback) != len(reports) {
		t.Errorf("reports sent again: %v", g.feedback[len(reports):])
	}
	g.mu.Unlock()

	stats, err := e.s.PrepModelStats(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]PrepModelStat{}
	for _, st := range stats {
		by[st.Model] = st
	}
	if a := by["tested/a"]; !a.Tested || a.Scored != 10 || a.Verified != 10 || a.FalsePositives != 10 || a.Reported != 10 {
		t.Errorf("tested/a: %+v", a)
	}
	if c := by["new/c"]; c.Tested || c.Pending != 0 {
		t.Errorf("new/c: %+v", c)
	}
	if d := by["new/d"]; d.Scored != 1 || d.Reported != 1 || d.Pending != 0 || d.Waiting != 0 {
		t.Errorf("new/d: %+v", d)
	}
	rep, err := e.s.SurplusReport(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if s := rep.String(); !strings.Contains(s, "tested/a") || !strings.Contains(s, "tested, no more trials") {
		t.Errorf("report:\n%s", s)
	}
}

// A trial that keeps no picks (it failed, or a model left out answered)
// is a try: a prep gets prepTrialsPerTask tries, not one every sweep.
func TestPrepTrialTries(t *testing.T) {
	e, g, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "T"})
	e.setClock(e.clockNow().Add(15 * time.Minute))
	g.set(func(g *fakeGateway) { g.served = "opencode/space-bunny-free" })
	if _, err := e.s.UploadPrep(e.ctx, e.admin, i.Key, prepTestBundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	calls := func() int {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.bodies)
	}
	e.s.RunPrepTrials(e.ctx)
	if got := e.one(`SELECT model || ' ' || error FROM prep_trials WHERE issue_id = ? AND kind = 'chat'`, i.ID); got != "opencode/space-bunny-free served a model left out" {
		t.Fatalf("trial = %q", got)
	}
	g.set(func(g *fakeGateway) { g.served, g.answer = "new/d", "not json" })
	e.s.RunPrepTrials(e.ctx)
	if got := e.one(`SELECT COUNT(*) FROM prep_trials WHERE issue_id = ? AND kind = 'chat' AND model = '' AND error != ''`, i.ID); got != "1" {
		t.Fatalf("failed tries = %s", got)
	}
	before := calls()
	e.s.RunPrepTrials(e.ctx)
	if n := calls() - before; n != 0 {
		t.Errorf("a prep out of tries was asked again (%d requests)", n)
	}
	stats, err := e.s.PrepModelStats(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range stats {
		if st.Waiting != 0 {
			t.Errorf("a try that kept nothing waits to be scored: %+v", st)
		}
	}
}

// A surplus prep that failed never holds up the task's normal prep once
// it reaches the front; behind the front it waits as any failed prep.
func TestFailedPrepAheadAtFront(t *testing.T) {
	e, _, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "T"})
	e.setClock(e.clockNow().Add(15 * time.Minute))
	if _, err := e.s.db.W.Exec(`INSERT INTO task_preps(issue_id, text_hash, repo, commit_sha, machine, state, bundle, notes, model, dropped, error, held_out, priority, created_at, updated_at)
		VALUES (?, 'h', '', '', 'box', 'failed', '{}', '', '', 0, 'bad output', 0, ?, ?, ?)`, i.ID, prioritySurplus, e.s.nowMS(), e.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	iss, err := e.s.issueByID(e.ctx, e.s.db.R, i.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if due, err := e.s.prepIsDue(e.ctx, iss, e.s.now(), true); err != nil || !due {
		t.Errorf("at the front: due %v, %v", due, err)
	}
	if due, err := e.s.prepIsDue(e.ctx, iss, e.s.now(), false); err != nil || due {
		t.Errorf("behind the front: due %v, %v", due, err)
	}
}

// Another upload while a prep made ahead is pending (a second runner, the
// mark used up) stays that prep ahead, at surplus, with its outcome row.
func TestPrepAheadUploadedTwice(t *testing.T) {
	e, g, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "T"})
	e.setClock(e.clockNow().Add(15 * time.Minute))
	if _, err := e.s.db.W.Exec(`INSERT INTO task_preps(issue_id, text_hash, repo, commit_sha, machine, state, bundle, notes, model, dropped, error, held_out, priority, created_at, updated_at)
		VALUES (?, 'h', '', '', 'box', 'pending', '{}', '', '', 0, '', 0, ?, ?, ?)`, i.ID, prioritySurplus, e.s.nowMS(), e.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO prep_ahead(issue_id, position, created_at) VALUES (?, 2, ?)`, i.ID, e.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UploadPrep(e.ctx, e.admin, i.Key, prepTestBundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if r := routingOf(g.last()); r["priority"] != "surplus" {
		t.Errorf("asked at %v", r)
	}
	if got := e.one(`SELECT priority || ' ' || state FROM task_preps WHERE issue_id = ?`, i.ID); got != "surplus ready" {
		t.Errorf("prep = %s", got)
	}
	if got := e.one(`SELECT COUNT(*) || ' ' || MAX(position) || ':' || MAX(outcome) FROM prep_ahead WHERE issue_id = ?`, i.ID); got != "1 2:" {
		t.Errorf("prep_ahead = %s", got)
	}
}

// Notes that can't be read are kept with no truth (not scored as no
// picks, nothing reported), and the task's other picks are still scored.
func TestScorePrepUnreadableNotes(t *testing.T) {
	e, _, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "T"})
	f := e.forge("forgejo", "https://git.example.com", false)
	url := "https://git.example.com/acme/app/pulls/3"
	merged := e.clockNow().Add(time.Hour).UnixMilli()
	at := e.s.nowMS()
	for _, q := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, []any{p.ID, f.ID}},
		{`INSERT INTO pull_requests(forge_id, repo, number, url, state, head_sha, files_sha, files, merged_at, created_at, updated_at) VALUES (?, 'acme/app', 3, ?, 'merged', 'h1', 'h1', ?, ?, 0, 0)`,
			[]any{f.ID, url, `{"paths":["internal/merge.go","internal/gate.go"]}`, merged}},
		{`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/app', 3, 'merged', 0)`, []any{i.ID, url}},
		{`INSERT INTO task_preps(issue_id, text_hash, repo, commit_sha, machine, state, bundle, notes, model, dropped, error, held_out, priority, request_id, created_at, updated_at)
			VALUES (?, 'h', '', '', 'box', 'ready', '{}', '{not json', 'P · old/a', 0, '', 0, 'normal', 'req-prep', ?, ?)`, []any{i.ID, at, at}},
		{`INSERT INTO prep_trials(issue_id, model, notes, request_id, created_at) VALUES (?, 'new/d', ?, 'req-trial', ?)`,
			[]any{i.ID, `{"files":[{"path":"internal/merge.go"}]}`, at}},
	} {
		if _, err := e.s.db.W.Exec(q.q, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.scorePrep(e.ctx, i.ID, merged); err != nil {
		t.Fatal(err)
	}
	if got := e.one(`SELECT group_concat(source || ' ' || model || ' ' || picked || '/' || truth || '/' || hits || ' ' || request_id, '; ') FROM (SELECT * FROM prep_scores WHERE issue_id = ? ORDER BY source)`, i.ID); got != "prep old/a 0/0/0 ; trial new/d 1/2/1 req-trial" {
		t.Errorf("scores = %q", got)
	}
}

// A task PrepDue hands out at the front gets a normal prep, even while a
// prep made ahead of it is still pending at surplus.
func TestPrepAheadPendingAtFront(t *testing.T) {
	e, g, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "T"})
	e.setClock(e.clockNow().Add(15 * time.Minute))
	if _, err := e.s.db.W.Exec(`INSERT INTO task_preps(issue_id, text_hash, repo, commit_sha, machine, state, bundle, notes, model, dropped, error, held_out, priority, created_at, updated_at)
		VALUES (?, 'h', '', '', 'box', 'pending', '{}', '', '', 0, '', 0, ?, ?, ?)`, i.ID, prioritySurplus, e.s.nowMS(), e.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	e.s.markPrepAhead(i.ID, 1, false)
	if _, err := e.s.UploadPrep(e.ctx, e.admin, i.Key, prepTestBundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if r := routingOf(g.last()); r["priority"] != nil {
		t.Errorf("asked at %v", r)
	}
	if got := e.one(`SELECT priority || ' ' || state FROM task_preps WHERE issue_id = ?`, i.ID); got != "normal ready" {
		t.Errorf("prep = %s", got)
	}
}

// Surplus work needs a gateway with routes and routing: a prep made ahead
// before they were taken away goes at normal priority on the task's chain,
// and a chain asked at surplus skips a gateway with routing alone, which
// would serve a pinned model at normal priority.
func TestSurplusNeedsRoutes(t *testing.T) {
	e, g, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "T"})
	if err := e.s.setGatewayExtensions(e.ctx, []string{GatewayRouting, GatewayFallbacks}); err != nil {
		t.Fatal(err)
	}
	ctx, chain, err := e.s.prepChain(e.ctx, i.Key, prioritySurplus)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := e.s.taskChain(e.ctx, "prep")
	if priorityOf(ctx) == prioritySurplus || !slices.Equal(chain, want) {
		t.Errorf("surplus prep without routes: priority %q, chain %v", priorityOf(ctx), chain)
	}
	sent := func() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.bodies) }
	before := sent()
	var out map[string]any
	sctx := withGatewayRouting(e.ctx, surplusRouting("wp-test-"))
	if _, _, _, err := e.s.runChainAt(sctx, want, "system", "prompt", nil, &out, time.Minute); !errors.Is(err, errNoModel) || sent() != before {
		t.Errorf("surplus chain on a routing-only gateway = %v, %d requests", err, sent()-before)
	}
}
