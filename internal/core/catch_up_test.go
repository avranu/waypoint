package core

import (
	"errors"
	"strings"
	"testing"
)

// Catch up (WP-245) lists an agent's unreviewed closes with the agent's
// summary, its review screenshots first, and its try links completed with
// the project's production URL.
func TestCatchUp(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Catch", "")
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Add the live page"})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Second"})
	mine := e.issue(e.admin, IssueInput{Project: p.Key, Title: "A person's close"})
	for _, k := range []string{a.Key, b.Key} {
		if _, err := e.s.AgentTake(e.ctx, agent, k, ""); err != nil {
			t.Fatal(err)
		}
	}
	tg := AttachTarget{TargetIssue, a.ID}
	seen, err := e.s.AddAttachment(e.ctx, agent, tg, AttachmentInput{Name: "seen.png", Data: pngBytes})
	if err != nil {
		t.Fatal(err)
	}
	shot, err := e.s.AddAttachment(e.ctx, agent, tg, AttachmentInput{Name: "shot.png", Data: pngBytes, Review: true})
	if err != nil || !shot.Review {
		t.Fatalf("review shot: %+v %v", shot, err)
	}
	if _, err := e.s.AddAttachment(e.ctx, agent, tg, AttachmentInput{Name: "notes.txt", Data: []byte("text")}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"/live?x=1", "https://example.com/a", "/live?x=1"} {
		if _, err := e.s.AddTryLink(e.ctx, agent, a.Key, u); err != nil {
			t.Fatalf("try link %q: %v", u, err)
		}
	}
	for _, bad := range []string{"", "live", "//evil.example/x", "javascript:alert(1)", "ftp://x/y"} {
		if _, err := e.s.AddTryLink(e.ctx, agent, a.Key, bad); !IsInvalid(err) {
			t.Errorf("try link %q: %v", bad, err)
		}
	}
	if _, err := e.s.CreateEnvironment(e.ctx, e.admin, p.Key, EnvironmentInput{Name: str("prod"), Kind: str(EnvProduction), URL: str("https://catch.example/")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentDone(e.ctx, agent, a.Key, AgentDoneInput{Comment: "Added /live.\n\nIt lists what is deployed."}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentDone(e.ctx, agent, b.Key, AgentDoneInput{Comment: "Did b."}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, mine.Key, IssuePatch{Status: str("done")}); err != nil {
		t.Fatal(err)
	}

	c, err := e.s.CatchUp(e.ctx, e.admin, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 2 || len(c.Items) != 2 || c.Items[0].Issue.Key != a.Key || c.Items[1].Issue.Key != b.Key {
		t.Fatalf("catch up: %d %+v", c.Total, c.Items)
	}
	it := c.Items[0]
	if it.Agent != "anthropic/claude-opus-5-5" || it.Category != CatDone || !strings.HasPrefix(it.Summary, "Added /live.") {
		t.Errorf("item: %+v", it)
	}
	if len(it.Shots) != 2 || it.Shots[0].ID != shot.ID || it.Shots[1].ID != seen.ID {
		t.Errorf("shots (review first, images only): %+v", it.Shots)
	}
	if len(it.Try) != 2 || it.Try[0].URL != "https://catch.example/live?x=1" || it.Try[1].URL != "https://example.com/a" {
		t.Errorf("try links: %+v", it.Try)
	}
	if all, err := e.s.CatchUp(e.ctx, e.admin, ""); err != nil || all.Total != 2 {
		t.Errorf("everywhere: %+v %v", all, err)
	}

	if n, err := e.s.ReviewCloses(e.ctx, e.admin, []string{b.Key}); err != nil || n != 1 {
		t.Fatalf("review: %d %v", n, err)
	}
	if c, _ := e.s.CatchUp(e.ctx, e.admin, p.Key); c.Total != 1 {
		t.Errorf("after review: %+v", c.Items)
	}

	if _, err := e.s.NotFinished(e.ctx, agent, a.Key, "no"); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent sent work back: %v", err)
	}
	d, err := e.s.NotFinished(e.ctx, e.admin, a.Key, "The page is blank on a phone.")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status.Category != CatTodo || d.CloseUnreviewed {
		t.Errorf("not finished: %+v", d.Issue)
	}
	if got := e.comments(a.Key); len(got) == 0 || got[len(got)-1].Body != "Not finished: The page is blank on a phone." {
		t.Errorf("comments: %+v", got)
	}
	if _, err := e.s.NotFinished(e.ctx, e.admin, a.Key, ""); !IsInvalid(err) {
		t.Errorf("not finished on an open task: %v", err)
	}
	if c, _ := e.s.CatchUp(e.ctx, e.admin, p.Key); c.Total != 0 {
		t.Errorf("after not finished: %+v", c.Items)
	}

	bug, err := e.s.ReportProblem(e.ctx, e.admin, b.Key, "Button overlaps the title\nOnly at 390px.")
	if err != nil {
		t.Fatal(err)
	}
	if bug.Type != "bug" || bug.Title != "Button overlaps the title" || !strings.Contains(bug.Description, "Found while checking "+b.Key) {
		t.Errorf("bug: %+v", bug.Issue)
	}
	if len(bug.Relations) != 1 || bug.Relations[0].Issue.Key != b.Key {
		t.Errorf("relations: %+v", bug.Relations)
	}
	bare, err := e.s.ReportProblem(e.ctx, e.admin, b.Key, "")
	if err != nil || bare.Title != "Problem with "+b.Key+": Second" {
		t.Errorf("bare problem: %+v %v", bare, err)
	}
	if _, err := e.s.ReportProblem(e.ctx, agent, b.Key, "x"); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent reported a problem: %v", err)
	}
}
