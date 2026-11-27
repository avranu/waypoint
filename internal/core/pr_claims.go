package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxCrossProjectReason keeps a pasted essay out of the task's history.
const maxCrossProjectReason = 500

// claimPR claims a pull request for the session that has just pushed to or
// armed it through wp pr, the way wp next claims a task (WP-117). It returns
// a warning when another live session holds it, and that session keeps it;
// the action has gone ahead. A claim is only ever a warning, so a failure
// here is logged and never gets in the way. Waypoint's own rounds and people
// outside a session (Actor.Session 0) neither claim nor contest one.
func (s *Service) claimPR(ctx context.Context, actor *Actor, prID int64) string {
	if actor == nil || actor.Session == 0 {
		return ""
	}
	warning := ""
	err := s.write(ctx, actor, func(t *tx) error {
		var err error
		if warning, err = s.prHeldElsewhere(ctx, t, actor, prID); err != nil || warning != "" {
			return err
		}
		now := s.nowMS()
		_, err = t.ExecContext(ctx, `INSERT INTO pr_claims(pull_id, session_id, claimed_at, expires_at) VALUES (?,?,?,?)
			ON CONFLICT(pull_id) DO UPDATE SET
				claimed_at = CASE WHEN session_id = excluded.session_id THEN claimed_at ELSE excluded.claimed_at END,
				session_id = excluded.session_id, expires_at = excluded.expires_at`,
			prID, actor.Session, now, now+ClaimLease.Milliseconds())
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "claim pull request", "pull", prID, "err", err)
		return s.babysitActiveWarning(ctx, s.db.R, prID)
	}
	return joinNotes(warning, s.babysitActiveWarning(ctx, s.db.R, prID))
}

// prClaimWarning is claimPR's warning without claiming, for an action that
// may yet fail or that ends the claim by succeeding.
func (s *Service) prClaimWarning(ctx context.Context, actor *Actor, prID int64) string {
	if actor == nil || actor.Session == 0 {
		return s.babysitActiveWarning(ctx, s.db.R, prID)
	}
	warning, err := s.prHeldElsewhere(ctx, s.db.R, actor, prID)
	if err != nil {
		s.log.WarnContext(ctx, "read pull request claim", "pull", prID, "err", err)
		return s.babysitActiveWarning(ctx, s.db.R, prID)
	}
	return joinNotes(warning, s.babysitActiveWarning(ctx, s.db.R, prID))
}

// babysitActiveWarning warns a session or caller when Waypoint's babysitter
// currently has a queued or running round on prID (WP-1982, WP-2021).
func (s *Service) babysitActiveWarning(ctx context.Context, q queryer, prID int64) string {
	var round, number int
	var jobID int64
	var state string
	err := q.QueryRowContext(ctx, `SELECT b.round, j.id, j.state, p.number
		FROM babysits b
		JOIN runner_jobs j ON j.id = b.job_id
		JOIN pull_requests p ON p.id = b.pr_id
		WHERE b.pr_id = ? AND b.ended_at IS NULL AND j.state IN ('queued', 'running')
		ORDER BY b.id DESC LIMIT 1`, prID).Scan(&round, &jobID, &state, &number)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("Waypoint's babysitter currently has round %d %s on this pull request (job #%d). Wait for it to finish or run `wp pr babysit stop %d` before pushing so you don't collide.", round, state, jobID, number)
}

// prHeldElsewhere is the warning for a pull request another live session
// holds, "" when none does. A claim lasts ClaimLease past its last renewal
// or its holder's last sign of life, whichever is later: a session still
// working keeps it, as reporting activity keeps a task's claim.
func (s *Service) prHeldElsewhere(ctx context.Context, q queryer, actor *Actor, prID int64) (string, error) {
	now := s.nowMS()
	var holder, until, seen int64
	var state, title, cwd, machine string
	err := q.QueryRowContext(ctx, `SELECT c.session_id, c.expires_at, x.state, x.last_seen_at, coalesce(x.title, ''), coalesce(x.cwd, ''), coalesce(x.machine, '')
		FROM pr_claims c JOIN agent_sessions x ON x.id = c.session_id WHERE c.pull_id = ?`, prID).
		Scan(&holder, &until, &state, &seen, &title, &cwd, &machine)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if holder == actor.Session || max(until, seen+ClaimLease.Milliseconds()) <= now || sessionState(state, seen, now) == SessionEnded {
		return "", nil
	}
	who := "another session"
	if title != "" {
		who = "session “" + title + "”"
	}
	return sessionHoldsText(who, cwd, machine, seen, now) + ". This went ahead anyway: check you aren't working over each other.", nil
}

// NoteCrossProject keeps on the task's history why it was taken from outside
// the taker's project (WP-117). The claim has already happened, so a failure
// is logged, not returned. Coming back to a task it already holds says
// nothing new.
func (s *Service) NoteCrossProject(ctx context.Context, actor *Actor, reason string, task *AgentTask) {
	reason = strings.TrimSpace(reason)
	if reason == "" || task == nil || task.Resumed {
		return
	}
	if utf8.RuneCountInString(reason) > maxCrossProjectReason {
		reason = string([]rune(reason)[:maxCrossProjectReason])
	}
	issues := append([]*Issue{task.Issue.Issue}, task.Related...)
	err := s.write(ctx, actor, func(t *tx) error {
		for _, i := range issues {
			t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "cross-project reason", New: reason})
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "record cross-project reason", "issue", task.Issue.Key, "err", err)
	}
}
