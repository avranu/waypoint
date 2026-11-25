package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"
)

// Deferred work after the ship (WP-1858): tasks a session files once its ship
// has run are offered by bundling the list with a message the session takes
// anyway, by a message to the idle session while its cache is warm, and
// before autopilot moves it to its next task. A session with a large context
// is compacted first, so it reads the list in a small one.

const (
	// deferredCallsAfter is how many calls a session is expected to make after
	// an offer: the median calls after a manual compaction, 22 (wp usage
	// compact, 14 days to 2026-11-25).
	deferredCallsAfter = 22
	// compactBreakEvenDrop is the drop in context a manual compaction needs
	// before a round of compactBreakEvenCalls calls to pay for itself: 78k
	// tokens at 30 calls (WP-978 / X16 measured it). The cost of a
	// compaction is about constant, so the drop needed scales with
	// compactBreakEvenCalls / calls.
	compactBreakEvenDrop  = 78_000
	compactBreakEvenCalls = 30
	// deferredCompactKeeps is the context a manual compaction leaves: the
	// median of wp usage compact over the 14 days to 2026-11-25.
	deferredCompactKeeps = 46_000
	// deferredCompactSource is the runner job that compacts a session for an
	// offer; its source id is the session.
	deferredCompactSource = "deferred_compact"
	// deferredCompactWait is how long an offer waits for its compaction, or
	// for the session to idle so it can start, before it is told by message
	// only.
	deferredCompactWait = 10 * time.Minute
	// What a compaction for an offer is at (deferredCompactState).
	deferredCompactDone   = "done"
	deferredCompactWaits  = "waiting"
	deferredCompactFailed = "failed"
)

// deferredCompactFrom is the context from which compacting a session before
// an offer pays: what a compaction keeps, plus the drop that breaks even over
// the calls expected after the offer, about 152k tokens. It is measured, not
// priced (compactFrom models keep-warm restarts, and its rate isn't this
// one), and doesn't depend on the model. deferred_offers records the context
// at the offer and whether Waypoint compacted (context_tokens, compacted), so
// it can be measured again, as WP-ADR110 asks of a compaction form.
func deferredCompactFrom() int64 {
	return deferredCompactKeeps + compactBreakEvenDrop*compactBreakEvenCalls/deferredCallsAfter
}

// deferredSess is the part of a session the offers read.
type deferredSess struct {
	id, user         int64
	cli, cliID       string
	machine, state   string
	agent, model     string
	tokens           int64
	typed            bool
	stopped, endedAt sql.NullInt64
}

func (s *Service) deferredSession(ctx context.Context, id int64) (*deferredSess, error) {
	x := &deferredSess{id: id}
	err := s.db.R.QueryRowContext(ctx, `SELECT user_id, cli, cli_id, machine, state, agent, model, context_tokens, prompt_typed, stopped_at, ended_at FROM agent_sessions WHERE id = ?`, id).
		Scan(&x.user, &x.cli, &x.cliID, &x.machine, &x.state, &x.agent, &x.model, &x.tokens, &x.typed, &x.stopped, &x.endedAt)
	return x, err
}

// deferredTypable says what postToIdleSession checks of a session: Waypoint
// can post to it now (idle too, when needIdle).
func (s *Service) deferredTypable(ctx context.Context, id int64, needIdle bool) bool {
	x, err := s.deferredSession(ctx, id)
	if err != nil || x.endedAt.Valid || needIdle && x.state != SessionIdle {
		return false
	}
	if x.cliID == "" || !cliSessionRE.MatchString(x.cliID) {
		return false
	}
	if p, err := s.sessionPause(ctx, x.user, x.agent); err != nil || p != nil {
		return false
	}
	return !sessionLeftAlone(ctx, s.db.R, id) && runnerOnline(ctx, s, x.user, x.machine)
}

// deferredFound is what a session has not been offered.
type deferredFound struct {
	cands []*Issue
	task  int64
	keys  []string
}

// deferredBusy: the session's person is typing in it (the gate keep-warm
// uses), so Waypoint doesn't push a turn into it; it tries again later.
func (s *Service) deferredBusy(ctx context.Context, x *deferredSess) bool {
	return x.typed && s.personActive(ctx, x.user)
}

// deferredShipTime is when the session last shipped: the latest babysit it
// started (wp ship starts one), none when it never has.
func (s *Service) deferredShipTime(ctx context.Context, id int64) (int64, bool, error) {
	var at sql.NullInt64
	err := s.db.R.QueryRowContext(ctx, `SELECT max(created_at) FROM babysits WHERE session_id = ?`, id).Scan(&at)
	return at.Int64, at.Valid, err
}

// deferredFind finds the tasks the session filed after its ship that it
// wasn't offered, in the projects where autopilot and the limit are on and
// its user can read. A session that has not shipped (it started no babysit)
// has nothing filed "after" it: what it filed before the ship was the ship
// gate's.
func (s *Service) deferredFind(ctx context.Context, x *deferredSess) (*deferredFound, error) {
	if x.endedAt.Valid || !deferredModel(x.model) {
		return nil, nil
	}
	since, shipped, err := s.deferredShipTime(ctx, x.id)
	if err != nil || !shipped {
		return nil, err
	}
	since = max(since, s.nowMS()-originRecent.Milliseconds())
	user, err := s.userByID(ctx, s.db.R, x.user)
	if err != nil {
		return nil, err
	}
	if user.Disabled {
		return nil, nil
	}
	actor := &Actor{User: user, Session: x.id}
	ids, err := s.sessionTasks(ctx, s.db.R, actor)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	tasks, err := s.issuesByIDs(ctx, ids, false)
	if err != nil || len(tasks) == 0 {
		return nil, err
	}
	keys := map[int64]string{}
	var taskIDs []int64
	for _, i := range tasks {
		keys[i.ID] = i.Key
		taskIDs = append(taskIDs, i.ID)
	}
	projects, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT i.project_id FROM issues i JOIN statuses st ON st.id = i.status_id
		WHERE i.session_id = ? AND st.category NOT IN ('done','canceled') AND i.created_at >= ?
		AND NOT EXISTS (SELECT 1 FROM deferred_offers d WHERE d.session_id = i.session_id AND d.issue_id = i.id)`, x.id, since)
	if err != nil || len(projects) == 0 {
		return nil, err
	}
	f := &deferredFound{task: taskIDs[0]}
	for _, id := range taskIDs[:min(len(taskIDs), 3)] {
		f.keys = append(f.keys, keys[id])
	}
	for _, p := range projects {
		if ok, err := s.deferredOn(ctx, actor, p); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		c, err := s.deferredCandidates(ctx, actor, p, taskIDs, keys, since)
		if err != nil {
			return nil, err
		}
		f.cands = append(f.cands, c...)
	}
	if len(f.cands) == 0 {
		return nil, nil
	}
	return f, nil
}

// deferredOn says whether offers are made to the actor in the project:
// autopilot and the limit are on, and the user can read the project.
func (s *Service) deferredOn(ctx context.Context, actor *Actor, project int64) (bool, error) {
	role, err := s.projectRole(ctx, s.db.R, actor, project)
	if err != nil || role == RoleNone {
		return false, err
	}
	on, lim, err := s.autopilotFor(ctx, s.db.R, actor.id(), project)
	if err != nil {
		return false, err
	}
	return on && valueOf(lim.Deferred), nil
}

// deferredSend queues the offer's text for the session, once per session and
// first task, and has it posted when the session idles and Waypoint can. The
// rows are told only when the text was queued.
func (s *Service) deferredSend(ctx context.Context, id int64, offered []*Issue, text string, push bool) error {
	if _, err := s.storeMessage(ctx, id, text, deferredStepKey(id, offered[0].ID), messageFrom{}, false); err != nil {
		s.log.WarnContext(ctx, "deferred offer: queue the text", "session", id, "err", err)
		return err
	}
	ids := make([]int64, len(offered))
	for n, i := range offered {
		ids[n] = i.ID
	}
	// #nosec G202 -- the list is placeholders only; values are bound
	if _, err := s.writeRows(ctx, `UPDATE deferred_offers SET told_at = ? WHERE session_id = ? AND told_at IS NULL AND issue_id IN (`+placeholders(len(ids))+`)`,
		append([]any{s.nowMS(), id}, int64Args(ids)...)...); err != nil {
		s.log.WarnContext(ctx, "deferred offer: mark it told", "session", id, "err", err)
	}
	if push {
		s.postToIdleSession(ctx, id)
	}
	return nil
}

func deferredStepKey(session, issue int64) string {
	return "deferred:" + strconv.FormatInt(session, 10) + ":" + strconv.FormatInt(issue, 10)
}

// deferredBundle is the offer a message the session takes now carries (the
// hook's start or stop): one more part of it, and rows recorded in the take's
// own write. It never compacts, and never makes a turn by itself: it is
// asked for only when a message is being delivered. Nil when there is
// nothing to offer. The message is made first, and no row stays without it.
func (s *Service) deferredBundle(ctx context.Context, t *tx, x *AgentSession) (*SessionMessage, error) {
	if !deferredModel(x.Model) {
		return nil, nil
	}
	d, err := s.deferredSession(ctx, x.ID)
	if err != nil {
		return nil, err
	}
	f, err := s.deferredFind(ctx, d)
	if err != nil || f == nil {
		return nil, err
	}
	m := &SessionMessage{sessionID: x.ID, Body: deferredText(f.keys, deferredOfferOf(f.cands), true), State: MessageDelivered, stepKey: deferredStepKey(x.ID, f.cands[0].ID)}
	now := s.nowMS()
	res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO session_messages(session_id, body, step_key, created_at, passive, state, via, delivered_at) VALUES (?,?,?,?,0,'delivered','bundle',?)`,
		x.ID, m.Body, m.stepKey, now, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	m.ID, _ = res.LastInsertId()
	m.CreatedAt, m.deliveredAt, m.Via = msTime(now), sql.NullInt64{Int64: now, Valid: true}, "bundle"
	offered, err := s.insertDeferred(ctx, t, x.ID, f.cands, f.task, DeferredViaMessage, d.tokens, now, false)
	if err == nil && len(offered) == 0 {
		err = errors.New("no task was left to record")
	}
	if err != nil {
		if _, e := t.ExecContext(ctx, `DELETE FROM session_messages WHERE id = ?`, m.ID); e != nil {
			return nil, errors.Join(err, e)
		}
		return nil, err
	}
	return m, nil
}

// deferredCompactState is where the session's compaction for an offer is:
// "" none was queued, waiting, done (the session's SessionStart after it
// came, or it was typed so long ago that none is coming) or failed.
func (s *Service) deferredCompactState(ctx context.Context, id int64) string {
	var job, created, finished int64
	var state string
	var result sql.NullString
	err := s.db.R.QueryRowContext(ctx, `SELECT id, state, created_at, coalesce(finished_at, 0), result FROM runner_jobs WHERE source = ? AND source_id = ? ORDER BY id DESC LIMIT 1`,
		deferredCompactSource, id).Scan(&job, &state, &created, &finished, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		s.log.WarnContext(ctx, "deferred offer: read its compaction", "session", id, "err", err)
		return deferredCompactFailed
	}
	failed := func() string {
		if _, err := s.writeRows(ctx, `UPDATE agent_sessions SET warm_state = '' WHERE id = ? AND warm_state = ?`, id, warmCompacting); err != nil {
			s.log.WarnContext(ctx, "deferred offer: release the session", "session", id, "err", err)
		}
		return deferredCompactFailed
	}
	now := s.nowMS()
	switch state {
	case "queued", "running":
		if now-created < deferredCompactWait.Milliseconds() {
			return deferredCompactWaits
		}
		if state == "queued" {
			if _, err := s.cancelRunnerJob(ctx, SystemActor(), job, "no runner took the compaction before an offer in time"); err != nil {
				s.log.InfoContext(ctx, "deferred offer: cancel its compaction", "job", job, "err", err)
			}
		}
		return failed()
	case "done":
		var r struct {
			Delivered *bool `json:"delivered"`
		}
		if err := json.Unmarshal([]byte(result.String), &r); err != nil {
			s.log.WarnContext(ctx, "deferred offer: read its compaction's result", "session", id, "job", job, "err", err)
			return failed()
		}
		if r.Delivered != nil && !*r.Delivered {
			return failed()
		}
		var warmAt sql.NullInt64
		_ = s.db.R.QueryRowContext(ctx, `SELECT warm_at FROM agent_sessions WHERE id = ?`, id).Scan(&warmAt)
		switch {
		case warmAt.Valid && warmAt.Int64 >= created:
			return deferredCompactDone
		case finished >= now-deferredCompactWait.Milliseconds():
			return deferredCompactWaits
		}
		return failed()
	}
	return failed()
}

// compactForDeferred queues the session's compaction for an offer, once, as
// compactForReview does for a review: the session's warm state keeps it quiet
// until its SessionStart says it is done. It says whether it queued one.
func (s *Service) compactForDeferred(ctx context.Context, x *deferredSess) bool {
	if x.cli != "claude" {
		return false
	}
	var jobs int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_jobs WHERE source = ? AND source_id = ? AND state != 'cancelled'`, deferredCompactSource, x.id).Scan(&jobs); err != nil || jobs > 0 {
		return false
	}
	n, err := s.writeRows(ctx, `UPDATE agent_sessions SET warm_state = ? WHERE id = ? AND warm_state NOT IN (?, ?, ?, ?)`, warmCompacting, x.id, warmInPing, warmCompacting, warmCompacted, warmReview)
	if err != nil || n == 0 {
		return false
	}
	raw, _ := json.Marshal(MessageJob{CLIID: x.cliID, Compact: true})
	if _, err := enqueueRunnerJob(ctx, s, &Actor{User: &User{ID: x.user}}, RunnerJobInput{Kind: JobMessage, OwnerID: x.user, Label: x.machine, Input: raw,
		Source: deferredCompactSource, SourceID: x.id}); err != nil {
		if _, e := s.writeRows(ctx, `UPDATE agent_sessions SET warm_state = '' WHERE id = ? AND warm_state = ?`, x.id, warmCompacting); e != nil {
			s.log.WarnContext(ctx, "deferred offer: release the session", "session", x.id, "err", e)
		}
		s.log.InfoContext(ctx, "deferred offer: queue its compaction", "session", x.id, "err", err)
		return false
	}
	return true
}

// deferredIdle offers an idle session the tasks it filed after its ship,
// compacting it first when its context is large. It says whether it did
// something that a next step should wait one lap for: it queued the
// compaction, waits for it, or sent the offer. A session its person is typing
// in is left alone and nothing is recorded.
func (s *Service) deferredIdle(ctx context.Context, id int64) bool {
	x, err := s.deferredSession(ctx, id)
	if err != nil || s.deferredBusy(ctx, x) {
		return false
	}
	f, err := s.deferredFind(ctx, x)
	if err != nil {
		s.log.WarnContext(ctx, "deferred offer: look for tasks", "session", id, "err", err)
		return false
	}
	if f == nil {
		return false
	}
	state := s.deferredCompactState(ctx, id)
	switch {
	case state == deferredCompactWaits:
		return true
	case state == "" && x.tokens >= deferredCompactFrom() && x.cli == "claude" && s.deferredTypable(ctx, id, true):
		if s.compactForDeferred(ctx, x) {
			return true
		}
	}
	// recorded untold, and told once the text is queued: a send that fails
	// leaves them for the minute pass
	offered, err := s.recordDeferred(ctx, id, f.cands, f.task, DeferredViaIdle, x.tokens, nil, state == deferredCompactDone)
	if err != nil {
		s.log.WarnContext(ctx, "deferred offer: record it", "session", id, "err", err)
		return false
	}
	if len(offered) == 0 {
		return false
	}
	return s.deferredSend(ctx, id, offered, deferredText(f.keys, deferredOfferOf(offered), true), true) == nil
}

// deferredSweep is the minute pass: sessions whose offer waits (for its
// compaction, or for a send that failed), and idle sessions with tasks filed
// after their ship whose cache is still warm and no babysit is open on (a
// babysit's own news will wake them).
func (s *Service) deferredSweep(ctx context.Context) {
	waiting, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT session_id FROM deferred_offers WHERE told_at IS NULL LIMIT 50`)
	if err != nil {
		s.log.WarnContext(ctx, "deferred offers: read the untold", "err", err)
	}
	for _, id := range waiting {
		s.deferredUntold(ctx, id)
	}
	now := s.nowMS()
	idle, err := queryIDs(ctx, s.db.R, `SELECT a.id FROM agent_sessions a WHERE a.ended_at IS NULL AND a.state = ? AND a.stopped_at IS NOT NULL AND a.stopped_at >= ?
		AND (a.model LIKE '%opus%' OR a.model LIKE '%fable%' OR a.model LIKE '%mythos%')
		AND EXISTS (SELECT 1 FROM babysits s WHERE s.session_id = a.id)
		AND NOT EXISTS (SELECT 1 FROM deferred_offers u WHERE u.session_id = a.id AND u.told_at IS NULL)
		AND NOT EXISTS (SELECT 1 FROM babysits b WHERE b.session_id = a.id AND b.state NOT IN (?, ?, ?))
		AND EXISTS (SELECT 1 FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.session_id = a.id AND st.category NOT IN ('done','canceled') AND i.created_at >= ?
			AND NOT EXISTS (SELECT 1 FROM deferred_offers d WHERE d.session_id = a.id AND d.issue_id = i.id))
		ORDER BY a.stopped_at LIMIT 50`, SessionIdle, now-cacheWarm.Milliseconds(), BabysitMerged, BabysitStopped, BabysitGaveUp, now-originRecent.Milliseconds())
	if err != nil {
		s.log.WarnContext(ctx, "deferred offers: read the idle sessions", "err", err)
		return
	}
	for _, id := range idle {
		if warm, inTurn := s.cacheWarmNow(ctx, id); !warm || inTurn {
			continue
		}
		if !s.deferredTypable(ctx, id, true) {
			continue
		}
		s.deferredIdle(ctx, id)
	}
}

// deferredUntold tells a session the offers waiting for it: when it idles
// and has a large context it is compacted once, then told by message and a
// post to its inbox. The wording is the ship's unless the session has shipped
// since the rows were recorded. Past deferredCompactWait, with no runner to
// do it, the text is queued for the session's next hook.
func (s *Service) deferredUntold(ctx context.Context, id int64) {
	warn := func(what string, err error) {
		s.log.WarnContext(ctx, "deferred offers: "+what, "session", id, "err", err)
	}
	rows, err := queryIDs(ctx, s.db.R, `SELECT issue_id FROM deferred_offers WHERE session_id = ? AND told_at IS NULL ORDER BY id`, id)
	if err != nil {
		warn("read the untold", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	var offered, task, shippedAt sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT min(offered_at), max(task_issue_id), max(shipped_at) FROM deferred_offers WHERE session_id = ? AND told_at IS NULL`, id).Scan(&offered, &task, &shippedAt); err != nil {
		warn("read when they were recorded", err)
		return
	}
	x, err := s.deferredSession(ctx, id)
	if err != nil {
		warn("read the session", err)
		return
	}
	told := func() {
		if _, err := s.writeRows(ctx, `UPDATE deferred_offers SET told_at = ? WHERE session_id = ? AND told_at IS NULL`, s.nowMS(), id); err != nil {
			warn("mark them told", err)
		}
	}
	if x.endedAt.Valid {
		told()
		return
	}
	issues, err := s.issuesByIDs(ctx, rows, false)
	if err != nil {
		warn("read the tasks", err)
		return
	}
	issues = slices.DeleteFunc(issues, func(i *Issue) bool { return i.Status.Category == "done" || i.Status.Category == "canceled" })
	// autopilot and the limit are read again: they may be off now, and the
	// user may no longer read the project
	if len(issues) > 0 {
		user, err := s.userByID(ctx, s.db.R, x.user)
		if err != nil {
			warn("read the session's user", err)
			return
		}
		on := map[int64]bool{}
		for _, i := range issues {
			if _, seen := on[i.ProjectID]; seen {
				continue
			}
			ok, err := s.deferredOn(ctx, &Actor{User: user, Session: id}, i.ProjectID)
			if err != nil {
				// a failed read isn't "off": the rows stay untold for the next sweep
				warn("check autopilot", err)
				return
			}
			on[i.ProjectID] = ok && !user.Disabled
		}
		issues = slices.DeleteFunc(issues, func(i *Issue) bool { return !on[i.ProjectID] })
	}
	if len(issues) == 0 {
		told()
		return
	}
	late := s.nowMS()-offered.Int64 >= deferredCompactWait.Milliseconds()
	idle := x.state == SessionIdle && x.stopped.Valid
	state := s.deferredCompactState(ctx, id)
	if !late {
		switch {
		case !idle || state == deferredCompactWaits || s.deferredBusy(ctx, x):
			return
		case state == "" && x.tokens >= deferredCompactFrom() && x.cli == "claude" && s.deferredTypable(ctx, id, true):
			if s.compactForDeferred(ctx, x) {
				if _, err := s.writeRows(ctx, `UPDATE deferred_offers SET compacted = 1 WHERE session_id = ? AND told_at IS NULL`, id); err != nil {
					warn("mark them compacted", err)
				}
				return
			}
		}
	}
	var keys []string
	if task.Valid {
		if t, err := s.issuesByIDs(ctx, []int64{task.Int64}, false); err == nil && len(t) > 0 {
			keys = append(keys, t[0].Key)
		}
	}
	after := shippedAt.Valid
	if !after {
		_, after, err = s.deferredShipSince(ctx, id, offered.Int64)
		if err != nil {
			warn("read its ships", err)
			return
		}
	}
	if s.deferredSend(ctx, id, issues, deferredText(keys, deferredOfferOf(issues), after), !late) != nil {
		return
	}
	told()
}

// deferredShipSince says whether the session shipped (started a babysit) at
// or after the time.
func (s *Service) deferredShipSince(ctx context.Context, id, since int64) (int64, bool, error) {
	at, shipped, err := s.deferredShipTime(ctx, id)
	return at, shipped && at >= since, err
}
