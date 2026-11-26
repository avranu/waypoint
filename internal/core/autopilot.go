package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Waypoint's defaults (WP-ADR9 has the numbers behind them).
const (
	// DefaultAutopilotHourlyUSD: person-wide spend in the last hour, at list
	// price. Normal active hours: p50 $11, p99 $127 with X6, $75 only 6 of 219.
	DefaultAutopilotHourlyUSD = 75.0
	// DefaultAutopilotWeekAhead: points of the week's quota its use may run
	// ahead of an even pace (elapsed share of the week × 100).
	DefaultAutopilotWeekAhead = 15.0
	// DefaultAutopilotWeekMax: the week's use at which it always pauses,
	// leaving the rest for the person's own sessions.
	DefaultAutopilotWeekMax = 90.0
	// DefaultAutopilotWeekEnd: the week's last hours, before it resets, with
	// no ceiling (100%), after as many hours in which the ceilings rise to it:
	// what is left by the reset goes unused, and using it up takes a while.
	DefaultAutopilotWeekEnd = 12.0
	// DefaultAutopilotFiveHourMax: the five-hour window's use at which it
	// pauses until the window resets.
	DefaultAutopilotFiveHourMax = 90.0
	// DefaultAutopilotGrace: seconds between a finished Stop and the clear,
	// for a person who is there to keep the session.
	DefaultAutopilotGrace = 60
	// DefaultAutopilotMerge: wait for an unmerged pull request only when the
	// next task builds on it.
	DefaultAutopilotMerge = MergeWaitAuto
	// DefaultAutopilotDeferred: wp ship from an Opus or Fable session first
	// offers the tasks it filed meanwhile (WP-1858).
	DefaultAutopilotDeferred = true
	// DefaultAutopilotReview: no adversarial review round before moving a
	// session on, unless the person turns it on (WP-285).
	DefaultAutopilotReview = false
	// DefaultReviewPrompt is the review round's prompt when the person set
	// none: short, so the round costs the review and not a briefing. The
	// round runs before the task's pull request opens, so it ends with wp
	// ship (WP-688, WP-689, WP-1370: --pr is the same as plain wp ship).
	DefaultReviewPrompt = "Run an adversarial review. Assess and fix any legitimate issues it finds that are consistent with our codebase and goals. " +
		"If there are any findings, comments, data, or unrelated issues found, file them in waypoint so they aren't lost when the session is over. " +
		"When you're done, run `wp ship`."
	// wp688ReviewPrompt was the default from WP-688 to WP-1370, which asked
	// for --pr; saved with it gets the default too.
	wp688ReviewPrompt = "Run an adversarial review. Assess and fix any legitimate issues it finds that are consistent with our codebase and goals. " +
		"If there are any findings, comments, data, or unrelated issues found, file them in waypoint so they aren't lost when the session is over. " +
		"When you're done, run `wp ship --pr`."
	// legacyReviewPrompt was the default before WP-689: a person's settings
	// saved with it get the default as it is now.
	legacyReviewPrompt = "Run an adversarial review on this change, and correct any issues that are discovered."
	// wp689ReviewPrompt was WP-689's default, from before wp ship said what
	// it wants (--pr); saved with it gets the default too.
	wp689ReviewPrompt = "Run an adversarial review. Assess and fix any legitimate issues it finds that are consistent with our codebase and goals. " +
		"If there are any findings, comments, data, or unrelated issues found, file them in waypoint so they aren't lost when the session is over. " +
		"When you're done, run `wp ship`."
	// maxReviewPrompt: the longest review prompt a person can set.
	maxReviewPrompt = 2000
	// maxReviewJobPrompt: the longest prompt a review job types, room for an
	// audit's every finding at the most it stores of each (WP-769).
	maxReviewJobPrompt = 512 << 10
	// DefaultAutopilotPerHour: tasks it starts in one project in an hour;
	// more wait for the hour to roll on (the loop guard of WP-ADR12).
	DefaultAutopilotPerHour = 8
	// DefaultAutopilotMergeWaitHours: the longest a task waits for a merge
	// the merge policy holds it for.
	DefaultAutopilotMergeWaitHours = 3
	// DefaultAutopilotAtOnce: autopilot sessions running in one project at a
	// time (WP-ADR72), a person's own sessions at a task counted with them
	// (WP-1079). 0: no limit on moving a finished session on, and the sweep
	// starts none of its own.
	DefaultAutopilotAtOnce = 1
)

// Merge policies: when the next task waits for the last one's pull request.
const (
	MergeWaitAuto   = "auto"
	MergeWaitAlways = "always"
	MergeWaitNever  = "never"
)

// AutopilotLimits are the settings a person can set, for all their projects
// or one. A nil field inherits: a project's from the person's defaults,
// those from Waypoint's. 0 turns a limit off.
type AutopilotLimits struct {
	MaxHourlyUSD *float64 `json:"max_hourly_usd,omitempty"`
	WeekAhead    *float64 `json:"week_ahead,omitempty"`
	WeekMax      *float64 `json:"week_max,omitempty"`
	WeekEndHours *float64 `json:"week_end_hours,omitempty"`
	FiveHourMax  *float64 `json:"five_hour_max,omitempty"`
	Merge        *string  `json:"merge,omitempty"`
	GraceSeconds *int     `json:"grace_seconds,omitempty"`
	// Deferred: a ship by an Opus or Fable session stops once to list the
	// tasks it filed meanwhile that are ready, for it to do with the change
	// (WP-1858, deferred.go).
	Deferred *bool `json:"deferred,omitempty"`
	// Review: before moving a session on, compact it and ask it for an
	// adversarial review of its finished task (WP-285); ReviewPrompt is
	// what it is asked ("" or unset: DefaultReviewPrompt).
	Review       *bool   `json:"review,omitempty"`
	ReviewPrompt *string `json:"review_prompt,omitempty"`
	// ReviewSkip: whether review triage may skip the review of low-risk
	// changes (WP-692): on, shadow (record only) or off.
	ReviewSkip *string `json:"review_skip,omitempty"`
	// ReviewText is the reviewer's brief for review rounds ("" or unset:
	// DefaultReviewText) and ReviewModel who reviews: auto, session, or a
	// model as cli/model (WP-1320, review_profile.go).
	ReviewText  *string `json:"review_text,omitempty"`
	ReviewModel *string `json:"review_model,omitempty"`

	// Which tasks it takes (WP-284, WP-ADR49): every criterion set must
	// hold. "" or 0 or false leaves a criterion off.
	MinPriority   *string `json:"min_priority,omitempty"`    // low, medium, high or urgent
	DueWithinDays *int    `json:"due_within_days,omitempty"` // due within so many days (or overdue)
	Approved      *bool   `json:"approved,omitempty"`        // a person wrote or approved the task (is:approved)
	Planned       *bool   `json:"planned,omitempty"`         // a plan or a ready prep exists (is:planned)
	Query         *string `json:"query,omitempty"`           // a WQL query the tasks must also match

	// Pace (WP-284): how often it starts tasks in a project.
	PerHour        *int    `json:"per_hour,omitempty"`         // starts in an hour: the loop guard, never off
	PerDay         *int    `json:"per_day,omitempty"`          // starts in the person's calendar day
	SpacingMinutes *int    `json:"spacing_minutes,omitempty"`  // least time between two starts
	AtOnce         *int    `json:"at_once,omitempty"`          // sessions under way at a time: autopilot's, and a person's own at a task
	MergeWaitHours *int    `json:"merge_wait_hours,omitempty"` // the longest the merge policy holds a task
	Hours          *string `json:"hours,omitempty"`            // "09:00-17:00": when it may start tasks ("" any time)
	Days           *string `json:"days,omitempty"`             // "mon-fri": on which days ("" every day)

	// Use what's left at the week's end (WP-262), for all the person's
	// projects at once: over the week's last week_end_hours, start sessions
	// past at_once, up to SpendAtOnce across projects, paced to use the
	// week's quota up by its reset (autopilot_spend.go).
	SpendLeft   *bool `json:"spend_left,omitempty"`
	SpendAtOnce *int  `json:"spend_at_once,omitempty"`
}

// over lays o's set fields over l.
func (l AutopilotLimits) over(o AutopilotLimits) AutopilotLimits {
	lay(&l.MaxHourlyUSD, o.MaxHourlyUSD)
	lay(&l.WeekAhead, o.WeekAhead)
	lay(&l.WeekMax, o.WeekMax)
	lay(&l.WeekEndHours, o.WeekEndHours)
	lay(&l.FiveHourMax, o.FiveHourMax)
	lay(&l.Merge, o.Merge)
	lay(&l.GraceSeconds, o.GraceSeconds)
	lay(&l.Deferred, o.Deferred)
	lay(&l.Review, o.Review)
	lay(&l.ReviewPrompt, o.ReviewPrompt)
	lay(&l.ReviewSkip, o.ReviewSkip)
	lay(&l.ReviewText, o.ReviewText)
	lay(&l.ReviewModel, o.ReviewModel)
	lay(&l.MinPriority, o.MinPriority)
	lay(&l.DueWithinDays, o.DueWithinDays)
	lay(&l.Approved, o.Approved)
	lay(&l.Planned, o.Planned)
	lay(&l.Query, o.Query)
	lay(&l.PerHour, o.PerHour)
	lay(&l.PerDay, o.PerDay)
	lay(&l.SpacingMinutes, o.SpacingMinutes)
	lay(&l.AtOnce, o.AtOnce)
	lay(&l.MergeWaitHours, o.MergeWaitHours)
	lay(&l.Hours, o.Hours)
	lay(&l.Days, o.Days)
	lay(&l.SpendLeft, o.SpendLeft)
	lay(&l.SpendAtOnce, o.SpendAtOnce)
	return l
}

func lay[T any](dst **T, v *T) {
	if v != nil {
		*dst = v
	}
}

func ptrOf[T any](v T) *T { return &v }

// waypointAutopilotDefaults has every field set.
func waypointAutopilotDefaults() AutopilotLimits {
	return AutopilotLimits{MaxHourlyUSD: ptrOf(DefaultAutopilotHourlyUSD), WeekAhead: ptrOf(DefaultAutopilotWeekAhead), WeekMax: ptrOf(DefaultAutopilotWeekMax),
		WeekEndHours: ptrOf(DefaultAutopilotWeekEnd), FiveHourMax: ptrOf(DefaultAutopilotFiveHourMax), Merge: ptrOf(DefaultAutopilotMerge), GraceSeconds: ptrOf(DefaultAutopilotGrace),
		Deferred: ptrOf(DefaultAutopilotDeferred), Review: ptrOf(DefaultAutopilotReview), ReviewPrompt: ptrOf(DefaultReviewPrompt), ReviewSkip: ptrOf(DefaultReviewSkip),
		ReviewText: ptrOf(""), ReviewModel: ptrOf(ReviewModelAuto),
		MinPriority: ptrOf(""), DueWithinDays: ptrOf(0), Approved: ptrOf(false), Planned: ptrOf(false), Query: ptrOf(""),
		PerHour: ptrOf(DefaultAutopilotPerHour), PerDay: ptrOf(0), SpacingMinutes: ptrOf(0), AtOnce: ptrOf(DefaultAutopilotAtOnce), MergeWaitHours: ptrOf(DefaultAutopilotMergeWaitHours),
		Hours: ptrOf(""), Days: ptrOf(""), SpendLeft: ptrOf(false), SpendAtOnce: ptrOf(DefaultSpendAtOnce)}
}

// reviewPrompt is the review round's prompt under these limits.
func (l AutopilotLimits) reviewPrompt() string {
	return ownReviewPrompt(valueOf(l.ReviewPrompt))
}

func (l AutopilotLimits) validate() error {
	pct := func(name string, v *float64) error {
		if v != nil && (math.IsNaN(*v) || *v < 0 || *v > 100) {
			return invalid("%s must be 0-100 (0 turns it off)", name)
		}
		return nil
	}
	if l.MaxHourlyUSD != nil && (math.IsNaN(*l.MaxHourlyUSD) || *l.MaxHourlyUSD < 0 || *l.MaxHourlyUSD > 100000) {
		return invalid("the hourly limit must be 0-100000 dollars (0 turns it off)")
	}
	if err := pct("week_ahead", l.WeekAhead); err != nil {
		return err
	}
	if err := pct("week_max", l.WeekMax); err != nil {
		return err
	}
	if err := pct("five_hour_max", l.FiveHourMax); err != nil {
		return err
	}
	if l.WeekEndHours != nil && (math.IsNaN(*l.WeekEndHours) || *l.WeekEndHours < 0 || *l.WeekEndHours > 168) {
		return invalid("week_end_hours must be 0-168 (0: the week ceiling holds to the end)")
	}
	if l.Merge != nil {
		switch *l.Merge {
		case MergeWaitAuto, MergeWaitAlways, MergeWaitNever:
		default:
			return invalid("merge must be auto, always or never")
		}
	}
	if l.ReviewSkip != nil {
		switch *l.ReviewSkip {
		case ReviewSkipOn, ReviewSkipShadow, ReviewSkipOff:
		default:
			return invalid("review_skip must be on, shadow or off")
		}
	}
	if err := l.validateReviewProfile(); err != nil {
		return err
	}
	if l.SpendAtOnce != nil && (*l.SpendAtOnce < 1 || *l.SpendAtOnce > 20) {
		return invalid("spend_at_once must be 1-20")
	}
	if l.GraceSeconds != nil && (*l.GraceSeconds < 0 || *l.GraceSeconds > 3600) {
		return invalid("the grace period must be 0-3600 seconds")
	}
	if l.ReviewPrompt != nil {
		p := strings.TrimSpace(*l.ReviewPrompt)
		if utf8.RuneCountInString(p) > maxReviewPrompt {
			return invalid("the review prompt must be at most %d characters", maxReviewPrompt)
		}
		if strings.ContainsFunc(p, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) {
			return invalid("the review prompt can't hold tabs or other control characters")
		}
	}
	return l.validatePace()
}

// AutopilotProject is one project's autopilot as its person sees it.
type AutopilotProject struct {
	ProjectID   int64  `json:"project_id"`
	ProjectKey  string `json:"project_key"`
	ProjectName string `json:"project_name"`
	Enabled     bool   `json:"enabled"`
	// Limits: what this project sets itself; Effective: what applies.
	Limits    AutopilotLimits `json:"limits"`
	Effective AutopilotLimits `json:"effective"`
	// Criteria and Pace are the effective criteria and pace in words
	// (Criteria "": any ready task; WP-284).
	Criteria string `json:"criteria"`
	Pace     string `json:"pace"`
	// Steps: what autopilot is doing about this project's sessions now.
	Steps []*AutopilotStep `json:"steps"`
	// Sessions: the person's other open sessions in it, busy or idle
	// (WP-307), for the page to show what they do and start one.
	Sessions []*AutopilotOpenSession `json:"sessions"`
}

// AutopilotOverview is the Autopilot tab: the person's defaults, each
// project they can work in, the readings the limits look at, and how many
// summaries wait to be read.
type AutopilotOverview struct {
	Defaults   AutopilotLimits     `json:"defaults"`  // what the person set
	Effective  AutopilotLimits     `json:"effective"` // with Waypoint's defaults filled in
	Waypoint   AutopilotLimits     `json:"waypoint"`  // Waypoint's own
	Criteria   string              `json:"criteria"`  // the effective defaults' criteria in words
	Pace       string              `json:"pace"`      // and their pace
	Projects   []*AutopilotProject `json:"projects"`
	Meter      *AutopilotMeter     `json:"meter"`
	Unreviewed int                 `json:"unreviewed"`
	// Spend: the week's-end plan (WP-262).
	Spend *SpendPlan `json:"spend"`
}

// AutopilotMeter is what the limits read now.
type AutopilotMeter struct {
	HourlyUSD     float64    `json:"hourly_usd"`
	WeekPercent   *float64   `json:"week_percent,omitempty"`
	WeekEven      *float64   `json:"week_even,omitempty"`
	WeekResetsAt  *time.Time `json:"week_resets_at,omitempty"`
	WeekCap       *float64   `json:"week_cap,omitempty"` // the week ceiling now, by the default limits
	FiveHour      *float64   `json:"five_hour_percent,omitempty"`
	FiveResetsAt  *time.Time `json:"five_hour_resets_at,omitempty"`
	QuotaSampled  *time.Time `json:"quota_sampled_at,omitempty"`
	QuotaUnknown  bool       `json:"quota_unknown,omitempty"`
	MeasuredAt    time.Time  `json:"measured_at"`
	PausedBecause string     `json:"paused_because,omitempty"` // the default limits' verdict now
	// QuotaOffUntil: the person turned the quota limits off until then;
	// QuotaHeld is what they would pause for now if they were on.
	QuotaOffUntil *time.Time `json:"quota_off_until,omitempty"`
	QuotaHeld     string     `json:"quota_held,omitempty"`
	// GuardHold: the live quota guard holds automation back (WP-932); it
	// holds while the quota limits are off too, and lifts by wp report usage guard
	// lift or turning the guard off.
	GuardHold *GuardHold `json:"guard_hold,omitempty"`
}

// autopilotQuotaOffMax is how long the quota limits can be turned off for
// at once: a week, so the next one has them again.
const autopilotQuotaOffMax = 7 * 24 * time.Hour

// autopilotRow is one stored settings row.
type autopilotRow struct {
	enabled       bool
	limits        AutopilotLimits
	quotaOffUntil int64
}

// autopilotRows reads a person's settings: the defaults row (project 0) and
// each project's.
func (s *Service) autopilotRows(ctx context.Context, q queryer, userID int64) (map[int64]autopilotRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT coalesce(project_id, 0), enabled, limits, quota_off_until FROM autopilot_settings WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]autopilotRow{}
	for rows.Next() {
		var pid int64
		var en int
		var raw string
		r := autopilotRow{}
		if err := rows.Scan(&pid, &en, &raw, &r.quotaOffUntil); err != nil {
			return nil, err
		}
		r.enabled = en != 0
		_ = json.Unmarshal([]byte(raw), &r.limits)
		out[pid] = r
	}
	return out, rows.Err()
}

// autopilotFor says whether autopilot is on for the person in the project,
// and the limits that apply there.
func (s *Service) autopilotFor(ctx context.Context, q queryer, userID, projectID int64) (bool, AutopilotLimits, error) {
	eff := waypointAutopilotDefaults()
	if userID == 0 || projectID == 0 {
		return false, eff, nil
	}
	rows, err := s.autopilotRows(ctx, q, userID)
	if err != nil {
		return false, eff, err
	}
	eff = eff.over(rows[0].limits)
	p, ok := rows[projectID]
	if !ok {
		return false, eff, nil
	}
	return p.enabled, eff.over(p.limits), nil
}

// AutopilotOverview is the person's autopilot: settings, projects, readings.
func (s *Service) AutopilotOverview(ctx context.Context, a *Actor) (*AutopilotOverview, error) {
	if err := a.need(PermAgentsRead); err != nil {
		return nil, err
	}
	uid := a.id()
	rows, err := s.autopilotRows(ctx, s.db.R, uid)
	if err != nil {
		return nil, err
	}
	o := &AutopilotOverview{Defaults: rows[0].limits, Waypoint: waypointAutopilotDefaults(), Projects: []*AutopilotProject{}}
	o.Effective = o.Waypoint.over(o.Defaults)
	o.Criteria, o.Pace = o.Effective.criteriaText(), o.Effective.paceText()
	projects, err := s.ListProjects(ctx, a, false)
	if err != nil {
		return nil, err
	}
	steps, err := s.autopilotSteps(ctx, uid, 0)
	if err != nil {
		return nil, err
	}
	open, err := s.autopilotOpenSessions(ctx, uid)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Archived || (p.Role != "owner" && p.Role != "editor") {
			continue
		}
		r := rows[p.ID]
		ap := &AutopilotProject{ProjectID: p.ID, ProjectKey: p.Key, ProjectName: p.Name, Enabled: r.enabled, Limits: r.limits,
			Effective: o.Effective.over(r.limits), Steps: []*AutopilotStep{}, Sessions: open[p.ID]}
		ap.Criteria, ap.Pace = ap.Effective.criteriaText(), ap.Effective.paceText()
		if ap.Sessions == nil {
			ap.Sessions = []*AutopilotOpenSession{}
		}
		for _, st := range steps {
			if st.projectID == p.ID {
				ap.Steps = append(ap.Steps, st)
			}
		}
		o.Projects = append(o.Projects, ap)
	}
	m, err := s.autopilotMeter(ctx, uid)
	if err != nil {
		return nil, err
	}
	if why, _ := autopilotPaused(m, o.Effective, s.now()); why != "" {
		m.PausedBecause = why
	}
	if m.QuotaOffUntil != nil {
		m.QuotaHeld, _ = quotaPaused(m, o.Effective, s.now())
	}
	if c := weekCap(m, o.Effective, s.now()); c > 0 {
		m.WeekCap = ptrOf(math.Round(c*10) / 10)
	}
	o.Meter = m
	if o.Spend, err = s.spendPlan(ctx, uid, o.Effective, m, s.now()); err != nil {
		return nil, err
	}
	limit, limitArgs := taskSummariesFor(a)
	// #nosec G202 -- limit is built from constant clauses; values are bound
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM task_summaries ts WHERE ts.user_id = ? AND ts.reviewed_at IS NULL`+limit, append([]any{uid}, limitArgs...)...).Scan(&o.Unreviewed); err != nil {
		return nil, err
	}
	return o, nil
}

// AutopilotSettingInput changes the person's defaults (no project) or one
// project's settings. Enabled nil leaves it; Limits replaces what the row
// sets, and Reset clears it so everything inherits.
type AutopilotSettingInput struct {
	Enabled *bool           `json:"enabled,omitempty"`
	Limits  AutopilotLimits `json:"limits"`
	Merge   bool            `json:"merge,omitempty"` // only set the given limits, keep the others
	Reset   bool            `json:"reset,omitempty"`
	// QuotaOffUntil turns the quota limits off until then (defaults only); a
	// time not in the future turns them back on. Nil leaves it.
	QuotaOffUntil *time.Time `json:"quota_off_until,omitempty"`
}

// SetAutopilot saves the person's defaults (project "") or a project's.
func (s *Service) SetAutopilot(ctx context.Context, a *Actor, project string, in AutopilotSettingInput) (*AutopilotOverview, error) {
	if err := a.need(PermSessionsStart); err != nil {
		return nil, err
	}
	if offOnly := in.Enabled != nil && !*in.Enabled && in.Merge && !in.Reset && in.Limits == (AutopilotLimits{}) && in.QuotaOffUntil == nil; offOnly {
		if err := a.requireHuman("changing autopilot"); err != nil {
			return nil, err
		}
	} else if err := a.requirePersonDirect("changing autopilot, which starts work on your machines,"); err != nil {
		return nil, err
	}
	if err := a.canWrite(); err != nil {
		return nil, err
	}
	in.Limits = in.Limits.normalized()
	if err := in.Limits.validate(); err != nil {
		return nil, err
	}
	if err := s.checkCriteriaQuery(ctx, a, in.Limits); err != nil {
		return nil, err
	}
	var pid int64
	var p *Project
	if project != "" {
		var err error
		if p, err = s.GetProject(ctx, a, project); err != nil {
			return nil, err
		}
		if p.Role != "owner" && p.Role != "editor" {
			return nil, forbidden("you can only view %s", p.Key)
		}
		pid = p.ID
	} else if in.Enabled != nil {
		return nil, invalid("turn autopilot on per project: give one")
	}
	if project != "" && (in.Limits.SpendLeft != nil || in.Limits.SpendAtOnce != nil) {
		return nil, invalid("using what's left at the week's end is set for all your projects at once: give no project")
	}
	var quotaOff int64 = -1
	if in.QuotaOffUntil != nil {
		if project != "" {
			return nil, invalid("the quota limits are turned off for all your projects at once: give no project")
		}
		quotaOff = 0
		if in.QuotaOffUntil.After(s.now()) {
			if in.QuotaOffUntil.Sub(s.now()) > autopilotQuotaOffMax {
				return nil, invalid("the quota limits can be off for a week at most")
			}
			quotaOff = in.QuotaOffUntil.UnixMilli()
		}
	}
	uid := a.id()
	turnedOff := false
	var heldKeys []string
	var heldSteps []int64
	var heldBefore []bool
	err := s.write(ctx, a, func(t *tx) error {
		rows, err := s.autopilotRows(ctx, t, uid)
		if err != nil {
			return err
		}
		r, existed := rows[pid]
		before := r.limits
		switch {
		case in.Reset:
			r.limits = AutopilotLimits{}
		case in.Merge:
			r.limits = r.limits.over(in.Limits)
		default:
			keep := r.limits
			r.limits = in.Limits
			if pid == 0 && r.limits.SpendLeft == nil {
				r.limits.SpendLeft = keep.SpendLeft
			}
			if pid == 0 && r.limits.SpendAtOnce == nil {
				r.limits.SpendAtOnce = keep.SpendAtOnce
			}
		}
		if in.Enabled != nil {
			turnedOff = r.enabled && !*in.Enabled
			r.enabled = *in.Enabled
		}
		wasOff := r.quotaOffUntil > s.nowMS()
		if quotaOff >= 0 {
			r.quotaOffUntil = quotaOff
		}
		raw, _ := json.Marshal(r.limits)
		var proj any
		if pid != 0 {
			proj = pid
		}
		now := s.nowMS()
		if existed {
			if _, err := t.ExecContext(ctx, `UPDATE autopilot_settings SET enabled = ?, limits = ?, quota_off_until = ?, updated_at = ? WHERE user_id = ? AND coalesce(project_id, 0) = ?`,
				boolInt(r.enabled), string(raw), r.quotaOffUntil, now, uid, pid); err != nil {
				return err
			}
		} else if _, err := t.ExecContext(ctx, `INSERT INTO autopilot_settings(user_id, project_id, enabled, limits, quota_off_until, updated_at) VALUES (?,?,?,?,?,?)`,
			uid, proj, boolInt(r.enabled), string(raw), r.quotaOffUntil, now); err != nil {
			return err
		}
		if quotaOff > 0 && !wasOff {
			if _, err := t.ExecContext(ctx, `UPDATE autopilot_settings SET quota_off_at = ? WHERE user_id = ? AND coalesce(project_id, 0) = ?`, now, uid, pid); err != nil {
				return err
			}
		}
		action, what := "autopilot.defaults", "autopilot defaults"
		if p != nil {
			action, what = "autopilot.settings", "autopilot in "+p.Key
			if in.Enabled != nil {
				what += map[bool]string{true: " (on)", false: " (off)"}[*in.Enabled]
			}
		}
		if quotaOff > 0 {
			what += ", quota limits off until " + msTime(quotaOff).UTC().Format("Jan 2 15:04 UTC")
		} else if quotaOff == 0 {
			what += ", quota limits on"
		}
		what += reviewProfileChange(before, r.limits)
		t.audit(AuditEntry{Action: action, Category: AuditControl, ProjectID: pid, TargetType: "project", TargetID: pid, Target: what})
		if quotaOff > 0 {
			if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET due_at = ?, updated_at = ? WHERE user_id = ? AND state = ? AND reason IN (?, ?, ?)`,
				now, now, uid, StepWaiting, PauseWeek, PauseWeekMax, PauseFiveHour); err != nil {
				return err
			}
		}
		if !turnedOff {
			scope, args := "project_id = ?", []any{pid}
			if pid == 0 {
				scope, args = "1", nil
			}
			if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET due_at = ?, updated_at = ? WHERE user_id = ? AND state = ? AND reason IN (?, ?, ?, ?, ?, ?) AND `+scope,
				append([]any{now, now, uid, StepWaiting, WaitRate, WaitSchedule, WaitPace, WaitAtOnce, WaitNothing, WaitMerge}, args...)...); err != nil {
				return err
			}
		}
		if turnedOff {
			if err := s.stopOwnStarts(ctx, t, uid, pid, "off", "autopilot was turned off"); err != nil {
				return err
			}
			type live struct{ step, session, round int64 }
			var steps []live
			rows, err := t.QueryContext(ctx, `SELECT id, session_id, coalesce(review_id, 0) FROM autopilot_steps WHERE user_id = ? AND project_id = ? AND session_id IS NOT NULL AND state IN (?, ?, ?)`,
				uid, pid, StepDue, StepWaiting, StepStarting)
			if err != nil {
				return err
			}
			for rows.Next() {
				var l live
				if err := rows.Scan(&l.step, &l.session, &l.round); err != nil {
					_ = rows.Close()
					return err
				}
				steps = append(steps, l)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			for _, l := range steps {
				key, err := roundOwed(ctx, t, l.round)
				if err != nil {
					return err
				}
				if key != "" {
					before, err := heldOwed(ctx, t, l.round)
					if err != nil {
						return err
					}
					heldKeys, heldSteps, heldBefore = append(heldKeys, key), append(heldSteps, l.step), append(heldBefore, before != "")
					if _, err := t.ExecContext(ctx, `UPDATE autopilot_steps SET due_at = ?, updated_at = ? WHERE id = ? AND state = ? AND due_at > ?`,
						now, now, l.step, StepWaiting, now); err != nil {
						return err
					}
					continue
				}
				if err := s.stopStep(ctx, t, l.session, "off", "autopilot was turned off"); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(heldKeys) > 0 {
		title := "Autopilot off: " + heldKeys[0] + "'s review still runs"
		if len(heldKeys) > 1 {
			title = fmt.Sprintf("Autopilot off: %d reviews still run", len(heldKeys))
		}
		s.notifyURL(ctx, uid, nil, NotifyAutopilot, title, heldGoesOnNotice(p.Key, heldKeys, heldSteps, heldBefore), "/agents?tab=autopilot")
	}
	return s.AutopilotOverview(ctx, a)
}

// TurnOffAutopilot turns autopilot off in every project it is on for, as
// the person would one at a time, and answers the overview.
func (s *Service) TurnOffAutopilot(ctx context.Context, a *Actor) (*AutopilotOverview, error) {
	o, err := s.AutopilotOverview(ctx, a)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, p := range o.Projects {
		if !p.Enabled {
			continue
		}
		if _, err := s.SetAutopilot(ctx, a, p.ProjectKey, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.ProjectKey, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return s.AutopilotOverview(ctx, a)
}

// autopilotMeter reads what the limits look at for the person.
func (s *Service) autopilotMeter(ctx context.Context, uid int64) (*AutopilotMeter, error) {
	now := s.now()
	m := &AutopilotMeter{MeasuredAt: now}
	usd, err := s.lastHourSpend(ctx, uid, now)
	if err != nil {
		return nil, err
	}
	m.HourlyUSD = math.Round(usd*100) / 100
	var off int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT quota_off_until FROM autopilot_settings WHERE user_id = ? AND project_id IS NULL`, uid).Scan(&off); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if off > now.UnixMilli() {
		m.QuotaOffUntil = ptrOf(msTime(off))
	}
	m.GuardHold = s.guardHoldFor(ctx, uid)
	read := func(span string) (pct float64, reset, at time.Time, ok bool, err error) {
		var a, r int64
		err = s.db.R.QueryRowContext(ctx, `SELECT at, percent, resets_at FROM quota_samples WHERE user_id = ? AND span = ? ORDER BY at DESC LIMIT 1`, uid, span).Scan(&a, &pct, &r)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, time.Time{}, time.Time{}, false, nil
		}
		return pct, msTime(r), msTime(a), err == nil, err
	}
	pct, reset, at, ok, err := read("seven_day")
	if err != nil {
		return nil, err
	}
	if ok && reset.After(now) && now.Sub(at) < quotaFresh {
		even := 100 * (1 - float64(reset.Sub(at))/float64(7*24*time.Hour))
		m.WeekPercent, m.WeekEven, m.WeekResetsAt, m.QuotaSampled = ptrOf(pct), ptrOf(math.Round(max(0, even)*10)/10), ptrOf(reset), ptrOf(at)
	} else {
		m.QuotaUnknown = true
	}
	pct, reset, at, ok, err = read("five_hour")
	if err != nil {
		return nil, err
	}
	if ok && reset.After(now) && now.Sub(at) < 5*time.Hour {
		m.FiveHour, m.FiveResetsAt = ptrOf(pct), ptrOf(reset)
	}
	return m, nil
}

// lastHourSpend is the person's spend over the last 60 minutes at list
// price, every machine and session. usage_hours has hourly rows, so the
// hour before this one counts for the part of it inside the window.
// quotaFresh is how old a quota reading the limits still trust.
const quotaFresh = 6 * time.Hour

func (s *Service) lastHourSpend(ctx context.Context, uid int64, now time.Time) (float64, error) {
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return 0, err
	}
	nowMS := now.UnixMilli()
	cur := nowMS - nowMS%3_600_000
	prevShare := 1 - float64(nowMS-cur)/3_600_000
	rows, err := s.db.R.QueryContext(ctx, `SELECT hour, model, sum(input), sum(output), sum(cache_write_5m), sum(cache_write_1h), sum(cache_read)
		FROM usage_hours WHERE user_id = ? AND hour >= ? GROUP BY hour, model`, uid, cur-3_600_000)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	total := 0.0
	for rows.Next() {
		var hour, in, out, cw5, cw1, cr int64
		var model string
		if err := rows.Scan(&hour, &model, &in, &out, &cw5, &cw1, &cr); err != nil {
			return 0, err
		}
		p, ok := book.find(model)
		if !ok {
			continue
		}
		c := p.Cost(in, out, cw5, cw1, cr)
		if hour < cur {
			c *= prevShare
		}
		total += c
	}
	return total, rows.Err()
}

// Pause reasons.
const (
	PauseHourly   = "hourly"
	PauseWeek     = "week"
	PauseWeekMax  = "week_max"
	PauseFiveHour = "five_hour"
	PauseGuard    = "quota_guard"
)

// guardPausePrefix starts autopilotPaused's words for a quota guard hold.
const guardPausePrefix = "the quota guard holds automation back: "

// autopilotPaused says which limit holds autopilot back now, in words and
// as a reason, and when to look again; "" when none does. An unknown quota
// (no fresh reading) holds nothing back: the hourly limit still applies.
// While the person has the quota limits off, only the hourly one holds:
// it is the guard against a runaway, not a share of the quota.
func autopilotPaused(m *AutopilotMeter, l AutopilotLimits, now time.Time) (why string, recheck time.Time) {
	if max := valueOf(l.MaxHourlyUSD); max > 0 && m.HourlyUSD >= max {
		return fmt.Sprintf("the last hour cost $%.0f, over the $%.0f limit", m.HourlyUSD, max), now.Add(5 * time.Minute)
	}
	if h := m.GuardHold; h != nil && h.Until.After(now) {
		return guardPausePrefix + h.Reason, minTime(h.Until, now.Add(5*time.Minute))
	}
	if m.QuotaOffUntil != nil && m.QuotaOffUntil.After(now) {
		return "", time.Time{}
	}
	return quotaPaused(m, l, now)
}

// quotaPaused is autopilotPaused's quota limits: the five-hour window, the
// week's ceiling and its pace.
func quotaPaused(m *AutopilotMeter, l AutopilotLimits, now time.Time) (why string, recheck time.Time) {
	if m.FiveHour != nil && m.FiveResetsAt != nil {
		if max := weekEndRamp(valueOf(l.FiveHourMax), m, l, now); max > 0 && *m.FiveHour >= max {
			return fmt.Sprintf("the five-hour window is at %.0f%% (the limit is %.0f%% now) until it resets at %s", *m.FiveHour, max, m.FiveResetsAt.UTC().Format("15:04 UTC")),
				m.FiveResetsAt.Add(time.Minute)
		}
	}
	if m.WeekPercent != nil && m.WeekEven != nil {
		if max := weekCap(m, l, now); max > 0 && *m.WeekPercent >= max {
			return fmt.Sprintf("the week's quota is at %.0f%% (the limit is %.0f%% now)", *m.WeekPercent, max), now.Add(15 * time.Minute)
		}
		if ahead := valueOf(l.WeekAhead); ahead > 0 && *m.WeekPercent-*m.WeekEven > ahead {
			return fmt.Sprintf("the week's quota is at %.0f%%, %.0f points ahead of an even pace (%.0f%%; the limit is %.0f ahead)",
				*m.WeekPercent, *m.WeekPercent-*m.WeekEven, *m.WeekEven, ahead), now.Add(15 * time.Minute)
		}
	}
	return "", time.Time{}
}

// weekCap is the week ceiling at now: the week max, rising in a straight
// line to 100% by week_end_hours before the reset (0: no ceiling).
func weekCap(m *AutopilotMeter, l AutopilotLimits, now time.Time) float64 {
	return weekEndRamp(valueOf(l.WeekMax), m, l, now)
}

// weekEndRamp raises ceiling c in a straight line from 2×week_end_hours
// before the reset to 100% at week_end_hours before it, and keeps it there:
// by the reset, quota left over is quota lost. The reset is the one Claude
// Code's status line reports (rate_limits.seven_day.resets_at).
func weekEndRamp(c float64, m *AutopilotMeter, l AutopilotLimits, now time.Time) float64 {
	if c <= 0 || c >= 100 || m.WeekResetsAt == nil {
		return c
	}
	if end := valueOf(l.WeekEndHours); end > 0 {
		if left := m.WeekResetsAt.Sub(now).Hours(); left < 2*end {
			c += (100 - c) * min(1, (2*end-max(left, 0))/end)
		}
	}
	return c
}

// pauseReason names the limit in why, for telling the person once per reason.
func pauseReason(why string) string {
	switch {
	case strings.HasPrefix(why, "the last hour"):
		return PauseHourly
	case strings.HasPrefix(why, guardPausePrefix):
		return PauseGuard
	case strings.HasPrefix(why, "the five-hour"):
		return PauseFiveHour
	case strings.Contains(why, "points ahead"):
		return PauseWeek
	case strings.HasPrefix(why, "the week"):
		return PauseWeekMax
	}
	return "limit"
}

func valueOf[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
