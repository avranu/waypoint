package main

import (
	"net/url"
	"strconv"
)

var graphCmd = &command{
	name: "graph", group: "Knowledge", args: "THING [OTHER] [--hops N] [--limit N] [--sure] [--open] [--types T,…]",
	aliases: []string{"near"},
	summary: "what lies around a thing, or the nearest route between two",
	about: `Reads every way things relate as one graph (WP-385): issue relations,
parents, the task an issue was filed from, links and [[wikilinks]], pages
about a task, decisions and the goals they sit under, pull requests and
the files they changed, and the agent sessions that worked on or read a
task.

With one THING, lists what lies within --hops steps, nearest first, each
with the route there. With two, prints the nearest route between them.

Distance adds up each step: a parent, block or goal link is 1, a plain
"relates" or a link 1.5, a session naming a task 2. Steps an agent made
count 1.2 times, Waypoint's guesses 1.5 times. Passing through something
with many links adds 0.5·log2(its links), so a release pull request or a
busy goal joins things less than a direct link does.

Guesses (issues alike in words, Waypoint's own matches) are walked unless
--sure. THING is any code (UL-12, ADR7, PL3, R-4, a term), a file path, a
pull request URL or session:N.`,
	examples: []string{"wp graph WP-385", "wp graph WP-385 --hops 2 --types issue --open", "wp graph WP-232 WP-233",
		"wp graph internal/core/graph.go", "wp graph ADR7 --sure"},
	run: cmdGraph,
}

func cmdGraph(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	hops := fs.Int("hops", 3, "steps out, 1-6")
	limit := fs.Int("limit", 40, "most things listed")
	sure := fs.Bool("sure", false, "only links people and agents made, no guesses")
	open := fs.Bool("open", false, "only open issues (other kinds still listed)")
	types := fs.String("types", "", "list only these `KINDS`: issue,page,design,glossary,file,pull,session")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || len(pos) > 2 {
		return c.usage("graph what? Give one thing for what lies around it, or two for the route between them")
	}
	q := url.Values{}
	project := ""
	if sf.project != nil && *sf.project != "" {
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		project, _ = sc.home()
	} else {
		project = c.homeProject()
	}
	if project != "" {
		q.Set("project", project)
	}
	if *sure {
		q.Set("guesses", "0")
	}
	if len(pos) == 2 {
		q.Set("from", pos[0])
		q.Set("to", pos[1])
		return c.textCall("/api/v1/graph/path", q)
	}
	q.Set("ref", pos[0])
	q.Set("hops", strconv.Itoa(*hops))
	q.Set("limit", strconv.Itoa(*limit))
	if *open {
		q.Set("open", "1")
	}
	if *types != "" {
		q.Set("types", *types)
	}
	return c.textCall("/api/v1/graph", q)
}
