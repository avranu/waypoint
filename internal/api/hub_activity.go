package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// What agents did: runs, time, tokens, cost and the pull requests they led to. (WP-D15)
func (s *Server) activityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents/activity", s.authed(s.agentActivity))
}

func (s *Server) agentActivity(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	act, err := s.svc.AgentActivity(r.Context(), actorFrom(r), core.ActivityQuery{
		Agent: v.Get("agent"), Project: v.Get("project"), Days: queryInt(r, "days", 30)})
	if err != nil {
		return nil, err
	}
	if wantsText(r) {
		return textResponse(activityText(act, v.Get("agent"))), nil
	}
	return act, nil
}

// activityText is the activity in a few lines, for wp report activity.
func activityText(a *core.Activity, spec string) string {
	var b strings.Builder
	who := "Agents"
	if spec != "" {
		who = "Agents matching " + spec
	}
	days := int(a.To.Sub(a.From).Hours()/24) + 1
	t := a.Totals
	fmt.Fprintf(&b, "%s, last %d days: %s done in %s, %s, %s tokens in / %s out", who, days,
		activityPlural(t.TasksDone, "task"), activityPlural(t.Runs, "run"), activityHours(t.Seconds),
		activityCount(t.InputTokens), activityCount(t.OutputTokens))
	if t.CostUSD > 0 {
		fmt.Fprintf(&b, ", $%.2f", t.CostUSD)
	}
	fmt.Fprintf(&b, "; %s merged, %s.\n", activityPlural(t.PRsMerged, "PR"), activityPlural(t.BotReviews, "bot review"))
	if len(a.Agents) == 0 {
		b.WriteString("No agent work in this period.\n")
		return b.String()
	}
	b.WriteString("\n")
	for _, x := range a.Agents {
		if x.Done == 0 && x.Runs == 0 && x.Seconds == 0 && x.InputTokens+x.OutputTokens == 0 && x.CostUSD == 0 && x.PRsMerged == 0 &&
			x.Comments == 0 && x.IssuesCreated == 0 && x.Released == 0 && x.Expired == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s: %d done, %s, %s, %s tokens", x.Identity, x.Done, activityPlural(x.Runs, "run"), activityHours(x.Seconds),
			activityCount(x.InputTokens+x.OutputTokens))
		if x.CostUSD > 0 {
			fmt.Fprintf(&b, ", $%.2f", x.CostUSD)
		}
		if x.PRsMerged > 0 {
			fmt.Fprintf(&b, ", %s merged", activityPlural(x.PRsMerged, "PR"))
		}
		if x.IssuesCreated > 0 {
			fmt.Fprintf(&b, ", %s created", activityPlural(x.IssuesCreated, "issue"))
		}
		if x.Comments > 0 {
			fmt.Fprintf(&b, ", %s", activityPlural(x.Comments, "comment"))
		}
		if x.Released > 0 {
			fmt.Fprintf(&b, ", %d released", x.Released)
		}
		if x.Expired > 0 {
			fmt.Fprintf(&b, ", %d expired", x.Expired)
		}
		if len(x.Models) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(x.Models, ", "))
		}
		b.WriteString("\n")
	}
	if len(a.Runs) > 0 {
		b.WriteString("\nLatest runs:\n")
		for _, r := range a.Runs[:min(len(a.Runs), 10)] {
			outcome := r.Outcome
			if r.EndedAt == nil {
				outcome = "working"
			}
			fmt.Fprintf(&b, "  %s %s  %s · %s · %s, %s\n", r.StartedAt.Format("Jan 02 15:04Z"), r.Issue.Key, r.Agent, outcome,
				activityHours(r.Seconds), activityExcerpt(r.Issue.Title, 60))
		}
	}
	return b.String()
}

// activityHours writes a duration as 2h 05m or 12m.
func activityHours(sec int64) string {
	d := time.Duration(sec) * time.Second
	if d >= time.Hour {
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// activityCount writes a large number short: 12.3k, 4.1M.
func activityCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func activityPlural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func activityExcerpt(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
