package core

// againPolicy.settle reads no database and no clock, so every case of the rule can be listed in a test.

import (
	"errors"
	"maps"
)

// againPolicy is every number the rule turns on.
type againPolicy struct {
	bars                 map[string]againBar
	floors               map[string]float64
	cap                  int
	yieldFloor           float64
	yieldMin             int
	needBase, needGrowth int
}

// defaultAgainPolicy is the rule's numbers as they stand.
func defaultAgainPolicy() againPolicy {
	return againPolicy{bars: maps.Clone(againBars), floors: maps.Clone(againFloors), cap: reviewRoundsCap,
		yieldFloor: againYieldFloor, yieldMin: againYieldMin, needBase: againNeedBase, needGrowth: againNeedGrowth}
}

// needFor is the number of medium signals another round needs after the
// change's k-th round (k = 1 the first): needBase * needGrowth^(k-1). The growth is
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
	tier        string
	fixed, open int
	passed      bool
	unread      bool
	// spent: the loop had its one later round read by labels alone;
	// probed: it had its one probe.
	spent, probed           bool
	historyErr              error
	trusted                 bool
	trustErr                error
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
	// Without the model no repeat can be told, so a loop gets one labels-only round, and no PR opens on findings nobody followed up.
	unscoredLater := !scored && len(earlier) > 0
	c.need = p.needFor(c.index)
	calls := c.serious > 0 || c.weight > 0 && c.weight >= c.need || c.unchecked > 0
	calls = calls || unscoredLater && c.signals > 0
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
		if c.arm == AgainEnough && c.weight > 0 {
			c.arm = AgainWeighed
		}
	}
	if (c.arm == AgainEnough || c.arm == AgainWeighed) && c.probeable() {
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
		switch {
		case f.trustErr != nil:
			return false, errors.Join(errAgainRead, f.trustErr)
		case !f.trusted:
			c.arm = AgainUntrusted
		}
	}
	if c.arm == "" && !probe {
		if f.yieldErr == nil && f.reviewYield.n >= p.yieldMin {
			c.yieldP = f.reviewYield.p
			if c.yieldP < p.yieldFloor && scoresCall {
				c.arm = AgainLowYield
			}
		}
	}
	return probe, nil
}
