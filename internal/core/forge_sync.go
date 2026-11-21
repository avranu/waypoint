package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Two-way comment sync, cross-forge dedupe and auto-created projects
// (WP-D7, "Forges").

// ForgeComment is a comment on a forge issue.
type ForgeComment struct {
	ID        string
	Author    string
	Body      string
	URL       string
	UpdatedAt time.Time
}

// ForgeRepo is a repository a forge token can see.
type ForgeRepo struct {
	ID    int64  // the forge's id, which survives renames and transfers (0: not known)
	Path  string // owner/name, or group/subgroup/name on GitLab
	Group bool   // owned by an organisation or group rather than a person
}

// ForgeCommenter is the optional part of a ForgeClient that reads and
// writes issue comments.
type ForgeCommenter interface {
	ListComments(ctx context.Context, f *Forge, repo string, number int) ([]ForgeComment, error)
	PostComment(ctx context.Context, f *Forge, repo string, number int, body string) (*ForgeComment, error)
	EditComment(ctx context.Context, f *Forge, repo string, number int, id, body string) error
	DeleteComment(ctx context.Context, f *Forge, repo string, number int, id string) error
}

// keyedMutex serialises work per key (a forge issue, a repo, a namespace),
// so concurrent webhooks and syncs check-then-create one at a time. Waypoint
// is one process with one database writer, so in-process locks suffice.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	sync.Mutex
	users int
}

func (k *keyedMutex) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.users++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// forgeLocks guards forge imports and automatic projects.
var forgeLocks keyedMutex

// ForgeDiscoverer is the optional part of a ForgeClient that lists the
// repos a token can see, for auto-created projects.
type ForgeDiscoverer interface {
	ListRepos(ctx context.Context, f *Forge) ([]ForgeRepo, error)
	IsGroup(ctx context.Context, f *Forge, namespace string) (bool, error)
}

// ---- comments: forge → Waypoint ----

// importForgeComment mirrors one forge comment onto every Waypoint issue
// mirroring that forge issue. Comments Waypoint posted itself are
// recognised, so they don't come back as copies.
func (s *Service) importForgeComment(ctx context.Context, f *Forge, repo string, number int, action string, c ForgeComment) error {
	if c.ID == "" || number == 0 {
		return nil
	}
	issues, err := s.mirroredIssues(ctx, f, repo, number)
	if err != nil {
		return err
	}
	for _, id := range issues {
		if err := s.upsertForgeComment(ctx, f, id, action, c); err != nil {
			return err
		}
	}
	return nil
}

// mirroredIssues lists Waypoint issues mirroring a forge issue, in projects
// syncing that repo from this forge.
func (s *Service) mirroredIssues(ctx context.Context, f *Forge, repo string, number int) ([]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT el.issue_id, el.url FROM external_links el
		JOIN issues i ON i.id = el.issue_id
		JOIN project_repos r ON r.project_id = i.project_id AND r.forge_id = ? AND lower(r.repo) = lower(el.repo) AND r.sync_issues = 1
		WHERE el.mirror = 1 AND lower(el.repo) = lower(?) AND el.number = ?`, f.ID, repo, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	host := hostOf(f.BaseURL)
	var out []int64
	for rows.Next() {
		var id int64
		var u string
		if err := rows.Scan(&id, &u); err != nil {
			return nil, err
		}
		if strings.EqualFold(hostOf(u), host) { // a mirror on another forge has its own link
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func (s *Service) upsertForgeComment(ctx context.Context, f *Forge, issueID int64, action string, c ForgeComment) error {
	body := truncateBytes(strings.TrimSpace(c.Body), maxCommentLen)
	// Who isn't a Waypoint person writes only held text, which nothing but
	// people read until a person approves it (WP-279); a login agents may
	// write as writes unreviewed agent text (WP-282).
	outside, agent := s.forgeWriter(ctx, s.db.R, f, c.Author)
	sys := forgeSyncActor(f, c.Author, agent)
	return s.write(ctx, sys, func(t *tx) error {
		i, err := s.issueByID(ctx, t, issueID, false)
		if err != nil {
			return err
		}
		var heldAt int64
		var heldBody string
		var discarded bool
		err = t.QueryRowContext(ctx, `SELECT created_at, body, discarded FROM outside_text WHERE issue_id = ? AND forge_id = ? AND external_id = ?`,
			issueID, f.ID, c.ID).Scan(&heldAt, &heldBody, &discarded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		held := err == nil
		var cid int64
		var mine bool // our copy of a comment written elsewhere (Waypoint or another forge)
		err = t.QueryRowContext(ctx, `SELECT l.comment_id, l.origin = 0 FROM comment_links l JOIN comments c ON c.id = l.comment_id
			WHERE l.forge_id = ? AND l.external_id = ? AND c.issue_id = ?`, f.ID, c.ID, issueID).Scan(&cid, &mine)
		switch {
		case err == nil && action == "deleted":
			if mine {
				return nil // deleting the forge copy leaves the original
			}
			if _, err := t.ExecContext(ctx, `DELETE FROM comments WHERE id = ?`, cid); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
			return s.reindex(ctx, t, i.ID)
		case err == nil:
			if mine || body == "" {
				return nil
			}
			if outside {
				// An approved comment keeps its approved text; the new one waits.
				var cur string
				if err := t.QueryRowContext(ctx, `SELECT body FROM comments WHERE id = ?`, cid).Scan(&cur); err != nil {
					return err
				}
				if cur == body {
					return s.dropHeld(ctx, t, i, f, c.ID)
				}
				return s.holdOutside(ctx, t, f, heldText{issue: i, commentID: cid, externalID: c.ID, url: c.URL, author: c.Author, body: body})
			}
			if held {
				if err := s.dropHeld(ctx, t, i, f, c.ID); err != nil {
					return err
				}
			}
			var cur string
			if err := t.QueryRowContext(ctx, `SELECT body FROM comments WHERE id = ?`, cid).Scan(&cur); err != nil || cur == body {
				return err
			}
			if err := s.setForgeCommentBody(ctx, t, cid, body, agent); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
			return s.reindex(ctx, t, i.ID)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if action == "deleted" || body == "" {
			if held {
				return s.dropHeld(ctx, t, i, f, c.ID)
			}
			return nil
		}
		// Its copy here was deleted (or a person discarded it): it stays deleted.
		var gone bool
		if err := t.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM comment_tombstones WHERE issue_id = ? AND forge_id = ? AND external_id = ?)`,
			issueID, f.ID, c.ID).Scan(&gone); err != nil || gone {
			return err
		}
		// The webhook for a comment Waypoint just posted can beat the post's
		// own reply: recognise our copy by its marker and text instead of
		// copying it back.
		if strings.Contains(body, viaWaypoint) {
			var own int64
			err = t.QueryRowContext(ctx, `SELECT id FROM comments c WHERE issue_id = ? AND created_at > ? AND instr(?, body) > 0
				AND NOT EXISTS (SELECT 1 FROM comment_links l WHERE l.comment_id = c.id AND l.forge_id = ?)
				ORDER BY id DESC LIMIT 1`, issueID, s.nowMS()-10*60*1000, body, f.ID).Scan(&own)
			if err == nil {
				_, err = t.ExecContext(ctx, `INSERT OR IGNORE INTO comment_links(comment_id, forge_id, external_id, url) VALUES (?,?,?,?)`, own, f.ID, c.ID, c.URL)
				return err
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if discarded && heldBody == body {
			return nil // a person kept it out
		}
		if outside {
			return s.holdOutside(ctx, t, f, heldText{issue: i, externalID: c.ID, url: c.URL, author: c.Author, body: body})
		}
		created := s.nowMS()
		if held {
			// It waited until its author turned out to be a person: let it in
			// where it was written, quietly.
			created = heldAt
			if err := s.dropHeld(ctx, t, i, f, c.ID); err != nil {
				return err
			}
		}
		return s.addForgeComment(ctx, t, f, i, c.Author, c.ID, c.URL, body, created, held)
	})
}

// viaWaypoint marks comments Waypoint posts to forges.
const viaWaypoint = "(via Waypoint)"

// syncComments pulls a forge issue's comments (during a repo sync).
func (s *Service) syncComments(ctx context.Context, f *Forge, repo string, number int) error {
	cm, ok := s.Forges.(ForgeCommenter)
	if !ok || number == 0 {
		return nil
	}
	issues, err := s.mirroredIssues(ctx, f, repo, number)
	if err != nil || len(issues) == 0 {
		return err
	}
	list, err := cm.ListComments(ctx, f, repo, number)
	if err != nil {
		return err
	}
	for _, c := range list {
		for _, id := range issues {
			if err := s.upsertForgeComment(ctx, f, id, "created", c); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- comments: Waypoint → forge ----

// pushComment posts a new comment to every synced forge issue the issue
// mirrors, attributed to its author, and remembers the copy. A comment that
// came from one forge goes to the others (mirrors stay complete) but never
// back to its own.
func (s *Service) pushComment(ctx context.Context, e *Event) {
	cm, ok := s.Forges.(ForgeCommenter)
	commentID := dataInt(e, "comment_id")
	if !ok || commentID == 0 || e.Data["local"] == true {
		return // a local comment names machines and paths (addLocalComment)
	}
	text, err := s.forgeCommentText(ctx, commentID)
	if err != nil {
		return // gone meanwhile
	}
	links, err := s.externalLinks(ctx, s.db.R, e.IssueID)
	if err != nil {
		return
	}
	for _, l := range links {
		if !l.Mirror || l.Number == nil {
			continue // a forge issue that only mentions this one gets nothing
		}
		f := s.forgeForLink(ctx, l)
		if f == nil || f.ID == 0 || f.token == "" || !s.repoPostsComments(ctx, e.ProjectID, f.ID, l.Repo) {
			continue
		}
		var exists int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM comment_links WHERE comment_id = ? AND forge_id = ?`, commentID, f.ID).Scan(&exists); err != nil || exists > 0 {
			continue
		}
		repo, num := l.Repo, int(*l.Number)
		s.goBG(func() {
			ctx := context.Background()
			fc, err := cm.PostComment(ctx, f, repo, num, text)
			if err != nil {
				s.log.WarnContext(ctx, "post comment to forge", "repo", repo, "issue", num, "err", err)
				return
			}
			if _, err := s.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO comment_links(comment_id, forge_id, external_id, url) VALUES (?,?,?,?)`,
				commentID, f.ID, fc.ID, fc.URL); err != nil {
				s.log.WarnContext(ctx, "remember forge comment", "err", err)
			}
		})
	}
}

// forgeCommentText is a Waypoint comment as posted to a forge, attributed
// to its author.
func (s *Service) forgeCommentText(ctx context.Context, commentID int64) (string, error) {
	var body, author, extAuthor, extVia string
	if err := s.db.R.QueryRowContext(ctx, `SELECT c.body, coalesce(nullif(u.display_name, ''), u.username, ''), c.external_author, c.external_via
		FROM comments c LEFT JOIN users u ON u.id = c.author_id WHERE c.id = ?`, commentID).Scan(&body, &author, &extAuthor, &extVia); err != nil {
		return "", err
	}
	if extVia != "" {
		author = extAuthor + " on " + extVia
	}
	return fmt.Sprintf("**%s** %s:\n\n%s", cmp(author, "Waypoint"), viaWaypoint, body), nil
}

// forgeCommentChange is an edited or deleted Waypoint comment's copies on
// forges, gathered in the transaction (a deleted comment's links go with
// it) and pushed after commit.
type forgeCommentChange struct {
	commentID int64
	deleted   bool
	copies    []forgeCommentCopy // the ones Waypoint posted
}

type forgeCommentCopy struct {
	forgeID    int64
	externalID string
}

// forgeCommentChanged collects a comment's forge copies before it's edited
// or deleted. Deleting also leaves tombstones, so neither the forge comments
// it came from nor Waypoint's own copies (if deleting them on the forge
// fails) are imported again. Nil when the comment never reached a forge.
func (s *Service) forgeCommentChanged(ctx context.Context, t *tx, issueID, commentID int64, deleted bool) (*forgeCommentChange, error) {
	rows, err := t.QueryContext(ctx, `SELECT forge_id, external_id, origin FROM comment_links WHERE comment_id = ?`, commentID)
	if err != nil {
		return nil, err
	}
	ch := &forgeCommentChange{commentID: commentID, deleted: deleted}
	var all []forgeCommentCopy
	for rows.Next() {
		var c forgeCommentCopy
		var origin bool
		if err := rows.Scan(&c.forgeID, &c.externalID, &origin); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, c)
		if !origin { // someone else's comment on the forge stays theirs
			ch.copies = append(ch.copies, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	if deleted {
		for _, c := range all {
			if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO comment_tombstones(issue_id, forge_id, external_id, deleted_at) VALUES (?,?,?,?)`,
				issueID, c.forgeID, c.externalID, s.nowMS()); err != nil {
				return nil, err
			}
		}
	}
	return ch, nil
}

// pushCommentChange edits or deletes the copies Waypoint posted of a
// comment, on the forge issues the Waypoint issue mirrors.
func (s *Service) pushCommentChange(ctx context.Context, e *Event) {
	ch := e.forgeComment
	cm, ok := s.Forges.(ForgeCommenter)
	if !ok || ch == nil || len(ch.copies) == 0 {
		return
	}
	text := ""
	if !ch.deleted {
		var err error
		if text, err = s.forgeCommentText(ctx, ch.commentID); err != nil {
			return
		}
	}
	links, err := s.externalLinks(ctx, s.db.R, e.IssueID)
	if err != nil {
		return
	}
	for _, c := range ch.copies {
		for _, l := range links {
			if !l.Mirror || l.Number == nil {
				continue
			}
			f := s.forgeForLink(ctx, l)
			if f == nil || f.ID != c.forgeID || f.token == "" {
				continue
			}
			repo, num, id := l.Repo, int(*l.Number), c.externalID
			s.goBG(func() {
				ctx := context.Background()
				var err error
				if ch.deleted {
					err = cm.DeleteComment(ctx, f, repo, num, id)
				} else {
					err = cm.EditComment(ctx, f, repo, num, id, text)
				}
				if err != nil {
					s.log.WarnContext(ctx, "update comment on forge", "repo", repo, "issue", num, "deleted", ch.deleted, "err", err)
				}
			})
			break // one copy lives on one forge issue
		}
	}
}

// repoPostsComments: the project syncs this repo and posts comments to it.
func (s *Service) repoPostsComments(ctx context.Context, projectID, forgeID int64, repo string) bool {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_repos WHERE project_id = ? AND forge_id = ? AND lower(repo) = lower(?)
		AND sync_issues = 1 AND post_comments = 1`, projectID, forgeID, repo).Scan(&n)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read repo posts comments", "forgeID", forgeID, "repo", repo, "err", err)
		}
		return false
	}
	return n > 0
}

// repoSynced: the project syncs issues for this repo from this forge.
func (s *Service) repoSynced(ctx context.Context, projectID, forgeID int64, repo string) bool {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_repos WHERE project_id = ? AND forge_id = ? AND lower(repo) = lower(?) AND sync_issues = 1`,
		projectID, forgeID, repo).Scan(&n)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read repo synced", "forgeID", forgeID, "repo", repo, "err", err)
		}
		return false
	}
	return n > 0
}

// pushStateToMirrors closes or reopens an issue's other forge copies after
// one forge changed it (a GitHub mirror of a Forgejo repo, say).
func (s *Service) pushStateToMirrors(ctx context.Context, issueID, projectID int64, closed bool, exceptURL string) {
	links, err := s.externalLinks(ctx, s.db.R, issueID)
	if err != nil {
		return
	}
	state := map[bool]string{true: "closed", false: "open"}[closed]
	for _, l := range links {
		if !l.Mirror || l.Number == nil || l.URL == exceptURL || l.State == state {
			continue
		}
		f := s.forgeForLink(ctx, l)
		if f == nil || f.ID == 0 || f.token == "" || !s.repoSynced(ctx, projectID, f.ID, l.Repo) {
			continue
		}
		repo, num, id := l.Repo, int(*l.Number), l.ID
		s.goBG(func() {
			if err := s.Forges.SetIssueState(context.Background(), f, repo, num, closed); err != nil {
				s.log.WarnContext(ctx, "push issue state to mirror", "repo", repo, "issue", num, "err", err)
				return
			}
			if _, err := s.db.W.ExecContext(context.Background(), `UPDATE external_links SET state = ?, synced_at = ? WHERE id = ?`, state, s.nowMS(), id); err != nil {
				s.log.WarnContext(ctx, "best-effort write failed", "err", err)
			}
		})
	}
}

// ---- auto-created projects ----

// forgeAdmin is who auto-created projects belong to: the admin who
// connected the forge, or else the first admin.
func (s *Service) forgeAdmin(ctx context.Context, f *Forge) (*Actor, error) {
	if f.createdBy != nil {
		if u, err := s.userByID(ctx, s.db.R, *f.createdBy); err == nil && u.IsAdmin && !u.Disabled {
			return &Actor{User: u}, nil
		}
	}
	var id int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0 ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		return nil, fmt.Errorf("no admin to own auto-created projects: %w", err)
	}
	u, err := s.userByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	return &Actor{User: u}, nil
}

// ensureRepoProject makes sure a repo the forge reported belongs to a
// project syncing its issues: one already using it on this forge, one using
// a same-named repo on another forge (a mirror), or a new project nested
// under its organisation's. group says whether the owner is an
// organisation, when the caller knows; id is the forge's id for the repo
// (0: not known).
func (s *Service) ensureRepoProject(ctx context.Context, f *Forge, repo string, id int64, group *bool) error {
	if !validRepoFor(f.Kind, repo) {
		return invalid("repo %q", repo)
	}
	// One at a time per owner/name (across forges, for mirrors), so two
	// webhooks for a new repo make one project.
	unlock := forgeLocks.lock("repo|" + strings.ToLower(repo))
	defer unlock()
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?))
		+ (SELECT count(*) FROM forge_repo_ignores WHERE forge_id = ? AND repo = ?)`, f.ID, repo, f.ID, repo).Scan(&n); err != nil || n > 0 {
		return err // attached already, or detached on purpose
	}
	// A mirror: the same owner/name already synced from another forge.
	var projectID int64
	err := s.db.R.QueryRowContext(ctx, `SELECT project_id FROM project_repos WHERE lower(repo) = lower(?) ORDER BY sync_issues DESC, id LIMIT 1`, repo).Scan(&projectID)
	mirror := err == nil
	if errors.Is(err, sql.ErrNoRows) {
		admin, err := s.forgeAdmin(ctx, f)
		if err != nil {
			return err
		}
		ns, name := repoNamespace(repo)
		in := ProjectInput{Name: &name}
		if ns != "" && s.namespaceIsGroup(ctx, f, ns, group) {
			parent, err := s.namespaceProject(ctx, admin, f, ns)
			if err != nil {
				return err
			}
			in.Parent = Some(parent.Key)
		}
		p, err := s.CreateProject(ctx, admin, in)
		if err != nil {
			return err
		}
		projectID = p.ID
	} else if err != nil {
		return err
	}
	// A repo joined by name alone may not be the same repo at all (anyone
	// can own acme/web on another forge): it links PRs and commits, but its
	// issues and Waypoint's comments wait until a project owner turns sync
	// and posting on. Its bots stay off too: a mirror's pull requests are
	// twins of the first repo's (a mirror copies reviews across), so
	// reviewing both would double every review (WP-D14).
	on := boolInt(!mirror)
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO project_repos(project_id, forge_id, repo, forge_repo_id, sync_issues, post_comments, bots, created_at) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id, forge_id, repo) DO NOTHING`, projectID, f.ID, repo, id, on, on, on, s.nowMS()); err != nil {
		return err
	}
	if mirror {
		s.log.Info("repo joined a project by name; issue sync stays off until turned on", "forge", f.Name, "repo", repo, "project", projectID)
		return nil
	}
	var repoID int64
	if err := s.db.W.QueryRowContext(ctx, `SELECT id FROM project_repos WHERE project_id = ? AND forge_id = ? AND repo = ?`, projectID, f.ID, repo).Scan(&repoID); err != nil {
		return err
	}
	s.log.Info("repo attached automatically", "forge", f.Name, "repo", repo, "project", projectID)
	s.automate(ctx, SystemActor(), &Event{Kind: EvRepoAttached, ProjectID: projectID, New: repo, Data: map[string]any{"repo": repo, "forge": f.Name}})
	s.goBG(func() {
		if err := s.SyncRepo(context.Background(), repoID); err != nil {
			s.log.WarnContext(ctx, "initial repo sync failed", "repo", repo, "err", err)
		}
	})
	return nil
}

func (s *Service) namespaceIsGroup(ctx context.Context, f *Forge, ns string, hint *bool) bool {
	if hint != nil {
		return *hint
	}
	if strings.Contains(ns, "/") {
		return true // only groups nest
	}
	d, ok := s.Forges.(ForgeDiscoverer)
	if !ok {
		return false
	}
	g, err := d.IsGroup(ctx, f, ns)
	if err != nil {
		if readFault(err) && !isStatus(err, http.StatusNotFound) {
			s.log.WarnContext(ctx, "read namespace is group", "ns", ns, "err", err)
		}
		return false
	}
	return g
}

// namespaceProject is the project standing for an organisation or group,
// made the first time it's needed and shared by every forge. GitLab
// subgroups nest under their parent group's project.
func (s *Service) namespaceProject(ctx context.Context, admin *Actor, f *Forge, ns string) (*Project, error) {
	// Namespaces lock after repos and children before parents, so no cycle.
	unlock := forgeLocks.lock("ns|" + strings.ToLower(ns))
	defer unlock()
	var id int64
	err := s.db.R.QueryRowContext(ctx, `SELECT project_id FROM forge_namespaces WHERE namespace = ?`, ns).Scan(&id)
	if err == nil {
		return s.projectByID(ctx, s.db.R, id)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	parentNS, name := repoNamespace(ns)
	in := ProjectInput{Name: &name}
	if parentNS != "" {
		parent, err := s.namespaceProject(ctx, admin, f, parentNS)
		if err != nil {
			return nil, err
		}
		in.Parent = Some(parent.Key)
	}
	p, err := s.CreateProject(ctx, admin, in)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO forge_namespaces(namespace, project_id) VALUES (?, ?)`, ns, p.ID); err != nil {
		return nil, err
	}
	return p, nil
}

// DiscoveredRepo is a repo discovery would attach.
type DiscoveredRepo struct {
	Repo string `json:"repo"`
	// Namespace is the organisation or group whose project it nests under
	// ("" for a person's repo); NewNamespace says that project is new too.
	Namespace    string `json:"namespace,omitempty"`
	NewNamespace bool   `json:"new_namespace,omitempty"`
	// Joins names the project it joins as a mirror, making none.
	Joins string `json:"joins,omitempty"`
}

// RepoPick narrows discovery to some repos. Skipped repos are remembered,
// so automatic projects don't attach them later either.
type RepoPick struct {
	Repos []string `json:"repos"`
	Skip  []string `json:"skip"`
}

// PreviewDiscover lists the repos DiscoverRepos would attach, and the
// projects they'd make, without changing anything (admins).
func (s *Service) PreviewDiscover(ctx context.Context, actor *Actor, forgeID int64) ([]DiscoveredRepo, error) {
	f, err := s.discoverForge(ctx, actor, forgeID)
	if err != nil {
		return nil, err
	}
	repos, err := s.pendingRepos(ctx, f, false)
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveredRepo, 0, len(repos))
	for _, r := range repos {
		d := DiscoveredRepo{Repo: r.Path}
		// A repo attached under its old name (renamed or transferred) joins
		// its project; so does a mirror.
		err := s.db.R.QueryRowContext(ctx, `SELECT p.name FROM project_repos pr JOIN projects p ON p.id = pr.project_id
			WHERE (pr.forge_id = ? AND pr.forge_repo_id = ? AND ? != 0) OR lower(pr.repo) = lower(?)
			ORDER BY pr.forge_repo_id = ? DESC, pr.sync_issues DESC, pr.id LIMIT 1`, f.ID, r.ID, r.ID, r.Path, r.ID).Scan(&d.Joins)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if ns, _ := repoNamespace(r.Path); d.Joins == "" && ns != "" && r.Group {
			d.Namespace = ns
			var n int
			if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM forge_namespaces WHERE namespace = ?`, ns).Scan(&n); err != nil {
				return nil, err
			}
			d.NewNamespace = n == 0
		}
		out = append(out, d)
	}
	return out, nil
}

// DiscoverRepos attaches every repo the forge's token can see, or only
// those pick names, creating projects as needed (admins). It returns how
// many repos were new.
func (s *Service) DiscoverRepos(ctx context.Context, actor *Actor, forgeID int64, pick *RepoPick) (int, error) {
	f, err := s.discoverForge(ctx, actor, forgeID)
	if err != nil {
		return 0, err
	}
	if pick == nil {
		return s.discoverRepos(ctx, f)
	}
	repos, err := s.pendingRepos(ctx, f, true)
	if err != nil {
		return 0, err
	}
	// Only repos the forge reported: the names come from the browser.
	want := func(list []string) map[string]bool {
		m := map[string]bool{}
		for _, r := range list {
			m[strings.ToLower(r)] = true
		}
		return m
	}
	attach, skip := want(pick.Repos), want(pick.Skip)
	var picked []ForgeRepo
	err = s.write(ctx, actor, func(t *tx) error {
		for _, r := range repos {
			switch key := strings.ToLower(r.Path); {
			case attach[key]:
				picked = append(picked, r)
			case skip[key]:
				if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO forge_repo_ignores(forge_id, repo, created_at) VALUES (?,?,?)`, f.ID, r.Path, s.nowMS()); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return s.attachRepos(ctx, f, picked), nil
}

func (s *Service) discoverForge(ctx context.Context, actor *Actor, forgeID int64) (*Forge, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, ErrForbidden
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return nil, err
	}
	if f.token == "" {
		return nil, invalid("add an access token to %s first", f.Name)
	}
	return f, nil
}

func (s *Service) discoverRepos(ctx context.Context, f *Forge) (int, error) {
	repos, err := s.pendingRepos(ctx, f, true)
	if err != nil {
		return 0, err
	}
	return s.attachRepos(ctx, f, repos), nil
}

// pendingRepos lists the forge's repos that aren't attached on it yet and
// weren't detached on purpose. With follow, an attached repo reported under
// a new name is renamed first, so it isn't pending (WP-166).
func (s *Service) pendingRepos(ctx context.Context, f *Forge, follow bool) ([]ForgeRepo, error) {
	d, ok := s.Forges.(ForgeDiscoverer)
	if !ok {
		return nil, invalid("this forge client can't list repositories")
	}
	repos, err := d.ListRepos(ctx, f)
	if err != nil {
		return nil, invalid("couldn't list repositories: %s", PublicError(err))
	}
	var out []ForgeRepo
	for _, r := range repos {
		if follow {
			s.followRepo(ctx, f, r.Path, r.ID)
		}
		var n int
		if err := s.db.R.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?))
			+ (SELECT count(*) FROM forge_repo_ignores WHERE forge_id = ? AND repo = ?)`, f.ID, r.Path, f.ID, r.Path).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Service) attachRepos(ctx context.Context, f *Forge, repos []ForgeRepo) int {
	added := 0
	for _, r := range repos {
		g := r.Group
		if err := s.ensureRepoProject(ctx, f, r.Path, r.ID, &g); err != nil {
			s.log.WarnContext(ctx, "auto project", "forge", f.Name, "repo", r.Path, "err", err)
			continue
		}
		added++
	}
	return added
}

// ---- HTTP client: comments and repos ----

type apiComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	URL  string `json:"html_url"`
	User struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	} `json:"user"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (a apiComment) comment() ForgeComment {
	return ForgeComment{ID: strconv.FormatInt(a.ID, 10), Author: cmp(a.User.Login, a.User.Username), Body: a.Body, URL: a.URL, UpdatedAt: a.UpdatedAt}
}

func (c *httpForgeClient) ListComments(ctx context.Context, f *Forge, repo string, number int) ([]ForgeComment, error) {
	if f.Kind == "gitlab" {
		return c.glListComments(ctx, f, repo, number)
	}
	var out []ForgeComment
	for page := 1; page <= maxForgePages; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}}
		per := 100
		if f.Kind == "github" {
			q.Set("per_page", "100")
		} else {
			q.Set("limit", "50")
			per = 50
		}
		var list []apiComment
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?%s", repo, number, q.Encode()), nil, &list); err != nil {
			return nil, err
		}
		for _, a := range list {
			out = append(out, a.comment())
		}
		if len(list) < per {
			break
		}
	}
	return out, nil
}

func (c *httpForgeClient) PostComment(ctx context.Context, f *Forge, repo string, number int, body string) (*ForgeComment, error) {
	if f.Kind == "gitlab" {
		return c.glPostComment(ctx, f, repo, number, body)
	}
	var a apiComment
	if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), map[string]string{"body": body}, &a); err != nil {
		return nil, err
	}
	fc := a.comment()
	return &fc, nil
}

func (c *httpForgeClient) EditComment(ctx context.Context, f *Forge, repo string, number int, id, body string) error {
	if f.Kind == "gitlab" {
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/issues/%d/notes/%s", glProject(repo), number, url.PathEscape(id)), map[string]string{"body": body}, nil)
	}
	return c.do(ctx, f, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%s", repo, url.PathEscape(id)), map[string]string{"body": body}, nil)
}

func (c *httpForgeClient) DeleteComment(ctx context.Context, f *Forge, repo string, number int, id string) error {
	if f.Kind == "gitlab" {
		return c.do(ctx, f, http.MethodDelete, fmt.Sprintf("%s/issues/%d/notes/%s", glProject(repo), number, url.PathEscape(id)), nil, nil)
	}
	return c.do(ctx, f, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/comments/%s", repo, url.PathEscape(id)), nil, nil)
}

func (c *httpForgeClient) ListRepos(ctx context.Context, f *Forge) ([]ForgeRepo, error) {
	if f.Kind == "gitlab" {
		return c.glListRepos(ctx, f)
	}
	type apiRepo struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Owner    struct {
			Login    string `json:"login"`
			Username string `json:"username"`
			Type     string `json:"type"`
		} `json:"owner"`
	}
	orgs := map[string]bool{}
	if f.Kind != "github" {
		// Forgejo's repo owners don't say whether they're organisations.
		var list []struct {
			Name     string `json:"name"`
			Username string `json:"username"`
		}
		if err := c.do(ctx, f, http.MethodGet, "/user/orgs?limit=50", nil, &list); err != nil {
			return nil, err
		}
		for _, o := range list {
			orgs[strings.ToLower(cmp(o.Username, o.Name))] = true
		}
	}
	var out []ForgeRepo
	for page := 1; page <= maxForgePages; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}}
		per := 100
		if f.Kind == "github" {
			q.Set("per_page", "100")
			q.Set("affiliation", "owner,collaborator,organization_member")
		} else {
			q.Set("limit", "50")
			per = 50
		}
		var list []apiRepo
		if err := c.do(ctx, f, http.MethodGet, "/user/repos?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, r := range list {
			owner := strings.ToLower(cmp(r.Owner.Login, r.Owner.Username))
			out = append(out, ForgeRepo{ID: r.ID, Path: r.FullName, Group: r.Owner.Type == "Organization" || orgs[owner]})
		}
		if len(list) < per {
			break
		}
	}
	return out, nil
}

func (c *httpForgeClient) IsGroup(ctx context.Context, f *Forge, namespace string) (bool, error) {
	path := "/orgs/" + url.PathEscape(namespace)
	if f.Kind == "gitlab" {
		path = "/groups/" + url.PathEscape(namespace)
	}
	var v map[string]any
	if err := c.do(ctx, f, http.MethodGet, path, nil, &v); err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Whoami names the account the forge's token belongs to (WP-279).
func (c *httpForgeClient) Whoami(ctx context.Context, f *Forge) (string, error) {
	var u struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	}
	if err := c.do(ctx, f, http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	return cmp(u.Login, u.Username), nil
}
