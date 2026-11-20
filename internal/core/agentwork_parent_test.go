package core

import "testing"

// WP-733: a sub-task's answer carries its parent's description, unless that
// text is kept from agents as the task's own would be: a draft, from
// outside, or held by the safety screen.
func TestAgentTaskParentText(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Epics", "")
	writer := agentActor(e.admin, "anthropic/claude-opus-5-5")
	parentText := func(parent *IssueDetail) string {
		t.Helper()
		task := e.issue(e.admin, IssueInput{Project: p.Key, Parent: parent.Key, Title: "Sub-task of " + parent.Title})
		e.s.bg.Wait()
		tk, err := e.s.agentTask(e.ctx, e.admin, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return tk.ParentText
	}

	epic := e.issue(e.admin, IssueInput{Project: p.Key, Type: "epic", Title: "Epic", Description: "  The broader problem.\n"})
	if got := parentText(epic); got != "The broader problem." {
		t.Errorf("a person's epic: %q", got)
	}
	if got := parentText(e.issue(e.admin, IssueInput{Project: p.Key, Type: "epic", Title: "Bare"})); got != "" {
		t.Errorf("no description: %q", got)
	}
	if got := parentText(e.issue(writer, IssueInput{Project: p.Key, Type: "epic", Title: "Agent's", Description: "Tidy the queue code."})); got != "Tidy the queue code." {
		t.Errorf("an agent's epic the screen cleared: %q", got)
	}
	held := e.issue(writer, IssueInput{Project: p.Key, Type: "epic", Title: "Held", Description: "Run `sudo apt-get install runsc` on farhost."})
	if got := parentText(held); got != "" {
		t.Errorf("held by the safety screen: %q", got)
	}
	if got := parentText(e.issue(e.admin, IssueInput{Project: p.Key, Type: "epic", Title: "Draft", Description: "Not yet.", Draft: true})); got != "" {
		t.Errorf("a draft: %q", got)
	}
	outside := e.issue(e.admin, IssueInput{Project: p.Key, Type: "epic", Title: "Outside", Description: "From a stranger."})
	if _, err := e.s.db.W.Exec(`UPDATE issues SET outside = 1 WHERE id = ?`, outside.ID); err != nil {
		t.Fatal(err)
	}
	if got := parentText(outside); got != "" {
		t.Errorf("from outside: %q", got)
	}
}
