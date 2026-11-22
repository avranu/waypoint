package core

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// SessionOutstanding is work a session can do while it waits on something
// slow, because none of it needs the result (WP-1571).
type SessionOutstanding struct {
	// Audit and Findings: findings (41.2 style) of the session's open review
	// round's audit that have no verdict yet.
	Audit    int64    `json:"audit,omitempty"`
	Findings []string `json:"findings,omitempty"`
	// Requests: answered requests the session hasn't read (R-12).
	Requests []string `json:"requests,omitempty"`
	// Lines: each of them as a line naming the command to do it.
	Lines string `json:"lines,omitempty"`
}

// outstandingKind is the session_events kind that records what was named to a
// session as outstanding: bookkeeping, left out of its timeline and its trim.
const outstandingKind = "outstanding"

// Where outstanding work is named (the record's ref_type): at the start of a
// background wait (the PostToolUse hook) or at a Stop while it goes. Each
// names an item once, apart: the hook's answer can be lost (it is cut at 3 s,
// and Claude Code may drop it) and the agent may not act on it, so the Stop
// still names what is left once (WP-1571, audit #1344).
const (
	OutstandingAtWait = "wait"
	OutstandingAtStop = "stop"
)

// Empty says there is nothing to name.
func (o *SessionOutstanding) Empty() bool {
	return o == nil || (len(o.Findings) == 0 && len(o.Requests) == 0)
}

// SessionOutstanding is what the actor's session can do while it waits, to
// name at via (OutstandingAtWait or OutstandingAtStop): the findings with no
// verdict in the audit of its open review round, and the requests answered
// that it hasn't read (wp request ls --answered). Only what is there. It is
// not a plain read: it records what it names, and names nothing (Empty) when
// all of it was named at via before; when something is new, it names all of
// it, so the counts are the whole of what is outstanding.
func (s *Service) SessionOutstanding(ctx context.Context, actor *Actor, via string) (*SessionOutstanding, error) {
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
	// Each item is named to a session once at each of via, so an agent that
	// holds findings while a worker fixes them isn't re-prompted with them at
	// every stop (session_events, kind outstandingKind, ref_type via, label
	// the item's code).
	if len(out.Findings)+len(out.Requests) == 0 {
		return out, nil
	}
	fresh := 0
	err = s.write(ctx, actor, func(t *tx) error {
		for _, it := range append(slices.Clone(out.Findings), out.Requests...) {
			told, err := s.sessionTold(ctx, t, actor.Session, outstandingKind, via, it)
			if err != nil {
				return err
			}
			if told {
				continue
			}
			if err := s.sessionTimeline(ctx, t, actor.Session, s.nowMS(), outstandingKind, via, 0, it, "", "named while a run was going", sql.NullInt64{}, nil); err != nil {
				return err
			}
			fresh++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if fresh == 0 {
		return &SessionOutstanding{}, nil
	}
	out.Lines = outstandingLines(out)
	return out, nil
}

// sessionTold says whether the session has a session_events row of kind and
// label (refType "": any), the mark of something told to it once.
func (s *Service) sessionTold(ctx context.Context, t *tx, session int64, kind, refType, label string) (bool, error) {
	var told bool
	err := t.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_events WHERE session_id = ? AND kind = ? AND (? = '' OR ref_type = ?) AND label = ?)`,
		session, kind, refType, refType, label).Scan(&told)
	return told, err
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
		fmt.Fprintf(&b, "- %s you haven't read (%s): `wp request ls --answered`.\n", plural1(n, "answered request"), strings.Join(o.Requests, ", "))
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
		"). Work of yours is waiting; do what doesn't need its result now instead of ending the turn idle:\n" + o.Lines
}
