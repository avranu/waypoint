package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

var usageGuardCmd = &command{
	name: "guard", args: "[--days 14] [--now] [--hours] [--user NAME]",
	summary: "the quota guard: what stops runaway automation, its lines and trips, and the daily checks",
	about: `The live guard stops automation that spends your Claude Code quota in a
way it shouldn't, as it happens. At every tool call a session automation
drives (a task session, a runner job, one whose last prompt autopilot
pasted) is judged on its machine:

  churn    spend since its last change (an edit, a commit, a ship)
  session  spend since automation gave it its work
  hours    hours automation has driven it
  model    spend on a main model priced above every one automation
           usually uses
  hold     a hold the server set (below)

and a session of yours nobody typed in for idle_hours, on its spend since.
At a line the session is told to hand its task off; at 1.5 times it (3
times for the model rule) the guard stops it, ends a task session, backs
its task off before autopilot takes it again, and tells you. A prompt you
type carries a stopped session on; a session you typed in is yours, and
only the idle rule applies to it.

Each minute the server holds automation back (no autopilot starts; running
automation sessions told, then stopped 10 minutes or $2 later) when:

  burst    automation spent more in the last hour than burst_usd
  pace     your autopilot limits hold it back and the week still rose 3
           points while automation spent
  trips    3 sessions were stopped in 6 hours

A hold ends by itself (an hour; 15 minutes for pace, 2 hours for trips)
or with ` + "`wp usage guard lift`" + `. Lines you don't set follow your
usual usage (the last 28 days); ` + "`wp usage guard set`" + ` sets or
resets them, or turns the guard off. Only you change your guard: from the
web, your terminal, or a session you typed in.

Once a day Waypoint also checks the last 24 hours of your weekly Claude Code
quota: how many points of the week its readings rose by, how many of those
went to automation (autopilot's sessions until you type in them, and
runner jobs that ran Claude), and in which phase of the week:

  under    below an even pace
  ahead    at or ahead of an even pace, within autopilot's limits
  end      the week's last 2×week_end_hours, within the raised limits
  over     autopilot's limits (week_ahead, week_max) hold it back
  off      you turned the quota limits off
  unknown  no reading of the week

Points are shared out by usage at list price: each rise of the readings
over the hours it spans, and an hour's points by what each side spent in
it. "Nobody's" is points where no usage was reported.

A check is a breach when automation used 3 or more points while the limits
held it back, or a use-what's-left start came before the week's last
week_end_hours; you are notified. --now checks the last 24 hours without
storing it; --hours lists each hour that spent anything.`,
	examples: []string{"wp usage guard", "wp usage guard --now --hours", "wp usage guard --days 30"},
	run:      cmdUsageGuard,
	subs:     []*command{usageGuardSetCmd, usageGuardLiftCmd},
}

var usageGuardSetCmd = &command{
	name: "set", args: "[--off|--on] [--churn USD] [--session USD] [--hours H] [--idle-hours H] [--model-usd USD] [--models A,B] [--burst USD] [--pace on|off] [--default FIELD,…|all]",
	summary: "change the live quota guard's lines, or turn it off or on",
	about: `Sets the lines given and leaves the rest. 0 turns a rule off. --default
puts lines back to the usual (or default) ones: churn_usd, session_usd,
hours, idle_hours, model_usd, models, burst_usd, pace, or all. --models
names the models automation may use (id prefixes); without it, the ones
it usually uses.`,
	examples: []string{"wp usage guard set --churn 40 --session 150", "wp usage guard set --off", "wp usage guard set --default all", "wp usage guard set --pace off"},
	run:      cmdUsageGuardSet,
}

var usageGuardLiftCmd = &command{
	name: "lift", args: "[--for 2h]",
	summary:  "end the quota guard's hold on automation, and keep a new one off a while",
	examples: []string{"wp usage guard lift", "wp usage guard lift --for 6h"},
	run:      cmdUsageGuardLift,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageGuardCmd)
}

type guardReport struct {
	Who    string        `json:"who"`
	Days   int           `json:"days"`
	Checks []*guardCheck `json:"checks"`
	Now    *guardCheck   `json:"now"`
}

type guardCheck struct {
	From        time.Time  `json:"from"`
	To          time.Time  `json:"to"`
	ResetsAt    *time.Time `json:"resets_at"`
	WeekPercent *float64   `json:"week_percent"`
	WeekEven    *float64   `json:"week_even"`
	Resets      []struct {
		At         time.Time `json:"at"`
		WasPercent float64   `json:"was_percent"`
	} `json:"resets"`
	ResetNote     string             `json:"reset_note,omitempty"`
	Points        float64            `json:"points"`
	Unexplained   float64            `json:"unexplained"`
	Automation    float64            `json:"automation"`
	Person        float64            `json:"person"`
	ByPhase       map[string]float64 `json:"by_phase"`
	CostUSD       float64            `json:"cost_usd"`
	AutomationUSD float64            `json:"automation_usd"`
	Limits        struct {
		WeekAhead    float64    `json:"week_ahead"`
		WeekMax      float64    `json:"week_max"`
		WeekEndHours float64    `json:"week_end_hours"`
		SpendHours   float64    `json:"spend_hours"`
		OverMax      float64    `json:"over_max"`
		OffUntil     *time.Time `json:"off_until"`
	} `json:"limits"`
	EarlySpend []struct {
		At        time.Time `json:"at"`
		Issue     string    `json:"issue"`
		HoursLeft float64   `json:"hours_left"`
	} `json:"early_spend"`
	Sources []struct {
		Kind    string  `json:"kind"`
		ID      int64   `json:"id"`
		Title   string  `json:"title"`
		Points  float64 `json:"points"`
		Over    float64 `json:"over"`
		CostUSD float64 `json:"cost_usd"`
	} `json:"sources"`
	Hours []struct {
		Hour          time.Time   `json:"hour"`
		Phase         string      `json:"phase"`
		Percent       *float64    `json:"percent"`
		Even          *float64    `json:"even"`
		Points        float64     `json:"points"`
		Automation    float64     `json:"automation"`
		CostUSD       float64     `json:"cost_usd"`
		AutomationUSD float64     `json:"automation_usd"`
		Split         []guardPart `json:"split"`
	} `json:"hours"`
	Breach  bool     `json:"breach"`
	Reasons []string `json:"reasons"`
	// Path: "stored" (the daily check as kept) or "now" (computed for this request).
	Path  string      `json:"path"`
	Split *guardSplit `json:"split"`
}

// guardSplit is the server's QuotaGuardSplit: the check's dollars by source.
type guardSplit struct {
	Path         string      `json:"path"`
	Parts        []guardPart `json:"parts"`
	PartsUSD     float64     `json:"parts_usd"`
	RemainderUSD float64     `json:"remainder_usd"`
}

type guardPart struct {
	Source  string  `json:"source"` // cli or runner
	Machine string  `json:"machine"`
	CostUSD float64 `json:"cost_usd"`
}

func cmdUsageGuard(c *cli, args []string) error {
	fs := c.flags()
	days := fs.Int("days", 14, "`N` days of checks")
	now := fs.Bool("now", false, "also check the last 24 hours now (not stored)")
	hours := fs.Bool("hours", false, "list each hour that spent anything")
	user := fs.String("user", "", "admins: another person's (`NAME`)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{}
	q.Set("days", strconv.Itoa(*days))
	if *now {
		q.Set("now", "1")
	}
	setIf(q, "user", *user)
	raw, err := c.call("GET", "/api/v1/usage/guard?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	lq := url.Values{}
	lq.Set("days", "7")
	setIf(lq, "user", *user)
	live, err := c.call("GET", "/api/v1/usage/guard/live?"+lq.Encode(), nil)
	var ae *apiError
	if err != nil && (!errors.As(err, &ae) || ae.status != http.StatusNotFound) {
		return err
	}
	if len(live) == 0 {
		live = []byte("null")
	}
	both := guardBothJSON(live, raw, machineName())
	return c.emit(both, func() string {
		var r guardReport
		if json.Unmarshal(raw, &r) != nil {
			return string(raw)
		}
		out := guardReportText(&r, *hours, machineName())
		var l guardLiveReport
		if json.Unmarshal(live, &l) == nil && l.Policy != nil {
			out = guardLiveText(&l, time.Now()) + "\nDaily checks:\n" + out
		}
		return out
	})
}

var guardPhaseOrder = []string{"under", "ahead", "end", "over", "off", "unknown"}

// guardBothJSON is the --json output: the live and daily reports as they came
// and this machine's name as a JSON string.
func guardBothJSON(live, daily []byte, machine string) []byte {
	name, _ := json.Marshal(machine)
	return []byte(`{"live":` + string(live) + `,"daily":` + string(daily) + `,"this_machine":` + string(name) + `}`)
}

func guardReportText(r *guardReport, hours bool, machine string) string {
	var b strings.Builder
	breaches := 0
	for _, c := range r.Checks {
		if c.Breach {
			breaches++
		}
	}
	fmt.Fprintf(&b, "Quota guard (%s), last %d days: %d %s, %d %s.\n", r.Who, r.Days, len(r.Checks), pluralWord(len(r.Checks), "check", "checks"),
		breaches, pluralWord(breaches, "breach", "breaches"))
	if r.Now != nil {
		b.WriteString("\nNow (not stored):\n")
		guardCheckText(&b, r.Now, hours, machine)
	}
	for i, c := range r.Checks {
		b.WriteString("\n")
		guardCheckText(&b, c, hours, machine)
		if i == 0 && r.Now == nil {
			guardLimitsText(&b, c)
		}
	}
	if r.Now != nil {
		guardLimitsText(&b, r.Now)
	}
	return b.String()
}

func guardCheckText(b *strings.Builder, c *guardCheck, hours bool, machine string) {
	fmt.Fprintf(b, "%s to %s UTC", c.From.UTC().Format("Jan 2 15:04"), c.To.UTC().Format("Jan 2 15:04"))
	if c.WeekPercent != nil && c.WeekEven != nil && c.ResetsAt != nil {
		fmt.Fprintf(b, ": week at %.0f%% (even %.0f%%), resets %s UTC", *c.WeekPercent, *c.WeekEven, c.ResetsAt.UTC().Format("Jan 2 15:04"))
	}
	b.WriteString("\n")
	for _, r := range c.Resets {
		fmt.Fprintf(b, "  allowance reset at %s UTC (was %.0f%%); the even pace runs from there\n", r.At.UTC().Format("Jan 2 15:04"), r.WasPercent)
	}
	if c.ResetNote != "" {
		fmt.Fprintf(b, "  note: %s\n", c.ResetNote)
	}
	fmt.Fprintf(b, "  points %.1f: automation %.1f, person %.1f, nobody's %.1f\n", c.Points, c.Automation, c.Person, c.Unexplained)
	if c.Automation > 0 {
		var parts []string
		for _, p := range guardPhaseOrder {
			if v := c.ByPhase[p]; v > 0 {
				parts = append(parts, fmt.Sprintf("%s %.1f", p, v))
			}
		}
		fmt.Fprintf(b, "  automation by phase: %s\n", strings.Join(parts, " · "))
	}
	fmt.Fprintf(b, "  list price: $%.2f, automation $%.2f", c.CostUSD, c.AutomationUSD)
	if c.Path != "" {
		fmt.Fprintf(b, " (%s)", guardPathText(c.Path))
	}
	b.WriteString("\n")
	guardSplitText(b, c, machine)
	for _, r := range c.Reasons {
		fmt.Fprintf(b, "  BREACH: %s\n", r)
	}
	for _, s := range c.Sources {
		over := ""
		if s.Over > 0 {
			over = fmt.Sprintf(" (%.1f over)", s.Over)
		}
		fmt.Fprintf(b, "  %s %d %s: %.1f points%s, $%.2f\n", s.Kind, s.ID, s.Title, s.Points, over, s.CostUSD)
	}
	if !hours {
		return
	}
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  hour\tphase\tweek\teven\tpoints\tautomation\t$\tautomation $\tby source")
	for _, h := range c.Hours {
		if h.Points == 0 && h.CostUSD == 0 {
			continue
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%.1f\t%.1f\t%.2f\t%.2f\t%s\n", h.Hour.UTC().Format("Jan 2 15:04"), h.Phase, guardPct(h.Percent), guardPct(h.Even),
			h.Points, h.Automation, h.CostUSD, h.AutomationUSD, guardPartsInline(h.Split))
	}
	_ = tw.Flush()
}

func guardLimitsText(b *strings.Builder, c *guardCheck) {
	l := c.Limits
	fmt.Fprintf(b, "\nLimits checked against: week_ahead %s, week_max %s, week_end_hours %s; use what's left in the last %.0fh; a breach from %.0f points.\n",
		guardLimit(l.WeekAhead, "+%.0f"), guardLimit(l.WeekMax, "%.0f%%"), guardLimit(l.WeekEndHours, "%.0fh"), l.SpendHours, l.OverMax)
	if l.OffUntil != nil {
		fmt.Fprintf(b, "Quota limits off until %s UTC.\n", l.OffUntil.UTC().Format("Jan 2 15:04"))
	}
}

func guardLimit(v float64, format string) string {
	if v == 0 {
		return "off"
	}
	return fmt.Sprintf(format, v)
}

func guardPct(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

func guardPathText(path string) string {
	switch path {
	case "stored":
		return "the stored daily check, as kept when it ran"
	case "now":
		return "computed now from the usage hours, not stored"
	}
	return path
}

func guardPartName(p guardPart) string {
	switch {
	case p.Source == "runner":
		return "runner jobs"
	case p.Machine == "":
		return "cli, machine unknown"
	}
	return "cli, " + p.Machine
}

func guardPartsInline(ps []guardPart) string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s $%.2f", guardPartName(p), p.CostUSD))
	}
	return strings.Join(out, " · ")
}

// guardSplitText prints where a check's dollars came from, a remainder line
// when the parts do not add up to its list price, and what the server has
// for this machine, to set beside `wp usage where` for the same window.
func guardSplitText(b *strings.Builder, c *guardCheck, machine string) {
	sp := c.Split
	if sp == nil {
		return
	}
	fmt.Fprintf(b, "  by source, recomputed now from the usage hours and runner jobs: $%.2f\n", sp.PartsUSD)
	for _, p := range sp.Parts {
		fmt.Fprintf(b, "    %s: $%.2f\n", guardPartName(p), p.CostUSD)
	}
	if sp.RemainderUSD != 0 {
		fmt.Fprintf(b, "    remainder: $%.2f (the list price above less these parts: usage or prices changed since the check was stored)\n", sp.RemainderUSD)
	}
	mine := 0.0
	for _, p := range sp.Parts {
		if p.Source == "cli" && p.Machine == machine {
			mine += p.CostUSD
		}
	}
	fmt.Fprintf(b, "  this machine (%s): $%.2f on the server; compare `wp usage where --since %s --until %s` (its dollars are this machine's transcripts only)\n",
		machine, mine, c.From.UTC().Format("2006-01-02T15:04:05Z"), c.To.UTC().Format("2006-01-02T15:04:05Z"))
}
