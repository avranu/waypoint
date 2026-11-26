package main

import (
	"fmt"
	"slices"
)

// The reports and records that moved under wp report and wp record in stage 2
// of WP-ADR164. The commands themselves are where they were written; this
// file names the new parents and the ones that are not built yet.

var reportCICmd = &command{
	name: "ci", args: "[TOPIC] [FLAGS]",
	summary: "how CI fared: history, the merge line, tests, coverage, suppressions",
	about: `Reads of CI's record. wp report ci with no topic lists them. What CI is doing
now (wp ci status), a job's log (wp ci log), the workflow lint (wp ci lint) and
the merge-line doctor (wp doctor ci) stay under wp ci.`,
	examples: []string{"wp report ci history --days 90 --week", "wp report ci tests --flaky", "wp report ci coverage internal/core"},
	run:      cmdSubjects,
}

var reportQualityCmd = &command{
	name: "quality", args: "[TOPIC] [FLAGS]",
	summary: "what went wrong and was caught: mistakes, escaped bugs, review rounds, triage, stakes",
	about: `Reads of how well the checks and reviews held. wp report quality with no topic
lists them. wp escapes ls|show|trace|prove stay: they list rows and run rounds.`,
	examples: []string{"wp report quality mistakes", "wp report quality escapes --days 30", "wp report quality reviews -p WP"},
	run:      cmdSubjects,
}

var qualityEscapesCmd = &command{
	name: "escapes", args: "[-p KEY] [--repo R] [--days N]",
	summary: "bugs fixed: what introduced them, what found them, how long they lived",
	about: `The escape ledger: for each merged fix linked to a bug task, the change
blamed for it, how it was found and how long it lived. Prints escapes per
week, median lifetime, the split by stage and detector, and how many fixes
a test proves (it fails without the fix). Runners fill it; the rows, one
fix and the trace and prove rounds are wp escapes ls|show|trace|prove.`,
	examples: []string{"wp report quality escapes", "wp report quality escapes --days 30 -p WP"},
	run:      cmdEscapes,
}

var qualityReviewsCmd = &command{
	name: "reviews", args: "[-p PROJECT] [--days N]",
	summary:  "review rounds over the last days: findings, what held up, cost",
	about:    "Sums review rounds over the last --days (30), for -p or all projects.",
	examples: []string{"wp report quality reviews -p WP --days 90"},
	run:      cmdWorklogStats,
}

var qualityTriageCmd = &command{
	name: "triage", args: "[-p PROJECT] [--days N] [--rows]",
	summary:  "how review findings were assessed and labelled",
	about:    "The triage of review findings over the last --days (30), for -p or all projects. --rows with --json adds every assessment and its labels.",
	examples: []string{"wp report quality triage --days 14"},
	run:      cmdWorklogTriage,
}

var qualityStakesCmd = &command{
	name: "stakes", args: "[-p PROJECT] [--days N] [--rows]",
	summary:  "review rounds by stakes tier: what audits and Claude reviews did, cost and found",
	about:    "Review rounds sorted by stakes tier (high, elevated, normal) over the last --days (14). --rows with --json adds every round.",
	examples: []string{"wp report quality stakes --days 30"},
	run:      cmdWorklogStakes,
}

// notBuilt is a report named by WP-ADR164 that does not exist yet: one line
// naming what to run instead, and exit 2.
func notBuilt(name, instead string) *command {
	return &command{
		name: name, hidden: true, summary: "not built yet",
		about:    fmt.Sprintf("wp report %s is not built yet. Nearest now: %s.", name, instead),
		examples: []string{"wp report " + name},
		run: func(c *cli, args []string) error {
			_, _ = fmt.Fprintf(c.err, "wp report %s is not built yet; nearest now: %s\n", name, instead)
			return exitCode(2)
		},
	}
}

var (
	reportTasksCmd    = notBuilt("tasks", "wp ls, wp report activity")
	reportRequestsCmd = notBuilt("requests", "wp request ls --open")
)

// takeSubs removes the named subcommands from cmd and returns them.
func takeSubs(cmd *command, names ...string) []*command {
	var out []*command
	cmd.subs = slices.DeleteFunc(cmd.subs, func(s *command) bool {
		if slices.Contains(names, s.name) {
			out = append(out, s)
			return true
		}
		return false
	})
	return out
}
