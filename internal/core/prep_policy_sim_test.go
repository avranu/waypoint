package core

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// outcomes are seen a day after each task arrives, so the constants' cost shows here (worse-arm tasks, speed of following a change)

// simPerDay is exposed tasks a day: 29 a day were measured over the prep
// experiment's 16 days (page WP-X19). WP_PREP_SIM_PER_DAY sets another, for
// when only some of the takers send traces.
var simPerDay = func() int {
	if n, err := strconv.Atoi(os.Getenv("WP_PREP_SIM_PER_DAY")); err == nil && n > 0 {
		return n
	}
	return 25
}()

// simSpread is one task's outcome's spread, log scale, as measured.
const simSpread = 0.9

// simScenario is the arms' true effects against no pointers by day (log
// scale; lower is better).
type simScenario struct {
	name   string
	effect func(arm string, day int) float64
}

var simScenarios = []simScenario{
	{"no arm does anything", func(string, int) float64 { return 0 }},
	{"chat 35% worse", func(a string, _ int) float64 { return pick(a == prepArmChat, 0.3, 0) }},
	{"agent 26% better", func(a string, _ int) float64 { return pick(a == prepArmAgent, -0.3, 0) }},
	{"both 10% better", func(string, int) float64 { return -0.1 }},
	{"chat better, then worse from day 60", func(a string, d int) float64 {
		if a != prepArmChat {
			return 0
		}
		return pick(d < 60, -0.3, 0.3)
	}},
	{"agent starts helping at day 60", func(a string, d int) float64 { return pick(a == prepArmAgent && d >= 60, -0.3, 0) }},
	{"both 16% worse", func(string, int) float64 { return 0.15 }},
}

func pick(c bool, a, b float64) float64 {
	if c {
		return a
	}
	return b
}

// simPolicy gives the shares of none, chat and agent for a day's tasks.
type simPolicy interface {
	shares(day int, tasks []simTask) []float64
}

type simTask struct {
	day  int
	arm  int
	x    []float64
	y    float64
	seen bool
}

var simArms = []string{prepArmChat, prepArmAgent}

// simThompson is the policy as built, with a half-life (0: nothing ages).
type simThompson struct{ halfLife float64 }

func (p simThompson) shares(day int, tasks []simTask) []float64 {
	obs := make([]prepObs, 0, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		if !t.seen {
			continue
		}
		load := make([]float64, 2)
		if t.arm > 0 {
			load[t.arm-1] = 1
		}
		w := 1.0
		if p.halfLife > 0 {
			w = prepRecency(float64(day-t.day), p.halfLife)
		}
		obs = append(obs, prepObs{Y: t.y, X: t.x, Arm: load, Age: float64(day - t.day), W: w})
	}
	f := fitPrepArms(simArms, obs, simSpread)
	return prepShares(f.pBest([][]float64{{0, 0}, {1, 0}, {0, 1}}), nil)
}

// simMix is the policy as built: fits at every half-life, mixed by how well
// each forecast the last prepForecastDays days. The forecasts are kept from
// day to day here (each morning's fits score that day's tasks once they are
// seen), which is the same sum prepForecastScore works out afresh.
type simMix struct {
	halfLives []float64
	window    int
	fits      [][]*prepFit
	score     [][]float64 // each day's tasks are scored under that morning's fits, so no score uses later data
}

func (p *simMix) shares(day int, tasks []simTask) []float64 {
	hls, window := p.halfLives, p.window
	if hls == nil {
		hls = prepHalfLives
	}
	if window == 0 {
		window = prepForecastDays
	}
	obs := make([]prepObs, 0, len(tasks))
	var yesterday []*prepObs
	for i := range tasks {
		t := &tasks[i]
		if !t.seen {
			continue
		}
		load := make([]float64, 2)
		if t.arm > 0 {
			load[t.arm-1] = 1
		}
		obs = append(obs, prepObs{Y: t.y, X: t.x, Arm: load, Age: float64(day - t.day)})
		if t.day == day-1 {
			yesterday = append(yesterday, &obs[len(obs)-1])
		}
	}
	if day > 0 {
		sc := make([]float64, len(hls))
		for h, f := range p.fits[day-1] {
			sc[h] = f.logLik(yesterday)
		}
		p.score = append(p.score, sc)
	}
	ll := make([]float64, len(hls))
	for d := max(len(p.score)-window, 0); d < len(p.score); d++ {
		for h := range ll {
			ll[h] += p.score[d][h]
		}
	}
	m := &prepMix{HalfLives: hls, W: prepModelWeights(ll)}
	for _, hl := range hls {
		m.Fits = append(m.Fits, fitPrepArms(simArms, prepAged(obs, hl, 0), simSpread))
	}
	p.fits = append(p.fits, m.Fits)
	return prepShares(m.pBest([][]float64{{0, 0}, {1, 0}, {0, 1}}), nil)
}

// simFixed is a fixed equal split.
type simFixed struct{}

func (simFixed) shares(int, []simTask) []float64 { return []float64{1.0 / 3, 1.0 / 3, 1.0 / 3} }

// simCommit is the schedule this replaced: chat against agent, half each,
// until each has 40 measured tasks, then the one with the lower mean for
// everyone, and the test again 30 days after.
type simCommit struct {
	testing  bool
	from     int
	choice   int
	nextTest int
}

func (p *simCommit) shares(day int, tasks []simTask) []float64 {
	if !p.testing && day >= p.nextTest {
		p.testing, p.from = true, day
	}
	if p.testing {
		var n, sum [3]float64
		for i := range tasks {
			if t := &tasks[i]; t.seen && t.day >= p.from {
				n[t.arm]++
				sum[t.arm] += t.y
			}
		}
		if n[1] >= 40 && n[2] >= 40 {
			p.choice = 1
			if sum[2]/n[2] < sum[1]/n[1] {
				p.choice = 2
			}
			p.testing, p.nextTest = false, day+30
		} else {
			return []float64{0, 0.5, 0.5}
		}
	}
	out := make([]float64, 3)
	out[p.choice] = 1
	return out
}

type simResult struct {
	regret   float64
	worse    float64
	last     []float64
	afterAt  int // -1 if the best arm never holds half the tasks after day 60
	shareSum []float64
}

// simRun plays one policy through one scenario for days.
func simRun(sc simScenario, pol simPolicy, days int, seed uint64) simResult {
	r := &prepRand{s: seed}
	var tasks []simTask
	res := simResult{afterAt: -1, shareSum: make([]float64, 3)}
	var regret, worse float64
	for day := range days {
		for i := range tasks {
			if tasks[i].day < day {
				tasks[i].seen = true
			}
		}
		sh := pol.shares(day, tasks)
		res.last = sh
		for i := range sh {
			res.shareSum[i] += sh[i] / float64(days)
		}
		eff := []float64{0, sc.effect(prepArmChat, day), sc.effect(prepArmAgent, day)}
		best, bestAt := 0.0, 0
		for i, e := range eff {
			if e < best {
				best, bestAt = e, i
			}
		}
		if day >= 60 && res.afterAt < 0 && sh[bestAt] >= 0.5 {
			res.afterAt = day - 60
		}
		for range simPerDay {
			arm := prepPick(sh, r.float())
			u := r.float()
			pb, pc := 0.0, 0.0
			level := 0.0
			switch {
			case u < 0.3:
				pb, level = 1, 0.4
			case u < 0.4:
				pc, level = 1, -0.3
			}
			text := r.norm()
			x := []float64{1, pb, pc, text, float64(day)/float64(days) - 0.5}
			y := 2.2 + level + 0.2*text + eff[arm] + simSpread*r.norm()
			tasks = append(tasks, simTask{day: day, arm: arm, x: x, y: y})
			regret += eff[arm] - best
			if eff[arm]-best > 0.05 {
				worse++
			}
		}
	}
	n := float64(days * simPerDay)
	res.regret, res.worse = regret/n, worse/n
	return res
}

func simMean(sc simScenario, mk func() simPolicy, days, reps int) simResult {
	out := simResult{shareSum: make([]float64, 3), last: make([]float64, 3)}
	after, afterN := 0.0, 0
	for rep := range reps {
		r := simRun(sc, mk(), days, uint64(1000+rep))
		out.regret += r.regret / float64(reps)
		out.worse += r.worse / float64(reps)
		for i := range 3 {
			out.shareSum[i] += r.shareSum[i] / float64(reps)
			out.last[i] += r.last[i] / float64(reps)
		}
		if r.afterAt >= 0 {
			after += float64(r.afterAt)
			afterN++
		}
	}
	out.afterAt = -1
	if afterN > 0 {
		out.afterAt = int(math.Round(after / float64(afterN)))
	}
	return out
}

// TestPrepPolicySimulation holds the allocation to what it is for, on a
// short run of each case: an arm that is worse loses its tasks, one that
// is better gets them, and a change is followed.
func TestPrepPolicySimulation(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	mk := func() simPolicy { return &simMix{} }
	const reps = 4
	worse := simMean(simScenarios[1], mk, 60, reps)
	if worse.last[1] > 0.05 || worse.shareSum[1] > 0.12 {
		t.Errorf("chat 35%% worse: it still has %.2f of new tasks after 60 days and had %.2f of all (a fixed split gives 0.33)", worse.last[1], worse.shareSum[1])
	}
	better := simMean(simScenarios[2], mk, 60, reps)
	if better.last[2] < 0.8 {
		t.Errorf("agent 26%% better: it has %.2f of new tasks after 60 days", better.last[2])
	}
	null := simMean(simScenarios[0], mk, 60, reps)
	for i, s := range null.shareSum {
		if s < 0.1 || s > 0.6 {
			t.Errorf("no arm does anything: arm %d had %.2f of the tasks", i, s)
		}
	}
	drift := simMean(simScenarios[4], mk, 120, reps)
	if drift.last[1] > 0.2 {
		t.Errorf("chat worse from day 60: it has %.2f of new tasks at day 120", drift.last[1])
	}
	fixed := simMean(simScenarios[1], func() simPolicy { return simFixed{} }, 60, reps)
	if worse.regret > fixed.regret/2 {
		t.Errorf("chat 35%% worse: regret %.3f against a fixed split's %.3f", worse.regret, fixed.regret)
	}
}

// TestPrepPolicySimulationTable prints the comparison the constants were
// chosen on (WP_PREP_SIM=1; minutes).
func TestPrepPolicySimulationTable(t *testing.T) {
	if os.Getenv("WP_PREP_SIM") == "" {
		t.Skip("set WP_PREP_SIM=1 to print the table")
	}
	const days, reps = 180, 12
	type named struct {
		name string
		mk   func() simPolicy
	}
	pols := []named{
		{"as built: half-lives mixed", func() simPolicy { return &simMix{} }},
	}
	switch os.Getenv("WP_PREP_SIM") {
	case "mix":
		pols = []named{
			{"mix 14-120, 14 days", func() simPolicy { return &simMix{halfLives: []float64{14, 30, 60, 120}, window: 14} }},
			{"mix 3-120, 14 days", func() simPolicy { return &simMix{halfLives: []float64{3, 7, 14, 30, 60, 120}, window: 14} }},
			{"mix 3-120, 7 days", func() simPolicy { return &simMix{halfLives: []float64{3, 7, 14, 30, 60, 120}, window: 7} }},
			{"mix 7-120, 7 days", func() simPolicy { return &simMix{halfLives: []float64{7, 14, 30, 60, 120}, window: 7} }},
			{"mix 7-240, 28 days", func() simPolicy { return &simMix{halfLives: []float64{7, 14, 30, 60, 120, 240}, window: 28} }},
		}
	case "few":
		pols = append(pols, []named{
			{"thompson, half-life 60d", func() simPolicy { return simThompson{60} }},
			{"fixed thirds", func() simPolicy { return simFixed{} }},
			{"test 40+40, commit, again in 30d", func() simPolicy { return &simCommit{} }},
		}...)
	default:
		pols = append(pols, []named{
			{"thompson, half-life 14d", func() simPolicy { return simThompson{14} }},
			{"thompson, half-life 30d", func() simPolicy { return simThompson{30} }},
			{"thompson, half-life 60d", func() simPolicy { return simThompson{60} }},
			{"thompson, half-life 120d", func() simPolicy { return simThompson{120} }},
			{"thompson, nothing ages", func() simPolicy { return simThompson{0} }},
			{"fixed thirds", func() simPolicy { return simFixed{} }},
			{"test 40+40, commit, again in 30d", func() simPolicy { return &simCommit{} }},
		}...)
	}
	var b strings.Builder
	for _, sc := range simScenarios {
		fmt.Fprintf(&b, "\n%s (%d days, %d a day, %d runs)\n", sc.name, days, simPerDay, reps)
		fmt.Fprintf(&b, "  %-34s %8s %8s  %-20s %s\n", "policy", "regret", "worse", "shares none/chat/agent", "days to follow day 60")
		for _, p := range pols {
			r := simMean(sc, p.mk, days, reps)
			fmt.Fprintf(&b, "  %-34s %8.4f %7.1f%%  %.2f / %.2f / %.2f        %d\n", p.name, r.regret, r.worse*100, r.shareSum[0], r.shareSum[1], r.shareSum[2], r.afterAt)
		}
	}
	t.Log(b.String())
}
