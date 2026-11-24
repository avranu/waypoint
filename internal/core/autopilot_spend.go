package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// The sweep's starts skip the schedule, the pace's spacing and the daily count, but not the hourly spend limit or the loop guard.

const (
	// DefaultSpendAtOnce: autopilot sessions at once, across the person's
	// projects, while it uses what's left.
	DefaultSpendAtOnce = 3
	// spendSpacing: the least time between two such starts. The status line
	// posts a reading every few minutes and quota_samples keeps one a quarter
	// hour when nothing changed: a start shows in the slope by then.
	spendSpacing = 15 * time.Minute
	// spendQuiet: no prompt of theirs in this long and no session of theirs
	// working, and the person isn't using Claude Code.
	spendQuiet = 30 * time.Minute
	// spendReserve: points of the week left for the person while they use
	// Claude Code, outside the last hour.
	spendReserve = 10.0
	// spendLastHour: the end of the week, when it aims for 100% whatever the
	// person does.
	spendLastHour = time.Hour
	// spendSlopeMin: readings closer together than this give no slope.
	spendSlopeMin = 15 * time.Minute
)

// SpendPlan is the week's-end plan as the Autopilot tab shows it.
type SpendPlan struct {
	On       bool       `json:"on"`
	InWindow bool       `json:"in_window"`
	OpensAt  *time.Time `json:"opens_at,omitempty"` // when the week's end begins, before it does
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// The projection: the week's use now, its slope (points an hour; nil:
	// too few readings) and where it lands at the reset at that slope.
	Percent   *float64 `json:"percent,omitempty"`
	PerHour   *float64 `json:"per_hour,omitempty"`
	Projected *float64 `json:"projected,omitempty"`
	Target    float64  `json:"target,omitempty"`
	// PersonActive: the person is using Claude Code (spendQuiet).
	PersonActive bool `json:"person_active,omitempty"`
	Running      int  `json:"running"`
	Cap          int  `json:"cap"`
	// Wants: the projection falls short of the target, so a finished
	// session may go on to another task past at_once. Start: and the next
	// sweep may start one more session.
	Wants bool   `json:"wants"`
	Start bool   `json:"start"`
	Why   string `json:"why"`
	// Started: the sessions it started this week's end, newest first.
	Started []*SpendStart `json:"started"`
}

// SpendStart is one session the plan started.
type SpendStart struct {
	Step      int64     `json:"step"`
	Project   string    `json:"project"`
	Task      *IssueRef `json:"task,omitempty"`
	State     string    `json:"state"`
	Session   int64     `json:"session,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// weekReading is the latest seven_day reading of a week that hasn't reset,
// and the use's slope before it.
type weekReading struct {
	at, reset time.Time
	pct       float64
	perHour   *float64
}

func (s *Service) weekReading(ctx context.Context, uid int64, now time.Time) (*weekReading, error) {
	var at, reset int64
	var pct float64
	err := s.db.R.QueryRowContext(ctx, `SELECT at, percent, resets_at FROM quota_samples WHERE user_id = ? AND span = 'seven_day' ORDER BY at DESC LIMIT 1`, uid).Scan(&at, &pct, &reset)
	if errors.Is(err, sql.ErrNoRows) || err == nil && reset <= now.UnixMilli() {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	r := &weekReading{at: msTime(at), reset: msTime(reset), pct: pct}
	if now.Sub(r.at) > time.Hour {
		// No reading in the last hour means no session is at work, so use is taken as flat.
		r.perHour = ptrOf(0.0)
		return r, nil
	}
	// The slope takes the hour's own readings first: a reading from before an idle gap would spread a fresh burst over the gap.
	var baseAt int64
	var basePct float64
	const sameWeek = `user_id = ? AND span = 'seven_day' AND resets_at BETWEEN ? AND ?`
	lo, hi := reset-time.Hour.Milliseconds(), reset+time.Hour.Milliseconds()
	err = s.db.R.QueryRowContext(ctx, `SELECT at, percent FROM quota_samples WHERE `+sameWeek+` AND at BETWEEN ? AND ? ORDER BY at LIMIT 1`,
		uid, lo, hi, at-time.Hour.Milliseconds(), at-spendSlopeMin.Milliseconds()).Scan(&baseAt, &basePct)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.R.QueryRowContext(ctx, `SELECT at, percent FROM quota_samples WHERE `+sameWeek+` AND at <= ? ORDER BY at DESC LIMIT 1`,
			uid, lo, hi, at-time.Hour.Milliseconds()).Scan(&baseAt, &basePct)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.R.QueryRowContext(ctx, `SELECT at, percent FROM quota_samples WHERE `+sameWeek+` AND at < ? ORDER BY at LIMIT 1`,
			uid, lo, hi, at).Scan(&baseAt, &basePct)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	} else if err != nil {
		return nil, err
	}
	if span := r.at.Sub(msTime(baseAt)); span >= spendSlopeMin {
		r.perHour = ptrOf(max(0, (pct-basePct)/span.Hours()))
	}
	return r, nil
}

// personUsingClaude: a prompt of the person's in any session in the last
// spendQuiet, or a session they have typed into still at work.
func (s *Service) personUsingClaude(ctx context.Context, uid int64, now time.Time) (bool, error) {
	var on bool
	err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_sessions WHERE user_id = ? AND typed_at >= ?)
		OR EXISTS (SELECT 1 FROM agent_sessions WHERE user_id = ? AND typed_at IS NOT NULL AND ended_at IS NULL AND state = ? AND last_seen_at >= ?)`,
		uid, now.Add(-spendQuiet).UnixMilli(), uid, SessionWorking, now.Add(-sessionIdleAfter).UnixMilli()).Scan(&on)
	return on, err
}

// autopilotRunningAll counts the sessions autopilot has running across all
// the person's projects (autopilotRunning, waiting ones included).
func (s *Service) autopilotRunningAll(ctx context.Context, q queryer, uid, now int64) (int, error) {
	pids, err := queryIDs(ctx, q, `SELECT DISTINCT project_id FROM autopilot_steps WHERE user_id = ? AND project_id IS NOT NULL
		AND (state IN (?, ?, ?, ?) OR updated_at > ?)`, uid, StepDue, StepWaiting, StepStarting, StepStarted, now-7*24*time.Hour.Milliseconds())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, pid := range pids {
		r, err := s.autopilotRunning(ctx, q, uid, pid, 0, true, now)
		if err != nil {
			return 0, err
		}
		n += len(r)
	}
	return n, nil
}

// lastSpendStart is when the person's last start to use what's left was
// made (0: none).
func lastSpendStart(ctx context.Context, q queryer, uid int64) (int64, error) {
	var at sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT max(created_at) FROM autopilot_steps WHERE user_id = ? AND spend = 1`, uid).Scan(&at)
	return at.Int64, err
}

// spendLimits are the person's own settings over Waypoint's: the switch
// and its cap are for all their projects at once.
func (s *Service) spendLimits(ctx context.Context, q queryer, uid int64) (AutopilotLimits, error) {
	rows, err := s.autopilotRows(ctx, q, uid)
	if err != nil {
		return AutopilotLimits{}, err
	}
	return waypointAutopilotDefaults().over(rows[0].limits), nil
}

// spendPlan is the week's-end plan for the person now. lim is their own
// settings (spendLimits), m what the limits read.
func (s *Service) spendPlan(ctx context.Context, uid int64, lim AutopilotLimits, m *AutopilotMeter, now time.Time) (*SpendPlan, error) {
	p := &SpendPlan{On: valueOf(lim.SpendLeft), Cap: valueOf(lim.SpendAtOnce), Started: []*SpendStart{}}
	if !p.On {
		return p, nil
	}
	end := valueOf(lim.WeekEndHours)
	if end <= 0 {
		p.Why = "the week's end is set to 0 hours, so it starts nothing"
		return p, nil
	}
	r, err := s.weekReading(ctx, uid, now)
	if err != nil {
		return nil, err
	}
	if r == nil {
		p.Why = "no quota reading for this week yet: Claude Code's status line posts one while a session runs"
		return p, nil
	}
	p.ResetsAt = ptrOf(r.reset)
	opens := r.reset.Add(-time.Duration(end * float64(time.Hour)))
	user, err := s.userByID(ctx, s.db.R, uid)
	if err != nil {
		return nil, err
	}
	loc := s.actorLocation(&Actor{User: user})
	if now.Before(opens) {
		p.OpensAt = ptrOf(opens)
		p.Why = fmt.Sprintf("it starts in the week's last %s, %s", hoursWords(end), whenWords(opens.In(loc), now.In(loc)))
		return p, nil
	}
	p.InWindow = true
	if p.Started, err = s.spendStarted(ctx, uid, opens.UnixMilli()); err != nil {
		return nil, err
	}
	left := r.reset.Sub(now)
	proj := r.pct + valueOf(r.perHour)*r.reset.Sub(r.at).Hours()
	p.Percent, p.PerHour, p.Projected = ptrOf(r.pct), r.perHour, ptrOf(math.Round(proj*10)/10)
	if p.PersonActive, err = s.personUsingClaude(ctx, uid, now); err != nil {
		return nil, err
	}
	lastHour := left <= spendLastHour
	p.Target = 100
	if p.PersonActive && !lastHour {
		p.Target = 100 - spendReserve
	}
	if p.Running, err = s.autopilotRunningAll(ctx, s.db.R, uid, now.UnixMilli()); err != nil {
		return nil, err
	}
	last, err := lastSpendStart(ctx, s.db.R, uid)
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("%s left, %.0f%% used, on pace for %.0f%%: %s running", hoursWords(left.Hours()), r.pct, proj, plural1(p.Running, "autopilot session"))
	fiveMax := valueOf(lim.FiveHourMax)
	fiveHeld := p.PersonActive && !lastHour && fiveMax > 0 && m != nil && m.FiveHour != nil && *m.FiveHour >= fiveMax
	p.Wants = proj < p.Target && !fiveHeld
	switch {
	case proj >= p.Target && p.PersonActive && !lastHour:
		p.Why = fmt.Sprintf("%s; you're using Claude Code, so it leaves you %.0f points and starts nothing more", head, spendReserve)
	case proj >= p.Target:
		p.Why = head + "; enough to use it up, so it starts nothing more"
	case fiveHeld:
		p.Why = fmt.Sprintf("%s; you're using Claude Code and the five-hour window is at %.0f%%, so it waits", head, *m.FiveHour)
	case p.Running >= p.Cap:
		p.Why = fmt.Sprintf("%s; that's the most it runs at once (%d)", head, p.Cap)
	case last > 0 && now.UnixMilli()-last < spendSpacing.Milliseconds():
		p.Why = fmt.Sprintf("%s; it starts another %s, once the last one shows in the readings", head, whenWords(msTime(last).Add(spendSpacing).In(loc), now.In(loc)))
	default:
		p.Start = true
		p.Why = head + "; it starts one more when a task is ready"
		if p.PersonActive && !lastHour {
			p.Why += fmt.Sprintf(", leaving you %.0f points", spendReserve)
		}
	}
	return p, nil
}

// spendStarted lists the sessions started to use what's left since since.
func (s *Service) spendStarted(ctx context.Context, uid, since int64) ([]*SpendStart, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT st.id, p.key, st.state, coalesce(st.next_session, 0), st.created_at,
		coalesce(i.id, 0), coalesce(p.key || '-' || i.number, ''), coalesce(i.title, ''), coalesce(i.type, '')
		FROM autopilot_steps st JOIN projects p ON p.id = st.project_id LEFT JOIN issues i ON i.id = st.next_issue
		WHERE st.user_id = ? AND st.spend = 1 AND st.created_at >= ? ORDER BY st.created_at DESC, st.id DESC LIMIT 50`, uid, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SpendStart{}
	for rows.Next() {
		var x SpendStart
		var at int64
		var task IssueRef
		if err := rows.Scan(&x.Step, &x.Project, &x.State, &x.Session, &at, &task.ID, &task.Key, &task.Title, &task.Type); err != nil {
			return nil, err
		}
		x.StartedAt = msTime(at)
		if task.ID != 0 {
			x.Task = &task
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

// spendRoom re-checks, in the claim's transaction, that a start to use
// what's left still fits: under the cap across the person's projects, and
// spaced from the last one.
func (s *Service) spendRoom(ctx context.Context, t *tx, uid int64, cap int, now int64) (bool, error) {
	last, err := lastSpendStart(ctx, t, uid)
	if err != nil || last > 0 && now-last < spendSpacing.Milliseconds() {
		return false, err
	}
	n, err := s.autopilotRunningAll(ctx, t, uid, now)
	return n < cap, err
}

// hoursWords: "12 h", "40 min".
func hoursWords(h float64) string {
	if h < 1 {
		return fmt.Sprintf("%.0f min", math.Max(0, h*60))
	}
	return fmt.Sprintf("%.0f h", h)
}
