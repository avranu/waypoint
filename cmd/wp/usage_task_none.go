package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type noTaskReport struct {
	Who        string    `json:"who"`
	Days       int       `json:"days"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	TotalUSD   float64   `json:"total_usd"`
	TaskUSD    float64   `json:"task_usd"`
	NoTaskUSD  float64   `json:"no_task_usd"`
	CarryUSD   float64   `json:"carry_usd"`
	CarryTasks int       `json:"carry_tasks"`
	CarryOver  []struct {
		Key      string  `json:"key"`
		Title    string  `json:"title"`
		CostUSD  float64 `json:"cost_usd"`
		Sessions int     `json:"sessions"`
	} `json:"carry_over"`
	NeverUSD      float64 `json:"never_usd"`
	NeverSessions int     `json:"never_sessions"`
	Never         []struct {
		Machine   string    `json:"machine"`
		Session   string    `json:"session"`
		Repo      string    `json:"repo"`
		FirstHour time.Time `json:"first_hour"`
		CostUSD   float64   `json:"cost_usd"`
	} `json:"never"`
	OtherUSD float64 `json:"other_usd"`
}

func cmdUsageNoTask(c *cli, user string, days int) error {
	q := url.Values{}
	if user != "" {
		q.Set("user", user)
	}
	if days > 0 {
		q.Set("days", strconv.Itoa(days))
	}
	path := "/api/v1/usage/no-task"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.call("GET", path, nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r noTaskReport
		if err := json.Unmarshal(raw, &r); err != nil {
			return string(raw)
		}
		return noTaskText(&r)
	})
}

func noTaskText(r *noTaskReport) string {
	var b strings.Builder
	pct := func(v float64) string {
		if r.TotalUSD <= 0 {
			return ""
		}
		return fmt.Sprintf(" (%.0f%%)", 100*v/r.TotalUSD)
	}
	fmt.Fprintf(&b, "Spend, last %d days (%s), %s to %s\n", r.Days, r.Who, r.From.Format("2006-01-02"), r.To.Format("2006-01-02"))
	fmt.Fprintf(&b, "  total      $%.2f\n  on tasks   $%.2f%s\n  on no task $%.2f%s\n", r.TotalUSD, r.TaskUSD, pct(r.TaskUSD), r.NoTaskUSD, pct(r.NoTaskUSD))
	if r.CarryUSD > 0 {
		fmt.Fprintf(&b, "\nCarry-over $%.2f: after a run ended, same session, no other task claimed; %d task%s (top %d). Not in those tasks' cost.\n",
			r.CarryUSD, r.CarryTasks, plural(r.CarryTasks), len(r.CarryOver))
		for _, t := range r.CarryOver {
			fmt.Fprintf(&b, "  %-10s $%8.2f  %d session%s  %s\n", t.Key, t.CostUSD, t.Sessions, plural(t.Sessions), truncRunes(t.Title, 60))
		}
	}
	if r.NeverUSD > 0 {
		fmt.Fprintf(&b, "\nSessions that never had a run: $%.2f in %d (top %d by cost)\n", r.NeverUSD, r.NeverSessions, len(r.Never))
		for _, s := range r.Never {
			fmt.Fprintf(&b, "  $%8.2f  %s %s  %s  from %s\n", s.CostUSD, s.Machine, shortID(s.Session), cmpStr(s.Repo, "-"), s.FirstHour.Format("2006-01-02 15:04"))
		}
	}
	if r.OtherUSD > 0 {
		fmt.Fprintf(&b, "\nOther $%.2f: hours before a session's first run, or after a run on a task you cannot see.\n", r.OtherUSD)
	}
	return b.String()
}

// taskReviewText prints a task's review rounds, the tokens of their audits
// and its pull requests, each only when there is something to say.
func taskReviewText(b *strings.Builder, r *taskUsageReport) {
	for _, p := range r.PullRequests {
		line := fmt.Sprintf("Pull request %s#%d: %s", p.Repo, p.Number, p.State)
		if p.MergedAt != nil {
			line += " " + p.MergedAt.UTC().Format("2006-01-02 15:04")
		}
		fmt.Fprintln(b, line)
	}
	for n, rd := range r.Rounds {
		line := fmt.Sprintf("Review round %d %s %s", n+1, rd.At.UTC().Format("2006-01-02 15:04"), rd.State)
		if rd.Found != nil {
			line += fmt.Sprintf(": found %d", *rd.Found)
			if rd.Fixed != nil {
				line += fmt.Sprintf(", fixed %d", *rd.Fixed)
			}
			if rd.Rejected != nil {
				line += fmt.Sprintf(", rejected %d", *rd.Rejected)
			}
		}
		fmt.Fprintln(b, line)
	}
	if rv := r.Reviewer; rv != nil {
		fmt.Fprintf(b, "Reviewers: %d audit%s, %s tokens in, %s out.\n", rv.Audits, plural(rv.Audits), commas(rv.Input), commas(rv.Output))
	}
}
