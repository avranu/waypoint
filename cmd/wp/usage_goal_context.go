package main

import (
	"encoding/json"

	"git.jmann.me/claude/waypoint/internal/core"
)

var usageGoalContextCmd = &command{
	name: "goal-context", args: "",
	summary: "how much of a task's parent goals agents are shown, and what came of it",
	about: `Each task an agent takes that traces to a goal with parent goals is given
one way of showing them, once, and keeps it: goal (only the goal it traces
to), chain (every parent goal, titles as written) or pruned (a parent the
task's text or a goal already shown covers is left out, a long title is
clipped at a word, and the chain stops when the titles get long). A
person's goal is never reworded.

Shares of new tasks follow each arm's probability of the fewest drift flags
on the task's pull requests that no person answered "this is fine", with
project, type, the text's length, the parents shown and time taken out. An
arm worse than goal on a guard (not done in 14 days, review findings,
follow-up fix tasks) by more than its margin, with probability over 0.975
on at least 20 tasks, gets none.

Per arm, as the tasks came out: done, pull requests, drift flags, review
findings and follow-up fix tasks. Admins only.`,
	examples: []string{"wp report behavior goal-context", "wp report behavior goal-context --json"},
	run:      cmdUsageGoalContext,
}

func cmdUsageGoalContext(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	raw, err := c.call("GET", "/api/v1/usage/goal-context", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r core.GoalContextReport
		if json.Unmarshal(raw, &r) != nil {
			return string(raw)
		}
		return r.String()
	})
}
