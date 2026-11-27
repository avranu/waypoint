package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
)

var usageStalledCmd = &command{
	name: "stalled", args: "[--days 28] [--user NAME]",
	summary: "quiet background jobs: how their sessions were told and how the spells ended",
	about: `Each quiet spell of an agent's background job a runner reported, by the kind
of command (class: its program, with the subcommand for git, go, npm, wp
and the like; loop for a shell loop). For each: how many spells, how many
its session was told of, and how: woke (its prompt cache warm), at_stop
(read when its turn ended), passive (read at its next turn), keep_warm (in
place of a keep-warm ping) and cold_wake (its cache cold, woken after the
cold-wake time). How they ended: acted (the job ended within 15 minutes of
its session reading the notice), self (it moved again or ended without
that), withdrawn (it moved again before its session read the notice,
which was taken back unsent), unknown (the runner stopped reporting), open.
quiet: the median minutes quiet of the spells that ended by themselves.
now: the quiet time its jobs get under the policy for every project.

The quiet time is the shortest of 1, 1.5, 2, 3 and 4 times the setting at
which the share of spells quiet that long that were stuck reaches
precision_needs; with fewer than min_sample spells the setting stands.
Cold sessions are woken while at least cold_needs of cold wakes were acted
on. Set the times under Admin → AI, or a project's under its settings.`,
	examples: []string{"wp usage stalled", "wp usage stalled --days 7 --user all"},
	run:      cmdUsageStalled,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageStalledCmd)
}

type stallPolicyJSON struct {
	Off         bool `json:"off"`
	MinAge      int  `json:"min_age"`
	Quiet       int  `json:"quiet"`
	SkipTimeout int  `json:"skip_timeout"`
	Early       int  `json:"early"`
	ColdWake    int  `json:"cold_wake"`
	Fixed       bool `json:"fixed"`
}

type stalledUsageReport struct {
	Who       string               `json:"who"`
	Days      int                  `json:"days"`
	Policy    stallPolicyJSON      `json:"policy"`
	MinSample int                  `json:"min_sample"`
	Needs     float64              `json:"precision_needs"`
	ColdNeeds float64              `json:"cold_needs"`
	Classes   []stalledClassFigure `json:"classes"`
	ColdWakes int                  `json:"cold_wakes"`
	ColdActed int                  `json:"cold_acted"`
	ColdWake  *bool                `json:"cold_wake"`
	ColdWhy   string               `json:"cold_why"`
}

type stalledClassFigure struct {
	Class       string `json:"class"`
	Spells      int    `json:"spells"`
	Told        int    `json:"told"`
	Woke        int    `json:"woke"`
	AtStop      int    `json:"at_stop"`
	Passive     int    `json:"passive"`
	KeepWarm    int    `json:"keep_warm"`
	ColdWake    int    `json:"cold_wake"`
	Acted       int    `json:"acted"`
	Self        int    `json:"self"`
	Withdrawn   int    `json:"withdrawn"`
	Unknown     int    `json:"unknown"`
	Open        int    `json:"open"`
	MedianQuiet int    `json:"median_quiet"`
	QuietNow    int    `json:"quiet_now"`
}

func cmdUsageStalled(c *cli, args []string) error {
	fs := c.flags()
	days := fs.Int("days", 28, "`N` days back")
	user := fs.String("user", "", "admins: another person's (`NAME`, or all)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{}
	q.Set("days", strconv.Itoa(*days))
	setIf(q, "user", *user)
	raw, err := c.call("GET", "/api/v1/usage/stalled?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r stalledUsageReport
		if json.Unmarshal(raw, &r) != nil {
			return string(raw)
		}
		return stalledReportText(&r)
	})
}

func stalledReportText(r *stalledUsageReport) string {
	var b strings.Builder
	n := 0
	for _, c := range r.Classes {
		n += c.Spells
	}
	p := r.Policy
	fmt.Fprintf(&b, "Quiet background-job spells, last %d days (%s): %d.\n", r.Days, r.Who, n)
	if p.Off {
		b.WriteString("For every project: off (sessions aren't told; spells are still kept).\n")
	} else {
		fmt.Fprintf(&b, "For every project: jobs %d min old, %d min without progress, timeouts under %d min skipped, early from %d min at a keep-warm ping, cold sessions woken after %d min (0: never)%s.\n",
			p.MinAge, p.Quiet, p.SkipTimeout, p.Early, p.ColdWake, map[bool]string{true: ", times fixed"}[p.Fixed])
	}
	fmt.Fprintf(&b, "A kind's quiet time lengthens below %s stuck; cold wakes stop below %s acted on; under %d spells the setting stands.\n",
		warmPct(r.Needs), warmPct(r.ColdNeeds), r.MinSample)
	fmt.Fprintf(&b, "Cold wakes: %s acted on", warmRatio(r.ColdActed, r.ColdWakes))
	switch {
	case r.ColdWake == nil:
	case *r.ColdWake:
		b.WriteString("; cold sessions are woken")
	default:
		fmt.Fprintf(&b, "; cold sessions aren't woken now (%s)", r.ColdWhy)
	}
	b.WriteString(".\n")
	if n == 0 {
		return b.String()
	}
	b.WriteString("\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "class\tspells\ttold\twoke\tat_stop\tpassive\tkeep_warm\tcold_wake\tacted\tself\twithdrawn\tunknown\topen\tquiet\tnow")
	for _, c := range r.Classes {
		class := c.Class
		if class == "" {
			class = "-"
		}
		now := "-"
		if c.QuietNow > 0 {
			now = fmt.Sprintf("%d min", c.QuietNow)
		}
		quiet := "-"
		if c.Self+c.Withdrawn > 0 {
			quiet = fmt.Sprintf("%d min", c.MedianQuiet)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n", class, c.Spells, c.Told, c.Woke, c.AtStop, c.Passive, c.KeepWarm, c.ColdWake,
			c.Acted, c.Self, c.Withdrawn, c.Unknown, c.Open, quiet, now)
	}
	_ = tw.Flush()
	return b.String()
}
