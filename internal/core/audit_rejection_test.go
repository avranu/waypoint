package core

import (
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

func TestLowerEffortSonnetCannotRejectHigherEffortSonnet(t *testing.T) {
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
	if err := re.reject(re.session("claude-haiku-5-5", ""), 2, "b.go:2"); err == nil {
		t.Error("another session rejected the round's finding")
	}
}

func TestHelperPanelFindingCountsAsHaikus(t *testing.T) {
	re := newRejectionEnv(t, "claude/opus")
	re.exec(`UPDATE code_audit_findings SET lead = 'check.1' WHERE audit_id = ? AND ordinal = 1`, re.audit)
	if err := re.reject(re.session("claude-haiku-5-5", ""), 1, "a.go:1"); err != nil {
		t.Fatalf("haiku against the panel's finding: %v", err)
	}
	if err := re.reject(re.session("claude-haiku-5-5", ""), 2, "b.go:2"); err == nil || !strings.Contains(err.Error(), "found by opus") {
		t.Fatalf("haiku against opus's finding: %v", err)
	}
}

func TestOtherFamilyFindingIsNotCompared(t *testing.T) {
	re := newRejectionEnv(t, "cursor/grok-4.7-high")
	if err := re.reject(re.session("claude-haiku-5-5", ""), 1, "a.go:1"); err != nil {
		t.Fatal(err)
	}
	if f := re.finding(1); !strings.HasPrefix(f.DecidedStrength, "not_compared: ") {
		t.Errorf("strength = %q", f.DecidedStrength)
	}
}
