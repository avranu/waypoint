package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var usageFeatureCmd = &command{
	name: "feature", args: "KEY|VERSION [--window 30] [--repo OWNER/NAME] [--user NAME]",
	summary: "what tasks used with a release against without it",
	about: `Compares the tasks around a release by what each one's runs recorded, not
by the release's date. KEY is a task with a merged pull request: its release
is the first deployment started after its first merge (name a version for a
later one). A task counts as had when the server, the wp, the session's start
and the end all had the release and hooks ran; before when none did; partial
when they disagree; unknown when part of it wasn't recorded (runs or sessions
from before recording began, dev builds). Runs are those of the person and
repo asked about. Only had and before are compared. Medians, with a Mann-Whitney p-value; fewer than five
on a side is too few to say.`,
	examples: []string{"wp usage feature WP-192", "wp usage feature v0.80.0 --window 14"},
	run:      cmdUsageFeature,
}

var usageTaskCmd = &command{
	name: "task", args: "KEY [--user NAME]",
	summary: "what one task used, and what its agents had",
	about: `One task's tokens, cost, tool calls, edits, time and merged pull requests,
each run's versions (server, wp, session start, kit, Claude Code, hooks), its
experiment arms (admins), and the tools, skills and commands it used.`,
	examples: []string{"wp usage task WP-192"},
	run:      cmdUsageTask,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageFeatureCmd, usageTaskCmd)
}

type featureSide struct {
	N      int     `json:"n"`
	Median float64 `json:"median"`
}

type featureStat struct {
	Key    string      `json:"key"`
	Label  string      `json:"label"`
	Unit   string      `json:"unit"`
	Before featureSide `json:"before"`
	After  featureSide `json:"after"`
	Change *float64    `json:"change"`
	P      *float64    `json:"p"`
}

type featureReport struct {
	Ref     string         `json:"ref"`
	Title   string         `json:"title"`
	Release string         `json:"release"`
	LiveAt  time.Time      `json:"live_at"`
	Window  int            `json:"window"`
	After   int            `json:"days_after"`
	Who     string         `json:"who"`
	Counts  map[string]int `json:"counts"`
	Stats   []featureStat  `json:"stats"`
	MinN    int            `json:"min_n"`
	Tasks   []struct {
		Key     string  `json:"key"`
		State   string  `json:"state"`
		CostUSD float64 `json:"cost_usd"`
		Tokens  int64   `json:"tokens"`
	} `json:"tasks"`
	TaskRows int `json:"task_rows"`
}

func cmdUsageFeature(c *cli, args []string) error {
	fs := c.flags()
	window := fs.Int("window", 30, "`N` days either side of the release")
	repo := fs.String("repo", "", "only this `OWNER/NAME`")
	user := fs.String("user", "", "admins: another person's (`NAME`, or all)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("say which task or version")
	}
	q := url.Values{"ref": {pos[0]}, "window": {strconv.Itoa(*window)}}
	if *repo != "" {
		q.Set("repo", *repo)
	}
	if *user != "" {
		q.Set("user", *user)
	}
	raw, err := c.call("GET", "/api/v1/usage/feature?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r featureReport
		if err := json.Unmarshal(raw, &r); err != nil {
			return string(raw)
		}
		return featureText(&r)
	})
}

func featureText(r *featureReport) string {
	var b strings.Builder
	name := r.Ref
	if r.Title != "" {
		name += " " + r.Title
	}
	span := fmt.Sprintf("%d days either side", r.Window)
	if r.After < r.Window {
		span = fmt.Sprintf("%d days before and only %d after so far", r.Window, r.After)
	}
	fmt.Fprintf(&b, "%s: live in %s on %s; %s, %s.\n", name, r.Release, r.LiveAt.UTC().Format("2006-01-02 15:04 UTC"), span, r.Who)
	fmt.Fprintf(&b, "Tasks: %d had it, %d before it, %d partial, %d unknown.\n", r.Counts["had"], r.Counts["before"], r.Counts["partial"], r.Counts["unknown"])
	for _, s := range r.Stats {
		fmt.Fprintf(&b, "  %s: %s before (n=%d), %s with it (n=%d)", s.Label, statValue(s.Before.Median, s.Unit), s.Before.N, statValue(s.After.Median, s.Unit), s.After.N)
		if s.Before.N < r.MinN || s.After.N < r.MinN {
			b.WriteString(", too few to say\n")
			continue
		}
		if s.Change != nil {
			fmt.Fprintf(&b, ", %+.0f%%", *s.Change*100)
		}
		if s.P != nil {
			fmt.Fprintf(&b, ", p=%.3f", *s.P)
		}
		b.WriteString("\n")
	}
	for _, t := range r.Tasks {
		fmt.Fprintf(&b, "%-10s %-8s $%.2f  %s tokens\n", t.Key, t.State, t.CostUSD, commas(t.Tokens))
	}
	if r.TaskRows > len(r.Tasks) {
		fmt.Fprintf(&b, "… and %d more (--json has the same list).\n", r.TaskRows-len(r.Tasks))
	}
	return b.String()
}

func statValue(v float64, unit string) string {
	switch unit {
	case "usd":
		return fmt.Sprintf("$%.2f", v)
	case "ratio":
		return fmt.Sprintf("%.0f%%", v*100)
	case "hours":
		return fmt.Sprintf("%.1fh", v)
	}
	return commas(int64(v))
}

type taskUsageReport struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Who   string `json:"who"`
	Runs  []struct {
		Agent    string            `json:"agent"`
		Model    string            `json:"model"`
		Outcome  string            `json:"outcome"`
		Started  time.Time         `json:"started_at"`
		Features map[string]string `json:"features"`
	} `json:"runs"`
	Arms        map[string]string `json:"arms"`
	RunMinutes  int64             `json:"run_minutes"`
	ActiveHours int               `json:"active_hours"`
	Sessions    int               `json:"sessions"`
	Subagents   int64             `json:"subagents"`
	ToolCalls   int64             `json:"tool_calls"`
	ToolErrors  int64             `json:"tool_errors"`
	Edits       int64             `json:"edits"`
	PRs         int64             `json:"prs"`
	Files       int64             `json:"files"`
	Lines       int64             `json:"lines"`
	Input       int64             `json:"input"`
	Output      int64             `json:"output"`
	CacheWrite  int64             `json:"cache_write"`
	CacheRead   int64             `json:"cache_read"`
	CostUSD     float64           `json:"cost_usd"`
	Used        []struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Calls int64  `json:"calls"`
	} `json:"used"`
}

func cmdUsageTask(c *cli, args []string) error {
	fs := c.flags()
	user := fs.String("user", "", "admins: another person's (`NAME`, or all)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("say which task")
	}
	path := "/api/v1/usage/task/" + url.PathEscape(pos[0])
	if *user != "" {
		path += "?" + url.Values{"user": {*user}}.Encode()
	}
	raw, err := c.call("GET", path, nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r taskUsageReport
		if err := json.Unmarshal(raw, &r); err != nil {
			return string(raw)
		}
		return taskUsageText(&r)
	})
}

func taskUsageText(r *taskUsageReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (%s)\n", r.Key, r.Title, r.Who)
	fmt.Fprintf(&b, "$%.2f, %s tokens (%s cache reads), %s tool calls (%s failed), %s edits, %d subagents, %d sessions, %d active hours, %d run minutes.\n",
		r.CostUSD, commas(r.Input+r.Output+r.CacheWrite), commas(r.CacheRead), commas(r.ToolCalls), commas(r.ToolErrors), commas(r.Edits), r.Subagents, r.Sessions, r.ActiveHours, r.RunMinutes)
	if r.PRs > 0 {
		fmt.Fprintf(&b, "Merged: %d pull requests, %s files, %s lines.\n", r.PRs, commas(r.Files), commas(r.Lines))
	}
	for _, run := range r.Runs {
		fmt.Fprintf(&b, "Run %s %s %s: %s\n", run.Started.UTC().Format("2006-01-02 15:04"), run.Agent, cmpStr(run.Outcome, "open"), sortedPairs(run.Features))
	}
	if len(r.Arms) > 0 {
		fmt.Fprintf(&b, "Experiments: %s\n", sortedPairs(r.Arms))
	}
	if len(r.Used) > 0 {
		parts := make([]string, 0, len(r.Used))
		for _, u := range r.Used {
			parts = append(parts, fmt.Sprintf("%s ×%s", u.Name, commas(u.Calls)))
		}
		fmt.Fprintf(&b, "Used: %s\n", strings.Join(parts, ", "))
	}
	return b.String()
}

func sortedPairs(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}
