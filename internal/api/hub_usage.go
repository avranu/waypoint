package api

import (
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// When glossary entries, pages and design notes are used. (WP-D15)
func (s *Server) usageRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/knowledge/usage", a(s.itemUsage))
	mux.HandleFunc("GET /api/v1/agents/knowledge", a(s.knowledgeOverview))
}

func (s *Server) itemUsage(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ItemUsage(r.Context(), actorFrom(r), r.URL.Query().Get("type"), int64(queryInt(r, "id", 0)))
}

func (s *Server) knowledgeOverview(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.KnowledgeOverview(r.Context(), actorFrom(r), core.KnowledgeQuery{
		Agent: v.Get("agent"), Project: v.Get("project"), Days: queryInt(r, "days", 30)})
}
