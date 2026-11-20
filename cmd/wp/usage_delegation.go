package main

import (
	"encoding/json"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp usage delegation: how agents are told to split their work between
// models, and what came of each way (WP-1262).

var usageDelegationCmd = &command{
	name: "delegation", args: "",
	summary: "how agents are told to delegate to cheaper models, and what came of it",
	about: `Each task an agent takes is given one way of working, once, and keeps it
in its review rounds: none (nothing said: the agent's own way), scouts
(Haiku subagents search, read, summarize and review slices; the agent
edits) or manager (Haiku scouts, and a Sonnet subagent implements and fixes;
the agent decides, checks claims and reads the diff). A task whose risk is
high is given none (excluded); an agent whose usage Waypoint doesn't see
gets the best arm (best). Neither is evidence.

Shares of new tasks follow each arm's probability of costing least (the
task's Claude cost at API prices), with project, type, risk, the text's
length and time taken out. An arm worse than none on a guard (not done in
14 days, review findings on its pull requests, audit findings fixed in its
rounds, follow-up fix tasks, escapes) by more than its margin, with
probability over 0.975 on at least 20 tasks, gets none.

Per arm, as the tasks came out: cost (median, mean, share by model and by
main thread or subagents), subagents, tool calls, context, compactions,
done, pull requests, review findings by severity, audit findings by
verdict, follow-up fixes, tasks filed, escapes and review stakes tiers.
Admins only: it sums every person's usage.`,
	examples: []string{"wp usage delegation", "wp usage delegation --json"},
	run:      cmdUsageDelegation,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageDelegationCmd)
}

func cmdUsageDelegation(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	raw, err := c.call("GET", "/api/v1/usage/delegation", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r core.DelegationReport
		if json.Unmarshal(raw, &r) != nil {
			return string(raw)
		}
		return r.String()
	})
}
