package core

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	// MaxTaskSessions is how many task sessions a person has at once from
	// hand-offs (WP-53's per-person setting replaces it).
	MaxTaskSessions = 1
	// HandoffReserve is how long a reserved task waits for its session to
	// take it (wp next KEY) before the lease lapses.
	HandoffReserve = 10 * time.Minute
)

// Hand-off statuses.
const (
	HandoffReserved = "reserved"
	HandoffNone     = "none"
	HandoffLimit    = "limit"
	HandoffBusy     = "busy"
)

// Outcomes a task session's wp done reports (RecordHandoff).
const (
	OutcomeStarted = "started"
	OutcomeNone    = "none"
	OutcomeFailed  = "failed"
	OutcomeSkipped = "skipped"
)

// HandoffResult is Handoff's answer.
type HandoffResult struct {
	Status    string   `json:"status"`
	Task      *Issue   `json:"task,omitempty"`
	Related   []*Issue `json:"related,omitempty"`
	Live      int      `json:"live,omitempty"`
	Remaining []string `json:"remaining,omitempty"`
}

// liveTaskSessions counts a person's task sessions that are still going
// (other than except) plus tasks reserved for sessions that haven't started
// (leased by the agent with no run yet).
func (s *Service) liveTaskSessions(ctx context.Context, q queryer, userID, except int64, agent string) (int, error) {
	now := s.nowMS()
	var n, pending int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM agent_sessions WHERE user_id = ? AND kind = 'task' AND finished_at IS NULL AND ended_at IS NULL AND id != ?
		AND last_seen_at > ?`, userID, except, now-sessionIdleAfter.Milliseconds()).Scan(&n); err != nil {
		return 0, err
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM issues i WHERE i.claimed_by = ? AND i.claimed_until > ?
		AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = i.id AND r.ended_at IS NULL)`, agent, now).Scan(&pending); err != nil {
		return 0, err
	}
	return n + pending, nil
}

// Handoff picks the next ready task for the agent, ignoring any task that is
// leased (by this agent's other sessions too), and reserves it, plus with
// batch the related ones wp batch would take, for a session about to start.
func (s *Service) Handoff(ctx context.Context, actor *Actor, q AgentQuery, batch bool) (*HandoffResult, error) {
	if err := actor.Need(PermSessionsStart); err != nil {
		return nil, err
	}
	id, err := agentIdentity(actor, q.Agent)
	if err != nil {
		return nil, err
	}
	if actor.ReadOnly {
		return nil, invalid("a read-only token can't hand off")
	}
	if p, err := s.pauseFor(ctx, actor, id); err != nil {
		return nil, err
	} else if p != nil {
		return nil, &ErrAgentsPaused{Pause: p, Agent: id}
	}
	if actor.Session != 0 {
		if rem, err := s.boundRemaining(ctx, s.db.R, actor.Session); err != nil {
			return nil, err
		} else if len(rem) > 0 {
			return &HandoffResult{Status: HandoffBusy, Remaining: rem}, nil
		}
	}
	q.Peek, q.Agent = true, id
	ready, _, err := s.agentWork(ctx, actor, []string{id}, q)
	if err != nil {
		return nil, err
	}
	res := &HandoffResult{Status: HandoffNone}
	err = s.write(ctx, actor, func(t *tx) error {
		res = &HandoffResult{Status: HandoffNone}
		live, err := s.liveTaskSessions(ctx, t, actor.ID(), actor.Session, id)
		if err != nil {
			return err
		}
		if live >= MaxTaskSessions {
			res.Status, res.Live = HandoffLimit, live
			return nil
		}
		for _, qi := range ready {
			ok, err := s.reserve(ctx, t, qi.issue.ID, id)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			i, err := s.issueByID(ctx, t, qi.issue.ID, false)
			if err != nil {
				return err
			}
			res.Status, res.Task = HandoffReserved, i
			if batch {
				res.Related, err = s.reserveRelated(ctx, t, actor, i, id)
			}
			return err
		}
		return nil
	})
	return res, err
}

// reserve leases an unclaimed, takeable issue to the agent for HandoffReserve,
// with no run and no status change: the new session's wp next KEY adopts it.
func (s *Service) reserve(ctx context.Context, t *tx, issueID int64, agent string) (bool, error) {
	i, err := s.issueByID(ctx, t, issueID, false)
	if err != nil {
		return false, err
	}
	now := s.nowMS()
	if i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now {
		return false, nil
	}
	if !s.agentCanTake(ctx, i, agent, now) {
		return false, nil
	}
	if done, err := finishedUnreturned(ctx, t, i); err != nil || done {
		return false, err
	}
	res, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = ?, claimed_until = ?, claimed_at = ?
		WHERE id = ? AND (claimed_by = '' OR claimed_until IS NULL OR claimed_until < ?)`, agent, now+HandoffReserve.Milliseconds(), now, issueID, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// finishedUnreturned: an agent already finished the issue at its current
// workflow step, and nobody has sent it back since: moved it to Backlog or
// To Do, reopened it, or moved its step. Automatic starts (autopilot, a
// hand-off) never reserve such a task (WP-ADR12): a queue that kept offering
// finished work would otherwise start one session after another on it.
func finishedUnreturned(ctx context.Context, q queryer, i *Issue) (bool, error) {
	var done bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runs r WHERE r.issue_id = ? AND r.step = ? AND r.outcome = 'done' AND r.ended_at IS NOT NULL
		AND NOT EXISTS(SELECT 1 FROM events e WHERE e.issue_id = r.issue_id AND e.created_at > r.ended_at AND (e.kind = ?
			OR (e.kind = ? AND (json_extract(e.data, '$.new_category') IN (?, ?) OR json_extract(e.data, '$.old_category') IN (?, ?))))))`,
		i.ID, i.Step, EvIssueStep, EvIssueStatus, CatBacklog, CatTodo, CatDone, CatCanceled).Scan(&done)
	return done, err
}

// reserveRelated reserves what wp batch would add to the task (up to 10).
func (s *Service) reserveRelated(ctx context.Context, t *tx, actor *Actor, i *Issue, agent string) ([]*Issue, error) {
	var ids []int64
	task, err := s.agentTaskWith(ctx, actor, i.ID, false, "")
	if err != nil || task == nil {
		return nil, err
	}
	for _, r := range task.Issue.Relations {
		if r.Kind != "blocks" && !r.Named && r.State != CatDone && r.State != CatCanceled {
			ids = append(ids, r.Issue.ID)
		}
	}
	for _, c := range task.Issue.Children {
		if !c.Closed() {
			ids = append(ids, c.ID)
		}
	}
	var out []*Issue
	for _, rid := range ids {
		if len(out) >= 10 {
			break
		}
		if _, _, err := s.getIssueFor(ctx, t, actor, itoa64(rid), RoleEditor, false); err != nil {
			continue
		}
		if ok, err := s.reserve(ctx, t, rid, agent); err != nil {
			return nil, err
		} else if ok {
			r, err := s.issueByID(ctx, t, rid, false)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// ReleaseHandoff gives back tasks reserved by Handoff whose session could
// not be started: only those still without a run.
func (s *Service) ReleaseHandoff(ctx context.Context, actor *Actor, keys []string) error {
	id, err := agentIdentity(actor, "")
	if err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		for _, k := range keys {
			iid, err := s.resolveIssueID(ctx, t, k)
			if err != nil {
				continue
			}
			if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?
				AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = issues.id AND r.ended_at IS NULL)`, iid, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordHandoff notes on the calling task session what its wp done did about
// the next task. Only started and none let the session end; a task that was
// ready and not started (failed), or --no-next (skipped), leaves it open, and
// failed also tells the person.
func (s *Service) RecordHandoff(ctx context.Context, actor *Actor, outcome, detail string) error {
	switch outcome {
	case OutcomeStarted, OutcomeNone, OutcomeFailed, OutcomeSkipped:
	default:
		return invalid("unknown hand-off outcome %q", outcome)
	}
	if actor == nil || actor.Session == 0 {
		return nil
	}
	var title string
	var project sql.NullInt64
	err := s.write(ctx, actor, func(t *tx) error {
		if err := t.QueryRowContext(ctx, `SELECT title, project_id FROM agent_sessions WHERE id = ? AND user_id = ?`, actor.Session, actor.ID()).Scan(&title, &project); err != nil {
			return err
		}
		return s.sessionTimeline(ctx, t, actor.Session, s.nowMS(), "handoff", "", 0, outcome, "", detail, project, map[string]any{"outcome": outcome})
	})
	if err != nil {
		return err
	}
	if outcome == OutcomeFailed {
		s.notify(ctx, actor.ID(), nil, NotifyAutomated, "A task session couldn't start the next task", fmt.Sprintf("%s: %s. The session stays open; start the next task with wp start KEY --bg.", strings.TrimSpace(title), detail))
	}
	s.publishSession(ctx, actor.Session)
	return nil
}

// boundRemaining lists the keys a task session was bound to that it hasn't
// finished: not closed, and no wp done in this session.
func (s *Service) boundRemaining(ctx context.Context, q queryer, sessionID int64) ([]string, error) {
	var keys string
	if err := q.QueryRowContext(ctx, `SELECT task_keys FROM agent_sessions WHERE id = ?`, sessionID).Scan(&keys); err != nil {
		return nil, err
	}
	var rem []string
	for _, k := range splitTaskKeys(keys) {
		iid, err := s.resolveIssueID(ctx, q, k)
		if err != nil {
			continue
		}
		i, err := s.issueByID(ctx, q, iid, false)
		if err != nil {
			continue
		}
		if i.Closed() {
			continue
		}
		var done bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runs WHERE session_id = ? AND issue_id = ? AND outcome = 'done')`, sessionID, iid).Scan(&done); err != nil {
			return nil, err
		}
		if !done {
			rem = append(rem, k)
		}
	}
	return rem, nil
}
