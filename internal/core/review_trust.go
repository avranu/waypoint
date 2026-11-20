package core

// Trusted reviewers (WP-696). A review by a model that does this job well
// doesn't need a second model to check its findings, which is what put a
// second quota on every pull request. The findings it posts are the same
// counts the gateway gets (review_feedback.go): once a model has enough of them
// judged, they decide whether it is trusted, whichever way that goes. Until
// then the seed list does.
//
// A trusted reviewer's findings count for neither review arm: they would
// make "no verifier" look good, and that arm also carries the weak models.

import (
	"context"
	"strings"
)

// trustFloor is the lower bound a model's precision must reach to be
// trusted, at the bench quantile's mirror (10%). It sits between the models
// that review well (Haiku 5.5 at 58-64% over hundreds of findings) and those
// that don't (Gemini 3 Flash at 39%, the rest lower).
const trustFloor = 0.45

// trustedReviewerSeeds are trusted until measured, matched as prefixes of
// the normalized model name, so a reasoning level or a tier keeps the trust.
var trustedReviewerSeeds = []string{
	"claude-haiku-5-5",
	"claude-sonnet-5-5",
	"claude-opus-5-5",
	"grok-4.7",
	"muse-spark-1.3",
	"deepseek-4.1-flash",
}

// seedSpellings are the other names providers give a seeded model.
var seedSpellings = strings.NewReplacer("deepseek-v4.1-flash", "deepseek-4.1-flash", "deepseek-v4-1-flash", "deepseek-4.1-flash")

// reviewerTrust answers which models are trusted, per the records of a
// moment.
type reviewerTrust struct {
	seeds    []string
	measured map[string]bool // only models with enough judged findings
}

// runnerAliases are the short names the claude CLI takes for a seeded model.
var runnerAliases = map[string]string{"haiku": "claude-haiku-5-5", "sonnet": "claude-sonnet-5-5", "opus": "claude-opus-5-5"}

// runnerModel is the model in a runner entry's name ("claude sonnet
// (runner)", runnerEntryName); any other name is already the model.
func runnerModel(name string) string {
	entry, ok := strings.CutSuffix(strings.TrimSpace(name), " (runner)")
	if !ok {
		return name
	}
	_, model, _ := strings.Cut(entry, " ")
	if full, ok := runnerAliases[model]; ok {
		return full
	}
	return model
}

// recordOf is the name a model's findings are counted under, and whether
// that is a seed. Every variant of a seed shares one record, so what is
// measured of one reasoning level or tier decides for all of them. What
// follows the seed must not carry on its version: grok-4.70 isn't grok-4.7.
func (t reviewerTrust) recordOf(model string) (string, bool) {
	m := seedSpellings.Replace(normalizeModel(runnerModel(model)))
	for _, seed := range t.seeds {
		if rest, ok := strings.CutPrefix(m, seed); ok && (rest == "" || !strings.ContainsRune("0123456789.", rune(rest[0]))) {
			return seed, true
		}
	}
	return m, false
}

func (t reviewerTrust) trusts(model string) bool {
	name, seeded := t.recordOf(model)
	if ok, known := t.measured[name]; known {
		return ok
	}
	return seeded
}

// reviewerTrust reads the judged findings of every model. All verifier
// states count: what is being measured is the reviewer, not the arm. Enough
// findings is a count, whatever their age, so old dismissals aren't ignored
// in favor of the seed list; age only weighs them in the precision.
//
// What a verifier dropped before it was posted counts against a model that
// isn't seeded: trust takes its verifier away, so those findings are what it
// would post, and a good record of only what got through proves nothing. A
// seed is trusted until people judge otherwise, so there it is no judgment.
func (s *Service) reviewerTrust(ctx context.Context) (reviewerTrust, error) {
	seeds := s.trustSeeds
	if seeds == nil {
		seeds = trustedReviewerSeeds
	}
	out := reviewerTrust{seeds: seeds, measured: map[string]bool{}}
	fs, err := s.judgedFindings(ctx, VerifyKept, VerifyCorrected, VerifySkipped, VerifyTrusted, VerifyDropped, VerifyUnchecked)
	if err != nil {
		return out, err
	}
	type count struct{ useful, falsePos, judged float64 }
	counts := map[string]*count{}
	now := s.now()
	for _, f := range fs {
		m, seeded := out.recordOf(f.model)
		if m == "" || (seeded && f.state == FindingDropped && f.verify == VerifyDropped) {
			continue
		}
		c := counts[m]
		if c == nil {
			c = &count{}
			counts[m] = c
		}
		w := armWeight(now, f, f.version, "") // by age only: the reviewer, whatever its prompt
		c.judged++
		if f.useful {
			c.useful += w
		} else {
			c.falsePos += w
		}
	}
	for m, c := range counts {
		if c.judged >= armExploreUnder {
			out.measured[m] = betaQuantile(1-armBenchQ, 1+c.useful, 1+c.falsePos) >= trustFloor
		}
	}
	return out, nil
}

// markTrusted marks the findings a trusted reviewer wrote. A trusted reviewer
// merely agreeing isn't enough: the claim posted is the first reviewer's
// (mergeReviewers), and that is the model the judgment of it is charged to.
// Reading the records failing is no reason to skip a review: its findings
// then go to the verifier, which costs a call and nothing else.
func (s *Service) markTrusted(ctx context.Context, found []Finding) []Finding {
	if len(found) == 0 {
		return found
	}
	trust, err := s.reviewerTrust(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "review: read the reviewers' records; verifying as usual", "err", err)
		return found
	}
	for n := range found {
		if trust.trusts(found[n].Model) {
			found[n].Verify = VerifyTrusted
		}
	}
	return found
}
