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
	name: "task", args: "KEY [--user NAME] | --none [--days N] [--user NAME]",
	summary: "what one task used, and what its agents had; or the spend no task has",
	about: `One task's tokens, cost, tool calls, edits, time and merged pull requests,
each run's versions (server, wp, session start, kit, Claude Code, hooks), its
experiment arms (admins), and the tools, skills and commands it used. Also its
review rounds (found, fixed, rejected where counted), the tokens its review
audits used, and each pull request's state and merge time.

Spend in the task's session after its run ended, that no other task claimed
(a --skip-review ship ends the run; later review rounds follow), is printed
apart from the cost and is not in it.

--none instead splits the spend of the last --days (30) that no task has into
carry-over (hours after a run ended in a session that had one, by the task it
follows) and sessions that never had a run, with the same totals as the
Trends tab. Without --user it is your own spend.`,
	examples: []string{"wp usage task WP-192", "wp usage task --none --days 7"},
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
	Arms           map[string]string `json:"arms"`
	RunMinutes     int64             `json:"run_minutes"`
	ActiveHours    int               `json:"active_hours"`
	Sessions       int               `json:"sessions"`
	Subagents      int64             `json:"subagents"`
	ToolCalls      int64             `json:"tool_calls"`
	ToolErrors     int64             `json:"tool_errors"`
	Edits          int64             `json:"edits"`
	ShellEdits     *int64            `json:"shell_edits"`
	ShellUncounted *int64            `json:"shell_edits_uncounted"`
	ShellCounted   int64             `json:"shell_edits_counted"` // absent from an older server: 0
	// Edit-tool calls and writing Bash calls REPL scripts ran: in neither
	// figure above.
	ScriptEdits          int64   `json:"script_edits"`
	ScriptShellEdits     int64   `json:"script_shell_edits"`
	ScriptShellUncounted int64   `json:"script_shell_uncounted"`
	PRs                  int64   `json:"prs"`
	Files                int64   `json:"files"`
	Lines                int64   `json:"lines"`
	Input                int64   `json:"input"`
	Output               int64   `json:"output"`
	CacheWrite           int64   `json:"cache_write"`
	CacheRead            int64   `json:"cache_read"`
	CostUSD              float64 `json:"cost_usd"`
	Used                 []struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Calls int64  `json:"calls"`
	} `json:"used"`
	AfterRun *struct {
		CostUSD  float64 `json:"cost_usd"`
		Sessions int     `json:"sessions"`
	} `json:"after_run"`
	Rounds []struct {
		At       time.Time `json:"at"`
		State    string    `json:"state"`
		Found    *int      `json:"found"`
		Fixed    *int      `json:"fixed"`
		Rejected *int      `json:"rejected"`
	} `json:"rounds"`
	Reviewer *struct {
		Audits int   `json:"audits"`
		Input  int64 `json:"input"`
		Output int64 `json:"output"`
	} `json:"reviewer"`
	PullRequests []struct {
		Repo     string     `json:"repo"`
		Number   int        `json:"number"`
		State    string     `json:"state"`
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_requests"`
}

func cmdUsageTask(c *cli, args []string) error {
	fs := c.flags()
	user := fs.String("user", "", "admins: another person's (`NAME`, or all)")
	none := fs.Bool("none", false, "the spend no task has, instead of one task's")
	days := fs.Int("days", 0, "with --none: how many days back (default 30)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if *days < 0 {
		return c.usage("--days is a number of days back, 1 or more: wp usage task --none --days 7")
	}
	if *none {
		if len(pos) != 0 {
			return c.usage("--none is for all spend: leave out the task %s", pos[0])
		}
		return cmdUsageNoTask(c, *user, *days)
	}
	if *days != 0 {
		return c.usage("--days goes with --none: wp usage task --none --days %d", *days)
	}
	if len(pos) != 1 {
		return c.usage("say which task, or --none: wp usage task WP-192, wp usage task --none")
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
	fmt.Fprintf(&b, "$%.2f, %s tokens (%s cache reads), %s tool calls (%s failed), %s, %d subagents, %d sessions, %d active hours, %d run minutes.\n",
		r.CostUSD, commas(r.Input+r.Output+r.CacheWrite), commas(r.CacheRead), commas(r.ToolCalls), commas(r.ToolErrors), taskEditsText(r), r.Subagents, r.Sessions, r.ActiveHours, r.RunMinutes)
	if a := r.AfterRun; a != nil && a.CostUSD > 0 {
		fmt.Fprintf(&b, "After its run ended, same session, no other task claimed: $%.2f in %d session%s.\n", a.CostUSD, a.Sessions, plural(a.Sessions))
	}
	if r.PRs > 0 {
		fmt.Fprintf(&b, "Merged: %d pull requests, %s files, %s lines.\n", r.PRs, commas(r.Files), commas(r.Lines))
	}
	taskReviewText(&b, r)
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

// taskEditsText is editsText for a report; ShellEdits and ShellUncounted are nil
// when the server sent neither (an older server), which must not print as
// "shell edits 0".
func taskEditsText(r *taskUsageReport) string {
	if r.ShellEdits == nil && r.ShellUncounted == nil {
		return fmt.Sprintf("edits %s, shell edits unknown (server too old to say)", commas(r.Edits))
	}
	var shell, unc int64
	if r.ShellEdits != nil {
		shell = *r.ShellEdits
	}
	if r.ShellUncounted != nil {
		unc = *r.ShellUncounted
	}
	return editsText(r.Edits, shell, unc, r.ShellCounted) + scriptEditsText(r.ScriptEdits, r.ScriptShellEdits, r.ScriptShellUncounted)
}

// scriptEditsText says the edits REPL scripts ran, which the figures before
// it leave out (as wp usage facts does); "" without any.
func scriptEditsText(edits, shell, uncounted int64) string {
	switch {
	case edits == 0 && shell == 0 && uncounted == 0:
		return ""
	case uncounted > 0:
		return fmt.Sprintf(" (REPL scripts ran %s more edits and %s+ more shell edits; %s of their Bash calls not counted)", commas(edits), commas(shell), commas(uncounted))
	}
	return fmt.Sprintf(" (REPL scripts ran %s more edits and %s more shell edits)", commas(edits), commas(shell))
}

// editsText says the edits of the editing tools beside the Bash calls that
// wrote files. An hour whose client did not count Bash writes leaves the
// second figure a floor, or unknown when no hour was counted. counted is the
// Bash calls of counted hours: with some, a zero is "0+", not unknown.
func editsText(edits, shell, uncounted, counted int64) string {
	switch {
	case uncounted == 0:
		return fmt.Sprintf("edits %s, shell edits %s", commas(edits), commas(shell))
	case shell == 0 && counted == 0:
		return fmt.Sprintf("edits %s, shell edits unknown (%s Bash calls not counted)", commas(edits), commas(uncounted))
	}
	return fmt.Sprintf("edits %s, shell edits %s+ (%s Bash calls not counted)", commas(edits), commas(shell), commas(uncounted))
}
