package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Webhook states of an attached repo; "" means it was attached before
// Waypoint added webhooks itself.
const (
	hookPending   = "pending"
	hookInstalled = "installed"
	hookFailed    = "failed"
	hookCovered   = "covered"
)

// ForgeHook is a webhook on a forge repo.
type ForgeHook struct {
	ID  string
	URL string
}

// ForgeHooker is the optional part of a ForgeClient that manages repo and
// organisation webhooks.
type ForgeHooker interface {
	ListHooks(ctx context.Context, f *Forge, on hookOn) ([]ForgeHook, error)
	CreateHook(ctx context.Context, f *Forge, on hookOn, hookURL, secret string) (string, error)
	UpdateHook(ctx context.Context, f *Forge, on hookOn, id, hookURL, secret string) error
	DeleteHook(ctx context.Context, f *Forge, on hookOn, id string) error
	TestHook(ctx context.Context, f *Forge, on hookOn, id string) error
	ListOrgs(ctx context.Context, f *Forge) ([]string, error)
}

// repoHookJob is webhook work to do once a change is committed: install
// (or refresh) the hook for an attached repo or an organisation, or remove
// one Waypoint added.
type repoHookJob struct {
	install    int64
	installOrg int64
	remove     *Forge
	org        bool
	repo       string
	hookID     string
}

var errNoForgeToken = errors.New("no forge token")

func hookLock(forgeID int64, repo string) func() {
	return forgeLocks.lock(fmt.Sprintf("hook:%d:%s", forgeID, strings.ToLower(repo)))
}

func (s *Service) runRepoHook(ctx context.Context, j *repoHookJob) {
	switch {
	case j.remove != nil && j.org:
		s.removeOrgHook(ctx, j.remove, j.repo, j.hookID)
	case j.remove != nil:
		s.removeRepoHook(ctx, j.remove, j.repo, j.hookID)
	case j.installOrg != 0:
		s.installOrgHook(ctx, j.installOrg)
	default:
		s.installRepoHook(ctx, j.install)
	}
}

// orgCovers says whether an organisation hook gets a repo's events: the
// repo's owner on GitHub and Forgejo, the group or any subgroup on GitLab.
func orgCovers(kind, org, repo string) bool {
	org, repo = strings.ToLower(org), strings.ToLower(repo)
	if kind == "gitlab" {
		return strings.HasPrefix(repo, org+"/")
	}
	owner, _, _ := strings.Cut(repo, "/")
	return owner == org
}

// coveringOrg is the organisation hook (added or being added) that already
// brings a repo's events, or "".
func (s *Service) coveringOrg(ctx context.Context, f *Forge, repo string) string {
	rows, err := s.db.R.QueryContext(ctx, `SELECT org FROM forge_org_hooks WHERE forge_id = ? AND state IN (?, ?) ORDER BY id`, f.ID, hookInstalled, hookPending)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var org string
		if rows.Scan(&org) == nil && orgCovers(f.Kind, org, repo) {
			return org
		}
	}
	return ""
}

// installRepoHook makes sure the forge has a webhook for an attached repo
// and records the outcome on every project that has that repo. A repo an
// organisation hook covers gets none (two hooks would deliver everything
// twice), and loses one Waypoint added earlier.
func (s *Service) installRepoHook(ctx context.Context, repoID int64) {
	var forgeID int64
	var repo string
	if err := s.db.R.QueryRowContext(ctx, `SELECT forge_id, repo FROM project_repos WHERE id = ?`, repoID).Scan(&forgeID, &repo); err != nil {
		return
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return
	}
	defer hookLock(f.ID, repo)()
	if org := s.coveringOrg(ctx, f, repo); org != "" {
		s.dropRepoHooks(ctx, f, repo)
		return
	}
	id, created, hf, err := s.ensureForgeHook(ctx, f, onRepo(repo))
	state, note := hookInstalled, hookReachNote(f, s.BaseURL())
	if err != nil {
		state, note = hookFailed, hookErrorText(f, onRepo(repo), err)
		s.log.WarnContext(ctx, "forge webhook", "forge", f.Name, "repo", repo, "err", err)
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE project_repos SET hook_id = CASE WHEN ? = '' THEN hook_id ELSE ? END,
		hook_state = ?, hook_note = ?, hook_checked_at = ? WHERE forge_id = ? AND lower(repo) = lower(?)`,
		id, id, state, note, s.nowMS(), f.ID, repo)
	if err != nil {
		s.log.ErrorContext(ctx, "record forge webhook", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if h, ok := s.Forges.(ForgeHooker); ok && created {
			_ = h.DeleteHook(ctx, hf, onRepo(repo), id)
		}
		return
	}
	if state == hookInstalled {
		s.testHook(ctx, hf, onRepo(repo), id, created)
	}
}

// dropRepoHooks removes the repo webhook Waypoint manages for a repo an
// organisation hook now covers, and marks the repo covered.
func (s *Service) dropRepoHooks(ctx context.Context, f *Forge, repo string) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT hook_id FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?) AND hook_id != ''`, f.ID, repo)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if h, ok := s.Forges.(ForgeHooker); ok {
		for _, id := range ids {
			if err := deleteHook(ctx, h, f, onRepo(repo), id); err != nil && !isStatus(err, 404) && !errors.Is(err, errNoForgeToken) {
				s.log.WarnContext(ctx, "remove covered repo webhook", "forge", f.Name, "repo", repo, "err", err)
			}
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE project_repos SET hook_id = '', hook_state = ?, hook_note = '', hook_checked_at = ?
		WHERE forge_id = ? AND lower(repo) = lower(?)`, hookCovered, s.nowMS(), f.ID, repo); err != nil {
		s.log.ErrorContext(ctx, "record forge webhook", "err", err)
	}
}

func isStatus(err error, status int) bool {
	var ae *forgeAPIError
	return errors.As(err, &ae) && ae.status == status
}

// testHook asks the forge for a test delivery, so "last delivery" shows
// whether it gets through (GitHub pings a new hook by itself). Best effort.
func (s *Service) testHook(ctx context.Context, f *Forge, on hookOn, id string, created bool) {
	h, ok := s.Forges.(ForgeHooker)
	if !ok || id == "" || (created && f.Kind == "github") {
		return
	}
	if err := h.TestHook(ctx, f, on, id); err != nil && !errors.Is(err, errNoHookTest) {
		s.log.Info("forge webhook test delivery", "forge", f.Name, "target", on.name, "err", err)
	}
}

// hookForges are the forge as each token that may manage webhooks: the API
// token, then the bot token, which often has the wider scope.
func hookForges(f *Forge) []*Forge {
	if f.botToken == "" || f.botToken == f.token {
		return []*Forge{f}
	}
	return []*Forge{f, botForge(f)}
}

// refused: the token can't do this, so another one may.
func refused(err error) bool {
	return errors.Is(err, errNoForgeToken) || isStatus(err, 401) || isStatus(err, 403)
}

// ensureForgeHook finds or adds the webhook pointing at Waypoint, with the
// forge's current secret and the events Waypoint reads. hf is the forge as
// the token that managed it, for testing or removing it next.
func (s *Service) ensureForgeHook(ctx context.Context, f *Forge, on hookOn) (id string, created bool, hf *Forge, err error) {
	h, ok := s.Forges.(ForgeHooker)
	if !ok {
		return "", false, f, errors.New("this forge client can't manage webhooks")
	}
	var first error
	for _, hf := range hookForges(f) {
		id, created, err := ensureHookAs(ctx, h, hf, on)
		if err == nil || !refused(err) {
			return id, created, hf, err
		}
		first = cmpErr(first, err)
	}
	return "", false, f, first
}

func ensureHookAs(ctx context.Context, h ForgeHooker, f *Forge, on hookOn) (id string, created bool, err error) {
	if f.token == "" {
		return "", false, errNoForgeToken
	}
	hooks, err := h.ListHooks(ctx, f, on)
	if err != nil {
		return "", false, err
	}
	for _, x := range hooks {
		if sameHookURL(x.URL, f.WebhookURL) {
			return x.ID, false, h.UpdateHook(ctx, f, on, x.ID, f.WebhookURL, f.secret)
		}
	}
	id, err = h.CreateHook(ctx, f, on, f.WebhookURL, f.secret)
	return id, err == nil, err
}

// deleteHook removes a webhook as whichever token may.
func deleteHook(ctx context.Context, h ForgeHooker, f *Forge, on hookOn, id string) (err error) {
	for _, hf := range hookForges(f) {
		if hf.token == "" {
			continue
		}
		if err = h.DeleteHook(ctx, hf, on, id); err == nil || !refused(err) {
			return err
		}
	}
	return err
}

// removeRepoHook deletes a webhook Waypoint added, once no project has its
// repo any more. Best effort: the forge may already have lost it.
func (s *Service) removeRepoHook(ctx context.Context, f *Forge, repo, hookID string) {
	h, ok := s.Forges.(ForgeHooker)
	if hookID == "" || (f.token == "" && f.botToken == "") || !ok {
		return
	}
	defer hookLock(f.ID, repo)()
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?)`, f.ID, repo).Scan(&n); err != nil || n > 0 {
		return
	}
	if err := deleteHook(ctx, h, f, onRepo(repo), hookID); err != nil && !isStatus(err, 404) {
		s.log.WarnContext(ctx, "remove forge webhook", "forge", f.Name, "repo", repo, "err", err)
	}
}

// HookProblem is a linked repo no webhook delivery has come from: Waypoint
// couldn't add the webhook, or the forge can't reach Waypoint. Its bots and
// CI then follow the pull request sync, up to ten minutes late.
type HookProblem struct {
	Project string `json:"project"` // the key of a project, of the actor's, it is linked to
	RepoID  int64  `json:"repo_id"` // that link
	Repo    string `json:"repo"`
	State   string `json:"state"` // failed (not added), silent (added, or linked by hand, but nothing came)
	Note    string `json:"note,omitempty"`
}

// hookSilentAfter is how long an added webhook may go without its first
// delivery (Waypoint asks for a test one at once) before it counts as broken.
const hookSilentAfter = 15 * time.Minute

// hookProblems lists the repos, of live projects the actor owns (all of
// them for an admin), that no webhook delivery has come from; project
// narrows it to one project's. One row per repo however many projects
// link it.
func (s *Service) hookProblems(ctx context.Context, actor *Actor, project string) ([]*HookProblem, error) {
	out := []*HookProblem{}
	if actor == nil || actor.User == nil {
		return out, nil
	}
	quiet := s.now().Add(-hookSilentAfter).UnixMilli()
	owned, oargs := actor.MemberProjects(RoleOwner)
	// #nosec G202 -- owned is placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.key, min(r.id), r.repo, r.hook_state, r.hook_note
		FROM project_repos r JOIN projects p ON p.id = r.project_id
		WHERE p.archived = 0 AND r.hook_seen_at IS NULL
			AND (r.hook_state = ? OR (r.hook_state IN (?, '') AND coalesce(r.hook_checked_at, r.created_at) < ?))
			AND (? OR r.project_id IN `+owned+`)
			AND (? = '' OR p.key = ?)
		GROUP BY r.forge_id, lower(r.repo)
		ORDER BY lower(r.repo)`,
		append(append([]any{hookFailed, hookInstalled, quiet, actor.IsAdmin()}, oargs...), project, project)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		h := &HookProblem{}
		var state string
		if err := rows.Scan(&h.Project, &h.RepoID, &h.Repo, &state, &h.Note); err != nil {
			return nil, err
		}
		h.State = "silent"
		if state == hookFailed {
			h.State = "failed"
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RetryRepoHook installs (or refreshes) an attached repo's webhook again,
// say after the forge's token was given the right scope.
func (s *Service) RetryRepoHook(ctx context.Context, actor *Actor, projectRef string, id int64) error {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE project_repos SET hook_state = ?, hook_note = '' WHERE id = ? AND project_id = ?`, hookPending, id, p.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("project repo")
		}
		t.emit(&Event{Kind: EvRepoHook, ProjectID: p.ID, repoHook: &repoHookJob{install: id}})
		return nil
	})
}

// refreshRepoHooks queues the webhooks again for a forge's organisations
// and for each repo Waypoint manages the hook for (one per repo, however
// many projects have it).
func (s *Service) refreshRepoHooks(ctx context.Context, t *tx, forgeID int64) error {
	var evs []*Event
	rows, err := t.QueryContext(ctx, `SELECT id FROM forge_org_hooks WHERE forge_id = ?`, forgeID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		evs = append(evs, &Event{Kind: EvRepoHook, repoHook: &repoHookJob{installOrg: id}})
	}
	rows.Close()
	rows, err = t.QueryContext(ctx, `SELECT min(id), min(project_id) FROM project_repos
		WHERE forge_id = ? AND hook_state NOT IN ('', ?) GROUP BY lower(repo)`, forgeID, hookCovered)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, projectID int64
		if err := rows.Scan(&id, &projectID); err != nil {
			return err
		}
		evs = append(evs, &Event{Kind: EvRepoHook, ProjectID: projectID, repoHook: &repoHookJob{install: id}})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `UPDATE project_repos SET hook_state = ?, hook_note = '' WHERE forge_id = ? AND hook_state NOT IN ('', ?)`, hookPending, forgeID, hookCovered); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `UPDATE forge_org_hooks SET state = ?, note = '' WHERE forge_id = ?`, hookPending, forgeID); err != nil {
		return err
	}
	for _, e := range evs {
		t.emit(e)
	}
	return nil
}

// noteHookDelivery records that the forge got a signed delivery through
// about a repo (on the repo and on an organisation hook covering it), or
// an organisation's ping.
func (s *Service) noteHookDelivery(ctx context.Context, f *Forge, repo, org string) {
	now := s.nowMS()
	if repo != "" {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE project_repos SET hook_seen_at = ? WHERE forge_id = ? AND lower(repo) = lower(?)`, now, f.ID, repo); err != nil {
			s.log.WarnContext(ctx, "note webhook delivery", "err", err)
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE forge_org_hooks SET seen_at = ? WHERE forge_id = ? AND lower(substr(?, 1, length(org) + 1)) = lower(org) || '/'`, now, f.ID, repo); err != nil {
			s.log.WarnContext(ctx, "note webhook delivery", "err", err)
		}
	}
	if org != "" {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE forge_org_hooks SET seen_at = ? WHERE forge_id = ? AND lower(org) = lower(?)`, now, f.ID, org); err != nil {
			s.log.WarnContext(ctx, "note webhook delivery", "err", err)
		}
	}
}

func sameHookURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

// hookErrorText says in a sentence why a webhook couldn't be added.
func hookErrorText(f *Forge, on hookOn, err error) string {
	kind := kindLabel(f.Kind)
	if errors.Is(err, errNoForgeToken) {
		return "The forge has no API token, so Waypoint can't add the webhook. Add one in Admin → Forges, or set the webhook up by hand."
	}
	var ae *forgeAPIError
	if !errors.As(err, &ae) {
		return excerpt(fmt.Sprintf("Couldn't reach %s: %v", kind, err), 300)
	}
	switch ae.status {
	case 401:
		return kind + " rejected the forge's token; it may have expired or been revoked."
	case 403, 404:
		who := "The forge token can't"
		if len(hookForges(f)) > 1 {
			who = "Neither of the forge's tokens (API and bot) can"
		}
		said := ""
		if m := apiMessage(ae.body); m != "" {
			said = fmt.Sprintf(" %s said: %s", kind, excerpt(m, 160))
		}
		if on.org {
			if f.Kind == "gitlab" {
				return fmt.Sprintf("GitLab wouldn't add a webhook to the group %s. Group webhooks need GitLab Premium or Ultimate, a token with the api scope, and the Owner role on the group; without them, Waypoint adds a webhook to each linked repo instead.", on.name)
			}
			need := map[string]string{
				"github":  "the admin:org_hook scope (or Webhooks: read and write for the organisation on a fine-grained token) and owner rights on the organisation",
				"forgejo": "the write:organization scope and owner rights on the organisation",
			}[f.Kind]
			return fmt.Sprintf("%s manage webhooks on the organisation %s (or it doesn't exist). It needs %s.%s", who, on.name, need, said)
		}
		need := map[string]string{
			"github":  "the admin:repo_hook scope (or Webhooks: read and write on a fine-grained token) and admin rights on the repo",
			"gitlab":  "the api scope and the Maintainer role on the project",
			"forgejo": "the write:repository scope and admin rights on the repo",
		}[f.Kind]
		return fmt.Sprintf("%s manage webhooks on %s (or the repo doesn't exist). It needs %s.%s", who, on.name, need, said)
	case 400, 422:
		return excerpt(kind+" refused the webhook: "+apiMessage(ae.body), 300)
	}
	return fmt.Sprintf("%s answered HTTP %d.", kind, ae.status)
}

// apiMessage pulls the human part out of a forge's JSON error.
func apiMessage(body string) string {
	var e struct {
		Message any `json:"message"`
		Error   any `json:"error"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal([]byte(body), &e) != nil {
		return body
	}
	var parts []string
	for _, v := range []any{e.Message, e.Error} {
		switch v := v.(type) {
		case nil:
		case string:
			parts = append(parts, v)
		default:
			b, _ := json.Marshal(v)
			parts = append(parts, string(b))
		}
	}
	for _, x := range e.Errors {
		if x.Message != "" {
			parts = append(parts, x.Message)
		}
	}
	if len(parts) == 0 {
		return body
	}
	return strings.Join(parts, ": ")
}

// hookReachNote warns when the forge probably can't reach Waypoint's
// address: this machine only, or a private address while the forge is
// out on the internet.
func hookReachNote(f *Forge, waypointBase string) string {
	wu, err := url.Parse(waypointBase)
	if err != nil {
		return ""
	}
	fu, _ := url.Parse(f.BaseURL)
	forgeHost := ""
	if fu != nil {
		forgeHost = fu.Hostname()
	}
	wh := wu.Hostname()
	switch {
	case isLoopbackHost(wh) && !isLoopbackHost(forgeHost):
		return fmt.Sprintf("Waypoint's address (%s) only works on this machine, so %s can't deliver to it. Set WAYPOINT_BASE_URL to an address the forge can reach.", wu.Host, kindLabel(f.Kind))
	case isPrivateHost(wh) && !isPrivateHost(forgeHost) && !isLoopbackHost(forgeHost):
		return fmt.Sprintf("Waypoint's address (%s) looks private, so %s may not be able to deliver to it.", wu.Host, forgeHost)
	}
	return ""
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// isPrivateHost: a LAN address or a name that only resolves inside one.
func isPrivateHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback()
	}
	h = strings.ToLower(h)
	if !strings.Contains(h, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".internal", ".home.arpa", ".localhost"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// hookEvents are the webhook events Waypoint reads (see parseGitHubHook).
var hookEvents = []string{"issues", "issue_comment", "pull_request", "push"}

// hookEventsFor is what a forge's hooks subscribe to: GitHub sends reviews
// as events of their own; Forgejo's "pull_request" already brings them.
// Releases and CI (WP-D21) have events too: GitHub's status, check_run,
// check_suite and workflow_run; Forgejo's action runs (it has no status
// event, so Waypoint also polls while checks are pending). Existing hooks
// get them when they are saved or retried.
func hookEventsFor(kind string) []string {
	out := append([]string{}, hookEvents...)
	out = append(out, "release")
	if kind == "github" {
		return append(out, "pull_request_review", "pull_request_review_comment", "status", "check_run", "check_suite", "workflow_run")
	}
	return append(out, "action_run_failure", "action_run_recover", "action_run_success")
}

// hookOn is where a webhook lives: a repo, or an organisation (a GitLab
// group).
type hookOn struct {
	org  bool
	name string
}

func onRepo(repo string) hookOn { return hookOn{name: repo} }

type apiHook struct {
	ID     int64  `json:"id"`
	URL    string `json:"url"` // GitLab
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

func (c *httpForgeClient) hooksPath(f *Forge, on hookOn) string {
	switch {
	case f.Kind == "gitlab" && on.org:
		return "/groups/" + url.PathEscape(on.name) + "/hooks"
	case f.Kind == "gitlab":
		return glProject(on.name) + "/hooks"
	case on.org:
		return "/orgs/" + url.PathEscape(on.name) + "/hooks"
	}
	return "/repos/" + on.name + "/hooks"
}

func (c *httpForgeClient) hookBody(f *Forge, hookURL, secret string, create bool) map[string]any {
	if f.Kind == "gitlab" {
		return map[string]any{
			"url": hookURL, "token": secret,
			"push_events": true, "issues_events": true, "confidential_issues_events": true,
			"merge_requests_events": true, "note_events": true, "pipeline_events": true, "releases_events": true,
			"enable_ssl_verification": true,
		}
	}
	b := map[string]any{
		"active": true,
		"events": hookEventsFor(f.Kind),
		"config": map[string]string{"url": hookURL, "content_type": "json", "secret": secret},
	}
	if create {
		if f.Kind == "github" {
			b["name"] = "web"
		} else {
			b["type"] = "gitea"
		}
	}
	return b
}

func (c *httpForgeClient) ListHooks(ctx context.Context, f *Forge, on hookOn) ([]ForgeHook, error) {
	var hooks []apiHook
	if err := c.do(ctx, f, "GET", c.hooksPath(f, on)+"?per_page=100&limit=50", nil, &hooks); err != nil {
		return nil, err
	}
	out := make([]ForgeHook, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, ForgeHook{ID: fmt.Sprint(h.ID), URL: cmp(h.Config.URL, h.URL)})
	}
	return out, nil
}

func (c *httpForgeClient) CreateHook(ctx context.Context, f *Forge, on hookOn, hookURL, secret string) (string, error) {
	var h apiHook
	if err := c.do(ctx, f, "POST", c.hooksPath(f, on), c.hookBody(f, hookURL, secret, true), &h); err != nil {
		return "", err
	}
	if h.ID == 0 {
		return "", errors.New("the forge didn't say which webhook it made")
	}
	return fmt.Sprint(h.ID), nil
}

func (c *httpForgeClient) UpdateHook(ctx context.Context, f *Forge, on hookOn, id, hookURL, secret string) error {
	method := "PATCH"
	if f.Kind == "gitlab" {
		method = "PUT"
	}
	return c.do(ctx, f, method, c.hooksPath(f, on)+"/"+url.PathEscape(id), c.hookBody(f, hookURL, secret, false), nil)
}

func (c *httpForgeClient) DeleteHook(ctx context.Context, f *Forge, on hookOn, id string) error {
	return c.do(ctx, f, "DELETE", c.hooksPath(f, on)+"/"+url.PathEscape(id), nil, nil)
}

// TestHook asks the forge to send the hook a delivery now: GitHub a ping,
// Forgejo a push of the latest commit (before = after, which Waypoint
// reads as a ping), GitLab (16.11+) a sample push of the last commits.
// Forgejo can't test organisation hooks; errNoHookTest says so.
func (c *httpForgeClient) TestHook(ctx context.Context, f *Forge, on hookOn, id string) error {
	base := c.hooksPath(f, on) + "/" + url.PathEscape(id)
	switch {
	case f.Kind == "github":
		return c.do(ctx, f, "POST", base+"/pings", nil, nil)
	case f.Kind == "gitlab":
		return c.do(ctx, f, "POST", base+"/test/push_events", nil, nil)
	case on.org:
		return errNoHookTest
	}
	return c.do(ctx, f, "POST", base+"/tests", nil, nil)
}

var errNoHookTest = errors.New("the forge can't send a test delivery to this webhook")

// ListOrgs lists the organisations (GitLab: groups the token owns) the
// token could add an organisation webhook to.
func (c *httpForgeClient) ListOrgs(ctx context.Context, f *Forge) ([]string, error) {
	var out []string
	switch f.Kind {
	case "gitlab":
		var groups []struct {
			FullPath string `json:"full_path"`
		}
		if err := c.do(ctx, f, "GET", "/groups?min_access_level=50&per_page=100", nil, &groups); err != nil {
			return nil, err
		}
		for _, g := range groups {
			out = append(out, g.FullPath)
		}
	default:
		var orgs []struct {
			Login    string `json:"login"`
			Username string `json:"username"`
			Name     string `json:"name"`
		}
		if err := c.do(ctx, f, "GET", "/user/orgs?per_page=100&limit=50", nil, &orgs); err != nil {
			return nil, err
		}
		for _, o := range orgs {
			out = append(out, cmp(o.Login, cmp(o.Username, o.Name)))
		}
	}
	return out, nil
}
