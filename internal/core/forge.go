package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Forge is a connected GitHub, Forgejo or GitLab instance.
type Forge struct {
	ID               int64  `json:"id"`
	Kind             string `json:"kind"` // github, forgejo, gitlab
	Name             string `json:"name"`
	BaseURL          string `json:"base_url"`
	HasToken         bool   `json:"has_token"`
	HasWebhookSecret bool   `json:"has_webhook_secret"`
	WebhookURL       string `json:"webhook_url"`
	// AutoProjects makes a project (nested under its organisation's) for
	// every repo the forge reports, with issue sync on.
	AutoProjects bool `json:"auto_projects"`
	// HasBotToken: bots post with their own account's token (WP-D14).
	HasBotToken bool `json:"has_bot_token"`
	// MirrorPrivate: when a tool mirrors this forge's repos to another forge
	// and copies their reviews across, the paths it keeps off the other one
	// (globs). Findings on them stay out of review text the mirror copies.
	// Empty when nothing mirrors the forge (WP-459).
	MirrorPrivate []string `json:"mirror_private"`
	// MirrorAccount is the login that mirror posts its copies of the other
	// forge's reviews as: only its relays count as the copied reviewer's
	// (WP-459). Empty when nothing mirrors the forge.
	MirrorAccount string `json:"mirror_account"`
	// People are its logins that are Waypoint people or agents' accounts
	// (WP-283): what anyone else writes there is text from outside (WP-279).
	People    []ForgePerson `json:"people"`
	CreatedAt time.Time     `json:"created_at"`
	token     string
	botToken  string
	secret    string
	createdBy *int64
}

// forgeKinds are the forges Waypoint talks to.
var forgeKinds = []string{"github", "forgejo", "gitlab"}

// kindLabel is a forge kind as people write it.
func kindLabel(kind string) string {
	switch kind {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	}
	return "Forgejo"
}

// ForgeItem is an issue or pull request on a forge.
type ForgeItem struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"` // open, closed, merged, draft
	URL       string    `json:"url"`
	IsPR      bool      `json:"is_pr"`
	Labels    []string  `json:"labels"`
	UpdatedAt time.Time `json:"updated_at"`
	// Author is the forge login whose text it is: who opened it (on GitLab
	// webhooks, whoever made the change). "" when the forge didn't say.
	Author string `json:"author,omitempty"`
	// Confidential (GitLab): visible only to some of the forge's users, so
	// none of its content is copied into Waypoint.
	Confidential bool `json:"confidential,omitempty"`
}

// ForgeClient talks to forge APIs; replaceable in tests.
type ForgeClient interface {
	GetItem(ctx context.Context, f *Forge, repo string, number int, pr bool) (*ForgeItem, error)
	ListIssues(ctx context.Context, f *Forge, repo string, since time.Time) ([]ForgeItem, error)
	SetIssueState(ctx context.Context, f *Forge, repo string, number int, closed bool) error
}

// ExternalLink connects an issue to something outside Waypoint.
type ExternalLink struct {
	ID       int64      `json:"id"`
	Provider string     `json:"provider"` // github, forgejo, url
	Kind     string     `json:"kind"`     // issue, pull, commit, url
	URL      string     `json:"url"`
	Repo     string     `json:"repo"`
	Number   *int64     `json:"number"`
	Title    string     `json:"title"`
	State    string     `json:"state"`
	SyncedAt *time.Time `json:"synced_at"`
	// Mirror: the issue is this forge issue (imported; text, state and
	// comments sync). Otherwise the forge item only mentions the issue.
	Mirror bool `json:"mirror"`
	// Via is how a forge link was made (WP-379): "added" (by hand, or wp
	// ship), or the place in the item's text naming it as the issue's own
	// work (branch, title, body, subject); "" for links from before.
	Via     string `json:"via,omitempty"`
	forgeID int64
}

const forgeCols = `id, kind, name, base_url, token, webhook_secret, auto_projects, created_by, created_at, bot_token, mirror_private, mirror_account`

func (s *Service) scanForge(sc interface{ Scan(...any) error }) (*Forge, error) {
	var f Forge
	var c int64
	var by sql.NullInt64
	var mirror string
	if err := sc.Scan(&f.ID, &f.Kind, &f.Name, &f.BaseURL, &f.token, &f.secret, &f.AutoProjects, &by, &c, &f.botToken, &mirror, &f.MirrorAccount); err != nil {
		return nil, err
	}
	f.MirrorPrivate = strings.Fields(mirror)
	f.createdBy = ptrInt(by)
	f.HasToken, f.HasWebhookSecret, f.CreatedAt = f.token != "", f.secret != "", msTime(c)
	f.HasBotToken = f.botToken != ""
	f.WebhookURL = fmt.Sprintf("%s/api/v1/hooks/forge/%d", s.BaseURL(), f.ID)
	return &f, nil
}

func (s *Service) forges(ctx context.Context) ([]*Forge, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+forgeCols+` FROM forges ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Forge{}
	for rows.Next() {
		f, err := s.scanForge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Service) forgeByID(ctx context.Context, id int64) (*Forge, error) {
	f, err := s.scanForge(s.db.R.QueryRowContext(ctx, `SELECT `+forgeCols+` FROM forges WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("forge")
	}
	return f, err
}

// ListForges lists connected forges. Everyone can see them (to link repos);
// secrets are never returned.
func (s *Service) ListForges(ctx context.Context, actor *Actor) ([]*Forge, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	list, err := s.forges(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range list {
		if f.People, err = s.forgePeople(ctx, f.ID); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// ForgeInput creates or edits a forge connection.
type ForgeInput struct {
	Kind          *string `json:"kind"`
	Name          *string `json:"name"`
	BaseURL       *string `json:"base_url"`
	Token         *string `json:"token"`
	WebhookSecret *string `json:"webhook_secret"`
	AutoProjects  *bool   `json:"auto_projects"`
	// BotToken is a separate bot account's token for reviews and comments;
	// empty clears it.
	BotToken *string `json:"bot_token"`
	// MirrorPrivate replaces the paths a mirror keeps private; empty: none.
	MirrorPrivate *[]string `json:"mirror_private"`
	// MirrorAccount replaces the login the mirror posts as; empty: none.
	MirrorAccount *string `json:"mirror_account"`
}

// mirrorAccountRE is a forge login: GitHub's, Forgejo's and GitLab's, and
// an app's "name[bot]".
var mirrorAccountRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}(\[bot\])?$`)

// mirrorAccount checks the login a mirror posts as.
func mirrorAccount(in string) (string, error) {
	in = strings.TrimSpace(in)
	if in != "" && !mirrorAccountRE.MatchString(in) {
		return "", invalid("mirror_account: %q isn't a forge login", in)
	}
	return in, nil
}

// mirrorPrivate checks the paths a mirror keeps private and stores them one
// a line.
func mirrorPrivate(in []string) (string, error) {
	var globs []string
	for _, g := range in {
		globs = append(globs, strings.Fields(g)...)
	}
	if _, err := newGlobSet(globs); err != nil {
		return "", invalid("mirror_private: %v", err)
	}
	return strings.Join(globs, "\n"), nil
}

// CreateForge connects a forge (admin only, since it holds a token).
func (s *Service) CreateForge(ctx context.Context, actor *Actor, in ForgeInput) (*Forge, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, ErrForbidden
	}
	kind := deref(in.Kind)
	if !slices.Contains(forgeKinds, kind) {
		return nil, invalid("kind must be github, forgejo or gitlab")
	}
	base := strings.TrimRight(deref(in.BaseURL), "/")
	if base == "" || base == "?" {
		switch kind {
		case "github":
			base = "https://github.com"
		case "gitlab":
			base = "https://gitlab.com"
		}
	}
	if err := validForgeURL(base); err != nil {
		return nil, err
	}
	name := deref(in.Name)
	if name == "" {
		name = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	}
	secret := deref(in.WebhookSecret)
	if secret == "" {
		secret = newToken("")[:32]
	}
	auto := in.AutoProjects != nil && *in.AutoProjects
	var mirror string
	if in.MirrorPrivate != nil {
		var err error
		if mirror, err = mirrorPrivate(*in.MirrorPrivate); err != nil {
			return nil, err
		}
	}
	account, err := mirrorAccount(deref(in.MirrorAccount))
	if err != nil {
		return nil, err
	}
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO forges(kind, name, base_url, token, webhook_secret, auto_projects, created_by, created_at, bot_token, mirror_private, mirror_account) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		kind, name, base, deref(in.Token), secret, boolInt(auto), nullUser(actor.id()), s.nowMS(), strings.TrimSpace(deref(in.BotToken)), mirror, account)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	s.audit(ctx, actor, AuditEntry{Action: "forge.create", Category: AuditSettings, TargetType: "forge", TargetID: id,
		Target: name, Detail: "connected " + kindLabel(kind) + " at " + base})
	return s.forgeByID(ctx, id)
}

// UpdateForge edits a forge connection.
func validForgeURL(base string) error {
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return invalid("base_url must be like https://git.example.com")
	}
	return nil
}

func (s *Service) UpdateForge(ctx context.Context, actor *Actor, id int64, in ForgeInput) (*Forge, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, ErrForbidden
	}
	cur, err := s.forgeByID(ctx, id)
	if err != nil {
		return nil, err
	}
	sets, args := []string{}, []any{}
	// What changed, for the log (never the secrets themselves).
	var changes []string
	if in.Name != nil && strings.TrimSpace(*in.Name) != cur.Name {
		changes = append(changes, "renamed from "+cur.Name)
	}
	if in.BaseURL != nil && strings.TrimRight(*in.BaseURL, "/") != cur.BaseURL {
		changes = append(changes, "moved to "+strings.TrimRight(*in.BaseURL, "/"))
	}
	if in.Token != nil {
		changes = append(changes, "changed the token")
	}
	if in.BotToken != nil {
		changes = append(changes, "changed the bot token")
	}
	if in.WebhookSecret != nil {
		changes = append(changes, "changed the webhook secret")
	}
	if in.AutoProjects != nil && *in.AutoProjects != cur.AutoProjects {
		changes = append(changes, map[bool]string{true: "creates projects for new repos", false: "stopped creating projects"}[*in.AutoProjects])
	}
	if in.Name != nil {
		sets, args = append(sets, "name = ?"), append(args, strings.TrimSpace(*in.Name))
	}
	if in.BaseURL != nil {
		base := strings.TrimRight(*in.BaseURL, "/")
		if err := validForgeURL(base); err != nil {
			return nil, err
		}
		sets, args = append(sets, "base_url = ?"), append(args, base)
	}
	if in.Token != nil {
		sets, args = append(sets, "token = ?"), append(args, *in.Token)
	}
	if in.WebhookSecret != nil {
		// An empty secret would make every webhook fail verification.
		if strings.TrimSpace(*in.WebhookSecret) == "" {
			return nil, invalid("webhook secret can't be empty")
		}
		sets, args = append(sets, "webhook_secret = ?"), append(args, *in.WebhookSecret)
	}
	if in.AutoProjects != nil {
		sets, args = append(sets, "auto_projects = ?"), append(args, boolInt(*in.AutoProjects))
	}
	if in.BotToken != nil {
		sets, args = append(sets, "bot_token = ?"), append(args, strings.TrimSpace(*in.BotToken))
	}
	if in.MirrorPrivate != nil {
		mirror, err := mirrorPrivate(*in.MirrorPrivate)
		if err != nil {
			return nil, err
		}
		if mirror != strings.Join(cur.MirrorPrivate, "\n") {
			changes = append(changes, "changed the paths its mirror keeps private")
		}
		sets, args = append(sets, "mirror_private = ?"), append(args, mirror)
	}
	if in.MirrorAccount != nil {
		account, err := mirrorAccount(*in.MirrorAccount)
		if err != nil {
			return nil, err
		}
		if account != cur.MirrorAccount {
			changes = append(changes, "changed the account its mirror posts as")
		}
		sets, args = append(sets, "mirror_account = ?"), append(args, account)
	}
	if len(sets) > 0 {
		err := s.write(ctx, actor, func(t *tx) error {
			// #nosec G202 -- only fixed column names and ? placeholders are joined in; values are bound
			if _, err := t.ExecContext(ctx, `UPDATE forges SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, id)...); err != nil {
				return err
			}
			if len(changes) > 0 {
				name := cur.Name
				if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
					name = strings.TrimSpace(*in.Name)
				}
				t.audit(AuditEntry{Action: "forge.update", Category: AuditSettings, TargetType: "forge", TargetID: id,
					Target: name, Detail: strings.Join(changes, ", ")})
			}
			if in.Token == nil && in.WebhookSecret == nil && in.BotToken == nil {
				return nil
			}
			// A new secret or token (webhooks may be managed with the bot
			// token): refresh the webhooks Waypoint added (or tried to), so
			// they sign with the new secret or get made now.
			return s.refreshRepoHooks(ctx, t, id)
		})
		if err != nil {
			return nil, err
		}
	}
	return s.forgeByID(ctx, id)
}

// ForgeSecret reveals a forge's webhook secret to an admin (to paste into
// the forge's webhook settings).
func (s *Service) ForgeSecret(ctx context.Context, actor *Actor, id int64) (string, error) {
	if !actor.isAdmin() {
		return "", ErrForbidden
	}
	f, err := s.forgeByID(ctx, id)
	if err != nil {
		return "", err
	}
	return f.secret, nil
}

// DeleteForge removes a forge connection.
func (s *Service) DeleteForge(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	if !actor.isAdmin() {
		return ErrForbidden
	}
	f, err := s.forgeByID(ctx, id)
	if err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		// The webhooks Waypoint added would only bounce off from now on.
		rows, err := t.QueryContext(ctx, `SELECT DISTINCT repo, hook_id FROM project_repos WHERE forge_id = ? AND hook_id != ''`, id)
		if err != nil {
			return err
		}
		var jobs []*repoHookJob
		for rows.Next() {
			j := &repoHookJob{remove: f}
			if err := rows.Scan(&j.repo, &j.hookID); err != nil {
				rows.Close()
				return err
			}
			jobs = append(jobs, j)
		}
		rows.Close()
		rows, err = t.QueryContext(ctx, `SELECT org, hook_id FROM forge_org_hooks WHERE forge_id = ? AND hook_id != ''`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			j := &repoHookJob{remove: f, org: true}
			if err := rows.Scan(&j.repo, &j.hookID); err != nil {
				rows.Close()
				return err
			}
			jobs = append(jobs, j)
		}
		rows.Close()
		if _, err := t.ExecContext(ctx, `DELETE FROM forges WHERE id = ?`, id); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "forge.delete", Category: AuditSettings, TargetType: "forge", TargetID: id,
			Target: f.Name, Detail: "disconnected " + kindLabel(f.Kind) + " at " + f.BaseURL})
		for _, j := range jobs {
			t.emit(&Event{Kind: EvRepoHook, repoHook: j})
		}
		return nil
	})
}

// ---- project repos ----

// ProjectRepo attaches a forge repository to a project.
type ProjectRepo struct {
	ID           int64  `json:"id"`
	ForgeID      int64  `json:"forge_id"`
	ForgeName    string `json:"forge_name"`
	Repo         string `json:"repo"`
	SyncIssues   bool   `json:"sync_issues"`
	PostComments bool   `json:"post_comments"` // Waypoint comments go to its issues
	Bots         bool   `json:"bots"`          // automations act on its pull requests (WP-D14)
	// ReviewGate: Waypoint posts a waypoint/review status on its pull
	// requests, failing while findings at or above GateSeverity (critical,
	// high, medium, low) are open (WP-D21).
	ReviewGate   bool   `json:"review_gate"`
	GateSeverity string `json:"gate_severity"`
	// DeleteMergedBranches: Waypoint deletes the repo's merged branches on
	// the forge (WP-140).
	DeleteMergedBranches bool `json:"delete_merged_branches"`
	// LineCandidates: the merge line builds a candidate branch and tests
	// pull requests on it; off, each is tested on its own branch (ADR152).
	LineCandidates bool       `json:"line_candidates"`
	LastSyncedAt   *time.Time `json:"last_synced_at"`
	// Merge automatically and the rules every armed merge waits for (WP-133).
	MergePolicy
	// HookState is the forge webhook Waypoint added: pending, installed or
	// failed ("" when the repo was attached before Waypoint did that).
	HookState     string     `json:"hook_state"`
	HookNote      string     `json:"hook_note,omitempty"` // why it failed, or a warning
	HookCheckedAt *time.Time `json:"hook_checked_at"`
	HookSeenAt    *time.Time `json:"hook_seen_at"` // the last signed delivery about this repo
	// CoveredBy is the organisation whose webhook brings this repo's events
	// (so it has no repo webhook of its own).
	CoveredBy string `json:"covered_by,omitempty"`
	// DefaultBranch is the repo's default branch, when Merge automatically's
	// base scope turns on it and the forge said (WP-1041).
	DefaultBranch string `json:"default_branch,omitempty"`
}

var (
	repoRE   = lazyre.New(`^[\w.-]+/[\w.-]+$`)
	nestedRE = lazyre.New(`^[\w.-]+(/[\w.-]+)+$`) // GitLab: group/subgroup/…/name
)

// validRepoFor accepts owner/name (group/subgroup/…/name on GitLab); "."
// and ".." segments would change the API paths the forge token is sent to.
func validRepoFor(kind, repo string) bool {
	re := repoRE
	if kind == "gitlab" {
		re = nestedRE
	}
	if !re.MatchString(repo) || len(repo) > 255 {
		return false
	}
	for _, seg := range strings.Split(repo, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// ListProjectRepos lists repos attached to a project.
func (s *Service) ListProjectRepos(ctx context.Context, actor *Actor, projectRef string) ([]ProjectRepo, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, r.forge_id, f.name, r.repo, r.sync_issues, r.post_comments, r.bots, r.review_gate, r.gate_severity, r.last_synced_at,
		r.hook_state, r.hook_note, r.hook_checked_at, r.hook_seen_at,
		r.auto_merge, r.auto_merge_into, r.auto_merge_bases, r.required_checks, r.review_wait_minutes, r.quiet_minutes, r.external_reviewers, r.delete_merged_branches, r.line_candidates
		FROM project_repos r JOIN forges f ON f.id = r.forge_id WHERE r.project_id = ? ORDER BY r.repo`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectRepo{}
	for rows.Next() {
		var r ProjectRepo
		var last, checked, seen sql.NullInt64
		var auto bool
		var into, bases, required string
		var wait, quiet int
		var reviewers sql.NullString
		if err := rows.Scan(&r.ID, &r.ForgeID, &r.ForgeName, &r.Repo, &r.SyncIssues, &r.PostComments, &r.Bots, &r.ReviewGate, &r.GateSeverity, &last,
			&r.HookState, &r.HookNote, &checked, &seen, &auto, &into, &bases, &required, &wait, &quiet, &reviewers, &r.DeleteMergedBranches, &r.LineCandidates); err != nil {
			return nil, err
		}
		r.MergePolicy = readPolicy(auto, into, bases, required, wait, quiet, reviewers)
		r.LastSyncedAt, r.HookCheckedAt, r.HookSeenAt = ptrTime(last), ptrTime(checked), ptrTime(seen)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if f, err := s.forgeByID(ctx, out[i].ForgeID); err == nil {
			out[i].CoveredBy = s.coveringOrg(ctx, f, out[i].Repo)
		}
		if out[i].AutoMerge && out[i].needsDefault() {
			out[i].DefaultBranch, _ = s.repoDefaultBranch(ctx, out[i].ForgeID, out[i].Repo, false)
		}
	}
	return out, nil
}

// AddProjectRepo attaches a repo; with syncIssues its issues are imported
// and kept in sync both ways.
func (s *Service) AddProjectRepo(ctx context.Context, actor *Actor, projectRef string, forgeID int64, repo string, syncIssues bool) (*ProjectRepo, error) {
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
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return nil, err
	}
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if !validRepoFor(f.Kind, repo) {
		if f.Kind == "gitlab" {
			return nil, invalid("repo must look like group/name (subgroups are fine)")
		}
		return nil, invalid("repo must look like owner/name")
	}
	// The forge webhook is added (or refreshed) after commit, in dispatch.
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO project_repos(project_id, forge_id, repo, sync_issues, created_at, hook_state) VALUES (?,?,?,?,?,?)
			ON CONFLICT(project_id, forge_id, repo) DO UPDATE SET sync_issues = excluded.sync_issues, hook_state = excluded.hook_state, hook_note = ''`,
			p.ID, f.ID, repo, boolInt(syncIssues), s.nowMS(), hookPending); err != nil {
			return err
		}
		// Attached on purpose: automatic projects may pick it up again later.
		if _, err := t.ExecContext(ctx, `DELETE FROM forge_repo_ignores WHERE forge_id = ? AND repo = ?`, f.ID, repo); err != nil {
			return err
		}
		// LastInsertId is unreliable on the upsert path; look the row up.
		if err := t.QueryRowContext(ctx, `SELECT id FROM project_repos WHERE project_id = ? AND forge_id = ? AND repo = ?`, p.ID, f.ID, repo).Scan(&id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvRepoHook, ProjectID: p.ID, repoHook: &repoHookJob{install: id}})
		t.emit(&Event{Kind: EvRepoAttached, ProjectID: p.ID, New: repo, Data: map[string]any{"repo": repo, "forge": f.Name}})
		detail := "attached from " + f.Name
		if syncIssues {
			detail += ", syncing issues"
		}
		t.audit(AuditEntry{Action: "repo.attach", Category: AuditSettings, ProjectID: p.ID, TargetType: "repo", TargetID: id,
			Target: repo, Detail: detail})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if syncIssues {
		s.goBG(func() {
			if err := s.SyncRepo(context.Background(), id); err != nil {
				s.log.WarnContext(ctx, "initial repo sync failed", "repo", repo, "err", err)
			}
		})
	}
	pol := MergePolicy{AutoMerge: true, RequiredChecks: []string{}, ReviewWaitMinutes: defaultReviewWait, QuietMinutes: defaultQuiet, ExternalReviewers: []string{}}
	return &ProjectRepo{ID: id, ForgeID: f.ID, ForgeName: f.Name, Repo: repo, SyncIssues: syncIssues, HookState: hookPending, MergePolicy: pol}, nil
}

// UpdateProjectRepo switches issue sync or comment posting for an attached repo.
func (s *Service) UpdateProjectRepo(ctx context.Context, actor *Actor, projectRef string, id int64, syncIssues, postComments, bots *bool) error {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return err
	}
	sets, args := []string{}, []any{}
	var changes []string
	onOff := func(on bool, what string) string {
		return map[bool]string{true: "turned on ", false: "turned off "}[on] + what
	}
	if syncIssues != nil {
		sets, args = append(sets, "sync_issues = ?"), append(args, boolInt(*syncIssues))
		changes = append(changes, onOff(*syncIssues, "issue sync"))
	}
	if postComments != nil {
		sets, args = append(sets, "post_comments = ?"), append(args, boolInt(*postComments))
		changes = append(changes, onOff(*postComments, "posting comments"))
	}
	if bots != nil {
		sets, args = append(sets, "bots = ?"), append(args, boolInt(*bots))
		changes = append(changes, onOff(*bots, "bots"))
	}
	if len(sets) == 0 {
		return nil
	}
	return s.write(ctx, actor, func(t *tx) error {
		var repo string
		if err := t.QueryRowContext(ctx, `SELECT repo FROM project_repos WHERE id = ? AND project_id = ?`, id, p.ID).Scan(&repo); err != nil {
			return notFound("project repo")
		}
		// #nosec G202 -- only fixed column names and ? placeholders are joined in
		if _, err := t.ExecContext(ctx, `UPDATE project_repos SET `+strings.Join(sets, ", ")+` WHERE id = ? AND project_id = ?`, append(args, id, p.ID)...); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "repo.update", Category: AuditSettings, ProjectID: p.ID, TargetType: "repo", TargetID: id,
			Target: repo, Detail: strings.Join(changes, ", ")})
		return nil
	})
}

// RemoveProjectRepo detaches a repo (imported issues stay). Automatic
// projects remember that, so the repo isn't attached again on the next
// discovery.
func (s *Service) RemoveProjectRepo(ctx context.Context, actor *Actor, projectRef string, id int64) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		var forgeID int64
		var repo, hookID string
		if err := t.QueryRowContext(ctx, `SELECT forge_id, repo, hook_id FROM project_repos WHERE id = ? AND project_id = ?`, id, p.ID).Scan(&forgeID, &repo, &hookID); err != nil {
			return notFound("project repo")
		}
		if f, err := s.forgeByID(ctx, forgeID); err == nil && hookID != "" {
			// Removed once no other project has the repo.
			t.emit(&Event{Kind: EvRepoHook, ProjectID: p.ID, repoHook: &repoHookJob{remove: f, repo: repo, hookID: hookID}})
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM project_repos WHERE id = ?`, id); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "repo.detach", Category: AuditSettings, ProjectID: p.ID, TargetType: "repo", TargetID: id,
			Target: repo, Detail: "detached; its imported issues stay"})
		// Only when no other project still has it: then it's really unwanted.
		_, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO forge_repo_ignores(forge_id, repo, created_at)
			SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?))`,
			forgeID, repo, s.nowMS(), forgeID, repo)
		return err
	})
}

// ---- links ----

var (
	ghPathRE    = lazyre.New(`^/([\w.-]+/[\w.-]+)/(issues|pull|pulls|commit)/([0-9a-fA-F]+)/?$`)
	glPathRE    = lazyre.New(`^/([\w.-]+(?:/[\w.-]+)+)/-/(issues|merge_requests|commit)/([0-9a-fA-F]+)/?$`)
	issueKeyRE  = lazyre.New(`\b([A-Z][A-Z0-9]{1,9})-(\d+)\b`)
	branchKeyRE = lazyre.New(`(?i)\b([a-z][a-z0-9]{1,9})-(\d+)\b`)
)

// parseLink recognises GitHub, Forgejo and GitLab issue/PR/commit URLs.
func (s *Service) parseLink(ctx context.Context, raw string) (ExternalLink, error) {
	l := ExternalLink{Provider: "url", Kind: "url", URL: strings.TrimSpace(raw)}
	u, err := url.Parse(l.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return l, invalid("not a valid http(s) URL")
	}
	forges, rerr := s.forges(ctx)
	if rerr != nil {
		if readFault(rerr) {
			s.log.WarnContext(ctx, "forges failed", "err", rerr)
		}
	}
	var forge *Forge
	for _, f := range forges {
		fu, err := url.Parse(f.BaseURL)
		if err == nil && strings.EqualFold(fu.Host, u.Host) {
			forge = f
			break
		}
	}
	kind := ""
	switch {
	case forge != nil:
		kind = forge.Kind
	case strings.EqualFold(u.Host, "github.com"):
		kind = "github"
	case strings.EqualFold(u.Host, "gitlab.com"):
		kind = "gitlab"
	default:
		return l, nil
	}
	re := ghPathRE
	if kind == "gitlab" {
		re = glPathRE
	}
	m := re.FindStringSubmatch(u.Path)
	if m == nil {
		return l, nil
	}
	l.Provider = kind
	if forge != nil {
		l.forgeID = forge.ID
	}
	if !validRepoFor(kind, m[1]) {
		return l, nil
	}
	l.Repo = m[1]
	switch m[2] {
	case "issues":
		l.Kind = "issue"
	case "pull", "pulls", "merge_requests":
		l.Kind = "pull"
	case "commit":
		l.Kind = "commit"
		l.Title = m[3][:min(len(m[3]), 10)]
		return l, nil
	}
	n, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil || n <= 0 {
		// Not an issue or PR number (hex is only valid for commits).
		return ExternalLink{Provider: "url", Kind: "url", URL: l.URL}, nil
	}
	l.Number = &n
	return l, nil
}

func (s *Service) externalLinks(ctx context.Context, q queryer, issueID int64) ([]ExternalLink, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, provider, kind, url, repo, number, title, state, synced_at, mirror, via FROM external_links WHERE issue_id = ? ORDER BY id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExternalLink{}
	for rows.Next() {
		var l ExternalLink
		var num, synced sql.NullInt64
		if err := rows.Scan(&l.ID, &l.Provider, &l.Kind, &l.URL, &l.Repo, &num, &l.Title, &l.State, &synced, &l.Mirror, &l.Via); err != nil {
			return nil, err
		}
		l.Number, l.SyncedAt = ptrInt(num), ptrTime(synced)
		out = append(out, l)
	}
	return out, rows.Err()
}

// AddLink links an issue to a URL; forge issues and PRs get their title and
// state fetched.
func (s *Service) AddLink(ctx context.Context, actor *Actor, issueRef, rawURL, title string) (*ExternalLink, error) {
	l, err := s.parseLink(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	if title != "" {
		l.Title = title
	}
	var issueID int64
	err = s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, issueRef, RoleEditor, false)
		if err != nil {
			return err
		}
		issueID = i.ID
		// An agent linking its pull request is how its work is traced to the
		// PR (WP-D15), so the link remembers which agent made it.
		// Added, it stays however the item's text changes (WP-379).
		res, err := t.ExecContext(ctx, `INSERT INTO external_links(issue_id, provider, kind, url, repo, number, title, state, created_at, created_agent, via) VALUES (?,?,?,?,?,?,?,?,?,?,'added')
			ON CONFLICT(issue_id, url) DO UPDATE SET title = CASE WHEN excluded.title != '' THEN excluded.title ELSE title END,
				created_agent = CASE WHEN created_agent = '' THEN excluded.created_agent ELSE created_agent END, via = 'added'`,
			i.ID, l.Provider, l.Kind, l.URL, l.Repo, nullInt(l.Number), l.Title, l.State, s.nowMS(), actor.agentName())
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM issue_unlinks WHERE issue_id = ? AND url = ?`, i.ID, l.URL); err != nil {
			return err
		}
		l.ID, _ = res.LastInsertId()
		t.emit(&Event{Kind: EvIssueLinked, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: l.Kind, New: l.URL})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if l.Number != nil && l.Provider != "url" {
		s.goBG(func() {
			if err := s.RefreshLinks(context.Background(), issueID); err != nil {
				s.log.WarnContext(ctx, "refresh links", "issue", issueID, "err", err)
			}
		})
	}
	return &l, nil
}

// RemoveLink deletes an external link. Removing a pull request link that
// moved its issue (put it In Review) puts the issue back where it was, as
// long as nobody has moved it since and no other open or merged pull
// request is linked to it (WP-162).
func (s *Service) RemoveLink(ctx context.Context, actor *Actor, issueRef string, linkID int64) error {
	return s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, issueRef, RoleEditor, false)
		if err != nil {
			return err
		}
		var kind, linkURL string
		var mirror bool
		var from, to sql.NullInt64
		err = t.QueryRowContext(ctx, `SELECT kind, url, mirror, moved_from, moved_to FROM external_links WHERE id = ? AND issue_id = ?`, linkID, i.ID).Scan(&kind, &linkURL, &mirror, &from, &to)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("link")
		} else if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM external_links WHERE id = ? AND issue_id = ?`, linkID, i.ID); err != nil {
			return err
		}
		if !mirror && (kind == "pull" || kind == "commit" || kind == "issue") {
			// Its text is scanned again on every change: it must not link
			// the item back (WP-379). It stays a mention.
			var by any
			if actor.User != nil && actor.User.ID != 0 {
				by = actor.User.ID
			}
			if _, err := t.ExecContext(ctx, `INSERT OR REPLACE INTO issue_unlinks(issue_id, url, user_id, agent, created_at) VALUES (?,?,?,?,?)`,
				i.ID, linkURL, by, actor.agentName(), s.nowMS()); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
		if kind != "pull" || !from.Valid || !to.Valid || i.Status.ID != to.Int64 {
			return nil
		}
		var others int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM external_links WHERE issue_id = ? AND kind = 'pull' AND state IN ('', 'open', 'draft', 'merged')`, i.ID).Scan(&others); err != nil {
			return err
		}
		if others > 0 {
			return nil
		}
		back, err := s.statusByID(ctx, t, from.Int64)
		if err != nil {
			return nil // the status is gone: leave the issue alone
		}
		var pid int64
		if err := t.QueryRowContext(ctx, `SELECT project_id FROM statuses WHERE id = ?`, back.ID).Scan(&pid); err != nil || pid != i.ProjectID {
			return nil
		}
		var sets []string
		var args []any
		if err := s.setStatus(ctx, t, i, back, &sets, &args); err != nil {
			return err
		}
		sets, args = append(sets, "updated_at = ?"), append(args, s.nowMS(), i.ID)
		_, err = t.ExecContext(ctx, `UPDATE issues SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		return err
	})
}

// forgeForLink finds the forge a link belongs to.
func (s *Service) forgeForLink(ctx context.Context, l ExternalLink) *Forge {
	u, err := url.Parse(l.URL)
	if err != nil {
		return nil
	}
	forges, rerr := s.forges(ctx)
	if rerr != nil {
		if readFault(rerr) {
			s.log.WarnContext(ctx, "forges failed", "err", rerr)
		}
	}
	for _, f := range forges {
		fu, err := url.Parse(f.BaseURL)
		if err == nil && strings.EqualFold(fu.Host, u.Host) {
			return f
		}
	}
	// Unauthenticated access works for public repos.
	if strings.EqualFold(u.Host, "github.com") {
		return &Forge{Kind: "github", BaseURL: "https://github.com"}
	}
	if strings.EqualFold(u.Host, "gitlab.com") {
		return &Forge{Kind: "gitlab", BaseURL: "https://gitlab.com"}
	}
	return nil
}

// RefreshLinks re-fetches title and state for an issue's forge links. The
// forge's token is only used for repos attached to the issue's project:
// anyone who can edit an issue can paste a link, and the token must not
// read private repos on their behalf. Other links are fetched anonymously,
// which works for public repos and leaves the rest as bare links.
func (s *Service) RefreshLinks(ctx context.Context, issueID int64) error {
	links, err := s.externalLinks(ctx, s.db.R, issueID)
	if err != nil {
		return err
	}
	var projectID int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE id = ?`, issueID).Scan(&projectID); err != nil {
		return err
	}
	for _, l := range links {
		if l.Number == nil || (l.Kind != "issue" && l.Kind != "pull") {
			continue
		}
		f := s.forgeForLink(ctx, l)
		if f == nil {
			continue
		}
		if f.ID == 0 || !s.repoAttached(ctx, projectID, f.ID, l.Repo) {
			anon := *f
			anon.token = ""
			f = &anon
		}
		item, err := s.Forges.GetItem(ctx, f, l.Repo, int(*l.Number), l.Kind == "pull")
		if err != nil {
			s.log.Debug("refresh link", "url", l.URL, "err", err)
			continue
		}
		s.applyLinkState(ctx, l.URL, s.linkItem(ctx, f, item))
	}
	return nil
}

// repoAttached: the project has this repo attached from this forge.
func (s *Service) repoAttached(ctx context.Context, projectID, forgeID int64, repo string) bool {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_repos WHERE project_id = ? AND forge_id = ? AND lower(repo) = lower(?)`,
		projectID, forgeID, repo).Scan(&n)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read repo attached", "forgeID", forgeID, "repo", repo, "err", err)
		}
		return false
	}
	return n > 0
}

// applyLinkState updates every link to url with a forge item's state and
// emits events when a PR opens, merges or closes.
func (s *Service) applyLinkState(ctx context.Context, linkURL string, item *ForgeItem) {
	if item.Confidential {
		return // not even its title
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE forge_mentions SET title = ?, state = ? WHERE item_url = ? AND (title != ? OR state != ?)`,
		item.Title, item.State, linkURL, item.Title, item.State); err != nil {
		s.log.WarnContext(ctx, "apply link state", "err", err)
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT el.id, el.issue_id, el.kind, el.state FROM external_links el WHERE el.url = ?`, linkURL)
	if err != nil {
		return
	}
	type row struct {
		id, issueID int64
		kind, state string
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.issueID, &r.kind, &r.state); err != nil {
			s.log.WarnContext(ctx, "apply link state", "err", err)
			continue
		}
		list = append(list, r)
	}
	rows.Close()
	for _, r := range list {
		if r.state == item.State {
			if _, err := s.db.W.ExecContext(ctx, `UPDATE external_links SET title = ?, synced_at = ? WHERE id = ?`, item.Title, s.nowMS(), r.id); err != nil {
				s.log.WarnContext(ctx, "apply link state", "err", err)
			}
			continue
		}
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			if _, err := t.ExecContext(ctx, `UPDATE external_links SET title = ?, state = ?, synced_at = ? WHERE id = ?`, item.Title, item.State, s.nowMS(), r.id); err != nil {
				return err
			}
			i, err := s.issueByID(ctx, t, r.issueID, false)
			if err != nil {
				return err
			}
			kind := ""
			switch {
			case item.IsPR && item.State == "merged":
				kind = EvExternalPRMerged
			case item.IsPR && item.State == "closed":
				kind = EvExternalPRClosed
			case item.IsPR && item.State == "open": // new, reopened, or out of draft
				// Not while no person approved an outside one: approving it runs
				// this. Read in this write, so whichever of the two commits
				// second sees the other; a failed read rolls the state back for
				// the next sync (WP-1802).
				opened, err := s.emitPullOpened(ctx, t, r.issueID, linkURL, r.state, item.Title, item.Number, false)
				if err != nil || opened {
					return err
				}
			case !item.IsPR && item.State == "closed":
				kind = EvExternalIssueClose
			}
			if kind != "" {
				// New is the link's own URL, so a rule's move through it is
				// remembered on that link (WP-162) even when the forge's item
				// spells its URL differently.
				t.emit(&Event{Kind: kind, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, New: linkURL, Old: r.state,
					Data: map[string]any{"title": item.Title, "number": item.Number}})
			} else {
				t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
			}
			return nil
		})
		if err != nil {
			s.log.WarnContext(ctx, "apply link state", "err", err)
		}
	}
}

// ---- inbound webhooks ----

// HandleForgeWebhook verifies and processes a webhook from a forge.
func (s *Service) HandleForgeWebhook(ctx context.Context, forgeID int64, h http.Header, body []byte) error {
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return err
	}
	if f.secret == "" {
		return ErrUnauthorized
	}
	var ev *forgeEvent
	if f.Kind == "gitlab" {
		// GitLab sends the secret itself rather than a signature.
		if subtle.ConstantTimeCompare([]byte(h.Get("X-Gitlab-Token")), []byte(f.secret)) != 1 {
			return ErrUnauthorized
		}
		ev, err = parseGitLabHook(body)
	} else {
		mac := hmac.New(sha256.New, []byte(f.secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		got := strings.TrimPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
		if got == "" {
			got = h.Get("X-Forgejo-Signature")
		}
		if got == "" {
			got = h.Get("X-Gitea-Signature")
		}
		if !hmac.Equal([]byte(got), []byte(want)) {
			return ErrUnauthorized
		}
		ev, err = parseGitHubHook(cmp(h.Get("X-GitHub-Event"), cmp(h.Get("X-Forgejo-Event"), h.Get("X-Gitea-Event"))), body)
	}
	if err != nil {
		return invalid("bad payload: %v", err)
	}
	if ev == nil {
		return nil
	}
	s.noteHookDelivery(ctx, f, ev.repo, ev.org)
	return s.handleForgeEvent(ctx, f, ev)
}

// forgeEvent is a webhook, whichever forge sent it.
type forgeEvent struct {
	kind    string // ping, pull, issue, push, comment
	repo    string
	repoID  int64  // the forge's id for the repo, which survives renames (WP-166)
	group   *bool  // whether the repo's owner is an organisation or group, if the payload says
	org     string // an organisation hook's ping: the organisation
	item    *ForgeItem
	pr      *ForgePR // a pull request event's details, for bots
	branch  string
	commits []forgeCommit
	comment *forgeCommentEvent
	review  *forgeReviewEvent
	sender  string // who caused it, when the payload says
	// push (WP-D21): the ref pushed, the commits before and after, who
	// pushed and whether it made or deleted the branch.
	ref, before, after, pusher string
	created, deleted           bool
	defaultBranch              string // the repo's, when the payload says
	// ci: the commit whose checks changed ("" when the payload doesn't say).
	sha string
	// release: a release published.
	release *forgeRelease
}

type forgeCommit struct {
	id, message, url string
	at               string   // its timestamp as the payload gives it (RFC 3339)
	author           string   // its author's forge login, when the forge knows it
	paths            []string // files the commit added, removed or changed
}

// forgeRelease is a published release.
type forgeRelease struct{ tag, name, url string }

// forgeReviewEvent is a review, or a comment on a pull request's diff.
type forgeReviewEvent struct {
	number int
	kind   string // approved, changes_requested, reviewed, commented
	sha    string
	ForgeComment
}

type forgeCommentEvent struct {
	action string // created, edited, deleted
	number int    // the issue it's on
	onPR   bool
	ForgeComment
}

func (s *Service) handleForgeEvent(ctx context.Context, f *Forge, ev *forgeEvent) error {
	if ev.repo != "" {
		s.followRepo(ctx, f, ev.repo, ev.repoID)
	}
	if ev.repo != "" && f.AutoProjects {
		if err := s.ensureRepoProject(ctx, f, ev.repo, ev.repoID, ev.group); err != nil {
			s.log.WarnContext(ctx, "auto project", "forge", f.Name, "repo", ev.repo, "err", err)
		}
	}
	switch ev.kind {
	case "pull":
		if ev.branch == "" && ev.item != nil {
			// A payload without its branch must not make the scan take back
			// the links the branch made: use the one recorded before.
			_ = s.db.R.QueryRowContext(ctx, `SELECT head_ref FROM pull_requests WHERE forge_id = ? AND url = ?`, f.ID, ev.item.URL).Scan(&ev.branch)
		}
		if ev.pr != nil && !s.onForgePull(ctx, f, ev.repo, ev.pr) {
			// Not recorded, so not known to be from a person: its links set
			// nothing off until a sync records it (WP-1802).
			s.onPRActivity(ctx, f, ev.repo, ev.review)
			return nil
		}
		// Links on issues show its title only when a Waypoint person wrote
		// it, or a person approved it (WP-281).
		link := s.linkItem(ctx, f, ev.item)
		// Its own work is linked, other keys are mentions (WP-379); a
		// closed pull request links nothing new.
		open := ev.item.State == "open" || ev.item.State == "draft" || ev.item.State == "merged"
		if _, err := s.scanPull(ctx, f, ev.repo, ev.item, link, ev.branch, open); err != nil {
			s.log.WarnContext(ctx, "record forge mentions", "url", ev.item.URL, "err", err)
		}
		s.applyLinkState(ctx, ev.item.URL, link)
		s.onPRActivity(ctx, f, ev.repo, ev.review) // GitLab approvals come as merge request events
	case "review":
		if ev.review.Body != "" {
			s.scanComment(ctx, f, ev.repo, true, ev.review.number, "review", "created", ev.review.ForgeComment)
		}
		s.onPRActivity(ctx, f, ev.repo, ev.review)
		s.checkAfterActivity(ctx, f, ev.repo, ev.review.number)
	case "issue":
		// Links on other issues show its title only when a Waypoint person
		// wrote it (WP-279).
		link := s.linkItem(ctx, f, ev.item)
		if !ev.item.Confidential {
			if _, err := s.scanForgeIssue(ctx, f, ev.repo, ev.item, link); err != nil {
				s.log.WarnContext(ctx, "record forge mentions", "url", ev.item.URL, "err", err)
			}
		}
		if err := s.importForgeIssue(ctx, f, ev.repo, *ev.item); err != nil {
			return err
		}
		s.applyLinkState(ctx, ev.item.URL, link)
	case "push":
		for _, c := range ev.commits {
			if _, err := s.scanCommit(ctx, f, ev.repo, c); err != nil {
				s.log.WarnContext(ctx, "record forge mentions", "url", c.url, "err", err)
			}
		}
		s.onPush(ctx, f, ev)
	case "ci":
		s.onForgeCI(ctx, f, ev.repo, ev.sha)
	case "release":
		s.onRelease(ctx, f, ev)
	case "comment":
		s.scanComment(ctx, f, ev.repo, ev.comment.onPR, ev.comment.number, "comment", ev.comment.action, ev.comment.ForgeComment)
		if !ev.comment.onPR {
			return s.importForgeComment(ctx, f, ev.repo, ev.comment.number, ev.comment.action, ev.comment.ForgeComment)
		}
		if ev.comment.action == "created" {
			s.onPRComment(ctx, f, ev.repo, ev.comment.number, ev.comment.ForgeComment)
			s.onPRActivity(ctx, f, ev.repo, &forgeReviewEvent{number: ev.comment.number, kind: "commented", ForgeComment: ev.comment.ForgeComment})
			s.checkAfterActivity(ctx, f, ev.repo, ev.comment.number)
		}
	}
	return nil
}

// parseGitHubHook reads a GitHub or Forgejo/Gitea webhook (same shapes).
func parseGitHubHook(event string, body []byte) (*forgeEvent, error) {
	type user struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	}
	var p struct {
		Action      string `json:"action"`
		PullRequest *apiPR `json:"pull_request"`
		Sender      *user  `json:"sender"`
		// review_requested (GitHub; Forgejo sends it as a pull_request
		// event too): who was asked.
		RequestedReviewer *user `json:"requested_reviewer"`
		// GitHub's pull_request_review: the review; Forgejo's review events:
		// its kind and text.
		Review   *hookReview `json:"review"`
		CommitID string      `json:"commit_id"` // Forgejo: the head a review was on
		IsPull   bool        `json:"is_pull"`   // Forgejo: an issue_comment on a pull request
		Issue    *struct {
			Number  int    `json:"number"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			State   string `json:"state"`
			Labels  []struct {
				Name string `json:"name"`
			} `json:"labels"`
			PullRequest any  `json:"pull_request"`
			User        user `json:"user"`
		} `json:"issue"`
		Comment *hookComment `json:"comment"`
		Commits []struct {
			ID        string `json:"id"`
			Message   string `json:"message"`
			URL       string `json:"url"`
			Timestamp string `json:"timestamp"`
			Author    struct {
				Username string `json:"username"`
			} `json:"author"`
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		} `json:"commits"`
		Before  string `json:"before"`
		After   string `json:"after"`
		Ref     string `json:"ref"`
		Created bool   `json:"created"` // push: GitHub says when it made the branch
		Deleted bool   `json:"deleted"`
		Pusher  *struct {
			Login    string `json:"login"`
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"pusher"`
		// Checks: GitHub's status, check_run, check_suite and workflow_run, and
		// Forgejo's action_run_* carry the commit.
		SHA      string `json:"sha"`
		CheckRun *struct {
			HeadSHA string `json:"head_sha"`
		} `json:"check_run"`
		CheckSuite *struct {
			HeadSHA string `json:"head_sha"`
		} `json:"check_suite"`
		WorkflowRun *struct {
			HeadSHA string `json:"head_sha"`
		} `json:"workflow_run"`
		Run *struct {
			CommitSHA string `json:"commit_sha"`
			HeadSHA   string `json:"head_sha"`
			SHA       string `json:"sha"`
		} `json:"run"`
		Release *struct {
			TagName    string `json:"tag_name"`
			Name       string `json:"name"`
			HTMLURL    string `json:"html_url"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		} `json:"release"`
		Organization *struct {
			Login    string `json:"login"`
			Username string `json:"username"`
		} `json:"organization"`
		Repository struct {
			ID            int64  `json:"id"`
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
			Owner         struct {
				Type string `json:"type"` // GitHub: Organization or User
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	ev := &forgeEvent{repo: p.Repository.FullName, repoID: p.Repository.ID}
	if p.Sender != nil {
		ev.sender = cmp(p.Sender.Login, p.Sender.Username)
	}
	switch p.Repository.Owner.Type {
	case "Organization":
		ev.group = ptr(true)
	case "User":
		ev.group = ptr(false)
	}
	switch event {
	case "ping":
		ev.kind = "ping"
		if p.Organization != nil && ev.repo == "" {
			ev.org = cmp(p.Organization.Login, p.Organization.Username)
		}
	case "pull_request":
		if p.PullRequest == nil {
			return nil, nil
		}
		pr := p.PullRequest.pr()
		pr.actor = ev.sender
		if p.Action == "review_requested" && p.RequestedReviewer != nil {
			if who := cmp(p.RequestedReviewer.Login, p.RequestedReviewer.Username); who != "" {
				pr.requested = []string{who}
			}
		}
		ev.kind, ev.branch, ev.pr = "pull", pr.HeadRef, pr
		ev.item = &ForgeItem{Number: pr.Number, Title: stripWIP(pr.Title), Body: pr.Body, State: pr.State, URL: pr.URL, IsPR: true, Author: pr.Author}
	case "pull_request_review", "pull_request_review_comment",
		// Forgejo and Gitea: a review approving, asking for changes, or commenting.
		"pull_request_approved", "pull_request_rejected", "pull_request_comment",
		"pull_request_review_approved", "pull_request_review_rejected":
		if p.PullRequest == nil {
			return nil, nil
		}
		rv := parsePRReview(event, p.Action, p.PullRequest.Number, ev.sender, p.CommitID, p.Review, p.Comment)
		if rv == nil {
			return nil, nil
		}
		ev.kind, ev.review = "review", rv
	case "issues":
		is := p.Issue
		if is == nil || is.PullRequest != nil {
			return nil, nil
		}
		ev.kind = "issue"
		ev.item = &ForgeItem{Number: is.Number, Title: is.Title, Body: is.Body, State: is.State, URL: is.HTMLURL, Author: cmp(is.User.Login, is.User.Username)}
		for _, l := range is.Labels {
			ev.item.Labels = append(ev.item.Labels, l.Name)
		}
	case "issue_comment":
		if p.Issue == nil || p.Comment == nil {
			return nil, nil
		}
		action := p.Action
		if action != "edited" && action != "deleted" {
			action = "created"
		}
		ev.kind = "comment"
		ev.comment = &forgeCommentEvent{action: action, number: p.Issue.Number, onPR: p.Issue.PullRequest != nil || p.IsPull,
			ForgeComment: ForgeComment{ID: strconv.FormatInt(p.Comment.ID, 10), Author: cmp(p.Comment.User.Login, p.Comment.User.Username),
				Body: p.Comment.Body, URL: p.Comment.HTMLURL, UpdatedAt: p.Comment.UpdatedAt}}
	case "push":
		ev.kind = "push"
		if p.Before != "" && p.Before == p.After {
			// Forgejo's "test delivery": the latest commit, pushed again.
			ev.kind = "ping"
			break
		}
		ev.ref, ev.before, ev.after, ev.defaultBranch = p.Ref, p.Before, p.After, p.Repository.DefaultBranch
		ev.created = p.Created || (p.Before != "" && strings.Trim(p.Before, "0") == "")
		ev.deleted = p.Deleted || (p.After != "" && strings.Trim(p.After, "0") == "")
		if p.Pusher != nil {
			ev.pusher = cmp(p.Pusher.Login, cmp(p.Pusher.Username, p.Pusher.Name))
		}
		ev.pusher = cmp(ev.pusher, ev.sender)
		for _, c := range p.Commits {
			paths := append(append(slices.Clone(c.Added), c.Modified...), c.Removed...)
			ev.commits = append(ev.commits, forgeCommit{id: c.ID, message: c.Message, url: c.URL, author: c.Author.Username, at: c.Timestamp, paths: paths})
		}
	case "status", "check_run", "check_suite", "workflow_run", "action_run_failure", "action_run_recover", "action_run_success":
		ev.kind = "ci"
		switch {
		case p.CheckRun != nil:
			ev.sha = p.CheckRun.HeadSHA
		case p.CheckSuite != nil:
			ev.sha = p.CheckSuite.HeadSHA
		case p.WorkflowRun != nil:
			ev.sha = p.WorkflowRun.HeadSHA
		case p.Run != nil:
			ev.sha = cmp(p.Run.CommitSHA, cmp(p.Run.HeadSHA, p.Run.SHA))
		default:
			ev.sha = p.SHA
		}
	case "release":
		if p.Release == nil || p.Release.Draft || (p.Action != "published" && p.Action != "released" && p.Action != "created") {
			return nil, nil
		}
		ev.kind = "release"
		ev.release = &forgeRelease{tag: p.Release.TagName, name: cmp(p.Release.Name, p.Release.TagName), url: p.Release.HTMLURL}
	default:
		return nil, nil
	}
	return ev, nil
}

func ptr[T any](v T) *T { return &v }

// ---- issue sync ----

// importForgeIssue creates or updates the Waypoint issue mirroring a forge
// issue in every project syncing that repo. Imports of one forge issue (and
// of its twins on mirror forges, which share owner/name and number) run one
// at a time, so a webhook racing a sync can't create two copies.
func (s *Service) importForgeIssue(ctx context.Context, f *Forge, repo string, item ForgeItem) error {
	unlock := forgeLocks.lock(fmt.Sprintf("issue|%s|%d", strings.ToLower(repo), item.Number))
	defer unlock()
	// Forges allow longer text than Waypoint does: a title that is too long
	// keeps its whole text at the start of the description (WP-175); the body
	// is cut only past the description limit.
	if len(item.Title) > maxTitleLen {
		item.Title, item.Body = splitTitle(item.Title, item.Body, true)
	}
	item.Body = truncateBytes(item.Body, maxDescriptionLen)
	rows, err := s.db.R.QueryContext(ctx, `SELECT project_id FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?) AND sync_issues = 1`, f.ID, repo)
	if err != nil {
		return err
	}
	var projects []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, id)
	}
	rows.Close()
	sys := &Actor{User: SystemActor().User, ReadOnly: false}
	sys.User.DisplayName = f.Name
	// Who isn't a Waypoint person writes only drafts and held text, which
	// nothing but people read until a person approves it (WP-279). A login
	// agents may write as writes unreviewed agent text (WP-282).
	outside, agent := s.forgeWriter(ctx, s.db.R, f, item.Author)
	writer := *sys
	writer.textAgent = agent
	for _, pid := range projects {
		var issueID int64
		err := s.db.R.QueryRowContext(ctx, `SELECT el.issue_id FROM external_links el JOIN issues i ON i.id = el.issue_id
			WHERE el.url = ? AND el.mirror = 1 AND i.project_id = ?`, item.URL, pid).Scan(&issueID)
		if item.Confidential {
			// Made confidential on GitLab: keep the task, drop what it said.
			if err == nil {
				if err := s.hideConfidential(ctx, sys, f, issueID, item); err != nil {
					return err
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			continue
		}
		if errors.Is(err, sql.ErrNoRows) {
			// The same issue on a mirror of this repo (another forge): same
			// number and title. Link it there rather than import a twin.
			err = s.db.R.QueryRowContext(ctx, `SELECT el.issue_id FROM external_links el JOIN issues i ON i.id = el.issue_id
				WHERE i.project_id = ? AND el.mirror = 1 AND el.number = ? AND lower(el.repo) = lower(?) AND el.url != ?
				  AND lower(trim(i.title)) = lower(trim(?)) LIMIT 1`, pid, item.Number, repo, item.URL, item.Title).Scan(&issueID)
			if err == nil {
				if err := s.addMirrorLink(ctx, issueID, f, repo, item); err != nil {
					return err
				}
			}
		}
		closed := item.State == "closed"
		if errors.Is(err, sql.ErrNoRows) {
			typ := "task"
			for _, l := range item.Labels {
				if strings.EqualFold(l, "bug") {
					typ = "bug"
				}
			}
			status := ""
			if closed {
				status = CatDone
			}
			in := IssueInput{Project: itoa64(pid), Type: typ, Title: item.Title, Description: item.Body, Status: status, Labels: item.Labels}
			if outside {
				in.outside, in.outsideBy, in.outsideForge = true, item.Author, &f.ID
			}
			d, err := s.CreateIssue(ctx, &writer, in)
			if err != nil {
				return err
			}
			if err := s.addMirrorLink(ctx, d.ID, f, repo, item); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		// Existing: mirror title, description and open/closed.
		cur, err := s.issueByID(ctx, s.db.R, issueID, true)
		if err != nil {
			return err
		}
		p := IssuePatch{}
		if cur.Title != item.Title && item.Title != "" {
			p.Title = &item.Title
		}
		if cur.Description != item.Body {
			p.Description = &item.Body
		}
		if err := s.screenForgeText(ctx, f, cur, item, outside, agent, &p); err != nil {
			return err
		}
		if closed && !cur.Closed() {
			st := CatDone
			p.Status = &st
		} else if !closed && cur.Closed() {
			st := CatTodo
			p.Status = &st
		}
		if p.Title != nil || p.Description != nil || p.Status != nil {
			writer.forgeSync = true
			if _, err := s.UpdateIssue(ctx, &writer, itoa64(issueID), p); err != nil {
				return err
			}
			writer.forgeSync = false
			if p.Status != nil {
				// Not pushed back to this forge (it said so), but its mirrors
				// elsewhere follow.
				s.pushStateToMirrors(ctx, issueID, pid, closed, item.URL)
			}
		}
	}
	return nil
}

// addMirrorLink records that a Waypoint issue mirrors a forge issue. A
// mention link to the same URL on that issue becomes the mirror link.
func (s *Service) addMirrorLink(ctx context.Context, issueID int64, f *Forge, repo string, item ForgeItem) error {
	_, err := s.db.W.ExecContext(ctx, `INSERT INTO external_links(issue_id, provider, kind, url, repo, number, title, state, synced_at, created_at, mirror)
		VALUES (?,?,'issue',?,?,?,?,?,?,?,1) ON CONFLICT(issue_id, url) DO UPDATE SET mirror = 1, kind = 'issue'`,
		issueID, f.Kind, item.URL, repo, int64(item.Number), s.linkItem(ctx, f, &item).Title, item.State, s.nowMS(), s.nowMS())
	return err
}

// hideConfidential blanks a mirrored issue whose forge copy became
// confidential, and removes the comments imported from it. Its link stays,
// so if it becomes public again the next sync fills it back in.
func (s *Service) hideConfidential(ctx context.Context, sys *Actor, f *Forge, issueID int64, item ForgeItem) error {
	title := fmt.Sprintf("Confidential %s issue #%d", kindLabel(f.Kind), item.Number)
	desc := "This issue was made confidential on " + kindLabel(f.Kind) + ", so its text and comments are no longer copied here."
	a := *sys
	a.forgeSync = true
	if _, err := s.UpdateIssue(ctx, &a, itoa64(issueID), IssuePatch{Title: &title, Description: &desc}); err != nil {
		return err
	}
	return s.write(ctx, &a, func(t *tx) error {
		res, err := t.ExecContext(ctx, `DELETE FROM comments WHERE issue_id = ? AND id IN (SELECT comment_id FROM comment_links WHERE forge_id = ? AND origin = 1)`, issueID, f.ID)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM outside_text WHERE issue_id = ? AND forge_id = ?`, issueID, f.ID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE external_links SET title = '', state = 'confidential', synced_at = ? WHERE issue_id = ? AND url = ?`, s.nowMS(), issueID, item.URL); err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			i, err := s.issueByID(ctx, t, issueID, false)
			if err != nil {
				return err
			}
			t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
			return s.reindex(ctx, t, issueID)
		}
		return nil
	})
}

// SyncRepo pulls issues updated since the last sync for one project repo.
func (s *Service) SyncRepo(ctx context.Context, repoID int64) error {
	var forgeID int64
	var repo string
	var last sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT forge_id, repo, last_synced_at FROM project_repos WHERE id = ?`, repoID).Scan(&forgeID, &repo, &last); err != nil {
		return notFound("project repo")
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return err
	}
	since := time.Time{}
	if last.Valid {
		since = msTime(last.Int64).Add(-time.Minute)
	}
	started := s.nowMS()
	items, err := s.Forges.ListIssues(ctx, f, repo, since)
	if err != nil {
		return err
	}
	// One bad item must not hold back the rest. If any failed, the sync
	// point stays put so the next run retries them (imports are idempotent).
	var failed []error
	for _, it := range items {
		if err := s.importForgeIssue(ctx, f, repo, it); err != nil {
			s.log.WarnContext(ctx, "forge import failed", "repo", repo, "number", it.Number, "err", err)
			failed = append(failed, fmt.Errorf("#%d: %w", it.Number, err))
			continue
		}
		if it.Confidential {
			continue
		}
		if err := s.syncComments(ctx, f, repo, it.Number); err != nil {
			s.log.WarnContext(ctx, "forge comment sync failed", "repo", repo, "number", it.Number, "err", err)
			failed = append(failed, fmt.Errorf("#%d comments: %w", it.Number, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d issues failed to sync: %w", len(failed), len(items), errors.Join(failed...))
	}
	_, err = s.db.W.ExecContext(ctx, `UPDATE project_repos SET last_synced_at = ? WHERE id = ?`, started, repoID)
	return err
}

// SyncAllRepos syncs every repo with issue sync on, and refreshes open PR
// links and pull request records. Run periodically as a fallback for
// missed webhooks.
func (s *Service) SyncAllRepos(ctx context.Context) {
	// Forges that make projects automatically: pick up new repos first.
	if forges, err := s.forges(ctx); err == nil {
		for _, f := range forges {
			if f.AutoProjects && f.token != "" {
				if _, err := s.discoverRepos(ctx, f); err != nil {
					s.log.WarnContext(ctx, "discover repos", "forge", f.Name, "err", err)
				}
			}
		}
	}
	s.checkRepoNames(ctx)  // renamed and transferred repos (WP-166)
	s.seedForgePeople(ctx) // whose text isn't from outside (WP-279)
	type syncRepo struct {
		id   int64
		repo string
	}
	var repos []syncRepo
	err := rowsOf(ctx, s.db.R, `SELECT id, repo FROM project_repos WHERE sync_issues = 1 ORDER BY id`, nil, func(rows *sql.Rows) error {
		var r syncRepo
		if err := rows.Scan(&r.id, &r.repo); err != nil {
			return err
		}
		repos = append(repos, r)
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "forge sync: list repos", "err", err)
		}
		return
	}
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		// The repo by name: each one that fails is its own problem in
		// wp server problems (WP-ADR139).
		if err := s.SyncRepo(ctx, r.id); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "repo sync failed", "repo", r.repo, "id", r.id, "err", err)
		}
	}
	var issues []int64
	err = rowsOf(ctx, s.db.R, `SELECT DISTINCT issue_id FROM external_links WHERE kind = 'pull' AND state IN ('', 'open', 'draft') AND created_at > ? ORDER BY issue_id`,
		[]any{s.now().AddDate(0, -2, 0).UnixMilli()}, func(rows *sql.Rows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			issues = append(issues, id)
			return nil
		})
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "forge sync: list open pull request links", "err", err)
		}
		return
	}
	for _, id := range issues {
		if ctx.Err() != nil {
			return
		}
		if err := s.RefreshLinks(ctx, id); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "refresh links", "issue", id, "err", err)
		}
	}
	s.RefreshPullRequests(ctx)
	s.CheckLandedPRs(ctx) // WP-140
}

// pushIssueState closes or reopens the forge issue mirrored by a Waypoint
// issue when its status changes in Waypoint.
func (s *Service) pushIssueState(ctx context.Context, e *Event) {
	nc, _ := e.Data["new_category"].(string)
	oc, _ := e.Data["old_category"].(string)
	if isClosed(nc) == isClosed(oc) {
		return
	}
	links, err := s.externalLinks(ctx, s.db.R, e.IssueID)
	if err != nil {
		return
	}
	for _, l := range links {
		if !l.Mirror || l.Number == nil {
			continue // a forge issue that only mentions this one isn't closed with it
		}
		var synced int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_repos r JOIN forges f ON f.id = r.forge_id
			WHERE r.project_id = ? AND lower(r.repo) = lower(?) AND r.sync_issues = 1`, e.ProjectID, l.Repo).Scan(&synced); err != nil || synced == 0 {
			continue
		}
		f := s.forgeForLink(ctx, l)
		if f == nil || f.token == "" {
			continue
		}
		closed := isClosed(nc)
		num := int(*l.Number)
		s.goBG(func() {
			if err := s.Forges.SetIssueState(context.Background(), f, l.Repo, num, closed); err != nil {
				s.log.WarnContext(ctx, "push issue state to forge", "url", l.URL, "err", err)
				return
			}
			state := map[bool]string{true: "closed", false: "open"}[closed]
			if _, err := s.db.W.ExecContext(context.Background(), `UPDATE external_links SET state = ?, synced_at = ? WHERE id = ?`, state, s.nowMS(), l.ID); err != nil {
				s.log.WarnContext(ctx, "best-effort write failed", "err", err)
			}
		})
	}
}

// ---- HTTP client ----

// httpForgeClient calls forges' APIs: http in the background, patient for
// calls a person waits on (forgePatient).
type httpForgeClient struct {
	http, patient *http.Client
	// glJobs remembers which GitLab commit statuses are pipeline jobs
	// (gitlabJobAuthors); a status's kind never changes.
	glJobs glJobCache
}

func apiBase(f *Forge) string {
	if f.Kind == "github" {
		if strings.TrimRight(f.BaseURL, "/") == "https://github.com" {
			return "https://api.github.com"
		}
		return strings.TrimRight(f.BaseURL, "/") + "/api/v3"
	}
	if f.Kind == "gitlab" {
		return strings.TrimRight(f.BaseURL, "/") + "/api/v4"
	}
	return strings.TrimRight(f.BaseURL, "/") + "/api/v1"
}

func (c *httpForgeClient) do(ctx context.Context, f *Forge, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase(f)+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if f.token != "" {
		switch f.Kind {
		case "github":
			req.Header.Set("Authorization", "Bearer "+f.token)
		case "gitlab":
			req.Header.Set("PRIVATE-TOKEN", f.token)
		default:
			req.Header.Set("Authorization", "token "+f.token)
		}
	}
	resp, err := c.client(ctx).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return apiErrorOf(method, path, resp, b)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// forgeAPIError is a forge API answer that wasn't a success.
type forgeAPIError struct {
	method, path string
	status       int
	body         string
	// retryAfter is what the forge asked to wait (Retry-After); limited: the
	// forge refused for rate, not for the request (429, or a 403 that says
	// so), which trying later can change.
	retryAfter time.Duration
	limited    bool
}

// apiErrorOf is the error of a response that wasn't a success.
func apiErrorOf(method, path string, resp *http.Response, b []byte) *forgeAPIError {
	e := &forgeAPIError{method: method, path: path, status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			e.retryAfter = time.Duration(n) * time.Second
		} else if at, err := http.ParseTime(v); err == nil {
			e.retryAfter = max(time.Until(at), 0)
		}
	}
	low := strings.ToLower(e.body)
	e.limited = e.status == http.StatusTooManyRequests ||
		(e.status == http.StatusForbidden && (e.retryAfter > 0 || resp.Header.Get("X-RateLimit-Remaining") == "0" ||
			strings.Contains(low, "rate limit") || strings.Contains(low, "abuse detection")))
	return e
}

func (e *forgeAPIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d %s", e.method, e.path, e.status, e.body)
}

type apiIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	Merged      bool      `json:"merged"`
	Draft       bool      `json:"draft"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest any       `json:"pull_request"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	User struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	} `json:"user"`
}

func (a apiIssue) item(pr bool) *ForgeItem {
	it := &ForgeItem{Number: a.Number, Title: a.Title, Body: a.Body, State: a.State, URL: a.HTMLURL, IsPR: pr, UpdatedAt: a.UpdatedAt,
		Author: cmp(a.User.Login, a.User.Username)}
	if pr && a.Merged {
		it.State = "merged"
	} else if pr && a.Draft && a.State == "open" {
		it.State = "draft"
	}
	for _, l := range a.Labels {
		it.Labels = append(it.Labels, l.Name)
	}
	return it
}

func (c *httpForgeClient) GetItem(ctx context.Context, f *Forge, repo string, number int, pr bool) (*ForgeItem, error) {
	if f.Kind == "gitlab" {
		return c.glGetItem(ctx, f, repo, number, pr)
	}
	var a apiIssue
	kind := "issues"
	if pr {
		kind = "pulls"
	}
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/%s/%d", repo, kind, number), nil, &a); err != nil {
		return nil, err
	}
	return a.item(pr), nil
}

// maxForgePages bounds one sync (5,000-10,000 issues); a bigger first sync
// says so instead of silently stopping.
const maxForgePages = 100

func (c *httpForgeClient) ListIssues(ctx context.Context, f *Forge, repo string, since time.Time) ([]ForgeItem, error) {
	if f.Kind == "gitlab" {
		return c.glListIssues(ctx, f, repo, since)
	}
	var out []ForgeItem
	for page := 1; ; page++ {
		if page > maxForgePages {
			return nil, fmt.Errorf("%s has more than %d pages of issues; Waypoint syncs at most that many at once", repo, maxForgePages)
		}
		q := url.Values{"state": {"all"}, "page": {strconv.Itoa(page)}}
		if f.Kind == "github" {
			q.Set("per_page", "100")
		} else {
			q.Set("limit", "50")
			q.Set("type", "issues")
		}
		if !since.IsZero() {
			q.Set("since", since.UTC().Format(time.RFC3339))
		}
		var list []apiIssue
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/issues?%s", repo, q.Encode()), nil, &list); err != nil {
			return nil, err
		}
		for _, a := range list {
			if a.PullRequest != nil {
				continue
			}
			out = append(out, *a.item(false))
		}
		if len(list) == 0 || (f.Kind == "github" && len(list) < 100) || (f.Kind != "github" && len(list) < 50) {
			break
		}
	}
	slices.SortFunc(out, func(a, b ForgeItem) int { return a.Number - b.Number })
	return out, nil
}

func (c *httpForgeClient) SetIssueState(ctx context.Context, f *Forge, repo string, number int, closed bool) error {
	if f.Kind == "gitlab" {
		return c.glSetIssueState(ctx, f, repo, number, closed)
	}
	state := "open"
	if closed {
		state = "closed"
	}
	return c.do(ctx, f, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, number), map[string]string{"state": state}, nil)
}
