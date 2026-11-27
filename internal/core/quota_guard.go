package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/quota"
)

// EvQuotaGuard: a daily check found automation used quota its limits should
// have held back (WP-932). It reaches only its user.
const EvQuotaGuard = "usage.quota_guard"

const (
	quotaGuardEvery = 24 * time.Hour
	// quotaGuardLag keeps the hour before this one out: usage uploads and
	// quota readings trail.
	quotaGuardLag = time.Hour
	// quotaGuardOverMax is how many points automation may use while the
	// limits hold it back before it is a breach: the gates stop new starts,
	// so a session already running finishes its turn.
	quotaGuardOverMax = 3.0
	// quotaGuardSpendSlack is how early a use-what's-left start may be: the
	// sweep reads a quota reading a few minutes old.
	quotaGuardSpendSlack = 15 * time.Minute
	quotaGuardKeep       = 90 * 24 * time.Hour
	quotaGuardMaxDays    = 7
	quotaWeek            = quota.Week
)

// The phases of the week an hour falls in, as autopilot's gates saw it at
// the hour's start.
const (
	GuardPhaseUnder   = "under"
	GuardPhaseAhead   = "ahead"
	GuardPhaseEnd     = "end"
	GuardPhaseOver    = "over"
	GuardPhaseOff     = "off"
	GuardPhaseUnknown = "unknown"
)

var guardPhases = []string{GuardPhaseUnder, GuardPhaseAhead, GuardPhaseEnd, GuardPhaseOver, GuardPhaseOff, GuardPhaseUnknown}

// QuotaGuardCheck is one check: a window of whole hours and what the
// quota did in it.
type QuotaGuardCheck struct {
	ID   int64     `json:"id,omitempty"`
	At   time.Time `json:"at"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// The week at the window's end, as its last reading had it.
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
	WeekPercent *float64   `json:"week_percent,omitempty"`
	WeekEven    *float64   `json:"week_even,omitempty"`
	// Points: how far the week's readings rose in the window. Unexplained:
	// the part that fell where no usage was reported, so nobody's.
	Points      float64 `json:"points"`
	Unexplained float64 `json:"unexplained"`
	Automation  float64 `json:"automation"`
	Person      float64 `json:"person"`
	// ByPhase is automation's points by the phase of the week.
	ByPhase       map[string]float64 `json:"by_phase"`
	CostUSD       float64            `json:"cost_usd"`
	AutomationUSD float64            `json:"automation_usd"`
	Limits        QuotaGuardLimits   `json:"limits"`
	EarlySpend    []QuotaGuardStart  `json:"early_spend"`
	Sources       []QuotaGuardSource `json:"sources"`
	Hours         []QuotaGuardHour   `json:"hours"`
	Breach        bool               `json:"breach"`
	Reasons       []string           `json:"reasons"`
}

// QuotaGuardLimits is what the check held automation to: the loosest of
// the person's autopilot settings across the projects it is on in.
type QuotaGuardLimits struct {
	WeekAhead    float64    `json:"week_ahead"`
	WeekMax      float64    `json:"week_max"`
	WeekEndHours float64    `json:"week_end_hours"`
	SpendHours   float64    `json:"spend_hours"` // use-what's-left's window (the person's own week_end_hours)
	OverMax      float64    `json:"over_max"`
	OffFrom      *time.Time `json:"off_from,omitempty"` // when they went off, if known
	OffUntil     *time.Time `json:"off_until,omitempty"`
}

// QuotaGuardHour is one hour of the window.
type QuotaGuardHour struct {
	Hour          time.Time `json:"hour"`
	Phase         string    `json:"phase"`
	Percent       *float64  `json:"percent,omitempty"` // the week's, at the hour's start
	Even          *float64  `json:"even,omitempty"`
	Points        float64   `json:"points"`
	Automation    float64   `json:"automation"`
	CostUSD       float64   `json:"cost_usd"`
	AutomationUSD float64   `json:"automation_usd"`
}

// QuotaGuardSource is a session or runner job automation spent through.
type QuotaGuardSource struct {
	Kind    string  `json:"kind"` // session or job
	ID      int64   `json:"id,omitempty"`
	Title   string  `json:"title"`
	Points  float64 `json:"points"`
	Over    float64 `json:"over"` // of them, while the limits held autopilot back
	CostUSD float64 `json:"cost_usd"`
}

// QuotaGuardStart is a use-what's-left start before its window opened.
type QuotaGuardStart struct {
	At        time.Time `json:"at"`
	Issue     string    `json:"issue,omitempty"`
	HoursLeft float64   `json:"hours_left"`
}

// QuotaGuardReport is the stored checks of a period, and a fresh one when
// asked for.
type QuotaGuardReport struct {
	Who    string             `json:"who"`
	Days   int                `json:"days"`
	Checks []*QuotaGuardCheck `json:"checks"`
	Now    *QuotaGuardCheck   `json:"now,omitempty"`
}

// QuotaGuard is the person's checks over the last days, newest first; with
// fresh, also a check of the last 24 hours, not stored.
func (s *Service) QuotaGuard(ctx context.Context, actor *Actor, q UsageQuery, fresh bool) (*QuotaGuardReport, error) {
	uids, everyone, label, err := s.usageWho(ctx, actor, q.User)
	if err != nil {
		return nil, err
	}
	if everyone {
		return nil, invalid("the quota guard checks one person's quota: name them")
	}
	if q.Days <= 0 {
		q.Days = 14
	}
	q.Days = min(q.Days, 90)
	uid := uids[0]
	rep := &QuotaGuardReport{Who: label, Days: q.Days, Checks: []*QuotaGuardCheck{}}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, report FROM quota_guard_checks WHERE user_id = ? AND to_at > ? ORDER BY to_at DESC, id DESC`,
		uid, s.nowMS()-int64(q.Days)*24*time.Hour.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		c := &QuotaGuardCheck{}
		if json.Unmarshal([]byte(raw), c) != nil {
			continue
		}
		c.ID = id
		rep.Checks = append(rep.Checks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if fresh {
		to := s.now().UTC().Truncate(time.Hour).Add(-quotaGuardLag)
		if rep.Now, err = s.quotaGuardCheck(ctx, uid, to.Add(-quotaGuardEvery), to); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// RunQuotaGuard checks each person whose quota was read in the last two
// days, once a day, and tells them of a breach. Called hourly.
func (s *Service) RunQuotaGuard(ctx context.Context) {
	now := s.now().UTC()
	to := now.Truncate(time.Hour).Add(-quotaGuardLag)
	toMS := to.UnixMilli()
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT user_id FROM quota_samples WHERE span = 'seven_day' AND at >= ?`, toMS-2*24*time.Hour.Milliseconds())
	if err != nil {
		s.log.WarnContext(ctx, "quota guard", "err", err)
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
		var last sql.NullInt64
		if err := s.db.R.QueryRowContext(ctx, `SELECT max(to_at) FROM quota_guard_checks WHERE user_id = ?`, uid).Scan(&last); err != nil {
			s.log.WarnContext(ctx, "quota guard", "user", uid, "err", err)
			continue
		}
		from := to.Add(-quotaGuardEvery)
		if last.Valid {
			if last.Int64 > from.UnixMilli() {
				continue
			}
			from = msTime(max(last.Int64, to.Add(-quotaGuardMaxDays*24*time.Hour).UnixMilli())).UTC()
		}
		c, err := s.quotaGuardCheck(ctx, uid, from, to)
		if err != nil {
			s.log.WarnContext(ctx, "quota guard", "user", uid, "err", err)
			continue
		}
		if err := s.storeQuotaGuard(ctx, uid, c); err != nil {
			s.log.WarnContext(ctx, "quota guard", "user", uid, "err", err)
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM quota_guard_checks WHERE to_at < ?`, now.Add(-quotaGuardKeep).UnixMilli()); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// storeQuotaGuard keeps the check and, on a breach, tells the person.
func (s *Service) storeQuotaGuard(ctx context.Context, uid int64, c *QuotaGuardCheck) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.write(ctx, nil, func(t *tx) error {
		res, err := t.ExecContext(ctx, `INSERT INTO quota_guard_checks(user_id, at, from_at, to_at, breach, report) VALUES (?,?,?,?,?,?)`,
			uid, c.At.UnixMilli(), c.From.UnixMilli(), c.To.UnixMilli(), boolInt(c.Breach), string(raw))
		if err != nil {
			return err
		}
		if c.Breach {
			id, _ := res.LastInsertId()
			t.emit(&Event{Kind: EvQuotaGuard, UserID: uid, New: quotaGuardText(c),
				Data: map[string]any{"check": id, "automation": c.Automation, "over": c.ByPhase[GuardPhaseOver], "early_spend": len(c.EarlySpend)}})
		}
		return nil
	})
}

// quotaGuardText is the notification's body: the window, then each reason.
func quotaGuardText(c *QuotaGuardCheck) string {
	return fmt.Sprintf("%s to %s: %s. See wp usage guard.", c.From.UTC().Format("Jan 2 15:04"), c.To.UTC().Format("Jan 2 15:04 UTC"), strings.Join(c.Reasons, "; "))
}

func (s *Service) notifyQuotaGuard(ctx context.Context, e *Event) {
	s.notifyURL(ctx, e.UserID, e, NotifyQuota, "Automation used quota its limits should have saved", strings.TrimSpace(e.New), "/agents?tab=autopilot")
}

// quotaWeekLines reads the person's seven_day readings from since to before
// as weeks.
func (s *Service) quotaWeekLines(ctx context.Context, uid, since, before int64) ([]*quota.Line, error) {
	return quotaLines(ctx, s.db.R, uid, "seven_day", since, before)
}

// guardHour is an hour's spending, by who.
type guardHour struct {
	cost, auto float64
	sources    map[string]float64
	models     map[string]float64
}

// quotaGuardCheck judges [from, to): whole hours.
func (s *Service) quotaGuardCheck(ctx context.Context, uid int64, from, to time.Time) (*QuotaGuardCheck, error) {
	from, to = from.UTC().Truncate(time.Hour), to.UTC().Truncate(time.Hour)
	c := &QuotaGuardCheck{At: s.now().UTC(), From: from, To: to, ByPhase: map[string]float64{}, EarlySpend: []QuotaGuardStart{}, Sources: []QuotaGuardSource{}, Hours: []QuotaGuardHour{}, Reasons: []string{}}
	for _, p := range guardPhases {
		c.ByPhase[p] = 0
	}
	if !to.After(from) {
		return c, nil
	}
	lim, spendEnd, offUntil, err := s.quotaGuardLimits(ctx, uid)
	if err != nil {
		return nil, err
	}
	c.Limits = QuotaGuardLimits{WeekAhead: valueOf(lim.WeekAhead), WeekMax: valueOf(lim.WeekMax), WeekEndHours: valueOf(lim.WeekEndHours), SpendHours: spendEnd, OverMax: quotaGuardOverMax}
	off := guardOff{until: offUntil}
	if offUntil > from.UnixMilli() {
		c.Limits.OffUntil = ptrOf(msTime(offUntil).UTC())
		if err := s.db.R.QueryRowContext(ctx, `SELECT quota_off_at FROM autopilot_settings WHERE user_id = ? AND project_id IS NULL`, uid).Scan(&off.from); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if off.from > 0 {
			c.Limits.OffFrom = ptrOf(msTime(off.from).UTC())
		}
	}
	hourMS := time.Hour.Milliseconds()
	fromMS, toMS := from.UnixMilli(), to.UnixMilli()
	until := max(toMS, s.nowMS())
	weeks, err := s.quotaWeekLines(ctx, uid, fromMS-quotaWeek.Milliseconds(), until)
	if err != nil {
		return nil, err
	}
	kept := weeks[:0]
	for _, w := range weeks {
		if w.Reset > fromMS {
			kept = append(kept, w)
		}
	}
	weeks = kept
	five, err := quotaLines(ctx, s.db.R, uid, "five_hour", fromMS-quota.Fresh.Milliseconds(), toMS)
	if err != nil {
		return nil, err
	}
	first := fromMS
	for _, w := range weeks {
		first = min(first, w.Start)
	}
	first -= first % hourMS
	hours, err := s.guardSpending(ctx, uid, first, until, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	n := int((toMS - fromMS) / hourMS)
	pts := make([]float64, n)
	unexp := make([]float64, n)
	for _, w := range weeks {
		for i := 1; i < len(w.Knots); i++ {
			a, b := w.Knots[i-1], w.Knots[i]
			rise := w.Rise(i)
			if rise <= 0 || b.At <= fromMS || a.At >= toMS {
				continue
			}
			total := 0.0
			for h := a.At - a.At%hourMS; h < b.At; h += hourMS {
				if g := hours[h]; g != nil {
					total += g.cost * spanOverlap(h, h+hourMS, a.At, b.At) / float64(hourMS)
				}
			}
			for h := max(fromMS, a.At-a.At%hourMS); h < min(toMS, b.At); h += hourMS {
				idx := int((h - fromMS) / hourMS)
				ov := spanOverlap(h, h+hourMS, a.At, b.At)
				if total <= 0 {
					if b.At > a.At {
						unexp[idx] += rise * ov / float64(b.At-a.At)
					}
					continue
				}
				if g := hours[h]; g != nil {
					pts[idx] += rise * g.cost * ov / float64(hourMS) / total
				}
			}
		}
	}
	srcPts := map[string]*QuotaGuardSource{}
	for i := 0; i < n; i++ {
		h := fromMS + int64(i)*hourMS
		gh := QuotaGuardHour{Hour: msTime(h).UTC(), Points: round1(pts[i] + unexp[i])}
		gh.Phase, gh.Percent, gh.Even = guardPhase(lim, off, weeks, five, h)
		var auto float64
		if g := hours[h]; g != nil {
			gh.CostUSD, gh.AutomationUSD = round2(g.cost), round2(g.auto)
			c.CostUSD += g.cost
			c.AutomationUSD += g.auto
			if g.cost > 0 {
				auto = pts[i] * g.auto / g.cost
				for key, cost := range g.sources {
					src := srcPts[key]
					if src == nil {
						kind, id := splitSourceKey(key)
						src = &QuotaGuardSource{Kind: kind, ID: id}
						srcPts[key] = src
					}
					p := pts[i] * cost / g.cost
					src.Points += p
					src.CostUSD += cost
					if gh.Phase == GuardPhaseOver {
						src.Over += p
					}
				}
			}
		}
		gh.Automation = round1(auto)
		c.Points += pts[i] + unexp[i]
		c.Unexplained += unexp[i]
		c.Automation += auto
		c.ByPhase[gh.Phase] += auto
		c.Hours = append(c.Hours, gh)
	}
	c.Person = c.Points - c.Unexplained - c.Automation
	if len(weeks) > 0 {
		w := weeks[len(weeks)-1]
		if w.Reset > toMS-hourMS && len(w.Knots) > 1 {
			c.ResetsAt = ptrOf(msTime(w.Reset).UTC())
			c.WeekPercent = ptrOf(round1(w.At(toMS)))
			c.WeekEven = ptrOf(round1(w.Even(w.FromAt(toMS), toMS)))
		}
	}
	if err := s.guardEarlySpend(ctx, uid, c, weeks, spendEnd); err != nil {
		return nil, err
	}
	if err := s.guardSources(ctx, c, srcPts); err != nil {
		return nil, err
	}
	c.Points, c.Unexplained, c.Automation, c.Person = round1(c.Points), round1(c.Unexplained), round1(c.Automation), round1(max(0, c.Person))
	c.CostUSD, c.AutomationUSD = round2(c.CostUSD), round2(c.AutomationUSD)
	for p, v := range c.ByPhase {
		c.ByPhase[p] = round1(v)
	}
	if over := c.ByPhase[GuardPhaseOver]; over >= quotaGuardOverMax {
		c.Breach = true
		hrs := 0
		for _, h := range c.Hours {
			if h.Phase == GuardPhaseOver && h.Automation > 0 {
				hrs++
			}
		}
		c.Reasons = append(c.Reasons, fmt.Sprintf("automation used %.1f points of the week in %s while your autopilot limits held it back (%.0f or more is a breach)",
			over, plural1(hrs, "hour"), quotaGuardOverMax))
	}
	for _, e := range c.EarlySpend {
		c.Breach = true
		what := "a use-what's-left start"
		if e.Issue != "" {
			what += " (" + e.Issue + ")"
		}
		c.Reasons = append(c.Reasons, fmt.Sprintf("%s came %.1f hours before the week's reset; they start only in its last %.0f", what, e.HoursLeft, spendEnd))
	}
	return c, nil
}

// quotaGuardLimits is the loosest of the person's autopilot limits across
// the projects it is on in (their defaults when it is on in none), the
// window use-what's-left starts in, and until when the quota limits are off.
// A limit at 0 is off, so the loosest.
func (s *Service) quotaGuardLimits(ctx context.Context, uid int64) (lim AutopilotLimits, spendEnd float64, offUntil int64, err error) {
	rows, err := s.autopilotRows(ctx, s.db.R, uid)
	if err != nil {
		return lim, 0, 0, err
	}
	base := waypointAutopilotDefaults().over(rows[0].limits)
	spendEnd = valueOf(base.WeekEndHours)
	var ahead, wmax, end, five *float64
	loosest := func(dst **float64, v float64, zeroOff bool) {
		switch {
		case *dst == nil:
			*dst = ptrOf(v)
		case zeroOff && (v == 0 || **dst == 0):
			*dst = ptrOf(0.0)
		default:
			*dst = ptrOf(max(**dst, v))
		}
	}
	for pid, r := range rows {
		if pid == 0 || !r.enabled {
			continue
		}
		l := base.over(r.limits)
		loosest(&ahead, valueOf(l.WeekAhead), true)
		loosest(&wmax, valueOf(l.WeekMax), true)
		loosest(&end, valueOf(l.WeekEndHours), false)
		loosest(&five, valueOf(l.FiveHourMax), true)
	}
	lim = base
	if ahead != nil {
		lim.WeekAhead, lim.WeekMax, lim.WeekEndHours, lim.FiveHourMax = ahead, wmax, end, five
	}
	return lim, spendEnd, rows[0].quotaOffUntil, nil
}

// guardOff is when the person had the quota limits off: [from, until), from
// 0 when it isn't known (turned off before it was recorded).
type guardOff struct{ from, until int64 }

// guardPhase is the phase of the week at h, as autopilot's gates read it
// then (quotaPaused): from the latest readings taken by h, with the week's
// even pace when its reading was taken, and nothing from a reading older
// than quota.Fresh. It returns the week's percent and even pace it went by.
func guardPhase(lim AutopilotLimits, off guardOff, weeks, five []*quota.Line, h int64) (string, *float64, *float64) {
	if h < off.until && h >= off.from {
		return GuardPhaseOff, nil, nil
	}
	var w *quota.Line
	for _, x := range weeks {
		if h >= x.Start && h < x.Reset {
			w = x
		}
	}
	if w == nil {
		return GuardPhaseUnknown, nil, nil
	}
	k, ok := w.Seen(h)
	if !ok || h-k.At >= quota.Fresh.Milliseconds() {
		return GuardPhaseUnknown, nil, nil
	}
	pct, even := k.Pct, w.Even(k.From, k.At)
	now := msTime(h)
	m := &AutopilotMeter{WeekResetsAt: ptrOf(msTime(w.Reset))}
	p, e := ptrOf(round1(pct)), ptrOf(round1(even))
	for _, f := range five {
		if h < f.Start || h >= f.Reset {
			continue
		}
		if k, ok := f.Seen(h); ok && h-k.At < quota.Fresh.Milliseconds() {
			if c := weekEndRamp(valueOf(lim.FiveHourMax), m, lim, now); c > 0 && k.Pct >= c {
				return GuardPhaseOver, p, e
			}
		}
	}
	if c := weekCap(m, lim, now); c > 0 && pct >= c {
		return GuardPhaseOver, p, e
	}
	if ahead := valueOf(lim.WeekAhead); ahead > 0 && pct-even > ahead {
		return GuardPhaseOver, p, e
	}
	if end := valueOf(lim.WeekEndHours); end > 0 && float64(w.Reset-h) < 2*end*float64(time.Hour.Milliseconds()) {
		return GuardPhaseEnd, p, e
	}
	if pct >= even {
		return GuardPhaseAhead, p, e
	}
	return GuardPhaseUnder, p, e
}

// spanOverlap is how many ms [a, b) and [c, d) share.
func spanOverlap(a, b, c, d int64) float64 {
	return float64(max(0, min(b, d)-max(a, c)))
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// guardSpending is the person's usage per hour from first to before, at list
// price, and, in the window [from, to), how much of it was automation's, by
// source.
func (s *Service) guardSpending(ctx context.Context, uid, first, before, from, to int64) (map[int64]*guardHour, error) {
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]*guardHour{}
	get := func(h int64) *guardHour {
		g := out[h]
		if g == nil {
			g = &guardHour{sources: map[string]float64{}, models: map[string]float64{}}
			out[h] = g
		}
		return g
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT hour, machine, session, model, sum(input), sum(output), sum(cache_write_5m), sum(cache_write_1h), sum(cache_read)
		FROM usage_hours WHERE user_id = ? AND hour >= ? AND hour < ? GROUP BY hour, machine, session, model`, uid, first, before)
	if err != nil {
		return nil, err
	}
	type sessKey struct{ machine, session string }
	type sessHour struct {
		hour  int64
		cost  float64
		model string
	}
	bySession := map[sessKey][]sessHour{}
	for rows.Next() {
		var hour, in, outT, cw5, cw1, cr int64
		var machine, session, model string
		if err := rows.Scan(&hour, &machine, &session, &model, &in, &outT, &cw5, &cw1, &cr); err != nil {
			_ = rows.Close()
			return nil, err
		}
		p, ok := book.find(model)
		if !ok {
			continue
		}
		cost := p.Cost(in, outT, cw5, cw1, cr)
		if cost <= 0 {
			continue
		}
		get(hour).cost += cost
		if hour < from || hour >= to {
			continue
		}
		k := sessKey{machine, session}
		bySession[k] = append(bySession[k], sessHour{hour, cost, model})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	hourMS := time.Hour.Milliseconds()
	for k, list := range bySession {
		owners, sid, err := s.sessionOwners(ctx, uid, k.machine, k.session, to)
		if err != nil {
			return nil, err
		}
		if sid == 0 {
			continue
		}
		key := fmt.Sprintf("session:%d", sid)
		for _, sh := range list {
			share := owners.autoShare(sh.hour, sh.hour+hourMS)
			if share <= 0 {
				continue
			}
			g := get(sh.hour)
			g.auto += sh.cost * share
			g.sources[key] += sh.cost * share
			g.models[sh.model] += sh.cost * share
		}
	}
	jrows, err := s.db.R.QueryContext(ctx, `SELECT id, started_at, finished_at, coalesce(json_extract(result, '$.tokens.cost_usd'), 0) FROM runner_jobs
		WHERE owner_id = ? AND cli = 'claude' AND kind IN (?,?,?) AND source != 'manual'
		AND started_at IS NOT NULL AND finished_at IS NOT NULL AND started_at < ? AND finished_at >= ?`,
		uid, JobAgent, JobReview, JobAudit, before, first)
	if err != nil {
		return nil, err
	}
	defer jrows.Close()
	for jrows.Next() {
		var id, start, end int64
		var cost float64
		if err := jrows.Scan(&id, &start, &end, &cost); err != nil {
			return nil, err
		}
		if cost <= 0 {
			continue
		}
		end = max(end, start+1)
		key := fmt.Sprintf("job:%d", id)
		for h := start - start%hourMS; h < end; h += hourMS {
			if h < first || h >= before {
				continue
			}
			part := cost * spanOverlap(h, h+hourMS, start, end) / float64(end-start)
			g := get(h)
			g.cost += part
			g.auto += part
			g.sources[key] += part
		}
	}
	return out, jrows.Err()
}

// sessionOwner is who a session worked for from at on.
type sessionOwner struct {
	at   int64
	auto bool
}

// ownerLine is who a session worked for over time.
type ownerLine []sessionOwner

// sessionOwners is who the session worked for over time, from its prompts;
// sid is its agent_sessions id (0: Waypoint never heard of it).
func (s *Service) sessionOwners(ctx context.Context, uid int64, machine, cliID string, before int64) (ownerLine, int64, error) {
	var sid int64
	err := s.db.R.QueryRowContext(ctx, `SELECT id FROM agent_sessions WHERE user_id = ? AND machine = ? AND cli_id = ?`, uid, machine, cliID).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var started int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE next_session = ?`, sid).Scan(&started); err != nil {
		return nil, 0, err
	}
	line := ownerLine{{at: math.MinInt64, auto: started > 0}}
	rows, err := s.db.R.QueryContext(ctx, `SELECT detail, typed_kind, created_at FROM session_events WHERE session_id = ? AND kind = 'prompt' AND created_at < ? ORDER BY created_at, id`, sid, before)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var detail, typed string
		var at int64
		if err := rows.Scan(&detail, &typed, &at); err != nil {
			return nil, 0, err
		}
		auto, ok := promptOwner(detail, typed)
		if !ok || auto == line[len(line)-1].auto {
			continue
		}
		line = append(line, sessionOwner{at: at, auto: auto})
	}
	return line, sid, rows.Err()
}

// promptOwner says whether a prompt event is automation's (auto) or a
// person's; ok false when it keeps whoever came before it. Who typed it is
// what its hook recognised (typed, recogniseTyped), not how it reads.
func promptOwner(detail, typed string) (auto, ok bool) {
	switch {
	case typedAuto(typed):
		return true, true
	case typedByWaypointAlone(typed), isHarnessPrompt(pasteText(detail)):
		return false, false
	}
	return false, true
}

// autoShare is the share of [a, b) the session worked for automation.
func (l ownerLine) autoShare(a, b int64) float64 {
	if len(l) == 0 || b <= a {
		return 0
	}
	auto := 0.0
	for i, o := range l {
		end := int64(math.MaxInt64)
		if i+1 < len(l) {
			end = l[i+1].at
		}
		if o.auto {
			auto += spanOverlap(a, b, o.at, end)
		}
	}
	return auto / float64(b-a)
}

func splitSourceKey(key string) (string, int64) {
	kind, id, _ := strings.Cut(key, ":")
	var n int64
	_, _ = fmt.Sscan(id, &n)
	return kind, n
}

// guardSources names automation's ten biggest sources.
func (s *Service) guardSources(ctx context.Context, c *QuotaGuardCheck, srcs map[string]*QuotaGuardSource) error {
	list := make([]*QuotaGuardSource, 0, len(srcs))
	for _, v := range srcs {
		if v.Points >= 0.05 {
			list = append(list, v)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Points != list[j].Points {
			return list[i].Points > list[j].Points
		}
		return list[i].ID < list[j].ID
	})
	if len(list) > 10 {
		list = list[:10]
	}
	for _, v := range list {
		var err error
		switch v.Kind {
		case "session":
			var title, repo, issue string
			err = s.db.R.QueryRowContext(ctx, `SELECT x.title, x.repo, coalesce(p.key || '-' || i.number, '') FROM agent_sessions x
				LEFT JOIN issues i ON i.id = x.issue_id LEFT JOIN projects p ON p.id = i.project_id WHERE x.id = ?`, v.ID).Scan(&title, &repo, &issue)
			v.Title = strings.Join(nonEmpty(issue, title, repo), " · ")
		case "job":
			err = s.db.R.QueryRowContext(ctx, `SELECT kind || ': ' || title FROM runner_jobs WHERE id = ?`, v.ID).Scan(&v.Title)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		v.Points, v.Over, v.CostUSD = round1(v.Points), round1(v.Over), round2(v.CostUSD)
		c.Sources = append(c.Sources, *v)
	}
	return nil
}

func nonEmpty(parts ...string) []string {
	out := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// guardEarlySpend lists use-what's-left starts in the window made before
// the week's last spendEnd hours (with a little slack for the reading the
// sweep went by).
func (s *Service) guardEarlySpend(ctx context.Context, uid int64, c *QuotaGuardCheck, weeks []*quota.Line, spendEnd float64) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT st.created_at, coalesce(p.key || '-' || i.number, '') FROM autopilot_steps st
		LEFT JOIN issues i ON i.id = st.next_issue LEFT JOIN projects p ON p.id = i.project_id WHERE st.user_id = ? AND st.spend = 1 AND st.created_at >= ? AND st.created_at < ? ORDER BY st.created_at`, uid, c.From.UnixMilli(), c.To.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		var key string
		if err := rows.Scan(&at, &key); err != nil {
			return err
		}
		var w *quota.Line
		for _, x := range weeks {
			if at >= x.Start && at < x.Reset {
				w = x
			}
		}
		if w == nil {
			continue
		}
		left := time.Duration(w.Reset-at) * time.Millisecond
		if left > time.Duration(spendEnd*float64(time.Hour))+quotaGuardSpendSlack {
			c.EarlySpend = append(c.EarlySpend, QuotaGuardStart{At: msTime(at).UTC(), Issue: key, HoursLeft: round1(left.Hours())})
		}
	}
	return rows.Err()
}
