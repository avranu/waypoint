package api

import (
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

func (s *Server) listInstructions(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListInstructions(r.Context(), actorFrom(r), r.URL.Query().Get("project"))
}

func (s *Server) previewInstructions(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.PreviewInstructions(r.Context(), actorFrom(r), v.Get("project"), core.InstructionAudience{
		Scope: v.Get("scope"), Model: v.Get("model"), AgentType: v.Get("agent_type"), Compact: queryBool(r, "compact")})
}

func (s *Server) getInstruction(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetInstruction(r.Context(), actorFrom(r), id)
}

func (s *Server) createInstruction(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.InstructionInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateInstruction(r.Context(), actorFrom(r), in)
}

func (s *Server) updateInstruction(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.InstructionInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Project != "" {
		return nil, &core.InvalidError{Msg: "an instruction stays in its project: add a new one there"}
	}
	return s.svc.UpdateInstruction(r.Context(), actorFrom(r), id, in)
}

func (s *Server) reviewInstruction(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Rev     int   `json:"rev"`
		Approve *bool `json:"approve"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Approve == nil {
		return nil, &core.InvalidError{Msg: "approve: true or false"}
	}
	return s.svc.ReviewInstruction(r.Context(), actorFrom(r), id, in.Rev, *in.Approve)
}

func (s *Server) deleteInstruction(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteInstruction(r.Context(), actorFrom(r), id)
}
