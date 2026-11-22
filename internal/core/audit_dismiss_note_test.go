package core

import (
	"strings"
	"testing"
)

// An agent that dismisses a carried audit finding (AUDIT.N) says why, as for
// a bot's finding; a person need not, and other verdicts take no note
// (WP-1370: wp pr dismiss's help says agents must give one).
func TestAgentDismissesAuditFindingWithANote(t *testing.T) {
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
	f := ae.audit(a.ID).Findings
	if len(f) != 3 {
		t.Fatalf("findings = %d", len(f))
	}
	ag := *ae.admin
	ag.Agent = "claude"
	for _, note := range []string{"", "  \n"} {
		_, err := ae.s.MarkAuditFindings(ae.ctx, &ag, MarkFindings{IDs: []string{f[0].ID}, State: "rejected", Note: note})
		if err == nil || !strings.Contains(err.Error(), "say why") {
			t.Errorf("agent dismissal with note %q: %v, want a refusal asking for one", note, err)
		}
	}
	if _, err := ae.s.MarkAuditFindings(ae.ctx, &ag, MarkFindings{IDs: []string{f[0].ID}, State: "rejected", Note: "the caller holds the lock"}); err != nil {
		t.Errorf("agent dismissal with a note: %v", err)
	}
	if _, err := ae.s.MarkAuditFindings(ae.ctx, &ag, MarkFindings{IDs: []string{f[1].ID}, State: "confirmed"}); err != nil {
		t.Errorf("agent confirming with no note: %v", err)
	}
	if _, err := ae.s.MarkAuditFindings(ae.ctx, ae.admin, MarkFindings{IDs: []string{f[2].ID}, State: "rejected"}); err != nil {
		t.Errorf("a person dismissing with no note: %v", err)
	}
}
