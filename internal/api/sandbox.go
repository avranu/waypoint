package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// errPersonUI is what a token hears on a personUI route.
var errPersonUI = fmt.Errorf("sandbox settings are changed in the Waypoint UI: %w", core.ErrForbidden)

// personUI wraps a route only a person in the browser may use: a session
// cookie with X-Waypoint-Client, never a token. Each is permNever too, and
// TestPersonUIRoutes finds them by this wrapper.
func (s *Server) personUI(f apiFunc) http.HandlerFunc {
	authed := s.authed(f)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			s.writeError(w, r, errPersonUI)
			return
		}
		authed(w, r)
	}
}

func (s *Server) sandboxRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/projects/{key}/sandbox", a(s.projectSandbox))
	mux.HandleFunc("GET /api/v1/sandbox/machines", a(s.sandboxMachines))
	mux.HandleFunc("GET /api/v1/sandbox/log", a(s.sandboxLog))
	mux.HandleFunc("GET /api/v1/sandbox/key", a(s.sandboxKey))
	mux.HandleFunc("PATCH /api/v1/projects/{key}/sandbox", s.personUI(s.setProjectSandbox))
	mux.HandleFunc("PATCH /api/v1/sandbox/machines/{id}", s.personUI(s.setSandboxMachine))
	mux.HandleFunc("POST /api/v1/sandbox/log/{id}/revert", s.personUI(s.revertSandboxChange))
	mux.HandleFunc("POST /api/v1/sandbox/machines/{id}/ops", s.personUI(s.requestSandboxOp))
	mux.HandleFunc("POST /api/v1/sandbox/machines/{id}/drill", s.personUI(s.startSandboxDrill))
	mux.HandleFunc("POST /api/v1/me/reauth", s.personUI(s.reauth))
	mux.HandleFunc("GET /api/v1/me/reauth", a(s.reauthStatus))
}

func (s *Server) projectSandbox(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ProjectSandbox(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) setProjectSandbox(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.SandboxPolicyInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetProjectSandbox(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

// sandboxKey is the public key a machine's sandbox helper pins.
func (s *Server) sandboxKey(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SandboxKey(r.Context(), actorFrom(r))
}

func (s *Server) sandboxMachines(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SandboxMachines(r.Context(), actorFrom(r))
}

func (s *Server) setSandboxMachine(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.SandboxMachineInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetSandboxMachine(r.Context(), actorFrom(r), id, in)
}

func (s *Server) sandboxLog(w http.ResponseWriter, r *http.Request) (any, error) {
	q := core.SandboxLogQuery{Project: r.URL.Query().Get("project")}
	if v := r.URL.Query().Get("runner"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: runner must be a number", errBadRequest)
		}
		q.Runner = n
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		q.Limit, _ = strconv.Atoi(v)
	}
	return s.svc.SandboxLog(r.Context(), actorFrom(r), q)
}

func (s *Server) revertSandboxChange(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.RevertSandboxChange(r.Context(), actorFrom(r), id, in.Reason)
}

// requestSandboxOp asks a machine's sandbox helper to remove a machine
// user or finish interrupted work (WP-448).
func (s *Server) requestSandboxOp(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.SandboxOpInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.RequestSandboxOp(r.Context(), actorFrom(r), id, in)
}

// startSandboxDrill runs the sandbox escape drill on a machine now
// (WP-473).
func (s *Server) startSandboxDrill(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.StartSandboxDrill(r.Context(), actorFrom(r), id)
}

type reauthState struct {
	Until *time.Time `json:"until"`
}

// reauth checks the person's password again. Failures count against the
// same limits as signing in, so it can't be used to guess a password.
func (s *Server) reauth(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	a := actorFrom(r)
	ip := s.clientIP(r)
	if s.loginAddrBlocked(ip) {
		return nil, errTooMany
	}
	if blocked, err := s.loginBlocked(r.Context(), userAcct(a.User.ID), ip); err != nil {
		return nil, err
	} else if blocked {
		return nil, errTooMany
	}
	until, err := s.svc.Reauth(r.Context(), a, in.Password)
	var ie *core.InvalidError
	if errors.As(err, &ie) {
		s.loginFailed(userAcct(a.User.ID), ip)
	}
	if err != nil {
		return nil, err
	}
	return reauthState{Until: &until}, nil
}

func (s *Server) reauthStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	return reauthState{Until: s.svc.ReauthUntil(r.Context(), actorFrom(r))}, nil
}
