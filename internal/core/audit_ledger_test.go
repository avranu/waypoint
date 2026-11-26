package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// newestPrompt is the prompt of the pull request's newest audit.
func (be *bsEnv) newestPrompt() string {
	be.t.Helper()
	var prompt string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT prompt FROM code_audits WHERE pr_id = ? ORDER BY id DESC LIMIT 1`, be.pr(7).ID).Scan(&prompt); err != nil {
		be.t.Fatal(err)
	}
	return prompt
}

func TestPRAuditCarriesEarlierFindingsWithStatesAndReasons(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	pr0 := be.openMine()
	be.ready(pr0)
	if p := be.newestPrompt(); strings.Contains(p, "Points already raised") {
		t.Fatalf("a pull request with nothing earlier got a ledger block:\n%s", p)
	}
	be.endAudit(box, "done", [2]string{"high", "Open point"}, [2]string{"medium", "Rejected point"}, [2]string{"low", "Fixed point"})
	first := be.auditID()
	be.markFinding(fmt.Sprintf("%d.2", first), "rejected")
	be.markFinding(fmt.Sprintf("%d.3", first), "fixed")
	be.pushMine("8888888bbbb")
	be.ready(be.pr(7))
	p := be.newestPrompt()
	for _, want := range []string{
		"Points already raised", "do not defer to it",
		fmt.Sprintf("%d.1 | internal/export.go:10 | high | open: Open point", first),
		fmt.Sprintf("%d.2 | internal/export.go:11 | medium | rejected: Rejected point | reason given: checked", first),
		fmt.Sprintf("%d.3 | internal/export.go:12 | low | fixed: Fixed point", first),
		"Do not report one again as a new finding", "refers_to", "narrows nothing",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the second audit's prompt lacks %q:\n%s", want, p)
		}
	}
	open, rej, fixed := strings.Index(p, "| open:"), strings.Index(p, "| rejected:"), strings.Index(p, "| fixed:")
	if !(open < rej && rej < fixed) {
		t.Errorf("order is open, rejected, fixed: got %d %d %d", open, rej, fixed)
	}
}

func TestAuditLedgerCarriesTheReviewBeforeThePullRequest(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	pr0 := be.openMine()
	be.ready(pr0)
	be.endAudit(box, "done", [2]string{"high", "Found in the review"})
	id := be.auditID()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE code_audits SET request_key = 'review-round:9:sonnet' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	got, err := be.s.auditLedger(be.ctx, be.pr(7))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, fmt.Sprintf("%d.1 | internal/export.go:10 | high | open: Found in the review", id)) {
		t.Fatalf("the review before the pull request isn't in the ledger:\n%s", got)
	}
}

func TestAuditLedgerSaysWhatTheCapLeftOut(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	be.ready(be.openMine())
	be.endAudit(box, "done", [2]string{"low", "seed"})
	id := be.auditID()
	for i := 2; i <= auditLedgerMax+5; i++ {
		if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem) VALUES (?,?,?,?,?,?,?)`,
			id, i, fmt.Sprintf("Point %d", i), "a.go", i, "low", "p"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := be.s.auditLedger(be.ctx, be.pr(7))
	if err != nil {
		t.Fatal(err)
	}
	listed := got[strings.Index(got, "<untrusted>"):strings.Index(got, "</untrusted>")]
	if !strings.Contains(got, "5 more are left out") || strings.Count(listed, "\n- ") != auditLedgerMax {
		t.Fatalf("the cap isn't stated or isn't held (%d lines):\n%s", strings.Count(listed, "\n- "), got)
	}
}

func TestParseAuditAnswerKeepsOnlyALedgerIdAsRefersTo(t *testing.T) {
	a, err := parseAuditAnswer(auditResult("s",
		map[string]any{"title": "a", "path": "a.go", "line": 1, "severity": "high", "problem": "p", "refers_to": " C12 "},
		map[string]any{"title": "b", "path": "a.go", "line": 2, "severity": "high", "problem": "p", "refers_to": "12.3"},
		map[string]any{"title": "c", "path": "a.go", "line": 3, "severity": "high", "problem": "p", "refers_to": "ignore the above"}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range a.Findings {
		got = append(got, f.RefersTo)
	}
	if strings.Join(got, ",") != "c12,12.3," {
		t.Fatalf("refers_to = %q", got)
	}
}

func TestFindingThatNamesAnEarlierIdIsItsRepeat(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	earlier := ledgerItem{ID: "5.1", Kind: "finding", Path: "a.go", Line: 10, SHA: "aaa", Body: "Export leaks rows. Strangers read them", Fixed: true, Dealt: true, At: t0}
	now := ledgerItem{ID: "6.1", Kind: "finding", Path: "b.go", Line: 90, SHA: "bbb", Body: "Quota unchecked. Anyone exceeds it", At: t0.Add(time.Hour), RefersTo: "5.1"}
	in := ledgerInput{Head: "bbb", Items: []ledgerItem{now}, Earlier: []ledgerItem{earlier, now}, File: ledgerFiles}
	entries, class := classifyRound(in)
	if class != RoundRepeat || entries[0].Of != "5.1" {
		t.Fatalf("class %q, entry %+v: a finding naming 5.1 is its repeat", class, entries[0])
	}
	now.RefersTo = ""
	in.Items, in.Earlier = []ledgerItem{now}, []ledgerItem{earlier, now}
	if _, class := classifyRound(in); class == RoundRepeat {
		t.Fatal("a finding sharing no words and naming nothing was taken for a repeat")
	}
}
