package core

import (
	"context"
	"strings"
)

// Session pause (WP-146): a person's pause with Sessions set winds down the
// sessions it covers, not only the queue. The CLI's hooks pull it to a local
// file; the PreToolUse hooks then refuse new agents and tell the session to
// finish its step, write a hand-off and stop. Here, Waypoint stops waking
// those sessions itself: no news posted to an idle one, no keep-warm ping,
// nothing delivered at a Stop that would carry it on.

// SessionPauseState is what a session's hooks pull: the pause covering it,
// or none.
type SessionPauseState struct {
	Pause *AgentPause `json:"pause"`
	Guard *GuardState `json:"guard,omitempty"` // the live quota guard (WP-932)
}

// sessionPause is userID's pause that winds down sessions of agent, if any.
// "any" covers every session; another spec the agents it names.
// A failed read is an error, not "no pause": callers stop on it.
func (s *Service) sessionPause(ctx context.Context, userID int64, agent string) (*AgentPause, error) {
	pauses, err := s.pausesOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, p := range pauses {
		if p.Sessions && (strings.EqualFold(p.Spec, "any") || AgentMatches(p.Spec, agent)) {
			return p, nil
		}
	}
	return nil, nil
}

// pausedAgent is the agent identity a session of actor's records.
func pausedAgent(actor *Actor) string {
	return cmp(actor.agentName(), "anthropic/claude-code")
}

// SessionPause is the pause covering the actor's sessions, for the hooks.
func (s *Service) SessionPause(ctx context.Context, actor *Actor) (*SessionPauseState, error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return nil, ErrUnauthorized
	}
	p, err := s.sessionPause(ctx, actor.id(), pausedAgent(actor))
	if err != nil {
		return nil, err
	}
	return &SessionPauseState{Pause: p, Guard: s.guardState(ctx, actor.id())}, nil
}

// pausedStopNote is the line the person sees at a paused session's Stop.
func pausedStopNote(p *AgentPause) string {
	note := "Paused"
	if p.Note != "" {
		note += " (" + p.Note + ")"
	}
	return note + ": this session stops here and Waypoint doesn't wake it. Resume on Agents → Sessions, or wp agents unpause."
}

// resumeSessions posts what waited for the user's idle sessions while they
// were paused.
func (s *Service) resumeSessions(ctx context.Context, userID int64) {
	ids, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT x.id FROM agent_sessions x
		WHERE x.user_id = ? AND x.state = ? AND (
			EXISTS (SELECT 1 FROM session_messages m WHERE m.session_id = x.id AND m.state = 'queued' AND m.passive = 0 AND m.batch_until IS NULL)
			OR EXISTS (SELECT 1 FROM requests r WHERE r.session_id = x.id AND r.delivered_at IS NULL AND r.batch_until IS NULL))`, userID, SessionIdle)
	if err != nil {
		s.log.WarnContext(ctx, "resume sessions", "err", err)
		return
	}
	for _, id := range ids {
		s.postToIdleSession(ctx, id)
	}
}
