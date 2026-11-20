package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// actions lists a page's actions, newest first.
func actions(p *AuditPage) []string {
	out := make([]string, len(p.Entries))
	for n, r := range p.Entries {
		out[n] = r.Action
	}
	return out
}

func (e *env) audit(a *Actor, q AuditQuery) *AuditPage {
	e.t.Helper()
	p, err := e.s.ListAudit(e.ctx, a, q)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestAuditRecordsDecisions(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "Audit", "")
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor"); err != nil {
		t.Fatal(err)
	}
	// Setting the same role again isn't a decision.
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor"); err != nil {
		t.Fatal(err)
	}

	// Delegation by a person, on create and on edit; an agent's own
	// bookkeeping isn't a person delegating.
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Parser", Agent: "claude"})
	if _, err := e.s.UpdateIssue(e.ctx, sam, i.Key, IssuePatch{Agent: ptr("opus")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Agent: ptr("")}); err != nil {
		t.Fatal(err)
	}
	tok, _, err := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	opus, _ := e.s.TokenActor(e.ctx, tok)
	own := e.issue(opus, IssueInput{Project: p.Key, Title: "Note to self"})

	// Reviewing an agent's text says whose it was.
	if _, err := e.s.Review(e.ctx, e.admin, ReviewIssue, own.Key, ""); err != nil {
		t.Fatal(err)
	}
	// Reviewing again changes nothing, so records nothing.
	if _, err := e.s.Review(e.ctx, e.admin, ReviewIssue, own.Key, ""); err != nil {
		t.Fatal(err)
	}

	// Settings: an automation made, then turned off.
	a, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: ptr("Close stale"),
		Rule: &Rule{Trigger: Trigger{Event: "status_changed", To: "done"}, Actions: []Action{{Type: "comment", Value: "Thanks"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateAutomation(e.ctx, e.admin, a.ID, AutomationInput{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteIssue(e.ctx, e.admin, i.Key, false); err != nil {
		t.Fatal(err)
	}

	got := e.audit(e.admin, AuditQuery{Project: p.Key})
	want := []string{"issue.delete", "automation.update", "automation.create", "review", "undelegate", "delegate", "delegate", "member.set"}
	if !slices.Equal(actions(got), want) {
		t.Fatalf("actions = %v, want %v", actions(got), want)
	}
	byAction := map[string]*AuditRecord{}
	for _, r := range got.Entries {
		if byAction[r.Action] == nil {
			byAction[r.Action] = r
		}
	}
	if r := byAction["review"]; r.AboutAgent != "anthropic/claude-opus-5-5" || r.Target != own.Key || r.URL != "/issue/"+own.Key || r.Category != AuditApproval {
		t.Errorf("review = %+v", r)
	}
	if r := byAction["undelegate"]; r.AboutAgent != "opus" || r.Detail != "took it back from opus" || r.User == nil || r.User.Username != "robin" {
		t.Errorf("undelegate = %+v", r)
	}
	if r := got.Entries[5]; r.Detail != "for opus (was claude)" || r.User.Username != "sam" || r.AboutAgent != "opus" {
		t.Errorf("redelegate = %+v", r)
	}
	if r := byAction["automation.update"]; r.Detail != "turned it off" || r.Target != "Close stale" {
		t.Errorf("automation.update = %+v", r)
	}
	if r := byAction["issue.delete"]; r.TargetID == nil || !strings.Contains(r.Detail, "Parser") || r.Project == nil || r.Project.Key != p.Key {
		t.Errorf("issue.delete = %+v", r)
	}

	// The token is the admin's own, outside any project.
	all := e.audit(e.admin, AuditQuery{Category: AuditAccess})
	if !slices.Equal(actions(all), []string{"token.create", "member.set"}) || all.Entries[0].AboutAgent != "anthropic/claude-opus-5-5" {
		t.Errorf("access = %v %+v", actions(all), all.Entries[0])
	}

	// An agent filter reaches what's about matching agents: claude covers
	// the opus identity and the claude delegation; haiku covers nothing.
	claude := e.audit(e.admin, AuditQuery{Agent: "claude"})
	if !slices.Equal(actions(claude), []string{"review", "token.create", "undelegate", "delegate", "delegate"}) {
		t.Errorf("claude = %v", actions(claude))
	}
	if h := e.audit(e.admin, AuditQuery{Agent: "haiku"}); len(h.Entries) != 0 {
		t.Errorf("haiku = %v", actions(h))
	}
	if u := e.audit(e.admin, AuditQuery{User: "sam"}); !slices.Equal(actions(u), []string{"delegate"}) {
		t.Errorf("sam's = %v", actions(u))
	}
	if _, err := e.s.ListAudit(e.ctx, e.admin, AuditQuery{Category: "nonsense"}); !IsInvalid(err) {
		t.Errorf("bad category: %v", err)
	}
}

func TestAuditVisibilityAndPaging(t *testing.T) {
	e := newEnv(t)
	sam, kim := e.user("sam", false), e.user("kim", false)
	p := e.project(e.admin, "Shared", "")
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor"); err != nil {
		t.Fatal(err)
	}
	q := e.project(kim, "Private", "")
	for n := range 5 {
		e.issue(kim, IssueInput{Project: q.Key, Title: "Kim's " + string(rune('A'+n)), Agent: "claude"})
	}
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Shared work", Agent: "any"})
	if _, _, err := e.s.CreateToken(e.ctx, sam, TokenSpec{Name: "laptop"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.CreateToken(e.ctx, kim, TokenSpec{Name: "kim's"}); err != nil {
		t.Fatal(err)
	}

	// Sam sees the shared project and their own token, nothing of Kim's.
	got := e.audit(sam, AuditQuery{})
	if !slices.Equal(actions(got), []string{"token.create", "delegate", "member.set"}) {
		t.Errorf("sam sees %v", actions(got))
	}
	for _, r := range got.Entries {
		if r.Target == "kim's" || (r.Project != nil && r.Project.Key == q.Key) {
			t.Errorf("sam sees %+v", r)
		}
	}
	if _, err := e.s.ListAudit(e.ctx, sam, AuditQuery{Project: q.Key}); err == nil {
		t.Error("sam read another project's log")
	}
	// The admin sees everything.
	if n := len(e.audit(e.admin, AuditQuery{Limit: 200}).Entries); n != 9 {
		t.Errorf("admin sees %d entries", n)
	}

	// Pages continue where they left off, with no repeats.
	seen := map[int64]bool{}
	page := e.audit(kim, AuditQuery{Limit: 2})
	pages := 1
	for {
		for _, r := range page.Entries {
			if seen[r.ID] {
				t.Fatalf("entry %d twice", r.ID)
			}
			seen[r.ID] = true
		}
		if page.NextBefore == 0 {
			break
		}
		page = e.audit(kim, AuditQuery{Limit: 2, Before: page.NextBefore})
		pages++
	}
	if len(seen) != 6 || pages != 3 {
		t.Errorf("kim paged %d entries in %d pages", len(seen), pages)
	}
	// The same with an agent filter, which reads in batches.
	seen = map[int64]bool{}
	for before := int64(0); ; {
		page := e.audit(e.admin, AuditQuery{Agent: "claude", Limit: 2, Before: before})
		for _, r := range page.Entries {
			seen[r.ID] = true
		}
		if before = page.NextBefore; before == 0 {
			break
		}
	}
	if len(seen) != 6 { // kim's five, and "any" covers claude
		t.Errorf("claude entries = %d", len(seen))
	}

	csv, err := e.s.AuditCSV(e.ctx, sam, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(csv)), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "time (UTC),person,") || !strings.Contains(lines[2], "https://wp.test/issue/") {
		t.Errorf("csv = %q", csv)
	}
	if csvSafe("=SUM(A1)") != "'=SUM(A1)" || csvSafe("WP-1") != "WP-1" {
		t.Error("csvSafe")
	}
}

func TestAuditKnowledgeAndAccess(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Know", "")
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")

	// An agent's proposed design note, approved; its later edit, rejected.
	prop, err := e.s.CreateDesign(e.ctx, agent, DesignInput{Title: str("Dark mode"), Kind: str("feature"), Body: str("Follow the system."), Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ApproveDesign(e.ctx, e.admin, prop.ID, ""); err != nil {
		t.Fatal(err)
	}
	prop, _ = e.s.UpdateDesign(e.ctx, agent, prop.ID, DesignInput{Title: str("Themes")})
	if _, err := e.s.ReviewDesign(e.ctx, e.admin, prop.Proposals[0].ID, "reject", "keep the name"); err != nil {
		t.Fatal(err)
	}
	// A glossary entry locked against agents, then deleted by a person.
	g, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: str("Board"), Definition: str("Columns."), Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateGlossary(e.ctx, e.admin, g.ID, GlossaryInput{Locked: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteGlossary(e.ctx, e.admin, g.ID); err != nil {
		t.Fatal(err)
	}
	// The CLI signed in for an agent.
	d, err := e.s.StartDeviceLogin(e.ctx, "box", "anthropic/claude-code", "198.51.100.1", "wp/test")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.ApproveDeviceLogin(e.ctx, e.admin, d.UserCode); err != nil {
		t.Fatal(err)
	}
	// A token revoked; an invite made and revoked.
	_, tok, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "ci", ReadOnly: true})
	if err := e.s.RevokeAPIToken(e.ctx, e.admin, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateInvite(e.ctx, e.admin, "ann@example.com", false, 0); err != nil {
		t.Fatal(err)
	}
	inv, _ := e.s.ListInvites(e.ctx, e.admin)
	if err := e.s.RevokeInvite(e.ctx, e.admin, inv[0].ID); err != nil {
		t.Fatal(err)
	}

	got := e.audit(e.admin, AuditQuery{})
	want := []string{"invite.revoke", "invite.create", "token.revoke", "token.create", "device.approve",
		"glossary.delete", "glossary.lock", "design.reject", "design.approve"}
	if !slices.Equal(actions(got), want) {
		t.Fatalf("actions = %v, want %v", actions(got), want)
	}
	r := got.Entries
	if r[4].AboutAgent != "anthropic/claude-code" || !strings.Contains(r[4].Detail, "198.51.100.1") {
		t.Errorf("device = %+v", r[4])
	}
	if r[2].Detail != "revoked a read-only API token" || r[1].Detail != "invited ann@example.com" {
		t.Errorf("token/invite = %q, %q", r[2].Detail, r[1].Detail)
	}
	if r[7].Detail != "the edit by anthropic/claude-opus-5-5: keep the name" || r[7].AboutAgent != "anthropic/claude-opus-5-5" {
		t.Errorf("reject = %+v", r[7])
	}
	if r[8].Detail != "approved a proposed feature by anthropic/claude-opus-5-5" || r[8].URL != fmt.Sprintf("/design/%d", prop.ID) {
		t.Errorf("approve = %+v", r[8])
	}
}

func TestAuditSkipsAutomations(t *testing.T) {
	e := newEnv(t)
	if auditable(&Actor{User: e.admin.User, Automation: &Automation{ID: 1}}, AuditEntry{}) {
		t.Error("an automation's change is recorded as a person's")
	}
	if auditable(SystemActor(), AuditEntry{}) || !auditable(SystemActor(), AuditEntry{Actor: "robin"}) {
		t.Error("forge reports")
	}
	e.s.audit(e.ctx, SystemActor(), AuditEntry{Action: "pr.merged", Category: AuditApproval, Actor: "robin", Target: "o/r#1"})
	got := e.audit(e.admin, AuditQuery{})
	if len(got.Entries) != 1 || got.Entries[0].User != nil || got.Entries[0].Actor != "robin" {
		t.Errorf("forge entry = %+v", got.Entries)
	}
}
