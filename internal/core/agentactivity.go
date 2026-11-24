package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// ActivityQuery narrows the activity shown.
type ActivityQuery struct {
	Agent   string // an agent spec, matched against each run's identity and model
	Project string // a project key
	Days    int    // the period, ending now (default 30)
}

// ActivityTotals sums the period.
type ActivityTotals struct {
	TasksDone     int     `json:"tasks_done"`
	Runs          int     `json:"runs"`
	Seconds       int64   `json:"seconds"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CostUSD       float64 `json:"cost_usd"`
	PRsOpened     int     `json:"prs_opened"`
	PRsMerged     int     `json:"prs_merged"`
	IssuesCreated int     `json:"issues_created"`
	Comments      int     `json:"comments"`
	BotReviews    int     `json:"bot_reviews"`
}

// AgentActivity is one agent identity's share of the period.
type AgentActivity struct {
	Identity      string     `json:"identity"`
	Family        string     `json:"family"`
	Models        []string   `json:"models"`
	Runs          int        `json:"runs"`
	Done          int        `json:"done"`
	Released      int        `json:"released"`
	Expired       int        `json:"expired"` // its claim lapsed or its clock ran out
	Seconds       int64      `json:"seconds"`
	InputTokens   int64      `json:"input_tokens"`
	OutputTokens  int64      `json:"output_tokens"`
	CostUSD       float64    `json:"cost_usd"`
	PRsMerged     int        `json:"prs_merged"`
	IssuesCreated int        `json:"issues_created"`
	Comments      int        `json:"comments"`
	LastActive    *time.Time `json:"last_active"`
}

// ActivityDay is one day of the period, in the actor's time zone.
type ActivityDay struct {
	Date    string `json:"date"` // YYYY-MM-DD
	Seconds int64  `json:"seconds"`
	Tokens  int64  `json:"tokens"`
	Done    int    `json:"done"`
}

// BotActivity is what one model did for the review bots.
type BotActivity struct {
	Model    string `json:"model"`
	Reviews  int    `json:"reviews"`  // runs it answered in
	Findings int    `json:"findings"` // findings it raised
	Failed   int    `json:"failed"`   // calls that didn't answer (the next model was tried)
	Seconds  int64  `json:"seconds"`  // time spent waiting on it
}

// ActivityPR is a pull request linked to an agent's work.
type ActivityPR struct {
	URL    string `json:"url"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"` // open, draft, merged, closed, or '' when unknown
}

// ActivityIssue is the issue a run was on.
type ActivityIssue struct {
	Key            string `json:"key"`
	Title          string `json:"title"`
	ProjectKey     string `json:"project_key"`
	Status         string `json:"status"`
	StatusCategory string `json:"status_category"`
}

// ActivityRun is one run, for the list of recent work.
type ActivityRun struct {
	ID           int64         `json:"id"`
	Agent        string        `json:"agent"`
	Model        string        `json:"model,omitempty"`
	Step         string        `json:"step,omitempty"`
	Issue        ActivityIssue `json:"issue"`
	StartedAt    time.Time     `json:"started_at"`
	EndedAt      *time.Time    `json:"ended_at"`
	EndInferred  bool          `json:"end_inferred,omitempty"` // EndedAt is the sweep's bound for a run it never saw end (WP-1470)
	Seconds      int64         `json:"seconds"`
	Outcome      string        `json:"outcome"` // done, released, expired, paused, timeout; '' while working
	InputTokens  int64         `json:"input_tokens"`
	OutputTokens int64         `json:"output_tokens"`
	CostUSD      float64       `json:"cost_usd"`
	PRs          []ActivityPR  `json:"prs"`
}

// Activity is the answer.
type Activity struct {
	From   time.Time        `json:"from"`
	To     time.Time        `json:"to"`
	Totals ActivityTotals   `json:"totals"`
	Agents []*AgentActivity `json:"agents"`
	Daily  []ActivityDay    `json:"daily"`
	Bots   []*BotActivity   `json:"bots"`
	Runs   []ActivityRun    `json:"runs"`
}

// runMatches: a run belongs to spec when its identity or the model it
// reported does ("opus" finds anthropic/claude-code runs on Opus).
func runMatches(spec, agent, model string) bool {
	if spec == "" {
		return true
	}
	if model == "" {
		return AgentMatches(spec, agent)
	}
	return AgentMatches(spec, agent+"/"+model)
}

const activityRunsShown = 100

// AgentActivity reports what agents did in the period.
func (s *Service) AgentActivity(ctx context.Context, actor *Actor, q ActivityQuery) (*Activity, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if q.Days <= 0 {
		q.Days = 30
	}
	q.Days = min(q.Days, 365)
	q.Agent = strings.TrimSpace(q.Agent)
	now := s.nowMS()
	loc := s.actorLocation(actor)
	today := time.UnixMilli(now).In(loc)
	first := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(q.Days - 1))
	from := first.UnixMilli()

	where, args := visibleSQL("i.project_id", actor)
	if q.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		where, args = where+" AND i.project_id = ?", append(args, p.ID)
	}
	a := &activityBuilder{s: s, q: q, from: from, to: now, loc: loc, first: first,
		agents: map[string]*AgentActivity{}, spans: map[string][][2]int64{}}
	if err := a.runs(ctx, where, args); err != nil {
		return nil, err
	}
	if err := a.writing(ctx, where, args); err != nil {
		return nil, err
	}
	if err := a.pulls(ctx, where, args); err != nil {
		return nil, err
	}
	if err := a.reviewBots(ctx, actor, q.Project); err != nil {
		return nil, err
	}
	return a.result(), nil
}

type activityBuilder struct {
	s        *Service
	q        ActivityQuery
	from, to int64
	loc      *time.Location
	first    time.Time
	totals   ActivityTotals
	agents   map[string]*AgentActivity
	order    []string
	spans    map[string][][2]int64
	days     map[string]*ActivityDay
	runList  []ActivityRun
	issueIDs map[int64][]string
	bots     []*BotActivity
}

func (a *activityBuilder) agent(identity, model string) *AgentActivity {
	key := normalizeAgent(identity)
	x := a.agents[key]
	if x == nil {
		x = &AgentActivity{Identity: identity, Family: AgentFamily(identity), Models: []string{}}
		a.agents[key] = x
		a.order = append(a.order, key)
	}
	if model != "" && !slices.Contains(x.Models, model) {
		x.Models = append(x.Models, model)
	}
	return x
}

func (a *activityBuilder) active(x *AgentActivity, ms int64) {
	if ms > 0 && (x.LastActive == nil || x.LastActive.UnixMilli() < ms) {
		t := msTime(ms)
		x.LastActive = &t
	}
}

func (a *activityBuilder) day(ms int64) *ActivityDay {
	if a.days == nil {
		a.days = map[string]*ActivityDay{}
	}
	d := time.UnixMilli(ms).In(a.loc).Format(dateLayout)
	x := a.days[d]
	if x == nil {
		x = &ActivityDay{Date: d}
		a.days[d] = x
	}
	return x
}

// runs reads the runs that overlap the period. A run's tokens and cost count
// in the period it started in; its time counts where it falls.
func (a *activityBuilder) runs(ctx context.Context, where string, args []any) error {
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := a.s.db.R.QueryContext(ctx, `SELECT r.id, r.agent, r.model, r.step, r.started_at, r.ended_at,
			min(coalesce(r.ended_at, min(?, coalesce((SELECT x.claimed_until FROM issues x WHERE x.id = r.issue_id AND x.claimed_by = r.agent), r.started_at + ?))), ?),
			r.input_tokens, r.output_tokens, r.cost_usd, r.outcome, r.end_inferred,
			i.id, p.key || '-' || i.number, i.title, p.key, st.name, st.category
		FROM agent_runs r JOIN issues i ON i.id = r.issue_id JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE `+where+` AND r.started_at < ? AND coalesce(r.ended_at, ?) > ?
		ORDER BY r.started_at DESC, r.id DESC LIMIT 20000`,
		append(append([]any{a.to, ClaimLease.Milliseconds(), a.to}, args...), a.to, a.to, a.from)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	a.issueIDs = map[int64][]string{}
	for rows.Next() {
		var r ActivityRun
		var start, stop int64
		var end sql.NullInt64
		var issueID int64
		if err := rows.Scan(&r.ID, &r.Agent, &r.Model, &r.Step, &start, &end, &stop, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.Outcome, &r.EndInferred,
			&issueID, &r.Issue.Key, &r.Issue.Title, &r.Issue.ProjectKey, &r.Issue.Status, &r.Issue.StatusCategory); err != nil {
			return err
		}
		if !runMatches(a.q.Agent, r.Agent, r.Model) {
			continue
		}
		r.StartedAt, r.EndedAt = msTime(start), ptrTime(end)
		r.Seconds = max(0, (stop-start)/1000)
		x := a.agent(r.Agent, r.Model)
		a.active(x, stop)
		if !slices.Contains(a.issueIDs[issueID], r.Agent) {
			a.issueIDs[issueID] = append(a.issueIDs[issueID], r.Agent)
		}
		if lo, hi := max(start, a.from), min(stop, a.to); hi > lo {
			key := normalizeAgent(r.Agent)
			a.spans[key] = append(a.spans[key], [2]int64{lo, hi})
		}
		if start >= a.from {
			x.Runs++
			x.InputTokens += r.InputTokens
			x.OutputTokens += r.OutputTokens
			x.CostUSD += r.CostUSD
			a.day(start).Tokens += r.InputTokens + r.OutputTokens
		}
		if end.Valid && end.Int64 >= a.from {
			switch r.Outcome {
			case "done":
				x.Done++
				a.day(end.Int64).Done++
			case "released":
				x.Released++
			case "expired", "timeout":
				x.Expired++
			}
		}
		if len(a.runList) < activityRunsShown {
			r.PRs = []ActivityPR{}
			a.runList = append(a.runList, r)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return a.runPRs(ctx)
}

// runPRs attaches the pull requests linked to each listed run's issue.
func (a *activityBuilder) runPRs(ctx context.Context) error {
	byKey := map[string][]int{}
	keys := []any{}
	for n, r := range a.runList {
		if _, ok := byKey[r.Issue.Key]; !ok {
			keys = append(keys, r.Issue.Key)
		}
		byKey[r.Issue.Key] = append(byKey[r.Issue.Key], n)
	}
	if len(keys) == 0 {
		return nil
	}
	// #nosec G202 -- only placeholders are added
	rows, err := a.s.db.R.QueryContext(ctx, `SELECT p.key || '-' || i.number, l.url, l.repo, coalesce(l.number, 0), l.title,
			coalesce((SELECT pr.state FROM pull_requests pr WHERE pr.url = l.url LIMIT 1), l.state)
		FROM external_links l JOIN issues i ON i.id = l.issue_id JOIN projects p ON p.id = i.project_id
		WHERE l.kind = 'pull' AND p.key || '-' || i.number IN (`+placeholders(len(keys))+`) ORDER BY l.id`, keys...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var pr ActivityPR
		if err := rows.Scan(&key, &pr.URL, &pr.Repo, &pr.Number, &pr.Title, &pr.State); err != nil {
			return err
		}
		for _, n := range byKey[key] {
			a.runList[n].PRs = append(a.runList[n].PRs, pr)
		}
	}
	return rows.Err()
}

// writing counts the issues and comments agents wrote in the period.
func (a *activityBuilder) writing(ctx context.Context, where string, args []any) error {
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := a.s.db.R.QueryContext(ctx, `
		SELECT i.reporter_agent, 1, 0, i.created_at FROM issues i
			WHERE `+where+` AND i.reporter_agent != '' AND i.created_at >= ?
		UNION ALL
		SELECT c.agent, 0, 1, c.created_at FROM comments c JOIN issues i ON i.id = c.issue_id
			WHERE `+where+` AND c.agent != '' AND c.created_at >= ?`,
		append(append(append(slices.Clone(args), a.from), args...), a.from)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var agent string
		var issue, comment int
		var at int64
		if err := rows.Scan(&agent, &issue, &comment, &at); err != nil {
			return err
		}
		if !runMatches(a.q.Agent, agent, "") {
			continue
		}
		x := a.agent(agent, "")
		x.IssuesCreated += issue
		x.Comments += comment
		a.active(x, at)
	}
	return rows.Err()
}

// pulls counts the pull requests that came of agents' work: linked to an
// issue an agent in the period worked on, linked by an agent, or naming a
// coding agent in their text, opened or merged in the period.
func (a *activityBuilder) pulls(ctx context.Context, where string, args []any) error {
	mergedAt := map[string]int64{}
	ev, err := a.s.db.R.QueryContext(ctx, `SELECT new_value, min(created_at) FROM events WHERE kind = ? AND created_at >= ? GROUP BY new_value`,
		EvExternalPRMerged, a.from)
	if err != nil {
		return err
	}
	for ev.Next() {
		var url string
		var at int64
		if err := ev.Scan(&url, &at); err != nil {
			ev.Close()
			return err
		}
		mergedAt[url] = at
	}
	ev.Close()
	if err := ev.Err(); err != nil {
		return err
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := a.s.db.R.QueryContext(ctx, `SELECT l.url, l.issue_id, l.created_agent, l.created_at,
			coalesce(pr.agent, ''), pr.opened_at, pr.merged_at, coalesce(pr.state, l.state)
		FROM external_links l JOIN issues i ON i.id = l.issue_id
			LEFT JOIN pull_requests pr ON pr.url = l.url
		WHERE `+where+` AND l.kind = 'pull'`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pull struct {
		agents         []string
		opened, merged int64
	}
	byURL := map[string]*pull{}
	var urls []string
	for rows.Next() {
		var url, linker, named, state string
		var issueID, linked int64
		var opened, merged sql.NullInt64
		if err := rows.Scan(&url, &issueID, &linker, &linked, &named, &opened, &merged, &state); err != nil {
			return err
		}
		p := byURL[url]
		if p == nil {
			p = &pull{opened: linked}
			byURL[url] = p
			urls = append(urls, url)
		}
		if opened.Valid && opened.Int64 > 0 {
			p.opened = opened.Int64
		} else {
			p.opened = min(p.opened, linked)
		}
		switch {
		case merged.Valid:
			p.merged = merged.Int64
		case mergedAt[url] > 0:
			p.merged = mergedAt[url]
		case state == "merged" && p.merged == 0:
			p.merged = -1
		}
		add := func(ag string) {
			if ag != "" && !slices.ContainsFunc(p.agents, func(x string) bool { return normalizeAgent(x) == normalizeAgent(ag) }) {
				p.agents = append(p.agents, ag)
			}
		}
		for _, ag := range []string{linker, named} {
			if runMatches(a.q.Agent, ag, "") {
				add(ag)
			}
		}
		for _, ag := range a.issueIDs[issueID] {
			add(ag)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, url := range urls {
		p := byURL[url]
		if len(p.agents) == 0 {
			continue
		}
		if p.opened >= a.from {
			a.totals.PRsOpened++
		}
		if p.merged >= a.from {
			a.totals.PRsMerged++
			for _, ag := range p.agents {
				x := a.agent(ag, "")
				x.PRsMerged++
				a.active(x, p.merged)
			}
		}
	}
	return nil
}

// reviewBots sums what each model did for the review bots in the period.
func (a *activityBuilder) reviewBots(ctx context.Context, actor *Actor, project string) error {
	where, args := visibleSQL("r.project_id", actor)
	if project != "" {
		where, args = where+" AND r.project_id = (SELECT id FROM projects WHERE key = ?)", append(args, project)
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := a.s.db.R.QueryContext(ctx, `SELECT r.id, r.status, r.log,
			(SELECT json_group_object(model, n) FROM (SELECT f.model, count(*) AS n FROM bot_findings f WHERE f.run_id = r.id AND f.state != 'dropped' GROUP BY f.model))
		FROM bot_runs r WHERE `+where+` AND r.kind = 'review' AND coalesce(r.finished_at, r.created_at) >= ?`, append(args, a.from)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byModel := map[string]*BotActivity{}
	for rows.Next() {
		var id int64
		var status, logJSON string
		var findings sql.NullString
		if err := rows.Scan(&id, &status, &logJSON, &findings); err != nil {
			return err
		}
		var log []ModelAttempt
		_ = json.Unmarshal([]byte(logJSON), &log)
		counts := map[string]int{}
		if findings.Valid {
			_ = json.Unmarshal([]byte(findings.String), &counts)
		}
		answered := map[string]bool{}
		matched := false
		for _, at := range log {
			if !runMatches(a.q.Agent, at.Model, "") {
				continue
			}
			matched = true
			b := byModel[at.Model]
			if b == nil {
				b = &BotActivity{Model: at.Model}
				byModel[at.Model] = b
				a.bots = append(a.bots, b)
			}
			b.Seconds += at.MS / 1000
			if at.Outcome == "ok" {
				if !answered[at.Model] {
					answered[at.Model] = true
					b.Reviews++
					b.Findings += counts[at.Model]
				}
			} else if at.Outcome != "cancelled" {
				b.Failed++
			}
		}
		if status == "done" && (a.q.Agent == "" || (matched && len(answered) > 0)) {
			a.totals.BotReviews++
		}
	}
	slices.SortStableFunc(a.bots, func(x, y *BotActivity) int { return y.Reviews - x.Reviews })
	return rows.Err()
}

func (a *activityBuilder) result() *Activity {
	out := &Activity{From: msTime(a.from), To: msTime(a.to), Totals: a.totals, Agents: []*AgentActivity{},
		Bots: a.bots, Runs: a.runList}
	if out.Bots == nil {
		out.Bots = []*BotActivity{}
	}
	if out.Runs == nil {
		out.Runs = []ActivityRun{}
	}
	for _, k := range a.order {
		x := a.agents[k]
		for _, sp := range mergeSpans(a.spans[k]) {
			x.Seconds += (sp[1] - sp[0]) / 1000
			a.splitDays(sp[0], sp[1])
		}
		out.Agents = append(out.Agents, x)
		out.Totals.Runs += x.Runs
		out.Totals.TasksDone += x.Done
		out.Totals.Seconds += x.Seconds
		out.Totals.InputTokens += x.InputTokens
		out.Totals.OutputTokens += x.OutputTokens
		out.Totals.CostUSD += x.CostUSD
		out.Totals.IssuesCreated += x.IssuesCreated
		out.Totals.Comments += x.Comments
	}
	slices.SortStableFunc(out.Agents, func(x, y *AgentActivity) int {
		if c := cmpInt(y.Seconds, x.Seconds); c != 0 {
			return c
		}
		return (y.Done + y.Runs) - (x.Done + x.Runs)
	})
	for d := a.first; d.UnixMilli() <= a.to; d = d.AddDate(0, 0, 1) {
		key := d.Format(dateLayout)
		day := ActivityDay{Date: key}
		if x := a.days[key]; x != nil {
			day = *x
		}
		out.Daily = append(out.Daily, day)
	}
	return out
}

// splitDays adds a span's time to the days it falls on.
func (a *activityBuilder) splitDays(lo, hi int64) {
	for lo < hi {
		t := time.UnixMilli(lo).In(a.loc)
		next := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, a.loc).AddDate(0, 0, 1).UnixMilli()
		end := min(hi, next)
		a.day(lo).Seconds += (end - lo) / 1000
		lo = end
	}
}

// mergeSpans joins overlapping spans, so time is counted once.
func mergeSpans(spans [][2]int64) [][2]int64 {
	if len(spans) == 0 {
		return nil
	}
	sorted := slices.Clone(spans)
	slices.SortFunc(sorted, func(x, y [2]int64) int { return cmpInt(x[0], y[0]) })
	out := [][2]int64{sorted[0]}
	for _, sp := range sorted[1:] {
		last := &out[len(out)-1]
		if sp[0] > last[1] {
			out = append(out, sp)
			continue
		}
		last[1] = max(last[1], sp[1])
	}
	return out
}

func cmpInt(x, y int64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}
