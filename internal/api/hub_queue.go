package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The agent queue: what agents take next, pins and pauses. (WP-D15)
func (s *Server) queueRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/agents/queue", a(s.agentQueue))
	mux.HandleFunc("POST /api/v1/agents/queue/order", a(s.agentQueueOrder))
	mux.HandleFunc("POST /api/v1/agents/release/{key}", a(s.agentReleaseByPerson))
	mux.HandleFunc("GET /api/v1/agents/pauses", a(s.listAgentPauses))
	mux.HandleFunc("POST /api/v1/agents/pauses", a(s.pauseAgents))
	mux.HandleFunc("DELETE /api/v1/agents/pauses", a(s.resumeAgents))
	mux.HandleFunc("GET /api/v1/agents/session-places", a(s.taskSessionPlaces))
	mux.HandleFunc("POST /api/v1/agents/start/{key}", a(s.startTaskSession))
	mux.HandleFunc("GET /api/v1/agents/kickoff/{key}", a(s.taskKickoff))
}

// taskKickoff is the template a session started on the task gets its wp
// next answer in, autopilot's (WP-1037): for wp start and wp fanout.
func (s *Server) taskKickoff(w http.ResponseWriter, r *http.Request) (any, error) {
	p, err := s.svc.TaskKickoff(r.Context(), actorFrom(r), r.PathValue("key"))
	if err != nil {
		return nil, err
	}
	return map[string]string{"prompt": p}, nil
}

// taskSessionPlaces is where the person's runners can start a task session,
// per project (WP-71).
func (s *Server) taskSessionPlaces(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.TaskSessionPlaces(r.Context(), actorFrom(r))
}

// startTaskSession is the queue's Start button: a fresh session for the
// task on the person's runner, as wp start KEY --bg would start it.
func (s *Server) startTaskSession(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.svc.StartTaskSession(r.Context(), actorFrom(r), r.PathValue("key"))
	if errors.Is(err, core.ErrNoRunner) {
		return nil, &core.InvalidError{Msg: "you have no runner for that yet: connect one in Settings → Runners"}
	}
	return st, err
}

func (s *Server) agentQueue(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	q, err := s.svc.AgentQueueFor(r.Context(), actorFrom(r), v.Get("agent"), v.Get("project"))
	if err != nil || !wantsText(r) {
		return q, err
	}
	return textResponse(queueText(q, queryBool(r, "verbose"))), nil
}

func (s *Server) agentQueueOrder(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.QueueMove
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.MoveInQueue(r.Context(), actorFrom(r), in)
}

func (s *Server) agentReleaseByPerson(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.ReleaseClaim(r.Context(), actorFrom(r), r.PathValue("key"))
}

func (s *Server) listAgentPauses(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListAgentPauses(r.Context(), actorFrom(r))
}

func (s *Server) pauseAgents(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Spec     string `json:"spec"`
		Note     string `json:"note"`
		Sessions bool   `json:"sessions"` // running sessions wind down too (WP-146)
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.PauseAgents(r.Context(), actorFrom(r), in.Spec, in.Note, in.Sessions)
}

// resumeAgents ends the pause for ?spec= (every pause without one).
func (s *Server) resumeAgents(w http.ResponseWriter, r *http.Request) (any, error) {
	n, err := s.svc.ResumeAgents(r.Context(), actorFrom(r), r.URL.Query().Get("spec"))
	if err != nil {
		return nil, err
	}
	return map[string]int{"resumed": n}, nil
}

// queueText is the queue for the CLI: the order next hands work out, why
// each is there, and what waits; long tails and the ordering rules need
// verbose.
func queueText(q *core.AgentQueue, verbose bool) string {
	var b strings.Builder
	who := "every agent"
	switch {
	case q.Agent != "" && q.Exact && len(q.Covers) == 1:
		who = q.Covers[0]
	case q.Agent != "":
		who = q.Agent + " (" + strings.Join(q.Covers, ", ") + ")"
	}
	fmt.Fprintf(&b, "Queue for %s\n", who)
	for _, p := range q.Pauses {
		line := "PAUSED: agents matching " + p.Spec
		if strings.EqualFold(p.Spec, "any") {
			line = "PAUSED: all your agents"
		}
		if p.Note != "" {
			line += ": " + p.Note
		}
		b.WriteString(line + " (wp agents unpause " + p.Spec + ")\n")
	}
	if len(q.PausedProjects) > 0 {
		b.WriteString("Agents paused in " + strings.Join(q.PausedProjects, ", ") + "\n")
	}
	if len(q.Working) > 0 {
		b.WriteString("\nWorking now:\n")
		for _, it := range q.Working {
			fmt.Fprintf(&b, "  %s %s  (%s, %s)\n", it.Issue.Key, it.Issue.Title, it.Issue.ClaimedBy, humanDuration(it.Seconds))
		}
	}
	b.WriteString("\nUp next (wp next takes them in this order):\n")
	if len(q.Next) == 0 {
		b.WriteString("  nothing\n")
	}
	for i, it := range q.Next {
		if i == 15 && !verbose {
			fmt.Fprintf(&b, "  +%d more (-v)\n", len(q.Next)-i)
			break
		}
		line := fmt.Sprintf("  %2d. %s %s  [%s]", it.Position, it.Issue.Key, it.Issue.Title, it.Why)
		if q.Agent == "" || !q.Exact {
			line += " for " + core.AgentLabel(it.Issue.Agent)
		}
		b.WriteString(line + "\n")
	}
	if len(q.Waiting) > 0 {
		b.WriteString("\nWaiting:\n")
		for i, it := range q.Waiting {
			if i == 8 && !verbose {
				fmt.Fprintf(&b, "  +%d more (-v)\n", len(q.Waiting)-i)
				break
			}
			fmt.Fprintf(&b, "  %s %s  %s\n", it.Issue.Key, it.Issue.Title, it.Detail)
		}
	}
	if verbose {
		b.WriteString("\nOrder: work delegated to agents, then work anyone may take; in each, overdue, priority, due date, bugs before other work, bigger estimates first, planned work (a plan page or a task prep) first, then board order; pinned tasks (set on the Agents page) keep the places given them, ahead of the same automatic tasks.\n")
	}
	return b.String()
}
