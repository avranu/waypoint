package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

func (s *Server) listExperiments(w http.ResponseWriter, r *http.Request) (any, error) {
	l, err := s.svc.ListExperiments(r.Context(), actorFrom(r))
	if err != nil || !wantsText(r) {
		return l, err
	}
	return textResponse(experimentsText(l)), nil
}

func (s *Server) startExperiment(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ExperimentStart
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.StartExperiment(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) stopExperiment(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.StopExperiment(r.Context(), actorFrom(r), r.PathValue("key"), in.Reason)
}

func (s *Server) experimentResults(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	var run int64
	if raw := v.Get("run"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, &core.InvalidError{Msg: "run is a number"}
		}
		run = n
	}
	res, err := s.svc.ExperimentResults(r.Context(), actorFrom(r), r.PathValue("key"), run, v.Get("scope"))
	if err != nil || !wantsText(r) {
		return res, err
	}
	return textResponse(experimentResultsText(res)), nil
}

func (s *Server) issueArms(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.IssueArmsInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.IssueArms(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func experimentsText(l *core.Experiments) string {
	var b strings.Builder
	for i, e := range l.Experiments {
		if i > 0 {
			b.WriteString("\n")
		}
		state := "off"
		if e.Live != nil {
			state = "live: " + runLine(e.Live)
		}
		fmt.Fprintf(&b, "%s (%s) %s\n  %s\n  arms: %s\n", e.Key, e.Title, state, e.Question, armsLine(e.Arms, e.Weights))
		if e.Live != nil {
			fmt.Fprintf(&b, "  tasks so far: %s\n", countsLine(e.Arms, e.Live.Assigned, e.Live.Exposed))
		}
		if n := len(e.Past); n > 0 {
			p := e.Past[0]
			fmt.Fprintf(&b, "  last stopped %s: %s (%d past run%s)\n", p.StoppedAt.UTC().Format("2006-01-02"), p.StopReason, n, plural(n))
		}
	}
	return b.String()
}

func runLine(r *core.ExperimentRun) string {
	s := fmt.Sprintf("run %d since %s", r.ID, r.StartedAt.UTC().Format("2006-01-02"))
	if r.EndsAt != nil {
		s += ", ends " + r.EndsAt.UTC().Format("2006-01-02")
	}
	if r.Target > 0 {
		s += fmt.Sprintf(", target %d finished per arm", r.Target)
	}
	if len(r.Projects) > 0 {
		s += ", in " + strings.Join(r.Projects, " ")
	}
	return s
}

func armsLine(arms []core.ExperimentArm, w map[string]int) string {
	parts := make([]string, len(arms))
	for i, a := range arms {
		parts[i] = fmt.Sprintf("%s %d", a.Key, w[a.Key])
		if a.Control {
			parts[i] += " (control)"
		}
	}
	return strings.Join(parts, ", ")
}

func countsLine(arms []core.ExperimentArm, assigned, exposed map[string]int) string {
	parts := make([]string, len(arms))
	for i, a := range arms {
		parts[i] = fmt.Sprintf("%s %d (%d exposed)", a.Key, assigned[a.Key], exposed[a.Key])
	}
	return strings.Join(parts, ", ")
}

func experimentResultsText(r *core.ExperimentResults) string {
	var b strings.Builder
	state := "live"
	if r.Run.StoppedAt != nil {
		state = "stopped " + r.Run.StoppedAt.UTC().Format("2006-01-02") + ": " + r.Run.StopReason
	}
	fmt.Fprintf(&b, "%s, run %d (%s), %s; %s tasks\n", r.Title, r.Run.ID, state, r.Run.StartedAt.UTC().Format("2006-01-02")+" to "+r.To.UTC().Format("2006-01-02"), map[string]string{"done": "finished", "all": "all measured"}[r.Scope])
	fmt.Fprintf(&b, "\n%s\n", r.Verdict)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "Note: %s\n", n)
	}
	b.WriteString("\nArm                     assigned exposed measured  done  in scope\n")
	for _, a := range r.Arms {
		dr := "-"
		if a.DoneRate != nil {
			dr = fmt.Sprintf("%.0f%%", 100**a.DoneRate)
		}
		fmt.Fprintf(&b, "%-24s %8d %7d %8d %5s %9d\n", clip(a.Label, 24), a.Assigned, a.Exposed, a.Measured, dr, a.InScope)
	}
	if r.SRM != nil {
		fmt.Fprintf(&b, "Assignment matches the weights: p=%.2g\n", *r.SRM)
	}
	b.WriteString("\nMedian per task")
	for _, a := range r.Arms[min(1, len(r.Arms)):] {
		fmt.Fprintf(&b, "; %s against %s: change, 95%% interval, p (Holm)", a.Label, r.Arms[0].Label)
	}
	b.WriteString("\n")
	for mi, m := range r.Metrics {
		mark := " "
		if m.Key == r.Primary {
			mark = "*"
		}
		fmt.Fprintf(&b, "%s %-16s", mark, m.Label)
		for _, a := range r.Arms {
			st := a.Stats[mi]
			fmt.Fprintf(&b, " %10s", metricValue(m.Unit, st.Median))
		}
		for _, a := range r.Arms[1:] {
			st := a.Stats[mi]
			if st.Ratio == nil {
				b.WriteString("   -")
				continue
			}
			fmt.Fprintf(&b, "   %+.0f%%", 100*(*st.Ratio-1))
			if st.Low != nil && st.High != nil {
				fmt.Fprintf(&b, " [%+.0f%%, %+.0f%%]", 100*(*st.Low-1), 100*(*st.High-1))
			}
			if st.PAdj != nil {
				fmt.Fprintf(&b, " p=%.2g", *st.PAdj)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("\nCaveats:\n")
	for _, c := range r.Caveats {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	return b.String()
}

func metricValue(unit string, v *float64) string {
	if v == nil {
		return "-"
	}
	switch unit {
	case "usd":
		return fmt.Sprintf("$%.2f", *v)
	case "tokens":
		switch {
		case *v >= 1e6:
			return fmt.Sprintf("%.1fM", *v/1e6)
		case *v >= 1e3:
			return fmt.Sprintf("%.0fk", *v/1e3)
		}
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
