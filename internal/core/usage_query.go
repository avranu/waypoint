package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"
)

// Reading usage back (WP-D20): one overview for the Usage tab, with
// drill-downs. Everything is computed from hourly buckets, so months of
// data stay fast; the person's own data only, and admins may look at
// anyone's.

// UsageQuery narrows the usage shown.
type UsageQuery struct {
	Days    int    // the period, ending now (default 30, at most 365)
	Agent   string // an agent spec matched against the model: opus, haiku 5.5…
	Project string // a project key: its repos (and its sub-projects') sessions
	Machine string
	Session string // a CLI session id
	User    string // "" = you; admins: a username, or "all"
	// allTasks keeps every task in UsageTrends, uncut (experiments).
	allTasks bool
}

// UsageTotals sums tokens and their API-equivalent cost.
type UsageTotals struct {
	Messages       int64    `json:"messages"`
	Input          int64    `json:"input"` // uncached input tokens
	Output         int64    `json:"output"`
	CacheWrite     int64    `json:"cache_write"` // 5 minute and 1 hour
	CacheWrite1h   int64    `json:"cache_write_1h"`
	CacheRead      int64    `json:"cache_read"`
	CostUSD        float64  `json:"cost_usd"`
	HitRate        *float64 `json:"hit_rate"` // cache reads / all input; nil without input
	UnpricedTokens int64    `json:"unpriced_tokens,omitempty"`
}

type usageAcc struct {
	n, in, out, cw5, cw1, cr int64
	cost                     float64
	unpriced                 int64
}

func (a *usageAcc) add(n, in, out, cw5, cw1, cr int64, p ModelPrice, priced bool) {
	a.n += n
	a.in += in
	a.out += out
	a.cw5 += cw5
	a.cw1 += cw1
	a.cr += cr
	if priced {
		a.cost += p.Cost(in, out, cw5, cw1, cr)
	} else {
		a.unpriced += in + out + cw5 + cw1 + cr
	}
}

func (a *usageAcc) totals() UsageTotals {
	t := UsageTotals{Messages: a.n, Input: a.in, Output: a.out, CacheWrite: a.cw5 + a.cw1, CacheWrite1h: a.cw1, CacheRead: a.cr,
		CostUSD: a.cost, UnpricedTokens: a.unpriced}
	if all := a.in + a.cw5 + a.cw1 + a.cr; all > 0 {
		r := float64(a.cr) / float64(all)
		t.HitRate = &r
	}
	return t
}

// UsageDay is one day of one model.
type UsageDay struct {
	Date  string `json:"date"` // YYYY-MM-DD in your time zone
	Model string `json:"model"`
	UsageTotals
}

// UsageBreakdown is one row of a breakdown table.
type UsageBreakdown struct {
	Key         string     `json:"key"`
	Label       string     `json:"label"`
	Sub         string     `json:"sub,omitempty"`     // a second line: repo and machine of a session
	Project     string     `json:"project,omitempty"` // the project key a repo belongs to
	LastAt      *time.Time `json:"last_at,omitempty"`
	Compactions int64      `json:"compactions,omitempty"`
	// Sessions: the machine it ran on, and the agent session Waypoint holds
	// for it (WP-D16), when the hooks registered one, so the row can open it.
	Machine   string `json:"machine,omitempty"`
	SessionID int64  `json:"session_id,omitempty"`
	UsageTotals
}

// UsageTool is a tool (or MCP server, skill, command, subagent type).
type UsageTool struct {
	Group    string      `json:"group"` // builtin, mcp, skill, command, subagent
	Name     string      `json:"name"`
	Calls    int64       `json:"calls"`
	Errors   int64       `json:"errors"`
	Refusals int64       `json:"refusals"`
	Prompts  int64       `json:"prompts"`
	Bytes    int64       `json:"bytes"`
	Items    []UsageTool `json:"items,omitempty"` // an MCP server's tools
}

// UsageUnused is something configured on a machine that wasn't used.
type UsageUnused struct {
	Kind    string `json:"kind"` // mcp, skill, command, agent
	Name    string `json:"name"`
	Scope   string `json:"scope,omitempty"`
	Machine string `json:"machine"`
}

// UsageReason counts refusals with the same reason.
type UsageReason struct {
	Reason string `json:"reason"`
	Source string `json:"source"`
	Count  int64  `json:"count"`
}

// UsageRefusedCommand is a command (or tool) that was refused, with the
// allow rule that would let it through when a rule can.
type UsageRefusedCommand struct {
	Tool    string   `json:"tool"`
	Detail  string   `json:"detail"`
	Count   int64    `json:"count"`
	Reasons []string `json:"reasons"`
	Rule    string   `json:"rule,omitempty"`
}

// UsageRefusal row for the examples.
type UsageRefusalRow struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Tool    string    `json:"tool"`
	Detail  string    `json:"detail"`
	Reason  string    `json:"reason"`
	Source  string    `json:"source"`
	Excerpt string    `json:"excerpt"`
	Session string    `json:"session"`
	Repo    string    `json:"repo"`
	Machine string    `json:"machine"`
}

// UsageRefusals is the refusals section.
type UsageRefusals struct {
	Total    int64                 `json:"total"`
	Reasons  []UsageReason         `json:"reasons"`
	Daily    []UsageCountDay       `json:"daily"`
	Commands []UsageRefusedCommand `json:"commands"`
	Recent   []UsageRefusalRow     `json:"recent"`
}

// UsageCountDay is a count on a day.
type UsageCountDay struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}

// UsageMachine says when a machine last reported.
type UsageMachine struct {
	Name         string           `json:"name"`
	LastSyncAt   *time.Time       `json:"last_sync_at"`
	LastStatusAt *time.Time       `json:"last_status_at"`
	Version      string           `json:"wp_version"`
	Parsed       int64            `json:"parsed"`
	Skipped      int64            `json:"skipped"`
	Unknown      map[string]int64 `json:"unknown"`
}

// UsagePerson is someone with usage an admin may look at.
type UsagePerson struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// UsageOverview is the Usage tab.
type UsageOverview struct {
	From      time.Time        `json:"from"`
	To        time.Time        `json:"to"`
	Days      int              `json:"days"`
	Who       string           `json:"who"`      // whose usage: a username, or "all"
	HasData   bool             `json:"has_data"` // anything reported, in any period
	Totals    UsageTotals      `json:"totals"`
	Sessions  int              `json:"sessions"`
	Daily     []UsageDay       `json:"daily"`
	Models    []UsageBreakdown `json:"models"`
	Repos     []UsageBreakdown `json:"repos"`
	Machines  []UsageBreakdown `json:"machines"`
	Kinds     []UsageBreakdown `json:"kinds"`
	TopSess   []UsageBreakdown `json:"top_sessions"`
	Tools     []UsageTool      `json:"tools"`
	Bash      []UsageTool      `json:"bash"`
	Unused    []UsageUnused    `json:"unused"`
	NoInvent  bool             `json:"no_inventory"` // no machine has reported what is configured
	Refusals  UsageRefusals    `json:"refusals"`
	Quota     *UsageQuota      `json:"quota"`
	Advice    []UsageAdvice    `json:"suggestions"`
	Reported  []UsageMachine   `json:"reporting"`
	People    []UsagePerson    `json:"people,omitempty"` // admins: who has usage
	Prices    bool             `json:"prices_edit"`      // you may edit the price table
	Unpriced  []string         `json:"unpriced_models"`
	Truncated bool             `json:"truncated,omitempty"`
}

type usageScope struct {
	uids       []int64
	everyone   bool
	from, to   int64
	machine    string
	session    string
	repos      []string
	repoFilter bool
	models     []string
	modelsOn   bool
	// hideCommands replaces the text of commands (Actor.HidesCommandText).
	hideCommands bool
}

// usageCommandRedacted stands in for the text of a command.
const usageCommandRedacted = "[redacted]"

// where builds the conditions shared by usage_hours, tool_hours and
// permission_events; timeCol is hour or at.
func (sc *usageScope) where(timeCol string) (string, []any) {
	var conds []string
	var args []any
	switch {
	case sc.everyone:
		conds = append(conds, "1")
	case len(sc.uids) == 1:
		conds, args = append(conds, "user_id = ?"), append(args, sc.uids[0])
	default:
		conds = append(conds, "0")
	}
	conds, args = append(conds, timeCol+" >= ? AND "+timeCol+" < ?"), append(args, sc.from, sc.to)
	if sc.machine != "" {
		conds, args = append(conds, "machine = ?"), append(args, sc.machine)
	}
	if sc.session != "" {
		conds, args = append(conds, "session = ?"), append(args, sc.session)
	}
	if sc.repoFilter {
		if len(sc.repos) == 0 {
			conds = append(conds, "0")
		} else {
			conds = append(conds, "repo COLLATE NOCASE IN ("+placeholders(len(sc.repos))+")")
			for _, r := range sc.repos {
				args = append(args, r)
			}
		}
	}
	if sc.modelsOn {
		if len(sc.models) == 0 {
			conds = append(conds, "0")
		} else {
			conds = append(conds, "model IN ("+placeholders(len(sc.models))+")")
			for _, m := range sc.models {
				args = append(args, m)
			}
		}
	}
	return strings.Join(conds, " AND "), args
}

// usageWho works out whose usage the actor reads. An agent reads its own
// person's, without the text of commands (usageScope.hideCommands), and
// nobody else's. A runner job's token reaches its project and usage spans
// the person's projects, so it reads none (WP-ADR73).
func (s *Service) usageWho(ctx context.Context, actor *Actor, who string) (uids []int64, everyone bool, label string, err error) {
	if actor == nil || actor.User == nil {
		return nil, false, "", ErrUnauthorized
	}
	if err := actor.need(PermUsageRead); err != nil {
		return nil, false, "", err
	}
	if actor.scope != nil {
		return nil, false, "", forbidden("a runner job's token reaches its project; usage spans the person's")
	}
	who = strings.TrimSpace(who)
	if who != "" && !strings.EqualFold(who, actor.User.Username) {
		if err := actor.requireHuman("reading another person's usage"); err != nil {
			return nil, false, "", err
		}
	}
	switch {
	case who == "" || strings.EqualFold(who, actor.User.Username):
		return []int64{actor.User.ID}, false, actor.User.Username, nil
	case !actor.isAdmin():
		return nil, false, "", forbidden("usage is private to each person")
	case who == "all":
		return nil, true, "all", nil
	}
	var id int64
	var name string
	err = s.db.R.QueryRowContext(ctx, `SELECT id, username FROM users WHERE username = ? COLLATE NOCASE`, who).Scan(&id, &name)
	if err == sql.ErrNoRows {
		return nil, false, "", notFound("user " + who)
	}
	if err != nil {
		return nil, false, "", err
	}
	return []int64{id}, false, name, nil
}

// projectRepos is the repos attached to the project and its sub-projects.
func (s *Service) projectRepos(ctx context.Context, actor *Actor, ref string) ([]string, error) {
	p, err := s.projectByRef(ctx, s.db.R, ref)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, parent_id FROM projects`)
	if err != nil {
		return nil, err
	}
	children := map[int64][]int64{}
	for rows.Next() {
		var id int64
		var parent sql.NullInt64
		if err := rows.Scan(&id, &parent); err != nil {
			rows.Close()
			return nil, err
		}
		if parent.Valid {
			children[parent.Int64] = append(children[parent.Int64], id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids, queue := []int64{p.ID}, []int64{p.ID}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, c := range children[next] {
			if !slices.Contains(ids, c) {
				ids, queue = append(ids, c), append(queue, c)
			}
		}
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// #nosec G202 -- only ? placeholders are joined in; ids are bound
	rs, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT repo FROM project_repos WHERE project_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var repos []string
	for rs.Next() {
		var r string
		if err := rs.Scan(&r); err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rs.Err()
}

// repoProjects maps a repo (lower case) to the key of a project the actor
// can see that it is attached to.
func (s *Service) repoProjects(ctx context.Context, actor *Actor) (map[string]string, error) {
	where, args := visibleSQL("pr.project_id", actor)
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT lower(pr.repo), p.key FROM project_repos pr JOIN projects p ON p.id = pr.project_id
		WHERE `+where+` ORDER BY pr.project_id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var repo, key string
		if err := rows.Scan(&repo, &key); err != nil {
			return nil, err
		}
		out[repo] = key // the lowest project id wins: the loop ends on it
	}
	return out, rows.Err()
}

// usageScopeFor resolves a query: the period, whose data, and the filters.
func (s *Service) usageScopeFor(ctx context.Context, actor *Actor, q *UsageQuery) (*usageScope, *time.Location, time.Time, string, error) {
	uids, everyone, label, err := s.usageWho(ctx, actor, q.User)
	if err != nil {
		return nil, nil, time.Time{}, "", err
	}
	if q.Days <= 0 {
		q.Days = 30
	}
	q.Days = min(q.Days, 365)
	loc := s.actorLocation(actor)
	now := s.nowMS()
	today := time.UnixMilli(now).In(loc)
	first := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(q.Days - 1))
	sc := &usageScope{uids: uids, everyone: everyone, from: first.UnixMilli(), to: now + 1,
		machine: strings.TrimSpace(q.Machine), session: strings.TrimSpace(q.Session), hideCommands: actor.HidesCommandText()}
	if strings.TrimSpace(q.Project) != "" {
		repos, err := s.projectRepos(ctx, actor, q.Project)
		if err != nil {
			return nil, nil, time.Time{}, "", err
		}
		sc.repos, sc.repoFilter = repos, true
	}
	if spec := strings.TrimSpace(q.Agent); spec != "" {
		where, args := (&usageScope{uids: uids, everyone: everyone, from: sc.from, to: sc.to}).where("hour")
		// #nosec G202 -- where is built from fixed conditions; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT model FROM usage_hours WHERE `+where, args...)
		if err != nil {
			return nil, nil, time.Time{}, "", err
		}
		defer rows.Close()
		sc.modelsOn = true
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				return nil, nil, time.Time{}, "", err
			}
			if AgentMatches(spec, "anthropic/"+m) {
				sc.models = append(sc.models, m)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, nil, time.Time{}, "", err
		}
	}
	return sc, loc, first, label, nil
}

const (
	usageTopRows     = 25
	usageTopSessions = 30
	usageToolRows    = 30
	usageRecentShown = 25
)

// UsageOverview answers the Usage tab in one go.
func (s *Service) UsageOverview(ctx context.Context, actor *Actor, q UsageQuery) (*UsageOverview, error) {
	sc, loc, first, label, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return nil, err
	}
	o := &UsageOverview{From: first.UTC(), To: msTime(sc.to - 1), Days: q.Days, Who: label, Prices: actor.isAdmin(),
		Daily: []UsageDay{}, Models: []UsageBreakdown{}, Repos: []UsageBreakdown{}, Machines: []UsageBreakdown{}, Kinds: []UsageBreakdown{},
		TopSess: []UsageBreakdown{}, Tools: []UsageTool{}, Bash: []UsageTool{}, Unused: []UsageUnused{}, Advice: []UsageAdvice{},
		Reported: []UsageMachine{}, Unpriced: []string{}}
	if o.HasData, err = s.usageHasData(ctx, sc); err != nil {
		return nil, err
	}
	if actor.isAdmin() {
		if o.People, err = s.usagePeople(ctx); err != nil {
			return nil, err
		}
	}
	if o.Reported, err = s.usageMachines(ctx, sc); err != nil {
		return nil, err
	}
	b, err := s.usageTokens(ctx, sc, book, loc)
	if err != nil {
		return nil, err
	}
	b.fill(o)
	repoKey, err := s.repoProjects(ctx, actor)
	if err != nil {
		return nil, err
	}
	for i := range o.Repos {
		o.Repos[i].Project = repoKey[strings.ToLower(o.Repos[i].Key)]
	}
	if err := s.usageSessionMeta(ctx, sc, o, b, repoKey); err != nil {
		return nil, err
	}
	if err := s.usageToolsInto(ctx, sc, o); err != nil {
		return nil, err
	}
	if err := s.usageRefusalsInto(ctx, sc, loc, o); err != nil {
		return nil, err
	}
	if o.Quota, err = s.usageQuota(ctx, sc, q.Days); err != nil {
		return nil, err
	}
	if err := s.usageSuggest(ctx, sc, book, o, b); err != nil {
		return nil, err
	}
	return o, nil
}

func (s *Service) usageHasData(ctx context.Context, sc *usageScope) (bool, error) {
	all := &usageScope{uids: sc.uids, everyone: sc.everyone, from: 0, to: sc.to}
	where, args := all.where("hour")
	var one int
	// #nosec G202 -- where is built from fixed conditions; values are bound
	err := s.db.R.QueryRowContext(ctx, `SELECT 1 FROM usage_hours WHERE `+where+` LIMIT 1`, args...).Scan(&one)
	if err == sql.ErrNoRows {
		// A status line report alone is something to show too.
		w2, a2 := all.where("at")
		// #nosec G202 -- as above
		err = s.db.R.QueryRowContext(ctx, `SELECT 1 FROM quota_samples WHERE `+w2+` LIMIT 1`, a2...).Scan(&one)
		if err == sql.ErrNoRows {
			return false, nil
		}
	}
	return err == nil, err
}

func (s *Service) usagePeople(ctx context.Context) ([]UsagePerson, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT u.username, u.display_name FROM users u
		WHERE EXISTS (SELECT 1 FROM usage_hours h WHERE h.user_id = u.id) OR EXISTS (SELECT 1 FROM usage_machines m WHERE m.user_id = u.id)
		ORDER BY u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsagePerson{}
	for rows.Next() {
		var p UsagePerson
		if err := rows.Scan(&p.Username, &p.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) usageMachines(ctx context.Context, sc *usageScope) ([]UsageMachine, error) {
	var where string
	var args []any
	switch {
	case sc.everyone:
		where = "1"
	case len(sc.uids) == 1:
		where, args = "user_id = ?", []any{sc.uids[0]}
	default:
		where = "0"
	}
	// One row per person's machine, merged by name here: a name two people
	// share is one line, with each person's unknown-kind counts added up.
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT machine, last_sync_at, last_status_at, wp_version, parsed, skipped, unknown
		FROM usage_machines WHERE `+where+` ORDER BY machine, last_sync_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageMachine{}
	byName := map[string]int{}
	for rows.Next() {
		var m UsageMachine
		var sync, status sql.NullInt64
		var unknown sql.NullString
		if err := rows.Scan(&m.Name, &sync, &status, &m.Version, &m.Parsed, &m.Skipped, &unknown); err != nil {
			return nil, err
		}
		m.LastSyncAt, m.LastStatusAt = ptrTime(sync), ptrTime(status)
		m.Unknown = map[string]int64{}
		_ = json.Unmarshal([]byte(unknown.String), &m.Unknown)
		i, merged := byName[m.Name]
		if !merged {
			byName[m.Name] = len(out)
			out = append(out, m)
			continue
		}
		o := &out[i]
		o.Parsed += m.Parsed
		o.Skipped += m.Skipped
		for k, v := range m.Unknown {
			o.Unknown[k] += v
		}
		o.Version = cmp(m.Version, o.Version) // rows come oldest sync first: the newest version wins
		if m.LastSyncAt != nil && (o.LastSyncAt == nil || m.LastSyncAt.After(*o.LastSyncAt)) {
			o.LastSyncAt = m.LastSyncAt
		}
		if m.LastStatusAt != nil && (o.LastStatusAt == nil || m.LastStatusAt.After(*o.LastStatusAt)) {
			o.LastStatusAt = m.LastStatusAt
		}
	}
	return out, rows.Err()
}

// ---- tokens ----

type usageTokenData struct {
	total   usageAcc
	models  map[string]*usageAcc
	repos   map[string]*usageAcc
	machine map[string]*usageAcc
	kinds   map[string]*usageAcc
	session map[string]*usageAcc
	// per (kind, model), for the subagent advice
	kindModel map[[2]string]*usageAcc
	sessMeta  map[string]sessKey
	unpriced  map[string]bool
	days      map[string]map[string]*usageAcc
	book      *priceBook
}

type sessKey struct{ machine, repo string }

func accOf[K comparable](m map[K]*usageAcc, k K) *usageAcc {
	a := m[k]
	if a == nil {
		a = &usageAcc{}
		m[k] = a
	}
	return a
}

func (s *Service) usageTokens(ctx context.Context, sc *usageScope, book *priceBook, loc *time.Location) (*usageTokenData, error) {
	b := &usageTokenData{models: map[string]*usageAcc{}, repos: map[string]*usageAcc{}, machine: map[string]*usageAcc{}, kinds: map[string]*usageAcc{},
		session: map[string]*usageAcc{}, kindModel: map[[2]string]*usageAcc{}, sessMeta: map[string]sessKey{}, unpriced: map[string]bool{},
		days: map[string]map[string]*usageAcc{}, book: book}
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, repo, kind, model, sum(messages), sum(input), sum(output), sum(cache_write_5m), sum(cache_write_1h), sum(cache_read)
		FROM usage_hours WHERE `+where+` GROUP BY machine, session, repo, kind, model`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var machine, session, repo, kind, model string
		var n, in, out, cw5, cw1, cr int64
		if err := rows.Scan(&machine, &session, &repo, &kind, &model, &n, &in, &out, &cw5, &cw1, &cr); err != nil {
			rows.Close()
			return nil, err
		}
		p, ok := book.find(model)
		if !ok {
			b.unpriced[model] = true
		}
		b.total.add(n, in, out, cw5, cw1, cr, p, ok)
		accOf(b.models, model).add(n, in, out, cw5, cw1, cr, p, ok)
		accOf(b.repos, repo).add(n, in, out, cw5, cw1, cr, p, ok)
		accOf(b.machine, machine).add(n, in, out, cw5, cw1, cr, p, ok)
		accOf(b.kinds, kind).add(n, in, out, cw5, cw1, cr, p, ok)
		accOf(b.kindModel, [2]string{kind, model}).add(n, in, out, cw5, cw1, cr, p, ok)
		sk := machine + "\x00" + session
		accOf(b.session, sk).add(n, in, out, cw5, cw1, cr, p, ok)
		if _, seen := b.sessMeta[sk]; !seen {
			b.sessMeta[sk] = sessKey{machine, repo}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The daily series: hour buckets into the person's days.
	// #nosec G202 -- as above
	rows, err = s.db.R.QueryContext(ctx, `SELECT hour, model, sum(messages), sum(input), sum(output), sum(cache_write_5m), sum(cache_write_1h), sum(cache_read)
		FROM usage_hours WHERE `+where+` GROUP BY hour, model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hour, n, in, out, cw5, cw1, cr int64
		var model string
		if err := rows.Scan(&hour, &model, &n, &in, &out, &cw5, &cw1, &cr); err != nil {
			return nil, err
		}
		p, ok := book.find(model)
		day := time.UnixMilli(hour).In(loc).Format(dateLayout)
		if b.days[day] == nil {
			b.days[day] = map[string]*usageAcc{}
		}
		accOf(b.days[day], model).add(n, in, out, cw5, cw1, cr, p, ok)
	}
	return b, rows.Err()
}

func (b *usageTokenData) breakdowns(m map[string]*usageAcc, label func(string) (string, string)) []UsageBreakdown {
	out := make([]UsageBreakdown, 0, len(m))
	for k, a := range m {
		l, sub := k, ""
		if label != nil {
			l, sub = label(k)
		}
		out = append(out, UsageBreakdown{Key: k, Label: l, Sub: sub, UsageTotals: a.totals()})
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if xs, ys := x.Input+x.Output+x.CacheWrite, y.Input+y.Output+y.CacheWrite; xs != ys {
			return xs > ys
		}
		return x.Key < y.Key
	})
	return out
}

func (b *usageTokenData) fill(o *UsageOverview) {
	o.Totals = b.total.totals()
	o.Models = b.breakdowns(b.models, nil)
	o.Kinds = b.breakdowns(b.kinds, nil)
	o.Machines = b.breakdowns(b.machine, nil)
	o.Repos = b.breakdowns(b.repos, func(k string) (string, string) {
		if k == "" {
			return "(no repository)", ""
		}
		return k, ""
	})
	if len(o.Repos) > usageTopRows {
		o.Repos, o.Truncated = o.Repos[:usageTopRows], true
	}
	for m := range b.unpriced {
		o.Unpriced = append(o.Unpriced, m)
	}
	sort.Strings(o.Unpriced)
	o.Sessions = len(b.session)
	days := make([]string, 0, len(b.days))
	for d := range b.days {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days {
		models := make([]string, 0, len(b.days[d]))
		for m := range b.days[d] {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			o.Daily = append(o.Daily, UsageDay{Date: d, Model: m, UsageTotals: b.days[d][m].totals()})
		}
	}
}

// usageSessionMeta lists the heaviest sessions with what transcripts say about them.
func (s *Service) usageSessionMeta(ctx context.Context, sc *usageScope, o *UsageOverview, b *usageTokenData, repoKey map[string]string) error {
	return s.usageSessionMetaN(ctx, sc, o, b, repoKey, usageTopSessions)
}

func (s *Service) usageSessionMetaN(ctx context.Context, sc *usageScope, o *UsageOverview, b *usageTokenData, repoKey map[string]string, limit int) error {
	list := b.breakdowns(b.session, nil)
	if len(list) > limit {
		list, o.Truncated = list[:limit], true
	}
	if len(list) == 0 {
		return nil
	}
	keys := make([]any, 0, len(list))
	for _, l := range list {
		_, session, _ := strings.Cut(l.Key, "\x00")
		keys = append(keys, session)
	}
	var where string
	var args []any
	switch {
	case sc.everyone:
		where = "1"
	case len(sc.uids) == 1:
		where, args = "user_id = ?", []any{sc.uids[0]}
	default:
		where = "0"
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, repo, branch, title, last_at, compactions FROM usage_sessions
		WHERE `+where+` AND session IN (`+placeholders(len(keys))+`)`, append(args, keys...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type meta struct {
		repo, branch, title string
		last                sql.NullInt64
		compactions         int64
	}
	metas := map[string]meta{}
	for rows.Next() {
		var machine, session string
		var m meta
		if err := rows.Scan(&machine, &session, &m.repo, &m.branch, &m.title, &m.last, &m.compactions); err != nil {
			return err
		}
		metas[machine+"\x00"+session] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// The sessions the hooks registered, by machine and CLI session id.
	known := map[string]int64{}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	srows, err := s.db.R.QueryContext(ctx, `SELECT id, machine, cli_id FROM agent_sessions WHERE `+where+` AND cli_id IN (`+placeholders(len(keys))+`)`, append(args, keys...)...)
	if err != nil {
		return err
	}
	defer srows.Close()
	for srows.Next() {
		var id int64
		var machine, cli string
		if err := srows.Scan(&id, &machine, &cli); err != nil {
			return err
		}
		known[machine+"\x00"+cli] = id
	}
	if err := srows.Err(); err != nil {
		return err
	}
	for i := range list {
		machine, session, _ := strings.Cut(list[i].Key, "\x00")
		m := metas[list[i].Key]
		list[i].Machine, list[i].SessionID = machine, known[list[i].Key]
		sk := b.sessMeta[list[i].Key]
		repo := cmpOr(m.repo, sk.repo)
		list[i].Key = session
		list[i].Label = cmpOr(m.title, "Session "+session[:min(8, len(session))])
		list[i].Sub = strings.TrimSpace(repo + " · " + machine)
		list[i].Project = repoKey[strings.ToLower(repo)]
		list[i].LastAt = ptrTime(m.last)
		list[i].Compactions = m.compactions
	}
	o.TopSess = list
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- tools ----

func (s *Service) usageToolsInto(ctx context.Context, sc *usageScope, o *UsageOverview) error {
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT kind, tool, detail, sum(calls), sum(errors), sum(refusals), sum(prompts), sum(result_bytes)
		FROM tool_hours WHERE `+where+` GROUP BY kind, tool, detail`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	groups := map[string][]UsageTool{}
	mcp := map[string]*UsageTool{}
	for rows.Next() {
		var kind, tool, detail string
		var t UsageTool
		if err := rows.Scan(&kind, &tool, &detail, &t.Calls, &t.Errors, &t.Refusals, &t.Prompts, &t.Bytes); err != nil {
			return err
		}
		t.Group = kind
		switch kind {
		case "builtin":
			t.Name = tool
		case "mcp":
			t.Name = detail
			item := t
			item.Name = strings.TrimPrefix(tool, "mcp__"+detail+"__")
			item.Items = nil
			if cur := mcp[detail]; cur != nil {
				cur.Calls += t.Calls
				cur.Errors += t.Errors
				cur.Refusals += t.Refusals
				cur.Prompts += t.Prompts
				cur.Bytes += t.Bytes
				cur.Items = append(cur.Items, item)
			} else {
				t.Items = []UsageTool{item}
				mcp[detail] = &t
			}
			continue
		case "skill", "command", "subagent":
			t.Name = detail
		case "bash":
			t.Name = detail
			t.Group = "bash"
		default:
			continue
		}
		if t.Name == "" {
			continue
		}
		groups[t.Group] = append(groups[t.Group], t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range mcp {
		sort.Slice(t.Items, func(i, j int) bool { return t.Items[i].Calls > t.Items[j].Calls })
		if len(t.Items) > 8 {
			t.Items = t.Items[:8]
		}
		groups["mcp"] = append(groups["mcp"], *t)
	}
	for _, g := range []string{"builtin", "mcp", "skill", "command", "subagent"} {
		list := groups[g]
		sort.Slice(list, func(i, j int) bool {
			if list[i].Calls != list[j].Calls {
				return list[i].Calls > list[j].Calls
			}
			return list[i].Name < list[j].Name
		})
		if len(list) > usageToolRows {
			list = list[:usageToolRows]
		}
		o.Tools = append(o.Tools, list...)
	}
	bash := groups["bash"]
	sort.Slice(bash, func(i, j int) bool {
		if bash[i].Calls != bash[j].Calls {
			return bash[i].Calls > bash[j].Calls
		}
		return bash[i].Name < bash[j].Name
	})
	if len(bash) > 40 {
		bash = bash[:40]
	}
	o.Bash = append(o.Bash, bash...)
	return s.usageUnusedInto(ctx, sc, o)
}

// usageNorm makes names comparable across how Claude Code writes them
// (claude.ai Notion → claude_ai_Notion, /plugin:cmd → plugin:cmd).
func usageNorm(s string) string {
	s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "/"))
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ':' {
			return r
		}
		return '_'
	}, s)
}

func usageAfterColon(s string) string {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// usageUsedSet is every name of a kind used since from (any filter but the
// user, machine and period: unused means nowhere).
func (s *Service) usageUsedSet(ctx context.Context, sc *usageScope, from int64) (map[string]map[string]bool, error) {
	wide := &usageScope{uids: sc.uids, everyone: sc.everyone, from: from, to: sc.to}
	where, args := wide.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT kind, detail FROM tool_hours WHERE `+where+` AND kind IN ('mcp','skill','command','subagent') AND detail != ''`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[string]map[string]bool{"mcp": {}, "skill": {}, "command": {}, "agent": {}}
	for rows.Next() {
		var kind, detail string
		if err := rows.Scan(&kind, &detail); err != nil {
			return nil, err
		}
		if kind == "subagent" {
			kind = "agent"
		}
		n := usageNorm(detail)
		used[kind][n] = true
		used[kind][usageAfterColon(n)] = true
	}
	return used, rows.Err()
}

// usageUnusedSince lists what is configured but wasn't used since from.
func (s *Service) usageUnusedSince(ctx context.Context, sc *usageScope, from int64) (items []UsageUnused, inventory bool, err error) {
	used, err := s.usageUsedSet(ctx, sc, from)
	if err != nil {
		return nil, false, err
	}
	var where string
	var args []any
	switch {
	case sc.everyone:
		where = "1"
	case len(sc.uids) == 1:
		where, args = "user_id = ?", []any{sc.uids[0]}
	default:
		where = "0"
	}
	if sc.machine != "" {
		where, args = where+" AND machine = ?", append(args, sc.machine)
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT kind, name, scope, machine FROM usage_inventory WHERE `+where+` AND kind IN ('mcp','skill','command','agent') ORDER BY kind, name`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var u UsageUnused
		if err := rows.Scan(&u.Kind, &u.Name, &u.Scope, &u.Machine); err != nil {
			return nil, false, err
		}
		inventory = true
		n := usageNorm(u.Name)
		if used[u.Kind][n] || used[u.Kind][usageAfterColon(n)] {
			continue
		}
		k := u.Kind + "\x00" + u.Name
		if seen[k] { // the same thing on several machines is listed once
			continue
		}
		seen[k] = true
		items = append(items, u)
	}
	return items, inventory, rows.Err()
}

func (s *Service) usageUnusedInto(ctx context.Context, sc *usageScope, o *UsageOverview) error {
	items, inv, err := s.usageUnusedSince(ctx, sc, sc.from)
	if err != nil {
		return err
	}
	o.Unused, o.NoInvent = append(o.Unused, items...), !inv
	return nil
}
