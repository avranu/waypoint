package core

import (
	"strings"
	"testing"
)

// WP-1196: title limits count characters, so 300 CJK characters (900 bytes)
// are a normal title, and 501 are still too long to keep as one.
func TestTitleLimitCountsCharacters(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Chars", "")
	title := strings.Repeat("字", 300)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: title})
	if i.Title != title {
		t.Fatalf("a 300-character title was changed: kept %d characters", len([]rune(i.Title)))
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("字", maxTitleLen+1)
	j := e.issue(e.admin, IssueInput{Project: p.Key, Title: long})
	if j.Title == long || j.Description != long {
		t.Fatalf("an over-long title should become a short title with the text as description")
	}
}
