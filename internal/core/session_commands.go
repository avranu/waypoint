package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Session commands (WP-562, WP-ADR108): `/wp SUB` typed in a Claude Code session goes
// no further than its UserPromptSubmit hook. No model call runs; the person
// sees one line instead of a turn. The server answers the ones about what
// Waypoint does with the session (next, stop, continue, status, keep); the
// CLI answers help, btw and exe itself, on its machine.

// SessionCommandPrefix starts a session command.
const SessionCommandPrefix = "/wp"

// sessionCommands maps what may be typed after /wp to the command it is.
var sessionCommands = map[string]string{
	"next": "next",
	"stop": "stop", "pause": "stop", "done": "stop",
	"continue": "continue", "resume": "continue",
	"status": "status",
	"keep":   "keep",
	"review": "review", "audit": "review",
}

// SessionCommand is the server's command a prompt is ("next", "stop",
// "continue", "status", "keep", "review"), or "" for any other prompt. The
// CLI sends only the command (/wp next), never anything typed after it but
// /wp review's task key.
func SessionCommand(prompt string) string {
	p := strings.TrimSpace(prompt)
	if strings.EqualFold(p, "/"+AutopilotKeepWord) {
		return "keep"
	}
	rest, ok := strings.CutPrefix(p, SessionCommandPrefix+" ")
	if !ok {
		return ""
	}
	sub, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
	return sessionCommands[strings.ToLower(sub)]
}

// Lines the person sees instead of a turn.
const (
	SessionStoppedLine   = "Waypoint: stopped in this session. No next task, review, keep-warm ping, compaction or posted news comes here until you type /wp continue (or /wp next)."
	SessionContinuedLine = "Waypoint: on again in this session."
)

// sessionCommand answers a session command typed in x: the line the person
// sees ("": the prompt is no command, and goes on as any other). A step to
// move on now is left in ev.advance, for after the write.
func (s *Service) sessionCommand(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent) (string, error) {
	p := strings.TrimSpace(ev.Prompt)
	if strings.EqualFold(p, AutopilotKeepWord) {
		return s.autopilotKeep(ctx, t, x, false)
	}
	cmd := SessionCommand(p)
	if cmd == "" {
		return "", nil
	}
	if x.Kind == SessionTask && cmd != "status" && cmd != "review" {
		return "Waypoint: this is a task session; it hands off by itself when its task is done, so /wp " + cmd + " does nothing here.", nil
	}
	at := s.hookTime(ev)
	switch cmd {
	case "keep":
		return s.autopilotKeep(ctx, t, x, true)
	case "next":
		return s.sessionNext(ctx, t, x, ev, at)
	case "stop":
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET left_alone_at = coalesce(left_alone_at, ?) WHERE id = ?`, at, x.ID); err != nil {
			return "", err
		}
		if err := s.stopStep(ctx, t, x.ID, "left", "you stopped Waypoint in the session"); err != nil {
			return "", err
		}
		return SessionStoppedLine, s.sessionTimeline(ctx, t, x.ID, at, "autopilot", "", 0, "left", "", "You stopped Waypoint in this session (/wp stop)", x.projectID, nil)
	case "continue":
		res, err := t.ExecContext(ctx, `UPDATE agent_sessions SET left_alone_at = NULL WHERE id = ? AND left_alone_at IS NOT NULL`, x.ID)
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return "Waypoint: it wasn't stopped in this session. " + s.sessionStatus(ctx, t, x, at), nil
		}
		line := SessionContinuedLine + " Autopilot decides at the session's next stop; /wp next starts the next task now."
		return line, s.sessionTimeline(ctx, t, x.ID, at, "autopilot", "", 0, "continued", "", "You turned Waypoint back on in this session (/wp continue)", x.projectID, nil)
	case "status":
		return "Waypoint: " + s.sessionStatus(ctx, t, x, at), nil
	case "review":
		// Read after the write, from the database alone (review_progress.go).
		ev.review = &reviewAsk{session: x, key: reviewKeyArg(p)}
		return "Waypoint: reading the review…", nil
	}
	return "", nil
}

// reviewKeyArg is the task key typed after /wp review ("": none).
func reviewKeyArg(prompt string) string {
	f := strings.Fields(prompt)
	if len(f) < 3 {
		return ""
	}
	return SessionReviewKey(f[2])
}

// SessionReviewKey is the task key that what was typed after /wp review
// starts with ("": it names none). Only a key goes to the server.
func SessionReviewKey(rest string) string {
	f := strings.Fields(rest)
	if len(f) > 0 && reviewKeyRE.MatchString(f[0]) {
		return strings.ToUpper(f[0])
	}
	return ""
}

// sessionLeftAlone: the person typed /wp stop in the session, and not /wp
// continue since.
func sessionLeftAlone(ctx context.Context, q queryer, sessionID int64) bool {
	var left sql.NullInt64
	_ = q.QueryRowContext(ctx, `SELECT left_alone_at FROM agent_sessions WHERE id = ?`, sessionID).Scan(&left)
	return left.Valid
}

// sessionNext moves the session on to its next task now: a step that waits
// (on the person's idle hold, the grace, a merge) is due at once, as the
// web's Start now; with no step, one is made, as the web's Start. The
// limits, an adversarial review still to come and the runner's look at the
// screen still apply. A session that still holds a task isn't moved on: its
// work would be left behind.
func (s *Service) sessionNext(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent, at int64) (string, error) {
	if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET left_alone_at = NULL WHERE id = ?`, x.ID); err != nil {
		return "", err
	}
	st, err := scanStep(t.QueryRowContext(ctx, `SELECT `+stepCols+` FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`, x.ID, StepDue, StepWaiting, StepStarting))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		st = nil
	case err != nil:
		return "", err
	}
	if st != nil {
		switch {
		case st.state == StepStarting:
			return "Autopilot: it is already moving this session on.", nil
		case st.reason == WaitReviewing || st.reason == WaitAudit:
			return "Autopilot: " + cmp(st.detail, "its adversarial review is under way") + "; the next task comes after it.", nil
		}
		// Asked for: it moves on even a person's own session (WP-1319).
		if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET due_at = ?, created_at = min(created_at, ?), reason = CASE WHEN reason = ? THEN '' ELSE reason END, updated_at = ?, asked = 1
			WHERE id = ? AND state IN (?, ?)`, at, at-autopilotMergeWaitMax.Milliseconds(), WaitPrompted, s.nowMS(), st.id, StepDue, StepWaiting); err != nil {
			return "", err
		}
		st.asked = true
		ev.advance = st.withNow(s)
		line := "Autopilot: the next task starts now."
		if r, err := roundByID(ctx, t, st.reviewID.Int64); err == nil && r != nil && r.state == RoundPending {
			line = "Autopilot: " + r.key + "'s adversarial review starts now; the next task follows it."
		}
		return line, s.sessionTimeline(ctx, t, x.ID, at, "autopilot", "", 0, "next", "", "You moved the session on now (/wp next)", x.projectID, nil)
	}
	var held string
	if err := t.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM agent_runs r JOIN issues i ON i.id = r.issue_id JOIN projects p ON p.id = i.project_id
		WHERE r.session_id = ? AND r.ended_at IS NULL ORDER BY r.started_at DESC, r.id DESC LIMIT 1`, x.ID).Scan(&held); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if held != "" {
		return fmt.Sprintf("Autopilot: not moved on. This session still holds %s, and its work would be left behind; once it ships (or `wp release %s`), autopilot moves on.", held, held), nil
	}
	pid, err := s.sessionProject(ctx, t, x.ID)
	if err != nil {
		return "", err
	}
	if pid == 0 {
		return "Autopilot: not moved on. This session's repo isn't attached to a project, and it has taken no task.", nil
	}
	on, _, err := s.autopilotFor(ctx, t, x.userID, pid)
	if err != nil {
		return "", err
	}
	if !on {
		return fmt.Sprintf("Autopilot: not moved on. Autopilot is off in %s; turn it on there (Agents → Autopilot), then /wp next.", s.projectKeyOf(ctx, pid)), nil
	}
	_, run, err := lastDoneInSession(ctx, t, x.ID)
	if err != nil {
		return "", err
	}
	if err := s.putStep(ctx, t, x, pid, run, at, StepDue, "", "you typed /wp next", at, 0); err != nil {
		return "", err
	}
	// Moved on by hand: no grace, no merge wait, as Start, and asked for, so
	// it moves on a person's own session (WP-1319).
	if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET created_at = ?, attempts = 0, asked = 1 WHERE session_id = ? AND state = ?`, at-autopilotMergeWaitMax.Milliseconds(), x.ID, StepDue); err != nil {
		return "", err
	}
	if st, err = scanStep(t.QueryRowContext(ctx, `SELECT `+stepCols+` FROM autopilot_steps WHERE session_id = ? AND state = ?`, x.ID, StepDue)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "Autopilot: not moved on. It has no next task to start for this session right now.", nil
		}
		return "", err
	}
	st.byHand = true
	ev.advance = st
	return "Autopilot: the next task starts now.", s.sessionTimeline(ctx, t, x.ID, at, "autopilot", "", 0, "next", "", "You moved the session on now (/wp next)", x.projectID, nil)
}

// sessionProject is the project autopilot works in for the session (its
// repo's, else its task's), 0 if none.
func (s *Service) sessionProject(ctx context.Context, q queryer, sessionID int64) (int64, error) {
	var pid sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT `+sessionProjectSQL+` FROM agent_sessions s WHERE s.id = ?`, sessionID).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return pid.Int64, err
}

// sessionStatus says what Waypoint will do with the session next, in a
// line or two.
func (s *Service) sessionStatus(ctx context.Context, q queryer, x *AgentSession, at int64) string {
	if x.Kind == SessionTask {
		return "this is a task session; it hands off by itself when its task is done."
	}
	var left sql.NullInt64
	_ = q.QueryRowContext(ctx, `SELECT left_alone_at FROM agent_sessions WHERE id = ?`, x.ID).Scan(&left)
	if left.Valid {
		return fmt.Sprintf("stopped in this session %s ago (/wp stop): no next task, review, keep-warm ping, compaction or posted news. /wp continue turns it back on.",
			shortAgo(time.Duration(at-left.Int64)*time.Millisecond))
	}
	st, err := scanStep(q.QueryRowContext(ctx, `SELECT `+stepCols+` FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`, x.ID, StepDue, StepWaiting, StepStarting))
	if err == nil {
		what := "the next task"
		if r, err := roundByID(ctx, q, st.reviewID.Int64); err == nil && r != nil && r.state == RoundPending {
			what = r.key + "'s adversarial review"
		}
		why := ""
		if st.detail != "" {
			why = " (" + st.detail + ")"
		}
		switch {
		case st.state == StepStarting:
			return "autopilot is moving this session on now."
		case st.dueAt <= at:
			return "autopilot starts " + what + " within a minute" + why + "."
		}
		return fmt.Sprintf("autopilot starts %s in %s%s. /wp next starts it now; /keep or /wp stop keeps the session.", what, shortAgo(time.Duration(st.dueAt-at)*time.Millisecond), why)
	}
	var held string
	_ = q.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM agent_runs r JOIN issues i ON i.id = r.issue_id JOIN projects p ON p.id = i.project_id
		WHERE r.session_id = ? AND r.ended_at IS NULL ORDER BY r.started_at DESC, r.id DESC LIMIT 1`, x.ID).Scan(&held)
	pid, rerr := s.sessionProject(ctx, q, x.ID)
	if rerr != nil {
		if readFault(rerr) {
			s.log.WarnContext(ctx, "session project failed", "x", x.ID, "err", rerr)
		}
	}
	if pid == 0 {
		return "this session's repo isn't attached to a project, so autopilot leaves it alone."
	}
	on, _, err := s.autopilotFor(ctx, q, x.userID, pid)
	if err != nil {
		return "couldn't read autopilot's setting."
	}
	key := s.projectKeyOf(ctx, pid)
	switch {
	case !on:
		return "autopilot is off in " + key + ", so nothing moves this session on."
	case held != "":
		return "autopilot is on in " + key + "; this session holds " + held + ", and autopilot moves it on once that ships."
	}
	return "autopilot is on in " + key + " but isn't about to move this session on; it decides at the session's next stop, or /wp next starts the next task now."
}
