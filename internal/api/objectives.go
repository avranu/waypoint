package api

import (
	"fmt"
	"net/http"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Objectives (WP-927, WP-ADR111, WP-ADR142): a task worked in rounds until a check is
// met or a judge finds nothing left, by an amount every round, or as a loop.
// Waypoint reads every round itself; what a worker reports never decides.

func (s *Server) getObjective(w http.ResponseWriter, r *http.Request) (any, error) {
	o, err := s.svc.GetObjective(r.Context(), actorFrom(r), r.PathValue("key"))
	if err != nil || !wantsText(r) {
		return o, err
	}
	return textResponse(o.Text), nil
}

func (s *Server) setObjective(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ObjectiveInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	o, err := s.svc.SetObjective(r.Context(), actorFrom(r), r.PathValue("key"), in)
	if err != nil || !wantsText(r) {
		return o, err
	}
	return textResponse(o.Text), nil
}

func (s *Server) confirmObjective(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Rev string `json:"rev"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	o, err := s.svc.ConfirmObjective(r.Context(), actorFrom(r), r.PathValue("key"), in.Rev)
	if err != nil || !wantsText(r) {
		return o, err
	}
	return textResponse(o.Text), nil
}

// objectiveGo is a person steering an objective in effect: retry a reading
// that couldn't be taken, move on to the next round, accept a reading that waits,
// or end it.
func (s *Server) objectiveGo(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Action string `json:"action"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	o, err := s.svc.ObjectiveGo(r.Context(), actorFrom(r), r.PathValue("key"), in.Action)
	if err != nil || !wantsText(r) {
		return o, err
	}
	return textResponse(o.Text), nil
}

// diagnoseObjective records what a diagnose round found (WP-1449).
func (s *Server) diagnoseObjective(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ObjectiveDiagnoseInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	o, err := s.svc.ObjectiveDiagnose(r.Context(), actorFrom(r), r.PathValue("key"), in)
	if err != nil || !wantsText(r) {
		return o, err
	}
	return textResponse(o.Text), nil
}

func (s *Server) clearObjective(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.ClearObjective(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) recordObjectiveCheck(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ObjectiveCheckInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.RecordObjectiveCheck(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

func (s *Server) listObjectives(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	l, err := s.svc.ListObjectives(r.Context(), actorFrom(r), v.Get("project"), v.Get("all") == "1" || v.Get("all") == "true")
	if err != nil || !wantsText(r) {
		return l, err
	}
	return textResponse(objectivesText(l)), nil
}

func objectivesText(l []*core.Objective) string {
	if len(l) == 0 {
		return "No objectives. wp objective set KEY --check 'CMD' --until '< 1s' gives a task one.\n"
	}
	var b strings.Builder
	for _, o := range l {
		state := fmt.Sprintf("round %d", o.Round)
		switch {
		case o.State != core.ObjectiveActive:
			state = o.State
		case o.MaxRounds > 0:
			state += fmt.Sprintf(" of %d", o.MaxRounds)
		}
		if o.State == core.ObjectiveActive && o.Health != "" {
			state += ", " + o.Health
		}
		fmt.Fprintf(&b, "%s %q, %s: %s", o.Key, o.Title, state, o.Summary)
		if o.State == core.ObjectiveActive {
			// What Waypoint itself last read, not what a worker reported.
			if o.Verdict != nil {
				b.WriteString(". Last reading: " + o.Verdict.Line)
			}
			if o.HealthNote != "" {
				b.WriteString(". " + strings.TrimRight(o.HealthNote, "."))
			}
		}
		b.WriteString(".\n")
	}
	return b.String()
}
