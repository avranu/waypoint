package api

import (
	"fmt"
	"net/http"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// mistakes lists a project's recurring mistakes (WP-710): ?project=KEY,
// ?format=text.
func (s *Server) mistakes(w http.ResponseWriter, r *http.Request) (any, error) {
	project := r.URL.Query().Get("project")
	list, err := s.svc.MistakeClasses(r.Context(), actorFrom(r), project)
	if err != nil || !wantsText(r) {
		return list, err
	}
	return textResponse(mistakesText(project, list)), nil
}

// mistakesText is wp report quality mistakes.
func mistakesText(project string, list []*core.MistakeClass) string {
	if len(list) == 0 {
		return fmt.Sprintf("No recurring mistakes filed in %s yet: a class needs 3 independent instances within 14 days.\n", project)
	}
	var o strings.Builder
	counts := map[string]int{}
	recurred := 0
	for _, c := range list {
		counts[c.State]++
		recurred += c.Recurred
	}
	fmt.Fprintf(&o, "Recurring mistakes in %s: %d open, %d with a check landed (%d instance%s since), %d declined\n\n",
		project, counts[core.MistakeOpen], counts[core.MistakeLanded], recurred, plural(recurred), counts[core.MistakeDeclined])
	for _, c := range list {
		issue := c.Issue
		if issue == "" {
			issue = "(deleted)"
		}
		fmt.Fprintf(&o, "%-9s %-8s %s\n", issue, c.State, c.Label)
		fmt.Fprintf(&o, "%-18s %d instance%s, filed %s", "", c.Instances, plural(c.Instances), c.FiledAt.UTC().Format("2006-01-02"))
		if c.LandedAt != nil {
			fmt.Fprintf(&o, ", landed %s, %d since", c.LandedAt.UTC().Format("2006-01-02"), c.Recurred)
		}
		o.WriteString("\n")
	}
	return o.String()
}
