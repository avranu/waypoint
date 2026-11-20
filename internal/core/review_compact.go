package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Compacting a session for its review as soon as the review is triggered
// (WP-761). The round's job compacts the session right before typing the
// prompt, but when another model audits first that is after the audit: the wait is
// spent at full context and the cache cools under it. So the first look that
// finds the round waiting for its audit compacts the session then, and the
// round's job skips it.
//
// It's a light inbox job, like the cache warmer's compaction, so it doesn't
// wait for a build to finish. Nothing depends on it: if it is held or fails,
// the round's job compacts as it always has.

// reviewCompactSource is the source of that job; its source id is the round.
const reviewCompactSource = "review_compact"

// reviewCompactWait is how long a typed compaction may go without its
// SessionStart before a review stops waiting for it.
const reviewCompactWait = 10 * time.Minute

// compactForReview queues the round's compaction, once (again only after
// one was called off: its person used the session, WP-816). The session's warm
// state keeps the compaction quiet and the cache pings off until the
// review's prompt starts a turn. One Waypoint is keeping warm or has just
// compacted is left alone.
func (s *Service) compactForReview(ctx context.Context, st *stepRow, x *AgentSession, user *User, review *roundRow) {
	if x.CLI != "claude" {
		return
	}
	var jobs int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_jobs WHERE source = ? AND source_id = ? AND state != 'cancelled'`, reviewCompactSource, review.id).Scan(&jobs); err != nil || jobs > 0 {
		return
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE agent_sessions SET warm_state = ? WHERE id = ? AND warm_state NOT IN (?, ?, ?, ?)`, warmReview, x.ID, warmInPing, warmCompacting, warmCompacted, warmReview)
	if err != nil {
		s.log.WarnContext(ctx, "review compaction: mark the session", "session", x.ID, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	raw, _ := json.Marshal(MessageJob{CLIID: x.CLIID, Compact: true})
	if _, err := enqueueRunnerJob(ctx, s, &Actor{User: user}, RunnerJobInput{Kind: JobMessage, OwnerID: st.userID, Label: x.Machine, Input: raw,
		Source: reviewCompactSource, SourceID: review.id}); err != nil {
		s.unwarmReview(ctx, x.ID) // the round's job compacts it
		s.log.Info("review compaction: queue it", "session", x.ID, "err", err)
	}
}

// compactedForReview: the round's compaction ran and the session has done
// nothing since. Its SessionStart stamped warm_at; any turn since cleared the
// warm state.
func (s *Service) compactedForReview(ctx context.Context, sessionID, reviewID int64) bool {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_jobs j JOIN agent_sessions a ON a.id = ?
		WHERE j.source = ? AND j.source_id = ? AND j.state = 'done' AND a.warm_state = ? AND a.warm_at >= j.created_at`,
		sessionID, reviewCompactSource, reviewID, warmReview).Scan(&n)
	return err == nil && n > 0
}

// compactingForReview: the round's compaction was typed and the session hasn't
// said it finished. Its job is done within seconds, the compaction takes
// minutes, and a review started in between finds the screen busy and takes the
// warm state that the compaction's SessionStart needs. A compaction that never
// reports stops counting after reviewCompactWait, and the round's job compacts.
func (s *Service) compactingForReview(ctx context.Context, sessionID, reviewID, now int64) bool {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_jobs j JOIN agent_sessions a ON a.id = ?
		WHERE j.source = ? AND j.source_id = ? AND j.state = 'done' AND a.warm_state = ? AND coalesce(a.warm_at, 0) < j.created_at AND j.finished_at >= ?`,
		sessionID, reviewCompactSource, reviewID, warmReview, now-reviewCompactWait.Milliseconds()).Scan(&n)
	return err == nil && n > 0
}

// dropReviewCompact cancels the round's compaction if no runner took it yet:
// the round has moved on without it. The session's warm state is the caller's
// to settle, whether or not there was a job to cancel: the round's job needs
// it kept, and a review that won't run releases it with unwarmReview.
func (s *Service) dropReviewCompact(ctx context.Context, user *User, reviewID int64) {
	var id int64
	err := s.db.R.QueryRowContext(ctx, `SELECT id FROM runner_jobs WHERE source = ? AND source_id = ? AND state = 'queued'`, reviewCompactSource, reviewID).Scan(&id)
	if err != nil {
		return
	}
	if _, err := s.cancelRunnerJob(ctx, &Actor{User: user}, id, "the adversarial review has moved on"); err != nil {
		s.log.Info("review compaction: cancel it", "job", id, "err", err)
	}
}

// stopReviewCompact: the step stopped while its round waited, so nothing
// will type the review. The compaction doesn't go on into a session that's
// no longer autopilot's, and the cache is the cache warmer's again.
func (s *Service) stopReviewCompact(ctx context.Context, t *tx, sessionID, reviewID int64) error {
	if reviewID == 0 {
		return nil
	}
	var job int64
	err := t.QueryRowContext(ctx, `SELECT id FROM runner_jobs WHERE source = ? AND source_id = ? AND state = 'queued'`, reviewCompactSource, reviewID).Scan(&job)
	if err == nil {
		if err := t.finishRunnerJob(ctx, job, "cancelled", nil, nil, "the session's autopilot step stopped"); err != nil {
			return err
		}
	} else if err != sql.ErrNoRows {
		return err
	}
	_, err = t.ExecContext(ctx, `UPDATE agent_sessions SET warm_state = '' WHERE id = ? AND warm_state = ?`, sessionID, warmReview)
	return err
}

// reviewCompactFinished: a compaction that didn't happen leaves the session
// to the cache warmer until the round's job needs it, so its cache doesn't
// go cold unwatched while Grok audits. A cancelled one was already dealt with.
func (s *Service) reviewCompactFinished(ctx context.Context, e *Event) {
	if src, _ := e.Data["source"].(string); src != reviewCompactSource || e.Data["state"] == "cancelled" {
		return
	}
	review := dataInt(e, "source_id")
	if e.Data["state"] == "done" {
		var result sql.NullString
		_ = s.db.R.QueryRowContext(ctx, `SELECT result FROM runner_jobs WHERE id = ?`, dataInt(e, "job")).Scan(&result)
		var r struct {
			Delivered *bool `json:"delivered"`
		}
		_ = json.Unmarshal([]byte(result.String), &r)
		if r.Delivered == nil || *r.Delivered {
			return
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE agent_sessions SET warm_state = '' WHERE warm_state = ?
		AND id = (SELECT session_id FROM work_log WHERE id = ?)
		AND NOT EXISTS (SELECT 1 FROM autopilot_steps WHERE review_id = ? AND state = ?)`, warmReview, review, review, StepStarting); err != nil {
		s.log.WarnContext(ctx, "review compaction: release the round's session", "review", review, "err", err)
	}
}
