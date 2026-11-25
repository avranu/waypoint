package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

func (s *Server) listUnreviewed(w http.ResponseWriter, r *http.Request) (any, error) {
	items, err := s.svc.ListUnreviewed(r.Context(), actorFrom(r), r.URL.Query().Get("project"), queryInt(r, "limit", 200))
	if err != nil || !wantsText(r) {
		return items, err
	}
	return textResponse(unreviewedText(items)), nil
}

// review marks something reviewed: POST /review/{issue|comment|close|page|glossary}/{ref}.
func (s *Server) review(w http.ResponseWriter, r *http.Request) (any, error) {
	n, err := s.svc.Review(r.Context(), actorFrom(r), r.PathValue("type"), r.PathValue("ref"), r.URL.Query().Get("project"))
	if err != nil {
		return nil, err
	}
	return map[string]int{"reviewed": n}, nil
}

// risk is an object's provenance risk (WP-554): GET
// /risk/{issue|comment|page|design|glossary|pull}/{ref}.
func (s *Server) risk(w http.ResponseWriter, r *http.Request) (any, error) {
	risk, err := s.svc.RiskOf(r.Context(), actorFrom(r), r.PathValue("kind"), r.PathValue("ref"), r.URL.Query().Get("project"))
	if err != nil || !wantsText(r) {
		return risk, err
	}
	return textResponse(riskText(risk)), nil
}

// riskText is the level and, under it, each reason.
func riskText(r *core.ProvenanceRisk) string {
	var b strings.Builder
	b.WriteString("Risk: " + r.Level + "\n")
	for _, x := range r.Reasons {
		fmt.Fprintf(&b, "  %s (%s): %s\n", x.Level, x.Factor, x.Text)
	}
	return b.String()
}

// reviewCloses marks agents' closes of issues reviewed (WP-272):
// POST /review/closes {"issues":[keys]}.
func (s *Server) reviewCloses(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Issues []string `json:"issues"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	n, err := s.svc.ReviewCloses(r.Context(), actorFrom(r), in.Issues)
	if err != nil {
		return nil, err
	}
	return map[string]int{"reviewed": n}, nil
}

func (s *Server) pageCodesNext(w http.ResponseWriter, r *http.Request) (any, error) {
	next, err := s.svc.PageCodesNext(r.Context(), actorFrom(r), r.PathValue("key"))
	if err != nil || !wantsText(r) {
		return next, err
	}
	return textResponse(codesText(next)), nil
}

// reservePageCodes moves the next free codes up: {"PL": 15, "D": 27}.
func (s *Server) reservePageCodes(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.PageCodeNext
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	next, err := s.svc.ReservePageCodes(r.Context(), actorFrom(r), r.PathValue("key"), in)
	if err != nil || !wantsText(r) {
		return next, err
	}
	return textResponse(codesText(next)), nil
}

func unreviewedText(items []core.UnreviewedItem) string {
	if len(items) == 0 {
		return "Nothing written by agents is waiting for review.\n"
	}
	var b strings.Builder
	for _, it := range items {
		switch it.Type {
		case core.ReviewClose:
			fmt.Fprintf(&b, "close of %s %q (%s) by agent %s %s\n", it.Ref, it.Title, it.Excerpt, it.Agent, day(it.At))
		case core.ReviewComment:
			fmt.Fprintf(&b, "comment #%d on %s %q by agent %s %s: %s\n", it.ID, it.Ref, it.Title, it.Agent, day(it.At), it.Excerpt)
		case core.ReviewGlossary:
			fmt.Fprintf(&b, "term %q (#%d, %s) by agent %s %s\n", it.Title, it.ID, cmpStr(it.Project, "global"), it.Agent, day(it.At))
		case core.ReviewPage:
			fmt.Fprintf(&b, "page %s (#%d) %q by agent %s %s\n", it.Ref, it.ID, it.Title, it.Agent, day(it.At))
		default:
			fmt.Fprintf(&b, "issue %s %q by agent %s %s\n", it.Ref, it.Title, it.Agent, day(it.At))
		}
	}
	return b.String()
}

// pagesText lists pages one per line, for agents.
func pagesText(pages []*core.Page) string {
	if len(pages) == 0 {
		return "No pages.\n"
	}
	var b strings.Builder
	for _, p := range pages {
		b.WriteString(pageLine(p) + "\n")
	}
	return b.String()
}

// pageLine: "PL13 #9 Provider health [plan] project:API by agent X, not yet reviewed by a person".
func pageLine(p *core.Page) string {
	var b strings.Builder
	if p.Code != "" {
		b.WriteString(p.Code + " ")
	}
	fmt.Fprintf(&b, "#%d %s", p.ID, p.Title)
	if p.Category != "" {
		b.WriteString(" [" + p.Category + "]")
	}
	if p.ProjectKey != "" {
		b.WriteString(" project:" + p.ProjectKey)
	} else {
		b.WriteString(" (personal)")
	}
	if p.Draft {
		b.WriteString(" draft")
	}
	if p.ChildCount > 0 {
		fmt.Fprintf(&b, " (%d sub-pages)", p.ChildCount)
	}
	switch {
	case p.TextAgent != "" && !p.Unreviewed && p.ReviewedOnStory:
		by := "a person"
		if p.ReviewedBy != nil {
			by = p.ReviewedBy.DisplayName
		}
		b.WriteString(" — by agent " + p.TextAgent + "; " + by + " approved its story, not its text")
	case p.TextAgent != "":
		b.WriteString(" — " + writtenBy(p.TextBy, p.TextAgent, p.Unreviewed, p.ReviewedBy))
	}
	return b.String()
}

func pageRefText(r core.PageRef) string {
	if r.Code != "" {
		return fmt.Sprintf("%s (#%d) %s", r.Code, r.ID, r.Title)
	}
	return fmt.Sprintf("#%d %s", r.ID, r.Title)
}

// codesText lists a project's next free page codes.
func codesText(next core.PageCodeNext) string {
	keys := make([]string, 0, len(next))
	for k := range next {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s%d", k, next[k]))
	}
	return "Next free: " + strings.Join(parts, " · ") + "\n"
}
