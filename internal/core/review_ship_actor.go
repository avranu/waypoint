package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// shipAuthority is who the server ships as, and whether it still may
// (WP-1370). Waypoint opens the pull request for a session some minutes
// after the session's own `wp ship`, so it acts with the authority that ship
// had and still has now: the person enabled, the token the ship came with
// not revoked, expired or read-only and its permissions and scope applied
// (the same constructor as a request carrying it), a writer in the project,
// and the task still open, in the project and with the agent it was. A
// reason comes back instead of an actor when any of that has gone, and the
// caller ends the round with it (stopShip); the pull request isn't opened.
func (s *Service) shipAuthority(ctx context.Context, r *roundRow, bp *beforePR, in *shipRoundIn) (*Actor, string, error) {
	if in.user == nil {
		u, err := s.userByID(ctx, s.db.R, in.userID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, "the person who shipped it is gone", nil
			}
			return nil, "", err
		}
		in.user = u
	}
	if in.user.Disabled {
		return nil, in.user.Username + " is deactivated", nil
	}
	session := in.session
	if in.ended {
		session = 0
	}
	actor := &Actor{User: in.user, Agent: in.agent, Session: session}
	if bp.TokenID != 0 {
		t, err := s.tokenActor(ctx, "", bp.TokenID, false)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return nil, "the token the ship came with was revoked or has expired", nil
			}
			return nil, "", err
		}
		if t.User.ID != in.user.ID {
			return nil, "the token the ship came with isn't theirs any more", nil
		}
		if t.ReadOnly {
			return nil, "the token the ship came with is read-only", nil
		}
		// What the token may do, as the ship it came with could; the agent
		// and session are the round's.
		t.Agent, t.Session = actor.Agent, actor.Session
		actor = t
	}
	if err := actor.need(PermPullsWrite); err != nil {
		return nil, "the token the ship came with can't open pull requests (it lacks pulls.write)", nil
	}
	i, err := s.issueByID(ctx, s.db.R, r.issueID, false)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, "the task is gone", nil
		}
		return nil, "", err
	}
	p, err := s.projectByRef(ctx, s.db.R, bp.Project)
	if err != nil || p.ID != i.ProjectID {
		return nil, "the task isn't in " + bp.Project + " any more", nil
	}
	if p.Archived {
		return nil, p.Key + " is archived", nil
	}
	if role, err := s.projectRole(ctx, s.db.R, actor, p.ID); err != nil {
		return nil, "", err
	} else if role < RoleEditor {
		return nil, "they can't write to " + p.Key + " any more", nil
	}
	if i.Closed() {
		return nil, r.key + " is " + strings.ToLower(i.Status.Name), nil
	}
	if i.ClaimedBy != "" && i.ClaimedBy != in.agent {
		return nil, r.key + " is now held by " + i.ClaimedBy, nil
	}
	if !s.stillDelegated(ctx, i, in.agent) {
		return nil, r.key + " is no longer meant for " + in.agent, nil
	}
	return actor, "", nil
}

// stopShip ends a round whose pull request Waypoint won't open, and tells
// the person it was for, or else the task's reporter (WP-1370): never only
// a log line.
func (s *Service) stopShip(ctx context.Context, r *roundRow, in shipRoundIn, why string) error {
	return s.stopShipWith(ctx, r, in, "Waypoint did not open the pull request: "+why+".", "pull request not opened", why)
}

// stopShipWith is stopShip with its own words: body is what the round says
// and the person is told, title the notice's (after the task's key).
func (s *Service) stopShipWith(ctx context.Context, r *roundRow, in shipRoundIn, body, title, why string) error {
	now := s.nowMS()
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE work_log SET state = 'done', body = ?, ended_at = coalesce(ended_at, ?),
			read_state = ?, read_by = ?, read_at = ?, updated_at = ?, before_pr = CASE WHEN before_pr = '' THEN before_pr ELSE json_set(before_pr, '$.ship_stop', ?) END WHERE id = ?`,
			body, now, readDone, readByRules, now, now, why, r.id); err != nil {
			return err
		}
		s.emitWorkLog(ctx, t, r.issueID, r.id, RoundDone)
		// The person is told by the write that ends the ship (dispatch
		// sends it): the round ends and the notice stands or falls together.
		who := in.userID
		if in.user != nil {
			who = in.user.ID
		}
		if u, err := s.userByID(ctx, t, who); err != nil || u.Disabled {
			who = 0
			if err := t.QueryRowContext(ctx, `SELECT coalesce(reporter_id, 0) FROM issues WHERE id = ?`, r.issueID).Scan(&who); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if who == 0 {
			return nil
		}
		_, err := t.storeNotice(ctx, who, NotifyAutopilot, fmt.Sprintf("%s: %s", r.key, title), body, "/agents?tab=autopilot")
		return err
	})
	if err != nil {
		return err
	}
	// The run and the task's claim are let go: nothing is left that would try
	// this ship again, and the task is free for a person.
	if err := s.write(ctx, SystemActor(), func(t *tx) error {
		var open int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM agent_runs WHERE issue_id = ? AND agent = ? AND ended_at IS NULL`, r.issueID, in.agent).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			if err := s.endRun(ctx, t, r.issueID, in.agent, "released", nil); err != nil {
				return err
			}
		}
		// No event for the claim itself: endRun says the run ended, and a
		// claim's release is evented nowhere else either (dropClaim).
		_, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`, r.issueID, in.agent)
		return err
	}); err != nil {
		s.log.WarnContext(ctx, "release the run of a stopped ship", "round", r.id, "err", err)
	}
	s.log.Info("review: the server ship stopped", "round", r.id, "why", why)
	return nil
}

// shipAgent is the agent the server ships as: the one given, else the run
// the round belongs to, else the session's, and only then the default. A
// session of another agent isn't shipped as Claude Code (WP-1370).
func (s *Service) shipAgent(ctx context.Context, r *roundRow, agent string, session int64) string {
	if agent == "" && r.runID.Valid {
		if err := s.db.R.QueryRowContext(ctx, `SELECT agent FROM agent_runs WHERE id = ?`, r.runID.Int64).Scan(&agent); err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "read the agent of a round's run; the default is shipped as", "round", r.id, "err", err)
		}
	}
	if agent == "" && session != 0 {
		if err := s.db.R.QueryRowContext(ctx, `SELECT agent FROM agent_sessions WHERE id = ?`, session).Scan(&agent); err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "read the agent of a round's session; the default is shipped as", "round", r.id, "err", err)
		}
	}
	return cmp(agent, "anthropic/claude-code")
}
