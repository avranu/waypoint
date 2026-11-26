package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// ciHistory is how CI fared over time (WP-490): ?repo=OWNER/NAME,
// ?days=N, ?bucket=day|week.
func (s *Server) ciHistory(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	h, err := s.svc.CIHistory(r.Context(), actorFrom(r), core.CIHistoryInput{Repo: q.Get("repo"), Days: queryInt(r, "days", 0), Bucket: q.Get("bucket")})
	if err != nil || !wantsText(r) {
		return h, err
	}
	return textResponse(ciHistoryText(h, queryBool(r, "verbose"))), nil
}

// ciHistoryText is wp report ci history: the last seven days against the seven
// before, the latest buckets (all of them if verbose), then the jobs that changed.
func ciHistoryText(h *core.CIHistory, verbose bool) string {
	var o strings.Builder
	dur := func(p *int64) string {
		if p == nil {
			return "–"
		}
		return core.ShortDuration(time.Duration(*p) * time.Second)
	}
	hours := func(sec int64) string { return fmt.Sprintf("%.1fh", float64(sec)/3600) }
	failRate := func(st core.CIHistoryStats) string {
		if st.Jobs == 0 {
			return "–"
		}
		return fmt.Sprintf("%d (%.0f%%)", st.Failed, 100*float64(st.Failed)/float64(st.Jobs))
	}
	scope := "all repos"
	if h.Repo != "" {
		scope = h.Repo
	}
	fmt.Fprintf(&o, "CI history, %s, last %d days by %s", scope, h.Days, h.Bucket)
	if h.Since != nil {
		fmt.Fprintf(&o, " (kept since %s)", h.Since.UTC().Format("2006-01-02"))
	}
	o.WriteString("\n")
	if h.Since == nil {
		o.WriteString("\nNo CI runs kept yet: they are recorded as the forge's runs finish, and each repo's older runs are read once within the hour.\n")
		return o.String()
	}

	o.WriteString("\n                         last 7 days   7 days before\n")
	row := func(label, a, b string) { fmt.Fprintf(&o, "  %-22s %-13s %s\n", label, a, b) }
	n, b := h.Now, h.Before
	row("jobs", fmt.Sprint(n.Jobs), fmt.Sprint(b.Jobs))
	row("failed", failRate(n), failRate(b))
	row("runner time", hours(n.Busy), hours(b.Busy))
	row("wait for runner p50/p90", dur(n.WaitP50)+"/"+dur(n.WaitP90), dur(b.WaitP50)+"/"+dur(b.WaitP90))
	row("full runs", fmt.Sprint(n.Runs), fmt.Sprint(b.Runs))
	row("full run p50/p90", dur(n.RunP50)+"/"+dur(n.RunP90), dur(b.RunP50)+"/"+dur(b.RunP90))
	row("merges", fmt.Sprint(n.Merges), fmt.Sprint(b.Merges))
	row("time per merge p50", dur(n.MergeP50), dur(b.MergeP50))

	layout := "Jan 02"
	fmt.Fprintf(&o, "\n%-8s %5s %10s %8s %11s %9s %13s %7s %9s\n", h.Bucket, "jobs", "failed", "runner", "wait p50", "full runs", "run p50/p90", "merges", "per merge")
	var pts []core.CIHistoryPoint
	for _, p := range h.Points {
		span := 24 * time.Hour
		if h.Bucket == "week" {
			span = 7 * 24 * time.Hour
		}
		if !p.At.Add(span).After(*h.Since) {
			continue
		}
		pts = append(pts, p)
	}
	if !verbose && len(pts) > 7 {
		fmt.Fprintf(&o, "%-8s +%d earlier (-v)\n", "", len(pts)-7)
		pts = pts[len(pts)-7:]
	}
	for _, p := range pts {
		fmt.Fprintf(&o, "%-8s %5d %10s %8s %11s %9d %13s %7d %9s\n", p.At.UTC().Format(layout), p.Jobs, failRate(p.CIHistoryStats), hours(p.Busy),
			dur(p.WaitP50), p.Runs, dur(p.RunP50)+"/"+dur(p.RunP90), p.Merges, dur(p.MergeP50))
	}

	if len(h.Jobs) > 0 {
		o.WriteString("\nFull-tier jobs, most runner time first (p50 of green runs, last 7 days vs the 7 before):\n")
		for _, j := range h.Jobs {
			fmt.Fprintf(&o, "  %-40s %8s  was %-8s  runs %d (was %d), failed %d (was %d)\n", trimTo(j.Repo+" "+j.Name, 40),
				dur(j.P50), dur(j.BeforeP50), j.Runs, j.BeforeRuns, j.Failed, j.BeforeFailed)
		}
	}
	return o.String()
}

func trimTo(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
