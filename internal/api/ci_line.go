package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// ciLine is each merge line, its armed pull requests and what became of its
// speculative runs (WP-1658): ?repo=OWNER/NAME, ?days=N (default 7, at most 90).
func (s *Server) ciLine(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	rep, err := s.svc.CILine(r.Context(), actorFrom(r), core.CILineInput{Repo: q.Get("repo"), Days: queryInt(r, "days", 0)})
	if err != nil || !wantsText(r) {
		return rep, err
	}
	return textResponse(ciLineText(rep)), nil
}

// ciLineText is wp ci line: per line the armed pull requests in order, a row
// per day, the totals, and how the ended rows ended.
func ciLineText(rep *core.CILineReport) string {
	var o strings.Builder
	if len(rep.Lines) == 0 {
		fmt.Fprintf(&o, "No merge line has an armed pull request or a stack or batch in the last %d days.\n", rep.Days)
		return o.String()
	}
	n := func(v int) string {
		if v == 0 {
			return "-"
		}
		return fmt.Sprint(v)
	}
	for i, l := range rep.Lines {
		if i > 0 {
			o.WriteString("\n")
		}
		fmt.Fprintf(&o, "%s %s\n", l.Repo, l.Base)
		if len(l.Armed) == 0 {
			o.WriteString("  no armed pull requests\n")
		}
		for j, p := range l.Armed {
			queued := "not queued"
			if p.QueuedAt != nil {
				queued = "queued " + core.ShortDuration(rep.GeneratedAt.Sub(*p.QueuedAt)) + " ago"
			}
			wait := p.Babysit
			if p.Wait != "" {
				wait += ": " + p.Wait
			}
			fmt.Fprintf(&o, "  %d. #%d %s %s, %s; %s\n", j+1, p.Number, p.HeadSHA, p.State, queued, cmpStr(strings.TrimSpace(wait), "no babysit"))
			if p.Stack != nil {
				kind := "stack"
				if p.Stack.Batch {
					kind = "batch"
				}
				fmt.Fprintf(&o, "       %s %s (%s), %s, %s old\n", kind, p.Stack.Heads, p.Stack.Branch, p.Stack.State, core.ShortDuration(time.Duration(p.Stack.AgeSec)*time.Second))
			}
		}
		o.WriteString("\n")
		const row = "  %-10s %4s %5s %6s | %7s %6s %6s %8s %9s %7s | %6s %8s %5s %9s\n"
		fmt.Fprintf(&o, row, "day", "rows", "runs", "batch", "running", "passed", "merged", "failed", "conflict", "cancel", "merges", "on stack", "full", "full/merge")
		line := func(d core.CILineDay) {
			st := d.States
			full, per := "?", "?"
			if d.FullRuns != nil {
				full = fmt.Sprint(*d.FullRuns)
			}
			if d.FullPerMerge != nil {
				per = fmt.Sprintf("%.1f", *d.FullPerMerge)
			}
			fmt.Fprintf(&o, row, d.Date, n(d.Rows), n(d.Runs), n(d.Batches), n(st["running"]), n(st["passed"]+st["ready"]), n(st["merged"]), n(st["failed"]),
				n(st["conflict"]), n(st["cancelled"]+st["refused"]), n(d.Merges), n(d.MergedOnStack), full, per)
		}
		for _, d := range l.Days {
			line(d)
		}
		line(l.Totals)
		t := l.Totals
		if t.UsedShare != nil {
			fmt.Fprintf(&o, "  %d of %d runs (%.0f%%) were merged on.", t.RunsUsed, t.Runs, 100**t.UsedShare)
		} else {
			o.WriteString("  No run was started.")
		}
		fmt.Fprintf(&o, " %d of %d merges used a stack or batch.", t.MergedOnStack, t.Merges)
		if t.FullRuns == nil {
			o.WriteString(" No CI runs kept for the repo (full: ?).")
		} else if l.FullRunsRepoWide {
			o.WriteString(" Full runs are the repo's, all its lines.")
		}
		o.WriteString("\n")
		if len(l.Reasons) > 0 {
			o.WriteString("\n  ended for:\n")
			for _, r := range l.Reasons {
				fmt.Fprintf(&o, "    %-26s %3d", r.Reason, r.Count)
				if len(r.Examples) > 0 && r.Examples[0].Note != "" {
					fmt.Fprintf(&o, "  e.g. %s", trimTo(r.Examples[0].Note, 90))
				}
				o.WriteString("\n")
			}
		}
	}
	return o.String()
}
