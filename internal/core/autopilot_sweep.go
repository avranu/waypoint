package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Autopilot's own sessions (WP-799, WP-ADR72). A minute sweep looks at each
// project a person has turned autopilot on in, and when a task is ready and
// fewer sessions are under way there than the person's limit (atOnceCount:
// autopilot's, and the person's own at a task, WP-1079), it queues a new
// Claude Code session in tmux on that person's runner. A person's own
// sessions are never used or interrupted.
//
// Such a start is a step with no session (session_id NULL): the row is what
// holds the cap while the task is picked and the job is out.

const (
	// autopilotOwnAgent is who a session of autopilot's own takes tasks as
	// when the person has no earlier session in the project to go by.
	autopilotOwnAgent = "anthropic/claude-code"
	// A failed start isn't tried again for this long, doubling up to the most.
	ownStartBackoff    = 5 * time.Minute
	ownStartBackoffMax = time.Hour
)

// sweepAutopilot starts what each project with autopilot on has room for.
func (s *Service) sweepAutopilot(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT user_id, project_id FROM autopilot_settings WHERE enabled = 1 AND project_id IS NOT NULL`)
	if err != nil {
		s.log.WarnContext(ctx, "autopilot sweep", "err", err)
		return
	}
	type on struct{ user, project int64 }
	var list []on
	for rows.Next() {
		var o on
		if rows.Scan(&o.user, &o.project) == nil {
			list = append(list, o)
		}
	}
	_ = rows.Close()
	plans := map[int64]*SpendPlan{}
	for _, o := range list {
		plan, ok := plans[o.user]
		if !ok {
			var err error
			if plan, err = s.sweepSpendPlan(ctx, o.user); err != nil {
				s.log.WarnContext(ctx, "autopilot sweep: the week's-end plan", "user", o.user, "err", err)
			}
			plans[o.user] = plan
		}
		spent, err := s.sweepProject(ctx, o.user, o.project, plan)
		if err != nil {
			s.log.WarnContext(ctx, "autopilot sweep", "user", o.user, "project", o.project, "err", err)
		}
		if spent {
			plan.Start = false // one such start a sweep, then the readings decide again
		}
	}
}

// sweepSpendPlan is the person's week's-end plan for this sweep, nil when
// they haven't turned it on (WP-262).
func (s *Service) sweepSpendPlan(ctx context.Context, userID int64) (*SpendPlan, error) {
	lim, err := s.spendLimits(ctx, s.db.R, userID)
	if err != nil || !valueOf(lim.SpendLeft) {
		return nil, err
	}
	m, err := s.autopilotMeter(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.spendPlan(ctx, userID, lim, m, s.now())
}

// ownStartAgent is the identity a session of autopilot's own takes its task
// as: what the person's latest session in the project worked as, since a
// claim holds only for that exact identity, else Claude Code's own name.
func (s *Service) ownStartAgent(ctx context.Context, userID, projectID int64) string {
	var agent string
	_ = s.db.R.QueryRowContext(ctx, `SELECT r.agent FROM agent_runs r JOIN agent_sessions x ON x.id = r.session_id JOIN issues i ON i.id = r.issue_id
		WHERE x.user_id = ? AND i.project_id = ? AND r.agent != '' ORDER BY r.started_at DESC, r.id DESC LIMIT 1`, userID, projectID).Scan(&agent)
	return cmp(agent, autopilotOwnAgent)
}

// releaseOwnTask gives back the task reserved for a start (a step with no
// session) that didn't happen, unless a session has taken it up or the
// reservation lapsed and someone else holds the task now.
func releaseOwnTask(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, stepID int64) error {
	_, err := q.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL
		WHERE id = (SELECT next_issue FROM autopilot_steps WHERE id = ?)
		  AND claimed_by != '' AND claimed_by = (SELECT agent FROM autopilot_steps WHERE id = ?)
		  AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = issues.id AND r.ended_at IS NULL)`, stepID, stepID)
	return err
}

// bindOwnSessions fills in the session of a start whose session Waypoint
// hadn't heard of when its job finished: the first run on its task since.
// Without it the session would stop counting against the limit after that run.
// Only a session that began after the step, in a worktree as autopilot's do,
// can be it: a person's session already running never is.
func (s *Service) bindOwnSessions(ctx context.Context, userID, projectID, now int64) error {
	_, err := s.db.W.ExecContext(ctx, `UPDATE autopilot_steps SET next_session = (SELECT r.session_id FROM agent_runs r JOIN agent_sessions x ON x.id = r.session_id
			WHERE r.issue_id = autopilot_steps.next_issue AND x.user_id = autopilot_steps.user_id AND x.worktree = 1
			  AND x.started_at >= autopilot_steps.created_at AND r.started_at >= autopilot_steps.created_at ORDER BY r.started_at, r.id LIMIT 1),
			next_session_at = coalesce(next_session_at, updated_at)
		WHERE user_id = ? AND project_id = ? AND session_id IS NULL AND state = ? AND next_session IS NULL AND next_issue IS NOT NULL AND updated_at > ?`,
		userID, projectID, StepStarted, now-(24*time.Hour).Milliseconds())
	return err
}

// lastOwnStart is the project's latest step with no session, or nil.
func (s *Service) lastOwnStart(ctx context.Context, userID, projectID int64) (*stepRow, error) {
	st, err := scanStep(s.db.R.QueryRowContext(ctx, `SELECT `+stepCols+` FROM autopilot_steps WHERE user_id = ? AND project_id = ? AND session_id IS NULL ORDER BY id DESC LIMIT 1`, userID, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return st, err
}

// ownStartHeldOff: the last start failed, or a person stopped it, recently
// enough that the sweep leaves the project alone for now.
func ownStartHeldOff(last *stepRow, now int64) bool {
	since := time.Duration(now-last.updatedAt) * time.Millisecond
	switch {
	case last.state == StepFailed:
		wait := ownStartBackoff
		for i := 1; i < last.attempts && wait < ownStartBackoffMax; i++ {
			wait *= 2
		}
		return since < min(wait, ownStartBackoffMax)
	case last.state == StepStopped && last.reason == "stopped":
		return since < ownStartBackoffMax
	}
	return false
}

// sweepProject starts one session of autopilot's own in the project if it
// may, at most one per sweep. spend is the person's week's-end plan (nil:
// off): while it says start, a start may go past the project's at_once and
// its pace (WP-262). spent: such a start was claimed.
func (s *Service) sweepProject(ctx context.Context, userID, projectID int64, spend *SpendPlan) (spent bool, err error) {
	now := s.nowMS()
	on, lim, err := s.autopilotFor(ctx, s.db.R, userID, projectID)
	if err != nil || !on {
		return false, err
	}
	last, err := s.lastOwnStart(ctx, userID, projectID)
	if err != nil {
		return false, err
	}
	limit := valueOf(lim.AtOnce)
	if limit == 0 {
		return false, s.dropOwnWaiting(ctx, last)
	}
	if err := s.bindOwnSessions(ctx, userID, projectID, now); err != nil {
		return false, err
	}
	if last != nil && (last.state == StepStarting || ownStartHeldOff(last, now)) {
		return false, nil
	}
	m, err := s.autopilotMeter(ctx, userID)
	if err != nil {
		return false, err
	}
	if why, _ := autopilotPaused(m, lim, s.now()); why != "" {
		return false, nil
	}
	extra := spend != nil && spend.Start
	paceWait, _, _, err := s.autopilotPace(ctx, s.db.R, userID, projectID, lim, now, false)
	if err != nil || paceWait != "" && !extra {
		return false, err
	}
	running, err := s.atOnceCount(ctx, s.db.R, userID, projectID, 0, true, now)
	if err != nil || len(running) >= limit && !extra {
		return false, err
	}
	// When only the plan lets this one go, it is a start to use what's left.
	var spendStart *SpendPlan
	if paceWait != "" || len(running) >= limit {
		spendStart = spend
	}
	user, err := s.userByID(ctx, s.db.R, userID)
	if err != nil || user.Disabled {
		return false, err
	}
	agent := s.ownStartAgent(ctx, userID, projectID)
	actor := &Actor{User: user, Agent: agent}
	if p, err := s.pauseFor(ctx, actor, agent); err != nil || p != nil {
		return false, err
	}
	key := s.projectKeyOf(ctx, projectID)
	ready, _, err := s.agentWork(ctx, actor, []string{agent}, AgentQuery{Agent: agent, Project: key, Query: lim.criteriaQuery(), Peek: true})
	if err != nil {
		return false, err
	}
	if len(s.withoutFinished(ctx, ready)) == 0 {
		return false, s.dropOwnWaiting(ctx, last)
	}
	st := &stepRow{userID: userID, projectID: projectID}
	if trip, wait, _, err := s.autopilotLoop(ctx, st, lim, now); err != nil {
		return false, err
	} else if trip != "" {
		return false, s.autopilotTrip(ctx, st, trip)
	} else if wait != "" {
		return false, nil
	}
	places, err := s.sessionPlaces(ctx, userID, jobNeeds{kind: JobAutopilotStart, cli: "claude"})
	if err != nil {
		return false, err
	}
	var place *TaskSessionPlace
	for _, p := range places {
		if p.ProjectKey == key && p.Online {
			place = p
		}
	}
	if place == nil {
		return false, s.ownStartNeedsRunner(ctx, userID, projectID, key, last, now)
	}
	claimed, err := s.claimOwnStart(ctx, userID, projectID, last, now, spendStart)
	if err != nil || claimed == nil {
		return false, err
	}
	return claimed.spend, s.startOwnSession(ctx, claimed, actor, user, place, lim, now)
}

// claimOwnStart is the cap (WP-ADR72): autopilot still on, the pace still
// allowing a start, nothing of its own starting, room under the limit, and
// the row that holds the place, all in one transaction, so two checks at once
// can't both see room. nil: no room after all. With spend, the start is one
// to use what's left of the week (WP-262): the cap across the person's
// projects and the spacing between such starts replace the project's
// at_once and pace.
func (s *Service) claimOwnStart(ctx context.Context, userID, projectID int64, last *stepRow, now int64, spend *SpendPlan) (*stepRow, error) {
	var st *stepRow
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		on, lim, err := s.autopilotFor(ctx, t, userID, projectID)
		if err != nil || !on || valueOf(lim.AtOnce) == 0 {
			return err
		}
		if spend == nil {
			if wait, _, _, err := s.autopilotPace(ctx, t, userID, projectID, lim, now, false); err != nil || wait != "" {
				return err // the sweep's first look at the pace was before it looked for work
			}
		}
		var starting int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE user_id = ? AND project_id = ? AND session_id IS NULL AND state = ?`,
			userID, projectID, StepStarting).Scan(&starting); err != nil || starting > 0 {
			return err
		}
		if spend == nil {
			running, err := s.atOnceCount(ctx, t, userID, projectID, 0, true, now)
			if err != nil || len(running) >= valueOf(lim.AtOnce) {
				return err
			}
		} else if room, err := s.spendRoom(ctx, t, userID, spend.Cap, now); err != nil || !room {
			return err
		}
		claim := &stepRow{userID: userID, projectID: projectID, state: StepStarting, stopAt: now, dueAt: now, createdAt: now, updatedAt: now, spend: spend != nil}
		if last != nil && (last.state == StepFailed || last.state == StepWaiting) {
			claim.attempts, claim.notified = last.attempts, last.notified // what it has said and tried carries over
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM autopilot_steps WHERE user_id = ? AND project_id = ? AND session_id IS NULL AND state = ?`, userID, projectID, StepWaiting); err != nil {
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO autopilot_steps (user_id, project_id, session_id, stop_at, state, detail, due_at, attempts, notified, created_at, updated_at, spend)
			VALUES (?, ?, NULL, ?, ?, 'picking the next task', ?, ?, ?, ?, ?, ?)`, userID, projectID, now, StepStarting, now, claim.attempts, claim.notified, now, now, boolInt(claim.spend))
		if err != nil {
			return err
		}
		claim.id, err = res.LastInsertId()
		st = claim
		return err
	})
	return st, err
}

// startOwnSession picks the task for a claimed start and queues its job.
func (s *Service) startOwnSession(ctx context.Context, st *stepRow, actor *Actor, user *User, place *TaskSessionPlace, lim AutopilotLimits, now int64) error {
	x := &AgentSession{Machine: place.machine}
	next, _, err := s.pickAutopilotTask(ctx, actor, st, x, lim)
	if err != nil || next == nil {
		// Taken in the meantime, or not readable: nothing happened, so no trace.
		if _, derr := s.db.W.ExecContext(ctx, `DELETE FROM autopilot_steps WHERE id = ? AND state = ? AND job_id IS NULL`, st.id, StepStarting); derr != nil && err == nil {
			err = derr
		}
		return err
	}
	prompt := s.kickoffTemplate(ctx, st.projectID, next.ID)
	held, heldErr := s.autopilotHeld(ctx, x)
	if heldErr != nil {
		s.log.WarnContext(ctx, "autopilot: what other sessions hold", "step", st.id, "err", heldErr) // the sweep removes nothing this time
	}
	raw, _ := json.Marshal(AutopilotJob{Dir: place.Dir, Key: next.Key, Title: excerpt(next.Title, 200), Agent: actor.Agent, Step: st.id, Prompt: prompt, Held: held, HeldUnknown: heldErr != nil})
	job, err := enqueueRunnerJob(ctx, s, &Actor{User: user}, RunnerJobInput{Kind: JobAutopilotStart, OwnerID: st.userID, RunnerID: place.RunnerID, Input: raw,
		Source: autopilotSource, SourceID: st.id, ProjectID: st.projectID})
	if err != nil {
		s.releaseReserved(ctx, next.ID, actor.Agent)
		s.ownStartFailed(ctx, st, WaitRunner, next.Key, "couldn't queue the runner job: "+err.Error())
		return nil
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE autopilot_steps SET reason = '', detail = ?, next_issue = ?, job_id = ?, agent = ?, updated_at = ? WHERE id = ? AND state = ?`,
		"starting a session of its own for "+next.Key+spendWords(st.spend), next.ID, job.ID, actor.Agent, now, st.id, StepStarting)
	if err != nil {
		// Don't leave the step starting with no job: give the task back and end
		// it, so the sweep tries again later. A job already queued finds the step
		// unwanted (AutopilotStepWanted).
		s.releaseReserved(ctx, next.ID, actor.Agent)
		s.ownStartFailed(ctx, st, WaitRunner, next.Key, "couldn't record the runner job: "+err.Error())
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		s.releaseReserved(ctx, next.ID, actor.Agent) // stopped while its job was queued: the job finds it unwanted
	}
	return nil
}

// ownStartFailed ends a start that didn't happen: the sweep tries again
// after a wait that grows (ownStartHeldOff), and the person hears once.
func (s *Service) ownStartFailed(ctx context.Context, st *stepRow, reason, key, why string) {
	_, _ = s.db.W.ExecContext(ctx, `UPDATE autopilot_steps SET attempts = attempts + 1 WHERE id = ?`, st.id)
	if err := s.stepUpdate(ctx, st.id, StepFailed, reason, why, 0); err != nil {
		s.log.WarnContext(ctx, "autopilot: ending a failed start", "step", st.id, "err", err)
	}
	s.tellOnce(ctx, st, reason, "Autopilot couldn't start a session", fmt.Sprintf("Autopilot couldn't start a session in %s for %s: %s. It tries again by itself, less often each time.", s.projectKeyOf(ctx, st.projectID), key, why))
}

// ownStartFinished records how the job of a start went.
func (s *Service) ownStartFinished(ctx context.Context, st *stepRow, job *RunnerJob, res AutopilotJobResult, now int64) {
	nextKey := "the next task"
	if i, err := s.issueByID(ctx, s.db.R, st.nextIssue.Int64, false); err == nil {
		nextKey = i.Key
	}
	switch {
	case job.State == "done" && res.Outcome == "started":
		var next sql.NullInt64
		if res.NewCLI != "" {
			_ = s.db.R.QueryRowContext(ctx, `SELECT id FROM agent_sessions WHERE user_id = ? AND cli_id = ? AND worktree = 1 AND started_at >= (SELECT created_at FROM autopilot_steps WHERE id = ?) AND (issue_id = ? OR EXISTS (SELECT 1 FROM agent_runs r WHERE r.session_id = agent_sessions.id AND r.issue_id = ?)) ORDER BY id DESC LIMIT 1`, st.userID, res.NewCLI, st.id, st.nextIssue.Int64, st.nextIssue.Int64).Scan(&next)
		}
		detail := nextKey + " started in a session of its own" + spendWords(st.spend)
		if res.Warning != "" {
			detail += " (" + res.Warning + ")"
		}
		_, _ = s.db.W.ExecContext(ctx, `UPDATE autopilot_steps SET state = ?, reason = '', notified = '', detail = ?, next_session = ?, next_session_at = ?, attempts = 0, updated_at = ? WHERE id = ? AND state = ?`,
			StepStarted, excerpt(detail, 300), next, now, now, st.id, StepStarting)
		if next.Valid {
			project := sql.NullInt64{Int64: st.projectID, Valid: true}
			_ = s.write(ctx, SystemActor(), func(t *tx) error {
				return s.sessionTimeline(ctx, t, next.Int64, now, "autopilot", "", 0, "started", "", "autopilot started "+nextKey, project, map[string]any{"next": nextKey})
			})
		}
	case job.State == "done" && res.Outcome == "unwanted":
		// Stopped while the job was on its way: stopping released the task.
	default:
		_ = releaseOwnTask(ctx, s.db.W, st.id)
		reason := WaitJob
		if res.Outcome == "held" && res.Reason != "" {
			reason = res.Reason
		}
		s.ownStartFailed(ctx, st, reason, nextKey, cmp(cmp(job.Error, res.Detail), "the runner job "+job.State))
	}
}

// ownStartLanded: the start was stopped too late and its job reports a
// session all the same. It is running and took its task with wp next, so the
// step points at it and it counts against the limit; the step stays stopped.
func (s *Service) ownStartLanded(ctx context.Context, st *stepRow, jobID int64) {
	if st.sessionID != 0 || st.state != StepStopped {
		return
	}
	job, err := lookupRunnerJob(ctx, s, jobID)
	if err != nil || job.State != "done" {
		return
	}
	var res AutopilotJobResult
	if json.Unmarshal(job.Result, &res) != nil || res.Outcome != "started" || res.NewCLI == "" {
		return
	}
	_, _ = s.db.W.ExecContext(ctx, `UPDATE autopilot_steps SET next_session = (SELECT id FROM agent_sessions WHERE user_id = ? AND cli_id = ? AND worktree = 1 AND started_at >= autopilot_steps.created_at AND (issue_id = autopilot_steps.next_issue OR EXISTS (SELECT 1 FROM agent_runs r WHERE r.session_id = agent_sessions.id AND r.issue_id = autopilot_steps.next_issue)) ORDER BY id DESC LIMIT 1), next_session_at = coalesce(next_session_at, updated_at) WHERE id = ? AND next_issue IS NOT NULL`, st.userID, res.NewCLI, st.id)
}

// ownStartNeedsRunner keeps one waiting step that says why a ready task isn't
// being started, for the Autopilot tab, and tells the person once.
func (s *Service) ownStartNeedsRunner(ctx context.Context, userID, projectID int64, key string, last *stepRow, now int64) error {
	detail := fmt.Sprintf("a task is ready in %s, but no runner of yours can start a session for it: run an up-to-date wp runner, with tmux and --allow for the checkout, on a machine where you work on %s", key, key)
	st := last
	if last == nil || last.state != StepWaiting {
		res, err := s.db.W.ExecContext(ctx, `INSERT INTO autopilot_steps (user_id, project_id, session_id, stop_at, state, reason, detail, due_at, created_at, updated_at)
			VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?)`, userID, projectID, now, StepWaiting, WaitRunner, detail, now, now, now)
		if err != nil {
			return err
		}
		st = &stepRow{userID: userID, projectID: projectID}
		if st.id, err = res.LastInsertId(); err != nil {
			return err
		}
	}
	s.tellOnce(ctx, st, WaitRunner, "Autopilot needs a runner", "Autopilot can't start a session of its own: "+detail+".")
	return nil
}

// dropOwnWaiting removes the waiting step of a project that no longer has
// anything to start.
func (s *Service) dropOwnWaiting(ctx context.Context, last *stepRow) error {
	if last == nil || last.state != StepWaiting {
		return nil
	}
	_, err := s.db.W.ExecContext(ctx, `DELETE FROM autopilot_steps WHERE id = ? AND state = ?`, last.id, StepWaiting)
	return err
}

// stopOwnStart ends a step with no session, and gives back a task reserved
// for it. A job already out asks whether it is still wanted before it starts
// anything (AutopilotStepWanted).
func (s *Service) stopOwnStart(ctx context.Context, t *tx, id int64, reason, detail string) error {
	var next sql.NullInt64
	var state string
	err := t.QueryRowContext(ctx, `SELECT next_issue, state FROM autopilot_steps WHERE id = ? AND session_id IS NULL AND state IN (?, ?)`, id, StepWaiting, StepStarting).Scan(&next, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if state == StepStarting && next.Valid {
		if err := releaseOwnTask(ctx, t, id); err != nil {
			return err
		}
	}
	_, err = t.ExecContext(ctx, `UPDATE autopilot_steps SET state = ?, reason = ?, detail = ?, updated_at = ? WHERE id = ?`, StepStopped, reason, detail, s.nowMS(), id)
	return err
}

// stopOwnStarts ends every live step with no session in a project.
func (s *Service) stopOwnStarts(ctx context.Context, t *tx, userID, projectID int64, reason, detail string) error {
	ids, err := queryIDs(ctx, t, `SELECT id FROM autopilot_steps WHERE user_id = ? AND project_id = ? AND session_id IS NULL AND state IN (?, ?)`,
		userID, projectID, StepWaiting, StepStarting)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.stopOwnStart(ctx, t, id, reason, detail); err != nil {
			return err
		}
	}
	return nil
}

// spendWords says a start was made to use what's left of the week.
func spendWords(spend bool) string {
	if spend {
		return ", to use what's left of the week's quota"
	}
	return ""
}
