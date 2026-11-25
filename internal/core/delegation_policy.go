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
	"time"
)

const (
	delegArmNone    = "none"
	delegArmScouts  = "scouts"
	delegArmManager = "manager"

	// delegPolicyAge: the estimate is worked out again when older.
	delegPolicyAge = time.Hour
	// delegPolicyRetry: after a failed compute the last estimate (or the
	// error) stands this long; delegPolicyTimeout bounds one compute,
	// whatever the request that asked for it does.
	delegPolicyRetry   = 10 * time.Minute
	delegPolicyTimeout = 2 * time.Minute
	// delegClaimFresh: a task is drawn an arm only this soon after its claim
	// began, so the arm is the take's, not told mid-work.
	delegClaimFresh = 10 * time.Minute
	// delegWithin: a task not done this long after its draw counts as
	// undone, and its cost so far as its cost; follow-up fixes are counted
	// this long after it was done.
	delegWithin = 14 * 24 * time.Hour
	// delegEscapesWithin: escapes are counted this long after it was done.
	delegEscapesWithin = 30 * 24 * time.Hour
	// delegUsageWithin: an agent's usage is seen when its person's machines
	// sent some this recently.
	delegUsageWithin = 14 * 24 * time.Hour
)

// delegArmDef is a way of working. A changed text is a new key (scouts-2),
// tested as new; a key no longer here stays in the history and the
// estimate, and is never drawn.
type delegArmDef struct {
	Key, Label, Text, Round string
}

const delegClaims = "A worker's findings and claims are leads, not facts: check each before you act on it, and decide each one: fixed, rejected (say why in your summary), or filed (`wp new`). " +
	"Waypoint measures the task's cost by model and how its work holds up by itself; no report is needed."

var delegArmDefs = []delegArmDef{
	{Key: delegArmNone, Label: "nothing said"},
	{Key: delegArmScouts, Label: "Haiku scouts",
		Text: "Delegation (Waypoint's delegation trial, arm scouts): hand the reading to Haiku subagents (the haiku-worker agent type, or the Agent tool with model haiku): " +
			"finding where things are, reading files, logs and run output and summarizing them, running tests and reporting what failed, and a first review of slices of your diff. " +
			"Brief each with the paths and the question and ask for file:line evidence; run independent ones in parallel. You design, edit and fix yourself. " + delegClaims,
		Round: "Delegation (arm scouts): Haiku workers may check findings for you (one finding each, file:line evidence asked); you decide each verdict and make the fixes."},
	{Key: delegArmManager, Label: "Haiku scouts, Sonnet implements",
		Text: "Delegation (Waypoint's delegation trial, arm manager): work as the manager. Haiku subagents (the haiku-worker agent type, or the Agent tool with model haiku) do the searching, reading and summarizing of files, logs and test output, and review slices of the diff. " +
			"One Sonnet subagent (sonnet-worker, or the Agent tool with model sonnet) implements: give it the plan, the files, and the tests to write first, and send what tests and reviews find back to the same worker (SendMessage) rather than fixing it yourself. " +
			"You decide what to do, write the briefs, check what workers claim, and read the final diff before you ship. Take the work back only when the worker fails twice on the same thing, or the change is a few lines. " + delegClaims,
		Round: "Delegation (arm manager): hand the findings you judge real to your Sonnet worker (or a new sonnet-worker) to write the failing test and the fix, read its diff, and record each verdict yourself."},
}

func delegArmDef0(key string) (delegArmDef, bool) {
	for _, d := range delegArmDefs {
		if d.Key == key {
			return d, true
		}
	}
	return delegArmDef{}, false
}

func delegArmLabel(key string) string {
	if d, ok := delegArmDef0(key); ok {
		return d.Label
	}
	return key + " (no longer drawn)"
}

var (
	// The primary outcome is the task's Claude cost at API prices. Its
	// spread is that of 153 autopilot tasks measured before this was built
	// (sd of log cost 0.93; page X21).
	delegPrimary = prepOutcome{Key: "cost", Label: "the task's Claude cost (API prices)", Log: true, Spread: 0.9}
	delegGuards  = []prepOutcome{
		{Key: "undone", Label: "tasks not done in 14 days", Spread: 0.45, Margin: 0.05},
		{Key: "findings", Label: "review findings on the task's pull requests", Log: true, Spread: 0.9, Margin: 0.10},
		{Key: "rounds", Label: "audit findings fixed or confirmed in its review rounds", Log: true, Spread: 0.9, Margin: 0.10},
		{Key: "fixes", Label: "follow-up fix tasks filed against it", Log: true, Spread: 0.5, Margin: 0.10},
		{Key: "escapes", Label: "bugs its pull requests shipped, found later", Log: true, Spread: 0.3, Margin: 0.05},
	}
)

// delegRow is one task given an arm, and what came of it.
type delegRow struct {
	issue        int64
	key          string
	arm, policy  string
	risk         string
	textLen      int
	assigned     int64
	project      int64
	typ          string
	resolved     int64
	done         bool
	measured     bool
	usage        UsageTrendTask
	prs          int
	findings     float64
	severity     map[string]int
	audit        map[string]int
	fixes, filed float64
	escapes      float64
	tier         string
}

func (r *delegRow) cost() float64 { return r.usage.CostUSD }

// delegGuardFit is one guard's estimate.
type delegGuardFit struct {
	prepOutcome
	fit   *prepMix
	tasks int
}

// delegPolicy is the estimate as last worked out.
type delegPolicy struct {
	at       time.Time
	arms     []string
	primary  *prepMix
	guards   []delegGuardFit
	blocked  map[string]string
	rows     []*delegRow
	measured int
	byArm    map[string]int
}

// delegPolicyNow is the estimate, worked out again when older than
// delegPolicyAge. One compute runs at a time, under its own deadline rather
// than the asking request's; while it runs, or for delegPolicyRetry after
// one failed, takes get the last estimate (or, with none, the failure)
// instead of waiting or computing again.
func (s *Service) delegPolicyNow(ctx context.Context) (*delegPolicy, error) {
	if pol, ok, err := s.delegPolicyCached(false); ok {
		return pol, err
	}
	s.delegPolComp.Lock()
	defer s.delegPolComp.Unlock()
	if pol, ok, err := s.delegPolicyCached(true); ok {
		return pol, err
	}
	now := s.now()
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), delegPolicyTimeout)
	defer cancel()
	pol, err := s.delegPolicyCompute(cctx, now)
	s.delegPolMu.Lock()
	defer s.delegPolMu.Unlock()
	s.delegPolBusy = false
	if err != nil {
		s.delegPolRetry, s.delegPolErr = now.Add(delegPolicyRetry), err
		if s.delegPol != nil {
			s.log.WarnContext(ctx, "delegation policy: the last estimate stands", "err", err)
			return s.delegPol, nil
		}
		return nil, err
	}
	s.delegPol, s.delegPolRetry, s.delegPolErr = pol, time.Time{}, nil
	return pol, nil
}

// delegPolicyCached is the estimate to use without computing, if there is
// one (ok); claim marks a compute as running when there isn't.
func (s *Service) delegPolicyCached(claim bool) (*delegPolicy, bool, error) {
	s.delegPolMu.Lock()
	defer s.delegPolMu.Unlock()
	now, pol := s.now(), s.delegPol
	switch {
	case pol != nil && now.Sub(pol.at) < delegPolicyAge && !now.Before(pol.at):
		return pol, true, nil
	case now.Before(s.delegPolRetry):
		if pol != nil {
			return pol, true, nil
		}
		return nil, true, s.delegPolErr
	case pol != nil && s.delegPolBusy:
		return pol, true, nil
	}
	if claim {
		s.delegPolBusy = true
	}
	return nil, false, nil
}

// delegIn is SQL's IN list of the assigned tasks.
const delegIn = `(SELECT issue_id FROM delegation_assignments)`

// delegRows reads every task given an arm, and what came of it.
func (s *Service) delegRows(ctx context.Context, now time.Time) ([]*delegRow, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.issue_id, p.key || '-' || i.number, a.arm, a.policy, a.risk, a.text_len, a.assigned_at,
			i.project_id, i.type, coalesce(i.resolved_at, 0), coalesce(st.category = 'done', 0)
		FROM delegation_assignments a JOIN issues i ON i.id = a.issue_id JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		ORDER BY a.assigned_at, a.issue_id`)
	if err != nil {
		return nil, err
	}
	var out []*delegRow
	by := map[int64]*delegRow{}
	byKey := map[string]*delegRow{}
	oldest := now.UnixMilli()
	for rows.Next() {
		r := &delegRow{severity: map[string]int{}, audit: map[string]int{}}
		if err := rows.Scan(&r.issue, &r.key, &r.arm, &r.policy, &r.risk, &r.textLen, &r.assigned, &r.project, &r.typ, &r.resolved, &r.done); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		by[r.issue], byKey[r.key] = r, r
		oldest = min(oldest, r.assigned)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	days := int((now.UnixMilli()-oldest)/(24*time.Hour).Milliseconds()) + 2
	tr, err := s.UsageTrends(ctx, SystemActor(), UsageQuery{Days: min(days, 365), User: "all", allTasks: true})
	if err != nil {
		return nil, err
	}
	for _, t := range tr.Tasks {
		if r := byKey[t.Key]; r != nil {
			r.usage, r.measured = t, t.CostUSD > 0
		}
	}
	rows, err = s.db.R.QueryContext(ctx, `WITH x AS (SELECT DISTINCT el.issue_id, p.id AS pid
				FROM external_links el JOIN pull_requests p ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
				WHERE el.issue_id IN `+delegIn+`)
		SELECT x.issue_id, x.pid, coalesce(f.severity, ''), count(f.id), (SELECT count(*) FROM escapes e WHERE e.intro_pr_id = x.pid)
		FROM x LEFT JOIN bot_findings f ON f.pr_id = x.pid AND f.state != 'withdrawn'
		GROUP BY x.issue_id, x.pid, f.severity`)
	if err != nil {
		return nil, err
	}
	seen := map[[2]int64]bool{}
	for rows.Next() {
		var id, pid int64
		var sev string
		var n, esc int
		if err := rows.Scan(&id, &pid, &sev, &n, &esc); err != nil {
			rows.Close()
			return nil, err
		}
		r := by[id]
		if r == nil {
			continue
		}
		if k := [2]int64{id, pid}; !seen[k] {
			seen[k] = true
			r.prs++
			r.escapes += float64(esc)
		}
		if n > 0 {
			r.findings += float64(n)
			r.severity[sev] += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.delegCount(ctx, `SELECT issue_id, state, count(*) FROM (SELECT DISTINCT w.issue_id, f.id, f.state
			FROM work_log w JOIN code_audit_findings f ON f.audit_id = w.audit_id
			WHERE w.kind = 'review' AND w.issue_id IN `+delegIn+`) GROUP BY issue_id, state`, func(id int64, k string, n int) {
		if r := by[id]; r != nil {
			r.audit[k] += n
		}
	}); err != nil {
		return nil, err
	}
	if err := s.delegCount(ctx, `SELECT target_id, 'fix', count(*) FROM issue_relations
		WHERE kind = 'follows' AND subkind = 'fix' AND target_id IN `+delegIn+` GROUP BY target_id
		UNION ALL SELECT origin_issue_id, 'filed', count(*) FROM issues WHERE origin_issue_id IN `+delegIn+` GROUP BY origin_issue_id`,
		func(id int64, k string, n int) {
			if r := by[id]; r != nil {
				if k == "fix" {
					r.fixes += float64(n)
				} else {
					r.filed += float64(n)
				}
			}
		}); err != nil {
		return nil, err
	}
	if err := s.delegCount(ctx, `SELECT issue_id, tier, count(*) FROM review_stakes WHERE issue_id IN `+delegIn+` GROUP BY issue_id, tier`,
		func(id int64, k string, _ int) {
			if r := by[id]; r != nil && delegTierRank(k) > delegTierRank(r.tier) {
				r.tier = k
			}
		}); err != nil {
		return nil, err
	}
	return out, nil
}

// delegCount reads rows of (issue id, label, count).
func (s *Service) delegCount(ctx context.Context, query string, f func(id int64, label string, n int)) error {
	rows, err := s.db.R.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id sql.NullInt64
		var k string
		var n int
		if err := rows.Scan(&id, &k, &n); err != nil {
			return err
		}
		if id.Valid {
			f(id.Int64, k, n)
		}
	}
	return rows.Err()
}

func delegTierRank(t string) int {
	switch t {
	case "high":
		return 4
	case "elevated":
		return 3
	case "normal":
		return 2
	case "":
		return 0
	}
	return 1
}

// delegValue is a row's value of an outcome, when it has one yet. Only a
// task whose usage Waypoint saw (an agent worked it) has any.
func delegValue(o *prepOutcome, r *delegRow, now time.Time) (float64, bool) {
	if !r.measured {
		return 0, false
	}
	age := now.UnixMilli() - r.assigned
	since := now.UnixMilli() - r.resolved
	switch o.Key {
	case "cost":
		return math.Log(r.cost()), r.done || age > delegWithin.Milliseconds()
	case "undone":
		if r.done && r.resolved > 0 && r.resolved-r.assigned <= delegWithin.Milliseconds() {
			return 0, true
		}
		return 1, age > delegWithin.Milliseconds()
	case "findings":
		return math.Log1p(r.findings), r.done && r.prs > 0
	case "rounds":
		return math.Log1p(float64(r.audit["fixed"] + r.audit["confirmed"])), r.done
	case "fixes":
		return math.Log1p(r.fixes), r.done && r.resolved > 0 && since > delegWithin.Milliseconds()
	case "escapes":
		return math.Log1p(r.escapes), r.done && r.prs > 0 && r.resolved > 0 && since > delegEscapesWithin.Milliseconds()
	}
	return 0, false
}

// delegObservations turns the drawn rows that have the outcome into
// observations, with what was known of each task before its draw as
// covariates (project, type, risk, the text's length, when).
func delegObservations(o *prepOutcome, arms []string, rows []*delegRow, now time.Time) ([]prepObs, []*delegRow) {
	var use []*delegRow
	var ys []float64
	for _, r := range rows {
		if r.policy != "thompson" {
			continue
		}
		if y, ok := delegValue(o, r, now); ok {
			use = append(use, r)
			ys = append(ys, y)
		}
	}
	n := len(use)
	label := func(f func(*delegRow) string) [][]float64 {
		v := make([]string, n)
		for i, r := range use {
			v[i] = f(r)
		}
		return prepDummies(v, prepLabelMin)
	}
	num := func(f func(*delegRow) float64) []float64 {
		v := make([]float64, n)
		for i, r := range use {
			v[i] = f(r)
		}
		return prepStandard(v)
	}
	projects := label(func(r *delegRow) string { return fmt.Sprint(r.project) })
	types := label(func(r *delegRow) string { return r.typ })
	risks := label(func(r *delegRow) string { return r.risk })
	text := num(func(r *delegRow) float64 { return math.Log1p(float64(r.textLen)) })
	when := num(func(r *delegRow) float64 { return float64(r.assigned) })
	obs := make([]prepObs, n)
	for i, r := range use {
		x := []float64{1}
		x = append(x, projects[i]...)
		x = append(x, types[i]...)
		x = append(x, risks[i]...)
		x = append(x, text[i], when[i])
		load := make([]float64, len(arms))
		if j := slices.Index(arms, r.arm); j >= 0 {
			load[j] = 1
		}
		age := max(float64(now.UnixMilli()-r.assigned)/float64((24*time.Hour).Milliseconds()), 0)
		obs[i] = prepObs{Y: ys[i], X: x, Arm: load, Age: age, W: 1}
	}
	return obs, use
}

// delegPolicyCompute works the estimate out from every row.
func (s *Service) delegPolicyCompute(ctx context.Context, now time.Time) (*delegPolicy, error) {
	rows, err := s.delegRows(ctx, now)
	if err != nil {
		return nil, err
	}
	return delegPolicyFrom(rows, now), nil
}

// delegPolicyFrom is the estimate from rows (apart, for tests).
func delegPolicyFrom(rows []*delegRow, now time.Time) *delegPolicy {
	pol := &delegPolicy{at: now, blocked: map[string]string{}, rows: rows, byArm: map[string]int{}}
	for _, d := range delegArmDefs {
		if d.Key != delegArmNone {
			pol.arms = append(pol.arms, d.Key)
		}
	}
	for _, r := range rows {
		if r.policy == "thompson" && r.arm != delegArmNone && !slices.Contains(pol.arms, r.arm) {
			pol.arms = append(pol.arms, r.arm)
		}
	}
	obs, used := delegObservations(&delegPrimary, pol.arms, rows, now)
	pol.primary = fitPrepMix(pol.arms, obs, delegPrimary.Spread, nil)
	pol.measured = len(used)
	for _, r := range used {
		pol.byArm[r.arm]++
	}
	for i := range delegGuards {
		g := delegGuards[i]
		obs, used := delegObservations(&g, pol.arms, rows, now)
		gf := delegGuardFit{prepOutcome: g, fit: fitPrepMix(pol.arms, obs, g.Spread, pol.primary.W), tasks: len(used)}
		pol.guards = append(pol.guards, gf)
		for j, arm := range pol.arms {
			if _, out := pol.blocked[arm]; out || gf.fit.armN(j) < prepGuardMin {
				continue
			}
			if gf.fit.pWorse(j, g.Margin) > prepGuardP {
				mean, _ := gf.fit.effect(j)
				lo, hi := gf.fit.interval(j)
				pol.blocked[arm] = fmt.Sprintf("worse than nothing said on %s: %s", g.Label, prepEffectText(&g, mean, lo, hi))
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

// candidates are what a task may be given: nothing said, and every arm
// still drawn that isn't out. Their loads are on pol.arms.
func (p *delegPolicy) candidates(withBlocked bool) ([]string, [][]float64) {
	names := []string{delegArmNone}
	loads := [][]float64{make([]float64, len(p.arms))}
	for _, d := range delegArmDefs {
		j := slices.Index(p.arms, d.Key)
		if d.Key == delegArmNone || j < 0 || (!withBlocked && p.blocked[d.Key] != "") {
			continue
		}
		l := make([]float64, len(p.arms))
		l[j] = 1
		names, loads = append(names, d.Key), append(loads, l)
	}
	return names, loads
}

// best is the arm for a task that is no evidence: the likeliest best of
// those with prepGuardMin measured tasks, else nothing said.
func (p *delegPolicy) best(names []string, pbest []float64) int {
	at := 0
	for i, name := range names {
		j := slices.Index(p.arms, name)
		if i == 0 || j < 0 || p.primary.armN(j) < prepGuardMin {
			continue
		}
		if pbest[i] > pbest[at] {
			at = i
		}
	}
	return at
}

// delegDraw is a task's place in [0, 1) for this policy: fixed for the
// task, apart from the prep policy's, and nothing an agent can steer.
func delegDraw(issueID int64) float64 {
	h := sha256.Sum256(fmt.Appendf(nil, "delegation-policy/%d", issueID))
	return float64(binary.BigEndian.Uint64(h[:8])>>11) / (1 << 53)
}

// DelegationArm is the way of working a task was given.
type DelegationArm struct {
	Arm    string `json:"arm"`
	Policy string `json:"policy"` // thompson, best, excluded
	Text   string `json:"text,omitempty"`
}

func delegationArmOut(arm, policy string) *DelegationArm {
	d, _ := delegArmDef0(arm)
	return &DelegationArm{Arm: arm, Policy: policy, Text: d.Text}
}

// delegAssignmentOf is the task's arm, nil when it has none.
func (s *Service) delegAssignmentOf(ctx context.Context, q queryer, issueID int64) (*DelegationArm, error) {
	var arm, policy string
	err := q.QueryRowContext(ctx, `SELECT arm, policy FROM delegation_assignments WHERE issue_id = ?`, issueID).Scan(&arm, &policy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return delegationArmOut(arm, policy), nil
}

// delegUsageSeen says whether Waypoint sees the agent's usage: its
// person's machines sent some lately. Another's work tells nothing.
func (s *Service) delegUsageSeen(ctx context.Context, a *Actor) bool {
	if a == nil || a.User == nil {
		return false
	}
	var ok bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM usage_hours WHERE user_id = ? AND hour >= ?)`,
		a.User.ID, s.now().Add(-delegUsageWithin).UnixMilli()).Scan(&ok); err != nil {
		return false
	}
	return ok
}

// delegationArm is the task's way of working for an agent taking it,
// drawn the first time and kept. A task whose risk is high is given
// nothing said; an agent whose usage isn't seen, the best arm.
func (s *Service) delegationArm(ctx context.Context, a *Actor, i *Issue, via string) (*DelegationArm, error) {
	if got, err := s.delegAssignmentOf(ctx, s.db.R, i.ID); err != nil || got != nil {
		return got, err
	}
	if i.ClaimedAt == nil || s.now().Sub(*i.ClaimedAt) > delegClaimFresh {
		return nil, nil
	}
	r, err := s.issueRiskByID(ctx, s.db.R, SystemActor(), i.ID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("no risk for the task")
	}
	risk := r.Level
	arm, policy, propensity := delegArmNone, "excluded", 1.0
	shares := map[string]float64{}
	if risk != "high" {
		pol, err := s.delegPolicyNow(ctx)
		if err != nil {
			return nil, err
		}
		names, loads := pol.candidates(false)
		pb := pol.primary.pBest(loads)
		sh := prepShares(pb, nil)
		for n, name := range names {
			shares[name] = math.Round(sh[n]*1e4) / 1e4
		}
		if s.delegUsageSeen(ctx, a) {
			at := prepPick(sh, delegDraw(i.ID))
			arm, policy, propensity = names[at], "thompson", sh[at]
		} else {
			arm, policy = names[pol.best(names, pb)], "best"
		}
	}
	raw, _ := json.Marshal(shares)
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO delegation_assignments(issue_id, arm, policy, propensity, shares, risk, text_len, via, assigned_at)
		VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(issue_id) DO NOTHING`,
		i.ID, arm, policy, propensity, string(raw), risk, prepTextLen(i), via, s.nowMS()); err != nil {
		return nil, err
	}
	return s.delegAssignmentOf(ctx, s.db.W, i.ID)
}

// delegationRoundLine is what a review round's prompt adds for the task's
// arm ("" for none, or no arm).
func (s *Service) delegationRoundLine(ctx context.Context, issueID int64) string {
	got, err := s.delegAssignmentOf(ctx, s.db.R, issueID)
	if err != nil || got == nil {
		return ""
	}
	d, _ := delegArmDef0(got.Arm)
	return d.Round
}

// DelegationArmReport is one arm's figures.
type DelegationArmReport struct {
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
	// Effect on cost against nothing said (log; -0.1 is about 10% less),
	// its 90% interval, and as text.
	Effect float64 `json:"effect"`
	Lo     float64 `json:"lo"`
	Hi     float64 `json:"hi"`
	Text   string  `json:"text,omitempty"`
	Out    string  `json:"out,omitempty"`
	DelegationFigures
}

// DelegationFigures describe tasks of an arm as they came out (drawn ones
// with usage seen): no covariates taken out.
type DelegationFigures struct {
	CostMedian float64 `json:"cost_median"`
	CostMean   float64 `json:"cost_mean"`
	// CostShare: of their cost, the share by model family and main thread
	// or subagents ("opus", "haiku/sub").
	CostShare   map[string]float64 `json:"cost_share"`
	Subagents   float64            `json:"subagents_median"`
	ToolCalls   float64            `json:"tool_calls_median"`
	Compactions float64            `json:"compactions_mean"`
	AvgContext  float64            `json:"avg_context_median"`
	Done        int                `json:"done"`
	DoneIn14    int                `json:"done_in_14_days"`
	HoursToDone float64            `json:"hours_to_done_median"`
	PRs         int                `json:"prs"`
	Findings    map[string]int     `json:"findings"` // review findings by severity
	Audit       map[string]int     `json:"audit"`    // review rounds' audit findings by verdict
	Fixes       int                `json:"fix_tasks"`
	Filed       int                `json:"filed"`
	Escapes     int                `json:"escapes"`
	Tiers       map[string]int     `json:"stakes"` // tasks by their rounds' highest stakes tier
}

// DelegationGuard is one guard's figures for one arm.
type DelegationGuard struct {
	Guard string  `json:"guard"`
	Label string  `json:"label"`
	Arm   string  `json:"arm"`
	N     float64 `json:"n"`
	Text  string  `json:"text"`
	Worse float64 `json:"p_worse"`
}

// DelegationReport is the delegation policy and what came of it.
type DelegationReport struct {
	At       time.Time             `json:"at"`
	Outcome  string                `json:"outcome"`
	Given    int                   `json:"given"`    // tasks given an arm
	Excluded int                   `json:"excluded"` // high risk: nothing said
	Best     int                   `json:"best"`     // usage not seen: the best arm
	Spread   float64               `json:"spread"`
	Arms     []DelegationArmReport `json:"arms"`
	Guards   []DelegationGuard     `json:"guards"`
}

// DelegationPolicy reports the policy (wp usage delegation): every
// person's tasks, in sums, so admins only.
func (s *Service) DelegationPolicy(ctx context.Context, actor *Actor) (*DelegationReport, error) {
	if err := actor.need(PermUsageRead); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, forbidden("the delegation policy sums every person's usage: admins only")
	}
	p, err := s.delegPolicyNow(ctx)
	if err != nil {
		return nil, err
	}
	return p.report(s.now()), nil
}

func (p *delegPolicy) report(now time.Time) *DelegationReport {
	rep := &DelegationReport{At: p.at, Outcome: delegPrimary.Label, Spread: p.primary.sigma(), Arms: []DelegationArmReport{}, Guards: []DelegationGuard{}}
	drawn := map[string][]*delegRow{}
	for _, r := range p.rows {
		rep.Given++
		switch r.policy {
		case "excluded":
			rep.Excluded++
		case "best":
			rep.Best++
		case "thompson":
			drawn[r.arm] = append(drawn[r.arm], r)
		}
	}
	allNames, allLoads := p.candidates(true)
	pb := p.primary.pBest(allLoads)
	names, loads := p.candidates(false)
	shares := prepShares(p.primary.pBest(loads), nil)
	k := len(p.arms)
	add := func(arm string, j int) {
		a := DelegationArmReport{Arm: arm, Label: delegArmLabel(arm), Drawn: len(drawn[arm]), Measured: p.byArm[arm], N: p.primary.armN(j),
			Out: p.blocked[arm], DelegationFigures: delegFigures(drawn[arm], now)}
		if arm != delegArmNone {
			a.Effect, _ = p.primary.effect(j)
			a.Lo, a.Hi = p.primary.interval(j)
			a.Text = prepEffectText(&delegPrimary, a.Effect, a.Lo, a.Hi)
			if _, ok := delegArmDef0(arm); !ok && a.Out == "" {
				a.Out = "no longer drawn"
			}
		}
		if n := slices.Index(allNames, arm); n >= 0 {
			a.PBest = pb[n]
		}
		if n := slices.Index(names, arm); n >= 0 {
			a.Share = shares[n]
		}
		rep.Arms = append(rep.Arms, a)
	}
	add(delegArmNone, k)
	for j, arm := range p.arms {
		add(arm, j)
	}
	for _, g := range p.guards {
		for j, arm := range p.arms {
			if g.fit.armN(j) == 0 {
				continue
			}
			mean, _ := g.fit.effect(j)
			lo, hi := g.fit.interval(j)
			o := g.prepOutcome
			rep.Guards = append(rep.Guards, DelegationGuard{Guard: g.Key, Label: g.Label, Arm: arm, N: g.fit.armN(j),
				Text: prepEffectText(&o, mean, lo, hi), Worse: g.fit.pWorse(j, g.Margin)})
		}
	}
	return rep
}

// delegFigures sums an arm's tasks as they came out.
func delegFigures(rows []*delegRow, now time.Time) DelegationFigures {
	f := DelegationFigures{CostShare: map[string]float64{}, Findings: map[string]int{}, Audit: map[string]int{}, Tiers: map[string]int{}}
	var costs, subs, tools, comps, ctxs, hours []float64
	var total float64
	for _, r := range rows {
		if r.done {
			f.Done++
			if r.resolved > 0 && r.resolved-r.assigned <= delegWithin.Milliseconds() {
				f.DoneIn14++
			}
			if r.resolved >= r.assigned {
				hours = append(hours, float64(r.resolved-r.assigned)/float64(time.Hour.Milliseconds()))
			}
		}
		f.PRs += r.prs
		for k, n := range r.severity {
			f.Findings[k] += n
		}
		for k, n := range r.audit {
			f.Audit[k] += n
		}
		f.Fixes += int(r.fixes)
		f.Filed += int(r.filed)
		f.Escapes += int(r.escapes)
		if r.tier != "" {
			f.Tiers[r.tier]++
		}
		if !r.measured {
			continue
		}
		costs = append(costs, r.cost())
		subs = append(subs, float64(r.usage.Subagents))
		tools = append(tools, float64(r.usage.ToolCalls))
		comps = append(comps, float64(r.usage.Compactions))
		if r.usage.AvgContext != nil {
			ctxs = append(ctxs, *r.usage.AvgContext)
		}
		for k, c := range r.usage.CostBy {
			f.CostShare[k] += c
			total += c
		}
	}
	if total > 0 {
		for k := range f.CostShare {
			f.CostShare[k] = math.Round(f.CostShare[k]/total*1e4) / 1e4
		}
	}
	if len(costs) > 0 {
		f.CostMedian, f.CostMean = median(costs), mean(costs)
		f.Subagents, f.ToolCalls, f.Compactions = median(subs), median(tools), mean(comps)
	}
	if len(ctxs) > 0 {
		f.AvgContext = median(ctxs)
	}
	if len(hours) > 0 {
		f.HoursToDone = median(hours)
	}
	return f
}

// String is the report as wp usage delegation prints it: figures, no
// verdict.
func (r *DelegationReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "How agents are told to delegate (WP-1262), decided on %s, as of %s:\n", r.Outcome, r.At.Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "  %d tasks given an arm: %d excluded (high risk: nothing said), %d the best arm (usage not seen); the rest drawn.\n", r.Given, r.Excluded, r.Best)
	if r.Spread > 0 {
		fmt.Fprintf(&b, "  One task's cost varies by a factor of %.1f once covariates are out.\n", math.Exp(r.Spread))
	}
	for _, a := range r.Arms {
		fmt.Fprintf(&b, "\n  %s (%s): %.0f%% of new tasks, best with probability %.2f; %d drawn, %d measured (count as %.0f)\n",
			a.Arm, a.Label, a.Share*100, a.PBest, a.Drawn, a.Measured, a.N)
		if a.Text != "" {
			fmt.Fprintf(&b, "    cost against nothing said: %s\n", a.Text)
		}
		if a.Out != "" {
			fmt.Fprintf(&b, "    out: %s\n", a.Out)
		}
		f := a.DelegationFigures
		if a.Measured > 0 || f.CostMedian > 0 {
			fmt.Fprintf(&b, "    cost median $%.2f, mean $%.2f; by model: %s\n", f.CostMedian, f.CostMean, delegShareText(f.CostShare))
			fmt.Fprintf(&b, "    medians: %.0f subagents, %.0f tool calls, %.0fk context; compactions %.1f per task\n",
				f.Subagents, f.ToolCalls, f.AvgContext/1000, f.Compactions)
		}
		if a.Drawn > 0 {
			fmt.Fprintf(&b, "    %d done (%d within 14 days, median %.1f h); %d pull requests, review findings %s; audit findings %s\n",
				f.Done, f.DoneIn14, f.HoursToDone, f.PRs, delegCountText(f.Findings), delegCountText(f.Audit))
			fmt.Fprintf(&b, "    %d follow-up fix tasks, %d tasks filed from them, %d escapes; stakes %s\n", f.Fixes, f.Filed, f.Escapes, delegCountText(f.Tiers))
		}
	}
	if len(r.Guards) > 0 {
		b.WriteString("\n  Guards, against nothing said (an arm is out at 0.975 on 20 tasks):\n")
		for _, g := range r.Guards {
			fmt.Fprintf(&b, "    %-8s %-56s %s, worse with probability %.2f (%.0f tasks)\n", g.Arm, g.Label, g.Text, g.Worse, g.N)
		}
	}
	return b.String()
}

func delegShareText(m map[string]float64) string {
	if len(m) == 0 {
		return "none seen"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y string) int {
		if m[x] != m[y] {
			if m[x] > m[y] {
				return -1
			}
			return 1
		}
		return strings.Compare(x, y)
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %.1f%%", k, m[k]*100)
	}
	return strings.Join(parts, ", ")
}

func delegCountText(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}
