package api

import (
	"net/http"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Agent sessions (WP-D16): which agents are working where, filled by the
// CLI's hooks.
func (s *Server) sessionRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/sessions", a(s.listAgentSessions))
	mux.HandleFunc("POST /api/v1/sessions/hook", a(s.sessionHook))
	mux.HandleFunc("POST /api/v1/sessions/hook/await", a(s.sessionHookAwait))
	mux.HandleFunc("POST /api/v1/sessions/typed-start", a(s.sessionTypedStart))
	mux.HandleFunc("GET /api/v1/sessions/typed-prompts", a(s.sessionTypedPrompts))
	mux.HandleFunc("GET /api/v1/sessions/pause", a(s.sessionPause))
	mux.HandleFunc("POST /api/v1/sessions/pulse", a(s.sessionPulse))
	mux.HandleFunc("GET /api/v1/sessions/current", a(s.currentAgentSession))
	mux.HandleFunc("POST /api/v1/sessions/outstanding", a(s.sessionOutstanding))
	mux.HandleFunc("POST /api/v1/sessions/outstanding/told", a(s.sessionOutstandingTold))
	mux.HandleFunc("GET /api/v1/sessions/{id}", a(s.getAgentSession))
	// Follow-ups (WP-D22): steps that run when the agent truly finishes.
	mux.HandleFunc("GET /api/v1/sessions/{id}/follow-up", a(s.getFollowUp))
	mux.HandleFunc("PUT /api/v1/sessions/{id}/follow-up", a(s.saveFollowUp))
	mux.HandleFunc("DELETE /api/v1/sessions/{id}/follow-up", a(s.clearFollowUp))
	mux.HandleFunc("POST /api/v1/sessions/{id}/follow-up/run", a(s.runFollowUp))
}

func (s *Server) listAgentSessions(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.ListAgentSessions(r.Context(), actorFrom(r), core.AgentSessionQuery{
		User: v.Get("user"), Machine: v.Get("machine"), Repo: v.Get("repo"), Branch: v.Get("branch"), Project: v.Get("project"),
		State: v.Get("state"), Agent: v.Get("agent"), Limit: queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
	})
}

func (s *Server) getAgentSession(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetAgentSession(r.Context(), actorFrom(r), id)
}

// currentAgentSession finds one of your own sessions by the CLI's session id
// (cli_id, and machine), else by the X-Waypoint-Session header.
func (s *Server) currentAgentSession(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.CurrentAgentSession(r.Context(), actorFrom(r), v.Get("cli_id"), v.Get("machine"))
}

// sessionOutstanding is the work the calling session can do while it waits on
// something slow (WP-1571): the PostToolUse hook asks for it.
func (s *Server) sessionOutstanding(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SessionOutstanding(r.Context(), actorFrom(r), core.OutstandingAtWait)
}

// sessionOutstandingTold: the wait hook has given the decisions it was sent
// (request_ids), so they are marked told.
func (s *Server) sessionOutstandingTold(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Requests []int64  `json:"requests"`
		Findings []string `json:"findings"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := s.svc.SessionFindingsNamed(r.Context(), actorFrom(r), in.Findings); err != nil {
		return nil, err
	}
	return nil, s.svc.SessionDecisionsTold(r.Context(), actorFrom(r), in.Requests)
}

// sessionHook takes the events an agent's CLI hooks report, one call for a
// batch (events that couldn't be sent earlier come first).
func (s *Server) sessionHook(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Events []core.HookEvent `json:"events"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SessionHook(r.Context(), actorFrom(r), in.Events)
}

// sessionTypedStart: the text a session is about to be started with, noted
// before the session exists so its first prompt is known as Waypoint's
// (WP-1370).
func (s *Server) sessionTypedStart(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.RecordTypedStart(r.Context(), actorFrom(r), in.Text)
}

// sessionTypedPrompts: the digests of the texts Waypoint has typed into the
// session (?cli=ID) that no prompt has used yet, for a hook that sends no
// prompt text to match locally (WP-1370).
func (s *Server) sessionTypedPrompts(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.PendingTypedPrompts(r.Context(), actorFrom(r), r.URL.Query().Get("cli"))
}

// sessionPause is the pause winding the caller's sessions down, or null:
// the hooks pull it to a local file (WP-146).
func (s *Server) sessionPause(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SessionPause(r.Context(), actorFrom(r))
}

// sessionPulse: a machine's hooks report their sessions' spend and the
// quota guard's trips, and get the pause and the guard back (WP-932).
func (s *Server) sessionPulse(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.PulseInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SessionPulse(r.Context(), actorFrom(r), in)
}

// sessionHookAwait is a Stop hook waiting a few seconds for the message of a
// follow-up its stop set off (WP-D22).
func (s *Server) sessionHookAwait(w http.ResponseWriter, r *http.Request) (any, error) {
	waits(r)
	var in struct {
		SessionID string `json:"session_id"`
		Machine   string `json:"machine"`
		Wait      int    `json:"wait"` // seconds
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AwaitMessage(r.Context(), actorFrom(r), in.SessionID, in.Machine, time.Duration(in.Wait)*time.Second)
}

func (s *Server) getFollowUp(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetFollowUp(r.Context(), actorFrom(r), id)
}

func (s *Server) saveFollowUp(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.FollowUpInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SaveFollowUp(r.Context(), actorFrom(r), id, in)
}

func (s *Server) clearFollowUp(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.ClearFollowUp(r.Context(), actorFrom(r), id)
}

func (s *Server) runFollowUp(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.RunFollowUpNow(r.Context(), actorFrom(r), id)
}
