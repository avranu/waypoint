package api

import (
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Babysitting pull requests and their review comments (WP-134, ADR2 §1-2):
// start, stop and look; the work packet; verdicts on review comments.
func (s *Server) babysitRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/pulls/{id}/babysit", a(s.babysitStatus))
	mux.HandleFunc("POST /api/v1/pulls/{id}/babysit", a(s.startBabysit))
	mux.HandleFunc("DELETE /api/v1/pulls/{id}/babysit", a(s.stopBabysit))
	mux.HandleFunc("GET /api/v1/pulls/{id}/todo", a(s.pullTodo))
	mux.HandleFunc("GET /api/v1/babysit/rounds", a(s.babysitRounds))
	mux.HandleFunc("POST /api/v1/review-comments/ack", a(s.ackComments))
	mux.HandleFunc("POST /api/v1/review-comments/fixed", a(s.fixedComments))
	mux.HandleFunc("POST /api/v1/review-comments/{id}/dismiss", a(s.dismissComment))
	s.commentRoutes(mux)
}

func (s *Server) babysitStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.BabysitStatus(r.Context(), actorFrom(r), id)
}

// babysitRounds counts babysitting rounds by class over ?days= (WP-438).
func (s *Server) babysitRounds(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	return s.svc.GetRoundStats(r.Context(), actorFrom(r), q.Get("project"), queryIntIn(r, "days", 7, 1, 365), q.Get("replay") == "1")
}

func (s *Server) startBabysit(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.BabysitInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.StartBabysit(r.Context(), actorFrom(r), id, in)
}

func (s *Server) stopBabysit(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.StopBabysit(r.Context(), actorFrom(r), id)
}

func (s *Server) pullTodo(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.PRWorkPacket(r.Context(), actorFrom(r), id)
}

func (s *Server) ackComments(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AckInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AckReviewComments(r.Context(), actorFrom(r), in)
}

func (s *Server) fixedComments(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.FixedInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.FixedReviewComments(r.Context(), actorFrom(r), in)
}

func (s *Server) dismissComment(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.DismissReviewComment(r.Context(), actorFrom(r), r.PathValue("id"), in.Note)
}
