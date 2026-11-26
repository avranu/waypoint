package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

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
	return s.svc.GetPrep(r.Context(), actorFrom(r), r.PathValue("key"), r.URL.Query().Get("shown") == "1")
}
