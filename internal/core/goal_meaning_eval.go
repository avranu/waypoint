package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"
)

const (
	meaningEvalMin   = 40
	meaningEvalClass = 10
	meaningHeldClass = 3
	meaningHeldOut   = 3
	meaningDriftMin  = 20
	meaningDriftP    = 0.01
	meaningDriftRate = 0.02
	meaningEvalBoot  = 500
	meaningEvalWins  = 0.9
	meaningCombined  = 0.8
	meaningConstruct = "v1"
)

// meaningFit is one evaluation (meaning_fits).
type meaningFit struct {
	ID           int64             `json:"id"`
	Judgement    string            `json:"judgement"`
	Decider      string            `json:"decider"`
	Model        string            `json:"-"`
	Line         float64           `json:"line"`
	ErrPos       float64           `json:"err_pos"`
	ErrNeg       float64           `json:"err_neg"`
	Labels       int               `json:"labels"`
	HeldOut      int               `json:"held_out"`
	Candidates   []meaningCand     `json:"candidates"`
	LabelThrough int64             `json:"label_through"`
	Versions     map[string]string `json:"versions"`
	Reason       string            `json:"reason"`
	Note         string            `json:"note,omitempty"`
	FittedAt     int64             `json:"fitted_at"`
}

// meaningCand is a candidate's held-out figures.
type meaningCand struct {
	Name     string  `json:"name"`
	Line     float64 `json:"line"`
	Balanced float64 `json:"balanced_accuracy"`
	ErrPos   float64 `json:"err_pos"`
	ErrNeg   float64 `json:"err_neg"`
	Scored   int     `json:"scored"` // held-out labels it had its own score for
}

func (s *Service) meaningFitLatest(ctx context.Context, j string) (*meaningFit, error) {
	f := &meaningFit{}
	var cands, vers string
	err := s.db.R.QueryRowContext(ctx, `SELECT id, judgement, decider, model, line, err_pos, err_neg, labels, held_out, candidates,
		label_through, versions, reason, note, fitted_at FROM meaning_fits WHERE judgement = ? ORDER BY id DESC LIMIT 1`, j).Scan(
		&f.ID, &f.Judgement, &f.Decider, &f.Model, &f.Line, &f.ErrPos, &f.ErrNeg, &f.Labels, &f.HeldOut, &cands,
		&f.LabelThrough, &vers, &f.Reason, &f.Note, &f.FittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(cands), &f.Candidates)
	_ = json.Unmarshal([]byte(vers), &f.Versions)
	return f, nil
}

// meaningVersions are the scorers' versions now; "" for one not known.
func meaningVersions(st *meaningStore) map[string]string {
	return map[string]string{"embed": st.embedModel, "nli": st.nliModel, "llm": meaningLLMVersion, "construct": meaningConstruct}
}

// meaningRow is one label with its scores.
type meaningRow struct {
	id     int64
	y      bool
	source string
	grp    string
	f      map[string]float64
}

// meaningEvalRows reads a question's labels and their scores. pending:
// some label still waits for a score that is coming.
func (s *Service) meaningEvalRows(ctx context.Context, st *meaningStore, ch *meaningChain, ctxOf map[string]string, j string) ([]meaningRow, bool, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT l.id, l.a_hash, l.b_hash, l.y, l.source, l.grp, ta.text, tb.text FROM meaning_labels l
		JOIN meaning_texts ta ON ta.hash = l.a_hash JOIN meaning_texts tb ON tb.hash = l.b_hash WHERE l.judgement = ? ORDER BY l.id`, j)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []meaningRow
	pending := false
	for rows.Next() {
		var r meaningRow
		p := &meaningPair{judgement: j}
		if err := rows.Scan(&r.id, &p.aHash, &p.bHash, &r.y, &r.source, &r.grp, &p.aText, &p.bText); err != nil {
			return nil, false, err
		}
		if j == judgeOverlap {
			p.aCtx, p.bCtx = ctxOf[p.aHash], ctxOf[p.bHash]
		}
		r.f = st.features(p)
		if ch != nil && s.llmDue(st.llm[p.key()]) {
			pending = true
		}
		if st.nliModel != "" {
			if _, ok := r.f["nli_min"]; !ok {
				pending = true
			}
		}
		if st.embedModel != "" {
			if _, ok := r.f["embed"]; !ok {
				pending = true
			}
		}
		out = append(out, r)
	}
	return out, pending, rows.Err()
}

// meaningEvaluateDue runs each question's evaluation that is due. It says
// whether any fit was recorded that changes a decider.
func (s *Service) meaningEvaluateDue(ctx context.Context, st *meaningStore, ch *meaningChain, goals map[int64]*meaningGoal) bool {
	ctxOf := map[string]string{}
	for _, g := range goals {
		ctxOf[g.hash] = g.ctx
	}
	changed := false
	for _, j := range meaningJudgements {
		if ctx.Err() != nil {
			return changed
		}
		rows, pending, err := s.meaningEvalRows(ctx, st, ch, ctxOf, j)
		if err != nil {
			s.log.WarnContext(ctx, "goal meaning: read labels", "judgement", j, "err", err)
			continue
		}
		if pending {
			continue
		}
		inc, err := s.meaningFitLatest(ctx, j)
		if err != nil {
			s.log.WarnContext(ctx, "goal meaning: read the fit", "judgement", j, "err", err)
			continue
		}
		reason := meaningEvalReason(inc, rows, meaningVersions(st))
		if reason == "" {
			continue
		}
		f := meaningEvaluate(j, rows, inc)
		if f == nil {
			continue
		}
		f.Reason, f.Versions, f.FittedAt = reason, meaningVersions(st), s.nowMS()
		if err := s.meaningKeepFit(ctx, f); err != nil {
			s.log.WarnContext(ctx, "goal meaning: keep the fit", "judgement", j, "err", err)
			continue
		}
		s.log.Info("goal meaning: evaluated", "judgement", j, "reason", reason, "decider", f.Decider, "note", f.Note)
		if inc == nil || inc.Decider != f.Decider || inc.Line != f.Line || inc.Model != f.Model {
			changed = true
		}
	}
	return changed
}

// meaningEvalReason says why the question is to be evaluated now, or "".
func meaningEvalReason(inc *meaningFit, rows []meaningRow, versions map[string]string) string {
	pos, neg := 0, 0
	for _, r := range rows {
		if r.y {
			pos++
		} else {
			neg++
		}
	}
	enough := len(rows) >= meaningEvalMin && pos >= meaningEvalClass && neg >= meaningEvalClass
	if inc == nil {
		if enough {
			return "first"
		}
		return ""
	}
	if !enough {
		return ""
	}
	for k, v := range versions {
		if v != "" && inc.Versions[k] != v {
			return "version"
		}
	}
	d := inc.decider()
	var n, e [2]int
	for _, r := range rows {
		if r.id <= inc.LabelThrough {
			continue
		}
		_, yes, _, _ := d.decide(inc.Judgement, r.f)
		c := 0
		if r.y {
			c = 1
		}
		n[c]++
		if yes != r.y {
			e[c]++
		}
	}
	for c, rate := range []float64{inc.ErrNeg, inc.ErrPos} {
		if n[c] >= meaningDriftMin && binomUpper(n[c], e[c], max(rate, meaningDriftRate)) < meaningDriftP {
			return "drift"
		}
	}
	return ""
}

// binomUpper is P(X >= k) for X ~ Binomial(n, p).
func binomUpper(n, k int, p float64) float64 {
	if k <= 0 {
		return 1
	}
	if p <= 0 {
		return 0
	}
	if p >= 1 {
		return 1
	}
	lp, lq := math.Log(p), math.Log1p(-p)
	ln, _ := math.Lgamma(float64(n + 1))
	sum := 0.0
	for i := k; i <= n; i++ {
		a, _ := math.Lgamma(float64(i + 1))
		b, _ := math.Lgamma(float64(n - i + 1))
		sum += math.Exp(ln - a - b + float64(i)*lp + float64(n-i)*lq)
	}
	return min(sum, 1)
}

// meaningHeld says whether a group is held out.
func meaningHeld(grp string) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte(grp))
	return h.Sum32()%10 < meaningHeldOut
}

// meaningSplit holds out the labels of ~30% of the groups: behaviour
// labels only, once there are enough of each answer, else all.
func meaningSplit(rows []meaningRow) (train, held []meaningRow) {
	var bp, bn int
	for _, r := range rows {
		if r.source != labelConstructed {
			if r.y {
				bp++
			} else {
				bn++
			}
		}
	}
	behaviour := bp >= meaningEvalClass && bn >= meaningEvalClass
	for _, r := range rows {
		if meaningHeld(r.grp) && (!behaviour || r.source != labelConstructed) {
			held = append(held, r)
		} else {
			train = append(train, r)
		}
	}
	return train, held
}

// balanced is the mean of the rates of yes labels said yes and no labels
// said no; with the error rates on each.
func balanced(said, y []bool) (ba, errPos, errNeg float64) {
	var tp, fn, tn, fp float64
	for i := range y {
		switch {
		case y[i] && said[i]:
			tp++
		case y[i]:
			fn++
		case said[i]:
			fp++
		default:
			tn++
		}
	}
	if tp+fn > 0 {
		errPos = fn / (tp + fn)
	}
	if tn+fp > 0 {
		errNeg = fp / (tn + fp)
	}
	return 1 - (errPos+errNeg)/2, errPos, errNeg
}

// bestLine is the line on a score with the best balanced accuracy.
func bestLine(sc []float64, y []bool) (float64, float64) {
	vals := append([]float64(nil), sc...)
	sort.Float64s(vals)
	best, line := -1.0, math.Inf(1)
	said := make([]bool, len(sc))
	for i, t := range vals {
		if i > 0 && t == vals[i-1] {
			continue
		}
		for k := range sc {
			said[k] = sc[k] >= t
		}
		if ba, _, _ := balanced(said, y); ba > best {
			best, line = ba, t
		}
	}
	return line, best
}

// meaningFitCand fits one candidate on rows; nil when it can't be.
func meaningFitCand(j, name string, rows []meaningRow) *meaningDecider {
	d := &meaningDecider{j: j, name: name}
	switch name {
	case "prior":
		return d
	case "combined":
		var names []string
		for _, n := range meaningFeatureNames[j] {
			have := 0
			for _, r := range rows {
				if _, ok := r.f[n]; ok {
					have++
				}
			}
			if float64(have) >= meaningCombined*float64(len(rows)) && have > 0 {
				names = append(names, n)
			}
		}
		if len(names) < 2 {
			return nil
		}
		mean := make([]float64, len(names))
		cnt := make([]float64, len(names))
		for _, r := range rows {
			for i, n := range names {
				if v, ok := r.f[n]; ok {
					mean[i] += v
					cnt[i]++
				}
			}
		}
		for i := range mean {
			mean[i] /= cnt[i]
		}
		x := make([][]float64, len(rows))
		y := make([]bool, len(rows))
		for k, r := range rows {
			x[k] = make([]float64, len(names))
			for i, n := range names {
				if v, ok := r.f[n]; ok {
					x[k][i] = v
				} else {
					x[k][i] = mean[i]
				}
			}
			y[k] = r.y
		}
		m := fitLogit(names, x, y)
		sc := make([]float64, len(rows))
		for k := range x {
			sc[k] = m.score(x[k])
		}
		d.line, _ = bestLine(sc, y)
		m.Line = d.line
		d.model = m
		return d
	}
	var sc []float64
	var y []bool
	for _, r := range rows {
		if v, ok := r.f[name]; ok {
			sc = append(sc, v)
			y = append(y, r.y)
		}
	}
	pos := 0
	for _, v := range y {
		if v {
			pos++
		}
	}
	if pos == 0 || pos == len(y) {
		return nil
	}
	d.line, _ = bestLine(sc, y)
	return d
}

// meaningSay is what a decider decides of each row as it would live: its
// own answer, else the prior's, else no; and how many it had its own for.
func meaningSay(d *meaningDecider, rows []meaningRow) ([]bool, int) {
	said := make([]bool, len(rows))
	own := 0
	for i, r := range rows {
		if _, yes, ok := d.decideOwn(r.f); ok {
			said[i] = yes
			own++
			continue
		}
		_, yes, _ := meaningPrior(d.j, r.f)
		said[i] = yes
	}
	return said, own
}

// meaningEvaluate picks the question's decider; nil when the held-out
// labels can't tell.
func meaningEvaluate(j string, rows []meaningRow, inc *meaningFit) *meaningFit {
	train, held := meaningSplit(rows)
	y := make([]bool, len(held))
	hp := 0
	for i, r := range held {
		y[i] = r.y
		if r.y {
			hp++
		}
	}
	if hp < meaningHeldClass || len(held)-hp < meaningHeldClass {
		return nil
	}
	names := append([]string{"prior"}, meaningFeatureNames[j]...)
	names = append(names, "combined")
	type cand struct {
		d    *meaningDecider
		said []bool
		c    meaningCand
	}
	var cands []cand
	for _, n := range names {
		d := meaningFitCand(j, n, train)
		if d == nil {
			continue
		}
		said, own := meaningSay(d, held)
		ba, ep, en := balanced(said, y)
		cands = append(cands, cand{d, said, meaningCand{Name: n, Line: d.line, Balanced: ba, ErrPos: ep, ErrNeg: en, Scored: own}})
	}
	best := 0
	for i, c := range cands {
		if c.c.Balanced > cands[best].c.Balanced {
			best = i
		}
	}
	incD := &meaningDecider{j: j, name: "prior"}
	if inc != nil {
		incD = inc.decider()
	}
	incSaid, _ := meaningSay(incD, held)
	incBA, incEP, incEN := balanced(incSaid, y)
	out := &meaningFit{Judgement: j, Labels: len(rows), HeldOut: len(held), LabelThrough: rows[len(rows)-1].id}
	for _, c := range cands {
		out.Candidates = append(out.Candidates, c.c)
	}
	b := cands[best]
	wins := meaningWins(b.said, incSaid, y)
	sameAsInc := b.d.name == incD.name && (b.d.name == "prior" || inc != nil && b.d.name != "combined" && b.d.line == incD.line)
	if b.c.Balanced > incBA && wins >= meaningEvalWins && !sameAsInc {
		d := meaningFitCand(j, b.d.name, rows)
		if d == nil {
			d = b.d
		}
		out.Decider, out.Line, out.ErrPos, out.ErrNeg = d.name, d.line, b.c.ErrPos, b.c.ErrNeg
		if d.model != nil {
			raw, _ := json.Marshal(d.model)
			out.Model = string(raw)
		}
		out.Note = "adopted"
		return out
	}
	out.Decider, out.Line, out.ErrPos, out.ErrNeg = incD.name, incD.line, incEP, incEN
	if inc != nil {
		out.Model = inc.Model
	}
	out.Note = "kept"
	return out
}

// meaningWins is the share of resamples of the held-out labels in which a
// beats b on balanced accuracy (a fixed seed: the same labels, the same
// answer).
func meaningWins(a, b, y []bool) float64 {
	r := rand.New(rand.NewPCG(1035, 133)) // #nosec G404 -- resampling, not a secret
	n := len(y)
	sa, sb, sy := make([]bool, n), make([]bool, n), make([]bool, n)
	wins := 0
	for range meaningEvalBoot {
		for i := range n {
			k := r.IntN(n)
			sa[i], sb[i], sy[i] = a[k], b[k], y[k]
		}
		ba, _, _ := balanced(sa, sy)
		bb, _, _ := balanced(sb, sy)
		if ba > bb {
			wins++
		}
	}
	return float64(wins) / meaningEvalBoot
}

func (s *Service) meaningKeepFit(ctx context.Context, f *meaningFit) error {
	cands, _ := json.Marshal(f.Candidates)
	vers, _ := json.Marshal(f.Versions)
	return s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `INSERT INTO meaning_fits(judgement, decider, model, line, err_pos, err_neg, labels, held_out, candidates,
			label_through, versions, reason, note, fitted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			f.Judgement, f.Decider, f.Model, f.Line, f.ErrPos, f.ErrNeg, f.Labels, f.HeldOut, string(cands),
			f.LabelThrough, string(vers), f.Reason, f.Note, f.FittedAt)
		if err != nil {
			return err
		}
		f.ID, err = res.LastInsertId()
		return err
	})
}
