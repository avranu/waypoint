package core

import (
	"context"
	"fmt"
	"strings"
)

// SessionOutstanding is work a session can do while it waits on something
// slow, because none of it needs the result (WP-1571).
type SessionOutstanding struct {
	// Audit and Findings: findings (41.2 style) of the session's open review
	// round's audit that have no verdict yet.
	Audit    int64    `json:"audit,omitempty"`
	Findings []string `json:"findings,omitempty"`
	// Requests: answered requests the session hasn't been told of (R-12).
	Requests []string `json:"requests,omitempty"`
	// Lines: each of them as a line naming the command to do it.
	Lines string `json:"lines,omitempty"`
}

// Empty says there is nothing to name.
func (o *SessionOutstanding) Empty() bool {
	return o == nil || (len(o.Findings) == 0 && len(o.Requests) == 0)
}

// SessionOutstanding reads what the actor's session can do while it waits:
// the findings with no verdict in the audit of its open review round, and
// the requests answered that it hasn't been told of (wp request ls
// --answered). Only what is there.
func (s *Service) SessionOutstanding(ctx context.Context, actor *Actor) (*SessionOutstanding, error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return nil, ErrUnauthorized
	}
	out := &SessionOutstanding{}
	if actor.Session == 0 {
		return out, nil // not a session: nothing is its own
	}
	// The newest open round whose audit has ended with findings.
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM work_log WHERE kind = 'review' AND session_id = ? AND state IN (?, ?) ORDER BY id DESC`,
		actor.Session, RoundPending, RoundRunning)
	if err != nil {
		return nil, err
	}
	var rounds []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		rounds = append(rounds, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, round := range rounds {
		audit, _ := s.roundEndedAudit(ctx, round)
		if audit == 0 {
			continue
		}
		frows, err := s.db.R.QueryContext(ctx, `SELECT ordinal FROM code_audit_findings WHERE audit_id = ? AND state = 'open' ORDER BY ordinal`, audit)
		if err != nil {
			return nil, err
		}
		for frows.Next() {
			var n int
			if err := frows.Scan(&n); err != nil {
				_ = frows.Close()
				return nil, err
			}
			out.Findings = append(out.Findings, fmt.Sprintf("%d.%d", audit, n))
		}
		if err := frows.Close(); err != nil {
			return nil, err
		}
		if len(out.Findings) > 0 {
			out.Audit = audit
			break
		}
	}
	page, err := s.ListRequests(ctx, actor, RequestQuery{State: "answered", Mine: true, Session: actor.Session, Limit: 20})
	if err != nil {
		return nil, err
	}
	for _, r := range page.Requests {
		out.Requests = append(out.Requests, r.Code)
	}
	out.Lines = outstandingLines(out)
	return out, nil
}

// outstandingLines are the bullets naming each piece of work and its command.
func outstandingLines(o *SessionOutstanding) string {
	var b strings.Builder
	if n := len(o.Findings); n > 0 {
		shown := strings.Join(o.Findings[:min(n, 12)], ", ")
		if n > 12 {
			shown += ", …"
		}
		fmt.Fprintf(&b, "- %s without a verdict in your review round's audit #%d (%s): `wp audit show %d`, then `wp audit mark ID fixed -m \"what you did\"` or `rejected -m \"why\"`.\n",
			plural1(n, "finding"), o.Audit, shown, o.Audit)
	}
	if n := len(o.Requests); n > 0 {
		fmt.Fprintf(&b, "- %s you haven't acted on (%s): `wp request ls --answered`.\n", plural1(n, "answered request"), strings.Join(o.Requests, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// stopText is what a Stop says while a background run goes (running: what
// it is): the work that doesn't need its result. "" when there is none.
func (o *SessionOutstanding) stopText(running []string) string {
	if o.Empty() {
		return ""
	}
	return "Something is still running in the background (" + strings.Join(running[:min(len(running), 3)], "; ") +
		"). Work that doesn't need its result is waiting; do it now instead of ending the turn idle:\n" + o.Lines
}
