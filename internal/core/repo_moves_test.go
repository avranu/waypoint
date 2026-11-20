package core

import (
	"strings"
	"testing"
)

// projectNamed finds a project by name; nil when there's none.
func (e *env) projectNamed(name string) *Project {
	e.t.Helper()
	projects, err := e.s.ListProjects(e.ctx, e.admin, false)
	if err != nil {
		e.t.Fatal(err)
	}
	var found *Project
	for _, p := range projects {
		if p.Name == name {
			if found != nil {
				e.t.Fatalf("two projects named %s", name)
			}
			found = p
		}
	}
	return found
}

func (e *env) repoNames(key string) []string {
	e.t.Helper()
	repos, err := e.s.ListProjectRepos(e.ctx, e.admin, key)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, r := range repos {
		out = append(out, r.Repo)
	}
	return out
}

func TestReposFollowedAcrossRenamesAndTransfers(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	fj := e.forge("forgejo", "https://git.example.com", true)
	mem.groups["acme"], mem.groups["ops"] = true, true
	mem.repos[fj.ID] = []ForgeRepo{{ID: 7, Path: "acme/infra", Group: true}}
	mem.issues[mk(fj, "acme/infra")] = []ForgeItem{{Number: 1, Title: "Rotate the CA", State: "open", URL: "https://git.example.com/acme/infra/issues/1"}}
	if _, err := e.s.DiscoverRepos(e.ctx, e.admin, fj.ID, nil); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	infra, acme := e.projectNamed("infra"), e.projectNamed("acme")
	if infra == nil || acme == nil || infra.ParentID == nil || *infra.ParentID != acme.ID {
		t.Fatalf("infra = %+v under acme %+v", infra, acme)
	}
	keys := e.search(e.admin, "project:"+infra.Key)
	if len(keys) != 1 {
		t.Fatalf("imported %v", keys)
	}

	if _, err := e.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, url, head_repo, created_at, updated_at) VALUES (?,?,?,?,?,1,1)`,
		fj.ID, "acme/infra", 3, "https://git.example.com/acme/infra/pulls/3", "acme/infra"); err != nil {
		t.Fatal(err)
	}

	// Transferred to another organisation: the forge reports the same id
	// under the new name. The project follows; nothing is made twice.
	mem.mu.Lock()
	mem.repos[fj.ID] = []ForgeRepo{{ID: 7, Path: "ops/infra", Group: true}}
	mem.issues[mk(fj, "ops/infra")] = []ForgeItem{{Number: 1, Title: "Rotate the CA", State: "open", URL: "https://git.example.com/ops/infra/issues/1"},
		{Number: 2, Title: "New after the move", State: "open", URL: "https://git.example.com/ops/infra/issues/2"}}
	delete(mem.issues, mk(fj, "acme/infra"))
	mem.mu.Unlock()
	e.s.SyncAllRepos(e.ctx)
	e.s.Wait()
	if got := e.repoNames(infra.Key); len(got) != 1 || got[0] != "ops/infra" {
		t.Fatalf("repos after the transfer = %v", got)
	}
	if p := e.projectNamed("infra"); p == nil || p.ID != infra.ID {
		t.Fatalf("a second infra project: %+v", p)
	}
	ops := e.projectNamed("ops")
	if p := e.projectNamed("infra"); ops == nil || p.ParentID == nil || *p.ParentID != ops.ID {
		t.Errorf("infra didn't follow its repo to ops: %+v", p.ParentID)
	}
	keys = e.search(e.admin, "project:"+infra.Key)
	if len(keys) != 2 {
		t.Fatalf("issues after the move = %v, want the old one and #2", keys)
	}
	for _, k := range keys {
		for _, l := range e.get(e.admin, k).Links {
			if l.Repo != "ops/infra" || !strings.HasPrefix(l.URL, "https://git.example.com/ops/infra/issues/") {
				t.Errorf("%s link %s %s", k, l.Repo, l.URL)
			}
		}
	}

	var prRepo, prURL, headRepo string
	if err := e.s.db.R.QueryRow(`SELECT repo, url, head_repo FROM pull_requests WHERE number = 3`).Scan(&prRepo, &prURL, &headRepo); err != nil ||
		prRepo != "ops/infra" || prURL != "https://git.example.com/ops/infra/pulls/3" || headRepo != "ops/infra" {
		t.Errorf("pull request after the move: %s %s %s %v", prRepo, prURL, headRepo, err)
	}

	// Renamed again, and only a webhook says so.
	if err := e.hook(fj, "ping", map[string]any{"repository": map[string]any{"id": 7, "full_name": "ops/infrastructure"}}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if got := e.repoNames(infra.Key); len(got) != 1 || got[0] != "ops/infrastructure" {
		t.Fatalf("repos after the rename = %v", got)
	}

	// A new repo takes the old name: it's another repo, with a project of
	// its own; the moved one keeps its issues.
	mem.mu.Lock()
	mem.repos[fj.ID] = []ForgeRepo{{ID: 7, Path: "ops/infrastructure", Group: true}, {ID: 9, Path: "acme/infra", Group: true}}
	mem.issues[mk(fj, "acme/infra")] = []ForgeItem{{Number: 1, Title: "Fresh start", State: "open", URL: "https://git.example.com/acme/infra/issues/1"}}
	mem.mu.Unlock()
	e.s.SyncAllRepos(e.ctx)
	e.s.Wait()
	if got := e.repoNames(infra.Key); len(got) != 1 || got[0] != "ops/infrastructure" {
		t.Errorf("moved repo's project now has %v", got)
	}
	projects, _ := e.s.ListProjects(e.ctx, e.admin, false)
	var fresh *Project
	for _, p := range projects {
		if p.Name == "infra" && p.ID != infra.ID {
			fresh = p
		}
	}
	if fresh == nil || fresh.ParentID == nil || *fresh.ParentID != acme.ID {
		t.Fatalf("the new acme/infra has no project of its own under acme: %+v", fresh)
	}
	if k := e.search(e.admin, "project:"+fresh.Key); len(k) != 1 || e.get(e.admin, k[0]).Title != "Fresh start" {
		t.Errorf("new repo's issues = %v", k)
	}
	if k := e.search(e.admin, "project:"+infra.Key); len(k) != 2 {
		t.Errorf("moved repo's issues = %v", k)
	}
}

// A repo attached before Waypoint knew its id moved; discovery took it for
// a new repo. A person's move puts it right.
func TestMoveProjectRepoTakesOverTheDuplicate(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	fj := e.forge("forgejo", "https://git.example.com", true)
	mem.groups["acme"], mem.groups["ops"] = true, true
	mem.issues[mk(fj, "acme/infra")] = []ForgeItem{{Number: 1, Title: "Rotate the CA", State: "open", URL: "https://git.example.com/acme/infra/issues/1"}}
	mine := e.project(e.admin, "Infrastructure", "")
	r, err := e.s.AddProjectRepo(e.ctx, e.admin, mine.Key, fj.ID, "acme/infra", true)
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	old := e.search(e.admin, "project:"+mine.Key)
	if len(old) != 1 {
		t.Fatalf("imported %v", old)
	}

	mem.mu.Lock()
	mem.repos[fj.ID] = []ForgeRepo{{ID: 7, Path: "ops/infra", Group: true}, {ID: 8, Path: "ops/other", Group: true}}
	mem.issues[mk(fj, "ops/infra")] = []ForgeItem{{Number: 1, Title: "Rotate the CA", State: "open", URL: "https://git.example.com/ops/infra/issues/1"}}
	delete(mem.issues, mk(fj, "acme/infra"))
	mem.mu.Unlock()
	if _, err := e.s.DiscoverRepos(e.ctx, e.admin, fj.ID, nil); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	dup := e.projectNamed("infra")
	if dup == nil || len(e.search(e.admin, "project:"+dup.Key)) != 1 {
		t.Fatalf("expected the duplicate project discovery makes without an id: %+v", dup)
	}

	if _, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "ops/nope", MoveOptions{}); err == nil {
		t.Error("moved to a repo the forge doesn't have")
	}
	if _, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "acme/infra", MoveOptions{}); err == nil {
		t.Error("moved to its own name")
	}
	got, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "ops/infra", MoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if got.Repo != "ops/infra" {
		t.Errorf("moved = %+v", got)
	}
	if names := e.repoNames(dup.Key); len(names) != 0 {
		t.Errorf("duplicate still attached: %v", names)
	}
	// Its issue's link has the new name, so syncing finds it, not a twin.
	if err := e.s.SyncRepo(e.ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if keys := e.search(e.admin, "project:"+mine.Key); len(keys) != 1 || keys[0] != old[0] {
		t.Errorf("issues after the move = %v, want %v", keys, old)
	}
	if l := e.get(e.admin, old[0]).Links; len(l) != 1 || l[0].URL != "https://git.example.com/ops/infra/issues/1" || !l[0].Mirror {
		t.Errorf("links = %+v", l)
	}
	// A person put the project at the top: it stays there.
	if p, _ := e.s.projectByID(e.ctx, e.s.db.R, mine.ID); p.ParentID != nil {
		t.Errorf("a person's project was moved: parent %d", *p.ParentID)
	}
	// It knows the repo's id now: another repo isn't it.
	if _, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "ops/other", MoveOptions{}); err == nil || !strings.Contains(err.Error(), "another repository") {
		t.Errorf("moved onto another repo: %v", err)
	}
	// Discovery doesn't make the duplicate again.
	if n, err := e.s.DiscoverRepos(e.ctx, e.admin, fj.ID, nil); err != nil || n != 0 {
		t.Errorf("rediscover = %d, %v", n, err)
	}
	e.s.Wait()
	if names := e.repoNames(dup.Key); len(names) != 0 {
		t.Errorf("duplicate attached again: %v", names)
	}
}

// A repo deleted and made again, or replaced by another, has a new forge id:
// following can't tell it is the same, so a person relinks the project (WP-154).
func TestRelinkProjectRepo(t *testing.T) {
	e := newEnv(t)
	mem := newMemForge()
	e.s.Forges = mem
	fj := e.forge("forgejo", "https://git.example.com", true)
	mem.repos[fj.ID] = []ForgeRepo{{ID: 7, Path: "acme/old"}, {ID: 8, Path: "acme/new"}, {ID: 9, Path: "acme/other"}}
	mem.issues[mk(fj, "acme/old")] = []ForgeItem{{Number: 1, Title: "Rotate the CA", State: "open", URL: "https://git.example.com/acme/old/issues/1"}}
	mine := e.project(e.admin, "Mine", "")
	r, err := e.s.AddProjectRepo(e.ctx, e.admin, mine.Key, fj.ID, "acme/old", true)
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if _, err := e.s.db.W.Exec(`UPDATE project_repos SET forge_repo_id = 7 WHERE id = ?`, r.ID); err != nil {
		t.Fatal(err)
	}
	issues := e.search(e.admin, "project:"+mine.Key)
	if len(issues) != 1 {
		t.Fatalf("imported %v", issues)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO pull_requests (forge_id, repo, number, title, url, state, head_ref, base_ref, created_at, updated_at)
		VALUES (?, 'acme/old', 4, 'Old work', 'https://git.example.com/acme/old/pulls/4', 'merged', 'x', 'main', 1, 1)`, fj.ID); err != nil {
		t.Fatal(err)
	}

	// A move follows one repo; another repository needs a relink.
	if _, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "acme/new", MoveOptions{}); err == nil || !strings.Contains(err.Error(), "relink") {
		t.Fatalf("moved onto another repository: %v", err)
	}
	if _, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "acme/nope", MoveOptions{Relink: true}); err == nil {
		t.Error("relinked to a repo the forge doesn't have")
	}
	// Another project links the new repo: refused unless forced.
	theirs := e.project(e.admin, "Theirs", "")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, theirs.Key, fj.ID, "acme/new", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "acme/new", MoveOptions{Relink: true}); err == nil || !strings.Contains(err.Error(), "linked by project "+theirs.Key) {
		t.Fatalf("relinked onto another project's repo: %v", err)
	}
	if names := e.repoNames(mine.Key); len(names) != 1 || names[0] != "acme/old" {
		t.Fatalf("a refused relink changed %v", names)
	}
	got, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "acme/new", MoveOptions{Relink: true, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if got.Repo != "acme/new" || got.ID != r.ID {
		t.Errorf("relinked = %+v", got)
	}
	if names := e.repoNames(theirs.Key); len(names) != 0 {
		t.Errorf("the other project still links it: %v", names)
	}
	var known int64
	var prRepo, prURL string
	e.s.db.R.QueryRow(`SELECT forge_repo_id FROM project_repos WHERE id = ?`, r.ID).Scan(&known)
	e.s.db.R.QueryRow(`SELECT repo, url FROM pull_requests WHERE number = 4`).Scan(&prRepo, &prURL)
	if known != 8 || prRepo != "acme/new" || prURL != "https://git.example.com/acme/new/pulls/4" {
		t.Errorf("id %d, pull request %s %s", known, prRepo, prURL)
	}
	if l := e.get(e.admin, issues[0]).Links; len(l) != 1 || l[0].URL != "https://git.example.com/acme/new/issues/1" {
		t.Errorf("links = %+v", l)
	}
	if keys := e.search(e.admin, "project:"+mine.Key); len(keys) != 1 || keys[0] != issues[0] {
		t.Errorf("issues = %v, want %v", keys, issues)
	}

	// The same name made again: only the id changes.
	mem.mu.Lock()
	mem.repos[fj.ID] = []ForgeRepo{{ID: 11, Path: "acme/new"}}
	mem.mu.Unlock()
	if _, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "acme/new", MoveOptions{}); err == nil {
		t.Error("a move to its own name was accepted")
	}
	if _, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "acme/new", MoveOptions{Relink: true}); err != nil {
		t.Fatal(err)
	}
	e.s.db.R.QueryRow(`SELECT forge_repo_id FROM project_repos WHERE id = ?`, r.ID).Scan(&known)
	if known != 11 {
		t.Errorf("id after recreate = %d", known)
	}
	if _, err := e.s.MoveProjectRepo(e.ctx, e.admin, mine.Key, r.ID, "acme/new", MoveOptions{Relink: true}); err == nil {
		t.Error("relinked to what it links already")
	}
}
