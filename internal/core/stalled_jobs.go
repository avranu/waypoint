package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Stalled background jobs (WP-605, WP-ADR97; timing WP-1045, WP-ADR118). A
// runner watches its machine's Claude Code background jobs from /proc and
// reports each one that has been quiet (no progress) for a few minutes,
// again while it stays quiet, and once more when it moves again or ends.
// Waypoint keeps each quiet spell (stalled_jobs) and decides from the
// project's policy (StallPolicy) and the session's prompt cache whether and
// how to tell the session, once per spell; it never touches the job: no CPU
// isn't always stuck (a stalled download that resumes, a lock a person's
// session holds, a deploy that mustn't be cut short).
//
//   - Warm cache: once the job is stalled, the session is woken (its work
//     waits on its job), or mid-turn reads it at its Stop. And when its
//     keep-warm ping is due while a job has been quiet a while (Early), the
//     notice goes then in place of the ping (cache_warm.go): it costs the
//     ping's turn, not another.
//   - Cold cache: waking it re-reads its whole context, so the notice waits
//     for its next turn; once the job has made no progress for ColdWake it
//     is woken after all, so the work goes on without a person stepping in.
//   - A notice not yet read when the job moves again or ends is withdrawn:
//     it is better unsent.
//
// How each spell ended is the evidence: a kind of job whose quiet spells
// often end by themselves gets a longer quiet time, and cold wakes stop
// while they seldom lead to the job being stopped (stalled_learn.go).

// StalledJob is one quiet background job a runner reports.
type StalledJob struct {
	Session   string    `json:"session"` // the Claude Code session's id
	Task      string    `json:"task"`    // the background task's id
	PID       int       `json:"pid"`
	Group     bool      `json:"group"` // the pid leads its own process group
	Command   string    `json:"command"`
	StartedAt time.Time `json:"started_at"`
	Quiet     int       `json:"quiet_minutes"`
	Evidence  []string  `json:"evidence"`
	Running   []string  `json:"running"`
	// Timeout is the timeout its whole command runs under, in minutes (0:
	// none). Runners before WP-1045 skip one under an hour themselves.
	Timeout int `json:"timeout_minutes,omitempty"`
}

// SettledJob is a job the runner reported that moved again or ended.
type SettledJob struct {
	Session string `json:"session"`
	Task    string `json:"task"`
	How     string `json:"how"` // progress, ended
}

// StalledJobsReport is a runner's report. V 2: Jobs are every job quiet for
// the runner's floor (cmd/wp stallFloor), and Settled the ones it reported
// that moved again or ended; Waypoint applies the policy. Older runners
// report only jobs quiet 15 minutes in jobs 30 minutes old, once.
type StalledJobsReport struct {
	Runner  int64        `json:"runner"`
	Jobs    []StalledJob `json:"jobs"`
	Settled []SettledJob `json:"settled,omitempty"`
	V       int          `json:"v,omitempty"`
}

// StalledJobOutcome says what Waypoint did with one reported job.
type StalledJobOutcome struct {
	Task    string `json:"task"`
	Session string `json:"session"`
	Outcome string `json:"outcome"`
	// Retry: report it at the next look (an older runner reports a job once
	// unless asked again).
	Retry bool `json:"retry,omitempty"`
	// AgainIn: report it again in this many seconds while it stays quiet.
	AgainIn int `json:"again_in,omitempty"`
}

// StallPolicyVersion is what the report's answer says Waypoint does: 2 when
// it applies the policy to jobs from the runner's floor.
const StallPolicyVersion = 2

const (
	// stallAgainMax: a quiet job is reported at least this often, so a
	// spell with no report for stallLostAfter was lost (the runner restarted
	// or went away) and its notice decides nothing.
	stallAgainMax  = 15 * time.Minute
	stallAgainSoon = 5 * time.Minute
	stallLostAfter = 40 * time.Minute
	// stallActedWithin: a job that ends this soon after its session read
	// the notice was stopped for it.
	stallActedWithin = 15 * time.Minute
)

// How a session was told (stalled_jobs.told_how).
const (
	stallWoke     = "woke"      // warm and idle: woken now
	stallAtStop   = "at_stop"   // in a turn: read at its Stop
	stallPassive  = "passive"   // cold (or not to be woken): read at its next turn
	stallKeepWarm = "keep_warm" // went in place of a keep-warm ping
	stallColdWake = "cold_wake" // cold, woken after ColdWake
)

// ReportStalledJobs takes a runner's report: the quiet jobs and the ones
// that moved again or ended.
func (s *Service) ReportStalledJobs(ctx context.Context, actor *Actor, in StalledJobsReport) ([]StalledJobOutcome, error) {
	if err := actor.need(PermRunnersHost); err != nil {
		return nil, err
	}
	if err := runnerActor(actor); err != nil {
		return nil, err
	}
	r, err := s.runnerByID(ctx, s.db.R, in.Runner)
	if err != nil || r.Owner.ID != actor.User.ID {
		return nil, notFound("runner")
	}
	user := actor.User.ID
	if len(in.Settled) > 200 { // as many as a runner keeps (cmd/wp settle)
		in.Settled = in.Settled[:200]
	}
	for _, j := range in.Settled {
		if cliSessionRE.MatchString(j.Session) && cliSessionRE.MatchString(j.Task) {
			s.settleStalled(ctx, user, j)
		}
	}
	s.loseStalled(ctx, user)
	if len(in.Jobs) > 20 {
		in.Jobs = in.Jobs[:20]
	}
	out := []StalledJobOutcome{}
	for _, j := range in.Jobs {
		if !cliSessionRE.MatchString(j.Session) || !cliSessionRE.MatchString(j.Task) {
			continue
		}
		o, err := s.quietJob(ctx, user, r.Name, j, in.V >= StallPolicyVersion)
		if err != nil {
			s.log.WarnContext(ctx, "tell a session its background job stalled", "session", j.Session, "task", j.Task, "err", err)
			o = StalledJobOutcome{Task: j.Task, Session: j.Session, Outcome: "not told: " + err.Error(), Retry: true}
		}
		out = append(out, o)
	}
	return out, nil
}

// stallSpell is a quiet spell as kept.
type stallSpell struct {
	ID, Session, Project int64
	QuietFrom, StartedAt time.Time
	ToldAt               sql.NullInt64
	ToldHow              string
	Message              sql.NullInt64
	Class                string
}

// quietJob records one report of a quiet job and decides about its
// session.
func (s *Service) quietJob(ctx context.Context, userID int64, machine string, j StalledJob, v2 bool) (StalledJobOutcome, error) {
	o := StalledJobOutcome{Task: j.Task, Session: j.Session}
	// later: report it again in d while it stays quiet. An older runner
	// reports a job once unless asked again, at its next look (a minute), so
	// it is asked only while something is still to come: the job isn't
	// stalled yet, or a cold wake waits.
	later := func(d time.Duration, outcome string, pending bool) (StalledJobOutcome, error) {
		o.Outcome = outcome
		d = min(max(d, time.Minute), stallAgainMax)
		if v2 {
			o.AgainIn = int(d.Seconds())
		} else {
			o.Retry = pending
		}
		return o, nil
	}
	var id int64
	var project sql.NullInt64
	err := s.db.R.QueryRowContext(ctx, `SELECT id, project_id FROM agent_sessions WHERE user_id = ? AND cli_id = ? AND ended_at IS NULL
		ORDER BY (machine = ? COLLATE NOCASE) DESC, last_seen_at DESC LIMIT 1`, userID, j.Session, machine).Scan(&id, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return later(stallAgainMax, "no open Waypoint session "+j.Session, false)
	}
	if err != nil {
		return o, err
	}
	sp, err := s.recordQuiet(ctx, userID, id, project.Int64, j)
	if err != nil {
		return o, err
	}
	pol := s.stallPolicyFor(ctx, s.db.R, project.Int64)
	now := s.now()
	quiet := now.Sub(sp.QuietFrom)
	// A spell that can't be told can't be acted on, so while untold it isn't
	// evidence of how long the job's kind stays quiet and fine.
	skip := func(why string) {
		if sp.ToldAt.Valid {
			return
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE stalled_jobs SET skipped = ? WHERE id = ? AND skipped = ''`, why, sp.ID); err != nil {
			s.log.WarnContext(ctx, "stalled jobs: mark a spell skipped", "id", sp.ID, "err", err)
		}
	}
	switch {
	case pol.Off:
		skip("off")
		return later(stallAgainMax, "not told: stalled jobs aren't told in its project", false)
	case j.Timeout > 0 && j.Timeout < pol.SkipTimeout:
		skip("timeout")
		return later(stallAgainMax, fmt.Sprintf("not told: its %d-minute timeout ends it soon enough", j.Timeout), false)
	case sp.ToldAt.Valid:
		return s.toldAgain(ctx, userID, sp, pol, quiet, later)
	}
	th := s.stallThreshold(ctx, userID, sp.Class, pol)
	age := now.Sub(sp.StartedAt)
	if j.StartedAt.IsZero() {
		age = quiet
	}
	minAge := time.Duration(pol.MinAge) * time.Minute
	need := time.Duration(th.Quiet) * time.Minute
	if age < minAge || quiet < need {
		// Soon enough for an early notice at a keep-warm ping to read it fresh.
		return later(min(max(minAge-age, need-quiet), stallAgainSoon), "quiet, not stalled yet", true)
	}
	how, outcome, err := s.tellStalled(ctx, userID, id, sp, pol, th, j, quiet)
	if err != nil {
		return o, err
	}
	if how == stallPassive && pol.ColdWake > 0 {
		return later(time.Duration(max(pol.ColdWake, th.Quiet))*time.Minute-quiet, outcome, true)
	}
	return later(stallAgainMax, outcome, false)
}

// recordQuiet keeps a report on the job's open spell, starting one.
func (s *Service) recordQuiet(ctx context.Context, userID, sessionID, project int64, j StalledJob) (*stallSpell, error) {
	now := s.now()
	quietFrom := now.Add(-time.Duration(j.Quiet) * time.Minute)
	started := j.StartedAt
	if started.IsZero() || started.After(quietFrom) {
		started = quietFrom
	}
	raw, _ := json.Marshal(j)
	var sp stallSpell
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		err := t.QueryRowContext(ctx, `SELECT id FROM stalled_jobs WHERE user_id = ? AND cli_session = ? AND task = ? AND settled_at IS NULL`,
			userID, j.Session, j.Task).Scan(&sp.ID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A lost spell of the same job (its runner went away a while)
			// is this one: its unread notice gives way to this spell's.
			if _, err := t.ExecContext(ctx, `UPDATE session_messages SET state = 'cancelled' WHERE state = 'queued' AND id IN
				(SELECT message_id FROM stalled_jobs WHERE user_id = ? AND cli_session = ? AND task = ? AND settled = 'lost' AND message_id IS NOT NULL)`,
				userID, j.Session, j.Task); err != nil {
				return err
			}
			res, err := t.ExecContext(ctx, `INSERT INTO stalled_jobs (user_id, session_id, project_id, cli_session, task, class, command, report, started_at, quiet_from, seen_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`, userID, sessionID, nullID(project), j.Session, j.Task, jobClass(j.Command), excerpt(j.Command, 300), string(raw),
				started.UnixMilli(), quietFrom.UnixMilli(), now.UnixMilli())
			if err != nil {
				return err
			}
			sp.ID, _ = res.LastInsertId()
		case err != nil:
			return err
		default:
			// The runner counts from its first look: a restarted one sees
			// less quiet than there was.
			_, err = t.ExecContext(ctx, `UPDATE stalled_jobs SET seen_at = ?, report = ?, session_id = ?, project_id = ?, quiet_from = min(quiet_from, ?) WHERE id = ?`,
				now.UnixMilli(), string(raw), sessionID, nullID(project), quietFrom.UnixMilli(), sp.ID)
			if err != nil {
				return err
			}
		}
		var from2, started2 int64
		var sess, proj sql.NullInt64
		if err := t.QueryRowContext(ctx, `SELECT session_id, coalesce(project_id, 0), quiet_from, started_at, told_at, told_how, message_id, class FROM stalled_jobs WHERE id = ?`, sp.ID).
			Scan(&sess, &proj, &from2, &started2, &sp.ToldAt, &sp.ToldHow, &sp.Message, &sp.Class); err != nil {
			return err
		}
		sp.Session, sp.Project = sess.Int64, proj.Int64
		sp.QuietFrom, sp.StartedAt = msTime(from2), msTime(started2)
		return nil
	})
	return &sp, err
}

// toldAgain: a spell whose session was told is reported again. A passive
// notice not yet read wakes a cold session once the job has made no
// progress for ColdWake.
func (s *Service) toldAgain(ctx context.Context, userID int64, sp *stallSpell, pol StallPolicy, quiet time.Duration,
	later func(time.Duration, string, bool) (StalledJobOutcome, error)) (StalledJobOutcome, error) {
	told := "its session was told already"
	if sp.ToldHow != stallPassive || pol.ColdWake <= 0 || !sp.Message.Valid {
		return later(stallAgainMax, told, false)
	}
	th := s.stallThreshold(ctx, userID, sp.Class, pol)
	wakeAt := time.Duration(max(pol.ColdWake, th.Quiet)) * time.Minute
	if quiet < wakeAt {
		return later(wakeAt-quiet, told+"; it waits for its next turn", true)
	}
	if !th.ColdWake {
		return later(stallAgainMax, told+"; not woken: "+th.ColdWhy, false)
	}
	if why := s.wakeHeld(ctx, sp.Session); why != "" {
		return later(stallAgainMax, told+"; not woken: "+why, false)
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE session_messages SET passive = 0 WHERE id = ? AND state = 'queued' AND passive = 1`, sp.Message.Int64)
	if err != nil {
		return StalledJobOutcome{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return later(stallAgainMax, told+" (it has read it)", false)
	}
	now := s.nowMS()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE stalled_jobs SET told_how = ?, woke_at = ? WHERE id = ?`, stallColdWake, now, sp.ID); err != nil {
		return StalledJobOutcome{}, err
	}
	s.postToSession(ctx, sp.Session, true)
	outcome := fmt.Sprintf("woke its session after %d minutes without progress (its prompt cache is cold)", int(quiet.Minutes()))
	if m, err := s.messageByID(ctx, sp.Message.Int64); err == nil && m.State == MessageQueued {
		outcome = "its session reads it at its next turn (its runner couldn't wake it)"
	}
	s.stalledLine(ctx, sp.Session, now, fmt.Sprintf("Still no progress after %d min; %s.", int(quiet.Minutes()), outcome), sp, false)
	return later(stallAgainMax, outcome, false)
}

// tellStalled tells a stalled job's session, once per spell, and says how.
func (s *Service) tellStalled(ctx context.Context, userID, id int64, sp *stallSpell, pol StallPolicy, th stallThresholds, j StalledJob, quiet time.Duration) (how, outcome string, err error) {
	warm, inTurn := s.cacheWarmNow(ctx, id)
	held := s.wakeHeld(ctx, id)
	how = stallPassive
	switch {
	case warm && inTurn:
		how = stallAtStop
	case warm && held == "":
		how = stallWoke
	case !warm && held == "" && pol.ColdWake > 0 && th.ColdWake && quiet >= time.Duration(max(pol.ColdWake, th.Quiet))*time.Minute:
		how = stallColdWake
	}
	j.Quiet = int(quiet.Minutes())
	m, err := s.storeMessage(ctx, id, stalledText(j), stallKey(id, sp.ID), messageFrom{}, how == stallPassive)
	if err != nil {
		return "", "", err
	}
	outcome = "its session reads it at its next turn (its prompt cache is cold, so it wasn't woken)"
	switch {
	case how == stallAtStop:
		outcome = "its session reads it when its turn ends"
	case how == stallPassive && warm:
		outcome = "its session reads it at its next turn (not woken: " + held + ")"
	case how == stallPassive && held != "":
		outcome = "its session reads it at its next turn (its prompt cache is cold; not woken: " + held + ")"
	case how == stallPassive && pol.ColdWake > 0 && !th.ColdWake:
		outcome = "its session reads it at its next turn (its prompt cache is cold; not woken: " + th.ColdWhy + ")"
	case how == stallPassive && pol.ColdWake > 0:
		outcome = fmt.Sprintf("its session reads it at its next turn (its prompt cache is cold, so it wasn't woken; it is if there's still no progress after %d min)", max(pol.ColdWake, th.Quiet))
	case how == stallPassive:
	case m.State == MessageQueued:
		// Its work waits on its job, so it hears now (WP-ADR6 blocking news),
		// though it stopped waiting on that job.
		s.postToSession(ctx, id, true)
		outcome = "its session reads it at its next turn (its prompt cache is warm, but its runner couldn't wake it)"
		if how == stallColdWake {
			outcome = "its session reads it at its next turn (its runner couldn't wake it)"
		}
		if m, err := s.messageByID(ctx, m.ID); err == nil && m.State != MessageQueued {
			outcome = "woke its session (its prompt cache is warm)"
			if how == stallColdWake {
				outcome = fmt.Sprintf("woke its session after %d minutes without progress (its prompt cache is cold)", j.Quiet)
			}
		}
	}
	now := s.nowMS()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE stalled_jobs SET told_at = ?, told_how = ?, told_quiet = ?, threshold = ?, warm = ?, message_id = ?, woke_at = CASE WHEN ? THEN ? END WHERE id = ?`,
		now, how, j.Quiet, th.Quiet, warm, m.ID, how == stallColdWake, now, sp.ID); err != nil {
		return "", "", err
	}
	line := fmt.Sprintf("No progress for %d min; %s. `%s`", j.Quiet, outcome, excerpt(j.Command, 120))
	if th.Why != "" {
		line += " (" + th.Why + ")"
	}
	if len(j.Evidence) > 0 {
		line += " " + j.Evidence[0]
	}
	s.stalledLine(ctx, id, now, line, sp, warm)
	return how, outcome, nil
}

// stallKey is the notice's step key: one per spell.
func stallKey(sessionID, spell int64) string { return fmt.Sprintf("stalled:%d:%d", sessionID, spell) }

// stalledLine writes a line on the session's timeline. The notice is
// stored: a failed line must not make the runner retry, so it is logged,
// not returned.
func (s *Service) stalledLine(ctx context.Context, id, at int64, line string, sp *stallSpell, warm bool) {
	var task string
	_ = s.db.R.QueryRowContext(ctx, `SELECT task FROM stalled_jobs WHERE id = ?`, sp.ID).Scan(&task)
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		x, err := s.sessionByID(ctx, t, SystemActor(), id)
		if err != nil {
			return err
		}
		return s.sessionTimeline(ctx, t, id, at, "stalled", "", 0, task, "", line, x.projectID,
			map[string]any{"task": task, "spell": sp.ID, "warm": warm})
	})
	if err != nil {
		s.log.WarnContext(ctx, "stalled job: timeline line not written", "session", id, "task", task, "err", err)
	}
	s.publishSession(ctx, id)
}

// wakeHeld says why the session isn't to be woken: its person paused
// agents, or typed /wp stop in it. Empty: it may be.
func (s *Service) wakeHeld(ctx context.Context, id int64) string {
	var user int64
	var agent string
	var left bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT user_id, agent, left_alone_at IS NOT NULL FROM agent_sessions WHERE id = ?`, id).Scan(&user, &agent, &left); err != nil {
		return ""
	}
	if left {
		return "its person stopped Waypoint in it (/wp stop)"
	}
	if p, err := s.sessionPause(ctx, user, agent); err == nil && p != nil {
		return "its person paused agents"
	}
	return ""
}

// settleStalled: a job the runner reported moved again or ended. A notice
// its session hasn't read yet is withdrawn.
func (s *Service) settleStalled(ctx context.Context, userID int64, j SettledJob) {
	var sp stallSpell
	var delivered sql.NullInt64
	var state sql.NullString
	err := s.db.R.QueryRowContext(ctx, `SELECT x.id, coalesce(x.session_id, 0), x.told_at, x.told_how, x.message_id, m.state, m.delivered_at
		FROM stalled_jobs x LEFT JOIN session_messages m ON m.id = x.message_id
		WHERE x.user_id = ? AND x.cli_session = ? AND x.task = ? AND x.settled_at IS NULL`, userID, j.Session, j.Task).
		Scan(&sp.ID, &sp.Session, &sp.ToldAt, &sp.ToldHow, &sp.Message, &state, &delivered)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "stalled job: settle", "task", j.Task, "err", err)
		}
		return
	}
	how := "ended"
	if j.How == "progress" {
		how = "progress"
	}
	now := s.nowMS()
	outcome := "self"
	withdrawn := false
	switch {
	case !sp.ToldAt.Valid:
	case !sp.Message.Valid || !state.Valid:
		outcome = "unknown"
	case state.String == MessageQueued:
		res, err := s.db.W.ExecContext(ctx, `UPDATE session_messages SET state = 'cancelled' WHERE id = ? AND state = 'queued'`, sp.Message.Int64)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				outcome, withdrawn = "withdrawn", true
			}
		}
	case delivered.Valid && how == "ended" && now-delivered.Int64 <= stallActedWithin.Milliseconds():
		outcome = "acted"
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE stalled_jobs SET settled_at = ?, settled = ?, outcome = ? WHERE id = ? AND settled_at IS NULL`, now, how, outcome, sp.ID); err != nil {
		s.log.WarnContext(ctx, "stalled job: settle", "task", j.Task, "err", err)
		return
	}
	if withdrawn && sp.Session != 0 {
		what := "moved again"
		if how == "ended" {
			what = "ended"
		}
		s.stalledLine(ctx, sp.Session, now, "Background job "+what+" before its session read the notice; withdrew it unsent.", &sp, false)
	}
}

// loseStalled settles the person's spells no runner has reported for a
// while: the runner restarted or went away, so how they ended is unknown.
func (s *Service) loseStalled(ctx context.Context, userID int64) {
	if _, err := s.db.W.ExecContext(ctx, `UPDATE stalled_jobs SET settled_at = seen_at, settled = 'lost', outcome = 'unknown' WHERE user_id = ? AND settled_at IS NULL AND seen_at < ?`,
		userID, s.now().Add(-stallLostAfter).UnixMilli()); err != nil {
		s.log.WarnContext(ctx, "stalled jobs: settle lost spells", "err", err)
	}
}

// cacheWarmNow: the session's prompt cache is likely warm: it is in a turn
// (a prompt cleared stopped_at), or its last Stop (a keep-warm ping's
// included) is within its cache tier, an hour on the 1h tier and 5 minutes
// otherwise. Not the stored state, which stays "working" after a Stop while
// a background task runs, nor last_seen_at, which the session's waiting
// `wp` commands move.
func (s *Service) cacheWarmNow(ctx context.Context, id int64) (warm, inTurn bool) {
	var ttl string
	var stopped sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT cache_ttl, stopped_at FROM agent_sessions WHERE id = ?`, id).Scan(&ttl, &stopped); err != nil {
		return false, false
	}
	if !stopped.Valid {
		return true, true
	}
	life := 5 * time.Minute
	if ttl == "1h" {
		life = cacheWarm
	}
	return s.now().Before(msTime(stopped.Int64).Add(life - cacheMargin)), false
}

// tellEarly tells a session, about to be kept warm, of a background job
// that has been quiet for its project's Early time but isn't stalled yet:
// the notice goes in place of the keep-warm ping, at no cost of its own,
// where waiting for the stall would find the cache cold. It says which job
// it told of, or "" when there is none.
func (s *Service) tellEarly(ctx context.Context, sessionID, userID int64) string {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, coalesce(project_id, 0), quiet_from, started_at, class, report FROM stalled_jobs
		WHERE session_id = ? AND settled_at IS NULL AND told_at IS NULL AND seen_at >= ? ORDER BY quiet_from`,
		sessionID, s.now().Add(-stallAgainSoon-time.Minute).UnixMilli())
	if err != nil {
		s.log.WarnContext(ctx, "stalled jobs: early notice", "session", sessionID, "err", err)
		return ""
	}
	type cand struct {
		sp     stallSpell
		report string
	}
	var list []cand
	for rows.Next() {
		var c cand
		var from, started int64
		if rows.Scan(&c.sp.ID, &c.sp.Project, &from, &started, &c.sp.Class, &c.report) == nil {
			c.sp.QuietFrom, c.sp.StartedAt, c.sp.Session = msTime(from), msTime(started), sessionID
			list = append(list, c)
		}
	}
	rows.Close()
	now := s.now()
	for _, c := range list {
		pol := s.stallPolicyFor(ctx, s.db.R, c.sp.Project)
		var j StalledJob
		if pol.Off || pol.Early <= 0 || json.Unmarshal([]byte(c.report), &j) != nil {
			continue
		}
		if j.Timeout > 0 && j.Timeout < pol.SkipTimeout {
			continue
		}
		th := s.stallThreshold(ctx, userID, c.sp.Class, pol)
		quiet := now.Sub(c.sp.QuietFrom)
		// Early stretches with the stall time the evidence sets for its kind.
		early := time.Duration(pol.Early) * time.Minute * time.Duration(th.Quiet) / time.Duration(max(pol.Quiet, 1))
		if quiet < early || now.Sub(c.sp.StartedAt) < time.Duration(pol.MinAge)*time.Minute {
			continue
		}
		j.Quiet = int(quiet.Minutes())
		m, err := s.storeMessage(ctx, sessionID, stalledText(j), stallKey(sessionID, c.sp.ID), messageFrom{}, false)
		if err != nil {
			s.log.WarnContext(ctx, "stalled jobs: early notice", "session", sessionID, "err", err)
			continue
		}
		at := s.nowMS()
		if _, err := s.db.W.ExecContext(ctx, `UPDATE stalled_jobs SET told_at = ?, told_how = ?, told_quiet = ?, threshold = ?, warm = 1, message_id = ? WHERE id = ?`,
			at, stallKeepWarm, j.Quiet, th.Quiet, m.ID, c.sp.ID); err != nil {
			s.log.WarnContext(ctx, "stalled jobs: early notice", "session", sessionID, "err", err)
		}
		s.stalledLine(ctx, sessionID, at, fmt.Sprintf("No progress for %d min; told it in place of its keep-warm ping, before its prompt cache cools. `%s`", j.Quiet, excerpt(j.Command, 120)), &c.sp, true)
		return j.Task
	}
	return ""
}

// stalledText is what the session is told.
func stalledText(j StalledJob) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Waypoint: your background job %s has made no progress for %d minutes. Waypoint only watches it from /proc; it hasn't touched it.\n", j.Task, j.Quiet)
	fmt.Fprintf(&b, "Command: `%s`\n", excerpt(j.Command, 300))
	for _, e := range j.Evidence {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	if len(j.Running) > 0 {
		fmt.Fprintf(&b, "Running under it: %s.\n", strings.Join(j.Running, "; "))
	}
	stop := fmt.Sprintf("`kill %d`", j.PID)
	if j.Group {
		stop = fmt.Sprintf("`kill -- -%d` (its whole process group)", j.PID)
	}
	fmt.Fprintf(&b, "If it is stuck, stop it with TaskStop %s. This was seen some time ago: only use %s if `ps -o args= -p %d` still shows the command above, as the PID may have been reused. Then run what it was for again, once. If it waits on purpose, leave it. It may be one of your subagents' jobs.", j.Task, stop, j.PID)
	return b.String()
}
