package api

import (
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The Agents overview (WP-D16): everything the first tab shows in one call,
// recent changes for its paging, and what one session used.
func (s *Server) overviewRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/agents/overview", a(s.agentOverview))
	mux.HandleFunc("GET /api/v1/changes", a(s.recentChanges))
	mux.HandleFunc("GET /api/v1/sessions/{id}/usage", a(s.sessionUsage))
}

func (s *Server) agentOverview(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.AgentOverview(r.Context(), actorFrom(r), core.OverviewQuery{Agent: v.Get("agent"), Project: v.Get("project"), Days: queryInt(r, "days", 7)})
}

func (s *Server) recentChanges(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.RecentChanges(r.Context(), actorFrom(r), core.ChangeQuery{
		Kind: v.Get("kind"), Project: v.Get("project"), Agent: v.Get("agent"),
		Before: int64(queryInt(r, "before", 0)), Limit: queryInt(r, "limit", 20)})
}

func (s *Server) sessionUsage(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetSessionUsage(r.Context(), actorFrom(r), id)
}
