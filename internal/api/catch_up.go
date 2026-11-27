package api

import "net/http"

// catchUp lists it: GET /catch-up?project=KEY.
func (s *Server) catchUp(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.CatchUp(r.Context(), actorFrom(r), r.URL.Query().Get("project"))
}

// addTryLink records a page where an issue's change shows:
// POST /issues/{key}/try-links {"url": "https://…" or "/path"}.
func (s *Server) addTryLink(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddTryLink(r.Context(), actorFrom(r), r.PathValue("key"), in.URL)
}

// notFinished reopens a finished task: POST /issues/{key}/not-finished
// {"note": "…"}.
func (s *Server) notFinished(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.NotFinished(r.Context(), actorFrom(r), r.PathValue("key"), in.Note)
}

// reportProblem files a bug about a finished task:
// POST /issues/{key}/problems {"note": "…"}; a screenshot goes to the new
// bug's attachments.
func (s *Server) reportProblem(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.ReportProblem(r.Context(), actorFrom(r), r.PathValue("key"), in.Note)
}
