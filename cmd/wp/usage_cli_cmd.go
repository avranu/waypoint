package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var usageCLICmd = &command{
	name: "cli", args: "[--since 7d] [--repo REPO] [--user NAME] [-v]",
	summary: "how agents use wp: calls, help and error rates, what follows each command",
	about: `What wp usage sync counted in Claude Code's transcripts: sessions, wp calls,
help lookups and usage errors by week, the commands used most, the flags and
commands agents tried that don't exist, and what they ran next. Sessions
include subagents; help/main is per session a person's agent started; forge/s
is curl against a forge's API, ci-sleep a sleep beside wp pr or wp ci. Each
machine's next sync counts the transcripts it had synced before. Charts:
Agents → wp CLI.`,
	examples: []string{"wp usage cli", "wp usage cli --since 30d -v", "wp usage cli --repo claude/waypoint"},
	run:      cmdUsageCLI,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageCLICmd)
}

type cliPeriodJSON struct {
	Week            string   `json:"week"`
	Sessions        int64    `json:"sessions"`
	WP              int64    `json:"wp"`
	Help            int64    `json:"help"`
	SleepCI         int64    `json:"sleep_ci"`
	GhTea           int64    `json:"gh_tea"`
	BashPerSession  *float64 `json:"bash_per_session"`
	WPPerSession    *float64 `json:"wp_per_session"`
	HelpRate        *float64 `json:"help_rate"`
	ErrorRate       *float64 `json:"error_rate"`
	HelpPerMain     *float64 `json:"help_per_main"`
	CurlPerSession  *float64 `json:"curl_per_session"`
	SleepPerSession *float64 `json:"sleep_per_session"`
}

type cliCountJSON struct {
	Name string `json:"name"`
	N    int64  `json:"n"`
}

type cliFollowJSON struct {
	Name  string   `json:"name"`
	N     int64    `json:"n"`
	Share *float64 `json:"share"`
}

type cliCommandJSON struct {
	Head    string          `json:"head"`
	Calls   int64           `json:"calls"`
	Help    int64           `json:"help"`
	Errors  int64           `json:"errors"`
	Flags   []cliCountJSON  `json:"flags"`
	Follows []cliFollowJSON `json:"follows"`
}

type cliReportJSON struct {
	Days         int              `json:"days"`
	Who          string           `json:"who"`
	Repo         string           `json:"repo"`
	HasData      bool             `json:"has_data"`
	Total        cliPeriodJSON    `json:"total"`
	Weeks        []cliPeriodJSON  `json:"weeks"`
	Commands     []cliCommandJSON `json:"commands"`
	UnknownFlags []struct {
		Head string `json:"head"`
		Flag string `json:"flag"`
		N    int64  `json:"n"`
	} `json:"unknown_flags"`
	UnknownCommands []cliCountJSON `json:"unknown_commands"`
	ErrorClasses    []cliCountJSON `json:"error_classes"`
	Repos           []struct {
		Repo         string   `json:"repo"`
		Sessions     int64    `json:"sessions"`
		WP           int64    `json:"wp"`
		WPPerSession *float64 `json:"wp_per_session"`
		HelpRate     *float64 `json:"help_rate"`
		ErrorRate    *float64 `json:"error_rate"`
	} `json:"repos"`
}

func cmdUsageCLI(c *cli, args []string) error {
	fs := c.flags()
	since := fs.String("since", "7d", "how far back: `AGE` such as 7d, 4w")
	repo := fs.String("repo", "", "only this `REPO`")
	user := fs.String("user", "", "admins: another person's (`NAME`, or all)")
	verbose := fs.Bool("verbose", false, "what follows every command, flags, repos")
	short(fs, "v", "verbose")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	age, err := parseSince(*since)
	if err != nil || age <= 0 {
		return c.usage("--since takes an age such as 7d or 4w")
	}
	q := url.Values{"days": {strconv.Itoa(int(math.Ceil(age.Hours() / 24)))}}
	setIf(q, "repo", *repo)
	setIf(q, "user", *user)
	raw, err := c.call("GET", "/api/v1/usage/cli?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r cliReportJSON
		if json.Unmarshal(raw, &r) != nil {
			return string(raw)
		}
		return r.text(*verbose)
	})
}

func cliRate(v *float64, pct bool, places int) string {
	if v == nil {
		return "-"
	}
	if pct {
		return strconv.FormatFloat(*v*100, 'f', places, 64) + "%"
	}
	return strconv.FormatFloat(*v, 'f', places, 64)
}

func (p cliPeriodJSON) row(label string) string {
	return fmt.Sprintf("%-7s %5d %7s %5s %7s %6s %9s %7s %7s %8d %6d", label, p.Sessions, cliRate(p.BashPerSession, false, 0), cliRate(p.WPPerSession, false, 1),
		cliRate(p.HelpRate, true, 0), cliRate(p.ErrorRate, true, 1), cliRate(p.HelpPerMain, false, 2), cliRate(p.CurlPerSession, false, 2), cliRate(p.SleepPerSession, false, 2), p.SleepCI, p.GhTea)
}

// cliShort drops the "wp " every head starts with.
func cliShort(head string) string {
	return strings.TrimPrefix(head, "wp ")
}

func cliWeekLabel(date string) string {
	if t, err := time.Parse("2006-01-02", date); err == nil {
		return t.Format("Jan 02")
	}
	return date
}

// cliCountLine is one line of "name n" pairs, the first few.
func cliCountLine(title string, n int, each func(i int) string, limit int) string {
	if n == 0 {
		return ""
	}
	parts := make([]string, 0, limit)
	for i := 0; i < n && i < limit; i++ {
		parts = append(parts, each(i))
	}
	return title + ": " + strings.Join(parts, " · ") + "\n"
}

func (c *cliCommandJSON) followLine(limit int) string {
	if len(c.Follows) == 0 {
		return ""
	}
	return cliCountLine(fmt.Sprintf("After %s (%d)", c.Head, c.Calls-c.Help), len(c.Follows), func(i int) string {
		return cliRate(c.Follows[i].Share, true, 0) + " " + c.Follows[i].Name
	}, limit)
}

func (r *cliReportJSON) text(verbose bool) string {
	if r.Total.Sessions == 0 && r.Total.WP == 0 {
		if r.HasData {
			return fmt.Sprintf("No wp use in the last %d days.\n", r.Days)
		}
		return "Nothing counted yet: each machine counts its transcripts when it next syncs.\n"
	}
	var b strings.Builder
	scope := "all repos"
	if r.Repo != "" {
		scope = r.Repo
	}
	fmt.Fprintf(&b, "wp use, last %d days, %s: %d wp calls\n", r.Days, scope, r.Total.WP)
	fmt.Fprintf(&b, "%-7s %5s %7s %5s %7s %6s %9s %7s %7s %8s %6s\n", "week", "sess", "bash/s", "wp/s", "help/wp", "err/wp", "help/main", "forge/s", "sleep/s", "ci-sleep", "gh/tea")
	for _, w := range r.Weeks {
		if w.Sessions > 0 || w.WP > 0 {
			b.WriteString(w.row(cliWeekLabel(w.Week)) + "\n")
		}
	}
	top := r.Commands
	b.WriteString(cliCountLine("Top", len(top), func(i int) string { return fmt.Sprintf("%s %d", cliShort(top[i].Head), top[i].Calls) }, 10))
	var looked []cliCommandJSON
	for _, x := range top {
		if x.Help > 0 {
			looked = append(looked, x)
		}
	}
	sort.SliceStable(looked, func(i, j int) bool { return looked[i].Help > looked[j].Help })
	b.WriteString(cliCountLine("Help", len(looked), func(i int) string { return fmt.Sprintf("%s -h %d", cliShort(looked[i].Head), looked[i].Help) }, 6))
	limit := 6
	if verbose {
		limit = 30
	}
	uf := r.UnknownFlags
	b.WriteString(cliCountLine("Unknown flags", len(uf), func(i int) string { return fmt.Sprintf("%s %s %d", cliShort(uf[i].Head), uf[i].Flag, uf[i].N) }, limit))
	uc := r.UnknownCommands
	b.WriteString(cliCountLine("Unknown commands", len(uc), func(i int) string { return fmt.Sprintf("%s %d", cliShort(uc[i].Name), uc[i].N) }, limit))
	ec := r.ErrorClasses
	b.WriteString(cliCountLine("Errors", len(ec), func(i int) string { return fmt.Sprintf("%s %d", ec[i].Name, ec[i].N) }, limit))
	if !verbose {
		for i := range top {
			if top[i].Head == "wp next" {
				b.WriteString(top[i].followLine(5))
			}
		}
		b.WriteString("-v: what follows each command, flags, repos\n")
		return b.String()
	}
	for i := range top {
		x := &top[i]
		if x.Calls-x.Help < 5 {
			continue
		}
		b.WriteString(x.followLine(8))
		b.WriteString(cliCountLine("  flags", len(x.Flags), func(j int) string { return fmt.Sprintf("%s %d", x.Flags[j].Name, x.Flags[j].N) }, 8))
	}
	if len(r.Repos) > 0 {
		b.WriteString("Repos (wp calls, wp/s, help/wp, err/wp):\n")
		for _, x := range r.Repos {
			name := x.Repo
			if name == "" {
				name = "(no repo)"
			}
			fmt.Fprintf(&b, "  %s: %d, %s, %s, %s\n", name, x.WP, cliRate(x.WPPerSession, false, 1), cliRate(x.HelpRate, true, 0), cliRate(x.ErrorRate, true, 1))
		}
	}
	return b.String()
}
