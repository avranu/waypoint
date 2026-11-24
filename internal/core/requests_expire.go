package core

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Hook-filed requests (one tool call, or a turn's end) expire once the agent moves on or its session ends; agent-filed requests never expire.

// Why a request expired, as people read it.
const (
	expiredNewPrompt = "The agent moved on: a new prompt started a later turn in its session, so a decision would reach it out of context."
	expiredEnded     = "The agent's session ended, so a decision can no longer reach it."
	expiredQuiet     = "The agent's session hasn't been heard from in a day, so it reads as ended."
)

// expirable is the SQL for the requests that expire: those a hook filed for
// one tool call, or from the end of a turn.
const expirable = `r.state = 'open' AND (r.kind = 'permission' OR r.tool_use_id != '' OR r.source = 'auto')`

// expireRequests moves the open, expirable requests that also match where
// to expired, with the reason, and emits request.updated for each. ended
// says the session ended: then its held follow-up stays held.
func (s *Service) expireRequests(ctx context.Context, t *tx, where string, args []any, reason string, ended bool, at int64) (int, error) {
	// #nosec G202 -- where is one of this file's fixed conditions; values are bound
	rows, err := t.QueryContext(ctx, `SELECT r.id, r.number, r.kind, r.user_id, r.project_id, r.session_id, r.title, r.agent, coalesce(p.key, '')
		FROM requests r LEFT JOIN projects p ON p.id = r.project_id WHERE `+expirable+` AND `+where+` ORDER BY r.id`, args...)
	if err != nil {
		return 0, err
	}
	type row struct {
		id, user          int64
		number            int
		kind, title, agnt string
		key               string
		project, session  sql.NullInt64
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.number, &r.kind, &r.user, &r.project, &r.session, &r.title, &r.agnt, &r.key); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	result, _ := json.Marshal(RequestResult{Reason: reason})
	n := 0
	for _, r := range list {
		res, err := t.ExecContext(ctx, `UPDATE requests SET state = 'expired', decision = 'expire', result = ?, decided_at = ?, delivered_at = ?, snoozed_until = NULL, updated_at = ?
			WHERE id = ? AND state = 'open'`, string(result), at, at, at, r.id)
		if err != nil {
			return n, err
		}
		if c, _ := res.RowsAffected(); c == 0 {
			continue
		}
		n++
		e := requestEvent(EvRequestUpdated, r.id, requestRef(r.key, r.number), r.number, r.kind, RequestExpired, r.user, r.project, r.session, r.title, r.agnt)
		e.Data["decision"] = "expired"
		e.Data["reason"] = reason
		if ended {
			e.Data["session_ended"] = true
		}
		t.emit(e)
	}
	return n, nil
}

// expireOnPrompt expires the session's requests filed before a new prompt
// (UserPromptSubmit at at): the turn they were for is over.
func (s *Service) expireOnPrompt(ctx context.Context, t *tx, sessionID, at int64) error {
	_, err := s.expireRequests(ctx, t, `r.session_id = ? AND r.created_at < ?`, []any{sessionID, at}, expiredNewPrompt, false, at)
	return err
}

// expireOnEnd expires the session's requests when it ends (SessionEnd).
func (s *Service) expireOnEnd(ctx context.Context, t *tx, sessionID, at int64) error {
	_, err := s.expireRequests(ctx, t, `r.session_id = ? AND r.created_at <= ?`, []any{sessionID, at}, expiredEnded, true, at)
	return err
}

// ExpireRequests is the sweep (hourly, and when Waypoint starts) for what
// the hooks didn't say, or said before Waypoint expired requests: a later
// prompt on the session's timeline, a session that ended, or one not heard
// from in a day (sessionEndedAfter). It returns how many expired.
func (s *Service) ExpireRequests(ctx context.Context) (int, error) {
	n := 0
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		now := s.nowMS()
		passes := []struct {
			where  string
			args   []any
			reason string
			ended  bool
		}{
			{`EXISTS (SELECT 1 FROM session_events e WHERE e.session_id = r.session_id AND e.kind = 'prompt' AND e.created_at > r.created_at)`, nil, expiredNewPrompt, false},
			{`r.session_id IN (SELECT id FROM agent_sessions WHERE state = 'ended')`, nil, expiredEnded, true},
			{`r.session_id IN (SELECT id FROM agent_sessions WHERE last_seen_at < ?)`, []any{now - sessionEndedAfter.Milliseconds()}, expiredQuiet, true},
		}
		for _, p := range passes {
			c, err := s.expireRequests(ctx, t, p.where, p.args, p.reason, p.ended, now)
			n += c
			if err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}
