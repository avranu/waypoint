package core

import (
	"strings"
	"testing"
)

// A title over the limit is split rather than refused or cut, on create and
// on edit (WP-175): the title becomes a short stand-in and the description
// holds the whole text, as the web app's own split does (WP-68).
func TestTitleOverTheLimitIsSplit(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Titles", "kanban")
	long := strings.Repeat("x", maxTitleLen+1)
	made, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: p.Key, Title: long})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(made.Title) > maxTitleLen || made.Description != long {
		t.Fatalf("create: title %d bytes, description %d bytes", len(made.Title), len(made.Description))
	}
	is, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: p.Key, Title: strings.Repeat("y", maxTitleLen)})
	if err != nil {
		t.Fatalf("a title at the limit: %v", err)
	}
	got, err := e.s.UpdateIssue(e.ctx, e.admin, is.Key, IssuePatch{Title: str(long)})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(got.Title) > maxTitleLen || got.Description != long {
		t.Fatalf("update: title %d bytes, description %d bytes", len(got.Title), len(got.Description))
	}
}
