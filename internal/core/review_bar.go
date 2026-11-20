package core

import (
	"fmt"
	"strings"
)

// Trusted reviewers for high-stakes changes (WP-916, WP-ADR102). A review
// round's audit goes to the gateway's audit purpose, which serves
// whichever of its models ranks best and has room, free ones first. For a
// change whose triage found strong signs that it matters (highStakes), the
// round holds its audit to a quality bar instead: the gateway runs it only
// on a model it rates at least as well for audits as trustedAuditBar
// (routing.weakest). The gateway rates by benchmarks and
// by how callers report each model's findings fared (reportAuditFeedback),
// so the bar follows newer versions of the named model and admits any other
// model that does as well, with no change here.
//
// When no model at the bar can take the audit (none is that good outside
// the author's family, or they are all spent), the gateway says
// no_model_at_bar and the audit ends. What the round does then depends on
// its stakes tier (review_stakes.go, WP-1231): a high-stakes round goes to
// the Sonnet reviewer; an elevated one asks the gateway again at no bar.
// An elevated round's bar is lower, elevatedAuditBar (WP-1248).

// trustedAuditBar is the weakest model a high-stakes change's audit accepts,
// by the loose name the gateway rates (its best model of that name, so a
// newer Sonnet raises the bar with it). Grok 4.7 through Cursor is trusted
// as much, but the gateway can't run it as a task yet, and a
// request names one model as its bar.
//
// Not "sonnet": that is one of the gateway's routes (the cheapest way to
// reach Sonnet), and routing.weakest refuses a route with a 400, so every
// high-stakes audit failed before it started (WP-1134). "claude sonnet"
// names no route, purpose or provider, so the gateway matches it loosely:
// every model with both words in its name, the bar at the best of them.
const trustedAuditBar = "claude sonnet"

// elevatedAuditBar is the bar of an elevated change's audit (WP-1248): a
// medium-sized or medium-risk change asks for Claude Haiku or better, which
// keeps out the free models whose audits are mostly empty without holding
// out for the few at Sonnet's level. Not "haiku", for the same reason as
// trustedAuditBar: that is one of the gateway's routes.
const elevatedAuditBar = "claude haiku"

// highStakes says why a change's review needs a trusted reviewer, from the
// facts triage recorded at wp ship ("" when nothing does): a high-risk task,
// sensitive paths, or code the index shows breaks often or much depends on.
// Size alone doesn't count: it is what free models are measured on most.
func highStakes(f *TriageFeatures) string {
	var why []string
	if f.Provenance == RiskHigh {
		why = append(why, "the task's risk is high")
	}
	if len(f.Sensitive) > 0 {
		why = append(why, "it changes sensitive paths ("+strings.Join(f.Sensitive, ", ")+")")
	}
	if f.RiskyFiles > 0 {
		why = append(why, fmt.Sprintf("it changes %s with past bug fixes or many importers", plural1(f.RiskyFiles, "file")))
	}
	return strings.Join(why, "; ")
}
