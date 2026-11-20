package main

import (
	"context"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// WP-449: a task's comments reach the agent wherever it reads the task, right
// after the description: wp next (and wp next --peek, wp take) and wp show.
func TestTaskAnswersCarryTheComments(t *testing.T) {
	s := newSessionEnv(t)
	ctx := context.Background()
	i, err := s.svc.CreateIssue(ctx, s.me, core.IssueInput{Project: s.project.Key, Title: "Trim the logs", Description: "Keep a week of logs.", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.CreateIssue(ctx, s.me, core.IssueInput{Project: s.project.Key, Title: "A sub-task", Parent: i.Key}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.AddComment(ctx, s.me, i.Key, "Actually a month: audits read them.\nThe week was a typo."); err != nil {
		t.Fatal(err)
	}
	s.asAgent()
	check := func(what, out string) {
		t.Helper()
		desc, c, sub := strings.Index(out, "Keep a week of logs."), strings.Index(out, "Actually a month: audits read them."), strings.Index(out, "A sub-task")
		if desc < 0 || c < desc || sub < c {
			t.Errorf("%s: not description, comment, sub-tasks in that order:\n%s", what, out)
		}
		if !strings.Contains(out, "\n    The week was a typo.") {
			t.Errorf("%s: the comment's later line isn't kept under it:\n%s", what, out)
		}
	}
	check("wp next --peek", s.wp("next", "--peek", "--no-brief"))
	check("wp next KEY", s.wp("next", i.Key, "--no-brief"))
	check("wp show", s.wp("show", i.Key))
}

func TestWithPrep(t *testing.T) {
	const prep = "Pointers\n"
	for _, c := range []struct{ text, want string }{
		{"WP-1 x\n\ndesc\n\nComments:\n  [#1 a] b\n    c\n\nChildren:\n  WP-2\n",
			"WP-1 x\n\ndesc\n\nComments:\n  [#1 a] b\n    c\n\nPointers\n\nChildren:\n  WP-2\n"},
		{"WP-1 x\n\ndesc\n\nComments:\n  [#1 a] b\n",
			"WP-1 x\n\ndesc\n\nComments:\n  [#1 a] b\n\nPointers\n"},
		{"WP-1 x\n\ndesc\n\nLinks:\n  pr\n",
			"WP-1 x\n\ndesc\n\nPointers\n\nLinks:\n  pr\n"},
		{"WP-1 x\n\ndesc\n", "WP-1 x\n\ndesc\n\nPointers\n"},
	} {
		if got := withPrep(c.text, prep); got != c.want {
			t.Errorf("withPrep(%q)\n got %q\nwant %q", c.text, got, c.want)
		}
	}
}

// WP-449: the full brief keeps a short comment that says something, and says
// how many it left out.
func TestBriefTaskSectionComments(t *testing.T) {
	robin := &struct {
		Username string `json:"username"`
	}{"robin"}
	var cs []commentForBrief
	for _, b := range []string{"Not the north hall.", "ok", "c2 says more", "c3 says more", "c4 says more", "c5 says more", "c6 says more"} {
		cs = append(cs, commentForBrief{Author: robin, Body: b, CreatedAt: "2026-11-02T14:00:00Z"})
	}
	iss := issueForBrief{Key: "WP-9", Title: "T"}
	c := &cli{}
	out := c.taskSection(iss, cs[:1], 2000)
	if !strings.Contains(out, "@robin] Not the north hall.") {
		t.Errorf("a short comment was skipped:\n%s", out)
	}
	out = c.taskSection(iss, cs, 2000)
	if !strings.Contains(out, "] c6 says more") || strings.Contains(out, "] c2 says more") || strings.Contains(out, "] ok") ||
		!strings.Contains(out, "  … 2 earlier: wp show WP-9 has every comment\n") {
		t.Errorf("the newest four, then what is left:\n%s", out)
	}
}
