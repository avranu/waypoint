package api

import (
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Handling review comments end to end (WP-130): list, reply, react, resolve.
func (s *Server) commentRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/pulls/{id}/comments", a(s.listReviewComments))
	mux.HandleFunc("POST /api/v1/review-comments/{id}/reply", a(s.replyComment))
	mux.HandleFunc("POST /api/v1/review-comments/{id}/react", a(s.reactComment))
	mux.HandleFunc("POST /api/v1/review-comments/resolve", a(s.resolveComments))
	mux.HandleFunc("POST /api/v1/review-comments/unresolve", a(s.unresolveComments))
}

func (s *Server) listReviewComments(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.ListReviewComments(r.Context(), actorFrom(r), id)
}

func (s *Server) replyComment(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Body string `json:"body"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.ReplyToReviewComment(r.Context(), actorFrom(r), r.PathValue("id"), in.Body)
}

func (s *Server) reactComment(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Emoji string `json:"emoji"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.ReactToReviewComment(r.Context(), actorFrom(r), r.PathValue("id"), in.Emoji)
}

func (s *Server) resolveComments(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ResolveInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.ResolveReviewComments(r.Context(), actorFrom(r), in)
}

func (s *Server) unresolveComments(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UnresolveReviewComments(r.Context(), actorFrom(r), in.IDs)
}
