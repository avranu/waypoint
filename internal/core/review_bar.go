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
// (routing.weakest). The gateway rates by benchmarks and by how callers
// report each model's findings fared (reportAuditFeedback), so the bar admits
// any other model that does as well. It names one version: a newer one
// raises the bar only when the constant changes.
//
// There are three levels (WP-1440). Waypoint picks no model itself: it gives
// the gateway a purpose and a minimum, and the gateway chooses. High-stakes
// changes ask for trustedAuditBar, elevated ones for elevatedAuditBar
// (WP-1248), and every other request that reviews or audits code and has no
// higher bar asks for reviewFloorBar: the bot's reviewers and verifier, the
// round's audits, footprint audits, the audits of PRs, automations and
// objectives, and (the owner's later decision) fix checks, thread replies and
// held-edit checks. Prep, task safety, titles and estimates send no minimum.
// The floor is there at low stakes because a weak reviewer's false positives
// cost the agents that chase them tokens; it is not a claim about risk (the
// risk reasoning is for the two higher bars). A model a person named (a named
// reviewer, a CLI chain entry, a gateway model named for an audit) is not
// held to it.
//
// When no model at the bar can take the audit (or they are all spent), the
// gateway says no_model_at_bar and the audit ends. What the round does then
// depends on its stakes tier (review_stakes.go, WP-1231): a high-stakes round
// goes to the Sonnet reviewer; an elevated one asks the gateway again at the
// floor; a normal one, which asked at the floor already, goes to its Claude
// reviewer. A request is never sent again without its bar.

// reviewFloorBar is the minimum of every code-review request that has no
// higher one (WP-1440). It is a model name the gateway rates, not a route
// (see the trustedAuditBar comment about routes getting a 400).
const reviewFloorBar = "deepseek-v4.1-flash"

// trustedAuditBar is the weakest model a high-stakes change's audit accepts:
// Grok 4.7 (WP-1752), by the upstream id of Cursor's cursor/grok-4.7-medium,
// which the gateway resolves exactly. The bar, not the author's family, is
// what makes the reviewer trusted, so an audit held to it sends no
// exclude (auditFamilyExclude); few models outside Claude rated at Sonnet's
// level, so with that bar and the family exclusion these audits mostly failed.
//
// Not "grok": that is one of the gateway's routes, and routing.weakest
// refuses a route with a 400 (the same failure "sonnet" gave, WP-1134).
const trustedAuditBar = "grok-4.7-medium"

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
