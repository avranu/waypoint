package core

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SessionOutstanding is work a session can do while it waits on something
// slow, because none of it needs the result (WP-1571). Lines says all of it,
// each finding and each answer in full: a session told to run a command to
// read them spends a turn on its whole context doing so (WP-769).
type SessionOutstanding struct {
	// Audit and Findings: findings (41.2 style) of the session's open review
	// round's audit still to do (roundTodo: no verdict, or confirmed and not
	// filed).
	Audit    int64    `json:"audit,omitempty"`
	Findings []string `json:"findings,omitempty"`
	// Requests: the decided requests given in Lines (R-12), now marked told
	// (delivered_at) as a Stop or a wakeup would.
	Requests []string `json:"requests,omitempty"`
	// Lines: the findings and the answers, to put before the agent.
	Lines string `json:"lines,omitempty"`

	todo      []CodeAuditFinding
	delivered []*Request
}

// outstandingKind is the session_events kind that records what was named to a
// session as outstanding: bookkeeping, left out of its timeline and its trim.
const outstandingKind = "outstanding"

// Where outstanding work is named (the record's ref_type): at the start of a
// background wait (the PostToolUse hook) or at a Stop while it goes. Each
// records a finding once, apart: the hook's answer can be lost (it is cut at
// 3 s, and Claude Code may drop it) and the agent may not act on it, so the
// Stop still names what is left (WP-1571, audit #1344). A finding already
// recorded is named again only with a new one, which names the whole list.
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
// do in the audit of its open review round, and the decisions on its
// requests it hasn't been told of. It is not a plain read. Findings are
// recorded as named, and none are named (Empty) when all were named at via
// before; when one is new, all are, so the count is the whole. Decisions are
// taken as a Stop takes them (takeDecisions, bundled): marked told, so no
// later Stop or wakeup gives them again. A Stop gives them only with new
// findings: one that would carry the agent on for answers alone already
// took those that warrant it, and the rest wait for a turn's start (WP-176).
//
// A decision given at the wait whose reply the hook never gets (it is cut
// at 3 s) is lost to the agent, as at a Stop whose hook reply is lost; it
// stays readable with wp request show.
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
	// Each finding is named to a session once at each of via, so an agent
	// that holds findings while a worker fixes them isn't re-prompted with
	// them at every stop (session_events, kind outstandingKind, ref_type via,
	// label the finding's code).
	fresh := 0
	err = s.write(ctx, actor, func(t *tx) error {
		for _, it := range out.Findings {
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
		if fresh == 0 && via == OutstandingAtStop {
			return nil
		}
		var err error
		out.delivered, err = s.takeDecisions(ctx, t, actor, &AgentSession{ID: actor.Session}, false, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	if fresh == 0 {
		out.Audit, out.Findings, out.todo = 0, nil, nil
	}
	for _, r := range out.delivered {
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

// outstandingFindingsBudget is about how much of Lines the findings take
// in full; past it the rest are a line each. Claude Code keeps a hook's
// context whole up to 10,000 characters.
const outstandingFindingsBudget = 6000

// outstandingLines says the findings still to do, each in full while the
// budget lasts, how to mark them, and the decisions given with them.
func outstandingLines(o *SessionOutstanding) string {
	var b strings.Builder
	if n := len(o.todo); n > 0 {
		fmt.Fprintf(&b, "- %s still to do in your review round's audit #%d, below. Record each verdict: %s.\n",
			plural1(n, "finding"), o.Audit, markHint("ID", "why"))
		b.WriteString("<untrusted>\n")
		start := b.Len()
		for _, f := range o.todo {
			if b.Len()-start < outstandingFindingsBudget {
				writeAuditFinding(&b, f)
				continue
			}
			fmt.Fprintf(&b, "### %s [%s] %s", f.ID, f.Severity, untrusted(f.Title))
			if f.Path != "" {
				fmt.Fprintf(&b, " (%s%s)", untrusted(f.Path), lineSuffix(f.Line))
			}
			b.WriteString("\n")
		}
		b.WriteString("</untrusted>\n")
	}
	if text := DecisionsText(o.delivered); text != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text)
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
