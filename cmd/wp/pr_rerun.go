package main

import (
	"flag"
	"fmt"
	"net/url"
	"strings"
)

// prRerunCmd is wp pr rerun (WP-1661).
var prRerunCmd = &command{
	name: "rerun", aliases: []string{"retry-ci"}, args: "[N...] [--job NAME]",
	summary: "re-run the failed CI jobs of a pull request's head, without a push",
	about: `Re-runs the failed and cancelled jobs of the run that counts for each
workflow on the pull request's head (a flaky test, a runner that died). On
Forgejo the failed jobs' test results are first kept where they can be read
(the forge replaces the logs), and the failing tests are noted in the pull
request's history. --job NAME re-runs only that failed job (the name the run
shows, without the workflow prefix); Forgejo only. GitHub and GitLab re-run
their failed runs together.

At most twice per head, one count for all the workflows: a failure that
comes back twice is the code's, so push a fix. Whoever may merge the pull
request may re-run it; the forge must be able to re-run jobs (Forgejo 17 or
later). A run still going is not re-run: wait for it to end. Each re-run is
recorded in the pull request's activity and history.`,
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
	// Partial is why the forge refused the rest of the jobs.
	Partial string `json:"partial"`
	// Unrecorded: the jobs went again but Waypoint couldn't record it.
	Unrecorded bool `json:"unrecorded"`
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
	// A blank name (an unset shell variable) must not re-run every job.
	blankJob := false
	fs.Visit(func(f *flag.Flag) { blankJob = blankJob || f.Name == "job" && strings.TrimSpace(*job) == "" })
	if blankJob {
		return c.usage("--job needs a job's name; leave it out to re-run every failed job")
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
			if r.Partial != "" {
				fmt.Fprintf(&b, "\nthe forge refused the rest: %s; `wp pr log %d` shows what is still failed", r.Partial, d.Number)
			}
			if r.Unrecorded {
				b.WriteString("\nthe jobs went again, but Waypoint couldn't record it in the pull request's activity and history")
			}
			fmt.Fprintf(&b, "\nWait for it: wp pr wait %d", d.Number)
			return b.String()
		})
	})
}
