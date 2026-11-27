package core

import (
	"math"
	"slices"
	"testing"
)

// The arms' effects are estimated beside the covariates', so an arm that
// happened to get the harder project's tasks isn't blamed for the project.
func TestFitPrepArmsAdjustsForCovariates(t *testing.T) {
	r := &prepRand{s: 7}
	arms := []string{prepArmChat, prepArmAgent}
	const chat, agent, hard = -0.2, 0.15, 1.0
	var obs []prepObs
	var naive [3]float64
	var count [3]float64
	for range 900 {
		inHard := r.float() < 0.5
		u, arm := r.float(), 2
		switch {
		case inHard && u < 0.7, !inHard && u < 0.1:
			arm = 0
		case u > 0.85:
			arm = 1
		}
		y := 2 + 0.9*r.norm()
		x := []float64{1, 0}
		if inHard {
			y += hard
			x[1] = 1
		}
		load := make([]float64, 2)
		switch arm {
		case 0:
			y += chat
			load[0] = 1
		case 1:
			y += agent
			load[1] = 1
		}
		naive[arm] += y
		count[arm]++
		obs = append(obs, prepObs{Y: y, X: x, Arm: load, W: 1})
	}
	if raw := naive[0]/count[0] - naive[2]/count[2]; raw < 0.2 {
		t.Fatalf("the raw difference %.2f should make the chat arm look worse: the test confounds nothing", raw)
	}
	f := fitPrepArms(arms, obs, 0.9)
	for j, want := range []float64{chat, agent} {
		sd := math.Sqrt(f.Cov[j][j])
		if math.Abs(f.Mean[j]-want) > 2.5*sd || sd > 0.12 {
			t.Errorf("%s: effect %.3f ± %.3f, want %.2f", arms[j], f.Mean[j], sd, want)
		}
	}
	if math.Abs(f.Sigma-0.9) > 0.06 {
		t.Errorf("spread %.3f, want about 0.9", f.Sigma)
	}
	if got := f.ArmN[0] + f.ArmN[1] + f.ArmN[2]; math.Abs(got-900) > 1e-6 || f.N != 900 {
		t.Errorf("tasks counted: %v (%v)", f.ArmN, f.N)
	}
	p := f.pBest([][]float64{{0, 0}, {1, 0}, {0, 1}})
	if p[1] < 0.9 || p[2] > 0.01 {
		t.Errorf("probability of being best: %v", p)
	}
	if s := prepShares(p, nil); s[2] != 0 || math.Abs(s[0]+s[1]-1) > 1e-9 {
		t.Errorf("shares: %v", s)
	}
}

// With no data every arm is as likely the best as any other, and the
// estimate is the prior's.
func TestFitPrepArmsNoData(t *testing.T) {
	f := fitPrepArms([]string{prepArmChat, prepArmAgent}, nil, 0.9)
	if f.Mean[0] != 0 || math.Abs(math.Sqrt(f.Cov[0][0])-prepEffectSD) > 1e-6 || f.N != 0 {
		t.Fatalf("the prior: %+v", f)
	}
	p := f.pBest([][]float64{{0, 0}, {1, 0}, {0, 1}})
	if math.Abs(p[0]-0.25) > 0.02 || math.Abs(p[1]-0.375) > 0.02 || math.Abs(p[2]-0.375) > 0.02 {
		t.Errorf("probability of being best with no data: %v", p)
	}
	if p := f.pBest([][]float64{{0, 0}, {1, 0}}); math.Abs(p[0]-0.5) > 0.02 {
		t.Errorf("with one arm's picks and no data: %v", p)
	}
	obs := []prepObs{{Y: 5, X: []float64{1}, Arm: []float64{1, 0}, W: 1}, {Y: 1, X: []float64{1}, Arm: []float64{0, 0}, W: 1}}
	f = fitPrepArms([]string{prepArmChat, prepArmAgent}, obs, 0.9)
	if f.pWorse(0, 0.1) > prepGuardP {
		t.Errorf("two tasks take an arm out: P(worse) %.3f", f.pWorse(0, 0.1))
	}
}

// Tasks whose chat and agentic picks were the same files say what pointers
// do, not which of the two kinds is better.
func TestFitPrepArmsSamePicks(t *testing.T) {
	r := &prepRand{s: 11}
	var obs []prepObs
	for n := range 400 {
		y := 2 + 0.5*r.norm()
		load := []float64{0, 0}
		if n%2 == 0 {
			y -= 0.4
			load = []float64{0.5, 0.5}
		}
		obs = append(obs, prepObs{Y: y, X: []float64{1}, Arm: load, W: 1})
	}
	f := fitPrepArms([]string{prepArmChat, prepArmAgent}, obs, 0.9)
	if sum := f.Mean[0] + f.Mean[1]; math.Abs(sum/2+0.4) > 0.1 {
		t.Errorf("pointers' effect %.3f, want -0.4", sum/2)
	}
	if math.Abs(f.Mean[0]-f.Mean[1]) > 1e-6 {
		t.Errorf("the two arms are told apart by tasks that can't: %v", f.Mean)
	}
	p := f.pBest([][]float64{{0, 0}, {0.5, 0.5}})
	if p[1] < 0.99 {
		t.Errorf("pointers best with probability %.3f", p[1])
	}
}

func TestPrepSharesAndDraw(t *testing.T) {
	if s := prepShares([]float64{0.6, 0.395, 0.005}, nil); s[2] != 0 || math.Abs(s[0]-0.6/0.995) > 1e-9 {
		t.Errorf("a share under the minimum: %v", s)
	}
	if s := prepShares([]float64{0.2, 0.8}, []bool{false, true}); s[0] != 1 || s[1] != 0 {
		t.Errorf("a blocked candidate: %v", s)
	}
	if s := prepShares([]float64{0, 0}, []bool{true, true}); s[0] != 1 {
		t.Errorf("everything blocked: %v", s)
	}
	shares := []float64{0.25, 0, 0.75}
	for u, want := range map[float64]int{0: 0, 0.2499: 0, 0.25: 2, 0.9999: 2} {
		if got := prepPick(shares, u); got != want {
			t.Errorf("pick at %v = %d, want %d", u, got, want)
		}
	}
	var low int
	for id := int64(1); id <= 4000; id++ {
		u := prepDraw(id)
		if u != prepDraw(id) || u < 0 || u >= 1 {
			t.Fatalf("draw of %d: %v", id, u)
		}
		if u < 0.25 {
			low++
		}
	}
	if low < 900 || low > 1100 {
		t.Errorf("%d of 4000 draws under 0.25", low)
	}
}

// prepSeries is days of tasks, split evenly over no pointers and one arm
// whose effect is eff(day), aged from the last day back.
func prepSeries(r *prepRand, days, perDay int, eff func(day int) float64) []prepObs {
	var obs []prepObs
	for d := range days {
		for n := range perDay {
			y := 2 + 0.9*r.norm()
			load := []float64{0}
			if n%2 == 0 {
				y += eff(d)
				load[0] = 1
			}
			obs = append(obs, prepObs{Y: y, X: []float64{1}, Arm: load, Age: float64(days-d) - 0.5, W: 1})
		}
	}
	return obs
}

// How fast evidence ages is the data's: when an arm's effect turned four
// weeks ago the short half-lives forecast best and carry the estimate; when
// nothing changed the long ones keep theirs.
func TestPrepMixFollowsAChange(t *testing.T) {
	arms := []string{prepArmChat}
	turned := prepSeries(&prepRand{s: 3}, 120, 24, func(d int) float64 {
		if d < 92 {
			return -0.5
		}
		return 0.5
	})
	m := fitPrepMix(arms, turned, 0.9, nil)
	if len(m.W) != len(prepHalfLives) || len(m.Fits) != len(prepHalfLives) {
		t.Fatalf("mix: %+v", m)
	}
	var long float64
	for h, hl := range m.HalfLives {
		if hl >= 60 {
			long += m.W[h]
		}
	}
	if long > 0.05 || m.halfLife() > 20 {
		t.Errorf("after a change, weights by half-life %v: %v", prepHalfLives, m.W)
	}
	mean, _ := m.effect(0)
	if mean < 0.2 {
		t.Errorf("the estimate %.2f doesn't say the arm is worse, four weeks after it turned", mean)
	}
	if all := fitPrepArms(arms, prepAged(turned, 0, 0), 0.9); all.Mean[0] > -0.2 {
		t.Errorf("with no task aged the estimate is %.2f: the change isn't in the data", all.Mean[0])
	}
	if p := m.pBest([][]float64{{0}, {1}}); p[1] > 0.02 {
		t.Errorf("the arm's share after it turned worse: %.3f", p[1])
	}

	steady := prepSeries(&prepRand{s: 4}, 120, 24, func(int) float64 { return -0.3 })
	m = fitPrepMix(arms, steady, 0.9, nil)
	if hl := m.halfLife(); hl < 30 {
		t.Errorf("with nothing changing the mix's half-life is %.0f days: %v", hl, m.W)
	}
	lo, hi := m.interval(0)
	if lo > -0.3 || hi < -0.3 || hi-lo > 0.45 {
		t.Errorf("steady effect: [%.2f, %.2f], want -0.3 inside", lo, hi)
	}
	g := fitPrepMix(arms, steady, 0.9, m.W)
	if &g.W[0] != &m.W[0] {
		t.Error("a guard worked out its own weights")
	}
}

// An arm that does nothing is seldom taken out by a guard: over many sets
// of tasks, about as often as the guard's level says or less.
func TestPrepGuardFalseBlocks(t *testing.T) {
	arms := []string{prepArmChat}
	r := &prepRand{s: 5}
	even := make([]float64, len(prepHalfLives))
	for h := range even {
		even[h] = 1 / float64(len(even))
	}
	blocked := 0
	const sets = 400
	for range sets {
		obs := prepSeries(r, 30, 4, func(int) float64 { return 0 })
		m := fitPrepMix(arms, obs, 1.0, even)
		if m.armN(0) >= prepGuardMin && m.pWorse(0, 0.10) > prepGuardP {
			blocked++
		}
	}
	if rate := float64(blocked) / sets; rate > 0.03 {
		t.Errorf("an arm with no effect was taken out in %.1f%% of sets", rate*100)
	}
	obs := prepSeries(r, 30, 12, func(int) float64 { return 0.7 })
	m := fitPrepMix(arms, obs, 1.0, even)
	if m.pWorse(0, 0.10) <= prepGuardP {
		t.Errorf("an arm worse by 0.7: P(worse) %.3f", m.pWorse(0, 0.10))
	}
}

func TestInvertSPD(t *testing.T) {
	a := [][]float64{{4, 1, 0.5}, {1, 3, 0.2}, {0.5, 0.2, 2}}
	inv := invertSPD(a)
	for i := range 3 {
		for j := range 3 {
			var v float64
			for k := range 3 {
				v += a[i][k] * inv[k][j]
			}
			want := 0.0
			if i == j {
				want = 1
			}
			if math.Abs(v-want) > 1e-9 {
				t.Fatalf("A·A⁻¹[%d][%d] = %v", i, j, v)
			}
		}
	}
	inv = invertSPD([][]float64{{1, 1}, {1, 1}})
	if math.IsNaN(inv[0][0]) || math.IsInf(inv[0][0], 0) {
		t.Errorf("a singular matrix: %v", inv)
	}
}

func TestPrepDummies(t *testing.T) {
	labels := []string{"a", "a", "a", "b", "b", "c"}
	d := prepDummies(labels, 2)
	if len(d[0]) != 1 || d[0][0] != 0 || d[3][0] != 1 || d[5][0] != 0 {
		t.Errorf("dummies: %v", d)
	}
	s := prepStandard([]float64{1, 2, 3})
	if math.Abs(s[0]+s[2]) > 1e-12 || s[1] != 0 || math.Abs(s[2]-math.Sqrt(1.5)) > 1e-9 {
		t.Errorf("standardised: %v", s)
	}
	if s := prepStandard([]float64{2, 2}); s[0] != 0 {
		t.Errorf("a constant column: %v", s)
	}
}

// The draws are the ones a generator started at the fixed seed gives each
// fit, whatever was drawn before: the normals are kept between fits, and a
// fit's draws (so its shares) must not depend on which fits came first.
func TestPrepSampleDrawsAreTheSeedsOwn(t *testing.T) {
	fits := func() []*prepFit {
		return []*prepFit{
			{Arms: []string{"a", "b"}, Mean: []float64{0.2, -0.1}, Cov: [][]float64{{0.04, 0.01}, {0.01, 0.09}}},
			{Arms: []string{"a", "b"}, Mean: []float64{-0.3, 0}, Cov: [][]float64{{0.01, 0}, {0, 0.25}}},
			{Arms: []string{"a"}, Mean: []float64{0.1}, Cov: [][]float64{{0.04}}},
			{Arms: []string{"a", "b", "c"}, Mean: []float64{0, 0.1, -0.2}, Cov: [][]float64{{0.04, 0.01, 0}, {0.01, 0.09, 0.02}, {0, 0.02, 0.16}}},
			{Arms: []string{"a", "b"}, Mean: []float64{0, 0}, Cov: [][]float64{{0, 0}, {0, 0}}},
			{},
		}
	}
	// The generator's first normals from the seed, pinned so a change to the
	// seed or to the generator shows here and not only as moved shares.
	r := &prepRand{s: prepSeed}
	for i, want := range []float64{-1.2669898075097235, 2.2115371059329378, -1.3229606993117367} {
		if z := r.norm(); math.Abs(z-want) > 1e-12 {
			t.Fatalf("normal %d from the seed: %v, want %v", i, z, want)
		}
	}

	prepNormalsByArms.Lock()
	prepNormalsByArms.k = map[int][]float64{}
	prepNormalsByArms.Unlock()
	first := map[int][][]float64{}
	for n, f := range fits() {
		k := len(f.Arms)
		// The lower Cholesky factor of Cov, with sample's floor on the
		// diagonal (the all-zero fit reaches it).
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
		r := &prepRand{s: prepSeed}
		draws := f.sample()
		if len(draws) != prepDraws {
			t.Fatalf("%d draws, want %d", len(draws), prepDraws)
		}
		for d, got := range draws {
			z := make([]float64, k)
			for i := range z {
				z[i] = r.norm()
			}
			if len(got) != k {
				t.Fatalf("%d arms, draw %d: %v", k, d, got)
			}
			for i := range k {
				want := f.Mean[i]
				for j := 0; j <= i; j++ {
					want += l[i][j] * z[j]
				}
				if math.Abs(got[i]-want) > 1e-12 {
					t.Fatalf("%d arms, draw %d: %v, want arm %d %v", k, d, got, i, want)
				}
			}
		}
		first[n] = draws
	}

	// The same fits again, last first, with the kept normals dropped: each
	// gets exactly what it got before, and its kept normals are reused.
	prepNormalsByArms.Lock()
	prepNormalsByArms.k = map[int][]float64{}
	prepNormalsByArms.Unlock()
	again := fits()
	for n := len(again) - 1; n >= 0; n-- {
		f := again[n]
		draws := f.sample()
		for d := range draws {
			if !slices.Equal(draws[d], first[n][d]) {
				t.Fatalf("fit %d (%d arms), drawn after the others: draw %d is %v, was %v", n, len(f.Arms), d, draws[d], first[n][d])
			}
		}
		k := len(f.Arms)
		if z := prepNormals(k); k > 0 && &z[0] != &prepNormals(k)[0] {
			t.Errorf("%d arms: the normals are worked out again, not kept", k)
		}
	}
}
