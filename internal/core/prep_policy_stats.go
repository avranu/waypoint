package core

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
)

// each arm's share is the probability it is best (Thompson sampling); observations age by half-life, and the half-life mix is weighted by recent forecast accuracy

// prepObs is one task in a fit.
type prepObs struct {
	Y float64   // the outcome; lower is better
	X []float64 // covariates, the intercept first
	// Arm is the task's load on each arm's effect against no pointers: 1
	// for the arm it was shown, a half each when two arms' picks were the
	// same (it then says what pointers do, not which of the two is better).
	Arm []float64
	Age float64 // days since the task was taken
	W   float64 // how much of the observation is left, 0 to 1 (prepAged sets it)
}

// prepFit is the arms' effects against no pointers, as the data have them.
type prepFit struct {
	Arms  []string
	Mean  []float64   // per arm, in the outcome's unit (log: -0.1 is about 10% fewer)
	Cov   [][]float64 // of the effects
	Sigma float64     // what one task's outcome varies by, covariates taken out
	N     float64     // the observations' weights, summed
	// ArmN: each arm's weighted observations; the last is no pointers'.
	ArmN  []float64
	draws [][]float64
	// covariates' coefficients first, then the arms'
	beta []float64
	inv  [][]float64
	p    int
}

const (
	// prepEffectSD is how large an arm's effect is expected to be before
	// any data: on the log scale, 0.5 says a change of more than about
	// 60% either way would be a surprise. It is what an arm with no or
	// old evidence falls back to.
	prepEffectSD = 0.5
	// prepCovarSD is the same for a covariate's coefficient: wide, only
	// to keep a rare project's or type's column from deciding anything.
	prepCovarSD = 3.0
	// prepSigmaPrior counts as this many observations at the outcome's
	// expected spread, so a handful of tasks can't claim a tight estimate.
	prepSigmaPrior = 8.0
	// prepDraws: samples of the effects a share is counted over.
	prepDraws = 4000
	// prepMinShare: a smaller share is none; the others take it up.
	prepMinShare = 0.01
)

// fitPrepArms estimates the arms' effects. spread is the outcome's expected
// spread per task (what prepSigmaPrior observations are assumed to show).
// With no observations the effects are the prior's: zero, give or take
// prepEffectSD.
func fitPrepArms(arms []string, obs []prepObs, spread float64) *prepFit {
	k := len(arms)
	p := 1
	if len(obs) > 0 {
		p = len(obs[0].X)
	}
	d := p + k
	f := &prepFit{Arms: slices.Clone(arms), ArmN: make([]float64, k+1)}
	xtx := make([][]float64, d)
	for i := range xtx {
		xtx[i] = make([]float64, d)
	}
	xty := make([]float64, d)
	z := make([]float64, d)
	row := func(o *prepObs) {
		copy(z, o.X)
		for j := range k {
			z[p+j] = 0
			if j < len(o.Arm) {
				z[p+j] = o.Arm[j]
			}
		}
	}
	var sumW, sumY, sumYY float64
	for n := range obs {
		o := &obs[n]
		if o.W <= 0 || len(o.X) != p {
			continue
		}
		row(o)
		for i := range d {
			if z[i] == 0 {
				continue
			}
			xty[i] += o.W * z[i] * o.Y
			for j := range d {
				xtx[i][j] += o.W * z[i] * z[j]
			}
		}
		sumW += o.W
		sumY += o.W * o.Y
		sumYY += o.W * o.Y * o.Y
		none := 1.0
		for j := range k {
			f.ArmN[j] += o.W * z[p+j]
			none -= z[p+j]
		}
		f.ArmN[k] += o.W * max(none, 0)
	}
	f.N = sumW
	prior := make([]float64, d)
	prior[0] = 1e-8 // the intercept is unpenalised; only the covariates are shrunk
	for i := 1; i < p; i++ {
		prior[i] = 1 / (prepCovarSD * prepCovarSD)
	}
	for j := range k {
		prior[p+j] = 1 / (prepEffectSD * prepEffectSD)
	}
	sigma2 := spread * spread
	var inv [][]float64
	var beta []float64
	// the spread is re-estimated from the previous fit's residuals, a few times
	for range 3 {
		a := make([][]float64, d)
		for i := range d {
			a[i] = make([]float64, d)
			for j := range d {
				a[i][j] = xtx[i][j] / sigma2
			}
			a[i][i] += prior[i]
		}
		inv = invertSPD(a)
		beta = make([]float64, d)
		for i := range d {
			for j := range d {
				beta[i] += inv[i][j] * xty[j] / sigma2
			}
		}
		rss := sumYY
		for i := range d {
			rss -= 2 * beta[i] * xty[i]
			for j := range d {
				rss += beta[i] * xtx[i][j] * beta[j]
			}
		}
		rss = max(rss, 0)
		sigma2 = (rss + prepSigmaPrior*spread*spread) / (max(sumW-float64(d), 0) + prepSigmaPrior)
	}
	f.Sigma = math.Sqrt(sigma2)
	f.beta, f.inv, f.p = beta, inv, p
	f.Mean = make([]float64, k)
	f.Cov = make([][]float64, k)
	for i := range k {
		f.Mean[i] = beta[p+i]
		f.Cov[i] = make([]float64, k)
		for j := range k {
			f.Cov[i][j] = inv[p+i][p+j]
		}
	}
	return f
}

// forecast is what the fit expects of a task: the outcome's mean and
// variance (the task's own spread and the estimate's doubt).
func (f *prepFit) forecast(o *prepObs) (mean, variance float64) {
	k := len(f.Arms)
	z := make([]float64, f.p+k)
	for i := 0; i < f.p && i < len(o.X); i++ {
		z[i] = o.X[i]
	}
	for j := 0; j < k && j < len(o.Arm); j++ {
		z[f.p+j] = o.Arm[j]
	}
	variance = f.Sigma * f.Sigma
	for i, zi := range z {
		if zi == 0 {
			continue
		}
		mean += zi * f.beta[i]
		for j, zj := range z {
			variance += zi * f.inv[i][j] * zj
		}
	}
	return mean, variance
}

// prepHalfLives are the half-lives (days) the estimate is mixed over, and
// prepForecastDays the days each is judged on. Of the sets and windows
// simulated (7 to 240 days over 28, 14 to 120 over 14, 3 or 7 to 120 over
// 7 or 14) this one cost the fewest tasks a worse arm in six cases of
// seven; they differ from one another by far less than any of them does
// from a fixed half-life or a fixed split (page WP-X19).
var prepHalfLives = []float64{7, 14, 30, 60, 120, 240}

const prepForecastDays = 28

// prepAged is the observations at least back days old, each weighted by its
// age then: a half after each half-life (0: nothing ages).
func prepAged(obs []prepObs, halfLife, back float64) []prepObs {
	out := make([]prepObs, 0, len(obs))
	for _, o := range obs {
		if o.Age < back {
			continue
		}
		o.W = 1
		if halfLife > 0 {
			o.W = prepRecency(o.Age-back, halfLife)
		}
		out = append(out, o)
	}
	return out
}

// prepForecastScore is, for each half-life, the log likelihood of the tasks
// taken on each of the last days under the fit of the tasks before that
// day: how well it would have forecast them.
func prepForecastScore(arms []string, obs []prepObs, spread float64, halfLives []float64, days int) []float64 {
	ll := make([]float64, len(halfLives))
	for back := days; back >= 1; back-- {
		var test []*prepObs
		for i := range obs {
			if obs[i].Age >= float64(back-1) && obs[i].Age < float64(back) {
				test = append(test, &obs[i])
			}
		}
		if len(test) == 0 {
			continue
		}
		for h, hl := range halfLives {
			ll[h] += fitPrepArms(arms, prepAged(obs, hl, float64(back)), spread).logLik(test)
		}
	}
	return ll
}

// logLik is the log likelihood of tasks the fit didn't see.
func (f *prepFit) logLik(test []*prepObs) float64 {
	var ll float64
	for _, o := range test {
		if len(o.X) != f.p {
			continue
		}
		m, v := f.forecast(o)
		ll += -0.5*math.Log(2*math.Pi*v) - (o.Y-m)*(o.Y-m)/(2*v)
	}
	return ll
}

// prepModelWeights turns forecast scores into weights that sum to one.
func prepModelWeights(ll []float64) []float64 {
	w := make([]float64, len(ll))
	if len(ll) == 0 {
		return w
	}
	top := slices.Max(ll)
	var sum float64
	for i, l := range ll {
		w[i] = math.Exp(l - top)
		sum += w[i]
	}
	for i := range w {
		w[i] /= sum
	}
	return w
}

// prepMix is the estimate: a fit at each half-life, weighted by its
// forecasts.
type prepMix struct {
	HalfLives []float64
	W         []float64
	Fits      []*prepFit
}

// fitPrepMix fits at every half-life. w nil works the weights out from the
// forecasts; a guard's outcome is given the primary outcome's.
func fitPrepMix(arms []string, obs []prepObs, spread float64, w []float64) *prepMix {
	m := &prepMix{HalfLives: prepHalfLives, W: w}
	if w == nil {
		m.W = prepModelWeights(prepForecastScore(arms, obs, spread, prepHalfLives, prepForecastDays))
	}
	for _, hl := range prepHalfLives {
		m.Fits = append(m.Fits, fitPrepArms(arms, prepAged(obs, hl, 0), spread))
	}
	return m
}

// pBest is each candidate's probability of being the best (prepFit.pBest).
func (m *prepMix) pBest(cands [][]float64) []float64 {
	out := make([]float64, len(cands))
	for h, f := range m.Fits {
		if m.W[h] < 1e-4 {
			continue
		}
		for i, p := range f.pBest(cands) {
			out[i] += m.W[h] * p
		}
	}
	var sum float64
	for _, p := range out {
		sum += p
	}
	for i := range out {
		if sum > 0 {
			out[i] /= sum
		}
	}
	return out
}

// effect is an arm's effect and its spread across the mix.
func (m *prepMix) effect(arm int) (mean, sd float64) {
	var second float64
	for h, f := range m.Fits {
		mean += m.W[h] * f.Mean[arm]
		second += m.W[h] * (f.Cov[arm][arm] + f.Mean[arm]*f.Mean[arm])
	}
	return mean, math.Sqrt(max(second-mean*mean, 0))
}

// interval is an arm's effect's middle 90%.
func (m *prepMix) interval(arm int) (lo, hi float64) {
	mean, sd := m.effect(arm)
	return mean - 1.645*sd, mean + 1.645*sd
}

// pWorse is the probability that an arm is worse by more than margin.
func (m *prepMix) pWorse(arm int, margin float64) float64 {
	var p float64
	for h, f := range m.Fits {
		p += m.W[h] * f.pWorse(arm, margin)
	}
	return p
}

// armN is what an arm's tasks count for (the last index: no pointers').
func (m *prepMix) armN(arm int) float64 {
	var n float64
	for h, f := range m.Fits {
		n += m.W[h] * f.ArmN[arm]
	}
	return n
}

// sigma is one task's spread, covariates taken out.
func (m *prepMix) sigma() float64 {
	var v float64
	for h, f := range m.Fits {
		v += m.W[h] * f.Sigma * f.Sigma
	}
	return math.Sqrt(v)
}

// halfLife is the mix's mean half-life, days.
func (m *prepMix) halfLife() float64 {
	var v float64
	for h, hl := range m.HalfLives {
		v += m.W[h] * hl
	}
	return v
}

// invertSPD inverts a symmetric positive definite matrix by Cholesky. One
// that isn't (columns that say the same thing) gets a little added to its
// diagonal until it is.
func invertSPD(a [][]float64) [][]float64 {
	n := len(a)
	l := make([][]float64, n)
	for jitter := 0.0; ; jitter = max(jitter*10, 1e-9) {
		ok := true
		for i := range n {
			l[i] = make([]float64, n)
		}
		for i := 0; i < n && ok; i++ {
			for j := 0; j <= i; j++ {
				sum := a[i][j]
				if i == j {
					sum += jitter
				}
				for m := range j {
					sum -= l[i][m] * l[j][m]
				}
				if i == j {
					if sum <= 1e-12 || math.IsNaN(sum) {
						ok = false
						break
					}
					l[i][i] = math.Sqrt(sum)
				} else {
					l[i][j] = sum / l[j][j]
				}
			}
		}
		if ok || jitter > 1e6 {
			break
		}
	}
	li := make([][]float64, n)
	for i := range n {
		li[i] = make([]float64, n)
	}
	for i := range n {
		if l[i][i] == 0 {
			continue
		}
		li[i][i] = 1 / l[i][i]
		for j := range i {
			var sum float64
			for m := j; m < i; m++ {
				sum -= l[i][m] * li[m][j]
			}
			li[i][j] = sum / l[i][i]
		}
	}
	inv := make([][]float64, n)
	for i := range n {
		inv[i] = make([]float64, n)
	}
	for i := range n {
		for j := 0; j <= i; j++ {
			var sum float64
			for m := i; m < n; m++ {
				sum += li[m][i] * li[m][j]
			}
			inv[i][j], inv[j][i] = sum, sum
		}
	}
	return inv
}

// prepRand is a small fixed-seed generator (splitmix64), so a fit's shares
// are the same every time they are worked out from the same data. Nothing
// secret hangs on it.
type prepRand struct {
	s     uint64
	spare float64
	has   bool
}

func (r *prepRand) next() uint64 {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// float is uniform in (0, 1).
func (r *prepRand) float() float64 {
	return (float64(r.next()>>11) + 0.5) / (1 << 53)
}

// norm is a standard normal (Box-Muller).
func (r *prepRand) norm() float64 {
	if r.has {
		r.has = false
		return r.spare
	}
	u, v := r.float(), r.float()
	m := math.Sqrt(-2 * math.Log(u))
	r.spare, r.has = m*math.Sin(2*math.Pi*v), true
	return m * math.Cos(2*math.Pi*v)
}

// sample draws the effects prepDraws times, once.
func (f *prepFit) sample() [][]float64 {
	if f.draws != nil {
		return f.draws
	}
	k := len(f.Arms)
	l := make([][]float64, k)
	for i := range k {
		l[i] = make([]float64, k)
		for j := 0; j <= i; j++ {
			sum := f.Cov[i][j]
			for m := range j {
				sum -= l[i][m] * l[j][m]
			}
			if i == j {
				l[i][i] = math.Sqrt(max(sum, 1e-12))
			} else {
				l[i][j] = sum / l[j][j]
			}
		}
	}
	r := &prepRand{s: 0x5eed}
	out := make([][]float64, prepDraws)
	zs := make([]float64, k)
	for n := range out {
		for i := range k {
			zs[i] = r.norm()
		}
		row := make([]float64, k)
		for i := range k {
			row[i] = f.Mean[i]
			for j := 0; j <= i; j++ {
				row[i] += l[i][j] * zs[j]
			}
		}
		out[n] = row
	}
	f.draws = out
	return out
}

// pBest is, for each candidate, the probability that it has the lowest
// outcome. A candidate is its load on each arm's effect: all zero for no
// pointers, 1 at an arm for that arm, a half each for two arms whose picks
// are the same.
func (f *prepFit) pBest(cands [][]float64) []float64 {
	out := make([]float64, len(cands))
	if len(cands) == 0 {
		return out
	}
	draws := f.sample()
	for _, d := range draws {
		best, at := math.Inf(1), 0
		for c, load := range cands {
			var v float64
			for j, w := range load {
				if w != 0 && j < len(d) {
					v += w * d[j]
				}
			}
			if v < best {
				best, at = v, c
			}
		}
		out[at]++
	}
	for i := range out {
		out[i] /= float64(len(draws))
	}
	return out
}

// pWorse is the probability that an arm's effect is above margin (worse by
// more than it).
func (f *prepFit) pWorse(arm int, margin float64) float64 {
	sd := math.Sqrt(max(f.Cov[arm][arm], 0))
	if sd == 0 {
		if f.Mean[arm] > margin {
			return 1
		}
		return 0
	}
	return 1 - normCDF((margin-f.Mean[arm])/sd)
}

func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// prepShares turns probabilities of being best into shares of new tasks: a
// blocked candidate gets none, one under prepMinShare gets none, and the
// rest are scaled to sum to one. With every candidate blocked the first
// (no pointers) takes all.
func prepShares(pbest []float64, blocked []bool) []float64 {
	out := make([]float64, len(pbest))
	var sum float64
	for i, p := range pbest {
		if (i < len(blocked) && blocked[i]) || p < prepMinShare {
			continue
		}
		out[i] = p
		sum += p
	}
	if sum == 0 {
		if len(out) > 0 {
			out[0] = 1
		}
		return out
	}
	for i := range out {
		out[i] /= sum
	}
	return out
}

// prepDraw is a task's place in [0, 1): fixed for the task, and nothing an
// agent or the task's text can steer.
func prepDraw(issueID int64) float64 {
	h := sha256.Sum256(fmt.Appendf(nil, "prep-policy/%d", issueID))
	return float64(binary.BigEndian.Uint64(h[:8])>>11) / (1 << 53)
}

// prepPick is the candidate at u along the shares.
func prepPick(shares []float64, u float64) int {
	last := 0
	for i, s := range shares {
		if s <= 0 {
			continue
		}
		last = i
		if u < s {
			return i
		}
		u -= s
	}
	return last
}

// prepRecency is how much of an observation is left at age days: a half
// after each half-life.
func prepRecency(days, halfLife float64) float64 {
	if days <= 0 {
		return 1
	}
	return math.Pow(0.5, days/halfLife)
}

// prepDummies turns labels into 0/1 columns, one per label that at least
// min observations carry, leaving out the commonest (it is the baseline);
// rarer labels share the baseline too.
func prepDummies(labels []string, min int) [][]float64 {
	count := map[string]int{}
	for _, l := range labels {
		count[l]++
	}
	var keep []string
	for l, n := range count {
		if n >= min {
			keep = append(keep, l)
		}
	}
	slices.SortFunc(keep, func(a, b string) int {
		if count[a] != count[b] {
			return count[b] - count[a]
		}
		if a < b {
			return -1
		}
		return 1
	})
	if len(keep) > 0 {
		keep = keep[1:]
	}
	out := make([][]float64, len(labels))
	for i, l := range labels {
		out[i] = make([]float64, len(keep))
		for j, kl := range keep {
			if l == kl {
				out[i][j] = 1
			}
		}
	}
	return out
}

// prepStandard centres a column and scales it to unit spread (weights
// ignored: it only makes the prior on its coefficient mean something).
func prepStandard(v []float64) []float64 {
	out := make([]float64, len(v))
	if len(v) == 0 {
		return out
	}
	var mean float64
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	var ss float64
	for _, x := range v {
		ss += (x - mean) * (x - mean)
	}
	sd := math.Sqrt(ss / float64(len(v)))
	if sd == 0 {
		return out
	}
	for i, x := range v {
		out[i] = (x - mean) / sd
	}
	return out
}
