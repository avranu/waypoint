package core

import (
	"strings"
	"testing"
)

// An automation's open_pr step opens a pull request only for a run whose
// actor may (pulls.write), as the ship path requires (WP-1370): a token's run
// keeps the token's permissions, and one without pulls.write opens nothing.
func TestOpenPRStepNeedsPullsWrite(t *testing.T) {
	ae := newAgentEnv(t)
	rule := Rule{Trigger: Trigger{Event: "manual"}, AllowAgents: true, Inputs: []RuleInput{{Name: "branch", Type: "text"}}, Actions: []Action{
		{Type: "open_pr", ID: "pr", Extra: map[string]any{"head": "{{.Inputs.branch}}", "title": "From a token", "body": "x"}},
	}}
	a := ae.addAuto(ae.p, "OpenPerm", rule)
	run := func(perms []Permission, branch string) *AutomationRun {
		t.Helper()
		actor := *ae.admin
		actor.Agent = "anthropic/claude-opus-5-5"
		actor.Permissions = permissionSet(perms)
		r, err := ae.s.RunAutomationNow(ae.ctx, &actor, a.ID, map[string]any{"branch": branch}, "manual")
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		ae.work()
		return ae.runStatus(r)
	}
	r := run([]Permission{PermAutomationsManage, PermAutomationsRead, PermIssuesRead, PermPullsRead}, "review/no-write")
	created, _, _, _ := ae.sg.snapshot()
	if len(created) != 0 {
		t.Fatalf("a run without pulls.write opened %d pull requests: %+v", len(created), created)
	}
	if r.Status != RunFailed || !strings.Contains(r.Error, "pulls.write") {
		t.Errorf("run without pulls.write: %s %q, want a failure that names pulls.write", r.Status, r.Error)
	}
	r = run([]Permission{PermAutomationsManage, PermAutomationsRead, PermIssuesRead, PermPullsRead, PermPullsWrite}, "review/with-write")
	created, _, _, _ = ae.sg.snapshot()
	if r.Status != RunSucceeded || len(created) != 1 || created[0].Head != "review/with-write" {
		t.Errorf("run with pulls.write: %s %q, opened %+v", r.Status, r.Error, created)
	}
	p := ae.manual(ae.addAuto(ae.p, "OpenPerson", Rule{Trigger: Trigger{Event: "manual"}, Inputs: []RuleInput{{Name: "branch", Type: "text"}}, Actions: []Action{
		{Type: "open_pr", ID: "pr", Extra: map[string]any{"head": "{{.Inputs.branch}}", "title": "By a person"}},
	}}), map[string]any{"branch": "review/person"})
	ae.work()
	if got := ae.runStatus(p); got.Status != RunSucceeded {
		t.Errorf("a person's run: %s %q", got.Status, got.Error)
	}
}
