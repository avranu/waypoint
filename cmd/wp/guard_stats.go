package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var guardLogCmd = &command{
	name: "guard", group: "Setup", args: "[stats] [FLAGS]",
	summary: "summarize local claude-guard denials from guard.log",
	about: `Reads the local guard.log (and rotated guard.log.1) in wp's config directory
and reports denials and warnings by rule, reason, and top matched patterns.
Nothing is sent to the server.`,
	examples: []string{"wp guard", "wp guard stats --days 7", "wp guard stats --days 0 --json"},
	run:      cmdGuardStats,
}

var guardStatsCmd = &command{
	name: "stats", args: "[--days N] [--limit N]",
	summary:  "summarize local guard.log denials by rule and reason",
	about:    guardLogCmd.about,
	examples: []string{"wp guard stats", "wp guard stats --days 30", "wp guard stats --days 0 --json"},
	run:      cmdGuardStats,
}

func init() {
	guardLogCmd.subs = append(guardLogCmd.subs, guardStatsCmd)
}

type guardLogRow struct {
	Time    string `json:"time"`
	Session string `json:"session_id"`
	Tool    string `json:"tool"`
	Mode    string `json:"mode"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason"`
	Matched string `json:"matched"`
	What    string `json:"what"`
	Cwd     string `json:"cwd"`
	Task    string `json:"task"`
}

type guardCount struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

type guardReasonCount struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type guardStatsReport struct {
	Path       string             `json:"path"`
	Days       int                `json:"days"`
	Total      int                `json:"total"`
	Malformed  int                `json:"malformed,omitempty"`
	ByMode     []guardCount       `json:"by_mode"`
	ByRule     []guardCount       `json:"by_rule"`
	TopReasons []guardReasonCount `json:"top_reasons"`
	TopMatched []guardCount       `json:"top_matched"`
}

func cmdGuardStats(c *cli, args []string) error {
	fs := c.flags()
	days := fs.Int("days", 7, "look back `N` days (0 for all entries)")
	limit := fs.Int("limit", 12, "max top reasons/patterns to show")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	if *days < 0 {
		return c.usage("--days must be >= 0")
	}
	if *limit < 1 {
		return c.usage("--limit must be >= 1")
	}
	rep, err := readGuardStats(c.dir, *days, *limit, time.Now().UTC())
	if err != nil {
		return err
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return guardStatsText(rep) })
}

func readGuardStats(dir string, days, limit int, now time.Time) (*guardStatsReport, error) {
	if limit <= 0 {
		limit = 12
	}
	path := filepath.Join(dir, "guard.log")
	rep := &guardStatsReport{
		Path:       path,
		Days:       days,
		ByMode:     []guardCount{},
		ByRule:     []guardCount{},
		TopReasons: []guardReasonCount{},
		TopMatched: []guardCount{},
	}
	var cutoff time.Time
	if days > 0 {
		cutoff = now.AddDate(0, 0, -days)
	}
	modes := map[string]int{}
	rules := map[string]int{}
	reasons := map[[2]string]int{}
	matched := map[string]int{}
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p) // #nosec G304 -- wp's own state directory
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 2<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var row guardLogRow
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				rep.Malformed++
				continue
			}
			if !cutoff.IsZero() {
				ts, err := time.Parse(time.RFC3339, row.Time)
				if err != nil || ts.Before(cutoff) {
					continue
				}
			}
			rep.Total++
			rule := cmpStr(row.Rule, "(unknown)")
			modes[cmpStr(row.Mode, "(unknown)")]++
			rules[rule]++
			if row.Reason != "" {
				reasons[[2]string{rule, row.Reason}]++
			}
			if m := strings.TrimSpace(row.Matched); m != "" {
				if r := []rune(m); len(r) > 80 {
					m = string(r[:77]) + "..."
				}
				matched[m]++
			}
		}
		err = sc.Err()
		_ = f.Close()
		if err != nil {
			return nil, err
		}
	}
	rep.ByMode = sortGuardCounts(modes, 0)
	rep.ByRule = sortGuardCounts(rules, 0)
	rep.TopMatched = sortGuardCounts(matched, limit)
	for k, n := range reasons {
		rep.TopReasons = append(rep.TopReasons, guardReasonCount{Rule: k[0], Reason: k[1], Count: n})
	}
	slices.SortFunc(rep.TopReasons, func(a, b guardReasonCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		if c := strings.Compare(a.Rule, b.Rule); c != 0 {
			return c
		}
		return strings.Compare(a.Reason, b.Reason)
	})
	if len(rep.TopReasons) > limit {
		rep.TopReasons = rep.TopReasons[:limit]
	}
	return rep, nil
}

func sortGuardCounts(m map[string]int, limit int) []guardCount {
	out := make([]guardCount, 0, len(m))
	for k, n := range m {
		out = append(out, guardCount{Key: k, Count: n})
	}
	slices.SortFunc(out, func(a, b guardCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Key, b.Key)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func guardStatsText(r *guardStatsReport) string {
	var b strings.Builder
	scope := "all entries"
	if r.Days > 0 {
		scope = fmt.Sprintf("last %d days", r.Days)
	}
	if r.Total == 0 {
		fmt.Fprintf(&b, "No guard log entries (%s) in %s.\n", scope, r.Path)
		if r.Malformed > 0 {
			fmt.Fprintf(&b, "  (%d unreadable lines skipped)\n", r.Malformed)
		}
		return b.String()
	}
	var modeParts []string
	for _, m := range r.ByMode {
		modeParts = append(modeParts, fmt.Sprintf("%s %d", m.Key, m.Count))
	}
	fmt.Fprintf(&b, "Guard log (%s, %s): %d entries (%s)\n", scope, r.Path, r.Total, strings.Join(modeParts, ", "))
	if r.Malformed > 0 {
		fmt.Fprintf(&b, "  (%d unreadable lines skipped)\n", r.Malformed)
	}
	if len(r.ByRule) > 0 {
		b.WriteString("\nBy rule:\n")
		for _, x := range r.ByRule {
			fmt.Fprintf(&b, "  %-14s %4d  (%4.1f%%)\n", x.Key, x.Count, 100*float64(x.Count)/float64(r.Total))
		}
	}
	if len(r.TopReasons) > 0 {
		b.WriteString("\nTop reasons:\n")
		for _, x := range r.TopReasons {
			fmt.Fprintf(&b, "  %4d  %-10s  %s\n", x.Count, x.Rule, x.Reason)
		}
	}
	if len(r.TopMatched) > 0 {
		b.WriteString("\nTop matched patterns:\n")
		for _, x := range r.TopMatched {
			fmt.Fprintf(&b, "  %4d  %s\n", x.Count, x.Key)
		}
	}
	return b.String()
}
