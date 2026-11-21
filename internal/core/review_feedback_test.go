package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// asGateway makes the review env's provider the gateway.
func (re *reviewEnv) asGateway(t *testing.T) {
	t.Helper()
	chain, err := re.s.taskChain(re.ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	if err := declareGateway(re.s, chain[0].ProviderID); err != nil {
		t.Fatal(err)
	}
}

func (re *reviewEnv) reports(t *testing.T) []feedbackReport {
	t.Helper()
	re.s.ReportReviewFeedback(re.ctx)
	re.llm.mu.Lock()
	defer re.llm.mu.Unlock()
	out := re.llm.reports
	re.llm.reports = nil
	return out
}

// A review's request to the gateway carries its own id and asks for paid
// models ranked with the free ones; the verifier's carries neither. The
// finding remembers the request, and each change in how its findings fared
// is reported once.
func TestReviewFeedback(t *testing.T) {
	re := newReviewEnv(t) // diff+files, then the verifier runs
	re.asGateway(t)
	re.llm.answers[museModel] = loginFinding
	re.llm.answers[mimoModel] = `{"verdicts":[{"n":1,"verdict":"keep","reason":"yes","line":0,"severity":"","claim":""}]}`
	run := re.review(t)
	if len(run.Findings) != 1 {
		t.Fatalf("findings = %+v", run.Findings)
	}
	var id string
	for n, m := range re.llm.calls {
		a := re.llm.asks[n]
		switch m {
		case museModel:
			if !strings.HasPrefix(a.requestID, "wp-review-"+itoa64(run.ID)+"-") || a.paid != "allowed" || a.weakest != reviewFloorBar {
				t.Errorf("review request carried %+v", a)
			}
			id = a.requestID
		case mimoModel:
			if !strings.HasPrefix(a.requestID, "wp-verify-"+itoa64(run.ID)+"-") || a.weakest != reviewFloorBar || a.paid != "" {
				t.Errorf("verifier request carried %+v", a)
			}
		}
	}
	var linked string
	if err := re.s.db.R.QueryRow(`SELECT request_id FROM review_finding_requests WHERE finding_id = ?`, run.Findings[0].ID).Scan(&linked); err != nil || linked != id {
		t.Fatalf("finding's request = %q (%v), want %q", linked, err, id)
	}

	// Open: nothing to report.
	if got := re.reports(t); len(got) != 0 {
		t.Errorf("reported an open finding: %+v", got)
	}
	// Dismissed: one false positive, reported once.
	if _, err := re.s.DismissFinding(re.ctx, re.admin, run.Findings[0].ID, "the check is right"); err != nil {
		t.Fatal(err)
	}
	if got := re.reports(t); len(got) != 1 || got[0] != (feedbackReport{id, 0, 1, false}) {
		t.Errorf("after dismissing: %+v", got)
	}
	if got := re.reports(t); len(got) != 0 {
		t.Errorf("reported again with nothing changed: %+v", got)
	}
	// Reopened: the report is withdrawn (both counts 0).
	if _, err := re.s.ReopenFinding(re.ctx, re.admin, run.Findings[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := re.reports(t); len(got) != 1 || got[0] != (feedbackReport{id, 0, 0, false}) {
		t.Errorf("after reopening: %+v", got)
	}
	// An agent took it in its thread: verified.
	if _, err := re.s.db.W.Exec(`INSERT INTO review_comments(pr_id, kind, ext_id, finding_id, verdict, created_at, updated_at, seen_at) VALUES (?, 'finding', 'f1', ?, ?, 1, 1, 1)`,
		run.PR.ID, run.Findings[0].ID, VerdictAccepted); err != nil {
		t.Fatal(err)
	}
	if got := re.reports(t); len(got) != 1 || got[0] != (feedbackReport{id, 1, 0, false}) {
		t.Errorf("after an agent accepted it: %+v", got)
	}

	// A gateway that is down keeps the report for later.
	if _, err := re.s.db.W.Exec(`UPDATE bot_findings SET state = ? WHERE id = ?`, FindingFixed, run.Findings[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := re.s.db.W.Exec(`UPDATE review_requests SET verified = 0`); err != nil {
		t.Fatal(err)
	}
	re.llm.report = &llmError{status: 502, body: "down"}
	if got := re.reports(t); len(got) != 0 {
		t.Errorf("reports while down: %+v", got)
	}
	var tries int
	var next int64
	if err := re.s.db.R.QueryRow(`SELECT tries, next_try_at FROM review_requests WHERE request_id = ?`, id).Scan(&tries, &next); err != nil || tries != 1 || next <= re.s.nowMS() {
		t.Errorf("after a failure: tries %d next %d (%v)", tries, next, err)
	}
	re.llm.report = nil
	if got := re.reports(t); len(got) != 0 {
		t.Errorf("retried before its time: %+v", got)
	}
	if _, err := re.s.db.W.Exec(`UPDATE review_requests SET next_try_at = 0`); err != nil {
		t.Fatal(err)
	}
	if got := re.reports(t); len(got) != 1 || got[0] != (feedbackReport{id, 1, 0, false}) {
		t.Errorf("after the gateway came back: %+v", got)
	}
}

// A finding whose quoted code isn't in the file is a false positive from
// the start; a request the gateway never knew is given up after an hour.
func TestReviewFeedbackEvidenceAndUnknown(t *testing.T) {
	re := newReviewEnv(t)
	re.asGateway(t)
	re.llm.answers[museModel] = `{"findings":[{"file":"app/login.go","line":5,"severity":"high","claim":"Made up.","suggestion":"","evidence":"if user == nil { panic() }"}]}`
	re.llm.answers[mimoModel] = `{"verdicts":[]}`
	run := re.review(t)
	if run.Findings[0].State != FindingDropped {
		t.Fatalf("finding = %+v", run.Findings[0])
	}
	re.llm.report = &llmError{status: 404, body: `{"error":{"code":"request_not_found"}}`}
	if got := re.reports(t); len(got) != 0 {
		t.Fatal(got)
	}
	// Young: tried again later. Old: not within the window.
	var id string
	var created, next int64
	if err := re.s.db.R.QueryRow(`SELECT request_id, created_at, next_try_at FROM review_requests`).Scan(&id, &created, &next); err != nil {
		t.Fatal(err)
	}
	if next-re.s.nowMS() > (time.Hour).Milliseconds() {
		t.Errorf("a young unknown request waits %v", time.Duration(next-re.s.nowMS())*time.Millisecond)
	}
	if _, err := re.s.db.W.Exec(`UPDATE review_requests SET next_try_at = 0, created_at = ?`, re.s.nowMS()-2*time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	re.reports(t)
	if err := re.s.db.R.QueryRow(`SELECT next_try_at FROM review_requests`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next < re.s.nowMS()+20*24*time.Hour.Milliseconds() {
		t.Errorf("an old unknown request is tried again at %d", next)
	}
	re.llm.report = nil
	if _, err := re.s.db.W.Exec(`UPDATE review_requests SET next_try_at = 0`); err != nil {
		t.Fatal(err)
	}
	if got := re.reports(t); len(got) != 1 || got[0] != (feedbackReport{id, 0, 1, false}) {
		t.Errorf("evidence not in the file: %+v", got)
	}
}

// The HTTP client puts the ask on the wire: X-Request-ID, routing.paid,
// and POST /v1/feedback with the same key.
func TestGatewayFeedbackClient(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		ids, bodies = append(ids, r.URL.Path+" "+r.Header.Get("X-Request-ID")+" "+r.Header.Get("Authorization")), append(bodies, body)
		if r.URL.Path == "/v1/feedback" {
			w.Write([]byte(`{"request_id":"x"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"{\"points\": 3, \"confidence\": \"high\", \"reason\": \"ok\"}"}}]}`))
	}))
	defer srv.Close()
	c := gatewayClient()
	p := &AIProvider{Kind: "openai", BaseURL: srv.URL + "/v1", apiKey: "olm_test"}
	ctx := context.Background()
	if _, _, err := c.Complete(ctx, p, "free", "sys", "prompt", estimateSchema); err != nil {
		t.Fatal(err)
	}
	ctx = withGatewayAsk(ctx, &gatewayAsk{requestID: "wp-review-7-abc", paid: "allowed"})
	ctx = withGatewayExclude(ctx, []string{"opencode/muse-spark-1.3"})
	if _, _, err := c.Complete(ctx, p, "review", "sys", "prompt", estimateSchema); err != nil {
		t.Fatal(err)
	}
	if err := c.Feedback(context.Background(), p, feedbackReport{requestID: "wp-review-7-abc", verified: 2, falsePos: 3}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ids[0] != "/v1/chat/completions  Bearer olm_test" || bodies[0]["routing"] != nil {
		t.Errorf("a plain request: %q %v", ids[0], bodies[0]["routing"])
	}
	if ids[1] != "/v1/chat/completions wp-review-7-abc Bearer olm_test" {
		t.Errorf("asked as %q", ids[1])
	}
	if r, _ := bodies[1]["routing"].(map[string]any); r["paid"] != "allowed" {
		t.Errorf("routing = %v", bodies[1]["routing"])
	}
	if ex := fmt.Sprint(bodies[1]["exclude"]); ex != "[opencode/muse-spark-1.3 */muse-spark-1.3]" || bodies[0]["exclude"] != nil {
		t.Errorf("exclude = %s (a plain request: %v)", ex, bodies[0]["exclude"])
	}
	if ids[2] != "/v1/feedback  Bearer olm_test" || bodies[2]["request_id"] != "wp-review-7-abc" || bodies[2]["verified"] != 2.0 || bodies[2]["false_positives"] != 3.0 {
		t.Errorf("feedback %q %v", ids[2], bodies[2])
	}
	// An answer that raised nothing says so, with no counts (0 and 0
	// would withdraw a report).
	mu.Unlock()
	err := c.Feedback(context.Background(), p, feedbackReport{requestID: "wp-review-7-def", noFindings: true})
	mu.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if b := bodies[3]; len(b) != 2 || b["request_id"] != "wp-review-7-def" || b["no_findings"] != true {
		t.Errorf("no findings: %v", b)
	}
}

// A review answer that raised no findings is known by its request alone,
// and reported once as no_findings (WP-1230).
func TestReviewFeedbackNoFindings(t *testing.T) {
	re := newReviewEnv(t)
	re.asGateway(t)
	re.llm.answers[museModel] = `{"findings":[]}`
	run := re.review(t)
	if len(run.Findings) != 0 {
		t.Fatalf("findings = %+v", run.Findings)
	}
	var id string
	if err := re.s.db.R.QueryRow(`SELECT request_id FROM review_requests WHERE run_id = ?`, run.ID).Scan(&id); err != nil || !strings.HasPrefix(id, "wp-review-"+itoa64(run.ID)+"-") {
		t.Fatalf("request = %q (%v)", id, err)
	}
	if got := re.reports(t); len(got) != 1 || got[0] != (feedbackReport{id, 0, 0, true}) {
		t.Fatalf("reports = %+v", got)
	}
	if got := re.reports(t); len(got) != 0 {
		t.Errorf("reported again: %+v", got)
	}
}

func TestGatewayRoutingID(t *testing.T) {
	r := reviewRouting(12345)
	a, b := r.newID(), r.newID()
	if a == b || !strings.HasPrefix(a, "wp-review-12345-") || len(a) > 64 {
		t.Errorf("ids %q %q", a, b)
	}
	if strings.Trim(a, "-0123456789abcdefghijklmnopqrstuvwxyz") != "" {
		t.Errorf("%q has characters other than token ones", a)
	}
}
