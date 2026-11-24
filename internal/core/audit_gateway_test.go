package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Audits the LLM gateway runs as agent tasks (WP-617).

// taskLLM is a gateway that takes agent tasks and reports on them, over
// whatever LLM the environment had.
type taskLLM struct {
	LLM
	mu      sync.Mutex
	specs   []gatewayTaskSpec
	sizes   []int64 // the archive bytes each task came with
	refuse  []error // what the next CreateTask calls answer instead of a task
	tasks   map[string]*gatewayTask
	taskErr error // what Task answers instead
	cancels []string
	reports []feedbackReport
	report  error
	// limits and limitsErr are what the key's task_limits answer;
	// limitsCalls counts the asks.
	limits      GatewayLimits
	limitsErr   error
	limitsCalls int
	// limitsMeanwhile runs once, between an ask's answer and its return:
	// what another caller does while this ask is on its way back.
	limitsMeanwhile func()
}

func (l *taskLLM) TaskLimits(ctx context.Context, p *AIProvider) (GatewayLimits, error) {
	l.mu.Lock()
	l.limitsCalls++
	lim, err, meanwhile := l.limits, l.limitsErr, l.limitsMeanwhile
	l.limitsMeanwhile = nil
	l.mu.Unlock()
	if meanwhile != nil {
		meanwhile()
	}
	return lim, err
}

func (l *taskLLM) CreateTask(ctx context.Context, p *AIProvider, spec gatewayTaskSpec, archive io.Reader, size int64) (*gatewayTask, error) {
	n, _ := io.Copy(io.Discard, archive)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.specs, l.sizes = append(l.specs, spec), append(l.sizes, n)
	if len(l.refuse) > 0 {
		err := l.refuse[0]
		l.refuse = l.refuse[1:]
		return nil, err
	}
	t := &gatewayTask{ID: fmt.Sprintf("task_%d", len(l.tasks)+1), Status: "queued"}
	if spec.Weakest != "" {
		t.Bar = "weakest=" + spec.Weakest + ";from=request" // the header a bar gets
	}
	l.tasks[t.ID] = t
	return t, nil
}

func (l *taskLLM) Task(ctx context.Context, p *AIProvider, id string) (*gatewayTask, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.taskErr != nil {
		return nil, l.taskErr
	}
	t, ok := l.tasks[id]
	if !ok {
		return nil, &gatewayRefusal{status: http.StatusNotFound, code: "not_found", msg: "no such task"}
	}
	c := *t
	return &c, nil
}

func (l *taskLLM) CancelTask(ctx context.Context, p *AIProvider, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancels = append(l.cancels, id)
	return nil
}

func (l *taskLLM) Feedback(ctx context.Context, p *AIProvider, r feedbackReport) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.report != nil {
		return l.report
	}
	l.reports = append(l.reports, r)
	return nil
}

// ends is the gateway ending a task.
func (l *taskLLM) ends(id, status, servedBy, kind string, answer any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.tasks[id]
	t.Status, t.ServedBy = status, servedBy
	if answer != nil {
		raw, _ := json.Marshal(answer)
		t.Result = &struct {
			Text string          `json:"text"`
			JSON json.RawMessage `json:"json"`
		}{Text: "(as JSON)", JSON: raw}
	}
	if kind != "" {
		t.Error = &struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		}{Kind: kind, Message: "the gateway says " + kind}
	}
	t.Usage = &struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}{PromptTokens: 9000, CompletionTokens: 700}
}

func (l *taskLLM) sent() (int, gatewayTaskSpec) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.specs) == 0 {
		return 0, gatewayTaskSpec{}
	}
	return len(l.specs), l.specs[len(l.specs)-1]
}

func (l *taskLLM) reported() []feedbackReport {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.reports
	l.reports = nil
	return out
}

// taskGateway makes the service's LLM a gateway that takes agent tasks,
// and one of its providers the gateway.
func taskGateway(t *testing.T, s *Service) *taskLLM {
	t.Helper()
	var pid int64
	if err := s.db.R.QueryRow(`SELECT id FROM ai_providers ORDER BY id LIMIT 1`).Scan(&pid); err != nil {
		name, kind, base, key := "LLM gateway", "openai", "https://llm.example/v1", "olm_test"
		p, err := s.CreateAIProvider(context.Background(), SystemActor(), AIProviderInput{Name: &name, Kind: &kind, BaseURL: &base, APIKey: &key})
		if err != nil {
			t.Fatal(err)
		}
		pid = p.ID
	}
	if err := declareGateway(s, pid); err != nil {
		t.Fatal(err)
	}
	l := &taskLLM{LLM: s.LLM, tasks: map[string]*gatewayTask{}}
	s.LLM = l
	return l
}

// runnerSeen sets when a runner last polled: the clock's now (online) or the
// beginning of time (registered, offline).
func (e *env) runnerSeen(r *Runner, online bool) {
	e.t.Helper()
	at := int64(0)
	if online {
		at = e.s.nowMS()
	}
	if _, err := e.s.db.W.Exec(`UPDATE runners SET last_seen_at = ? WHERE id = ?`, at, r.ID); err != nil {
		e.t.Fatal(err)
	}
}

// gatewayBox is a runner of the admin's whose wp can send an audit to the
// gateway, and has no CLI.
func (ae *agentEnv) gatewayBox() *Runner {
	return ae.runner(ae.admin, "gwbox", func(r *RunnerRegistration) { r.Features = []string{FeatureAudit, FeatureGatewayAudit} })
}

// takeOn is that runner taking the next job.
func (ae *agentEnv) takeOn(r *Runner) *RunnerClaim {
	ae.t.Helper()
	ae.work()
	w := ae.rpoll(ae.admin, r)
	if w.Job == nil {
		ae.t.Fatal("no job was queued for " + r.Name)
	}
	return w.Job
}

func (ae *agentEnv) startTask(job int64) *GatewayTaskView {
	ae.t.Helper()
	archive := []byte("a packed checkout")
	v, err := ae.s.StartGatewayTask(ae.ctx, ae.admin, job, "Audit this.\n\nThe change: …", bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		ae.t.Fatal(err)
	}
	return v
}

func (ae *agentEnv) task(job int64) *GatewayTaskView {
	ae.t.Helper()
	v, err := ae.s.GatewayTask(ae.ctx, ae.admin, job)
	if err != nil {
		ae.t.Fatal(err)
	}
	return v
}

var gatewayFinding = map[string]any{"title": "Merge reads a nil map", "path": "internal/merge.go", "line": 10, "severity": "high", "problem": "A problem", "confirm": "Merge twice"}

// A person's audit names the gateway; a runner with no CLI hands the
// checkout over, Waypoint sends it on with what the job said (the model,
// the answer's form), and the audit is the model's that ran it. How its
// findings fare is reported to the gateway by task id.
func TestAuditThroughGateway(t *testing.T) {
	ae := newAgentEnv(t)
	if _, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway"}}); err == nil || !strings.Contains(err.Error(), "no LLM gateway that runs agent tasks") {
		t.Fatalf("no gateway: %v", err)
	}
	gw := taskGateway(t, ae.s)
	if _, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway"}}); err == nil || !strings.Contains(err.Error(), "send an audit to the gateway") {
		t.Fatalf("no runner that can: %v", err)
	}
	box := ae.gatewayBox()
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway", "claude/opus"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Models, ",") != "gateway/audit,claude/opus" {
		t.Fatalf("models = %v", a.Models)
	}
	ae.work()
	// The runner with the CLIs has no wp for it; the one without a CLI does.
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatalf("a runner without the feature took %+v", w.Job)
	}
	claim := ae.takeOn(box)
	in := agentJobOf(t, claim)
	// The job's time is the gateway's four hours and more, and the
	// helpers' own (their panel runs beside the model, in the runner).
	if claim.Kind != JobAudit || claim.Timeout <= gatewayAuditTimeout || claim.Timeout > runnerAuditMaxTimeout || in.CLI != GatewayCLI || in.Model != "audit" || in.JSONSchema == "" || len(in.GatewayExclude) != 0 || !in.ReadOnly || in.GatewayArchiveMax != GatewayArchiveMax || in.GatewayFileMax != GatewayFileMax {
		t.Fatalf("job = %s %ds %+v", claim.Kind, claim.Timeout, in)
	}

	// Another person's runner can't speak for the job.
	sam := ae.user("sam", true)
	if _, err := ae.s.StartGatewayTask(ae.ctx, sam, claim.ID, "x", strings.NewReader("x"), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("another person's job: %v", err)
	}
	if _, err := ae.s.GatewayTask(ae.ctx, ae.admin, claim.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a job with no task yet: %v", err)
	}
	for why, call := range map[string]func() error{
		"no prompt": func() error {
			_, err := ae.s.StartGatewayTask(ae.ctx, ae.admin, claim.ID, " ", strings.NewReader("x"), 1)
			return err
		},
		"too large": func() error {
			_, err := ae.s.StartGatewayTask(ae.ctx, ae.admin, claim.ID, "x", strings.NewReader("x"), GatewayArchiveMax+1)
			return err
		},
	} {
		var inv *InvalidError
		if err := call(); !errors.As(err, &inv) {
			t.Errorf("%s: %v", why, err)
		}
	}

	v := ae.startTask(claim.ID)
	n, spec := gw.sent()
	if v.Status != "queued" || v.ID != "task_1" || n != 1 || spec.Model != "audit" || spec.Schema != in.JSONSchema || spec.Timeout != gatewayTaskRunMax ||
		!strings.Contains(spec.Prompt, "The change") || gw.sizes[0] != int64(len("a packed checkout")) {
		t.Fatalf("view = %+v, sent %d %+v (%v bytes)", v, n, spec, gw.sizes)
	}
	gw.ends("task_1", "running", "", "", nil)
	if v := ae.task(claim.ID); v.Status != "running" || v.ID != "task_1" || v.JSON != nil {
		t.Fatalf("running = %+v", v)
	}
	gw.ends("task_1", "succeeded", "deepseek/deepseek-v4.1-flash", "", map[string]any{"summary": "Read the merge.", "findings": []any{gatewayFinding,
		map[string]any{"title": "A lock is held across a call", "path": "internal/queue.go", "line": 4, "severity": "medium", "problem": "Another", "confirm": "Read it"}}})
	v = ae.task(claim.ID)
	if v.Status != GatewayTaskSucceeded || v.Model != "deepseek/deepseek-v4.1-flash" || !strings.Contains(string(v.JSON), "nil map") || v.InputTokens != 9000 || v.OutputTokens != 700 {
		t.Fatalf("succeeded = %+v", v)
	}
	// What the runner reports of it (cmd/wp execGatewayAudit).
	var structured any
	_ = json.Unmarshal(v.JSON, &structured)
	ae.finish(claim.ID, "done", map[string]any{"answer": v.Text, "model": v.Model, "gateway_task": v.ID, "structured": structured,
		"tokens": map[string]any{"input": v.InputTokens, "output": v.OutputTokens}}, "")
	got := ae.audit(a.ID)
	if got.Status != AuditDone || got.Model != "gateway/deepseek/deepseek-v4.1-flash" || len(got.Findings) != 2 || got.Tokens["input"] == nil {
		t.Fatalf("audit = %s by %q: %+v (%s)", got.Status, got.Model, got.Findings, got.Error)
	}
	if label := auditModelLabel(got.Model); label != "deepseek/deepseek-v4.1-flash (through the gateway)" {
		t.Errorf("label = %q", label)
	}

	// Nothing is reported until a finding is decided; then each change once.
	ae.s.ReportReviewFeedback(ae.ctx)
	if r := gw.reported(); len(r) != 0 {
		t.Fatalf("reported undecided findings: %+v", r)
	}
	mark := func(i int, state string) {
		t.Helper()
		if _, err := ae.s.MarkAuditFindings(ae.ctx, ae.admin, MarkFindings{IDs: []string{got.Findings[i].ID}, State: state, Note: "checked"}); err != nil {
			t.Fatal(err)
		}
		ae.s.ReportReviewFeedback(ae.ctx)
	}
	mark(0, "fixed")
	if r := gw.reported(); len(r) != 1 || r[0] != (feedbackReport{"task_1", 1, 0, false}) {
		t.Fatalf("after a fix: %+v", r)
	}
	ae.s.ReportReviewFeedback(ae.ctx)
	if r := gw.reported(); len(r) != 0 {
		t.Fatalf("reported the same counts twice: %+v", r)
	}
	mark(1, "rejected")
	if r := gw.reported(); len(r) != 1 || r[0] != (feedbackReport{"task_1", 1, 1, false}) {
		t.Fatalf("after a rejection: %+v", r)
	}
	// A gateway that doesn't know the task yet is asked again later.
	gw.report = &llmError{status: http.StatusNotFound, body: `{"error":{"code":"request_not_found"}}`}
	mark(1, "confirmed")
	gw.report = nil
	ae.s.ReportReviewFeedback(ae.ctx)
	if r := gw.reported(); len(r) != 0 {
		t.Fatalf("asked again before its time: %+v", r)
	}
	ae.setClock(ae.clockNow().Add(feedbackRetryMax + time.Minute))
	ae.s.ReportReviewFeedback(ae.ctx)
	if r := gw.reported(); len(r) != 1 || r[0] != (feedbackReport{"task_1", 2, 0, false}) {
		t.Fatalf("after the wait: %+v", r)
	}
}

// What the gateway says went wrong decides what the chain does: busy and
// spent are the gateway's own state, which its next model name isn't asked
// about again; a model it refuses this key is left alone for an hour; and
// an answer from the family that wrote the code is no answer.
func TestGatewayAuditFailures(t *testing.T) {
	ae := newAgentEnv(t)
	gw := taskGateway(t, ae.s)
	box := ae.gatewayBox()
	request := func(models ...string) *CodeAudit {
		t.Helper()
		a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: models})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	fail := func(job int64, kind string) {
		t.Helper()
		ae.finish(job, "failed", map[string]any{"gateway_error": kind, "answer": "not in the form"}, "The audit through the gateway failed ("+kind+")")
	}

	// Busy at the gateway: its other name isn't tried, the next CLI is, and
	// the whole chain goes again later.
	a := request("gateway", "gateway/free", "claude/opus")
	claim := ae.takeOn(box)
	if v := ae.startTask(claim.ID); v.Status != "queued" {
		t.Fatalf("view = %+v", v)
	}
	fail(claim.ID, "rate_limit")
	claim = ae.takeOn(ae.box)
	if in := agentJobOf(t, claim); in.CLI != "claude" || len(in.GatewayExclude) != 0 {
		t.Fatalf("after a busy gateway = %+v", in)
	}
	ae.finish(claim.ID, "failed", map[string]any{}, "rate limit exceeded")
	got := ae.audit(a.ID)
	if got.Status != AuditQueued || got.Retries != 1 || len(got.Attempts) != 3 || got.Attempts[0].Outcome != "busy" || got.Attempts[1].Outcome != "busy" ||
		got.Attempts[1].Job != 0 || !strings.Contains(got.Attempts[1].Error, "busy for gateway/audit") {
		t.Fatalf("waiting audit = %s retries %d %+v", got.Status, got.Retries, got.Attempts)
	}

	// The second pass: the gateway has no such model for this key, which is
	// remembered; its free models are out of allowance; Claude answers.
	ae.setClock(ae.clockNow().Add(16 * time.Minute))
	ae.s.CheckAudits(ae.ctx)
	claim = ae.takeOn(box)
	gw.refuse = []error{&gatewayRefusal{status: http.StatusNotFound, code: "not_found", msg: "no model named audit"}}
	if v := ae.startTask(claim.ID); v.Status != GatewayTaskRefused || v.ErrorKind != "not_found" || v.RetryAfter != 0 {
		t.Fatalf("refused = %+v", v)
	}
	fail(claim.ID, "not_found")
	claim = ae.takeOn(box)
	if in := agentJobOf(t, claim); in.CLI != GatewayCLI || in.Model != "free" {
		t.Fatalf("the gateway's other model = %+v", in)
	}
	gw.refuse = []error{&gatewayRefusal{status: http.StatusTooManyRequests, code: "quota", msg: "the key's daily allowance is spent"}}
	if v := ae.startTask(claim.ID); v.Status != GatewayTaskRefused || v.ErrorKind != "quota" || v.RetryAfter != 0 {
		t.Fatalf("spent = %+v", v)
	}
	fail(claim.ID, "quota")
	claim = ae.takeOn(ae.box)
	ae.finish(claim.ID, "done", auditResult("Read it.", gatewayFinding), "")
	got = ae.audit(a.ID)
	if got.Status != AuditDone || got.Model != "claude/opus" || got.Attempts[3].Outcome != "model" || got.Attempts[4].Outcome != "spent" {
		t.Fatalf("audit = %s by %q %+v", got.Status, got.Model, got.Attempts)
	}
	ae.s.ReportReviewFeedback(ae.ctx)
	if _, err := ae.s.MarkAuditFindings(ae.ctx, ae.admin, MarkFindings{IDs: []string{got.Findings[0].ID}, State: "fixed"}); err != nil {
		t.Fatal(err)
	}
	ae.s.ReportReviewFeedback(ae.ctx)
	if r := gw.reported(); len(r) != 0 {
		t.Errorf("a task that gave no findings was reported on: %+v", r)
	}

	// Within the hour the refused model is passed over without a job.
	b := request("gateway", "claude/opus")
	if in := agentJobOf(t, ae.takeOn(ae.box)); in.CLI != "claude" {
		t.Fatalf("after a refused model = %+v", in)
	}
	if got := ae.audit(b.ID); len(got.Attempts) != 1 || got.Attempts[0].Outcome != "unavailable" || !strings.Contains(got.Attempts[0].Error, "no model named audit") {
		t.Fatalf("attempts = %+v", got.Attempts)
	}
	if _, err := ae.s.CancelAudit(ae.ctx, ae.admin, b.ID); err != nil {
		t.Fatal(err)
	}
	ae.setClock(ae.clockNow().Add(gatewayRefusedFor + time.Minute))

	// With only the gateway in the chain, busy still means later, not failed.
	c := request("gateway", "gateway/free")
	claim = ae.takeOn(box)
	fail(claim.ID, "queue_full")
	if got := ae.audit(c.ID); got.Status != AuditQueued || got.Retries != 1 || got.RetryAt == nil || len(got.Attempts) != 2 {
		t.Fatalf("a busy gateway alone = %s retries %d %+v (%s)", got.Status, got.Retries, got.Attempts, got.Error)
	}
	if _, err := ae.s.CancelAudit(ae.ctx, ae.admin, c.ID); err != nil {
		t.Fatal(err)
	}

	// An answer out of form is asked once more, then kept, and the limit's
	// advice doesn't speak of Cursor when Cursor wasn't asked.
	d := request("gateway")
	claim = ae.takeOn(box)
	fail(claim.ID, "invalid_output")
	if got := ae.audit(d.ID); got.Status == AuditFailed || len(got.Attempts) != 1 || got.Attempts[0].Outcome != "unreadable" {
		t.Fatalf("out of form once = %s %+v", got.Status, got.Attempts)
	}
	claim = ae.takeOn(box)
	fail(claim.ID, "invalid_output")
	if got := ae.audit(d.ID); got.Status != AuditFailed || len(got.Attempts) != 2 || got.Attempts[1].Outcome != "unreadable" || got.Answer != "not in the form" {
		t.Fatalf("out of form twice = %s %+v %q", got.Status, got.Attempts, got.Answer)
	}
	e := request("gateway")
	claim = ae.takeOn(box)
	fail(claim.ID, "quota")
	if got := ae.audit(e.ID); got.Status != AuditLimited || strings.Contains(got.Error, "Cursor") || !strings.Contains(got.Error, "limits reset") {
		t.Fatalf("limited = %s %q", got.Status, got.Error)
	}
}

// The relay's own answers: the models the job leaves out, tasks that are
// lost or can't be asked about, the job's time, and stopping.
func TestGatewayTaskRelay(t *testing.T) {
	ae := newAgentEnv(t)
	gw := taskGateway(t, ae.s)
	box := ae.gatewayBox()
	// As a review round asks: the session's model wrote the code.
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Via: "system", purpose: AuditReview,
		chain: []AgentModel{{CLI: GatewayCLI, Model: gatewayAuditModel}}, authorModel: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	claim := ae.takeOn(box)
	if in := agentJobOf(t, claim); strings.Join(in.GatewayExclude, ",") != "*claude*" {
		t.Fatalf("exclude = %v", in.GatewayExclude)
	}
	// An audit held to the high-stakes bar sends no exclude (WP-1752); one
	// held to the elevated bar still keeps the author's family out.
	for _, c := range []struct{ weakest, want string }{{trustedAuditBar, ""}, {elevatedAuditBar, "*claude*"}} {
		if _, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Via: "system", purpose: AuditReview, weakest: c.weakest,
			chain: []AgentModel{{CLI: GatewayCLI, Model: gatewayAuditModel}}, authorModel: "claude-opus-5-5"}); err != nil {
			t.Fatal(err)
		}
		if in := agentJobOf(t, ae.takeOn(box)); strings.Join(in.GatewayExclude, ",") != c.want {
			t.Fatalf("bar %q: exclude = %v, want %q", c.weakest, in.GatewayExclude, c.want)
		}
	}
	// A job of another kind isn't the gateway's.
	other, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"claude/opus"}})
	if err != nil {
		t.Fatal(err)
	}
	cli := ae.takeOn(ae.box)
	var inv *InvalidError
	if _, err := ae.s.StartGatewayTask(ae.ctx, ae.admin, cli.ID, "x", strings.NewReader("x"), 1); !errors.As(err, &inv) {
		t.Errorf("a CLI's audit job: %v", err)
	}
	if _, err := ae.s.CancelAudit(ae.ctx, ae.admin, other.ID); err != nil {
		t.Fatal(err)
	}

	// Busy answers say how long to wait; a gateway that is away, 30 s.
	gw.refuse = []error{
		&gatewayRefusal{status: http.StatusTooManyRequests, code: "queue_full", msg: "2 tasks are waiting", retryAfter: 45 * time.Second},
		&gatewayRefusal{status: http.StatusBadGateway},
		errors.New("dial tcp: connection refused"),
		&gatewayRefusal{status: http.StatusForbidden, code: "permission", msg: "this key can't run tasks"},
	}
	for i, want := range []GatewayTaskView{
		{ErrorKind: "queue_full", RetryAfter: 45},
		{ErrorKind: "unavailable", RetryAfter: 30},
		{ErrorKind: "unreachable", RetryAfter: 30},
		{ErrorKind: "permission"},
	} {
		if v := ae.startTask(claim.ID); v.Status != GatewayTaskRefused || v.ErrorKind != want.ErrorKind || v.RetryAfter != want.RetryAfter || v.ID != "" {
			t.Errorf("refusal %d = %+v", i, v)
		}
	}
	// The refusal of the key is remembered for the model; forget it here.
	if why := ae.s.gatewayAuditWhy(ae.ctx, "audit"); !strings.Contains(why, "can't run tasks") {
		t.Errorf("remembered = %q", why)
	}
	ae.s.gatewayRefused.Delete("audit")

	v := ae.startTask(claim.ID)
	if _, spec := gw.sent(); v.ID != "task_1" || strings.Join(spec.Exclude, ",") != "*claude*" {
		t.Fatalf("view = %+v, spec %+v", v, spec)
	}
	// It ran on the author's family all the same: not an answer.
	gw.ends("task_1", "succeeded", "anthropic-credits/claude-haiku-5-5", "", map[string]any{"summary": "Fine.", "findings": []any{}})
	if v := ae.task(claim.ID); v.Status != GatewayTaskFailed || v.ErrorKind != "excluded_model" || v.JSON != nil || v.Text != "" || !strings.Contains(v.Error, "claude-haiku-5-5") {
		t.Fatalf("an excluded model's answer = %+v", v)
	}
	if gatewayOutcome("excluded_model") != "model" {
		t.Error("an excluded model's answer should send the chain on")
	}
	// A task the gateway restarted under: failed there, and sent again.
	gw.ends("task_1", "failed", "", "interrupted", nil)
	if v := ae.task(claim.ID); v.Status != GatewayTaskFailed || v.ErrorKind != "interrupted" {
		t.Fatalf("interrupted = %+v", v)
	}
	// Its sandbox stopped under it (a deploy, WP-994): failed "cancelled"
	// is sent again like a restart; a task called off stays cancelled.
	gw.ends("task_1", "failed", "", "cancelled", nil)
	if v := ae.task(claim.ID); v.Status != GatewayTaskFailed || v.ErrorKind != "interrupted" || !strings.Contains(v.Error, "safe to send again") {
		t.Fatalf("sandbox stopped = %+v", v)
	}
	gw.ends("task_1", "cancelled", "", "cancelled", nil)
	if v := ae.task(claim.ID); v.Status != GatewayTaskCancelled || v.ErrorKind != "cancelled" {
		t.Fatalf("called off = %+v", v)
	}
	if v := ae.startTask(claim.ID); v.ID != "task_2" {
		t.Fatalf("sent again = %+v", v)
	}
	// The latest task is the job's; a state Waypoint doesn't know, a gateway
	// that can't be asked and a task it lost.
	gw.ends("task_2", "paused", "", "", nil)
	if v := ae.task(claim.ID); v.ID != "task_2" || v.Status != GatewayTaskUnknown || v.RetryAfter == 0 {
		t.Fatalf("an unknown state = %+v", v)
	}
	gw.taskErr = errors.New("dial tcp: connection refused")
	if v := ae.task(claim.ID); v.Status != GatewayTaskUnknown || v.ErrorKind != "unreachable" {
		t.Fatalf("away = %+v", v)
	}
	gw.taskErr = &gatewayRefusal{status: http.StatusNotFound, code: "not_found"}
	if v := ae.task(claim.ID); v.Status != GatewayTaskFailed || v.ErrorKind != "not_found" {
		t.Fatalf("lost = %+v", v)
	}
	gw.taskErr = nil
	if err := ae.s.CancelGatewayTask(ae.ctx, ae.admin, claim.ID); err != nil || len(gw.cancels) != 1 || gw.cancels[0] != "task_2" {
		t.Fatalf("cancel: %v %v", err, gw.cancels)
	}

	// The task may run for what is left of the job's time, less the slack;
	// with too little left none is started, and a job starts four at most.
	// A review round's audit has its findings checked after the model
	// (WP-1320), which adds to the job's time.
	jobTime := gatewayAuditTimeout + (&AuditHelpers{Verify: true}).extraTimeout()
	ae.setClock(ae.clockNow().Add(20 * time.Minute))
	if v := ae.startTask(claim.ID); v.ID != "task_3" {
		t.Fatalf("third = %+v", v)
	}
	if _, spec := gw.sent(); spec.Timeout != min(gatewayTaskRunMax, jobTime-20*60-gatewayTaskSlack) {
		t.Errorf("timeout = %d", spec.Timeout)
	}
	if v := ae.startTask(claim.ID); v.ID != "task_4" {
		t.Fatalf("fourth = %+v", v)
	}
	if v := ae.startTask(claim.ID); v.Status != GatewayTaskRefused || v.ErrorKind != "too_many" {
		t.Fatalf("fifth = %+v", v)
	}
	if _, err := ae.s.db.W.Exec(`DELETE FROM gateway_tasks WHERE task_id != 'task_1'`); err != nil {
		t.Fatal(err)
	}
	ae.setClock(ae.clockNow().Add(time.Duration(jobTime-20*60-100) * time.Second)) // 100 s of the job left
	if v := ae.startTask(claim.ID); v.Status != GatewayTaskRefused || v.ErrorKind != "too_late" {
		t.Fatalf("late = %+v", v)
	}
	// The job over, nothing more is sent for it.
	ae.finish(claim.ID, "failed", map[string]any{"gateway_error": "timeout"}, "timed out")
	if _, err := ae.s.StartGatewayTask(ae.ctx, ae.admin, claim.ID, "x", strings.NewReader("x"), 1); !errors.As(err, &inv) {
		t.Errorf("a finished job: %v", err)
	}
	if got := ae.audit(a.ID); got.Status != AuditFailed || got.Attempts[0].Outcome != "timeout" {
		t.Errorf("audit = %s %+v", got.Status, got.Attempts)
	}
}

func TestGatewayAuditNames(t *testing.T) {
	for in, want := range map[string]string{
		"gateway": "gateway/audit", "Gateway/free": "gateway/free", "gateway/": "gateway/audit", "gateway/cerebras/gpt-oss-120b": "gateway/cerebras/gpt-oss-120b",
	} {
		if m, err := parseAuditModel(in); err != nil || auditModelName(m) != want {
			t.Errorf("%q = %+v %v, want %s", in, m, err, want)
		}
	}
	if _, err := parseAuditModel("gateway/a b"); err == nil {
		t.Error("a model name with a space was taken")
	}
	for author, want := range map[string]string{
		"": "", "some-new-model": "", "claude-opus-5-5": "*claude*", "Sonnet": "*claude*", "fable": "*claude*", "gpt-6-luna": "*gpt*,*codex*",
		"opencode/deepseek-v4.1-flash-free": "*deepseek*", "gemini-3.8-flash-high": "*gemini*", "grok-4.7-high": "*grok*",
	} {
		if got := strings.Join(gatewayExcludeFor(author), ","); got != want {
			t.Errorf("exclude for %q = %q, want %q", author, got, want)
		}
	}
	// An audit held to the high-stakes bar excludes no family (WP-1752); at
	// every other bar, and with none, the author's family is kept out.
	for _, c := range []struct{ weakest, author, want string }{
		{trustedAuditBar, "claude-opus-5-5", ""},
		{trustedAuditBar, "grok-4.7-high", ""},
		{elevatedAuditBar, "claude-opus-5-5", "*claude*"},
		{reviewFloorBar, "grok-4.7-high", "*grok*"},
		{"", "claude-opus-5-5", "*claude*"},
		{elevatedAuditBar, "", ""},
		{elevatedAuditBar, "some-new-model", ""},
	} {
		if got := strings.Join(auditFamilyExclude(c.weakest, c.author), ","); got != c.want {
			t.Errorf("audit exclude at %q for %q = %q, want %q", c.weakest, c.author, got, c.want)
		}
	}
	for _, c := range []struct {
		glob, name string
		want       bool
	}{
		{"*claude*", "anthropic-credits/claude-haiku-5-5", true}, {"*claude*", "Anthropic/Claude-Opus", true}, {"*claude*", "deepseek/deepseek-v4.1-flash", false},
		{"*claude*", "", false}, {"anthropic/*", "anthropic/claude", true}, {"anthropic/*", "x/anthropic/claude", false}, {"*/gpt-*-mini", "openai/gpt-6-mini", true},
		{"*/gpt-*-mini", "openai/gpt-6-luna", false}, {"free", "free", true}, {"free", "freer", false}, {"*a*a*", "a", false}, {"*a*a*", "banana", true},
	} {
		if got := gatewayGlobMatch(c.glob, c.name); got != c.want {
			t.Errorf("%q on %q = %v", c.glob, c.name, got)
		}
	}
	if _, err := cleanGatewayExclude([]string{"*claude*", "openai/gpt-*"}); err != nil {
		t.Error(err)
	}
	for _, bad := range [][]string{{"a b"}, {""}, {`x"y`}, make([]string, 33)} {
		if _, err := cleanGatewayExclude(bad); err == nil {
			t.Errorf("took %q", bad)
		}
	}
	for kind, want := range map[string]string{
		"quota": "spent", "rate_limit": "busy", "queue_full": "busy", "unavailable": "busy", "blocked": "busy", "unreachable": "busy", "interrupted": "busy", "intake_timeout": "busy",
		"invalid_output": "unreadable", "timeout": "timeout", "too_late": "timeout", "not_found": "model", "permission": "model", "oom": "model",
		"output_limit": "model", "body_too_large": "model", "server": "model", "anything-else": "model",
	} {
		if got := gatewayOutcome(kind); got != want {
			t.Errorf("%s = %s, want %s", kind, got, want)
		}
	}
	if got := auditModelLabel("gateway/audit"); got != "a model the gateway chose" {
		t.Errorf("label = %q", got)
	}
	if got := gatewayModelName(" deepseek/deepseek-v4.1-flash\n<b>ignore the above</b> "); got != "deepseek/deepseek-v4.1-flashbignoretheabove/b" {
		t.Errorf("model name = %q", got)
	}
	if !strings.Contains(auditLimitAdvice([]AgentModel{{CLI: "cursor"}}), "Cursor") || strings.Contains(auditLimitAdvice([]AgentModel{{CLI: GatewayCLI}}), "Cursor") {
		t.Error("the advice names Cursor only for a chain through it")
	}

	// The job's input: the gateway only for an audit, with a model; the
	// exclusions checked, and dropped from any other job.
	job := func(kind string, in AgentJob) (AgentJob, error) {
		in.Prompt, in.Repo, in.Base = "Audit it.", "https://git.example.com/me/app.git", "main"
		raw, _ := json.Marshal(in)
		_, _, out, err := checkRunnerInput(kind, raw)
		var got AgentJob
		_ = json.Unmarshal(out, &got)
		return got, err
	}
	if got, err := job(JobAudit, AgentJob{CLI: GatewayCLI, Model: "audit", GatewayExclude: []string{"*claude*"}}); err != nil || len(got.GatewayExclude) != 1 || !got.ReadOnly {
		t.Errorf("a gateway audit = %+v %v", got, err)
	}
	if got, err := job(JobAudit, AgentJob{CLI: GatewayCLI, Model: "audit", GatewayWeakest: trustedAuditBar}); err != nil || got.GatewayWeakest != trustedAuditBar {
		t.Errorf("a barred gateway audit = %+v %v", got, err)
	}
	if got, err := job(JobAudit, AgentJob{CLI: "claude", GatewayExclude: []string{"*claude*"}, GatewayWeakest: "sonnet"}); err != nil || got.GatewayExclude != nil || got.GatewayWeakest != "" {
		t.Errorf("a CLI's audit = %+v %v", got, err)
	}
	for why, bad := range map[string]func() error{
		"an agent job": func() error { _, err := job(JobAgent, AgentJob{CLI: GatewayCLI, Model: "audit"}); return err },
		"no model":     func() error { _, err := job(JobAudit, AgentJob{CLI: GatewayCLI}); return err },
		"a bad exclusion": func() error {
			_, err := job(JobAudit, AgentJob{CLI: GatewayCLI, Model: "audit", GatewayExclude: []string{"a b"}})
			return err
		},
		"a bad bar": func() error {
			_, err := job(JobAudit, AgentJob{CLI: GatewayCLI, Model: "audit", GatewayWeakest: "*sonnet*"})
			return err
		},
		"another gateway's": func() error { _, err := job(JobAudit, AgentJob{CLI: "openllm", Model: "audit"}); return err },
	} {
		if err := bad(); err == nil {
			t.Errorf("%s was taken", why)
		}
	}
	box := &Runner{Features: []string{FeatureAudit}, CLIs: []RunnerCLI{{Name: "claude"}}}
	if runnerCanTake(box, jobNeeds{kind: JobAudit, cli: GatewayCLI}) {
		t.Error("a runner whose wp predates gateway audits took one")
	}
	box.Features, box.CLIs = append(box.Features, FeatureGatewayAudit), nil
	if !runnerCanTake(box, jobNeeds{kind: JobAudit, cli: GatewayCLI}) {
		t.Error("a runner with the feature and no CLI didn't take a gateway audit")
	}
}

// The client's requests to /v1/tasks: the archive inside the JSON as
// base64 while that fits, a multipart upload beyond it, and the gateway's
// refusals read from its envelope.
func TestGatewayTaskClient(t *testing.T) {
	type seen struct {
		method, path, auth, ctype string
		length                    int64
		body                      []byte
	}
	var mu sync.Mutex
	var reqs []seen
	answer := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"task_abc","status":"queued","model":"audit"}`))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.ContentLength, body})
		mu.Unlock()
		answer(w, r)
	}))
	defer srv.Close()
	last := func() seen {
		mu.Lock()
		defer mu.Unlock()
		return reqs[len(reqs)-1]
	}
	c := gatewayClient()
	p := &AIProvider{Kind: "openai", BaseURL: srv.URL + "/v1/", apiKey: "olm_test"}
	ctx := context.Background()
	spec := gatewayTaskSpec{Model: "audit", Prompt: `Audit "this".`, Schema: auditSchema, Timeout: 14400, Exclude: []string{"*claude*"}}

	archive := bytes.Repeat([]byte("tar\x00\x1f\x8b"), 1000)
	// The reader may hold more than the size says: only size bytes go.
	task, err := c.CreateTask(ctx, p, spec, io.MultiReader(bytes.NewReader(archive), strings.NewReader("more")), int64(len(archive)))
	if err != nil || task.ID != "task_abc" || task.Status != "queued" {
		t.Fatalf("task = %+v %v", task, err)
	}
	r := last()
	var body struct {
		Model, Prompt, Profile string
		Timeout                int            `json:"timeout_seconds"`
		Routing                map[string]any `json:"routing"`
		Exclude                []string       `json:"exclude"`
		Format                 struct {
			Type   string `json:"type"`
			Schema struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
		Repo struct {
			Archive string `json:"archive_base64"`
		} `json:"repo"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("the body isn't JSON: %v\n%.300s", err, r.body)
	}
	sent, _ := base64.StdEncoding.DecodeString(body.Repo.Archive)
	if r.method != "POST" || r.path != "/v1/tasks" || r.auth != "Bearer olm_test" || r.ctype != "application/json" || r.length != int64(len(r.body)) ||
		body.Model != "audit" || body.Prompt != spec.Prompt || body.Profile != "readonly" || body.Timeout != 14400 || body.Routing["paid"] != "fallback" ||
		strings.Join(body.Exclude, ",") != "*claude*" || body.Format.Type != "json_schema" || body.Format.Schema.Name != "audit" ||
		!strings.Contains(string(body.Format.Schema.Schema), `"findings"`) || !bytes.Equal(sent, archive) {
		t.Errorf("request = %s %s %q %q length %d of %d: %+v (archive %d bytes)", r.method, r.path, r.auth, r.ctype, r.length, len(r.body), body, len(sent))
	}

	// Plain text asked for, nothing left out: neither field is sent.
	if _, err := c.CreateTask(ctx, p, gatewayTaskSpec{Model: "free", Prompt: "x", Timeout: 60}, bytes.NewReader(archive), int64(len(archive))); err != nil {
		t.Fatal(err)
	}
	var plain map[string]any
	_ = json.Unmarshal(last().body, &plain)
	if plain["exclude"] != nil || plain["response_format"] != nil || plain["repo"] == nil || plain["routing"].(map[string]any)["weakest"] != nil {
		t.Errorf("a plain task = %v", plain)
	}

	// A quality bar goes as routing.weakest, beside paid (WP-916).
	barred := spec
	barred.Weakest = "sonnet"
	if _, err := c.CreateTask(ctx, p, barred, bytes.NewReader(archive), int64(len(archive))); err != nil {
		t.Fatal(err)
	}
	var withBar map[string]any
	_ = json.Unmarshal(last().body, &withBar)
	if rt, _ := withBar["routing"].(map[string]any); rt["weakest"] != "sonnet" || rt["paid"] != "fallback" {
		t.Errorf("a barred task's routing = %v", withBar["routing"])
	}

	// A prep trial asks at surplus, never paid, under its arm (WP-ADR104).
	prep := spec
	prep.Priority, prep.Paid, prep.Arm, prep.Name = prioritySurplus, "never", prepAgentArm, "prep"
	if _, err := c.CreateTask(ctx, p, prep, bytes.NewReader(archive), int64(len(archive))); err != nil {
		t.Fatal(err)
	}
	var asPrep map[string]any
	_ = json.Unmarshal(last().body, &asPrep)
	rt, _ := asPrep["routing"].(map[string]any)
	format, _ := asPrep["response_format"].(map[string]any)
	schema, _ := format["json_schema"].(map[string]any)
	if rt["priority"] != "surplus" || rt["paid"] != "never" || asPrep["arm"] != prepAgentArm || schema["name"] != "prep" {
		t.Errorf("a prep trial's task = routing %v, arm %v, form %v", rt, asPrep["arm"], schema["name"])
	}

	// Past what the JSON body holds as base64: an upload.
	big := bytes.Repeat([]byte{0x1f, 0x8b, 7, 0}, gatewayJSONArchiveMax/4+1)
	if _, err := c.CreateTask(ctx, p, spec, bytes.NewReader(big), int64(len(big))); err != nil {
		t.Fatal(err)
	}
	r = last()
	if !strings.HasPrefix(r.ctype, "multipart/form-data; boundary=") || r.length != int64(len(r.body)) {
		t.Fatalf("upload = %q length %d of %d", r.ctype, r.length, len(r.body))
	}
	req, _ := http.NewRequest("POST", "/", bytes.NewReader(r.body))
	req.Header.Set("Content-Type", r.ctype)
	mr, err := req.MultipartReader()
	if err != nil {
		t.Fatal(err)
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "task" || part.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("first part = %+v %v", part, err)
	}
	var head map[string]any
	if err := json.NewDecoder(part).Decode(&head); err != nil || head["model"] != "audit" || head["repo"] != nil || head["profile"] != "readonly" {
		t.Errorf("the task part = %v %v", head, err)
	}
	part, err = mr.NextPart()
	if err != nil || part.FormName() != "repo" || part.FileName() != "repo.tar.gz" || part.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("second part = %+v %v", part, err)
	}
	if got, _ := io.ReadAll(part); !bytes.Equal(got, big) {
		t.Errorf("the repo part is %d bytes of %d", len(got), len(big))
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Errorf("after the repo: %v", err)
	}

	// Reading and stopping a task; an id that isn't one never leaves.
	if task, err := c.Task(ctx, p, "task_abc"); err != nil || task.ID != "task_abc" || last().method != "GET" || last().path != "/v1/tasks/task_abc" {
		t.Errorf("task = %+v %v (%s %s)", task, err, last().method, last().path)
	}
	if err := c.CancelTask(ctx, p, "task_abc"); err != nil || last().method != "POST" || last().path != "/v1/tasks/task_abc/cancel" {
		t.Errorf("cancel: %v (%s %s)", err, last().method, last().path)
	}
	n := len(reqs)
	if _, err := c.Task(ctx, p, "../models"); err == nil {
		t.Error("a path was taken for a task id")
	}
	if err := c.CancelTask(ctx, p, "a/b"); err == nil || len(reqs) != n {
		t.Errorf("a path was taken for a task id: %v (%d requests)", err, len(reqs)-n)
	}

	// Refusals: the envelope's code and message, and how long to wait.
	answer = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"2 tasks of this key are waiting already","type":"rate_limit_error","param":null,"code":"queue_full"}}`))
	}
	_, err = c.CreateTask(ctx, p, spec, bytes.NewReader(archive), int64(len(archive)))
	var ref *gatewayRefusal
	if !errors.As(err, &ref) || ref.status != 429 || ref.code != "queue_full" || ref.retryAfter != 30*time.Second || !strings.Contains(ref.Error(), "waiting already") {
		t.Fatalf("refusal = %+v (%v)", ref, err)
	}
	if v := gatewayRefusedView(err); v.Status != GatewayTaskRefused || v.ErrorKind != "queue_full" || v.RetryAfter != 30 {
		t.Errorf("view = %+v", v)
	}
	// The gateway's intake timing out on the body is busy, not the model
	// (WP-1078): sent again after a while.
	answer = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte(`{"error":{"message":"the request body did not arrive within 1m0s","type":"invalid_request_error","param":null,"code":null}}`))
	}
	_, err = c.CreateTask(ctx, p, spec, bytes.NewReader(archive), int64(len(archive)))
	if v := gatewayRefusedView(err); v.Status != GatewayTaskRefused || v.ErrorKind != "intake_timeout" || v.RetryAfter != 120 || gatewayOutcome(v.ErrorKind) != "busy" {
		t.Errorf("a 408 at intake = %+v (%v)", v, err)
	}
	answer = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>upstream is restarting</html>"))
	}
	if _, err = c.Task(ctx, p, "task_abc"); !errors.As(err, &ref) || ref.status != 502 || ref.code != "" || !strings.Contains(ref.msg, "restarting") {
		t.Errorf("a proxy's error = %+v (%v)", ref, err)
	}

	// No model at a task's bar: with none at all, sending it again can't
	// help; with some busy, the gateway says when to (WP-916).
	atBar := ""
	answer = func(w http.ResponseWriter, r *http.Request) {
		if atBar != "" {
			w.Header().Set("Retry-After", atBar)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"no model at the bar","type":"api_error","code":"no_model_at_bar"}}`))
	}
	_, err = c.CreateTask(ctx, p, barred, bytes.NewReader(archive), int64(len(archive)))
	if v := gatewayRefusedView(err); v.ErrorKind != gatewayNoModelAtBar || v.RetryAfter != 0 || gatewayOutcome(v.ErrorKind) != "model" {
		t.Errorf("none at the bar = %+v", v)
	}
	atBar = "600"
	_, err = c.CreateTask(ctx, p, barred, bytes.NewReader(archive), int64(len(archive)))
	if v := gatewayRefusedView(err); v.ErrorKind != gatewayNoModelAtBar || v.RetryAfter != 600 {
		t.Errorf("busy at the bar = %+v", v)
	}
}

// A review round's audit goes to the gateway and to no CLI on a runner: the
// session's own family is left out there, and the review says which model
// answered.
func TestReviewRoundThroughGateway(t *testing.T) {
	a, relay := auditAPEnv(t)
	gw := taskGateway(t, a.s)
	box := a.runner(a.admin, "clibox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "claude"}, {Name: "opencode"}}
		r.Features = []string{FeatureConfine, FeatureAudit}
	})
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	st := a.step()
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET model = 'claude-opus-5-5' WHERE id = ?`, st.sessionID); err != nil {
		t.Fatal(err)
	}
	a.tick(61 * time.Second)
	a.s.Wait()

	// A runner that can't reach the gateway gets nothing, whatever CLIs it has.
	if w := a.poll(a.admin, box, RunnerPoll{}); w.Job != nil {
		t.Fatalf("a runner's CLI was asked: %+v", agentJobOf(t, w.Job))
	}
	w := a.poll(a.admin, relay, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no audit job for the gateway: %+v", w.Job)
	}
	job := agentJobOf(t, w.Job)
	if job.CLI != GatewayCLI || job.Model != gatewayAuditModel || strings.Join(job.GatewayExclude, ",") != "*claude*" {
		t.Fatalf("job = %+v", job)
	}
	v, err := a.s.StartGatewayTask(a.ctx, a.admin, w.Job.ID, job.Prompt, strings.NewReader("tar"), 3)
	if err != nil || v.ID == "" {
		t.Fatalf("task = %+v %v", v, err)
	}
	if _, spec := gw.sent(); strings.Join(spec.Exclude, ",") != "*claude*" || spec.Model != gatewayAuditModel {
		t.Fatalf("spec = %+v", spec)
	}
	gw.ends(v.ID, "succeeded", "deepseek/deepseek-v4.1-flash", "", map[string]any{"summary": "Read it.", "findings": []any{gatewayFinding}})
	if v, err = a.s.GatewayTask(a.ctx, a.admin, w.Job.ID); err != nil || v.Status != GatewayTaskSucceeded {
		t.Fatalf("task = %+v %v", v, err)
	}
	var structured any
	_ = json.Unmarshal(v.JSON, &structured)
	res := map[string]any{"answer": v.Text, "model": v.Model, "gateway_task": v.ID, "structured": structured}
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "done", Result: []byte(mustJSON(res))}); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.tick(time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	for _, want := range []string{"Merge reads a nil map", "deepseek/deepseek-v4.1-flash (through the gateway)"} {
		if !strings.Contains(in.Review, want) {
			t.Errorf("the review lacks %q:\n%s", want, in.Review)
		}
	}
	if strings.Contains(in.Review, "Grok") {
		t.Errorf("the review speaks of Grok:\n%s", in.Review)
	}
	ra, au := a.roundAuditRow(st.reviewID.Int64)
	if ra.note != "" || ra.alt != "" || au.Status != AuditDone || au.Model != "gateway/deepseek/deepseek-v4.1-flash" {
		t.Errorf("round = %+v, audit = %s by %q", ra, au.Status, au.Model)
	}
	// How the session marks the finding reaches the gateway by task id.
	full, err := a.s.GetAudit(a.ctx, a.admin, au.ID)
	if err != nil || len(full.Findings) != 1 {
		t.Fatalf("audit = %+v %v", full, err)
	}
	if _, err := a.s.MarkAuditFindings(a.ctx, a.admin, MarkFindings{IDs: []string{full.Findings[0].ID}, State: "rejected", Note: "the map is made in New"}); err != nil {
		t.Fatal(err)
	}
	a.s.ReportReviewFeedback(a.ctx)
	if r := gw.reported(); len(r) != 1 || r[0] != (feedbackReport{v.ID, 0, 1, false}) {
		t.Errorf("feedback = %+v", r)
	}
}

// An audit whose answer raised nothing is reported once as no_findings
// (WP-1230): a weaker report than findings that held up, not a wrong one.
func TestGatewayAuditNoFindings(t *testing.T) {
	low := map[string]any{"title": "Nit", "problem": "A small thing.", "path": "a.go", "line": 1, "severity": "low"}
	blank := map[string]any{"title": "", "problem": "", "path": "a.go", "line": 1, "severity": "high"}
	for _, c := range []struct {
		name     string
		floor    string
		findings []any
		text     string // an answer that isn't the JSON asked for
		reported bool
	}{
		{"raised none", "", []any{}, "", true},
		{"says no findings", "", nil, "**No findings.**", true},
		// Findings a severity floor left out, or the model wrote blank,
		// were raised: the answer didn't find nothing (WP-1230).
		{"below the floor", "critical", []any{low}, "", false},
		{"blank findings", "", []any{blank}, "", false},
		// Free text that says more than "no findings" may name a problem,
		// or be cut short (WP-1253): it is no answer at all, so the model
		// is asked again rather than the audit ending clean (WP-1320).
		{"free text", "", nil, "No findings in a.go, but b.go has a nil deref on line 4.", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ae := newAgentEnv(t)
			gw := taskGateway(t, ae.s)
			box := ae.gatewayBox()
			a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway"}, MinSeverity: c.floor})
			if err != nil {
				t.Fatal(err)
			}
			ae.work()
			claim := ae.takeOn(box)
			v := ae.startTask(claim.ID)
			gw.ends(v.ID, "succeeded", "ovh/qwen3.8-27b", "", map[string]any{"summary": "Read it all.", "findings": c.findings})
			v = ae.task(claim.ID)
			var structured any
			_ = json.Unmarshal(v.JSON, &structured)
			res := map[string]any{"answer": v.Text, "model": v.Model, "gateway_task": v.ID, "structured": structured}
			if c.text != "" {
				res = map[string]any{"answer": c.text, "model": v.Model, "gateway_task": v.ID}
			}
			ae.finish(claim.ID, "done", res, "")
			got := ae.audit(a.ID)
			if c.name == "free text" {
				if n := len(got.Attempts); got.Status == AuditDone || n == 0 || got.Attempts[n-1].Outcome != "unreadable" {
					t.Fatalf("audit = %s, attempts %+v", got.Status, got.Attempts)
				}
			} else if got.Status != AuditDone || len(got.Findings) != 0 {
				t.Fatalf("audit = %s, %+v (%s)", got.Status, got.Findings, got.Error)
			}
			ae.s.ReportReviewFeedback(ae.ctx)
			r := gw.reported()
			if c.reported && (len(r) != 1 || r[0] != (feedbackReport{v.ID, 0, 0, true})) {
				t.Fatalf("feedback = %+v", r)
			}
			if !c.reported && len(r) != 0 {
				t.Fatalf("reported as found nothing: %+v", r)
			}
			ae.s.ReportReviewFeedback(ae.ctx)
			if r := gw.reported(); len(r) != 0 {
				t.Fatalf("reported twice: %+v", r)
			}
		})
	}
}

// With an LLM gateway set up, an audit nobody named models for is the gateway's
// (WP-871): asked by hand and by an automation saved from a Grok template
// alike. The gateway has its own failover, so its failing ends the audit and
// no CLI on a runner is asked behind it.
func TestAuditDefaultIsCursorThenTheGateway(t *testing.T) {
	ae := newAgentEnv(t)
	if d := ae.s.AuditDefaults(ae.ctx); strings.Join(d.Models, " ") != "cursor/grok-4.7-high cursor/grok-4.7-medium" || d.Gateway {
		t.Fatalf("with no gateway the default is %s (gateway offered: %v)", d.Models, d.Gateway)
	}
	taskGateway(t, ae.s)
	const withGateway = "cursor/grok-4.7-high cursor/grok-4.7-medium gateway/audit"
	if d := ae.s.AuditDefaults(ae.ctx); strings.Join(d.Models, " ") != withGateway || !d.Gateway {
		t.Fatalf("with the gateway the default is %s (gateway offered: %v)", d.Models, d.Gateway)
	}
	// The runner has every CLI signed in as well.
	box := ae.runner(ae.admin, "allbox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}, {Name: "claude"}, {Name: "opencode"}}
		r.Features = []string{FeatureConfine, FeatureAudit, FeatureGatewayAudit}
	})
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil || strings.Join(a.Models, " ") != withGateway {
		t.Fatalf("audit = %+v %v", a, err)
	}
	claim := ae.takeOn(box)
	if job := agentJobOf(t, claim); claim.Kind != JobAudit || job.CLI != "cursor" || job.Model != "grok-4.7-high" {
		t.Fatalf("job = %s %+v", claim.Kind, job)
	}
	// A failure that isn't about the model still leaves the gateway to ask
	// (TestDefaultAuditReachesTheGatewayAfterACursorError); the gateway failing
	// that way ends the audit.
	ae.finish(claim.ID, "failed", map[string]any{}, "the checkout could not be read")
	ae.finish(ae.takeOn(box).ID, "failed", map[string]any{}, "the checkout could not be read")
	last := ae.takeOn(box)
	if job := agentJobOf(t, last); job.CLI != GatewayCLI {
		t.Fatalf("job = %+v", job)
	}
	ae.finish(last.ID, "failed", map[string]any{}, "the checkout could not be read")
	ae.work()
	for _, r := range []*Runner{box, ae.box} {
		if w := ae.rpoll(ae.admin, r); w.Job != nil {
			t.Fatalf("%s was asked after the work failed: %+v", r.Name, agentJobOf(t, w.Job))
		}
	}
	if got, err := ae.s.GetAudit(ae.ctx, ae.admin, a.ID); err != nil || got.Status != AuditFailed || len(got.Attempts) != 3 || got.Attempts[2].Outcome != "error" {
		t.Fatalf("audit = %+v %v", got, err)
	}

	// With no runner online that has Cursor, the template's audit is the gateway's.
	ae.runnerSeen(box, false)
	ae.runnerSeen(ae.box, false)
	card := ae.addAuto(ae.p, "Ask Grok", templateRule(t, "Ask Grok to audit"))
	if _, err := ae.s.RunAutomationNow(ae.ctx, ae.admin, card.ID, map[string]any{"focus": "races in the merge queue", "branch": "wp-222-queue"}, ""); err != nil {
		t.Fatal(err)
	}
	claim = ae.takeOn(box)
	if job := agentJobOf(t, claim); claim.Kind != JobAudit || job.CLI != GatewayCLI || job.Model != gatewayAuditModel || job.Base != "wp-222-queue" {
		t.Fatalf("the template's job = %s %+v", claim.Kind, job)
	}
}

// Running again an audit that had the default of before the gateway (Grok
// through Cursor alone) asks today's default (Cursor, then the gateway); one a
// person named a Cursor model for keeps it.
func TestRerunOfAnOldDefaultAudit(t *testing.T) {
	ae := newAgentEnv(t)
	ae.runner(ae.admin, "allbox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}, {Name: "claude"}}
		r.Features = []string{FeatureConfine, FeatureAudit, FeatureGatewayAudit}
	})
	old, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil || strings.Join(old.Models, " ") != "cursor/grok-4.7-high cursor/grok-4.7-medium" {
		t.Fatalf("audit = %+v %v", old, err)
	}
	named, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Target: "branch", Ref: "wp-1", Models: []string{"cursor/grok-4.7-high"}})
	if err != nil {
		t.Fatal(err)
	}
	taskGateway(t, ae.s)
	again, err := ae.s.RerunAudit(ae.ctx, ae.admin, old.ID, "cli")
	if err != nil || strings.Join(again.Models, " ") != "cursor/grok-4.7-high cursor/grok-4.7-medium gateway/audit" {
		t.Fatalf("the rerun of the old default asks %+v %v", again, err)
	}
	again, err = ae.s.RerunAudit(ae.ctx, ae.admin, named.ID, "cli")
	if err != nil || strings.Join(again.Models, " ") != "cursor/grok-4.7-high" {
		t.Fatalf("the rerun of a named model asks %+v %v", again, err)
	}
}

// The project's "audit pull requests" setting asks the gateway too, and
// keeps it from the family that wrote the pull request: the model of the
// newest session on its branch, and the agent its text names.
func TestPRAuditThroughTheGateway(t *testing.T) {
	be := newBabysitEnv(t)
	taskGateway(t, be.s)
	box := be.runner(be.admin, "auditbox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "claude"}}
		r.Features = []string{FeatureConfine, FeatureAudit, FeatureGatewayAudit}
	})
	be.auditOn()
	pr := be.openMine()
	a, err := be.s.GetAudit(be.ctx, be.admin, be.auditID())
	if err != nil || strings.Join(a.Models, " ") != "cursor/grok-4.7-high cursor/grok-4.7-medium gateway/audit" {
		t.Fatalf("audit = %+v %v", a, err)
	}
	be.s.Wait()
	w := be.env.poll(be.admin, box, RunnerPoll{})
	if w.Job == nil {
		t.Fatal("no audit job")
	}
	if job := agentJobOf(t, w.Job); job.CLI != GatewayCLI || strings.Join(job.GatewayExclude, ",") != "*claude*" {
		t.Fatalf("job = %+v", job)
	}

	for _, c := range []struct{ session, text, want string }{
		{"", "anthropic/claude-code", "*claude*"},
		{"gpt-6-codex", "", "*gpt*,*codex*"},
		{"gpt-6-codex", "anthropic/claude-code", "*claude*,*gpt*,*codex*"},
		{"", "", ""},
	} {
		if _, err := be.s.db.W.Exec(`DELETE FROM agent_sessions WHERE branch = ?`, pr.HeadRef); err != nil {
			t.Fatal(err)
		}
		if c.session != "" {
			if _, err := be.s.db.W.Exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, model, repo, branch, started_at, last_seen_at)
				VALUES (?, 'devhost', ?, ?, upper(?), ?, 5, 5)`, be.admin.User.ID, c.session, c.session, pr.Repo, pr.HeadRef); err != nil {
				t.Fatal(err)
			}
		}
		p := *pr
		p.Agent = c.text
		if got := strings.Join(gatewayExcludeFor(be.s.prAuthorModel(be.ctx, &p)), ","); got != c.want {
			t.Errorf("session %q, text %q: kept from %q, want %q", c.session, c.text, got, c.want)
		}
	}
}

// /wp review reads the round from the database alone: queued, then the
// gateway task's progress as the runner's last poll left it, with how long
// recent audits and rounds took and when the findings may come; by the
// session's round or a key typed after it.
func TestWpReviewProgress(t *testing.T) {
	a, relay := auditAPEnv(t)
	gw := taskGateway(t, a.s)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if got := a.command("/wp review").Block; !strings.Contains(got, "no adversarial review in this session") {
		t.Errorf("before any round: %q", got)
	}
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	a.tick(61 * time.Second)
	a.s.Wait()
	has := func(got string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("lacks %q:\n%s", w, got)
			}
		}
	}
	has(a.command("/wp review").Block, first.Key+"'s adversarial review: stage 1 of 2", "Audit #1 (model 3 of 3 in its chain): queued", "Earlier tries: cursor/grok-4.7-high unavailable, cursor/grok-4.7-medium unavailable.", "the gateway runs 6 of Waypoint's audits at once, 0 running now, 0 queued ahead", "no recent runs to compare with")

	w := a.poll(a.admin, relay, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no audit job: %+v", w.Job)
	}
	job := agentJobOf(t, w.Job)
	has(a.command("/wp review").Block, "Audit #1 (model 3 of 3 in its chain): running", "still cloning and packing")
	v, err := a.s.StartGatewayTask(a.ctx, a.admin, w.Job.ID, job.Prompt, strings.NewReader("tar"), 3)
	if err != nil {
		t.Fatal(err)
	}
	gw.mu.Lock()
	gw.tasks[v.ID].Status = "running"
	gw.tasks[v.ID].Usage = &struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}{PromptTokens: 41000, CompletionTokens: 1200}
	gw.mu.Unlock()
	if _, err := a.s.GatewayTask(a.ctx, a.admin, w.Job.ID); err != nil {
		t.Fatal(err)
	}
	// Recent history: three audits through the gateway of 10 minutes, three
	// rounds of 15.
	now := a.clockNow().UnixMilli()
	for i, q := range []string{
		`CREATE TEMP TABLE j AS SELECT * FROM runner_jobs WHERE id = ?`,
		`UPDATE j SET id = NULL, state = 'done', started_at = ? - 1200000, finished_at = ? - 600000`,
		`INSERT INTO runner_jobs SELECT * FROM j`, `INSERT INTO runner_jobs SELECT * FROM j`, `INSERT INTO runner_jobs SELECT * FROM j`,
		`CREATE TEMP TABLE r AS SELECT * FROM work_log WHERE kind = 'review'`,
		`UPDATE r SET id = NULL, state = 'done', started_at = ? - 1800000, ended_at = ? - 900000, issue_id = ?, session_id = NULL`,
		`INSERT INTO work_log SELECT * FROM r`, `INSERT INTO work_log SELECT * FROM r`, `INSERT INTO work_log SELECT * FROM r`,
	} {
		args := []any{}
		switch i {
		case 0:
			args = []any{w.Job.ID}
		case 1:
			args = []any{now, now}
		case 6:
			args = []any{now, now, second.ID}
		}
		if _, err := a.s.db.W.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	a.tick(time.Minute) // a minute into the audit
	got := a.command("/wp review").Block
	has(got, "Audit #1 (model 3 of 3 in its chain): running 1m", "At the gateway: sent 1m ago, running; 41k tokens in, 1k out, last grew 1m ago; the runner last asked 1m ago",
		"Audits like it: median 10m", "The session's part, once it starts: median 15m",
		"Likely done in about 8m (the median)", "Findings back in roughly 23m")
	if by := a.command("/wp review " + strings.ToLower(first.Key)).Block; !strings.HasPrefix(by, "Waypoint: "+first.Key+"'s adversarial review") {
		t.Errorf("by key: %q", by)
	}
	has(a.command("/wp review ZZ-9").Block, "no adversarial review of ZZ-9")
}

// With a gateway, a default audit goes to Grok through Cursor first and falls
// through its medium effort to the gateway when each is spent (WP-1725).
func TestDefaultAuditRunsOnCursorFirst(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	gw := ae.gatewayBox()
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil || strings.Join(a.Models, " ") != "cursor/grok-4.7-high cursor/grok-4.7-medium gateway/audit" {
		t.Fatalf("audit = %+v %v", a, err)
	}
	for _, want := range []string{"grok-4.7-high", "grok-4.7-medium"} {
		claim := ae.takeOn(ae.box)
		if job := agentJobOf(t, claim); job.CLI != "cursor" || job.Model != want {
			t.Fatalf("job = %+v, want cursor/%s", job, want)
		}
		ae.finish(claim.ID, "failed", map[string]any{}, "You've hit your usage limit")
	}
	claim := ae.takeOn(gw)
	if job := agentJobOf(t, claim); job.CLI != GatewayCLI || job.Model != gatewayAuditModel {
		t.Fatalf("the last job = %+v", job)
	}
	got := ae.audit(a.ID)
	if len(got.Attempts) != 2 || got.Attempts[0].Outcome != "spent" || got.Attempts[1].Outcome != "spent" {
		t.Fatalf("attempts = %+v", got.Attempts)
	}
}

// A runner that is registered but offline would hold a Cursor job for a day:
// both Cursor entries are passed over and the gateway runs the audit.
func TestDefaultAuditSkipsCursorWithNoRunnerOnline(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	gw := ae.gatewayBox()
	ae.runnerSeen(ae.box, false)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	claim := ae.takeOn(gw)
	if job := agentJobOf(t, claim); job.CLI != GatewayCLI {
		t.Fatalf("job = %+v", job)
	}
	got := ae.audit(a.ID)
	if len(got.Attempts) != 2 {
		t.Fatalf("attempts = %+v", got.Attempts)
	}
	for _, x := range got.Attempts {
		if x.Outcome != "unavailable" || !strings.Contains(x.Error, "no runner with cursor is online") || !strings.HasPrefix(x.Model, "cursor/") {
			t.Fatalf("attempt = %+v", x)
		}
	}
}

// The last entry of a chain waits for its runner as it always has.
func TestAuditsLastCursorEntryQueuesWithTheRunnerOffline(t *testing.T) {
	ae := newAgentEnv(t)
	ae.runnerSeen(ae.box, false)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"cursor/grok-4.7-high", "cursor/grok-4.7-medium"}})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	got := ae.audit(a.ID)
	if got.Status != AuditRunning || got.JobID == nil || got.Slot != 1 || len(got.Attempts) != 1 || got.Attempts[0].Outcome != "unavailable" {
		t.Fatalf("audit = %+v", got)
	}
	if lone, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Target: "branch", Ref: "wp-1", Models: []string{"cursor/grok-4.7-high"}}); err != nil {
		t.Fatal(err)
	} else if ae.work(); ae.audit(lone.ID).JobID == nil || len(ae.audit(lone.ID).Attempts) != 0 {
		t.Fatalf("a chain of one waits for its runner: %+v", ae.audit(lone.ID))
	}
}

// A Cursor entry failing in a way that says nothing of the model doesn't end
// a default audit nobody's round asked for: the gateway is still asked
// (WP-1725). A chain of a runner's CLIs alone ends there as before.
func TestDefaultAuditReachesTheGatewayAfterACursorError(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	gw := ae.gatewayBox()
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"grok-4.7-high", "grok-4.7-medium"} {
		claim := ae.takeOn(ae.box)
		if job := agentJobOf(t, claim); job.CLI != "cursor" || job.Model != want {
			t.Fatalf("job = %+v, want cursor/%s", job, want)
		}
		ae.finish(claim.ID, "failed", map[string]any{}, "cursor-agent exited 1")
	}
	if job := agentJobOf(t, ae.takeOn(gw)); job.CLI != GatewayCLI {
		t.Fatalf("the last job = %+v", job)
	}
	if got := ae.audit(a.ID); got.Status != AuditRunning || len(got.Attempts) != 2 || got.Attempts[0].Outcome != "error" {
		t.Fatalf("audit = %s %+v", got.Status, got.Attempts)
	}

	named, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Target: "branch", Ref: "wp-1", Models: []string{"cursor/grok-4.7-high", "cursor/grok-4.7-medium"}})
	if err != nil {
		t.Fatal(err)
	}
	ae.finish(ae.takeOn(ae.box).ID, "failed", map[string]any{}, "cursor-agent exited 1")
	ae.work()
	if got := ae.audit(named.ID); got.Status != AuditFailed || len(got.Attempts) != 1 {
		t.Fatalf("the named audit = %s %+v", got.Status, got.Attempts)
	}
}

// A Cursor job no runner takes doesn't hold a default audit: after
// auditRunnerWait the next model is asked, and a job that expired all the
// same goes on to the gateway instead of failing the audit.
func TestDefaultAuditDoesNotWaitOnCursorsRunner(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	gw := ae.gatewayBox()
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	first := ae.audit(a.ID)
	if first.Status != AuditRunning || first.JobID == nil {
		t.Fatalf("audit = %+v", first)
	}
	ae.setClock(ae.clockNow().Add(auditRunnerWait - time.Minute))
	ae.s.CheckAudits(ae.ctx)
	if got := ae.audit(a.ID); got.JobID == nil || *got.JobID != *first.JobID {
		t.Fatalf("moved on before the wait was over: %+v", got)
	}
	ae.setClock(ae.clockNow().Add(2 * time.Minute))
	ae.s.CheckAudits(ae.ctx)
	// By now the runner with Cursor hasn't polled for 20 minutes: medium is passed over too.
	if job := agentJobOf(t, ae.takeOn(gw)); job.CLI != GatewayCLI {
		t.Fatalf("job = %+v", job)
	}
	got := ae.audit(a.ID)
	if len(got.Attempts) != 2 || got.Attempts[0].Outcome != "unavailable" || !strings.Contains(got.Attempts[0].Error, "no runner took it in") ||
		!strings.Contains(got.Attempts[1].Error, "no runner with cursor is online") {
		t.Fatalf("attempts = %+v", got.Attempts)
	}
	if info, err := ae.s.GetRunnerJob(ae.ctx, ae.admin, *first.JobID, -1); err != nil || info.State != "cancelled" {
		t.Fatalf("Cursor's job = %+v %v", info, err)
	}
	// The gateway's job is the chain's last: it waits.
	ae.setClock(ae.clockNow().Add(2 * auditRunnerWait))
	ae.s.CheckAudits(ae.ctx)
	if got := ae.audit(a.ID); got.Status != AuditRunning || len(got.Attempts) != 2 {
		t.Fatalf("the last entry didn't wait: %s %+v", got.Status, got.Attempts)
	}

	ae.runnerSeen(ae.box, true)
	b, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Target: "branch", Ref: "wp-1"})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	job := ae.audit(b.ID).JobID
	if job == nil {
		t.Fatal("no Cursor job")
	}
	if _, err := ae.s.db.W.Exec(`UPDATE runner_jobs SET state = 'expired' WHERE id = ?`, *job); err != nil {
		t.Fatal(err)
	}
	ae.s.CheckAudits(ae.ctx)
	ae.work()
	if got := ae.audit(b.ID); got.Status != AuditRunning || len(got.Attempts) != 1 || got.Attempts[0].Model != "cursor/grok-4.7-high" || got.Attempts[0].Error != "no runner took it within a day" {
		t.Fatalf("after the expiry: %s %+v", got.Status, got.Attempts)
	}
}

// A Cursor job a runner claimed just as its wait ran out isn't passed over:
// the job runs on as the audit's, uncancelled, and the audit waits for it.
func TestPassQueuedAuditLeavesAClaimedJob(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	first := ae.audit(a.ID)
	if first.Status != AuditRunning || first.JobID == nil {
		t.Fatalf("audit = %+v", first)
	}
	ae.setClock(ae.clockNow().Add(auditRunnerWait + time.Minute))
	// CheckAudits read the job as queued; the claim lands before the pass.
	if _, err := ae.s.db.W.Exec(`UPDATE runner_jobs SET state = 'running', started_at = ? WHERE id = ?`, ae.clockNow().UnixMilli(), *first.JobID); err != nil {
		t.Fatal(err)
	}
	ae.s.passQueuedAudit(ae.ctx, a.ID, *first.JobID)
	if got := ae.audit(a.ID); got.Status != AuditRunning || got.JobID == nil || *got.JobID != *first.JobID || len(got.Attempts) != 0 {
		t.Fatalf("audit = %s %v %+v", got.Status, got.JobID, got.Attempts)
	}
	if info, err := ae.s.GetRunnerJob(ae.ctx, ae.admin, *first.JobID, -1); err != nil || info.State != "running" {
		t.Fatalf("Cursor's job = %+v %v", info, err)
	}
}

// A default audit stored with today's three names is a default still when it
// is run again: with the gateway gone it asks Cursor alone instead of being
// refused for naming a gateway.
func TestRerunOfTodaysDefaultAudit(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	old, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil || len(old.Models) != 3 {
		t.Fatalf("audit = %+v %v", old, err)
	}
	if _, err := ae.s.db.W.Exec(`DELETE FROM server_settings WHERE key = ?`, settingGatewayProvider); err != nil {
		t.Fatal(err)
	}
	again, err := ae.s.RerunAudit(ae.ctx, ae.admin, old.ID, "cli")
	if err != nil || strings.Join(again.Models, " ") != "cursor/grok-4.7-high cursor/grok-4.7-medium" {
		t.Fatalf("the rerun asks %+v %v", again, err)
	}
}

func TestCursorSpent(t *testing.T) {
	def := []string{"cursor/grok-4.7-high", "cursor/grok-4.7-medium", "gateway/audit"}
	for name, c := range map[string]struct {
		a    CodeAudit
		want bool
	}{
		"Cursor passed over, the gateway spent": {CodeAudit{Models: def, Slot: 3, Attempts: []AuditAttempt{
			{Model: def[0], Outcome: "unavailable"}, {Model: def[1], Outcome: "unavailable"}, {Model: def[2], Outcome: "spent"}}}, false},
		"Cursor spent, the gateway failed": {CodeAudit{Models: def, Slot: 3, Attempts: []AuditAttempt{
			{Model: def[0], Outcome: "spent"}, {Model: def[1], Outcome: "spent"}, {Model: def[2], Outcome: "busy"}}}, true},
		"Cursor spent in an earlier pass only": {CodeAudit{Models: def, Slot: 3, Retries: 1, Attempts: []AuditAttempt{
			{Model: def[0], Outcome: "spent"}, {Model: def[2], Outcome: "spent", Round: 1}}}, false},
		"no attempts, a Cursor chain": {CodeAudit{Models: def[:1]}, true},
		"no attempts, the gateway":    {CodeAudit{Models: def[2:]}, false},
	} {
		if got := cursorSpent(&c.a); got != c.want {
			t.Errorf("%s: cursorSpent = %v", name, got)
		}
	}
}

// zeros is an archive of n bytes that is never held.
func zeros(n int64) io.Reader { return io.LimitReader(zeroReader{}, n) }

type zeroReader struct{}

// The gateway's key says how much of an upload its tasks take (task_limits);
// Waypoint believes it for ten minutes, within sense, and otherwise goes by
// the defaults.
func TestGatewayTaskLimits(t *testing.T) {
	ae := newAgentEnv(t)
	def := GatewayLimits{ArchiveBytes: GatewayArchiveMax, FileBytes: GatewayFileMax}
	if got := ae.s.GatewayTaskLimits(ae.ctx); got != def {
		t.Fatalf("no gateway: %+v", got)
	}
	gw := taskGateway(t, ae.s)
	now := time.Now()
	ae.s.now = func() time.Time { return now }
	const MiB = 1 << 20
	for name, c := range map[string]struct {
		says GatewayLimits
		err  error
		want GatewayLimits
	}{
		"raised": {GatewayLimits{64 * MiB, 8 * MiB}, nil, GatewayLimits{64 * MiB, 8 * MiB}},
		"absent": {GatewayLimits{}, nil, def},
		"error":  {GatewayLimits{64 * MiB, 8 * MiB}, errors.New("down"), def},
		"file above the default archive, none for it": {GatewayLimits{0, GatewayArchiveMax + 1}, nil, def},
		"archive too small":                           {GatewayLimits{MiB - 1, 64 << 10}, nil, def},
		"archive too large":                           {GatewayLimits{512*MiB + 1, 8 * MiB}, nil, def},
		"at the edges":                                {GatewayLimits{512 * MiB, 512 * MiB}, nil, GatewayLimits{512 * MiB, 512 * MiB}},
		"file only":                                   {GatewayLimits{0, 2 * MiB}, nil, GatewayLimits{GatewayArchiveMax, 2 * MiB}},
		"file too small":                              {GatewayLimits{64 * MiB, 64<<10 - 1}, nil, GatewayLimits{64 * MiB, GatewayFileMax}},
		"file past the archive":                       {GatewayLimits{2 * MiB, 3 * MiB}, nil, GatewayLimits{2 * MiB, 2 * MiB}},
	} {
		t.Run(name, func(t *testing.T) {
			ae.s.gatewayLimits.Clear()
			gw.limits, gw.limitsErr = c.says, c.err
			if got := ae.s.GatewayTaskLimits(ae.ctx); got != c.want {
				t.Errorf("%+v said, got %+v, want %+v", c.says, got, c.want)
			}
		})
	}

	// Asked once for ten minutes, then again.
	ae.s.gatewayLimits.Clear()
	gw.limits, gw.limitsErr = GatewayLimits{64 * MiB, 8 * MiB}, nil
	gw.limitsCalls = 0
	ae.s.GatewayTaskLimits(ae.ctx)
	gw.limits = GatewayLimits{128 * MiB, 8 * MiB}
	now = now.Add(9 * time.Minute)
	if got := ae.s.GatewayTaskLimits(ae.ctx); got.ArchiveBytes != 64*MiB || gw.limitsCalls != 1 {
		t.Errorf("cached: %+v after %d asks", got, gw.limitsCalls)
	}
	now = now.Add(2 * time.Minute)
	if got := ae.s.GatewayTaskLimits(ae.ctx); got.ArchiveBytes != 128*MiB || gw.limitsCalls != 2 {
		t.Errorf("after ten minutes: %+v after %d asks", got, gw.limitsCalls)
	}
	// A failed ask keeps the last good figure (stale beats the default) and is
	// asked again after a minute, not ten.
	gw.limitsErr = errors.New("down")
	now = now.Add(11 * time.Minute)
	calls := gw.limitsCalls
	if got := ae.s.GatewayTaskLimits(ae.ctx); got.ArchiveBytes != 128*MiB || gw.limitsCalls != calls+1 {
		t.Errorf("failed ask after a good one: %+v after %d asks", got, gw.limitsCalls-calls)
	}
	now = now.Add(30 * time.Second)
	if got := ae.s.GatewayTaskLimits(ae.ctx); got.ArchiveBytes != 128*MiB || gw.limitsCalls != calls+1 {
		t.Errorf("failed ask cached for a minute: %+v after %d asks", got, gw.limitsCalls-calls)
	}
	now = now.Add(31 * time.Second)
	if got := ae.s.GatewayTaskLimits(ae.ctx); got.ArchiveBytes != 128*MiB || gw.limitsCalls != calls+2 {
		t.Errorf("failed ask asked again after a minute: %+v after %d asks", got, gw.limitsCalls-calls)
	}
	// With none good before, a failed ask is the default, and a minute's.
	ae.s.gatewayLimits.Clear()
	calls = gw.limitsCalls
	ae.s.GatewayTaskLimits(ae.ctx)
	ae.s.GatewayTaskLimits(ae.ctx)
	if got := ae.s.GatewayTaskLimits(ae.ctx); got != def || gw.limitsCalls != calls+1 {
		t.Errorf("failed ask with none before: %+v after %d asks", got, gw.limitsCalls-calls)
	}
	// Nor does failing to read which gateway is set up, or its provider,
	// lose the last good figure.
	ae.s.gatewayLimits.Clear()
	gw.limitsErr, gw.limits = nil, GatewayLimits{64 * MiB, 8 * MiB}
	ae.s.GatewayTaskLimits(ae.ctx)
	now = now.Add(11 * time.Minute)
	gone, cancel := context.WithCancel(ae.ctx)
	cancel()
	if got := ae.s.GatewayTaskLimits(gone); got.ArchiveBytes != 64*MiB {
		t.Errorf("gateway setting unread: %+v", got)
	}
	ae.s.gatewayLimits.Clear()
	if got := ae.s.GatewayTaskLimits(gone); got != def {
		t.Errorf("gateway setting unread, none before: %+v", got)
	}
	// A failed ask doesn't undo a good answer another caller stored while it
	// was on its way: with none before, and with an older one.
	for name, before := range map[string]bool{"none before": false, "one before": true} {
		ae.s.gatewayLimits.Clear()
		if before {
			gw.limitsErr, gw.limits = nil, GatewayLimits{32 * MiB, 8 * MiB}
			ae.s.GatewayTaskLimits(ae.ctx)
			now = now.Add(11 * time.Minute)
		}
		gw.limitsErr = errors.New("down")
		gw.limitsMeanwhile = func() {
			gw.mu.Lock()
			gw.limitsErr, gw.limits = nil, GatewayLimits{64 * MiB, 8 * MiB}
			gw.mu.Unlock()
			if got := ae.s.GatewayTaskLimits(ae.ctx); got.ArchiveBytes != 64*MiB {
				t.Errorf("%s: the good answer meanwhile: %+v", name, got)
			}
		}
		if got := ae.s.GatewayTaskLimits(ae.ctx); got.ArchiveBytes != 64*MiB {
			t.Errorf("%s: the failed ask answers %+v, not the good answer stored meanwhile", name, got)
		}
		if got := ae.s.GatewayTaskLimits(ae.ctx); got.ArchiveBytes != 64*MiB {
			t.Errorf("%s: after a failed ask, the cache holds %+v, not the good answer stored meanwhile", name, got)
		}
	}
	// A successful answer that states nothing is the default, kept ten minutes.
	ae.s.gatewayLimits.Clear()
	gw.limitsErr, gw.limits = nil, GatewayLimits{}
	calls = gw.limitsCalls
	ae.s.GatewayTaskLimits(ae.ctx)
	now = now.Add(9 * time.Minute)
	if got := ae.s.GatewayTaskLimits(ae.ctx); got != def || gw.limitsCalls != calls+1 {
		t.Errorf("an answer stating nothing: %+v after %d asks", got, gw.limitsCalls-calls)
	}
}

// A job packed against the gateway's raised limit is taken at that limit
// whatever the live figure has become since: the lookup failing or lapsing
// must not turn a 30 MiB archive away.
func TestGatewayUploadUsesJobsLimit(t *testing.T) {
	ae := newAgentEnv(t)
	gw := taskGateway(t, ae.s)
	gw.limits = GatewayLimits{ArchiveBytes: 64 << 20, FileBytes: 8 << 20}
	box := ae.gatewayBox()
	if _, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway"}}); err != nil {
		t.Fatal(err)
	}
	claim := ae.takeOn(box)
	// The lookup now fails and nothing is cached.
	gw.limits, gw.limitsErr = GatewayLimits{}, errors.New("down")
	ae.s.gatewayLimits.Clear()
	if got := ae.s.GatewayTaskLimits(ae.ctx).ArchiveBytes; got != GatewayArchiveMax {
		t.Fatalf("live figure = %d", got)
	}
	if got, err := ae.s.GatewayUploadMax(ae.ctx, ae.admin, claim.ID); got != 64<<20 || err != nil {
		t.Errorf("upload max = %d, %v, want the job's 64 MiB", got, err)
	}
	const size = 30 << 20
	v, err := ae.s.StartGatewayTask(ae.ctx, ae.admin, claim.ID, "Audit.", zeros(size), size)
	if err != nil || v.Status != "queued" {
		t.Fatalf("%d bytes: %+v, %v", size, v, err)
	}
	// A figure outside what the gateway could state isn't the job's to set:
	// the upload is held to the gateway's limit as known now.
	for _, carried := range []int64{gatewayArchiveCeiling + 1, 1 << 40, gatewayArchiveFloor - 1} {
		j := &gatewayJob{in: AgentJob{GatewayArchiveMax: carried}}
		if got := ae.s.archiveMax(ae.ctx, j); got != GatewayArchiveMax {
			t.Errorf("a job carrying %d: upload max %d, want the live %d", carried, got, GatewayArchiveMax)
		}
	}
	if got := ae.s.archiveMax(ae.ctx, &gatewayJob{in: AgentJob{GatewayArchiveMax: gatewayArchiveCeiling}}); got != gatewayArchiveCeiling {
		t.Errorf("a job carrying the ceiling: %d", got)
	}
	// Not a job of this person's, or not an audit: turned away before the
	// upload is read.
	if got, err := ae.s.GatewayUploadMax(ae.ctx, ae.admin, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such job: %d, %v", got, err)
	}
}

// httpLLM.TaskLimits reads the key's task_limits from GET /key.
func TestHTTPLLMTaskLimits(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"task_limits":{"archive_bytes":67108864,"file_bytes":8388608}}`)
	}))
	defer srv.Close()
	p := &AIProvider{Kind: "openai", BaseURL: srv.URL + "/v1", apiKey: "olm_test"}
	got, err := gatewayClient().TaskLimits(context.Background(), p)
	if err != nil || got != (GatewayLimits{ArchiveBytes: 64 << 20, FileBytes: 8 << 20}) {
		t.Fatalf("limits %+v: %v", got, err)
	}
	if path != "GET /v1/key" || auth != "Bearer olm_test" {
		t.Errorf("asked %q with %q", path, auth)
	}
}

// The runner's left_out is the gateway attempt's alone, and kept short.
func TestAuditAttemptLeftOutOnlyGateway(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"claude/opus"}})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	ae.finish(claim.ID, "failed", map[string]any{"left_out": "3 binary files (1 MiB)"}, "boom")
	if got := ae.audit(a.ID); len(got.Attempts) == 0 || got.Attempts[0].LeftOut != "" {
		t.Errorf("a CLI attempt's left out: %+v", got.Attempts)
	}

	taskGateway(t, ae.s)
	box := ae.gatewayBox()
	a, err = ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway"}})
	if err != nil {
		t.Fatal(err)
	}
	claim = ae.takeOn(box)
	ae.finish(claim.ID, "failed", map[string]any{"gateway_error": "body_too_large", "left_out": strings.Repeat("x", 500)}, "too big")
	got := ae.audit(a.ID)
	if len(got.Attempts) == 0 || len([]rune(got.Attempts[0].LeftOut)) != 300 {
		t.Fatalf("a long left out: %+v", got.Attempts)
	}
}

// The gateway raising its limit lets a larger archive through; the job
// carries the figures to the runner.
func TestGatewayRaisedArchiveLimit(t *testing.T) {
	ae := newAgentEnv(t)
	gw := taskGateway(t, ae.s)
	gw.limits = GatewayLimits{ArchiveBytes: 64 << 20, FileBytes: 8 << 20}
	box := ae.gatewayBox()
	if _, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway"}}); err != nil {
		t.Fatal(err)
	}
	claim := ae.takeOn(box)
	if in := agentJobOf(t, claim); in.GatewayArchiveMax != 64<<20 || in.GatewayFileMax != 8<<20 {
		t.Errorf("the job's limits: %d, %d", in.GatewayArchiveMax, in.GatewayFileMax)
	}
	var inv *InvalidError
	if _, err := ae.s.StartGatewayTask(ae.ctx, ae.admin, claim.ID, "Audit.", zeros(64<<20+1), 64<<20+1); !errors.As(err, &inv) || !strings.Contains(err.Error(), "64") {
		t.Errorf("past the raised limit: %v", err)
	}
	const size = 30 << 20 // over the default
	v, err := ae.s.StartGatewayTask(ae.ctx, ae.admin, claim.ID, "Audit.", zeros(size), size)
	if err != nil || v.Status != "queued" || gw.sizes[0] != size {
		t.Fatalf("%d bytes: %+v, %v (sent %v)", size, v, err, gw.sizes)
	}
}

// What the runner left out of a checkout lands on the attempt, whether the
// model answered or not.
func TestAuditAttemptLeftOut(t *testing.T) {
	ae := newAgentEnv(t)
	taskGateway(t, ae.s)
	box := ae.gatewayBox()
	const left = "350 binary files (16.9 MiB), 5 files over 256 KiB (2.1 MiB)"

	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway"}})
	if err != nil {
		t.Fatal(err)
	}
	claim := ae.takeOn(box)
	ae.finish(claim.ID, "done", map[string]any{"answer": "Read it.", "left_out": left, "structured": map[string]any{"summary": "Read the merge.", "findings": []any{gatewayFinding}}}, "")
	got := ae.audit(a.ID)
	if got.Status != AuditDone || len(got.Attempts) != 1 || got.Attempts[0].Outcome != "answered" || got.Attempts[0].LeftOut != left {
		t.Fatalf("answered: %s %+v", got.Status, got.Attempts)
	}

	a, err = ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"gateway"}})
	if err != nil {
		t.Fatal(err)
	}
	claim = ae.takeOn(box)
	ae.finish(claim.ID, "failed", map[string]any{"gateway_error": "body_too_large", "left_out": left}, "The checkout packs to more than 20 MiB.")
	got = ae.audit(a.ID)
	if len(got.Attempts) == 0 || got.Attempts[0].LeftOut != left || got.Attempts[0].Error == "" {
		t.Fatalf("failed: %s %+v", got.Status, got.Attempts)
	}
}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }
