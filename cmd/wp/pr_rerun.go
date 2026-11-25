package main

import (
	"fmt"
	"net/url"
	"strings"
)

// prRerunCmd is wp pr rerun (WP-1661).
var prRerunCmd = &command{
	name: "rerun", aliases: []string{"retry-ci"}, args: "[N...] [--job NAME]",
	summary: "re-run the failed CI jobs of a pull request's head, without a push",
	about: `Re-runs the failed and cancelled jobs of the newest run on the pull
request's head (a flaky test, a runner that died), keeping the failed logs
and test results as evidence first. --job NAME re-runs only that failed job
(the name the run shows, without the workflow prefix).

At most twice per head: a failure that comes back twice is the code's, so
push a fix. Whoever may merge the pull request may re-run it; the forge must
be able to re-run jobs (Forgejo 17 or later). Each re-run is recorded in the
pull request's activity.`,
	examples: []string{"wp pr rerun 12", "wp pr rerun 12 --job backend", "wp pr wait 12"},
	run:      cmdPRRerun,
}

type prRerunResult struct {
	Jobs   []string `json:"jobs"`
	Runs   int      `json:"runs"`
	RunURL string   `json:"run_url"`
	Head   string   `json:"head"`
	Count  int      `json:"count"`
	Max    int      `json:"max"`
}

func cmdPRRerun(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	job := fs.String("job", "", "re-run only the failed `NAME`d job")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	return c.eachPR(sf, pos, *repo, false, func(d *prDetail, _ []byte) error {
		path := fmt.Sprintf("/api/v1/pulls/%d/rerun", d.ID)
		if *job != "" {
			path += "?job=" + url.QueryEscape(*job)
		}
		raw, err := c.call("POST", path, nil)
		if err != nil {
			return err
		}
		var r prRerunResult
		if err := jsonInto(raw, &r); err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var b strings.Builder
			what := fmt.Sprintf("%d run%s", r.Runs, plural(r.Runs))
			if len(r.Jobs) > 0 {
				what = fmt.Sprintf("%d job%s (%s)", len(r.Jobs), plural(len(r.Jobs)), strings.Join(r.Jobs, ", "))
			}
			fmt.Fprintf(&b, "%s#%d: re-ran %s on %s (%d of %d for this head).", d.Repo, d.Number, what, shortSHA(r.Head), r.Count, r.Max)
			if r.RunURL != "" {
				fmt.Fprintf(&b, "\nRun: %s", r.RunURL)
			}
			fmt.Fprintf(&b, "\nWait for it: wp pr wait %d", d.Number)
			return b.String()
		})
	})
}
