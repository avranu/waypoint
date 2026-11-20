package api

import (
	"fmt"
	"net/http"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// reviewStats is how the review bot's findings held up (WP-371):
// ?project=KEY, ?days=N, ?format=text.
func (s *Server) reviewStats(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	st, err := s.svc.ReviewStats(r.Context(), actorFrom(r), core.ReviewStatsInput{Project: q.Get("project"), Days: queryInt(r, "days", 0)})
	if err != nil || !wantsText(r) {
		return st, err
	}
	return textResponse(reviewStatsText(st)), nil
}

// reviewStatsText is wp pr review-stats.
func reviewStatsText(st *core.ReviewStats) string {
	var o strings.Builder
	scope := "every project"
	if st.Project != "" {
		scope = st.Project
	}
	t := st.Totals
	fmt.Fprintf(&o, "Review findings, %s, last %d days: %d findings from %d reviews, %s\n", scope, st.Days, t.Findings, t.Runs, judgedText(t.Judged))
	if t.Findings == 0 {
		o.WriteString("\nNo review findings in this window.\n")
		return o.String()
	}
	fmt.Fprintf(&o, "  %d dropped by the evidence check, %d dropped by the verifier and not posted; %s tokens%s\n",
		t.Evidence, t.Withheld, thousands(t.Tokens), perUsefulText(t.TokensPerUseful))

	o.WriteString("\nReviewers (model · strategy · prompt version):\n")
	for _, r := range st.Reviewers {
		name := r.Model
		if name == "" {
			name = "(no model)"
		}
		fmt.Fprintf(&o, "  %s%s · %s · %s\n", name, providersText(r.Providers), dash(r.Strategy), dash(r.PromptVersion))
		fmt.Fprintf(&o, "    %d findings, %s", r.Findings, judgedText(r.Judged))
		if r.Evidence > 0 || r.Withheld > 0 {
			fmt.Fprintf(&o, "; dropped %d evidence, %d verifier", r.Evidence, r.Withheld)
		}
		fmt.Fprintf(&o, "; %d reviews, %s tokens%s\n", r.Runs, thousands(r.Tokens), perUsefulText(r.TokensPerUseful))
	}

	if len(st.Verifiers) > 0 {
		o.WriteString("\nVerifiers:\n")
		for _, v := range st.Verifiers {
			fmt.Fprintf(&o, "  %s%s: %d reviews, %s tokens\n", v.Model, providersText(v.Providers), v.Runs, thousands(v.Tokens))
			fmt.Fprintf(&o, "    kept: %s\n", judgedText(v.Kept))
			fmt.Fprintf(&o, "    dropped %d, %d posted anyway: %s", v.Dropped, v.Posted.Useful+v.Posted.False+v.Posted.Unjudged, judgedText(v.Posted))
			if v.DropAccuracy != nil {
				fmt.Fprintf(&o, "; drops right %s", pctInterval(*v.DropAccuracy, *v.DropLow, *v.DropHigh))
			}
			o.WriteString("\n")
		}
	}

	o.WriteString("\nVerifier on against off (kept findings against those of runs without one):\n")
	for _, c := range []struct {
		name string
		c    core.ReviewArmClass
	}{{"verified", st.Verify.Verified}, {"no verifier", st.Verify.Skipped}} {
		fmt.Fprintf(&o, "  %-11s %d findings from %d reviews, %s; %s tokens%s\n", c.name, c.c.Findings, c.c.Runs, judgedText(c.c.Judged),
			thousands(c.c.Tokens), perUsefulText(c.c.TokensPerUseful))
	}
	o.WriteString("\nUseful: fixed, or taken by an agent; false: dismissed or withdrawn; a leftover audit's verdict counts once given. Intervals are 90%.\n")
	return o.String()
}

func judgedText(j core.Judged) string {
	s := fmt.Sprintf("%d useful, %d false, %d unjudged", j.Useful, j.False, j.Unjudged)
	if j.Precision != nil {
		s += ": precision " + pctInterval(*j.Precision, *j.Low, *j.High)
	}
	return s
}

func pctInterval(p, lo, hi float64) string {
	return fmt.Sprintf("%.0f%% (%.0f–%.0f%%)", 100*p, 100*lo, 100*hi)
}

func providersText(ps []string) string {
	if len(ps) == 0 {
		return ""
	}
	return " (" + strings.Join(ps, ", ") + ")"
}

func perUsefulText(n int64) string {
	if n == 0 {
		return ""
	}
	return ", " + thousands(n) + " per useful finding"
}

func dash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// thousands writes n with comma separators.
func thousands(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}
