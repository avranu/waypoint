package api

import (
	"net/http"
	"strconv"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Autopilot (WP-194, WP-ADR9): a person's settings per project, what it is
// doing about their sessions, and the summaries agents left.
func (s *Server) autopilotRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/autopilot", a(s.autopilotOverview))
	mux.HandleFunc("PUT /api/v1/autopilot", a(s.setAutopilotDefaults))
	mux.HandleFunc("PUT /api/v1/autopilot/projects/{key}", a(s.setAutopilotProject))
	mux.HandleFunc("POST /api/v1/autopilot/off", a(s.turnOffAutopilot))
	mux.HandleFunc("POST /api/v1/autopilot/steps/{id}/{action}", a(s.autopilotStepAction))
	mux.HandleFunc("GET /api/v1/autopilot/steps/{id}/wanted", a(s.autopilotStepWanted))
	mux.HandleFunc("POST /api/v1/autopilot/sessions/{id}/start", a(s.autopilotStartSession))
	mux.HandleFunc("GET /api/v1/autopilot/activity", a(s.autopilotActivity))
	mux.HandleFunc("GET /api/v1/autopilot/summaries", a(s.taskSummaries))
	mux.HandleFunc("POST /api/v1/autopilot/summaries/reviewed", a(s.reviewTaskSummaries))
	// Who can review a round, and the reviewer's default brief (WP-1320).
	mux.HandleFunc("GET /api/v1/autopilot/review-models", a(s.reviewModelOptions))
	// Its wording (WP-308): for every project, and as one project uses it.
	mux.HandleFunc("GET /api/v1/autopilot/text", a(s.autopilotText))
	mux.HandleFunc("PUT /api/v1/admin/autopilot-text", a(s.setAutopilotText))
	mux.HandleFunc("GET /api/v1/projects/{key}/autopilot-text", a(s.projectAutopilotText))
	// When sessions are told of stalled background jobs (WP-1045).
	mux.HandleFunc("GET /api/v1/stalled-jobs/policy", a(s.stallPolicy))
	mux.HandleFunc("PUT /api/v1/admin/stalled-jobs", a(s.setStallPolicy))
	mux.HandleFunc("GET /api/v1/projects/{key}/stalled-jobs", a(s.projectStallPolicy))
}

func (s *Server) stallPolicy(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetStallPolicy(r.Context(), actorFrom(r))
}

// setStallPolicy: {"policy": {...}} sets it for every project; {"policy":
// null} goes back to the default.
func (s *Server) setStallPolicy(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Policy *core.StallPolicy `json:"policy"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetStallPolicy(r.Context(), actorFrom(r), in.Policy)
}

func (s *Server) projectStallPolicy(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ProjectStallPolicy(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) reviewModelOptions(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ReviewModelOptions(r.Context(), actorFrom(r))
}

func (s *Server) autopilotText(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GetAutopilotText(r.Context(), actorFrom(r))
}

func (s *Server) setAutopilotText(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AutopilotTextInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetAutopilotText(r.Context(), actorFrom(r), in)
}

func (s *Server) projectAutopilotText(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ProjectAutopilotText(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) autopilotOverview(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.AutopilotOverview(r.Context(), actorFrom(r))
}

func (s *Server) setAutopilotDefaults(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AutopilotSettingInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetAutopilot(r.Context(), actorFrom(r), "", in)
}

// turnOffAutopilot: autopilot off in every project at once.
func (s *Server) turnOffAutopilot(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.TurnOffAutopilot(r.Context(), actorFrom(r))
}

func (s *Server) setAutopilotProject(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AutopilotSettingInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetAutopilot(r.Context(), actorFrom(r), r.PathValue("key"), in)
}

// autopilotStepAction: stop what autopilot was about to do with a session,
// or do it now.
func (s *Server) autopilotStepAction(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.AutopilotStepAction(r.Context(), actorFrom(r), id, r.PathValue("action"))
}

// autopilotStartSession: start the next task in an idle session now (WP-307).
func (s *Server) autopilotStartSession(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.StartAutopilotSession(r.Context(), actorFrom(r), id)
}

// autopilotStepWanted: the runner's last check before it types into a
// session: is the step still waiting for job ?job=N.
func (s *Server) autopilotStepWanted(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	job, _ := strconv.ParseInt(r.URL.Query().Get("job"), 10, 64)
	ok, err := s.svc.AutopilotStepWanted(r.Context(), actorFrom(r), id, job)
	return map[string]bool{"wanted": ok}, err
}

func (s *Server) autopilotActivity(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	days, _ := strconv.Atoi(v.Get("days"))
	offset, _ := strconv.Atoi(v.Get("offset"))
	return s.svc.AutopilotActivity(r.Context(), actorFrom(r), days, offset)
}

func (s *Server) taskSummaries(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	q := core.TaskSummaryQuery{Project: v.Get("project"), All: v.Get("all") == "1" || v.Get("all") == "true"}
	q.Before, _ = strconv.ParseInt(v.Get("before"), 10, 64)
	q.Limit, _ = strconv.Atoi(v.Get("limit"))
	return s.svc.TaskSummaries(r.Context(), actorFrom(r), q)
}

func (s *Server) reviewTaskSummaries(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		IDs     []int64 `json:"ids"`
		All     bool    `json:"all"`
		Project string  `json:"project"`
		Unread  bool    `json:"unread"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	n, err := s.svc.ReviewTaskSummaries(r.Context(), actorFrom(r), in.IDs, in.All, in.Project, in.Unread)
	return map[string]int{"updated": n}, err
}
