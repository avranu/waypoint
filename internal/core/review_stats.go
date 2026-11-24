package core

// Findings dropped before posting are never judged; a leftover audit's verdict decides the rest (confirmed or fixed useful, rejected false).

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"
)

const (
	reviewStatsDays    = 30
	reviewStatsMaxDays = 730
	// statsQ: the interval is the 5% to 95% quantiles of Beta(1+useful,
	// 1+false), a 90% interval with a uniform prior, as arms bound theirs.
	statsQ = 0.05
)

// ReviewStatsInput says which findings to count.
type ReviewStatsInput struct {
	Project string // a project key; "" every project (admins)
	Days    int    // findings raised in the past Days days; 0 the default
}

// ReviewStats is how the review bot's findings held up.
type ReviewStats struct {
	Project string    `json:"project,omitempty"`
	Days    int       `json:"days"`
	Since   time.Time `json:"since"`
	// Totals: every finding of the window, as reviewers count them.
	Totals    ReviewerStat     `json:"totals"`
	Reviewers []ReviewerStat   `json:"reviewers"`
	Verifiers []VerifierStat   `json:"verifiers"`
	Verify    ReviewVerifyStat `json:"verify"`
}

// Judged is how a set of posted findings fared, and the precision it shows.
type Judged struct {
	Useful   int `json:"useful"`
	False    int `json:"false"`
	Unjudged int `json:"unjudged"`
	// Precision is useful / (useful + false), nil before any is judged;
	// Low and High its 90% interval.
	Precision *float64 `json:"precision"`
	Low       *float64 `json:"low"`
	High      *float64 `json:"high"`
}

// ReviewerStat is one reviewer model's findings with one strategy and
// prompt version.
type ReviewerStat struct {
	// Model is the normalized name (normalizeModel): the same weights from
	// any provider; Providers what served it.
	Model         string   `json:"model,omitempty"`
	Providers     []string `json:"providers,omitempty"`
	Strategy      string   `json:"strategy,omitempty"`       // "" before WP-363 recorded it
	PromptVersion string   `json:"prompt_version,omitempty"` // likewise
	Findings      int      `json:"findings"`                 // all it raised, posted or not
	Judged
	Evidence int `json:"evidence_dropped"` // dropped by the evidence check
	Withheld int `json:"withheld"`         // dropped by the verifier and not posted
	// Tokens the reviewer spent (its share of each run's review tokens),
	// and per useful finding (0 with none).
	Tokens          int64 `json:"tokens"`
	TokensPerUseful int64 `json:"tokens_per_useful"`
	Runs            int   `json:"runs"`
}

// VerifierStat is one verifier model's verdicts and how the findings fared.
type VerifierStat struct {
	Model     string   `json:"model"`
	Providers []string `json:"providers,omitempty"`
	// Kept: findings it kept or corrected, posted.
	Kept Judged `json:"kept"`
	// Dropped: every finding it dropped; Posted those posted anyway to
	// check it, and how they fared. A drop was right when the finding
	// turned out false: DropAccuracy is false / judged of Posted (nil
	// before one is judged), DropLow and DropHigh its 90% interval.
	Dropped      int      `json:"dropped"`
	Posted       Judged   `json:"posted"`
	DropAccuracy *float64 `json:"drop_accuracy"`
	DropLow      *float64 `json:"drop_low"`
	DropHigh     *float64 `json:"drop_high"`
	Tokens       int64    `json:"tokens"`
	Runs         int      `json:"runs"`
}

// ReviewVerifyStat sets the findings the verifier kept against those of
// runs whose arm had no verifier.
type ReviewVerifyStat struct {
	Verified ReviewArmClass `json:"verified"`
	Skipped  ReviewArmClass `json:"skipped"`
}

// ReviewArmClass is the findings and runs of one side.
type ReviewArmClass struct {
	Findings int `json:"findings"`
	Judged
	Runs            int   `json:"runs"`
	Tokens          int64 `json:"tokens"` // review and verify tokens of its runs
	TokensPerUseful int64 `json:"tokens_per_useful"`
}

// ReviewStats counts how the review bot's findings held up. Every project
// is for admins; one project for those who can see it.
func (s *Service) ReviewStats(ctx context.Context, actor *Actor, in ReviewStatsInput) (*ReviewStats, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	days := in.Days
	if days <= 0 {
		days = reviewStatsDays
	}
	if days > reviewStatsMaxDays {
		return nil, invalid("days: at most %d", reviewStatsMaxDays)
	}
	var projectID int64
	if in.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, in.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		projectID = p.ID
	} else if !actor.isAdmin() {
		return nil, forbidden("review stats across every project are for admins: name a project")
	}
	since := s.now().Add(-time.Duration(days) * 24 * time.Hour)
	fs, runs, err := s.reviewStatRows(ctx, projectID, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := tallyReviewStats(fs, runs)
	out.Project, out.Days, out.Since = strings.ToUpper(in.Project), days, since
	return out, nil
}

// statFinding is one finding as stats read it.
type statFinding struct {
	run                   int64
	model, verifier       string
	strategy, version     string
	state, verify, reason string
	taken                 bool
	audit                 string
}

// statRun is one review run as stats read it.
type statRun struct {
	id                int64
	strategy, version string
	arm               string
	verified          bool
	reviewers         []string
	review, verify    int64
}

func (s *Service) reviewStatRows(ctx context.Context, projectID, since int64) ([]statFinding, []statRun, error) {
	const where = `r.kind = 'review' AND r.status = 'done' AND r.created_at >= ? AND (? = 0 OR r.project_id = ?)`
	args := []any{since, projectID, projectID}
	rows, err := s.db.R.QueryContext(ctx, `
		SELECT r.id, r.strategy, r.prompt_version, r.arm, r.verified, r.reviewer_models,
			0, 0
		FROM bot_runs r WHERE `+where+` ORDER BY r.id`, args...)
	if err != nil {
		return nil, nil, err
	}
	var runs []statRun
	for rows.Next() {
		var r statRun
		var reviewers string
		if err := rows.Scan(&r.id, &r.strategy, &r.version, &r.arm, &r.verified, &reviewers, &r.review, &r.verify); err != nil {
			rows.Close()
			return nil, nil, err
		}
		_ = json.Unmarshal([]byte(reviewers), &r.reviewers)
		runs = append(runs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	fargs := append([]any{VerdictAccepted, VerdictFixing, VerdictFixed, VerdictVerified}, args...)
	rows, err = s.db.R.QueryContext(ctx, `
		SELECT f.run_id, f.model, f.verified_by, f.strategy, f.prompt_version, f.state, f.verify, f.verify_reason,
			EXISTS (SELECT 1 FROM review_comments c WHERE c.finding_id = f.id AND c.verdict IN (?, ?, ?, ?)),
			COALESCE(a.state, '')
		FROM bot_findings f
		JOIN bot_runs r ON r.id = f.run_id
		LEFT JOIN code_audit_findings a ON a.id = f.audit_finding_id
		WHERE `+where+` ORDER BY f.id`, fargs...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var fs []statFinding
	for rows.Next() {
		var f statFinding
		if err := rows.Scan(&f.run, &f.model, &f.verifier, &f.strategy, &f.version, &f.state, &f.verify, &f.reason, &f.taken, &f.audit); err != nil {
			return nil, nil, err
		}
		fs = append(fs, f)
	}
	return fs, runs, rows.Err()
}

// Fates of a finding, as stats count them.
const (
	fateUseful   = "useful"
	fateFalse    = "false"
	fateUnjudged = "unjudged"
	fateEvidence = "evidence"
	fateWithheld = "withheld"
)

// fateOf is how a finding fared.
func fateOf(f statFinding) string {
	switch f.audit {
	case "confirmed", "fixed":
		return fateUseful
	case "rejected":
		return fateFalse
	}
	switch f.state {
	case FindingDropped:
		if f.verify == VerifyUnchecked && f.reason == evidenceMissing {
			return fateEvidence
		}
		return fateWithheld
	case FindingFixed:
		return fateUseful
	case FindingOpen, FindingResolved:
		if f.taken {
			return fateUseful
		}
	case FindingDismissed, FindingWithdrawn:
		return fateFalse
	}
	return fateUnjudged
}

// count adds a posted finding's fate.
func (j *Judged) count(fate string) {
	switch fate {
	case fateUseful:
		j.Useful++
	case fateFalse:
		j.False++
	case fateUnjudged:
		j.Unjudged++
	}
}

// settle sets the precision and its interval from the counts.
func (j *Judged) settle() {
	j.Precision, j.Low, j.High = nil, nil, nil
	n := j.Useful + j.False
	if n == 0 {
		return
	}
	p := round3(float64(j.Useful) / float64(n))
	lo := round3(betaQuantile(statsQ, 1+float64(j.Useful), 1+float64(j.False)))
	hi := round3(betaQuantile(1-statsQ, 1+float64(j.Useful), 1+float64(j.False)))
	j.Precision, j.Low, j.High = &p, &lo, &hi
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// providerOf is the provider part of a served model id: what comes before
// its first "/" ("groq" of "groq/openai/gpt-oss-120b"), "" without one.
func providerOf(id string) string {
	id = strings.TrimSpace(id)
	if n := strings.Index(id, "/"); n > 0 {
		return strings.ToLower(id[:n])
	}
	return ""
}

func addProvider(list []string, id string) []string {
	if p := providerOf(id); p != "" && !slices.Contains(list, p) {
		list = append(list, p)
		slices.Sort(list)
	}
	return list
}

func perUseful(tokens int64, useful int) int64 {
	if useful == 0 {
		return 0
	}
	return int64(math.Round(float64(tokens) / float64(useful)))
}

// tallyReviewStats counts findings and runs into stats.
//
// Tokens: a run's review tokens are shared evenly among its reviewers (the
// models WP-365 records, else those its findings came from), each under
// the strategy and prompt version its findings carry, else the run's; its
// verify tokens among the verifier models that judged its findings. A run
// with no reviewer known (no findings, before WP-365) counts in the totals
// only.
func tallyReviewStats(fs []statFinding, runs []statRun) *ReviewStats {
	out := &ReviewStats{Reviewers: []ReviewerStat{}, Verifiers: []VerifierStat{}}
	type rkey struct{ model, strategy, version string }
	revs := map[rkey]*ReviewerStat{}
	rev := func(k rkey) *ReviewerStat {
		if revs[k] == nil {
			revs[k] = &ReviewerStat{Model: k.model, Strategy: k.strategy, PromptVersion: k.version}
		}
		return revs[k]
	}
	vers := map[string]*VerifierStat{}
	ver := func(m string) *VerifierStat {
		if vers[m] == nil {
			vers[m] = &VerifierStat{Model: m}
		}
		return vers[m]
	}
	type runSeen struct {
		models    []string
		keys      map[string]rkey
		verifiers []string
		skipped   bool
		trusted   bool
	}
	seen := map[int64]*runSeen{}
	for _, f := range fs {
		fate := fateOf(f)
		k := rkey{normalizeModel(f.model), f.strategy, f.version}
		r := rev(k)
		r.Providers = addProvider(r.Providers, f.model)
		r.Findings++
		r.count(fate)
		out.Totals.Findings++
		out.Totals.count(fate)
		switch fate {
		case fateEvidence:
			r.Evidence++
			out.Totals.Evidence++
		case fateWithheld:
			r.Withheld++
			out.Totals.Withheld++
		}
		rs := seen[f.run]
		if rs == nil {
			rs = &runSeen{keys: map[string]rkey{}}
			seen[f.run] = rs
		}
		if f.model != "" && !slices.Contains(rs.models, f.model) {
			rs.models = append(rs.models, f.model)
		}
		if _, ok := rs.keys[k.model]; !ok {
			rs.keys[k.model] = k
		}
		switch f.verify {
		case VerifyKept, VerifyCorrected:
			out.Verify.Verified.Findings++
			out.Verify.Verified.count(fate)
		case VerifySkipped:
			out.Verify.Skipped.Findings++
			out.Verify.Skipped.count(fate)
			rs.skipped = true
		case VerifyTrusted:
			rs.trusted = true
		}
		if f.verifier == "" {
			continue
		}
		vm := normalizeModel(f.verifier)
		v := ver(vm)
		v.Providers = addProvider(v.Providers, f.verifier)
		if !slices.Contains(rs.verifiers, vm) {
			rs.verifiers = append(rs.verifiers, vm)
		}
		switch f.verify {
		case VerifyKept, VerifyCorrected:
			v.Kept.count(fate)
		case VerifyDropped:
			v.Dropped++
			v.Posted.count(fate)
		}
	}
	for _, run := range runs {
		out.Totals.Runs++
		out.Totals.Tokens += run.review + run.verify
		rs := seen[run.id]
		if rs == nil {
			rs = &runSeen{keys: map[string]rkey{}}
		}
		reviewers := run.reviewers
		if len(reviewers) == 0 {
			reviewers = rs.models
		}
		var names, raw []string
		for _, m := range reviewers {
			if n := normalizeModel(m); n != "" && !slices.Contains(names, n) {
				names, raw = append(names, n), append(raw, m)
			}
		}
		for i, n := range names {
			k, ok := rs.keys[n]
			if !ok {
				k = rkey{n, run.strategy, run.version}
			}
			r := rev(k)
			r.Providers = addProvider(r.Providers, raw[i])
			r.Runs++
			r.Tokens += share(run.review, len(names), i)
		}
		for i, vm := range rs.verifiers {
			v := ver(vm)
			v.Runs++
			v.Tokens += share(run.verify, len(rs.verifiers), i)
		}
		switch {
		case run.verified:
			out.Verify.Verified.Runs++
			out.Verify.Verified.Tokens += run.review + run.verify
		case rs.skipped || (strings.HasSuffix(run.arm, "/none") && !rs.trusted):
			out.Verify.Skipped.Runs++
			out.Verify.Skipped.Tokens += run.review + run.verify
		}
	}
	for _, r := range revs {
		r.settle()
		r.TokensPerUseful = perUseful(r.Tokens, r.Useful)
		out.Reviewers = append(out.Reviewers, *r)
	}
	slices.SortFunc(out.Reviewers, func(a, b ReviewerStat) int {
		if d := (b.Useful + b.False) - (a.Useful + a.False); d != 0 {
			return d
		}
		if d := b.Findings - a.Findings; d != 0 {
			return d
		}
		return strings.Compare(a.Model+"\x00"+a.Strategy+"\x00"+a.PromptVersion, b.Model+"\x00"+b.Strategy+"\x00"+b.PromptVersion)
	})
	for _, v := range vers {
		v.Kept.settle()
		v.Posted.settle()
		if v.Posted.Precision != nil {
			// a right drop is a false finding: the complement, bounds swapped
			acc, lo, hi := round3(1-*v.Posted.Precision), round3(1-*v.Posted.High), round3(1-*v.Posted.Low)
			v.DropAccuracy, v.DropLow, v.DropHigh = &acc, &lo, &hi
		}
		out.Verifiers = append(out.Verifiers, *v)
	}
	slices.SortFunc(out.Verifiers, func(a, b VerifierStat) int {
		if d := (b.Kept.Useful + b.Kept.False + b.Kept.Unjudged + b.Dropped) - (a.Kept.Useful + a.Kept.False + a.Kept.Unjudged + a.Dropped); d != 0 {
			return d
		}
		return strings.Compare(a.Model, b.Model)
	})
	out.Totals.settle()
	out.Totals.TokensPerUseful = perUseful(out.Totals.Tokens, out.Totals.Useful)
	for _, c := range []*ReviewArmClass{&out.Verify.Verified, &out.Verify.Skipped} {
		c.settle()
		c.TokensPerUseful = perUseful(c.Tokens, c.Useful)
	}
	return out
}

// share is part i of n of tokens, the remainder to the first parts, so the
// parts add up.
func share(tokens int64, n, i int) int64 {
	if n <= 0 {
		return 0
	}
	q, r := tokens/int64(n), tokens%int64(n)
	if int64(i) < r {
		return q + 1
	}
	return q
}
