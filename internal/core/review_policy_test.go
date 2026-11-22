package core

import (
	"errors"
	"fmt"
	"testing"
)

// policyFinding is a round's finding for the rule's tests.
func policyFinding(id int64, severity, state, verdict string) againFinding {
	return againFinding{id: id, audit: 1, ordinal: int(id), title: fmt.Sprintf("finding %d", id), severity: severity, state: state, verdict: verdict}
}

// policyAnswers answers every finding with one severity, the fixes
// question with fixes, and each repeat question as repeats says (finding
// index, 1-based, to earlier index; others new).
func policyAnswers(n int, severity, fixes string, repeats map[int]int) map[string]jevAnswer {
	ans := map[string]jevAnswer{"fixes": {Choice: fixes, Probabilities: map[string]float64{fixes: 1}}}
	for i := 1; i <= n; i++ {
		ans[fmt.Sprintf("f%d", i)] = jevAnswer{Choice: severity, Probabilities: map[string]float64{severity: 1}}
		call := "new"
		if e, ok := repeats[i]; ok {
			call = fmt.Sprintf("e%d", e)
		}
		ans[fmt.Sprintf("r%d", i)] = jevAnswer{Choice: call, Probabilities: map[string]float64{call: 1}}
	}
	return ans
}

// The whole rule, a case a line: settle reads nothing but what it is given.
func TestAgainPolicySettle(t *testing.T) {
	boom := errors.New("database is locked")
	good := againFacts{tier: StakesElevated, trusted: true, probeYield: againYieldRead{p: 0.5}, reviewYield: againYieldRead{p: 0.5, n: 50}}
	with := func(edit func(*againFacts)) againFacts {
		f := good
		edit(&f)
		return f
	}
	earlier := []againFinding{{id: 90, title: "an earlier finding", severity: "high", state: "fixed"}}
	fixedHigh := []againFinding{policyFinding(1, "high", "fixed", "")}
	fixedLow := []againFinding{policyFinding(1, "low", "fixed", "")}
	for _, tc := range []struct {
		name               string
		findings, earlier  []againFinding
		severity, fixes    string
		repeats            map[int]int
		unanswered, absent bool // the model failed; there is none
		facts              againFacts
		arm                string // "": another round
		probe, fails       bool
	}{
		// What sends a round on.
		{name: "a fixed finding at the floor is a signal", findings: []againFinding{policyFinding(1, "medium", "fixed", "")}, severity: "cosmetic", fixes: "complete", facts: good},
		{name: "a signal needs no record", findings: fixedHigh, severity: "cosmetic", fixes: "complete",
			facts: with(func(f *againFacts) {
				f.trusted, f.trustErr, f.yieldErr, f.probeErr, f.historyErr = false, boom, boom, boom, boom
			})},
		{name: "a signal outlasts a poor yield record", findings: fixedHigh, severity: "critical", fixes: "complete",
			facts: with(func(f *againFacts) { f.reviewYield = againYieldRead{p: 0.01, n: 50} })},
		{name: "a confirmed finding stands as a fixed one does", findings: []againFinding{policyFinding(1, "high", "confirmed", "")}, severity: "minor", fixes: "complete", facts: good},
		{name: "under the floor, the model's score reaches the bar", findings: fixedLow, severity: "serious", fixes: "complete", facts: good},
		{name: "under the normal floor, a medium the model calls serious", findings: []againFinding{policyFinding(1, "medium", "fixed", "")}, severity: "serious", fixes: "complete",
			facts: with(func(f *againFacts) { f.tier = StakesNormal })},
		{name: "partial fixes from a trusted reviewer", findings: fixedLow, severity: "minor", fixes: "partial", facts: good},
		{name: "risky fixes need no record", findings: fixedLow, severity: "minor", fixes: "risky",
			facts: with(func(f *againFacts) { f.trusted, f.trustErr = false, boom })},
		{name: "an unchecked rejection at the floor", findings: []againFinding{policyFinding(1, "high", "rejected", "")}, severity: "minor", fixes: "complete",
			facts: with(func(f *againFacts) { f.passed = true })},
		{name: "the check's finding, rejected in turn", findings: []againFinding{policyFinding(1, "high", "rejected", ""), {id: 2, audit: 2, ordinal: 1, title: "[1.1] the rejection is wrong", severity: "high", state: "rejected"}},
			severity: "minor", fixes: "complete", facts: good},
		{name: "an unknown tier is held as elevated", findings: []againFinding{policyFinding(1, "medium", "fixed", "")}, severity: "minor", fixes: "complete",
			facts: with(func(f *againFacts) { f.tier = "no such tier" })},
		{name: "no model on a first round: the labels decide", findings: fixedHigh, absent: true, facts: good},
		{name: "no model on a later round: one round by labels", findings: fixedHigh, earlier: earlier, absent: true, facts: good},

		// What opens the pull request.
		{name: "nothing serious", findings: fixedLow, severity: "minor", fixes: "complete", facts: good, arm: AgainEnough},
		{name: "a medium on a normal change the model calls minor", findings: []againFinding{policyFinding(1, "medium", "fixed", "")}, severity: "minor", fixes: "complete",
			facts: with(func(f *againFacts) { f.tier, f.probed = StakesNormal, true }), arm: AgainEnough},
		{name: "a refuted finding is no signal", findings: []againFinding{policyFinding(1, "critical", "fixed", VerdictRefuted)}, severity: "critical", fixes: "complete", facts: good, arm: AgainEnough},
		{name: "a rejection the check upheld is no signal", findings: []againFinding{policyFinding(1, "high", "rejected", ""), policyFinding(2, "low", "fixed", "")}, severity: "minor", fixes: "complete",
			facts: with(func(f *againFacts) { f.fixed = 1 }), arm: AgainEnough},
		{name: "every finding repeats", findings: fixedHigh, earlier: earlier, severity: "critical", fixes: "complete", repeats: map[int]int{1: 1}, facts: good, arm: AgainRepeat},
		{name: "partial fixes of repeats alone", findings: fixedHigh, earlier: earlier, severity: "critical", fixes: "partial", repeats: map[int]int{1: 1}, facts: good, arm: AgainRepeat},
		{name: "the scores call, the reviewer has no record", findings: fixedLow, severity: "serious", fixes: "complete",
			facts: with(func(f *againFacts) { f.trusted = false }), arm: AgainUntrusted},
		{name: "the scores call, rounds here rarely pay", findings: fixedLow, severity: "serious", fixes: "complete",
			facts: with(func(f *againFacts) { f.reviewYield = againYieldRead{p: 0.01, n: 50} }), arm: AgainLowYield},
		{name: "unchecked rejections under the floor", findings: []againFinding{policyFinding(1, "low", "rejected", "")}, severity: "minor", fixes: "complete",
			facts: with(func(f *againFacts) { f.passed = true }), arm: AgainRejected},
		{name: "the model failed, nothing over the floor", findings: fixedLow, unanswered: true, facts: good, arm: AgainUnscored},
		{name: "no model, nothing over the floor", findings: fixedLow, absent: true, facts: good, arm: AgainNoModel},
		{name: "no model on a later round, its one round by labels spent", findings: fixedHigh, earlier: earlier, absent: true,
			facts: with(func(f *againFacts) { f.spent = true }), arm: AgainNoModel},
		{name: "the model failed on a later round, its round by labels spent", findings: fixedHigh, earlier: earlier, unanswered: true,
			facts: with(func(f *againFacts) { f.spent = true }), arm: AgainUnscored},

		// Records that aren't long enough, or can't be read, end nothing.
		{name: "a short yield record ends no loop", findings: fixedLow, severity: "serious", fixes: "complete",
			facts: with(func(f *againFacts) { f.reviewYield = againYieldRead{p: 0.01, n: 9} })},
		{name: "an unreadable yield record ends no loop", findings: fixedLow, severity: "serious", fixes: "complete",
			facts: with(func(f *againFacts) { f.reviewYield, f.yieldErr = againYieldRead{p: 0, n: 50}, boom })},

		// An own report nothing could read against its reviewer's words
		// calls a round as an unupheld rejection does, once a loop.
		{name: "an unread report alone calls a round", absent: true,
			facts: with(func(f *againFacts) { f.passed, f.unread = true, true })},
		{name: "an unread report, the loop's one unscored round spent", earlier: earlier, absent: true, arm: AgainNoModel,
			facts: with(func(f *againFacts) { f.passed, f.unread, f.spent = true, true, true })},
		{name: "a report read against its reviewer calls nothing", absent: true, arm: AgainRejected, facts: good},

		// The probe: a medium that stood on a normal change, once a loop.
		{name: "a probe", findings: []againFinding{policyFinding(1, "medium", "fixed", "")}, severity: "minor", fixes: "complete",
			facts: with(func(f *againFacts) { f.tier = StakesNormal }), probe: true},
		{name: "a probe whose record says no", findings: []againFinding{policyFinding(1, "medium", "fixed", "")}, severity: "minor", fixes: "complete",
			facts: with(func(f *againFacts) { f.tier, f.probeYield = StakesNormal, againYieldRead{p: 0.1} }), arm: AgainEnough},

		// Reads the rule needed and didn't get stop the decision.
		{name: "the scores call, the reviewer's record unreadable", findings: fixedLow, severity: "serious", fixes: "complete",
			facts: with(func(f *againFacts) { f.trusted, f.trustErr = false, boom }), fails: true},
		{name: "a probe, the loop's history unreadable", findings: []againFinding{policyFinding(1, "medium", "fixed", "")}, severity: "minor", fixes: "complete",
			facts: with(func(f *againFacts) { f.tier, f.historyErr = StakesNormal, boom }), fails: true},
		{name: "a probe, its record unreadable", findings: []againFinding{policyFinding(1, "medium", "fixed", "")}, severity: "minor", fixes: "complete",
			facts: with(func(f *againFacts) { f.tier, f.probeErr = StakesNormal, boom }), fails: true},
		{name: "a later round by labels, the loop's history unreadable", findings: fixedHigh, earlier: earlier, absent: true,
			facts: with(func(f *againFacts) { f.historyErr = boom }), fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := tc.facts
			if facts.fixed == 0 && facts.open == 0 {
				for _, f := range tc.findings {
					if f.state == "fixed" {
						facts.fixed++
					}
				}
			}
			var ans map[string]jevAnswer
			answered := !tc.unanswered && !tc.absent
			if answered {
				ans = policyAnswers(len(tc.findings), tc.severity, tc.fixes, tc.repeats)
			}
			d := againDecision{audit: 1, index: len(tc.earlier) + 1, yieldP: -1}
			probe, err := defaultAgainPolicy().settle(&d, tc.findings, tc.earlier, ans, answered, tc.absent, facts)
			if tc.fails {
				if !errors.Is(err, errAgainRead) || !errors.Is(err, boom) {
					t.Fatalf("err = %v, want the read's error", err)
				}
				return
			}
			if err != nil || d.arm != tc.arm || probe != tc.probe {
				t.Fatalf("arm %q probe %v err %v, want arm %q probe %v (decision %+v)", d.arm, probe, err, tc.arm, tc.probe, d)
			}
		})
	}
}

// A bar no finding under its tier's floor can reach decides nothing: the
// model's severity scores were dead weight on three tiers of four until
// WP-1320's audit. Every tier's bar must be reachable by a finding one
// label under its floor at some score the model can give, and not by the
// model calling such a finding minor.
func TestAgainBarsReachable(t *testing.T) {
	p := defaultAgainPolicy()
	under := map[float64]float64{labelScore["medium"]: labelScore["low"], labelScore["high"]: labelScore["medium"]}
	for _, tier := range []string{StakesHigh, StakesElevated, StakesUnknown, StakesNormal} {
		label, ok := under[p.floor(tier)]
		if !ok {
			t.Fatalf("%s: no label under its floor %v", tier, p.floor(tier))
		}
		bar := p.bar(tier)
		if top := (label + 10) / 2; top < bar.worst {
			t.Errorf("%s: a finding labelled %v can reach at most %v, under its bar %v", tier, label, top, bar.worst)
		}
		if low := (label + 3) / 2; low >= bar.worst {
			t.Errorf("%s: a finding labelled %v the model calls minor scores %v, over its bar %v", tier, label, low, bar.worst)
		}
		if bar.fixes > bar.risky || bar.risky > 9 {
			t.Errorf("%s: fixes bar %v, risky %v: out of the fixes scale's order", tier, bar.fixes, bar.risky)
		}
	}
	if p.bar("no such tier") != p.bar(StakesUnknown) || p.floor("") != p.floor(StakesUnknown) {
		t.Error("a tier the rule doesn't know isn't held as unknown")
	}
}

// When a loop has raised more than the repeat judge reads, the earlier
// findings in the files this round points to are the ones kept.
func TestNearestEarlier(t *testing.T) {
	var earlier []againFinding
	for i := range 40 {
		earlier = append(earlier, againFinding{id: int64(i + 1), path: fmt.Sprintf("pkg/f%d.go", i)})
	}
	if got := nearestEarlier(earlier[:30], nil, 30); len(got) != 30 || got[0].id != 1 {
		t.Fatalf("at the bound: %d, first %d", len(got), got[0].id)
	}
	got := nearestEarlier(earlier, []againFinding{{path: "pkg/f39.go"}, {path: "pkg/f35.go"}, {path: ""}}, 30)
	if len(got) != 30 || got[0].id != 36 || got[1].id != 40 || got[2].id != 1 || got[29].id != 28 {
		t.Fatalf("kept %d: first %d, %d, %d, last %d", len(got), got[0].id, got[1].id, got[2].id, got[29].id)
	}
	if earlier[0].id != 1 || earlier[39].id != 40 {
		t.Fatal("the earlier findings were reordered in place")
	}
}

// The stop rule weighs the round and how many serious findings there are
// (WP-1569): a medium signal weighs 1, a high 2 (a critical is one too, and
// calls on any round besides); after the k-th review round the weight
// needed for another is 1 at k = 1, else 2^(k-1); an unchecked rejection
// calls on any round. d.index is 1-based: the round being decided is the
// index-th, which is len(earlier)+1.
func TestAgainPolicyWeighs(t *testing.T) {
	of := func(severity string, n int) []againFinding {
		var fs []againFinding
		for i := range n {
			fs = append(fs, policyFinding(int64(i+1), severity, "fixed", ""))
		}
		return fs
	}
	mixed := []againFinding{policyFinding(1, "medium", "fixed", ""), policyFinding(2, "high", "fixed", "")}
	base := againFacts{tier: StakesElevated, trusted: true, probed: true, probeYield: againYieldRead{p: 0.5}, reviewYield: againYieldRead{p: 0.5, n: 50}}
	grows := func(p *againPolicy) { p.needBase, p.needGrowth = 3, 3 }
	normal := func(f *againFacts) { f.tier = StakesNormal }
	// A high label alone scores over the elevated bar however the model reads
	// it, so a high the weight doesn't call is still called by the scores
	// when the reviewer's record backs them; with no record the weight shows.
	untrusted := func(f *againFacts) { f.trusted = false }
	for _, tc := range []struct {
		name         string
		k            int // the k-th review round of the change is being decided
		findings     []againFinding
		facts        func(*againFacts)
		policy       func(*againPolicy)
		calls        bool
		arm          string // when it doesn't call: the arm it takes
		need, weight int
	}{
		{name: "round 1, one medium", k: 1, findings: of("medium", 1), calls: true, need: 1, weight: 1},
		{name: "round 2, one medium", k: 2, findings: of("medium", 1), need: 2, weight: 1},
		{name: "round 2, two mediums", k: 2, findings: of("medium", 2), calls: true, need: 2, weight: 2},
		{name: "round 2, one high", k: 2, findings: of("high", 1), calls: true, need: 2, weight: 2},
		{name: "round 2, a medium and a high", k: 2, findings: mixed, calls: true, need: 2, weight: 3},
		{name: "round 3, one high: too light for the weight, an untrusted reviewer's scores can't call it", k: 3, findings: of("high", 1), need: 4, weight: 2, arm: AgainUntrusted, facts: untrusted},
		{name: "round 3, one high: the scores call it for a trusted reviewer", k: 3, findings: of("high", 1), calls: true, need: 4, weight: 2},
		{name: "round 3, two highs", k: 3, findings: of("high", 2), calls: true, need: 4, weight: 4},
		{name: "round 4, seven mediums", k: 4, findings: of("medium", 7), need: 8, weight: 7},
		{name: "round 4, eight mediums", k: 4, findings: of("medium", 8), calls: true, need: 8, weight: 8},
		{name: "a critical at round 6", k: 6, findings: of("critical", 1), calls: true, need: 32, weight: 2},
		{name: "a critical at round 9", k: 9, findings: of("critical", 1), calls: true, need: 256, weight: 2},
		{name: "unchecked at a late round", k: 7, calls: true, need: 64, facts: func(f *againFacts) { f.passed = true },
			findings: []againFinding{policyFinding(1, "high", "rejected", "")}},
		{name: "normal stakes: mediums weigh nothing", k: 1, findings: of("medium", 3), need: 1, facts: normal},
		{name: "normal stakes: a high at round 1", k: 1, findings: of("high", 1), calls: true, need: 1, weight: 2, facts: normal},
		{name: "normal stakes: a high at round 3", k: 3, findings: of("high", 1), need: 4, weight: 2, arm: AgainUntrusted, facts: func(f *againFacts) { normal(f); untrusted(f) }},
		{name: "a base and growth of 3: a high at round 2", k: 2, findings: of("high", 1), need: 9, weight: 2, arm: AgainUntrusted, facts: untrusted, policy: grows},
		{name: "a base and growth of 3: two mediums at round 1", k: 1, findings: of("medium", 2), need: 3, weight: 2, policy: grows},
		{name: "a base and growth of 3: three mediums at round 1", k: 1, findings: of("medium", 3), calls: true, need: 3, weight: 3, policy: grows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var earlier []againFinding
			for i := 1; i < tc.k; i++ {
				earlier = append(earlier, againFinding{id: int64(100 + i), title: fmt.Sprintf("earlier %d", i), severity: "high", state: "fixed"})
			}
			facts := base
			if tc.facts != nil {
				tc.facts(&facts)
			}
			p := defaultAgainPolicy()
			if tc.policy != nil {
				tc.policy(&p)
			}
			for _, f := range tc.findings {
				if f.state == "fixed" {
					facts.fixed++
				}
			}
			ans := policyAnswers(len(tc.findings), "minor", "complete", nil)
			// index is the round's place among the change's rounds, 1 the first.
			d := againDecision{audit: 1, index: len(earlier) + 1, yieldP: -1}
			if _, err := p.settle(&d, tc.findings, earlier, ans, true, false, facts); err != nil {
				t.Fatal(err)
			}
			if called := d.arm == ""; called != tc.calls || d.need != tc.need || d.weight != tc.weight || !tc.calls && tc.arm != "" && d.arm != tc.arm {
				t.Fatalf("arm %q need %d weight %d, want calls %v need %d weight %d", d.arm, d.need, d.weight, tc.calls, tc.need, tc.weight)
			}
		})
	}
}
