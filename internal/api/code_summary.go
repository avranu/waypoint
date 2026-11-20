package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// maxCodeSummaryBody bounds one summary upload (WP-182): thousands of files
// and co-change pairs as JSON.
const maxCodeSummaryBody = 24 << 20

// uploadCodeSummary replaces the code summary of an attached repo.
func (s *Server) uploadCodeSummary(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.CodeSummaryUpload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCodeSummaryBody)).Decode(&in); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return s.svc.UploadCodeSummary(r.Context(), actorFrom(r), r.PathValue("key"), id, &in)
}

// codeSummary reads it, with the numbers of the files named by ?file=
// (repeatable), or the top hotspots.
func (s *Server) codeSummary(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.CodeSummary(r.Context(), actorFrom(r), r.PathValue("key"), id, r.URL.Query()["file"])
}
