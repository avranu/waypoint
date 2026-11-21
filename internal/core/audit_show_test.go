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
	if rows := strings.Count(c, "\n| "); rows != 6 { // the header and 5 findings
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

func longFindings(n int, from int) []map[string]any {
	var out []map[string]any
	for i := range n {
		out = append(out, map[string]any{"title": fmt.Sprintf("Long finding %d", from+i), "path": "internal/a.go", "line": i + 1, "severity": "low",
			"problem": strings.Repeat("it goes wrong. ", 200), "confirm": "y"})
	}
	return out
}

// The findings the guard cuts are kept in the audit's answer text, and the
// summary says where (WP-1421).
func TestAuditGuardCutIsKeptInTheAnswer(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Instructions: "Look.", Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	ae.finish(claim.ID, "done", auditResult("Read it.", longFindings(150, 1)...), "")
	got := ae.audit(a.ID)
	if len(got.Findings) != findingsGuard || !strings.Contains(got.Summary, "50 were cut") || !strings.Contains(got.Summary, "they are in the audit's answer text") || strings.Contains(got.Summary, "answer as it came") {
		t.Fatalf("%d findings, summary %q", len(got.Findings), got.Summary)
	}
	if !strings.Contains(got.Answer, auditCutHeading) || !strings.Contains(got.Answer, "Long finding 150") || !strings.Contains(got.Answer, "Long finding 101") || strings.Contains(got.Answer, "Long finding 100\"") {
		t.Errorf("the answer lacks the cut findings (%d bytes)", len(got.Answer))
	}
}

// A runaway answer's cut findings are not stored without bound, and the
// summary says when they were cut short.
func TestAuditGuardCutTextIsBounded(t *testing.T) {
	a := &CodeAudit{}
	f := &auditAnswer{}
	for i := range findingsGuard + 3 {
		f.Findings = append(f.Findings, CodeAuditFinding{Title: fmt.Sprintf("F%d", i), Severity: "low", Problem: strings.Repeat("x", auditCutMax/2)})
	}
	f.keep(a)
	if f.guardCut != 3 || !f.cutShort || len([]rune(f.cutJSON)) > auditCutMax+1 {
		t.Fatalf("cut %d short %v len %d", f.guardCut, f.cutShort, len(f.cutJSON))
	}
	if n := f.keepNote(a, findingsGuard+3); !strings.Contains(n, "cut short") {
		t.Errorf("note %q", n)
	}
}

func auditWithFindings(n, shown int) *CodeAudit {
	a := &CodeAudit{ID: 7, Model: "m", Title: "T", Instructions: "x", MaxFindings: shown}
	for i := range n {
		a.Findings = append(a.Findings, CodeAuditFinding{ID: fmt.Sprintf("7.%d", i+1), Ordinal: i + 1, Title: fmt.Sprintf("Finding %d", i+1), Severity: "low", Problem: "p", State: "open"})
	}
	return a
}

// The fix task of an ordinary audit lists the findings shown at a time and
// says how many more there are and how to read them (WP-1421, 1248.5).
func TestAuditTaskListsTheShownAndCountsTheRest(t *testing.T) {
	text := auditTaskText(auditWithFindings(8, 5), "https://wp.test")
	if !strings.Contains(text, "### 7.5 ") || strings.Contains(text, "### 7.6 ") {
		t.Errorf("the task lists the wrong findings:\n%s", text)
	}
	if !strings.Contains(text, "3 more findings: `wp audit show 7 --all` lists them; mark them too") {
		t.Errorf("no line for the rest:\n%s", text)
	}
	// Exactly what is shown, or fewer: no line for more.
	for _, n := range []int{5, 3} {
		if text := auditTaskText(auditWithFindings(n, 5), "https://wp.test"); strings.Contains(text, "more finding") || !strings.Contains(text, fmt.Sprintf("### 7.%d ", n)) {
			t.Errorf("%d findings, shown 5:\n%s", n, text)
		}
	}
}
