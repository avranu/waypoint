package api

import "net/http"

// The Agents page (WP-D15): the queue agents take work from, what they
// did, the pull requests, knowledge use and the audit log. Each area
// registers its routes in its own file.
func (s *Server) agentsHubRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/agents", a(s.knownAgents))
	s.queueRoutes(mux)
	s.activityRoutes(mux)
	s.pullRoutes(mux)
	s.usageRoutes(mux)
	s.usageAnalyticsRoutes(mux)
	s.usageTranscriptRoutes(mux)
	s.otlpRoutes(mux)
	s.overviewRoutes(mux)
	s.auditRoutes(mux)
}

func (s *Server) knownAgents(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.KnownAgents(r.Context(), actorFrom(r))
}
