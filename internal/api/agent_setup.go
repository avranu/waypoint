package api

import (
	"net/http"
	"strconv"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Agent setup (WP-208, WP-ADR44): every repo's and machine's agent files,
// the shared instructions and publishing them, editing a repo's file
// through a pull request, and machines reporting themselves.
func (s *Server) agentSetupRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/agent-setup", a(s.getAgentSetup))
	mux.HandleFunc("GET /api/v1/agent-setup/file", a(s.getAgentFile))
	mux.HandleFunc("PUT /api/v1/agent-setup/file", a(s.editAgentFile))
	mux.HandleFunc("GET /api/v1/agent-setup/machine-file", a(s.getMachineFile))
	mux.HandleFunc("PUT /api/v1/agent-setup/shared", a(s.saveShared))
	mux.HandleFunc("POST /api/v1/agent-setup/publish", a(s.publishShared))
	mux.HandleFunc("POST /api/v1/agent-setup/refresh", a(s.refreshAgentSetup))
	mux.HandleFunc("POST /api/v1/agent-setup/machines", a(s.reportMachine))
}

func (s *Server) getAgentSetup(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetAgentSetup(r.Context(), actorFrom(r))
}

func (s *Server) getAgentFile(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	forge, _ := strconv.ParseInt(q.Get("forge"), 10, 64)
	return s.svc.GetAgentFile(r.Context(), actorFrom(r), forge, q.Get("repo"), q.Get("path"))
}

func (s *Server) editAgentFile(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AgentFileEdit
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.EditAgentFile(r.Context(), actorFrom(r), in)
}

func (s *Server) getMachineFile(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	user, _ := strconv.ParseInt(q.Get("user"), 10, 64)
	return s.svc.GetMachineFile(r.Context(), actorFrom(r), user, q.Get("machine"), q.Get("path"))
}

func (s *Server) saveShared(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SaveSharedInstructions(r.Context(), actorFrom(r), in.Text)
}

func (s *Server) publishShared(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.PublishShared(r.Context(), actorFrom(r))
}

func (s *Server) refreshAgentSetup(w http.ResponseWriter, r *http.Request) (any, error) {
	if err := s.svc.RefreshAgentSetupNow(r.Context(), actorFrom(r)); err != nil {
		return nil, err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil, nil
}

func (s *Server) reportMachine(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.MachineReport
	if err := decodeLarge(w, r, &in); err != nil { // a newer wp may say more
		return nil, err
	}
	if err := s.svc.ReportMachine(r.Context(), actorFrom(r), in); err != nil {
		return nil, err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil, nil
}
