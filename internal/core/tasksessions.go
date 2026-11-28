package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Session kinds.
const (
	SessionTask  = "task"
	SessionYours = "yours"
)

// BigContext is the context, in tokens, past which a session that already
// finished a task should hand the next one to a fresh session.
const BigContext = 150_000

const maxTaskKeys = 20

var taskKeyRE = lazyre.New(`^[A-Z][A-Z0-9]{0,9}-[0-9]{1,9}$`)

// splitTaskKeys reads a WP_TASK binding (KEY[,KEY…]): upper-cased, in
// order, each once; anything that isn't an issue key is dropped.
func splitTaskKeys(s string) []string {
	var out []string
	for _, k := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		k = strings.ToUpper(strings.TrimSpace(k))
		if !taskKeyRE.MatchString(k) || slices.Contains(out, k) {
			continue
		}
		if out = append(out, k); len(out) == maxTaskKeys {
			break
		}
	}
	return out
}

// BigSessionText is the note for a session that holds a lot of context
// after finishing a task (the story's wording).
func BigSessionText(doneKey string, contextTokens int64) string {
	return fmt.Sprintf("%s is done and this session holds %s: the next task is cheaper in a fresh session.", doneKey, kTokens(contextTokens))
}

// kTokens writes a token count the way people say it: 180k, 1.2M.
func kTokens(n int64) string {
	switch {
	case n >= 999_500:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprint(n)
}

// lastDoneInSession is the task most recently finished (wp done) in a
// session, and its run; "" if none.
func lastDoneInSession(ctx context.Context, q queryer, sessionID int64) (string, int64, error) {
	var key string
	var run int64
	err := q.QueryRowContext(ctx, `SELECT p.key || '-' || i.number, r.id FROM agent_runs r JOIN issues i ON i.id = r.issue_id
		JOIN projects p ON p.id = i.project_id WHERE r.session_id = ? AND r.outcome = 'done' AND r.ended_at IS NOT NULL
		ORDER BY r.ended_at DESC, r.id DESC LIMIT 1`, sessionID).Scan(&key, &run)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return key, run, err
}

// lastEndedInSession is the task most recently finished or given back on
// purpose in a session, and its run; released says which. "" if neither. A
// run that lapsed or was swept doesn't count: only task_releases says an
// agent meant it (WP-ADR75).
func lastEndedInSession(ctx context.Context, q queryer, sessionID int64) (key string, run int64, released bool, err error) {
	var outcome string
	err = q.QueryRowContext(ctx, `SELECT p.key || '-' || i.number, r.id, r.outcome FROM agent_runs r JOIN issues i ON i.id = r.issue_id
		JOIN projects p ON p.id = i.project_id WHERE r.session_id = ? AND r.ended_at IS NOT NULL
		AND (r.outcome = 'done' OR (r.outcome = 'released' AND EXISTS(SELECT 1 FROM task_releases tr WHERE tr.run_id = r.id)))
		ORDER BY r.ended_at DESC, r.id DESC LIMIT 1`, sessionID).Scan(&key, &run, &outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return key, run, outcome == "released", err
}

// finishedThisTurn is the session's last finished task and run, if no prompt
// has started another turn since; "" otherwise. The prompt that delivers that
// task's own review round is still about it, unless a person sent another.
func (s *Service) finishedThisTurn(ctx context.Context, t *tx, sessionID int64) (string, int64, error) {
	key, run, err := lastDoneInSession(ctx, t, sessionID)
	if err != nil || key == "" {
		return "", 0, err
	}
	var issueID, ended int64
	if err := t.QueryRowContext(ctx, `SELECT issue_id, ended_at FROM agent_runs WHERE id = ?`, run).Scan(&issueID, &ended); err != nil {
		return "", 0, err
	}
	prompted, err := s.promptedSince(ctx, t, sessionID, ended)
	if err != nil || prompted {
		return "", 0, err
	}
	var later bool
	if err := t.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_events WHERE session_id = ? AND kind = 'prompt' AND created_at > ?)`, sessionID, ended).Scan(&later); err != nil {
		return "", 0, err
	}
	if later {
		if r, err := reviewRound(ctx, t, sessionID, issueID); err != nil || r == nil || r.state != RoundRunning {
			return "", 0, err
		}
	}
	return key, run, nil
}

// bigSessionNote is, at a Stop, the one line the person sees (Claude Code's
// systemMessage, never model context) when a session of theirs holds more
// than BigContext after a task in it was finished. It is shown once per
// finished task, and never in a task session: those hand off by ending
// (WP-34 slice 2).
func (s *Service) bigSessionNote(ctx context.Context, t *tx, sessionID int64) (string, error) {
	var kind string
	var tokens int64
	var noted sql.NullInt64
	if err := t.QueryRowContext(ctx, `SELECT kind, context_tokens, noted_run FROM agent_sessions WHERE id = ?`, sessionID).Scan(&kind, &tokens, &noted); err != nil {
		return "", err
	}
	if kind == SessionTask || tokens <= BigContext {
		return "", nil
	}
	key, run, err := lastDoneInSession(ctx, t, sessionID)
	if err != nil || key == "" || (noted.Valid && run == noted.Int64) {
		return "", err
	}
	if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET noted_run = ? WHERE id = ?`, run, sessionID); err != nil {
		return "", err
	}
	return BigSessionText(key, tokens), nil
}

// SessionHint is what the CLI tells the server about the session a take
// comes from, beyond its id: the WP_TASK binding in its environment and the
// context its last model call carried (both may be empty).
type SessionHint struct {
	Task          string
	ContextTokens int64
}

// TaskSessionWarning is what wp next, batch or take adds to its answer about
// the session it ran in (WP-34). It never refuses: the task is claimed
// either way. In a task session, tasks its binding doesn't cover (not a
// bound task, a sub-task of one or related to one) should get their own
// session. In your own session, a session that finished a task and holds
// more than BigContext should hand the next one to a fresh session. Empty
// when there is nothing to say, or for a task the agent already held.
func (s *Service) TaskSessionWarning(ctx context.Context, actor *Actor, t *AgentTask, hint SessionHint) string {
	if t == nil || t.Issue == nil || t.Issue.Issue == nil || t.Resumed || actor == nil {
		return ""
	}
	kind, bound, tokens := "", splitTaskKeys(hint.Task), hint.ContextTokens
	if actor.Session != 0 {
		var k, keys string
		var stored int64
		err := s.db.R.QueryRowContext(ctx, `SELECT kind, task_keys, context_tokens FROM agent_sessions WHERE id = ? AND user_id = ?`,
			actor.Session, actor.ID()).Scan(&k, &keys, &stored)
		if err == nil {
			kind = k
			if b := splitTaskKeys(keys); len(b) > 0 {
				bound = b
			}
			if tokens <= 0 {
				tokens = stored
			}
		}
	}
	taken := append([]*Issue{t.Issue.Issue}, t.Related...)
	if len(bound) > 0 || kind == SessionTask {
		var off []string
		for _, i := range taken {
			if !s.boundCovers(ctx, bound, i) {
				off = append(off, i.Key)
			}
		}
		if len(off) == 0 {
			return ""
		}
		verb := "isn't"
		if len(off) > 1 {
			verb = "aren't"
		}
		return fmt.Sprintf("Note: this session was started for %s, and %s %s part of that work (claimed here anyway; wp start %s --bg gives it its own session).",
			strings.Join(bound, ", "), strings.Join(off, ", "), verb, off[0])
	}
	if actor.Session == 0 || tokens <= BigContext {
		return ""
	}
	key, _, err := lastDoneInSession(ctx, s.db.R, actor.Session)
	if err != nil || key == "" || strings.EqualFold(key, t.Issue.Key) {
		return ""
	}
	return "Note: " + BigSessionText(key, tokens) + " (" + t.Issue.Key + " is claimed anyway.)"
}

// boundCovers: the issue is part of a task session's work: one of its bound
// tasks, a sub-task of one at any depth (wp next EPIC hands those out,
// WP-730), or related to one (what wp batch would claim).
func (s *Service) boundCovers(ctx context.Context, bound []string, i *Issue) bool {
	if slices.Contains(bound, strings.ToUpper(i.Key)) {
		return true
	}
	var ids []int64
	for _, k := range bound {
		if id, err := s.resolveIssueID(ctx, s.db.R, k); err == nil {
			if id == i.ID {
				return true
			}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return false
	}
	if i.ParentID != nil && s.underAny(ctx, *i.ParentID, ids) {
		return true
	}
	var related bool
	args := append(append([]any{i.ID}, int64Args(ids)...), append([]any{i.ID}, int64Args(ids)...)...)
	// #nosec G202 -- only ? placeholders are joined in
	err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issue_relations WHERE named_in IS NULL AND ((source_id = ? AND target_id IN (`+placeholders(len(ids))+`))
		OR (target_id = ? AND source_id IN (`+placeholders(len(ids))+`))))`, args...).Scan(&related)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read bound covers", "issue", i.ID, "err", err)
		}
		return false
	}
	return related
}

// underAny: the issue parentID, or one of its ancestors, is among ids.
func (s *Service) underAny(ctx context.Context, parentID int64, ids []int64) bool {
	var under bool
	args := append([]any{parentID}, int64Args(ids)...)
	// #nosec G202 -- only ? placeholders are joined in
	err := s.db.R.QueryRowContext(ctx, `WITH RECURSIVE up(id, depth) AS (SELECT ?, 0
			UNION ALL SELECT x.parent_id, up.depth + 1 FROM issues x JOIN up ON x.id = up.id WHERE x.parent_id IS NOT NULL AND up.depth < 50)
		SELECT EXISTS(SELECT 1 FROM up WHERE id IN (`+placeholders(len(ids))+`))`, args...).Scan(&under)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read under any", "parentID", parentID, "err", err)
		}
		return false
	}
	return under
}
