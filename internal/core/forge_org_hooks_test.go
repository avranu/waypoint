package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func signedGitHub(secret, event string, body []byte) http.Header {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return h
}

func TestRepoHookTestDelivery(t *testing.T) {
	for _, kind := range []string{"github", "forgejo", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			fake, srv := newHookForge(t, kind)
			f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr(kind), BaseURL: ptr(srv.URL), Token: ptr("tok")})
			p := e.project(e.admin, "Alpha", "kanban")
			r, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false)
			if err != nil {
				t.Fatal(err)
			}
			e.repoState(p.Key, r.ID)
			want := []string{"acme/app"}
			if kind == "github" {
				want = nil
			}
			if got := fake.tested(); !slices.Equal(got, want) {
				t.Errorf("test deliveries after adding: %v, want %v", got, want)
			}
			if err := e.s.RetryRepoHook(e.ctx, e.admin, p.Key, r.ID); err != nil {
				t.Fatal(err)
			}
			e.repoState(p.Key, r.ID)
			if got := fake.tested(); len(got) != len(want)+1 {
				t.Errorf("test deliveries after retry: %v", got)
			}
		})
	}
}

func TestForgejoTestPushIsAPing(t *testing.T) {
	e := newEnv(t)
	_, srv := newHookForge(t, "forgejo")
	f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
	p := e.project(e.admin, "Alpha", "kanban")
	r, _ := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Thing"})
	e.repoState(p.Key, r.ID)
	body := []byte(`{"ref":"refs/heads/main","before":"abc","after":"abc","commits":[{"message":"fix ` + i.Key + `","url":"https://git.test/acme/app/commit/abc"}],
		"repository":{"full_name":"acme/app","owner":{"username":"acme"}}}`)
	h := signedGitHub(secret, "", body)
	h.Del("X-GitHub-Event")
	h.Set("X-Forgejo-Event", "push")
	h.Set("X-Forgejo-Signature", strings.TrimPrefix(h.Get("X-Hub-Signature-256"), "sha256="))
	h.Del("X-Hub-Signature-256")
	if err := e.s.HandleForgeWebhook(e.ctx, f.ID, h, body); err != nil {
		t.Fatal(err)
	}
	if st := e.repoState(p.Key, r.ID); st.HookSeenAt == nil {
		t.Error("the test delivery wasn't noted")
	}
	if links, _ := e.s.externalLinks(e.ctx, e.s.db.R, i.ID); len(links) != 0 {
		t.Errorf("a test delivery linked a commit: %+v", links)
	}
}

func TestDeleteProjectRemovesItsHooks(t *testing.T) {
	e := newEnv(t)
	fake, srv := newHookForge(t, "github")
	f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("github"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	a := e.project(e.admin, "Alpha", "kanban")
	b := e.project(e.admin, "Beta", "kanban")
	for _, x := range []struct {
		key, repo string
	}{{a.Key, "acme/shared"}, {b.Key, "acme/shared"}, {a.Key, "acme/own"}} {
		if _, err := e.s.AddProjectRepo(e.ctx, e.admin, x.key, f.ID, x.repo, false); err != nil {
			t.Fatal(err)
		}
	}
	e.s.Wait()
	fake.only("acme/shared")
	fake.only("acme/own")
	if err := e.s.DeleteProject(e.ctx, e.admin, a.Key); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if n := fake.count("acme/own"); n != 0 {
		t.Errorf("the deleted project's own repo kept its webhook (%d)", n)
	}
	fake.only("acme/shared")
}

func TestOrgHookLifecycle(t *testing.T) {
	for _, kind := range []string{"github", "forgejo", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			fake, srv := newHookForge(t, kind)
			fake.orgs = []string{"acme", "other"}
			f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr(kind), BaseURL: ptr(srv.URL), Token: ptr("tok")})
			secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
			p := e.project(e.admin, "Alpha", "kanban")
			app, _ := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false)
			lib, _ := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "other/lib", false)
			e.s.Wait()
			fake.only("acme/app")

			orgs, err := e.s.ForgeOrgs(e.ctx, e.admin, f.ID)
			if err != nil || !slices.Equal(orgs, []string{"acme", "other"}) {
				t.Fatalf("orgs: %v %v", orgs, err)
			}
			oh, err := e.s.AddOrgHook(e.ctx, e.admin, f.ID, "Acme")
			if err != nil {
				t.Fatal(err)
			}
			e.s.Wait()
			_, body := fake.only("org:Acme")
			if u := cmp(asString(hookConfig(body)["url"]), asString(body["url"])); u != f.WebhookURL {
				t.Errorf("org hook points at %q", u)
			}
			if s := cmp(asString(hookConfig(body)["secret"]), asString(body["token"])); s != secret {
				t.Errorf("org hook secret %q", s)
			}
			if n := fake.count("acme/app"); n != 0 {
				t.Errorf("the covered repo kept its own hook (%d): deliveries would arrive twice", n)
			}
			if st := e.repoState(p.Key, app.ID); st.HookState != hookCovered || st.CoveredBy != "Acme" {
				t.Errorf("covered repo: %+v", st)
			}
			if st := e.repoState(p.Key, lib.ID); st.HookState != hookInstalled || st.CoveredBy != "" {
				t.Errorf("repo outside the org: %+v", st)
			}
			hooks, _ := e.s.ListOrgHooks(e.ctx, e.admin, f.ID)
			if len(hooks) != 1 || hooks[0].State != hookInstalled || hooks[0].ID != oh.ID {
				t.Fatalf("org hooks: %+v", hooks)
			}
			if got, want := slices.Contains(fake.tested(), "org:Acme"), kind == "gitlab"; got != want {
				t.Errorf("org test delivery requested: %v, want %v (%v)", got, want, fake.tested())
			}

			two, _ := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/two", false)
			if st := e.repoState(p.Key, two.ID); st.HookState != hookCovered || fake.count("acme/two") != 0 {
				t.Errorf("new repo in a covered org: %+v (%d hooks)", st, fake.count("acme/two"))
			}

			if kind == "github" {
				body := []byte(`{"zen":"hi","repository":{"full_name":"acme/two","owner":{"type":"Organization"}}}`)
				if err := e.s.HandleForgeWebhook(e.ctx, f.ID, signedGitHub(secret, "ping", body), body); err != nil {
					t.Fatal(err)
				}
				if hooks, _ := e.s.ListOrgHooks(e.ctx, e.admin, f.ID); hooks[0].SeenAt == nil {
					t.Error("a delivery from a repo in the org wasn't noted on its hook")
				}
			}

			if err := e.s.RemoveOrgHook(e.ctx, e.admin, f.ID, oh.ID); err != nil {
				t.Fatal(err)
			}
			e.s.Wait()
			if n := fake.count("org:Acme"); n != 0 {
				t.Errorf("org hook kept after removal (%d)", n)
			}
			for _, r := range []*ProjectRepo{app, two} {
				if st := e.repoState(p.Key, r.ID); st.HookState != hookInstalled || st.CoveredBy != "" {
					t.Errorf("%s after the org hook went: %+v", r.Repo, st)
				}
				fake.only(r.Repo)
			}

			if _, err := e.s.AddOrgHook(e.ctx, e.admin, f.ID, "other"); err != nil {
				t.Fatal(err)
			}
			e.s.Wait()
			fake.only("org:other")
			if err := e.s.DeleteForge(e.ctx, e.admin, f.ID); err != nil {
				t.Fatal(err)
			}
			e.s.Wait()
			if n := fake.count("org:other"); n != 0 {
				t.Errorf("org hook kept after the forge was deleted (%d)", n)
			}
		})
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func TestOrgHookFailureKeepsRepoHooks(t *testing.T) {
	e := newEnv(t)
	fake, srv := newHookForge(t, "gitlab")
	fake.refuse["org:grp"] = 404
	f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("gitlab"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	p := e.project(e.admin, "Alpha", "kanban")
	r, _ := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "grp/sub/app", false)
	if _, err := e.s.AddOrgHook(e.ctx, e.admin, f.ID, "grp"); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	hooks, _ := e.s.ListOrgHooks(e.ctx, e.admin, f.ID)
	if len(hooks) != 1 || hooks[0].State != hookFailed || !strings.Contains(hooks[0].Note, "Premium") {
		t.Fatalf("failed group hook: %+v", hooks)
	}
	if st := e.repoState(p.Key, r.ID); st.HookState != hookInstalled || st.CoveredBy != "" {
		t.Errorf("repo under a failed group hook: %+v", st)
	}
	fake.only("grp/sub/app")
	delete(fake.refuse, "org:grp")
	if _, err := e.s.AddOrgHook(e.ctx, e.admin, f.ID, "grp"); err != nil {
		t.Fatal(err)
	}
	if st := e.repoState(p.Key, r.ID); st.HookState != hookCovered || st.CoveredBy != "grp" || fake.count("grp/sub/app") != 0 {
		t.Errorf("after retry: %+v (%d repo hooks)", st, fake.count("grp/sub/app"))
	}
}

func TestOrgHookAccessAndValidation(t *testing.T) {
	e := newEnv(t)
	_, srv := newHookForge(t, "github")
	f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("github"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	bob := e.user("bob", false)
	if _, err := e.s.AddOrgHook(e.ctx, bob, f.ID, "acme"); err == nil {
		t.Error("a non-admin added an organisation webhook")
	}
	if _, err := e.s.ListOrgHooks(e.ctx, bob, f.ID); err == nil {
		t.Error("a non-admin listed organisation webhooks")
	}
	if _, err := e.s.ForgeOrgs(e.ctx, bob, f.ID); err == nil {
		t.Error("a non-admin listed the forge's organisations")
	}
	ro := &Actor{User: e.admin.User, ReadOnly: true}
	if _, err := e.s.AddOrgHook(e.ctx, ro, f.ID, "acme"); err == nil {
		t.Error("a read-only token added an organisation webhook")
	}
	for _, bad := range []string{"", "acme/app", "..", "a b"} {
		if _, err := e.s.AddOrgHook(e.ctx, e.admin, f.ID, bad); err == nil {
			t.Errorf("organisation %q accepted", bad)
		}
	}
	h, err := e.s.AddOrgHook(e.ctx, e.admin, f.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.RemoveOrgHook(e.ctx, bob, f.ID, h.ID); err == nil {
		t.Error("a non-admin removed an organisation webhook")
	}
	if err := e.s.RemoveOrgHook(e.ctx, e.admin, f.ID+1, h.ID); err == nil {
		t.Error("removed through another forge")
	}
}

func TestOrgPingNoted(t *testing.T) {
	e := newEnv(t)
	_, srv := newHookForge(t, "github")
	f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("github"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
	if _, err := e.s.AddOrgHook(e.ctx, e.admin, f.ID, "acme"); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	body := []byte(`{"zen":"hi","hook":{"type":"Organization"},"organization":{"login":"ACME"}}`)
	if err := e.s.HandleForgeWebhook(e.ctx, f.ID, signedGitHub(secret, "ping", body), body); err != nil {
		t.Fatal(err)
	}
	if hooks, _ := e.s.ListOrgHooks(e.ctx, e.admin, f.ID); len(hooks) != 1 || hooks[0].SeenAt == nil {
		t.Errorf("organisation ping not noted: %+v", hooks)
	}
}
