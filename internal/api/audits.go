package api

import (
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Audits (WP-348, WP-ADR20): a model on the asker's runner, Grok through
// Cursor by default, reads a repository, branch, commit, range or pull
// request and reports findings; it never changes anything.
func (s *Server) codeAuditRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/audits", a(s.listAudits))
	mux.HandleFunc("POST /api/v1/audits", a(s.requestAudit))
	mux.HandleFunc("GET /api/v1/audits/defaults", a(s.auditDefaults))
	mux.HandleFunc("GET /api/v1/audits/nitpicks", a(s.openNitpicks))
	mux.HandleFunc("POST /api/v1/audits/findings", a(s.markAuditFindings))
	mux.HandleFunc("GET /api/v1/audits/{id}", a(s.getAudit))
	mux.HandleFunc("POST /api/v1/audits/{id}/rerun", a(s.rerunAudit))
	mux.HandleFunc("POST /api/v1/audits/{id}/cancel", a(s.cancelAudit))
	mux.HandleFunc("POST /api/v1/audits/{id}/fix", a(s.fixAudit))
}

func (s *Server) listAudits(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.ListAudits(r.Context(), actorFrom(r), core.AuditFilter{
		Project: v.Get("project"), PRID: int64(queryInt(r, "pr_id", 0)), Status: v.Get("status"), Limit: queryIntIn(r, "limit", 50, 1, 200),
	})
}

func (s *Server) requestAudit(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AuditInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Via == "" {
		in.Via = "api"
	}
	return s.svc.RequestAudit(r.Context(), actorFrom(r), in)
}

// openNitpicks lists the nitpicks still open on files (WP-695): wp next
// shows them for the files a task's brief names.
func (s *Server) openNitpicks(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.OpenNitpicks(r.Context(), actorFrom(r), v.Get("project"), v["path"], queryIntIn(r, "limit", 10, 1, 50))
}

func (s *Server) auditDefaults(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.AuditDefaults(r.Context()), nil
}

func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetAudit(r.Context(), actorFrom(r), id)
}

func (s *Server) rerunAudit(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Via string `json:"via"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.RerunAudit(r.Context(), actorFrom(r), id, cmpVia(in.Via))
}

func (s *Server) cancelAudit(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.CancelAudit(r.Context(), actorFrom(r), id)
}

func (s *Server) fixAudit(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.FixAudit(r.Context(), actorFrom(r), id)
}

func (s *Server) markAuditFindings(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.MarkFindings
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.MarkAuditFindings(r.Context(), actorFrom(r), in)
}

// reportReview is wp audit report: what the review a session ran itself
// found, recorded as its round's findings (WP-1320).
func (s *Server) reportReview(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ReviewReportInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.ReportReview(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func cmpVia(v string) string {
	switch v {
	case "web", "cli", "api":
		return v
	}
	return "api"
}
