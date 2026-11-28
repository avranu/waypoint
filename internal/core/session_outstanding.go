package core

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// SessionOutstanding is work a session can do while it waits on something
// slow, because none of it needs the result (WP-1571). Lines says all of it,
// so the session needn't run a command to read any of it, which costs a turn
// on its whole context (WP-769): the findings it has in full from its
// round's prompt are named, and the answers are given in full.
type SessionOutstanding struct {
	// Audit and Findings: findings (41.2 style) still to do (roundTodo: no
	// verdict, or confirmed and not filed) in the audit of the session's
	// review round whose prompt went in (RoundRunning). Before it goes in
	// they are the prompt's to give.
	Audit    int64    `json:"audit,omitempty"`
	Findings []string `json:"findings,omitempty"`
	// Requests: the decided requests given in Lines (R-12). At a Stop they
	// are marked told (delivered_at) as its own are; at the wait the hook
	// marks them (RequestIDs, SessionDecisionsTold) once it has given them,
	// so a reply it never gets loses no answer. Delivery is at least once:
	// two hooks at once, or a Stop or a turn's start before the hook says it
	// told them, can give an answer twice.
	Requests   []string `json:"requests,omitempty"`
	RequestIDs []int64  `json:"request_ids,omitempty"`
	// Lines: the findings and the answers, to put before the agent.
	Lines string `json:"lines,omitempty"`

	todo      []CodeAuditFinding
	decisions []*Request
}

// outstandingKind is the session_events kind that records what was named to a
// session as outstanding: bookkeeping, left out of its timeline and its trim.
const outstandingKind = "outstanding"

// Where outstanding work is named (the record's ref_type): at the start of a
// background wait (the PostToolUse hook, which records it once its answer is
// written) or at a Stop while it goes. A Stop counts a finding recorded at
// either as named, so it doesn't spend a turn repeating the wait (WP-1861);
// a wait counts only its own record, so it still names what a Stop did. A
// finding already recorded is named again only with a new one, which names
// the whole list. Claude Code can drop the hook's answer after it is written,
// and then the Stop stays quiet too: the cost WP-1861 accepts.
const (
	OutstandingAtWait = "wait"
	OutstandingAtStop = "stop"
)

// Empty says there is nothing to name.
func (o *SessionOutstanding) Empty() bool {
	return o == nil || (len(o.Findings) == 0 && len(o.Requests) == 0)
}

// SessionOutstanding is what the actor's session can do while it waits, to
// give at via (OutstandingAtWait or OutstandingAtStop): the findings still to
// do in the audit of its review round whose prompt went in (at a Stop, one
// still running because a subagent it started runs on, roundAtStop), and the
// decisions on its requests it hasn't been told of. It is not a plain read.
// Findings are recorded as named, and none are named when all count as named
// already (outstandingKind's rows, as OutstandingAtWait says); when one is
// new, all are, so the count is the whole. At the wait, decisions are given as a bundled Stop would (takeDecisions) but left
// untold until the hook says it gave them, and only as many as fit the
// hook's room. A Stop takes them, and gives them only with new findings: one
// that would carry the agent on for answers alone already took those that
// warrant it, and the rest wait for a turn's start (WP-176).
func (s *Service) SessionOutstanding(ctx context.Context, actor *Actor, via string) (*SessionOutstanding, error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return nil, ErrUnauthorized
	}
	out := &SessionOutstanding{}
	if actor.Session == 0 {
		return out, nil
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM work_log WHERE kind = 'review' AND session_id = ? AND state = ? ORDER BY id DESC`,
		actor.Session, RoundRunning)
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
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, round := range rounds {
		id, _ := s.roundEndedAudit(ctx, round)
		if id == 0 {
			continue
		}
		a, err := s.auditRow(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := s.loadFindings(ctx, a); err != nil {
			return nil, err
		}
		if todo := s.roundTodo(ctx, a); len(todo) > 0 {
			out.Audit, out.todo = id, todo
			for _, f := range todo {
				out.Findings = append(out.Findings, f.ID)
			}
			break
		}
	}
	fresh := 0
	err = s.write(ctx, actor, func(t *tx) error {
		for _, it := range out.Findings {
			// Printed at the wait counts at a Stop; the wait's own record doesn't
			// hide a finding the Stop named.
			seenVia := via
			if via == OutstandingAtStop {
				seenVia = ""
			}
			told, err := s.sessionTold(ctx, t, actor.Session, outstandingKind, seenVia, it)
			if err != nil {
				return err
			}
			if told {
				continue
			}
			if via != OutstandingAtWait {
				if err := s.sessionTimeline(ctx, t, actor.Session, s.nowMS(), outstandingKind, via, 0, it, "", "named while a run was going", sql.NullInt64{}, nil); err != nil {
					return err
				}
			}
			fresh++
		}
		if fresh == 0 && via == OutstandingAtStop {
			return nil
		}
		if via == OutstandingAtStop {
			var err error
			out.decisions, err = s.takeDecisions(ctx, t, actor, &AgentSession{ID: actor.Session}, false, true)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if fresh == 0 {
		out.Audit, out.Findings, out.todo = 0, nil, nil
	}
	if via == OutstandingAtWait {
		where, args := decisionsWhere(actor, &AgentSession{ID: actor.Session}, false)
		list, err := s.loadRequests(ctx, actor, true, where+` ORDER BY r.id LIMIT 50`, args...)
		if err != nil {
			return nil, err
		}
		var quiet []int64
		list = slices.DeleteFunc(list, func(r *Request) bool {
			if r.quiet() {
				quiet = append(quiet, r.ID)
			}
			return r.quiet()
		})
		if err := s.SessionDecisionsTold(ctx, actor, quiet); err != nil {
			return nil, err
		}
		out.decisions = fitDecisions(list, outstandingRoom-len(outstandingLines(out)))
		for _, r := range out.decisions {
			out.RequestIDs = append(out.RequestIDs, r.ID)
		}
	}
	for _, r := range out.decisions {
		out.Requests = append(out.Requests, r.Code)
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

// outstandingRoom is how long the wait hook's Lines may be: Claude Code
// keeps a hook's context whole up to 10,000 characters, and the ETA goes
// before them.
const outstandingRoom = 9000

// outstandingFindingsRoom is how much of outstandingRoom the findings' lines
// take at most, so long titles and paths leave room for the answers.
const outstandingFindingsRoom = 6000

// fitDecisions is as many of list, in order, as DecisionsText says in room
// characters; one too long for it waits for a Stop or a turn's start, which
// have no such limit.
func fitDecisions(list []*Request, room int) []*Request {
	var fit []*Request
	for _, r := range list {
		if len(DecisionsText(append(slices.Clone(fit), r))) <= room {
			fit = append(fit, r)
		}
	}
	return fit
}

// outstandingLines names the findings still to do, which the session has in
// full from its round's prompt, says how to mark them, and gives the
// decisions in full.
func outstandingLines(o *SessionOutstanding) string {
	var b strings.Builder
	if n := len(o.todo); n > 0 {
		fmt.Fprintf(&b, "- %s still to do in your review round's audit #%d, in full in the round's prompt. Record each verdict: %s.\n",
			plural1(n, "finding"), o.Audit, markHint("ID", "why"))
		b.WriteString("<untrusted>\n")
		shown, size := 0, 0
		for _, f := range o.todo[:min(n, 30)] {
			line := fmt.Sprintf("%s [%s] %s", f.ID, f.Severity, untrusted(f.Title))
			if f.Path != "" {
				line += fmt.Sprintf(" (%s%s)", untrusted(f.Path), lineSuffix(f.Line))
			}
			if size += len(line) + 1; size > outstandingFindingsRoom {
				break
			}
			b.WriteString(line + "\n")
			shown++
		}
		b.WriteString("</untrusted>\n")
		if n > shown {
			fmt.Fprintf(&b, "and %d more.\n", n-shown)
		}
	}
	if text := DecisionsText(o.decisions); text != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SessionFindingsNamed records findings (41.2 style) as named at the wait to
// the actor's session: the hook says so once it has printed them.
func (s *Service) SessionFindingsNamed(ctx context.Context, actor *Actor, findings []string) error {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return ErrUnauthorized
	}
	if len(findings) > 200 {
		return invalid("at most 200 findings")
	}
	if actor.Session == 0 || len(findings) == 0 {
		return nil
	}
	return s.write(ctx, actor, func(t *tx) error {
		for _, it := range findings {
			if len(it) > 40 {
				return invalid("finding id too long")
			}
			told, err := s.sessionTold(ctx, t, actor.Session, outstandingKind, OutstandingAtWait, it)
			if err != nil {
				return err
			}
			if told {
				continue
			}
			if err := s.sessionTimeline(ctx, t, actor.Session, s.nowMS(), outstandingKind, OutstandingAtWait, 0, it, "", "named while a run was going", sql.NullInt64{}, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// SessionDecisionsTold marks the decisions ids of the actor's session told:
// the wait hook says so once it has given them (SessionOutstanding). Only
// decided requests of the session that are to be told are marked.
func (s *Service) SessionDecisionsTold(ctx context.Context, actor *Actor, ids []int64) error {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return ErrUnauthorized
	}
	if len(ids) > 50 {
		return invalid("at most 50 requests")
	}
	if actor.Session == 0 || len(ids) == 0 {
		return nil
	}
	return s.write(ctx, actor, func(t *tx) error {
		// #nosec G202 -- only ? placeholders are joined in
		_, err := t.ExecContext(ctx, `UPDATE requests SET delivered_at = ? WHERE session_id = ? AND user_id = ? AND delivered_at IS NULL AND decided_at IS NOT NULL AND via = '' AND id IN (`+placeholders(len(ids))+`)`,
			append([]any{s.nowMS(), actor.Session, actor.ID()}, int64Args(ids)...)...)
		return err
	})
}

// stopText is what a Stop says while a background run goes (running: what
// it is): the work that doesn't need its result. "" when there is none.
func (o *SessionOutstanding) stopText(running []string) string {
	if o.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Still running in the background:\n")
	for _, r := range running[:min(len(running), 3)] {
		b.WriteString("- " + excerpt(r, 100) + "\n")
	}
	if n := len(running) - 3; n > 0 {
		fmt.Fprintf(&b, "- and %d more\n", n)
	}
	return b.String() + "Work of yours is waiting; do what doesn't need those results now instead of ending the turn idle:\n" + o.Lines
}
