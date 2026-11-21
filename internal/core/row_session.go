package core

import (
	"context"
	"database/sql"
	"errors"
)

// rowQuerier is what a session lookup needs: a transaction or a pool.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sessionStamp is the agent session behind an actor and the task that session
// held when it acted, for the session_id and session_issue_id columns of a row
// the actor writes (WP-ADR145 point 6). Both are NULL (nil) when the actor
// has no session (a person, a client that sent none) or the session holds no
// task: unknown, never zero. One mechanism for every table: a write path
// takes both from here and does not read the header itself.
func sessionStamp(ctx context.Context, q rowQuerier, a *Actor) (session, task any) {
	if a == nil || a.Session == 0 {
		return nil, nil
	}
	var held sql.NullInt64
	var ran, open bool
	// The session row may be gone (deleted since the header was resolved);
	// then the task is unknown and the session id would break the key.
	err := q.QueryRowContext(ctx, `SELECT s.issue_id,
			EXISTS (SELECT 1 FROM agent_runs r WHERE r.session_id = s.id AND r.issue_id = s.issue_id),
			EXISTS (SELECT 1 FROM agent_runs r WHERE r.session_id = s.id AND r.issue_id = s.issue_id AND (r.ended_at IS NULL OR r.outcome = 'paused'))
		FROM agent_sessions s WHERE s.id = ?`, a.Session).Scan(&held, &ran, &open)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	// Any other failure (busy, I/O): the session is known from the header, the
	// task is not. Keep the session so the row stays countable; NULL the task.
	if err != nil {
		return a.Session, nil
	}
	// agent_sessions.issue_id is the last task claimed and is never cleared:
	// when every run of the session on it has ended, the task is finished
	// with, not held (a session with no run on it keeps the benefit of the doubt).
	if held.Valid && (open || !ran) {
		task = held.Int64
	}
	return a.Session, task
}

// sessionStamp is the stamp for a row written in this transaction.
func (t *tx) sessionStamp(ctx context.Context) (session, task any) {
	return sessionStamp(ctx, t, t.actor)
}

// creditSession is the stamp for a row credited to agent: the acting session
// only when the credit is an agent's, so a person's text never names a session.
func (t *tx) creditSession(ctx context.Context, agent string) (session, task any) {
	if agent == "" {
		return nil, nil
	}
	return t.sessionStamp(ctx)
}
