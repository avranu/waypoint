package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// wp worklog (WP-285): a task's work log, a note on it, and what
// adversarial reviews found across a project.

var worklogCmd = &command{
	name: "worklog", group: "Issues", args: `[KEY] [-m "TEXT" [--tag found|fixed|rejected]] | [KEY] --review -f FILE|-m "REPORT" | stats [-p PROJECT] [--days N] | triage [-p PROJECT] [--days N] [--rows] | stakes [-p PROJECT] [--days N] [--rows]`,
	summary: "a task's work log: review rounds, findings; add a note",
	about: `Oldest first, recorded automatically. -m adds a note; --tag counts it as
an issue found, fixed or rejected. KEY defaults to your current task.
--review attaches the report of an adversarial review you ran yourself
(-f FILE, - for stdin, or -m): Waypoint counts its findings and wp ship
takes it as the task's review round, asking you for its findings (wp
audit report) before the pull request opens. An adversarial-review
subagent's report is attached on its own when it finishes.
stats sums review rounds over the last --days (30), for -p or all
projects. stakes sorts them by stakes tier (high, elevated, normal):
what their audits and Claude reviews did, cost and found (--days 14).`,
	examples: []string{"wp worklog WP-12", `wp worklog WP-12 -m "Missed the nil map" --tag found`, "wp worklog WP-12 --review -f review.md", "wp worklog stats -p WP --days 90"},
	run:      cmdWorklog,
}

func cmdWorklog(c *cli, args []string) error {
	if len(args) > 0 && args[0] == "stats" {
		return cmdWorklogStats(c, args[1:])
	}
	if len(args) > 0 && args[0] == "triage" {
		return cmdWorklogTriage(c, args[1:])
	}
	if len(args) > 0 && args[0] == "stakes" {
		return cmdWorklogStakes(c, args[1:])
	}
	fs := c.flags()
	msg := fs.String("message", "", "add a note (`TEXT`)")
	short(fs, "m", "message")
	tag := fs.String("tag", "", "count the note as `found|fixed|rejected`")
	review := fs.Bool("review", false, "attach the report of an adversarial review you ran (WP-1089)")
	file := fs.String("file", "", "read the note or report from `FILE` (- for stdin)")
	short(fs, "f", "file")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	if *file != "" {
		if *msg != "" {
			return c.usage("give -m or -f, not both")
		}
		body, err := readBody(*file)
		if err != nil {
			return err
		}
		if *msg = strings.TrimSpace(body); *msg == "" {
			return c.usage("%s is empty", *file)
		}
	}
	if *msg == "" && len(rest) > 0 && (*tag != "" || *review) {
		*msg = joinArgs(rest) // wp worklog KEY --tag found "text"
		rest = nil
	}
	if len(rest) > 0 {
		return c.usage("unexpected %q (a note goes after -m)", joinArgs(rest))
	}
	if *tag != "" && *msg == "" {
		return c.usage("--tag needs a note (-m TEXT)")
	}
	if *review && *msg == "" {
		return c.usage("--review needs the review's report (-f FILE or -m TEXT)")
	}
	if *review && *tag != "" {
		return c.usage("Waypoint counts a review's findings itself: leave out --tag")
	}
	if *msg != "" {
		raw, err := c.call("POST", "/api/v1/issues/"+esc(key)+"/worklog", map[string]any{"body": *msg, "tag": *tag, "review": *review})
		if err != nil {
			return err
		}
		if *review {
			return c.emit(raw, func() string {
				return "Attached the review to " + key + "'s work log. wp ship takes it as the task's review round once you report what it found: wp audit report " + key + " -f FILE (or --none -m \"what it covered\")"
			})
		}
		return c.emit(raw, func() string { return "Added a note to " + key + "'s work log" })
	}
	raw, err := c.call("GET", "/api/v1/issues/"+esc(key)+"/worklog", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var l workLog
		if err := json.Unmarshal(raw, &l); err != nil {
			return string(raw)
		}
		return l.text()
	})
}

type workLog struct {
	Issue struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	} `json:"issue"`
	Summary struct {
		Reviews, Counted, Found, Fixed, Rejected, Pushed, Uncounted int
		PRFindings                                                  int `json:"pr_findings"`
		PRFixed                                                     int `json:"pr_fixed"`
		PRRejected                                                  int `json:"pr_rejected"`
		PROpen                                                      int `json:"pr_open"`
		Notes                                                       int
		OwnReviews                                                  int `json:"own_reviews"`
		OwnFound                                                    int `json:"own_found"`
	} `json:"summary"`
	Entries []struct {
		Kind, State, Tag, Title, Body, Agent, By, Reason, Severity, Path string
		Line                                                             int
		At                                                               time.Time
		Author                                                           *struct {
			Username string `json:"username"`
		}
		PR *struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
		}
		Found, Fixed, Rejected *int
		Items                  []struct{ Claim, File, Outcome string }
		Pushed                 bool
		ReadState              string `json:"read_state"`
		Audit                  int64
		AuditAlt               string `json:"audit_alt"`
		AuditNote              string `json:"audit_note"`
		History                []struct {
			At              time.Time
			State, By, Note string
		}
	} `json:"entries"`
}

func (l *workLog) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", l.Issue.Key, l.Issue.Title)
	s := l.Summary
	var parts []string
	if s.Reviews > 0 {
		p := fmt.Sprintf("%d review round%s", s.Reviews, plural(s.Reviews))
		if s.Counted > 0 || s.Found > 0 {
			p += fmt.Sprintf(": %d found, %d fixed, %d rejected", s.Found, s.Fixed, s.Rejected)
		}
		if s.Pushed > 0 {
			p += fmt.Sprintf(", %d pushed fixes", s.Pushed)
		}
		parts = append(parts, p)
	} else if s.Found > 0 { // only tagged notes
		parts = append(parts, fmt.Sprintf("%d found, %d fixed, %d rejected", s.Found, s.Fixed, s.Rejected))
	}
	if s.OwnReviews > 0 {
		parts = append(parts, fmt.Sprintf("%d own review%s: %d found", s.OwnReviews, plural(s.OwnReviews), s.OwnFound))
	}
	if s.PRFindings > 0 {
		parts = append(parts, fmt.Sprintf("PR review: %d (%d fixed, %d rejected, %d open)", s.PRFindings, s.PRFixed, s.PRRejected, s.PROpen))
	}
	if s.Notes > 0 {
		parts = append(parts, fmt.Sprintf("%d note%s", s.Notes, plural(s.Notes)))
	}
	if len(parts) > 0 {
		b.WriteString(strings.Join(parts, " · ") + "\n")
	}
	if len(l.Entries) == 0 {
		b.WriteString("Nothing logged yet.\n")
		return b.String()
	}
	b.WriteString("\n")
	for _, e := range l.Entries {
		who := e.Agent
		if e.Author != nil && who == "" {
			who = e.Author.Username
		}
		if e.By != "" {
			who = e.By
		}
		line := e.At.Local().Format("Jan 2 15:04") + "  " + e.Kind
		if e.State != "" {
			line += " [" + e.State + "]"
		}
		if e.Tag != "" {
			line += " (" + e.Tag + ")"
		}
		line += "  " + e.Title
		if who != "" {
			line += "  — " + who
		}
		b.WriteString(line + "\n")
		if e.Kind == "review" || e.Kind == "own_review" {
			switch {
			case e.Found != nil:
				fmt.Fprintf(&b, "    found %d, fixed %d, rejected %d", *e.Found, deref(e.Fixed), deref(e.Rejected))
				if e.Pushed {
					b.WriteString(", pushed")
				}
				b.WriteString("\n")
			case e.ReadState == "pending":
				b.WriteString("    counting its report\n")
			}
			for _, it := range e.Items {
				where := ""
				if it.File != "" {
					where = " (" + it.File + ")"
				}
				fmt.Fprintf(&b, "    - %s: %s%s\n", it.Outcome, it.Claim, where)
			}
			if e.Reason != "" {
				b.WriteString("    " + e.Reason + "\n")
			}
			switch {
			case e.AuditNote != "":
				b.WriteString("    its own prompt: " + e.AuditNote + "\n")
			case e.Audit != 0 && e.AuditAlt != "":
				fmt.Fprintf(&b, "    another model's findings: wp audit show %d (%s)\n", e.Audit, e.AuditAlt)
			case e.Audit != 0:
				fmt.Fprintf(&b, "    another model's findings: wp audit show %d\n", e.Audit)
			}
		}
		if e.Path != "" {
			where := e.Path
			if e.Line > 0 {
				where += ":" + strconv.Itoa(e.Line)
			}
			b.WriteString("    " + where + "\n")
		}
		for _, h := range e.History {
			line := "    " + h.At.Local().Format("Jan 2 15:04") + " " + h.State
			if h.By != "" {
				line += " by " + h.By
			}
			if h.Note != "" {
				line += ": " + h.Note
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func cmdWorklogStats(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "only `PROJECT`")
	short(fs, "p", "project")
	days := fs.Int("days", 30, "over the last `N` days (up to 365)")
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
	raw, err := c.call("GET", "/api/v1/worklog/stats?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var st struct {
			Days     int `json:"days"`
			Projects []struct {
				Key string `json:"key"`
				workLogTotals
			} `json:"projects"`
			Total    workLogTotals  `json:"total"`
			Evidence []evidenceWeek `json:"evidence"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return string(raw)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Adversarial review rounds, last %d days\n", st.Days)
		if st.Total.Rounds == 0 {
			b.WriteString("None yet: turn them on with wp autopilot set --review on.\n")
			writeEvidenceWeeks(&b, st.Evidence)
			return b.String()
		}
		fmt.Fprintf(&b, "%-8s %6s %7s %10s %6s %9s %6s %8s %6s\n", "PROJECT", "ROUNDS", "COUNTED", "FOUND ANY", "FOUND", "PER ROUND", "FIXED", "REJECTED", "PUSHED")
		row := func(name string, t workLogTotals) {
			fmt.Fprintf(&b, "%-8s %6d %7d %10d %6d %9.1f %6d %8d %6d\n", name, t.Rounds, t.Counted, t.WithIssues, t.Found, t.FoundPerRound, t.Fixed, t.Rejected, t.Pushed)
		}
		for _, p := range st.Projects {
			row(p.Key, p.workLogTotals)
		}
		if len(st.Projects) > 1 {
			row("all", st.Total)
		}
		writeEvidenceWeeks(&b, st.Evidence)
		return b.String()
	})
}

type evidenceWeek struct {
	Week       string `json:"week"`
	Ships      int    `json:"ships"`
	OnTree     int    `json:"on_tree"`
	Mismatches int    `json:"mismatches"`
}

// writeEvidenceWeeks is the weekly table of test evidence and claims.
func writeEvidenceWeeks(b *strings.Builder, weeks []evidenceWeek) {
	if len(weeks) == 0 {
		return
	}
	b.WriteString("\nShips with test evidence, per week\n")
	fmt.Fprintf(b, "%-10s %6s %8s %11s\n", "WEEK OF", "SHIPS", "ON TREE", "MISMATCHES")
	for _, w := range weeks {
		fmt.Fprintf(b, "%-10s %6d %8d %11d\n", w.Week, w.Ships, w.OnTree, w.Mismatches)
	}
}

type workLogTotals struct {
	Rounds        int     `json:"rounds"`
	Counted       int     `json:"counted"`
	WithIssues    int     `json:"with_issues"`
	Found         int     `json:"found"`
	Fixed         int     `json:"fixed"`
	Rejected      int     `json:"rejected"`
	Pushed        int     `json:"pushed"`
	FoundPerRound float64 `json:"found_per_round"`
}
