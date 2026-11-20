package main

import (
	"fmt"
	"os"
)

var prEditCmd = &command{
	name: "edit", args: "[N] [--title T] [--body-file FILE]",
	summary: "change a pull request's title or description",
	about: `Whoever may merge the pull request may edit it (an agent: its own). A
draft keeps its "WIP: " prefix.`,
	examples: []string{`wp pr edit 41 --title "Fix the redirect loop"`},
	run:      cmdPREdit,
}

func cmdPREdit(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	title := fs.String("title", "", "the new `TITLE`")
	file := fs.String("body-file", "", "replace the description with `FILE` (- for stdin)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if *title == "" && *file == "" {
		return c.usage("say what to change: --title or --body-file")
	}
	d, _, err := c.loadPR(sf, pos, *repo)
	if err != nil {
		return err
	}
	req := map[string]any{}
	if *title != "" {
		req["title"] = *title
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
