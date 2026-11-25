package api

import "net/http"

func (s *Server) projectDashboard(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ProjectDashboard(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) dashboards(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.Dashboards(r.Context(), actorFrom(r))
}

// dashboardFix files a task for an agent with the tasks behind one of the
// dashboard's problems, or returns the open one already filed.
func (s *Server) dashboardFix(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Signal string `json:"signal"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	issue, created, err := s.svc.FileDashboardFix(r.Context(), actorFrom(r), r.PathValue("key"), in.Signal)
	if err != nil {
		return nil, err
	}
	return map[string]any{"issue": issue, "created": created}, nil
}
