package core

import (
	"fmt"
	"strings"
)

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
