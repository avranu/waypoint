package main

import "fmt"

var prUrgentCmd = &command{
	name: "urgent", args: "[N...] [--off]",
	summary: "put a pull request's CI on the urgent lane, or take it off",
	about: `Labels the pull request ci-urgent so its jobs start ahead of the CI queue,
it goes ahead in its merge queue, and its babysit rounds don't wait for the
line or its checks (--off removes it; runs already started are unchanged,
and it keeps the place it has).

Almost never use this: every urgent job slows CI for every repo on that
machine. It is for the rare pull request that must skip the line, like a fix
to CI while CI is stuck. An issue's priority is not a reason.`,
	examples: []string{"wp pr urgent 12", "wp pr urgent", "wp pr urgent 12 --off"},
	run:      cmdPRUrgent,
}

func cmdPRUrgent(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	off := fs.Bool("off", false, "take the ci-urgent label off")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	method := "POST"
	if *off {
		method = "DELETE"
	}
	return c.eachPR(sf, pos, *repo, false, func(d *prDetail, _ []byte) error {
		was := d.Urgent
		raw, err := c.call(method, fmt.Sprintf("/api/v1/pulls/%d/urgent", d.ID), nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			switch {
			case *off && !was:
				return fmt.Sprintf("%s#%d has no ci-urgent label: its runs wait their turn.", d.Repo, d.Number)
			case *off:
				return fmt.Sprintf("%s#%d: ci-urgent label off; its next run waits its turn.", d.Repo, d.Number)
			case was:
				return fmt.Sprintf("%s#%d is already ci-urgent.", d.Repo, d.Number)
			}
			return fmt.Sprintf("%s#%d is ci-urgent: its run starts again on the urgent lane.\n%s", d.Repo, d.Number, urgentNote(d.Number))
		})
	})
}

// urgentNote tells whoever just marked pull request n urgent what that
// costs and how to undo it (WP-543).
func urgentNote(n int) string {
	return fmt.Sprintf("This pull request is marked urgent, so it delays other CI runs to cut the line; if you didn't intend that, run `wp pr urgent %d --off`.", n)
}
