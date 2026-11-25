package api

import (
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The agent kit (WP-ADR2 §4): the kit and what each repo gets from it,
// what checkouts report after `wp kit sync`, rolling a sync out to your
// runners, sharing a component through a pull request, and (admins) which
// repo is the kit.
func (s *Server) kitRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/kit", a(s.getKit))
	mux.HandleFunc("GET /api/v1/kit/resolve", a(s.resolveKit))
	mux.HandleFunc("POST /api/v1/kit/reports", a(s.reportKit))
	mux.HandleFunc("GET /api/v1/kit/checkouts", a(s.kitCheckouts))
	mux.HandleFunc("POST /api/v1/kit/rollout", a(s.rolloutKit))
	mux.HandleFunc("POST /api/v1/kit/add", a(s.addToKit))
	mux.HandleFunc("POST /api/v1/kit/read", a(s.readKitNow))
	mux.HandleFunc("GET /api/v1/admin/kit", a(s.getKitSource))
	mux.HandleFunc("PUT /api/v1/admin/kit", a(s.setKitSource))
}

func (s *Server) getKit(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetKit(r.Context(), actorFrom(r))
}

func (s *Server) resolveKit(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ResolveKit(r.Context(), actorFrom(r), r.URL.Query().Get("repo"))
}

func (s *Server) reportKit(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.KitReport
	if err := decodeLarge(w, r, &in); err != nil {
		return nil, err
	}
	if err := s.svc.ReportKitCheckout(r.Context(), actorFrom(r), in); err != nil {
		return nil, err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil, nil
}

func (s *Server) kitCheckouts(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListKitCheckouts(r.Context(), actorFrom(r))
}

func (s *Server) rolloutKit(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.RolloutKit(r.Context(), actorFrom(r))
}

func (s *Server) addToKit(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.KitAddInput
	if err := decodeLarge(w, r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddToKit(r.Context(), actorFrom(r), in)
}

func (s *Server) readKitNow(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ReadKitNow(r.Context(), actorFrom(r))
}

func (s *Server) getKitSource(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetKitSource(r.Context(), actorFrom(r))
}

func (s *Server) setKitSource(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		RepoID int64 `json:"repo_id"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetKitSource(r.Context(), actorFrom(r), in.RepoID)
}
