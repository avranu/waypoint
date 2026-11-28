package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	goalArmGoal   = "goal"
	goalArmChain  = "chain"
	goalArmPruned = "pruned"

	// goalCtxSteps: how many parents up the walk goes; a cycle or a runaway
	// hierarchy ends there (WP's own is at most 5 deep).
	goalCtxSteps = 8
	// goalCtxClip: a title longer than this many runes is clipped, at a
	// word, in the pruned arm.
	goalCtxClip = 160
	// goalCtxBudget: the pruned arm stops showing parents once the titles
	// shown total this many runes; the rest are counted as left out (Above).
	goalCtxBudget = 360

	// goalCtxPolicyAge: the estimate is worked out again when older.
	goalCtxPolicyAge = time.Hour
	// goalCtxPolicyRetry: after a failed compute the last estimate (or the
	// error) stands this long; goalCtxPolicyTimeout bounds one compute.
	goalCtxPolicyRetry   = 10 * time.Minute
	goalCtxPolicyTimeout = 2 * time.Minute
	// goalCtxClaimFresh: a task is drawn an arm only this soon after its
	// claim began, so the arm is the take's, not told mid-work.
	goalCtxClaimFresh = 10 * time.Minute
	// goalCtxWithin: a task not done this long after its draw counts as
	// undone; follow-up fixes are counted this long after it was done.
	goalCtxWithin = 14 * 24 * time.Hour
)

// goalArmDefs are the arms, the control first. A changed rule is a new key,
// tested as new; a key no longer here stays in the history and the
// estimate, and is never drawn.
var goalArmDefs = []struct{ Key, Label string }{
	{goalArmGoal, "the goal only"},
	{goalArmChain, "every parent goal, as written"},
	{goalArmPruned, "parents the task and goal don't already convey, as judged by the goals model, clipped, within a length"},
}

// goalData says a row's arm was drawn at random with its propensity kept:
// from the estimate, or from the no-data prior when none could be had.
func goalData(policy string) bool { return policy == "thompson" || policy == "prior" }

func goalArmKnown(key string) bool {
	return slices.ContainsFunc(goalArmDefs, func(d struct{ Key, Label string }) bool { return d.Key == key })
}

func goalArmLabel(key string) string {
	for _, d := range goalArmDefs {
		if d.Key == key {
			return d.Label
		}
	}
	return key + " (no longer drawn)"
}

// GoalStep is a parent goal shown under a task's goal.
type GoalStep struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Clipped bool   `json:"clipped,omitempty"` // the title was cut at a word, "…" ends it
}

// goalParents are the goal's parents the actor may be shown, nearest
// first: approved goals they can read. Others are skipped and the walk goes
// on past them, up to goalCtxSteps parents, and it stops at a cycle.
func (s *Service) goalParents(ctx context.Context, q queryer, actor *Actor, goalID int64) []GoalStep {
	var next sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT parent_id FROM design_notes WHERE id = ?`, goalID).Scan(&next); err != nil {
		return nil
	}
	seen := map[int64]bool{goalID: true}
	var out []GoalStep
	for range goalCtxSteps {
		if !next.Valid || seen[next.Int64] {
			break
		}
		id := next.Int64
		seen[id] = true
		var approved bool
		next = sql.NullInt64{}
		if err := q.QueryRowContext(ctx, `SELECT parent_id, kind = 'goal' AND status = 'active' AND (approved_at IS NOT NULL OR requested_by IS NOT NULL)
			FROM design_notes WHERE id = ?`, id).Scan(&next, &approved); err != nil {
			break
		}
		if !approved {
			continue
		}
		if it, err := s.kitem(ctx, actor, KDesign, id, ""); err == nil {
			out = append(out, GoalStep{ID: id, Title: it.Title})
		}
	}
	return out
}

// goalClip cuts a title longer than goalCtxClip runes at a word and ends it
// with "…"; the words kept are the title's own.
func goalClip(title string) (string, bool) {
	if utf8.RuneCountInString(title) <= goalCtxClip {
		return title, false
	}
	r := []rune(title)
	end := -1
	if unicode.IsSpace(r[goalCtxClip]) {
		end = goalCtxClip
	} else {
		for i := goalCtxClip - 1; i > 0; i-- {
			if unicode.IsSpace(r[i]) {
				end = i
				break
			}
		}
	}
	if end <= 0 {
		return title, false
	}
	cut := r[:end]
	return strings.TrimRight(string(cut), " ,;:.-") + "…", true
}

// goalChain is the parents the arm shows and how many it leaves out.
// covered are the parents the goals model judged the task and its goal
// already convey (nil: no current judgement, so none are left out as
// covered); the pruned arm leaves exactly those out (WP-ADR133).
func goalChain(arm string, parents []GoalStep, covered map[int64]bool) ([]GoalStep, int) {
	switch arm {
	case goalArmChain:
		return slices.Clone(parents), 0
	case goalArmPruned:
		var out []GoalStep
		above, shown := 0, 0
		for _, p := range parents {
			if shown >= goalCtxBudget || covered[p.ID] {
				above++
				continue
			}
			p.Title, p.Clipped = goalClip(p.Title)
			out = append(out, p)
			shown += utf8.RuneCountInString(p.Title)
		}
		return out, above
	}
	return nil, 0
}

// parseCovered reads a JSON array of goal ids.
func parseCovered(raw string) (map[int64]bool, bool) {
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, false
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, true
}

// coveredJSON is the ids in order, as JSON.
func coveredJSON(covered map[int64]bool) string {
	ids := make([]int64, 0, len(covered))
	for id := range covered {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	raw, _ := json.Marshal(ids)
	return string(raw)
}

// goalCoveredShown is the covered set a read of the task uses: the one
// recorded when it was drawn (NULL: none), else the current judgement.
func (s *Service) goalCoveredShown(ctx context.Context, q queryer, d *IssueDetail) map[int64]bool {
	var raw sql.NullString
	err := q.QueryRowContext(ctx, `SELECT covered FROM goal_context_assignments WHERE issue_id = ?`, d.ID).Scan(&raw)
	switch {
	case err == nil:
		if !raw.Valid {
			return nil
		}
		m, _ := parseCovered(raw.String)
		return m
	case errors.Is(err, sql.ErrNoRows):
		m, _ := s.goalCoveredCurrent(ctx, q, d)
		return m
	}
	return nil
}

// goalContextShow fills a task's goal trace with the arm's chain.
func (s *Service) goalContextShow(ctx context.Context, q queryer, actor *Actor, d *IssueDetail, arm string) {
	g := d.Goal
	if g == nil || g.Goal == nil || arm == goalArmGoal || arm == "" {
		return
	}
	parents := s.goalParents(ctx, q, actor, g.Goal.ID)
	g.Chain, g.Above = goalChain(arm, parents, s.goalCoveredShown(ctx, q, d))
	if len(g.Chain) > 0 || g.Above > 0 {
		g.Arm = arm
	} else {
		g.Chain = nil
	}
}

// goalContextRead is what a read of the task shows (never a draw): its
// assignment's arm, else the arm the cached estimate favours, else goal.
func (s *Service) goalContextRead(ctx context.Context, q queryer, actor *Actor, d *IssueDetail) {
	if d.Goal == nil || d.Goal.Goal == nil {
		return
	}
	arm := goalArmGoal
	var got string
	err := q.QueryRowContext(ctx, `SELECT arm FROM goal_context_assignments WHERE issue_id = ?`, d.ID).Scan(&got)
	switch {
	case err == nil:
		arm = got
	case errors.Is(err, sql.ErrNoRows):
		if pol := s.goalPol.peek(); pol != nil && actor.ActsAsPerson() {
			arm = pol.best()
		}
	}
	s.goalContextShow(ctx, q, actor, d, arm)
}

// goalDraw is a task's place in [0, 1) for this policy: fixed for the
// task, apart from the other policies', and nothing an agent can steer.
func goalDraw(issueID int64) float64 {
	h := sha256.Sum256(fmt.Appendf(nil, "goal-context-policy/%d", issueID))
	return float64(binary.BigEndian.Uint64(h[:8])>>11) / (1 << 53)
}

// goalContextDraw draws the task's arm, the first time an agent holding a
// fresh claim gets its text, and keeps it. "" when there is nothing to
// choose: another holds the task, the claim is old, or no arm would show a
// parent. It writes, so it is never called inside a write transaction.
func (s *Service) goalContextDraw(ctx context.Context, actor *Actor, d *IssueDetail, via string) (string, error) {
	i := d.Issue
	if !actor.IsAgent() || actor.ReadOnly || d.Goal == nil || d.Goal.Goal == nil || i.ClaimedBy == "" || i.ClaimedBy != actor.AgentName() {
		return "", nil
	}
	var arm string
	err := s.db.R.QueryRowContext(ctx, `SELECT arm FROM goal_context_assignments WHERE issue_id = ?`, i.ID).Scan(&arm)
	if err == nil {
		return arm, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if via == "" || i.ClaimedAt == nil || s.now().Sub(*i.ClaimedAt) > goalCtxClaimFresh {
		return "", nil
	}
	var earlier bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE issue_id = ?1 AND started_at < ?2)
		OR EXISTS (SELECT 1 FROM work_log WHERE issue_id = ?1 AND created_at < ?2)`, i.ID, i.ClaimedAt.UnixMilli()).Scan(&earlier); err != nil {
		return "", err
	}
	if earlier {
		return "", nil
	}
	parents := s.goalParents(ctx, s.db.R, actor, d.Goal.Goal.ID)
	covered, judged := s.goalCoveredCurrent(ctx, s.db.R, d)
	full, _ := goalChain(goalArmChain, parents, nil)
	if len(full) == 0 {
		return "", nil
	}
	names := []string{goalArmGoal, goalArmChain}
	if cut, above := goalChain(goalArmPruned, parents, covered); above > 0 || !samePlain(cut, full) {
		names = append(names, goalArmPruned)
	}
	policy := "thompson"
	pol, err := s.goalPolNow(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "goal context policy: drawing from the prior", "issue", i.ID, "err", err)
		pol, policy = goalPolicyFrom(nil, s.now()), "prior"
	}
	sh := prepShares(pol.pBest(names), nil)
	shares := map[string]float64{}
	for n, name := range names {
		shares[name] = math.Round(sh[n]*1e4) / 1e4
	}
	at := prepPick(sh, goalDraw(i.ID))
	raw, _ := json.Marshal(shares)
	var recorded sql.NullString
	if judged {
		recorded = sql.NullString{String: coveredJSON(covered), Valid: true}
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO goal_context_assignments(issue_id, arm, policy, propensity, shares, candidates, chain_len, text_len, via, covered, assigned_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(issue_id) DO NOTHING`,
		i.ID, names[at], policy, sh[at], string(raw), strings.Join(names, ","), len(full), prepTextLen(i), via, recorded, s.nowMS()); err != nil {
		return "", err
	}
	err = s.db.W.QueryRowContext(ctx, `SELECT arm FROM goal_context_assignments WHERE issue_id = ?`, i.ID).Scan(&arm)
	return arm, err
}

// samePlain says the clipped chain is the whole one: the same parents,
// none clipped.
func samePlain(cut, full []GoalStep) bool {
	if len(cut) != len(full) {
		return false
	}
	for i := range cut {
		if cut[i].Clipped || cut[i].ID != full[i].ID {
			return false
		}
	}
	return true
}

// goalContextFor shows the task's goal as its drawn arm says, for an agent
// that takes it. A failed draw logs and leaves the goal alone.
func (s *Service) goalContextFor(ctx context.Context, actor *Actor, d *IssueDetail, via string) {
	arm, err := s.goalContextDraw(ctx, actor, d, via)
	if err != nil {
		s.log.WarnContext(ctx, "goal context policy: draw the task's arm", "issue", d.ID, "err", err)
		return
	}
	if arm != "" {
		d.Goal.Chain, d.Goal.Above, d.Goal.Arm = nil, 0, ""
		s.goalContextShow(ctx, s.db.R, actor, d, arm)
	}
}

// The primary outcome is the count of drift flags on the task's pull
// requests that no person answered "this is fine", on the log scale. Its
// spread is a prior, not measured: 0.4 is a guess for flags counted per
// task, until the first tasks' outcomes are in. Guards are the delegation
// policy's, less its cost-based ones.
var (
	goalPrimary = prepOutcome{Key: "drift", Label: "drift flags on the task's pull requests no person called fine", Log: true, Spread: 0.4}
	goalGuards  = []prepOutcome{
		{Key: "undone", Label: "tasks not done in 14 days", Spread: 0.45, Margin: 0.05},
		{Key: "findings", Label: "review findings on the task's pull requests", Log: true, Spread: 0.9, Margin: 0.10},
		{Key: "fixes", Label: "follow-up fix tasks filed against it", Log: true, Spread: 0.5, Margin: 0.10},
	}
)

// goalRow is one task given an arm, and what came of it.
type goalRow struct {
	issue    int64
	arm      string
	policy   string
	chainLen int
	textLen  int
	assigned int64
	project  int64
	typ      string
	resolved int64
	done     bool
	prs      int
	judged   bool
	checked  int
	errored  int
	drift    float64
	findings float64
	fixes    float64
}

type goalFit struct {
	prepOutcome
	fit   *prepMix
	tasks int
}

type goalPolicy struct {
	at       time.Time
	arms     []string
	primary  *prepMix
	guards   []goalFit
	blocked  map[string]string
	rows     []*goalRow
	measured int
	byArm    map[string]int
}

// goalCtxCache holds the estimate, worked out again hourly; one compute
// runs at a time, under its own deadline, and a failed one is not retried
// for goalCtxPolicyRetry.
type goalCtxCache struct {
	mu    sync.Mutex
	comp  sync.Mutex
	pol   *goalPolicy
	busy  bool
	retry time.Time
	err   error
}

// peek is the estimate as cached, however old; nil when none yet. Reads
// never compute one.
func (c *goalCtxCache) peek() *goalPolicy {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pol
}

func (s *Service) goalPolCached(claim bool) (*goalPolicy, bool, error) {
	c := &s.goalPol
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	switch {
	case c.pol != nil && now.Sub(c.pol.at) < goalCtxPolicyAge && !now.Before(c.pol.at):
		return c.pol, true, nil
	case now.Before(c.retry):
		if c.pol != nil {
			return c.pol, true, nil
		}
		return nil, true, c.err
	case c.pol != nil && c.busy:
		return c.pol, true, nil
	}
	if claim {
		c.busy = true
	}
	return nil, false, nil
}

func (s *Service) goalPolNow(ctx context.Context) (*goalPolicy, error) {
	if pol, ok, err := s.goalPolCached(false); ok {
		return pol, err
	}
	c := &s.goalPol
	c.comp.Lock()
	defer c.comp.Unlock()
	if pol, ok, err := s.goalPolCached(true); ok {
		return pol, err
	}
	defer func() {
		c.mu.Lock()
		c.busy = false
		c.mu.Unlock()
	}()
	now := s.now()
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), goalCtxPolicyTimeout)
	defer cancel()
	pol, err := s.goalPolCompute(cctx, now)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.busy = false
	if err != nil {
		c.retry, c.err = now.Add(goalCtxPolicyRetry), err
		if c.pol != nil {
			s.log.WarnContext(ctx, "goal context policy: the last estimate stands", "err", err)
			return c.pol, nil
		}
		return nil, err
	}
	c.pol, c.retry, c.err = pol, time.Time{}, nil
	return pol, nil
}

const goalIn = `(SELECT issue_id FROM goal_context_assignments)`

// goalPRs is SQL for the distinct (task, pull request) pairs of the
// assigned tasks, linked as delegRows joins them.
const goalPRs = `SELECT DISTINCT el.issue_id, p.id AS pid
	FROM external_links el JOIN pull_requests p ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
	WHERE el.issue_id IN ` + goalIn

func (s *Service) goalRows(ctx context.Context) ([]*goalRow, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.issue_id, a.arm, a.policy, a.chain_len, a.text_len, a.assigned_at, a.covered IS NOT NULL,
			i.project_id, i.type, coalesce(i.resolved_at, 0), coalesce(st.category = 'done', 0)
		FROM goal_context_assignments a JOIN issues i ON i.id = a.issue_id JOIN statuses st ON st.id = i.status_id
		ORDER BY a.assigned_at, a.issue_id`)
	if err != nil {
		return nil, err
	}
	var out []*goalRow
	by := map[int64]*goalRow{}
	for rows.Next() {
		r := &goalRow{}
		if err := rows.Scan(&r.issue, &r.arm, &r.policy, &r.chainLen, &r.textLen, &r.assigned, &r.judged, &r.project, &r.typ, &r.resolved, &r.done); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		by[r.issue] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	rows, err = s.db.R.QueryContext(ctx, `SELECT x.issue_id, coalesce(c.verdict, ''), coalesce(f.answer, '')
		FROM (`+goalPRs+`) x LEFT JOIN pr_goal_checks c ON c.pr_id = x.pid LEFT JOIN goal_drift_flags f ON f.id = c.flag_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var verdict, answer string
		if err := rows.Scan(&id, &verdict, &answer); err != nil {
			rows.Close()
			return nil, err
		}
		r := by[id]
		if r == nil {
			continue
		}
		r.prs++
		if verdict == "clear" || verdict == "flag" {
			r.checked++
		}
		if verdict == "error" {
			r.errored++
		}
		if verdict == "flag" && answer != "intent" {
			r.drift++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.delegCount(ctx, `SELECT x.issue_id, 'f', count(f.id) FROM (`+goalPRs+`) x
		JOIN bot_findings f ON f.pr_id = x.pid AND f.state != 'withdrawn' GROUP BY x.issue_id`, func(id int64, _ string, n int) {
		if r := by[id]; r != nil {
			r.findings += float64(n)
		}
	}); err != nil {
		return nil, err
	}
	err = s.delegCount(ctx, `SELECT target_id, 'fix', count(*) FROM issue_relations
		WHERE kind = 'follows' AND subkind = 'fix' AND target_id IN `+goalIn+` GROUP BY target_id`, func(id int64, _ string, n int) {
		if r := by[id]; r != nil {
			r.fixes += float64(n)
		}
	})
	return out, err
}

// goalValue is a row's value of an outcome, when it has one yet.
func goalValue(o *prepOutcome, r *goalRow, now time.Time) (float64, bool) {
	age := now.UnixMilli() - r.assigned
	since := now.UnixMilli() - r.resolved
	switch o.Key {
	case "drift":
		return math.Log1p(r.drift), r.done && r.prs > 0 && r.checked == r.prs
	case "undone":
		if r.done && r.resolved > 0 && r.resolved-r.assigned <= goalCtxWithin.Milliseconds() {
			return 0, true
		}
		return 1, age > goalCtxWithin.Milliseconds()
	case "findings":
		return math.Log1p(r.findings), r.done && r.prs > 0
	case "fixes":
		return math.Log1p(r.fixes), r.done && r.resolved > 0 && since > goalCtxWithin.Milliseconds()
	}
	return 0, false
}

// goalObservations turns the drawn rows that have the outcome into
// observations, with what was known before the draw as covariates
// (project, type, the text's length, the parents under chain, when).
func goalObservations(o *prepOutcome, arms []string, rows []*goalRow, now time.Time) ([]prepObs, []*goalRow) {
	var use []*goalRow
	var ys []float64
	for _, r := range rows {
		if !goalData(r.policy) {
			continue
		}
		if y, ok := goalValue(o, r, now); ok {
			use = append(use, r)
			ys = append(ys, y)
		}
	}
	n := len(use)
	label := func(f func(*goalRow) string) [][]float64 {
		v := make([]string, n)
		for i, r := range use {
			v[i] = f(r)
		}
		return prepDummies(v, prepLabelMin)
	}
	num := func(f func(*goalRow) float64) []float64 {
		v := make([]float64, n)
		for i, r := range use {
			v[i] = f(r)
		}
		return prepStandard(v)
	}
	projects := label(func(r *goalRow) string { return fmt.Sprint(r.project) })
	types := label(func(r *goalRow) string { return r.typ })
	text := num(func(r *goalRow) float64 { return math.Log1p(float64(r.textLen)) })
	chain := num(func(r *goalRow) float64 { return float64(r.chainLen) })
	when := num(func(r *goalRow) float64 { return float64(r.assigned) })
	obs := make([]prepObs, n)
	for i, r := range use {
		x := []float64{1}
		x = append(x, projects[i]...)
		x = append(x, types[i]...)
		x = append(x, text[i], chain[i], when[i])
		load := make([]float64, len(arms))
		if j := slices.Index(arms, r.arm); j >= 0 {
			load[j] = 1
		}
		age := max(float64(now.UnixMilli()-r.assigned)/float64((24*time.Hour).Milliseconds()), 0)
		obs[i] = prepObs{Y: ys[i], X: x, Arm: load, Age: age, W: 1}
	}
	return obs, use
}

func (s *Service) goalPolCompute(ctx context.Context, now time.Time) (*goalPolicy, error) {
	rows, err := s.goalRows(ctx)
	if err != nil {
		return nil, err
	}
	return goalPolicyFrom(rows, now), nil
}

// goalPolicyFrom is the estimate from rows (apart, for tests).
func goalPolicyFrom(rows []*goalRow, now time.Time) *goalPolicy {
	pol := &goalPolicy{at: now, blocked: map[string]string{}, rows: rows, byArm: map[string]int{}}
	for _, d := range goalArmDefs {
		if d.Key != goalArmGoal {
			pol.arms = append(pol.arms, d.Key)
		}
	}
	for _, r := range rows {
		if goalData(r.policy) && r.arm != goalArmGoal && !slices.Contains(pol.arms, r.arm) {
			pol.arms = append(pol.arms, r.arm)
		}
	}
	obs, used := goalObservations(&goalPrimary, pol.arms, rows, now)
	pol.primary = fitPrepMix(pol.arms, obs, goalPrimary.Spread, nil)
	pol.measured = len(used)
	for _, r := range used {
		pol.byArm[r.arm]++
	}
	for i := range goalGuards {
		g := goalGuards[i]
		obs, used := goalObservations(&g, pol.arms, rows, now)
		gf := goalFit{prepOutcome: g, fit: fitPrepMix(pol.arms, obs, g.Spread, pol.primary.W), tasks: len(used)}
		pol.guards = append(pol.guards, gf)
		for j, arm := range pol.arms {
			if _, out := pol.blocked[arm]; out || gf.fit.armN(j) < prepGuardMin {
				continue
			}
			if gf.fit.pWorse(j, g.Margin) > prepGuardP {
				mean, _ := gf.fit.effect(j)
				lo, hi := gf.fit.interval(j)
				pol.blocked[arm] = fmt.Sprintf("worse than the goal only on %s: %s", g.Label, prepEffectText(&g, mean, lo, hi))
			}
		}
	}
	fits := slices.Clone(pol.primary.Fits)
	for _, g := range pol.guards {
		fits = append(fits, g.fit.Fits...)
	}
	for _, f := range fits {
		f.sample()
	}
	return pol
}

// loads are the named arms' loads on pol.arms (goal's is none).
func (p *goalPolicy) loads(names []string) [][]float64 {
	out := make([][]float64, len(names))
	for n, name := range names {
		out[n] = make([]float64, len(p.arms))
		if j := slices.Index(p.arms, name); j >= 0 {
			out[n][j] = 1
		}
	}
	return out
}

// pBest is each named arm's probability of being best, and none for one
// a guard has put out.
func (p *goalPolicy) pBest(names []string) []float64 {
	pb := p.primary.pBest(p.loads(names))
	for n, name := range names {
		if p.blocked[name] != "" {
			pb[n] = 0
		}
	}
	return pb
}

// best is the arm a read shows when the task has none: the likeliest best
// of those with prepGuardMin measured tasks and not out, else goal.
func (p *goalPolicy) best() string {
	names := append([]string{goalArmGoal}, p.arms...)
	pb := p.pBest(names)
	at := 0
	for i, name := range names {
		if j := slices.Index(p.arms, name); i == 0 || j < 0 || !goalArmKnown(name) || p.primary.armN(j) < prepGuardMin {
			continue
		}
		if pb[i] > pb[at] {
			at = i
		}
	}
	return names[at]
}

// GoalContextArmReport is one arm's figures.
type GoalContextArmReport struct {
	Arm   string `json:"arm"`
	Label string `json:"label"`
	// Share of new tasks; PBest before guards took any out.
	Share float64 `json:"share"`
	PBest float64 `json:"p_best"`
	// Drawn: tasks given it at random; Measured: of them, with the primary
	// outcome; N what they count for now, aged.
	Drawn    int     `json:"drawn"`
	Measured int     `json:"measured"`
	N        float64 `json:"n"`
	// Effect on drift flags against the goal only (log), its 90% interval,
	// and as text.
	Effect float64 `json:"effect"`
	Lo     float64 `json:"lo"`
	Hi     float64 `json:"hi"`
	Text   string  `json:"text,omitempty"`
	Out    string  `json:"out,omitempty"`
	GoalContextFigures
}

// GoalContextFigures describe an arm's drawn tasks as they came out.
type GoalContextFigures struct {
	Done       int `json:"done"`
	Judged     int `json:"judged"` // drawn with a coverage judgement
	PRs        int `json:"prs"`
	DriftFlags int `json:"drift_flags"`
	Findings   int `json:"findings"`
	Fixes      int `json:"fix_tasks"`
	// Done tasks the drift outcome leaves out, by why: no pull request, a
	// check still pending, or a check that ended in error.
	NoPR         int `json:"no_pr"`
	CheckPending int `json:"check_pending"`
	CheckError   int `json:"check_error"`
}

// GoalContextGuard is one guard's figures for one arm.
type GoalContextGuard struct {
	Guard string  `json:"guard"`
	Label string  `json:"label"`
	Arm   string  `json:"arm"`
	N     float64 `json:"n"`
	Text  string  `json:"text"`
	Worse float64 `json:"p_worse"`
}

// GoalContextReport is the policy and what came of it.
type GoalContextReport struct {
	At      time.Time              `json:"at"`
	Outcome string                 `json:"outcome"`
	Given   int                    `json:"given"`
	Spread  float64                `json:"spread"`
	Arms    []GoalContextArmReport `json:"arms"`
	Guards  []GoalContextGuard     `json:"guards"`
}

// GoalContextPolicy reports the policy (wp usage goal-context): every
// project's tasks, in sums, so admins only.
func (s *Service) GoalContextPolicy(ctx context.Context, actor *Actor) (*GoalContextReport, error) {
	if err := actor.Need(PermUsageRead); err != nil {
		return nil, err
	}
	if !actor.IsAdmin() {
		return nil, forbidden("the goal context policy sums every project's tasks: admins only")
	}
	p, err := s.goalPolNow(ctx)
	if err != nil {
		return nil, err
	}
	return p.report(), nil
}

func (p *goalPolicy) report() *GoalContextReport {
	rep := &GoalContextReport{At: p.at, Outcome: goalPrimary.Label, Spread: p.primary.sigma(), Arms: []GoalContextArmReport{}, Guards: []GoalContextGuard{}}
	drawn := map[string][]*goalRow{}
	for _, r := range p.rows {
		rep.Given++
		if goalData(r.policy) {
			drawn[r.arm] = append(drawn[r.arm], r)
		}
	}
	names := append([]string{goalArmGoal}, p.arms...)
	pb := p.primary.pBest(p.loads(names))
	var open []string
	for _, name := range names {
		if p.blocked[name] == "" {
			open = append(open, name)
		}
	}
	sh := prepShares(p.primary.pBest(p.loads(open)), nil)
	for n, arm := range names {
		a := GoalContextArmReport{Arm: arm, Label: goalArmLabel(arm), PBest: pb[n], Drawn: len(drawn[arm]), Measured: p.byArm[arm],
			Out: p.blocked[arm], GoalContextFigures: goalFigures(drawn[arm])}
		if arm == goalArmGoal {
			a.N = p.primary.armN(len(p.arms))
		} else {
			a.N = p.primary.armN(n - 1)
			a.Effect, _ = p.primary.effect(n - 1)
			a.Lo, a.Hi = p.primary.interval(n - 1)
			a.Text = prepEffectText(&goalPrimary, a.Effect, a.Lo, a.Hi)
			if !goalArmKnown(arm) && a.Out == "" {
				a.Out = "no longer drawn"
			}
		}
		if k := slices.Index(open, arm); k >= 0 {
			a.Share = sh[k]
		}
		rep.Arms = append(rep.Arms, a)
	}
	for _, g := range p.guards {
		for j, arm := range p.arms {
			if g.fit.armN(j) == 0 {
				continue
			}
			mean, _ := g.fit.effect(j)
			lo, hi := g.fit.interval(j)
			o := g.prepOutcome
			rep.Guards = append(rep.Guards, GoalContextGuard{Guard: g.Key, Label: g.Label, Arm: arm, N: g.fit.armN(j),
				Text: prepEffectText(&o, mean, lo, hi), Worse: g.fit.pWorse(j, g.Margin)})
		}
	}
	return rep
}

func goalFigures(rows []*goalRow) GoalContextFigures {
	var f GoalContextFigures
	for _, r := range rows {
		if r.judged {
			f.Judged++
		}
		if r.done {
			f.Done++
			switch {
			case r.prs == 0:
				f.NoPR++
			case r.checked < r.prs && r.errored > 0:
				f.CheckError++
			case r.checked < r.prs:
				f.CheckPending++
			}
		}
		f.PRs += r.prs
		f.DriftFlags += int(r.drift)
		f.Findings += int(r.findings)
		f.Fixes += int(r.fixes)
	}
	return f
}

// String is the report as wp usage goal-context prints it: figures, no
// verdict.
func (r *GoalContextReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "How much of a task's goal chain agents are shown (WP-1035), decided on %s, as of %s:\n", r.Outcome, r.At.Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "  %d tasks given an arm.\n", r.Given)
	for _, a := range r.Arms {
		fmt.Fprintf(&b, "\n  %s (%s): %.0f%% of new tasks, best with probability %.2f; %d drawn, %d measured (count as %.0f)\n",
			a.Arm, a.Label, a.Share*100, a.PBest, a.Drawn, a.Measured, a.N)
		if a.Text != "" {
			fmt.Fprintf(&b, "    drift flags against the goal only: %s\n", a.Text)
		}
		if a.Out != "" {
			fmt.Fprintf(&b, "    out: %s\n", a.Out)
		}
		if a.Drawn > 0 {
			fmt.Fprintf(&b, "    %d of %d drawn with a coverage judgement\n", a.Judged, a.Drawn)
			fmt.Fprintf(&b, "    %d done, %d pull requests, %d drift flags, %d review findings, %d follow-up fix tasks\n",
				a.Done, a.PRs, a.DriftFlags, a.Findings, a.Fixes)
			fmt.Fprintf(&b, "    done but not measured: %d with no pull request, %d with a check pending, %d with a check in error\n",
				a.NoPR, a.CheckPending, a.CheckError)
		}
	}
	if len(r.Guards) > 0 {
		b.WriteString("\n  Guards, against the goal only (an arm is out at 0.975 on 20 tasks):\n")
		for _, g := range r.Guards {
			fmt.Fprintf(&b, "    %-8s %-56s %s, worse with probability %.2f (%.0f tasks)\n", g.Arm, g.Label, g.Text, g.Worse, g.N)
		}
	}
	return b.String()
}
