package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// usageTrendRepos is how many repos get their own series; the rest are "other".
const usageTrendRepos = 4

const usageTrendTasks = 300

// Additive measures, per day: the totals of the day.
var trendSums = []string{"cost", "tokens", "cache_read", "messages", "sessions", "tool_calls", "tool_errors", "subagents", "tasks"}

// Average measures, per day: null on a day without the data.
var trendAvgs = []string{"avg_context", "over_200k", "hit_rate", "cost_per_session"}

// UsageTrendRepo is one repo's (or the rest's) additive series.
type UsageTrendRepo struct {
	Repo    string               `json:"repo"` // "" = no repository; "*" = every other repo
	Label   string               `json:"label"`
	Project string               `json:"project,omitempty"`
	CostUSD float64              `json:"cost_usd"`
	Series  map[string][]float64 `json:"series"`
}

// UsageMark is a moment drawn on the charts.
type UsageMark struct {
	Date   string    `json:"date"` // the reader's day
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"` // instructions, cli
	Repo   string    `json:"repo,omitempty"`
	Title  string    `json:"title"`
	URL    string    `json:"url,omitempty"`
	Author string    `json:"author,omitempty"`
	Count  int       `json:"count"` // commits (or versions) that day
	Paths  []string  `json:"paths,omitempty"`
}

// UsageTrendPR is a merged pull request, offered as a pivot.
type UsageTrendPR struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	MergedAt time.Time `json:"merged_at"`
}

// UsageTrendTask is what one task used in the period.
type UsageTrendTask struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Done        bool       `json:"done"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	Points      *float64   `json:"points,omitempty"`
	Sessions    int        `json:"sessions"`
	Subagents   int64      `json:"subagents"`
	ToolCalls   int64      `json:"tool_calls"`
	ToolErrors  int64      `json:"tool_errors"`
	ActiveHours int        `json:"active_hours"`
	Compactions int64      `json:"compactions"` // each session's go to the task it spent most on only (WP-1470)
	AvgContext  *float64   `json:"avg_context"`
	FirstAt     time.Time  `json:"first_at"`
	LastAt      time.Time  `json:"last_at"`
	PRs         int        `json:"prs"`
	Lines       int64      `json:"lines"` // added + deleted, in its merged pull requests
	Files       int64      `json:"files"`
	Commits     int64      `json:"commits"`
	// CostBy is its cost (USD) by model family, main thread or subagents
	// ("opus", "haiku/sub"…: usageCostKey).
	CostBy map[string]float64 `json:"cost_by,omitempty"`
	UsageTotals
}

// UsageFactor is how strongly a measure goes with a task's cost: Spearman's
// rank correlation over the tasks that have both.
type UsageFactor struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Rho    *float64 `json:"rho"` // nil: too few tasks, or no spread
	N      int      `json:"n"`
	Enough bool     `json:"enough"` // at least usageFactorMin tasks
}

const usageFactorMin = 8

// UsageTrends is the Trends tab.
type UsageTrends struct {
	From     time.Time             `json:"from"`
	To       time.Time             `json:"to"`
	Days     int                   `json:"days"`
	Who      string                `json:"who"`
	HasData  bool                  `json:"has_data"`
	Dates    []string              `json:"dates"`
	Sums     map[string][]float64  `json:"sums"`
	Avgs     map[string][]*float64 `json:"avgs"`
	Repos    []UsageTrendRepo      `json:"repos"`
	Marks    []UsageMark           `json:"marks"`
	PRs      []UsageTrendPR        `json:"prs"`
	Tasks    []UsageTrendTask      `json:"tasks"`
	TaskRows int                   `json:"task_rows"` // tasks before the list was cut
	NoTask   UsageTotals           `json:"no_task"`   // usage no task was found for
	Factors  []UsageFactor         `json:"factors"`
}

type trendKey struct {
	machine, session string
	hour             int64
	repo             string
}

type trendBucket struct {
	acc                     usageAcc
	by                      map[string]float64
	tools, errors, subagent int64
	spawns                  int64
	calls, ctx, over200     int64
}

func (b *trendBucket) subagents() int64 { return max(b.subagent, b.spawns) }

// usageCostKey names a share of a task's cost (WP-1262: who did the work):
// the model's family (opus, sonnet, haiku, fable, mythos, else the model),
// with "/sub" for subagents'.
func usageCostKey(model string, main bool) string {
	f := modelFamily(model)
	if !main {
		f += "/sub"
	}
	return f
}

// trendBuckets reads usage_hours, tool_hours and ledger_hours by session hour.
func (s *Service) trendBuckets(ctx context.Context, sc *usageScope, book *priceBook) (map[trendKey]*trendBucket, error) {
	out := map[trendKey]*trendBucket{}
	at := func(k trendKey) *trendBucket {
		b := out[k]
		if b == nil {
			b = &trendBucket{}
			out[k] = b
		}
		return b
	}
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, hour, repo, model, kind = 'main', sum(messages), sum(input), sum(output), sum(cache_write_5m), sum(cache_write_1h), sum(cache_read)
		FROM usage_hours WHERE `+where+` GROUP BY machine, session, hour, repo, model, kind = 'main'`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k trendKey
		var model string
		var main bool
		var n, in, outT, cw5, cw1, cr int64
		if err := rows.Scan(&k.machine, &k.session, &k.hour, &k.repo, &model, &main, &n, &in, &outT, &cw5, &cw1, &cr); err != nil {
			rows.Close()
			return nil, err
		}
		p, ok := book.find(model)
		b := at(k)
		before := b.acc.cost
		b.acc.add(n, in, outT, cw5, cw1, cr, p, ok)
		if c := b.acc.cost - before; c > 0 {
			if b.by == nil {
				b.by = map[string]float64{}
			}
			b.by[usageCostKey(model, main)] += c
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// #nosec G202 -- as above
	rows, err = s.db.R.QueryContext(ctx, `SELECT machine, session, hour, repo,
			sum(CASE WHEN kind IN ('builtin', 'mcp') THEN calls ELSE 0 END), sum(CASE WHEN kind IN ('builtin', 'mcp') THEN errors ELSE 0 END),
			sum(CASE WHEN kind = 'subagent' THEN calls ELSE 0 END)
		FROM tool_hours WHERE `+where+` GROUP BY machine, session, hour, repo`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k trendKey
		var calls, errs, sub int64
		if err := rows.Scan(&k.machine, &k.session, &k.hour, &k.repo, &calls, &errs, &sub); err != nil {
			rows.Close()
			return nil, err
		}
		b := at(k)
		b.tools += calls
		b.errors += errs
		b.subagent += sub
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// #nosec G202 -- as above
	rows, err = s.db.R.QueryContext(ctx, `SELECT machine, session, hour, repo,
			sum(CASE WHEN kind = 'main' THEN calls ELSE 0 END), sum(CASE WHEN kind = 'main' THEN ctx ELSE 0 END),
			sum(CASE WHEN kind = 'main' THEN over_200k ELSE 0 END), sum(forks + fresh)
		FROM ledger_hours WHERE `+where+` GROUP BY machine, session, hour, repo`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k trendKey
		var calls, cx, o200, spawns int64
		if err := rows.Scan(&k.machine, &k.session, &k.hour, &k.repo, &calls, &cx, &o200, &spawns); err != nil {
			return nil, err
		}
		b := at(k)
		b.calls += calls
		b.ctx += cx
		b.over200 += o200
		b.spawns += spawns
	}
	return out, rows.Err()
}

type runSpan struct {
	issue      int64
	start, end int64
}

type trendTaskInfo struct {
	key, title string
	resolved   *time.Time
	points     *float64
	done       bool
}

type taskAttribution struct {
	runs  map[sessKeyAt][]runSpan
	tasks map[int64]trendTaskInfo
	all   map[sessKeyAt][]runSpan // every run of a session, for carryOf (loadCarry)
}

const (
	runGrace   = int64(time.Hour / time.Millisecond)
	runLead    = int64(2 * time.Hour / time.Millisecond)
	hourMillis = int64(time.Hour / time.Millisecond)
)

// taskOf is the task an hour of a session was for, 0 for none.
func (a *taskAttribution) taskOf(k sessKeyAt, hour int64) int64 {
	spans := a.runs[k]
	if len(spans) == 0 {
		return 0
	}
	if spans[0].start == 0 {
		return spans[0].issue
	}
	end := hour + hourMillis
	i := sort.Search(len(spans), func(i int) bool { return spans[i].start >= end }) - 1
	if i < 0 {
		if spans[0].start-end < runLead {
			return spans[0].issue
		}
		return 0
	}
	if r := spans[i]; r.end != 0 && r.end+runGrace < hour {
		return 0
	}
	return spans[i].issue
}

// sessWhere is the person (and machine) condition on agent_sessions as col.
func sessWhere(sc *usageScope, col string) (string, []any) {
	where, args := "0", []any(nil)
	switch {
	case sc.everyone:
		where = "1"
	case len(sc.uids) == 1:
		where, args = col+".user_id = ?", []any{sc.uids[0]}
	}
	if sc.machine != "" {
		where, args = where+" AND "+col+".machine = ?", append(args, sc.machine)
	}
	return where, args
}

func (s *Service) taskAttribution(ctx context.Context, actor *Actor, sc *usageScope) (*taskAttribution, error) {
	a := &taskAttribution{runs: map[sessKeyAt][]runSpan{}, tasks: map[int64]trendTaskInfo{}}
	where, args := sessWhere(sc, "s")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT s.machine, s.cli_id, r.issue_id, r.started_at, coalesce(r.ended_at, 0) FROM agent_runs r
		JOIN agent_sessions s ON s.id = r.session_id
		WHERE `+where+` AND r.started_at < ? AND coalesce(r.ended_at, ?) >= ? ORDER BY r.started_at, r.id`,
		append(args, sc.to+runLead, sc.to, sc.from-runGrace)...)
	if err != nil {
		return nil, err
	}
	ids := map[int64]bool{}
	for rows.Next() {
		var k sessKeyAt
		var r runSpan
		if err := rows.Scan(&k.machine, &k.session, &r.issue, &r.start, &r.end); err != nil {
			rows.Close()
			return nil, err
		}
		if r.start == 0 {
			r.start = 1
		}
		a.runs[k] = append(a.runs[k], r)
		ids[r.issue] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A session that took a task with no run recorded is the task's whole.
	// One with a run is read by its runs: that its run is older than the
	// window does not make today's hours the task's (they are carry-over).
	// #nosec G202 -- as above
	rows, err = s.db.R.QueryContext(ctx, `SELECT s.machine, s.cli_id, s.issue_id FROM agent_sessions s
		WHERE `+where+` AND s.issue_id IS NOT NULL AND s.last_seen_at >= ?
		AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.session_id = s.id)`, append(args, sc.from)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k sessKeyAt
		var id int64
		if err := rows.Scan(&k.machine, &k.session, &id); err != nil {
			rows.Close()
			return nil, err
		}
		if len(a.runs[k]) == 0 {
			a.runs[k] = []runSpan{{issue: id}}
			ids[id] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.loadTaskInfo(ctx, actor, ids, a.tasks); err != nil {
		return nil, err
	}
	for k, spans := range a.runs {
		kept := spans[:0]
		for _, r := range spans {
			if _, ok := a.tasks[r.issue]; ok {
				kept = append(kept, r)
			} else {
				kept = append(kept, runSpan{issue: 0, start: r.start, end: r.end})
			}
		}
		a.runs[k] = kept
	}
	return a, nil
}

// loadTaskInfo reads the tasks of ids the actor can see into into.
func (s *Service) loadTaskInfo(ctx context.Context, actor *Actor, ids map[int64]bool, into map[int64]trendTaskInfo) error {
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	vis, vargs := visibleSQL("i.project_id", actor)
	for start := 0; start < len(list); start += 500 {
		chunk := list[start:min(start+500, len(list))]
		qargs := append([]any(nil), vargs...)
		for _, id := range chunk {
			qargs = append(qargs, id)
		}
		// #nosec G202 -- vis is built from fixed conditions; ids are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, p.key, i.number, i.title, i.resolved_at, i.points, st.category FROM issues i
			JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
			WHERE `+vis+` AND i.id IN (`+placeholders(len(chunk))+`)`, qargs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, number int64
			var key, title, cat string
			var resolved sql.NullInt64
			var points sql.NullFloat64
			if err := rows.Scan(&id, &key, &number, &title, &resolved, &points, &cat); err != nil {
				rows.Close()
				return err
			}
			t := trendTaskInfo{key: fmt.Sprintf("%s-%d", key, number), title: title, done: cat == CatDone}
			if resolved.Valid {
				ts := msTime(resolved.Int64)
				t.resolved = &ts
			}
			if points.Valid {
				v := points.Float64
				t.points = &v
			}
			into[id] = t
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// UsageTrends answers the Trends tab.
func (s *Service) UsageTrends(ctx context.Context, actor *Actor, q UsageQuery) (*UsageTrends, error) {
	sc, loc, first, label, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return nil, err
	}
	o := &UsageTrends{From: first.UTC(), To: msTime(sc.to - 1), Days: q.Days, Who: label, Sums: map[string][]float64{}, Avgs: map[string][]*float64{},
		Repos: []UsageTrendRepo{}, Marks: []UsageMark{}, PRs: []UsageTrendPR{}, Tasks: []UsageTrendTask{}, Factors: []UsageFactor{}}
	if o.HasData, err = s.usageHasData(ctx, sc); err != nil {
		return nil, err
	}
	dayIdx := map[string]int{}
	for d := first; d.UnixMilli() < sc.to; d = d.AddDate(0, 0, 1) {
		ds := d.Format(dateLayout)
		dayIdx[ds] = len(o.Dates)
		o.Dates = append(o.Dates, ds)
	}
	nd := len(o.Dates)
	for _, m := range trendSums {
		o.Sums[m] = make([]float64, nd)
	}
	for _, m := range trendAvgs {
		o.Avgs[m] = make([]*float64, nd)
	}
	buckets, err := s.trendBuckets(ctx, sc, book)
	if err != nil {
		return nil, err
	}
	attr, err := s.taskAttribution(ctx, actor, sc)
	if err != nil {
		return nil, err
	}
	repoKey, err := s.repoProjects(ctx, actor)
	if err != nil {
		return nil, err
	}

	type dayAgg struct {
		acc                      usageAcc
		calls, ctx, over200      int64
		sessions, tasks          map[string]bool
		tools, errors, subagents int64
	}
	days := make([]dayAgg, nd)
	for i := range days {
		days[i].sessions, days[i].tasks = map[string]bool{}, map[string]bool{}
	}
	type repoAgg struct {
		acc    usageAcc
		series map[string][]float64
		sess   []map[string]bool
	}
	repos := map[string]*repoAgg{}
	tasks := map[int64]*trendTaskAgg{}
	var noTask usageAcc
	sessMeta, err := s.trendSessionMeta(ctx, sc)
	if err != nil {
		return nil, err
	}
	for k, b := range buckets {
		di, ok := dayIdx[time.UnixMilli(k.hour).In(loc).Format(dateLayout)]
		if !ok {
			continue
		}
		sk := sessKeyAt{k.machine, k.session}
		skey := k.machine + "\x00" + k.session
		d := &days[di]
		d.acc.addAcc(&b.acc)
		d.calls += b.calls
		d.ctx += b.ctx
		d.over200 += b.over200
		d.tools += b.tools
		d.errors += b.errors
		d.subagents += b.subagents()
		if b.acc.n > 0 {
			d.sessions[skey] = true
		}
		r := repos[strings.ToLower(k.repo)]
		if r == nil {
			r = &repoAgg{series: map[string][]float64{}, sess: make([]map[string]bool, nd)}
			for _, m := range trendSums {
				if m != "tasks" {
					r.series[m] = make([]float64, nd)
				}
			}
			repos[strings.ToLower(k.repo)] = r
		}
		r.acc.addAcc(&b.acc)
		r.series["cost"][di] += b.acc.cost
		r.series["tokens"][di] += float64(b.acc.in + b.acc.out + b.acc.cw5 + b.acc.cw1)
		r.series["cache_read"][di] += float64(b.acc.cr)
		r.series["messages"][di] += float64(b.acc.n)
		r.series["tool_calls"][di] += float64(b.tools)
		r.series["tool_errors"][di] += float64(b.errors)
		r.series["subagents"][di] += float64(b.subagents())
		if b.acc.n > 0 {
			if r.sess[di] == nil {
				r.sess[di] = map[string]bool{}
			}
			r.sess[di][skey] = true
		}
		id := attr.taskOf(sk, k.hour)
		if id == 0 {
			noTask.addAcc(&b.acc)
			continue
		}
		d.tasks[fmt.Sprint(id)] = true
		t := tasks[id]
		if t == nil {
			t = &trendTaskAgg{sessions: map[sessKeyAt]float64{}, hours: map[int64]bool{}, first: k.hour, last: k.hour, branches: map[sessKeyAt][2]string{}}
			tasks[id] = t
		}
		t.acc.addAcc(&b.acc)
		for key, c := range b.by {
			if t.by == nil {
				t.by = map[string]float64{}
			}
			t.by[key] += c
		}
		t.tools += b.tools
		t.errors += b.errors
		t.subagents += b.subagents()
		t.calls += b.calls
		t.ctx += b.ctx
		t.sessions[sk] += b.acc.cost + 1e-12*float64(b.acc.n)
		t.hours[k.hour] = true
		t.first, t.last = min(t.first, k.hour), max(t.last, k.hour)
		if m, ok := sessMeta[sk]; ok && m.branch != "" {
			t.branches[sk] = [2]string{strings.ToLower(cmpOr(m.repo, k.repo)), m.branch}
		}
	}
	o.NoTask = noTask.totals()
	for i, d := range days {
		o.Sums["cost"][i] = d.acc.cost
		o.Sums["tokens"][i] = float64(d.acc.in + d.acc.out + d.acc.cw5 + d.acc.cw1)
		o.Sums["cache_read"][i] = float64(d.acc.cr)
		o.Sums["messages"][i] = float64(d.acc.n)
		o.Sums["sessions"][i] = float64(len(d.sessions))
		o.Sums["tool_calls"][i] = float64(d.tools)
		o.Sums["tool_errors"][i] = float64(d.errors)
		o.Sums["subagents"][i] = float64(d.subagents)
		o.Sums["tasks"][i] = float64(len(d.tasks))
		if d.calls > 0 {
			o.Avgs["avg_context"][i] = f64p(float64(d.ctx) / float64(d.calls))
			o.Avgs["over_200k"][i] = f64p(float64(d.over200) / float64(d.calls))
		}
		if t := d.acc.totals(); t.HitRate != nil {
			o.Avgs["hit_rate"][i] = t.HitRate
		}
		if len(d.sessions) > 0 {
			o.Avgs["cost_per_session"][i] = f64p(d.acc.cost / float64(len(d.sessions)))
		}
	}

	keys := make([]string, 0, len(repos))
	for k := range repos {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := repos[keys[i]].acc, repos[keys[j]].acc
		if a.cost != b.cost {
			return a.cost > b.cost
		}
		if x, y := a.in+a.out+a.cw5+a.cw1, b.in+b.out+b.cw5+b.cw1; x != y {
			return x > y
		}
		return keys[i] < keys[j]
	})
	var other *UsageTrendRepo
	for i, k := range keys {
		r := repos[k]
		r.series["sessions"] = make([]float64, nd)
		for di, m := range r.sess {
			r.series["sessions"][di] = float64(len(m))
		}
		if i < usageTrendRepos {
			lbl := k
			if k == "" {
				lbl = "(no repository)"
			}
			o.Repos = append(o.Repos, UsageTrendRepo{Repo: k, Label: lbl, Project: repoKey[k], CostUSD: r.acc.cost, Series: r.series})
			continue
		}
		if other == nil {
			other = &UsageTrendRepo{Repo: "*", Label: "Other repos", Series: map[string][]float64{}}
			for m := range r.series {
				other.Series[m] = make([]float64, nd)
			}
		}
		other.CostUSD += r.acc.cost
		for m, vals := range r.series {
			for di, v := range vals {
				other.Series[m][di] += v
			}
		}
	}
	if other != nil {
		o.Repos = append(o.Repos, *other)
	}

	if err := s.trendTasksInto(ctx, o, attr, tasks, sessMeta, q.allTasks); err != nil {
		return nil, err
	}
	if o.Marks, err = s.usageMarks(ctx, actor, sc, loc, repoKey); err != nil {
		return nil, err
	}
	if o.PRs, err = s.trendPRs(ctx, sc, repoKey); err != nil {
		return nil, err
	}
	return o, nil
}

func f64p(v float64) *float64 { return &v }

func (a *usageAcc) addAcc(b *usageAcc) {
	a.n += b.n
	a.in += b.in
	a.out += b.out
	a.cw5 += b.cw5
	a.cw1 += b.cw1
	a.cr += b.cr
	a.cost += b.cost
	a.unpriced += b.unpriced
}

type trendSessionInfo struct {
	repo, branch string
	compactions  int64
}

func (s *Service) trendSessionMeta(ctx context.Context, sc *usageScope) (map[sessKeyAt]trendSessionInfo, error) {
	where, args := userWhere(sc)
	if sc.machine != "" {
		where, args = where+" AND machine = ?", append(args, sc.machine)
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, repo, branch, compactions FROM usage_sessions WHERE `+where+` AND coalesce(last_at, ?) >= ?`,
		append(args, sc.from, sc.from)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[sessKeyAt]trendSessionInfo{}
	for rows.Next() {
		var k sessKeyAt
		var m trendSessionInfo
		if err := rows.Scan(&k.machine, &k.session, &m.repo, &m.branch, &m.compactions); err != nil {
			return nil, err
		}
		if m.branch == "main" || m.branch == "master" || m.branch == "HEAD" {
			m.branch = ""
		}
		out[k] = m
	}
	return out, rows.Err()
}

// trendTaskAgg is what one task used.
type trendTaskAgg struct {
	acc                      usageAcc
	by                       map[string]float64
	tools, errors, subagents int64
	calls, ctx               int64
	sessions                 map[sessKeyAt]float64
	hours                    map[int64]bool
	first, last              int64
	branches                 map[sessKeyAt][2]string
}

type mergedPR struct {
	id                               int64
	additions, deletions, files, cms int64
}

// mergedPRsByBranch is every merged pull request by repo (lower case) and
// head branch.
func (s *Service) mergedPRsByBranch(ctx context.Context) (map[[2]string][]mergedPR, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, lower(repo), head_ref, coalesce(additions, 0), coalesce(deletions, 0), coalesce(changed_files, 0), coalesce(commits, 0)
		FROM pull_requests WHERE state = 'merged' AND head_ref != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string][]mergedPR{}
	for rows.Next() {
		var p mergedPR
		var repo, branch string
		if err := rows.Scan(&p.id, &repo, &branch, &p.additions, &p.deletions, &p.files, &p.cms); err != nil {
			return nil, err
		}
		k := [2]string{repo, branch}
		out[k] = append(out[k], p)
	}
	return out, rows.Err()
}

// trendTasksInto fills the task table and the factors.
func (s *Service) trendTasksInto(ctx context.Context, o *UsageTrends, attr *taskAttribution, tasks map[int64]*trendTaskAgg,
	sessMeta map[sessKeyAt]trendSessionInfo, uncut bool) error {
	prs, err := s.mergedPRsByBranch(ctx)
	if err != nil {
		return err
	}
	topTask := map[sessKeyAt]int64{}
	topCost := map[sessKeyAt]float64{}
	for id, t := range tasks {
		for sk, c := range t.sessions {
			if c > topCost[sk] || (c == topCost[sk] && id < topTask[sk]) {
				topTask[sk], topCost[sk] = id, c
			}
		}
	}
	for id, t := range tasks {
		info := attr.tasks[id]
		row := UsageTrendTask{Key: info.key, Title: info.title, Done: info.done, ResolvedAt: info.resolved, Points: info.points,
			Sessions: len(t.sessions), Subagents: t.subagents, ToolCalls: t.tools, ToolErrors: t.errors, ActiveHours: len(t.hours),
			FirstAt: msTime(t.first), LastAt: msTime(t.last + hourMillis - 1), CostBy: t.by, UsageTotals: t.acc.totals()}
		if t.calls > 0 {
			row.AvgContext = f64p(float64(t.ctx) / float64(t.calls))
		}
		seen := map[int64]bool{}
		for sk := range t.sessions {
			if topTask[sk] != id {
				continue
			}
			row.Compactions += sessMeta[sk].compactions
			b, ok := t.branches[sk]
			if !ok {
				continue
			}
			for _, p := range prs[b] {
				if !seen[p.id] {
					seen[p.id] = true
					row.PRs++
					row.Lines += p.additions + p.deletions
					row.Files += p.files
					row.Commits += p.cms
				}
			}
		}
		o.Tasks = append(o.Tasks, row)
	}
	sort.Slice(o.Tasks, func(i, j int) bool {
		a, b := o.Tasks[i], o.Tasks[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		return a.Key < b.Key
	})
	o.Factors = usageFactors(o.Tasks)
	o.TaskRows = len(o.Tasks)
	if len(o.Tasks) > usageTrendTasks && !uncut {
		o.Tasks = o.Tasks[:usageTrendTasks]
	}
	return nil
}

// usageFactors ranks what goes with a task's cost. Tasks without cost
// (no prices) are left out; the PR measures count tasks with a merged pull
// request, points those with an estimate.
func usageFactors(tasks []UsageTrendTask) []UsageFactor {
	type factor struct {
		key, label string
		val        func(t *UsageTrendTask) (float64, bool)
	}
	always := func(f func(t *UsageTrendTask) float64) func(t *UsageTrendTask) (float64, bool) {
		return func(t *UsageTrendTask) (float64, bool) { return f(t), true }
	}
	withPR := func(f func(t *UsageTrendTask) float64) func(t *UsageTrendTask) (float64, bool) {
		return func(t *UsageTrendTask) (float64, bool) { return f(t), t.PRs > 0 }
	}
	list := []factor{
		{"tool_calls", "Tool calls", always(func(t *UsageTrendTask) float64 { return float64(t.ToolCalls) })},
		{"tool_errors", "Tool errors", always(func(t *UsageTrendTask) float64 { return float64(t.ToolErrors) })},
		{"sessions", "Sessions", always(func(t *UsageTrendTask) float64 { return float64(t.Sessions) })},
		{"subagents", "Subagents", always(func(t *UsageTrendTask) float64 { return float64(t.Subagents) })},
		{"active_hours", "Hours worked", always(func(t *UsageTrendTask) float64 { return float64(t.ActiveHours) })},
		{"avg_context", "Average context", func(t *UsageTrendTask) (float64, bool) {
			if t.AvgContext == nil {
				return 0, false
			}
			return *t.AvgContext, true
		}},
		{"compactions", "Compactions (a session's, credited to its top-cost task)", always(func(t *UsageTrendTask) float64 { return float64(t.Compactions) })},
		{"lines", "Lines changed", withPR(func(t *UsageTrendTask) float64 { return float64(t.Lines) })},
		{"files", "Files changed", withPR(func(t *UsageTrendTask) float64 { return float64(t.Files) })},
		{"commits", "Commits", withPR(func(t *UsageTrendTask) float64 { return float64(t.Commits) })},
		{"points", "Story points", func(t *UsageTrendTask) (float64, bool) {
			if t.Points == nil {
				return 0, false
			}
			return *t.Points, true
		}},
	}
	out := make([]UsageFactor, 0, len(list))
	for _, f := range list {
		var x, y []float64
		for i := range tasks {
			t := &tasks[i]
			if t.CostUSD <= 0 {
				continue
			}
			if v, ok := f.val(t); ok {
				x, y = append(x, v), append(y, t.CostUSD)
			}
		}
		u := UsageFactor{Key: f.key, Label: f.label, N: len(x), Enough: len(x) >= usageFactorMin}
		if u.Enough {
			u.Rho = spearman(x, y)
		}
		out = append(out, u)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Rho != nil) != (b.Rho != nil) {
			return a.Rho != nil
		}
		if a.Rho != nil && math.Abs(*a.Rho) != math.Abs(*b.Rho) {
			return math.Abs(*a.Rho) > math.Abs(*b.Rho)
		}
		return false
	})
	return out
}

// usageMarks reads the marks of the period: instruction changes in repos
// the reader can see (one per repo and day), and the first day each Claude
// Code version appeared in the reader's sessions.
func (s *Service) usageMarks(ctx context.Context, actor *Actor, sc *usageScope, loc *time.Location, repoKey map[string]string) ([]UsageMark, error) {
	out := []UsageMark{}
	want := map[string]bool{}
	for _, r := range sc.repos {
		want[strings.ToLower(r)] = true
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT repo, at, author, title, url, paths FROM usage_marks WHERE at >= ? AND at < ? ORDER BY at, id`, sc.from, sc.to)
	if err != nil {
		return nil, err
	}
	at := map[[2]string]int{}
	for rows.Next() {
		var m UsageMark
		var ms int64
		var paths string
		if err := rows.Scan(&m.Repo, &ms, &m.Author, &m.Title, &m.URL, &paths); err != nil {
			rows.Close()
			return nil, err
		}
		lr := strings.ToLower(m.Repo)
		if _, ok := repoKey[lr]; !ok || (sc.repoFilter && !want[lr]) {
			continue
		}
		var ps []string
		_ = json.Unmarshal([]byte(paths), &ps)
		m.At, m.Date, m.Kind, m.Count = msTime(ms), time.UnixMilli(ms).In(loc).Format(dateLayout), "instructions", 1
		k := [2]string{lr, m.Date}
		if i, ok := at[k]; ok {
			cur := &out[i]
			cur.Count++
			for _, p := range ps {
				if !slices.Contains(cur.Paths, p) {
					cur.Paths = append(cur.Paths, p)
				}
			}
			continue
		}
		m.Paths = ps
		at[k] = len(out)
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	where, args := userWhere(sc)
	if sc.machine != "" {
		where, args = where+" AND machine = ?", append(args, sc.machine)
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT version, min(first_at) FROM usage_sessions WHERE `+where+` AND version != '' AND first_at IS NOT NULL
		GROUP BY version HAVING min(first_at) >= ? AND min(first_at) < ? ORDER BY min(first_at)`, append(args, sc.from, sc.to)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cliDay := map[string]int{}
	for rows.Next() {
		var v string
		var ms int64
		if err := rows.Scan(&v, &ms); err != nil {
			return nil, err
		}
		date := time.UnixMilli(ms).In(loc).Format(dateLayout)
		if i, ok := cliDay[date]; ok {
			out[i].Title, out[i].Count = "Claude Code "+v, out[i].Count+1
			continue
		}
		cliDay[date] = len(out)
		out = append(out, UsageMark{Date: date, At: msTime(ms), Kind: "cli", Title: "Claude Code " + v, Count: 1})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// trendPRs lists the pull requests merged in the period, newest first.
func (s *Service) trendPRs(ctx context.Context, sc *usageScope, repoKey map[string]string) ([]UsageTrendPR, error) {
	want := map[string]bool{}
	for _, r := range sc.repos {
		want[strings.ToLower(r)] = true
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT repo, number, title, url, coalesce(merged_at, closed_at, updated_at) AS m FROM pull_requests
		WHERE state = 'merged' AND coalesce(merged_at, closed_at, updated_at) >= ? AND coalesce(merged_at, closed_at, updated_at) < ? ORDER BY m DESC`, sc.from, sc.to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageTrendPR{}
	for rows.Next() {
		var p UsageTrendPR
		var ms int64
		if err := rows.Scan(&p.Repo, &p.Number, &p.Title, &p.URL, &ms); err != nil {
			return nil, err
		}
		lr := strings.ToLower(p.Repo)
		if _, ok := repoKey[lr]; !ok || (sc.repoFilter && !want[lr]) {
			continue
		}
		p.MergedAt = msTime(ms)
		if out = append(out, p); len(out) >= 200 {
			break
		}
	}
	return out, rows.Err()
}

// ranks gives each value its rank (1-based), ties the average of theirs.
func ranks(v []float64) []float64 {
	idx := make([]int, len(v))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return v[idx[a]] < v[idx[b]] })
	out := make([]float64, len(v))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && v[idx[j+1]] == v[idx[i]] {
			j++
		}
		r := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[idx[k]] = r
		}
		i = j + 1
	}
	return out
}

// spearman is the rank correlation of x and y; nil when either has no spread.
func spearman(x, y []float64) *float64 {
	if len(x) < 3 || len(x) != len(y) {
		return nil
	}
	rx, ry := ranks(x), ranks(y)
	n := float64(len(x))
	var mx, my float64
	for i := range rx {
		mx += rx[i]
		my += ry[i]
	}
	mx, my = mx/n, my/n
	var sxy, sxx, syy float64
	for i := range rx {
		dx, dy := rx[i]-mx, ry[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return nil
	}
	r := sxy / math.Sqrt(sxx*syy)
	return &r
}

// mannWhitney is the two-sided p-value of the Mann–Whitney U test (normal
// approximation with tie correction and continuity correction); nil when a
// side is empty or nothing varies.
func mannWhitney(a, b []float64) *float64 {
	n1, n2 := len(a), len(b)
	if n1 == 0 || n2 == 0 {
		return nil
	}
	all := append(append([]float64(nil), a...), b...)
	r := ranks(all)
	var r1 float64
	for i := 0; i < n1; i++ {
		r1 += r[i]
	}
	u := r1 - float64(n1*(n1+1))/2
	n := float64(n1 + n2)
	counts := map[float64]float64{}
	for _, v := range all {
		counts[v]++
	}
	var ties float64
	for _, t := range counts {
		ties += t*t*t - t
	}
	variance := float64(n1*n2) / 12 * ((n + 1) - ties/(n*(n-1)))
	if variance <= 0 {
		return nil
	}
	mu := float64(n1*n2) / 2
	z := math.Max(math.Abs(u-mu)-0.5, 0) / math.Sqrt(variance)
	p := math.Erfc(z / math.Sqrt2)
	return &p
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	if len(c)%2 == 1 {
		return c[len(c)/2]
	}
	return (c[len(c)/2-1] + c[len(c)/2]) / 2
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var t float64
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}
