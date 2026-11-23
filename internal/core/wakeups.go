package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// Batched wake-ups (WP-209, WP-ADR6). A wake-up makes the agent read its
// whole context again, so one wake-up should carry as much news as it can.
// News the agent is blocked on (wp ship --blocking, wp request ask
// --blocking, a tell_agent step) goes at once, or waits only while the
// session's other babysits may still say something, and at most
// batchBlockingWait. Everything else that wakes it (CI gave up, a person
// is needed, an answered request, --notify news) can wait: until just
// before the session's prompt cache cools, or, when the cache is already
// cold, up to batchColdWait, and it goes earlier with any wake-up that
// happens anyway. Blocking news never waits past a warm cache either. A
// hook the agent runs anyway takes batched news at once. (Quiet news never
// wakes the agent at all.)
const (
	batchBlockingWait = 5 * time.Minute
	batchColdWait     = 24 * time.Hour
	// cacheWarm is how long after its last activity a session's prompt cache
	// is counted warm (Claude Code asks for Anthropic's 1-hour cache; the API
	// default is 5 minutes, and Claude Code falls back to it in usage
	// overage), and cacheMargin how long before that a batch goes.
	cacheWarm   = time.Hour
	cacheMargin = 45 * time.Second
)

// babysitMoving are the states in which a babysit is likely to say
// something soon; needs_person waits on a person, however long that takes.
var babysitMoving = []string{BabysitWatching, BabysitWorking, BabysitWaitingReview, BabysitReady}

// batchOrTell decides when news that wakes a session goes: hold puts it in
// the session's batch until a time (ms). Blocking news is held only while
// the session's babysits other than babysitID are still moving; other news
// is held unless it is already due. News that goes takes the whole batch
// with it.
func (s *Service) batchOrTell(ctx context.Context, sessionID, babysitID int64, blocking bool, hold func(until int64) error) {
	moving := len(s.movingSiblings(ctx, sessionID, babysitID)) > 0
	if !blocking || moving {
		if until := s.batchDeadline(ctx, sessionID, blocking); until.After(s.now()) {
			// Blocking news held only for this babysit goes now, with this.
			if err := hold(until.UnixMilli()); err == nil {
				if moving {
					return
				}
				blocking, herr := s.hasBlockingBatch(ctx, sessionID)
				if herr != nil {
					// Unknown is not "nothing is blocked on it": it stays held, to its deadline.
					if readFault(herr) {
						s.log.WarnContext(ctx, "read whether a session's batch is blocking", "session", sessionID, "err", herr)
					}
					return
				}
				if !blocking {
					return
				}
			}
		}
	}
	s.releaseBatch(ctx, sessionID)
}

// hasBlockingBatch: news the agent is blocked on waits in a batch for the
// session. A failed read is an error, not "none".
func (s *Service) hasBlockingBatch(ctx context.Context, sessionID int64) (bool, error) {
	var held bool
	err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM session_messages WHERE session_id = ? AND state = 'queued' AND batch_until IS NOT NULL AND blocking = 1)
		OR EXISTS (SELECT 1 FROM requests WHERE session_id = ? AND delivered_at IS NULL AND batch_until IS NOT NULL AND blocking = 1)`, sessionID, sessionID).Scan(&held)
	return held, err
}

// babysitNotifiedNews: news the agent asked for with --notify (merged,
// closed, stopped, ready), which has nothing for it to act on.
func babysitNotifiedNews(key string) bool {
	k, _, _ := strings.Cut(key, "@")
	switch k {
	case "merged", "closed", "stopped", "ready":
		return true
	}
	return false
}

// heldToWait: m is --notify news waiting in a batch, which the agent isn't
// blocked on and has nothing to act on (or a cross-filed task's end, WP-378),
// so it doesn't continue a stop alone.
func heldToWait(ctx context.Context, t *tx, m *SessionMessage) bool {
	parts := strings.SplitN(m.stepKey, ":", 3)
	if len(parts) != 3 || (parts[0] != "crossrepo" && (parts[0] != "babysit" || !babysitNotifiedNews(parts[2]))) {
		return false
	}
	var held bool
	_ = t.QueryRowContext(ctx, `SELECT batch_until IS NOT NULL AND blocking = 0 FROM session_messages WHERE id = ?`, m.ID).Scan(&held)
	return held
}

// movingSiblings are the session's babysits but one still moving that have
// no news waiting in a batch.
func (s *Service) movingSiblings(ctx context.Context, sessionID, except int64) []int64 {
	rows, err := s.db.R.QueryContext(ctx, `SELECT b.id FROM babysits b WHERE b.session_id = ? AND b.id != ? AND b.ended_at IS NULL AND b.state IN (?,?,?,?)
		AND NOT EXISTS (SELECT 1 FROM session_messages m WHERE m.session_id = b.session_id AND m.state = 'queued' AND m.batch_until IS NOT NULL
			AND m.step_key LIKE 'babysit:' || b.id || ':%')`,
		sessionID, except, babysitMoving[0], babysitMoving[1], babysitMoving[2], babysitMoving[3])
	if err != nil {
		// Can't tell, so assume one is moving: its news waits for the deadline.
		return []int64{except}
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// batchDeadline is when news batched for a session goes regardless.
func (s *Service) batchDeadline(ctx context.Context, sessionID int64, blocking bool) time.Time {
	var seen int64
	_ = s.db.R.QueryRowContext(ctx, `SELECT last_seen_at FROM agent_sessions WHERE id = ?`, sessionID).Scan(&seen)
	now := s.now()
	warmUntil := msTime(seen).Add(cacheWarm - cacheMargin)
	warm := now.Before(warmUntil)
	switch {
	case blocking && warm && warmUntil.Before(now.Add(batchBlockingWait)):
		return warmUntil
	case blocking:
		return now.Add(batchBlockingWait)
	case warm:
		return warmUntil
	}
	return now.Add(batchColdWait)
}

// releaseBatch tells a session its batched news and whatever else waits to
// wake it: to a Stop hook waiting for it, or posted to an idle agent's inbox
// socket in one message.
func (s *Service) releaseBatch(ctx context.Context, sessionID int64) {
	s.releaseBatchWaiting(ctx, sessionID, false)
}

// releaseBatchWaiting is releaseBatch; waiting posts as postToSession does, so
// a session stopped on background work is told too.
func (s *Service) releaseBatchWaiting(ctx context.Context, sessionID int64, waiting bool) {
	for _, q := range []string{
		`UPDATE session_messages SET batch_until = NULL WHERE session_id = ? AND state = 'queued' AND batch_until IS NOT NULL`,
		`UPDATE requests SET batch_until = NULL WHERE session_id = ? AND batch_until IS NOT NULL`,
	} {
		if _, err := s.db.W.ExecContext(ctx, q, sessionID); err != nil {
			// Keep going: the other table's news must not stay batched.
			s.log.WarnContext(ctx, "release batched news", "session", sessionID, "err", err)
		}
	}
	s.stopWaits.wake(sessionID)
	s.postToSession(ctx, sessionID, waiting)
}

// releaseDueBatches tells sessions their batched news once it is due, or
// once blocking news has no babysit left to wait for (from CheckBabysits).
func (s *Service) releaseDueBatches(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT session_id, min(until) FROM (
		SELECT session_id, batch_until AS until FROM session_messages WHERE state = 'queued' AND batch_until IS NOT NULL
		UNION ALL SELECT session_id, batch_until FROM requests WHERE delivered_at IS NULL AND batch_until IS NOT NULL AND session_id IS NOT NULL) GROUP BY session_id`)
	if err != nil {
		return
	}
	type due struct{ session, until int64 }
	var list []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.session, &d.until) == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	for _, d := range list {
		release := d.until <= s.nowMS()
		if !release && len(s.movingSiblings(ctx, d.session, 0)) == 0 {
			blocking, err := s.hasBlockingBatch(ctx, d.session)
			if err != nil {
				if readFault(err) {
					s.log.WarnContext(ctx, "read whether a session's batch is blocking", "session", d.session, "err", err) // held to its deadline
				}
			}
			release = err == nil && blocking
		}
		if release {
			s.releaseBatch(ctx, d.session)
		}
	}
}

// takeBatched hands an agent asking after its babysits (wp pr babysit
// --status) the news batched for its session.
func (s *Service) takeBatched(ctx context.Context, sessionID int64) string {
	var list []*SessionMessage
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		rows, err := t.QueryContext(ctx, `SELECT `+messageCols+` FROM session_messages m LEFT JOIN users u ON u.id = m.created_by
			WHERE m.session_id = ? AND m.state = 'queued' AND m.batch_until IS NOT NULL ORDER BY m.id`, sessionID)
		if err != nil {
			return err
		}
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				rows.Close()
				return err
			}
			list = append(list, m)
		}
		rows.Close()
		for _, m := range list {
			if _, err := t.ExecContext(ctx, `UPDATE session_messages SET state = 'delivered', via = 'pull', delivered_at = ?, batch_until = NULL WHERE id = ?`, s.nowMS(), m.ID); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		s.log.WarnContext(ctx, "hand over batched news", "session", sessionID, "err", err)
		return ""
	}
	return MessagesText(list)
}

// sessionMessageSource is the runner job that posted one queued session
// message to an idle agent's inbox socket (before WP-209), and
// sessionBatchSource the one that posts a batch (message_batches): queued
// messages and answered requests together.
const (
	sessionMessageSource = "session_message"
	sessionBatchSource   = "session_batch"
)

// postToIdleSession hands what waits to wake a session (queued messages,
// answered requests that wake it) to it straight away when it sits idle and
// its person's runner on that machine can post to its inbox socket: the
// agent starts one turn with them all. Messages count as being sent and
// answers as delivered while the job runs; if the job fails they wait again,
// for the hooks.
func (s *Service) postToIdleSession(ctx context.Context, sessionID int64) {
	s.postToSession(ctx, sessionID, false)
}

// postToSession is postToIdleSession; waiting also posts to a session that
// stopped while its background work runs (stored as working, since that
// work's report starts its next turn), as news about that work must (WP-605).
func (s *Service) postToSession(ctx context.Context, sessionID int64, waiting bool) {
	// Say what held back a post when news is waiting for it (not for every
	// call: most find nothing to post).
	skip := func(why string) {
		var n int
		if s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM session_messages WHERE session_id = ? AND state = 'queued' AND passive = 0 AND batch_until IS NULL`, sessionID).Scan(&n) == nil && n > 0 {
			s.log.InfoContext(ctx, "session message not posted", "session", sessionID, "queued", n, "why", why)
		}
	}
	var cli, machine, state, agent string
	var user int64
	var stopped sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT cli_id, machine, state, user_id, agent, stopped_at FROM agent_sessions WHERE id = ?`, sessionID).Scan(&cli, &machine, &state, &user, &agent, &stopped); err != nil {
		return
	}
	if waiting && state == SessionWorking && stopped.Valid {
		state = SessionIdle
	}
	if state != SessionIdle || cli == "" || !cliSessionRE.MatchString(cli) {
		skip("the session is " + state + " (a working agent reads it at its next stop; an ended one at its next start)")
		return
	}
	if p, err := s.sessionPause(ctx, user, agent); err != nil || p != nil {
		skip("the agent is paused")
		return // paused (WP-146): it waits for the person's next prompt, or for Resume
	}
	if sessionLeftAlone(ctx, s.db.R, sessionID) {
		skip("its person stopped Waypoint in the session")
		return // its person typed /wp stop there (WP-562): it waits for their next prompt
	}
	if !runnerOnline(ctx, s, user, machine) {
		skip("no runner is online on " + machine)
		return
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+messageCols+` FROM session_messages m LEFT JOIN users u ON u.id = m.created_by
		WHERE m.session_id = ? AND m.state = 'queued' AND m.passive = 0 AND m.batch_until IS NULL ORDER BY m.id`, sessionID)
	if err != nil {
		return
	}
	var msgs []*SessionMessage
	for rows.Next() {
		if m, err := scanMessage(rows); err == nil {
			msgs = append(msgs, m)
		}
	}
	rows.Close()
	msgs = slices.DeleteFunc(msgs, func(m *SessionMessage) bool { return s.shipWaits.holds(ctx, s.db.R, s.now(), sessionID, m) }) // a wp ship waits for it (WP-1669)
	reqs, err := s.loadRequests(ctx, SystemActor(), true, `r.session_id = ? AND r.user_id = ? AND r.delivered_at IS NULL AND r.batch_until IS NULL AND r.state IN ('`+strings.Join(decidedStates, "','")+`') ORDER BY r.id`, sessionID, user)
	if err != nil {
		return
	}
	reqs = slices.DeleteFunc(reqs, func(r *Request) bool { return r.Via != "" || !r.wakes() })
	if len(msgs) == 0 && len(reqs) == 0 {
		return
	}
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO message_batches(session_id, created_at) VALUES (?,?)`, sessionID, s.nowMS())
	if err != nil {
		return
	}
	batch, err := res.LastInsertId()
	if err != nil {
		return
	}
	var sentMsgs []*SessionMessage
	for _, m := range msgs {
		res, err := s.db.W.ExecContext(ctx, `UPDATE session_messages SET state = 'sending', sent_with = ? WHERE id = ? AND state = 'queued'`, batch, m.ID)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 { // else a hook took it meanwhile
			sentMsgs = append(sentMsgs, m)
		}
	}
	var sentReqs []*Request
	for _, rq := range reqs {
		res, err := s.db.W.ExecContext(ctx, `UPDATE requests SET delivered_at = ?, sent_with = ? WHERE id = ? AND delivered_at IS NULL`, s.nowMS(), batch, rq.ID)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			sentReqs = append(sentReqs, rq)
		}
	}
	if len(sentMsgs) == 0 && len(sentReqs) == 0 {
		return
	}
	s.log.InfoContext(ctx, "session messages posted", "session", sessionID, "batch", batch, "messages", len(sentMsgs), "requests", len(sentReqs))
	raw, _ := json.Marshal(MessageJob{CLIID: cli, Body: joinText(DecisionsText(sentReqs), MessagesText(sentMsgs))})
	previousPostedAt, postedAt, marked := markPosted(ctx, s, sessionID)
	if _, err := enqueueRunnerJob(ctx, s, &Actor{User: &User{ID: user}}, RunnerJobInput{Kind: JobMessage, OwnerID: user, Label: machine, Input: raw,
		Source: sessionBatchSource, SourceID: batch}); err != nil {
		s.log.WarnContext(ctx, "post to an idle session", "session", sessionID, "err", err)
		s.unsendBatch(ctx, batch)
		if marked {
			restorePosted(ctx, s, sessionID, previousPostedAt, postedAt)
		}
	}
}

// unsendBatch puts what a batch's job didn't post back to wait for the
// agent's hooks.
func (s *Service) unsendBatch(ctx context.Context, batch int64) {
	_, _ = s.db.W.ExecContext(ctx, `UPDATE session_messages SET state = 'queued' WHERE sent_with = ? AND state = 'sending'`, batch)
	_, _ = s.db.W.ExecContext(ctx, `UPDATE requests SET delivered_at = NULL WHERE sent_with = ?`, batch)
}

// sessionMessageJobFinished records how posting to an idle session went:
// delivered, or waiting again for the agent's next stop or start.
func (s *Service) sessionMessageJobFinished(ctx context.Context, e *Event) {
	src, _ := e.Data["source"].(string)
	id := dataInt(e, "source_id")
	done := e.Data["state"] == "done"
	switch {
	case src == sessionMessageSource && done:
		_, _ = s.db.W.ExecContext(ctx, `UPDATE session_messages SET state = 'delivered', via = 'socket', delivered_at = ? WHERE id = ? AND state = 'sending'`, s.nowMS(), id)
	case src == sessionMessageSource:
		_, _ = s.db.W.ExecContext(ctx, `UPDATE session_messages SET state = 'queued' WHERE id = ? AND state = 'sending'`, id)
	case src == sessionBatchSource && done:
		var sessionID, posted int64
		_ = s.db.R.QueryRowContext(ctx, `SELECT session_id FROM message_batches WHERE id = ?`, id).Scan(&sessionID)
		// It reached the agent when the runner started the job (a second or so
		// before it reported): the agent may already have done that turn and
		// finished, and that finish counts for a tell_agent step waiting on it.
		_ = s.db.R.QueryRowContext(ctx, `SELECT coalesce(started_at, created_at) FROM runner_jobs WHERE id = ?`, dataInt(e, "job")).Scan(&posted)
		if posted == 0 {
			posted = s.nowMS()
		}
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			x, err := s.sessionByID(ctx, t, SystemActor(), sessionID)
			if err != nil {
				return err
			}
			rows, err := t.QueryContext(ctx, `SELECT id FROM session_messages WHERE sent_with = ? AND state = 'sending'`, id)
			if err != nil {
				return err
			}
			var ids []int64
			for rows.Next() {
				var m int64
				if rows.Scan(&m) == nil {
					ids = append(ids, m)
				}
			}
			rows.Close()
			for _, m := range ids {
				if _, err := t.ExecContext(ctx, `UPDATE session_messages SET state = 'delivered', via = 'socket', delivered_at = ? WHERE id = ?`, posted, m); err != nil {
					return err
				}
				t.emit(sessionEvent(EvSessionMessage, x, map[string]any{"message": m, "via": "socket"}))
			}
			_, err = t.ExecContext(ctx, `UPDATE agent_sessions SET state = 'working', wait_reason = '', pending = '', finished_at = NULL
				WHERE id = ? AND (finished_at IS NULL OR finished_at < ?)`, x.ID, posted)
			return err
		})
		if err != nil {
			s.log.WarnContext(ctx, "record a posted batch", "batch", id, "err", err)
		}
	case src == sessionBatchSource:
		s.unsendBatch(ctx, id)
	}
}

// repostNewsAfter is how long news about a round waits in a session's queue
// before the sweep posts it again: the post that was tried when it was
// queued found the session busy, paused, left alone, or without a runner,
// and nothing else comes back for it while the session stays idle.
const repostNewsAfter = 2 * time.Minute

// repostQueuedNews posts the audit and check news still queued to the idle
// sessions that can take it now (WP-1370): the news a round waits on must
// not depend on the one try made when it was queued.
func (s *Service) repostQueuedNews(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT m.session_id FROM session_messages m JOIN agent_sessions x ON x.id = m.session_id
		WHERE m.state = 'queued' AND m.passive = 0 AND m.batch_until IS NULL AND x.ended_at IS NULL AND m.created_at < ?
		AND (m.step_key LIKE 'check:%' OR m.step_key LIKE 'audit:%') LIMIT 50`, s.nowMS()-repostNewsAfter.Milliseconds())
	if err != nil {
		s.log.WarnContext(ctx, "queued news to post again", "err", err)
		return
	}
	ids, err := scanIDs(rows)
	if err != nil {
		s.log.WarnContext(ctx, "queued news to post again: read them", "err", err)
		return
	}
	for _, id := range ids {
		s.postToSession(ctx, id, true)
	}
}
