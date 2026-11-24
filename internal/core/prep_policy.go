package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	prepArmNone  = "none"
	prepArmChat  = "chat"
	prepArmAgent = "agent"

	// prepPolicyAge: the estimate is worked out again after this long even
	// if no trace landed, for the outcomes that come without one.
	prepPolicyAge = time.Hour
	// prepTraceSlack: a trace's take may be stamped this much before the
	// server recorded it (clocks, and the request's own time).
	prepTraceSlack = 10 * time.Minute
	// prepTraceOpen: a trace with no edit and no end counts as over when
	// nothing was added to it for this long.
	prepTraceOpen = 12 * time.Hour
	// prepSame: chat and agentic picks sharing this much of their files
	// (Jaccard) are the same pointers: drawing between them tells nothing.
	prepSame = 0.8
	// prepTracesWithin: an agent whose person's machines sent a trace
	// this recently can be measured; another's task is not a test.
	prepTracesWithin = 14 * 24 * time.Hour
	// prepDoneWithin: a task counts as done if it is within this long of
	// being taken.
	prepDoneWithin = 14 * 24 * time.Hour
	// prepGuardP and prepGuardMin: an arm is out while a guard says, with
	// this probability and on at least this many tasks of the arm, that it
	// is worse than nothing by more than the guard's margin.
	prepGuardP   = 0.975
	prepGuardMin = 20.0
	// prepSnapshotMove: a share moving this much is kept as history.
	prepSnapshotMove = 0.02
	// prepLabelMin: a project, type or model with fewer tasks shares the
	// baseline's column.
	prepLabelMin = 10
)

// prepArmDef is a kind of pointers. A changed prompt or pipeline is a new
// key (agent-2), tested as new; a key no longer here stays in the history
// and in the estimate, and is never drawn.
type prepArmDef struct {
	Key, Label string
}

var prepArmDefs = []prepArmDef{
	{prepArmNone, "no pointers"},
	{prepArmChat, "chat prep's picks"},
	{prepArmAgent, "agentic prep's picks"},
}

func prepArmLabel(key string) string {
	for _, d := range prepArmDefs {
		if d.Key == key {
			return d.Label
		}
	}
	return key + " (no longer drawn)"
}

// prepOutcome is what the policy is decided on, and the guards beside it.
// Every one is "lower is better".
type prepOutcome struct {
	Key, Label string
	Log        bool    // on the log scale: an effect is a ratio
	Spread     float64 // expected spread per task (fitPrepArms)
	Margin     float64 // a guard: worse by more than this takes an arm out
}

var (
	// The primary outcome is the task's goal as filed: fewer searches and
	// reads to find where to work. Spreads are those of 422 tasks' first
	// takes measured before this was built (page WP-X19): 1.01 for the
	// primary outcome, 0.89 once project, type, model and starting context
	// are taken out; 1.2 for the whole task's navigation.
	prepPrimary = prepOutcome{Key: "pre_nav", Label: "navigation calls before the first edit", Log: true, Spread: 0.9}
	prepGuards  = []prepOutcome{
		{Key: "nav", Label: "navigation calls, whole task", Log: true, Spread: 1.0, Margin: 0.10},
		{Key: "undone", Label: "tasks not done in 14 days", Spread: 0.45, Margin: 0.05},
		{Key: "findings", Label: "review findings on the task's pull requests", Log: true, Spread: 0.9, Margin: 0.10},
	}
)

// prepRow is one task an agent took with a drawn arm, and what came of it.
type prepRow struct {
	issue          int64
	arm            string
	same           bool
	textLen        int
	agentReady     bool
	exposed        int64
	started        int64
	project        int64
	typ            string
	resolved       int64
	done           bool
	traced, closed bool
	edited         bool
	model          string
	preNav, nav    float64
	ctxStart       float64
	prs            int
	findings       float64
	files          string
}

// prepGuardFit is one guard's estimate.
type prepGuardFit struct {
	prepOutcome
	fit   *prepMix
	tasks int
}

// prepPolicy is the estimate as last worked out.
type prepPolicy struct {
	at       time.Time
	arms     []string
	primary  *prepMix
	allTime  *prepFit
	guards   []prepGuardFit
	blocked  map[string]string
	offline  []PrepOfflineArm
	exposed  int
	measured int
	byArm    map[string]int
}

// prepPolicyNow is the estimate, worked out again if a trace landed since,
// or it is older than prepPolicyAge.
func (s *Service) prepPolicyNow(ctx context.Context) (*prepPolicy, error) {
	s.prepPolMu.Lock()
	defer s.prepPolMu.Unlock()
	now := s.now()
	if s.prepPol != nil && !s.prepPolStale && now.Sub(s.prepPol.at) < prepPolicyAge && !now.Before(s.prepPol.at) {
		return s.prepPol, nil
	}
	pol, err := s.prepPolicyCompute(ctx, now)
	if err != nil {
		return nil, err
	}
	s.prepPol, s.prepPolStale = pol, false
	s.prepSnapshot(ctx, pol)
	return pol, nil
}

// prepPolicyTouch marks the estimate out of date when one of the tasks has
// a drawn arm (a trace of it landed, or its picks were scored).
func (s *Service) prepPolicyTouch(ctx context.Context, issueIDs []int64) {
	if len(issueIDs) == 0 {
		return
	}
	args := make([]any, len(issueIDs))
	for i, id := range issueIDs {
		args[i] = id
	}
	var hit bool
	// #nosec G202 -- only placeholders are joined in
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM prep_assignments WHERE issue_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`))`, args...).Scan(&hit); err != nil || !hit {
		return
	}
	s.prepPolMu.Lock()
	s.prepPolStale = true
	s.prepPolMu.Unlock()
}

// prepRows reads the tasks agents took with a drawn arm. A task given the
// best arm because its agent couldn't be measured (policy best) was not
// drawn at random and is no evidence: it is left out of every estimate.
func (s *Service) prepRows(ctx context.Context, now time.Time) ([]*prepRow, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.issue_id, a.arm, a.same, a.text_len, a.agent_ready, a.exposed_at, i.project_id, i.type,
			coalesce(i.resolved_at, 0), coalesce(st.category = 'done', 0)
		FROM prep_assignments a JOIN issues i ON i.id = a.issue_id JOIN statuses st ON st.id = i.status_id
		WHERE a.exposed_at IS NOT NULL AND a.policy != 'best' ORDER BY a.exposed_at, a.issue_id`)
	if err != nil {
		return nil, err
	}
	var out []*prepRow
	by := map[int64]*prepRow{}
	for rows.Next() {
		r := &prepRow{}
		if err := rows.Scan(&r.issue, &r.arm, &r.same, &r.textLen, &r.agentReady, &r.exposed, &r.project, &r.typ, &r.resolved, &r.done); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		by[r.issue] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.R.QueryContext(ctx, `SELECT t.issue_id, t.started_at, t.last_at, t.ended, t.model, t.locate, t.infile, t.reads, t.delegated,
			t.pre_locate, t.pre_infile, t.pre_reads, t.pre_delegated, t.first_edit_at, t.ctx_start, t.files
		FROM task_traces t JOIN prep_assignments a ON a.issue_id = t.issue_id
		WHERE a.exposed_at IS NOT NULL AND a.policy != 'best' AND t.started_at >= a.exposed_at - ?
		ORDER BY t.issue_id, t.started_at, t.id`, prepTraceSlack.Milliseconds())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var issue, started, last, locate, infile, reads, delegated, preLocate, preInfile, preReads, preDelegated, ctxStart int64
		var ended, model, files string
		var firstEdit sql.NullInt64
		if err := rows.Scan(&issue, &started, &last, &ended, &model, &locate, &infile, &reads, &delegated,
			&preLocate, &preInfile, &preReads, &preDelegated, &firstEdit, &ctxStart, &files); err != nil {
			rows.Close()
			return nil, err
		}
		r := by[issue]
		if r == nil || r.traced {
			continue
		}
		r.traced, r.started = true, started
		r.edited = firstEdit.Valid
		r.closed = ended != "" || now.UnixMilli()-last >= prepTraceOpen.Milliseconds()
		r.model = model
		r.preNav = float64(preLocate) + float64(preInfile) + float64(preReads) + float64(preDelegated)
		r.nav = float64(locate) + float64(infile) + float64(reads) + float64(delegated)
		r.ctxStart = float64(ctxStart)
		r.files = files
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.R.QueryContext(ctx, `SELECT x.issue_id, COUNT(*), coalesce(SUM(x.n), 0) FROM (
			SELECT DISTINCT el.issue_id, p.id, (SELECT COUNT(*) FROM bot_findings f WHERE f.pr_id = p.id AND f.state != 'withdrawn') AS n
			FROM prep_assignments a JOIN external_links el ON el.issue_id = a.issue_id
			JOIN pull_requests p ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
			WHERE a.exposed_at IS NOT NULL AND a.policy != 'best') x GROUP BY x.issue_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var prs int
		var n float64
		if err := rows.Scan(&id, &prs, &n); err != nil {
			rows.Close()
			return nil, err
		}
		if r := by[id]; r != nil {
			r.prs, r.findings = prs, n
		}
	}
	rows.Close()
	return out, rows.Err()
}

// at is when the task's evidence is from: its take, or (not taken yet) when
// its pointers were first printed.
func (r *prepRow) at() int64 {
	if r.traced {
		return r.started
	}
	return r.exposed
}

// prepValue is a row's value of an outcome, when it has one yet.
func prepValue(o *prepOutcome, r *prepRow, now time.Time) (float64, bool) {
	switch o.Key {
	case "pre_nav":
		return math.Log1p(r.preNav), r.traced && (r.edited || r.closed)
	case "nav":
		return math.Log1p(r.nav), r.traced && r.closed
	case "undone":
		if !r.traced {
			return 0, false
		}
		in := r.done && r.resolved > 0 && r.resolved-r.started <= prepDoneWithin.Milliseconds()
		if in {
			return 0, true
		}
		return 1, now.UnixMilli()-r.started > prepDoneWithin.Milliseconds()
	case "findings":
		return math.Log1p(r.findings), r.traced && r.done && r.prs > 0
	}
	return 0, false
}

// prepObservations turns the rows that have the outcome into observations:
// the covariates are what was known of each task before its arm was drawn
// (project, type, the taking agent's model, the task text's length, the
// context the session already held, whether agentic picks were ready, and
// when), so the arms are compared like with like.
func prepObservations(o *prepOutcome, arms []string, rows []*prepRow, now time.Time) ([]prepObs, []*prepRow) {
	var use []*prepRow
	var ys []float64
	for _, r := range rows {
		if y, ok := prepValue(o, r, now); ok {
			use = append(use, r)
			ys = append(ys, y)
		}
	}
	n := len(use)
	label := func(f func(*prepRow) string) [][]float64 {
		v := make([]string, n)
		for i, r := range use {
			v[i] = f(r)
		}
		return prepDummies(v, prepLabelMin)
	}
	num := func(f func(*prepRow) float64) []float64 {
		v := make([]float64, n)
		for i, r := range use {
			v[i] = f(r)
		}
		return prepStandard(v)
	}
	projects := label(func(r *prepRow) string { return fmt.Sprint(r.project) })
	types := label(func(r *prepRow) string { return r.typ })
	models := label(func(r *prepRow) string { return r.model })
	text := num(func(r *prepRow) float64 { return math.Log1p(float64(r.textLen)) })
	held := num(func(r *prepRow) float64 { return math.Log1p(r.ctxStart) })
	when := num(func(r *prepRow) float64 { return float64(r.at()) })
	ready := num(func(r *prepRow) float64 { return float64(b2i(r.agentReady)) })
	obs := make([]prepObs, n)
	for i, r := range use {
		x := []float64{1}
		x = append(x, projects[i]...)
		x = append(x, types[i]...)
		x = append(x, models[i]...)
		x = append(x, text[i], held[i], when[i], ready[i])
		load := make([]float64, len(arms))
		for j, a := range arms {
			switch {
			case r.same && (r.arm == prepArmChat || r.arm == prepArmAgent) && (a == prepArmChat || a == prepArmAgent):
				load[j] = 0.5
			case !r.same && a == r.arm:
				load[j] = 1
			}
		}
		age := max(float64(now.UnixMilli()-r.at())/float64((24*time.Hour).Milliseconds()), 0)
		obs[i] = prepObs{Y: ys[i], X: x, Arm: load, Age: age, W: 1}
	}
	return obs, use
}

// prepPolicyCompute works the estimate out from every row.
func (s *Service) prepPolicyCompute(ctx context.Context, now time.Time) (*prepPolicy, error) {
	rows, err := s.prepRows(ctx, now)
	if err != nil {
		return nil, err
	}
	pol := &prepPolicy{at: now, blocked: map[string]string{}, exposed: len(rows), byArm: map[string]int{}}
	for _, d := range prepArmDefs {
		if d.Key != prepArmNone {
			pol.arms = append(pol.arms, d.Key)
		}
	}
	for _, r := range rows {
		if r.arm != prepArmNone && !slices.Contains(pol.arms, r.arm) {
			pol.arms = append(pol.arms, r.arm)
		}
	}
	obs, used := prepObservations(&prepPrimary, pol.arms, rows, now)
	pol.primary = fitPrepMix(pol.arms, obs, prepPrimary.Spread, nil)
	pol.measured = len(used)
	for _, r := range used {
		pol.byArm[r.arm]++
	}
	pol.allTime = fitPrepArms(pol.arms, prepAged(obs, 0, 0), prepPrimary.Spread)
	for i := range prepGuards {
		g := prepGuards[i]
		obs, used := prepObservations(&g, pol.arms, rows, now)
		gf := prepGuardFit{prepOutcome: g, fit: fitPrepMix(pol.arms, obs, g.Spread, pol.primary.W), tasks: len(used)}
		pol.guards = append(pol.guards, gf)
		for j, arm := range pol.arms {
			if _, out := pol.blocked[arm]; out || gf.fit.armN(j) < prepGuardMin {
				continue
			}
			if gf.fit.pWorse(j, g.Margin) > prepGuardP {
				mean, _ := gf.fit.effect(j)
				lo, hi := gf.fit.interval(j)
				pol.blocked[arm] = fmt.Sprintf("worse than no pointers on %s: %s", g.Label, prepEffectText(&g, mean, lo, hi))
			}
		}
	}
	if pol.offline, err = s.prepOffline(ctx, rows); err != nil {
		return nil, err
	}
	for _, o := range pol.offline {
		if _, out := pol.blocked[o.Arm]; !out && o.Screen != "" {
			pol.blocked[o.Arm] = o.Screen
		}
	}
	return pol, nil
}

// prepEffectText is an effect and its 90% interval, as a person reads it:
// a ratio's as a percentage, a rate's in points.
func prepEffectText(o *prepOutcome, mean, lo, hi float64) string {
	if o.Log {
		pct := func(v float64) float64 { return (math.Exp(v) - 1) * 100 }
		return fmt.Sprintf("%+.0f%% [%+.0f%%, %+.0f%%]", pct(mean), pct(lo), pct(hi))
	}
	return fmt.Sprintf("%+.0f points [%+.0f, %+.0f]", mean*100, lo*100, hi*100)
}

// prepCandidate is something a task may be given.
type prepCandidate struct {
	arm  string
	load []float64
}

// candidates are what a task with these arms' picks may be given: nothing,
// and each arm that is drawn, has picks and isn't out. When the chat and
// agentic picks are the same files they are one candidate (the chat
// prep's, which is made anyway), loaded on both.
func (p *prepPolicy) candidates(avail map[string]bool, same bool) []prepCandidate {
	out := []prepCandidate{{arm: prepArmNone, load: make([]float64, len(p.arms))}}
	for _, d := range prepArmDefs {
		j := slices.Index(p.arms, d.Key)
		if d.Key == prepArmNone || j < 0 || !avail[d.Key] || p.blocked[d.Key] != "" {
			continue
		}
		if same && d.Key == prepArmAgent {
			continue
		}
		load := make([]float64, len(p.arms))
		load[j] = 1
		if k := slices.Index(p.arms, prepArmAgent); same && d.Key == prepArmChat && k >= 0 {
			load[j], load[k] = 0.5, 0.5
		}
		out = append(out, prepCandidate{arm: d.Key, load: load})
	}
	return out
}

// shares are the candidates' shares of new tasks.
func (p *prepPolicy) shares(cands []prepCandidate) []float64 {
	loads := make([][]float64, len(cands))
	for i, c := range cands {
		loads[i] = c.load
	}
	return prepShares(p.primary.pBest(loads), nil)
}

// prepBestTie: shares this close don't tell two arms apart.
const prepBestTie = 0.05

// prepBest is the candidate with the largest share; among those within
// prepBestTie of it, the pointers agents have had so far (the chat prep's)
// before the agentic ones, and those before nothing.
func prepBest(cands []prepCandidate, shares []float64) int {
	top := slices.Max(shares)
	rank := func(arm string) int {
		switch arm {
		case prepArmChat:
			return 0
		case prepArmNone:
			return 2
		}
		return 1
	}
	at := -1
	for n, c := range cands {
		if shares[n] <= 0 || shares[n] < top-prepBestTie {
			continue
		}
		if at < 0 || rank(c.arm) < rank(cands[at].arm) {
			at = n
		}
	}
	return max(at, 0)
}

// prepAssignment is a task's drawn arm.
type prepAssignment struct {
	Arm     string
	Policy  string
	Exposed bool
}

func (s *Service) prepAssignmentOf(ctx context.Context, issueID int64) (*prepAssignment, error) {
	var a prepAssignment
	var exposed sql.NullInt64
	err := s.db.R.QueryRowContext(ctx, `SELECT arm, policy, exposed_at FROM prep_assignments WHERE issue_id = ?`, issueID).Scan(&a.Arm, &a.Policy, &exposed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Exposed = exposed.Valid
	return &a, nil
}

// prepPickFiles are the files some notes point at.
func prepPickFiles(n *PrepNotes) map[string]bool {
	out := map[string]bool{}
	if n == nil {
		return out
	}
	for _, f := range n.Files {
		out[f.Path] = true
	}
	for _, y := range n.Symbols {
		out[y.Path] = true
	}
	return out
}

// prepSamePicks says whether two sets of picks are the same pointers.
func prepSamePicks(a, b map[string]bool) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	both := 0
	for f := range a {
		if b[f] {
			both++
		}
	}
	return float64(both)/float64(len(a)+len(b)-both) >= prepSame
}

// prepMeasurable says whether what the agent does with a task will be
// seen: a machine of its person sent a navigation trace lately. Another's
// task tells nothing, so it is given the best arm, not a drawn one.
func (s *Service) prepMeasurable(ctx context.Context, a *Actor) bool {
	if a == nil || a.User == nil {
		return false
	}
	var ok bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM task_traces WHERE user_id = ? AND updated_at >= ?)`,
		a.User.ID, s.now().Add(-prepTracesWithin).UnixMilli()).Scan(&ok); err != nil {
		return false
	}
	return ok
}

// prepTextLen is the length of a task's text in characters, as migration
// 0157 counted it for the tasks it imported.
func prepTextLen(i *Issue) int {
	return utf8.RuneCountInString(i.Title) + utf8.RuneCountInString(i.Description)
}

// prepDrawArm draws a task's arm, the first time an agent is given its
// ready prep, and keeps it. nil when the task has no picks of any arm:
// there is nothing to choose between.
func (s *Service) prepDrawArm(ctx context.Context, a *Actor, i *Issue, chat, agent *PrepNotes) (*prepAssignment, error) {
	cf, af := prepPickFiles(chat), prepPickFiles(agent)
	avail := map[string]bool{prepArmChat: len(cf) > 0, prepArmAgent: len(af) > 0}
	if !avail[prepArmChat] && !avail[prepArmAgent] {
		return nil, nil
	}
	pol, err := s.prepPolicyNow(ctx)
	if err != nil {
		return nil, err
	}
	same := prepSamePicks(cf, af)
	cands := pol.candidates(avail, same)
	shares := pol.shares(cands)
	at, policy := 0, "thompson"
	if s.prepMeasurable(ctx, a) {
		at = prepPick(shares, prepDraw(i.ID))
	} else {
		policy = "best"
		at = prepBest(cands, shares)
	}
	propensity := shares[at]
	if policy == "best" {
		propensity = 1
	}
	kept := map[string]float64{}
	for n, c := range cands {
		kept[c.arm] = math.Round(shares[n]*1e4) / 1e4
	}
	raw, _ := json.Marshal(kept)
	now := s.nowMS()
	arm := cands[at].arm
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO prep_assignments(issue_id, arm, policy, propensity, shares, same, text_len, agent_ready, assigned_at)
		VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(issue_id) DO NOTHING`,
		i.ID, arm, policy, propensity, string(raw), b2i(same), prepTextLen(i), b2i(avail[prepArmAgent]), now); err != nil {
		return nil, err
	}
	got, err := s.prepAssignmentOf(ctx, i.ID)
	if err != nil || got == nil {
		return nil, err
	}
	if got.Arm == prepArmNone {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE task_preps SET held_out = 1 WHERE issue_id = ? AND NOT held_out`, i.ID); err != nil {
			return nil, err
		}
	}
	return got, nil
}

// prepPolicyApply gives an agent the pointers of the task's arm, drawing
// the arm if it has none, and with exposed records that an agent taking
// the task was given them. p is the prep as an agent may see it (notes
// already left out when held out).
func (s *Service) prepPolicyApply(ctx context.Context, a *Actor, i *Issue, p *TaskPrep, exposed bool) {
	if p.State != "ready" {
		return
	}
	if a.ReadOnly {
		if got, err := s.prepAssignmentOf(ctx, i.ID); err != nil || got == nil {
			return
		}
	}
	got, err := s.prepAssignmentOf(ctx, i.ID)
	if err != nil {
		s.log.WarnContext(ctx, "prep policy: read the task's arm", "issue", i.ID, "err", err)
		return
	}
	agent, model, _ := s.agentPrepNotes(ctx, i.ID)
	if got == nil && !p.HeldOut {
		if got, err = s.prepDrawArm(ctx, a, i, p.Notes, agent); err != nil {
			s.log.WarnContext(ctx, "prep policy: draw the task's arm", "issue", i.ID, "err", err)
			return
		}
	}
	if got == nil && p.HeldOut {
		if _, err := s.db.W.ExecContext(ctx, `INSERT INTO prep_assignments(issue_id, arm, policy, propensity, text_len, assigned_at)
			VALUES (?,?,?,?,?,?) ON CONFLICT(issue_id) DO NOTHING`, i.ID, prepArmNone, "import", 0.2, prepTextLen(i), s.nowMS()); err != nil {
			s.log.WarnContext(ctx, "prep policy: keep a held-out task's arm", "issue", i.ID, "err", err)
			return
		}
		got = &prepAssignment{Arm: prepArmNone, Policy: "import"}
	}
	if got == nil {
		return
	}
	if exposed && !got.Exposed {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE prep_assignments SET exposed_at = ? WHERE issue_id = ? AND exposed_at IS NULL`, s.nowMS(), i.ID); err != nil {
			s.log.WarnContext(ctx, "prep policy: note the exposure", "issue", i.ID, "err", err)
		}
	}
	switch got.Arm {
	case prepArmNone:
		p.Notes, p.HeldOut = nil, true
	case prepArmAgent:
		if agent == nil || p.Notes == nil || len(prepPickFiles(agent)) == 0 {
			return
		}
		notes := *p.Notes
		notes.Files, notes.Symbols, notes.Tests, notes.Questions = agent.Files, agent.Symbols, agent.Tests, agent.Questions
		p.Notes, p.Source = &notes, prepArmAgent
		if model != "" {
			p.Model = model
		}
	}
}

// agentPrepNotes is a task's agentic picks, when it has some made from the
// prep's present text.
func (s *Service) agentPrepNotes(ctx context.Context, issueID int64) (*PrepNotes, string, bool) {
	var notes, model string
	err := s.db.R.QueryRowContext(ctx, `SELECT t.notes, t.model FROM prep_trials t JOIN task_preps tp ON tp.issue_id = t.issue_id AND tp.text_hash = t.text_hash
		WHERE t.issue_id = ? AND t.kind = ? AND t.error = '' ORDER BY t.created_at DESC LIMIT 1`, issueID, prepAgentKind).Scan(&notes, &model)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "prep policy: read the agentic picks", "issue", issueID, "err", err)
		}
		return nil, "", false
	}
	var n PrepNotes
	if json.Unmarshal([]byte(notes), &n) != nil {
		return nil, "", false
	}
	return &n, strings.TrimPrefix(model, prepAgentModelPrefix), true
}

// PrepPolicyArm is one arm in the policy's report.
type PrepPolicyArm struct {
	Arm   string `json:"arm"`
	Label string `json:"label"`
	// Tasks: measured tasks of the arm; N: what they count for now, aged.
	Tasks int     `json:"tasks"`
	N     float64 `json:"n"`
	// Share of new tasks with every arm's picks ready; PBest before
	// anything was taken out.
	Share float64 `json:"share"`
	PBest float64 `json:"p_best"`
	// Effect against no pointers on the primary outcome's scale (log:
	// -0.1 is about 10% fewer), with its 90% interval; AllTime the same
	// with no task aged.
	Effect  float64 `json:"effect"`
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
	AllTime float64 `json:"all_time"`
	Text    string  `json:"text,omitempty"`
	Out     string  `json:"out,omitempty"` // why it gets no tasks
}

// PrepPolicyGuard is one guard's figures for one arm.
type PrepPolicyGuard struct {
	Guard string  `json:"guard"`
	Label string  `json:"label"`
	Tasks int     `json:"tasks"`
	Arm   string  `json:"arm"`
	N     float64 `json:"n"`
	Text  string  `json:"text"`
	Worse float64 `json:"p_worse"` // probability it is worse than no pointers by more than the margin
}

// PrepPolicySnapshot is the policy when its shares last moved.
type PrepPolicySnapshot struct {
	At     time.Time          `json:"at"`
	Shares map[string]float64 `json:"shares"`
	Reason string             `json:"reason"`
}

// PrepPolicyAgeing is one half-life (days) and its weight in the estimate.
type PrepPolicyAgeing struct {
	HalfLife float64 `json:"half_life"`
	Weight   float64 `json:"weight"`
}

// PrepPolicyReport is what agents are shown of a prep, and why.
type PrepPolicyReport struct {
	At       time.Time `json:"at"`
	Outcome  string    `json:"outcome"`
	Exposed  int       `json:"exposed"`  // tasks agents took with a drawn arm
	Measured int       `json:"measured"` // of them, with the outcome
	// Spread is one task's outcome's spread once the covariates are taken
	// out; Tells the smallest difference from no pointers the present data
	// would show (as a share: 0.12 is 12%).
	Spread float64 `json:"spread"`
	Tells  float64 `json:"tells"`
	// Ageing: how fast evidence ages, as the forecasts weigh the half-lives.
	Ageing  []PrepPolicyAgeing   `json:"ageing"`
	Arms    []PrepPolicyArm      `json:"arms"`
	Guards  []PrepPolicyGuard    `json:"guards"`
	Offline []PrepOfflineArm     `json:"offline"`
	History []PrepPolicySnapshot `json:"history"`
}

// armReport is the arms as the report and the snapshots list them.
func (p *prepPolicy) armReport() []PrepPolicyArm {
	avail := map[string]bool{}
	for _, a := range p.arms {
		avail[a] = true
	}
	open := *p
	open.blocked = map[string]string{}
	all := open.candidates(avail, false)
	pb := p.primary.pBest(func() [][]float64 {
		l := make([][]float64, len(all))
		for i, c := range all {
			l[i] = c.load
		}
		return l
	}())
	cands := p.candidates(avail, false)
	shares := p.shares(cands)
	k := len(p.arms)
	out := []PrepPolicyArm{{Arm: prepArmNone, Label: prepArmLabel(prepArmNone), Tasks: p.byArm[prepArmNone], N: p.primary.armN(k)}}
	for j, arm := range p.arms {
		mean, _ := p.primary.effect(j)
		lo, hi := p.primary.interval(j)
		a := PrepPolicyArm{Arm: arm, Label: prepArmLabel(arm), Tasks: p.byArm[arm], N: p.primary.armN(j), Effect: mean, Lo: lo, Hi: hi,
			AllTime: p.allTime.Mean[j], Text: prepEffectText(&prepPrimary, mean, lo, hi), Out: p.blocked[arm]}
		if a.Out == "" && !slices.ContainsFunc(prepArmDefs, func(d prepArmDef) bool { return d.Key == arm }) {
			a.Out = "no longer drawn"
		}
		out = append(out, a)
	}
	for i := range out {
		for n, c := range all {
			if c.arm == out[i].Arm {
				out[i].PBest = pb[n]
			}
		}
		for n, c := range cands {
			if c.arm == out[i].Arm {
				out[i].Share = shares[n]
			}
		}
	}
	return out
}

// prepSnapshot keeps the policy as history when a share moved by more than
// prepSnapshotMove since the last one kept, or an arm went out or came back.
func (s *Service) prepSnapshot(ctx context.Context, p *prepPolicy) {
	arms := p.armReport()
	shares := map[string]float64{}
	for _, a := range arms {
		shares[a.Arm] = math.Round(a.Share*1e4) / 1e4
	}
	var lastShares, lastArms string
	err := s.db.R.QueryRowContext(ctx, `SELECT shares, arms FROM prep_policy_snapshots ORDER BY id DESC LIMIT 1`).Scan(&lastShares, &lastArms)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.WarnContext(ctx, "prep policy: read the last snapshot", "err", err)
		return
	}
	var reasons []string
	if err == nil {
		was := map[string]float64{}
		var wasArms []PrepPolicyArm
		_ = json.Unmarshal([]byte(lastShares), &was)
		_ = json.Unmarshal([]byte(lastArms), &wasArms)
		out := map[string]string{}
		for _, a := range wasArms {
			out[a.Arm] = a.Out
		}
		for _, a := range arms {
			switch {
			case a.Out != "" && out[a.Arm] == "":
				reasons = append(reasons, a.Arm+" out: "+a.Out)
			case a.Out == "" && out[a.Arm] != "":
				reasons = append(reasons, a.Arm+" back")
			case math.Abs(shares[a.Arm]-was[a.Arm]) > prepSnapshotMove:
				reasons = append(reasons, fmt.Sprintf("%s %.0f%% to %.0f%%", a.Arm, was[a.Arm]*100, shares[a.Arm]*100))
			}
		}
		if len(reasons) == 0 {
			return
		}
	} else {
		reasons = []string{"the policy's first estimate"}
	}
	rawShares, _ := json.Marshal(shares)
	rawArms, _ := json.Marshal(arms)
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO prep_policy_snapshots(at, shares, arms, reason) VALUES (?,?,?,?)`,
		p.at.UnixMilli(), string(rawShares), string(rawArms), strings.Join(reasons, "; ")); err != nil {
		s.log.WarnContext(ctx, "prep policy: keep a snapshot", "err", err)
	}
}

// prepPolicyReport reads the report (for wp ai surplus).
func (s *Service) prepPolicyReport(ctx context.Context) (*PrepPolicyReport, error) {
	p, err := s.prepPolicyNow(ctx)
	if err != nil {
		return nil, err
	}
	rep := &PrepPolicyReport{At: p.at, Outcome: prepPrimary.Label, Exposed: p.exposed, Measured: p.measured, Spread: p.primary.sigma(),
		Arms: p.armReport(), Guards: []PrepPolicyGuard{}, Offline: p.offline, History: []PrepPolicySnapshot{}}
	for h, hl := range p.primary.HalfLives {
		rep.Ageing = append(rep.Ageing, PrepPolicyAgeing{HalfLife: hl, Weight: p.primary.W[h]})
	}
	narrow := math.Inf(1)
	for j := range p.arms {
		if _, sd := p.primary.effect(j); p.primary.armN(j) > 0 {
			narrow = min(narrow, sd)
		}
	}
	if !math.IsInf(narrow, 1) {
		rep.Tells = 1 - math.Exp(-1.645*narrow)
	}
	for _, g := range p.guards {
		for j, arm := range p.arms {
			if g.fit.armN(j) == 0 {
				continue
			}
			mean, _ := g.fit.effect(j)
			lo, hi := g.fit.interval(j)
			o := g.prepOutcome
			rep.Guards = append(rep.Guards, PrepPolicyGuard{Guard: g.Key, Label: g.Label, Tasks: g.tasks, Arm: arm, N: g.fit.armN(j),
				Text: prepEffectText(&o, mean, lo, hi), Worse: g.fit.pWorse(j, g.Margin)})
		}
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT at, shares, reason FROM prep_policy_snapshots ORDER BY id DESC LIMIT 8`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		var raw string
		h := PrepPolicySnapshot{Shares: map[string]float64{}}
		if err := rows.Scan(&at, &raw, &h.Reason); err != nil {
			return nil, err
		}
		h.At = msTime(at)
		_ = json.Unmarshal([]byte(raw), &h.Shares)
		rep.History = append(rep.History, h)
	}
	return rep, rows.Err()
}

// String is the report as wp ai surplus prints it: figures, no verdict.
func (r *PrepPolicyReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nWhat agents are shown of a prep (WP-ADR104), decided on %s:\n", r.Outcome)
	fmt.Fprintf(&b, "  %d tasks taken with a drawn arm, %d measured (a navigation trace of the take).", r.Exposed, r.Measured)
	if r.Tells > 0 {
		fmt.Fprintf(&b, " One task varies by a factor of %.1f; the data would show a difference of %.0f%% or more.", math.Exp(r.Spread), r.Tells*100)
	}
	b.WriteString("\n")
	if len(r.Ageing) > 0 && r.Measured > 0 {
		fmt.Fprintf(&b, "  A task counts half after (by how well each forecast the last %d days):", prepForecastDays)
		for _, a := range r.Ageing {
			fmt.Fprintf(&b, " %.0fd %.2f", a.HalfLife, a.Weight)
		}
		b.WriteString("\n")
	}
	for _, a := range r.Arms {
		fmt.Fprintf(&b, "  %-22s %3.0f%% of new tasks", a.Label, a.Share*100)
		if a.Arm != prepArmNone {
			fmt.Fprintf(&b, "  %s against no pointers", a.Text)
			if math.Abs(a.AllTime-a.Effect) > 0.05 {
				fmt.Fprintf(&b, " (%+.0f%% with no task aged)", (math.Exp(a.AllTime)-1)*100)
			}
		}
		fmt.Fprintf(&b, "  best with probability %.2f, %d tasks (count as %.0f)", a.PBest, a.Tasks, a.N)
		if a.Out != "" {
			fmt.Fprintf(&b, "\n  %-22s out: %s", "", a.Out)
		}
		b.WriteString("\n")
	}
	if len(r.Guards) > 0 {
		b.WriteString("  Guards, against no pointers (an arm is out at 0.975 on 20 tasks):\n")
		for _, g := range r.Guards {
			fmt.Fprintf(&b, "    %-10s %-44s %s, worse with probability %.2f (%.0f tasks)\n", g.Arm, g.Label, g.Text, g.Worse, g.N)
		}
	}
	if len(r.Offline) > 0 {
		b.WriteString("  Picks scored without showing them:\n")
		for _, o := range r.Offline {
			b.WriteString(o.String())
		}
	}
	if len(r.History) > 0 {
		b.WriteString("  When the shares moved, newest first:\n")
		for _, h := range r.History {
			fmt.Fprintf(&b, "    %s  %s\n", h.At.Format("2006-01-02 15:04"), h.Reason)
		}
	}
	return b.String()
}
