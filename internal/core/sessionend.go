package core

import (
	"context"
	"database/sql"
	"strings"
)

// only sessions Waypoint started for tasks are ended, and only once truly finished (no background work, no open request); a person's own sessions are never ended

// taskSessionDone reports whether a task session is done for good: it truly
// finished at this Stop, holds no unfinished run, and finished at least one
// task with wp done.
func (s *Service) taskSessionDone(ctx context.Context, q queryer, sessionID int64) (bool, error) {
	var kind, pending, keys string
	var finished sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT kind, pending, task_keys, finished_at FROM agent_sessions WHERE id = ?`, sessionID).Scan(&kind, &pending, &keys, &finished); err != nil {
		return false, err
	}
	if kind != SessionTask || pending != "" || !finished.Valid || len(splitTaskKeys(keys)) == 0 {
		return false, nil
	}
	var open int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM agent_runs WHERE session_id = ? AND ended_at IS NULL`, sessionID).Scan(&open); err != nil || open > 0 {
		return false, err
	}
	key, run, err := lastDoneInSession(ctx, q, sessionID)
	if err != nil || key == "" {
		return false, err
	}
	if rem, err := s.boundRemaining(ctx, q, sessionID); err != nil || len(rem) > 0 {
		return false, err
	}
	// Its last wp done must have handed off, or found nothing to hand to:
	// a ready task it could not start, or --no-next, keeps the session open.
	var ended sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT ended_at FROM agent_runs WHERE id = ?`, run).Scan(&ended); err != nil {
		return false, err
	}
	var outcome sql.NullString
	err = q.QueryRowContext(ctx, `SELECT json_extract(data, '$.outcome') FROM session_events WHERE session_id = ? AND kind = 'handoff' AND created_at >= ?
		ORDER BY id DESC LIMIT 1`, sessionID, ended.Int64).Scan(&outcome)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if outcome.String != OutcomeStarted && outcome.String != OutcomeNone {
		return false, nil
	}
	prompted, err := s.promptedAfterDone(ctx, q, sessionID, run)
	return !prompted, err
}

// promptedAfterDone: a person prompted the session after its last wp done
// (a prompt event newer than the run's end), so they are using it and it is
// not ended. Prompts Waypoint sent itself (a follow-up's or message job's
// text) don't count. Hooks run with --no-prompts record none.
func (s *Service) promptedAfterDone(ctx context.Context, q queryer, sessionID, run int64) (bool, error) {
	var ended sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT ended_at FROM agent_runs WHERE id = ?`, run).Scan(&ended); err != nil {
		return false, err
	}
	return s.promptedSince(ctx, q, sessionID, ended.Int64)
}

// promptedSince: a person prompted the session after since (0: ever), as
// promptedAfterDone counts them.
func (s *Service) promptedSince(ctx context.Context, q queryer, sessionID, since int64) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT detail, typed_kind FROM session_events WHERE session_id = ? AND kind = 'prompt' AND created_at > ?`, sessionID, since)
	if err != nil {
		return false, err
	}
	type prompt struct{ text, typed string }
	var prompts []prompt
	for rows.Next() {
		var d, typed string
		if err := rows.Scan(&d, &typed); err != nil {
			_ = rows.Close()
			return false, err
		}
		prompts = append(prompts, prompt{d, typed})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(prompts) == 0 {
		return false, err
	}
	mrows, err := q.QueryContext(ctx, `SELECT body FROM session_messages WHERE session_id = ?`, sessionID)
	if err != nil {
		return false, err
	}
	var bodies []string
	for mrows.Next() {
		var b string
		if err := mrows.Scan(&b); err != nil {
			_ = mrows.Close()
			return false, err
		}
		bodies = append(bodies, strings.Join(strings.Fields(b), " "))
	}
	_ = mrows.Close()
	if err := mrows.Err(); err != nil {
		return false, err
	}
	for _, pr := range prompts {
		p := pasteText(strings.TrimSpace(strings.TrimSuffix(pr.text, "…")))
		own := ownPrompt(p, pr.typed)
		for _, b := range bodies {
			if strings.HasPrefix(b, p) {
				own = true
				break
			}
		}
		if !own {
			return true, nil
		}
	}
	return false, nil
}
