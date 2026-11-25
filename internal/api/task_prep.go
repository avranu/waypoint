package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Task prep (WP-212, WP-ADR8): runners ask which tasks to prepare and
// upload a code bundle for each; wp next and wp brief read the result.

// maxPrepBody bounds one bundle upload; the service holds it to less.
const maxPrepBody = 1 << 20

func (s *Server) prepDue(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.PrepDue(r.Context(), actorFrom(r), r.URL.Query().Get("machine"), queryInt(r, "limit", 0))
}

func (s *Server) uploadPrep(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.PrepBundle
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPrepBody)).Decode(&in); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return s.svc.UploadPrep(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) getPrep(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	shown := q.Get("shown") == "1"
	p, err := s.svc.GetPrep(r.Context(), actorFrom(r), r.PathValue("key"), shown)
	// Only a wp that can print the evidence asks for it, so no other fetch
	// counts as showing it (WP-ADR157).
	if err == nil && q.Get("evidence") == "1" {
		s.svc.AttachPrepEvidence(r.Context(), actorFrom(r), p, shown)
	}
	return p, err
}
