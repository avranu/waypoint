package core

import (
	"fmt"
	"strings"
	"testing"
)

func lowFindings(n int) []map[string]any {
	var out []map[string]any
	for i := range n {
		out = append(out, map[string]any{"title": fmt.Sprintf("Nit %d", i+1), "path": "internal/a.go", "line": i + 1, "severity": "low", "problem": "x", "confirm": "y"})
	}
	return out
}

// max_findings is how many are shown at a time (WP-1421): an audit asked
// with 5 whose model reports 12 keeps 12, the pull request's comment lists
// 5 and counts the 7 more, and the model was told no number.
func TestAuditShownAtATime(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Instructions: "Look.", MaxFindings: 5, Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	in := agentJobOf(t, claim)
	if strings.Contains(in.JSONSchema, "maxItems") || strings.Contains(in.Prompt, "At most") || !strings.Contains(in.Prompt, "Report every problem you find, worst first") {
		t.Errorf("the model was told a number:\n%s\n%s", in.JSONSchema, in.Prompt)
	}
	for _, n := range []string{" 5 ", " 5.", "at most 5"} {
		if strings.Contains(in.Prompt, "findings"+n) || strings.Contains(in.Prompt, "most"+n) {
			t.Errorf("the prompt names the number %q", n)
		}
	}
	ae.finish(claim.ID, "done", auditResult("Read it.", lowFindings(12)...), "")
	got := ae.audit(a.ID)
	if got.Status != AuditDone || len(got.Findings) != 12 || got.Counts.Total != 12 || got.MaxFindings != 5 || AuditShown(got) != 5 || strings.Contains(got.Summary, "cut") {
		t.Fatalf("audit = %s %d findings, %q", got.Status, len(got.Findings), got.Summary)
	}
	c := auditComment(got, "https://wp.example")
	if rows := strings.Count(c, "\n| "); rows != 7 { // the header, the rule and 5 findings
		t.Errorf("rows = %d:\n%s", rows, c)
	}
	if !strings.Contains(c, "| 5 | low") || strings.Contains(c, "| 6 | low") || !strings.Contains(c, "7 more findings: https://wp.example/audits/") || !strings.Contains(c, "### Audit: 12 findings") {
		t.Errorf("comment:\n%s", c)
	}
}

// Past the guard, 100 are kept and the summary says how many were cut.
func TestAuditPastTheGuardSaysSo(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Instructions: "Look.", Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	ae.finish(claim.ID, "done", auditResult("Read it.", lowFindings(findingsGuard+3)...), "")
	got := ae.audit(a.ID)
	if len(got.Findings) != findingsGuard || !strings.Contains(got.Summary, "The answer held 103 findings; 100 are kept") || !strings.Contains(got.Summary, "3 were cut") {
		t.Errorf("%d findings, summary %q", len(got.Findings), got.Summary)
	}
}

// A rule or audit stored with the old top (30) or more still loads and runs.
func TestStoredMaxFindingsOverTheOldTopStillRuns(t *testing.T) {
	ae := newAgentEnv(t)
	for _, n := range []int{30, 50} {
		a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Instructions: "Look.", MaxFindings: n, Via: "web"})
		if err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		if _, err := ae.s.db.W.ExecContext(ae.ctx, `UPDATE code_audits SET max_findings = 250 WHERE id = ?`, a.ID); err != nil {
			t.Fatal(err)
		}
		ae.work()
		claim := ae.take()
		ae.finish(claim.ID, "done", auditResult("Read it.", lowFindings(3)...), "")
		got := ae.audit(a.ID)
		if got.Status != AuditDone || got.MaxFindings != 250 || AuditShown(got) != 250 || len(got.Findings) != 3 {
			t.Errorf("%d: %s %d %d", n, got.Status, got.MaxFindings, len(got.Findings))
		}
	}
	c, err := cleanAuditCriteria(AuditInput{MaxFindings: 100})
	if err != nil || c.maxFindings != 100 {
		t.Errorf("100: %+v %v", c, err)
	}
	if _, err := cleanAuditCriteria(AuditInput{MaxFindings: 101}); err == nil {
		t.Error("101 was taken")
	}
}
