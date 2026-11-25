package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// Live guard events; each reaches only its user.
const (
	EvQuotaGuardHold = "usage.quota_guard_hold"
	EvQuotaGuardStop = "usage.quota_guard_stop"
)

// The guard's rules, as trips and holds name them.
const (
	GuardRuleChurn   = "churn"
	GuardRuleSession = "session"
	GuardRuleHours   = "hours"
	GuardRuleModel   = "model"
	GuardRuleIdle    = "idle"
	GuardRuleHold    = "hold"
	GuardRuleBurst   = "burst"
	GuardRulePace    = "pace"
	GuardRuleTrips   = "trips"
)

const (
	guardBaselineDays  = 28
	guardBaselineEvery = 24 * time.Hour
	guardPulseKeep     = 2 * 24 * time.Hour
	guardTripKeep      = 90 * 24 * time.Hour
	// guardTripsHold: this many sessions stopped within guardTripsSpan hold
	// automation back for guardTripsFor.
	guardTripsHold = 3
	guardTripsSpan = 6 * time.Hour
	guardTripsFor  = 2 * time.Hour
	guardBurstFor  = time.Hour
	// A pace hold lasts this long; the week rising past the line again while
	// the limits hold autopilot back starts another.
	guardPaceFor = 15 * time.Minute
	// guardPaceSpend: automation must still be spending (dollars since the
	// over phase began) for a pace hold; the rise is shared out by spend,
	// so the person's own use doesn't hold automation back.
	guardPaceSpend = 1.0
	// guardLiftQuiet: after a person lifts a hold, no new one for this long
	// unless they say otherwise.
	guardLiftQuiet = 2 * time.Hour
)

// QuotaGuardSettings is what the person set; a nil field takes the default
// from their usage, and 0 turns that rule off.
type QuotaGuardSettings struct {
	Off        bool     `json:"off,omitempty"`         // the live guard does nothing
	ChurnUSD   *float64 `json:"churn_usd,omitempty"`   // spend with no progress
	SessionUSD *float64 `json:"session_usd,omitempty"` // spend since automation gave a session its work
	Hours      *float64 `json:"hours,omitempty"`       // hours automation has driven a session
	IdleHours  *float64 `json:"idle_hours,omitempty"`  // a person's session nobody typed in for this long
	ModelUSD   *float64 `json:"model_usd,omitempty"`   // spend on a model pricier than the usual ones
	Models     []string `json:"models,omitempty"`      // the models automation may use (empty: the usual ones)
	BurstUSD   *float64 `json:"burst_usd,omitempty"`   // automation's spend in an hour
	Pace       *bool    `json:"pace,omitempty"`        // hold while the week's limits hold autopilot back
}

// GuardPolicy is the lines in force: the settings over the defaults.
type GuardPolicy struct {
	ChurnUSD   float64  `json:"churn_usd"`
	SessionUSD float64  `json:"session_usd"`
	Hours      float64  `json:"hours"`
	IdleHours  float64  `json:"idle_hours"`
	ModelUSD   float64  `json:"model_usd"`
	Models     []string `json:"models,omitempty"`
	BurstUSD   float64  `json:"burst_usd"`
	Pace       bool     `json:"pace"`
	// From says where each line came from: set (by the person), usual (the
	// baseline) or default (no baseline yet).
	From map[string]string `json:"from"`
}

// GuardHold holds automation back: no autopilot starts, and running
// automation sessions wind down.
type GuardHold struct {
	Rule   string    `json:"rule"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
}

// GuardState is what the session hooks keep a copy of.
type GuardState struct {
	Off    bool         `json:"off,omitempty"`
	Policy *GuardPolicy `json:"policy,omitempty"`
	Hold   *GuardHold   `json:"hold,omitempty"`
}

// GuardBaseline is automation's usual spend, from the last 28 days.
type GuardBaseline struct {
	At         time.Time          `json:"at"`
	Days       int                `json:"days"`
	Sessions   int                `json:"sessions"`    // sessions automation drove
	SessionP50 float64            `json:"session_p50"` // their cost, at list price
	SessionP90 float64            `json:"session_p90"`
	Hours      int                `json:"hours"`    // hours automation spent anything in
	HourP95    float64            `json:"hour_p95"` // its spend in such an hour
	HourMax    float64            `json:"hour_max"`
	Models     map[string]float64 `json:"models"` // share of automation's session cost, by model
}

// guardWatch is the sweep's state for one person.
type guardWatch struct {
	Hold       *GuardHold `json:"hold,omitempty"`
	QuietUntil *time.Time `json:"quiet_until,omitempty"`
	OverSince  *time.Time `json:"over_since,omitempty"`
	OverFrom   float64    `json:"over_from,omitempty"`
	// TripsFrom: stops before it led to a trips hold or a lift already,
	// so they don't count again.
	TripsFrom *time.Time     `json:"trips_from,omitempty"`
	Baseline  *GuardBaseline `json:"baseline,omitempty"`
}

// The defaults, before a baseline and as its floors.
const (
	guardChurnFloor   = 20.0
	guardSessionFloor = 60.0
	guardHoursDefault = 12.0
	guardIdleDefault  = 8.0
	guardModelDefault = 3.0
	guardBurstFloor   = 60.0
	// guardModelShare: a model is one automation usually uses from this
	// share of its session cost.
	guardModelShare = 0.05
	// guardBaselineMin: fewer automation sessions or hours than this make
	// no baseline; the defaults stand.
	guardBaselineMin = 5
)

// guardPolicy is the lines in force for settings set over baseline b.
func guardPolicy(set QuotaGuardSettings, b *GuardBaseline) *GuardPolicy {
	p := &GuardPolicy{From: map[string]string{}}
	usualSessions := b != nil && b.Sessions >= guardBaselineMin
	usualHours := b != nil && b.Hours >= guardBaselineMin
	pick := func(name string, set *float64, usual bool, usualV, def float64) float64 {
		switch {
		case set != nil:
			p.From[name] = "set"
			return max(0, *set)
		case usual && usualV > def:
			p.From[name] = "usual"
			return math.Round(usualV)
		}
		p.From[name] = "default"
		return def
	}
	var p50, p90, h95 float64
	if b != nil {
		p50, p90, h95 = b.SessionP50, b.SessionP90, b.HourP95
	}
	p.ChurnUSD = pick("churn_usd", set.ChurnUSD, usualSessions, max(p50, p90/2), guardChurnFloor)
	p.SessionUSD = pick("session_usd", set.SessionUSD, usualSessions, 3*p90, guardSessionFloor)
	p.Hours = pick("hours", set.Hours, false, 0, guardHoursDefault)
	p.IdleHours = pick("idle_hours", set.IdleHours, false, 0, guardIdleDefault)
	p.ModelUSD = pick("model_usd", set.ModelUSD, false, 0, guardModelDefault)
	p.BurstUSD = pick("burst_usd", set.BurstUSD, usualHours, 3*h95, guardBurstFloor)
	p.Pace = set.Pace == nil || *set.Pace
	switch {
	case len(set.Models) > 0:
		p.Models, p.From["models"] = slices.Clone(set.Models), "set"
	case b != nil && len(b.Models) > 0:
		for m, share := range b.Models {
			if share >= guardModelShare {
				p.Models = append(p.Models, m)
			}
		}
		sort.Strings(p.Models)
		p.From["models"] = "usual"
	default:
		p.From["models"] = "default"
	}
	return p
}

// guardSettings is what the person set.
func (s *Service) guardSettings(ctx context.Context, q queryer, uid int64) (QuotaGuardSettings, error) {
	var set QuotaGuardSettings
	var raw string
	err := q.QueryRowContext(ctx, `SELECT settings FROM quota_guard_settings WHERE user_id = ?`, uid).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return set, nil
	}
	if err != nil {
		return set, err
	}
	_ = json.Unmarshal([]byte(raw), &set)
	return set, nil
}

func (s *Service) loadGuardWatch(ctx context.Context, q queryer, uid int64) (*guardWatch, error) {
	w := &guardWatch{}
	var raw string
	err := q.QueryRowContext(ctx, `SELECT state FROM quota_guard_state WHERE user_id = ?`, uid).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(raw), w)
	return w, nil
}

func saveGuardWatch(ctx context.Context, t *tx, uid int64, w *guardWatch) error {
	raw, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `INSERT INTO quota_guard_state(user_id, state) VALUES (?, ?) ON CONFLICT(user_id) DO UPDATE SET state = excluded.state`, uid, string(raw))
	return err
}

// activeHold is w's hold, if it still holds at now.
func (w *guardWatch) activeHold(now time.Time) *GuardHold {
	if w == nil || w.Hold == nil || !w.Hold.Until.After(now) {
		return nil
	}
	return w.Hold
}

// guardState is what uid's session hooks are sent; nil when it can't be
// read, which leaves their copy as it is.
func (s *Service) guardState(ctx context.Context, uid int64) *GuardState {
	set, err := s.guardSettings(ctx, s.db.R, uid)
	if err != nil {
		return nil
	}
	if set.Off {
		return &GuardState{Off: true}
	}
	w, err := s.loadGuardWatch(ctx, s.db.R, uid)
	if err != nil {
		return nil
	}
	return &GuardState{Policy: guardPolicy(set, w.Baseline), Hold: w.activeHold(s.now())}
}

// guardHoldFor is the hold that keeps uid's autopilot from starting
// anything now, if any (autopilotMeter).
func (s *Service) guardHoldFor(ctx context.Context, uid int64) *GuardHold {
	set, err := s.guardSettings(ctx, s.db.R, uid)
	if err != nil || set.Off {
		return nil
	}
	w, err := s.loadGuardWatch(ctx, s.db.R, uid)
	if err != nil {
		return nil
	}
	return w.activeHold(s.now())
}

// SessionPulse is one session's spend as its hooks last counted it.
type SessionPulse struct {
	Session string  `json:"session"` // the CLI's session id
	Cost    float64 `json:"cost"`    // the session tree's list-price total
	Auto    bool    `json:"auto"`    // automation drives it
	Model   string  `json:"model,omitempty"`
}

// GuardTripInput is a session's hooks telling or stopping it.
type GuardTripInput struct {
	Session string    `json:"session"`
	Rule    string    `json:"rule"`
	Action  string    `json:"action"` // told or stopped
	Cost    float64   `json:"cost"`
	Detail  string    `json:"detail"`
	At      time.Time `json:"at"`
}

// PulseInput is what a machine's hooks send at most every 30 seconds.
type PulseInput struct {
	Machine  string           `json:"machine"`
	Sessions []SessionPulse   `json:"sessions"`
	Trips    []GuardTripInput `json:"trips"`
}

// PulseResult is the pause and the guard, for the hooks' local copy.
type PulseResult struct {
	Pause *AgentPause `json:"pause"`
	Guard *GuardState `json:"guard"`
}

// SessionPulse records a machine's sessions' spend and the guard's trips,
// and answers with the pause and the guard.
func (s *Service) SessionPulse(ctx context.Context, actor *Actor, in PulseInput) (*PulseResult, error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return nil, ErrUnauthorized
	}
	machine := cmp(strings.TrimSpace(in.Machine), "unknown")
	if len(machine) > 200 || len(in.Sessions) > 200 || len(in.Trips) > 50 {
		return nil, invalid("too many sessions or trips in one pulse")
	}
	uid := actor.id()
	now := s.nowMS()
	var stops []*Event
	if len(in.Sessions) > 0 || len(in.Trips) > 0 {
		err := s.write(ctx, actor, func(t *tx) error {
			for _, p := range in.Sessions {
				if p.Session == "" || len(p.Session) > 200 || p.Cost < 0 || math.IsNaN(p.Cost) || math.IsInf(p.Cost, 0) {
					continue
				}
				if _, err := t.ExecContext(ctx, `INSERT INTO session_pulses(user_id, machine, cli_id, at, cost, auto, model) VALUES (?,?,?,?,?,?,?)`,
					uid, machine, p.Session, now, p.Cost, boolInt(p.Auto), excerpt(p.Model, 100)); err != nil {
					return err
				}
			}
			for _, tr := range in.Trips {
				ev, err := s.guardTrip(ctx, t, actor, machine, tr)
				if err != nil {
					return err
				}
				if ev != nil {
					stops = append(stops, ev)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	res := &PulseResult{Guard: s.guardState(ctx, uid)}
	var err error
	if res.Pause, err = s.sessionPause(ctx, uid, pausedAgent(actor)); err != nil {
		return nil, err
	}
	return res, nil
}

// guardTrip records one trip. A stop leaves the session alone (as /wp
// stop: autopilot carries it on no more) and keeps the tasks it held from
// being handed out again at once (a release's back-off); the person is told.
func (s *Service) guardTrip(ctx context.Context, t *tx, actor *Actor, machine string, in GuardTripInput) (*Event, error) {
	if in.Session == "" || len(in.Session) > 200 {
		return nil, nil
	}
	switch in.Action {
	case "told", "stopped":
	default:
		return nil, nil
	}
	switch in.Rule {
	case GuardRuleChurn, GuardRuleSession, GuardRuleHours, GuardRuleModel, GuardRuleIdle, GuardRuleHold:
	default:
		return nil, nil
	}
	uid := actor.id()
	at := s.nowMS()
	if !in.At.IsZero() && in.At.UnixMilli() < at && at-in.At.UnixMilli() < 24*time.Hour.Milliseconds() {
		at = in.At.UnixMilli()
	}
	detail := excerpt(strings.TrimSpace(in.Detail), 1000)
	var sid sql.NullInt64
	var kind, title string
	var project sql.NullInt64
	err := t.QueryRowContext(ctx, `SELECT id, kind, title, project_id FROM agent_sessions WHERE user_id = ? AND machine = ? AND cli_id = ?`, uid, machine, in.Session).
		Scan(&sid, &kind, &title, &project)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO quota_guard_trips(user_id, session_id, machine, cli_id, rule, action, cost, detail, at) VALUES (?,?,?,?,?,?,?,?,?)`,
		uid, sid, machine, in.Session, in.Rule, in.Action, math.Round(in.Cost*100)/100, detail, at); err != nil {
		return nil, err
	}
	if !sid.Valid {
		return nil, nil
	}
	label := "quota guard: told"
	if in.Action == "stopped" {
		label = "quota guard: stopped"
	}
	if err := s.sessionTimeline(ctx, t, sid.Int64, at, "guard", "", 0, label, "", detail, project, map[string]any{"rule": in.Rule, "cost": in.Cost}); err != nil {
		return nil, err
	}
	if in.Action != "stopped" {
		return nil, nil
	}
	if kind != SessionTask {
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET left_alone_at = coalesce(left_alone_at, ?) WHERE id = ?`, at, sid.Int64); err != nil {
			return nil, err
		}
		if err := s.stopStep(ctx, t, sid.Int64, "guard", "the quota guard stopped the session"); err != nil {
			return nil, err
		}
	}
	keys, err := s.guardBackOff(ctx, t, sid.Int64)
	if err != nil {
		return nil, err
	}
	ev := &Event{Kind: EvQuotaGuardStop, UserID: uid, New: detail,
		Data: map[string]any{"session": sid.Int64, "rule": in.Rule, "cost": in.Cost, "title": title, "tasks": strings.Join(keys, ", ")}}
	t.emit(ev)
	return ev, nil
}

// guardBackOff keeps the tasks session sid was working on from being
// handed out again for a release's wait, so a task that churned doesn't
// start over at once; their keys.
func (s *Service) guardBackOff(ctx context.Context, t *tx, sid int64) ([]string, error) {
	rows, err := t.QueryContext(ctx, `SELECT r.id, r.issue_id, r.agent, p.key || '-' || i.number FROM agent_runs r JOIN issues i ON i.id = r.issue_id JOIN projects p ON p.id = i.project_id
		WHERE r.session_id = ? AND r.ended_at IS NULL`, sid)
	if err != nil {
		return nil, err
	}
	type run struct {
		id, issue  int64
		agent, key string
	}
	var runs []run
	for rows.Next() {
		var r run
		if err := rows.Scan(&r.id, &r.issue, &r.agent, &r.key); err != nil {
			_ = rows.Close()
			return nil, err
		}
		runs = append(runs, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var keys []string
	for _, r := range runs {
		wait, _, err := s.nextReleaseWait(ctx, t, r.issue)
		if err != nil {
			return nil, err
		}
		until := s.nowMS() + wait.Milliseconds()
		if _, err := t.ExecContext(ctx, `INSERT INTO task_releases(issue_id, agent, session_id, run_id, released_at, until_at) VALUES (?,?,?,?,?,?)`,
			r.issue, r.agent, sid, r.id, s.nowMS(), until); err != nil {
			return nil, err
		}
		keys = append(keys, r.key)
	}
	return keys, nil
}

// RunGuardWatch is the minute sweep: for each person whose automation
// spent anything in the last hour, or who is held, the hour's spend and the
// week's pace.
func (s *Service) RunGuardWatch(ctx context.Context) {
	now := s.now()
	hourAgo := now.Add(-time.Hour).UnixMilli()
	rows, err := s.db.R.QueryContext(ctx, `SELECT user_id FROM session_pulses WHERE at >= ? AND auto = 1
		UNION SELECT owner_id FROM runner_jobs WHERE cli = 'claude' AND finished_at >= ? AND owner_id IS NOT NULL
		UNION SELECT user_id FROM quota_guard_trips WHERE at >= ? AND action = 'stopped'
		UNION SELECT user_id FROM quota_guard_state WHERE json_extract(state, '$.hold') IS NOT NULL OR json_extract(state, '$.over_since') IS NOT NULL`,
		hourAgo, hourAgo, now.Add(-guardTripsSpan).UnixMilli())
	if err != nil {
		s.log.WarnContext(ctx, "quota guard watch", "err", err)
		return
	}
	var uids []int64
	for rows.Next() {
		var uid int64
		if rows.Scan(&uid) == nil {
			uids = append(uids, uid)
		}
	}
	_ = rows.Close()
	for _, uid := range uids {
		if ctx.Err() != nil {
			return
		}
		if err := s.guardWatchUser(ctx, uid, now); err != nil {
			s.log.WarnContext(ctx, "quota guard watch", "user", uid, "err", err)
		}
	}
}

// guardWatchUser judges one person's last hour and the week's pace now.
func (s *Service) guardWatchUser(ctx context.Context, uid int64, now time.Time) error {
	set, err := s.guardSettings(ctx, s.db.R, uid)
	if err != nil {
		return err
	}
	w, err := s.loadGuardWatch(ctx, s.db.R, uid)
	if err != nil {
		return err
	}
	before, _ := json.Marshal(w)
	var hold *GuardHold
	if set.Off {
		w.Hold, w.OverSince = nil, nil
	} else {
		pol := guardPolicy(set, w.Baseline)
		if hold, err = s.guardJudge(ctx, uid, now, w, pol); err != nil {
			return err
		}
	}
	if w.Hold != nil && !w.Hold.Until.After(now) {
		w.Hold = nil
	}
	held := w.Hold != nil
	if hold != nil {
		if held && w.Hold.Rule == hold.Rule {
			hold.Since = w.Hold.Since
			hold.Until = maxTime(hold.Until, w.Hold.Until)
		}
		if !held || hold.Until.After(w.Hold.Until) || w.Hold.Rule != hold.Rule {
			w.Hold = hold
		}
	}
	after, _ := json.Marshal(w)
	if string(before) == string(after) {
		return nil
	}
	return s.write(ctx, nil, func(t *tx) error {
		cur, err := s.loadGuardWatch(ctx, t, uid)
		if err != nil {
			return err
		}
		if got, _ := json.Marshal(cur); string(got) != string(before) {
			return nil
		}
		if err := saveGuardWatch(ctx, t, uid, w); err != nil {
			return err
		}
		if hold != nil && !held {
			if _, err := t.ExecContext(ctx, `INSERT INTO quota_guard_trips(user_id, rule, action, detail, at) VALUES (?,?,?,?,?)`,
				uid, hold.Rule, "held", hold.Reason, now.UnixMilli()); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvQuotaGuardHold, UserID: uid, New: hold.Reason,
				Data: map[string]any{"rule": hold.Rule, "until": hold.Until}})
		}
		return nil
	})
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// guardJudge is the hold the sweep's rules call for now, if any; it keeps
// w's record of the over phase.
func (s *Service) guardJudge(ctx context.Context, uid int64, now time.Time, w *guardWatch, pol *GuardPolicy) (*GuardHold, error) {
	quiet := w.QuietUntil != nil && w.QuietUntil.After(now)
	if !quiet {
		w.QuietUntil = nil
	}
	var hold *GuardHold
	if pol.BurstUSD > 0 {
		spent, err := s.automationSpend(ctx, uid, now.Add(-time.Hour), now)
		if err != nil {
			return nil, err
		}
		if spent > pol.BurstUSD && !quiet {
			hold = &GuardHold{Rule: GuardRuleBurst, Since: now, Until: now.Add(guardBurstFor),
				Reason: fmt.Sprintf("automation spent $%.0f in the last hour, over the guard's $%.0f line for an hour (%s)", spent, pol.BurstUSD, guardLineFrom(pol.From["burst_usd"], "three times its usual busy hour"))}
		}
	}
	m, err := s.autopilotMeter(ctx, uid)
	if err != nil {
		return nil, err
	}
	lim, _, offUntil, err := s.quotaGuardLimits(ctx, uid)
	if err != nil {
		return nil, err
	}
	why := ""
	if offUntil <= now.UnixMilli() && m.WeekPercent != nil {
		why, _ = quotaPaused(m, lim, now)
	}
	if why == "" || !pol.Pace {
		w.OverSince, w.OverFrom = nil, 0
	} else {
		if w.OverSince == nil || *m.WeekPercent < w.OverFrom {
			w.OverSince, w.OverFrom = ptrOf(now), *m.WeekPercent
		}
		rose := *m.WeekPercent - w.OverFrom
		spent, all, err := s.guardSpend(ctx, uid, *w.OverSince, now)
		if err != nil {
			return nil, err
		}
		points := rose
		if all > spent {
			points = rose * spent / all
		}
		if points >= quotaGuardOverMax && spent >= guardPaceSpend && !quiet && hold == nil {
			hold = &GuardHold{Rule: GuardRulePace, Since: now, Until: now.Add(guardPaceFor),
				Reason: fmt.Sprintf("the week's quota rose %.0f points since %s while your autopilot limits held automation back (%s); automation spent $%.0f of the $%.0f spent, so about %.0f of those points",
					rose, w.OverSince.UTC().Format("Jan 2 15:04 UTC"), why, spent, all, points)}
			w.OverSince, w.OverFrom = ptrOf(now), *m.WeekPercent
		}
	}
	if hold == nil && !quiet {
		since := now.Add(-guardTripsSpan)
		if w.TripsFrom != nil {
			since = maxTime(since, w.TripsFrom.Add(time.Millisecond))
		}
		var n int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(DISTINCT cli_id) FROM quota_guard_trips WHERE user_id = ? AND action = 'stopped' AND rule NOT IN (?,?) AND at >= ?`,
			uid, GuardRuleIdle, GuardRuleHold, since.UnixMilli()).Scan(&n); err != nil {
			return nil, err
		}
		if n >= guardTripsHold && (w.Hold == nil || w.Hold.Rule != GuardRuleTrips) {
			hold = &GuardHold{Rule: GuardRuleTrips, Since: now, Until: now.Add(guardTripsFor),
				Reason: fmt.Sprintf("the guard stopped %d automation sessions in the last %d hours", n, int(guardTripsSpan.Hours()))}
			w.TripsFrom = ptrOf(now)
		}
	}
	return hold, nil
}

func guardLineFrom(from, usual string) string {
	switch from {
	case "set":
		return "the line you set"
	case "usual":
		return usual
	}
	return "the default, until there is a month of your usage to go by"
}

// guardPulseLook is how far back a session's pulse before a window is
// looked for, as the start of its first rise in it.
const guardPulseLook = 6 * time.Hour

// guardSpend is what was spent in [from, to] at list price, by automation
// and in all as Waypoint saw it. Sessions count each pulse's rise over the
// one before, to whoever drove the session at that pulse, so a person's
// spend in a session automation also drives stays theirs. Claude runner
// jobs (automation's) count by the share of their run in the window.
func (s *Service) guardSpend(ctx context.Context, uid int64, from, to time.Time) (auto, all float64, err error) {
	f, tt := from.UnixMilli(), to.UnixMilli()
	rows, err := s.db.R.QueryContext(ctx, `SELECT auto, cost - prev FROM (
		SELECT at, auto, cost, LAG(cost) OVER (PARTITION BY machine, cli_id ORDER BY at, id) AS prev
		FROM session_pulses WHERE user_id = ? AND at >= ? AND at <= ?)
		WHERE at >= ? AND prev IS NOT NULL AND cost > prev`, uid, f-guardPulseLook.Milliseconds(), tt, f)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var a bool
		var rise float64
		if err := rows.Scan(&a, &rise); err != nil {
			_ = rows.Close()
			return 0, 0, err
		}
		all += rise
		if a {
			auto += rise
		}
	}
	if err := rows.Close(); err != nil {
		return 0, 0, err
	}
	jrows, err := s.db.R.QueryContext(ctx, `SELECT started_at, finished_at, coalesce(json_extract(result, '$.tokens.cost_usd'), 0) FROM runner_jobs
		WHERE cli = 'claude' AND finished_at >= ? AND owner_id = ? AND kind IN (?,?,?) AND source != 'manual'
		AND started_at IS NOT NULL AND started_at <= ?`,
		f, uid, JobAgent, JobReview, JobAudit, tt)
	if err != nil {
		return 0, 0, err
	}
	defer jrows.Close()
	for jrows.Next() {
		var start, end int64
		var cost float64
		if err := jrows.Scan(&start, &end, &cost); err != nil {
			return 0, 0, err
		}
		if cost <= 0 {
			continue
		}
		end = max(end, start+1)
		part := cost * spanOverlap(f, tt+1, start, end) / float64(end-start)
		auto += part
		all += part
	}
	return math.Round(auto*100) / 100, math.Round(all*100) / 100, jrows.Err()
}

// automationSpend is automation's part of guardSpend.
func (s *Service) automationSpend(ctx context.Context, uid int64, from, to time.Time) (float64, error) {
	auto, _, err := s.guardSpend(ctx, uid, from, to)
	return auto, err
}

// refreshGuardBaselines measures each active person's automation over the
// last 28 days, at most once a day. Called hourly.
func (s *Service) refreshGuardBaselines(ctx context.Context) {
	now := s.now()
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT user_id FROM usage_hours WHERE hour >= ?`, now.Add(-48*time.Hour).UnixMilli())
	if err != nil {
		s.log.WarnContext(ctx, "quota guard baseline", "err", err)
		return
	}
	var uids []int64
	for rows.Next() {
		var uid int64
		if rows.Scan(&uid) == nil {
			uids = append(uids, uid)
		}
	}
	_ = rows.Close()
	for _, uid := range uids {
		if ctx.Err() != nil {
			return
		}
		w, err := s.loadGuardWatch(ctx, s.db.R, uid)
		if err != nil {
			continue
		}
		if w.Baseline != nil && now.Sub(w.Baseline.At) < guardBaselineEvery {
			continue
		}
		b, err := s.guardBaseline(ctx, uid, now)
		if err != nil {
			s.log.WarnContext(ctx, "quota guard baseline", "user", uid, "err", err)
			continue
		}
		if err := s.write(ctx, nil, func(t *tx) error {
			cur, err := s.loadGuardWatch(ctx, t, uid)
			if err != nil {
				return err
			}
			cur.Baseline = b
			return saveGuardWatch(ctx, t, uid, cur)
		}); err != nil {
			s.log.WarnContext(ctx, "quota guard baseline", "user", uid, "err", err)
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM session_pulses WHERE at < ?`, now.Add(-guardPulseKeep).UnixMilli()); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM quota_guard_trips WHERE at < ?`, now.Add(-guardTripKeep).UnixMilli()); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// guardBaseline is automation's usual spend over the last 28 whole hours'
// days, by the daily check's own split of each session by its owner.
func (s *Service) guardBaseline(ctx context.Context, uid int64, now time.Time) (*GuardBaseline, error) {
	to := now.UTC().Truncate(time.Hour)
	from := to.Add(-guardBaselineDays * 24 * time.Hour)
	hours, err := s.guardSpending(ctx, uid, from.UnixMilli(), to.UnixMilli(), from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	b := &GuardBaseline{At: now, Days: guardBaselineDays, Models: map[string]float64{}}
	sessions := map[string]float64{}
	var hourly []float64
	var modelTotal float64
	for _, h := range hours {
		if h.auto > 0.01 {
			hourly = append(hourly, h.auto)
		}
		for k, c := range h.sources {
			if strings.HasPrefix(k, "session:") {
				sessions[k] += c
			}
		}
		for m, c := range h.models {
			b.Models[m] += c
			modelTotal += c
		}
	}
	var costs []float64
	for _, c := range sessions {
		if c > 0.01 {
			costs = append(costs, c)
		}
	}
	b.Sessions, b.Hours = len(costs), len(hourly)
	b.SessionP50, b.SessionP90 = round2(percentile(costs, 50)), round2(percentile(costs, 90))
	b.HourP95 = round2(percentile(hourly, 95))
	if len(hourly) > 0 {
		b.HourMax = round2(slices.Max(hourly))
	}
	for m, c := range b.Models {
		if modelTotal > 0 {
			b.Models[m] = math.Round(c/modelTotal*1000) / 1000
		}
	}
	return b, nil
}

// QuotaGuardTrip is one time the guard told, stopped or held.
type QuotaGuardTrip struct {
	At      time.Time `json:"at"`
	Session int64     `json:"session,omitempty"`
	Title   string    `json:"title,omitempty"`
	Machine string    `json:"machine,omitempty"`
	Rule    string    `json:"rule"`
	Action  string    `json:"action"`
	Cost    float64   `json:"cost,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// QuotaGuardLive is the live guard as the person sees it.
type QuotaGuardLive struct {
	Who        string             `json:"who"`
	Settings   QuotaGuardSettings `json:"settings"`
	Policy     *GuardPolicy       `json:"policy"`
	Baseline   *GuardBaseline     `json:"baseline,omitempty"`
	Hold       *GuardHold         `json:"hold,omitempty"`
	QuietUntil *time.Time         `json:"quiet_until,omitempty"`
	OverSince  *time.Time         `json:"over_since,omitempty"`
	HourUSD    float64            `json:"hour_usd"` // automation's spend in the last hour
	Trips      []QuotaGuardTrip   `json:"trips"`
	Days       int                `json:"days"`
}

// QuotaGuardLiveState is the live guard for the actor (or, for an admin,
// the person named), with its trips over the last days.
func (s *Service) QuotaGuardLiveState(ctx context.Context, actor *Actor, who string, days int) (*QuotaGuardLive, error) {
	uids, everyone, label, err := s.usageWho(ctx, actor, who)
	if err != nil {
		return nil, err
	}
	if everyone {
		return nil, invalid("the quota guard is one person's: name them")
	}
	uid := uids[0]
	if days <= 0 {
		days = 7
	}
	days = min(days, 90)
	set, err := s.guardSettings(ctx, s.db.R, uid)
	if err != nil {
		return nil, err
	}
	w, err := s.loadGuardWatch(ctx, s.db.R, uid)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := &QuotaGuardLive{Who: label, Settings: set, Policy: guardPolicy(set, w.Baseline), Baseline: w.Baseline, Days: days, Trips: []QuotaGuardTrip{}}
	if !set.Off {
		out.Hold, out.OverSince = w.activeHold(now), w.OverSince
		if w.QuietUntil != nil && w.QuietUntil.After(now) {
			out.QuietUntil = w.QuietUntil
		}
	}
	if out.HourUSD, err = s.automationSpend(ctx, uid, now.Add(-time.Hour), now); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT g.at, coalesce(g.session_id, 0), coalesce(x.title, ''), g.machine, g.rule, g.action, g.cost, g.detail
		FROM quota_guard_trips g LEFT JOIN agent_sessions x ON x.id = g.session_id WHERE g.user_id = ? AND g.at >= ? ORDER BY g.at DESC, g.id DESC LIMIT 200`,
		uid, now.Add(-time.Duration(days)*24*time.Hour).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tr QuotaGuardTrip
		var at int64
		if err := rows.Scan(&at, &tr.Session, &tr.Title, &tr.Machine, &tr.Rule, &tr.Action, &tr.Cost, &tr.Detail); err != nil {
			return nil, err
		}
		tr.At = msTime(at)
		out.Trips = append(out.Trips, tr)
	}
	return out, rows.Err()
}

// guardChangeAllowed: the guard's settings and holds are the person's. An
// agent's token, an automation, or a session automation drives (a task
// session, or one whose last prompt was autopilot's) may not change them; a
// person in the web, their terminal or a session they are typing in may.
func (s *Service) guardChangeAllowed(ctx context.Context, a *Actor) error {
	const what = "changing the quota guard"
	if err := a.requireHuman(what); err != nil {
		return err
	}
	if a.Automation != nil {
		return forbidden("%s is for people, not automations", what)
	}
	if a.Session == 0 {
		if a.AgentEnv != "" {
			return forbidden("%s is for people: in the web, your own terminal, or a session you are typing in", what)
		}
		return nil
	}
	var machine, cli, kind string
	if err := s.db.R.QueryRowContext(ctx, `SELECT machine, cli_id, kind FROM agent_sessions WHERE id = ? AND user_id = ?`, a.Session, a.id()).Scan(&machine, &cli, &kind); err != nil {
		return forbidden("%s is for people: in the web, your own terminal, or a session you are typing in", what)
	}
	if kind == SessionTask {
		return forbidden("%s is for people, not a task session (ask with wp request change)", what)
	}
	line, _, err := s.sessionOwners(ctx, a.id(), machine, cli, s.nowMS()+1)
	if err != nil {
		return err
	}
	if len(line) > 0 && line[len(line)-1].auto {
		return forbidden("%s is for people, not a session autopilot drives (type in it first, or use the web)", what)
	}
	return nil
}

// QuotaGuardSettingsInput changes the settings: Settings replaces them
// whole unless Merge, which sets only the fields given (Clear names fields
// to put back to their defaults).
type QuotaGuardSettingsInput struct {
	Settings QuotaGuardSettings `json:"settings"`
	Merge    bool               `json:"merge"`
	On       *bool              `json:"on,omitempty"` // with Merge: turn the guard on or off
	Clear    []string           `json:"clear,omitempty"`
}

// SetQuotaGuard changes the actor's guard settings.
func (s *Service) SetQuotaGuard(ctx context.Context, actor *Actor, in QuotaGuardSettingsInput) (*QuotaGuardLive, error) {
	if err := actor.need(PermSessionsStart); err != nil {
		return nil, err
	}
	if err := s.guardChangeAllowed(ctx, actor); err != nil {
		return nil, err
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	uid := actor.id()
	for _, v := range []*float64{in.Settings.ChurnUSD, in.Settings.SessionUSD, in.Settings.Hours, in.Settings.IdleHours, in.Settings.ModelUSD, in.Settings.BurstUSD} {
		if v != nil && (*v < 0 || math.IsNaN(*v) || *v > 100000) {
			return nil, invalid("a line is a number from 0 (off) up")
		}
	}
	if len(in.Settings.Models) > 20 {
		return nil, invalid("at most 20 models")
	}
	err := s.write(ctx, actor, func(t *tx) error {
		set := in.Settings
		if in.Merge {
			cur, err := s.guardSettings(ctx, t, uid)
			if err != nil {
				return err
			}
			mergeGuardSettings(&cur, in.Settings)
			if in.On != nil {
				cur.Off = !*in.On
			}
			for _, f := range in.Clear {
				if err := clearGuardSetting(&cur, f); err != nil {
					return err
				}
			}
			set = cur
		}
		raw, err := json.Marshal(set)
		if err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, `INSERT INTO quota_guard_settings(user_id, settings, updated_at, updated_by) VALUES (?,?,?,?)
			ON CONFLICT(user_id) DO UPDATE SET settings = excluded.settings, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
			uid, string(raw), s.nowMS(), uid)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.QuotaGuardLiveState(ctx, actor, "", 7)
}

func mergeGuardSettings(cur *QuotaGuardSettings, in QuotaGuardSettings) {
	for _, f := range []struct{ dst, src **float64 }{
		{&cur.ChurnUSD, &in.ChurnUSD}, {&cur.SessionUSD, &in.SessionUSD}, {&cur.Hours, &in.Hours},
		{&cur.IdleHours, &in.IdleHours}, {&cur.ModelUSD, &in.ModelUSD}, {&cur.BurstUSD, &in.BurstUSD},
	} {
		if *f.src != nil {
			*f.dst = *f.src
		}
	}
	if in.Models != nil {
		cur.Models = in.Models
	}
	if in.Pace != nil {
		cur.Pace = in.Pace
	}
	if in.Off {
		cur.Off = true
	}
}

func clearGuardSetting(set *QuotaGuardSettings, field string) error {
	switch field {
	case "churn_usd":
		set.ChurnUSD = nil
	case "session_usd":
		set.SessionUSD = nil
	case "hours":
		set.Hours = nil
	case "idle_hours":
		set.IdleHours = nil
	case "model_usd":
		set.ModelUSD = nil
	case "models":
		set.Models = nil
	case "burst_usd":
		set.BurstUSD = nil
	case "pace":
		set.Pace = nil
	case "all":
		*set = QuotaGuardSettings{Off: set.Off}
	default:
		return invalid("no setting %q", field)
	}
	return nil
}

// LiftQuotaGuard ends the actor's hold now, and keeps the sweep from
// holding again for quietFor (a duration such as 2h; 2 hours when empty; at
// most a week).
func (s *Service) LiftQuotaGuard(ctx context.Context, actor *Actor, quietFor string) (*QuotaGuardLive, error) {
	quiet := guardLiftQuiet
	if quietFor != "" {
		d, err := time.ParseDuration(quietFor)
		if err != nil || d <= 0 {
			return nil, invalid("for: a duration such as 2h or 30m")
		}
		quiet = d
	}
	if err := actor.need(PermSessionsStart); err != nil {
		return nil, err
	}
	if err := s.guardChangeAllowed(ctx, actor); err != nil {
		return nil, err
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	quiet = min(quiet, 7*24*time.Hour)
	uid := actor.id()
	now := s.now()
	err := s.write(ctx, actor, func(t *tx) error {
		w, err := s.loadGuardWatch(ctx, t, uid)
		if err != nil {
			return err
		}
		rule := ""
		if w.Hold != nil {
			rule = w.Hold.Rule
		}
		w.Hold, w.QuietUntil, w.TripsFrom = nil, ptrOf(now.Add(quiet)), ptrOf(now)
		w.OverSince, w.OverFrom = nil, 0
		if err := saveGuardWatch(ctx, t, uid, w); err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, `INSERT INTO quota_guard_trips(user_id, rule, action, detail, at) VALUES (?,?,?,?,?)`,
			uid, cmp(rule, "none"), "lifted", fmt.Sprintf("you lifted it; no new hold until %s", now.Add(quiet).UTC().Format("Jan 2 15:04 UTC")), now.UnixMilli())
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.QuotaGuardLiveState(ctx, actor, "", 7)
}

func (s *Service) notifyQuotaGuardLive(ctx context.Context, e *Event) {
	switch e.Kind {
	case EvQuotaGuardHold:
		s.notifyURL(ctx, e.UserID, e, NotifyQuota, "Quota guard: automation held back",
			strings.TrimSpace(e.New)+". Autopilot starts nothing and running automation sessions wind down until it passes; wp usage guard lift ends it.", "/agents?tab=autopilot")
	case EvQuotaGuardStop:
		title := "Quota guard stopped a session"
		if t, _ := e.Data["title"].(string); t != "" {
			title = "Quota guard stopped " + t
		}
		body := strings.TrimSpace(e.New)
		if keys, _ := e.Data["tasks"].(string); keys != "" {
			body += " " + keys + " waits a while before it is handed out again."
		}
		link := "/agents?tab=sessions"
		if id := fmt.Sprint(e.Data["session"]); id != "" && id != "0" && id != "<nil>" {
			link = "/agents/sessions/" + id
		}
		s.notifyURL(ctx, e.UserID, e, NotifyQuota, title, body, link)
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
