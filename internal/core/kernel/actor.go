package kernel

import (
	"fmt"
	"slices"
	"strings"
)

// Restricted reports whether the actor came in with a permission token.
func (a *Actor) Restricted() bool { return a != nil && a.Permissions != nil }

// Allows reports whether the actor may do what p covers. People in the
// browser and unrestricted tokens may do everything their role allows.
func (a *Actor) Allows(p Permission) bool { return a == nil || a.Permissions.Has(p) }

// need refuses an actor whose token lacks p, and records the refusal so
// the token's owner sees it on the settings page.
func (a *Actor) Need(p Permission) error {
	if a.Allows(p) {
		return nil
	}
	return &PermissionError{Permission: p}
}

// CanManageTokens refuses permission tokens: whatever they were granted,
// they can't make, change or reveal tokens, so they can't grant themselves
// more.
func (a *Actor) CanManageTokens() error {
	if err := a.CanWrite(); err != nil {
		return err
	}
	if a.Restricted() {
		return fmt.Errorf("a token with permissions can't manage tokens: %w", ErrForbidden)
	}
	return nil
}

// person is a signed-in person in a browser: not a token, an automation or
// an MCP client.
func (a *Actor) Person() bool {
	return a != nil && a.User != nil && a.User.ID != 0 && a.TokenID == 0 && a.Automation == nil && !a.MCP && a.Agent == ""
}

// Roles, ordered so a higher value includes everything below it.
const (
	RoleNone   = 0
	RoleViewer = 1
	RoleEditor = 2
	RoleOwner  = 3
)

// MemberProjects is the SQL set, for `project_id IN <set>`, of the projects
// the actor is a member of with at least role min; for a job's scoped token
// (WP-418), those of its projects it has that role in.
func (a *Actor) MemberProjects(min int) (string, []any) {
	if a.Scope != nil {
		ids := make([]int64, 0, len(a.Scope))
		for id, r := range a.Scope {
			if r >= min {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return "(" + Placeholders(len(ids)) + ")", Int64Args(ids)
	}
	roles := ""
	switch {
	case min >= RoleOwner:
		roles = " AND role = 'owner'"
	case min == RoleEditor:
		roles = " AND role IN ('editor','owner')"
	}
	return "(SELECT project_id FROM project_members WHERE user_id = ?" + roles + ")", []any{a.ID()}
}

// AutomationRef is what an Actor carries of the automation that performs a
// change: the only fields read through the actor.
type AutomationRef struct {
	ID        int64
	Name      string
	ProjectID int64
	// SavedBy is the id of the person who last saved the automation (WP-D18),
	// 0 when an agent did.
	SavedBy int64
}

// RunnerOwner is the user whose runners the automation's steps may use: the
// one who last saved it; 0 when there is none (or no automation).
func (r *AutomationRef) RunnerOwner() int64 {
	if r == nil {
		return 0
	}
	return r.SavedBy
}

// Actor is who is performing an operation.
type Actor struct {
	User *User
	// ReadOnly is set for API tokens created read-only.
	ReadOnly bool
	// Permissions is what a permission token may do (WP-111); nil for
	// people in the browser and unrestricted tokens.
	Permissions PermissionSet
	// TokenID is the API token behind the request, if any.
	TokenID int64
	// RunnerJob is the runner job whose own token (WP-418) is behind the
	// request, if any.
	RunnerJob int64
	Scope     map[int64]int
	// Agent is the acting LLM's identity ("anthropic/claude-opus-5-5"), from
	// an agent token or the X-Waypoint-Agent header; empty for people.
	Agent string
	// MCP is set for requests over the MCP endpoint, which only agents use.
	MCP bool
	// AgentEnv names the coding agent whose shell the request came from
	// (the CLI's X-Waypoint-Agent-Env header), even when the caller said it
	// is a person (WAYPOINT_AGENT=none): whatever it reads lands in the
	// agent's context, so it never sees drafts (WP-42).
	AgentEnv string
	// Prestart: a launcher takes a task for a task session it is about to
	// start (the CLI's X-Waypoint-Prestart header, WP-1037). No person drives
	// that session, so the take counts as unattended.
	Prestart bool
	// Automation is set when an automation performs the change on behalf of
	// User (the person whose action triggered it).
	Automation  *AutomationRef
	Depth       int
	SessionHash string
	// Session is the agent session (WP-D16) the request came from, set from
	// the X-Waypoint-Session header: what the actor does is recorded on it.
	Session int64
	// ClientVersion is the wp that made the request, from its User-Agent
	// (WP-256); "" for anything else.
	ClientVersion string
	Budget        *int
	ForgeSync     bool
	TextAgent     string
}

func (a *Actor) ID() int64 {
	if a == nil || a.User == nil {
		return 0
	}
	return a.User.ID
}

// IsAgent reports whether an LLM agent is acting (WP-D10): it has an
// agent identity or came in over MCP. Everyone else counts as a person.
// A permission token always counts as an agent (WP-111).
func (a *Actor) IsAgent() bool { return a != nil && (a.Agent != "" || a.MCP || a.Permissions != nil) }

// HidesDrafts reports whether drafts (WP-42) are hidden from the actor:
// they are for people, so agents, anything run in an agent's shell or
// session, and automations never see them, wherever they look.
func (a *Actor) HidesDrafts() bool {
	return a == nil || a.User == nil || a.IsAgent() || a.AgentEnv != "" || a.Session != 0 || a.Automation != nil
}

// HidesCommandText reports whether the text of commands agents ran is kept
// from the actor: the same readers as drafts, as whatever it reads lands in
// an agent's context. They still see that a command was refused or risky,
// and its name.
func (a *Actor) HidesCommandText() bool { return a.HidesDrafts() }

// AgentName is the agent's identity for provenance ("mcp" when an MCP
// client didn't say); empty for people.
func (a *Actor) AgentName() string {
	switch {
	case a == nil:
		return ""
	case a.Agent != "":
		return a.Agent
	case a.MCP:
		return "mcp"
	}
	return ""
}

// RequireHuman refuses agents: approving, locking and deleting intent is
// for people.
func (a *Actor) RequireHuman(what string) error {
	if a.IsAgent() {
		return Forbidden("%s is for people, not agents", what)
	}
	return nil
}

// RequirePersonDirect is RequireHuman for what an agent mustn't do even
// with its person's token (WP-ADR9): the request comes from the web or the
// person's own terminal, not from a coding agent's shell, an agent session
// or an automation.
func (a *Actor) RequirePersonDirect(what string) error {
	if err := a.RequireHuman(what); err != nil {
		return err
	}
	if a.AgentEnv != "" || a.Session != 0 || a.Automation != nil {
		return Forbidden("%s is for people, in the web or their own terminal, not in an agent's shell", what)
	}
	return nil
}

// ActsAsPerson: the actor is a person acting directly (RequirePersonDirect
// would let them through). Only such an act counts as a person's intent
// in the trace to goals (WP-ADR50); anything else counts as an agent's.
func (a *Actor) ActsAsPerson() bool {
	return a != nil && a.User != nil && a.RequirePersonDirect("") == nil
}

// WritesAsAgent: what the actor writes isn't a person's own act (an agent's
// token, or a person's token in an agent's shell, session or automation), so
// it never approves itself: a design note it writes is a proposal, or counts
// on a person's task, and its edits are applied or pending (WP-ADR96).
func (a *Actor) WritesAsAgent() bool { return !a.ActsAsPerson() }

// CreditAgent is the agent an act is credited to in provenance: the
// agent's own name, or for a person's account acting through an agent's
// shell, session or an automation, a stand-in, so it never reads as the
// person's own act. "" only for a person acting directly (or no user).
func (a *Actor) CreditAgent() string {
	if n := a.AgentName(); n != "" {
		return n
	}
	switch {
	case a == nil || a.User == nil || a.ActsAsPerson():
		return ""
	case a.Automation != nil:
		return "automation"
	}
	return "agent-shell"
}

// IsAdmin: a job's scoped token is never an admin, whoever's it is.
func (a *Actor) IsAdmin() bool { return a != nil && a.User != nil && a.User.IsAdmin && a.Scope == nil }

func (a *Actor) ViaAutomation() any {
	if a == nil || a.Automation == nil || a.Automation.ID <= 0 {
		return nil
	}
	return a.Automation.ID
}

func (a *Actor) CanWrite() error {
	if a == nil || a.User == nil {
		return ErrUnauthorized
	}
	if a.ReadOnly {
		return fmt.Errorf("read-only token: %w", ErrForbidden)
	}
	return nil
}

// SystemActor acts with full access, for CLI bootstrap and background jobs.
func SystemActor() *Actor {
	return &Actor{User: &User{ID: 0, Username: "system", DisplayName: "Waypoint", IsAdmin: true}}
}

// Placeholders is n SQL placeholders for an IN list, or NULL for none.
func Placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Int64Args is ids as query arguments.
func Int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
