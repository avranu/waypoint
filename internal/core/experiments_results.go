package core

// Experiment results (WP-254, WP-ADR53). Each exposed task is measured on
// the usage the trends already attribute to tasks, plus its pull requests'
// review findings and babysit rounds; the arms are compared with medians,
// a bootstrap interval on the ratio of medians, Mann-Whitney tests with a
// Holm correction across metrics, a test on the done rates, and a sample
// ratio check that says when the assignment itself went wrong.

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"time"
)

// ExperimentMetric is something a task is measured by. Lower is better
// for all of them.
type ExperimentMetric struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Unit  string `json:"unit"` // usd, tokens, count, hours
}

type expTask struct {
	key      string
	done     bool
	measured bool
	row      UsageTrendTask
	findings float64
	rounds   float64
	// Later tasks that follow this one, and those of them that fix it.
	followups float64
	fixes     float64
	// What its self-reviews found, and what its adversarial reviews did by a
	// count no model read; the bool says there was such a count.
	selfReview   float64
	selfReviewed bool
	reviewFound  float64
	reviewed     bool
	prComments   float64 // review comments that aren't a bot finding's
}

// experimentMetric is one measure of a task, and its value when it has one.
type experimentMetric struct {
	ExperimentMetric
	get func(t *expTask) (float64, bool)
}

var experimentMetrics = []experimentMetric{
	{ExperimentMetric{"cost_usd", "Cost", "usd"}, func(t *expTask) (float64, bool) { return t.row.CostUSD, t.row.CostUSD > 0 }},
	{ExperimentMetric{"tokens", "Tokens", "tokens"}, func(t *expTask) (float64, bool) {
		u := t.row.UsageTotals
		return float64(u.Input + u.Output + u.CacheWrite + u.CacheRead), true
	}},
	{ExperimentMetric{"output", "Output tokens", "tokens"}, func(t *expTask) (float64, bool) { return float64(t.row.Output), true }},
	{ExperimentMetric{"messages", "Turns", "count"}, func(t *expTask) (float64, bool) { return float64(t.row.Messages), true }},
	{ExperimentMetric{"tool_calls", "Tool calls", "count"}, func(t *expTask) (float64, bool) { return float64(t.row.ToolCalls), true }},
	{ExperimentMetric{"tool_errors", "Tool errors", "count"}, func(t *expTask) (float64, bool) { return float64(t.row.ToolErrors), true }},
	{ExperimentMetric{"active_hours", "Active hours", "hours"}, func(t *expTask) (float64, bool) { return float64(t.row.ActiveHours), true }},
	{ExperimentMetric{"sessions", "Sessions", "count"}, func(t *expTask) (float64, bool) { return float64(t.row.Sessions), true }},
	{ExperimentMetric{"subagents", "Subagents", "count"}, func(t *expTask) (float64, bool) { return float64(t.row.Subagents), true }},
	{ExperimentMetric{"compactions", "Compactions", "count"}, func(t *expTask) (float64, bool) { return float64(t.row.Compactions), true }},
	{ExperimentMetric{"avg_context", "Average context", "tokens"}, func(t *expTask) (float64, bool) {
		if t.row.AvgContext == nil {
			return 0, false
		}
		return *t.row.AvgContext, true
	}},
	{ExperimentMetric{"findings", "Review findings", "count"}, func(t *expTask) (float64, bool) { return t.findings, t.row.PRs > 0 }},
	{ExperimentMetric{"rounds", "Babysit rounds", "count"}, func(t *expTask) (float64, bool) { return t.rounds, t.row.PRs > 0 }},
	{ExperimentMetric{"followups", "Follow-up tasks", "count"}, func(t *expTask) (float64, bool) { return t.followups, true }},
	{ExperimentMetric{"fixes", "Follow-up fixes", "count"}, func(t *expTask) (float64, bool) { return t.fixes, true }},
	{ExperimentMetric{"self_review", "Self-review findings", "count"}, func(t *expTask) (float64, bool) { return t.selfReview, t.selfReviewed }},
	{ExperimentMetric{"review_found", "Adversarial review findings", "count"}, func(t *expTask) (float64, bool) { return t.reviewFound, t.reviewed }},
	{ExperimentMetric{"pr_comments", "Review comments", "count"}, func(t *expTask) (float64, bool) { return t.prComments, t.row.PRs > 0 }},
}

// ExperimentStat is one metric in one arm, and how it compares with the
// control's.
type ExperimentStat struct {
	Key    string   `json:"key"`
	N      int      `json:"n"`
	Median *float64 `json:"median"`
	Mean   *float64 `json:"mean"`
	// Ratio is this arm's median over the control's, with a 95% bootstrap
	// interval; P is Mann-Whitney's, PAdj that after Holm's correction for
	// all the comparisons in the run.
	Ratio *float64 `json:"ratio,omitempty"`
	Low   *float64 `json:"low,omitempty"`
	High  *float64 `json:"high,omitempty"`
	P     *float64 `json:"p,omitempty"`
	PAdj  *float64 `json:"p_adj,omitempty"`
}

// ExperimentArmResult is one arm's tasks.
type ExperimentArmResult struct {
	ExperimentArm
	Weight   int              `json:"weight"`
	Assigned int              `json:"assigned"`
	Exposed  int              `json:"exposed"`
	Measured int              `json:"measured"` // exposed with usage put on them
	Done     int              `json:"done"`     // of the exposed
	DoneRate *float64         `json:"done_rate"`
	DoneP    *float64         `json:"done_p,omitempty"` // two-proportion test against the control
	InScope  int              `json:"in_scope"`
	Stats    []ExperimentStat `json:"stats"`
}

// ExperimentResults compares a run's arms.
type ExperimentResults struct {
	Experiment string                `json:"experiment"`
	Title      string                `json:"title"`
	Run        *ExperimentRun        `json:"run"`
	Scope      string                `json:"scope"` // done (finished tasks) or all (every measured one)
	Primary    string                `json:"primary"`
	Metrics    []ExperimentMetric    `json:"metrics"`
	Arms       []ExperimentArmResult `json:"arms"`
	// SRM is the p-value of the assignment matching the weights: very
	// small means randomisation broke and nothing else here can be trusted.
	SRM        *float64 `json:"srm"`
	ExposedSRM *float64 `json:"exposed_srm"`
	// MDE is the smallest change of the primary metric, as a fraction, this
	// run could see (80% power, 5% two-sided); PerArm the finished tasks per
	// arm it would take to see a 20% change.
	MDE     *float64  `json:"mde,omitempty"`
	PerArm  int       `json:"per_arm,omitempty"`
	Verdict string    `json:"verdict"`
	Kind    string    `json:"kind"` // few, srm, difference, none
	Interim bool      `json:"interim"`
	Notes   []string  `json:"notes"`
	Caveats []string  `json:"caveats"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
}

const (
	experimentMinN      = 10 // tasks per arm before anything is said
	experimentBootstrap = 1000
)

// ExperimentResults compares the arms of a run (0: the live run, else the
// latest). scope is done (default) or all.
func (s *Service) ExperimentResults(ctx context.Context, actor *Actor, key string, runID int64, scope string) (*ExperimentResults, error) {
	if err := actor.canReadExperiments(); err != nil {
		return nil, err
	}
	d, ok := experimentDef(key)
	if !ok {
		return nil, notFound("experiment " + key)
	}
	var run *ExperimentRun
	if runID != 0 {
		r, err := s.experimentRun(ctx, runID)
		if err != nil {
			return nil, err
		}
		if r.Experiment != key {
			return nil, notFound(fmt.Sprintf("run %d of %s", runID, key))
		}
		run = r
	} else {
		runs, err := s.experimentRuns(ctx, "r.experiment = ?", key)
		if err != nil {
			return nil, err
		}
		if len(runs) == 0 {
			return nil, notFound(key + " run")
		}
		run = runs[0]
	}
	switch scope {
	case "":
		scope = "done"
	case "done", "all":
	default:
		return nil, invalid("scope is done or all")
	}
	tasks, err := s.experimentTasks(ctx, actor, run)
	if err != nil {
		return nil, err
	}
	return experimentCompare(d, run, tasks, scope, s.now()), nil
}

// experimentTasks is each exposed unit of a run, by arm, with its measures.
func (s *Service) experimentTasks(ctx context.Context, actor *Actor, run *ExperimentRun) (map[string][]*expTask, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT u.issue_id, u.arm, p.key || '-' || i.number, st.category FROM experiment_units u
		JOIN issues i ON i.id = u.issue_id JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE u.run_id = ? AND u.exposed_at IS NOT NULL`, run.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[string]*expTask{}
	byID := map[int64]*expTask{}
	out := map[string][]*expTask{}
	for rows.Next() {
		var id int64
		var arm, k, cat string
		if err := rows.Scan(&id, &arm, &k, &cat); err != nil {
			return nil, err
		}
		t := &expTask{key: k, done: cat == CatDone}
		byKey[k], byID[id] = t, t
		out[arm] = append(out[arm], t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(byKey) == 0 {
		return out, nil
	}
	// Everyone's usage from the run's first day to now: work on a task
	// goes on after the run stops, so a stopped run is still measured to
	// today (its units are fixed).
	days := int(math.Ceil(float64(s.nowMS()-run.StartedAt.UnixMilli())/float64(24*hourMillis))) + 1
	tr, err := s.UsageTrends(ctx, actor, UsageQuery{Days: min(days, 365), User: "all", allTasks: true})
	if err != nil {
		return nil, err
	}
	for _, r := range tr.Tasks {
		if t, ok := byKey[r.Key]; ok {
			t.row, t.measured = r, true
			t.done = r.Done || t.done
		}
	}
	if err := s.experimentReviewMeasures(ctx, byID); err != nil {
		return nil, err
	}
	return out, nil
}

// experimentReviewMeasures adds how each task held up: its pull requests'
// review findings (not withdrawn), other review comments and babysit rounds,
// what its own reviews found, and the tasks that follow it.
func (s *Service) experimentReviewMeasures(ctx context.Context, byID map[int64]*expTask) error {
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if err := s.experimentLaterMeasures(ctx, byID, args); err != nil {
			return err
		}
		// #nosec G202 -- only placeholders are joined in
		rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT el.issue_id, p.id,
				(SELECT count(*) FROM bot_findings f WHERE f.pr_id = p.id AND f.state != 'withdrawn'),
				(SELECT coalesce(max(b.round), 0) FROM babysits b WHERE b.pr_id = p.id),
				(SELECT count(*) FROM review_comments c WHERE c.pr_id = p.id AND c.finding_id IS NULL AND c.kind != 'finding' AND c.gone_at IS NULL)
			FROM external_links el JOIN pull_requests p ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
			WHERE el.issue_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, prID int64 // prID keeps DISTINCT per pull request
			var f, r, c float64
			if err := rows.Scan(&id, &prID, &f, &r, &c); err != nil {
				rows.Close()
				return err
			}
			byID[id].findings += f
			byID[id].rounds += r
			byID[id].prComments += c
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// experimentLaterMeasures adds, for the tasks whose ids are args, what came
// back about each: follow-up tasks and review findings.
func (s *Service) experimentLaterMeasures(ctx context.Context, byID map[int64]*expTask, args []any) error {
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	for _, m := range []struct {
		query string // yields the task's id and two numbers
		first []any  // its arguments before the ids
		add   func(t *expTask, a, b float64)
	}{
		// Not Waypoint's own guess from files two tasks both changed.
		{`SELECT target_id, count(*), coalesce(sum(subkind = 'fix'), 0) FROM issue_relations
			WHERE kind = 'follows' AND coalesce(found, '') != ? AND target_id IN ` + in + ` GROUP BY target_id`, []any{relationFoundFiles},
			func(t *expTask, all, fixes float64) { t.followups, t.fixes = all, fixes }},
		{`SELECT issue_id, sum(found), 0 FROM work_log
			WHERE kind = ? AND reason != ? AND found IS NOT NULL AND issue_id IN ` + in + ` GROUP BY issue_id`,
			[]any{WorkOwnReview, ownReviewPanel},
			func(t *expTask, found, _ float64) { t.selfReview, t.selfReviewed = found, true }},
		// The measured counts of WorkLogTotals: none a model read.
		{`SELECT issue_id, sum(found), 0 FROM work_log
			WHERE kind = ? AND coalesce(read_by, '') IN ('', ?) AND found IS NOT NULL AND issue_id IN ` + in + ` GROUP BY issue_id`,
			[]any{WorkReview, readByRules},
			func(t *expTask, found, _ float64) { t.reviewFound, t.reviewed = found, true }},
	} {
		rows, err := s.db.R.QueryContext(ctx, m.query, append(m.first, args...)...) // #nosec G202 -- only placeholders are joined in
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var a, b float64
			if err := rows.Scan(&id, &a, &b); err != nil {
				rows.Close()
				return err
			}
			m.add(byID[id], a, b)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func experimentCompare(d *ExperimentDef, run *ExperimentRun, tasks map[string][]*expTask, scope string, now time.Time) *ExperimentResults {
	o := &ExperimentResults{Experiment: d.Key, Title: d.Title, Run: run, Scope: scope, Primary: d.Primary, Notes: []string{},
		Caveats: append(slices.Clone(d.Caveats), experimentCaveats...), From: run.StartedAt, To: now.UTC()}
	if run.StoppedAt != nil {
		o.To = *run.StoppedAt
	}
	for _, m := range experimentMetrics {
		o.Metrics = append(o.Metrics, m.ExperimentMetric)
	}
	inScope := func(t *expTask) bool { return t.measured && (scope == "all" || t.done) }
	values := func(arm string, mi int) []float64 {
		var v []float64
		for _, t := range tasks[arm] {
			if !inScope(t) {
				continue
			}
			if x, ok := experimentMetrics[mi].get(t); ok {
				v = append(v, x)
			}
		}
		return v
	}
	ctl := d.control()
	// Resampling for an interval, seeded by the run so a page shows the
	// same numbers twice.
	rng := rand.New(rand.NewPCG(uint64(run.ID), 0x5eed)) // #nosec G404 G115 -- not for secrets; run ids are positive
	type pref struct{ arm, metric int }
	var ps []float64
	var pAt []pref
	for ai, a := range d.Arms {
		r := ExperimentArmResult{ExperimentArm: a, Weight: run.Weights[a.Key], Assigned: run.Assigned[a.Key], Exposed: run.Exposed[a.Key]}
		for _, t := range tasks[a.Key] {
			if t.measured {
				r.Measured++
			}
			if t.done {
				r.Done++
			}
			if inScope(t) {
				r.InScope++
			}
		}
		if n := len(tasks[a.Key]); n > 0 {
			r.DoneRate = f64p(float64(r.Done) / float64(n))
		}
		for mi, m := range experimentMetrics {
			v := values(a.Key, mi)
			st := ExperimentStat{Key: m.Key, N: len(v)}
			if len(v) > 0 {
				st.Median, st.Mean = f64p(median(v)), f64p(mean(v))
			}
			if a.Key != ctl {
				c := values(ctl, mi)
				if len(c) > 0 && len(v) > 0 {
					if cm := median(c); cm > 0 {
						st.Ratio = f64p(median(v) / cm)
						if len(c) >= experimentMinN/2 && len(v) >= experimentMinN/2 {
							st.Low, st.High = bootstrapRatio(rng, v, c)
						}
					}
					if p := mannWhitney(v, c); p != nil {
						st.P = p
						ps, pAt = append(ps, *p), append(pAt, pref{ai, mi})
					}
				}
			}
			r.Stats = append(r.Stats, st)
		}
		o.Arms = append(o.Arms, r)
	}
	for i, p := range holm(ps) {
		o.Arms[pAt[i].arm].Stats[pAt[i].metric].PAdj = f64p(p)
	}
	// Done rates against the control's.
	c := &o.Arms[0]
	for i := 1; i < len(o.Arms); i++ {
		a := &o.Arms[i]
		o.Arms[i].DoneP = twoProportion(a.Done, len(tasks[a.Key]), c.Done, len(tasks[ctl]))
	}
	// Sample ratio: assigned and exposed counts against the weights.
	var w, assigned, exposed []float64
	for _, a := range o.Arms {
		if a.Weight > 0 {
			w, assigned, exposed = append(w, float64(a.Weight)), append(assigned, float64(a.Assigned)), append(exposed, float64(a.Exposed))
		}
	}
	o.SRM, o.ExposedSRM = srmP(assigned, w), srmP(exposed, w)

	// The primary metric's detectable change.
	pi := metricIndex(d.Primary)
	if pi >= 0 && len(o.Arms) > 1 {
		cv := values(ctl, pi)
		var tv []float64
		for _, a := range d.Arms[1:] {
			if run.Weights[a.Key] > 0 {
				tv = values(a.Key, pi)
				break
			}
		}
		if sd, ok := logSD(append(slices.Clone(cv), tv...)); ok && len(cv) > 1 && len(tv) > 1 {
			o.MDE = f64p(math.Exp(2.8*sd*math.Sqrt(1/float64(len(cv))+1/float64(len(tv)))) - 1)
			o.PerArm = int(math.Ceil(2 * math.Pow(2.8*sd/math.Log(1.2), 2)))
		}
	}
	experimentVerdict(o, d, run, pi)
	return o
}

func experimentVerdict(o *ExperimentResults, d *ExperimentDef, run *ExperimentRun, pi int) {
	short := []string{}
	for _, a := range o.Arms {
		if a.Weight > 0 && a.InScope < experimentMinN {
			short = append(short, fmt.Sprintf("%s has %d", a.Label, a.InScope))
		}
	}
	live := run.StoppedAt == nil
	if live && run.Target > 0 {
		for _, a := range o.Arms {
			if a.Weight > 0 && a.Done < run.Target {
				o.Interim = true
			}
		}
	}
	if o.Interim {
		o.Notes = append(o.Notes, "An early look: the run hasn't reached its target. Stopping as soon as a difference shows inflates false positives, so decide at the target or the end date.")
	}
	for i := 1; i < len(o.Arms); i++ {
		a := o.Arms[i]
		if a.DoneP != nil && *a.DoneP < 0.1 && a.DoneRate != nil && o.Arms[0].DoneRate != nil {
			o.Notes = append(o.Notes, fmt.Sprintf("Done rates differ (%s %.0f%%, %s %.0f%%, p=%.2g): comparing only finished tasks is biased, so read the all scope too, and count the done rate as a result.",
				o.Arms[0].Label, 100**o.Arms[0].DoneRate, a.Label, 100**a.DoneRate, *a.DoneP))
		}
	}
	if o.ExposedSRM != nil && *o.ExposedSRM < 0.001 && (o.SRM == nil || *o.SRM >= 0.001) {
		o.Notes = append(o.Notes, "The arms reach the point they differ at in different numbers: something about an arm decides whether its tasks get there, so exposed tasks aren't comparable. Look at why before trusting the rest.")
	}
	switch {
	case o.SRM != nil && *o.SRM < 0.001:
		o.Kind = "srm"
		o.Verdict = fmt.Sprintf("Assignment doesn't match the weights (p=%.2g): tasks reached the arms unevenly, so the comparison can't be trusted. Look for an arm that some tasks can't get into, or a version of wp that doesn't ask.", *o.SRM)
	case len(short) > 0:
		o.Kind = "few"
		o.Verdict = fmt.Sprintf("Too few tasks to say anything yet (%s; %d each needed).", strings.Join(short, ", "), experimentMinN)
	case pi < 0:
		o.Kind = "none"
		o.Verdict = "No primary metric."
	default:
		m := experimentMetrics[pi]
		var diff, same []string
		for _, a := range o.Arms[1:] {
			if a.Weight == 0 {
				continue
			}
			st := a.Stats[pi]
			if st.Ratio == nil {
				continue
			}
			ci := ""
			if st.Low != nil && st.High != nil {
				ci = fmt.Sprintf(" (95%% interval %s to %s)", pctChange(*st.Low), pctChange(*st.High))
			}
			if st.PAdj != nil && *st.PAdj < 0.05 {
				diff = append(diff, fmt.Sprintf("%s: median %s %s than %s%s, p=%.2g after correction", a.Label, strings.ToLower(m.Label), moreLess(*st.Ratio), o.Arms[0].Label, ci, *st.PAdj))
			} else {
				same = append(same, fmt.Sprintf("%s: %s%s", a.Label, pctChange(*st.Ratio), ci))
			}
		}
		if len(diff) > 0 {
			o.Kind = "difference"
			o.Verdict = strings.Join(diff, "; ") + "."
		} else {
			o.Kind = "none"
			o.Verdict = "No difference in " + strings.ToLower(m.Label) + " the data can tell from chance"
			if len(same) > 0 {
				o.Verdict += " (" + strings.Join(same, "; ") + ")"
			}
			if o.MDE != nil {
				o.Verdict += fmt.Sprintf(". With this many tasks only a change of about %.0f%% or more would show", 100**o.MDE)
				if o.PerArm > 0 {
					o.Verdict += fmt.Sprintf("; seeing 20%% takes about %d finished tasks per arm", o.PerArm)
				}
			}
			o.Verdict += ". No difference found is not the same as no difference."
		}
	}
}

func metricIndex(key string) int {
	for i, m := range experimentMetrics {
		if m.Key == key {
			return i
		}
	}
	return -1
}

func pctChange(r float64) string {
	return fmt.Sprintf("%+.0f%%", 100*(r-1))
}

func moreLess(r float64) string {
	if r < 1 {
		return fmt.Sprintf("%.0f%% lower", 100*(1-r))
	}
	return fmt.Sprintf("%.0f%% higher", 100*(r-1))
}

// bootstrapRatio is a 95% percentile interval of median(a)/median(b).
func bootstrapRatio(rng *rand.Rand, a, b []float64) (*float64, *float64) {
	rs := make([]float64, 0, experimentBootstrap)
	ra, rb := make([]float64, len(a)), make([]float64, len(b))
	for range experimentBootstrap {
		for i := range ra {
			ra[i] = a[rng.IntN(len(a))]
		}
		for i := range rb {
			rb[i] = b[rng.IntN(len(b))]
		}
		if mb := median(rb); mb > 0 {
			rs = append(rs, median(ra)/mb)
		}
	}
	if len(rs) < experimentBootstrap/2 {
		return nil, nil
	}
	sort.Float64s(rs)
	lo, hi := rs[int(0.025*float64(len(rs)))], rs[min(len(rs)-1, int(0.975*float64(len(rs))))]
	return &lo, &hi
}

// holm adjusts p-values for multiple comparisons (Holm-Bonferroni),
// keeping their order.
func holm(ps []float64) []float64 {
	n := len(ps)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return ps[idx[i]] < ps[idx[j]] })
	out := make([]float64, n)
	prev := 0.0
	for rank, i := range idx {
		adj := math.Min(1, ps[i]*float64(n-rank))
		adj = math.Max(adj, prev)
		out[i], prev = adj, adj
	}
	return out
}

// twoProportion is the two-sided p-value of x1/n1 and x2/n2 differing.
func twoProportion(x1, n1, x2, n2 int) *float64 {
	if n1 == 0 || n2 == 0 {
		return nil
	}
	p := float64(x1+x2) / float64(n1+n2)
	se := math.Sqrt(p * (1 - p) * (1/float64(n1) + 1/float64(n2)))
	if se == 0 {
		return nil
	}
	z := (float64(x1)/float64(n1) - float64(x2)/float64(n2)) / se
	return f64p(math.Erfc(math.Abs(z) / math.Sqrt2))
}

// srmP is the chi-square p-value of counts matching weights.
func srmP(counts, weights []float64) *float64 {
	if len(counts) < 2 {
		return nil
	}
	var total, wsum float64
	for i := range counts {
		total += counts[i]
		wsum += weights[i]
	}
	if total < 20 || wsum == 0 {
		return nil
	}
	chi := 0.0
	for i := range counts {
		e := total * weights[i] / wsum
		chi += (counts[i] - e) * (counts[i] - e) / e
	}
	return f64p(gammaQ(float64(len(counts)-1)/2, chi/2))
}

// gammaQ is the regularised upper incomplete gamma function Q(a, x)
// (Numerical Recipes' series and continued fraction).
func gammaQ(a, x float64) float64 {
	if x <= 0 {
		return 1
	}
	lg, _ := math.Lgamma(a)
	if x < a+1 {
		sum, del, ap := 1/a, 1/a, a
		for range 500 {
			ap++
			del *= x / ap
			sum += del
			if math.Abs(del) < math.Abs(sum)*1e-14 {
				break
			}
		}
		return 1 - sum*math.Exp(-x+a*math.Log(x)-lg)
	}
	const tiny = 1e-300
	b := x + 1 - a
	c, dd := 1/tiny, 1/b
	h := dd
	for i := 1; i <= 500; i++ {
		an := -float64(i) * (float64(i) - a)
		b += 2
		dd = an*dd + b
		if math.Abs(dd) < tiny {
			dd = tiny
		}
		c = b + an/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		dd = 1 / dd
		del := dd * c
		h *= del
		if math.Abs(del-1) < 1e-14 {
			break
		}
	}
	return math.Exp(-x+a*math.Log(x)-lg) * h
}

// logSD is the standard deviation of log(x) over the positive values.
func logSD(v []float64) (float64, bool) {
	var l []float64
	for _, x := range v {
		if x > 0 {
			l = append(l, math.Log(x))
		}
	}
	if len(l) < 3 {
		return 0, false
	}
	m := mean(l)
	ss := 0.0
	for _, x := range l {
		ss += (x - m) * (x - m)
	}
	sd := math.Sqrt(ss / float64(len(l)-1))
	return sd, sd > 0
}
