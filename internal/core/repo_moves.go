package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// A forge's repo id survives renames and transfers, so a moved repo is followed by id, not taken for a new one.

// ForgeRepoFinder is the optional part of a ForgeClient that looks a repo
// up by name and by the forge's id.
type ForgeRepoFinder interface {
	GetRepo(ctx context.Context, f *Forge, repo string) (*ForgeRepo, error)
	RepoByID(ctx context.Context, f *Forge, id int64) (*ForgeRepo, error)
}

// errRepoTaken: the new name is attached on the forge already (a duplicate
// made before Waypoint knew the repo's id); only a person's move takes it over.
var errRepoTaken = errors.New("the new name is attached already")

// followRepo is called with each repo name and id a forge reports (webhooks,
// discovery). It records the id of a repo attached without one, and follows
// a known id that arrives under another name: the repo moved.
func (s *Service) followRepo(ctx context.Context, f *Forge, repo string, id int64) {
	if id == 0 || repo == "" {
		return
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT repo, forge_repo_id FROM project_repos
		WHERE forge_id = ? AND (forge_repo_id = ? OR lower(repo) = lower(?))`, f.ID, id, repo)
	if err != nil {
		return
	}
	var moved string
	var unknown bool
	var stale []int64
	for rows.Next() {
		var name string
		var known int64
		if rows.Scan(&name, &known) != nil {
			continue
		}
		switch same := strings.EqualFold(name, repo); {
		case known == id && !same:
			moved = name
		case same && known == 0:
			unknown = true
		case same && known != id:
			stale = append(stale, known)
		}
	}
	rows.Close()
	// A new repo took the name of one that moved: follow that one first.
	for _, old := range stale {
		s.findMovedRepo(ctx, f, repo, old)
	}
	if moved != "" {
		if err := s.moveRepo(ctx, SystemActor(), f, moved, repo, id, false, false); err != nil {
			s.log.WarnContext(ctx, "follow moved repo", "forge", f.Name, "from", moved, "to", repo, "err", err)
		}
		return
	}
	if unknown {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE project_repos SET forge_repo_id = ? WHERE forge_id = ? AND lower(repo) = lower(?) AND forge_repo_id = 0`,
			id, f.ID, repo); err != nil {
			s.log.WarnContext(ctx, "record forge repo id", "repo", repo, "err", err)
		}
	}
}

// findMovedRepo asks the forge where the repo with this id is now, and
// follows it there from name.
func (s *Service) findMovedRepo(ctx context.Context, f *Forge, name string, id int64) {
	finder, ok := s.Forges.(ForgeRepoFinder)
	if !ok {
		return
	}
	now, err := finder.RepoByID(ctx, f, id)
	if isStatus(err, http.StatusNotFound) {
		s.log.Debug("moved repo not found", "forge", f.Name, "repo", name, "id", id) // deleted, or the token can't see it
		return
	} else if err != nil {
		s.log.WarnContext(ctx, "look up moved repo", "forge", f.Name, "repo", name, "id", id, "err", err)
		return
	}
	if strings.EqualFold(now.Path, name) {
		return
	}
	if err := s.moveRepo(ctx, SystemActor(), f, name, now.Path, id, false, false); err != nil {
		s.log.WarnContext(ctx, "follow moved repo", "forge", f.Name, "from", name, "to", now.Path, "err", err)
	}
}

// checkRepoNames asks the forge about every attached repo by name, part of
// the periodic sync: it records ids Waypoint lacks, and catches moves no
// webhook or discovery reported (a forge that redirects the old name, a
// repo that answers 404 or is another repo now).
func (s *Service) checkRepoNames(ctx context.Context) {
	finder, ok := s.Forges.(ForgeRepoFinder)
	if !ok {
		return
	}
	type attached struct {
		forgeID, known int64
		repo           string
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, min(repo), max(forge_repo_id) FROM project_repos GROUP BY forge_id, lower(repo)`)
	if err != nil {
		return
	}
	var list []attached
	for rows.Next() {
		var a attached
		if rows.Scan(&a.forgeID, &a.repo, &a.known) == nil {
			list = append(list, a)
		}
	}
	rows.Close()
	forges := map[int64]*Forge{}
	for _, a := range list {
		f, ok := forges[a.forgeID]
		if !ok {
			var rerr error
			f, rerr = s.forgeByID(ctx, a.forgeID)
			if readFault(rerr) && !errors.Is(rerr, ErrNotFound) { // A removed forge (not found) is not followed; other read faults are logged.
				s.log.WarnContext(ctx, "read the forge of an attached repo to follow its moves", "forge", a.forgeID, "repo", a.repo, "err", rerr)
			}
			forges[a.forgeID] = f
		}
		if f == nil {
			continue
		}
		got, err := finder.GetRepo(ctx, f, a.repo)
		switch {
		case err == nil && (a.known == 0 || got.ID == a.known):
			if !strings.EqualFold(got.Path, a.repo) {
				if err := s.moveRepo(ctx, SystemActor(), f, a.repo, got.Path, got.ID, false, false); err != nil {
					s.log.WarnContext(ctx, "follow moved repo", "forge", f.Name, "from", a.repo, "to", got.Path, "err", err)
				}
			} else {
				s.followRepo(ctx, f, a.repo, got.ID)
			}
		case a.known != 0 && (err == nil || isStatus(err, http.StatusNotFound)):
			s.findMovedRepo(ctx, f, a.repo, a.known)
		case err != nil && !isStatus(err, http.StatusNotFound):
			s.log.Debug("check repo name", "forge", f.Name, "repo", a.repo, "err", err)
		}
	}
}

// MoveOptions are how a person's move of a repo differs from following a
// rename (WP-154).
type MoveOptions struct {
	// Relink points the attachment at a different repository on the forge
	// (a recreated or replaced one, not the same one under a new name); the
	// project keeps its settings, issue links and pull request history, which
	// take the new repo's name. Without it a repo with another forge id is
	// refused.
	Relink bool `json:"relink"`
	// Force lets a relink take a repo other projects link too, detaching it
	// from them; without it that is refused.
	Force bool `json:"force"`
}

// MoveProjectRepo follows a repo renamed or transferred on its forge: the
// attachment, and everything Waypoint recorded under the old name, take
// the new name (project owners). It's for repos attached before Waypoint
// recorded their ids, whose move it couldn't see. An attachment the move
// made under the new name (automatic projects took the repo for a new
// one) is replaced; the project it made stays, for a person to delete.
func (s *Service) MoveProjectRepo(ctx context.Context, actor *Actor, projectRef string, id int64, newRepo string, opt MoveOptions) (*ProjectRepo, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return nil, err
	}
	var forgeID, known int64
	var old string
	if err := s.db.R.QueryRowContext(ctx, `SELECT forge_id, repo, forge_repo_id FROM project_repos WHERE id = ? AND project_id = ?`, id, p.ID).
		Scan(&forgeID, &old, &known); err != nil {
		return nil, notFound("project repo")
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return nil, err
	}
	newRepo = strings.Trim(strings.TrimSpace(newRepo), "/")
	if !validRepoFor(f.Kind, newRepo) {
		return nil, invalid("repo must look like owner/name")
	}
	if strings.EqualFold(newRepo, old) && !opt.Relink {
		return nil, invalid("%s is its name already", old)
	}
	finder, ok := s.Forges.(ForgeRepoFinder)
	if !ok {
		return nil, invalid("this forge client can't look repositories up")
	}
	got, err := finder.GetRepo(ctx, f, newRepo)
	if err != nil {
		return nil, invalid("couldn't find %s on %s: %s", newRepo, f.Name, PublicError(err))
	}
	if known != 0 && got.ID != known && !opt.Relink {
		return nil, invalid("%s on %s is another repository, not %s moved; to link the project to it instead, relink", got.Path, f.Name, old)
	}
	if strings.EqualFold(got.Path, old) && got.ID == known {
		return nil, invalid("%s is linked to that repository already", old)
	}
	taken, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT project_id FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?)`, f.ID, got.Path)
	if err != nil {
		return nil, err
	}
	for _, pid := range taken {
		if opt.Relink && pid != p.ID && !opt.Force {
			name := itoa64(pid)
			if other, err := s.projectByID(ctx, s.db.R, pid); err == nil {
				name = other.Key
			}
			return nil, invalid("%s is linked by project %s already; relinking would detach it there (force to do that)", got.Path, name)
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, pid, RoleOwner); err != nil {
			return nil, err
		}
	}
	if opt.Relink && !opt.Force && !strings.EqualFold(got.Path, old) {
		sharing, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT project_id FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?) AND project_id != ?`, f.ID, old, p.ID)
		if err != nil {
			return nil, err
		}
		if len(sharing) > 0 {
			return nil, invalid("%d other project(s) link %s too; relinking moves them to %s as well (force to do that)", len(sharing), old, got.Path)
		}
	}
	if strings.EqualFold(got.Path, old) {
		// The same name, another repository (deleted and made again): only the id changes.
		if err := s.write(ctx, actor, func(t *tx) error {
			if _, err := t.ExecContext(ctx, `UPDATE project_repos SET forge_repo_id = ? WHERE id = ?`, got.ID, id); err != nil {
				return err
			}
			t.audit(AuditEntry{Action: "repo.relink", Category: AuditSettings, ProjectID: p.ID, TargetType: "repo", TargetID: id,
				Target: got.Path, Detail: fmt.Sprintf("linked to the repository %s now has on %s (id %d, was %d)", got.Path, f.Name, got.ID, known)})
			return nil
		}); err != nil {
			return nil, err
		}
	} else if err := s.moveRepo(ctx, actor, f, old, got.Path, got.ID, true, opt.Relink); err != nil {
		return nil, err
	}
	repos, err := s.ListProjectRepos(ctx, actor, p.Key)
	if err != nil {
		return nil, err
	}
	for i := range repos {
		if repos[i].ID == id {
			return &repos[i], nil
		}
	}
	return nil, notFound("project repo")
}

// moveRepo renames a repo on one forge from old to repo: every project's
// attachment of it, the links, pull requests, checks and branch pushes
// recorded under the old name. Projects nested where automatic projects
// put them follow the repo to its new owner's project. takeOver replaces
// attachments of the new name that aren't of another repo; otherwise they
// stop the move.
func (s *Service) moveRepo(ctx context.Context, actor *Actor, f *Forge, old, repo string, id int64, takeOver, relink bool) error {
	if !validRepoFor(f.Kind, repo) {
		return invalid("repo %q", repo)
	}
	locks := []string{"repo|" + strings.ToLower(old), "repo|" + strings.ToLower(repo)}
	slices.Sort(locks)
	for _, l := range slices.Compact(locks) {
		defer forgeLocks.lock(l)()
	}
	base := strings.TrimRight(f.BaseURL, "/") + "/"
	oldURL, newURL := base+old+"/", base+repo+"/"
	rewrite := func(col string) (string, []any) {
		return `CASE WHEN lower(substr(` + col + `, 1, ?)) = lower(?) THEN ? || substr(` + col + `, ?) ELSE ` + col + ` END`,
			[]any{len(oldURL), oldURL, newURL, len(oldURL) + 1}
	}
	var moved []int64
	forgeActor := ""
	if actor.id() == 0 {
		forgeActor = f.Name
	}
	err := s.write(ctx, actor, func(t *tx) error {
		type attachment struct{ id, project, known int64 }
		read := func(name string) ([]attachment, error) {
			rows, err := t.QueryContext(ctx, `SELECT id, project_id, forge_repo_id FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?)`, f.ID, name)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []attachment
			for rows.Next() {
				var a attachment
				if err := rows.Scan(&a.id, &a.project, &a.known); err != nil {
					return nil, err
				}
				out = append(out, a)
			}
			return out, rows.Err()
		}
		from, err := read(old)
		if err != nil || len(from) == 0 {
			return err // moved already
		}
		to, err := read(repo)
		if err != nil {
			return err
		}
		for _, a := range to {
			if !takeOver {
				return errRepoTaken
			}
			if a.known != 0 && a.known != id && !relink {
				return invalid("%s on %s is attached as another repository", repo, f.Name)
			}
			if _, err := t.ExecContext(ctx, `DELETE FROM project_repos WHERE id = ?`, a.id); err != nil {
				return err
			}
			t.audit(AuditEntry{Action: "repo.detach", Category: AuditSettings, ProjectID: a.project, TargetType: "repo", TargetID: a.id,
				Target: repo, Detail: "replaced by " + old + ", which moved there"})
		}
		if _, err := t.ExecContext(ctx, `UPDATE project_repos SET repo = ?, forge_repo_id = ? WHERE forge_id = ? AND lower(repo) = lower(?)`,
			repo, id, f.ID, old); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM forge_repo_ignores WHERE forge_id = ? AND repo = ?`, f.ID, repo); err != nil {
			return err
		}
		// A link an issue already has under the new name stays under the old one (UPDATE OR IGNORE).
		expr, args := rewrite("url")
		if _, err := t.ExecContext(ctx, `UPDATE OR IGNORE external_links SET url = `+expr+`, repo = CASE WHEN lower(repo) = lower(?) THEN ? ELSE repo END
			WHERE lower(substr(url, 1, ?)) = lower(?)`, append(args, old, repo, len(oldURL), oldURL)...); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE comment_links SET url = `+expr+` WHERE forge_id = ? AND lower(substr(url, 1, ?)) = lower(?)`,
			append(args, f.ID, len(oldURL), oldURL)...); err != nil {
			return err
		}
		// Pull requests under the old name keep their merge arming and review state; new-name copies with the same number are dropped.
		if _, err := t.ExecContext(ctx, `DELETE FROM pull_requests WHERE forge_id = ? AND lower(repo) = lower(?)
			AND number IN (SELECT number FROM pull_requests WHERE forge_id = ? AND lower(repo) = lower(?))`, f.ID, repo, f.ID, old); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET repo = ?, url = `+expr+`,
			head_repo = CASE WHEN lower(head_repo) = lower(?) THEN ? ELSE head_repo END WHERE forge_id = ? AND lower(repo) = lower(?)`,
			append(append([]any{repo}, args...), old, repo, f.ID, old)...); err != nil {
			return err
		}
		// A cache row already under the new name wins; the old name's duplicates are dropped.
		for _, table := range []string{"ci_heads", "branch_activity"} {
			// #nosec G202 -- fixed table names
			if _, err := t.ExecContext(ctx, `UPDATE OR IGNORE `+table+` SET repo = ? WHERE forge_id = ? AND lower(repo) = lower(?)`, repo, f.ID, old); err != nil {
				return err
			}
			// #nosec G202 -- fixed table names
			if _, err := t.ExecContext(ctx, `DELETE FROM `+table+` WHERE forge_id = ? AND lower(repo) = lower(?)`, f.ID, old); err != nil {
				return err
			}
		}
		if _, err := t.ExecContext(ctx, `UPDATE OR IGNORE branch_heads SET repo = ? WHERE repo = ?`, repo, old); err != nil {
			return err
		}
		for _, a := range from {
			moved = append(moved, a.project)
			// Its webhook: an organisation's hook may cover it now, or no longer.
			t.emit(&Event{Kind: EvRepoHook, ProjectID: a.project, repoHook: &repoHookJob{install: a.id}})
			t.emit(&Event{Kind: EvProjectUpdated, ProjectID: a.project})
			detail, action := fmt.Sprintf("%s was renamed or transferred on %s", old, f.Name), "repo.move"
			if relink {
				detail, action = fmt.Sprintf("linked to %s on %s instead of %s", repo, f.Name, old), "repo.relink"
			}
			t.audit(AuditEntry{Action: action, Category: AuditSettings, ProjectID: a.project, TargetType: "repo", TargetID: a.id,
				Target: repo, Actor: forgeActor, Detail: detail})
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.log.Info("repo moved", "forge", f.Name, "from", old, "to", repo, "projects", moved)
	for _, pid := range moved {
		s.followNamespace(ctx, f, pid, old, repo)
	}
	return nil
}

// followNamespace moves a project to where automatic projects would put
// it under the repo's new name, when it sits where they'd have put it
// under the old one: under the new owner's organisation project (made if
// need be), or at the top for a person's repo. A project a person
// arranged elsewhere stays.
func (s *Service) followNamespace(ctx context.Context, f *Forge, projectID int64, old, repo string) {
	oldNS, _ := repoNamespace(old)
	newNS, _ := repoNamespace(repo)
	if strings.EqualFold(oldNS, newNS) {
		return
	}
	p, err := s.projectByID(ctx, s.db.R, projectID)
	if err != nil {
		return
	}
	var was int64
	err = s.db.R.QueryRowContext(ctx, `SELECT project_id FROM forge_namespaces WHERE lower(namespace) = lower(?)`, oldNS).Scan(&was)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return
	}
	cur := int64(0)
	if p.ParentID != nil {
		cur = *p.ParentID
	}
	// Only a project still where Waypoint put it follows the repo; a person's own project stays.
	if cur != was || (was == 0 && s.namespaceIsGroup(ctx, f, oldNS, nil)) {
		return
	}
	admin, err := s.forgeAdmin(ctx, f)
	if err != nil {
		return
	}
	in, want := ProjectInput{Parent: Opt[string]{Set: true}}, int64(0)
	if s.namespaceIsGroup(ctx, f, newNS, nil) {
		parent, err := s.namespaceProject(ctx, admin, f, newNS)
		if err != nil {
			s.log.WarnContext(ctx, "moved repo's organisation project", "namespace", newNS, "err", err)
			return
		}
		in.Parent, want = Some(parent.Key), parent.ID
	}
	if want == cur {
		return
	}
	if _, err := s.UpdateProject(ctx, admin, p.Key, in); err != nil {
		s.log.WarnContext(ctx, "move project with its repo", "project", p.Key, "err", err)
	}
}

type apiRepoRef struct {
	ID                int64  `json:"id"`
	FullName          string `json:"full_name"`
	PathWithNamespace string `json:"path_with_namespace"` // GitLab
}

func (r apiRepoRef) repo() *ForgeRepo {
	return &ForgeRepo{ID: r.ID, Path: cmp(r.FullName, r.PathWithNamespace)}
}

func (c *httpForgeClient) GetRepo(ctx context.Context, f *Forge, repo string) (*ForgeRepo, error) {
	path := "/repos/" + repo
	if f.Kind == "gitlab" {
		path = glProject(repo)
	}
	var r apiRepoRef
	if err := c.do(ctx, f, http.MethodGet, path, nil, &r); err != nil {
		return nil, err
	}
	return r.repo(), nil
}

func (c *httpForgeClient) RepoByID(ctx context.Context, f *Forge, id int64) (*ForgeRepo, error) {
	path := fmt.Sprintf("/repositories/%d", id)
	if f.Kind == "gitlab" {
		path = "/projects/" + url.PathEscape(fmt.Sprint(id))
	}
	var r apiRepoRef
	if err := c.do(ctx, f, http.MethodGet, path, nil, &r); err != nil {
		return nil, err
	}
	return r.repo(), nil
}
