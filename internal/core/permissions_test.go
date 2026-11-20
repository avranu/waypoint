package core

import (
	"strings"
	"testing"
)

// A permission token's change can set off an automation, but the run can do
// no more than the token: its runner step is refused (WP-111).
func TestAutomationKeepsTokenPermissions(t *testing.T) {
	ae := newAgentEnv(t)
	a := ae.addAuto(ae.p, "Echo", Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{
		{Type: "run_command", Extra: map[string]any{"command": "echo {{.Issue.Title}}", "dir": "/work"}},
	}})
	assistant := permissionPresets[0].Permissions
	hermes := &Actor{User: ae.admin.User, Agent: "nous/hermes", Permissions: permissionSet(assistant), TokenID: 1}
	if !hermes.IsAgent() || !(&Actor{User: ae.admin.User, Permissions: PermissionSet{}}).IsAgent() {
		t.Fatal("a permission token should count as an agent")
	}
	ae.issue(hermes, IssueInput{Project: ae.p.Key, Title: "x; touch /tmp/pwned"})
	ae.work()
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatalf("the token's issue queued a runner job: %+v", w.Job)
	}
	runs, err := ae.s.ListAutomationRuns(ae.ctx, ae.admin, a.ID, 5, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v %v", runs, err)
	}
	if r := ae.runStatus(runs[0]); r.Status != RunFailed || !strings.Contains(r.Error, "runners.run") {
		t.Errorf("run = %s %q", r.Status, r.Error)
	}
	// The same change by Robin herself runs the step.
	ae.issue(ae.admin, IssueInput{Project: ae.p.Key, Title: "hello"})
	ae.work()
	if w := ae.rpoll(ae.admin, ae.box); w.Job == nil {
		t.Error("Robin's issue should queue the job")
	}
}

// Delegating an issue to agents needs the queue permission.
func TestDelegationNeedsQueuePermission(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Home", "")
	hermes := &Actor{User: e.admin.User, Agent: "nous/hermes", Permissions: permissionSet(permissionPresets[0].Permissions)}
	if _, err := e.s.CreateIssue(e.ctx, hermes, IssueInput{Project: p.Key, Title: "x", Agent: "any"}); err == nil {
		t.Error("created a delegated issue without agents.manage")
	}
	i := e.issue(hermes, IssueInput{Project: p.Key, Title: "y"})
	any := "any"
	if _, err := e.s.UpdateIssue(e.ctx, hermes, i.Key, IssuePatch{Agent: &any}); err == nil {
		t.Error("delegated an issue without agents.manage")
	}
	if _, err := e.s.UpdateIssue(e.ctx, hermes, i.Key, IssuePatch{Comment: "hi"}); err != nil {
		t.Errorf("assistant comment with an edit: %v", err)
	}
	noComment := &Actor{User: e.admin.User, Agent: "x/y", Permissions: permissionSet([]Permission{PermIssuesEdit})}
	if _, err := e.s.UpdateIssue(e.ctx, noComment, i.Key, IssuePatch{Comment: "hi"}); err == nil {
		t.Error("commented through an edit without issues.comment")
	}
}
