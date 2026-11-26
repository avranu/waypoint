package core

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
	"git.jmann.me/claude/waypoint/internal/transcript"
)

// What a run or a task had of a release.
const (
	FeatureHad     = "had"
	FeatureBefore  = "before"
	FeaturePartial = "partial"
	FeatureUnknown = "unknown"
)

// FeatureQuery says which release, and how many days either side of it.
type FeatureQuery struct {
	Ref    string // a task (WP-123) or a version (v0.80.0)
	Window int    // days on each side (default 30, at most 90)
	Repo   string
}

// UsageFeatureTask is one task's side of the comparison.
type UsageFeatureTask struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	FirstAt   time.Time `json:"first_at"`
	CostUSD   float64   `json:"cost_usd"`
	Tokens    int64     `json:"tokens"` // no cache reads
	ToolCalls int64     `json:"tool_calls"`
}

// UsageFeature compares tasks without a release (Before) and with it
// (After).
type UsageFeature struct {
	Ref       string             `json:"ref"`
	Title     string             `json:"title,omitempty"` // the task's
	Release   string             `json:"release"`
	LiveAt    time.Time          `json:"live_at"`
	Window    int                `json:"window"`     // days before the release
	DaysAfter int                `json:"days_after"` // days after it so far: at most Window
	Who       string             `json:"who"`
	Counts    map[string]int     `json:"counts"` // tasks per state
	Stats     []CompareStat      `json:"stats"`
	Tasks     []UsageFeatureTask `json:"tasks"`
	TaskRows  int                `json:"task_rows"` // tasks before the list was cut
	MinN      int                `json:"min_n"`
}

const usageFeatureTasks = 200

var featureVersionRE = lazyre.New(`^v?[0-9]+\.[0-9]+`)

// knownVersion: a version that can be ordered ("dev" and "" can't).
func knownVersion(v string) bool { return featureVersionRE.MatchString(v) }

// runFacts is what one run had.
type runFacts struct {
	issue     int64
	inSession bool
	values    map[string]string
}

// runVersions are the versions a run is judged by.
var runVersions = []string{featureServer, featureServerEnd, featureWP, featureWPEnd,
	featureSessionServer, featureSessionServerEnd, featureSessionWP, featureSessionWPEnd}

// state is what the run had of a release. A session keeps the hooks and
// instructions it started with, so its own versions count; without a
// session no hooks ran, so a release can only have been partly there. Had
// needs all of it to be known: a session from before recording, or a dev
// build, might have been on anything. (A claim from the web has no wp.)
func (r *runFacts) state(release string) string {
	if !knownVersion(r.values[featureServer]) {
		return FeatureUnknown
	}
	had, not, unsure := 0, 0, 0
	if r.inSession && (r.values[featureSessionServer] == "" || r.values[featureSessionWP] == "") {
		unsure++
	}
	for _, name := range runVersions {
		switch v := r.values[name]; {
		case v == "":
		case !knownVersion(v):
			unsure++
		case versionLess(v, release):
			not++
		default:
			had++
		}
	}
	switch {
	case had == 0:
		return FeatureBefore
	case not > 0 || !r.inSession:
		return FeaturePartial
	case unsure > 0:
		return FeatureUnknown
	}
	return FeatureHad
}

// taskFeatureState folds a task's runs into one state: runs that disagree
// make it partial.
func taskFeatureState(runs []*runFacts, release string) string {
	state := FeatureUnknown
	for n, r := range runs {
		st := r.state(release)
		if n > 0 && st != state {
			return FeaturePartial
		}
		state = st
	}
	return state
}

// runFactsIn is every run in the scope's period, by issue: those of the
// scope's people and repos, whose usage is what's being compared.
func (s *Service) runFactsIn(ctx context.Context, sc *usageScope) (map[int64][]*runFacts, error) {
	const span = `r.started_at < ? AND coalesce(r.ended_at, ?) >= ?`
	args := []any{sc.to + runLead, sc.to, sc.from - runGrace}
	where, scoped := span, append([]any(nil), args...)
	switch {
	case sc.everyone:
	case len(sc.uids) == 1:
		where, scoped = where+` AND coalesce(s.user_id, r.user_id) = ?`, append(scoped, sc.uids[0])
	default:
		where += ` AND 0`
	}
	if sc.repoFilter {
		where += ` AND (coalesce(s.repo, '') = ''`
		if len(sc.repos) > 0 {
			where += ` OR s.repo COLLATE NOCASE IN (` + placeholders(len(sc.repos)) + `)`
			for _, repo := range sc.repos {
				scoped = append(scoped, repo)
			}
		}
		where += `)`
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, r.issue_id, r.session_id IS NOT NULL
		FROM agent_runs r LEFT JOIN agent_sessions s ON s.id = r.session_id WHERE `+where+` ORDER BY r.started_at, r.id`, scoped...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byRun := map[int64]*runFacts{}
	out := map[int64][]*runFacts{}
	for rows.Next() {
		var id int64
		r := &runFacts{values: map[string]string{}}
		if err := rows.Scan(&id, &r.issue, &r.inSession); err != nil {
			return nil, err
		}
		byRun[id] = r
		out[r.issue] = append(out[r.issue], r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	frows, err := s.db.R.QueryContext(ctx, `SELECT f.run_id, f.feature, f.value FROM run_features f JOIN agent_runs r ON r.id = f.run_id WHERE `+span, args...)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	for frows.Next() {
		var id int64
		var name, value string
		if err := frows.Scan(&id, &name, &value); err != nil {
			return nil, err
		}
		if r := byRun[id]; r != nil {
			r.values[name] = value
		}
	}
	return out, frows.Err()
}

// thisServerEnv is the environment this build runs in: one a deployment of
// its version was recorded for, in the project asked about (0: any). Versions
// from any other environment are another product's, and say nothing about
// what agents here had. Nothing tells a server which project it is, so with
// several to choose from, only naming a task can settle it.
func (s *Service) thisServerEnv(ctx context.Context, project int64) (int64, error) {
	if !knownVersion(s.cfg.Version) {
		return 0, invalid("this server is a %q build, which has no release to compare against", cmp(s.cfg.Version, "dev"))
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, project_id FROM environments WHERE kind IN ('production', 'staging') AND (? = 0 OR project_id = ?)
		AND EXISTS (SELECT 1 FROM deployments d WHERE d.environment_id = environments.id AND d.version = ? AND d.status IN ('succeeded', 'detected'))
		ORDER BY `+writtenEnvOrder, project, project, s.cfg.Version)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var env, first int64
	for rows.Next() {
		var id, p int64
		if err := rows.Scan(&id, &p); err != nil {
			return 0, err
		}
		if env == 0 {
			env, first = id, p
		} else if p != first {
			return 0, invalid("several projects have deployed a version called %s, so this server's can't be told apart; name a task instead", s.cfg.Version)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if env == 0 {
		return 0, invalid("no environment has a recorded deployment of this server's version (%s)", s.cfg.Version)
	}
	return env, nil
}

// featureRelease finds the release a task or version names, and when it
// went live.
func (s *Service) featureRelease(ctx context.Context, actor *Actor, ref string) (*UsageFeature, int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, 0, invalid("feature: a task (WP-123) or a version (v0.80.0) is required")
	}
	o := &UsageFeature{Ref: ref}
	if knownVersion(ref) {
		env, err := s.thisServerEnv(ctx, 0)
		if err != nil {
			return nil, 0, err
		}
		var at int64
		err = s.db.R.QueryRowContext(ctx, `SELECT version, coalesce(finished_at, started_at) FROM deployments
			WHERE environment_id = ? AND version IN (?, ?) AND status IN ('succeeded', 'detected') ORDER BY 2, id LIMIT 1`, env, ref, "v"+ref).Scan(&o.Release, &at)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, invalid("%s was never deployed here", ref)
		}
		return o, at, err
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false)
	if err != nil {
		return nil, 0, err
	}
	o.Ref, o.Title = i.Key, i.Title
	env, err := s.thisServerEnv(ctx, i.ProjectID)
	if err != nil {
		if IsInvalid(err) && knownVersion(s.cfg.Version) {
			return nil, 0, invalid("%s isn't in the project this server is deployed from, so no release of this server contains it", i.Key)
		}
		return nil, 0, err
	}
	var merged sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT min(p.merged_at) FROM external_links el
		JOIN pull_requests p ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
		WHERE el.issue_id = ? AND p.state = 'merged'`, i.ID).Scan(&merged); err != nil {
		return nil, 0, err
	}
	if !merged.Valid {
		return nil, 0, invalid("%s has no merged pull request, so no release contains it", i.Key)
	}
	var at int64
	err = s.db.R.QueryRowContext(ctx, `SELECT version, coalesce(finished_at, started_at) FROM deployments
		WHERE environment_id = ? AND status IN ('succeeded', 'detected') AND version != '' AND started_at >= ?
		ORDER BY 2, id LIMIT 1`, env, merged.Int64).Scan(&o.Release, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, invalid("%s is merged but not deployed yet, so no run can have had it", i.Key)
	}
	return o, at, err
}

// UsageFeature compares what tasks used with and without a release.
func (s *Service) UsageFeature(ctx context.Context, actor *Actor, uq UsageQuery, q FeatureQuery) (*UsageFeature, error) {
	o, at, err := s.featureRelease(ctx, actor, q.Ref)
	if err != nil {
		return nil, err
	}
	if q.Window <= 0 {
		q.Window = 30
	}
	q.Window = min(q.Window, 90)
	uq.Days = 1
	sc, _, _, label, err := s.usageScopeFor(ctx, actor, &uq)
	if err != nil {
		return nil, err
	}
	span := int64(q.Window) * 24 * hourMillis
	sc.from, sc.to = at-at%hourMillis-span, min(at+span, sc.to)
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
	facts, err := s.runFactsIn(ctx, sc)
	if err != nil {
		return nil, err
	}
	units := map[int64]*compareUnit{}
	for k, b := range buckets {
		sk := sessKeyAt{k.machine, k.session}
		id := attr.taskOf(sk, k.hour)
		if id == 0 {
			continue
		}
		u := units[id]
		if u == nil {
			u = newCompareUnit()
			units[id] = u
		}
		u.add(sk, k.hour, b)
	}
	o.LiveAt, o.Window, o.Who, o.MinN = msTime(at), q.Window, label, compareMinN
	o.DaysAfter = int((max(sc.to-at, 0) + 24*hourMillis - 1) / (24 * hourMillis))
	o.Counts = map[string]int{FeatureHad: 0, FeatureBefore: 0, FeaturePartial: 0, FeatureUnknown: 0}
	o.Tasks = []UsageFeatureTask{}
	sides := [2]map[string]*compareUnit{{}, {}}
	for id, u := range units {
		info, ok := attr.tasks[id]
		if !ok {
			continue
		}
		state := taskFeatureState(facts[id], o.Release)
		o.Counts[state]++
		switch state {
		case FeatureBefore:
			sides[0][itoa64(id)] = u
		case FeatureHad:
			sides[1][itoa64(id)] = u
		}
		first := int64(0)
		for h := range u.hours {
			if first == 0 || h < first {
				first = h
			}
		}
		o.Tasks = append(o.Tasks, UsageFeatureTask{Key: info.key, Title: info.title, State: state, FirstAt: msTime(first), CostUSD: u.acc.cost,
			Tokens: u.acc.in + u.acc.out + u.acc.cw5 + u.acc.cw1, ToolCalls: u.tools})
	}
	o.Stats = compareStats(sides, []compareMeasure{cmCost, cmTokens, cmTools, cmSubagents, cmSessions, cmContext, cmHours})
	slices.SortFunc(o.Tasks, func(a, b UsageFeatureTask) int {
		if c := a.FirstAt.Compare(b.FirstAt); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	o.TaskRows = len(o.Tasks)
	o.Tasks = o.Tasks[:min(len(o.Tasks), usageFeatureTasks)]
	return o, nil
}

// TaskUsageRun is one run on a task and what it had.
type TaskUsageRun struct {
	Agent    string            `json:"agent"`
	Model    string            `json:"model,omitempty"`
	Outcome  string            `json:"outcome,omitempty"`
	Started  time.Time         `json:"started_at"`
	Ended    *time.Time        `json:"ended_at,omitempty"`
	Features map[string]string `json:"features"`
}

// TaskUsageTool is one tool, skill or command a task used.
type TaskUsageTool struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Calls  int64  `json:"calls"`
	Errors int64  `json:"errors"`
}

// TaskUsage is what one task used and what its agents had.
type TaskUsage struct {
	Key         string            `json:"key"`
	Title       string            `json:"title"`
	Who         string            `json:"who"`
	Runs        []TaskUsageRun    `json:"runs"`
	Arms        map[string]string `json:"arms"` // experiment → arm
	RunMinutes  int64             `json:"run_minutes"`
	ActiveHours int               `json:"active_hours"`
	Sessions    int               `json:"sessions"`
	Subagents   int64             `json:"subagents"`
	ToolCalls   int64             `json:"tool_calls"`
	ToolErrors  int64             `json:"tool_errors"`
	Edits       int64             `json:"edits"` // calls of the editing tools that did not fail
	// ShellEdits counts Bash calls that wrote the work (redirects, sed -i,
	// formatters, scripts that write). It is only known for the calls the
	// client read (uploaded as shell_edit or shell_none);
	// ShellEditsUncounted is the other Bash calls (not read whole, or
	// synced by a client too old to read them), so ShellEdits is then a floor.
	ShellEdits          int64 `json:"shell_edits"`
	ShellEditsUncounted int64 `json:"shell_edits_uncounted"`
	// ShellEditsCounted is the Bash calls the client read; with it above 0
	// a zero ShellEdits is "0+".
	ShellEditsCounted int64 `json:"shell_edits_counted"`
	// ScriptEdits and ScriptShellEdits are the edit-tool calls and writing
	// Bash calls a REPL script ran (model <synthetic>), not the model: they
	// are in none of the figures above, as in session facts.
	ScriptEdits      int64 `json:"script_edits"`
	ScriptShellEdits int64 `json:"script_shell_edits"`
	// ScriptShellUncounted is the Bash calls REPL scripts ran that the client
	// did not read (too long, or an older client); not in ShellEditsUncounted.
	ScriptShellUncounted int64           `json:"script_shell_uncounted"`
	PRs                  int64           `json:"prs"`   // merged
	Files                int64           `json:"files"` // changed in them
	Lines                int64           `json:"lines"` // added + deleted in them
	Used                 []TaskUsageTool `json:"used"`
	// AfterRun is spend in the task's sessions after its runs ended that no
	// other task claimed (WP-1884); nil without any. Never part of CostUSD.
	AfterRun *TaskCarryOver `json:"after_run,omitempty"`
	// Review rounds, the reviewers' tokens and the pull requests (WP-1882);
	// each absent when the task has none.
	Rounds       []TaskUsageRound   `json:"rounds,omitempty"`
	Reviewer     *TaskUsageReviewer `json:"reviewer,omitempty"`
	PullRequests []TaskUsagePR      `json:"pull_requests,omitempty"`
	UsageTotals
}

const taskUsageTools = 40

// taskRuns reads a task's runs, oldest first, and the period they cover.
func (s *Service) taskRuns(ctx context.Context, issueID int64) (runs []TaskUsageRun, from, to, minutes int64, err error) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, r.agent, r.model, r.outcome, r.started_at, r.ended_at, r.session_id IS NOT NULL,
			coalesce(s.cli, ''), coalesce(u.version, '')
		FROM agent_runs r LEFT JOIN agent_sessions s ON s.id = r.session_id
		LEFT JOIN usage_sessions u ON u.user_id = s.user_id AND u.machine = s.machine AND u.session = s.cli_id
		WHERE r.issue_id = ? ORDER BY r.started_at, r.id`, issueID)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	defer rows.Close()
	runs = []TaskUsageRun{}
	var ids []int64
	for rows.Next() {
		var id, started int64
		var ended sql.NullInt64
		var inSession bool
		var cli, cliVersion string
		var r TaskUsageRun
		if err := rows.Scan(&id, &r.Agent, &r.Model, &r.Outcome, &started, &ended, &inSession, &cli, &cliVersion); err != nil {
			return nil, 0, 0, 0, err
		}
		r.Started = msTime(started)
		end := now
		if ended.Valid {
			t := msTime(ended.Int64)
			r.Ended, end = &t, ended.Int64
		}
		r.Features = map[string]string{"hooks": "off"}
		if inSession {
			r.Features["hooks"] = "on"
		}
		for name, v := range map[string]string{"cli": cli, featureCLIVersion: cliVersion} {
			if v != "" {
				r.Features[name] = v
			}
		}
		if from == 0 || started < from {
			from = started
		}
		to = max(to, end)
		minutes += (end - started) / 60000
		runs, ids = append(runs, r), append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, 0, err
	}
	for n, id := range ids {
		frows, err := s.db.R.QueryContext(ctx, `SELECT feature, value FROM run_features WHERE run_id = ?`, id)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		for frows.Next() {
			var name, value string
			if err := frows.Scan(&name, &value); err != nil {
				frows.Close()
				return nil, 0, 0, 0, err
			}
			runs[n].Features[name] = value
		}
		frows.Close()
		if err := frows.Err(); err != nil {
			return nil, 0, 0, 0, err
		}
	}
	return runs, from, to, minutes, nil
}

// UsageForTask is what one task used and what its agents had.
func (s *Service) UsageForTask(ctx context.Context, actor *Actor, uq UsageQuery, key string) (*TaskUsage, error) {
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, key, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	uq.Days = 1
	sc, _, _, label, err := s.usageScopeFor(ctx, actor, &uq)
	if err != nil {
		return nil, err
	}
	o := &TaskUsage{Key: i.Key, Title: i.Title, Who: label, Arms: map[string]string{}, Used: []TaskUsageTool{}}
	var from, to int64
	if o.Runs, from, to, o.RunMinutes, err = s.taskRuns(ctx, i.ID); err != nil {
		return nil, err
	}
	if actor.canReadExperiments() == nil {
		if err := s.taskArms(ctx, i.ID, o); err != nil {
			return nil, err
		}
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(p.changed_files), 0), coalesce(sum(coalesce(p.additions, 0) + coalesce(p.deletions, 0)), 0)
		FROM pull_requests p WHERE p.state = 'merged' AND p.id IN (SELECT q.id FROM external_links el
			JOIN pull_requests q ON el.kind = 'pull' AND (el.url = q.url OR (el.repo = q.repo COLLATE NOCASE AND el.number = q.number)) WHERE el.issue_id = ?)`,
		i.ID).Scan(&o.PRs, &o.Files, &o.Lines); err != nil {
		return nil, err
	}
	if err := s.taskReviewInto(ctx, i.ID, o); err != nil {
		return nil, err
	}
	if len(o.Runs) == 0 {
		return o, nil
	}
	first := from - runLead
	// The window runs to now, not to the last run's end, to see the hours
	// after it that carry over (carryOf).
	sc.from = first - first%hourMillis
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
	// A session that holds the task with no run of its own (another session
	// took the run) is the task's while the task's runs last, not for as long
	// as it lives: what it spends after the last run ended is not this cost.
	for k, spans := range attr.runs {
		if spans[0].start == 0 && spans[0].issue == i.ID {
			attr.runs[k] = []runSpan{{issue: i.ID, start: 1, end: to}}
		}
	}
	if err := attr.loadCarry(ctx, s, actor, sc); err != nil {
		return nil, err
	}
	u := newCompareUnit()
	carry, carrySessions := 0.0, map[sessKeyAt]bool{}
	for k, b := range buckets {
		sk := sessKeyAt{k.machine, k.session}
		if attr.taskOf(sk, k.hour) == i.ID {
			u.add(sk, k.hour, b)
		} else if b.acc.cost > 0 && attr.carryOf(sk, k.hour) == i.ID {
			carry += b.acc.cost
			carrySessions[sk] = true
		}
	}
	if carry > 0 {
		o.AfterRun = &TaskCarryOver{CostUSD: carry, Sessions: len(carrySessions)}
	}
	o.UsageTotals, o.ActiveHours, o.Sessions = u.acc.totals(), len(u.hours), len(u.sessions)
	o.Subagents, o.ToolCalls, o.ToolErrors = u.subagents, u.tools, u.errors
	return o, s.taskToolsInto(ctx, sc, attr, i.ID, o)
}

// taskArms fills the experiment arms a task was in; the newest run of an
// experiment wins.
func (s *Service) taskArms(ctx context.Context, issueID int64, o *TaskUsage) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.experiment, u.arm FROM experiment_units u JOIN experiment_runs r ON r.id = u.run_id WHERE u.issue_id = ? ORDER BY r.id`, issueID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, arm string
		if err := rows.Scan(&name, &arm); err != nil {
			return err
		}
		o.Arms[name] = arm
	}
	return rows.Err()
}

// taskToolsInto fills what the task's hours used, most-called first.
func (s *Service) taskToolsInto(ctx context.Context, sc *usageScope, attr *taskAttribution, issueID int64, o *TaskUsage) error {
	where, args := sc.where("hour")
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT machine, session, hour, kind, tool, detail, via, model LIKE '<%', sum(calls), sum(errors), sum(refusals) FROM tool_hours WHERE `+where+`
		GROUP BY machine, session, hour, kind, tool, detail, via, model LIKE '<%'`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type toolKey struct{ kind, name string }
	seen := map[toolKey]*TaskUsageTool{}
	bash := map[trendKey]int64{}
	scriptBash, scriptRead := map[trendKey]int64{}, map[trendKey]int64{} // the same, of REPL scripts' calls
	read := map[trendKey]int64{}                                         // Bash calls the client read whole
	for rows.Next() {
		var k trendKey
		var kind, tool, detail, via string
		var calls, errs, refused int64
		var byScript bool // a REPL script ran these calls (model <synthetic>)
		if err := rows.Scan(&k.machine, &k.session, &k.hour, &kind, &tool, &detail, &via, &byScript, &calls, &errs, &refused); err != nil {
			return err
		}
		// A transcript row's refusal is one of its calls (usage_parse.go); an
		// OTLP row counts a rejected tool_decision apart from the tool_result
		// events (usage_otlp.go), so there the calls hold no refusal.
		inCalls := refused
		if via == "otlp" {
			inCalls = 0
		}
		if attr.taskOf(sessKeyAt{k.machine, k.session}, k.hour) != issueID {
			continue
		}
		if kind == "builtin" && transcript.IsEditTool(tool) {
			// A call is an error or a refusal, never both (usage_parse.go), and
			// neither wrote anything. That a refusal is inside calls holds for
			// transcript rows; OTLP rows keep refusals out of calls (inCalls).
			if byScript {
				o.ScriptEdits += max(calls-errs-inCalls, 0)
			} else {
				o.Edits += max(calls-errs-inCalls, 0)
			}
		}
		if kind == "builtin" && tool == "Bash" {
			if byScript {
				scriptBash[k] += calls
			} else {
				bash[k] += calls
			}
		}
		switch kind {
		case transcript.ShellUnknownKind:
			// One of the hour's Bash calls (builtin Bash has it too) that the
			// client could not read: uncounted, by not being among the read.
			continue
		case transcript.ShellNoneKind:
			if byScript {
				scriptRead[k] += calls
			} else {
				read[k] += calls
			}
			continue
		case transcript.ShellEditKind:
			// A non-zero exit still counts (sed -i ... && go test writes before
			// the test fails); a refused call wrote nothing.
			if byScript {
				scriptRead[k] += calls
				o.ScriptShellEdits += max(calls-inCalls, 0)
			} else {
				read[k] += calls
				o.ShellEdits += max(calls-inCalls, 0)
			}
			continue
		}
		name := strings.TrimSpace(tool + " " + detail)
		t := seen[toolKey{kind, name}]
		if t == nil {
			t = &TaskUsageTool{Kind: kind, Name: name}
			seen[toolKey{kind, name}] = t
		}
		t.Calls += calls
		t.Errors += errs
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for k, n := range bash {
		// Every call the client read has a shell_edit or shell_none row; the
		// rest of the hour's Bash calls were not read (shell_unknown) or came
		// from a client that did not read them, and are uncounted.
		r := min(read[k], n)
		o.ShellEditsCounted += r
		o.ShellEditsUncounted += n - r
	}
	for k, n := range scriptBash {
		o.ScriptShellUncounted += n - min(scriptRead[k], n)
	}
	for _, t := range seen {
		o.Used = append(o.Used, *t)
	}
	slices.SortFunc(o.Used, func(a, b TaskUsageTool) int {
		if a.Calls != b.Calls {
			return int(b.Calls - a.Calls)
		}
		return strings.Compare(a.Kind+a.Name, b.Kind+b.Name)
	})
	o.Used = o.Used[:min(len(o.Used), taskUsageTools)]
	return nil
}
