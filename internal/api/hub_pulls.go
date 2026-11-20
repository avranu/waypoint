package api

import (
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Pull requests: where they went, who reviewed and approved them, what agents
// spent; and the note bots sign their reviews with. (WP-D15)
func (s *Server) pullRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/pulls", a(s.listPulls))
	mux.HandleFunc("GET /api/v1/pulls/{id}", a(s.getPull))
	mux.HandleFunc("GET /api/v1/admin/review-note", a(s.getReviewNote))
	mux.HandleFunc("PUT /api/v1/admin/review-note", a(s.setReviewNote))
	mux.HandleFunc("GET /api/v1/admin/ship-reply", a(s.getShipReply))
	mux.HandleFunc("PUT /api/v1/admin/ship-reply", a(s.setShipReply))
	mux.HandleFunc("GET /api/v1/admin/pr-bodies", a(s.getPRBodies))
	mux.HandleFunc("PUT /api/v1/admin/pr-bodies", a(s.setPRBodyMode))
	mux.HandleFunc("GET /api/v1/admin/email", a(s.getEmailSettings))
	mux.HandleFunc("PUT /api/v1/admin/email", a(s.setEmailSettings))
	mux.HandleFunc("POST /api/v1/admin/email/test", a(s.testEmail))
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.ListPullRequests(r.Context(), actorFrom(r), core.PRQuery{
		Project: v.Get("project"), Agent: v.Get("agent"), State: v.Get("state"), Text: v.Get("q"), Repo: v.Get("repo"), Days: queryInt(r, "days", 30)})
}

func (s *Server) getPull(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetPullRequest(r.Context(), actorFrom(r), id)
}

func (s *Server) getReviewNote(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetReviewNote(r.Context(), actorFrom(r))
}

func (s *Server) setReviewNote(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Note string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetReviewNote(r.Context(), actorFrom(r), in.Note)
}

func (s *Server) getShipReply(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetShipReply(r.Context(), actorFrom(r))
}

func (s *Server) setShipReply(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Reply string `json:"reply"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetShipReply(r.Context(), actorFrom(r), in.Reply)
}

// PR descriptions both ways, and the switch between them (WP-55).
func (s *Server) getPRBodies(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.PRBodies(r.Context(), actorFrom(r), queryInt(r, "days", 30), queryInt(r, "show", 0))
}

func (s *Server) setPRBodyMode(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Mode string `json:"mode"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := s.svc.SetPRBodyMode(r.Context(), actorFrom(r), in.Mode); err != nil {
		return nil, err
	}
	return s.svc.PRBodies(r.Context(), actorFrom(r), 30, 0)
}

func (s *Server) getEmailSettings(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetEmailSettings(r.Context(), actorFrom(r))
}

func (s *Server) setEmailSettings(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.EmailSettingsInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetEmailSettings(r.Context(), actorFrom(r), in)
}

func (s *Server) testEmail(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		To string `json:"to"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := s.svc.SendTestEmail(r.Context(), actorFrom(r), in.To); err != nil {
		return nil, err
	}
	return map[string]bool{"sent": true}, nil
}
