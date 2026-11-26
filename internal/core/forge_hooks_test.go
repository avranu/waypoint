package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookForge is a fake GitHub, Forgejo or GitLab that keeps repo webhooks.
type hookForge struct {
	t      *testing.T
	kind   string
	mu     sync.Mutex
	next   int64
	hooks  map[string]map[string]map[string]any
	calls  []string
	status int
	body   string
	orgs   []string
	refuse map[string]int
	tests  []string
	writer string
}

func (h *hookForge) tested() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.tests)
}

func newHookForge(t *testing.T, kind string) (*hookForge, *httptest.Server) {
	h := &hookForge{t: t, kind: kind, next: 100, hooks: map[string]map[string]map[string]any{}, refuse: map[string]int{}}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *hookForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	path := r.URL.EscapedPath()
	prefix := map[string]string{"github": "/api/v3", "forgejo": "/api/v1", "gitlab": "/api/v4"}[h.kind]
	path = strings.TrimPrefix(path, prefix)
	h.calls = append(h.calls, r.Method+" "+path)
	if h.status != 0 {
		w.WriteHeader(h.status)
		_, _ = w.Write([]byte(h.body))
		return
	}
	if r.Header.Get("Authorization") == "" && r.Header.Get("PRIVATE-TOKEN") == "" {
		w.WriteHeader(401)
		return
	}
	if r.Method == "GET" && (path == "/user/orgs" || path == "/groups") {
		out := []map[string]any{}
		for _, o := range h.orgs {
			out = append(out, map[string]any{"login": o, "username": o, "full_path": o})
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var repo, rest string
	switch {
	case strings.HasPrefix(path, "/orgs/") || strings.HasPrefix(path, "/groups/"):
		p := strings.TrimPrefix(strings.TrimPrefix(path, "/orgs/"), "/groups/")
		enc, after, _ := strings.Cut(p, "/")
		org, _ := url.PathUnescape(enc)
		repo, rest = "org:"+org, after
	case h.kind == "gitlab":
		p := strings.TrimPrefix(path, "/projects/")
		enc, after, _ := strings.Cut(p, "/")
		repo, _ = url.PathUnescape(enc)
		rest = after
	default:
		parts := strings.SplitN(strings.TrimPrefix(path, "/repos/"), "/", 3)
		repo, rest = parts[0]+"/"+parts[1], parts[2]
	}
	if h.refuse[repo] != 0 {
		w.WriteHeader(h.refuse[repo])
		return
	}
	if h.writer != "" && r.Method != "GET" && !strings.HasSuffix(r.Header.Get("Authorization"), " "+h.writer) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"token does not have at least one of required scope(s): [write:repository]"}`))
		return
	}
	if r.Method == "POST" && (strings.HasSuffix(rest, "/tests") || strings.HasSuffix(rest, "/pings") || strings.HasSuffix(rest, "/test/push_events")) {
		h.tests = append(h.tests, repo)
		w.WriteHeader(204)
		return
	}
	id := strings.TrimPrefix(strings.TrimPrefix(rest, "hooks"), "/")
	if h.hooks[repo] == nil {
		h.hooks[repo] = map[string]map[string]any{}
	}
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case r.Method == "GET" && id == "":
		out := []map[string]any{}
		for hid, b := range h.hooks[repo] {
			var n int64
			fmt.Sscan(hid, &n)
			if h.kind == "gitlab" {
				out = append(out, map[string]any{"id": n, "url": b["url"]})
			} else {
				out = append(out, map[string]any{"id": n, "config": map[string]any{"url": hookConfig(b)["url"]}})
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && id == "":
		h.next++
		hid := fmt.Sprint(h.next)
		h.hooks[repo][hid] = body
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": h.next})
	case (r.Method == "PATCH" || r.Method == "PUT") && id != "":
		if h.hooks[repo][id] == nil {
			w.WriteHeader(404)
			return
		}
		h.hooks[repo][id] = body
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	case r.Method == "DELETE" && id != "":
		if h.hooks[repo][id] == nil {
			w.WriteHeader(404)
			return
		}
		delete(h.hooks[repo], id)
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}

func hookConfig(b map[string]any) map[string]any {
	c, _ := b["config"].(map[string]any)
	return c
}

// only returns the repo's single webhook body, failing if there isn't one.
func (h *hookForge) only(repo string) (string, map[string]any) {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.hooks[repo]); n != 1 {
		h.t.Fatalf("%s has %d webhooks, want 1 (calls %v)", repo, n, h.calls)
	}
	for id, b := range h.hooks[repo] {
		return id, b
	}
	return "", nil
}

func (h *hookForge) count(repo string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.hooks[repo])
}

func (e *env) repoState(key string, id int64) ProjectRepo {
	e.t.Helper()
	e.s.Wait()
	repos, err := e.s.ListProjectRepos(e.ctx, e.admin, key)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range repos {
		if r.ID == id {
			return r
		}
	}
	e.t.Fatalf("repo %d not in %s", id, key)
	return ProjectRepo{}
}

func TestRepoHookLifecycle(t *testing.T) {
	for _, kind := range []string{"github", "forgejo", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			fake, srv := newHookForge(t, kind)
			f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr(kind), BaseURL: ptr(srv.URL), Token: ptr("tok")})
			if err != nil {
				t.Fatal(err)
			}
			secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
			a := e.project(e.admin, "Alpha", "kanban")
			b := e.project(e.admin, "Beta", "kanban")
			repo := "acme/app"
			ra, err := e.s.AddProjectRepo(e.ctx, e.admin, a.Key, f.ID, repo, false)
			if err != nil {
				t.Fatal(err)
			}
			if st := e.repoState(a.Key, ra.ID); st.HookState != hookInstalled || st.HookNote != "" || st.HookCheckedAt == nil {
				t.Fatalf("state after attach: %+v", st)
			}
			_, body := fake.only(repo)
			gotURL, gotSecret := hookConfig(body)["url"], hookConfig(body)["secret"]
			if kind == "gitlab" {
				gotURL, gotSecret = body["url"], body["token"]
				for _, k := range []string{"issues_events", "merge_requests_events", "note_events", "push_events"} {
					if body[k] != true {
						t.Errorf("gitlab hook lacks %s: %v", k, body)
					}
				}
			} else {
				evs, _ := json.Marshal(body["events"])
				for _, ev := range hookEvents {
					if !strings.Contains(string(evs), `"`+ev+`"`) {
						t.Errorf("hook lacks event %s: %s", ev, evs)
					}
				}
				if hookConfig(body)["content_type"] != "json" {
					t.Errorf("content type: %v", body)
				}
			}
			if gotURL != f.WebhookURL || gotSecret != secret {
				t.Errorf("hook points at %v with %v; want %s with the forge secret", gotURL, gotSecret, f.WebhookURL)
			}

			if err := e.s.RetryRepoHook(e.ctx, e.admin, a.Key, ra.ID); err != nil {
				t.Fatal(err)
			}
			rb, err := e.s.AddProjectRepo(e.ctx, e.admin, b.Key, f.ID, repo, false)
			if err != nil {
				t.Fatal(err)
			}
			if st := e.repoState(b.Key, rb.ID); st.HookState != hookInstalled {
				t.Fatalf("second project: %+v", st)
			}
			fake.only(repo)

			if _, err := e.s.UpdateForge(e.ctx, e.admin, f.ID, ForgeInput{WebhookSecret: ptr("rotated-secret")}); err != nil {
				t.Fatal(err)
			}
			e.s.Wait()
			_, body = fake.only(repo)
			if hookConfig(body)["secret"] != "rotated-secret" && body["token"] != "rotated-secret" {
				t.Errorf("secret not refreshed: %v", body)
			}

			if err := e.s.RemoveProjectRepo(e.ctx, e.admin, a.Key, ra.ID); err != nil {
				t.Fatal(err)
			}
			e.s.Wait()
			fake.only(repo)
			if err := e.s.RemoveProjectRepo(e.ctx, e.admin, b.Key, rb.ID); err != nil {
				t.Fatal(err)
			}
			e.s.Wait()
			if n := fake.count(repo); n != 0 {
				t.Errorf("%d hooks left after the repo was detached everywhere", n)
			}
		})
	}
}

func TestRepoHookReusesExisting(t *testing.T) {
	e := newEnv(t)
	fake, srv := newHookForge(t, "github")
	f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("github"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	fake.hooks["acme/app"] = map[string]map[string]any{"77": {"config": map[string]any{"url": f.WebhookURL + "/", "secret": "old"}, "events": []any{"push"}}}
	p := e.project(e.admin, "Alpha", "kanban")
	r, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false)
	if err != nil {
		t.Fatal(err)
	}
	if st := e.repoState(p.Key, r.ID); st.HookState != hookInstalled {
		t.Fatalf("state: %+v", st)
	}
	id, body := fake.only("acme/app")
	if id != "77" || hookConfig(body)["secret"] == "old" || !slices.Contains(fake.calls, "PATCH /repos/acme/app/hooks/77") {
		t.Errorf("existing hook not reused and refreshed: id %s body %v calls %v", id, body, fake.calls)
	}
	if err := e.s.DeleteForge(e.ctx, e.admin, f.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if n := fake.count("acme/app"); n != 0 {
		t.Errorf("hook kept after the forge was removed: %d", n)
	}
}

func TestRepoHookFailures(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	cases := []struct {
		kind, token string
		status      int
		body, want  string
	}{
		{"github", "", 0, "", "no API token"},
		{"github", "tok", 403, `{"message":"Resource not accessible by personal access token"}`, "admin:repo_hook"},
		{"forgejo", "tok", 404, `{"message":"Not Found"}`, "write:repository"},
		{"gitlab", "tok", 403, `{"message":"403 Forbidden"}`, "Maintainer"},
		{"gitlab", "tok", 422, `{"message":{"url":["is blocked: Requests to the local network are not allowed"]}}`, "is blocked: Requests to the local network"},
		{"github", "tok", 401, `{"message":"Bad credentials"}`, "rejected the forge's token"},
	}
	for i, c := range cases {
		fake, srv := newHookForge(t, c.kind)
		fake.status, fake.body = c.status, c.body
		f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr(c.kind), Name: ptr(fmt.Sprint("f", i)), BaseURL: ptr(srv.URL), Token: ptr(c.token)})
		if err != nil {
			t.Fatal(err)
		}
		r, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, fmt.Sprint("acme/app", i), false)
		if err != nil {
			t.Fatal(err)
		}
		st := e.repoState(p.Key, r.ID)
		if st.HookState != hookFailed || !strings.Contains(st.HookNote, c.want) {
			t.Errorf("%s %d: %s %q, want failed with %q", c.kind, c.status, st.HookState, st.HookNote, c.want)
		}
		if c.token != "" {
			continue
		}
		if _, err := e.s.UpdateForge(e.ctx, e.admin, f.ID, ForgeInput{Token: ptr("tok")}); err != nil {
			t.Fatal(err)
		}
		if st := e.repoState(p.Key, r.ID); st.HookState != hookInstalled {
			t.Errorf("after adding a token: %+v", st)
		}
	}
}

func TestRepoHookBotToken(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	fake, srv := newHookForge(t, "forgejo")
	fake.mu.Lock()
	fake.writer = "bot-tok"
	fake.mu.Unlock()
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("f"), BaseURL: ptr(srv.URL), Token: ptr("read-tok")})
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false)
	if err != nil {
		t.Fatal(err)
	}
	if st := e.repoState(p.Key, r.ID); st.HookState != hookFailed || !strings.Contains(st.HookNote, "The forge token can't manage webhooks") ||
		!strings.Contains(st.HookNote, "Forgejo said: token does not have at least one of required scope(s)") {
		t.Fatalf("read-only token: %+v", st)
	}
	if _, err := e.s.UpdateForge(e.ctx, e.admin, f.ID, ForgeInput{BotToken: ptr("bot-tok")}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if st := e.repoState(p.Key, r.ID); st.HookState != hookInstalled {
		t.Fatalf("after the bot token: %+v", st)
	}
	fake.only("acme/app")
	if got := fake.tested(); !slices.Equal(got, []string{"acme/app"}) {
		t.Errorf("test deliveries = %v", got)
	}
	if err := e.s.RemoveProjectRepo(e.ctx, e.admin, p.Key, r.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	fake.mu.Lock()
	n := len(fake.hooks["acme/app"])
	fake.mu.Unlock()
	if n != 0 {
		t.Errorf("%d webhooks left after detaching", n)
	}
	fake.mu.Lock()
	fake.writer = "admin-tok"
	fake.mu.Unlock()
	r, err = e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false)
	if err != nil {
		t.Fatal(err)
	}
	if st := e.repoState(p.Key, r.ID); st.HookState != hookFailed || !strings.Contains(st.HookNote, "Neither of the forge's tokens (API and bot) can manage webhooks") {
		t.Errorf("neither token: %+v", st)
	}
}

func TestRepoHookRetryAccess(t *testing.T) {
	e := newEnv(t)
	_, srv := newHookForge(t, "github")
	f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("github"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	p := e.project(e.admin, "Alpha", "kanban")
	other := e.project(e.admin, "Other", "kanban")
	r, _ := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false)
	bob := e.user("bob", false)
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "bob", "editor"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RetryRepoHook(e.ctx, bob, p.Key, r.ID); err == nil {
		t.Error("an editor (not owner) retried the webhook")
	}
	if err := e.s.RetryRepoHook(e.ctx, e.admin, other.Key, r.ID); err == nil {
		t.Error("retried a repo through another project")
	}
}

func TestRepoHookDeliveryNoted(t *testing.T) {
	e := newEnv(t)
	_, srv := newHookForge(t, "github")
	f, _ := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("github"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
	p := e.project(e.admin, "Alpha", "kanban")
	r, _ := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "Acme/App", false)
	if st := e.repoState(p.Key, r.ID); st.HookSeenAt != nil {
		t.Fatal("seen before any delivery")
	}
	body := []byte(`{"zen":"hi","repository":{"full_name":"acme/app","owner":{"type":"User"}}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-GitHub-Event", "ping")
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if err := e.s.HandleForgeWebhook(e.ctx, f.ID, h, body); err != nil {
		t.Fatal(err)
	}
	if st := e.repoState(p.Key, r.ID); st.HookSeenAt == nil {
		t.Error("a signed delivery wasn't noted")
	}
}

func TestHookReachNote(t *testing.T) {
	gh := &Forge{Kind: "github", BaseURL: "https://github.com"}
	lan := &Forge{Kind: "forgejo", BaseURL: "https://git.home.lan"}
	for _, c := range []struct {
		f    *Forge
		base string
		want string
	}{
		{gh, "http://localhost:8080", "only works on this machine"},
		{gh, "http://192.168.1.5:21200", "looks private"},
		{gh, "http://waypoint:8080", "looks private"},
		{gh, "https://waypoint.example.com", ""},
		{lan, "http://192.168.1.5:21200", ""},
		{lan, "http://127.0.0.1:8080", "only works on this machine"},
		{&Forge{Kind: "forgejo", BaseURL: "http://127.0.0.1:3000"}, "http://localhost:8080", ""},
	} {
		got := hookReachNote(c.f, c.base)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s → %s: %q, want %q", c.f.BaseURL, c.base, got, c.want)
		}
	}
}

func TestHookProblemsWaitOnOwners(t *testing.T) {
	e := newEnv(t)
	fake, srv := newHookForge(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("f"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
	sam, viv := e.user("sam", false), e.user("viv", false)
	alpha, beta := e.project(e.admin, "Alpha", "kanban"), e.project(e.admin, "Beta", "kanban")
	if err := e.s.SetMember(e.ctx, e.admin, alpha.Key, "sam", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetMember(e.ctx, e.admin, alpha.Key, "viv", "editor"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.refuse["acme/broken"] = 403
	fake.mu.Unlock()
	broken, _ := e.s.AddProjectRepo(e.ctx, e.admin, alpha.Key, f.ID, "acme/broken", false)
	quiet, _ := e.s.AddProjectRepo(e.ctx, e.admin, alpha.Key, f.ID, "acme/quiet", false)
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, beta.Key, f.ID, "acme/broken", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, alpha.Key, f.ID, "acme/fine", false); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	body := []byte(`{"repository":{"full_name":"acme/fine"}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Gitea-Event", "push")
	h.Set("X-Forgejo-Event", "push")
	h.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
	h.Set("X-Forgejo-Signature", hex.EncodeToString(mac.Sum(nil)))
	if err := e.s.HandleForgeWebhook(e.ctx, f.ID, h, body); err != nil {
		t.Fatal(err)
	}
	repos := func(w *Waiting) string {
		var out []string
		for _, p := range w.Hooks {
			out = append(out, p.Project+":"+p.Repo+":"+p.State)
		}
		return strings.Join(out, " ")
	}
	w, err := e.s.WaitingOnYou(e.ctx, sam)
	if err != nil {
		t.Fatal(err)
	}
	if got := repos(w); got != alpha.Key+":acme/broken:failed" || w.Total != 1 || w.Hooks[0].RepoID != broken.ID || !strings.Contains(w.Hooks[0].Note, "write:repository") {
		t.Fatalf("at once: %q total %d %+v", got, w.Total, w.Hooks)
	}
	e.setClock(e.clockNow().Add(hookSilentAfter + time.Minute))
	if w, _ = e.s.WaitingOnYou(e.ctx, sam); repos(w) != alpha.Key+":acme/broken:failed "+alpha.Key+":acme/quiet:silent" || w.Total != 1 || w.Hooks[1].RepoID != quiet.ID {
		t.Errorf("later: %q total %d", repos(w), w.Total)
	}
	if w, _ = e.s.WaitingOnYou(e.ctx, viv); len(w.Hooks) != 0 || w.Total != 0 {
		t.Errorf("editor: %+v", w)
	}
	if w, _ = e.s.WaitingOnYou(e.ctx, e.admin); repos(w) != alpha.Key+":acme/broken:failed "+alpha.Key+":acme/quiet:silent" {
		t.Errorf("admin: %q", repos(w))
	}
	if w, _ = e.s.waitingOnYou(e.ctx, e.admin, beta.Key, ""); repos(w) != beta.Key+":acme/broken:failed" {
		t.Errorf("beta: %q", repos(w))
	}
	if w, _ = e.s.waitingOnYou(e.ctx, e.admin, "", "claude"); len(w.Hooks) != 0 {
		t.Errorf("agent narrowed: %q", repos(w))
	}
	fake.mu.Lock()
	delete(fake.refuse, "acme/broken")
	fake.mu.Unlock()
	if err := e.s.RetryRepoHook(e.ctx, e.admin, alpha.Key, broken.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if w, _ = e.s.WaitingOnYou(e.ctx, sam); repos(w) != alpha.Key+":acme/quiet:silent" {
		t.Errorf("retried, no delivery yet: %q", repos(w))
	}
}
