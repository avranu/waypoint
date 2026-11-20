package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestRetryRepoHookAccess(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	mk := func(name string, admin bool) *core.Actor {
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct horse battery", IsAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		return &core.Actor{User: u}
	}
	robin, bob := mk("robin", true), mk("bob", false)
	kind, base, name := "forgejo", "https://git.example.com", "Forgejo"
	f, err := svc.CreateForge(ctx, robin, core.ForgeInput{Kind: &kind, BaseURL: &base, Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	pname, key := "Alpha", "ALP"
	if _, err := svc.CreateProject(ctx, robin, core.ProjectInput{Name: &pname, Key: &key}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMember(ctx, robin, key, "bob", "editor"); err != nil {
		t.Fatal(err)
	}
	r, err := svc.AddProjectRepo(ctx, robin, key, f.ID, "acme/app", false)
	if err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	as := func(a *core.Actor, readOnly bool) *client {
		tok, _, err := svc.CreateAPIToken(ctx, a, "t", readOnly, 0)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	path := fmt.Sprintf("/api/v1/projects/%s/repos/%d/hook", key, r.ID)
	for _, c := range []struct {
		who  string
		c    *client
		path string
		want int
	}{
		{"editor", as(bob, false), path, 403},
		{"read-only owner token", as(robin, true), path, 403},
		{"owner, unknown repo", as(robin, false), fmt.Sprintf("/api/v1/projects/%s/repos/%d/hook", key, r.ID+99), 404},
		{"owner", as(robin, false), path, 204},
	} {
		if code, b := c.c.do("POST", c.path, nil); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.who, code, b, c.want)
		}
	}
	svc.Wait()
	var repos []core.ProjectRepo
	as(robin, false).json("GET", "/api/v1/projects/"+key+"/repos", nil, &repos)
	if len(repos) != 1 || repos[0].HookState != "failed" || repos[0].HookNote == "" {
		t.Errorf("after retry with no forge token: %+v", repos)
	}
}

func TestOrgHookAndDeliveryLogAccess(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	mk := func(name string, admin bool) *core.Actor {
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct horse battery", IsAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		return &core.Actor{User: u}
	}
	robin, bob := mk("robin", true), mk("bob", false)
	kind, base, name := "github", "https://github.example.com", "GitHub"
	f, err := svc.CreateForge(ctx, robin, core.ForgeInput{Kind: &kind, BaseURL: &base, Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	pname, key := "Alpha", "ALP"
	if _, err := svc.CreateProject(ctx, robin, core.ProjectInput{Name: &pname, Key: &key}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMember(ctx, robin, key, "bob", "editor"); err != nil {
		t.Fatal(err)
	}
	// A webhook to an address the egress policy refuses: logged, not sent.
	u, ev := "http://127.0.0.1:1/hook", "issue.created"
	h, err := svc.CreateWebhook(ctx, robin, key, core.WebhookInput{URL: &u, Events: &ev})
	if err != nil {
		t.Fatal(err)
	}
	title := "Logged"
	if _, err := svc.CreateIssue(ctx, robin, core.IssueInput{Project: key, Title: title}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	as := func(a *core.Actor, readOnly bool) *client {
		tok, _, err := svc.CreateAPIToken(ctx, a, "t", readOnly, 0)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	var log []core.WebhookDelivery
	as(robin, false).json("GET", fmt.Sprintf("/api/v1/webhooks/%d/deliveries", h.ID), nil, &log)
	if len(log) != 1 || log[0].Error == "" {
		t.Fatalf("log: %+v", log)
	}
	replay := fmt.Sprintf("/api/v1/webhooks/%d/deliveries/%d/replay", h.ID, log[0].ID)
	org := map[string]string{"org": "acme"}
	orgs := fmt.Sprintf("/api/v1/forges/%d/org-hooks", f.ID)
	for _, c := range []struct {
		who, method, path string
		c                 *client
		body              any
		want              int
	}{
		{"editor reads the log", "GET", fmt.Sprintf("/api/v1/webhooks/%d/deliveries", h.ID), as(bob, false), nil, 403},
		{"editor replays", "POST", replay, as(bob, false), nil, 403},
		{"read-only owner replays", "POST", replay, as(robin, true), nil, 403},
		{"unknown delivery", "POST", fmt.Sprintf("/api/v1/webhooks/%d/deliveries/%d/replay", h.ID, log[0].ID+99), as(robin, false), nil, 404},
		{"owner replays", "POST", replay, as(robin, false), nil, 200},
		{"non-admin lists org hooks", "GET", orgs, as(bob, false), nil, 403},
		{"non-admin adds an org hook", "POST", orgs, as(bob, false), org, 403},
		{"read-only admin adds an org hook", "POST", orgs, as(robin, true), org, 403},
		{"admin adds an org hook", "POST", orgs, as(robin, false), org, 200},
		{"admin, bad org", "POST", orgs, as(robin, false), map[string]string{"org": "acme/app"}, 400},
		{"admin lists org hooks", "GET", orgs, as(robin, false), nil, 200},
	} {
		if code, b := c.c.do(c.method, c.path, c.body); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.who, code, b, c.want)
		}
	}
	svc.Wait()
	var hooks []core.OrgHook
	as(robin, false).json("GET", orgs, nil, &hooks)
	if len(hooks) != 1 || hooks[0].State != "failed" || hooks[0].Note == "" {
		t.Fatalf("org hook without a forge token: %+v", hooks)
	}
	if code, b := as(bob, false).do("DELETE", fmt.Sprintf("%s/%d", orgs, hooks[0].ID), nil); code != 403 {
		t.Errorf("non-admin removed an org hook: %d %s", code, b)
	}
	if code, b := as(robin, false).do("DELETE", fmt.Sprintf("%s/%d", orgs, hooks[0].ID), nil); code != 204 {
		t.Errorf("admin removes an org hook: %d %s", code, b)
	}
}
