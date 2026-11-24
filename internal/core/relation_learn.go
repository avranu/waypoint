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
	"sort"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/semantic"
)

// The ranker only reorders and thins what the blend shows; people decide only on shown pairs, so their decisions say nothing below the blend's line.

const (
	// rankerMinTrain: decisions from before the held-out month the fit
	// needs; a few hundred, as WP-958 asked.
	rankerMinTrain = 200
	// rankerMinTest: decisions in the held-out month to judge it on.
	rankerMinTest = 50
	// rankerMinClass: accepted and not, each, in either part.
	rankerMinClass = 10
	// rankerHoldout is the month the fit is judged on.
	rankerHoldout = 30 * 24 * time.Hour
	// rankerIgnoredAfter: a suggestion shown this long ago with no
	// decision was ignored.
	rankerIgnoredAfter = 24 * time.Hour
	// rankerRefit: fits are this far apart.
	rankerRefit = 24 * time.Hour
	// rankerRecall: the line keeps this share of what people accepted.
	rankerRecall = 0.95
	// rankerPool: the most candidates, best by the blend, the ranker
	// scores for one issue.
	rankerPool = 40
	// rankerResamples and rankerWinShare: the model is adopted when it
	// beats the blend's held-out AUC in at least this share of paired
	// resamples of the held-out month (a fixed seed, so a fit is
	// repeatable).
	rankerResamples = 500
	rankerWinShare  = 0.9
	// rankerL2 keeps weights small when few decisions back them.
	rankerL2 = 1.0
)

// Suggestion sources, as relation_suggestions.source records them. Only
// words and concepts come from scores the ranker learns about.
const (
	sugText     = "text"
	sugForge    = "forge"
	sugTitle    = "title"
	sugWords    = "words"
	sugConcepts = "concepts"
	// sugUnrecorded: dismissed with no recorded look (its recording
	// failed); kept to keep the pair apart, never trained on.
	sugUnrecorded = "unrecorded"
)

// relFeatures are what the ranker knows of a suggested pair: the scores
// the blend uses, and how the two issues sit.
type relFeatures struct {
	Word, Concept                                  float64
	HasConcept                                     bool
	SameProject, SharedParent, SharedGoal, SrcDone bool
	AgeDays                                        float64
}

// relFeatureNames name x's entries, for the weights a fit records.
var relFeatureNames = []string{"word", "concept", "same_project", "shared_parent", "shared_goal", "log_age_days", "source_done"}

func (f relFeatures) x() []float64 {
	return []float64{f.Word, f.Concept, b2f(f.SameProject), b2f(f.SharedParent), b2f(f.SharedGoal), math.Log1p(f.AgeDays), b2f(f.SrcDone)}
}

// blend is the fixed score (WP-ADR91) the ranker has to beat.
func (f relFeatures) blend() float64 { return f.Word + simBlendWeight*f.Concept }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// pairInfo is what features need of one issue.
type pairInfo struct {
	project, parent, created, goal int64
}

// pairFeatures works out features for the issue i (text vectors src and
// concept) against each of ids.
func (s *Service) pairFeatures(ctx context.Context, idx *simIndex, i *Issue, src map[string]float64, concept []float64, ids []int64) (map[int64]relFeatures, error) {
	all := append([]int64{i.ID}, ids...)
	ph := strings.TrimSuffix(strings.Repeat("?,", len(all)), ",")
	args := make([]any, len(all))
	for n, id := range all {
		args[n] = id
	}
	// #nosec G202 -- placeholders only; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, i.project_id, coalesce(i.parent_id, 0), i.created_at, coalesce(gt.goal_id, 0)
		FROM issues i LEFT JOIN goal_traces gt ON gt.item_type = 'issue' AND gt.item_id = i.id WHERE i.id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	info := make(map[int64]pairInfo, len(all))
	for rows.Next() {
		var id int64
		var p pairInfo
		if err := rows.Scan(&id, &p.project, &p.parent, &p.created, &p.goal); err != nil {
			return nil, err
		}
		info[id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	me := info[i.ID]
	done := i.Status.Category == "done" || i.Status.Category == "canceled"
	out := make(map[int64]relFeatures, len(ids))
	for _, id := range ids {
		o, ok := info[id]
		if !ok {
			continue
		}
		f := relFeatures{
			SameProject:  o.project == me.project,
			SharedParent: me.parent != 0 && o.parent == me.parent,
			SharedGoal:   me.goal != 0 && o.goal == me.goal,
			SrcDone:      done,
			AgeDays:      math.Abs(float64(o.created-me.created)) / float64(24*time.Hour/time.Millisecond),
		}
		if d := idx.docs[id]; d != nil {
			f.Word = cosine(src, d.vec)
			if concept != nil && d.concept != nil {
				f.Concept, f.HasConcept = semantic.Cos(concept, d.concept), true
			}
		}
		out[id] = f
	}
	return out, nil
}

// dismissedRelations are the issues a person said aren't related to this
// one, from either end.
func (s *Service) dismissedRelations(ctx context.Context, issueID int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT CASE WHEN issue_id = ? THEN other_id ELSE issue_id END FROM relation_suggestions
		WHERE outcome = 'dismissed' AND (issue_id = ? OR other_id = ?)`, issueID, issueID, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// noteShown records the suggestions a person was shown on issue i. A pair
// shown again with no decision yet takes the new features and time; one
// decided keeps its decision. A failure is logged: the person still gets
// the suggestions.
func (s *Service) noteShown(ctx context.Context, actor *Actor, idx *simIndex, i *Issue, src map[string]float64, concept []float64, shown []RelationSuggestion, probs map[int64]float64) {
	ids := make([]int64, len(shown))
	for n, r := range shown {
		ids[n] = r.id
	}
	feats, err := s.pairFeatures(ctx, idx, i, src, concept, ids)
	if err == nil {
		err = s.recordShown(ctx, actor, i.ID, shown, feats, probs)
	}
	if err != nil {
		s.log.WarnContext(ctx, "relation suggestions not recorded", "issue", i.Key, "err", err)
	}
}

func (s *Service) recordShown(ctx context.Context, actor *Actor, issueID int64, shown []RelationSuggestion, feats map[int64]relFeatures, probs map[int64]float64) error {
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.nowMS()
	for _, r := range shown {
		f, ok := feats[r.id]
		if !ok {
			continue
		}
		var conceptCol, pCol any
		if f.HasConcept {
			conceptCol = f.Concept
		}
		if p, ok := probs[r.id]; ok {
			pCol = p
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO relation_suggestions(issue_id, other_id, kind, source, confidence, shown_by, shown_at,
				word, concept, same_project, shared_parent, shared_goal, age_days, source_done, blend, model_p)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(issue_id, other_id) DO UPDATE SET kind = excluded.kind, source = excluded.source, confidence = excluded.confidence,
				shown_by = excluded.shown_by, shown_at = excluded.shown_at, times_shown = times_shown + 1, word = excluded.word,
				concept = excluded.concept, same_project = excluded.same_project, shared_parent = excluded.shared_parent,
				shared_goal = excluded.shared_goal, age_days = excluded.age_days, source_done = excluded.source_done,
				blend = excluded.blend, model_p = excluded.model_p
			WHERE relation_suggestions.outcome IS NULL`,
			issueID, r.id, r.Kind, r.source, r.Confidence, nullUser(actor.id()), now,
			f.Word, conceptCol, f.SameProject, f.SharedParent, f.SharedGoal, f.AgeDays, f.SrcDone, f.blend(), pCol); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// noteRelated accepts suggestions of a pair someone has now related by
// choice: whoever relates two issues says they are related, whichever end
// it was suggested on, and even after a dismissal.
func (s *Service) noteRelated(ctx context.Context, t *tx, actor *Actor, a, b int64) error {
	_, err := t.ExecContext(ctx, `UPDATE relation_suggestions SET outcome = 'accepted', decided_at = ?, decided_by = ?
		WHERE ((issue_id = ? AND other_id = ?) OR (issue_id = ? AND other_id = ?)) AND coalesce(outcome, '') != 'accepted'`,
		s.nowMS(), nullUser(actor.id()), a, b, b, a)
	return err
}

// unnoteRelated undoes noteRelated once no relation is left between a and
// b: a removed relation no longer says a person took the suggestion.
func (s *Service) unnoteRelated(ctx context.Context, t *tx, a, b int64) error {
	_, err := t.ExecContext(ctx, `UPDATE relation_suggestions SET outcome = NULL, decided_at = NULL, decided_by = NULL
		WHERE ((issue_id = ? AND other_id = ?) OR (issue_id = ? AND other_id = ?)) AND outcome = 'accepted'
		AND NOT EXISTS (SELECT 1 FROM issue_relations WHERE (source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?))`,
		a, b, b, a, a, b, b, a)
	return err
}

// DismissRelationSuggestion says otherRef, suggested on ref, isn't related
// to it: it isn't suggested between the two again, from either end, and
// the ranker learns from it (WP-958). Both ends' records of the pair take
// the one decision; with none recorded (the look's recording failed), one
// is kept to keep the pair apart, which the ranker doesn't learn from.
func (s *Service) DismissRelationSuggestion(ctx context.Context, actor *Actor, ref, otherRef string) error {
	return s.decideRelationSuggestion(ctx, actor, ref, otherRef, true)
}

// UndoRelationDismissal takes a dismissal back: the pair is undecided, and
// suggested again.
func (s *Service) UndoRelationDismissal(ctx context.Context, actor *Actor, ref, otherRef string) error {
	return s.decideRelationSuggestion(ctx, actor, ref, otherRef, false)
}

// sugPair matches a pair's records from both ends: a, b, b, a.
const sugPair = `((issue_id = ? AND other_id = ?) OR (issue_id = ? AND other_id = ?))`

func (s *Service) decideRelationSuggestion(ctx context.Context, actor *Actor, ref, otherRef string, dismiss bool) error {
	what := "dismissing a suggestion"
	if !dismiss {
		what = "undoing a dismissal"
	}
	if err := actor.requirePersonDirect(what); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		a, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		b, _, err := s.getIssueFor(ctx, t, actor, otherRef, RoleViewer, false)
		if err != nil {
			return err
		}
		if a.ID == b.ID {
			return invalid("%s can't be suggested for itself", a.Key)
		}
		if !dismiss {
			_, err = t.ExecContext(ctx, `UPDATE relation_suggestions SET outcome = NULL, decided_at = NULL, decided_by = NULL
				WHERE `+sugPair+` AND outcome = 'dismissed'`, a.ID, b.ID, b.ID, a.ID)
			return err
		}
		var related, recorded int
		// Only a link the text does not name (named_in unset) blocks a dismissal; a text-named one can still be offered and dismissed.
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM issue_relations WHERE ((source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?))
			AND named_in IS NULL`,
			a.ID, b.ID, b.ID, a.ID).Scan(&related); err != nil {
			return err
		}
		if related > 0 {
			return invalid("%s and %s are related already; remove the relation instead", a.Key, b.Key)
		}
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM relation_suggestions WHERE `+sugPair, a.ID, b.ID, b.ID, a.ID).Scan(&recorded); err != nil {
			return err
		}
		now := s.nowMS()
		if recorded == 0 {
			_, err = t.ExecContext(ctx, `INSERT INTO relation_suggestions(issue_id, other_id, kind, source, confidence, shown_by, shown_at,
					word, same_project, shared_parent, shared_goal, age_days, source_done, blend, outcome, decided_at, decided_by)
				VALUES (?, ?, 'relates', ?, '', ?, ?, 0, 0, 0, 0, 0, 0, 0, 'dismissed', ?, ?)`,
				a.ID, b.ID, sugUnrecorded, nullUser(actor.id()), now, now, nullUser(actor.id()))
			return err
		}
		_, err = t.ExecContext(ctx, `UPDATE relation_suggestions SET outcome = 'dismissed', decided_at = ?, decided_by = ?
			WHERE `+sugPair+` AND outcome IS NOT 'dismissed'`, now, nullUser(actor.id()), a.ID, b.ID, b.ID, a.ID)
		return err
	})
}

// relRanker is a fitted logistic regression on standardized features.
type relRanker struct {
	Features []string  `json:"features"`
	Mean     []float64 `json:"mean"`
	SD       []float64 `json:"sd"`
	W        []float64 `json:"w"` // the intercept first
	// Line: suggestions scoring below it aren't shown.
	Line float64 `json:"line"`
}

// p is the chance the ranker gives that a person takes the suggestion.
func (r *relRanker) p(f relFeatures) float64 {
	return r.score(f.x())
}

func (r *relRanker) score(x []float64) float64 {
	z := r.W[0]
	for j, v := range x {
		z += r.W[j+1] * (v - r.Mean[j]) / r.SD[j]
	}
	return 1 / (1 + math.Exp(-z))
}

// fitLogistic fits the relation ranker over relFeatureNames.
func fitLogistic(x [][]float64, y []bool) *relRanker {
	return fitLogit(relFeatureNames, x, y)
}

// fitLogit fits a logistic regression over the named features by Newton's
// method with an L2 penalty (not on the intercept): deterministic for the
// same rows. The goal meaning evaluator uses it too (goal_meaning_eval.go).
func fitLogit(names []string, x [][]float64, y []bool) *relRanker {
	d := len(names)
	r := &relRanker{Features: names, Mean: make([]float64, d), SD: make([]float64, d), W: make([]float64, d+1)}
	n := float64(len(x))
	for _, row := range x {
		for j, v := range row {
			r.Mean[j] += v / n
		}
	}
	for _, row := range x {
		for j, v := range row {
			r.SD[j] += (v - r.Mean[j]) * (v - r.Mean[j]) / n
		}
	}
	for j := range r.SD {
		r.SD[j] = math.Sqrt(r.SD[j])
		if r.SD[j] < 1e-12 {
			r.SD[j] = 1
		}
	}
	z := make([][]float64, len(x))
	for i, row := range x {
		z[i] = make([]float64, d+1)
		z[i][0] = 1
		for j, v := range row {
			z[i][j+1] = (v - r.Mean[j]) / r.SD[j]
		}
	}
	for range 50 {
		g := make([]float64, d+1)
		h := make([][]float64, d+1)
		for j := range h {
			h[j] = make([]float64, d+1)
		}
		for i, zi := range z {
			var s float64
			for j, v := range zi {
				s += r.W[j] * v
			}
			p := 1 / (1 + math.Exp(-s))
			e := p - b2f(y[i])
			w := p * (1 - p)
			for j, v := range zi {
				g[j] += e * v
				for k := j; k < len(zi); k++ {
					h[j][k] += w * v * zi[k]
				}
			}
		}
		for j := range h {
			for k := range j {
				h[j][k] = h[k][j]
			}
			if j > 0 {
				g[j] += rankerL2 * r.W[j]
				h[j][j] += rankerL2
			}
		}
		step := solve(h, g)
		if step == nil {
			break
		}
		var most float64
		for j, v := range step {
			r.W[j] -= v
			most = max(most, math.Abs(v))
		}
		if most < 1e-9 {
			break
		}
	}
	return r
}

// solve solves a·x = b by Gaussian elimination with partial pivoting; nil
// when a is singular. a and b are overwritten.
func solve(a [][]float64, b []float64) []float64 {
	n := len(b)
	for c := range n {
		p := c
		for r := c + 1; r < n; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[p][c]) {
				p = r
			}
		}
		if math.Abs(a[p][c]) < 1e-12 {
			return nil
		}
		a[c], a[p] = a[p], a[c]
		b[c], b[p] = b[p], b[c]
		for r := c + 1; r < n; r++ {
			f := a[r][c] / a[c][c]
			for k := c; k < n; k++ {
				a[r][k] -= f * a[c][k]
			}
			b[r] -= f * b[c]
		}
	}
	x := make([]float64, n)
	for r := n - 1; r >= 0; r-- {
		s := b[r]
		for k := r + 1; k < n; k++ {
			s -= a[r][k] * x[k]
		}
		x[r] = s / a[r][r]
	}
	return x
}

// aucOf is the chance a random accepted suggestion scores above a random
// other one (ties count half); NaN without both.
func aucOf(score []float64, y []bool) float64 {
	idx := make([]int, len(score))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return score[idx[a]] < score[idx[b]] })
	var pos, neg, ranks float64
	for i := 0; i < len(idx); {
		j := i
		for j < len(idx) && score[idx[j]] == score[idx[i]] {
			j++
		}
		avg := float64(i+j+1) / 2
		for k := i; k < j; k++ {
			if y[idx[k]] {
				pos++
				ranks += avg
			} else {
				neg++
			}
		}
		i = j
	}
	if pos == 0 || neg == 0 {
		return math.NaN()
	}
	return (ranks - pos*(pos+1)/2) / (pos * neg)
}

// pairedWins is the share of resamples of the rows in which a's AUC is
// above b's; resamples missing a class don't count.
func pairedWins(a, b []float64, y []bool) float64 {
	// #nosec G404 -- a fixed seed on purpose: the same decisions give the same verdict
	rng := rand.New(rand.NewPCG(958, 779))
	n := len(y)
	sa, sb, sy := make([]float64, n), make([]float64, n), make([]bool, n)
	var won, counted float64
	for range rankerResamples {
		for i := range n {
			k := rng.IntN(n)
			sa[i], sb[i], sy[i] = a[k], b[k], y[k]
		}
		ua, ub := aucOf(sa, sy), aucOf(sb, sy)
		if math.IsNaN(ua) || math.IsNaN(ub) {
			continue
		}
		counted++
		if ua > ub {
			won++
		}
	}
	if counted == 0 {
		return 0
	}
	return won / counted
}

// rankerRow is one decision the ranker learns from.
type rankerRow struct {
	at int64
	f  relFeatures
	y  bool
}

// rankerFit is one fit and what it was judged on.
type rankerFit struct {
	Train, Test                  int
	Accepted, Dismissed, Ignored int
	AUCModel, AUCBlend, Wins     float64 // NaN when not judged
	Adopted                      bool
	Note                         string
	Ranker                       *relRanker
}

// fitRelationRanker fits a ranker on the decisions so far and judges it
// against the blend on the held-out month: adopted, Ranker is refitted
// on every decision.
func (s *Service) fitRelationRanker(ctx context.Context) (*rankerFit, error) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT shown_at, coalesce(outcome, ''), word, coalesce(concept, 0), concept IS NOT NULL,
			same_project, shared_parent, shared_goal, age_days, source_done
		FROM relation_suggestions rs WHERE source IN (?, ?) AND (outcome IS NOT NULL OR (shown_at < ?
			-- related since by someone else (an agent): not ignored, and not a person's take either
			AND NOT EXISTS (SELECT 1 FROM issue_relations r WHERE (r.source_id = rs.issue_id AND r.target_id = rs.other_id)
				OR (r.source_id = rs.other_id AND r.target_id = rs.issue_id)))) ORDER BY id`,
		sugWords, sugConcepts, now-rankerIgnoredAfter.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fit := &rankerFit{AUCModel: math.NaN(), AUCBlend: math.NaN(), Wins: math.NaN()}
	var all []rankerRow
	for rows.Next() {
		var r rankerRow
		var outcome string
		if err := rows.Scan(&r.at, &outcome, &r.f.Word, &r.f.Concept, &r.f.HasConcept, &r.f.SameProject, &r.f.SharedParent, &r.f.SharedGoal, &r.f.AgeDays, &r.f.SrcDone); err != nil {
			return nil, err
		}
		switch outcome {
		case "accepted":
			r.y = true
			fit.Accepted++
		case "dismissed":
			fit.Dismissed++
		default:
			fit.Ignored++
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cut := now - rankerHoldout.Milliseconds()
	var train, test []rankerRow
	for _, r := range all {
		if r.at < cut {
			train = append(train, r)
		} else {
			test = append(test, r)
		}
	}
	fit.Train, fit.Test = len(train), len(test)
	enough := func(rows []rankerRow, least int, what string) bool {
		var pos int
		for _, r := range rows {
			if r.y {
				pos++
			}
		}
		switch {
		case len(rows) < least:
			fit.Note = fmt.Sprintf("%d decisions %s; it needs %d", len(rows), what, least)
		case pos < rankerMinClass || len(rows)-pos < rankerMinClass:
			fit.Note = fmt.Sprintf("%d accepted of %d decisions %s; it needs %d of each", pos, len(rows), what, rankerMinClass)
		default:
			return true
		}
		return false
	}
	if !enough(train, rankerMinTrain, "before the held-out month") || !enough(test, rankerMinTest, "in the held-out month") {
		return fit, nil
	}
	model := fitLogistic(rowsX(train))
	ms, bs, ys := make([]float64, len(test)), make([]float64, len(test)), make([]bool, len(test))
	for n, r := range test {
		ms[n], bs[n], ys[n] = model.p(r.f), r.f.blend(), r.y
	}
	fit.AUCModel, fit.AUCBlend = aucOf(ms, ys), aucOf(bs, ys)
	fit.Wins = pairedWins(ms, bs, ys)
	if !(fit.AUCModel > fit.AUCBlend && fit.Wins >= rankerWinShare) {
		fit.Note = "the blend ranks the held-out month as well or better"
		return fit, nil
	}
	// The line comes from the held-out month, so the recall it keeps is out of sample.
	var took []float64
	for n, r := range test {
		if r.y {
			took = append(took, ms[n])
		}
	}
	slices.Sort(took)
	model.Line = took[int(float64(len(took))*(1-rankerRecall))]
	fit.Adopted, fit.Ranker = true, model
	return fit, nil
}

func rowsX(rows []rankerRow) ([][]float64, []bool) {
	x, y := make([][]float64, len(rows)), make([]bool, len(rows))
	for n, r := range rows {
		x[n], y[n] = r.f.x(), r.y
	}
	return x, y
}

// recordFit keeps a fit's figures, and its model when adopted.
func (s *Service) recordFit(ctx context.Context, f *rankerFit) error {
	model := ""
	if f.Ranker != nil {
		b, err := json.Marshal(f.Ranker)
		if err != nil {
			return err
		}
		model = string(b)
	}
	null := func(v float64) any {
		if math.IsNaN(v) {
			return nil
		}
		return math.Round(v*10000) / 10000
	}
	_, err := s.db.W.ExecContext(ctx, `INSERT INTO relation_ranker_fits(fitted_at, train, test, accepted, dismissed, ignored, auc_model, auc_blend, wins, adopted, model, note)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, s.nowMS(), f.Train, f.Test, f.Accepted, f.Dismissed, f.Ignored,
		null(f.AUCModel), null(f.AUCBlend), null(f.Wins), f.Adopted, model, f.Note)
	return err
}

// rankerState is the ranker in use: nil while the blend ranks.
type rankerState struct {
	mu      sync.Mutex
	r       *relRanker
	at      time.Time
	loaded  bool
	fitting bool
	tried   time.Time
}

// relationRanker is the adopted ranker, nil while the blend ranks. It
// starts a fit in the background when the last is a day old.
func (s *Service) relationRanker(ctx context.Context) *relRanker {
	c := &s.ranker
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loaded = true
		var at int64
		var adopted bool
		var model string
		err := s.db.R.QueryRowContext(ctx, `SELECT fitted_at, adopted, model FROM relation_ranker_fits ORDER BY id DESC LIMIT 1`).Scan(&at, &adopted, &model)
		switch {
		case err == nil:
			c.at = time.UnixMilli(at)
			var r relRanker
			if adopted && json.Unmarshal([]byte(model), &r) == nil && len(r.W) == len(relFeatureNames)+1 && len(r.SD) == len(relFeatureNames) && len(r.Mean) == len(relFeatureNames) {
				c.r = &r
			}
		case !errors.Is(err, sql.ErrNoRows):
			s.log.WarnContext(ctx, "relation ranker not loaded", "err", err)
		}
	}
	if s.now().Sub(c.at) >= rankerRefit && !c.fitting && s.now().Sub(c.tried) >= conceptRetry && s.bgCtx.Err() == nil {
		c.fitting, c.tried = true, s.now()
		s.goBG(func() {
			f, err := s.fitRelationRanker(s.bgCtx)
			if err == nil {
				err = s.recordFit(s.bgCtx, f)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			c.fitting = false
			if err != nil {
				s.log.WarnContext(ctx, "relation ranker not fitted", "err", err)
				return
			}
			c.r, c.at = f.Ranker, s.now()
			s.log.Info("relation ranker fitted", "train", f.Train, "test", f.Test, "accepted", f.Accepted, "dismissed", f.Dismissed,
				"ignored", f.Ignored, "auc_model", f.AUCModel, "auc_blend", f.AUCBlend, "wins", f.Wins, "adopted", f.Adopted, "note", f.Note)
		})
	}
	return c.r
}

// rankMatches orders similarity matches by the ranker, best first, and
// drops those below its line. It returns each kept match's score.
func (s *Service) rankMatches(ctx context.Context, r *relRanker, idx *simIndex, i *Issue, src map[string]float64, concept []float64, pool []simMatch) ([]simMatch, map[int64]float64, error) {
	ids := make([]int64, len(pool))
	for n, m := range pool {
		ids[n] = m.id
	}
	feats, err := s.pairFeatures(ctx, idx, i, src, concept, ids)
	if err != nil {
		return nil, nil, err
	}
	probs := make(map[int64]float64, len(pool))
	var kept []simMatch
	for _, m := range pool {
		f, ok := feats[m.id]
		if !ok {
			continue
		}
		if p := r.p(f); p >= r.Line {
			probs[m.id] = p
			kept = append(kept, m)
		}
	}
	sort.SliceStable(kept, func(a, b int) bool { return probs[kept[a].id] > probs[kept[b].id] })
	return kept, probs, nil
}
