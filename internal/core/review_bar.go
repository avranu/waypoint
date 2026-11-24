package core

import (
	"fmt"
	"strings"
)

// Waypoint passes the gateway a minimum bar, never a model, and a request is never sent again without its bar.

// reviewFloorBar is the minimum of every code-review request that has no
// higher one (WP-1440). It is a model name the gateway rates, not a route
// (see the trustedAuditBar comment about routes getting a 400).
const reviewFloorBar = "deepseek-v4.1-flash"

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
