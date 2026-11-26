package main

import (
	"net/url"
	"strconv"
)

var activityCmd = &command{
	name: "activity", group: "Tasks", args: "[SPEC] [--days N]",
	summary: "what agents did lately: tasks, time, cost, PRs",
	about: `SPEC narrows to an agent as when delegating (claude, opus,
anthropic/claude-code). Same numbers as the Agents page.`,
	examples: []string{"wp report activity claude --days 7", "wp report activity -A"},
	run:      cmdActivity,
}

func cmdActivity(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	days := fs.Int("days", 30, "last `N` days")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return c.usage("give one agent spec (quote one with spaces: \"haiku 5.5\")")
	}
	if *days < 1 || *days > 365 {
		return c.usage("--days must be 1 to 365")
	}
	q := url.Values{"days": {strconv.Itoa(*days)}}
	if len(pos) == 1 {
		q.Set("agent", pos[0])
	}
	sc, err := c.scopeFor(sf)
	if err != nil {
		return err
	}
	if !sc.all() {
		home, err := sc.home()
		if err != nil {
			return err
		}
		q.Set("project", home)
		c.note("Activity in %s, from %s.", home, sc.from)
	}
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", "/api/v1/agents/activity?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}
