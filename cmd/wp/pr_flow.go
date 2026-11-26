package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

var prFlowCmd = &command{
	name: "flow", args: "[--days N] [--repo REPO]",
	summary: "where merged pull requests' open time went, by stage, for a repo and the last days",
	about: `Opened to merged for the pull requests merged in the span (p50, p75, p90),
the median per day, and the share of their open time per stage: each minute
counts against one stage, the first that applies of a round's job (waiting for a
runner, running), the gates holding the merge (those at once share the
minute), work left with no round sent, parked for a person, a draft in the merge
line, and nothing recorded. Time also divides into converging (the pull
request holds no place in the merge line) and integrating (it holds one,
whatever it waits for there), so a stage can have a row in each. Pull
requests still open are counted apart, by their open time so far. It ends with
the three numbers WP-ADR163 watches: heads, full CI runs and rounds per merged
pull request. Default 7 days, at most 31; the repo here, or every repo of the
project outside a git repo.`,
	examples: []string{"wp pr flow", "wp pr flow --days 14 --json"},
	run:      cmdPRFlow,
}

type prFlowStage struct {
	Stage string  `json:"stage"`
	Group string  `json:"group"`
	Hours float64 `json:"hours"`
	Share float64 `json:"share"`
	PerPR *struct {
		P50 int64 `json:"p50_s"`
		P75 int64 `json:"p75_s"`
		P90 int64 `json:"p90_s"`
	} `json:"per_pr"`
	Why []struct {
		Text  string  `json:"text"`
		Hours float64 `json:"hours"`
	} `json:"why"`
}

type prFlowGroup struct {
	Group string  `json:"group"`
	Hours float64 `json:"hours"`
	Share float64 `json:"share"`
}

// prFlow is the response of GET /projects/{key}/pulls/flow.
type prFlow struct {
	Repo           string    `json:"repo"`
	Days           int       `json:"days"`
	AsOf           time.Time `json:"as_of"`
	Merged         int       `json:"merged"`
	Hours          float64   `json:"hours"`
	OpenedToMerged *struct {
		P50 int64 `json:"p50_s"`
		P75 int64 `json:"p75_s"`
		P90 int64 `json:"p90_s"`
	} `json:"opened_to_merged"`
	Groups []prFlowGroup `json:"groups"`
	Stages []prFlowStage `json:"stages"`
	Daily  []struct {
		Day    string        `json:"day"`
		Merged int           `json:"merged"`
		P50S   int64         `json:"opened_to_merged_p50_s"`
		Hours  float64       `json:"hours"`
		Groups []prFlowGroup `json:"groups"`
		Top    []struct {
			Stage string  `json:"stage"`
			Share float64 `json:"share"`
		} `json:"top"`
	} `json:"daily"`
	Watch struct {
		Heads    float64 `json:"heads_per_merge"`
		FullRuns float64 `json:"full_runs_per_merge"`
		Rounds   float64 `json:"rounds_per_merge"`
	} `json:"watch"`
	StillOpen struct {
		Count  int           `json:"count"`
		Hours  float64       `json:"hours"`
		Groups []prFlowGroup `json:"groups"`
		Stages []prFlowStage `json:"stages"`
		// Truncated: more are open than are read.
		Truncated bool `json:"truncated"`
	} `json:"still_open"`
	Truncated bool `json:"truncated"`
}

func cmdPRFlow(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "the `REPO` (owner/name); default the repo here")
	days := fs.Int("days", 7, "the last `N` days (at most 31)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("wp pr flow takes no pull request number: give --days and --repo")
	}
	sc, err := c.scopeFor(sf)
	if err != nil {
		return err
	}
	project, err := sc.home()
	if err != nil {
		return err
	}
	if *repo == "" {
		if r, in := gitRepoHere(); in {
			*repo = r
		}
	}
	q := url.Values{"days": {fmt.Sprint(*days)}}
	if *repo != "" {
		q.Set("repo", *repo)
	}
	raw, err := c.call("GET", fmt.Sprintf("/api/v1/projects/%s/pulls/flow?%s", esc(project), q.Encode()), nil)
	if err != nil {
		return err
	}
	var f prFlow
	if err := jsonInto(raw, &f); err != nil {
		return err
	}
	return c.emit(raw, f.text)
}

func flowMin(s int64) string { return timelineDur(s) }

func (f *prFlow) text() string {
	var b strings.Builder
	scope := f.Repo
	if scope == "" {
		scope = "every repo of the project"
	}
	fmt.Fprintf(&b, "Pull request flow, %s, merged in the last %d days (to %s)\n\n", scope, f.Days, f.AsOf.UTC().Format("2006-01-02 15:04 UTC"))
	if f.Merged == 0 {
		b.WriteString("No pull requests merged in the span.\n")
	} else {
		o := f.OpenedToMerged
		fmt.Fprintf(&b, "%d merged pull requests, open %.0f h in all. Opened to merged: p50 %s, p75 %s, p90 %s\n", f.Merged, f.Hours, flowMin(o.P50), flowMin(o.P75), flowMin(o.P90))
		b.WriteString(groupLine(f.Groups) + "\n")
		b.WriteString("\nPer day (UTC): merged, opened to merged p50, then where that day's open hours went\n")
		for _, d := range f.Daily {
			var top []string
			for _, t := range d.Top {
				top = append(top, fmt.Sprintf("%s %.0f%%", trimStage(t.Stage), 100*t.Share))
			}
			med := "-"
			if d.Merged > 0 {
				med = flowMin(d.P50S)
			}
			fmt.Fprintf(&b, "  %s  %2d merged  p50 %-7s %4.0f h: %s\n", d.Day, d.Merged, med, d.Hours, strings.Join(top, ", "))
		}
		b.WriteString("\nStages: hours, share of open time, per pull request p50 / p90 (in minutes)\n")
		for _, s := range f.Stages {
			p50, p90 := int64(0), int64(0)
			if s.PerPR != nil {
				p50, p90 = s.PerPR.P50, s.PerPR.P90
			}
			fmt.Fprintf(&b, "  %5.0f h %3.0f%%  %4d / %4d  %-11s %s\n", s.Hours, 100*s.Share, p50/60, p90/60, s.Group, s.Stage)
			for _, w := range s.Why {
				fmt.Fprintf(&b, "                                        %5.1f h waiting: %s\n", w.Hours, w.Text)
			}
		}
		fmt.Fprintf(&b, "\nPer merged pull request: %.1f heads, %.1f full CI runs, %.1f rounds\n", f.Watch.Heads, f.Watch.FullRuns, f.Watch.Rounds)
	}
	if o := f.StillOpen; o.Count > 0 {
		fmt.Fprintf(&b, "\nStill open: %d pull requests, %.0f h open so far. %s\n", o.Count, o.Hours, groupLine(o.Groups))
		for _, s := range o.Stages {
			fmt.Fprintf(&b, "  %5.0f h %3.0f%%  %-11s %s\n", s.Hours, 100*s.Share, s.Group, s.Stage)
		}
	}
	if f.Truncated {
		b.WriteString("\nMore pull requests merged than are read here (400): the oldest merges were left out. Use fewer --days.\n")
	}
	if f.StillOpen.Truncated {
		b.WriteString("\nMore pull requests are open than are read here (150): the newest were left out.\n")
	}
	return b.String()
}

func groupLine(gs []prFlowGroup) string {
	var p []string
	for _, g := range gs {
		p = append(p, fmt.Sprintf("%s %.0f h (%.0f%%)", g.Group, g.Hours, 100*g.Share))
	}
	return strings.Join(p, ", ")
}

func trimStage(s string) string {
	if i := strings.Index(s, " ("); i > 0 {
		return s[:i]
	}
	return s
}
