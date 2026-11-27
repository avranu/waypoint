package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

type rejectionEnv struct {
	*agentEnv
	audit    int64
	sessions int
}

// newRejectionEnv is an audit of a pull request with three findings, found by
// model.
func newRejectionEnv(t *testing.T, model string) *rejectionEnv {
	t.Helper()
	ae := newAgentEnv(t)
	ae.prHook("opened", ForgePR{Number: 7, Title: "Export the board", HeadRef: "export", HeadSHA: "abc1234def", HeadRepo: "me/app", Author: ae.admin.User.Username})
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{PRID: ae.pr(7).ID, Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	ae.finish(claim.ID, "done", auditResult("Read it.",
		map[string]any{"title": "One", "path": "a.go", "line": 1, "severity": "P1", "problem": "x"},
		map[string]any{"title": "Two", "path": "b.go", "line": 2, "severity": "P1", "problem": "y"},
		map[string]any{"title": "Three", "path": "c.go", "line": 3, "severity": "P1", "problem": "z"},
	), "")
	ae.work()
	ae.exec(`UPDATE code_audits SET model = ? WHERE id = ?`, model, a.ID)
	return &rejectionEnv{agentEnv: ae, audit: a.ID}
}

func (ae *agentEnv) exec(q string, args ...any) {
	ae.t.Helper()
	if _, err := ae.s.db.W.Exec(q, args...); err != nil {
		ae.t.Fatal(err)
	}
}

func (re *rejectionEnv) session(model, effort string) *Actor {
	re.t.Helper()
	re.sessions++
	res, err := re.s.db.W.Exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, model, started_at, last_seen_at) VALUES (?, 'devhost', ?, ?, 5, 5)`, re.admin.User.ID, fmt.Sprintf("cli-%d", re.sessions), model)
	if err != nil {
		re.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if effort != "" {
		re.exec(`INSERT INTO session_observed(session_id, at, kind, value, source) VALUES (?, 6, 'effort', ?, 'test')`, id, effort)
	}
	ag := *re.admin
	ag.Agent, ag.Session = "claude", id
	return &ag
}

func (re *rejectionEnv) reject(actor *Actor, n int, evidence string) error {
	_, err := re.s.MarkAuditFindings(re.ctx, actor, MarkFindings{IDs: []string{re.id(n)}, State: "rejected", Note: "wrong", Evidence: evidence})
	return err
}

func (re *rejectionEnv) id(n int) string { return fmt.Sprintf("%d.%d", re.audit, n) }

func (re *rejectionEnv) finding(n int) CodeAuditFinding {
	re.t.Helper()
	return re.agentEnv.audit(re.audit).Findings[n-1]
}

func TestHaikuSessionCannotRejectSonnetsFinding(t *testing.T) {
	re := newRejectionEnv(t, "claude/sonnet")
	err := re.reject(re.session("claude-haiku-5-5", "high"), 1, "a.go:1 holds the lock")
	if err == nil || !strings.Contains(err.Error(), "haiku 5.5 (high) can't reject") || !strings.Contains(err.Error(), "found by sonnet") || !strings.Contains(err.Error(), "wp audit mark "+re.id(1)+" confirmed --issue KEY") {
		t.Fatalf("err = %v", err)
	}
	if f := re.finding(1); f.State != "open" {
		t.Errorf("state = %s", f.State)
	}
}

func TestSessionOfUnknownModelCannotReject(t *testing.T) {
	re := newRejectionEnv(t, "claude/haiku")
	err := re.reject(re.session("", ""), 1, "a.go:1")
	if err == nil || !strings.Contains(err.Error(), "doesn't know which model this session runs") {
		t.Fatalf("err = %v", err)
	}
}

func TestRejectingEffortIsComparedOnlyOnceAuditsRecordTheFinders(t *testing.T) {
	re := newRejectionEnv(t, "claude-sonnet-5-5")
	// Audits record no effort, so the finder's is unknown and counts as equal:
	// the effort comparison is in the strength package's tests.
	if err := re.reject(re.session("claude-sonnet-5-5", "low"), 1, "a.go:1"); err != nil {
		t.Fatalf("equal model, unknown finder effort: %v", err)
	}
	if err := re.reject(re.session("claude-sonnet-5", "max"), 2, "b.go:2"); err == nil || !strings.Contains(err.Error(), "sonnet 5.0 (max) can't reject") {
		t.Fatalf("older sonnet: %v", err)
	}
}

func TestEqualOrStrongerRejectsWithEvidenceAndKeepsIt(t *testing.T) {
	re := newRejectionEnv(t, "claude/sonnet")
	if err := re.reject(re.session("claude-opus-5-5", "high"), 1, "  a.go:1 holds the lock  "); err != nil {
		t.Fatal(err)
	}
	f := re.finding(1)
	if f.State != "rejected" || f.Evidence != "a.go:1 holds the lock" || f.DecidedModel != "claude-opus-5-5" || f.DecidedEffort != "high" || !strings.HasPrefix(f.DecidedStrength, "at_least: ") {
		t.Errorf("finding = %+v", f)
	}
	if err := re.reject(re.session("claude-sonnet-5-5", ""), 2, "b.go:2"); err != nil {
		t.Errorf("an equal one: %v", err)
	}
}

func TestRejectionNeedsEvidence(t *testing.T) {
	re := newRejectionEnv(t, "claude/sonnet")
	for _, ev := range []string{"", "  \n"} {
		err := re.reject(re.session("claude-opus-5-5", ""), 1, ev)
		if err == nil || !strings.Contains(err.Error(), "--evidence") {
			t.Errorf("evidence %q: %v", ev, err)
		}
	}
	if err := re.reject(re.session("claude-opus-5-5", ""), 1, strings.Repeat("é", 4001)); err == nil || !strings.Contains(err.Error(), "at most 4000") {
		t.Errorf("long evidence: %v", err)
	}
}

func TestPersonRejectsWithoutEvidenceOrStrength(t *testing.T) {
	re := newRejectionEnv(t, "claude/opus")
	if err := re.reject(re.admin, 1, ""); err != nil {
		t.Fatal(err)
	}
	if f := re.finding(1); f.State != "rejected" || f.DecidedModel != "" || f.DecidedStrength != "" {
		t.Errorf("finding = %+v", f)
	}
}

func TestRoundsOwnJobRejectsWithoutStrengthOrEvidence(t *testing.T) {
	re := newRejectionEnv(t, "claude/opus")
	res, err := re.s.db.W.Exec(`INSERT INTO runner_jobs(owner_id, kind, input, timeout, created_at) VALUES (?, 'agent', '{"model":"haiku"}', 60, 0)`, re.admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, _ := res.LastInsertId()
	if res, err := re.s.db.W.Exec(`UPDATE babysits SET job_id = ?, ended_at = NULL WHERE pr_id = ?`, job, re.pr(7).ID); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n == 0 {
		re.exec(`INSERT INTO babysits(pr_id, job_id, owner_id, created_at, updated_at) VALUES (?, ?, ?, 0, 0)`, re.pr(7).ID, job, re.admin.User.ID)
	}
	ag := *re.admin
	ag.Agent, ag.RunnerJob = "claude", job
	if err := re.reject(&ag, 1, ""); err != nil {
		t.Fatalf("the round's own job: %v", err)
	}
}

func TestSessionRejectsItsOwnRoundsFindings(t *testing.T) {
	re := newRejectionEnv(t, "claude/opus")
	ag := re.session("claude-haiku-5-5", "")
	var project string
	if err := re.s.db.R.QueryRow(`SELECT key FROM projects LIMIT 1`).Scan(&project); err != nil {
		t.Fatal(err)
	}
	task := re.issue(re.admin, IssueInput{Project: project, Title: "The task"})
	res, err := re.s.db.W.Exec(`INSERT INTO work_log(kind, issue_id, session_id, state, created_at, updated_at, audit_id) VALUES ('review', ?, ?, 'running', 0, 0, ?)`, task.ID, ag.Session, re.audit)
	if err != nil {
		t.Fatal(err)
	}
	round, _ := res.LastInsertId()
	re.exec(`UPDATE code_audits SET request_key = ? WHERE id = ?`, "review-round:"+strconv.FormatInt(round, 10), re.audit)
	if err := re.reject(ag, 1, ""); err != nil {
		t.Fatalf("a session's own round: %v", err)
	}
	if err := re.reject(re.session("claude-haiku-5-5", ""), 2, "b.go:2"); err == nil || !strings.Contains(err.Error(), "haiku 5.5 can't reject "+re.id(2)+", found by opus") {
		t.Errorf("another session rejected the round's finding: %v", err)
	}
}

func TestRejectionOfUnrankedModelSaysWhichModel(t *testing.T) {
	re := newRejectionEnv(t, "claude/haiku")
	err := re.reject(re.session("deepseek-4.1", ""), 1, "a.go:1")
	if err == nil || !strings.Contains(err.Error(), "Waypoint doesn't rank deepseek-4.1, the model this session runs, so it can't reject") {
		t.Fatalf("err = %v", err)
	}
}

func TestEvidenceGoesOnlyWithARejection(t *testing.T) {
	re := newRejectionEnv(t, "claude/sonnet")
	_, err := re.s.MarkAuditFindings(re.ctx, re.admin, MarkFindings{IDs: []string{re.id(1)}, State: "confirmed", Evidence: "a.go:1"})
	if err == nil || !strings.Contains(err.Error(), "evidence goes with a rejection") {
		t.Fatalf("err = %v", err)
	}
}

func TestEvidenceOfExactlyTheLimit(t *testing.T) {
	re := newRejectionEnv(t, "claude/sonnet")
	if err := re.reject(re.session("claude-opus-5-5", ""), 1, strings.Repeat("é", 4000)); err != nil {
		t.Fatal(err)
	}
	if f := re.finding(1); len([]rune(f.Evidence)) != 4000 {
		t.Errorf("evidence kept %d characters", len([]rune(f.Evidence)))
	}
}

func TestRejectingAFindingThatIsNotThere(t *testing.T) {
	re := newRejectionEnv(t, "claude/sonnet")
	_, err := re.s.MarkAuditFindings(re.ctx, re.session("claude-opus-5-5", ""), MarkFindings{IDs: []string{re.id(9)}, State: "rejected", Note: "wrong", Evidence: "x.go:1"})
	if err == nil || !strings.Contains(err.Error(), "finding "+re.id(9)) {
		t.Fatalf("err = %v", err)
	}
}

func TestReopeningOrConfirmingClearsTheRejectionsRecord(t *testing.T) {
	re := newRejectionEnv(t, "claude/sonnet")
	ag := re.session("claude-opus-5-5", "high")
	for n, state := range map[int]string{1: "open", 2: "confirmed"} {
		if err := re.reject(ag, n, "a.go:1 holds the lock"); err != nil {
			t.Fatal(err)
		}
		if _, err := re.s.MarkAuditFindings(re.ctx, re.admin, MarkFindings{IDs: []string{re.id(n)}, State: state, Note: "it is real"}); err != nil {
			t.Fatal(err)
		}
		if f := re.finding(n); f.State != state || f.Evidence != "" || f.DecidedModel != "" || f.DecidedEffort != "" || f.DecidedStrength != "" {
			t.Errorf("%s: finding = %+v", state, f)
		}
	}
}

func TestRunnerJobRejectsAsItsInputsModelAndEffort(t *testing.T) {
	re := newRejectionEnv(t, "claude-sonnet-5-5")
	res, err := re.s.db.W.Exec(`INSERT INTO runner_jobs(owner_id, kind, input, timeout, created_at) VALUES (?, 'agent', '{"model":"claude-haiku-5-5","effort":"high"}', 60, 0)`, re.admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, _ := res.LastInsertId()
	ag := *re.admin
	ag.Agent, ag.RunnerJob = "claude", job
	if err := re.reject(&ag, 1, "a.go:1"); err == nil || !strings.Contains(err.Error(), "haiku 5.5 (high) can't reject") {
		t.Fatalf("err = %v", err)
	}
	re.exec(`UPDATE runner_jobs SET input = '{"model":"claude-opus-5-5","effort":"low"}' WHERE id = ?`, job)
	if err := re.reject(&ag, 1, "a.go:1"); err != nil {
		t.Fatal(err)
	}
	if f := re.finding(1); f.DecidedModel != "claude-opus-5-5" || f.DecidedEffort != "low" {
		t.Errorf("finding = %+v", f)
	}
}

func TestSessionEffortFallsBackToItsSnapshot(t *testing.T) {
	re := newRejectionEnv(t, "claude-sonnet-5-5")
	ag := re.session("claude-sonnet-5-5", "")
	re.exec(`INSERT INTO session_snapshots(session_id, at, trigger_event, effort) VALUES (?, 7, 'test', 'max')`, ag.Session)
	if err := re.reject(ag, 1, "a.go:1"); err != nil {
		t.Fatal(err)
	}
	if f := re.finding(1); f.DecidedEffort != "max" {
		t.Errorf("effort = %q", f.DecidedEffort)
	}
}

func TestHelperPanelFindingCountsAsHaikus(t *testing.T) {
	re := newRejectionEnv(t, "claude/opus")
	re.exec(`UPDATE code_audit_findings SET lead = 'tests.1' WHERE audit_id = ? AND ordinal = 1`, re.audit)
	// A lead naming no panel check is no panel finding, as PanelFinding reads it.
	re.exec(`UPDATE code_audit_findings SET lead = 'check.1' WHERE audit_id = ? AND ordinal = 2`, re.audit)
	re.exec(`UPDATE code_audit_findings SET lead = 'goals.2' WHERE audit_id = ? AND ordinal = 3`, re.audit)
	// One finding it may reject and one it may not: none of the batch is.
	_, err := re.s.MarkAuditFindings(re.ctx, re.session("claude-haiku-5-5", ""), MarkFindings{IDs: []string{re.id(3), re.id(2)}, State: "rejected", Note: "wrong", Evidence: "c.go:3"})
	if err == nil || !strings.Contains(err.Error(), "can't reject "+re.id(2)) {
		t.Fatalf("mixed batch: %v", err)
	}
	if f := re.finding(3); f.State != "open" {
		t.Errorf("the panel's finding of a refused batch is %s", f.State)
	}
	if err := re.reject(re.session("claude-haiku-5-5", ""), 1, "a.go:1"); err != nil {
		t.Fatalf("haiku against the panel's finding: %v", err)
	}
	if err := re.reject(re.session("claude-haiku-5-5", ""), 2, "b.go:2"); err == nil || !strings.Contains(err.Error(), "found by opus") {
		t.Fatalf("haiku against opus's finding with a lead naming no check: %v", err)
	}
}

// Nothing is known of a finder outside the ordered families (an audit of
// another provider), so it counts as the strongest family (WP-1825).
func TestOtherFamilyFindingTakesTheStrongestFamily(t *testing.T) {
	re := newRejectionEnv(t, "cursor/grok-4.7-high")
	err := re.reject(re.session("claude-opus-5-5", "max"), 1, "a.go:1")
	if err == nil || !strings.Contains(err.Error(), "only fable or mythos may reject") {
		t.Fatalf("opus against grok's finding: %v", err)
	}
	if err := re.reject(re.session("claude-fable-5-2", ""), 1, "a.go:1"); err != nil {
		t.Fatal(err)
	}
	if f := re.finding(1); !strings.HasPrefix(f.DecidedStrength, "at_least: ") {
		t.Errorf("strength = %q", f.DecidedStrength)
	}
}

// A review's own report from a round no session started has no author
// model: it isn't left open to any agent, but counts as the strongest.
func TestOwnReportWithoutAuthorSessionTakesTheStrongestFamily(t *testing.T) {
	re := newRejectionEnv(t, ownReportModel)
	re.exec(`UPDATE code_audits SET request_key = 'review-round:999999:own' WHERE id = ?`, re.audit)
	if err := re.reject(re.session("claude-opus-5-5", ""), 1, "a.go:1"); err == nil || !strings.Contains(err.Error(), "found by unknown") {
		t.Fatalf("opus against an own report of no session: %v", err)
	}
}

// A finding that doesn't exist says so, before asking for evidence.
func TestRejectingMissingFindingSaysNotFound(t *testing.T) {
	re := newRejectionEnv(t, "claude-sonnet-5-5")
	_, err := re.s.MarkAuditFindings(re.ctx, re.session("claude-opus-5-5", ""), MarkFindings{IDs: []string{re.id(9)}, State: "rejected", Note: "wrong"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// Only the verdict's underscores are spelled out; the reason, which can name
// a model like my_model-1, is shown as stored (WP-1825).
func TestRejectedAsSpellsOutOnlyTheVerdict(t *testing.T) {
	for in, want := range map[string]string{
		"not_compared: my_model-1 isn't a model Waypoint ranks": "m (high), not compared: my_model-1 isn't a model Waypoint ranks",
		"at_least: opus is a stronger family than sonnet":       "m (high), at least: opus is a stronger family than sonnet",
		"at_least": "m (high), at least",
	} {
		if got := RejectedAs(CodeAuditFinding{DecidedModel: "m", DecidedEffort: "high", DecidedStrength: in}); got != want {
			t.Errorf("RejectedAs(%q) = %q, want %q", in, got, want)
		}
	}
}
