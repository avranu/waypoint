package core

// Before/after compares usage in the days before a moment against the days after it, per session, task and day, by medians with a Mann–Whitney p-value; few samples are reported, not hidden.

import (
	"context"
	"strings"
	"time"
)

// CompareQuery is the pivot and the window on each side of it.
type CompareQuery struct {
	At     time.Time
	Window int    // days on each side (default 7, at most 90)
	Repo   string // narrows to one repo
}

// CompareSide sums one side.
type CompareSide struct {
	N      int     `json:"n"`
	Median float64 `json:"median"`
	Mean   float64 `json:"mean"`
	Total  float64 `json:"total"`
}

// CompareStat is one measure before and after.
type CompareStat struct {
	Key    string      `json:"key"`
	Label  string      `json:"label"`
	Unit   string      `json:"unit"` // usd, tokens, count, ratio, hours
	Before CompareSide `json:"before"`
	After  CompareSide `json:"after"`
	Change *float64    `json:"change"` // after median / before median − 1; nil when before is 0
	P      *float64    `json:"p"`
}

// CompareWindow is one side's period.
type CompareWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Days float64   `json:"days"`
}

// UsageCompare is the before/after card.
type UsageCompare struct {
	At       time.Time     `json:"at"`
	Window   int           `json:"window"`
	Repo     string        `json:"repo,omitempty"`
	Who      string        `json:"who"`
	Before   CompareWindow `json:"before"`
	After    CompareWindow `json:"after"`
	Sessions []CompareStat `json:"sessions"`
	Tasks    []CompareStat `json:"tasks"`
	Daily    []CompareStat `json:"daily"`
	MinN     int           `json:"min_n"` // fewer samples than this on a side: too few to say
}

const compareMinN = 5

type compareUnit struct {
	acc                      usageAcc
	tools, errors, subagents int64
	calls, ctx               int64
	hours                    map[int64]bool
	sessions                 map[sessKeyAt]bool
}

func newCompareUnit() *compareUnit {
	return &compareUnit{hours: map[int64]bool{}, sessions: map[sessKeyAt]bool{}}
}

func (u *compareUnit) add(sk sessKeyAt, hour int64, b *trendBucket) {
	u.acc.addAcc(&b.acc)
	u.tools += b.tools
	u.errors += b.errors
	u.subagents += b.subagents()
	u.calls += b.calls
	u.ctx += b.ctx
	u.hours[hour] = true
	u.sessions[sk] = true
}

type compareMeasure struct {
	key, label, unit string
	val              func(u *compareUnit) (float64, bool)
}

func cmAlways(f func(u *compareUnit) float64) func(u *compareUnit) (float64, bool) {
	return func(u *compareUnit) (float64, bool) { return f(u), true }
}

var (
	cmCost      = compareMeasure{"cost", "Cost", "usd", cmAlways(func(u *compareUnit) float64 { return u.acc.cost })}
	cmTokens    = compareMeasure{"tokens", "Tokens (no cache reads)", "tokens", cmAlways(func(u *compareUnit) float64 { return float64(u.acc.in + u.acc.out + u.acc.cw5 + u.acc.cw1) })}
	cmMessages  = compareMeasure{"messages", "Messages", "count", cmAlways(func(u *compareUnit) float64 { return float64(u.acc.n) })}
	cmTools     = compareMeasure{"tool_calls", "Tool calls", "count", cmAlways(func(u *compareUnit) float64 { return float64(u.tools) })}
	cmErrors    = compareMeasure{"tool_errors", "Tool errors", "count", cmAlways(func(u *compareUnit) float64 { return float64(u.errors) })}
	cmSubagents = compareMeasure{"subagents", "Subagents", "count", cmAlways(func(u *compareUnit) float64 { return float64(u.subagents) })}
	cmHours     = compareMeasure{"active_hours", "Hours worked", "hours", cmAlways(func(u *compareUnit) float64 { return float64(len(u.hours)) })}
	cmSessions  = compareMeasure{"sessions", "Sessions", "count", cmAlways(func(u *compareUnit) float64 { return float64(len(u.sessions)) })}
	cmContext   = compareMeasure{"avg_context", "Average context", "tokens", func(u *compareUnit) (float64, bool) {
		if u.calls == 0 {
			return 0, false
		}
		return float64(u.ctx) / float64(u.calls), true
	}}
	cmHitRate = compareMeasure{"hit_rate", "Cache hit rate", "ratio", func(u *compareUnit) (float64, bool) {
		t := u.acc.totals()
		if t.HitRate == nil {
			return 0, false
		}
		return *t.HitRate, true
	}}
)

// UsageCompare compares the window before q.At with the window after it.
func (s *Service) UsageCompare(ctx context.Context, actor *Actor, uq UsageQuery, q CompareQuery) (*UsageCompare, error) {
	if q.At.IsZero() {
		return nil, invalid("at: the moment to compare around is required")
	}
	if q.Window <= 0 {
		q.Window = 7
	}
	q.Window = min(q.Window, 90)
	uq.Days = 1
	sc, loc, _, label, err := s.usageScopeFor(ctx, actor, &uq)
	if err != nil {
		return nil, err
	}
	at := q.At.UnixMilli()
	now := sc.to
	if at >= now {
		return nil, invalid("at: the moment must be in the past")
	}
	// The hour holding the pivot counts as after; for days, the pivot's own day and today are left out.
	pivot := at - at%hourMillis
	span := int64(q.Window) * 24 * hourMillis
	hourFrom, hourTo := pivot-span, min(pivot+span, now)
	pt := time.UnixMilli(at).In(loc)
	pivotDay := time.Date(pt.Year(), pt.Month(), pt.Day(), 0, 0, 0, 0, loc)
	nt := time.UnixMilli(now).In(loc)
	today := time.Date(nt.Year(), nt.Month(), nt.Day(), 0, 0, 0, 0, loc)
	dayFrom, dayTo := pivotDay.AddDate(0, 0, -q.Window), pivotDay.AddDate(0, 0, q.Window+1)
	if dayTo.After(today) {
		dayTo = today
	}
	sc.from, sc.to = min(hourFrom, dayFrom.UnixMilli()), max(hourTo, dayTo.UnixMilli())
	sc.narrowToRepo(q.Repo)
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return nil, err
	}
	buckets, err := s.trendBuckets(ctx, sc, book)
	if err != nil {
		return nil, err
	}
	attr, err := s.taskAttribution(ctx, actor, sc)
	if err != nil {
		return nil, err
	}
	o := &UsageCompare{At: msTime(at), Window: q.Window, Repo: strings.TrimSpace(q.Repo), Who: label, MinN: compareMinN,
		Before: CompareWindow{From: msTime(hourFrom), To: msTime(at), Days: float64(at-hourFrom) / float64(24*hourMillis)},
		After:  CompareWindow{From: msTime(at), To: msTime(hourTo), Days: float64(hourTo-at) / float64(24*hourMillis)}}
	var sess, tasks, days [2]map[string]*compareUnit
	for i := range 2 {
		sess[i], tasks[i], days[i] = map[string]*compareUnit{}, map[string]*compareUnit{}, map[string]*compareUnit{}
	}
	unit := func(m map[string]*compareUnit, k string) *compareUnit {
		u := m[k]
		if u == nil {
			u = newCompareUnit()
			m[k] = u
		}
		return u
	}
	for d := dayFrom; d.Before(pivotDay); d = d.AddDate(0, 0, 1) {
		unit(days[0], d.Format(dateLayout))
	}
	for d := pivotDay.AddDate(0, 0, 1); d.Before(dayTo); d = d.AddDate(0, 0, 1) {
		unit(days[1], d.Format(dateLayout))
	}
	for k, b := range buckets {
		sk := sessKeyAt{k.machine, k.session}
		ds := time.UnixMilli(k.hour).In(loc).Format(dateLayout)
		for side := range 2 {
			if u := days[side][ds]; u != nil {
				u.add(sk, k.hour, b)
			}
		}
		if k.hour < hourFrom || k.hour >= hourTo {
			continue
		}
		side := 0
		if k.hour >= pivot {
			side = 1
		}
		unit(sess[side], k.machine+"\x00"+k.session).add(sk, k.hour, b)
		if id := attr.taskOf(sk, k.hour); id != 0 {
			unit(tasks[side], itoa64(id)).add(sk, k.hour, b)
		}
	}
	o.Sessions = compareStats(sess, []compareMeasure{cmCost, cmTokens, cmMessages, cmTools, cmErrors, cmSubagents, cmContext, cmHitRate, cmHours})
	o.Tasks = compareStats(tasks, []compareMeasure{cmCost, cmTokens, cmTools, cmSubagents, cmSessions, cmContext, cmHours})
	o.Daily = compareStats(days, []compareMeasure{cmCost, cmTokens, cmMessages, cmTools, cmSessions, cmSubagents})
	return o, nil
}

// narrowToRepo keeps one repo of the scope's ("" keeps them all).
func (sc *usageScope) narrowToRepo(repo string) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return
	}
	if !sc.repoFilter {
		sc.repos, sc.repoFilter = []string{repo}, true
		return
	}
	var keep []string
	for _, r := range sc.repos {
		if strings.EqualFold(r, repo) {
			keep = append(keep, r)
		}
	}
	sc.repos = keep
}

func compareStats(sides [2]map[string]*compareUnit, measures []compareMeasure) []CompareStat {
	out := make([]CompareStat, 0, len(measures))
	for _, m := range measures {
		var vals [2][]float64
		for i := range 2 {
			for _, u := range sides[i] {
				if v, ok := m.val(u); ok {
					vals[i] = append(vals[i], v)
				}
			}
		}
		st := CompareStat{Key: m.key, Label: m.label, Unit: m.unit, Before: compareSide(vals[0]), After: compareSide(vals[1])}
		if st.Before.Median != 0 && st.Before.N > 0 && st.After.N > 0 {
			st.Change = f64p(st.After.Median/st.Before.Median - 1)
		}
		st.P = mannWhitney(vals[0], vals[1])
		out = append(out, st)
	}
	return out
}

func compareSide(v []float64) CompareSide {
	s := CompareSide{N: len(v), Median: median(v), Mean: mean(v)}
	for _, x := range v {
		s.Total += x
	}
	return s
}
