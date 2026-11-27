package core

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func (e *env) syncedRepo(f *Forge, project, repo string) int64 {
	e.t.Helper()
	r, err := e.s.AddProjectRepo(e.ctx, e.admin, project, f.ID, repo, true)
	if err != nil {
		e.t.Fatal(err)
	}
	e.s.Wait()
	return r.ID
}

func ghIssue(repo string, n int, title, body, state string) map[string]any {
	return map[string]any{"action": "opened", "repository": map[string]any{"full_name": repo},
		"issue": map[string]any{"number": n, "title": title, "body": body, "state": state,
			"html_url": fmt.Sprintf("https://github.com/%s/issues/%d", repo, n), "user": map[string]any{"login": "me"}}}
}

func ghComment(repo string, n, id int, action, body, who string) map[string]any {
	return map[string]any{"action": action, "repository": map[string]any{"full_name": repo},
		"issue":   map[string]any{"number": n, "html_url": fmt.Sprintf("https://github.com/%s/issues/%d", repo, n)},
		"comment": map[string]any{"id": id, "body": body, "user": map[string]any{"login": who}}}
}

// A forge issue that mentions a Waypoint issue isn't it: no text, state or
// comments flow either way, and a "follow-up to" in its body is a mention,
// not a link (WP-379).
func TestForgeMentionIsNotAMirror(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("github", "", false)
	p := e.project(e.admin, "Web", "")
	native := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Native title", Description: "native description"})
	e.syncedRepo(f, p.Key, "me/web")
	if err := e.hook(f, "issues", ghIssue("me/web", 7, "Unrelated GitHub title", "Follow-up to "+native.Key, "open")); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	got := e.get(e.admin, native.Key)
	if got.Title != "Native title" || got.Description != "native description" {
		t.Errorf("mention overwrote the issue: %q / %q", got.Title, got.Description)
	}
	if len(got.Links) != 0 {
		t.Errorf("a follow-up linked: %+v", got.Links)
	}
	if m := got.ForgeMentions; len(m) != 1 || m[0].Kind != "issue" || m[0].Verb != "follows" || m[0].Number == nil || *m[0].Number != 7 {
		t.Errorf("mention: %+v", m)
	}
	if keys := e.search(e.admin, "project:"+p.Key); len(keys) != 2 {
		t.Errorf("the mentioning issue wasn't imported on its own: %v", keys)
	}
	if _, err := e.s.AddComment(e.ctx, e.admin, native.Key, "INTERNAL: customer threatening to churn"); err != nil {
		t.Fatal(err)
	}
	done := CatDone
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, native.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	if err := e.hook(f, "issue_comment", ghComment("me/web", 7, 55, "created", "Random outsider comment", "eve")); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	mem.mu.Lock()
	closed := mem.closed[mk(f, "me/web", 7)]
	mem.mu.Unlock()
	if posted := mem.postedTo(f); len(posted) != 0 || closed {
		t.Errorf("native issue reached the mentioning issue: posted %v, closed %v", posted, closed)
	}
	if cs := e.comments(native.Key); len(cs) != 1 {
		t.Errorf("outsider comment landed on the native issue: %d comments", len(cs))
	}
}

// The same repo synced into two projects: both get the forge's comments.
func TestForgeCommentReachesEveryProject(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("github", "", false)
	a, b := e.project(e.admin, "Alpha", ""), e.project(e.admin, "Beta", "")
	e.syncedRepo(f, a.Key, "me/web")
	betaRepo := e.syncedRepo(f, b.Key, "me/web")
	if err := e.hook(f, "issues", ghIssue("me/web", 3, "Shared", "", "open")); err != nil {
		t.Fatal(err)
	}
	if err := e.hook(f, "issue_comment", ghComment("me/web", 3, 77, "created", "Seen it", "bob")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Project{a, b} {
		keys := e.search(e.admin, "project:"+p.Key)
		if len(keys) != 1 || len(e.comments(keys[0])) != 1 {
			t.Errorf("%s: issues %v", p.Key, keys)
		}
	}
	mem.mu.Lock()
	mem.issues[mk(f, "me/web")] = []ForgeItem{{Number: 3, Title: "Shared", State: "open", URL: "https://github.com/me/web/issues/3"}}
	mem.comments[mk(f, "me/web", 3)] = []ForgeComment{{ID: "77", Author: "bob", Body: "Seen it"}}
	mem.mu.Unlock()
	if err := e.s.SyncRepo(e.ctx, betaRepo); err != nil {
		t.Errorf("beta sync: %v", err)
	}
}

// Webhooks racing syncs make one task per forge issue and one project per
// repo or organisation.
func TestForgeConcurrentImports(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("github", "", false)
	p := e.project(e.admin, "Web", "")
	e.syncedRepo(f, p.Key, "me/web")
	item := ForgeItem{Number: 9, Title: "Racy", State: "open", URL: "https://github.com/me/web/issues/9"}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if err := e.s.importForgeIssue(e.ctx, f, "me/web", item); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if keys := e.search(e.admin, "project:"+p.Key); len(keys) != 1 {
		t.Errorf("concurrent imports made %d issues", len(keys))
	}

	auto := e.forge("forgejo", "https://git.example.com", true)
	mem.groups["acme"] = true
	for _, repo := range []string{"acme/a", "acme/b", "acme/a", "acme/c", "acme/b"} {
		wg.Go(func() {
			if err := e.s.ensureRepoProject(e.ctx, auto, repo, 0, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	e.s.Wait()
	var acme, attached int
	e.s.db.R.QueryRow(`SELECT count(*) FROM projects WHERE name = 'acme'`).Scan(&acme)
	e.s.db.R.QueryRow(`SELECT count(*) FROM project_repos WHERE forge_id = ?`, auto.ID).Scan(&attached)
	if acme != 1 || attached != 3 {
		t.Errorf("acme projects %d (want 1), attachments %d (want 3)", acme, attached)
	}
}

// Waypoint comment edits and deletes reach the forge copies Waypoint
// posted; a deleted forge comment doesn't come back on the next sync.
func TestForgeCommentEditsAndDeletes(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("github", "", false)
	p := e.project(e.admin, "Web", "")
	repo := e.syncedRepo(f, p.Key, "me/web")
	if err := e.hook(f, "issues", ghIssue("me/web", 3, "Login loops", "", "open")); err != nil {
		t.Fatal(err)
	}
	key := e.search(e.admin, "project:"+p.Key)[0]
	c, err := e.s.AddComment(e.ctx, e.admin, key, "oops, wrong issue")
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	forgeCopy := func() []ForgeComment {
		mem.mu.Lock()
		defer mem.mu.Unlock()
		return append([]ForgeComment(nil), mem.comments[mk(f, "me/web", 3)]...)
	}
	if err := e.s.UpdateComment(e.ctx, e.admin, c.ID, "the right text"); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if fc := forgeCopy(); len(fc) != 1 || !strings.Contains(fc[0].Body, "the right text") {
		t.Errorf("edit didn't reach the forge: %+v", fc)
	}
	if err := e.s.DeleteComment(e.ctx, e.admin, c.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if fc := forgeCopy(); len(fc) != 0 {
		t.Errorf("delete didn't reach the forge: %+v", fc)
	}

	if err := e.hook(f, "issue_comment", ghComment("me/web", 3, 501, "created", "pasted a secret", "bob")); err != nil {
		t.Fatal(err)
	}
	cs := e.comments(key)
	if len(cs) != 1 {
		t.Fatalf("comments %+v", cs)
	}
	if err := e.s.DeleteComment(e.ctx, e.admin, cs[0].ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	mem.mu.Lock()
	mem.issues[mk(f, "me/web")] = []ForgeItem{{Number: 3, Title: "Login loops", State: "open", URL: "https://github.com/me/web/issues/3"}}
	mem.comments[mk(f, "me/web", 3)] = []ForgeComment{{ID: "501", Author: "bob", Body: "pasted a secret"}, {ID: "502", Author: "carol", Body: "new one"}}
	mem.mu.Unlock()
	if err := e.s.SyncRepo(e.ctx, repo); err != nil {
		t.Fatal(err)
	}
	if cs := e.comments(key); len(cs) != 1 || cs[0].Body != "new one" {
		t.Errorf("after sync: %+v", cs)
	}
}

// Pasted links only use the forge's token for repos attached to the
// project, and never keep a confidential GitLab issue's title.
func TestForgeLinkRefreshScope(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("gitlab", "", false)
	p := e.project(e.admin, "App", "")
	mem.issues[mk(f, "group/secret")] = []ForgeItem{{Number: 3, Title: "Acquisition of ExampleCo", State: "open"}}
	mem.issues[mk(f, "group/app")] = []ForgeItem{{Number: 4, Title: "Hidden", State: "open", Confidential: true}}
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Mine"})
	if _, err := e.s.AddLink(e.ctx, e.admin, i.Key, "https://gitlab.com/group/secret/-/issues/3", ""); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	mem.mu.Lock()
	tokens := append([]string(nil), mem.tokens...)
	mem.mu.Unlock()
	if len(tokens) != 1 || tokens[0] != "" {
		t.Errorf("unattached repo fetched with the token: %q", tokens)
	}
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "group/app", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddLink(e.ctx, e.admin, i.Key, "https://gitlab.com/group/app/-/issues/4", ""); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	for _, l := range e.get(e.admin, i.Key).Links {
		if strings.Contains(l.URL, "group/app") && l.Title != "" {
			t.Errorf("confidential title stored: %q", l.Title)
		}
	}
}

// A repo detached from an automatic project stays detached.
func TestForgeDetachedRepoStaysDetached(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("github", "", true)
	mem.repos[f.ID] = []ForgeRepo{{Path: "me/web"}}
	if n, err := e.s.DiscoverRepos(e.ctx, e.admin, f.ID, nil); err != nil || n != 1 {
		t.Fatalf("discover = %d %v", n, err)
	}
	e.s.Wait()
	var key string
	var id int64
	e.s.db.R.QueryRow(`SELECT p.key, r.id FROM project_repos r JOIN projects p ON p.id = r.project_id`).Scan(&key, &id)
	if err := e.s.RemoveProjectRepo(e.ctx, e.admin, key, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.s.DiscoverRepos(e.ctx, e.admin, f.ID, nil); n != 0 {
		t.Errorf("rediscovery re-attached it: %d", n)
	}
	if err := e.hook(f, "ping", map[string]any{"repository": map[string]any{"full_name": "me/web"}}); err != nil {
		t.Fatal(err)
	}
	var projects int
	e.s.db.R.QueryRow(`SELECT count(*) FROM projects`).Scan(&projects)
	if projects != 1 {
		t.Errorf("webhook made another project: %d", projects)
	}
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, key, f.ID, "me/web", true); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	var ignored int
	e.s.db.R.QueryRow(`SELECT count(*) FROM forge_repo_ignores`).Scan(&ignored)
	if ignored != 0 {
		t.Errorf("ignore kept after attaching by hand")
	}
}

// Discovery can be previewed and narrowed; skipped repos, and the repos of
// a deleted project, aren't attached again.
func TestForgeDiscoverPickAndDelete(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("github", "", true)
	mem.repos[f.ID] = []ForgeRepo{{Path: "acme/web", Group: true}, {Path: "acme/api", Group: true}, {Path: "me/notes"}}
	mem.groups["acme"] = true
	preview, err := e.s.PreviewDiscover(e.ctx, e.admin, f.ID)
	if err != nil || len(preview) != 3 {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	if p := preview[0]; p.Repo != "acme/web" || p.Namespace != "acme" || !p.NewNamespace || p.Joins != "" {
		t.Errorf("preview[0] = %+v", p)
	}
	if p := preview[2]; p.Namespace != "" || p.NewNamespace {
		t.Errorf("preview[2] = %+v", p)
	}
	var projects int
	if _, err := e.s.PreviewDiscover(e.ctx, e.user("sam", false), f.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("member preview: %v", err)
	}
	if e.s.db.R.QueryRow(`SELECT count(*) FROM projects`).Scan(&projects); projects != 0 {
		t.Fatalf("preview made %d projects", projects)
	}
	n, err := e.s.DiscoverRepos(e.ctx, e.admin, f.ID, &RepoPick{Repos: []string{"ACME/web", "evil/x"}, Skip: []string{"me/notes", "evil/y"}})
	e.s.Wait()
	if err != nil || n != 1 {
		t.Fatalf("pick = %d, %v", n, err)
	}
	if e.s.db.R.QueryRow(`SELECT count(*) FROM projects`).Scan(&projects); projects != 2 {
		t.Errorf("projects = %d, want acme and web", projects)
	}
	var ignores int
	if e.s.db.R.QueryRow(`SELECT count(*) FROM forge_repo_ignores`).Scan(&ignores); ignores != 1 {
		t.Errorf("ignores = %d, want me/notes", ignores)
	}
	if preview, _ = e.s.PreviewDiscover(e.ctx, e.admin, f.ID); len(preview) != 1 || preview[0].Repo != "acme/api" || preview[0].NewNamespace {
		t.Errorf("preview after = %+v", preview)
	}
	var key string
	e.s.db.R.QueryRow(`SELECT p.key FROM projects p JOIN project_repos r ON r.project_id = p.id WHERE r.repo = 'acme/web'`).Scan(&key)
	if err := e.s.DeleteProject(e.ctx, e.admin, key); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if n, _ := e.s.DiscoverRepos(e.ctx, e.admin, f.ID, nil); n != 1 {
		t.Errorf("rediscovery attached %d, want only acme/api", n)
	}
	e.s.db.R.QueryRow(`SELECT count(*) FROM project_repos WHERE repo = 'acme/web'`).Scan(&n)
	if n != 0 {
		t.Errorf("deleted project's repo came back")
	}
}

// Deleting a hand-made project ignores its repos too, unless another project
// still has them; attaching one by hand lifts the ignore.
func TestForgeDeleteHandMadeProjectIgnoresRepos(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("github", "", false)
	mem.repos[f.ID] = []ForgeRepo{{Path: "me/web"}, {Path: "me/shared"}}
	ignores := func() (n int) {
		e.s.db.R.QueryRow(`SELECT count(*) FROM forge_repo_ignores`).Scan(&n)
		return n
	}
	a, b, c := e.project(e.admin, "Alpha", "kanban"), e.project(e.admin, "Beta", "kanban"), e.project(e.admin, "Gamma", "kanban")
	for _, add := range []struct {
		p    *Project
		repo string
	}{{a, "me/web"}, {a, "me/shared"}, {b, "me/shared"}} {
		if _, err := e.s.AddProjectRepo(e.ctx, e.admin, add.p.Key, f.ID, add.repo, true); err != nil {
			t.Fatal(err)
		}
	}
	e.s.Wait()
	if err := e.s.DeleteProject(e.ctx, e.admin, a.Key); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	var repo string
	if err := e.s.db.R.QueryRow(`SELECT repo FROM forge_repo_ignores`).Scan(&repo); err != nil || repo != "me/web" || ignores() != 1 {
		t.Fatalf("ignores = %d (%q, %v), want only me/web (me/shared is still on Beta)", ignores(), repo, err)
	}
	preview, err := e.s.PreviewDiscover(e.ctx, e.admin, f.ID)
	if err != nil || len(preview) != 0 {
		t.Errorf("preview offers %+v, %v", preview, err)
	}
	if n, err := e.s.DiscoverRepos(e.ctx, e.admin, f.ID, nil); err != nil || n != 0 {
		t.Errorf("discover attached %d, %v", n, err)
	}
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, c.Key, f.ID, "me/web", true); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if ignores() != 0 {
		t.Errorf("ignore kept after attaching by hand")
	}
}

// A GitLab issue made confidential is emptied here, and filled back in if
// it becomes public again.
func TestGitLabIssueMadeConfidential(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	f := e.forge("gitlab", "", false)
	p := e.project(e.admin, "App", "")
	e.syncedRepo(f, p.Key, "grp/app")
	secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
	send := func(payload string) {
		t.Helper()
		h := http.Header{}
		h.Set("X-Gitlab-Token", secret)
		if err := e.s.HandleForgeWebhook(e.ctx, f.ID, h, []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	issue := func(conf bool) string {
		return fmt.Sprintf(`{"object_kind":"issue","user":{"username":"me"},"project":{"path_with_namespace":"grp/app"},
			"object_attributes":{"iid":4,"title":"Public bug","description":"public desc","state":"opened","confidential":%v,
			"url":"https://gitlab.com/grp/app/-/issues/4"}}`, conf)
	}
	send(issue(false))
	send(`{"object_kind":"note","user":{"username":"alice"},"project":{"path_with_namespace":"grp/app"},
		"object_attributes":{"id":77,"note":"details","noteable_type":"Issue"},"issue":{"iid":4}}`)
	key := e.search(e.admin, "project:"+p.Key)[0]
	if len(e.comments(key)) != 1 {
		t.Fatal("note not imported")
	}
	send(issue(true))
	got := e.get(e.admin, key)
	if strings.Contains(got.Title, "Public bug") || strings.Contains(got.Description, "public desc") || len(e.comments(key)) != 0 {
		t.Errorf("confidential issue still shown: %q %q, %d comments", got.Title, got.Description, len(e.comments(key)))
	}
	send(issue(false))
	if got := e.get(e.admin, key); got.Title != "Public bug" {
		t.Errorf("not restored when public again: %q", got.Title)
	}
}
