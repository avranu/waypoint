package core

import (
	"strings"
	"testing"
)

// A parent can't be marked done while a sub-task is open (WP-ADR123; WP-680 was closed
// with 8 of its 9 sub-tasks still To Do); Won't Do stays a choice.
func TestParentNotDoneWithOpenChildren(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Parents", "")
	str := func(s string) *string { return &s }
	epic := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Epic"})
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Part A", Parent: epic.Key})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Part B", Parent: epic.Key})

	_, err := e.s.UpdateIssue(e.ctx, e.admin, epic.Key, IssuePatch{Status: str("done")})
	if err == nil || !strings.Contains(err.Error(), "2 of its sub-tasks are still open") ||
		!strings.Contains(err.Error(), a.Key) || !strings.Contains(err.Error(), b.Key) {
		t.Fatalf("closing a parent with open sub-tasks: %v", err)
	}
	// An agent shipping it is refused the same way.
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	if _, err := e.s.UpdateIssue(e.ctx, agent, epic.Key, IssuePatch{Status: str("done")}); err == nil {
		t.Fatal("an agent closed a parent with open sub-tasks")
	}
	// Bulk edits too.
	if _, err := e.s.BulkUpdate(e.ctx, e.admin, []string{epic.Key}, IssuePatch{Status: str("done")}, false); err == nil ||
		!strings.Contains(err.Error(), "sub-tasks are still open") {
		t.Fatalf("a bulk edit on a parent with open sub-tasks: %v", err)
	}
	if got, _ := e.s.GetIssue(e.ctx, e.admin, epic.Key); got != nil && got.Status.Category == CatDone {
		t.Fatal("a bulk edit closed a parent with open sub-tasks")
	}

	// Closing the whole tree at once closes the sub-tasks first.
	tree := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Tree"})
	leaf := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Leaf", Parent: tree.Key})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Grandchild", Parent: leaf.Key})
	if _, err := e.s.BulkUpdate(e.ctx, e.admin, []string{tree.Key}, IssuePatch{Status: str("done")}, true); err != nil {
		t.Fatalf("closing a whole tree: %v", err)
	}

	// One left open: still refused, and the message names it.
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, a.Key, IssuePatch{Status: str("done")}); err != nil {
		t.Fatal(err)
	}
	_, err = e.s.UpdateIssue(e.ctx, e.admin, epic.Key, IssuePatch{Status: str("done")})
	if err == nil || !strings.Contains(err.Error(), "1 of its sub-tasks is still open ("+b.Key+")") {
		t.Fatalf("one open sub-task: %v", err)
	}

	// A sub-task dropped (Won't Do) counts as closed.
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, b.Key, IssuePatch{Status: str("canceled")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, epic.Key, IssuePatch{Status: str("done")}); err != nil {
		t.Fatalf("all sub-tasks closed, the parent should close: %v", err)
	}

	// Dropping a parent with open sub-tasks stays allowed.
	epic2 := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Dropped epic"})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Open part", Parent: epic2.Key})
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, epic2.Key, IssuePatch{Status: str("canceled")}); err != nil {
		t.Fatalf("Won't Do on a parent with open sub-tasks: %v", err)
	}
}
