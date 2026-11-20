package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A task given back (WP-776, WP-ADR75). What it waits for, if the agent said,
// is a hold (hold_when.go). With nothing said it waits a little anyway, and
// longer each time it comes back, so one task can't eat every session.

const (
	releaseBaseWait = 30 * time.Minute
	releaseMaxWait  = 24 * time.Hour
)

// ReleaseInput is a give-back: who, why, and what the task waits for.
type ReleaseInput struct {
	Agent   string `json:"agent"`
	Comment string `json:"comment"`
	HoldInput
}

// ReleaseResult is what happened to a task given back.
type ReleaseResult struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// Asks whether a person has stepped in on a task since release r: their
// comment or a review. Ends r's wait and starts the count again.
const releasePersonSince = `EXISTS(SELECT 1 FROM comments c WHERE c.issue_id = r.issue_id AND ((c.agent = '' AND c.author_id IS NOT NULL AND c.created_at > r.released_at) OR coalesce(c.reviewed_at, 0) > r.released_at))
	OR EXISTS(SELECT 1 FROM issues ri WHERE ri.id = r.issue_id AND (coalesce(ri.reviewed_at, 0) > r.released_at OR coalesce(ri.close_reviewed_at, 0) > r.released_at))`

// AgentRelease gives a claim back without finishing, saying why and what it
// waits for, if anything.
func (s *Service) AgentRelease(ctx context.Context, actor *Actor, ref string, in ReleaseInput) (*ReleaseResult, error) {
	id, err := agentIdentity(actor, in.Agent)
	if err != nil {
		return nil, err
	}
	until, err := s.holdUntil(actor, in.Until)
	if err != nil {
		return nil, err
	}
	res := &ReleaseResult{}
	var projectID int64
	err = s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if i.ClaimedBy != id {
			return invalid("%s isn't claimed by %s", i.Key, id)
		}
		res.Key, projectID = i.Key, i.ProjectID
		c := strings.TrimSpace(in.Comment)
		if c != "" {
			if _, err := s.addComment(ctx, t, actor, i, c); err != nil {
				return err
			}
		}
		if until != nil || in.conditional() {
			when, err := s.holdWhenFor(ctx, t, actor, i, in.HoldInput)
			if err != nil {
				return err
			}
			if err := s.personsHold(actor, i); err != nil {
				return err
			}
			note := cmp(strings.TrimSpace(in.Note), excerpt(c, 160))
			if err := s.placeHold(ctx, t, actor, i, until, when, note); err != nil {
				return err
			}
			h := &Hold{When: when}
			if until != nil {
				u := msTime(*until)
				h.Until = &u
			}
			res.Message = fmt.Sprintf("Released %s. It's on hold %s, so nobody gets it until then (wp hold %s --off lifts it).", i.Key, holdSpan(h, s.actorLocation(actor)), i.Key)
			return nil
		}
		if err := s.endRun(ctx, t, i.ID, id, "released", nil); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ?`, i.ID); err != nil {
			return err
		}
		wait, n, err := s.nextReleaseWait(ctx, t, i.ID)
		if err != nil {
			return err
		}
		at := s.nowMS() + wait.Milliseconds()
		if err := s.recordRelease(ctx, t, i.ID, id, &at); err != nil {
			return err
		}
		res.Message = fmt.Sprintf("Released %s. It isn't handed out for %s%s; a person's comment ends that, and wp next %s takes it by name. To say what it waits for instead: wp release takes --until, --after, --request, --reviewed or --person.",
			i.Key, waitWords(wait), releaseCount(n), i.Key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if actor.Session != 0 && s.autopilotSessionNote(ctx, actor.Session, projectID) != "" {
		res.Message += " Autopilot starts the next task after this session stops."
	}
	return res, nil
}

func releaseCount(n int) string {
	if n <= 1 {
		return ""
	}
	return fmt.Sprintf(" (given back %d times in a row, and each one doubles the wait)", n)
}

// waitWords says a wait the way the notes do: "30 minutes", "2 hours".
func waitWords(d time.Duration) string {
	if d < time.Hour {
		return plural1(max(1, int(d.Round(time.Minute).Minutes())), "minute")
	}
	return plural1(int(d.Round(time.Hour).Hours()), "hour")
}

// nextReleaseWait is how long a release made now keeps the task from being
// handed out, and how many in a row it makes.
func (s *Service) nextReleaseWait(ctx context.Context, q queryer, issueID int64) (time.Duration, int, error) {
	// Releases since the last time a person stepped in or a run finished it.
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM task_releases r WHERE r.issue_id = ? AND r.until_at IS NOT NULL AND NOT (`+releasePersonSince+`)
		AND NOT EXISTS(SELECT 1 FROM agent_runs d WHERE d.issue_id = r.issue_id AND d.outcome = 'done' AND d.ended_at > r.released_at)`, issueID).Scan(&n); err != nil {
		return 0, 0, err
	}
	wait := releaseBaseWait
	for k := 0; k < n && wait < releaseMaxWait; k++ {
		wait *= 2
	}
	return min(wait, releaseMaxWait), n + 1, nil
}

// recordRelease notes that the run the agent just ended on the issue was
// given back on purpose. until nil: a hold decides when it comes back.
func (s *Service) recordRelease(ctx context.Context, t *tx, issueID int64, agent string, until *int64) error {
	var run, session sql.NullInt64
	err := t.QueryRowContext(ctx, `SELECT id, session_id FROM agent_runs WHERE issue_id = ? AND agent = ? AND outcome = 'released' ORDER BY id DESC LIMIT 1`, issueID, agent).Scan(&run, &session)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = t.ExecContext(ctx, `INSERT INTO task_releases(issue_id, agent, session_id, run_id, released_at, until_at) VALUES (?,?,?,?,?,?)`,
		issueID, agent, session, run, s.nowMS(), nullInt(until))
	return err
}

// releaseWaits is the issues a release keeps from being handed out, and until when.
func (s *Service) releaseWaits(ctx context.Context, q queryer) (map[int64]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.issue_id, max(r.until_at) FROM task_releases r WHERE r.until_at > ? AND NOT (`+releasePersonSince+`) GROUP BY r.issue_id`, s.nowMS())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int64{}
	for rows.Next() {
		var id, until int64
		if err := rows.Scan(&id, &until); err != nil {
			return nil, err
		}
		out[id] = until
	}
	return out, rows.Err()
}

// releaseWaitOn is one issue's release wait: when it ends, 0 if there is none.
func (s *Service) releaseWaitOn(ctx context.Context, q queryer, issueID int64) (int64, error) {
	var until sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT max(r.until_at) FROM task_releases r WHERE r.issue_id = ? AND r.until_at > ? AND NOT (`+releasePersonSince+`)`, issueID, s.nowMS()).Scan(&until); err != nil {
		return 0, err
	}
	return until.Int64, nil
}

// inReleaseWait: a release keeps the task from being handed out.
func (s *Service) inReleaseWait(ctx context.Context, issueID int64) bool {
	until, err := s.releaseWaitOn(ctx, s.db.R, issueID)
	return err != nil || until != 0 // unsure means it waits
}

// endReleaseWait lets a task out of its wait now, for a person's wp hold
// --off; false when no release keeps it. NULL rather than now, so the count
// of releases in a row starts again too.
func (s *Service) endReleaseWait(ctx context.Context, t *tx, issueID int64) (bool, error) {
	if until, err := s.releaseWaitOn(ctx, t, issueID); err != nil || until == 0 {
		return false, err
	}
	_, err := t.ExecContext(ctx, `UPDATE task_releases SET until_at = NULL WHERE issue_id = ? AND until_at IS NOT NULL`, issueID)
	return true, err
}
