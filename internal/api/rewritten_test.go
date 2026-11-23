package api

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The rewritten-work call refuses what it can't act on, as a 400 that names
// the field, before anything is recorded.
func TestRewrittenRefusesMalformedCalls(t *testing.T) {
	ts, svc := setup(t)
	link, err := svc.BootstrapInvite(context.Background())
	if err != nil || link == nil {
		t.Fatal("bootstrap invite", err)
	}
	tok := link.URL[strings.LastIndex(link.URL, "/")+1:]
	jar, _ := cookiejar.New(nil)
	web := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}, web: true}
	if code, b := web.do("POST", "/api/v1/auth/invite/"+tok, map[string]any{"username": "robin", "email": "robin@example.com", "password": "correct horse battery"}); code != 200 {
		t.Fatalf("accept: %d %s", code, b)
	}
	if code, b := web.do("POST", "/api/v1/projects", map[string]any{"name": "Waypoint", "key": "WAY"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}
	if code, b := web.do("POST", "/api/v1/issues", map[string]any{"project": "WAY", "title": "x"}); code != 200 {
		t.Fatalf("issue: %d %s", code, b)
	}
	for _, c := range []struct {
		name string
		body string
		want string
	}{
		{"blank repo", `{"repo":"","head":"abc1234","prs":[]}`, "repo"},
		{"no repo", `{"head":"abc1234","prs":[]}`, "repo"},
		{"no prs", `{"repo":"o/r","head":"abc1234"}`, "prs"},
		{"long repo", `{"repo":"` + strings.Repeat("a", 1<<20) + `","head":"abc1234","prs":[]}`, "repo"},
		{"long head", `{"repo":"o/r","head":"` + strings.Repeat("a", 1<<20) + `","prs":[]}`, "head"},
	} {
		code, b := web.do("POST", "/api/v1/issues/WAY-1/rewritten", c.body)
		if code != 400 || !strings.Contains(string(b), c.want) {
			t.Errorf("%s: %d %.200s, want a 400 naming %q", c.name, code, b, c.want)
		}
	}
	if code, b := web.do("POST", "/api/v1/issues/WAY-1/rewritten", `{"repo":"o/r","head":"abc1234","prs":[]}`); code != 200 {
		t.Errorf("a well-formed empty call: %d %s", code, b)
	}
}

// The rewritten-work call writes relations, so a token that may only read is
// refused at the route (WP-1370).
func TestRewrittenNeedsIssuesEdit(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	admin := &core.Actor{User: u}
	name, key := "Way", "WAY"
	if _, err := svc.CreateProject(ctx, admin, core.ProjectInput{Name: &name, Key: &key}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateIssue(ctx, admin, core.IssueInput{Project: "WAY", Title: "x"}); err != nil {
		t.Fatal(err)
	}
	mk := func(perms ...core.Permission) *client {
		tok, _, err := svc.CreateToken(ctx, admin, core.TokenSpec{Name: "t", Permissions: perms})
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	body := `{"repo":"o/r","head":"abc1234","prs":[]}`
	if code, b := mk(core.PermIssuesRead).do("POST", "/api/v1/issues/WAY-1/rewritten", body); code != 403 {
		t.Errorf("read-only token: %d %s, want 403", code, b)
	}
	if code, b := mk(core.PermIssuesRead, core.PermIssuesEdit).do("POST", "/api/v1/issues/WAY-1/rewritten", body); code != 200 {
		t.Errorf("editing token: %d %s, want 200", code, b)
	}
}
