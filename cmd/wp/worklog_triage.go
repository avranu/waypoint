package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// maxChangeDiff is the most numstat and diff wp ship sends for review triage
// (WP-692), well inside the API's body limit even once JSON has escaped it.
// Past it they are left out and the review stays.
const maxChangeDiff = 256 * 1024

// changeFacts is what the checkout says about the branch, for review triage:
// its files, its diff and how many commits. nil when git can't say; the
// server then keeps the review.
func changeFacts(base string) map[string]any {
	ref := strings.TrimSpace(base)
	if ref != "" {
		ref = "origin/" + ref
	} else {
		ref = defaultBranchRef(".")
	}
	if ref == "" {
		return nil
	}
	// Unescaped, so an accented path matches the code index's.
	numstat, err := localGit("-c", "core.quotepath=off", "diff", "--no-renames", "--numstat", ref+"...HEAD")
	if err != nil || numstat == "" || len(numstat) > maxChangeDiff {
		return nil
	}
	out := map[string]any{"numstat": numstat}
	if n, err := localGit("rev-list", "--count", ref+"..HEAD"); err == nil {
		if c, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			out["commits"] = c
		}
	}
	diff, err := localGit("-c", "core.quotepath=off", "diff", "--no-renames", "--unified=0", ref+"...HEAD")
	if err != nil || len(numstat)+len(diff) > maxChangeDiff {
		out["clipped"] = true
		return out
	}
	out["diff"] = diff
	return out
}

type triageTotals struct {
	Assessed     int `json:"assessed"`
	WouldSkip    int `json:"would_skip"`
	Skipped      int `json:"skipped"`
	AgentSkip    int `json:"agent_skip"`
	Reviewed     int `json:"reviewed"`
	Holdout      int `json:"holdout"`
	ShadowRuns   int `json:"shadow_runs"`
	Sampled      int `json:"sampled"`
	Missed       int `json:"sampled_found_any"`
	Counted      int `json:"counted"`
	FoundAny     int `json:"found_any"`
	Found        int `json:"found"`
	Fixed        int `json:"fixed"`
	Useful       int `json:"useful_findings"`
	UsefulOnSkip int `json:"useful_findings_on_skipped"`
	Escapes      int `json:"escapes"`
	EscapesSkip  int `json:"escapes_on_skipped"`
}

// cmdWorklogTriage is wp worklog triage: what review triage decided and how
// it turned out.
func cmdWorklogTriage(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "only `PROJECT`")
	short(fs, "p", "project")
	days := fs.Int("days", 30, "over the last `N` days (up to 365)")
	rows := fs.Bool("rows", false, "with --json, every assessment and its labels")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	if *days < 1 || *days > 365 {
		return c.usage("--days must be 1 to 365")
	}
	q := url.Values{"days": {strconv.Itoa(*days)}}
	if *project != "" {
		q.Set("project", *project)
	}
	if *rows {
		q.Set("rows", "1")
	}
	raw, err := c.call("GET", "/api/v1/worklog/triage?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var st struct {
			Days    int    `json:"days"`
			Version string `json:"version"`
			Rules   []struct {
				Rule   string `json:"rule"`
				Active bool   `json:"active"`
				Shadow bool   `json:"shadow"`
				triageTotals
			} `json:"rules"`
			Total   triageTotals `json:"total"`
			Avoided struct {
				Rounds           int     `json:"rounds"`
				MeanRoundMinutes float64 `json:"mean_round_minutes"`
				Minutes          float64 `json:"minutes"`
				GrokAudits       float64 `json:"grok_audits"`
			} `json:"avoided"`
		}
		if err := jsonInto(raw, &st); err != nil {
			return string(raw)
		}
		if st.Total.Assessed == 0 {
			return fmt.Sprintf("No changes assessed in the last %d days.\n", st.Days)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Review triage, last %d days (%s)\n", st.Days, st.Version)
		fmt.Fprintf(&b, "%-22s %6s %6s %6s %8s %6s %8s %6s %7s %6s %6s %7s %7s\n", "RULE", "SEEN", "WOULD", "SKIP", "HOLDOUT", "SHADOW", "SAMPLED", "MISSED", "COUNTED", "FOUND", "FIXED", "USEFUL", "ESCAPES")
		row := func(name string, t triageTotals) {
			fmt.Fprintf(&b, "%-22s %6d %6d %6d %8d %6d %8d %6d %7d %6d %6d %7d %7d\n", name, t.Assessed, t.WouldSkip, t.Skipped, t.Holdout, t.ShadowRuns, t.Sampled, t.Missed, t.Counted, t.Found, t.Fixed, t.Useful, t.Escapes)
		}
		for _, r := range st.Rules {
			name := r.Rule
			switch {
			case r.Active:
				name += " (on)"
			case r.Shadow:
				name += " (shadow)"
			}
			row(name, r.triageTotals)
		}
		row("all", st.Total)
		fmt.Fprintf(&b, "\nSAMPLED: finished reviews of changes a rule would have skipped, picked without looking at the change. MISSED: those that found something, which skipping would have missed.\n")
		fmt.Fprintf(&b, "COUNTED, FOUND, FIXED: every finished review, including ones an agent asked for.\n")
		fmt.Fprintf(&b, "On skipped changes: %d useful findings on their pull requests, %d escapes traced back.\n", st.Total.UsefulOnSkip, st.Total.EscapesSkip)
		fmt.Fprintf(&b, "Avoided: %d reviews, about %.0f minutes and %.1f Grok audits (mean review %.0f min).\n", st.Avoided.Rounds, st.Avoided.Minutes, st.Avoided.GrokAudits, st.Avoided.MeanRoundMinutes)
		return b.String()
	})
}
