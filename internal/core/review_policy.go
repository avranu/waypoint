package core

// The rule that decides whether another review round comes before a pull
// request (WP-1320), apart from everything it reads. reviewAgain gathers
// the facts (the round's findings and marks, the change's stakes, the
// loop's history, the reviewer's and the rounds' records) and asks the
// decision model; againPolicy.settle turns them into an arm. settle reads
// no database and no clock, so every case of the rule can be listed in a
// test, and the numbers it turns on are one value (againPolicy), not
// constants spread through the code that gathers.

import (
	"errors"
	"maps"
)

// againPolicy is every number the rule turns on.
type againPolicy struct {
	// bars and floors by stakes tier (againBars, againFloors); a tier
	// without one takes the unknown tier's.
	bars   map[string]againBar
	floors map[string]float64
	// cap: rounds of one change before its pull request past which no more
	// come (reviewRoundsCap).
	cap int
	// yieldFloor, yieldMin: a record of at least yieldMin rounds, drawn
	// under yieldFloor, ends a loop the scores alone called for; a probe
	// needs its record's draw at or over yieldFloor.
	yieldFloor float64
	yieldMin   int
	// needBase, needGrowth: the signal weight another round needs after the
	// change's k-th round (k = 1 the first) is needBase * needGrowth^(k-1)
	// (WP-1569): 1 after round 1, then 2, 4, 8 and on.
	needBase, needGrowth int
}

// defaultAgainPolicy is the rule's numbers as they stand.
func defaultAgainPolicy() againPolicy {
	return againPolicy{bars: maps.Clone(againBars), floors: maps.Clone(againFloors), cap: reviewRoundsCap,
		yieldFloor: againYieldFloor, yieldMin: againYieldMin, needBase: againNeedBase, needGrowth: againNeedGrowth}
}

// needFor is the signal weight another round needs after the change's
// k-th round (k = 1 the first): needBase * needGrowth^(k-1). The growth is
// capped where it can't overflow; the round cap ends loops long before.
func (p againPolicy) needFor(k int) int {
	need := p.needBase
	for range min(max(k, 1)-1, 30) {
		need *= p.needGrowth
	}
	return need
}

func (p againPolicy) bar(tier string) againBar {
	if b, ok := p.bars[tier]; ok {
		return b
	}
	return p.bars[StakesUnknown]
}

func (p againPolicy) floor(tier string) float64 {
	if f, ok := p.floors[tier]; ok {
		return f
	}
	return p.floors[StakesUnknown]
}

// againFacts is what a decision turns on besides the round's findings and
// the decision model's answers. Every read is made before settle; one that
// failed is carried as its error and stops the decision only when the rule
// comes to need it, so a round a signal calls isn't lost to a record it
// never reads, and none is decided as if a failed read had said no.
type againFacts struct {
	// tier is the change's stakes tier (roundRisk).
	tier string
	// fixed and open count the session's marks over every finding of the
	// round; passed: the check of those marks was passed over.
	fixed, open int
	passed      bool
	// unread: the check passed with an own report nothing read against
	// its reviewer's words (checkMarks).
	unread bool
	// spent: the loop had its one later round read by labels alone;
	// probed: it had its one probe.
	spent, probed bool
	historyErr    error
	// trusted: the reviewer's record backs its claims (againTrust).
	trusted  bool
	trustErr error
	// probeYield and reviewYield are the draws from the records of the
	// rounds that followed probes, and again decisions, at this index.
	probeYield, reviewYield againYieldRead
	probeErr, yieldErr      error
}

// errAgainRead marks a decision stopped by a read it needed: the ship is
// refused with the read's error rather than the pull request opening on a
// rule that couldn't be applied.
var errAgainRead = errors.New("the review round couldn't be decided")

// settle takes the arm from the decision model's answers (none: answered
// false; noModel: there is no decision model, or it is waiting on its
// budget) on c and fs, its own copy of the round's findings: c.arm "" when
// another round comes, and probe when that round is a probe. It is the
// whole rule: reviewAgain's decision and what decideAgain reads each set
// of asks as deciding are both this.
func (p againPolicy) settle(c *againDecision, fs, earlier []againFinding, ans map[string]jevAnswer, answered, noModel bool, f againFacts) (probe bool, err error) {
	bar, floor := p.bar(f.tier), p.floor(f.tier)
	scored := answered && c.score(fs, earlier, ans)
	if !scored {
		c.labelOnly(fs)
		c.labels = 1
	}
	c.scored = fs
	c.countSignals(fs, floor)
	c.countUnchecked(fs, floor, f.passed)
	if c.unread = f.unread; f.unread {
		c.unchecked++
	}
	// From the second round on, without the model no repeat can be told:
	// by labels every finding is new, and rounds read that way could go on
	// over the same ground. A signal or an unchecked rejection still gets
	// one such round a loop, so the pull request doesn't open on findings
	// no one followed up (WP-1320's live check of its own first round, 2.2).
	unscoredLater := !scored && len(earlier) > 0
	// A signal's weight must reach what this round index needs, and a
	// critical one calls on any round (WP-1569): c.index is the round being
	// decided, 1-based (reviewAgain sets it to len(earlier)+1), so after
	// the first round any signal calls and each later round asks for twice
	// the weight. An unchecked rejection calls on any round, as before.
	c.need = p.needFor(c.index)
	calls := c.critical || c.signals > 0 && c.weight >= c.need || c.unchecked > 0
	if unscoredLater && calls && f.historyErr != nil {
		return false, errors.Join(errAgainRead, f.historyErr)
	}
	spent := unscoredLater && f.spent
	switch {
	case calls && !spent:
		c.arm = ""
	case unscoredLater && noModel:
		c.arm = AgainNoModel
	case unscoredLater:
		c.arm = AgainUnscored
	case f.fixed == 0 && f.open == 0:
		c.arm = AgainRejected // the rejections were unchecked, but none would have been a signal
	case !scored && noModel:
		c.arm = AgainNoModel
	case !scored:
		c.arm = AgainUnscored
	default:
		c.arm = c.pick(bar)
	}
	if c.arm == AgainEnough && c.probeable() {
		// One probe a loop: probes skip stakes, the reviewer's record and
		// the yield floor, so rounds that kept scoring under the bar would
		// otherwise go on to the cap on the probe draw alone.
		if err := errors.Join(f.historyErr, f.probeErr); err != nil {
			return false, errors.Join(errAgainRead, err)
		}
		if !f.probed {
			if c.yieldP = f.probeYield.p; c.yieldP >= p.yieldFloor {
				c.arm, probe = "", true
			}
		}
	}
	// A signal is backed by the session's own mark, the check of it and
	// the check against the code: neither the reviewer's record nor the
	// yield record overrides it.
	scoresCall := c.arm == "" && !probe && !calls
	if scoresCall && c.fixesMean < bar.risky {
		// How the findings and fixes sound is what calls: the reviewer's
		// record must back it.
		switch {
		case f.trustErr != nil:
			return false, errors.Join(errAgainRead, f.trustErr)
		case !f.trusted:
			c.arm = AgainUntrusted
		}
	}
	if c.arm == "" && !probe {
		// A record that couldn't be read ends no loop: the round comes.
		if f.yieldErr == nil && f.reviewYield.n >= p.yieldMin {
			c.yieldP = f.reviewYield.p
			if c.yieldP < p.yieldFloor && scoresCall {
				c.arm = AgainLowYield
			}
		}
	}
	return probe, nil
}
