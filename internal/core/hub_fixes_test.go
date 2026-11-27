package core

import (
	"encoding/json"
	"testing"
)

// batch claims related work only where agents may take new work.
func TestBatchSkipsPausedProjects(t *testing.T) {
	e := newEnv(t)
	alpha := e.project(e.admin, "Alpha", "")
	beta := e.project(e.admin, "Beta", "")
	a := e.issue(e.admin, IssueInput{Project: alpha.Key, Title: "Start", Agent: "any", Status: "todo"})
	b := e.issue(e.admin, IssueInput{Project: beta.Key, Title: "Related", Agent: "any", Status: "todo"})
	if err := e.s.AddRelation(e.ctx, e.admin, a.Key, "relates", b.Key); err != nil {
		t.Fatal(err)
	}
	st := beta.Settings
	st.AgentsPaused = true
	if _, err := e.s.UpdateProject(e.ctx, e.admin, beta.Key, ProjectInput{Settings: &st}); err != nil {
		t.Fatal(err)
	}
	task, err := e.s.AgentBatch(e.ctx, agentActor(e.admin, "anthropic/claude-opus-5-5"), AgentQuery{Project: alpha.Key})
	if err != nil || task == nil || task.Issue.Key != a.Key {
		t.Fatalf("batch = %+v, %v", task, err)
	}
	if len(task.Related) != 0 {
		t.Errorf("batch claimed %s in a paused project", task.Related[0].Key)
	}
	if got := e.get(e.admin, b.Key); got.ClaimedBy != "" {
		t.Errorf("%s claimed by %s", b.Key, got.ClaimedBy)
	}
}

// GitHub's pull request list says merged_at, never merged: true.
func TestGitHubListMergedState(t *testing.T) {
	var a apiPR
	if err := json.Unmarshal([]byte(`{"number": 7, "state": "closed", "merged_at": "2026-10-12T10:00:00Z", "closed_at": "2026-10-12T10:00:00Z"}`), &a); err != nil {
		t.Fatal(err)
	}
	if got := a.pr().State; got != "merged" {
		t.Errorf("merged PR from the list is %q", got)
	}
	a = apiPR{}
	if err := json.Unmarshal([]byte(`{"number": 8, "state": "closed", "closed_at": "2026-10-12T10:00:00Z"}`), &a); err != nil {
		t.Fatal(err)
	}
	if got := a.pr().State; got != "closed" {
		t.Errorf("closed PR is %q", got)
	}
}

// Knowledge use never reveals a private page's text or a page in a
// project the reader isn't in.
func TestKnowledgeUseVisibility(t *testing.T) {
	u := newUsageEnv(t)
	e := u.env
	term, avoid := "Zebra", []string{"horse"}
	zebra, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: &term, Avoid: &avoid})
	if err != nil {
		t.Fatal(err)
	}
	alice, bob := e.user("alice", false), e.user("bob", false)

	if _, err := e.s.CreatePage(e.ctx, alice, PageInput{Title: str("Notes"), Body: str("a zebra, not a horse")}); err != nil {
		t.Fatal(err)
	}
	if got := u.usage(bob, KGlossary, zebra.ID); got.Counts.Mentions+got.Counts.Avoided != 0 || got.LastUsedAt != nil {
		t.Errorf("bob sees uses from alice's private page: %+v", got.Counts)
	}

	other := e.project(bob, "Other", "")
	code := u.page.Code
	if code == "" {
		t.Fatal("the pilot page has no code")
	}
	e.issue(bob, IssueInput{Project: other.Key, Title: "See " + u.p.Key + "-" + code})
	ov, err := e.s.KnowledgeOverview(e.ctx, bob, KnowledgeQuery{Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	if ov.Totals.Mentions != 0 || ov.Totals.ItemsUsed != 0 {
		t.Errorf("bob's totals reveal a page he can't read: %+v", ov.Totals)
	}
	if got := u.usage(u.person, KPage, u.page.ID); got.Counts.Mentions != 1 {
		t.Errorf("robin sees %d mentions of the page, want 1", got.Counts.Mentions)
	}
}
