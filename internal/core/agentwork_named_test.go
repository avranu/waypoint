package core

import (
	"fmt"
	"strings"
	"testing"
)

// WP-804: a relation carries the other task's status name and, once it's
// finished, when, whichever side of the link the task is on.
func TestRelationStatusAndResolved(t *testing.T) {
	e := newEnv(t)
	ann := e.user("ann", true)
	p := e.project(ann, "Work", "")
	task := e.issue(ann, IssueInput{Project: p.Key, Title: "The task"})
	open := e.issue(ann, IssueInput{Project: p.Key, Title: "Open"})
	finished := e.issue(ann, IssueInput{Project: p.Key, Title: "Finished"})
	if err := e.s.AddRelation(e.ctx, ann, task.Key, "relates", open.Key); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddRelation(e.ctx, ann, finished.Key, "blocks", task.Key); err != nil {
		t.Fatal(err)
	}
	done := "done"
	if _, err := e.s.UpdateIssue(e.ctx, ann, finished.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	d, err := e.s.GetIssue(e.ctx, ann, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]Relation{}
	for _, r := range d.Relations {
		seen[r.Issue.Key] = r
	}
	if r := seen[open.Key]; r.Status != "To Do" || r.ResolvedAt != nil {
		t.Errorf("open: %+v", r)
	}
	if r := seen[finished.Key]; r.Status != "Done" || r.ResolvedAt == nil || r.Kind != "blocked_by" {
		t.Errorf("finished: %+v", r)
	}
}

// WP-545: a task's answer lists the other tasks its text names, once each,
// first named first, leaving out the ones it lists already and the ones the
// agent can't see.
func TestAgentTaskNamed(t *testing.T) {
	e := newEnv(t)
	ann := e.user("ann", true)
	sam := e.user("sam", false)
	p := e.project(ann, "Work", "")
	secret := e.issue(ann, IssueInput{Project: e.project(ann, "Hidden", "").Key, Title: "Hidden"})
	parent := e.issue(ann, IssueInput{Project: p.Key, Type: "epic", Title: "Epic"})
	task := e.issue(ann, IssueInput{Parent: parent.Key, Title: "The task"})
	rel := e.issue(ann, IssueInput{Project: p.Key, Title: "Related one"})
	child := e.issue(ann, IssueInput{Parent: task.Key, Title: "Child"})
	named := e.issue(ann, IssueInput{Project: p.Key, Title: "Named one"})
	other := e.issue(ann, IssueInput{Project: p.Key, Title: "Named in a comment"})
	if err := e.s.AddRelation(e.ctx, ann, task.Key, "relates", rel.Key); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ShareIssue(e.ctx, ann, parent.Key, "sam", "editor"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ShareIssue(e.ctx, ann, named.Key, "sam", "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ShareIssue(e.ctx, ann, other.Key, "sam", "viewer"); err != nil {
		t.Fatal(err)
	}
	desc := fmt.Sprintf("Like %s, see %s and %s (and %s again). Not %s, %s, %s, ADR-0007 or UTF-8.",
		named.Key, secret.Key, named.Key, named.Key, task.Key, parent.Key, child.Key)
	// An agent's text: what a person names becomes a relation (WP-781), and
	// the list leaves out relations.
	writer := *ann
	writer.Agent = "anthropic/claude-opus-5-5"
	if _, err := e.s.UpdateIssue(e.ctx, &writer, task.Key, IssuePatch{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddComment(e.ctx, ann, task.Key, "Also `wp show "+other.Key+"` and "+rel.Key+"."); err != nil {
		t.Fatal(err)
	}

	got := func(a *Actor) string {
		tk, err := e.s.agentTask(e.ctx, a, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, i := range tk.Named {
			keys = append(keys, i.Key+" "+i.Title+" ["+i.Status.Name+"]")
		}
		return strings.Join(keys, "; ")
	}
	if g, want := got(ann), named.Key+" Named one [To Do]; "+secret.Key+" Hidden [To Do]; "+other.Key+" Named in a comment [To Do]"; g != want {
		t.Errorf("ann: %s\nwant %s", g, want)
	}
	if g, want := got(sam), named.Key+" Named one [To Do]; "+other.Key+" Named in a comment [To Do]"; g != want {
		t.Errorf("sam (can't see %s): %s\nwant %s", secret.Key, g, want)
	}

	// The cap.
	var many []string
	for n := range namedMax + 3 {
		many = append(many, e.issue(ann, IssueInput{Project: p.Key, Title: fmt.Sprint("m", n)}).Key)
	}
	desc = strings.Join(many, " ")
	if _, err := e.s.UpdateIssue(e.ctx, &writer, task.Key, IssuePatch{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	if tk, _ := e.s.agentTask(e.ctx, ann, task.ID); len(tk.Named) != namedMax || tk.Named[0].Key != many[0] {
		t.Errorf("cap: %d named", len(tk.Named))
	}
}

// wp next EPIC hands out the epic's first open sub-task, as wp next would,
// and opens no run on the epic; one it already holds there comes back. An
// epic none of whose sub-tasks can be taken now is taken as named (WP-730).
func TestAgentTakeEpicHandsOutItsSubTask(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Epics", "")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	epic := e.issue(e.admin, IssueInput{Project: p.Key, Type: "epic", Title: "Epic", Agent: "claude"})
	low := e.issue(e.admin, IssueInput{Project: p.Key, Parent: epic.Key, Title: "Low", Agent: "claude", Priority: "low"})
	urgent := e.issue(e.admin, IssueInput{Project: p.Key, Parent: epic.Key, Title: "Urgent", Agent: "claude", Priority: "urgent"})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Elsewhere", Agent: "claude", Priority: "urgent"})
	runsOn := func(id int64) int {
		var n int
		_ = e.s.db.R.QueryRow(`SELECT count(*) FROM agent_runs WHERE issue_id = ?`, id).Scan(&n)
		return n
	}

	got, err := e.s.AgentTake(e.ctx, opus, epic.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Issue.Key != urgent.Key || !strings.Contains(got.Why, "A sub-task of "+epic.Key) {
		t.Fatalf("took %s (%q), want %s", got.Issue.Key, got.Why, urgent.Key)
	}
	if d := e.get(e.admin, epic.Key); d.ClaimedBy != "" || runsOn(epic.ID) != 0 {
		t.Errorf("the epic was taken: claimed by %q, %d runs", d.ClaimedBy, runsOn(epic.ID))
	}
	// Asked again, the sub-task it holds comes back.
	if again, err := e.s.AgentTake(e.ctx, opus, epic.Key, ""); err != nil || again.Issue.Key != urgent.Key || !again.Resumed {
		t.Fatalf("again = %+v, %v", again, err)
	}

	// Nothing under it can be taken: the epic is, as named.
	blocker := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Blocker", Agent: "none"})
	if err := e.s.AddRelation(e.ctx, e.admin, blocker.Key, "blocks", low.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentDone(e.ctx, opus, urgent.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.AgentTake(e.ctx, opus, epic.Key, ""); err != nil || got.Issue.Key != epic.Key {
		t.Fatalf("blocked sub-task: took %+v, %v", got, err)
	}
}
