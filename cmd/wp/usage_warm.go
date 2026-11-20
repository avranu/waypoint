package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
)

// wp usage warm: keep-warm decisions and what became of the sessions
// (WP-1000, WP-ADR95).

var usageWarmCmd = &command{
	name: "warm", args: "[--days 28] [--user NAME]",
	summary: "keep-warm pings and compactions, and whether the sessions were used again",
	about: `Each decision Waypoint made a few minutes before a session's prompt cache
would cool, by the kind of wait it named (class): work (its own background
work or babysits), requests (open requests, its person active), chat (its
person was working with it), unattended (autopilot's), finished,
clearable, unmerged (its task not merged yet: compacted from 60k), and the *_cap and requests_away variants that cool. For each:
how many were pinged, compacted, let cool or sent batched news (changed:
how many the evidence turned from the rule's action), and what became of
the sessions: used within the hour a ping buys, used again at all, or
cleared, ended or still open first.

A ping pays when the class's share used within the hour reaches
ping_needs; a compaction when its share used again is enough for the
session's context ("now" names the context from which it pays). With fewer
than the minimum sample of decisions the rules alone decide. The same
figures decide each ping and compaction.`,
	examples: []string{"wp usage warm", "wp usage warm --days 7 --user all"},
	run:      cmdUsageWarm,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageWarmCmd)
}

type warmReport struct {
	Who          string            `json:"who"`
	Days         int               `json:"days"`
	Model        string            `json:"model"`
	PingNeeds    float64           `json:"ping_needs"`
	CompactKeeps int               `json:"compact_keeps"`
	CompactOut   int               `json:"compact_out"`
	MinSample    int               `json:"min_sample"`
	Classes      []warmClassFigure `json:"classes"`
}

type warmClassFigure struct {
	Class       string `json:"class"`
	Decisions   int    `json:"decisions"`
	Pings       int    `json:"pings"`
	Compacts    int    `json:"compactions"`
	Cools       int    `json:"cools"`
	News        int    `json:"news"`
	Changed     int    `json:"changed"`
	Soon        int    `json:"used_within_hour"`
	SoonOf      int    `json:"used_within_hour_of"`
	Later       int    `json:"used_again"`
	LaterOf     int    `json:"used_again_of"`
	Cleared     int    `json:"cleared"`
	Ended       int    `json:"ended"`
	Open        int    `json:"open"`
	PingPays    *bool  `json:"ping_pays"`
	CompactFrom int64  `json:"compact_from"`
}

func cmdUsageWarm(c *cli, args []string) error {
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
	raw, err := c.call("GET", "/api/v1/usage/warm?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r warmReport
		if json.Unmarshal(raw, &r) != nil {
			return string(raw)
		}
		return warmReportText(&r)
	})
}

func warmReportText(r *warmReport) string {
	var b strings.Builder
	n := 0
	for _, c := range r.Classes {
		n += c.Decisions
	}
	fmt.Fprintf(&b, "Keep-warm decisions, last %d days (%s): %d.\n", r.Days, r.Who, n)
	if n == 0 {
		return b.String()
	}
	model := r.Model
	if model == "" {
		model = "default"
	}
	fmt.Fprintf(&b, "At %s prices a ping pays from %s used within the hour; a compaction leaves ~%dk and writes ~%dk. Under %d decisions a class keeps the rules.\n\n",
		model, warmPct(r.PingNeeds), r.CompactKeeps/1000, r.CompactOut/1000, r.MinSample)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "class\tdecided\tping\tcompact\tcool\tnews\tchanged\tused ≤1h\tused again\tcleared\tended\topen\tnow")
	for _, c := range r.Classes {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%d\t%d\t%d\t%s\n", c.Class, c.Decisions, c.Pings, c.Compacts, c.Cools, c.News, c.Changed,
			warmRatio(c.Soon, c.SoonOf), warmRatio(c.Later, c.LaterOf), c.Cleared, c.Ended, c.Open, warmNow(c))
	}
	_ = tw.Flush()
	return b.String()
}

func warmRatio(n, of int) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d %s", n, of, warmPct(float64(n)/float64(of)))
}

func warmPct(x float64) string {
	if x < 0.1 {
		return fmt.Sprintf("%.1f%%", x*100)
	}
	return fmt.Sprintf("%.0f%%", x*100)
}

// warmNow is what the evidence makes of a class's ping and compaction.
func warmNow(c warmClassFigure) string {
	var parts []string
	switch {
	case c.PingPays == nil:
		parts = append(parts, "ping: rules")
	case *c.PingPays:
		parts = append(parts, "ping pays")
	default:
		parts = append(parts, "no ping")
	}
	switch {
	case c.CompactFrom == 0:
		parts = append(parts, "compact: rules")
	case c.CompactFrom < 0:
		parts = append(parts, "no compaction")
	default:
		parts = append(parts, fmt.Sprintf("compact from %dk", c.CompactFrom/1000))
	}
	return strings.Join(parts, ", ")
}
