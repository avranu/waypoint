package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Permission names one thing a permission token may do, e.g. "issues.edit".
type Permission string

// Risk says how much a permission can do if the token leaks or the agent
// holding it misbehaves.
type Risk string

const (
	RiskSafe      Risk = "safe"
	RiskCareful   Risk = "careful"
	RiskDangerous Risk = "dangerous"
)

const (
	PermIssuesRead    Permission = "issues.read"
	PermIssuesCreate  Permission = "issues.create"
	PermIssuesEdit    Permission = "issues.edit"
	PermIssuesComment Permission = "issues.comment"
	PermIssuesDelete  Permission = "issues.delete"
	PermIssuesShare   Permission = "issues.share"
	PermIssuesPublish Permission = "issues.publish"
	PermTimeTrack     Permission = "time.track"

	PermSearch           Permission = "search"
	PermPagesRead        Permission = "pages.read"
	PermPagesWrite       Permission = "pages.write"
	PermPagesDelete      Permission = "pages.delete"
	PermGlossaryRead     Permission = "glossary.read"
	PermGlossaryWrite    Permission = "glossary.write"
	PermGlossaryDelete   Permission = "glossary.delete"
	PermDesignRead       Permission = "design.read"
	PermDesignWrite      Permission = "design.write"
	PermDesignDelete     Permission = "design.delete"
	PermKnowledgeApprove Permission = "knowledge.approve"
	PermLinksWrite       Permission = "links.write"
	PermAttachmentsRead  Permission = "attachments.read"
	PermAttachmentsWrite Permission = "attachments.write"

	PermRequestsRead   Permission = "requests.read"
	PermRequestsCreate Permission = "requests.create"
	PermRequestsAnswer Permission = "requests.answer"
	PermRequestsRun    Permission = "requests.run"

	PermNotifications Permission = "notifications"
	PermViewsWrite    Permission = "views.write"
	PermProfile       Permission = "profile"

	PermSprintsManage  Permission = "sprints.manage"
	PermProjectsManage Permission = "projects.manage"
	PermReposManage    Permission = "repos.manage"
	PermWebhooksManage Permission = "webhooks.manage"

	PermTasksWork      Permission = "tasks.work"
	PermAgentsRead     Permission = "agents.read"
	PermAgentsManage   Permission = "agents.manage"
	PermSessionsRead   Permission = "sessions.read"
	PermSessionsReport Permission = "sessions.report"
	PermSessionsStart  Permission = "sessions.start"
	PermRunnersRead    Permission = "runners.read"
	PermRunnersRun     Permission = "runners.run"
	PermRunnersHost    Permission = "runners.host"
	PermCIReport       Permission = "ci.report"

	PermPullsRead         Permission = "pulls.read"
	PermPullsWrite        Permission = "pulls.write"
	PermPullsMerge        Permission = "pulls.merge"
	PermAuditsRun         Permission = "audits.run"
	PermEnvironmentsRead  Permission = "environments.read"
	PermEnvironmentsWrite Permission = "environments.deploy"
	PermAutomationsRead   Permission = "automations.read"
	PermAutomationsManage Permission = "automations.manage"
	PermAIUse             Permission = "ai.use"
	PermAIManage          Permission = "ai.manage"

	PermUsageRead Permission = "usage.read"
	// PermTranscriptsRead: raw transcripts and session facts (WP-1587).
	PermTranscriptsRead Permission = "transcripts.read"
	PermAuditRead       Permission = "audit.read"
	PermServerRead      Permission = "server.read"
	PermForgesManage    Permission = "forges.manage"
	PermAdmin           Permission = "admin"
)

// PermissionInfo describes a permission for the token settings page.
type PermissionInfo struct {
	Key         Permission `json:"key"`
	Label       string     `json:"label"`
	Description string     `json:"description"`
	Risk        Risk       `json:"risk"`
}

// PermissionGroup is one section of the catalog.
type PermissionGroup struct {
	Key         string           `json:"key"`
	Label       string           `json:"label"`
	Permissions []PermissionInfo `json:"permissions"`
}

// PermissionPreset is a starting selection the settings page offers.
type PermissionPreset struct {
	Key         string       `json:"key"`
	Label       string       `json:"label"`
	Description string       `json:"description"`
	Permissions []Permission `json:"permissions"`
}

// PermissionCatalog is everything the settings page needs to edit a token.
type PermissionCatalog struct {
	Groups  []PermissionGroup  `json:"groups"`
	Presets []PermissionPreset `json:"presets"`
}

var permissionGroups = []PermissionGroup{
	{Key: "issues", Label: "Issues", Permissions: []PermissionInfo{
		{PermIssuesRead, "Read issues", "Issues, comments, history, boards, backlogs, sprints, reports and projects.", RiskSafe},
		{PermIssuesCreate, "Create issues", "New issues and sub-tasks, from templates too.", RiskSafe},
		{PermIssuesEdit, "Edit issues", "Fields, status, assignees, labels, relations and links, one at a time or in bulk.", RiskSafe},
		{PermIssuesComment, "Comment", "Add comments, and edit or delete its own.", RiskSafe},
		{PermTimeTrack, "Track time", "Timers and time entries.", RiskSafe},
		{PermIssuesShare, "Share issues", "Give a person access to an issue outside their projects.", RiskCareful},
		{PermIssuesPublish, "Publish drafts", "Publish draft issues, or make issues drafts again.", RiskCareful},
		{PermIssuesDelete, "Delete issues", "Delete issues for good.", RiskDangerous},
	}},
	{Key: "knowledge", Label: "Pages, glossary and design", Permissions: []PermissionInfo{
		{PermSearch, "Search", "Search issues, pages, glossary, design notes and comments at once; resolve keys and links.", RiskSafe},
		{PermPagesRead, "Read pages", "Plans, decisions and notes.", RiskSafe},
		{PermPagesWrite, "Write pages", "Create, edit and publish pages; link them to issues.", RiskSafe},
		{PermGlossaryRead, "Read the glossary", "Terms and words to avoid.", RiskSafe},
		{PermGlossaryWrite, "Edit the glossary", "Add and edit terms (locked terms stay locked for agents).", RiskSafe},
		{PermDesignRead, "Read design notes", "Design notes and specs.", RiskSafe},
		{PermDesignWrite, "Propose design notes", "Propose notes, edit drafts, add to approved ones.", RiskSafe},
		{PermLinksWrite, "Link things", "Link terms, notes, pages and issues to each other.", RiskSafe},
		{PermAttachmentsRead, "Read attachments", "Download screenshots and files attached to anything.", RiskSafe},
		{PermAttachmentsWrite, "Attach files", "Upload files; edit the captions of, and delete, only the files it uploaded.", RiskSafe},
		{PermKnowledgeApprove, "Approve and review", "Approve design notes and revisions, and mark agent work reviewed.", RiskCareful},
		{PermPagesDelete, "Delete pages", "Delete pages and their codes.", RiskDangerous},
		{PermGlossaryDelete, "Delete glossary terms", "Delete terms.", RiskDangerous},
		{PermDesignDelete, "Delete design notes", "Delete design notes.", RiskDangerous},
	}},
	{Key: "you", Label: "Requests and you", Permissions: []PermissionInfo{
		{PermRequestsRead, "Read requests", "Questions and requests waiting on people.", RiskSafe},
		{PermRequestsCreate, "Ask people", "Ask a person a question or for a change, a command or a to-do; withdraw its own.", RiskSafe},
		{PermNotifications, "Notifications", "Read your notifications and mark them read.", RiskSafe},
		{PermViewsWrite, "Saved views", "Create and change your saved views.", RiskSafe},
		{PermProfile, "Your profile", "Change your name, avatar, calendar feed and push settings.", RiskCareful},
		{PermRequestsAnswer, "Answer requests", "Answer or snooze requests as you, approving what agents asked for.", RiskDangerous},
		{PermRequestsRun, "Run request commands", "Run the commands in a request.", RiskDangerous},
	}},
	{Key: "projects", Label: "Projects", Permissions: []PermissionInfo{
		{PermSprintsManage, "Plan sprints", "Create, change, start and close sprints (deleting one needs Manage projects).", RiskCareful},
		{PermProjectsManage, "Manage projects", "Create, change and delete projects, members, statuses, labels, templates and workflows.", RiskDangerous},
		{PermReposManage, "Manage repositories", "Attach repositories to projects, sync them, install their hooks.", RiskDangerous},
		{PermWebhooksManage, "Manage webhooks", "Project webhooks and their deliveries.", RiskDangerous},
	}},
	{Key: "agents", Label: "Agents, sessions and runners", Permissions: []PermissionInfo{
		{PermAgentsRead, "See agent work", "The Agents page: queue, activity, overview, knowledge use.", RiskSafe},
		{PermSessionsRead, "See sessions", "Agent sessions and their follow-ups.", RiskSafe},
		{PermRunnersRead, "See runners", "Runners and their jobs, logs included.", RiskCareful},
		{PermTasksWork, "Take tasks", "Take tasks from the agent queue, finish, pause and release them.", RiskCareful},
		{PermSessionsReport, "Report sessions", "Send session hooks, usage and telemetry, as Claude Code's hooks do.", RiskCareful},
		{PermAgentsManage, "Manage the queue", "Reorder the agent queue, pause agents, take tasks back, and change who may take an issue (delegating it to agents).", RiskCareful},
		{PermSessionsStart, "Start sessions", "Start coding-agent sessions, hand off to fresh ones, run follow-ups.", RiskDangerous},
		{PermRunnersRun, "Run jobs on runners", "Queue commands, code changes and coding agents on your machines.", RiskDangerous},
		{PermRunnersHost, "Be a runner", "Register as a runner and receive its jobs; remove runners.", RiskDangerous},
		{PermCIReport, "Report CI hosts", "Report a CI runner host's resources and what its jobs used, and read every job's usage, as the CI usage reporter does (admins' tokens).", RiskCareful},
	}},
	{Key: "code", Label: "Code, deploys and automations", Permissions: []PermissionInfo{
		{PermPullsRead, "See pull requests", "Pull requests, their checks, readiness and branches, and code audits and their findings.", RiskSafe},
		{PermEnvironmentsRead, "See environments", "Environments and deployments.", RiskSafe},
		{PermAutomationsRead, "See automations", "Automations, bots and their runs.", RiskSafe},
		{PermPullsWrite, "Open pull requests", "Open pull requests, re-check them (which runs a review job on a runner), dismiss and reopen findings.", RiskDangerous},
		{PermAuditsRun, "Ask for audits", "Ask a model to audit a repository, branch or pull request from your runner's checkout (the one the gateway picks by default), mark its findings, and have Claude check and fix them.", RiskCareful},
		{PermAIUse, "Use AI helpers", "Estimates, titles, sub-task suggestions and related issues, including the automatic ones on new issues; spends AI credit and may run a context script on your runner.", RiskCareful},
		{PermPullsMerge, "Merge pull requests", "Arm and disarm merges.", RiskDangerous},
		{PermEnvironmentsWrite, "Deploy", "Record deployments; create, start, stop and delete environments.", RiskDangerous},
		{PermAutomationsManage, "Manage automations", "Create, change, run and test automations and bots.", RiskDangerous},
		{PermAIManage, "Configure AI", "AI providers and which model does what.", RiskDangerous},
	}},
	{Key: "admin", Label: "Administration", Permissions: []PermissionInfo{
		{PermUsageRead, "See usage", "Token usage, costs and prices.", RiskSafe},
		{PermTranscriptsRead, "Read session transcripts", "The raw transcripts of your own coding-agent sessions and the facts derived from them: every command, file and tool result they hold.", RiskCareful},
		{PermAuditRead, "Read the audit log", "Who changed access, settings and secrets.", RiskCareful},
		{PermServerRead, "Read server diagnostics", "The server's status, recent log (secrets blanked), metrics, profiles (goroutine dumps, CPU and heap; a CPU profile samples the running server) and AI request counts, as wp server and wp ai usage show them (admins' tokens).", RiskCareful},
		{PermForgesManage, "Manage forges", "Forge connections and their secrets.", RiskDangerous},
		{PermAdmin, "Administer Waypoint", "Users, invites, global webhooks, system limits (admins only).", RiskDangerous},
	}},
}

var permissionPresets = []PermissionPreset{
	{Key: "assistant", Label: "Assistant",
		Description: "Reads and writes issues, pages, the glossary and design notes, searches, comments and asks you things. Starts, runs, merges and deletes nothing.",
		Permissions: []Permission{PermIssuesRead, PermIssuesCreate, PermIssuesEdit, PermIssuesComment, PermTimeTrack,
			PermSearch, PermPagesRead, PermPagesWrite, PermGlossaryRead, PermGlossaryWrite, PermDesignRead, PermDesignWrite,
			PermLinksWrite, PermAttachmentsRead, PermAttachmentsWrite, PermRequestsRead, PermRequestsCreate, PermNotifications}},
	{Key: "read", Label: "Read only", Description: "Every safe read; changes nothing.", Permissions: readPermissions()},
}

// readPermissions are the safe permissions that only read.
func readPermissions() []Permission {
	return []Permission{PermIssuesRead, PermSearch, PermPagesRead, PermGlossaryRead, PermDesignRead, PermAttachmentsRead,
		PermRequestsRead, PermAgentsRead, PermSessionsRead, PermPullsRead, PermEnvironmentsRead, PermAutomationsRead, PermUsageRead}
}

var knownPermissions = func() map[Permission]PermissionInfo {
	m := map[Permission]PermissionInfo{}
	for _, g := range permissionGroups {
		for _, p := range g.Permissions {
			m[p.Key] = p
		}
	}
	return m
}()

// Permissions is the catalog of permissions and presets.
func (s *Service) Permissions() PermissionCatalog {
	return PermissionCatalog{Groups: permissionGroups, Presets: permissionPresets}
}

// KnownPermission reports whether p is in the catalog.
func KnownPermission(p Permission) bool { _, ok := knownPermissions[p]; return ok }

// PermissionLabel is p's label in the catalog, or p itself.
func PermissionLabel(p Permission) string {
	if i, ok := knownPermissions[p]; ok {
		return i.Label
	}
	return string(p)
}

// PermissionSet is what a permission token may do. A nil set means the
// token is not restricted.
type PermissionSet map[Permission]bool

// Has reports whether the set grants p; a nil set grants everything.
func (ps PermissionSet) Has(p Permission) bool { return ps == nil || ps[p] }

// List is the set's permissions in a stable order.
func (ps PermissionSet) List() []Permission {
	out := make([]Permission, 0, len(ps))
	for p, ok := range ps {
		if ok {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// normalizePermissions checks a token's permission list and returns it
// sorted and de-duplicated. nil stays nil (unrestricted); an empty list is
// a token that can only say who it is.
func normalizePermissions(in []Permission) ([]Permission, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]Permission, 0, len(in))
	for _, p := range in {
		p = Permission(strings.TrimSpace(string(p)))
		if !KnownPermission(p) {
			return nil, invalid("unknown permission %q", p)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}

func permissionsJSON(ps []Permission) any {
	if ps == nil {
		return nil
	}
	b, _ := json.Marshal(ps)
	return string(b)
}

func parsePermissions(v sql.NullString) []Permission {
	if !v.Valid {
		return nil
	}
	out := []Permission{}
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return []Permission{}
	}
	return out
}

func permissionSet(ps []Permission) PermissionSet {
	if ps == nil {
		return nil
	}
	set := PermissionSet{}
	for _, p := range ps {
		set[p] = true
	}
	return set
}

// Restricted reports whether the actor came in with a permission token.
func (a *Actor) Restricted() bool { return a != nil && a.Permissions != nil }

// Allows reports whether the actor may do what p covers. People in the
// browser and unrestricted tokens may do everything their role allows.
func (a *Actor) Allows(p Permission) bool { return a == nil || a.Permissions.Has(p) }

// need refuses an actor whose token lacks p, and records the refusal so
// the token's owner sees it on the settings page.
func (a *Actor) need(p Permission) error {
	if a.Allows(p) {
		return nil
	}
	return &PermissionError{Permission: p}
}

// PermissionError is a refusal because the token lacks a permission.
type PermissionError struct {
	Permission Permission
	// What was refused, when it is not a route or a tool: shown to the owner.
	What string
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("this token lacks the %q permission (%s); its owner can grant it in Settings → API tokens", string(e.Permission), PermissionLabel(e.Permission))
}

func (e *PermissionError) Unwrap() error { return ErrForbidden }

// TokenRefusal is something a permission token was refused, counted per
// permission, so its owner can decide whether to grant it.
type TokenRefusal struct {
	Permission Permission `json:"permission"`
	Count      int        `json:"count"`
	LastAt     time.Time  `json:"last_at"`
	What       string     `json:"what"`
}

// RecordRefusal counts a refusal for the actor's token. It is best-effort:
// a failure to record never changes the answer.
func (s *Service) RecordRefusal(ctx context.Context, actor *Actor, p Permission, what string) {
	if actor == nil || actor.TokenID == 0 || !actor.Restricted() {
		return
	}
	if len(what) > 200 {
		what = what[:200]
	}
	_, err := s.db.W.ExecContext(ctx, `INSERT INTO api_token_refusals(token_id, permission, count, last_at, what) VALUES (?,?,1,?,?)
		ON CONFLICT(token_id, permission) DO UPDATE SET count = count + 1, last_at = excluded.last_at, what = excluded.what`,
		actor.TokenID, string(p), s.nowMS(), what)
	if err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// Refused records err's refusal when it is a missing permission, and
// returns err unchanged, so callers can wrap any service call with it.
func (s *Service) Refused(ctx context.Context, actor *Actor, what string, err error) error {
	var pe *PermissionError
	if errors.As(err, &pe) {
		if pe.What != "" {
			what = pe.What
		}
		s.RecordRefusal(ctx, actor, pe.Permission, what)
	}
	return err
}

func (s *Service) tokenRefusals(ctx context.Context, ids []int64) (map[int64][]TokenRefusal, error) {
	out := map[int64][]TokenRefusal{}
	if len(ids) == 0 {
		return out, nil
	}
	// #nosec G202 -- only placeholders are concatenated; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT token_id, permission, count, last_at, what FROM api_token_refusals
		WHERE token_id IN (`+placeholders(len(ids))+`) ORDER BY last_at DESC`, int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, at int64
		var r TokenRefusal
		var p string
		if err := rows.Scan(&id, &p, &r.Count, &at, &r.What); err != nil {
			return nil, err
		}
		r.Permission, r.LastAt = Permission(p), msTime(at)
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}

// ClearTokenRefusals forgets a token's refusals.
func (s *Service) ClearTokenRefusals(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.canManageTokens(); err != nil {
		return err
	}
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE id = ? AND user_id = ?`, id, actor.id()).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return notFound("token")
	}
	_, err := s.db.W.ExecContext(ctx, `DELETE FROM api_token_refusals WHERE token_id = ?`, id)
	return err
}

// canManageTokens refuses permission tokens: whatever they were granted,
// they can't make, change or reveal tokens, so they can't grant themselves
// more.
func (a *Actor) canManageTokens() error {
	if err := a.canWrite(); err != nil {
		return err
	}
	if a.Restricted() {
		return fmt.Errorf("a token with permissions can't manage tokens: %w", ErrForbidden)
	}
	return nil
}

// TokenPatch changes a token: its name, and what it may do.
type TokenPatch struct {
	Name *string
	// SetPermissions applies Permissions (nil: unrestricted).
	SetPermissions bool
	Permissions    []Permission
	ReadOnly       *bool
}

// UpdateAPIToken changes one of the actor's tokens. Changing what a token
// may do is for people: an agent can't widen its own or another's access.
func (s *Service) UpdateAPIToken(ctx context.Context, actor *Actor, id int64, p TokenPatch) (*APIToken, error) {
	if err := actor.canManageTokens(); err != nil {
		return nil, err
	}
	if (p.SetPermissions || p.ReadOnly != nil) && actor.IsAgent() {
		return nil, forbidden("changing what a token may do is for people, not agents")
	}
	old, err := s.apiToken(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	t := *old
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return nil, invalid("token name is required")
		}
		t.Name = name
	}
	if p.SetPermissions {
		perms, err := normalizePermissions(p.Permissions)
		if err != nil {
			return nil, err
		}
		t.Permissions = perms
	}
	if p.ReadOnly != nil {
		t.ReadOnly = *p.ReadOnly
	}
	if t.Permissions != nil {
		t.ReadOnly = false
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE api_tokens SET name = ?, read_only = ?, permissions = ? WHERE id = ? AND user_id = ?`,
		t.Name, boolInt(t.ReadOnly), permissionsJSON(t.Permissions), id, actor.id()); err != nil {
		return nil, err
	}
	if detail := accessChange(old, &t); detail != "" {
		s.audit(ctx, actor, AuditEntry{Action: "token.permissions", Category: AuditAccess, TargetType: "token", TargetID: t.ID,
			Target: t.Name, Detail: detail, About: t.Agent})
	}
	return s.apiToken(ctx, actor, id)
}

// accessChange describes a change in what a token may do, for the audit log.
func accessChange(old, t *APIToken) string {
	access := func(t *APIToken) string {
		switch {
		case t.Permissions != nil:
			return "permissions"
		case t.ReadOnly:
			return "read-only"
		}
		return "full access"
	}
	from, to := access(old), access(t)
	if from != "permissions" || to != "permissions" {
		if from == to {
			return ""
		}
		if to == "permissions" {
			return "limited to " + permissionNames(t.Permissions)
		}
		return "changed from " + from + " to " + to
	}
	var added, removed []Permission
	for _, p := range t.Permissions {
		if !slices.Contains(old.Permissions, p) {
			added = append(added, p)
		}
	}
	for _, p := range old.Permissions {
		if !slices.Contains(t.Permissions, p) {
			removed = append(removed, p)
		}
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "granted "+permissionNames(added))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+permissionNames(removed))
	}
	return strings.Join(parts, "; ")
}

func permissionNames(ps []Permission) string {
	if len(ps) == 0 {
		return "nothing"
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}
