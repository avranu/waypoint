package core

import (
	"strings"
	"testing"
	"time"
)

func TestParseAuditSince(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in, spec string
		at       time.Time
	}{
		{"", "7d", now.AddDate(0, 0, -7)},
		{"7d", "7d", now.AddDate(0, 0, -7)},
		{"7 days ago", "7d", now.AddDate(0, 0, -7)},
		{"24h", "24h", now.Add(-24 * time.Hour)},
		{"2 weeks", "2w", now.AddDate(0, 0, -14)},
		{"1mo", "1mo", now.AddDate(0, 0, -30)},
		{"2026-10-28", "2026-10-28", time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)},
		{"2026-10-28T09:30:00+02:00", "2026-10-28T07:30:00Z", time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)},
	} {
		at, spec, err := parseAuditSince(c.in, now)
		if err != nil || spec != c.spec || !at.Equal(c.at) {
			t.Errorf("%q: %s %s %v, want %s %s", c.in, at, spec, err, c.at, c.spec)
		}
	}
	for _, bad := range []string{"0d", "tomorrow", "2026-12-01", "2y", "500d", "-3d"} {
		if _, _, err := parseAuditSince(bad, now); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	if w := sinceWords("7d", now); w != "the last 7 days" {
		t.Error(w)
	}
	if w := sinceWords("1w", now); w != "the last week" {
		t.Error(w)
	}
	if w := sinceWords("2026-10-28", time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)); w != "since 28 Oct 2026" {
		t.Error(w)
	}
}

func TestCleanAuditCriteria(t *testing.T) {
	c, err := cleanAuditCriteria(AuditInput{Focus: []string{"Security, concurrency", "security"}, Paths: []string{"./internal/core\nweb/src/**/*.tsx", "internal/core"},
		Exclude: []string{"*_test.go"}, MinSeverity: "Medium", MaxFindings: 5})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.focus, ",") != "security,concurrency" || strings.Join(c.paths, ",") != "internal/core,web/src/**/*.tsx" ||
		strings.Join(c.exclude, ",") != "*_test.go" || c.minSeverity != "medium" || c.maxFindings != 5 {
		t.Errorf("criteria = %+v", c)
	}
	if c, err := cleanAuditCriteria(AuditInput{MinSeverity: "low"}); err != nil || c.minSeverity != "" || c.maxFindings != 0 {
		t.Errorf("low is everything: %+v %v", c, err)
	}
	for _, bad := range []AuditInput{
		{Focus: []string{"vibes"}},
		{Paths: []string{":(top)etc"}},
		{Paths: []string{"../secrets"}},
		{Exclude: []string{"/etc"}},
		{Exclude: []string{"-x"}},
		{Paths: []string{"a\\b"}},
		{MinSeverity: "urgent"},
		{MaxFindings: 101},
		{MaxFindings: -1},
		{Paths: []string{strings.Repeat("a/", 101)}},
		{Paths: []string{"1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21"}},
	} {
		if _, err := cleanAuditCriteria(bad); err == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
}

func TestAuditAnswerKeep(t *testing.T) {
	f := &auditAnswer{Findings: []CodeAuditFinding{{Severity: "critical"}, {Severity: "high"}, {Severity: "medium"}, {Severity: "low"}}}
	if n := f.keep(&CodeAudit{MinSeverity: "high"}); n != 2 || len(f.Findings) != 2 {
		t.Errorf("high floor: dropped %d, kept %+v", n, f.Findings)
	}
	// max_findings is how many are shown at a time, not a cut: all are kept.
	if n := f.keep(&CodeAudit{MaxFindings: 1}); n != 0 || len(f.Findings) != 2 || f.Findings[0].Severity != "critical" {
		t.Errorf("asked for 1: dropped %d, kept %+v", n, f.Findings)
	}
	// An ordinary audit with max_findings 5 whose answer holds 8 keeps 8 and says so.
	var eight []CodeAuditFinding
	for range 8 {
		eight = append(eight, CodeAuditFinding{Severity: "low"})
	}
	g := &auditAnswer{Findings: eight}
	ca := &CodeAudit{MaxFindings: 5}
	if n := g.keep(ca); n != 0 || len(g.Findings) != 8 {
		t.Errorf("5 asked, 8 held: dropped %d, kept %d", n, len(g.Findings))
	}
	if note := g.keepNote(ca, 8); note != "" {
		t.Errorf("nothing was cut, so the note is empty: %q", note)
	}
	// Past the guard the worst are kept and the note says how many were cut.
	var lots []CodeAuditFinding
	for range findingsGuard + 3 {
		lots = append(lots, CodeAuditFinding{Severity: "low"})
	}
	h := &auditAnswer{Findings: lots}
	if n := h.keep(ca); n != 3 || len(h.Findings) != findingsGuard {
		t.Errorf("past the guard: dropped %d, kept %d", n, len(h.Findings))
	}
	if note := h.keepNote(ca, findingsGuard+3); !strings.Contains(note, "held 103") || !strings.Contains(note, "3 were cut") || !strings.Contains(note, "kept in the audit") {
		t.Errorf("guard note = %q", note)
	}
	if strings.Contains(auditSchema, "maxItems") {
		t.Errorf("the model is told a number: %s", auditSchema)
	}
	if AuditShown(&CodeAudit{}) != 15 || AuditShown(&CodeAudit{MaxFindings: 50}) != 50 {
		t.Error("shown at a time: 0 is 15, a stored 50 is kept")
	}
}

// An audit of what landed on a branch over a period (WP-548): the runner
// is sent the period's start as a time and the paths; the criteria are in
// the prompt and kept on the audit; what the model reports beyond them is
// dropped, and the range the runner read is recorded. Run again, a
// relative period is measured from then.
func TestAuditOfRecentChangesWithCriteria(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Since: "7d", Focus: []string{"security", "data"},
		Paths: []string{"internal"}, Exclude: []string{"*_test.go"}, MinSeverity: "high", MaxFindings: 2, Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Target != "recent" || a.Since != "7d" || a.SinceAt == nil || strings.Join(a.Focus, ",") != "security,data" ||
		strings.Join(a.Paths, ",") != "internal" || a.MinSeverity != "high" || a.MaxFindings != 2 || !strings.Contains(a.Title, "the last 7 days") ||
		!strings.Contains(a.Title, "(internal)") {
		t.Fatalf("audit = %+v", a)
	}
	ae.work()
	claim := ae.take()
	in := agentJobOf(t, claim)
	if in.DiffFrom != "" || in.DiffSince != a.SinceAt.Unix() || strings.Join(in.DiffPaths, ",") != "internal" || strings.Join(in.DiffExclude, ",") != "*_test.go" ||
		strings.Contains(in.JSONSchema, "maxItems") {
		t.Fatalf("job = %+v", in)
	}
	for _, want := range []string{"everything that landed on it the last 7 days", "Look hardest at:", "Security: injection", "Data integrity",
		"Audit only these parts of the repository: `internal`", "Leave these out: `*_test.go`", "Report every problem you find, worst first", "Report only findings of high severity or worse"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
	res := auditResult("Read a week.",
		map[string]any{"title": "Low thing", "path": "internal/a.go", "line": 1, "severity": "low", "problem": "x"},
		map[string]any{"title": "Leaks tokens", "path": "internal/b.go", "line": 2, "severity": "critical", "problem": "x"},
		map[string]any{"title": "Wrong totals", "path": "internal/c.go", "line": 3, "severity": "high", "problem": "x"},
		map[string]any{"title": "Wrong sums", "path": "internal/d.go", "line": 4, "severity": "high", "problem": "x"},
	)
	res["base"], res["diff_from"], res["change_commits"] = "bbbbbbbbbbbb", "aaaaaaaaaaaa", float64(12)
	ae.finish(claim.ID, "done", res, "")
	got := ae.audit(a.ID)
	// max_findings (2) is how many are shown at a time, not a cut: the three
	// at or over the floor are kept, and the summary says what the floor left out.
	if got.Status != AuditDone || got.Counts.Total != 3 || got.FromSHA != "aaaaaaaaaaaa" || got.ToSHA != "bbbbbbbbbbbb" || got.CommitCount == nil || *got.CommitCount != 12 ||
		!strings.Contains(got.Summary, "(1 of the answer's 4 findings were below the audit's lowest severity, high, and are left out.)") ||
		strings.Contains(got.Summary, "Asked for") {
		t.Fatalf("done = %s %+v from %s to %s commits %v %q (%s)", got.Status, got.Counts, got.FromSHA, got.ToSHA, got.CommitCount, got.Summary, got.Error)
	}

	again, err := ae.s.RerunAudit(ae.ctx, ae.admin, a.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if again.Target != "recent" || again.Since != "7d" || strings.Join(again.Paths, ",") != "internal" || again.MinSeverity != "high" || again.MaxFindings != 2 ||
		strings.Join(again.Focus, ",") != "security,data" || again.SinceAt.Before(*a.SinceAt) {
		t.Fatalf("run again = %+v", again)
	}
}

// A runner that finds nothing to read ends the audit done, with its reason
// and no model, and nothing posted.
func TestAuditWithNothingToRead(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Target: "recent", Since: "1d", Paths: []string{"docs"}})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	ae.finish(claim.ID, "done", map[string]any{"nothing": "Nothing to audit: the 3 commits change nothing in the paths asked for.", "base": "b", "diff_from": "a", "change_commits": float64(0)}, "")
	got := ae.audit(a.ID)
	if got.Status != AuditDone || got.Model != "" || !strings.Contains(got.Summary, "Nothing to audit") || got.Counts.Total != 0 ||
		len(got.Attempts) != 1 || got.Attempts[0].Outcome != "nothing" || got.CommitCount == nil || *got.CommitCount != 0 {
		t.Fatalf("audit = %s %q %q %+v", got.Status, got.Model, got.Summary, got.Attempts)
	}
}

func TestRequestAuditCriteriaRefusals(t *testing.T) {
	ae := newAgentEnv(t)
	for _, tc := range []struct {
		in   AuditInput
		want string
	}{
		{AuditInput{Project: ae.p.Key, Target: "commit", Ref: "abc1234", Since: "7d"}, "for recent changes"},
		{AuditInput{Project: ae.p.Key, Since: "next week"}, "isn't a period"},
		{AuditInput{Project: ae.p.Key, Target: "recent", Since: "2y"}, "isn't a period"},
		{AuditInput{Project: ae.p.Key, Focus: []string{"style"}}, "isn't a focus area"},
		{AuditInput{Project: ae.p.Key, Paths: []string{"../x"}}, "isn't a path inside"},
	} {
		if _, err := ae.s.RequestAudit(ae.ctx, ae.admin, tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want %q", tc.in, err, tc.want)
		}
	}
}

// The audit step carries the criteria; the weekly gallery card audits the
// week that landed on the default branch.
func TestAuditStepCriteria(t *testing.T) {
	ae := newAgentEnv(t)
	card := ae.addAuto(ae.p, "Week", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "audit", ID: "audit", Extra: map[string]any{
		"target": "recent", "since": "3d", "focus": "security, tests", "paths": []any{"internal"}, "exclude": []any{"docs"}, "min_severity": "high", "max_findings": 3}}}})
	if _, err := ae.s.RunAutomationNow(ae.ctx, ae.admin, card.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	ae.work()
	in := agentJobOf(t, ae.take())
	if in.DiffSince == 0 || strings.Join(in.DiffPaths, ",") != "internal" || strings.Join(in.DiffExclude, ",") != "docs" || !strings.Contains(in.Prompt, "the last 3 days") ||
		!strings.Contains(in.Prompt, "Tests: tests that") || !strings.Contains(in.Prompt, "Report every problem you find, worst first") || !strings.Contains(in.Prompt, "high severity or worse") {
		t.Fatalf("job = %+v", in)
	}

	ae.addAuto(ae.p, "Weekly", templateRule(t, "Weekly Grok audit of what landed"))
	ae.setClock(time.Date(2026, 11, 9, 9, 1, 0, 0, time.UTC)) // a Monday
	ae.s.RunTimedAutomations(ae.ctx)
	ae.work()
	in = agentJobOf(t, ae.take())
	if in.DiffSince == 0 || in.Base != "main" || !strings.Contains(in.Prompt, "the last 7 days") {
		t.Fatalf("weekly job = %+v", in)
	}

	for _, bad := range []map[string]any{{"target": "recent", "since": "soon"}, {"focus": "vibes"}, {"max_findings": 101}, {"paths": []any{"../x"}}} {
		r := Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "audit", ID: "audit", Extra: bad}}}
		name := "Bad"
		if _, err := ae.s.CreateAutomation(ae.ctx, ae.admin, ae.p.Key, AutomationInput{Name: &name, Rule: &r}); err == nil {
			t.Errorf("%v was saved", bad)
		}
	}
}
