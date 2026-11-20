package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// wp usage risky: the risky commands wp usage sync found (WP-647).

var usageRiskyCmd = &command{
	name: "risky", args: "[--days 7] [--high] [--kind KIND] [--refused] [--limit 30] [--user NAME]",
	summary: "risky shell commands agents ran: deletes, force pushes, production",
	about: `Lists, newest first, the shell commands agents ran that delete, discard or
overwrite what can't be had back, or reach production, another machine, a
cluster or a disk, as wp usage sync found them in Claude Code's
transcripts: what it did, whether it ran, the task its session held, and
the command, redacted, on one line of up to 300 characters (--json has it
whole). high: production, other machines, shared
history, deletes outside any checkout; medium: local work or state a
person might want back. Kinds: delete, discard, force_push, push_main,
branch_delete, history, sql, cluster, deploy, service, docker, disk, power,
permissions, remote_script. Also under Agents → Audit → Risky commands.`,
	examples: []string{"wp usage risky --high", "wp usage risky --days 30 --kind force_push"},
	run:      cmdUsageRisky,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageRiskyCmd)
}

type riskyListRow struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Level   string    `json:"level"`
	Label   string    `json:"label"`
	Command string    `json:"command"`
	Segment string    `json:"segment"`
	Outcome string    `json:"outcome"`
	Source  string    `json:"source"`
	Repo    string    `json:"repo"`
	Machine string    `json:"machine"`
	Issue   *struct {
		Key string `json:"key"`
	} `json:"issue"`
}

type riskyListPage struct {
	Rows   []riskyListRow `json:"rows"`
	Counts *struct {
		High    int64 `json:"high"`
		Medium  int64 `json:"medium"`
		Refused int64 `json:"refused"`
	} `json:"counts"`
}

func cmdUsageRisky(c *cli, args []string) error {
	fs := c.flags()
	days := fs.Int("days", 7, "`N` days back")
	high := fs.Bool("high", false, "high only")
	kind := fs.String("kind", "", "only this `KIND`")
	refused := fs.Bool("refused", false, "only those refused before they ran")
	limit := fs.Int("limit", 30, "at most `N`")
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
	q.Set("limit", strconv.Itoa(*limit))
	if *high {
		q.Set("level", "high")
	}
	setIf(q, "kind", *kind)
	setIf(q, "user", *user)
	if *refused {
		q.Set("outcome", "refused")
	}
	raw, err := c.call("GET", "/api/v1/usage/risky?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var p riskyListPage
		if json.Unmarshal(raw, &p) != nil {
			return string(raw)
		}
		return riskyListText(&p, *days)
	})
}

func riskyListText(p *riskyListPage, days int) string {
	var b strings.Builder
	if c := p.Counts; c != nil {
		fmt.Fprintf(&b, "Last %d days: %d high, %d medium (%d refused).\n", days, c.High, c.Medium, c.Refused)
	}
	if len(p.Rows) == 0 {
		b.WriteString("None that match.\n")
		return b.String()
	}
	for _, r := range p.Rows {
		where := r.Repo
		if where == "" {
			where = r.Machine
		}
		if r.Issue != nil {
			where += " " + r.Issue.Key
		}
		outcome := r.Outcome
		if r.Source != "" {
			outcome += " by " + r.Source
		}
		at := r.At
		fmt.Fprintf(&b, "%-6s %s, %s (%s, %s): %s\n", r.Level, r.Label, outcome, where, ago(&at), excerpt(r.Command, riskyLineRunes))
		if r.Segment != "" {
			fmt.Fprintf(&b, "       …%s\n", excerpt(r.Segment, riskyLineRunes))
		}
	}
	return b.String()
}
