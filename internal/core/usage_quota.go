package core

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
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
	Span      string            `json:"span"` // five_hour, seven_day
	Label     string            `json:"label"`
	Seconds   int64             `json:"seconds"` // the window's length
	Percent   float64           `json:"percent"`
	ResetsAt  time.Time         `json:"resets_at"`
	SampledAt time.Time         `json:"sampled_at"`
	Machine   string            `json:"machine"`
	Status    string            `json:"status"`    // on_track, will_run_out, at_limit, too_early, reset
	Projected float64           `json:"projected"` // percent used when the window ends, at this rate
	RunsOutAt *time.Time        `json:"runs_out_at"`
	Even      float64           `json:"even"` // percent used right now if spread evenly
	History   []UsageQuotaPoint `json:"history"`
}

// UsageQuota is the subscription's windows.
type UsageQuota struct {
	Windows []UsageQuotaWindow `json:"windows"`
}

var quotaSpans = []struct {
	span, label string
	length      time.Duration
}{
	{"seven_day", "7 days", 7 * 24 * time.Hour},
	{"five_hour", "5 hours", 5 * time.Hour},
}

// quotaPace says where a window is heading: its average rate so far,
// carried to the window's end.
func quotaPace(now, sampledAt, resetsAt time.Time, length time.Duration, pct float64) (status string, projected float64, runsOut *time.Time, even float64) {
	if !resetsAt.After(now) {
		return "reset", pct, nil, 0
	}
	start := resetsAt.Add(-length)
	if e := now.Sub(start); e > 0 {
		even = min(100, 100*float64(e)/float64(length))
	}
	if pct >= 100 {
		return "at_limit", pct, &sampledAt, even
	}
	elapsed := sampledAt.Sub(start)
	if elapsed < length/20 {
		return "too_early", pct, nil, even
	}
	rate := pct / float64(elapsed)
	projected = rate * float64(length)
	if projected > 100 && rate > 0 {
		t := sampledAt.Add(time.Duration((100 - pct) / rate))
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
		var at, reset int64
		err := s.db.R.QueryRowContext(ctx, `SELECT at, percent, resets_at, machine FROM quota_samples WHERE user_id = ? AND span = ? ORDER BY at DESC LIMIT 1`, uid, sp.span).
			Scan(&at, &w.Percent, &reset, &w.Machine)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		w.Span, w.Label, w.Seconds = sp.span, sp.label, int64(sp.length/time.Second)
		w.SampledAt, w.ResetsAt = msTime(at), msTime(reset)
		w.Status, w.Projected, w.RunsOutAt, w.Even = quotaPace(now, w.SampledAt, w.ResetsAt, sp.length, w.Percent)
		since := now.Add(-time.Duration(days) * 24 * time.Hour)
		if sp.span == "five_hour" {
			since = now.Add(-48 * time.Hour)
		}
		since = since.Add(-sp.length / 4)
		if w.History, err = s.quotaHistory(ctx, uid, sp.span, since.UnixMilli()); err != nil {
			return nil, err
		}
		q.Windows = append(q.Windows, w)
	}
	if len(q.Windows) == 0 {
		return nil, nil
	}
	return q, nil
}

const quotaPoints = 400

func (s *Service) quotaHistory(ctx context.Context, uid int64, span string, since int64) ([]UsageQuotaPoint, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT at, percent, resets_at FROM quota_samples WHERE user_id = ? AND span = ? AND at >= ? ORDER BY at`, uid, span, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageQuotaPoint{}
	for rows.Next() {
		var at, reset int64
		var p UsageQuotaPoint
		if err := rows.Scan(&at, &p.Percent, &reset); err != nil {
			return nil, err
		}
		p.At, p.ResetsAt = msTime(at), msTime(reset)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > quotaPoints {
		step := float64(len(out)-1) / float64(quotaPoints-1)
		thin := make([]UsageQuotaPoint, 0, quotaPoints)
		for i := 0; i < quotaPoints; i++ {
			thin = append(thin, out[int(float64(i)*step)])
		}
		out = thin
	}
	return out, nil
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
