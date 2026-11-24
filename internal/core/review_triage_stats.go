package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
)

// Outcome labels are read from where they live when asked for, so they keep arriving after the row is written.

// TriageRuleStats is one floor or rule's record.
type TriageRuleStats struct {
	Rule   string `json:"rule"`
	Active bool   `json:"active,omitempty"` // a rule that skips
	Shadow bool   `json:"shadow,omitempty"` // a rule that only records
	Floor  bool   `json:"floor,omitempty"`
	TriageTotals
}

// TriageTotals are the counts for a rule, a project or everything.
type TriageTotals struct {
	Assessed  int `json:"assessed"`
	WouldSkip int `json:"would_skip"`
	Skipped   int `json:"skipped"`    // no review ran
	AgentSkip int `json:"agent_skip"` // of those, wp ship --skip-review
	OwnReview int `json:"own_review"` // of those, rounds from before WP-1320 skipped because the session reviewed it itself (WP-1089); no new round is counted here
	Reviewed  int `json:"reviewed"`
	OwnRound  int `json:"own_round"`   // of those, rounds the session reviewed itself: its report is the round (WP-1320; a round since then is reviewed, not skipped)
	Holdout   int `json:"holdout"`     // reviewed though an active rule would skip
	ShadowRun int `json:"shadow_runs"` // reviewed; a shadow rule or mode would skip

	// What every review found, whatever sent the change to it.
	Counted  int `json:"counted"`
	FoundAny int `json:"found_any"`
	Found    int `json:"found"`
	Fixed    int `json:"fixed"`
	Rejected int `json:"rejected"`

	// The rules' misses: what the sampled reviews found (triageSampled).
	Sampled         int `json:"sampled"`
	SampledFoundAny int `json:"sampled_found_any"`
	SampledFound    int `json:"sampled_found"`

	// Later evidence on the pull request of the branch assessed, counted
	// once per branch.
	Useful         int `json:"useful_findings"`
	UsefulOnSkip   int `json:"useful_findings_on_skipped"`
	Escapes        int `json:"escapes"`
	EscapesOnSkips int `json:"escapes_on_skipped"`
}

// TriageAvoided is the estimated saving. Tokens can't be attributed to one
// round (usage is hourly per session), so it counts what would have run.
type TriageAvoided struct {
	Rounds           int     `json:"rounds"`
	MeanRoundMinutes float64 `json:"mean_round_minutes"`
	Minutes          float64 `json:"minutes"`
	AuditsPerRound   float64 `json:"audits_per_round"`
	GrokAudits       float64 `json:"grok_audits"`
}

// TriageExport is one row with its labels, for assessing the rules or
// training a model.
type TriageExport struct {
	ID        int64           `json:"id"`
	Task      string          `json:"task"`
	Repo      string          `json:"repo"`
	Head      string          `json:"head"`
	Rule      string          `json:"rule"`
	WouldSkip bool            `json:"would_skip"`
	Action    string          `json:"action"`
	Why       string          `json:"why,omitempty"`
	Version   string          `json:"version"`
	At        int64           `json:"at"`
	Features  json.RawMessage `json:"features"`

	RoundState string `json:"round_state,omitempty"`
	Found      *int64 `json:"found,omitempty"`
	Fixed      *int64 `json:"fixed,omitempty"`
	Rejected   *int64 `json:"rejected,omitempty"`
	Useful     int    `json:"useful_findings"`
	Escapes    int    `json:"escapes"`
}

// ReviewTriageStats is GET /worklog/triage.
type ReviewTriageStats struct {
	Days    int                `json:"days"`
	Version string             `json:"version"`
	Rules   []*TriageRuleStats `json:"rules"`
	Total   TriageTotals       `json:"total"`
	Avoided TriageAvoided      `json:"avoided"`
	Rows    []*TriageExport    `json:"rows,omitempty"`
	// Truncated: more rows than triageRowLimit; the totals still count them.
	Truncated bool `json:"truncated,omitempty"`
}

// triageRowLimit bounds an export.
const triageRowLimit = 5000

// GetReviewTriageStats reports triage for the projects the actor can see in
// full. withRows adds the rows and their labels.
func (s *Service) GetReviewTriageStats(ctx context.Context, actor *Actor, projectRef string, days int, withRows bool) (*ReviewTriageStats, error) {
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		return nil, invalid("days must be 1-365")
	}
	var projects []*Project
	if projectRef != "" {
		p, err := s.GetProject(ctx, actor, projectRef)
		if err != nil {
			return nil, err
		}
		projects = []*Project{p}
	} else {
		all, err := s.ListProjects(ctx, actor, false)
		if err != nil {
			return nil, err
		}
		projects = all
	}
	out := &ReviewTriageStats{Days: days, Version: TriageVersion, Rules: []*TriageRuleStats{}}
	since := s.nowMS() - int64(days)*24*3600*1000
	rules := map[string]*TriageRuleStats{}
	var ids []int64
	for _, p := range projects {
		if p.Role == "guest" {
			continue // a guest sees shared issues, not the project's numbers
		}
		ids = append(ids, p.ID)
		if err := s.triageProject(ctx, p, since, withRows, rules, out); err != nil {
			return nil, err
		}
	}
	for _, r := range rules {
		out.Rules = append(out.Rules, r)
	}
	slices.SortFunc(out.Rules, func(a, b *TriageRuleStats) int {
		if c := b.Assessed - a.Assessed; c != 0 {
			return c
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	if err := s.triageAvoided(ctx, ids, since, out); err != nil {
		return nil, err
	}
	return out, nil
}

func newTriageRuleStats(rule string) *TriageRuleStats {
	r := &TriageRuleStats{Rule: rule, Floor: strings.HasPrefix(rule, "floor:")}
	for _, t := range triageRules {
		if t.name == rule {
			r.Active, r.Shadow = t.active, !t.active
		}
	}
	return r
}

func (s *Service) triageProject(ctx context.Context, p *Project, since int64, withRows bool, rules map[string]*TriageRuleStats, out *ReviewTriageStats) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT t.id, t.issue_id, t.repo, t.head, t.rule, t.would_skip, t.action, t.why, t.version, t.features, t.created_at,
			coalesce(w.state, ''), w.found, w.fixed, w.rejected, i.number
		FROM review_triage t JOIN issues i ON i.id = t.issue_id LEFT JOIN work_log w ON w.id = t.round_id
		WHERE t.project_id = ? AND t.created_at >= ? AND t.why != ? ORDER BY t.id DESC`, p.ID, since, triageWhySuperseded)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		e      TriageExport
		issue  int64
		number int64
	}
	var all []*row
	for rows.Next() {
		var r row
		var would int
		var features string
		var found, fixed, rejected sql.NullInt64
		if err := rows.Scan(&r.e.ID, &r.issue, &r.e.Repo, &r.e.Head, &r.e.Rule, &would, &r.e.Action, &r.e.Why, &r.e.Version, &features, &r.e.At,
			&r.e.RoundState, &found, &fixed, &rejected, &r.number); err != nil {
			return err
		}
		r.e.WouldSkip = would != 0
		r.e.Features = json.RawMessage(features)
		r.e.Found, r.e.Fixed, r.e.Rejected = triageCount(found), triageCount(fixed), triageCount(rejected)
		r.e.Task = p.Key + "-" + itoa64(r.number)
		all = append(all, &r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()
	// A branch shipped twice has two rows: its later evidence counts once,
	// on the newest (the rows are newest first).
	type branch struct {
		issue      int64
		repo, head string
	}
	seen := map[branch]bool{}
	for _, r := range all {
		useful, escapes, err := s.triageEvidence(ctx, r.issue, r.e.Repo, r.e.Head)
		if err != nil {
			return err
		}
		r.e.Useful, r.e.Escapes = useful, escapes
		b := branch{r.issue, strings.ToLower(r.e.Repo), r.e.Head}
		first := !seen[b]
		seen[b] = true
		st := rules[r.e.Rule]
		if st == nil {
			st = newTriageRuleStats(r.e.Rule)
			rules[r.e.Rule] = st
		}
		st.add(&r.e, first)
		out.Total.add(&r.e, first)
		switch {
		case !withRows:
		case len(out.Rows) >= triageRowLimit:
			out.Truncated = true
		default:
			e := r.e
			out.Rows = append(out.Rows, &e)
		}
	}
	return nil
}

// triageCount is a nullable count as JSON wants it.
func triageCount(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

func (t *TriageTotals) add(e *TriageExport, first bool) {
	t.Assessed++
	if e.WouldSkip {
		t.WouldSkip++
	}
	if e.Action == triageSkip {
		t.Skipped++
		switch e.Why {
		case triageWhyAgentSkip:
			t.AgentSkip++
		case triageWhyOwnReview:
			t.OwnReview++
		}
	} else {
		t.Reviewed++
		if e.Why == triageWhyOwnReview {
			t.OwnRound++
		}
	}
	switch e.Why {
	case triageWhyHoldout:
		t.Holdout++
	case triageWhyShadowRule, triageWhyModeShadow:
		t.ShadowRun++
	}
	if e.Found != nil {
		t.Counted++
		if *e.Found > 0 {
			t.FoundAny++
		}
		t.Found += int(*e.Found)
		if e.Fixed != nil {
			t.Fixed += int(*e.Fixed)
		}
		if e.Rejected != nil {
			t.Rejected += int(*e.Rejected)
		}
		if triageSampled(e) {
			t.Sampled++
			t.SampledFound += int(*e.Found)
			if *e.Found > 0 {
				t.SampledFoundAny++
			}
		}
	}
	if first {
		t.Useful += e.Useful
		t.Escapes += e.Escapes
		if e.Action == triageSkip {
			t.UsefulOnSkip += e.Useful
			t.EscapesOnSkips += e.Escapes
		}
	}
}

// triageSampled is a review of a change a rule would have skipped, with
// nothing about the change having sent it there, so what it found is what
// skipping would have missed. A wp ship --review isn't one: the agent asked
// because something looked wrong.
func triageSampled(e *TriageExport) bool {
	if !e.WouldSkip || e.Action == triageSkip {
		return false
	}
	switch e.Why {
	case triageWhyHoldout, triageWhyShadowRule, triageWhyModeShadow, triageWhyModeOff:
		return true
	}
	return false
}

// triageEvidence counts the useful findings on the pull requests of the
// branch assessed and the escapes traced back to them. The task's other
// branches are other changes.
func (s *Service) triageEvidence(ctx context.Context, issueID int64, repo, head string) (useful, escapes int, err error) {
	const prs = `SELECT p.id FROM pull_requests p JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number)) WHERE el.issue_id = ? AND p.repo = ? COLLATE NOCASE AND p.head_ref = ?`
	branch := []any{issueID, repo, head}
	args := append(append([]any{}, branch...), findingUsefulArgs()...)
	if err = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM bot_findings f WHERE f.pr_id IN (`+prs+`) AND `+findingUseful, args...).Scan(&useful); err != nil {
		return 0, 0, err
	}
	err = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM escapes e WHERE e.intro_pr_id IN (`+prs+`)`, branch...).Scan(&escapes)
	return useful, escapes, err
}

// triageAvoided estimates what the skips saved, from how long these
// projects' reviews took and how often one had a Grok audit. Only rule
// skips count: an agent's --skip-review wasn't triage's doing, nor was a round
// skipped for the session's own review (only rounds from before WP-1320).
func (s *Service) triageAvoided(ctx context.Context, projects []int64, since int64, out *ReviewTriageStats) error {
	out.Avoided.Rounds = out.Total.Skipped - out.Total.AgentSkip - out.Total.OwnReview
	if len(projects) == 0 {
		return nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(projects)), ",")
	args := make([]any, 0, len(projects)+1)
	for _, p := range projects {
		args = append(args, p)
	}
	args = append(args, since)
	var mean sql.NullFloat64
	var rounds, audited int
	// #nosec G202 -- only "?" placeholders are joined in
	err := s.db.R.QueryRowContext(ctx, `SELECT avg(CASE WHEN w.started_at IS NOT NULL AND w.ended_at IS NOT NULL THEN w.ended_at - w.started_at END) / 60000.0, count(*), count(w.audit_id)
		FROM work_log w JOIN issues i ON i.id = w.issue_id WHERE i.project_id IN (`+in+`) AND w.kind = 'review' AND w.state = 'done' AND w.created_at >= ?`, args...).Scan(&mean, &rounds, &audited)
	if err != nil {
		return err
	}
	out.Avoided.MeanRoundMinutes = mean.Float64
	out.Avoided.Minutes = float64(out.Avoided.Rounds) * mean.Float64
	if rounds > 0 {
		out.Avoided.AuditsPerRound = float64(audited) / float64(rounds)
		out.Avoided.GrokAudits = float64(out.Avoided.Rounds) * out.Avoided.AuditsPerRound
	}
	return nil
}
