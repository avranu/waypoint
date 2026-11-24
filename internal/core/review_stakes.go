package core

// Stakes tiers (WP-1231, WP-1248, WP-ADR121, narrowing WP-ADR102): how
// hard a review round's audit is held to a quality bar, and which rounds
// may spend Claude quota on their review. A round's audit is free: the
// LLM gateway runs it on a model outside the author's family, or at the
// high-stakes bar on any model it rates that well (WP-1752). A
// review on Claude (the reviewer of WP-ADR117 in a new session, or the
// working session's own) spends the quota other work needs, so the tier
// decides which Claude model reviews when the free audit can't be had in
// time (roundStakes.reviewer).
//
//   - high: the task's risk is high, a file outside tests touches security,
//     credentials, data migrations, deploys or CI (stakesPathRE), the
//     change is very large (over stakesHugeLines), or a large one (over
//     triageMaxLines) touches risky files. Its audit is held to
//     trustedAuditBar (Grok 4.7 or better), and a slow or failed audit
//     starts the Sonnet reviewer (WP-ADR117).
//   - elevated: a large change (over triageMaxLines), ADR102's other signs
//     (risky files in a smaller change, the looser sensitive names: token,
//     hooks, guard, go.mod ...), or a task whose risk couldn't be read. Its
//     audit is held to elevatedAuditBar (Claude Haiku or better); an audit
//     that fails is asked again at no bar, and a slow one starts the Haiku
//     reviewer beside it (WP-1306), the first to end being the round's.
//   - normal: no bar, so the gateway chooses, and the same handling.
//   - The lowest-risk changes (docs, comments) skip the round altogether:
//     that is triage's (review_triage.go, WP-ADR79), before any tier.
//
// Size counts (stakes-v2): over 10-17..11-16, every one of the 26 useful
// findings a pull request's review raised after its round had missed them
// was on a change over 400 lines, and rounds on changes over 1000 lines
// fixed 6.1 findings each against 1.3-1.9 below 400 (WP-1248).
//
// When no free audit can be had (the asks failed, the audit read nothing,
// a person cancelled it, or no runner could take it), the tier's reviewer
// reviews: every round gets a review. A normal round's
// failed audit goes there at once, since asking again would make the same
// request. A round triage never saw takes the tier its task's last round
// was given; with none, or on a Waypoint without the gateway, it keeps the
// high tier's handling with no bar (unknown).
//
// A round's tier is decided once and stored (review_stakes), so the stakes
// report (wp worklog stakes) shows what each round was given; rounds from
// before that are recomputed by the current rule, StakesRule.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	StakesHigh     = "high"
	StakesElevated = "elevated"
	StakesNormal   = "normal"
	// StakesUnknown is the report's name for rounds triage never saw.
	StakesUnknown = "unknown"
	// StakesRule names the rule changeStakes applies.
	StakesRule = "stakes-v2"
)

// stakesPathRE is what makes a change high-stakes by path alone: access,
// secrets, data migrations, deployment and CI. Narrower than
// sensitivePathRE, whose other words (token, acl, security, hooks, guard,
// infra, nginx, go.mod, package.json, Makefile) only make a change
// elevated: on 11-13..11-16 they put the trusted bar on 38 of 43 Waypoint
// rounds.
var stakesPathRE = lazyre.New(`auth|login|oauth|secret|password|credential|crypto|sandbox|permission|migrations/|\.sql$|\.env|deploy/|terraform|k8s|helm|dockerfile|workflows/`)

// stakesHugeLines is the size over which a change is high-stakes on its
// own (WP-1248).
const stakesHugeLines = 1000

// changeStakes is a change's stakes tier and why, from triage's facts.
// Facts triage couldn't read say nothing about the change: unknown.
func changeStakes(f *TriageFeatures) (tier, why string) {
	if f.NoFacts {
		return StakesUnknown, "triage had no facts about the change"
	}
	lines := f.lines()
	var high []string
	if f.Provenance == RiskHigh {
		high = append(high, "the task's risk is high")
	}
	var paths []string
	for _, p := range f.Sensitive {
		if stakesPath(p) {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 {
		high = append(high, "it changes security, data or deployment paths ("+strings.Join(paths, ", ")+")")
	}
	switch {
	case lines > stakesHugeLines:
		high = append(high, fmt.Sprintf("it changes %d lines", lines))
	case f.RiskyFiles > 0 && lines > triageMaxLines:
		high = append(high, fmt.Sprintf("its %d changed lines touch %s with past bug fixes or many importers", lines, plural1(f.RiskyFiles, "file")))
	}
	if len(high) > 0 {
		return StakesHigh, strings.Join(high, "; ")
	}
	var elevated []string
	if lines > triageMaxLines {
		elevated = append(elevated, fmt.Sprintf("it changes %d lines", lines))
	}
	if f.RiskErr {
		elevated = append(elevated, "the task's risk couldn't be read")
	}
	if why := highStakes(f); why != "" {
		elevated = append(elevated, why)
	}
	if len(elevated) > 0 {
		return StakesElevated, strings.Join(elevated, "; ")
	}
	return StakesNormal, ""
}

// stakesPath says whether a path makes a change high-stakes on its own: one
// outside tests that stakesPathRE matches.
func stakesPath(p string) bool {
	return classifyFile(p) != fileTest && stakesPathRE.MatchString(strings.ToLower(p))
}

// tierBar is a tier's audit bar, for a gateway that serves audits: the
// high and elevated tiers' own, and the floor of every code review for the
// normal and unknown ones (WP-1440). It is what decideStakes stores whether
// or not a gateway is set; roundStakes answers with no bar when none is, and
// RequestAudit records the floor only on an audit whose chain asks the
// gateway to choose.
func tierBar(tier string) string {
	switch tier {
	case StakesHigh:
		return trustedAuditBar
	case StakesElevated:
		return elevatedAuditBar
	}
	return reviewFloorBar
}

// roundStakes is a review round's stakes: its tier ("": unknown, the old
// handling), why, and its audit's quality bar.
type roundStakes struct {
	tier, why string
	bar       string
}

// reviewer is the model of the round's Claude reviewer (WP-ADR117), the
// review in a new session that starts when its free audit can't be used or
// likely won't end by the round's patience: Sonnet for a high-stakes round
// or one whose stakes aren't known, Haiku for the rest, the bars the owner set
// per risk ("biggest: Sonnet or better; medium: Haiku or better"). A round
// whose audit is slow gets this review in its place: before WP-1306 an
// elevated or normal round's slow audit went on as the pull request's with
// no review in its place, and the round ended "found no issues" while the
// audit landed on the pull request up to three hours later. A round with
// neither (review_model "session", or no runner that can run claude) is the
// session's own, with its adversarial-review subagent, and counts once the
// session reported what that found (review_report.go).
func (r roundStakes) reviewer() AgentModel {
	if r.tier == "" || r.tier == StakesHigh {
		return AgentModel{CLI: "claude", Model: "sonnet"}
	}
	return AgentModel{CLI: "claude", Model: "haiku"}
}

// reviewerName is the reviewer's name in text: "Sonnet review".
func (r roundStakes) reviewerName() string {
	return upperFirst(r.reviewer().Model) + " review"
}

// barFor is the bar of the round's audit asked under key: the tier's, but
// the floor for the one an elevated round asks after its first failed
// (reviewOtherAuditKey): it must not hold out for Haiku's level again. If no
// model meets even the floor, the round goes to its Claude reviewer, which
// is the right end. A Waypoint without the gateway has no bar.
func (r roundStakes) barFor(key string) string {
	if key == reviewOtherAuditKey && r.tier == StakesElevated && r.bar != "" {
		return reviewFloorBar
	}
	return r.bar
}

// roundStakes is a round's stakes: the ones it was given (review_stakes),
// else decided now and kept. A Waypoint without the gateway has none: no
// bar, the unknown tier's bars for another round (againBars), and Sonnet
// as its Claude reviewer.
func (s *Service) roundStakes(ctx context.Context, roundID int64) (roundStakes, error) {
	if _, _, why := s.gatewayTasksLLM(ctx); why != "" {
		return roundStakes{}, nil
	}
	var r roundStakes
	err := s.db.R.QueryRowContext(ctx, `SELECT tier, why, bar FROM review_stakes WHERE round_id = ?`, roundID).Scan(&r.tier, &r.why, &r.bar)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		if r, err = s.decideStakes(ctx, roundID); err != nil {
			return roundStakes{}, err
		}
	default:
		return roundStakes{}, err
	}
	if r.tier == StakesUnknown {
		r.tier = ""
	}
	if r.bar == "" {
		// Stored before WP-1440, when a normal round had no bar: it asks
		// at the floor now, as a new round does.
		r.bar = reviewFloorBar
	}
	return r, nil
}

// roundRisk is the stakes tier of the change a round reviews, for the
// decision about another round (reviewAgain): the tier the round was
// given, else decided now and kept; unknown only when nothing is known of
// the change. It is the change's risk whoever reviews it: roundStakes,
// which is about the gateway's audit and its bar, answers with no tier on
// a Waypoint without the gateway, and read through it every change there
// was decided as unknown, whatever its task's risk, its paths or its size.
// A read that fails is the caller's to stop on.
func (s *Service) roundRisk(ctx context.Context, roundID int64) (string, error) {
	var tier string
	err := s.db.R.QueryRowContext(ctx, `SELECT tier FROM review_stakes WHERE round_id = ?`, roundID).Scan(&tier)
	if errors.Is(err, sql.ErrNoRows) {
		var r roundStakes
		r, err = s.decideStakes(ctx, roundID)
		tier = r.tier
	}
	if err != nil {
		return "", err
	}
	return cmp(tier, StakesUnknown), nil
}

// decideStakes decides a round's stakes and stores them: from the round's
// triage facts by StakesRule; for a round triage never saw (a second round,
// one asked for by hand), the tier its task's last round was given, at that
// tier's bar now; else unknown.
func (s *Service) decideStakes(ctx context.Context, roundID int64) (roundStakes, error) {
	var issueID int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT issue_id FROM work_log WHERE id = ?`, roundID).Scan(&issueID); err != nil {
		return roundStakes{}, err
	}
	var r roundStakes
	source := "rule"
	f, err := s.roundTriageFeatures(ctx, roundID)
	if err != nil {
		return roundStakes{}, err
	}
	if f != nil {
		r.tier, r.why = changeStakes(f)
	} else {
		var prev int64
		err := s.db.R.QueryRowContext(ctx, `SELECT round_id, tier, why FROM review_stakes WHERE issue_id = ? AND round_id < ? AND tier != ?
			ORDER BY round_id DESC LIMIT 1`, issueID, roundID, StakesUnknown).Scan(&prev, &r.tier, &r.why)
		switch {
		case err == nil:
			source = "inherited"
			r.why = fmt.Sprintf("round %d's tier: %s", prev, r.why)
		case errors.Is(err, sql.ErrNoRows):
			r.tier, r.why, source = StakesUnknown, "triage never saw the change", "none"
		default:
			return roundStakes{}, err
		}
	}
	r.bar = tierBar(r.tier)
	// No event: the stakes are read when the round plans its review.
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO review_stakes(round_id, issue_id, rule, tier, why, bar, source, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, roundID, issueID, StakesRule, r.tier, r.why, r.bar, source, s.nowMS()); err != nil {
			return err
		}
		// Two callers at once: the first's stands.
		return t.QueryRowContext(ctx, `SELECT tier, why, bar FROM review_stakes WHERE round_id = ?`, roundID).Scan(&r.tier, &r.why, &r.bar)
	})
	return r, err
}

// roundTriageFeatures is the facts of a round's latest triage (nil: none,
// or unreadable).
func (s *Service) roundTriageFeatures(ctx context.Context, roundID int64) (*TriageFeatures, error) {
	var raw string
	err := s.db.R.QueryRowContext(ctx, `SELECT features FROM review_triage WHERE round_id = ? ORDER BY id DESC LIMIT 1`, roundID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f TriageFeatures
	if json.Unmarshal([]byte(raw), &f) != nil {
		return nil, nil
	}
	return &f, nil
}

// stakesNoClaude is in the audit_note of a round whose slow audit went on
// as the pull request's with no Claude review in its place: the round's
// plan is planHandedOver's from then on. No round is noted so since WP-1306
// (every round's slow audit starts its reviewer); the rounds from before
// keep their plan, and the stakes report counts them.
const stakesNoClaude = "no Claude review starts"

// planHandedOver is the plan of a round whose slow audit went on as the
// pull request's (roundAuditOutlives hands it over once the round is past
// pending): before the pull request, the session opens it; with one open,
// the round ends, and the audit holds the merge while it runs and its
// findings are the babysitter's.
func (s *Service) planHandedOver(ctx context.Context, review *roundRow, auditID int64, first *beforePR, now int64) (reviewPlan, error) {
	if first != nil {
		prompt := handOverPrompt(auditID)
		return reviewPlan{prompt: prompt}, s.setRoundPrompt(ctx, review.id, prompt)
	}
	body := fmt.Sprintf("Audit #%d (%s/audits/%d) was still going, and the change's stakes don't call for a Claude review in its place: it goes on as the pull request's audit, which holds the merge until it ends.",
		auditID, s.BaseURL(), auditID)
	return reviewPlan{done: true}, s.endPendingRoundDone(ctx, review, body, now)
}

// handOverPrompt is the prompt of a round before its pull request whose
// slow audit goes on as the pull request's.
func handOverPrompt(auditID int64) string {
	return promptText(fmt.Sprintf("Audit #%d (adversarial review by another model) hasn't ended, and this change's stakes don't call for a Claude review in its place. "+
		"Run `wp ship`, then tell the user so: the audit goes on as the pull request's and holds its merge until it ends.", auditID))
}

// ---- the stakes report (wp worklog stakes) ----

// StakesTierStats is what the review rounds of one tier did and cost.
type StakesTierStats struct {
	Tier   string         `json:"tier"`
	Rounds int            `json:"rounds"`
	States map[string]int `json:"states"` // the rounds by state
	// Recomputed: rounds from before tiers were stored, put in this tier
	// by the current rule rather than the one that applied to them.
	Recomputed int `json:"recomputed"`
	// The free audit: asked at a tier bar above the floor (reviewFloorBar,
	// which every round has, WP-1440); of those, ended without an
	// answer; asked again at the floor after one failed (elevated); handed to
	// the pull request when slow, with no Claude review (elevated, normal;
	// none since WP-1306).
	Barred        int `json:"barred"`
	BarFailed     int `json:"bar_failed"`
	FreeReasks    int `json:"free_reasks"`
	SlowHandOvers int `json:"slow_handovers"`
	// Claude's part: Claude reviews started (Sonnet, or Haiku below high
	// stakes since WP-1306; one figure for both), those whose findings were the
	// round's, their minutes and tokens; and rounds the working session
	// reviewed on its own prompt.
	Reviewers       int     `json:"claude_reviews"`
	ReviewersUsed   int     `json:"claude_reviews_used"`
	ReviewerMinutes float64 `json:"claude_minutes"`
	ReviewerInput   int64   `json:"claude_input_tokens"`
	ReviewerOutput  int64   `json:"claude_output_tokens"`
	OwnReviews      int     `json:"own_reviews"`
	// What the reviews found: rounds whose audit or reviewer answered, of
	// those with no findings; the rounds' findings and how they were
	// judged; the minutes from a round's start to its audit's answer.
	Answered     int     `json:"answered"`
	Empty        int     `json:"empty"`
	Found        int     `json:"found"`
	Fixed        int     `json:"fixed"`
	Rejected     int     `json:"rejected"`
	AuditMinutes float64 `json:"mean_audit_minutes"`
	// What the round missed: useful findings the branch's pull request
	// reviews raised, and escapes traced to it.
	Useful  int `json:"useful_on_prs"`
	Escapes int `json:"escapes"`
	// The tasks whose rounds were in this tier (a task in several, in its
	// highest), and what they took and led to: Outcomes has each measure's
	// median and mean over the tasks it applies to (admins: it reads every
	// person's usage).
	Tasks    int             `json:"tasks"`
	Outcomes []StakesOutcome `json:"outcomes,omitempty"`

	auditMinutes []float64
}

// StakesOutcome is one measure of a tier's tasks.
type StakesOutcome struct {
	ExperimentMetric
	N      int     `json:"n"`
	Median float64 `json:"median"`
	Mean   float64 `json:"mean"`
}

// StakesModelStats is how one model's round audits went in one tier.
type StakesModelStats struct {
	Tier     string `json:"tier"`
	Model    string `json:"model"`
	Answered int    `json:"answered"`
	Empty    int    `json:"empty"`
	Findings int    `json:"findings"`
	Fixed    int    `json:"fixed"` // fixed or confirmed
	Rejected int    `json:"rejected"`
}

// StakesRound is one round, for an export.
type StakesRound struct {
	Round    int64  `json:"round"`
	Task     string `json:"task"`
	At       int64  `json:"at"`
	Tier     string `json:"tier"`
	Why      string `json:"why,omitempty"`
	Rule     string `json:"rule,omitempty"` // the rule that gave the tier; "" recomputed by the current one
	State    string `json:"state"`
	Audit    int64  `json:"audit,omitempty"`
	Bar      string `json:"bar,omitempty"`
	Model    string `json:"model,omitempty"`
	Note     string `json:"note,omitempty"`
	Alt      string `json:"alt,omitempty"`
	Reviewer int64  `json:"claude_review,omitempty"`
	Found    int    `json:"found"`
	Fixed    int    `json:"fixed"`
	Rejected int    `json:"rejected"`
	Useful   int    `json:"useful_on_prs"`
}

// ReviewStakesStats reports review rounds by stakes tier.
type ReviewStakesStats struct {
	Days      int                 `json:"days"`
	Rule      string              `json:"rule"`
	Tiers     []*StakesTierStats  `json:"tiers"`
	Models    []*StakesModelStats `json:"models"`
	Rows      []*StakesRound      `json:"rows,omitempty"`
	Truncated bool                `json:"truncated,omitempty"`
	// OutcomesNote says why the tiers have no task outcomes.
	OutcomesNote string `json:"outcomes_note,omitempty"`
}

// GetReviewStakesStats reports the review rounds of the past days by
// stakes tier, for the projects the actor can see in full.
func (s *Service) GetReviewStakesStats(ctx context.Context, actor *Actor, projectRef string, days int, withRows bool) (*ReviewStakesStats, error) {
	if days <= 0 {
		days = 14
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
	out := &ReviewStakesStats{Days: days, Rule: StakesRule, Tiers: []*StakesTierStats{}, Models: []*StakesModelStats{}}
	tiers := map[string]*StakesTierStats{}
	models := map[[2]string]*StakesModelStats{}
	since := s.nowMS() - int64(days)*24*3600*1000
	taskTier := map[int64]string{}
	for _, p := range projects {
		if p.Role == "guest" {
			continue
		}
		if err := s.stakesProject(ctx, p, since, withRows, tiers, models, taskTier, out); err != nil {
			return nil, err
		}
	}
	order := []string{StakesHigh, StakesElevated, StakesNormal, StakesUnknown}
	if err := s.stakesOutcomes(ctx, actor, days, taskTier, tiers, out); err != nil {
		return nil, err
	}
	for _, name := range order {
		if t := tiers[name]; t != nil {
			if n := len(t.auditMinutes); n > 0 {
				var sum float64
				for _, m := range t.auditMinutes {
					sum += m
				}
				t.AuditMinutes = sum / float64(n)
			}
			out.Tiers = append(out.Tiers, t)
		}
	}
	for _, m := range models {
		out.Models = append(out.Models, m)
	}
	slices.SortFunc(out.Models, func(a, b *StakesModelStats) int {
		if c := slices.Index(order, a.Tier) - slices.Index(order, b.Tier); c != 0 {
			return c
		}
		if c := b.Answered - a.Answered; c != 0 {
			return c
		}
		return strings.Compare(a.Model, b.Model)
	})
	return out, nil
}

// stakesAudit is what the report reads of one audit.
type stakesAudit struct {
	id                   int64
	status, weakest      string
	model                string
	nothing              bool
	createdAt            int64
	startedAt, finished  int64
	findings, fixed, rej int
	input, output        int64
}

func (s *Service) stakesAuditOf(ctx context.Context, where string, arg any) (*stakesAudit, error) {
	var a stakesAudit
	var tokens, attempts string
	var started, finished sql.NullInt64
	// #nosec G202 -- where is one of this file's constants
	err := s.db.R.QueryRowContext(ctx, `SELECT a.id, a.status, a.weakest, a.model, a.tokens, a.attempts, a.created_at, a.started_at, a.finished_at,
			(SELECT count(*) FROM code_audit_findings f WHERE f.audit_id = a.id),
			(SELECT count(*) FROM code_audit_findings f WHERE f.audit_id = a.id AND f.state IN ('fixed', 'confirmed')),
			(SELECT count(*) FROM code_audit_findings f WHERE f.audit_id = a.id AND f.state = 'rejected')
		FROM code_audits a WHERE `+where+` ORDER BY a.id DESC LIMIT 1`, arg).Scan(&a.id, &a.status, &a.weakest, &a.model, &tokens, &attempts, &a.createdAt, &started, &finished,
		&a.findings, &a.fixed, &a.rej)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.startedAt, a.finished = started.Int64, finished.Int64
	var t map[string]any
	if json.Unmarshal([]byte(tokens), &t) == nil {
		in, _ := anyFloat(t["input"])
		out, _ := anyFloat(t["output"])
		a.input, a.output = int64(in), int64(out)
	}
	var at []AuditAttempt
	if json.Unmarshal([]byte(attempts), &at) == nil && len(at) > 0 {
		a.nothing = at[len(at)-1].Outcome == "nothing"
	}
	return &a, nil
}

func (s *Service) stakesProject(ctx context.Context, p *Project, since int64, withRows bool, tiers map[string]*StakesTierStats, models map[[2]string]*StakesModelStats,
	taskTier map[int64]string, out *ReviewStakesStats) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT w.id, w.issue_id, i.number, w.state, w.created_at, coalesce(w.audit_id, 0), w.audit_note, w.audit_alt,
			coalesce(w.found, 0), coalesce(w.fixed, 0), coalesce(w.rejected, 0), coalesce(t.features, ''), coalesce(t.repo, ''), coalesce(t.head, ''),
			coalesce(rs.tier, ''), coalesce(rs.why, ''), coalesce(rs.rule, ''), coalesce(t.action, '')
		FROM work_log w JOIN issues i ON i.id = w.issue_id
		LEFT JOIN review_triage t ON t.id = (SELECT max(x.id) FROM review_triage x WHERE x.round_id = w.id)
		LEFT JOIN review_stakes rs ON rs.round_id = w.id
		WHERE i.project_id = ? AND w.kind = 'review' AND w.created_at >= ?
		ORDER BY w.id DESC`, p.ID, since)
	if err != nil {
		return err
	}
	type round struct {
		r                  StakesRound
		issue              int64
		features, repo, hd string
		action             string
		auditID            int64
	}
	var all []*round
	for rows.Next() {
		var x round
		var number int64
		if err := rows.Scan(&x.r.Round, &x.issue, &number, &x.r.State, &x.r.At, &x.auditID, &x.r.Note, &x.r.Alt,
			&x.r.Found, &x.r.Fixed, &x.r.Rejected, &x.features, &x.repo, &x.hd, &x.r.Tier, &x.r.Why, &x.r.Rule, &x.action); err != nil {
			_ = rows.Close()
			return err
		}
		x.r.Task = p.Key + "-" + itoa64(number)
		all = append(all, &x)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	// A branch shipped twice has a round for each: its pull request's
	// evidence counts once, on the newest (the rows are newest first).
	type branch struct {
		issue      int64
		repo, head string
	}
	seen := map[branch]bool{}
	for _, x := range all {
		recomputed := x.r.Tier == ""
		if recomputed {
			x.r.Tier = StakesUnknown
			var f TriageFeatures
			if x.features != "" && json.Unmarshal([]byte(x.features), &f) == nil {
				x.r.Tier, x.r.Why = changeStakes(&f)
			}
		}
		t := tiers[x.r.Tier]
		if t == nil {
			t = &StakesTierStats{Tier: x.r.Tier, States: map[string]int{}}
			tiers[x.r.Tier] = t
		}
		// A round triage skipped never gets a stored tier: it isn't one
		// from before tiers were stored.
		if recomputed && x.action != triageSkip {
			t.Recomputed++
		}
		if prev, ok := taskTier[x.issue]; !ok || stakesOrder(x.r.Tier) < stakesOrder(prev) {
			taskTier[x.issue] = x.r.Tier
		}
		t.Rounds++
		t.States[x.r.State]++
		t.Found += x.r.Found
		t.Fixed += x.r.Fixed
		t.Rejected += x.r.Rejected
		// The round's first audit (its own, or the pull request's it took),
		// the one asked after it, and its Sonnet reviewer.
		first, err := s.stakesAuditOf(ctx, `a.request_key = ?`, fmt.Sprintf(reviewAuditKey, x.r.Round))
		if err != nil {
			return err
		}
		used, err := s.stakesAuditOf(ctx, `a.id = ?`, x.auditID)
		if err != nil {
			return err
		}
		if first == nil {
			first = used
		}
		other, err := s.stakesAuditOf(ctx, `a.request_key = ?`, fmt.Sprintf(reviewOtherAuditKey, x.r.Round))
		if err != nil {
			return err
		}
		rv, err := s.stakesAuditOf(ctx, `a.request_key = ?`, fmt.Sprintf(reviewerAuditKey, x.r.Round))
		if err != nil {
			return err
		}
		if first != nil && first.weakest != "" && first.weakest != reviewFloorBar {
			t.Barred++
			x.r.Bar = first.weakest
			if first.status == AuditFailed || first.status == AuditLimited {
				t.BarFailed++
			}
		}
		// Asked again at the floor (before WP-1440: at no bar) after a tier
		// bar's audit.
		if other != nil && (other.weakest == "" || other.weakest == reviewFloorBar) && first != nil && first.weakest != "" && first.weakest != reviewFloorBar {
			t.FreeReasks++
		}
		slow := strings.HasPrefix(x.r.Note, roundAuditOutwaited)
		if slow && strings.Contains(x.r.Note, stakesNoClaude) {
			t.SlowHandOvers++
		}
		if rv != nil {
			x.r.Reviewer = rv.id
			t.Reviewers++
			if rv.status == AuditDone && !rv.nothing {
				t.ReviewersUsed++
			}
			if rv.startedAt > 0 && rv.finished > rv.startedAt {
				t.ReviewerMinutes += float64(rv.finished-rv.startedAt) / 60000
			}
			t.ReviewerInput += rv.input
			t.ReviewerOutput += rv.output
		}
		reviewerUsed := rv != nil && rv.status == AuditDone && !rv.nothing
		if x.r.Note != "" && !reviewerUsed && !strings.Contains(x.r.Note, stakesNoClaude) && x.r.State == RoundDone {
			t.OwnReviews++
		}
		// The free audit's answer: the one the round used, unless it went
		// to the pull request (then whichever of its asks answered).
		ans := used
		if ans == nil || ans.status != AuditDone {
			ans = other
		}
		if ans != nil && ans.status == AuditDone && !ans.nothing && ans.id != x.r.Reviewer {
			x.r.Audit, x.r.Model = ans.id, ans.model
			t.Answered++
			if ans.findings == 0 {
				t.Empty++
			}
			if ans.finished > 0 {
				t.auditMinutes = append(t.auditMinutes, float64(ans.finished-x.r.At)/60000)
			}
			k := [2]string{x.r.Tier, ans.model}
			m := models[k]
			if m == nil {
				m = &StakesModelStats{Tier: x.r.Tier, Model: ans.model}
				models[k] = m
			}
			m.Answered++
			if ans.findings == 0 {
				m.Empty++
			}
			m.Findings += ans.findings
			m.Fixed += ans.fixed
			m.Rejected += ans.rej
		}
		if b := (branch{x.issue, strings.ToLower(x.repo), x.hd}); x.repo != "" && !seen[b] {
			seen[b] = true
			useful, escapes, err := s.triageEvidence(ctx, x.issue, x.repo, x.hd)
			if err != nil {
				return err
			}
			x.r.Useful = useful
			t.Useful += useful
			t.Escapes += escapes
		}
		switch {
		case !withRows:
		case len(out.Rows) >= triageRowLimit:
			out.Truncated = true
		default:
			r := x.r
			out.Rows = append(out.Rows, &r)
		}
	}
	return nil
}

// stakesOrder ranks tiers, the highest first.
func stakesOrder(tier string) int {
	switch tier {
	case StakesHigh:
		return 0
	case StakesElevated:
		return 1
	case StakesNormal:
		return 2
	}
	return 3
}

// stakesExtraMetrics are the stakes report's measures of a task beyond the
// experiments' (experimentMetrics): what it led to after its rounds.
var stakesExtraMetrics = []ExperimentMetric{
	{Key: "followups", Label: "Follow-ups filed", Unit: "count"},
	{Key: "followup_fixes", Label: "Follow-up fixes filed", Unit: "count"},
}

// stakesOutcomes measures each tier's tasks the way experiments measure
// their arms (experimentMetrics: cost, tokens, turns, tool calls, active
// hours, review findings on the pull requests, babysit rounds...) and adds
// the follow-ups filed on them. It reads every person's usage, so only an
// admin gets it.
func (s *Service) stakesOutcomes(ctx context.Context, actor *Actor, days int, taskTier map[int64]string, tiers map[string]*StakesTierStats, out *ReviewStakesStats) error {
	if len(taskTier) == 0 {
		return nil
	}
	if !actor.isAdmin() {
		out.OutcomesNote = "task outcomes need an admin: they read every person's usage"
		return nil
	}
	byID := map[int64]*expTask{}
	byKey := map[string]*expTask{}
	ids := make([]int64, 0, len(taskTier))
	for id := range taskTier {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	extra := map[int64][2]float64{}
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		// #nosec G202 -- only placeholders are joined in
		rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, p.key || '-' || i.number, st.category,
				(SELECT count(*) FROM issue_relations r WHERE r.target_id = i.id AND r.kind = 'follows'),
				(SELECT count(*) FROM issue_relations r WHERE r.target_id = i.id AND r.kind = 'follows' AND r.subkind = 'fix')
			FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
			WHERE i.id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var k, cat string
			var fu, fx float64
			if err := rows.Scan(&id, &k, &cat, &fu, &fx); err != nil {
				_ = rows.Close()
				return err
			}
			t := &expTask{key: k, done: cat == CatDone}
			byID[id], byKey[k] = t, t
			extra[id] = [2]float64{fu, fx}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	// Usage from a week before the window, so a task whose round is early
	// in it is measured whole.
	tr, err := s.UsageTrends(ctx, actor, UsageQuery{Days: min(days+7, 365), User: "all", allTasks: true})
	if err != nil {
		return err
	}
	for _, r := range tr.Tasks {
		if t, ok := byKey[r.Key]; ok {
			t.row, t.measured = r, true
			t.done = r.Done || t.done
		}
	}
	if err := s.experimentReviewMeasures(ctx, byID); err != nil {
		return err
	}
	type acc struct{ values [][]float64 }
	n := len(experimentMetrics) + len(stakesExtraMetrics)
	per := map[string]*acc{}
	for id, t := range byID {
		tier := taskTier[id]
		a := per[tier]
		if a == nil {
			a = &acc{values: make([][]float64, n)}
			per[tier] = a
		}
		if tt := tiers[tier]; tt != nil {
			tt.Tasks++
		}
		if t.measured {
			for i, m := range experimentMetrics {
				if v, ok := m.get(t); ok {
					a.values[i] = append(a.values[i], v)
				}
			}
		}
		for j := range stakesExtraMetrics {
			a.values[len(experimentMetrics)+j] = append(a.values[len(experimentMetrics)+j], extra[id][j])
		}
	}
	for tier, a := range per {
		tt := tiers[tier]
		if tt == nil {
			continue
		}
		for i, v := range a.values {
			var m ExperimentMetric
			if i < len(experimentMetrics) {
				m = experimentMetrics[i].ExperimentMetric
			} else {
				m = stakesExtraMetrics[i-len(experimentMetrics)]
			}
			o := StakesOutcome{ExperimentMetric: m, N: len(v)}
			if len(v) > 0 {
				o.Median, o.Mean = median(v), mean(v)
			}
			tt.Outcomes = append(tt.Outcomes, o)
		}
	}
	return nil
}
