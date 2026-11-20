package api

import (
	"net/http"
	"strconv"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The audit log: what people decided. (WP-D15)
func (s *Server) auditRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/audit", a(s.listAudit))
	mux.HandleFunc("GET /api/v1/audit.csv", a(s.auditCSV))
}

func auditQuery(r *http.Request) core.AuditQuery {
	v := r.URL.Query()
	before, _ := strconv.ParseInt(v.Get("before"), 10, 64)
	return core.AuditQuery{
		Project:  v.Get("project"),
		User:     v.Get("user"),
		Category: v.Get("category"),
		Agent:    v.Get("agent"),
		Days:     queryInt(r, "days", 0),
		Before:   before,
		Limit:    queryInt(r, "limit", 50),
	}
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListAudit(r.Context(), actorFrom(r), auditQuery(r))
}

func (s *Server) auditCSV(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.svc.AuditCSV(r.Context(), actorFrom(r), auditQuery(r))
	if err != nil {
		return nil, err
	}
	w.Header().Set("Content-Disposition", `attachment; filename="waypoint-audit-`+time.Now().UTC().Format("2006-01-02")+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	return rawResponse{contentType: "text/csv; charset=utf-8", body: b}, nil
}
