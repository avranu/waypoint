package main

import "net/url"

var mistakesCmd = &command{
	name: "mistakes", group: "Code and CI", args: "[-p PROJECT]", summary: "recurring kinds of mistake, their tasks, and whether a check stopped them",
	about: `Each hour Waypoint groups its own records, without a model: bug tasks by the
files their merged fixes changed, review and audit findings that held up, data
races and flakes from CI, and tests that keep going red on the default branch.
Three independent instances of one kind within 14 days file one task to make
it a check, with the instances listed. At most 2 tasks a day per project.

A class is filed once. Closing its task as Won't Do declines it; Done counts
as the check landing, and instances after that are shown as recurrences.
A project's settings can turn the filing off.`,
	examples: []string{"wp report quality mistakes", "wp report quality mistakes -p WP", "wp report quality mistakes --json"},
	run:      cmdMistakes,
}

func cmdMistakes(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "in `PROJECT` (key, name or repo; default this repo's)")
	short(fs, "p", "project")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	sc, err := c.scopeFor(scopeFlags{project: project})
	if err != nil {
		return err
	}
	home, err := sc.home()
	if err != nil {
		return err
	}
	q := url.Values{"project": {home}}
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", "/api/v1/mistakes?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}
