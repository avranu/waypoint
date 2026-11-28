package core

// The identity types and the permission catalogue live in internal/core/kernel
// (WP-ADR151). These aliases and re-exports are the names the API and the CLI
// use; they stay for good.

import (
	"context"

	"git.jmann.me/claude/waypoint/internal/core/email"
	"git.jmann.me/claude/waypoint/internal/core/kernel"
	"git.jmann.me/claude/waypoint/internal/core/users"
)

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

var (
	SessionEfforts         = kernel.SessionEfforts
	SessionPermissionModes = kernel.SessionPermissionModes
)

// SystemActor acts with full access, for CLI bootstrap and background jobs.
func SystemActor() *Actor { return kernel.SystemActor() }

// KnownPermission reports whether p is in the catalog.
func KnownPermission(p Permission) bool { return kernel.KnownPermission(p) }

// PermissionLabel is p's label in the catalog, or p itself.
func PermissionLabel(p Permission) string { return kernel.PermissionLabel(p) }

// Event is something that happened; see kernel.Event.
type Event = kernel.Event

// Audit entries and their categories are the kernel's (it writes audit_log).
type AuditEntry = kernel.AuditEntry

const (
	AuditApproval   = kernel.AuditApproval
	AuditDelegation = kernel.AuditDelegation
	AuditControl    = kernel.AuditControl
	AuditSettings   = kernel.AuditSettings
	AuditAccess     = kernel.AuditAccess
)

// The email domain's types (internal/core/email).
type (
	EmailSettings      = email.Settings
	EmailSettingsInput = email.SettingsInput
)

const (
	SMTPStartTLS = email.StartTLS
	SMTPTLS      = email.TLS
	SMTPNone     = email.None
)

// EmailConfigured says whether email can be sent. It takes no actor, so it is
// not on the email domain's Service.
func (s *Service) EmailConfigured(ctx context.Context) bool { return s.mailer.Configured(ctx) }

// The users domain's types and functions (internal/core/users).
type (
	APIToken          = users.APIToken
	TokenSpec         = users.TokenSpec
	TokenPatch        = users.TokenPatch
	TokenRefusal      = users.TokenRefusal
	UserLink          = users.UserLink
	PendingInvite     = users.PendingInvite
	AcceptInviteInput = users.AcceptInviteInput
	Session           = users.Session
	CreateUserInput   = users.CreateUserInput
	UserPatch         = users.UserPatch

	PermissionPreset  = users.PermissionPreset
	PermissionCatalog = users.PermissionCatalog
)

const CacheWarmCompact = users.CacheWarmCompact

func HashPassword(pw string) string      { return users.HashPassword(pw) }
func CheckPassword(hash, pw string) bool { return users.CheckPassword(hash, pw) }
func WarmLogin()                         { users.WarmLogin() }
func CanonicalIP(ip string) string       { return users.CanonicalIP(ip) }

// The users domain's operations that take no actor: sign-in has none yet, and
// public reads need none. The rest are on its Service, embedded in Service.
func (s *Service) UserByID(ctx context.Context, id int64) (*User, error) {
	return s.users.UserByID(ctx, id)
}
func (s *Service) Login(ctx context.Context, login, password, userAgent, ip string) (string, *User, error) {
	return s.users.Login(ctx, login, password, userAgent, ip)
}
func (s *Service) LoginAccountID(ctx context.Context, login string) (int64, error) {
	return s.users.LoginAccountID(ctx, login)
}
func (s *Service) SignedInFrom(ctx context.Context, userID int64, ip string) (bool, error) {
	return s.users.SignedInFrom(ctx, userID, ip)
}
func (s *Service) SessionActor(ctx context.Context, tok string) (*Actor, error) {
	return s.users.SessionActor(ctx, tok)
}
func (s *Service) SessionUser(ctx context.Context, tok string) (*User, error) {
	return s.users.SessionUser(ctx, tok)
}
func (s *Service) Logout(ctx context.Context, tok string) error { return s.users.Logout(ctx, tok) }
func (s *Service) TokenActor(ctx context.Context, tok string) (*Actor, error) {
	return s.users.TokenActor(ctx, tok)
}
func (s *Service) GetLink(ctx context.Context, tok string) (*UserLink, error) {
	return s.users.GetLink(ctx, tok)
}
func (s *Service) AcceptInvite(ctx context.Context, tok string, in AcceptInviteInput, userAgent, ip string) (string, *User, error) {
	return s.users.AcceptInvite(ctx, tok, in, userAgent, ip)
}
func (s *Service) ResetPassword(ctx context.Context, tok, password string) error {
	return s.users.ResetPassword(ctx, tok, password)
}
func (s *Service) Avatar(ctx context.Context, id int64) ([]byte, string, error) {
	return s.users.Avatar(ctx, id)
}
func (s *Service) UserCount(ctx context.Context) (int, error) { return s.users.UserCount(ctx) }
func (s *Service) BootstrapInvite(ctx context.Context) (*UserLink, error) {
	return s.users.BootstrapInvite(ctx)
}
func (s *Service) Permissions() PermissionCatalog { return s.users.Permissions() }
