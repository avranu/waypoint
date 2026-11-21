package core

import (
	"context"
	"database/sql"
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
	// The session row may be gone (deleted since the header was resolved);
	// then the task is unknown and the session id would break the key.
	err := q.QueryRowContext(ctx, `SELECT issue_id FROM agent_sessions WHERE id = ?`, a.Session).Scan(&held)
	if err != nil {
		return nil, nil
	}
	if held.Valid {
		task = held.Int64
	}
	return a.Session, task
}

// sessionStamp is the stamp for a row written in this transaction.
func (t *tx) sessionStamp(ctx context.Context) (session, task any) {
	return sessionStamp(ctx, t, t.actor)
}
