package core

import "context"

// audit records a decision outside a write (one that changed nothing in
// Waypoint's own tables, or a forge's report).
func (s *Service) audit(ctx context.Context, actor *Actor, e AuditEntry) {
	if !auditable(actor, e) {
		return
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
