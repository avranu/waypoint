package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"time"

	"git.jmann.me/claude/waypoint/internal/wql"
)

// BurndownPoint is one day of a sprint.
type BurndownPoint struct {
	Date            string   `json:"date"`
	RemainingPoints *float64 `json:"remaining_points"` // nil for future days
	RemainingIssues *int     `json:"remaining_issues"`
	IdealPoints     float64  `json:"ideal_points"`
}

// Burndown is a sprint's remaining work per day.
type Burndown struct {
	Sprint *Sprint          `json:"sprint"`
	Total  float64          `json:"total_points"`
	Days   []BurndownPoint  `json:"days"`
	Scope  []map[string]any `json:"scope_changes,omitempty"`
}

// SprintBurndown computes remaining points per day for a sprint, using each
// issue's resolution time. Scope is the sprint's current issues.
func (s *Service) SprintBurndown(ctx context.Context, actor *Actor, sprintID int64) (*Burndown, error) {
	sp, err := s.GetSprint(ctx, actor, sprintID)
	if err != nil {
		return nil, err
	}
	b := &Burndown{Sprint: sp, Days: []BurndownPoint{}}
	if sp.StartDate == nil || sp.EndDate == nil {
		return b, nil
	}
	loc := s.actorLocation(actor)
	start, err1 := time.ParseInLocation(dateLayout, *sp.StartDate, loc)
	end, err2 := time.ParseInLocation(dateLayout, *sp.EndDate, loc)
	if err1 != nil || err2 != nil || end.Before(start) {
		return b, nil
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT coalesce(i.points, 0), i.resolved_at, st.category FROM issues i JOIN statuses st ON st.id = i.status_id
		WHERE i.sprint_id = ? AND st.category != 'canceled'`, sprintID)
	if err != nil {
		return nil, err
	}
	type item struct {
		points   float64
		resolved int64
	}
	var items []item
	for rows.Next() {
		var it item
		var res sql.NullInt64
		var cat string
		if err := rows.Scan(&it.points, &res, &cat); err != nil {
			rows.Close()
			return nil, err
		}
		if cat == CatDone && res.Valid {
			it.resolved = res.Int64
		}
		items = append(items, it)
		b.Total += it.points
	}
	rows.Close()
	days := int(end.Sub(start).Hours()/24) + 1
	now := s.now()
	for d := 0; d < days; d++ {
		day := start.AddDate(0, 0, d)
		pt := BurndownPoint{Date: day.Format(dateLayout)}
		if days > 1 {
			pt.IdealPoints = b.Total * (1 - float64(d)/float64(days-1))
		}
		if !day.After(now) {
			cutoff := day.AddDate(0, 0, 1).UnixMilli()
			var rem float64
			var n int
			for _, it := range items {
				if it.resolved == 0 || it.resolved >= cutoff {
					rem += it.points
					n++
				}
			}
			pt.RemainingPoints, pt.RemainingIssues = &rem, &n
		}
		b.Days = append(b.Days, pt)
	}
	return b, nil
}

// VelocityEntry is one closed sprint's commitment and delivery.
type VelocityEntry struct {
	SprintID  int64   `json:"sprint_id"`
	Name      string  `json:"name"`
	Committed float64 `json:"committed"`
	Completed float64 `json:"completed"`
	ClosedAt  string  `json:"closed_at"`
}

// Velocity returns the last closed sprints, oldest first.
func (s *Service) Velocity(ctx context.Context, actor *Actor, projectRef string, limit int) ([]VelocityEntry, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, name, coalesce(committed_points, 0), coalesce(completed_points, 0), closed_at FROM sprints
		WHERE project_id = ? AND state = 'closed' ORDER BY closed_at DESC LIMIT ?`, p.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VelocityEntry{}
	for rows.Next() {
		var v VelocityEntry
		var closed int64
		if err := rows.Scan(&v.SprintID, &v.Name, &v.Committed, &v.Completed, &closed); err != nil {
			return nil, err
		}
		v.ClosedAt = msTime(closed).Format(dateLayout)
		out = append(out, v)
	}
	slices.Reverse(out)
	return out, rows.Err()
}

// WeekStat is one week of flow.
type WeekStat struct {
	Week            string  `json:"week"` // Monday, YYYY-MM-DD
	CreatedIssues   int     `json:"created_issues"`
	CreatedPoints   float64 `json:"created_points"`
	CompletedIssues int     `json:"completed_issues"`
	CompletedPoints float64 `json:"completed_points"`
	// Cumulative totals at the end of the week (burnup).
	ScopePoints float64 `json:"scope_points"`
	DonePoints  float64 `json:"done_points"`
	ScopeIssues int     `json:"scope_issues"`
	DoneIssues  int     `json:"done_issues"`
}

func weekStart(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	offset := (int(d.Weekday()) + 6) % 7
	return d.AddDate(0, 0, -offset)
}

type flowIssue struct {
	id       int64
	points   float64
	pointsOK bool
	created  int64
	resolved int64
	canceled bool
	typ      string
	children int
	rank     float64
	parentID int64
}

// leafWork loads issues that represent actual work: everything except
// epics/parents whose points would double count their children.
func (s *Service) flowIssues(ctx context.Context, projectID int64, extra string, args ...any) ([]flowIssue, error) {
	// #nosec G202 -- extra is a fixed condition from this package; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, i.points, i.created_at, i.resolved_at, st.category, i.type, i.rank, coalesce(i.parent_id, 0),
		(SELECT count(*) FROM issues c WHERE c.parent_id = i.id)
		FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.project_id = ? `+extra, append([]any{projectID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []flowIssue
	for rows.Next() {
		var f flowIssue
		var pts sql.NullFloat64
		var res sql.NullInt64
		var cat string
		if err := rows.Scan(&f.id, &pts, &f.created, &res, &cat, &f.typ, &f.rank, &f.parentID, &f.children); err != nil {
			return nil, err
		}
		f.points, f.pointsOK = pts.Float64, pts.Valid
		if cat == CatDone && res.Valid {
			f.resolved = res.Int64
		}
		f.canceled = cat == CatCanceled
		if f.children > 0 {
			continue
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Throughput returns weekly created/completed work and cumulative burnup.
func (s *Service) Throughput(ctx context.Context, actor *Actor, projectRef string, weeks int) ([]WeekStat, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	if weeks <= 0 || weeks > 104 {
		weeks = 12
	}
	weeks = min(weeks, 104)
	issues, err := s.flowIssues(ctx, p.ID, "")
	if err != nil {
		return nil, err
	}
	loc := s.actorLocation(actor)
	first := weekStart(s.now().In(loc)).AddDate(0, 0, -7*(weeks-1))
	out := make([]WeekStat, weeks)
	for w := range out {
		ws := first.AddDate(0, 0, 7*w)
		we := ws.AddDate(0, 0, 7).UnixMilli()
		st := &out[w]
		st.Week = ws.Format(dateLayout)
		for _, f := range issues {
			if f.canceled {
				continue
			}
			if f.created >= ws.UnixMilli() && f.created < we {
				st.CreatedIssues++
				st.CreatedPoints += f.points
			}
			if f.resolved >= ws.UnixMilli() && f.resolved < we {
				st.CompletedIssues++
				st.CompletedPoints += f.points
			}
			if f.created < we {
				st.ScopeIssues++
				st.ScopePoints += f.points
				if f.resolved != 0 && f.resolved < we {
					st.DoneIssues++
					st.DonePoints += f.points
				}
			}
		}
	}
	return out, nil
}

// FlowDay counts issues per status category at the end of a day.
type FlowDay struct {
	Date   string         `json:"date"`
	Counts map[string]int `json:"counts"`
}

// CumulativeFlow replays status changes to count issues per category per day.
func (s *Service) CumulativeFlow(ctx context.Context, actor *Actor, projectRef string, days int) ([]FlowDay, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	if days <= 0 || days > 365 {
		days = 30
	}
	days = min(days, 365)
	type change struct {
		at       int64
		old, new string
	}
	type hist struct {
		created int64
		current string
		changes []change
	}
	issues := map[int64]*hist{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, i.created_at, st.category FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.project_id = ?`, p.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, c int64
		var cat string
		if err := rows.Scan(&id, &c, &cat); err != nil {
			rows.Close()
			return nil, err
		}
		issues[id] = &hist{created: c, current: cat}
	}
	rows.Close()
	rows, err = s.db.R.QueryContext(ctx, `SELECT issue_id, created_at, data FROM events WHERE project_id = ? AND kind = 'issue.status_changed' ORDER BY id`, p.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, at int64
		var data string
		if err := rows.Scan(&id, &at, &data); err != nil {
			rows.Close()
			return nil, err
		}
		h := issues[id]
		if h == nil {
			continue
		}
		var d struct {
			Old string `json:"old_category"`
			New string `json:"new_category"`
		}
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			continue
		}
		h.changes = append(h.changes, change{at: at, old: d.Old, new: d.New})
	}
	rows.Close()
	loc := s.actorLocation(actor)
	today := time.Date(s.now().In(loc).Year(), s.now().In(loc).Month(), s.now().In(loc).Day(), 0, 0, 0, 0, loc)
	out := make([]FlowDay, days)
	for d := 0; d < days; d++ {
		day := today.AddDate(0, 0, d-days+1)
		cutoff := day.AddDate(0, 0, 1).UnixMilli()
		counts := map[string]int{}
		for _, h := range issues {
			if h.created >= cutoff {
				continue
			}
			cat := h.current
			for _, c := range h.changes {
				if c.at >= cutoff {
					cat = c.old
					break
				}
			}
			if cat != "" {
				counts[cat]++
			}
		}
		out[d] = FlowDay{Date: day.Format(dateLayout), Counts: counts}
	}
	return out, nil
}

// Forecast projects when remaining work will be done from recent throughput.
type Forecast struct {
	// Basis is "points" when most work is estimated, otherwise "issues".
	Basis            string    `json:"basis"`
	Remaining        float64   `json:"remaining"`
	RemainingIssues  int       `json:"remaining_issues"`
	Unestimated      int       `json:"unestimated"`
	AssumedPoints    float64   `json:"assumed_points"`
	WeeklyThroughput []float64 `json:"weekly_throughput"`
	AveragePerWeek   float64   `json:"average_per_week"`
	// Completion dates at 50/85/95% confidence (Monte Carlo); empty when
	// there's no recent throughput to project from.
	P50     string `json:"p50,omitempty"`
	P85     string `json:"p85,omitempty"`
	P95     string `json:"p95,omitempty"`
	Message string `json:"message,omitempty"`
}

// ForecastScope selects the work to forecast: a whole project, or the open
// work matching a query (e.g. "under:WAY-12" for an epic).
func (s *Service) ForecastWork(ctx context.Context, actor *Actor, projectRef, query string) (*Forecast, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	all, err := s.flowIssues(ctx, p.ID, "")
	if err != nil {
		return nil, err
	}
	scope := map[int64]bool{}
	if query != "" {
		q, err := wql.Parse(query)
		if err != nil {
			return nil, invalid("%s", err.Error())
		}
		c, err := s.compileQuery(ctx, actor, q, p.ID)
		if err != nil {
			return nil, err
		}
		sqlText, args := c.sql(`SELECT i.id FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id LEFT JOIN sprints sp ON sp.id = i.sprint_id`)
		rows, err := s.db.R.QueryContext(ctx, sqlText, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			scope[id] = true
		}
		rows.Close()
	}
	return s.forecast(all, func(f flowIssue) bool { return query == "" || scope[f.id] }, p.Settings, s.actorLocation(actor)), nil
}

func (s *Service) forecast(all []flowIssue, inScope func(flowIssue) bool, settings ProjectSettings, loc *time.Location) *Forecast {
	fc := &Forecast{}
	var estimated, estimatedSum float64
	var openEstimated, openTotal int
	for _, f := range all {
		if f.canceled {
			continue
		}
		if f.pointsOK {
			estimated++
			estimatedSum += f.points
		}
		if f.resolved == 0 && inScope(f) {
			openTotal++
			if f.pointsOK {
				openEstimated++
			}
		}
	}
	fc.RemainingIssues = openTotal
	fc.Unestimated = openTotal - openEstimated
	usePoints := openTotal > 0 && float64(openEstimated)/float64(openTotal) >= 0.5
	fc.Basis = "issues"
	if usePoints {
		fc.Basis = "points"
		fc.AssumedPoints = settings.DefaultPoints
		if fc.AssumedPoints <= 0 {
			fc.AssumedPoints = 1
			if estimated > 0 {
				fc.AssumedPoints = math.Round(estimatedSum/estimated*10) / 10
			}
		}
	}
	size := func(f flowIssue) float64 {
		if !usePoints {
			return 1
		}
		if f.pointsOK {
			return f.points
		}
		return fc.AssumedPoints
	}
	for _, f := range all {
		if !f.canceled && f.resolved == 0 && inScope(f) {
			fc.Remaining += size(f)
		}
	}
	weeks := settings.VelocityWeeks
	now := s.now().In(loc)
	first := weekStart(now).AddDate(0, 0, -7*weeks)
	fc.WeeklyThroughput = make([]float64, weeks)
	for _, f := range all {
		if f.resolved == 0 || f.canceled {
			continue
		}
		w := int(math.Floor(float64(f.resolved-first.UnixMilli()) / float64(7*24*time.Hour/time.Millisecond)))
		if w >= 0 && w < weeks {
			fc.WeeklyThroughput[w] += size(f)
		}
	}
	var sum float64
	for _, v := range fc.WeeklyThroughput {
		sum += v
	}
	fc.AveragePerWeek = math.Round(sum/float64(weeks)*10) / 10
	if fc.Remaining == 0 {
		fc.Message = "Nothing left to do."
		today := now.Format(dateLayout)
		fc.P50, fc.P85, fc.P95 = today, today, today
		return fc
	}
	if sum == 0 {
		fc.Message = "No work finished in the last " + itoa(weeks) + " weeks, so there's nothing to project from yet."
		return fc
	}
	rng := rand.New(rand.NewPCG(42, uint64(len(all)))) // #nosec G404 -- deterministic forecast seed by design
	const runs = 2000
	results := make([]int, runs)
	for r := 0; r < runs; r++ {
		left, n := fc.Remaining, 0
		for left > 0 && n < 520 {
			left -= fc.WeeklyThroughput[rng.IntN(weeks)]
			n++
		}
		results[r] = n
	}
	sort.Ints(results)
	at := func(pct float64) string {
		w := results[int(float64(runs-1)*pct)]
		if w >= 520 {
			return ""
		}
		return now.AddDate(0, 0, 7*w).Format(dateLayout)
	}
	fc.P50, fc.P85, fc.P95 = at(0.5), at(0.85), at(0.95)
	return fc
}

// EpicProgress is an epic's completion and projected finish.
type EpicProgress struct {
	Epic        *Issue    `json:"epic"`
	Total       int       `json:"total"`
	Done        int       `json:"done"`
	Points      float64   `json:"points"`
	DonePoints  float64   `json:"done_points"`
	Forecast    *Forecast `json:"forecast"`
	Sequential  string    `json:"sequential_p85,omitempty"`
	Description string    `json:"-"`
}

// Roadmap returns open epics in rank order with progress, each forecast on
// its own and, sequentially, as if worked in rank order.
func (s *Service) Roadmap(ctx context.Context, actor *Actor, projectRef string) ([]EpicProgress, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	epics, err := s.loadIssues(ctx, s.db.R, issueSelect(false)+` WHERE i.project_id = ? AND i.type = 'epic' AND st.category NOT IN ('done','canceled') AND `+draftsHidden(actor, "i")+` ORDER BY i.rank`, p.ID)
	if err != nil {
		return nil, err
	}
	all, err := s.flowIssues(ctx, p.ID, "")
	if err != nil {
		return nil, err
	}
	parents := map[int64]int64{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, coalesce(parent_id, 0) FROM issues WHERE project_id = ?`, p.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, pid int64
		if err := rows.Scan(&id, &pid); err != nil {
			rows.Close()
			return nil, err
		}
		parents[id] = pid
	}
	rows.Close()
	under := func(id, epic int64) bool {
		for n := 0; n < 50 && id != 0; n++ {
			id = parents[id]
			if id == epic {
				return true
			}
		}
		return false
	}
	loc := s.actorLocation(actor)
	out := []EpicProgress{}
	cumulative := map[int64]bool{}
	for _, e := range epics {
		ep := EpicProgress{Epic: e}
		for _, f := range all {
			if !under(f.id, e.ID) || f.canceled {
				continue
			}
			ep.Total++
			ep.Points += f.points
			if f.resolved != 0 {
				ep.Done++
				ep.DonePoints += f.points
			}
		}
		eid := e.ID
		ep.Forecast = s.forecast(all, func(f flowIssue) bool { return under(f.id, eid) }, p.Settings, loc)
		for _, f := range all {
			if under(f.id, eid) {
				cumulative[f.id] = true
			}
		}
		seq := s.forecast(all, func(f flowIssue) bool { return cumulative[f.id] }, p.Settings, loc)
		ep.Sequential = seq.P85
		out = append(out, ep)
	}
	return out, nil
}

// TimeReport sums tracked time.
type TimeReport struct {
	TotalSeconds int64        `json:"total_seconds"`
	ByDay        []TimeBucket `json:"by_day"`
	ByProject    []TimeBucket `json:"by_project"`
	ByIssue      []TimeBucket `json:"by_issue"`
	ByUser       []TimeBucket `json:"by_user,omitempty"`
	// Agents' time comes from their runs (claim to done/release/expiry):
	// kept apart from people's, not in TotalSeconds.
	AgentSeconds int64            `json:"agent_seconds"`
	ByAgent      []TimeBucket     `json:"by_agent"`
	Raw          map[string]int64 `json:"-"`
}

// TimeBucket is a labelled total.
type TimeBucket struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Seconds int64  `json:"seconds"`
}

// GetTimeReport summarises time between from and to: the actor's own, or
// everyone's within a project.
func (s *Service) GetTimeReport(ctx context.Context, actor *Actor, from, to time.Time, projectRef string) (*TimeReport, error) {
	if !to.After(from) {
		return nil, invalid("the end of the range must be after the start")
	}
	if to.Sub(from) > 366*24*time.Hour {
		return nil, invalid("time reports cover at most a year")
	}
	q := TimeQuery{From: &from, To: &to, Project: projectRef, all: true}
	entries, err := s.ListTimeEntries(ctx, actor, q)
	if err != nil {
		return nil, err
	}
	loc := s.actorLocation(actor)
	r := &TimeReport{}
	day, proj, iss, usr := map[string]int64{}, map[string]int64{}, map[string]int64{}, map[string]int64{}
	labels := map[string]string{}
	fromMS, toMS := from.UnixMilli(), to.UnixMilli()
	now := s.nowMS()
	for _, e := range entries {
		start := max(e.StartedAt.UnixMilli(), fromMS)
		end := now
		if e.EndedAt != nil {
			end = e.EndedAt.UnixMilli()
		}
		end = min(end, toMS)
		if end <= start {
			continue
		}
		secs := (end - start) / 1000
		r.TotalSeconds += secs
		for t := start; t < end; {
			lt := time.UnixMilli(t).In(loc)
			next := time.Date(lt.Year(), lt.Month(), lt.Day()+1, 0, 0, 0, 0, loc).UnixMilli()
			seg := min(next, end) - t
			day[lt.Format(dateLayout)] += seg / 1000
			t = min(next, end)
		}
		pk := cmp(e.ProjectKey, "—")
		proj[pk] += secs
		if e.Issue != nil {
			iss[e.Issue.Key] += secs
			labels[e.Issue.Key] = e.Issue.Title
		} else {
			iss["—"] += secs
			labels["—"] = "No issue"
		}
		usr[e.User.Username] += secs
		labels["u:"+e.User.Username] = e.User.DisplayName
	}
	for d := from.In(loc); d.Before(to); d = d.AddDate(0, 0, 1) {
		k := d.Format(dateLayout)
		r.ByDay = append(r.ByDay, TimeBucket{Key: k, Label: d.Format("Mon Jan 2"), Seconds: day[k]})
	}
	toBuckets := func(m map[string]int64, label func(string) string) []TimeBucket {
		out := []TimeBucket{}
		for k, v := range m {
			out = append(out, TimeBucket{Key: k, Label: label(k), Seconds: v})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Seconds > out[j].Seconds })
		return out
	}
	r.ByProject = toBuckets(proj, func(k string) string { return k })
	r.ByIssue = toBuckets(iss, func(k string) string { return labels[k] })
	if projectRef != "" {
		r.ByUser = toBuckets(usr, func(k string) string { return labels["u:"+k] })
	}
	agents, err := s.agentTime(ctx, actor, fromMS, toMS, projectRef)
	if err != nil {
		return nil, err
	}
	for _, v := range agents {
		r.AgentSeconds += v
	}
	r.ByAgent = toBuckets(agents, func(k string) string { return k })
	return r, nil
}

// agentTime sums agents' runs between two times: the actor's own agents
// (runs under their tokens), or every agent's within a project.
func (s *Service) agentTime(ctx context.Context, actor *Actor, fromMS, toMS int64, projectRef string) (map[string]int64, error) {
	cond, arg := "r.user_id = ?", any(actor.id())
	if projectRef != "" {
		p, err := s.projectByRef(ctx, s.db.R, projectRef)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		cond, arg = "i.project_id = ?", p.ID
	}
	now := s.nowMS()
	// #nosec G202 -- cond is one of two fixed conditions
	rows, err := s.db.R.QueryContext(ctx, `SELECT agent, start, stop FROM (
			SELECT r.agent AS agent, max(r.started_at, ?) AS start, min(coalesce(r.ended_at, min(?, coalesce(
				(SELECT x.claimed_until FROM issues x WHERE x.id = r.issue_id AND x.claimed_by = r.agent), r.started_at + ?))), ?) AS stop
			FROM agent_runs r JOIN issues i ON i.id = r.issue_id
			WHERE `+cond+` AND r.started_at < ?)
		WHERE stop > start ORDER BY agent, start`, fromMS, now, ClaimLease.Milliseconds(), toMS, arg, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	var agent string
	var curStart, curEnd int64
	flush := func() {
		if agent != "" && curEnd > curStart {
			out[agent] += (curEnd - curStart) / 1000
		}
	}
	for rows.Next() {
		var a string
		var start, end int64
		if err := rows.Scan(&a, &start, &end); err != nil {
			return nil, err
		}
		if a != agent || start > curEnd {
			flush()
			agent, curStart, curEnd = a, start, end
			continue
		}
		curEnd = max(curEnd, end)
	}
	flush()
	return out, rows.Err()
}
