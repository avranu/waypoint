package main

import (
	"math"
	"net/url"
	"strconv"
)

var prReviewStatsCmd = &command{
	name: "review-stats", args: "[-p PROJECT] [--since 30d]", summary: "how the review bot's findings held up, per reviewer and verifier model",
	about: `Counts the review bot's findings of a window (default the last 30 days, at
most 730) and how they held up, from what each review run records (WP-363):

  - per reviewer model × context strategy × prompt version: findings,
    useful (fixed, or taken by an agent), false (dismissed or withdrawn),
    unjudged, dropped by the evidence check or by the verifier and not
    posted, precision (useful / judged) with a 90% interval, and the tokens
    the reviewer spent, per useful finding too;
  - per verifier model: how the findings it kept fared, and of those it
    dropped, how the ones posted anyway fared: a drop was right when the
    finding turned out false;
  - the findings the verifier kept against those of runs without a
    verifier, with each side's tokens per useful finding.

Models count by the weights, whichever provider served them ("groq/openai/
gpt-oss-120b" and "ollama-eshu/gpt-oss:120b" are gpt-oss-120b), with the
providers listed. A finding moved to an audit when its pull request merged
counts by the audit's verdict once it has one (confirmed or fixed: useful;
rejected: false). Every project by default (admins); -p one project.
--json prints it all. Admin → AI shows the same.`,
	examples: []string{"wp pr review-stats", "wp pr review-stats -p WP --since 90d", "wp pr review-stats --json"},
	run:      cmdPRReviewStats,
}

func cmdPRReviewStats(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "only this `PROJECT`'s reviews (key, name or repo); default every project")
	short(fs, "p", "project")
	since := fs.String("since", "", "findings raised within `DURATION`: 30d, 12w (default 30d, at most 730d)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	d, err := parseSince(*since)
	if err != nil {
		return c.usage("%v", err)
	}
	q := url.Values{}
	if d > 0 {
		q.Set("days", strconv.Itoa(max(1, int(math.Ceil(d.Hours()/24)))))
	}
	if *project != "" {
		sc, err := c.findScope(*project, "-p "+*project)
		if err != nil {
			return err
		}
		home, err := sc.home()
		if err != nil {
			return err
		}
		q.Set("project", home)
	}
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", "/api/v1/reviews/stats?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}
