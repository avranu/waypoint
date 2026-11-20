package api

import (
	"fmt"
	"net/http"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The relation graph (WP-385): what lies around a thing, and the nearest
// route between two.

func (s *Server) graphNear(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	q := core.GraphQuery{Ref: v.Get("ref"), Project: v.Get("project"), Hops: min(max(queryInt(r, "hops", 3), 1), 6),
		Limit: min(max(queryInt(r, "limit", 40), 1), 500), Guesses: v.Get("guesses") != "0" && v.Get("guesses") != "false", Open: v.Get("open") == "1"}
	for _, t := range strings.Split(v.Get("types"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			q.Types = append(q.Types, t)
		}
	}
	g, err := s.svc.Neighbourhood(r.Context(), actorFrom(r), q)
	if err != nil || !wantsText(r) {
		return g, err
	}
	return textResponse(graphNearText(g, q)), nil
}

func (s *Server) graphPath(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	p, err := s.svc.Path(r.Context(), actorFrom(r), v.Get("from"), v.Get("to"), v.Get("project"), v.Get("guesses") != "0" && v.Get("guesses") != "false")
	if err != nil || !wantsText(r) {
		return p, err
	}
	return textResponse(graphPathText(p)), nil
}

// graphTitle is a node's title in agent-facing text: people outside write
// pull request, issue and session titles, so it is marked and can't close
// its marker or start a line of its own.
func graphTitle(title string) string {
	return "<untrusted>" + oneLine(clip(title, 70)) + "</untrusted>"
}

// graphNodeText is a node in a line: its code, title and state. A file's
// path and a pull request's code come from the forge too, so they are kept
// to one line.
func graphNodeText(n core.GraphNode) string {
	ref := oneLine(n.Ref)
	switch {
	case n.Type == core.KDesign && n.Kind != "" && strings.HasPrefix(ref, "#"):
		ref = n.Kind + " " + ref
	case n.Type == core.KGlossary:
		ref = "term"
	case n.Type == core.KFile:
		return ref
	case n.Type == core.KSession:
		return ref + " " + graphTitle(n.Title)
	}
	s := ref + " " + graphTitle(n.Title)
	if n.State != "" {
		s += " [" + n.State + "]"
	}
	return s
}

// graphStepText is one step of a route: how it reads, and who made it
// when not a person.
func graphStepText(e core.GraphEdge) string {
	s := e.Label
	switch e.Basis {
	case "inferred":
		s += " (guess"
		if e.Why != "" {
			s += ": " + oneLine(e.Why)
		}
		s += ")"
	case "agent":
		s += " (agent)"
	}
	return s
}

func graphNearText(g *core.GraphView, q core.GraphQuery) string {
	var b strings.Builder
	b.WriteString(graphNodeText(g.Center) + "\n")
	if len(g.Nodes) == 0 {
		b.WriteString("Nothing is linked to it yet")
		if !g.Guesses {
			b.WriteString(", and guesses are off")
		}
		b.WriteString(".\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Within %d steps, nearest first (distance: the route there):\n", q.Hops)
	names := map[string]string{g.Center.Node: oneLine(g.Center.Ref)}
	for _, h := range g.Nodes {
		names[h.Node] = oneLine(h.Ref)
	}
	for _, h := range g.Nodes {
		var route []string
		for i, e := range h.Path {
			step := graphStepText(e)
			if i < len(h.Path)-1 {
				name := names[e.To]
				if name == "" {
					name = "(hidden by the filters)"
				}
				step += " " + name
			}
			route = append(route, step)
		}
		fmt.Fprintf(&b, "  %4.1f  %s: %s\n", h.Distance, graphNodeText(h.GraphNode), strings.Join(route, " → "))
	}
	if g.More {
		if q.Limit < 500 {
			fmt.Fprintf(&b, "More lie within reach (--limit %d, or --hops %d).\n", min(q.Limit*2, 500), max(1, q.Hops-1))
		} else {
			fmt.Fprintf(&b, "More lie within reach (--hops %d, or --types).\n", max(1, q.Hops-1))
		}
	}
	if !g.Guesses {
		b.WriteString("Only links people and agents made; without --sure, issues alike in words and Waypoint's matches join them.\n")
	}
	return b.String()
}

// nearbyText lists work in progress near a task in its answer (WP-385),
// with the route that joins them, so the agent checks before changing the
// same thing.
func nearbyText(hits []core.GraphHit) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nIn progress nearby (another session may be changing the same code; `wp graph KEY` shows more):\n")
	for _, h := range hits {
		var route []string
		for _, e := range h.Path {
			route = append(route, graphStepText(e))
		}
		fmt.Fprintf(&b, "  %s %s (%s)\n", oneLine(h.Ref), graphTitle(h.Title), strings.Join(route, " → "))
	}
	return b.String()
}

func graphPathText(p *core.GraphPath) string {
	var b strings.Builder
	if !p.Found {
		fmt.Fprintf(&b, "Nothing links %s and %s", graphNodeText(p.From), graphNodeText(p.To))
		b.WriteString(", within 6 steps.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%s → %s: distance %.1f, %d steps\n", oneLine(p.From.Ref), oneLine(p.To.Ref), p.Distance, p.Hops)
	b.WriteString("  " + graphNodeText(p.Nodes[0]) + "\n")
	for i, e := range p.Path {
		fmt.Fprintf(&b, "    %s %s\n", graphStepText(e), graphNodeText(p.Nodes[i+1]))
	}
	return b.String()
}
