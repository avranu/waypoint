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
	quotaWeek            = 7 * 24 * time.Hour
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
	// Resets are the allowance resets inside the window (WP-1881). A check
	// stored before they were read has none (null); ResetNote says so when the
	// window held one, and its figures are kept as stored.
	Resets    []QuotaGuardReset `json:"resets"`
	ResetNote string            `json:"reset_note,omitempty"`
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
	// Path says which computation made the figures above: "stored" (the
	// daily check, kept when it ran) or "now" (computed for this request).
	Path string `json:"path,omitempty"`
	// Split is cost_usd by source, recomputed when read (QuotaGuardSplit).
	Split   *QuotaGuardSplit `json:"split,omitempty"`
	Breach  bool             `json:"breach"`
	Reasons []string         `json:"reasons"`
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
	// Split is CostUSD by source, recomputed when read.
	Split []QuotaGuardPart `json:"split,omitempty"`
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
		c.Path = GuardPathStored
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
		rep.Now.Path = GuardPathNow
	}
	for _, c := range append(append([]*QuotaGuardCheck{}, rep.Checks...), rep.Now) {
		if c == nil {
			continue
		}
		if c.Resets == nil {
			if err := s.guardResetsOnRead(ctx, uid, c); err != nil {
				return nil, err
			}
		}
		if err := s.guardSplitInto(ctx, uid, c); err != nil {
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

// quotaKnot is the week's high-water mark at a time.
type quotaKnot struct {
	at  int64
	pct float64
}

// QuotaGuardReset is the allowance restarting mid-week: the percent it stood
// at, and when the first reading below it came.
type QuotaGuardReset struct {
	At         time.Time `json:"at"`
	WasPercent float64   `json:"was_percent"`
}

// quotaWeekLine is one week's readings, as a rising line from 0 at its start
// (and again from 0 at each allowance reset).
type quotaWeekLine struct {
	start, reset int64
	// resets are the allowance resets in the week, oldest first: the even
	// pace at a time starts from the last one at or before it (weekEvenAt).
	resets []quotaReset
	raw    []quotaSample
	knots  []quotaKnot
}

// at is the week's mark at t: the line between the readings around t, the
// last reading after them.
func (w *quotaWeekLine) at(t int64) float64 {
	k := w.knots
	for i := 1; i < len(k); i++ {
		if t <= k[i].at {
			span := float64(k[i].at - k[i-1].at)
			if span <= 0 {
				return k[i].pct
			}
			return k[i-1].pct + (k[i].pct-k[i-1].pct)*float64(t-k[i-1].at)/span
		}
	}
	return k[len(k)-1].pct
}

// seen is the latest reading taken by t, as the gates had it then.
func (w *quotaWeekLine) seen(t int64) (quotaKnot, bool) {
	for i := len(w.knots) - 1; i >= 1; i-- {
		if w.knots[i].at <= t {
			return w.knots[i], true
		}
	}
	return quotaKnot{}, false
}

// quotaWeekLines reads the person's seven_day readings from since to before
// as weeks. Readings of one week can name resets a little apart; one more
// than an hour from the week's is the next week, and a stale one naming an
// earlier week is left out.
func (s *Service) quotaWeekLines(ctx context.Context, uid, since, before int64) ([]*quotaWeekLine, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT at, percent, resets_at FROM quota_samples WHERE user_id = ? AND span = 'seven_day' AND at >= ? AND at < ? ORDER BY at, id`,
		uid, since, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var weeks []*quotaWeekLine
	var cur *quotaWeekLine
	hour := time.Hour.Milliseconds()
	for rows.Next() {
		var at, reset int64
		var pct float64
		if err := rows.Scan(&at, &pct, &reset); err != nil {
			return nil, err
		}
		switch {
		case cur != nil && reset < cur.reset-hour:
			continue
		case cur == nil || reset > cur.reset+hour:
			cur = &quotaWeekLine{start: reset - quotaWeek.Milliseconds(), reset: reset}
			weeks = append(weeks, cur)
		}
		if at < cur.start {
			continue
		}
		cur.raw = append(cur.raw, quotaSample{at, pct, reset})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, w := range weeks {
		w.build()
	}
	return weeks, nil
}

// build turns the week's readings into its line. A reset (quotaResetsIn)
// starts a new segment: the line holds to the reset's time, then restarts
// from the first reading after it, and the high-water mark restarts too.
func (w *quotaWeekLine) build() {
	kept, resets, startAt := quotaResetsIn(w.raw)
	w.resets = resets
	w.knots = []quotaKnot{{w.start, 0}}
	next := 0
	for i, x := range kept {
		last := w.knots[len(w.knots)-1]
		if next < len(startAt) && startAt[next] == i {
			next++
			w.knots = append(w.knots, quotaKnot{x.at, last.pct}, quotaKnot{x.at, x.pct})
			continue
		}
		w.knots = append(w.knots, quotaKnot{x.at, max(last.pct, x.pct)})
	}
}

// guardHour is an hour's spending, by who.
type guardHour struct {
	cost, auto float64
	sources    map[string]float64
	models     map[string]float64
	// machines is the cost of CLI sessions by the machine that reported
	// them ("" is a row that names none); jobs is the runner jobs' cost.
	machines map[string]float64
	jobs     float64
	// jobSessions are the sources (job:N) of runner jobs whose cost came from
	// their session's usage hours, so a baseline counts each as a session.
	jobSessions map[string]bool
}

// quotaGuardCheck judges [from, to): whole hours.
func (s *Service) quotaGuardCheck(ctx context.Context, uid int64, from, to time.Time) (*QuotaGuardCheck, error) {
	from, to = from.UTC().Truncate(time.Hour), to.UTC().Truncate(time.Hour)
	c := &QuotaGuardCheck{At: s.now().UTC(), From: from, To: to, ByPhase: map[string]float64{}, Resets: []QuotaGuardReset{}, EarlySpend: []QuotaGuardStart{}, Sources: []QuotaGuardSource{}, Hours: []QuotaGuardHour{}, Reasons: []string{}}
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
		if w.reset > fromMS {
			kept = append(kept, w)
		}
	}
	weeks = kept
	five, err := s.fiveReadings(ctx, uid, fromMS-quotaFresh.Milliseconds(), toMS)
	if err != nil {
		return nil, err
	}
	first := fromMS
	for _, w := range weeks {
		first = min(first, w.start)
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
		for i := 1; i < len(w.knots); i++ {
			a, b := w.knots[i-1], w.knots[i]
			rise := b.pct - a.pct
			if rise <= 0 || b.at <= fromMS || a.at >= toMS {
				continue
			}
			total := 0.0
			for h := a.at - a.at%hourMS; h < b.at; h += hourMS {
				if g := hours[h]; g != nil {
					total += g.cost * spanOverlap(h, h+hourMS, a.at, b.at) / float64(hourMS)
				}
			}
			for h := max(fromMS, a.at-a.at%hourMS); h < min(toMS, b.at); h += hourMS {
				idx := int((h - fromMS) / hourMS)
				ov := spanOverlap(h, h+hourMS, a.at, b.at)
				if total <= 0 {
					if b.at > a.at {
						unexp[idx] += rise * ov / float64(b.at-a.at)
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
		if w.reset > toMS-hourMS && len(w.knots) > 1 {
			c.ResetsAt = ptrOf(msTime(w.reset).UTC())
			c.WeekPercent = ptrOf(round1(w.at(toMS)))
			c.WeekEven = ptrOf(round1(weekEvenAt(w, toMS)))
		}
	}
	for _, w := range weeks {
		for _, r := range w.resets {
			if r.At >= fromMS && r.At < toMS {
				c.Resets = append(c.Resets, QuotaGuardReset{At: msTime(r.At).UTC(), WasPercent: round1(r.Was)})
			}
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

// fiveReadings are the person's five_hour readings in [since, before).
func (s *Service) fiveReadings(ctx context.Context, uid, since, before int64) ([]fiveReading, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT at, percent, resets_at FROM quota_samples WHERE user_id = ? AND span = 'five_hour' AND at >= ? AND at < ? ORDER BY at, id`, uid, since, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fiveReading
	for rows.Next() {
		var f fiveReading
		if err := rows.Scan(&f.at, &f.pct, &f.reset); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// guardOff is when the person had the quota limits off: [from, until), from
// 0 when it isn't known (turned off before it was recorded).
type guardOff struct{ from, until int64 }

// fiveReading is one five_hour reading.
type fiveReading struct {
	at, reset int64
	pct       float64
}

// guardPhase is the phase of the week at h, as autopilot's gates read it
// then (quotaPaused): from the latest readings taken by h, with the week's
// even pace when its reading was taken, and nothing from a reading older
// than quotaFresh. It returns the week's percent and even pace it went by.
func guardPhase(lim AutopilotLimits, off guardOff, weeks []*quotaWeekLine, five []fiveReading, h int64) (string, *float64, *float64) {
	if h < off.until && h >= off.from {
		return GuardPhaseOff, nil, nil
	}
	var w *quotaWeekLine
	for _, x := range weeks {
		if h >= x.start && h < x.reset {
			w = x
		}
	}
	if w == nil {
		return GuardPhaseUnknown, nil, nil
	}
	k, ok := w.seen(h)
	if !ok || h-k.at >= quotaFresh.Milliseconds() {
		return GuardPhaseUnknown, nil, nil
	}
	pct, even := k.pct, weekEvenAt(w, k.at)
	now := msTime(h)
	m := &AutopilotMeter{WeekResetsAt: ptrOf(msTime(w.reset))}
	p, e := ptrOf(round1(pct)), ptrOf(round1(even))
	for i := len(five) - 1; i >= 0; i-- {
		f := five[i]
		if f.at > h {
			continue
		}
		if f.reset > h && h-f.at < quotaFresh.Milliseconds() {
			if c := weekEndRamp(valueOf(lim.FiveHourMax), m, lim, now); c > 0 && f.pct >= c {
				return GuardPhaseOver, p, e
			}
		}
		break
	}
	if c := weekCap(m, lim, now); c > 0 && pct >= c {
		return GuardPhaseOver, p, e
	}
	if ahead := valueOf(lim.WeekAhead); ahead > 0 && pct-even > ahead {
		return GuardPhaseOver, p, e
	}
	if end := valueOf(lim.WeekEndHours); end > 0 && float64(w.reset-h) < 2*end*float64(time.Hour.Milliseconds()) {
		return GuardPhaseEnd, p, e
	}
	if pct >= even {
		return GuardPhaseAhead, p, e
	}
	return GuardPhaseUnder, p, e
}

// weekEvenAt is an even pace's share of the allowance at t: of the week, or,
// after an allowance reset, of what remained from the reset in force at t (the
// last at or before it) to resets_at. A reset after t does not touch t.
func weekEvenAt(w *quotaWeekLine, t int64) float64 {
	var from int64
	for _, r := range w.resets {
		if r.At <= t {
			from = r.At
		}
	}
	return quotaEven(from, w.reset, t)
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
			g = &guardHour{sources: map[string]float64{}, models: map[string]float64{}, machines: map[string]float64{}}
			out[h] = g
		}
		return g
	}
	jobs, err := s.guardJobs(ctx, uid, first, before)
	if err != nil {
		return nil, err
	}
	bySess := map[string]int64{} // CLI session id -> the runner job that ran it
	for _, j := range jobs {
		if j.session != "" {
			bySess[j.session] = j.id
		}
	}
	// A job counts as the larger of its cost_usd and its session's priced
	// hours: the hours carry what they can, and what they do not cover (a model
	// with no price, hours not uploaded yet) is added from cost_usd below. They
	// are the session's hours anywhere, not the range this call reads, so the
	// check and the split decide it alike.
	sessCost := map[int64]float64{}
	for sess, id := range bySess {
		mrows, err := s.db.R.QueryContext(ctx, `SELECT model, sum(input), sum(output), sum(cache_write_5m), sum(cache_write_1h), sum(cache_read)
			FROM usage_hours WHERE user_id = ? AND session = ? GROUP BY model`, uid, sess)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var model string
			var in, outT, cw5, cw1, cr int64
			if err := mrows.Scan(&model, &in, &outT, &cw5, &cw1, &cr); err != nil {
				_ = mrows.Close()
				return nil, err
			}
			if p, ok := book.find(model); ok {
				sessCost[id] += p.Cost(in, outT, cw5, cw1, cr)
			}
		}
		if err := mrows.Close(); err != nil {
			return nil, err
		}
		if err := mrows.Err(); err != nil {
			return nil, err
		}
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
		if id, ok := bySess[session]; ok {
			// A runner job's session reports its hours like any other, and
			// the job's own cost_usd is the same dollars: count them once,
			// here, as the job's, and leave the session out of the CLI part
			// and out of autoShare (the job is already automation's).
			g := get(hour)
			g.auto += cost
			g.jobs += cost
			key := fmt.Sprintf("job:%d", id)
			g.sources[key] += cost
			if hour >= from && hour < to {
				// Inside the window it is also a session and a model of
				// automation's, as the CLI sessions below are (guardBaseline
				// reads both).
				g.models[model] += cost
				if g.jobSessions == nil {
					g.jobSessions = map[string]bool{}
				}
				g.jobSessions[key] = true
			}
			continue
		}
		get(hour).machines[machine] += cost
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
	for _, j := range jobs {
		start, end := j.start, j.end
		cost := j.cost - sessCost[j.id]
		if cost <= 0 {
			continue
		}
		end = max(end, start+1)
		key := fmt.Sprintf("job:%d", j.id)
		for h := start - start%hourMS; h < end; h += hourMS {
			if h < first || h >= before {
				continue
			}
			part := cost * spanOverlap(h, h+hourMS, start, end) / float64(end-start)
			g := get(h)
			g.cost += part
			g.auto += part
			g.jobs += part
			g.sources[key] += part
		}
	}
	return out, nil
}

// guardJob is a runner job that ran Claude for automation.
type guardJob struct {
	id, start, end int64
	cost           float64
	session        string // the CLI session id the runner recorded in the result; "" on older jobs
}

// guardJobs is the automation's claude runner jobs that overlap [first, before).
func (s *Service) guardJobs(ctx context.Context, uid, first, before int64) ([]guardJob, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, started_at, finished_at, coalesce(json_extract(result, '$.tokens.cost_usd'), 0), coalesce(json_extract(result, '$.session_id'), '') FROM runner_jobs
		WHERE owner_id = ? AND cli = 'claude' AND kind IN (?,?,?) AND source != 'manual'
		AND started_at IS NOT NULL AND finished_at IS NOT NULL AND started_at < ? AND finished_at >= ?`,
		uid, JobAgent, JobReview, JobAudit, before, first)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []guardJob
	for rows.Next() {
		var j guardJob
		if err := rows.Scan(&j.id, &j.start, &j.end, &j.cost, &j.session); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
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
func (s *Service) guardEarlySpend(ctx context.Context, uid int64, c *QuotaGuardCheck, weeks []*quotaWeekLine, spendEnd float64) error {
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
		var w *quotaWeekLine
		for _, x := range weeks {
			if at >= x.start && at < x.reset {
				w = x
			}
		}
		if w == nil {
			continue
		}
		left := time.Duration(w.reset-at) * time.Millisecond
		if left > time.Duration(spendEnd*float64(time.Hour))+quotaGuardSpendSlack {
			c.EarlySpend = append(c.EarlySpend, QuotaGuardStart{At: msTime(at).UTC(), Issue: key, HoursLeft: round1(left.Hours())})
		}
	}
	return rows.Err()
}
