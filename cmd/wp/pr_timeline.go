package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

var prTimelineCmd = &command{
	name: "timeline", args: "[N] [--repo REPO]",
	summary: "a pull request's ship-to-merge timeline: runner waits, CI runs, holds after green",
	about: `Every push, arming, CI run and hold of one pull request in time order, and
first three totals: waiting for a CI runner and running CI, each added over all
its runs (reruns and failed ones too, and runs at the same time each count),
and held after green (the union of its holds from when their head's CI passed,
however short). They are not parts of the time it was open: runs overlap, and
a hold can wait on a merge queue run. Read from what Waypoint kept; nothing is
re-run.`,
	examples: []string{"wp pr timeline 12", "wp pr timeline 12 --json"},
	run:      cmdPRTimeline,
}

// prTimeline is the response of GET /projects/{key}/pulls/{number}/timeline (WP-1806).
type prTimeline struct {
	State    string     `json:"state"`
	OpenedAt time.Time  `json:"opened_at"`
	MergedAt *time.Time `json:"merged_at"`
	ClosedAt *time.Time `json:"closed_at"`
	Pushes   []struct {
		At   time.Time `json:"at"`
		Head string    `json:"head"`
	} `json:"pushes"`
	Armed []struct {
		At   time.Time `json:"at"`
		What string    `json:"what"`
		By   string    `json:"by"`
	} `json:"armed"`
	Runs []struct {
		Number   int64     `json:"number"`
		Workflow string    `json:"workflow"`
		Event    string    `json:"event"`
		Tier     string    `json:"tier"`
		Stack    bool      `json:"stack"`
		Head     string    `json:"head"`
		Status   string    `json:"status"`
		QueuedAt time.Time `json:"queued_at"`
		WaitS    *int64    `json:"wait_s"`
		RunS     *int64    `json:"run_s"`
	} `json:"runs"`
	Holds []struct {
		What        string     `json:"what"`
		Head        string     `json:"head"`
		From        time.Time  `json:"from"`
		Until       *time.Time `json:"until"`
		AfterGreenS int64      `json:"after_green_s"`
	} `json:"holds"`
	Totals struct {
		OpenToMergedS *int64 `json:"open_to_merged_s"`
		CIWaitS       int64  `json:"ci_wait_s"`
		CIRunS        int64  `json:"ci_run_s"`
		HeldS         int64  `json:"held_s"`
	} `json:"totals"`
}

func cmdPRTimeline(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name), if the number is ambiguous")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	project, n, repo2, err := c.prArgs(pos, sf, *repo)
	if err != nil {
		return err
	}
	q := url.Values{}
	if repo2 != "" {
		q.Set("repo", repo2)
	}
	raw, err := c.call("GET", fmt.Sprintf("/api/v1/projects/%s/pulls/%d/timeline?%s", esc(project), n, q.Encode()), nil)
	if err != nil {
		return err
	}
	var t prTimeline
	if err := jsonInto(raw, &t); err != nil {
		return err
	}
	return c.emit(raw, func() string { return t.text(time.Now()) })
}

// timelineDur is seconds as 3m12s under an hour and 4h37m above.
func timelineDur(s int64) string {
	d := time.Duration(s) * time.Second
	if d >= time.Hour {
		return holdTook(d)
	}
	return shortDuration(d)
}

func (t *prTimeline) text(now time.Time) string {
	var b strings.Builder
	end := now
	head := "open for"
	if t.MergedAt != nil {
		end, head = *t.MergedAt, "open to merged"
	} else if t.ClosedAt != nil {
		end, head = *t.ClosedAt, "closed unmerged after"
	}
	total := int64(end.Sub(t.OpenedAt).Seconds())
	if t.Totals.OpenToMergedS != nil {
		total = *t.Totals.OpenToMergedS
	}
	fmt.Fprintf(&b, "%s %s; over its %d runs %s waiting for a runner and %s running CI; %s held after green\n",
		head, timelineDur(total), len(t.Runs), timelineDur(t.Totals.CIWaitS), timelineDur(t.Totals.CIRunS), timelineDur(t.Totals.HeldS))

	type line struct {
		at   time.Time
		text string
	}
	lines := []line{{t.OpenedAt, "opened"}}
	for _, p := range t.Pushes {
		lines = append(lines, line{p.At, "pushed " + p.Head})
	}
	for _, r := range t.Runs {
		s := fmt.Sprintf("run #%d %s", r.Number, r.Workflow)
		if r.Tier != "" {
			s += " (" + r.Tier + ")"
		}
		if r.Stack {
			s += " on the merge queue's stack"
		}
		if r.WaitS != nil {
			s += ": waited " + timelineDur(*r.WaitS)
		} else {
			s += ": never started"
		}
		if r.RunS != nil {
			s += ", ran " + timelineDur(*r.RunS)
		}
		lines = append(lines, line{r.QueuedAt, s + ", " + r.Status})
	}
	for _, a := range t.Armed {
		// merged_at has the merge's own line; this one is when it was asked for.
		s := map[string]string{"arm": "armed", "disarm": "disarmed", "merge": "merge started"}[a.What]
		if s == "" {
			s = a.What
		}
		if a.By != "" {
			s += " by " + a.By
		}
		lines = append(lines, line{a.At, s})
	}
	for _, h := range t.Holds {
		until, open := now, true
		if h.Until != nil {
			until, open = *h.Until, false
		}
		s := "held " + timelineDur(int64(until.Sub(h.From).Seconds()))
		if open {
			s += " so far"
		}
		if h.AfterGreenS > 0 {
			s += " (" + timelineDur(h.AfterGreenS) + " after green)"
		}
		lines = append(lines, line{h.From, s + ": " + h.What})
	}
	if t.MergedAt != nil {
		lines = append(lines, line{*t.MergedAt, "merged"})
	} else if t.ClosedAt != nil {
		lines = append(lines, line{*t.ClosedAt, "closed"})
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at.Before(lines[j].at) })
	for _, l := range lines {
		fmt.Fprintf(&b, "%s  %s\n", l.at.Local().Format("2006-01-02 15:04:05"), l.text)
	}
	return strings.TrimRight(b.String(), "\n")
}
