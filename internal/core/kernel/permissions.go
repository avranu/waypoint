package kernel

import (
	"fmt"
	"slices"
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

	PermUsageRead    Permission = "usage.read"
	PermAuditRead    Permission = "audit.read"
	PermServerRead   Permission = "server.read"
	PermForgesManage Permission = "forges.manage"
	PermAdmin        Permission = "admin"
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

var PermissionGroups = []PermissionGroup{
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
		{PermAuditRead, "Read the audit log", "Who changed access, settings and secrets.", RiskCareful},
		{PermServerRead, "Read server diagnostics", "The server's status, recent log (secrets blanked), metrics and AI request counts, as wp server and wp ai usage show them (admins' tokens).", RiskCareful},
		{PermForgesManage, "Manage forges", "Forge connections and their secrets.", RiskDangerous},
		{PermAdmin, "Administer Waypoint", "Users, invites, global webhooks, system limits (admins only).", RiskDangerous},
	}},
}

var knownPermissions = func() map[Permission]PermissionInfo {
	m := map[Permission]PermissionInfo{}
	for _, g := range PermissionGroups {
		for _, p := range g.Permissions {
			m[p.Key] = p
		}
	}
	return m
}()

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
