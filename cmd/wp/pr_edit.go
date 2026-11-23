package main

import (
	"fmt"
	"os"
)

var prEditCmd = &command{
	name: "edit", args: "[N] [--title T] [--body-file FILE] [--base BRANCH]",
	summary: "change a pull request's title, description or base",
	about: `Whoever may merge the pull request may edit it (an agent: its own). A
draft keeps its "WIP: " prefix. --base moves it to merge into another
branch, keeping its number and reviews.`,
	examples: []string{`wp pr edit 41 --title "Fix the redirect loop"`, `wp pr edit 41 --base egre-24-gate-go`},
	run:      cmdPREdit,
}

func cmdPREdit(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	title := fs.String("title", "", "the new `TITLE`")
	file := fs.String("body-file", "", "replace the description with `FILE` (- for stdin)")
	base := fs.String("base", "", "merge it into `BRANCH` instead")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if *title == "" && *file == "" && *base == "" {
		return c.usage("say what to change: --title, --body-file or --base")
	}
	d, _, err := c.loadPR(sf, pos, *repo)
	if err != nil {
		return err
	}
	req := map[string]any{}
	if *title != "" {
		req["title"] = *title
	}
	if *base != "" {
		req["base"] = *base
	}
	switch *file {
	case "":
	case "-":
		b, err := readAll(os.Stdin)
		if err != nil {
			return err
		}
		req["body"] = b
	default:
		b, err := os.ReadFile(*file) // #nosec G304 -- the file the user named
		if err != nil {
			return err
		}
		req["body"] = string(b)
	}
	raw, err := c.call("PATCH", fmt.Sprintf("/api/v1/pulls/%d", d.ID), req)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return fmt.Sprintf("Edited %s#%d.", d.Repo, d.Number) })
}
