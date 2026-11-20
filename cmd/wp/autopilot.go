package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// wp autopilot (WP-194, WP-ADR9): turn autopilot on or off per project, set
// its limits, see what it is doing, and read what agents left in summaries.

var autopilotCmd = &command{
	name: "autopilot", group: "Tasks", args: "[on|off|set|summaries|reviewed|stop|now …]",
	summary: "restart finished sessions on the next task (agents read it, people change it)",
	about: `When a session in an autopilot project finishes its task, Waypoint waits a
minute, then restarts it in tmux on the next task (as wp next would pick
it). Type keep or /keep in that minute to keep the session. A session outside
tmux can't be moved on, and you are told so.

Limits pause it and it resumes by itself: the last hour's spend, the week's
quota running ahead of an even pace or past 90%, and the five-hour window
past 90% (wp autopilot set). Turning it on and changing limits is for you,
in the web or your own terminal, never from an agent's shell.
A task that builds on a pull request of the session that hasn't merged waits
for it (--merge auto, at most 3 hours, --merge-wait).

Criteria narrow which tasks it starts (all you set must hold): --priority,
--due-within DAYS, --approved on, --planned on, --query with any search.
It takes them in wp next's order. Pace limits how often it starts one, in
each project: --per-hour, --per-day, --spacing, --at-once (tasks under way
at a time across your sessions, 1 by default), and --hours and --days to
start them in. Start and now skip the hours, the spacing and --at-once.

With --review on, a session is first asked for an adversarial review of its
change (wp worklog KEY), a minute after it stops; on and off only decide
whether the next task starts, so reviews run either way. In your own agent sessions
the review comes before the pull request: wp ship pushes the branch and says
to end the turn; the review's prompt asks for wp ship --pr, which opens the
pull request (wp ship -h: --review, --pr, --skip-review). A pull request already open waits for its review before
merging. If the review can't run (no runner, a failed job), Waypoint opens the pull
request without it and says so. A round's reviewer works from your brief
(--review-text-file; wp autopilot reviewer --brief prints the default) and is
who --review-model names: auto (the stakes-tiered chain), session (the
session reviews itself), or a model such as gateway/MODEL or claude/sonnet
(wp autopilot reviewer lists them). Each round's findings are scored, and
another round follows while new, serious findings keep coming on a change
whose stakes call for it (wp pr rounds). Every session keeps the agent's last message as its
summary (wp autopilot summaries).

Without a subcommand: what autopilot is doing in each project and the limit
readings. -p picks the project for on, off and set.`,
	examples: []string{"wp autopilot", "wp autopilot on", "wp autopilot set --max-hourly 50 --week-ahead 10", "wp autopilot summaries"},
	run:      cmdAutopilot,
}

func init() {
	autopilotCmd.subs = []*command{
		{name: "on", args: "[-p PROJECT]", summary: "turn autopilot on in the project (this repo's by default)", examples: []string{"wp autopilot on", "wp autopilot on -p APP"}, run: func(c *cli, args []string) error { return cmdAutopilotToggle(c, args, true) }},
		{name: "off", args: "[-p PROJECT]", summary: "turn it off there: a step it was about to take stops; a review held before its pull request still runs", examples: []string{"wp autopilot off", "wp autopilot off -p APP"}, run: func(c *cli, args []string) error { return cmdAutopilotToggle(c, args, false) }},
		{name: "set", args: "[-p PROJECT | --defaults] [LIMITS] [--reset]", summary: "set limits for a project, or your defaults for every project",
			examples: []string{"wp autopilot set --defaults --max-hourly 50", "wp autopilot set -p APP --week-ahead 0 --merge never", "wp autopilot set -p APP --reset", "wp autopilot set --defaults --review on",
				"wp autopilot set -p WP --priority high --approved on", "wp autopilot set --defaults --per-day 10 --hours 09:00-18:00 --days mon-fri", "wp autopilot set -p WP --merge always --merge-wait 168", "wp autopilot set --defaults --spend-left on"},
			run: cmdAutopilotSet},
		{name: "reviewer", args: "[--brief]", summary: "who can review a round (for --review-model), or --brief: the default reviewer's brief, to edit for --review-text-file",
			examples: []string{"wp autopilot reviewer", "wp autopilot reviewer --brief > brief.md"}, run: cmdAutopilotReviewer},
		{name: "summaries", args: "[--all] [-p PROJECT] [--limit N]", summary: "what agents said after finishing their tasks: the ones you haven't read, or --all",
			examples: []string{"wp autopilot summaries", "wp autopilot summaries --all -p APP"}, run: cmdAutopilotSummaries},
		{name: "reviewed", args: "ID… | --all [-p PROJECT]", summary: "mark summaries read (--unread: unread again)", examples: []string{"wp autopilot reviewed 12 13", "wp autopilot reviewed --all"}, run: cmdAutopilotReviewed},
		{name: "stop", args: "STEP", summary: "stop what autopilot was about to do with a session (the step number wp autopilot shows)",
			examples: []string{"wp autopilot stop 4"}, run: func(c *cli, args []string) error { return cmdAutopilotStep(c, args, "stop") }},
		{name: "now", args: "STEP", summary: "do it now: no grace, merge wait, hours or spacing (the limits and counts still apply)",
			examples: []string{"wp autopilot now 4"}, run: func(c *cli, args []string) error { return cmdAutopilotStep(c, args, "now") }},
	}
}

type apLimits struct {
	MaxHourlyUSD *float64 `json:"max_hourly_usd,omitempty"`
	WeekAhead    *float64 `json:"week_ahead,omitempty"`
	WeekMax      *float64 `json:"week_max,omitempty"`
	WeekEndHours *float64 `json:"week_end_hours,omitempty"`
	FiveHourMax  *float64 `json:"five_hour_max,omitempty"`
	Merge        *string  `json:"merge,omitempty"`
	GraceSeconds *int     `json:"grace_seconds,omitempty"`
	Review       *bool    `json:"review,omitempty"`
	ReviewPrompt *string  `json:"review_prompt,omitempty"`
	ReviewSkip   *string  `json:"review_skip,omitempty"`
	// The reviewer's brief and who reviews a round (WP-1320).
	ReviewText  *string `json:"review_text,omitempty"`
	ReviewModel *string `json:"review_model,omitempty"`
	// Criteria and pace (WP-284): the server words them (criteria, pace);
	// set here only to tell a project's own settings apart.
	MinPriority    *string `json:"min_priority,omitempty"`
	DueWithinDays  *int    `json:"due_within_days,omitempty"`
	Approved       *bool   `json:"approved,omitempty"`
	Planned        *bool   `json:"planned,omitempty"`
	Query          *string `json:"query,omitempty"`
	PerHour        *int    `json:"per_hour,omitempty"`
	PerDay         *int    `json:"per_day,omitempty"`
	SpacingMinutes *int    `json:"spacing_minutes,omitempty"`
	AtOnce         *int    `json:"at_once,omitempty"`
	MergeWaitHours *int    `json:"merge_wait_hours,omitempty"`
	Hours          *string `json:"hours,omitempty"`
	Days           *string `json:"days,omitempty"`
	// Use what's left at the week's end (WP-262): your defaults only.
	SpendLeft   *bool `json:"spend_left,omitempty"`
	SpendAtOnce *int  `json:"spend_at_once,omitempty"`
}

// apSpend is the week's-end plan (WP-262).
type apSpend struct {
	On       bool   `json:"on"`
	InWindow bool   `json:"in_window"`
	Cap      int    `json:"cap"`
	Why      string `json:"why"`
	Started  []struct {
		Project string `json:"project"`
		State   string `json:"state"`
		Task    *struct {
			Key string `json:"key"`
		} `json:"task"`
	} `json:"started"`
}

type apStep struct {
	ID      int64     `json:"id"`
	State   string    `json:"state"`
	Reason  string    `json:"reason"`
	Detail  string    `json:"detail"`
	DueAt   time.Time `json:"due_at"`
	Session *struct { // nil: a session of autopilot's own, still starting
		Title   string `json:"title"`
		Machine string `json:"machine"`
	} `json:"session"`
	Done *struct {
		Key string `json:"key"`
	} `json:"done"`
	Next *struct {
		Key string `json:"key"`
	} `json:"next"`
}

type apOverview struct {
	Effective apLimits `json:"effective"`
	Criteria  string   `json:"criteria"`
	Pace      string   `json:"pace"`
	Projects  []struct {
		ProjectKey string   `json:"project_key"`
		Enabled    bool     `json:"enabled"`
		Limits     apLimits `json:"limits"`
		Effective  apLimits `json:"effective"`
		Criteria   string   `json:"criteria"`
		Pace       string   `json:"pace"`
		Steps      []apStep `json:"steps"`
	} `json:"projects"`
	Meter struct {
		HourlyUSD     float64  `json:"hourly_usd"`
		WeekPercent   *float64 `json:"week_percent"`
		WeekEven      *float64 `json:"week_even"`
		WeekCap       *float64 `json:"week_cap"`
		FiveHour      *float64 `json:"five_hour_percent"`
		QuotaUnknown  bool     `json:"quota_unknown"`
		PausedBecause string   `json:"paused_because"`
	} `json:"meter"`
	Unreviewed int      `json:"unreviewed"`
	Spend      *apSpend `json:"spend"`
}

func (l apLimits) String() string {
	f := func(p *float64, unit string) string {
		if p == nil {
			return "?"
		}
		if *p == 0 {
			return "off"
		}
		return strconv.FormatFloat(*p, 'f', -1, 64) + unit
	}
	merge, grace := "auto", 60
	if l.Merge != nil {
		merge = *l.Merge
	}
	if l.MergeWaitHours != nil && merge != "never" {
		merge += fmt.Sprintf(" (up to %d h)", *l.MergeWaitHours)
	}
	if l.GraceSeconds != nil {
		grace = *l.GraceSeconds
	}
	review := "off"
	if l.Review != nil && *l.Review {
		review = "on"
		if l.ReviewPrompt != nil && strings.TrimSpace(*l.ReviewPrompt) != "" {
			review += " (own prompt)"
		}
		if l.ReviewSkip != nil && *l.ReviewSkip != "on" {
			review += ", low-risk skips " + *l.ReviewSkip
		}
	}
	if l.ReviewModel != nil && *l.ReviewModel != "" && *l.ReviewModel != "auto" {
		review += ", reviewer " + *l.ReviewModel
	}
	if l.ReviewText != nil && strings.TrimSpace(*l.ReviewText) != "" {
		review += ", own reviewer brief"
	}
	return fmt.Sprintf("hourly $%s · week +%s ahead, max %s, 100%% for the last %s · five hours %s · merge %s · grace %ds · review %s",
		strings.TrimSuffix(f(l.MaxHourlyUSD, ""), ""), f(l.WeekAhead, " pts"), f(l.WeekMax, "%"), f(l.WeekEndHours, " h"), f(l.FiveHourMax, "%"), merge, grace, review)
}

func cmdAutopilot(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unknown subcommand %q", pos[0])
	}
	raw, err := c.call("GET", "/api/v1/autopilot", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var o apOverview
		if json.Unmarshal(raw, &o) != nil {
			return string(raw)
		}
		return autopilotText(&o)
	})
}

func autopilotText(o *apOverview) string {
	var b strings.Builder
	m := o.Meter
	fmt.Fprintf(&b, "Last hour: $%.2f", m.HourlyUSD)
	if m.WeekPercent != nil && m.WeekEven != nil {
		fmt.Fprintf(&b, " · week %.0f%% (even pace %.0f%%", *m.WeekPercent, *m.WeekEven)
		if m.WeekCap != nil {
			fmt.Fprintf(&b, ", pauses at %.0f%%", *m.WeekCap)
		}
		b.WriteString(")")
	} else if m.QuotaUnknown {
		b.WriteString(" · week: no recent quota reading")
	}
	if m.FiveHour != nil {
		fmt.Fprintf(&b, " · five hours %.0f%%", *m.FiveHour)
	}
	b.WriteString("\n")
	if m.PausedBecause != "" {
		fmt.Fprintf(&b, "Paused by your defaults now: %s.\n", m.PausedBecause)
	}
	fmt.Fprintf(&b, "Defaults: %s\n", o.Effective)
	b.WriteString(takesLine("  ", o.Criteria, o.Pace))
	b.WriteString(spendText(o.Spend))
	on := 0
	for _, p := range o.Projects {
		if !p.Enabled && len(p.Steps) == 0 {
			continue
		}
		state := "off"
		if p.Enabled {
			state, on = "on", on+1
		}
		fmt.Fprintf(&b, "\n%s: %s", p.ProjectKey, state)
		if p.Limits != (apLimits{}) {
			fmt.Fprintf(&b, " (%s)", p.Effective)
		}
		b.WriteString("\n")
		if p.Criteria != o.Criteria || p.Pace != o.Pace {
			line := takesLine("  ", p.Criteria, p.Pace)
			if line == "" {
				line = "  any ready task\n"
			}
			b.WriteString(line)
		}
		for _, s := range p.Steps {
			if s.Session == nil {
				fmt.Fprintf(&b, "  step %d · a session of its own", s.ID)
			} else {
				fmt.Fprintf(&b, "  step %d · %s", s.ID, cmpStr(s.Session.Title, "a session"))
				if s.Session.Machine != "" {
					fmt.Fprintf(&b, " on %s", s.Session.Machine)
				}
			}
			if s.Done != nil {
				fmt.Fprintf(&b, " · %s done", s.Done.Key)
			}
			fmt.Fprintf(&b, " · %s", s.State)
			if s.Next != nil {
				fmt.Fprintf(&b, " %s", s.Next.Key)
			}
			if s.Detail != "" {
				fmt.Fprintf(&b, ": %s", s.Detail)
			}
			if s.State == "due" || s.State == "waiting" {
				fmt.Fprintf(&b, " (next look %s)", s.DueAt.Local().Format("15:04"))
			}
			b.WriteString("\n")
		}
	}
	if on == 0 {
		b.WriteString("\nAutopilot is off in every project. wp autopilot on turns it on in this repo's.\n")
	}
	if o.Unreviewed > 0 {
		fmt.Fprintf(&b, "\n%d task summar%s to read: wp autopilot summaries\n", o.Unreviewed, map[bool]string{true: "y", false: "ies"}[o.Unreviewed == 1])
	}
	return b.String()
}

// spendText is the week's-end plan, when it is on (WP-262).
func spendText(p *apSpend) string {
	if p == nil || !p.On {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Use what's left at the week's end (up to %d at once): %s\n", p.Cap, cmpStr(p.Why, "on"))
	if len(p.Started) > 0 {
		var keys []string
		for _, s := range p.Started {
			k := s.Project
			if s.Task != nil {
				k = s.Task.Key
			}
			keys = append(keys, k+" ("+s.State+")")
		}
		fmt.Fprintf(&b, "  started this week's end: %s\n", strings.Join(keys, ", "))
	}
	return b.String()
}

// takesLine says which tasks autopilot takes and at what pace, if either
// is narrower than Waypoint's.
func takesLine(indent, criteria, pace string) string {
	var parts []string
	if criteria != "" {
		parts = append(parts, "only tasks "+criteria)
	} else if pace != "" {
		parts = append(parts, "any ready task")
	}
	if pace != "" {
		parts = append(parts, "pace: "+pace)
	}
	if len(parts) == 0 {
		return ""
	}
	return indent + strings.Join(parts, " · ") + "\n"
}

// autopilotProject is the project on, off and set act on.
func (c *cli) autopilotProject(sf scopeFlags) (string, error) {
	sc, err := c.scopeFor(sf)
	if err != nil {
		return "", err
	}
	if sc.all() {
		return "", c.usage("autopilot is set per project: give one with -p PROJECT")
	}
	return sc.home()
}

func cmdAutopilotToggle(c *cli, args []string, on bool) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	key, err := c.autopilotProject(sf)
	if err != nil {
		return err
	}
	raw, err := c.call("PUT", "/api/v1/autopilot/projects/"+url.PathEscape(key), map[string]any{"enabled": on, "merge": true, "limits": map[string]any{}})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		if on {
			return fmt.Sprintf("Autopilot is on in %s: it starts tasks in sessions of its own and leaves yours alone.\n", key)
		}
		return fmt.Sprintf("Autopilot is off in %s.\n", key)
	})
}

func cmdAutopilotSet(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	defaults := fs.Bool("defaults", false, "set your defaults for every project instead of one project's")
	reset := fs.Bool("reset", false, "clear what it sets, so everything inherits")
	hourly := fs.Float64("max-hourly", 0, "pause once the last hour cost more than `USD` (0: off)")
	ahead := fs.Float64("week-ahead", 0, "pause once the week's quota runs more than `POINTS` ahead of an even pace (0: off)")
	weekMax := fs.Float64("week-max", 0, "pause once the week's quota reaches `PERCENT` (0: off)")
	weekEnd := fs.Float64("week-end", 0, "ceilings rise to 100% over the week's last `HOURS` (0: hold)")
	five := fs.Float64("five-hour", 0, "pause once the five-hour window reaches `PERCENT` (0: off)")
	merge := fs.String("merge", "", "wait for unmerged PRs: `auto` (only a task building on one), always, never")
	grace := fs.Int("grace", 0, "`SECONDS` between a finished session and its restart")
	review := fs.String("review", "", "`on` or off: review a session's task before moving on")
	reviewPrompt := fs.String("review-prompt", "", "the review's `PROMPT` (\"\" for the default)")
	reviewSkip := fs.String("review-skip", "", "low-risk changes: `on` skips their review, shadow only records, off reviews all")
	reviewText := fs.String("review-text-file", "", "the reviewer's brief for every review round, from `FILE` (- for stdin; \"\" for the default; wp autopilot reviewer --brief prints it)")
	reviewModel := fs.String("review-model", "", "who reviews a round: `auto`, session (the session itself), or cli/model: gateway/MODEL, claude/sonnet, codex/MODEL (wp autopilot reviewer lists them)")
	priority := fs.String("priority", "", "only start tasks of `PRIORITY` or above: low, medium, high, urgent (any: off)")
	dueWithin := fs.Int("due-within", 0, "only start tasks due within `DAYS`, overdue included (0: off)")
	approved := fs.String("approved", "", "`on`: only start tasks a person wrote or approved (is:approved); off")
	planned := fs.String("planned", "", "`on`: only start tasks a published plan names or a free model prepared (is:planned); off")
	query := fs.String("query", "", "only start tasks that also match this `WQL` (\"\": off)")
	perHour := fs.Int("per-hour", 0, "start at most `N` tasks an hour in a project (1-60; the loop guard, never off)")
	perDay := fs.Int("per-day", 0, "start at most `N` tasks a day in a project, in your time zone (0: off)")
	spacing := fs.Int("spacing", 0, "leave at least `MINUTES` between two starts in a project (0: off)")
	atOnce := fs.Int("at-once", 0, "run at most `N` sessions in a project: autopilot's, and your own while at a task (default 1; 0: starts none of its own)")
	mergeWait := fs.Int("merge-wait", 0, "a task waits for a merge at most `HOURS` (1-168), then starts anyway")
	hours := fs.String("hours", "", "only start tasks between these times, in your time zone: `09:00-17:00` (22:00-06:00 overnight; any: off)")
	days := fs.String("days", "", "only start tasks on these days: `mon-fri`, sat,sun (all: off)")
	spendLeft := fs.String("spend-left", "", "`on`: over the week's last --week-end hours, start sessions past --at-once so the week's quota is used by its reset; off (with --defaults)")
	spendAtOnce := fs.Int("spend-at-once", 0, "while it uses what's left, run at most `N` autopilot sessions across your projects (1-20, default 3; with --defaults)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	lim := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "max-hourly":
			lim["max_hourly_usd"] = *hourly
		case "week-ahead":
			lim["week_ahead"] = *ahead
		case "week-max":
			lim["week_max"] = *weekMax
		case "week-end":
			lim["week_end_hours"] = *weekEnd
		case "five-hour":
			lim["five_hour_max"] = *five
		case "merge":
			lim["merge"] = *merge
		case "grace":
			lim["grace_seconds"] = *grace
		case "review":
			lim["review"] = *review == "on"
		case "review-prompt":
			lim["review_prompt"] = *reviewPrompt
		case "review-skip":
			lim["review_skip"] = *reviewSkip
		case "review-text-file":
			lim["review_text"] = ""
		case "review-model":
			lim["review_model"] = *reviewModel
		case "priority":
			lim["min_priority"] = *priority
		case "due-within":
			lim["due_within_days"] = *dueWithin
		case "approved":
			lim["approved"] = *approved == "on"
		case "planned":
			lim["planned"] = *planned == "on"
		case "query":
			lim["query"] = *query
		case "per-hour":
			lim["per_hour"] = *perHour
		case "per-day":
			lim["per_day"] = *perDay
		case "spacing":
			lim["spacing_minutes"] = *spacing
		case "at-once":
			lim["at_once"] = *atOnce
		case "merge-wait":
			lim["merge_wait_hours"] = *mergeWait
		case "hours":
			lim["hours"] = *hours
		case "days":
			lim["days"] = *days
		case "spend-left":
			lim["spend_left"] = *spendLeft == "on"
		case "spend-at-once":
			lim["spend_at_once"] = *spendAtOnce
		}
	})
	for name, v := range map[string]string{"approved": *approved, "planned": *planned, "spend-left": *spendLeft} {
		if v != "" && v != "on" && v != "off" {
			return c.usage("--%s is on or off", name)
		}
	}
	if *review != "" && *review != "on" && *review != "off" {
		return c.usage("--review is on or off")
	}
	if _, ok := lim["review_text"]; ok && *reviewText != "" {
		text, err := readBody(*reviewText)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return c.usage("%s is empty: --review-text-file \"\" goes back to the default brief", *reviewText)
		}
		lim["review_text"] = text
	}
	if len(lim) == 0 && !*reset {
		return c.usage("set what? limits (--max-hourly, --week-ahead, --week-max, --week-end, --five-hour, --merge, --merge-wait, --grace, --review, --review-prompt, --review-skip, --review-text-file, --review-model), " +
			"which tasks (--priority, --due-within, --approved, --planned, --query), pace (--per-hour, --per-day, --spacing, --at-once, --hours, --days), " +
			"the week's end (--defaults --spend-left, --spend-at-once) or --reset")
	}
	path := "/api/v1/autopilot"
	where := "your defaults"
	if !*defaults {
		key, err := c.autopilotProject(sf)
		if err != nil {
			return err
		}
		path, where = "/api/v1/autopilot/projects/"+url.PathEscape(key), key
	}
	raw, err := c.call("PUT", path, map[string]any{"limits": lim, "merge": true, "reset": *reset})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		out := "Saved autopilot settings for " + where + ".\n"
		var o apOverview
		if json.Unmarshal(raw, &o) != nil {
			return out
		}
		criteria, pace := o.Criteria, o.Pace
		for _, p := range o.Projects {
			if !*defaults && p.ProjectKey == where {
				criteria, pace = p.Criteria, p.Pace
			}
		}
		if line := takesLine("", criteria, pace); line != "" {
			out += strings.ToUpper(line[:1]) + line[1:]
		}
		return out
	})
}

// cmdAutopilotReviewer lists what --review-model takes, or prints the
// default reviewer's brief (WP-1320).
func cmdAutopilotReviewer(c *cli, args []string) error {
	fs := c.flags()
	brief := fs.Bool("brief", false, "print the default reviewer's brief instead")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	raw, err := c.call("GET", "/api/v1/autopilot/review-models", nil)
	if err != nil {
		return err
	}
	type option struct {
		Value string `json:"value"`
		Label string `json:"label"`
		Note  string `json:"note"`
	}
	var o struct {
		Modes       []option `json:"modes"`
		Gateway     []option `json:"gateway"`
		Runners     []option `json:"runners"`
		DefaultText string   `json:"default_text"`
		MaxText     int      `json:"max_text"`
	}
	if *brief {
		if err := json.Unmarshal(raw, &o); err != nil {
			return err
		}
		_, err := fmt.Fprintln(c.out, o.DefaultText)
		return err
	}
	return c.emit(raw, func() string {
		if json.Unmarshal(raw, &o) != nil {
			return string(raw)
		}
		var b strings.Builder
		group := func(name string, opts []option, none string) {
			fmt.Fprintf(&b, "%s:\n", name)
			if len(opts) == 0 {
				fmt.Fprintf(&b, "  (%s)\n", none)
			}
			for _, x := range opts {
				line := "  " + x.Value + "  " + x.Label
				if x.Note != "" {
					line += " (" + x.Note + ")"
				}
				b.WriteString(line + "\n")
			}
		}
		group("Modes", o.Modes, "none")
		group("Behind the LLM gateway", o.Gateway, "no gateway set up")
		group("On your runners", o.Runners, "no signed-in coding-agent CLI on your runners")
		fmt.Fprintf(&b, "Set one: wp autopilot set --defaults --review-model VALUE (any cli/model works). The reviewer's brief: wp autopilot reviewer --brief > brief.md, edit it, wp autopilot set --defaults --review-text-file brief.md (at most %d characters).", o.MaxText)
		return b.String()
	})
}

type apSummary struct {
	ID    int64 `json:"id"`
	Issue *struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	} `json:"issue"`
	Machine    string     `json:"machine"`
	Agent      string     `json:"agent"`
	Body       string     `json:"body"`
	Asks       []string   `json:"asks"`
	Request    string     `json:"request"`
	Autopilot  bool       `json:"autopilot"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ReviewedAt *time.Time `json:"reviewed_at"`
}

func cmdAutopilotSummaries(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "only this `PROJECT`'s")
	short(fs, "p", "project")
	all := fs.Bool("all", false, "read ones too")
	limit := fs.Int("limit", 20, "at most `N`")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}}
	if *project != "" {
		q.Set("project", *project)
	}
	if *all {
		q.Set("all", "1")
	}
	raw, err := c.call("GET", "/api/v1/autopilot/summaries?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var page struct {
			Summaries  []apSummary `json:"summaries"`
			Unreviewed int         `json:"unreviewed"`
		}
		if json.Unmarshal(raw, &page) != nil {
			return string(raw)
		}
		if len(page.Summaries) == 0 {
			return "No task summaries to read.\n"
		}
		var b strings.Builder
		for _, s := range page.Summaries {
			key, title := "a task", ""
			if s.Issue != nil {
				key, title = s.Issue.Key, s.Issue.Title
			}
			fmt.Fprintf(&b, "── #%d %s %s · %s", s.ID, key, title, s.UpdatedAt.Local().Format("Jan 2 15:04"))
			if s.Machine != "" {
				fmt.Fprintf(&b, " · %s", s.Machine)
			}
			if s.Autopilot {
				b.WriteString(" · autopilot moved on")
			}
			if s.ReviewedAt != nil {
				b.WriteString(" · read")
			}
			fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(s.Body))
			if len(s.Asks) > 0 {
				fmt.Fprintf(&b, "Unfiled asks found in it (%s): %s\n", cmpStr(s.Request, "not filed"), strings.Join(s.Asks, " | "))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d unread. wp autopilot reviewed ID… (or --all) marks them read.\n", page.Unreviewed)
		return b.String()
	})
}

func cmdAutopilotReviewed(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "with --all: only this `PROJECT`'s")
	short(fs, "p", "project")
	all := fs.Bool("all", false, "every unread one")
	unread := fs.Bool("unread", false, "mark them unread again")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	var ids []int64
	for _, a := range pos {
		n, err := strconv.ParseInt(strings.TrimPrefix(a, "#"), 10, 64)
		if err != nil || n <= 0 {
			return c.usage("%q isn't a summary number", a)
		}
		ids = append(ids, n)
	}
	if len(ids) == 0 && !*all {
		return c.usage("which summaries? Give their numbers, or --all")
	}
	raw, err := c.call("POST", "/api/v1/autopilot/summaries/reviewed", map[string]any{"ids": ids, "all": *all, "project": *project, "unread": *unread})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r struct {
			Updated int `json:"updated"`
		}
		_ = json.Unmarshal(raw, &r)
		return fmt.Sprintf("Marked %d summar%s %s.\n", r.Updated, map[bool]string{true: "y", false: "ies"}[r.Updated == 1], map[bool]string{true: "unread", false: "read"}[*unread])
	})
}

func cmdAutopilotStep(c *cli, args []string, action string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("which step? wp autopilot shows their numbers")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(pos[0], "#"), 10, 64)
	if err != nil || id <= 0 {
		return c.usage("%q isn't a step number", pos[0])
	}
	if _, err := c.call("POST", fmt.Sprintf("/api/v1/autopilot/steps/%d/%s", id, action), nil); err != nil {
		return err
	}
	if action == "stop" {
		c.println("Stopped: that session stays as it is.")
	} else {
		c.println("Starting it now (the limits still apply): wp autopilot shows how it went.")
	}
	return nil
}
