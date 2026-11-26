package core

// The identity types and the permission catalogue live in internal/core/kernel
// (WP-ADR151). These aliases and re-exports are the names the API and the CLI
// use; they stay for good.

import "git.jmann.me/claude/waypoint/internal/core/kernel"

type (
	Actor           = kernel.Actor
	AutomationRef   = kernel.AutomationRef
	User            = kernel.User
	UserRef         = kernel.UserRef
	UserPrefs       = kernel.UserPrefs
	SessionPrefs    = kernel.SessionPrefs
	SessionSettings = kernel.SessionSettings

	Permission      = kernel.Permission
	Risk            = kernel.Risk
	PermissionInfo  = kernel.PermissionInfo
	PermissionGroup = kernel.PermissionGroup
	PermissionSet   = kernel.PermissionSet
	PermissionError = kernel.PermissionError
)

// Roles, ordered so a higher value includes everything below it.
const (
	RoleNone   = kernel.RoleNone
	RoleViewer = kernel.RoleViewer
	RoleEditor = kernel.RoleEditor
	RoleOwner  = kernel.RoleOwner
)

const (
	RiskSafe      = kernel.RiskSafe
	RiskCareful   = kernel.RiskCareful
	RiskDangerous = kernel.RiskDangerous
)

const (
	PermIssuesRead        = kernel.PermIssuesRead
	PermIssuesCreate      = kernel.PermIssuesCreate
	PermIssuesEdit        = kernel.PermIssuesEdit
	PermIssuesComment     = kernel.PermIssuesComment
	PermIssuesDelete      = kernel.PermIssuesDelete
	PermIssuesShare       = kernel.PermIssuesShare
	PermIssuesPublish     = kernel.PermIssuesPublish
	PermTimeTrack         = kernel.PermTimeTrack
	PermSearch            = kernel.PermSearch
	PermPagesRead         = kernel.PermPagesRead
	PermPagesWrite        = kernel.PermPagesWrite
	PermPagesDelete       = kernel.PermPagesDelete
	PermGlossaryRead      = kernel.PermGlossaryRead
	PermGlossaryWrite     = kernel.PermGlossaryWrite
	PermGlossaryDelete    = kernel.PermGlossaryDelete
	PermDesignRead        = kernel.PermDesignRead
	PermDesignWrite       = kernel.PermDesignWrite
	PermDesignDelete      = kernel.PermDesignDelete
	PermKnowledgeApprove  = kernel.PermKnowledgeApprove
	PermLinksWrite        = kernel.PermLinksWrite
	PermAttachmentsRead   = kernel.PermAttachmentsRead
	PermAttachmentsWrite  = kernel.PermAttachmentsWrite
	PermRequestsRead      = kernel.PermRequestsRead
	PermRequestsCreate    = kernel.PermRequestsCreate
	PermRequestsAnswer    = kernel.PermRequestsAnswer
	PermRequestsRun       = kernel.PermRequestsRun
	PermNotifications     = kernel.PermNotifications
	PermViewsWrite        = kernel.PermViewsWrite
	PermProfile           = kernel.PermProfile
	PermSprintsManage     = kernel.PermSprintsManage
	PermProjectsManage    = kernel.PermProjectsManage
	PermReposManage       = kernel.PermReposManage
	PermWebhooksManage    = kernel.PermWebhooksManage
	PermTasksWork         = kernel.PermTasksWork
	PermAgentsRead        = kernel.PermAgentsRead
	PermAgentsManage      = kernel.PermAgentsManage
	PermSessionsRead      = kernel.PermSessionsRead
	PermSessionsReport    = kernel.PermSessionsReport
	PermSessionsStart     = kernel.PermSessionsStart
	PermRunnersRead       = kernel.PermRunnersRead
	PermRunnersRun        = kernel.PermRunnersRun
	PermRunnersHost       = kernel.PermRunnersHost
	PermCIReport          = kernel.PermCIReport
	PermPullsRead         = kernel.PermPullsRead
	PermPullsWrite        = kernel.PermPullsWrite
	PermPullsMerge        = kernel.PermPullsMerge
	PermAuditsRun         = kernel.PermAuditsRun
	PermEnvironmentsRead  = kernel.PermEnvironmentsRead
	PermEnvironmentsWrite = kernel.PermEnvironmentsWrite
	PermAutomationsRead   = kernel.PermAutomationsRead
	PermAutomationsManage = kernel.PermAutomationsManage
	PermAIUse             = kernel.PermAIUse
	PermAIManage          = kernel.PermAIManage
	PermUsageRead         = kernel.PermUsageRead
	PermAuditRead         = kernel.PermAuditRead
	PermServerRead        = kernel.PermServerRead
	PermForgesManage      = kernel.PermForgesManage
	PermAdmin             = kernel.PermAdmin
)

const gotifyMask = kernel.GotifyMask

var (
	SessionEfforts         = kernel.SessionEfforts
	SessionPermissionModes = kernel.SessionPermissionModes

	permissionGroups = kernel.PermissionGroups
)

// SystemActor acts with full access, for CLI bootstrap and background jobs.
func SystemActor() *Actor { return kernel.SystemActor() }

// KnownPermission reports whether p is in the catalog.
func KnownPermission(p Permission) bool { return kernel.KnownPermission(p) }

// PermissionLabel is p's label in the catalog, or p itself.
func PermissionLabel(p Permission) string { return kernel.PermissionLabel(p) }
