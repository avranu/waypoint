package core

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LedgerStats is what a set of calls looked like.
type LedgerStats struct {
	Calls      int64    `json:"calls"` // calls measured
	AvgContext float64  `json:"avg_context"`
	Over200k   int64    `json:"over_200k"`
	Over300k   int64    `json:"over_300k"`
	Share200k  *float64 `json:"share_200k"` // of calls
	Share300k  *float64 `json:"share_300k"`
	ReadShare  *float64 `json:"read_share_300k"` // of cache reads, made by calls over 300k
	// The cold figures are INFERRED, not measured (WP-1470, WP-ADR145): a call
	// that wrote the cache after a gap the CLI judged longer than the cache
	// lived. The server never sees the gap, only the CLI's guess; computing it
	// from per-call facts is WP-1551. So the price is an estimate, named so.
	ColdStarts int64    `json:"cold_starts"`
	ColdShare  *float64 `json:"cold_share"` // of calls
	ColdTokens int64    `json:"cold_tokens"`
	ColdExtra  float64  `json:"cold_extra_est_usd"` // estimate: the writes' price over reading the same tokens warm, if the guess is right
}

// LedgerDay is one day of the series.
type LedgerDay struct {
	Date       string   `json:"date"`
	Calls      int64    `json:"calls"`
	AvgContext float64  `json:"avg_context"`
	Share300k  *float64 `json:"share_300k"`
	ColdStarts int64    `json:"cold_starts"`
}

// LedgerRow is a session, machine or task in a table.
type LedgerRow struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Sub     string `json:"sub,omitempty"`
	Task    string `json:"task,omitempty"` // the issue key a session holds
	Machine string `json:"machine,omitempty"`
	LedgerStats
	CostUSD    float64    `json:"cost_usd"`
	CacheRead  int64      `json:"cache_read"`
	Sessions   int        `json:"sessions,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// LedgerSpawn compares forks with fresh subagents of one type.
type LedgerSpawn struct {
	Type         string  `json:"type"`
	Forks        int64   `json:"forks"`
	Fresh        int64   `json:"fresh"`
	ForkContext  float64 `json:"fork_context"`  // average at the first call
	FreshContext float64 `json:"fresh_context"` // likewise
}

// LedgerYieldModel is one model's share of a yield.
type LedgerYieldModel struct {
	Model string `json:"model"`
	Items int    `json:"items"` // tasks (or pull requests) it worked on
	UsageTotals
}

// LedgerPrepGroup is what the closed tasks of one prep group cost, and the
// tool calls their sessions made.
type LedgerPrepGroup struct {
	Group string `json:"group"` // shown, held_out, none
	LedgerYield
	ToolCalls int64 `json:"tool_calls"`
}

// LedgerYield is what closed tasks (or merged pull requests) cost.
type LedgerYield struct {
	Done     int                `json:"done"`     // closed or merged in the period
	Measured int                `json:"measured"` // of those, with sessions that reported usage
	Totals   UsageTotals        `json:"totals"`
	Models   []LedgerYieldModel `json:"models"`
}

// UsageLedger is the ledger section of the Usage tab.
type UsageLedger struct {
	From     time.Time     `json:"from"`
	To       time.Time     `json:"to"`
	Days     int           `json:"days"`
	Who      string        `json:"who"`
	Overall  LedgerStats   `json:"overall"`
	Daily    []LedgerDay   `json:"daily"`
	Spawns   []LedgerSpawn `json:"spawns"` // first row: every type together
	Sessions []LedgerRow   `json:"sessions"`
	Machines []LedgerRow   `json:"machines"`
	Tasks    []LedgerRow   `json:"tasks"`
	Closed   LedgerYield   `json:"closed_tasks"`
	Merged   LedgerYield   `json:"merged_prs"`
	// Prep splits the closed tasks by task prep (WP-212): shown to the
	// agent that took it, held out (made, not shown), or none.
	Prep []LedgerPrepGroup `json:"prep"`
	// Unmeasured: usage from before the ledger existed that hasn't been read
	// again yet (run `wp usage sync`).
	Unmeasured int64 `json:"unmeasured_messages"`
	Truncated  bool  `json:"truncated,omitempty"`
}

type ledgerAcc struct {
	calls, ctx, o200, o300, cr300, cold, w5, w1 int64
	forks, forkCtx, fresh, freshCtx             int64
	extra                                       float64
	cacheRead                                   int64
	cost                                        float64
	sessions                                    map[string]bool
}

func (a *ledgerAcc) stats() LedgerStats {
	st := LedgerStats{Calls: a.calls, Over200k: a.o200, Over300k: a.o300, ColdStarts: a.cold, ColdTokens: a.w5 + a.w1, ColdExtra: a.extra}
	if a.calls > 0 {
		c := float64(a.calls)
		st.AvgContext = float64(a.ctx) / c
		s2, s3, cs := float64(a.o200)/c, float64(a.o300)/c, float64(a.cold)/c
		st.Share200k, st.Share300k, st.ColdShare = &s2, &s3, &cs
	}
	if a.cacheRead > 0 {
		r := float64(a.cr300) / float64(a.cacheRead)
		st.ReadShare = &r
	}
	return st
}

func (a *ledgerAcc) addTotals(o *ledgerAcc) {
	a.calls += o.calls
	a.ctx += o.ctx
	a.o200 += o.o200
	a.o300 += o.o300
	a.cr300 += o.cr300
	a.cold += o.cold
	a.w5 += o.w5
	a.w1 += o.w1
	a.forks += o.forks
	a.forkCtx += o.forkCtx
	a.fresh += o.fresh
	a.freshCtx += o.freshCtx
	a.extra += o.extra
	a.cacheRead += o.cacheRead
	a.cost += o.cost
}

func ledgerAccOf[K comparable](m map[K]*ledgerAcc, k K) *ledgerAcc {
	a := m[k]
	if a == nil {
		a = &ledgerAcc{}
		m[k] = a
	}
	return a
}

type sessKeyAt struct{ machine, session string }

// UsageLedger answers the ledger section in one go.
func (s *Service) UsageLedger(ctx context.Context, actor *Actor, q UsageQuery) (*UsageLedger, error) {
	sc, loc, first, label, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return nil, err
	}
	out := &UsageLedger{From: first.UTC(), To: msTime(sc.to - 1), Days: q.Days, Who: label, Daily: []LedgerDay{}, Spawns: []LedgerSpawn{},
		Sessions: []LedgerRow{}, Machines: []LedgerRow{}, Tasks: []LedgerRow{}}
	out.Closed.Models, out.Merged.Models, out.Prep = []LedgerYieldModel{}, []LedgerYieldModel{}, []LedgerPrepGroup{}

	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, kind, model, sum(calls), sum(ctx), sum(over_200k), sum(over_300k), sum(cr_300k),
			sum(cold), sum(cold_w5), sum(cold_w1), sum(forks), sum(fork_ctx), sum(fresh), sum(fresh_ctx)
		FROM ledger_hours WHERE `+where+` GROUP BY machine, session, kind, model`, args...)
	if err != nil {
		return nil, err
	}
	var all ledgerAcc
	bySess := map[sessKeyAt]*ledgerAcc{}
	byMachine := map[string]*ledgerAcc{}
	byKind := map[string]*ledgerAcc{}
	for rows.Next() {
		var machine, session, kind, model string
		var a ledgerAcc
		if err := rows.Scan(&machine, &session, &kind, &model, &a.calls, &a.ctx, &a.o200, &a.o300, &a.cr300, &a.cold, &a.w5, &a.w1,
			&a.forks, &a.forkCtx, &a.fresh, &a.freshCtx); err != nil {
			rows.Close()
			return nil, err
		}
		if p, ok := book.find(model); ok {
			a.extra = (float64(a.w5)*(p.CacheWrite5m-p.CacheRead) + float64(a.w1)*(p.CacheWrite1h-p.CacheRead)) / 1e6
		}
		all.addTotals(&a)
		ledgerAccOf(bySess, sessKeyAt{machine, session}).addTotals(&a)
		ledgerAccOf(byMachine, machine).addTotals(&a)
		if kind != "main" {
			ledgerAccOf(byKind, kind).addTotals(&a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	period, err := s.sessionModelUsage(ctx, sc, book)
	if err != nil {
		return nil, err
	}
	for k, pm := range period {
		var cr, cost = int64(0), 0.0
		for _, u := range pm {
			cr += u.cr
			cost += u.cost
		}
		if a := bySess[k]; a != nil {
			a.cacheRead, a.cost = cr, cost
		} else {
			ledgerAccOf(bySess, k).cacheRead = cr
			bySess[k].cost = cost
		}
		ledgerAccOf(byMachine, k.machine).cacheRead += cr
		byMachine[k.machine].cost += cost
		all.cacheRead += cr
		all.cost += cost
	}
	out.Overall = all.stats()
	var msgs int64
	// #nosec G202 -- as above
	if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(sum(messages), 0) FROM usage_hours WHERE `+where, args...).Scan(&msgs); err != nil {
		return nil, err
	}
	out.Unmeasured = max(msgs-all.calls, 0)

	if out.Daily, err = s.ledgerDaily(ctx, sc, loc); err != nil {
		return nil, err
	}

	out.Spawns = ledgerSpawns(&all, byKind)

	tasks, err := s.ledgerTasks(ctx, actor, sc)
	if err != nil {
		return nil, err
	}
	meta, err := s.ledgerSessionMeta(ctx, sc)
	if err != nil {
		return nil, err
	}
	for k, a := range bySess {
		m := meta[k]
		r := LedgerRow{Key: k.session, Machine: k.machine, LedgerStats: a.stats(), CostUSD: a.cost, CacheRead: a.cacheRead,
			Label: m.title}
		if r.Label == "" {
			r.Label = "Session " + shortID(k.session)
		}
		sub := []string{}
		if m.repo != "" {
			sub = append(sub, m.repo)
		}
		if t, ok := tasks.bySession[k]; ok {
			r.Task = t.key
			sub = append(sub, t.key)
		}
		r.Sub = strings.Join(sub, " · ")
		out.Sessions = append(out.Sessions, r)
	}
	sort.Slice(out.Sessions, func(i, j int) bool {
		a, b := out.Sessions[i], out.Sessions[j]
		if da, db := a.AvgContext*float64(a.Calls), b.AvgContext*float64(b.Calls); da != db {
			return da > db
		}
		return a.Key < b.Key
	})
	if len(out.Sessions) > usageTopSessions {
		out.Sessions, out.Truncated = out.Sessions[:usageTopSessions], true
	}
	for name, a := range byMachine {
		out.Machines = append(out.Machines, LedgerRow{Key: name, Label: name, LedgerStats: a.stats(), CostUSD: a.cost, CacheRead: a.cacheRead})
	}
	sort.Slice(out.Machines, func(i, j int) bool { return out.Machines[i].Label < out.Machines[j].Label })

	perTask := map[int64]*ledgerAcc{}
	for k, a := range bySess {
		if t, ok := tasks.bySession[k]; ok {
			ta := ledgerAccOf(perTask, t.id)
			ta.addTotals(a)
			if ta.sessions == nil {
				ta.sessions = map[string]bool{}
			}
			ta.sessions[k.machine+"\x00"+k.session] = true
		}
	}
	for id, a := range perTask {
		t := tasks.byID[id]
		r := LedgerRow{Key: t.key, Label: t.title, LedgerStats: a.stats(), CostUSD: a.cost, CacheRead: a.cacheRead, Sessions: len(a.sessions),
			ResolvedAt: t.resolved}
		out.Tasks = append(out.Tasks, r)
	}
	sort.Slice(out.Tasks, func(i, j int) bool {
		if out.Tasks[i].CostUSD != out.Tasks[j].CostUSD {
			return out.Tasks[i].CostUSD > out.Tasks[j].CostUSD
		}
		return out.Tasks[i].Key < out.Tasks[j].Key
	})
	if len(out.Tasks) > usageTopSessions {
		out.Tasks, out.Truncated = out.Tasks[:usageTopSessions], true
	}

	allTime := *sc
	allTime.from = 0
	life, err := s.sessionModelUsage(ctx, &allTime, book)
	if err != nil {
		return nil, err
	}
	closedIDs := tasks.closedIDs
	sessionsOf := map[int64][]sessKeyAt{}
	for k, t := range tasks.bySession {
		sessionsOf[t.id] = append(sessionsOf[t.id], k)
	}
	items := map[string][]sessKeyAt{}
	for id := range closedIDs {
		items[fmt.Sprint(id)] = sessionsOf[id]
	}
	out.Closed = yieldOf(len(closedIDs), items, life, book)
	if out.Prep, err = s.ledgerPrep(ctx, &allTime, closedIDs, sessionsOf, life, book); err != nil {
		return nil, err
	}
	prs, err := s.ledgerMergedPRs(ctx, actor, sc)
	if err != nil {
		return nil, err
	}
	out.Merged = yieldOf(prs.done, prs.sessions, life, book)
	return out, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func ledgerSpawns(all *ledgerAcc, byKind map[string]*ledgerAcc) []LedgerSpawn {
	mk := func(name string, a *ledgerAcc) LedgerSpawn {
		sp := LedgerSpawn{Type: name, Forks: a.forks, Fresh: a.fresh}
		if a.forks > 0 {
			sp.ForkContext = float64(a.forkCtx) / float64(a.forks)
		}
		if a.fresh > 0 {
			sp.FreshContext = float64(a.freshCtx) / float64(a.fresh)
		}
		return sp
	}
	out := []LedgerSpawn{mk("all", all)}
	if all.forks+all.fresh == 0 {
		return []LedgerSpawn{}
	}
	var types []LedgerSpawn
	for k, a := range byKind {
		if a.forks+a.fresh > 0 {
			types = append(types, mk(k, a))
		}
	}
	sort.Slice(types, func(i, j int) bool {
		if ti, tj := types[i].Forks+types[i].Fresh, types[j].Forks+types[j].Fresh; ti != tj {
			return ti > tj
		}
		return types[i].Type < types[j].Type
	})
	return append(out, types...)
}

func (s *Service) ledgerDaily(ctx context.Context, sc *usageScope, loc *time.Location) ([]LedgerDay, error) {
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT hour, sum(calls), sum(ctx), sum(over_300k), sum(cold) FROM ledger_hours WHERE `+where+` GROUP BY hour ORDER BY hour`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type day struct{ calls, ctx, o300, cold int64 }
	days := map[string]*day{}
	var order []string
	for rows.Next() {
		var hour int64
		var d day
		if err := rows.Scan(&hour, &d.calls, &d.ctx, &d.o300, &d.cold); err != nil {
			return nil, err
		}
		k := time.UnixMilli(hour).In(loc).Format(dateLayout)
		cur := days[k]
		if cur == nil {
			cur = &day{}
			days[k] = cur
			order = append(order, k)
		}
		cur.calls += d.calls
		cur.ctx += d.ctx
		cur.o300 += d.o300
		cur.cold += d.cold
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []LedgerDay{}
	for _, k := range order {
		d := days[k]
		ld := LedgerDay{Date: k, Calls: d.calls, ColdStarts: d.cold}
		if d.calls > 0 {
			ld.AvgContext = float64(d.ctx) / float64(d.calls)
			sh := float64(d.o300) / float64(d.calls)
			ld.Share300k = &sh
		}
		out = append(out, ld)
	}
	return out, nil
}

// modelUse is one model's tokens within a session.
type modelUse struct {
	in, out, cw5, cw1, cr, n int64
	cost                     float64
	priced                   bool
	p                        ModelPrice
}

// sessionModelUsage sums usage_hours by session and model.
func (s *Service) sessionModelUsage(ctx context.Context, sc *usageScope, book *priceBook) (map[sessKeyAt]map[string]*modelUse, error) {
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, model, sum(messages), sum(input), sum(output), sum(cache_write_5m), sum(cache_write_1h), sum(cache_read)
		FROM usage_hours WHERE `+where+` GROUP BY machine, session, model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[sessKeyAt]map[string]*modelUse{}
	for rows.Next() {
		var machine, session, model string
		var u modelUse
		if err := rows.Scan(&machine, &session, &model, &u.n, &u.in, &u.out, &u.cw5, &u.cw1, &u.cr); err != nil {
			return nil, err
		}
		u.p, u.priced = book.find(model)
		if u.priced {
			u.cost = u.p.Cost(u.in, u.out, u.cw5, u.cw1, u.cr)
		}
		k := sessKeyAt{machine, session}
		if out[k] == nil {
			out[k] = map[string]*modelUse{}
		}
		out[k][model] = &u
	}
	return out, rows.Err()
}

type ledgerSessionInfo struct {
	title, repo string
}

func (s *Service) ledgerSessionMeta(ctx context.Context, sc *usageScope) (map[sessKeyAt]ledgerSessionInfo, error) {
	where, args := userWhere(sc)
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, title, repo FROM usage_sessions WHERE `+where+` AND coalesce(last_at, 0) >= ?`, append(args, sc.from)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[sessKeyAt]ledgerSessionInfo{}
	for rows.Next() {
		var machine, session string
		var m ledgerSessionInfo
		if err := rows.Scan(&machine, &session, &m.title, &m.repo); err != nil {
			return nil, err
		}
		out[sessKeyAt{machine, session}] = m
	}
	return out, rows.Err()
}

// userWhere is the person condition alone.
func userWhere(sc *usageScope) (string, []any) {
	switch {
	case sc.everyone:
		return "1", nil
	case len(sc.uids) == 1:
		return "user_id = ?", []any{sc.uids[0]}
	}
	return "0", nil
}

type ledgerTask struct {
	id       int64
	key      string
	title    string
	resolved *time.Time
}

type ledgerTaskSet struct {
	bySession map[sessKeyAt]ledgerTask
	byID      map[int64]ledgerTask
	closedIDs map[int64]bool
}

// ledgerTasks joins sessions to the tasks they hold. Only tasks in projects
// the actor can see are named.
func (s *Service) ledgerTasks(ctx context.Context, actor *Actor, sc *usageScope) (*ledgerTaskSet, error) {
	set := &ledgerTaskSet{bySession: map[sessKeyAt]ledgerTask{}, byID: map[int64]ledgerTask{}, closedIDs: map[int64]bool{}}
	vis, vargs := visibleSQL("i.project_id", actor)
	// #nosec G202 -- vis is built from fixed conditions; values are bound
	irows, err := s.db.R.QueryContext(ctx, `SELECT i.id, p.key, i.number, i.title, i.resolved_at FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE `+vis+` AND i.id IN (SELECT issue_id FROM agent_sessions WHERE issue_id IS NOT NULL)`, vargs...)
	if err != nil {
		return nil, err
	}
	defer irows.Close()
	for irows.Next() {
		var t ledgerTask
		var key string
		var number int64
		var resolved sql.NullInt64
		if err := irows.Scan(&t.id, &key, &number, &t.title, &resolved); err != nil {
			return nil, err
		}
		t.key = fmt.Sprintf("%s-%d", key, number)
		if resolved.Valid {
			ts := msTime(resolved.Int64)
			t.resolved = &ts
			if resolved.Int64 >= sc.from && resolved.Int64 < sc.to {
				set.closedIDs[t.id] = true
			}
		}
		set.byID[t.id] = t
	}
	if err := irows.Err(); err != nil {
		return nil, err
	}
	where, args := userWhere(sc)
	// #nosec G202 -- where is built from fixed conditions; values are bound
	srows, err := s.db.R.QueryContext(ctx, `SELECT machine, cli_id, issue_id FROM agent_sessions WHERE `+where+` AND issue_id IS NOT NULL`, args...)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var machine, cli string
		var id int64
		if err := srows.Scan(&machine, &cli, &id); err != nil {
			return nil, err
		}
		if t, ok := set.byID[id]; ok {
			set.bySession[sessKeyAt{machine, cli}] = t
		}
	}
	return set, srows.Err()
}

type ledgerPRs struct {
	done     int
	sessions map[string][]sessKeyAt
}

// ledgerMergedPRs lists the pull requests merged in the period (of repos the
// actor can see) with the sessions that worked on their branch.
func (s *Service) ledgerMergedPRs(ctx context.Context, actor *Actor, sc *usageScope) (*ledgerPRs, error) {
	vis, err := s.repoProjects(ctx, actor)
	if err != nil {
		return nil, err
	}
	out := &ledgerPRs{sessions: map[string][]sessKeyAt{}}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, lower(repo), head_ref FROM pull_requests
		WHERE state = 'merged' AND coalesce(closed_at, updated_at) >= ? AND coalesce(closed_at, updated_at) < ?`, sc.from, sc.to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pr struct {
		id           int64
		repo, branch string
	}
	var list []pr
	for rows.Next() {
		var p pr
		if err := rows.Scan(&p.id, &p.repo, &p.branch); err != nil {
			return nil, err
		}
		if _, ok := vis[p.repo]; ok {
			list = append(list, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.done = len(list)
	where, args := userWhere(sc)
	if sc.machine != "" {
		where, args = where+" AND machine = ?", append(args, sc.machine)
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	srows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, lower(repo), branch FROM usage_sessions WHERE `+where+` AND branch NOT IN ('', 'main', 'master', 'HEAD')`, args...)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	byBranch := map[[2]string][]sessKeyAt{}
	for srows.Next() {
		var machine, session, repo, branch string
		if err := srows.Scan(&machine, &session, &repo, &branch); err != nil {
			return nil, err
		}
		k := [2]string{repo, branch}
		byBranch[k] = append(byBranch[k], sessKeyAt{machine, session})
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}
	for _, p := range list {
		out.sessions[fmt.Sprint(p.id)] = byBranch[[2]string{p.repo, p.branch}]
	}
	return out, nil
}

// yieldOf totals what the sessions of each item used.
func yieldOf(done int, items map[string][]sessKeyAt, usage map[sessKeyAt]map[string]*modelUse, book *priceBook) LedgerYield {
	y := LedgerYield{Done: done, Models: []LedgerYieldModel{}}
	var total usageAcc
	models := map[string]*usageAcc{}
	modelItems := map[string]int{}
	for _, sessions := range items {
		seen := map[string]bool{}
		any := false
		for _, k := range sessions {
			for model, u := range usage[k] {
				any = true
				p, ok := book.find(model)
				total.add(u.n, u.in, u.out, u.cw5, u.cw1, u.cr, p, ok)
				accOf(models, model).add(u.n, u.in, u.out, u.cw5, u.cw1, u.cr, p, ok)
				seen[model] = true
			}
		}
		if any {
			y.Measured++
		}
		for m := range seen {
			modelItems[m]++
		}
	}
	y.Totals = total.totals()
	for m, a := range models {
		y.Models = append(y.Models, LedgerYieldModel{Model: m, Items: modelItems[m], UsageTotals: a.totals()})
	}
	sort.Slice(y.Models, func(i, j int) bool {
		if y.Models[i].CostUSD != y.Models[j].CostUSD {
			return y.Models[i].CostUSD > y.Models[j].CostUSD
		}
		return y.Models[i].Model < y.Models[j].Model
	})
	return y
}

// ledgerPrep splits closed tasks by prep (WP-212, WP-ADR8). A task counts
// as shown, or held out, once an agent taking it fetched a ready prep;
// held-out tasks went through the same queue and sweep, so they are the
// fair comparison for shown ones.
func (s *Service) ledgerPrep(ctx context.Context, sc *usageScope, closed map[int64]bool, sessionsOf map[int64][]sessKeyAt,
	usage map[sessKeyAt]map[string]*modelUse, book *priceBook) ([]LedgerPrepGroup, error) {
	group := map[int64]string{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT issue_id, held_out FROM task_preps WHERE shown_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var held bool
		if err := rows.Scan(&id, &held); err != nil {
			rows.Close()
			return nil, err
		}
		if closed[id] {
			group[id] = map[bool]string{true: "held_out", false: "shown"}[held]
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	trows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, sum(calls) FROM tool_hours WHERE `+where+` GROUP BY machine, session`, args...)
	if err != nil {
		return nil, err
	}
	calls := map[sessKeyAt]int64{}
	for trows.Next() {
		var k sessKeyAt
		var n int64
		if err := trows.Scan(&k.machine, &k.session, &n); err != nil {
			trows.Close()
			return nil, err
		}
		calls[k] = n
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return nil, err
	}
	out := []LedgerPrepGroup{}
	for _, g := range []string{"shown", "held_out", "none"} {
		items := map[string][]sessKeyAt{}
		var n int
		var tc int64
		for id := range closed {
			if gg, ok := group[id]; ok && gg != g || !ok && g != "none" {
				continue
			}
			n++
			items[fmt.Sprint(id)] = sessionsOf[id]
			for _, k := range sessionsOf[id] {
				tc += calls[k]
			}
		}
		out = append(out, LedgerPrepGroup{Group: g, LedgerYield: yieldOf(n, items, usage, book), ToolCalls: tc})
	}
	return out, nil
}
