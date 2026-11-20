package core

import (
	"strings"
	"testing"
)

func TestOpenPullRequest(t *testing.T) {
	e := newEnv(t)
	g := newShipForge()
	e.s.Forges = g
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot") // the token's login is its connector's (WP-279)
	in := OpenPRInput{Project: p.Key, Head: "feature", Title: "Do it", Body: "Because."}

	if _, err := e.s.OpenPullRequest(e.ctx, e.admin, in); err == nil || !strings.Contains(err.Error(), "no repository attached") {
		t.Errorf("no repo attached: %v", err)
	}
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.OpenPullRequest(e.ctx, e.admin, in); err == nil || !strings.Contains(err.Error(), "no API token") {
		t.Errorf("no token: %v", err)
	}
	if _, err := e.s.UpdateForge(e.ctx, e.admin, f.ID, ForgeInput{Token: str("t")}); err != nil {
		t.Fatal(err)
	}

	// Someone who can't edit the project can't open pull requests in it.
	outsider := e.user("sam", false)
	if _, err := e.s.OpenPullRequest(e.ctx, outsider, in); err == nil {
		t.Error("an outsider opened a pull request")
	}
	if _, err := e.s.OpenPullRequest(e.ctx, e.admin, OpenPRInput{Project: p.Key, Head: "main", Title: "x"}); err == nil || !strings.Contains(err.Error(), "base branch") {
		t.Errorf("from the base branch: %v", err)
	}
	if _, err := e.s.OpenPullRequest(e.ctx, e.admin, OpenPRInput{Project: p.Key, Head: "feature"}); !IsInvalid(err) {
		t.Errorf("no title: %v", err)
	}

	// An agent opens one: it is the agent's, linked to the issue, and a
	// second call finds the same pull request.
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	iss, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: p.Key, Title: "Do it"})
	if err != nil {
		t.Fatal(err)
	}
	in.Issue = iss.Key
	res, err := e.s.OpenPullRequest(e.ctx, agent, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Existing || res.Warning != "" || res.Pull.Title != "Do it" {
		t.Errorf("first open: %+v warning %q", res.Pull.PRSummary, res.Warning)
	}
	if len(g.created) != 1 || g.created[0].Base != "main" || g.created[0].Body != "Because." {
		t.Errorf("created: %+v", g.created)
	}
	again, err := e.s.OpenPullRequest(e.ctx, agent, in)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Existing || again.Pull.ID != res.Pull.ID || len(g.created) != 1 {
		t.Errorf("second open: existing=%v created=%d", again.Existing, len(g.created))
	}
	in.Title = ""
	if retry, err := e.s.OpenPullRequest(e.ctx, agent, in); err != nil || !retry.Existing {
		t.Errorf("a retry without a title: %v", err)
	}
	d, err := e.s.GetIssue(e.ctx, e.admin, iss.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Links) != 1 || d.Links[0].Kind != "pull" {
		t.Errorf("links: %+v", d.Links)
	}
	if pr, err := e.s.prByID(e.ctx, e.s.db.R, res.Pull.ID); err != nil || pr.Agent != "anthropic/claude-opus-5-5" {
		t.Errorf("the pull request's agent: %+v %v", pr, err)
	}
	// The agent may arm what it opened.
	if _, err := e.s.MergePR(e.ctx, agent, res.Pull.ID, MergeInput{WhenReady: true}); err != nil {
		t.Errorf("arming its own: %v", err)
	}
}
