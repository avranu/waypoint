package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// memForge is an in-memory forge for every kind: issues, comments, repos.
type memForge struct {
	mu       sync.Mutex
	issues   map[string][]ForgeItem    // forgeID|repo
	comments map[string][]ForgeComment // forgeID|repo|number
	closed   map[string]bool           // forgeID|repo|number
	repos    map[int64][]ForgeRepo
	groups   map[string]bool
	posted   []string // bodies Waypoint posted
	tokens   []string // the token each GetItem used
	nextID   int
}

func newMemForge() *memForge {
	return &memForge{issues: map[string][]ForgeItem{}, comments: map[string][]ForgeComment{}, closed: map[string]bool{},
		repos: map[int64][]ForgeRepo{}, groups: map[string]bool{}}
}

func mk(f *Forge, repo string, n ...int) string {
	k := fmt.Sprintf("%d|%s", f.ID, strings.ToLower(repo))
	for _, x := range n {
		k += fmt.Sprintf("|%d", x)
	}
	return k
}

func (m *memForge) GetItem(ctx context.Context, f *Forge, repo string, n int, pr bool) (*ForgeItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = append(m.tokens, f.token)
	for _, it := range m.issues[mk(f, repo)] {
		if it.Number == n {
			it.Author = cmp(it.Author, "me")
			return &it, nil
		}
	}
	return nil, errors.New("HTTP 404")
}

func (m *memForge) ListIssues(ctx context.Context, f *Forge, repo string, since time.Time) ([]ForgeItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]ForgeItem(nil), m.issues[mk(f, repo)]...)
	for n := range out {
		out[n].Author = cmp(out[n].Author, "me") // issues without one are the forge owner's
	}
	return out, nil
}

func (m *memForge) SetIssueState(ctx context.Context, f *Forge, repo string, n int, closed bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed[mk(f, repo, n)] = closed
	return nil
}

func (m *memForge) ListComments(ctx context.Context, f *Forge, repo string, n int) ([]ForgeComment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ForgeComment(nil), m.comments[mk(f, repo, n)]...), nil
}

func (m *memForge) PostComment(ctx context.Context, f *Forge, repo string, n int, body string) (*ForgeComment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	c := ForgeComment{ID: fmt.Sprintf("%d", 9000+m.nextID), Author: "waypoint-bot", Body: body}
	m.comments[mk(f, repo, n)] = append(m.comments[mk(f, repo, n)], c)
	m.posted = append(m.posted, fmt.Sprintf("%d|%s|%d|%s", f.ID, repo, n, body))
	return &c, nil
}

func (m *memForge) EditComment(ctx context.Context, f *Forge, repo string, n int, id, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.comments[mk(f, repo, n)] {
		if c.ID == id {
			m.comments[mk(f, repo, n)][i].Body = body
			return nil
		}
	}
	return errors.New("HTTP 404")
}

func (m *memForge) DeleteComment(ctx context.Context, f *Forge, repo string, n int, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.comments[mk(f, repo, n)]
	for i, c := range list {
		if c.ID == id {
			m.comments[mk(f, repo, n)] = append(list[:i:i], list[i+1:]...)
			return nil
		}
	}
	return errors.New("HTTP 404")
}

func (m *memForge) ListRepos(ctx context.Context, f *Forge) ([]ForgeRepo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ForgeRepo(nil), m.repos[f.ID]...), nil
}

func (m *memForge) IsGroup(ctx context.Context, f *Forge, ns string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.groups[strings.ToLower(ns)], nil
}

func (m *memForge) GetRepo(ctx context.Context, f *Forge, repo string) (*ForgeRepo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.repos[f.ID] {
		if strings.EqualFold(r.Path, repo) {
			return &r, nil
		}
	}
	return nil, &forgeAPIError{method: "GET", path: "/repos/" + repo, status: http.StatusNotFound}
}

func (m *memForge) RepoByID(ctx context.Context, f *Forge, id int64) (*ForgeRepo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.repos[f.ID] {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, &forgeAPIError{method: "GET", path: fmt.Sprintf("/repositories/%d", id), status: http.StatusNotFound}
}

func (m *memForge) postedTo(f *Forge) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, p := range m.posted {
		if strings.HasPrefix(p, fmt.Sprintf("%d|", f.ID)) {
			out = append(out, p)
		}
	}
	return out
}

func (e *env) forge(kind, base string, auto bool) *Forge {
	e.t.Helper()
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str(kind), BaseURL: str(base), Token: str("tok"), AutoProjects: &auto})
	if err != nil {
		e.t.Fatal(err)
	}
	// These tests' forge users (and the bots they meet) are people here;
	// outside_test.go and outside_pulls_test.go have strangers.
	e.forgePeople(f, "me", "alice", "bob", "carol", "dana", "sam", "robin", "ann", "alex", "agent", "claude", "erin", "dave", "person", "x",
		"llm-review", "waypoint-bot", "wp-bot", "bot", "copilot-reviewer", "coderabbitai[bot]", "renovate[bot]")
	return f
}

// forgePeople maps forge logins to the admin (WP-279), as logins only the
// admin writes as (WP-282).
func (e *env) forgePeople(f *Forge, logins ...string) {
	e.t.Helper()
	var in []ForgePersonInput
	for _, l := range logins {
		in = append(in, ForgePersonInput{Login: l, User: e.admin.User.Username, PersonOnly: true})
	}
	if _, err := e.s.SetForgePeople(e.ctx, e.admin, f.ID, in); err != nil {
		e.t.Fatal(err)
	}
}

// hook sends a signed GitHub/Forgejo-style webhook.
func (e *env) hook(f *Forge, event string, payload any) error {
	body, _ := json.Marshal(payload)
	secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return e.s.HandleForgeWebhook(e.ctx, f.ID, h, body)
}

func (e *env) comments(key string) []Comment {
	e.t.Helper()
	cs, err := e.s.ListComments(e.ctx, e.admin, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return cs
}

func TestGitLabForge(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("gitlab", "", false)
	if f.BaseURL != "https://gitlab.com" {
		t.Errorf("default base = %s", f.BaseURL)
	}
	p := e.project(e.admin, "App", "")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "grp/../x", false); err == nil {
		t.Error(".. accepted in a GitLab path")
	}
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "grp/sub/app", true); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
	send := func(token string, payload string) error {
		h := http.Header{}
		h.Set("X-Gitlab-Token", token)
		return e.s.HandleForgeWebhook(e.ctx, f.ID, h, []byte(payload))
	}
	issue := `{"object_kind":"issue","user":{"username":"me"},"project":{"path_with_namespace":"grp/sub/app"},
		"object_attributes":{"iid":4,"title":"Broken search","description":"It 500s","state":"opened","url":"https://gitlab.com/grp/sub/app/-/issues/4"},
		"labels":[{"title":"bug"}]}`
	if err := send("wrong", issue); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	if err := send(secret, issue); err != nil {
		t.Fatal(err)
	}
	keys := e.search(e.admin, "project:"+p.Key)
	if len(keys) != 1 {
		t.Fatalf("imported %v", keys)
	}
	got := e.get(e.admin, keys[0])
	if got.Type != "bug" || got.Title != "Broken search" || len(got.Links) != 1 || got.Links[0].Provider != "gitlab" {
		t.Errorf("imported issue: %s %q %+v", got.Type, got.Title, got.Links)
	}
	// Confidential issues stay on GitLab.
	conf := strings.Replace(strings.Replace(issue, `"iid":4`, `"iid":5,"confidential":true`, 1), "issues/4", "issues/5", 1)
	if err := send(secret, conf); err != nil {
		t.Fatal(err)
	}
	if n := len(e.search(e.admin, "project:"+p.Key)); n != 1 {
		t.Errorf("confidential issue imported: %d issues", n)
	}
	// Notes become comments by their author.
	note := `{"object_kind":"note","user":{"username":"alice"},"project":{"path_with_namespace":"grp/sub/app"},
		"object_attributes":{"id":77,"note":"Happens on page 2","noteable_type":"Issue","url":"https://gitlab.com/grp/sub/app/-/issues/4#note_77"},
		"issue":{"iid":4}}`
	if err := send(secret, note); err != nil {
		t.Fatal(err)
	}
	cs := e.comments(keys[0])
	if len(cs) != 1 || cs[0].Author == nil || cs[0].Author.DisplayName != "alice" || cs[0].Via != "GitLab" {
		t.Fatalf("note: %+v", cs)
	}
	// Merge requests mentioning the key link up.
	mr := fmt.Sprintf(`{"object_kind":"merge_request","project":{"path_with_namespace":"grp/sub/app"},
		"object_attributes":{"iid":9,"title":"Fix %s","state":"merged","url":"https://gitlab.com/grp/sub/app/-/merge_requests/9","source_branch":"fix"}}`, keys[0])
	if err := send(secret, mr); err != nil {
		t.Fatal(err)
	}
	if links := e.get(e.admin, keys[0]).Links; len(links) != 2 {
		t.Errorf("MR not linked: %+v", links)
	}
	// Links from pasted URLs.
	l, err := e.s.parseLink(e.ctx, "https://gitlab.com/grp/sub/app/-/merge_requests/3")
	if err != nil || l.Kind != "pull" || l.Repo != "grp/sub/app" || l.Number == nil || *l.Number != 3 {
		t.Errorf("parseLink = %+v %v", l, err)
	}
}

func TestForgeCommentSync(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("github", "", false)
	p := e.project(e.admin, "Web", "")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "me/web", true); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	issue := map[string]any{"action": "opened", "repository": map[string]any{"full_name": "me/web"},
		"issue": map[string]any{"number": 3, "title": "Login loops", "body": "", "state": "open", "html_url": "https://github.com/me/web/issues/3", "user": map[string]any{"login": "me"}}}
	if err := e.hook(f, "issues", issue); err != nil {
		t.Fatal(err)
	}
	key := e.search(e.admin, "project:"+p.Key)[0]
	comment := func(action string, id int, body string) map[string]any {
		return map[string]any{"action": action, "repository": map[string]any{"full_name": "me/web"},
			"issue":   map[string]any{"number": 3, "title": "Login loops", "html_url": "https://github.com/me/web/issues/3"},
			"comment": map[string]any{"id": id, "body": body, "html_url": "https://github.com/me/web/issues/3#c", "user": map[string]any{"login": "bob"}}}
	}
	// Forge → Waypoint, once, then edited.
	for range 2 {
		if err := e.hook(f, "issue_comment", comment("created", 501, "Seen on Safari")); err != nil {
			t.Fatal(err)
		}
	}
	e.s.Wait()
	if cs := e.comments(key); len(cs) != 1 || cs[0].Author.DisplayName != "bob" || cs[0].Via != "GitHub" {
		t.Fatalf("after create: %+v", cs)
	}
	if len(mem.postedTo(f)) != 0 {
		t.Error("a forge comment was posted back to its forge")
	}
	if err := e.hook(f, "issue_comment", comment("edited", 501, "Seen on Safari 18")); err != nil {
		t.Fatal(err)
	}
	if cs := e.comments(key); cs[0].Body != "Seen on Safari 18" {
		t.Errorf("edit not mirrored: %q", cs[0].Body)
	}
	// Waypoint → forge, and the echo doesn't come back as a copy.
	if _, err := e.s.AddComment(e.ctx, e.admin, key, "Fixed in the next release"); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	posted := mem.postedTo(f)
	if len(posted) != 1 || !strings.Contains(posted[0], "Fixed in the next release") || !strings.Contains(posted[0], viaWaypoint) {
		t.Fatalf("posted = %v", posted)
	}
	echo := mem.comments[mk(f, "me/web", 3)]
	if err := e.hook(f, "issue_comment", comment("created", 9001, echo[len(echo)-1].Body)); err != nil {
		t.Fatal(err)
	}
	if cs := e.comments(key); len(cs) != 2 {
		t.Errorf("echo duplicated the comment: %d comments", len(cs))
	}
	// The webhook can also beat the post's reply (no link yet): still
	// recognised as ours.
	if _, err := e.s.AddComment(e.ctx, e.admin, key, "Racing the webhook"); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if _, err := e.s.db.W.Exec(`DELETE FROM comment_links WHERE external_id = '9002'`); err != nil {
		t.Fatal(err)
	}
	if err := e.hook(f, "issue_comment", comment("created", 9002, "**robin** "+viaWaypoint+":\n\nRacing the webhook")); err != nil {
		t.Fatal(err)
	}
	if cs := e.comments(key); len(cs) != 3 {
		t.Errorf("racing echo duplicated: %d comments", len(cs))
	}
	// A comment deleted on the forge goes here too.
	if err := e.hook(f, "issue_comment", comment("deleted", 501, "")); err != nil {
		t.Fatal(err)
	}
	if cs := e.comments(key); len(cs) != 2 || cs[0].Body != "Fixed in the next release" {
		t.Errorf("after delete: %+v", cs)
	}
	// A sync pulls comments the webhooks missed, without duplicating.
	mem.mu.Lock()
	mem.issues[mk(f, "me/web")] = []ForgeItem{{Number: 3, Title: "Login loops", State: "open", URL: "https://github.com/me/web/issues/3"}}
	mem.comments[mk(f, "me/web", 3)] = append(mem.comments[mk(f, "me/web", 3)], ForgeComment{ID: "900", Author: "carol", Body: "Missed one"})
	mem.mu.Unlock()
	repos, _ := e.s.ListProjectRepos(e.ctx, e.admin, p.Key)
	for range 2 {
		if err := e.s.SyncRepo(e.ctx, repos[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	e.s.Wait()
	if cs := e.comments(key); len(cs) != 3 || cs[2].Body != "Missed one" {
		t.Errorf("after sync: %+v", cs)
	}
}

func TestForgeAutoProjectsAndMirrors(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	fj := e.forge("forgejo", "https://git.example.com", true)
	gh := e.forge("github", "", true)
	mem.repos[fj.ID] = []ForgeRepo{{Path: "acme/web", Group: true}, {Path: "robin/dotfiles"}}
	mem.groups["acme"] = true
	mem.issues[mk(fj, "acme/web")] = []ForgeItem{{Number: 5, Title: "Fix login", State: "open", URL: "https://git.example.com/acme/web/issues/5"}}
	mem.issues[mk(gh, "acme/web")] = []ForgeItem{{Number: 5, Title: "Fix login", State: "open", URL: "https://github.com/acme/web/issues/5"}}

	n, err := e.s.DiscoverRepos(e.ctx, e.admin, fj.ID, nil)
	e.s.Wait()
	if err != nil || n != 2 {
		t.Fatalf("discover = %d, %v", n, err)
	}
	if again, _ := e.s.DiscoverRepos(e.ctx, e.admin, fj.ID, nil); again != 0 {
		t.Errorf("rediscovery added %d", again)
	}
	projects, _ := e.s.ListProjects(e.ctx, e.admin, false)
	byName := map[string]*Project{}
	for _, p := range projects {
		byName[p.Name] = p
	}
	acme, web, dot := byName["acme"], byName["web"], byName["dotfiles"]
	if acme == nil || web == nil || dot == nil {
		t.Fatalf("projects = %v", byName)
	}
	if web.ParentID == nil || *web.ParentID != acme.ID || dot.ParentID != nil {
		t.Errorf("nesting: web.parent=%v dotfiles.parent=%v", web.ParentID, dot.ParentID)
	}
	if keys := e.search(e.admin, "project:"+web.Key); len(keys) != 1 {
		t.Fatalf("web issues = %v", keys)
	}
	// A webhook from a repo nobody attached yet: a new project under acme.
	if err := e.hook(fj, "ping", map[string]any{"repository": map[string]any{"full_name": "acme/api"}}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	projects, _ = e.s.ListProjects(e.ctx, e.admin, false)
	for _, p := range projects {
		if p.Name == "api" && (p.ParentID == nil || *p.ParentID != acme.ID) {
			t.Errorf("api project not under acme: %v", p.ParentID)
		}
	}

	// The GitHub mirror of acme/web joins the same project, and its copy of
	// issue #5 joins the same task.
	mem.repos[gh.ID] = []ForgeRepo{{Path: "acme/web", Group: true}}
	if n, err := e.s.DiscoverRepos(e.ctx, e.admin, gh.ID, nil); err != nil || n != 1 {
		t.Fatalf("mirror discover = %d, %v", n, err)
	}
	e.s.Wait()
	if after, _ := e.s.ListProjects(e.ctx, e.admin, false); len(after) != len(projects) {
		t.Errorf("mirror made a new project: %d → %d", len(projects), len(after))
	}
	// Same name isn't proof of the same repo: it joins with sync and
	// posting off until a project owner turns them on.
	keys := e.search(e.admin, "project:"+web.Key)
	if links := e.get(e.admin, keys[0]).Links; len(keys) != 1 || len(links) != 1 {
		t.Fatalf("mirror synced before anyone said so: %v, %d links", keys, len(links))
	}
	repos, _ := e.s.ListProjectRepos(e.ctx, e.admin, web.Key)
	on := true
	for _, r := range repos {
		if r.ForgeID != gh.ID {
			continue
		}
		if r.SyncIssues || r.PostComments || r.Bots {
			t.Errorf("mirror joined with sync=%v post=%v bots=%v", r.SyncIssues, r.PostComments, r.Bots)
		}
		if err := e.s.UpdateProjectRepo(e.ctx, e.admin, web.Key, r.ID, &on, &on, nil); err != nil {
			t.Fatal(err)
		}
		if err := e.s.SyncRepo(e.ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	keys = e.search(e.admin, "project:"+web.Key)
	if len(keys) != 1 {
		t.Fatalf("mirror issue imported twice: %v", keys)
	}
	if links := e.get(e.admin, keys[0]).Links; len(links) != 2 {
		t.Errorf("task has %d links, want both forges", len(links))
	}

	// Closing on GitHub closes the task and the Forgejo copy.
	closed := map[string]any{"action": "closed", "repository": map[string]any{"full_name": "acme/web"},
		"issue": map[string]any{"number": 5, "title": "Fix login", "state": "closed", "html_url": "https://github.com/acme/web/issues/5", "user": map[string]any{"login": "me"}}}
	if err := e.hook(gh, "issues", closed); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if st := e.get(e.admin, keys[0]).Status.Category; st != CatDone {
		t.Errorf("task status = %s", st)
	}
	mem.mu.Lock()
	fjClosed, ghPushed := mem.closed[mk(fj, "acme/web", 5)], mem.closed[mk(gh, "acme/web", 5)]
	mem.mu.Unlock()
	if !fjClosed || ghPushed {
		t.Errorf("forgejo closed=%v, pushed back to github=%v", fjClosed, ghPushed)
	}
	// A GitHub comment reaches the Forgejo copy, not GitHub again.
	if err := e.hook(gh, "issue_comment", map[string]any{"action": "created", "repository": map[string]any{"full_name": "acme/web"},
		"issue":   map[string]any{"number": 5, "title": "Fix login"},
		"comment": map[string]any{"id": 42, "body": "Done here", "user": map[string]any{"login": "dana"}}}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if len(mem.postedTo(gh)) != 0 || len(mem.postedTo(fj)) != 1 {
		t.Errorf("posted gh=%v fj=%v", mem.postedTo(gh), mem.postedTo(fj))
	}
}
