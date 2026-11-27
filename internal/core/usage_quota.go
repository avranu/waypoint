package core

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/quota"
)

// UsageQuotaPoint is one status line sample.
type UsageQuotaPoint struct {
	At       time.Time `json:"at"`
	Percent  float64   `json:"percent"`
	ResetsAt time.Time `json:"resets_at"`
}

// UsageQuotaWindow is one window (five hours, seven days) as last seen,
// with where it is heading.
type UsageQuotaWindow struct {
	Span      string     `json:"span"` // five_hour, seven_day
	Label     string     `json:"label"`
	Seconds   int64      `json:"seconds"` // the window's length
	Percent   float64    `json:"percent"`
	ResetsAt  time.Time  `json:"resets_at"`
	SampledAt time.Time  `json:"sampled_at"`
	Machine   string     `json:"machine"`
	Status    string     `json:"status"`    // on_track, will_run_out, at_limit, too_early, reset
	Projected float64    `json:"projected"` // percent used when the window ends, at the recent rate
	RunsOutAt *time.Time `json:"runs_out_at"`
	Even      float64    `json:"even"` // percent used right now if spread evenly since paced_from
	// PacedFrom is when the pace began: the window's start, or the first
	// reading after the last reset inside it. Resets are those readings'
	// times, one for each reset, oldest first.
	PacedFrom time.Time         `json:"paced_from"`
	Resets    []time.Time       `json:"resets"`
	History   []UsageQuotaPoint `json:"history"`
}

// UsageQuota is the subscription's windows.
type UsageQuota struct {
	Windows []UsageQuotaWindow `json:"windows"`
}

var quotaSpans = []struct {
	span, label string
	length      time.Duration
	// halfLife is how fast use stops counting in the rate; lookback how far
	// before the current window the rate reads.
	halfLife, lookback time.Duration
}{
	// A day's half-life takes in a whole day and night; the week before
	// says how fast the person works when the window has only just begun.
	{"seven_day", "7 days", quotaWeek, 24 * time.Hour, quotaWeek},
	// A five-hour window starts when use does: what came before it was idle.
	{"five_hour", "5 hours", 5 * time.Hour, time.Hour, 0},
}

// quotaPace says where a window is heading: the use now, carried to the
// window's end at the recent rate. lines end with the window; earlier ones
// only inform the rate.
func quotaPace(now, sampledAt time.Time, lines []*quota.Line, halfLife time.Duration) (status string, projected float64, runsOut *time.Time, even float64) {
	l := lines[len(lines)-1]
	pct, resetsAt := l.Last().Pct, msTime(l.Reset)
	if !resetsAt.After(now) {
		return "reset", pct, nil, 0
	}
	even = l.Even(l.Last().From, now.UnixMilli())
	if pct >= 100 {
		return "at_limit", pct, &sampledAt, even
	}
	rate, span := quota.Rate(lines, sampledAt.UnixMilli(), halfLife)
	if span < time.Duration(l.Reset-l.Start)*time.Millisecond/20 {
		return "too_early", pct, nil, even
	}
	projected = pct + rate*float64(l.Reset-sampledAt.UnixMilli())
	if projected > 100 && rate > 0 {
		t := sampledAt.Add(time.Duration((100-pct)/rate) * time.Millisecond)
		if t.Before(resetsAt) {
			return "will_run_out", projected, &t, even
		}
	}
	return "on_track", projected, nil, even
}

// usageQuota reads the windows for one person; nil for several.
func (s *Service) usageQuota(ctx context.Context, sc *usageScope, days int) (*UsageQuota, error) {
	if sc.everyone || len(sc.uids) != 1 {
		return nil, nil
	}
	uid := sc.uids[0]
	now := time.UnixMilli(s.nowMS())
	q := &UsageQuota{Windows: []UsageQuotaWindow{}}
	for _, sp := range quotaSpans {
		var w UsageQuotaWindow
		var latest int64
		err := s.db.R.QueryRowContext(ctx, `SELECT machine, at FROM quota_samples WHERE user_id = ? AND span = ? ORDER BY at DESC, id DESC LIMIT 1`, uid, sp.span).Scan(&w.Machine, &latest)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		since := now.Add(-time.Duration(days) * 24 * time.Hour)
		if sp.span == "five_hour" {
			since = now.Add(-48 * time.Hour)
		}
		since = since.Add(-sp.length / 4)
		// Whole windows: the history's first, the ones the rate reads, and
		// the latest however long ago it was.
		load := minTime(minTime(since, now.Add(-sp.length-sp.lookback)), msTime(latest)).Add(-sp.length)
		lines, err := quotaLines(ctx, s.db.R, uid, sp.span, load.UnixMilli(), math.MaxInt64)
		if err != nil {
			return nil, err
		}
		if len(lines) == 0 {
			continue
		}
		cur := lines[len(lines)-1]
		first := len(lines) - 1
		for first > 0 && lines[first-1].Reset > cur.Start-sp.lookback.Milliseconds() {
			first--
		}
		w.Span, w.Label, w.Seconds = sp.span, sp.label, int64(sp.length/time.Second)
		w.Percent, w.SampledAt, w.ResetsAt = cur.Last().Pct, msTime(cur.SeenAt), msTime(cur.Reset)
		w.Status, w.Projected, w.RunsOutAt, w.Even = quotaPace(now, w.SampledAt, lines[first:], sp.halfLife)
		w.PacedFrom, w.Resets = msTime(cur.Last().From), []time.Time{}
		for _, at := range cur.Resets() {
			w.Resets = append(w.Resets, msTime(at))
		}
		w.History = quotaHistory(lines, since.UnixMilli())
		q.Windows = append(q.Windows, w)
	}
	if len(q.Windows) == 0 {
		return nil, nil
	}
	return q, nil
}

const quotaPoints = 400

// quotaHistory is the points of the windows that end after since, each window
// whole, thinned to about quotaPoints. The last point before each fall stays,
// so a window's use can be added up from what is left.
func quotaHistory(lines []*quota.Line, since int64) []UsageQuotaPoint {
	all := []UsageQuotaPoint{}
	keep := []bool{}
	for _, l := range lines {
		if l.Reset <= since {
			continue
		}
		for i := 1; i < len(l.Knots); i++ {
			k := l.Knots[i]
			all = append(all, UsageQuotaPoint{At: msTime(k.At), Percent: k.Pct, ResetsAt: msTime(l.Reset)})
			keep = append(keep, k.Zeroed || i == len(l.Knots)-1 || l.Knots[i+1].Zeroed)
		}
	}
	if len(all) <= quotaPoints {
		return all
	}
	step := float64(len(all)-1) / float64(quotaPoints-1)
	for i := 0; i < quotaPoints; i++ {
		keep[int(float64(i)*step)] = true
	}
	thin := make([]UsageQuotaPoint, 0, quotaPoints)
	for i, p := range all {
		if keep[i] {
			thin = append(thin, p)
		}
	}
	return thin
}

// allowRule is the permission rule that would let a refused call through.
func allowRule(tool, detail string) string {
	switch {
	case tool == "Bash" && detail != "":
		return "Bash(" + detail + ":*)"
	case tool == "":
		return ""
	}
	return tool
}

func (s *Service) usageRefusalsInto(ctx context.Context, sc *usageScope, loc *time.Location, o *UsageOverview) error {
	r := UsageRefusals{Reasons: []UsageReason{}, Daily: []UsageCountDay{}, Commands: []UsageRefusedCommand{}, Recent: []UsageRefusalRow{}}
	where, args := sc.where("at")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT tool, detail, reason, source, count(*) FROM permission_events WHERE `+where+` GROUP BY tool, detail, reason, source`, args...)
	if err != nil {
		return err
	}
	reasons := map[[2]string]int64{}
	cmds := map[[2]string]*UsageRefusedCommand{}
	hooks := map[[2]string]bool{}
	for rows.Next() {
		var tool, detail, reason, source string
		var n int64
		if err := rows.Scan(&tool, &detail, &reason, &source, &n); err != nil {
			rows.Close()
			return err
		}
		r.Total += n
		reasons[[2]string{reason, source}] += n
		k := [2]string{tool, detail}
		c := cmds[k]
		if c == nil {
			c = &UsageRefusedCommand{Tool: tool, Detail: detail}
			cmds[k] = c
		}
		c.Count += n
		if reason != "" && !stringIn(c.Reasons, reason) {
			c.Reasons = append(c.Reasons, reason)
		}
		if source == "hook" || source == "safety_check" {
			hooks[k] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for k, n := range reasons {
		r.Reasons = append(r.Reasons, UsageReason{Reason: k[0], Source: k[1], Count: n})
	}
	sort.Slice(r.Reasons, func(i, j int) bool {
		if r.Reasons[i].Count != r.Reasons[j].Count {
			return r.Reasons[i].Count > r.Reasons[j].Count
		}
		return r.Reasons[i].Reason < r.Reasons[j].Reason
	})
	for k, c := range cmds {
		sort.Strings(c.Reasons)
		if !hooks[k] && (c.Tool != "Bash" || !riskyCommand(c.Detail)) {
			c.Rule = allowRule(c.Tool, c.Detail)
		}
		r.Commands = append(r.Commands, *c)
	}
	sort.Slice(r.Commands, func(i, j int) bool {
		if r.Commands[i].Count != r.Commands[j].Count {
			return r.Commands[i].Count > r.Commands[j].Count
		}
		return r.Commands[i].Detail < r.Commands[j].Detail
	})
	if len(r.Commands) > usageTopRows {
		r.Commands = r.Commands[:usageTopRows]
	}
	// #nosec G202 -- as above
	rows, err = s.db.R.QueryContext(ctx, `SELECT at/3600000*3600000, count(*) FROM permission_events WHERE `+where+` GROUP BY 1`, args...)
	if err != nil {
		return err
	}
	days := map[string]int64{}
	for rows.Next() {
		var h, n int64
		if err := rows.Scan(&h, &n); err != nil {
			rows.Close()
			return err
		}
		days[time.UnixMilli(h).In(loc).Format(dateLayout)] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for d, n := range days {
		r.Daily = append(r.Daily, UsageCountDay{Date: d, Count: n})
	}
	sort.Slice(r.Daily, func(i, j int) bool { return r.Daily[i].Date < r.Daily[j].Date })
	if r.Recent, err = s.usageRefusalRows(ctx, sc, 0, usageRecentShown); err != nil {
		return err
	}
	o.Refusals = r
	return nil
}

func stringIn(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Service) usageRefusalRows(ctx context.Context, sc *usageScope, before int64, limit int) ([]UsageRefusalRow, error) {
	where, args := sc.where("at")
	if before > 0 {
		where, args = where+" AND id < ?", append(args, before)
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, at, tool, detail, reason, source, excerpt, session, repo, machine FROM permission_events WHERE `+where+` ORDER BY id DESC LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRefusalRow{}
	for rows.Next() {
		var r UsageRefusalRow
		var at int64
		if err := rows.Scan(&r.ID, &at, &r.Tool, &r.Detail, &r.Reason, &r.Source, &r.Excerpt, &r.Session, &r.Repo, &r.Machine); err != nil {
			return nil, err
		}
		r.At = msTime(at)
		if sc.hideCommands {
			r.Excerpt = usageCommandRedacted
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageRefusals pages through refused calls, newest first; before is the
// id of the last one seen.
func (s *Service) UsageRefusals(ctx context.Context, actor *Actor, q UsageQuery, before int64, limit int) ([]UsageRefusalRow, error) {
	sc, _, _, _, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
	return s.usageRefusalRows(ctx, sc, before, min(max(limit, 1), 200))
}

// UsageBreakdowns pages a breakdown past the overview's top rows: by repo,
// machine, session, kind or model.
func (s *Service) UsageBreakdowns(ctx context.Context, actor *Actor, q UsageQuery, by string, limit int) ([]UsageBreakdown, error) {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	sc, loc, _, _, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return nil, err
	}
	b, err := s.usageTokens(ctx, sc, book, loc)
	if err != nil {
		return nil, err
	}
	o := &UsageOverview{}
	repoKey, err := s.repoProjects(ctx, actor)
	if err != nil {
		return nil, err
	}
	var list []UsageBreakdown
	switch by {
	case "repo":
		list = b.breakdowns(b.repos, func(k string) (string, string) { return cmpOr(k, "(no repository)"), "" })
		for i := range list {
			list[i].Project = repoKey[strings.ToLower(list[i].Key)]
		}
	case "machine":
		list = b.breakdowns(b.machine, nil)
	case "kind":
		list = b.breakdowns(b.kinds, nil)
	case "model":
		list = b.breakdowns(b.models, nil)
	case "session":
		if err := s.usageSessionMetaN(ctx, sc, o, b, repoKey, limit); err != nil {
			return nil, err
		}
		list = o.TopSess
	default:
		return nil, invalid("by is repo, machine, session, kind or model")
	}
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}
