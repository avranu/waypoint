package kernel

import (
	"context"
	"strings"
)

// Audit categories.
const (
	AuditApproval   = "approval"
	AuditDelegation = "delegation"
	AuditControl    = "control"
	AuditSettings   = "settings"
	AuditAccess     = "access"
)

// AuditEntry is one decision.
type AuditEntry struct {
	Action     string // review, delegate, agent.pause, pr.merged, …
	Category   string
	ProjectID  int64 // 0: not about one project
	TargetType string
	TargetID   int64
	Target     string // a label people recognise: WP-12, a rule's name, owner/repo#12
	URL        string
	Detail     string // one line
	// About is the agent whose work or access the decision is about (the
	// agent delegated to, whose text was approved).
	About string
	// Actor is a forge login, for decisions a forge reported (a merge); the
	// person and agent otherwise come from the transaction's actor.
	Actor string
}

// Audit records a decision made in this transaction, by its actor.
// Automations and Waypoint's own work aren't people deciding, so they
// aren't recorded unless a forge names who acted. The kernel owns writing
// audit_log; reading it is the owner's.
func (t *Tx) Audit(e AuditEntry) {
	if !Auditable(t.Actor, e) {
		return
	}
	var userID any
	agent := ""
	if a := t.Actor; a != nil && a.User != nil && a.User.ID != 0 {
		userID, agent = a.User.ID, a.AgentName()
	}
	var project, target any
	if e.ProjectID != 0 {
		project = e.ProjectID
	}
	if e.TargetID != 0 {
		target = e.TargetID
	}
	if _, err := t.ExecContext(context.Background(), `INSERT INTO audit_log(user_id, actor, agent, about_agent, action, category, project_id, target_type, target_id, target, url, detail, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, userID, e.Actor, agent, e.About, e.Action, e.Category, project, e.TargetType, target,
		Excerpt(e.Target, 200), e.URL, Excerpt(strings.Join(strings.Fields(e.Detail), " "), 300), t.k.NowMS()); err != nil {
		t.k.Log.ErrorContext(t.Ctx, "record audit entry", "action", e.Action, "err", err)
	}
}

// Auditable: a person (or an agent through their token) decided, or a
// forge names who did.
func Auditable(a *Actor, e AuditEntry) bool {
	if a != nil && a.User != nil && a.User.ID != 0 {
		return a.Automation == nil
	}
	return e.Actor != ""
}
