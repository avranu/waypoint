package core

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"
)

// Carry-over (WP-1884): a ship with --skip-review closes the run, so later
// review rounds in the same session land on no task. taskOf keeps saying so;
// carryOf says whose work such an hour most likely was, and it is reported
// beside a task's cost, never in it.

// loadCarry reads every run of the scope's sessions that began before the
// window ended, whatever the window's start: the run a late hour carries
// over from may have ended long before it.
func (a *taskAttribution) loadCarry(ctx context.Context, s *Service, actor *Actor, sc *usageScope) error {
	a.all = map[sessKeyAt][]runSpan{}
	where, args := sessWhere(sc, "s")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT s.machine, s.cli_id, r.issue_id, r.started_at, coalesce(r.ended_at, 0) FROM agent_runs r
		JOIN agent_sessions s ON s.id = r.session_id
		WHERE `+where+` AND r.started_at < ? ORDER BY r.started_at, r.id`, append(args, sc.to)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var k sessKeyAt
		var r runSpan
		if err := rows.Scan(&k.machine, &k.session, &r.issue, &r.start, &r.end); err != nil {
			return err
		}
		r.start = max(r.start, 1)
		a.all[k] = append(a.all[k], r)
		if _, ok := a.tasks[r.issue]; !ok {
			ids[r.issue] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return s.loadTaskInfo(ctx, actor, ids, a.tasks)
}

// carryOf is the task an hour that taskOf gave to none carries over to: the
// task of the session's latest run that began before the hour ended. 0 when
// taskOf had a task for it, when the session never had a run by then, or
// when that run's task is not one the actor can see. loadCarry comes first.
func (a *taskAttribution) carryOf(k sessKeyAt, hour int64) int64 {
	if a.taskOf(k, hour) != 0 {
		return 0
	}
	spans := a.all[k]
	end := hour + hourMillis
	i := sort.Search(len(spans), func(i int) bool { return spans[i].start >= end }) - 1
	if i < 0 {
		return 0
	}
	if _, ok := a.tasks[spans[i].issue]; !ok {
		return 0
	}
	return spans[i].issue
}

// TaskCarryOver is a task's session spend after its run ended, with no other
// task claiming it.
type TaskCarryOver struct {
	CostUSD  float64 `json:"cost_usd"`
	Sessions int     `json:"sessions"`
}

// NoTaskCarry is one task the unclaimed spend carries over to.
type NoTaskCarry struct {
	Key      string  `json:"key"`
	Title    string  `json:"title"`
	CostUSD  float64 `json:"cost_usd"`
	Sessions int     `json:"sessions"`
}

// NoTaskSession is a session that never had a run, with its spend.
type NoTaskSession struct {
	Machine   string    `json:"machine"`
	Session   string    `json:"session"`
	Repo      string    `json:"repo"`
	FirstHour time.Time `json:"first_hour"`
	CostUSD   float64   `json:"cost_usd"`
}

const noTaskTop = 10

// NoTaskUsage is the spend no task was found for, split by why.
type NoTaskUsage struct {
	Who       string    `json:"who"`
	Days      int       `json:"days"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	TotalUSD  float64   `json:"total_usd"`
	TaskUSD   float64   `json:"task_usd"`
	NoTaskUSD float64   `json:"no_task_usd"`
	// CarryUSD is no-task spend in a session that had run earlier; CarryTasks
	// is the number of tasks it would go to and CarryOver the top ten.
	CarryUSD   float64       `json:"carry_usd"`
	CarryTasks int           `json:"carry_tasks"`
	CarryOver  []NoTaskCarry `json:"carry_over"`
	// NeverUSD is spend in sessions that never had a run; NeverSessions their
	// number and Never the costliest ten.
	NeverUSD      float64         `json:"never_usd"`
	NeverSessions int             `json:"never_sessions"`
	Never         []NoTaskSession `json:"never"`
	// OtherUSD is the rest: hours before a session's first run, and hours
	// carrying over to a task the actor can't see.
	OtherUSD float64 `json:"other_usd"`
}

// UsageNoTask splits the window's spend into what taskOf gave a task and
// what it gave none, the latter into carry-over and sessions that never had
// a run. Its totals are the Trends tab's for the same window and person.
func (s *Service) UsageNoTask(ctx context.Context, actor *Actor, q UsageQuery) (*NoTaskUsage, error) {
	sc, _, first, label, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
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
	if err := attr.loadCarry(ctx, s, actor, sc); err != nil {
		return nil, err
	}
	o := &NoTaskUsage{Who: label, Days: q.Days, From: first.UTC(), To: msTime(sc.to - 1), CarryOver: []NoTaskCarry{}, Never: []NoTaskSession{}}
	type carryAgg struct {
		cost     float64
		sessions map[sessKeyAt]bool
	}
	type neverAgg struct {
		cost  float64
		first int64
		repo  string
	}
	carry := map[int64]*carryAgg{}
	never := map[sessKeyAt]*neverAgg{}
	for k, b := range buckets {
		sk := sessKeyAt{k.machine, k.session}
		o.TotalUSD += b.acc.cost
		if attr.taskOf(sk, k.hour) != 0 {
			o.TaskUSD += b.acc.cost
			continue
		}
		o.NoTaskUSD += b.acc.cost
		if id := attr.carryOf(sk, k.hour); id != 0 {
			c := carry[id]
			if c == nil {
				c = &carryAgg{sessions: map[sessKeyAt]bool{}}
				carry[id] = c
			}
			c.cost += b.acc.cost
			if b.acc.cost > 0 {
				c.sessions[sk] = true
			}
			o.CarryUSD += b.acc.cost
			continue
		}
		if len(attr.all[sk]) > 0 {
			o.OtherUSD += b.acc.cost
			continue
		}
		n := never[sk]
		if n == nil {
			n = &neverAgg{first: k.hour, repo: k.repo}
			never[sk] = n
		}
		n.cost += b.acc.cost
		if k.hour < n.first {
			n.first, n.repo = k.hour, k.repo
		}
		o.NeverUSD += b.acc.cost
	}
	for id, c := range carry {
		t := attr.tasks[id]
		o.CarryOver = append(o.CarryOver, NoTaskCarry{Key: t.key, Title: t.title, CostUSD: c.cost, Sessions: len(c.sessions)})
	}
	o.CarryTasks = len(o.CarryOver)
	slices.SortFunc(o.CarryOver, func(a, b NoTaskCarry) int {
		if a.CostUSD != b.CostUSD {
			return costCmp(b.CostUSD, a.CostUSD)
		}
		return strings.Compare(a.Key, b.Key)
	})
	o.CarryOver = o.CarryOver[:min(len(o.CarryOver), noTaskTop)]
	for k, n := range never {
		o.Never = append(o.Never, NoTaskSession{Machine: k.machine, Session: k.session, Repo: n.repo, FirstHour: msTime(n.first).UTC(), CostUSD: n.cost})
	}
	o.NeverSessions = len(o.Never)
	slices.SortFunc(o.Never, func(a, b NoTaskSession) int {
		if a.CostUSD != b.CostUSD {
			return costCmp(b.CostUSD, a.CostUSD)
		}
		return strings.Compare(a.Machine+a.Session, b.Machine+b.Session)
	})
	o.Never = o.Never[:min(len(o.Never), noTaskTop)]
	return o, nil
}

func costCmp(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
