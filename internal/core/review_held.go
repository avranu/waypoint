package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A review held before its pull request (WP-688, WP-ADR60) holds up a
// session, a task and the queue behind them, so it must not be able to go
// quiet (WP-752). Three things keep it moving:
//
//   - the run stays open while it waits, whatever the session's Stop hooks
//     do (AgentPause, expireRuns), so the review always has a task to
//     belong to;
//   - a round no step is carrying is picked up again (reviveHeldRounds);
//   - a round that can't run for a reason of Waypoint's own opens the pull
//     request anyway (shipHeld), says so on the task, and the session moves
//     on. CI and the review gate still apply to it. A person cancelling the
//     review is a decision: nothing opens then, and they are told;
//   - turning autopilot off (WP-ADR85) stops the next task, not the one already
//     shipped: its review still runs, or its pull request opens without it
//     (heldOwed). Overnight on 2026-11-11 the switch skipped three held
//     reviews and their pull requests never opened.
//
// What the review is doing shows on the session (SessionReview) and in the
// task's work log.
const (
	// heldStopLost: a round this old that no step is carrying lost its Stop.
	heldStopLost = 3 * time.Minute
	// heldNoRunnerWait: how long a held review waits for a runner to appear.
	heldNoRunnerWait = 30 * time.Minute
	// heldScreenWait: how long it waits for a session's screen to come free.
	heldScreenWait = 2 * time.Hour
	// heldShipRetry: how long Waypoint keeps trying to open the pull request.
	heldShipRetry = 30 * time.Minute
	// heldReviveEvery keeps the watchdog from re-arming a step that just
	// ended without its round.
	heldReviveEvery = 10 * time.Minute
	// sessionReviewShown: how long a review that didn't finish stays on its
	// session.
	sessionReviewShown = 6 * time.Hour
	// roundCancelledBy starts the reason of a round a person cancelled.
	roundCancelledBy = "Cancelled by "
)

// runHeldForReview is true of an agent_runs row with a review before its
// pull request still to come.
const runHeldForReview = `EXISTS (SELECT 1 FROM work_log held WHERE held.kind = 'review' AND held.run_id = agent_runs.id
	AND held.before_pr != '' AND held.state IN ('pending', 'running'))`

// reviveHeldRounds gives a step back to held rounds that have none: the
// session's Stop never reached reviewFirstAtStop, or its step went away.
func (s *Service) reviveHeldRounds(ctx context.Context) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT w.id, w.session_id, w.run_id, coalesce(x.project_id, i.project_id)
		FROM work_log w JOIN issues i ON i.id = w.issue_id JOIN agent_sessions x ON x.id = w.session_id JOIN agent_runs r ON r.id = w.run_id
		WHERE w.kind = 'review' AND w.state = 'pending' AND w.before_pr != '' AND w.created_at < ? AND x.ended_at IS NULL AND r.ended_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM autopilot_steps st WHERE st.review_id = w.id AND (st.state IN ('due', 'waiting', 'starting') OR st.updated_at > ?))
			AND NOT EXISTS (SELECT 1 FROM agent_runs o WHERE o.session_id = w.session_id AND o.id > w.run_id AND o.issue_id != w.issue_id)
		LIMIT 50`, now-heldStopLost.Milliseconds(), now-heldReviveEvery.Milliseconds())
	if err != nil {
		return
	}
	type lost struct{ round, session, run, project int64 }
	var list []lost
	for rows.Next() {
		var l lost
		if rows.Scan(&l.round, &l.session, &l.run, &l.project) == nil {
			list = append(list, l)
		}
	}
	_ = rows.Close()
	for _, l := range list {
		x, err := scanSession(s.db.R.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, l.session), now)
		if err != nil || x.State == SessionWorking {
			continue // still going: its Stop decides
		}
		if err := s.write(ctx, SystemActor(), func(t *tx) error {
			return s.putStep(ctx, t, x, l.project, l.run, now, StepDue, "", "", now, l.round)
		}); err != nil {
			s.log.WarnContext(ctx, "autopilot: revive a held review", "round", l.round, "err", err)
			continue
		}
		s.log.Info("autopilot: picked up a held review nothing was carrying", "round", l.round, "session", l.session)
	}
}

// heldReviewEnded begins the reason of a round whose review ended but whose
// findings couldn't be given to the session (reviewJobFinished): the pull
// request opens without them, not without a review.
const heldReviewEnded = "the adversarial review ended (audit #"

// shipHeld does what the session's wp ship would have done after the
// review that couldn't run: it opens the pull request, arms and babysits
// it, moves the task to In Review and ends the run. Opening again finds
// the pull request already open, so a retry is safe.
func (s *Service) shipHeld(ctx context.Context, st *stepRow, x *AgentSession, user *User, agent, why string) error {
	r, err := roundByID(ctx, s.db.R, st.reviewID.Int64)
	if err != nil {
		return err
	}
	bp := roundBeforePR(ctx, s.db.R, st.reviewID.Int64)
	if r == nil || bp == nil {
		return errors.New("its review round is gone")
	}
	title := bp.Title
	if strings.TrimSpace(title) == "" {
		if err := s.db.R.QueryRowContext(ctx, `SELECT title FROM issues WHERE id = ?`, r.issueID).Scan(&title); err != nil {
			return err
		}
	}
	actor := &Actor{User: user, Agent: agent, Session: x.ID}
	// A review that ended with its findings undelivered did run.
	given := !strings.HasPrefix(why, heldReviewEnded)
	note := "Waypoint opened this without an adversarial review: " + why + "."
	if !given {
		note = "Waypoint opened this without the adversarial review's findings being given to the session: " + why + "."
	}
	// The Haiku checks' confirmed problems that no reviewing model's
	// findings carry are said, with the audit that holds them.
	var panel string
	if n, audit := s.panelConfirmed(ctx, r.id); n > 0 {
		panel = fmt.Sprintf(" Before the reviewing model failed, Waypoint's Haiku checks confirmed %s against the code; audit #%d holds them (`wp audit show %d`).", plural1(n, "problem"), audit, audit)
		note += panel
	}
	res, err := s.OpenPullRequest(ctx, actor, OpenPRInput{
		Project: bp.Project, Repo: bp.Repo, Head: bp.Branch, Base: bp.Base,
		Title: title, Body: strings.TrimSpace(bp.Body + "\n\n" + note),
		Draft: bp.Draft, Ready: bp.Ready, Urgent: bp.Urgent,
		Issue: r.key, For: r.key, Review: ShipSkip, Reason: why, noSweep: true, byWaypoint: true,
	})
	if err != nil {
		return err
	}
	if res.Held != "" {
		return errors.New(res.Held)
	}
	if res.Pull == nil {
		return errors.New("no pull request came back")
	}
	pr := res.Pull
	var notes []string
	if len(res.OtherTasks) > 0 {
		notes = append(notes, "that branch's pull request belongs to "+strings.Join(res.OtherTasks, ", ")+", so it wasn't armed")
	} else {
		if !bp.Draft && (pr.State != "draft" || pr.WipAuto) {
			if _, err := s.MergePR(ctx, actor, pr.ID, MergeInput{WhenReady: true}); err != nil {
				notes = append(notes, "couldn't arm it: "+err.Error())
			}
			if _, err := s.StartBabysit(ctx, actor, pr.ID, BabysitInput{Via: "ship"}); err != nil {
				notes = append(notes, "couldn't babysit it: "+err.Error())
			}
		}
		lead := "No adversarial review ran before this pull request"
		if !given {
			lead = "The adversarial review's findings weren't given to the session before this pull request"
		}
		status := "In Review"
		if _, err := s.UpdateIssue(ctx, actor, r.key, IssuePatch{Status: &status, ViaLink: pr.URL,
			Comment: fmt.Sprintf("%s: %s. Waypoint opened %s so the task doesn't sit unshipped; CI and the review gate still apply.%s", lead, why, pr.URL, panel)}); err != nil {
			notes = append(notes, "left the task's status alone: "+err.Error())
		}
	}
	if err := s.endHeldRun(ctx, actor, r.issueID, agent); err != nil {
		return err
	}
	body := fmt.Sprintf("%s's adversarial review didn't run (%s), so Waypoint opened %s#%d without it. CI and the review gate still apply.%s", r.key, why, pr.Repo, pr.Number, panel)
	if !given {
		body = fmt.Sprintf("%s's adversarial review findings weren't given to the session (%s), so Waypoint opened %s#%d without them. CI and the review gate still apply.%s", r.key, why, pr.Repo, pr.Number, panel)
	}
	if len(notes) > 0 {
		body += " It " + strings.Join(notes, "; ") + "."
		s.log.WarnContext(ctx, "autopilot: after opening a held pull request", "task", r.key, "notes", notes)
	}
	if bp.Urgent && pr.Urgent {
		body += fmt.Sprintf(" Its wp ship marked it urgent, so it delays other CI runs to cut the line: `wp pr urgent %d --off` takes that off.", pr.Number)
	} else if pr.Urgent && !res.Existing {
		// wp ship didn't ask for it, so the task's tag did; no one reads this
		// open's notice.
		body += " " + taggedUrgentNotice(r.key, pr.Number)
	}
	s.notifyURL(ctx, st.userID, nil, NotifyAutopilot, r.key+" opened without its adversarial review", body, "/agents?tab=autopilot")
	return nil
}

// heldOwed names the task whose review before its pull request the round
// is, while that pull request is still to come from the step carrying it:
// the run is open, the task unresolved and no pull request linked ("": no
// such round). Turning autopilot off leaves such a step to finish: run the
// review, open the pull request without it, or say why it can't.
func heldOwed(ctx context.Context, q queryer, roundID int64) (string, error) {
	if roundID == 0 {
		return "", nil
	}
	var key string
	err := q.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM work_log w JOIN agent_runs r ON r.id = w.run_id
		JOIN issues i ON i.id = w.issue_id JOIN projects p ON p.id = i.project_id
		WHERE w.id = ? AND w.before_pr != '' AND r.ended_at IS NULL AND i.resolved_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM external_links l WHERE l.issue_id = w.issue_id AND l.kind = 'pull')`, roundID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return key, err
}

// roundOwed names the task whose adversarial review round the step carries
// while turning autopilot off leaves it to run: one held before its pull
// request (heldOwed), or one asked for or running after it (roundLive).
// The switch is about the next task (WP-1007); WP-ADR94 left rounds after
// the pull request to it "for when one is missed", and from 2026-11-13 to
// 2026-11-15 the switch skipped nine of them (WP-1159, WP-ADR113).
func roundOwed(ctx context.Context, q queryer, roundID int64) (string, error) {
	if key, err := heldOwed(ctx, q, roundID); key != "" || err != nil {
		return key, err
	}
	return roundLive(ctx, q, roundID)
}

// roundLive names the task whose round it is while the round is still to
// run or running ("": it isn't, or there is no round).
func roundLive(ctx context.Context, q queryer, roundID int64) (string, error) {
	if roundID == 0 {
		return "", nil
	}
	var key string
	err := q.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM work_log w JOIN issues i ON i.id = w.issue_id JOIN projects p ON p.id = i.project_id
		WHERE w.id = ? AND w.kind = 'review' AND w.state IN (?, ?)`, roundID, RoundPending, RoundRunning).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return key, err
}

// heldGoesOnNotice tells a person who turned autopilot off what it still
// finishes there, and how to stop that too. held says which rounds hold
// their pull request back (before it); for the others none waits on the round
// (the pull request is open already, or the task's run is over), and stopping one only drops its review (audit #883).
func heldGoesOnNotice(project string, keys []string, steps []int64, held []bool) string {
	head := fmt.Sprintf("Autopilot is off in %s and starts nothing more there. ", project)
	if len(keys) == 1 {
		if held[0] {
			return head + fmt.Sprintf("%s's adversarial review was asked for when it shipped, so it still runs; a pull request held for it opens after it (or without it, if it can't run). To stop it: `wp autopilot stop %d` (a held pull request then stays unopened).", keys[0], steps[0])
		}
		return head + fmt.Sprintf("%s's adversarial review was asked for when it shipped, so it still runs; no pull request waits on it. To stop it: `wp autopilot stop %d`.", keys[0], steps[0])
	}
	stops := make([]string, len(keys))
	var heldKeys []string
	for n, key := range keys {
		stops[n] = fmt.Sprintf("`wp autopilot stop %d` (%s)", steps[n], key)
		if held[n] {
			heldKeys = append(heldKeys, key)
		}
	}
	body := fmt.Sprintf("The adversarial reviews of %s were asked for when they shipped, so they still run.", strings.Join(keys, ", "))
	switch {
	case len(heldKeys) == 0:
		body += " No pull request waits on them."
	case len(heldKeys) == len(keys):
		body += " Pull requests held for them open after them (or without them, if they can't run); stopping one leaves its pull request unopened."
	default:
		body += fmt.Sprintf(" The pull requests of %s are held for theirs and open after them (or without them, if they can't run); stopping one of those leaves its pull request unopened. None waits on the others.", strings.Join(heldKeys, ", "))
	}
	return head + body + " To stop one: " + strings.Join(stops, ", ") + "."
}

// noRunnerSince is when a held review began going without a runner: since the
// runner dropped out, not since the round was written, or one blip would end
// a review that had been waiting its turn. A runner that is online but can't
// run autopilot won't fix itself, so that counts from the round.
func noRunnerSince(created int64, r *Runner) int64 {
	if r != nil && !r.Online && r.LastSeenAt != nil {
		return max(created, r.LastSeenAt.UnixMilli())
	}
	return created
}

// endHeldRun finishes the run and claim of a task whose pull request is
// out, as wp done does for one: the merge finishes the task itself.
func (s *Service) endHeldRun(ctx context.Context, actor *Actor, issueID int64, agent string) error {
	return s.write(ctx, actor, func(t *tx) error {
		var open int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM agent_runs WHERE issue_id = ? AND agent = ? AND ended_at IS NULL`, issueID, agent).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			if err := s.endRun(ctx, t, issueID, agent, "done", nil); err != nil {
				return err
			}
		}
		_, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`, issueID, agent)
		return err
	})
}

// SessionReview is where a session's newest adversarial review has got to,
// so nobody has to guess whether a quiet session is waiting on one. A
// review that finished isn't shown.
type SessionReview struct {
	Task string `json:"task"`
	// Stage: queued, audit, starting, running, waiting (on the screen, a
	// runner, its turn), failed, skipped or interrupted.
	Stage  string    `json:"stage"`
	Label  string    `json:"label"`
	Detail string    `json:"detail,omitempty"`
	Since  time.Time `json:"since"`
	// Audit is the audit it waits for or used.
	Audit int64 `json:"audit,omitempty"`
}

// reviewStage says what a round is doing: state and reason are the round's,
// stepState, stepReason and stepDetail those of the step carrying it ("":
// none), alt why the round asked for a second audit (WP-820). An empty
// stage is one that isn't shown.
func reviewStage(state, reason, stepState, stepReason, stepDetail, auditJob, alt string) (stage, label, detail string) {
	switch state {
	case RoundRunning:
		return "running", "Reviewing", "The session is working through its adversarial review."
	case RoundFailed:
		return "failed", "Review failed", reason
	case RoundSkipped:
		return "skipped", "Review skipped", reason
	case RoundInterrupted:
		return "interrupted", "Review interrupted", reason
	case RoundPending:
	default:
		return "", "", ""
	}
	switch {
	case stepState == "":
		return "queued", "Review queued", "It starts once the session stops."
	case stepState == StepStarting:
		return "starting", "Review starting", "A runner is starting it in the session."
	case stepReason == WaitAudit && alt != "" && (auditJob == "" || auditJob == "queued"):
		return "audit_queued", "Audit queued", "Another model's audit is waiting for a free slot on the Waypoint runner: " + alt
	case stepReason == WaitAudit && alt != "":
		return "audit", "Another model auditing", "Another model is reading the change, after the first audit couldn't be used: " + alt
	case stepReason == WaitAudit && (auditJob == "" || auditJob == "queued"):
		return "audit_queued", "Audit queued", "Another model's audit is waiting for a free slot on the Waypoint runner."
	case stepReason == WaitAudit:
		return "audit", "Another model auditing", "Another model is reading the change. The session gets its findings when it's done."
	case stepState == StepDue:
		return "queued", "Review queued", "It starts within a minute or so."
	}
	return "waiting", "Review waiting", stepDetail
}

// sessionReviews fills in each session's newest review.
func (s *Service) sessionReviews(ctx context.Context, actor *Actor, list []*AgentSession) error {
	ids := make([]int64, len(list))
	byID := make(map[int64]*AgentSession, len(list))
	for n, x := range list {
		ids[n], byID[x.ID] = x.ID, x
	}
	now := s.nowMS()
	// #nosec G202 -- only ? placeholders and a fixed clause are joined in
	rows, err := s.db.R.QueryContext(ctx, `SELECT w.session_id, p.key || '-' || i.number, w.state, w.reason, w.created_at,
			CASE WHEN w.state IN ('pending', 'running') THEN 0 ELSE coalesce(w.ended_at, w.updated_at) END, coalesce(w.audit_id, 0),
			coalesce(st.state, ''), coalesce(st.reason, ''), coalesce(st.detail, ''), coalesce(aj.state, ''), w.audit_alt,
			EXISTS (SELECT 1 FROM agent_runs later WHERE later.session_id = w.session_id AND later.id > w.run_id)
		FROM work_log w JOIN issues i ON i.id = w.issue_id JOIN projects p ON p.id = i.project_id
		LEFT JOIN autopilot_steps st ON st.review_id = w.id AND st.state IN ('due', 'waiting', 'starting')
		LEFT JOIN code_audits au ON au.id = w.audit_id LEFT JOIN runner_jobs aj ON aj.id = au.job_id
		WHERE w.id IN (SELECT max(id) FROM work_log WHERE kind = 'review' AND session_id IN (`+placeholders(len(ids))+`) GROUP BY session_id) AND `+draftsHidden(actor, "i"), int64Args(ids)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sid, created, ended, audit int64
		var task, state, reason, stepState, stepReason, stepDetail, auditJob, alt string
		var later bool
		if err := rows.Scan(&sid, &task, &state, &reason, &created, &ended, &audit, &stepState, &stepReason, &stepDetail, &auditJob, &alt, &later); err != nil {
			return err
		}
		if ended > 0 && (later || now-ended > sessionReviewShown.Milliseconds()) {
			continue // the session has gone on from it
		}
		stage, label, detail := reviewStage(state, reason, stepState, stepReason, stepDetail, auditJob, alt)
		if stage == "" {
			continue
		}
		since := created
		if ended > 0 {
			since = ended
		}
		byID[sid].Review = &SessionReview{Task: task, Stage: stage, Label: label, Detail: detail, Since: time.UnixMilli(since).UTC(), Audit: audit}
	}
	return rows.Err()
}

// liveStepRound is roundLive for the round the session's live step
// carries ("": none, or it has run).
func (s *Service) liveStepRound(ctx context.Context, q queryer, sessionID int64) (string, error) {
	var round int64
	err := q.QueryRowContext(ctx, `SELECT coalesce(review_id, 0) FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?) ORDER BY id DESC LIMIT 1`,
		sessionID, StepDue, StepWaiting, StepStarting).Scan(&round)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return roundLive(ctx, q, round)
}

// strandedShipWindow is how long after a round ended, or after its run or
// session ended or moved on if that was later, Waypoint keeps trying to open
// the pull request it left unopened.
const strandedShipWindow = 24 * time.Hour

// strandedBackoff is how long a round whose pull request couldn't be opened
// (the branch is gone, the forge refuses) waits before the next try; each
// further failure doubles it, up to strandedBackoffMax.
const (
	strandedBackoff    = 10 * time.Minute
	strandedBackoffMax = 6 * time.Hour
)

// strandedBatch is how many rounds one sweep tries: it opens pull requests
// inline on the minute tick, so a backlog (after a deploy) is opened
// gradually. A variable so a test can make the batch smaller.
var strandedBatch = 5

// strandedTry is what strandedRetry holds for a round: when it may be tried
// next and how many tries in a row failed.
type strandedTry struct {
	next  int64
	tries int
}

// strandedFailed records a failed try of round and logs why.
func (s *Service) strandedFailed(ctx context.Context, round, now int64, what string, err error) {
	tries := 1
	if prev, ok := s.strandedRetry.Load(round); ok {
		tries = prev.(strandedTry).tries + 1
	}
	wait := strandedBackoffMax
	if tries <= 16 {
		wait = min(strandedBackoff<<(tries-1), strandedBackoffMax)
	}
	s.strandedRetry.Store(round, strandedTry{next: now + wait.Milliseconds(), tries: tries})
	s.log.WarnContext(ctx, "review round: a stranded round was not opened", "what", what, "round", round, "tries", tries, "err", err)
}

// openStrandedRounds opens the pull request of a task whose review before it
// never ran, when no autopilot step is left to do it (WP-1325). A step opens
// it as the round ends (heldUnshipped, shipHeld) only while the run is open
// and its session has a step; a session that ended, a run that was released
// or finished, a person's /clear, a step stopped: each left the pushed
// branch with no pull request and nothing that would ever open one.
//
// It takes rounds before a pull request that ended skipped, failed or
// interrupted (an agent's skip whose open failed is one: a skip is not
// refused), and whose run or session is over or has moved on to another
// task, and opens the pull request as shipHeld does (the round recorded as
// not reviewed; the round's audit, if it lives, goes on as the pull
// request's, handOverRoundAudits). Not one a person cancelled (theirs), not
// one a newer round replaced or restarted, not a task that has a pull
// request already (the session shipped it, or this ran before) or is
// resolved. Opening is idempotent: the pull request it opens is linked to
// the task, which this reads, and one that already exists is found by the
// open. A task another session holds an open run on since is left alone
// (shipHeld ends the agent's open runs on the task).
//
// The window (strandedShipWindow) runs from the latest of the round's end,
// its run's end, its session's end and the start of the session's next run
// on another task, so a round that ended long before its session did is
// still opened. A round whose pull request can't be opened, or whose
// session or user can't be read, waits strandedBackoff, doubling with each
// failure; the batch is chosen from the rounds not backing off, so failing
// ones don't starve the rest. The back-off of a round the query no longer
// returns (opened, resolved, aged out) is forgotten, so the map holds no
// more than the rounds in the window.
func (s *Service) openStrandedRounds(ctx context.Context) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT w.id, w.session_id, w.run_id, w.state, w.reason, r.agent, coalesce(x.project_id, i.project_id)
		FROM work_log w JOIN agent_runs r ON r.id = w.run_id JOIN agent_sessions x ON x.id = w.session_id JOIN issues i ON i.id = w.issue_id
		WHERE w.kind = 'review' AND w.before_pr != '' AND w.state IN (?, ?, ?)
			AND coalesce(w.reason, '') NOT LIKE ? AND coalesce(w.reason, '') NOT IN (?, ?)
			AND (r.ended_at IS NOT NULL OR x.ended_at IS NOT NULL
				OR EXISTS (SELECT 1 FROM agent_runs o WHERE o.session_id = w.session_id AND o.id > w.run_id AND o.issue_id != w.issue_id)) AND i.resolved_at IS NULL
			AND max(w.updated_at, coalesce(r.ended_at, 0), coalesce(x.ended_at, 0),
				coalesce((SELECT min(o.started_at) FROM agent_runs o WHERE o.session_id = w.session_id AND o.id > w.run_id AND o.issue_id != w.issue_id), 0)) > ?
			AND w.id = (SELECT max(m.id) FROM work_log m WHERE m.kind = 'review' AND m.issue_id = w.issue_id)
			AND NOT EXISTS (SELECT 1 FROM external_links l WHERE l.issue_id = w.issue_id AND l.kind = 'pull')
			AND NOT EXISTS (SELECT 1 FROM agent_runs o WHERE o.issue_id = w.issue_id AND o.id != w.run_id AND o.ended_at IS NULL)
		ORDER BY w.id`,
		RoundSkipped, RoundFailed, RoundInterrupted, roundCancelledBy+"%", roundReplaced, roundRestarted, now-strandedShipWindow.Milliseconds())
	if err != nil {
		s.log.WarnContext(ctx, "review round: reading the rounds that never ran", "err", err)
		return
	}
	type stranded struct {
		round, session, run, project int64
		state, reason, agent         string
	}
	var list []stranded
	inWindow := map[int64]bool{}
	for rows.Next() {
		var l stranded
		if err := rows.Scan(&l.round, &l.session, &l.run, &l.state, &l.reason, &l.agent, &l.project); err != nil {
			s.log.WarnContext(ctx, "review round: reading a round that never ran", "err", err)
			continue
		}
		inWindow[l.round] = true
		if next, ok := s.strandedRetry.Load(l.round); ok && next.(strandedTry).next > now {
			continue // its last try failed a short while ago
		}
		if len(list) < strandedBatch {
			list = append(list, l)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		s.log.WarnContext(ctx, "review round: reading the rounds that never ran", "err", err)
		return // the window is only partly known: nothing is forgotten
	}
	s.strandedRetry.Range(func(k, _ any) bool {
		if !inWindow[k.(int64)] {
			s.strandedRetry.Delete(k)
		}
		return true
	})
	for _, l := range list {
		x, err := scanSession(s.db.R.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, l.session), now)
		if err != nil {
			s.strandedFailed(ctx, l.round, now, "read the session of a round that never ran", err)
			continue
		}
		user, err := s.userByID(ctx, s.db.R, x.userID)
		if err != nil {
			s.strandedFailed(ctx, l.round, now, "read the user of a round that never ran", err)
			continue
		}
		why := cmp(strings.TrimSpace(l.reason), "its adversarial review didn't run")
		if l.state == RoundInterrupted {
			why = "its adversarial review was interrupted (" + why + ")"
		}
		st := &stepRow{userID: x.userID, projectID: l.project, sessionID: l.session, reviewID: sql.NullInt64{Int64: l.round, Valid: true}}
		// Said apart from the step's own opens, on the task and in the pull
		// request's body.
		why += "; its session had ended or moved on by then"
		if err := s.shipHeld(ctx, st, x, user, l.agent, why); err != nil {
			s.strandedFailed(ctx, l.round, now, "open the pull request of a round that never ran", err)
			continue
		}
		s.strandedRetry.Delete(l.round)
		s.log.Info("review round: opened the pull request of a round that never ran", "round", l.round, "why", why)
	}
}
