package api

import (
	"net/http"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Environments and deployments (WP-D19).
func (s *Server) environmentRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/environments", a(s.listEnvironments))
	mux.HandleFunc("GET /api/v1/environments/summary", a(s.environmentSummary))
	mux.HandleFunc("GET /api/v1/environments/at", a(s.environmentsAt))
	mux.HandleFunc("GET /api/v1/delivery", a(s.delivery))
	mux.HandleFunc("GET /api/v1/environments/{id}", a(s.getEnvironment))
	mux.HandleFunc("PATCH /api/v1/environments/{id}", a(s.updateEnvironment))
	mux.HandleFunc("DELETE /api/v1/environments/{id}", a(s.deleteEnvironment))
	mux.HandleFunc("POST /api/v1/environments/{id}/stop", a(s.stopEnvironment))
	mux.HandleFunc("POST /api/v1/environments/{id}/check", a(s.checkEnvironment))
	mux.HandleFunc("GET /api/v1/environments/{id}/deployments", a(s.listDeployments))
	mux.HandleFunc("POST /api/v1/environments/{id}/deployments", a(s.recordDeployment))
	mux.HandleFunc("GET /api/v1/projects/{key}/environments", a(s.listProjectEnvironments))
	mux.HandleFunc("POST /api/v1/projects/{key}/environments", a(s.createEnvironment))
	mux.HandleFunc("GET /api/v1/projects/{key}/environments/{name}", a(s.getNamedEnvironment))
	mux.HandleFunc("POST /api/v1/projects/{key}/environments/{name}/deployments", a(s.recordNamedDeployment))
	mux.HandleFunc("POST /api/v1/projects/{key}/environments/{name}/up", a(s.envUp))
	mux.HandleFunc("POST /api/v1/projects/{key}/environments/{name}/down", a(s.envDown))
	mux.HandleFunc("POST /api/v1/projects/{key}/environments/{name}/vouch", a(s.envVouch))
}

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListEnvironments(r.Context(), actorFrom(r), r.URL.Query().Get("project"), queryBool(r, "stopped"))
}

func (s *Server) listProjectEnvironments(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListEnvironments(r.Context(), actorFrom(r), r.PathValue("key"), queryBool(r, "stopped"))
}

func (s *Server) environmentSummary(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SummarizeEnvironments(r.Context(), actorFrom(r))
}

// environmentsAt: where commits (?sha=a,b) of a repo (?repo=owner/name) run.
func (s *Server) environmentsAt(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.EnvironmentsForCommits(r.Context(), actorFrom(r), v.Get("repo"), strings.Split(v.Get("sha"), ","))
}

// delivery: recent work by where it stands on its way to production
// (WP-389), for ?project=KEY or every project, merged work of ?days=N.
func (s *Server) delivery(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.Delivery(r.Context(), actorFrom(r), core.DeliveryQuery{Project: r.URL.Query().Get("project"), Days: queryInt(r, "days", 0)})
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetEnvironment(r.Context(), actorFrom(r), id)
}

func (s *Server) getNamedEnvironment(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetEnvironmentNamed(r.Context(), actorFrom(r), r.PathValue("key"), r.PathValue("name"))
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.EnvironmentInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateEnvironment(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.EnvironmentInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateEnvironment(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteEnvironment(r.Context(), actorFrom(r), id)
}

func (s *Server) stopEnvironment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.StopEnvironment(r.Context(), actorFrom(r), id)
}

func (s *Server) checkEnvironment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.CheckEnvironmentNow(r.Context(), actorFrom(r), id)
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.ListDeployments(r.Context(), actorFrom(r), id, queryInt(r, "limit", 50))
}

// recordDeployment is what CI calls: {commit, branch, version, status,
// url, notes} with a token that can write to the project.
func (s *Server) recordDeployment(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.DeployInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.RecordDeployment(r.Context(), actorFrom(r), id, in)
}

// recordNamedDeployment is recordDeployment by project and environment
// name; an environment that doesn't exist yet is created.
func (s *Server) recordNamedDeployment(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.DeployInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.RecordDeploymentNamed(r.Context(), actorFrom(r), r.PathValue("key"), r.PathValue("name"), in)
}

func (s *Server) envUp(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.EnvUpInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	in.Name = r.PathValue("name")
	return s.svc.UpEnvironment(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

// envVouch: a person vouches for the environment's addresses, so agents
// may read from them; an agent's call asks a person to (WP-1087). Reading
// is the route's permission: what it changes, it checks in core.
func (s *Server) envVouch(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.EnvVouchInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.VouchEnvironment(r.Context(), actorFrom(r), r.PathValue("key"), r.PathValue("name"), in)
}

func (s *Server) envDown(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.DownEnvironment(r.Context(), actorFrom(r), r.PathValue("key"), r.PathValue("name"))
}
