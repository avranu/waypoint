package main

import (
	"os"
	"slices"
)

// A task session (WP_TASK, WP-97) ends itself once its tasks are done: wp
// done reports that nothing follows, and the Stop hook then ends it (the
// server checks no bound task is left). A fan-out package session tells its
// coordinator instead, which starts the next package (WP-148). wp done no
// longer starts the next task itself (WP-271): autopilot does that.

// reportHandoff tells the server what this task session's wp done did about
// the next task: only "started" and "none" let it end the session (WP-97).
func (c *cli) reportHandoff(outcome, detail string) {
	if len(splitKeys(os.Getenv("WP_TASK"))) == 0 {
		return
	}
	_, _ = c.call("POST", "/api/v1/agent/handoff/outcome", map[string]any{"outcome": outcome, "detail": detail})
}

// sessionDone runs after the tasks in keys are finished. It does nothing
// when this session is bound to tasks and none of them is among keys: the
// session's own work isn't done.
func (c *cli) sessionDone(keys ...string) {
	if held := splitKeys(os.Getenv("WP_TASK")); len(held) > 0 &&
		!slices.ContainsFunc(held, func(h string) bool { return slices.ContainsFunc(keys, func(k string) bool { return sameKey(h, k) }) }) {
		return
	}
	if id, _ := fanoutEnv(); id > 0 {
		c.fanoutHandOff(id)
		return
	}
	c.reportHandoff("none", "")
}
