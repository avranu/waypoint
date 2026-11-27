package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The rules arms are drawn by.
const (
	armWindow       = 90 * 24 * time.Hour
	armHalfAfter    = 30 * 24 * time.Hour
	armExploreUnder = 15.0
	armBenchBound   = 0.20
	armBenchQ       = 0.90
	armBenchedOdds  = 1.0 / 20
	armCostWindow   = 30 * 24 * time.Hour
	armCostRuns     = 5
	settingArmPins  = "review_arm_pins"
)

// Arm states, as Admin → AI shows them.
const (
	ArmExploring   = "exploring"
	ArmInUse       = "in_use"
	ArmBenched     = "benched"
	ArmPinned      = "pinned"
	ArmUnavailable = "unavailable"
)

// reviewArm is one way to review.
type reviewArm struct {
	strategy reviewStrategy
	verify   bool
}

// reviewStrategies are the context strategies arms are made of, the first
// the one a tie goes to. WP-511's diff+defs joins here when it lands.
var reviewStrategies = []reviewStrategy{strategyDiffFiles, strategyDiff}

// reviewArms is every arm, in tie order: each strategy verified, then not.
func reviewArms() []reviewArm {
	var out []reviewArm
	for _, st := range reviewStrategies {
		out = append(out, reviewArm{st, true}, reviewArm{st, false})
	}
	return out
}

// key names an arm in settings, records and the API: "diff+files/verify".
func (a reviewArm) key() string {
	if a.verify {
		return a.strategy.name + "/verify"
	}
	return a.strategy.name + "/none"
}

func (a reviewArm) label() string {
	if a.verify {
		return a.strategy.name + ", verified"
	}
	return a.strategy.name + ", no verifier"
}

func armByKey(k string) (reviewArm, bool) {
	for _, a := range reviewArms() {
		if a.key() == k {
			return a, true
		}
	}
	return reviewArm{}, false
}

// armTally is what an arm's findings and runs say about it.
type armTally struct {
	useful, falsePos     float64
	rawUseful, rawFalse  int
	models               map[string]*ArmModel
	tokens               float64
	runs                 int
	lastRun              int64
	cost                 float64
	pin                  string
	available, explore   bool
	benched              bool
	upper, mean          float64
	draw                 float64
	drawnIn, benchedDraw bool
}

func (t *armTally) judged() float64 { return t.useful + t.falsePos }

// findingUseful is the SQL condition (on bot_findings f) for a finding that
// held up, with its arguments: fixed, or open or resolved with its thread's
// verdict saying an agent took it. A false one is dismissed or withdrawn.
// review_feedback.go reports the same two counts to the gateway.
const findingUseful = `(f.state = ? OR (f.state IN (?, ?) AND EXISTS (
	SELECT 1 FROM review_comments c WHERE c.finding_id = f.id AND c.verdict IN (?, ?, ?, ?))))`

func findingUsefulArgs() []any {
	return []any{FindingFixed, FindingOpen, FindingResolved, VerdictAccepted, VerdictFixing, VerdictFixed, VerdictVerified}
}

// armFinding is one judged finding, as arms count it.
type armFinding struct {
	strategy, verify, version, model, state string
	created                                 int64
	useful                                  bool
}

// armOf is the arm a finding counts for: its strategy, and the verifier on
// when the verifier kept or corrected it, off when its run had none. One
// the verifier dropped but was posted anyway, or that no verifier judged,
// is not what either arm would have posted, and counts for neither.
func armOf(f armFinding) (string, bool) {
	switch f.verify {
	case VerifyKept, VerifyCorrected:
		return f.strategy + "/verify", true
	case VerifySkipped:
		return f.strategy + "/none", true
	}
	return "", false
}

func (s *Service) armFindings(ctx context.Context) ([]armFinding, error) {
	return s.judgedFindings(ctx, VerifyKept, VerifyCorrected, VerifySkipped)
}

// judgedFindings are the arm window's findings with one of these verifier
// states that were judged useful or false, by the counts the gateway gets: a
// finding dropped because its quote isn't in the file is false too. So is
// one the verifier dropped, for a caller that asks for that state.
func (s *Service) judgedFindings(ctx context.Context, states ...string) ([]armFinding, error) {
	args := append(findingUsefulArgs(), s.now().Add(-armWindow).UnixMilli())
	for _, st := range states {
		args = append(args, st)
	}
	args = append(args, findingUsefulArgs()...)
	args = append(args, FindingDismissed, FindingWithdrawn, FindingDropped, VerifyUnchecked, evidenceMissing, FindingDropped, VerifyDropped)
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(states)), ", ")
	// #nosec G202 -- only placeholders are concatenated; values are bound
	rows, err := s.db.R.QueryContext(ctx, `
		SELECT f.strategy, f.verify, f.prompt_version, f.model, f.state, f.created_at, CASE WHEN `+findingUseful+` THEN 1 ELSE 0 END
		FROM bot_findings f
		WHERE f.created_at >= ? AND f.strategy != '' AND f.verify IN (`+marks+`)
			AND (`+findingUseful+` OR f.state IN (?, ?) OR (f.state = ? AND f.verify = ? AND f.verify_reason = ?) OR (f.state = ? AND f.verify = ?))`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []armFinding
	for rows.Next() {
		var f armFinding
		if err := rows.Scan(&f.strategy, &f.verify, &f.version, &f.model, &f.state, &f.created, &f.useful); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// previousVersions is, per strategy, the prompt version before the current
// one: the newest other version its judged findings came from.
func previousVersions(fs []armFinding) map[string]string {
	cur := map[string]string{}
	for _, st := range reviewStrategies {
		cur[st.name] = st.promptVersion()
	}
	newest := map[string]int64{}
	out := map[string]string{}
	for _, f := range fs {
		if f.version == "" || f.version == cur[f.strategy] {
			continue
		}
		if f.created > newest[f.strategy] {
			newest[f.strategy], out[f.strategy] = f.created, f.version
		}
	}
	return out
}

// armWeight is how much one finding counts: by age, then by prompt version.
func armWeight(now time.Time, f armFinding, current, previous string) float64 {
	age := now.Sub(msTime(f.created))
	w := 1.0
	switch {
	case age >= armWindow:
		return 0
	case age >= armHalfAfter:
		w = 0.5
	}
	switch f.version {
	case current:
	case previous:
		w *= 0.5
	default:
		return 0
	}
	return w
}

// armTallies counts every arm's findings, runs, cost and pin.
func (s *Service) armTallies(ctx context.Context, canVerify bool) (map[string]*armTally, error) {
	out := map[string]*armTally{}
	for _, a := range reviewArms() {
		out[a.key()] = &armTally{models: map[string]*ArmModel{}, cost: 1, available: canVerify || !a.verify}
	}
	fs, err := s.armFindings(ctx)
	if err != nil {
		return nil, err
	}
	prev := previousVersions(fs)
	now := s.now()
	for _, f := range fs {
		k, ok := armOf(f)
		t := out[k]
		if !ok || t == nil {
			continue
		}
		w := armWeight(now, f, strategyNamed(f.strategy).promptVersion(), prev[f.strategy])
		m := t.models[f.model]
		if m == nil {
			m = &ArmModel{Model: f.model}
			t.models[f.model] = m
		}
		if f.useful {
			t.useful += w
			t.rawUseful++
			m.Useful++
		} else {
			t.falsePos += w
			t.rawFalse++
			m.FalsePositives++
		}
	}
	if err := s.armRuns(ctx, out); err != nil {
		return nil, err
	}
	pins := s.armPins(ctx)
	for k, t := range out {
		t.pin = pins[k]
		t.mean = (1 + t.useful) / (2 + t.judged())
		t.upper = betaQuantile(armBenchQ, 1+t.useful, 1+t.falsePos)
		t.explore = t.judged() < armExploreUnder
		t.benched = !t.explore && t.upper < armBenchBound
	}
	return out, nil
}

// strategyNamed is the strategy of that name; an unknown name has none, so
// no prompt version matches it.
func strategyNamed(name string) reviewStrategy {
	for _, st := range reviewStrategies {
		if st.name == name {
			return st
		}
	}
	return reviewStrategy{name: name}
}

// armRuns reads each arm's runs of the cost window: how many drew it, the
// last, and the mean tokens of those with one reviewer (a second reviewer's
// tokens are not the arm's), which sets each arm's cost relative to the
// cheapest measured.
func (s *Service) armRuns(ctx context.Context, tallies map[string]*armTally) error {
	rows, err := s.db.R.QueryContext(ctx, `
		SELECT arm, COUNT(*), MAX(created_at),
			SUM(CASE WHEN json_array_length(reviewer_models) = 1 AND tokens > 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN json_array_length(reviewer_models) = 1 THEN tokens ELSE 0 END)
		FROM (SELECT arm, created_at, reviewer_models,
				0 AS tokens
			FROM bot_runs WHERE kind = 'review' AND status = 'done' AND arm != '' AND created_at >= ?)
		GROUP BY arm`, s.now().Add(-armCostWindow).UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var arm string
		var runs, measured int
		var last, tokens int64
		if err := rows.Scan(&arm, &runs, &last, &measured, &tokens); err != nil {
			return err
		}
		t := tallies[arm]
		if t == nil {
			continue
		}
		t.runs, t.lastRun = runs, last
		if measured >= armCostRuns {
			t.tokens = float64(tokens) / float64(measured)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cheapest := 0.0
	for _, t := range tallies {
		if t.tokens > 0 && (cheapest == 0 || t.tokens < cheapest) {
			cheapest = t.tokens
		}
	}
	for _, t := range tallies {
		if t.tokens > 0 && cheapest > 0 {
			t.cost = t.tokens / cheapest
		}
	}
	return nil
}

// armPins are the arms an admin pinned on or off.
func (s *Service) armPins(ctx context.Context) map[string]string {
	var v string
	out := map[string]string{}
	if err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, settingArmPins).Scan(&v); err != nil {
		return out
	}
	_ = json.Unmarshal([]byte(v), &out)
	return out
}

// drawArms picks the arm for each of a run's reviewers (slots, at least
// one). canVerify: a verifier model is set up and the action allows one.
// Reading the records failing is no reason to skip a review: it then
// reviews with the first available arm.
func (s *Service) drawArms(ctx context.Context, canVerify bool, slots int) []reviewArm {
	tallies, err := s.armTallies(ctx, canVerify)
	if err != nil {
		s.log.WarnContext(ctx, "review arms: read the records; using the first arm", "err", err)
		tallies = map[string]*armTally{}
		for _, a := range reviewArms() {
			tallies[a.key()] = &armTally{cost: 1, available: canVerify || !a.verify, explore: true}
		}
	}
	return s.chooseArms(tallies, slots)
}

// chooseArms is drawArms on tallies already read.
func (s *Service) chooseArms(tallies map[string]*armTally, slots int) []reviewArm {
	arms := reviewArms()
	usable := func(a reviewArm) bool {
		t := tallies[a.key()]
		return t != nil && t.available && t.pin != "off"
	}
	var pinned []reviewArm
	for _, a := range arms {
		if usable(a) && tallies[a.key()].pin == "on" {
			pinned = append(pinned, a)
		}
	}
	var cands []reviewArm
	if len(pinned) > 0 {
		cands = pinned
	} else {
		for _, a := range arms {
			if !usable(a) {
				continue
			}
			t := tallies[a.key()]
			if t.benched {
				if s.draw() >= armBenchedOdds {
					continue
				}
				t.benchedDraw = true
			}
			cands = append(cands, a)
		}
		if len(cands) == 0 {
			for _, a := range arms {
				if usable(a) {
					cands = append(cands, a)
				}
			}
		}
	}
	if len(cands) == 0 {
		return slices.Repeat([]reviewArm{{reviewStrategies[0], false}}, max(slots, 1))
	}
	best := -1
	for i, a := range cands {
		t := tallies[a.key()]
		t.draw = s.sampleBeta(1+t.useful, 1+t.falsePos) / max(t.cost, 1e-9)
		t.drawnIn = true
		if best < 0 || t.draw > tallies[cands[best].key()].draw {
			best = i
		}
	}
	out := []reviewArm{cands[best]}
	for len(out) < slots {
		next := out[0]
		if len(pinned) == 0 {
			top := -1.0
			for _, a := range cands {
				t := tallies[a.key()]
				if a.verify == out[0].verify && t.explore && !slices.Contains(out, a) && t.draw > top {
					next, top = a, t.draw
				}
			}
		}
		out = append(out, next)
	}
	return out
}

// sampleBeta draws from Beta(a, b); tests set Service.beta.
func (s *Service) sampleBeta(a, b float64) float64 {
	if s.beta != nil {
		return s.beta(a, b)
	}
	x, y := gammaSample(a), gammaSample(b)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// gammaSample draws from Gamma(k, 1) (Marsaglia and Tsang).
func gammaSample(k float64) float64 {
	if k < 1 {
		return gammaSample(k+1) * math.Pow(rand.Float64(), 1/k) // #nosec G404 -- sampling for an experiment
	}
	d := k - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := rand.NormFloat64() // #nosec G404 -- sampling for an experiment
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rand.Float64() // #nosec G404 -- sampling for an experiment
		if u < 1-0.0331*x*x*x*x || math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}

// betaQuantile is the q-quantile of Beta(a, b), by bisection on the
// regularized incomplete beta function.
func betaQuantile(q, a, b float64) float64 {
	lo, hi := 0.0, 1.0
	for range 60 {
		mid := (lo + hi) / 2
		if incBeta(mid, a, b) < q {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// incBeta is the regularized incomplete beta function I_x(a, b).
func incBeta(x, a, b float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log(1-x))
	if x < (a+1)/(a+b+2) {
		return front * betaCF(x, a, b) / a
	}
	return 1 - front*betaCF(1-x, b, a)/b
}

// betaCF is the continued fraction of the incomplete beta (modified Lentz).
func betaCF(x, a, b float64) float64 {
	const tiny = 1e-300
	qab, qap, qam := a+b, a+1, a-1
	c, d := 1.0, 1-qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= 300; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < 3e-12 {
			break
		}
	}
	return h
}

type gatewayExcludeKey struct{}

// withGatewayExclude asks the gateway to leave models out of the requests made
// with ctx (its exclude field): the reviewer's models for the
// verifier, the first reviewer's for a second one. Other providers never
// see it.
func withGatewayExclude(ctx context.Context, models []string) context.Context {
	var pats []string
	for _, m := range models {
		for _, p := range excludePatterns(m) {
			if !slices.Contains(pats, p) {
				pats = append(pats, p)
			}
		}
	}
	if len(pats) == 0 {
		return ctx
	}
	return context.WithValue(ctx, gatewayExcludeKey{}, pats[:min(len(pats), maxExcludes)])
}

func withoutGatewayExclude(ctx context.Context) context.Context {
	if gatewayExcludeOf(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, gatewayExcludeKey{}, []string(nil))
}

func gatewayExcludeOf(ctx context.Context) []string {
	p, _ := ctx.Value(gatewayExcludeKey{}).([]string)
	return p
}

// maxExcludes is the most patterns the gateway takes in one request.
const maxExcludes = 32

// excludePatterns are the gateway patterns for a served model: itself, and
// the same weights on any provider ("*/gpt-oss-120b"; the gateway's * also
// crosses "/").
func excludePatterns(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	last := model
	if n := strings.LastIndex(model, "/"); n >= 0 {
		last = model[n+1:]
	}
	if last == "" || strings.ContainsAny(last, "*?[") {
		return []string{model}
	}
	return []string{model, "*/" + last}
}

// The file text diff+files may add, scaled to the context window of the
// model the gateway last answered a review with: the window in characters
// (charsPerToken a token, conservatively for code), half of it for the
// prompt, less the diff part and the rest of the prompt.
const (
	reviewBudgetMin   = 8000
	reviewBudgetMax   = 160000
	charsPerToken     = 3
	reviewPromptOther = 8000
)

// ReviewBudget is how much file text diff+files adds, and why.
type ReviewBudget struct {
	Chars         int    `json:"chars"`
	Model         string `json:"model,omitempty"`          // the model it was scaled to
	ContextLength int    `json:"context_length,omitempty"` // that model's window, in tokens
}

func budgetFor(contextLength int) int {
	return min(max(contextLength*charsPerToken/2-reviewChunkChars-reviewPromptOther, reviewBudgetMin), reviewBudgetMax)
}

// reviewFileBudget is the budget for this run: scaled to the last review
// model's context_length from the gateway's /v1/models, else the
// review_context_chars setting (32000 by default). The gateway skips a
// model whose window a prompt doesn't fit, so a guess too big costs a
// smaller model's turn, not the review.
func (s *Service) reviewFileBudget(ctx context.Context) ReviewBudget {
	out := ReviewBudget{Chars: int(s.serverNumber(ctx, settingContextBudget, defaultContextBudget))}
	model := s.lastReviewModel(ctx)
	pid := s.gatewayID(ctx)
	if model == "" || pid == 0 {
		return out
	}
	p, err := s.aiProvider(ctx, pid)
	if err != nil {
		return out
	}
	ms, err := s.providerModels(ctx, p)
	if err != nil {
		return out
	}
	n := 0
	for _, m := range ms {
		if m.ContextLength <= 0 {
			continue
		}
		if m.ID == model {
			n = m.ContextLength
			break
		}
		if n == 0 && sameModel(m.ID, model) {
			n = m.ContextLength
		}
	}
	if n == 0 {
		return out
	}
	return ReviewBudget{Chars: budgetFor(n), Model: model, ContextLength: n}
}

// lastReviewModel is the model that answered the newest review run.
func (s *Service) lastReviewModel(ctx context.Context) string {
	var models string
	err := s.db.R.QueryRowContext(ctx, `SELECT reviewer_models FROM bot_runs WHERE kind = 'review' AND reviewer_models != '[]' ORDER BY id DESC LIMIT 1`).Scan(&models)
	if err == nil {
		var list []string
		if json.Unmarshal([]byte(models), &list) == nil && len(list) > 0 {
			return list[0]
		}
	}
	var m string
	_ = s.db.R.QueryRowContext(ctx, `SELECT model FROM bot_findings WHERE model != '' ORDER BY id DESC LIMIT 1`).Scan(&m)
	return m
}

// ReviewArms is each arm's state and why, for Admin → AI.
type ReviewArms struct {
	Arms   []ReviewArm  `json:"arms"`
	Recall []ArmRecall  `json:"recall"`
	Budget ReviewBudget `json:"budget"`
	// The rules, for the page's words.
	ExploreUnder float64 `json:"explore_under"`
	BenchBound   float64 `json:"bench_bound"`
	BenchedOdds  int     `json:"benched_odds"` // 1 in this many runs
	// Gateway: an LLM gateway is set up and picks each review's model;
	// without one the review models set up in Admin → AI run (WP-459).
	Gateway bool `json:"gateway"`
}

// ReviewArm is one arm.
type ReviewArm struct {
	Key            string     `json:"key"`
	Strategy       string     `json:"strategy"`
	Verify         bool       `json:"verify"`
	Label          string     `json:"label"`
	State          string     `json:"state"` // exploring, in_use, benched, pinned, unavailable
	Why            string     `json:"why"`
	Pin            string     `json:"pin,omitempty"`   // on, off
	Useful         float64    `json:"useful"`          // weighted
	FalsePositives float64    `json:"false_positives"` // weighted
	Precision      float64    `json:"precision"`       // the posterior mean
	Upper          float64    `json:"upper"`           // its 90% upper bound
	Cost           float64    `json:"cost"`            // tokens relative to the cheapest arm; 1 until measured
	Tokens         float64    `json:"tokens"`          // mean tokens a run, 0 until measured
	Runs           int        `json:"runs"`            // runs that drew it, past 30 days
	LastRun        *time.Time `json:"last_run,omitempty"`
	// Models are the models the gateway answered its runs with, and how
	// their findings fared (judged in the past 90 days, unweighted).
	Models []ArmModel `json:"models"`
}

// ArmModel is how one model's findings fared on an arm.
type ArmModel struct {
	Model          string `json:"model"`
	Useful         int    `json:"useful"`
	FalsePositives int    `json:"false_positives"`
}

// ArmRecall is one model's findings in runs where another reviewer
// answered too: what it found alone, how many of those held up, and how
// many that held up another found and it missed.
type ArmRecall struct {
	Model      string `json:"model"`
	Runs       int    `json:"runs"`
	Alone      int    `json:"alone"`
	AloneHeld  int    `json:"alone_held"`
	MissedHeld int    `json:"missed_held"`
}

// ReviewArms is each arm's state (admins).
func (s *Service) ReviewArms(ctx context.Context, actor *Actor) (*ReviewArms, error) {
	if !actor.isAdmin() {
		return nil, forbidden("review arms are for admins")
	}
	vchain, err := s.taskChain(ctx, "verify_review")
	if err != nil {
		return nil, err
	}
	tallies, err := s.armTallies(ctx, len(vchain) > 0)
	if err != nil {
		return nil, err
	}
	for _, t := range tallies {
		t.available = t.available || t.runs > 0
	}
	out := &ReviewArms{Gateway: s.gatewayID(ctx) != 0, Arms: []ReviewArm{}, ExploreUnder: armExploreUnder, BenchBound: armBenchBound, BenchedOdds: int(math.Round(1 / armBenchedOdds))}
	for _, a := range reviewArms() {
		t := tallies[a.key()]
		v := ReviewArm{Key: a.key(), Strategy: a.strategy.name, Verify: a.verify, Label: a.label(), Pin: t.pin,
			Useful: round2(t.useful), FalsePositives: round2(t.falsePos), Precision: round2(t.mean), Upper: round2(t.upper),
			Cost: round2(t.cost), Tokens: math.Round(t.tokens), Runs: t.runs, Models: []ArmModel{}}
		if t.lastRun > 0 {
			lr := msTime(t.lastRun)
			v.LastRun = &lr
		}
		v.State, v.Why = armState(t)
		for _, m := range t.models {
			v.Models = append(v.Models, *m)
		}
		slices.SortFunc(v.Models, func(x, y ArmModel) int {
			return (y.Useful + y.FalsePositives) - (x.Useful + x.FalsePositives)
		})
		out.Arms = append(out.Arms, v)
	}
	if out.Recall, err = s.armRecall(ctx); err != nil {
		return nil, err
	}
	out.Budget = s.reviewFileBudget(ctx)
	return out, nil
}

// armState names an arm's state and says why, in a sentence.
func armState(t *armTally) (string, string) {
	pct := func(f float64) string { return fmt.Sprintf("%.0f%%", 100*f) }
	judged := strconv.FormatFloat(t.judged(), 'f', -1, 64) + " judged findings"
	switch {
	case !t.available:
		return ArmUnavailable, "No verifier model is set up (Admin → AI → Verify review findings), so only a review rule that names its own verifier draws it."
	case t.pin == "on":
		return ArmPinned, "Pinned on: every review uses it (or one of the other pinned arms)."
	case t.pin == "off":
		return ArmPinned, "Pinned off: no review uses it."
	case t.explore:
		return ArmExploring, fmt.Sprintf("Exploring: %s of %.0f needed; drawn like the others, and a second reviewer goes to it.", judged, armExploreUnder)
	case t.benched:
		return ArmBenched, fmt.Sprintf("Benched: at most %s of its findings hold up (90%% bound, %s), under %s; still drawn on 1 run in %d.",
			pct(t.upper), judged, pct(armBenchBound), int(math.Round(1/armBenchedOdds)))
	}
	cost := ""
	if t.cost > 1.005 {
		cost = fmt.Sprintf(" at %.2f× the tokens of the cheapest arm", t.cost)
	}
	return ArmInUse, fmt.Sprintf("In use: about %s of its findings hold up (%s)%s; each run draws on that.", pct(t.mean), judged, cost)
}

// armRecall reads, for runs with more than one reviewer, what each model
// found alone and what it missed that held up.
func (s *Service) armRecall(ctx context.Context) ([]ArmRecall, error) {
	since := s.now().Add(-armWindow).UnixMilli()
	by := map[string]*ArmRecall{}
	get := func(m string) *ArmRecall {
		if by[m] == nil {
			by[m] = &ArmRecall{Model: m}
		}
		return by[m]
	}
	runs, err := s.db.R.QueryContext(ctx, `SELECT reviewer_models FROM bot_runs WHERE kind = 'review' AND created_at >= ? AND json_array_length(reviewer_models) >= 2`, since)
	if err != nil {
		return nil, err
	}
	for runs.Next() {
		var ms string
		if err := runs.Scan(&ms); err != nil {
			runs.Close()
			return nil, err
		}
		var list []string
		_ = json.Unmarshal([]byte(ms), &list)
		for _, m := range list {
			get(m).Runs++
		}
	}
	runs.Close()
	if err := runs.Err(); err != nil {
		return nil, err
	}
	args := append(findingUsefulArgs(), since, FindingDropped)
	rows, err := s.db.R.QueryContext(ctx, `
		SELECT f.model, r.reviewer_models, CASE WHEN `+findingUseful+` THEN 1 ELSE 0 END
		FROM bot_findings f JOIN bot_runs r ON r.id = f.run_id
		WHERE f.created_at >= ? AND f.agreed = 1 AND f.state != ? AND json_array_length(r.reviewer_models) >= 2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var model, ms string
		var useful bool
		if err := rows.Scan(&model, &ms, &useful); err != nil {
			return nil, err
		}
		a := get(model)
		a.Alone++
		if !useful {
			continue
		}
		a.AloneHeld++
		var list []string
		_ = json.Unmarshal([]byte(ms), &list)
		for _, m := range list {
			if !sameModel(m, model) {
				get(m).MissedHeld++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []ArmRecall{}
	for _, a := range by {
		out = append(out, *a)
	}
	slices.SortFunc(out, func(x, y ArmRecall) int {
		if x.Runs != y.Runs {
			return y.Runs - x.Runs
		}
		return strings.Compare(x.Model, y.Model)
	})
	return out, nil
}

// PinReviewArm pins an arm on or off ("" unpins it) (admins).
func (s *Service) PinReviewArm(ctx context.Context, actor *Actor, key, pin string) (*ReviewArms, error) {
	if !actor.isAdmin() {
		return nil, forbidden("review arms are for admins")
	}
	if _, ok := armByKey(key); !ok {
		return nil, invalid("no review arm is called %q", key)
	}
	if pin != "" && pin != "on" && pin != "off" {
		return nil, invalid("pin is on, off or empty")
	}
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	pins := map[string]string{}
	var cur string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, settingArmPins).Scan(&cur); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal([]byte(cur), &pins); err != nil {
			return nil, fmt.Errorf("the stored arm pins are unreadable: %w", err)
		}
		if pins == nil {
			pins = map[string]string{}
		}
	}
	if pin == "" {
		delete(pins, key)
	} else {
		pins[key] = pin
	}
	b, err := json.Marshal(pins)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, settingArmPins, string(b)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ReviewArms(ctx, actor)
}
