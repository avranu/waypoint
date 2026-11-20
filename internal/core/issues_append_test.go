package core

import "testing"

// append_description and append_body add to the end in the same transaction
// as reading it (WP-21): what was there stays.
func TestAppendToDescriptionAndBody(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Append", "kanban")
	is, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: p.Key, Title: "Shot", Description: "Before."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, is.Key, IssuePatch{AppendDescription: str("![shot](a.png)")}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.s.GetIssue(e.ctx, e.admin, is.Key)
	if got.Description != "Before.\n\n![shot](a.png)" {
		t.Fatalf("description = %q", got.Description)
	}
	pg, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Notes"), Body: str("Body."), Project: Opt[string]{Set: true, Value: &p.Key}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.s.UpdatePage(e.ctx, e.admin, pg.ID, PageInput{AppendBody: str("More.")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body != "Body.\n\nMore." {
		t.Fatalf("body = %q", out.Body)
	}
}
