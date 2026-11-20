package api

import (
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Fan-outs (WP-148): a plan's packages as budgeted task sessions.

func (s *Server) createFanout(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.FanoutInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateFanout(r.Context(), actorFrom(r), in)
}

func (s *Server) listFanouts(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListFanouts(r.Context(), actorFrom(r), r.URL.Query().Get("all") == "1")
}

func (s *Server) getFanout(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetFanout(r.Context(), actorFrom(r), id)
}

func (s *Server) claimFanout(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Machine string `json:"machine"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.ClaimFanout(r.Context(), actorFrom(r), id, in.Machine)
}

func (s *Server) stopFanout(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.StopFanout(r.Context(), actorFrom(r), id)
}

func (s *Server) fanoutItemStarted(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Attach string `json:"attach_id"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.svc.FanoutItemStarted(r.Context(), actorFrom(r), id, r.PathValue("key"), in.Attach)
}

func (s *Server) fanoutItemFailed(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.svc.FanoutItemFailed(r.Context(), actorFrom(r), id, r.PathValue("key"), in.Note)
}

func (s *Server) endFanoutItem(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Outcome string  `json:"outcome"`
		Spent   float64 `json:"spent"`
		Note    string  `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.svc.EndFanoutItem(r.Context(), actorFrom(r), id, r.PathValue("key"), in.Outcome, in.Spent, in.Note)
}
