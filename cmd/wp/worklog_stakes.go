package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// cmdWorklogStakes is wp worklog stakes (WP-1231, WP-1248): review rounds by
// stakes tier, what their free audits and Claude reviews did, cost and
// found, and what the tasks in each tier took and led to.
func cmdWorklogStakes(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "only `PROJECT`")
	short(fs, "p", "project")
	days := fs.Int("days", 14, "over the last `N` days (up to 365)")
	rows := fs.Bool("rows", false, "with --json, every round")
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
	raw, err := c.call("GET", "/api/v1/worklog/stakes?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var st struct {
			Days  int    `json:"days"`
			Rule  string `json:"rule"`
			Tiers []struct {
				Tier            string  `json:"tier"`
				Rounds          int     `json:"rounds"`
				Recomputed      int     `json:"recomputed"`
				Barred          int     `json:"barred"`
				BarFailed       int     `json:"bar_failed"`
				FreeReasks      int     `json:"free_reasks"`
				SlowHandOvers   int     `json:"slow_handovers"`
				Reviewers       int     `json:"claude_reviews"`
				ReviewersUsed   int     `json:"claude_reviews_used"`
				ReviewerMinutes float64 `json:"claude_minutes"`
				ReviewerInput   int64   `json:"claude_input_tokens"`
				ReviewerOutput  int64   `json:"claude_output_tokens"`
				OwnReviews      int     `json:"own_reviews"`
				Answered        int     `json:"answered"`
				Empty           int     `json:"empty"`
				Found           int     `json:"found"`
				Fixed           int     `json:"fixed"`
				Rejected        int     `json:"rejected"`
				AuditMinutes    float64 `json:"mean_audit_minutes"`
				Useful          int     `json:"useful_on_prs"`
				Escapes         int     `json:"escapes"`
				Tasks           int     `json:"tasks"`
				Outcomes        []struct {
					Label  string  `json:"label"`
					N      int     `json:"n"`
					Median float64 `json:"median"`
				} `json:"outcomes"`
			} `json:"tiers"`
			OutcomesNote string `json:"outcomes_note"`
			Models       []struct {
				Tier     string `json:"tier"`
				Model    string `json:"model"`
				Answered int    `json:"answered"`
				Empty    int    `json:"empty"`
				Findings int    `json:"findings"`
				Fixed    int    `json:"fixed"`
				Rejected int    `json:"rejected"`
			} `json:"models"`
		}
		if err := jsonInto(raw, &st); err != nil {
			return string(raw)
		}
		if len(st.Tiers) == 0 {
			return fmt.Sprintf("No review rounds in the last %d days.\n", st.Days)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Review rounds by stakes, last %d days (%s)\n", st.Days, st.Rule)
		fmt.Fprintf(&b, "%-9s %6s %6s %7s %6s %5s %7s %6s %7s %5s %6s %6s %6s %6s %6s %7s\n",
			"TIER", "ROUNDS", "BARRED", "BARFAIL", "REASK", "SLOW", "CLAUDE", "USED", "CL-MIN", "OWN", "ANSWER", "EMPTY", "FOUND", "FIXED", "USEFUL", "ESCAPES")
		for _, t := range st.Tiers {
			fmt.Fprintf(&b, "%-9s %6d %6d %7d %6d %5d %7d %6d %7.0f %5d %6d %6d %6d %6d %6d %7d\n",
				t.Tier, t.Rounds, t.Barred, t.BarFailed, t.FreeReasks, t.SlowHandOvers, t.Reviewers, t.ReviewersUsed, t.ReviewerMinutes, t.OwnReviews,
				t.Answered, t.Empty, t.Found, t.Fixed, t.Useful, t.Escapes)
		}
		b.WriteString("\nBARRED: first audits held above the floor to their tier's bar (high: Grok 4.7 or better, Claude Sonnet before WP-1752; elevated: Claude Haiku or better); BARFAIL: of those, ended with no answer. REASK: asked again at the floor (deepseek-v4.1-flash) after that.\n")
		b.WriteString("SLOW: slow audits handed to the pull request with no Claude review. CLAUDE: Claude reviews started (Sonnet for high stakes, Haiku below since WP-1306; USED: theirs were the round's findings; CL-MIN: their minutes).\n")
		b.WriteString("OWN: rounds the working session reviewed itself. ANSWER/EMPTY: free audits that answered, and of those, with no findings.\n")
		b.WriteString("FOUND/FIXED: the rounds' findings. USEFUL: useful findings on the branch's pull request after the round. ESCAPES: bugs traced back to it.\n")
		var recomputed int
		for _, t := range st.Tiers {
			recomputed += t.Recomputed
		}
		if recomputed > 0 {
			fmt.Fprintf(&b, "%d rounds from before tiers were stored are put in tiers by %s.\n", recomputed, st.Rule)
		}
		// The tasks' measures: one row a measure, one column a tier, each
		// cell the median over the tasks it applies to (how many).
		var labels []string
		for _, t := range st.Tiers {
			if len(t.Outcomes) > len(labels) {
				labels = labels[:0]
				for _, o := range t.Outcomes {
					labels = append(labels, o.Label)
				}
			}
		}
		if len(labels) > 0 {
			b.WriteString("\nTasks by their highest tier: median (tasks measured)\n")
			fmt.Fprintf(&b, "%-26s", "")
			for _, t := range st.Tiers {
				fmt.Fprintf(&b, " %16s", fmt.Sprintf("%s (%d)", t.Tier, t.Tasks))
			}
			b.WriteString("\n")
			for i, l := range labels {
				fmt.Fprintf(&b, "%-26s", l)
				for _, t := range st.Tiers {
					if i >= len(t.Outcomes) || t.Outcomes[i].N == 0 {
						fmt.Fprintf(&b, " %16s", "-")
						continue
					}
					fmt.Fprintf(&b, " %16s", fmt.Sprintf("%s (%d)", trimFloat(t.Outcomes[i].Median), t.Outcomes[i].N))
				}
				b.WriteString("\n")
			}
		} else if st.OutcomesNote != "" {
			fmt.Fprintf(&b, "\nNo task outcomes: %s.\n", st.OutcomesNote)
		}
		if len(st.Models) > 0 {
			fmt.Fprintf(&b, "\n%-9s %-44s %6s %6s %8s %6s %8s\n", "TIER", "MODEL", "ANSWER", "EMPTY", "FINDINGS", "FIXED", "REJECTED")
			for _, m := range st.Models {
				fmt.Fprintf(&b, "%-9s %-44s %6d %6d %8d %6d %8d\n", m.Tier, m.Model, m.Answered, m.Empty, m.Findings, m.Fixed, m.Rejected)
			}
		}
		return b.String()
	})
}

// trimFloat prints a measure without needless decimals.
func trimFloat(v float64) string {
	switch {
	case v == float64(int64(v)):
		return strconv.FormatInt(int64(v), 10)
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}
