package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AutopilotOpenSession is one of the person's open sessions in a project
// that autopilot isn't doing anything with.
type AutopilotOpenSession struct {
	AutopilotSession
	// State: working, waiting or idle, as on the Sessions tab; Fresh: it
	// (re)started and nothing was typed in it yet, so it sits at an empty
	// prompt whatever its state says.
	State string    `json:"state"`
	Fresh bool      `json:"fresh,omitempty"`
	Task  *IssueRef `json:"task,omitempty"` // the task it is working on
	// Startable: autopilot can start the next task in it now; else Why not.
	Startable  bool      `json:"startable"`
	Why        string    `json:"why,omitempty"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// sessionProjectSQL is the project a session (s) works in, as autopilot
// sees it: its repo's, else that of the task it holds or last took.
const sessionProjectSQL = `coalesce(s.project_id, (SELECT ci.project_id FROM issues ci WHERE ci.id = s.issue_id),
	(SELECT ri.project_id FROM agent_runs rr JOIN issues ri ON ri.id = rr.issue_id WHERE rr.session_id = s.id ORDER BY rr.started_at DESC, rr.id DESC LIMIT 1))`

// autopilotOpenLimit: sessions listed per project.
const autopilotOpenLimit = 20

// autopilotOpenSessions lists the person's open sessions by project: their
// own (not task sessions), heard from in the last day, with no live step.
func (s *Service) autopilotOpenSessions(ctx context.Context, uid int64) (map[int64][]*AutopilotOpenSession, error) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+sessionCols+`, coalesce(p.key || '-' || i.number, ''), coalesce(i.title, ''), coalesce(i.type, ''),
		EXISTS (SELECT 1 FROM agent_runs r WHERE r.session_id = s.id AND r.ended_at IS NULL), coalesce(`+sessionProjectSQL+`, 0)
		FROM agent_sessions s LEFT JOIN issues i ON i.id = s.issue_id LEFT JOIN projects p ON p.id = i.project_id
		WHERE s.user_id = ? AND s.ended_at IS NULL AND s.state != ? AND s.kind != ? AND s.last_seen_at > ?
		AND NOT EXISTS (SELECT 1 FROM autopilot_steps st WHERE st.session_id = s.id AND st.state IN (?, ?, ?))
		ORDER BY s.last_seen_at DESC LIMIT 200`,
		uid, SessionEnded, SessionTask, now-sessionEndedAfter.Milliseconds(), StepDue, StepWaiting, StepStarting)
	if err != nil {
		return nil, err
	}
	type row struct {
		x    *AgentSession
		task IssueRef
		busy bool
		pid  int64
	}
	var list []row
	for rows.Next() {
		var r row
		x, err := scanSession(scanWith{rows, []any{&r.task.Key, &r.task.Title, &r.task.Type, &r.busy, &r.pid}}, now)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if x.State == SessionEnded || r.pid == 0 {
			continue
		}
		r.x = x
		list = append(list, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := map[int64][]*AutopilotOpenSession{}
	for _, r := range list {
		pid := r.pid
		if len(out[pid]) >= autopilotOpenLimit {
			continue
		}
		o := &AutopilotOpenSession{
			AutopilotSession: AutopilotSession{ID: r.x.ID, Title: r.x.Title, Machine: r.x.Machine, URL: fmt.Sprintf("/agents/sessions/%d", r.x.ID)},
			State:            r.x.State, LastSeenAt: r.x.LastSeenAt,
		}
		if r.busy && r.task.Key != "" {
			o.Task = &r.task
		}
		o.Fresh = r.x.State == SessionWorking && s.freshSession(ctx, r.x.ID)
		o.Why = openSessionBusy(r.x, o.Fresh, r.busy)
		o.Startable = o.Why == ""
		out[pid] = append(out[pid], o)
	}
	return out, nil
}

// scanWith scans a row's leading columns as sc's dest does, then extra.
type scanWith struct {
	sc    interface{ Scan(...any) error }
	extra []any
}

func (w scanWith) Scan(dest ...any) error { return w.sc.Scan(append(dest, w.extra...)...) }

// openSessionBusy says why autopilot can't start a task in the session now,
// or "" when it can.
func openSessionBusy(x *AgentSession, fresh, holdsTask bool) string {
	switch {
	case x.State == SessionWaiting:
		return "it is waiting on you"
	case holdsTask:
		return "it is working on a task; autopilot starts the next one after it"
	case x.State == SessionWorking && !fresh:
		return "it is working"
	}
	return ""
}

// StartAutopilotSession has autopilot start the next task in one of the
// person's idle sessions now, as it would in one that had just finished a
// task (the person's Start button, WP-307). The limits, the merge policy's
// order and the runner's check of its screen still apply.
func (s *Service) StartAutopilotSession(ctx context.Context, a *Actor, sessionID int64) error {
	if err := a.Need(PermSessionsStart); err != nil {
		return err
	}
	if err := a.RequirePersonDirect("starting a task with autopilot"); err != nil {
		return err
	}
	if err := a.CanWrite(); err != nil {
		return err
	}
	now := s.nowMS()
	var pid sql.NullInt64
	x, err := scanSession(scanWith{s.db.R.QueryRowContext(ctx, `SELECT `+sessionCols+`, `+sessionProjectSQL+` FROM agent_sessions s WHERE s.id = ? AND s.user_id = ?`,
		sessionID, a.ID()), []any{&pid}}, now)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	switch {
	case x.State == SessionEnded:
		return invalid("that session has ended")
	case x.Kind == SessionTask:
		return invalid("that is a task session: it hands off by itself")
	case !pid.Valid:
		return invalid("that session's repo isn't attached to a project")
	}
	on, _, err := s.autopilotFor(ctx, s.db.R, a.ID(), pid.Int64)
	if err != nil {
		return err
	}
	if !on {
		return invalid("autopilot is off in %s: turn it on there first", s.projectKeyOf(ctx, pid.Int64))
	}
	var open int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM agent_runs WHERE session_id = ? AND ended_at IS NULL`, x.ID).Scan(&open); err != nil {
		return err
	}
	if why := openSessionBusy(x, x.State == SessionWorking && s.freshSession(ctx, x.ID), open > 0); why != "" {
		return invalid("autopilot can't start a task in that session now: %s", why)
	}
	var st *stepRow
	err = s.write(ctx, a, func(t *tx) error {
		var live int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`, x.ID, StepDue, StepWaiting, StepStarting).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return invalid("autopilot is already about to start a task in that session (Start now on its line starts it at once)")
		}
		_, run, err := lastDoneInSession(ctx, t, x.ID)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET left_alone_at = NULL WHERE id = ?`, x.ID); err != nil {
			return err
		}
		if err := s.putStep(ctx, t, x, pid.Int64, run, now, StepDue, "", "you started it", now, 0); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET created_at = ?, attempts = 0, asked = 1 WHERE session_id = ? AND state = ?`, now-autopilotMergeWaitMax.Milliseconds(), x.ID, StepDue); err != nil {
			return err
		}
		st, err = scanStep(t.QueryRowContext(ctx, `SELECT `+stepCols+` FROM autopilot_steps WHERE session_id = ? AND state = ?`, x.ID, StepDue))
		return err
	})
	if err != nil {
		return err
	}
	st.byHand = true
	s.goBG(func() {
		if err := s.advanceStep(context.WithoutCancel(ctx), st); err != nil {
			if readFault(err) && !missingOrDenied(err) {
				s.log.WarnContext(ctx, "advance step failed", "session", sessionID, "err", err)
			}
		}
	})
	return nil
}
