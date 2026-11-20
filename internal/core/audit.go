package core

import (
	"context"
	"strings"
)

// The audit log (WP-D15): what people decided about agents' work and about
// Waypoint itself: reviews and approvals, delegating work to agents,
// pausing and ordering agents, settings and access. Issue history keeps
// every field change; this keeps the decisions, across projects, in one
// place a person can read.

// Audit categories.
const (
	AuditApproval   = "approval"   // reviews of agent-written text, design decisions, PR approvals and merges
	AuditDelegation = "delegation" // work handed to agents or taken back
	AuditControl    = "control"    // pausing agents, the queue's order, releasing claims, bot runs
	AuditSettings   = "settings"   // automations, AI models, forges, the review note
	AuditAccess     = "access"     // tokens, device logins, members
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

// audit records a decision made in this transaction, by its actor.
// Automations and Waypoint's own work aren't people deciding, so they
// aren't recorded unless a forge names who acted.
func (t *tx) audit(e AuditEntry) {
	if !auditable(t.actor, e) {
		return
	}
	var userID any
	agent := ""
	if a := t.actor; a != nil && a.User != nil && a.User.ID != 0 {
		userID, agent = a.User.ID, a.agentName()
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
		excerpt(e.Target, 200), e.URL, excerpt(strings.Join(strings.Fields(e.Detail), " "), 300), t.s.nowMS()); err != nil {
		t.s.log.ErrorContext(t.ctx, "record audit entry", "action", e.Action, "err", err)
	}
}

// auditable: a person (or an agent through their token) decided, or a
// forge names who did.
func auditable(a *Actor, e AuditEntry) bool {
	if a != nil && a.User != nil && a.User.ID != 0 {
		return a.Automation == nil
	}
	return e.Actor != ""
}

// audit records a decision outside a write (one that changed nothing in
// Waypoint's own tables, or a forge's report).
func (s *Service) audit(ctx context.Context, actor *Actor, e AuditEntry) {
	if !auditable(actor, e) {
		return // and no write: Waypoint's own setup may call this mid-transaction
	}
	if err := s.write(ctx, actor, func(t *tx) error { t.audit(e); return nil }); err != nil {
		s.log.ErrorContext(ctx, "record audit entry", "action", e.Action, "err", err)
	}
}

func derefInt(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// pageLabel names a page in the log: its code and title.
func pageLabel(pg *Page) string {
	if pg.Code != "" {
		return pg.Code + " " + pg.Title
	}
	return pg.Title
}

// auditDelegation records a person handing an issue to agents, to other
// agents, or taking it back (to Anyone or Humans only). Moves between the
// two states that aren't delegation aren't recorded.
func auditDelegation(t *tx, projectID, issueID int64, key, from, to string) {
	if !agentDelegated(from) && !agentDelegated(to) {
		return
	}
	e := AuditEntry{Action: "delegate", Category: AuditDelegation, ProjectID: projectID, TargetType: "issue", TargetID: issueID,
		Target: key, URL: "/issue/" + key, About: to}
	switch {
	case !agentDelegated(to):
		e.Action, e.About, e.Detail = "undelegate", from, "took it back from "+from
	case !agentDelegated(from):
		e.Detail = "for " + to
	default:
		e.Detail = "for " + to + " (was " + from + ")"
	}
	t.audit(e)
}
