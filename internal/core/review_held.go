package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
//     review is a decision: nothing opens then, and they are told. A check
//     of the session's marks that didn't run (failed, limited, cancelled,
//     by a person too) isn't one: the pull request opens and says the marks
//     went unchecked, and they go to its review gate (checkDidntRun);
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
// review that couldn't run: it opens the pull request without it, arms and
// babysits it, moves the task to In Review and ends the run. Opening again
// finds the pull request already open, so a retry is safe.
func (s *Service) shipHeld(ctx context.Context, st *stepRow, x *AgentSession, user *User, agent, why string) error {
	return s.shipRound(ctx, shipRoundIn{round: st.reviewID.Int64, session: x.ID, user: user, agent: agent, userID: st.userID, why: why})
}

// shipReviewed does what the session's wp ship would have done once its
// adversarial review left nothing for the session to do: the pull request
// opens as the round's session, armed and babysat, the task moves to In
// Review, the run ends, the person is told and the session gets one passive
// message with the result (WP-1370). The round ends as reviewed. Safe to
// call again and from several places: one pull request, one message.
func (s *Service) shipReviewed(ctx context.Context, roundID int64) error {
	return s.shipRound(ctx, shipRoundIn{round: roundID})
}

// reviewedToShip says whether a round's review ended with nothing for the
// session to do while its pull request is still to open, so Waypoint opens
// it (shipReviewed): the round is done, was asked before a pull request, has
// another model's audit (or its reviewer's) that finished with every
// finding decided, the session's run is open and no pull request is open
// from the branch. Findings still to decide are the session's: they go
// to it as they always did.
func (s *Service) reviewedToShip(ctx context.Context, roundID int64) (bool, error) {
	r, err := roundByID(ctx, s.db.R, roundID)
	if err != nil || r == nil || r.state != RoundDone || !r.runID.Valid {
		return false, err
	}
	bp := roundBeforePR(ctx, s.db.R, roundID)
	if bp == nil {
		return false, nil
	}
	var open bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE id = ? AND ended_at IS NULL)`, r.runID.Int64).Scan(&open); err != nil || !open {
		return false, err
	}
	var existing bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pull_requests WHERE repo = ? COLLATE NOCASE AND head_ref = ? AND state IN ('open', 'draft'))`,
		bp.Repo, bp.Branch).Scan(&existing); err != nil || existing {
		return false, err
	}
	// A check of the marks that disagreed, or is still going, is what the round
	// waits on: a round that is Done doesn't ship around it (R2a).
	if need, err := s.roundNeeds(ctx, roundID); err != nil {
		return false, err
	} else if need == needCheck || need == needTell {
		return false, nil
	}
	ra, err := s.roundAuditOf(ctx, roundID)
	if err != nil {
		return false, err
	}
	ids := []int64{ra.id}
	if rv, err := s.roundReviewer(ctx, roundID); err == nil && rv != nil {
		ids = append(ids, rv.ID)
	}
	seen := false
	for _, id := range ids {
		if id == 0 {
			continue
		}
		a, err := s.auditRow(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if a.Status != AuditDone {
			continue
		}
		if err := s.loadFindings(ctx, a); err != nil {
			return false, err
		}
		if a.readNothing() {
			continue
		}
		seen = true
		if len(s.roundTodo(ctx, a)) > 0 {
			return false, nil
		}
	}
	return seen, nil
}

// shipRoundIn says whose ship it is: user, agent and session are the
// round's own when not given (a call with no step in hand). why is set for
// a review that didn't run: the pull request then opens without it, and
// the texts say so; "" is a review that ran.
type shipRoundIn struct {
	round   int64
	session int64
	user    *User
	userID  int64
	agent   string
	why     string
}

// shipRound is the one server-side ship of a round's branch: shipHeld (the
// review couldn't run) and shipReviewed (it ran, and left nothing to do).
func (s *Service) shipRound(ctx context.Context, in shipRoundIn) error {
	defer s.shipLocks.lock(in.round)()
	r, err := roundByID(ctx, s.db.R, in.round)
	if err != nil {
		return err
	}
	bp := roundBeforePR(ctx, s.db.R, in.round)
	if r == nil || bp == nil {
		return errors.New("its review round is gone")
	}
	reviewed := in.why == ""
	// A check of the marks that never gave an answer (failed, hit a limit,
	// cancelled, or by a person) is not "nothing left to do": the PR opens,
	// and says what wasn't checked (WP-1370).
	unchecked := ""
	if reviewed {
		unchecked = s.checkDidntRun(ctx, in.round)
	}
	facts, ran := "", false
	if !reviewed {
		// What the review did is told from what was recorded, not from why it ended.
		facts, ran = s.roundFacts(ctx, in.round)
		if facts != "" {
			in.why += " (" + facts + ")"
		}
	}
	if in.session == 0 {
		in.session = r.sessionID.Int64
	}
	in.agent = s.shipAgent(ctx, r, in.agent, in.session)
	if in.userID == 0 {
		_ = s.db.R.QueryRowContext(ctx, `SELECT user_id FROM agent_sessions WHERE id = ?`, in.session).Scan(&in.userID)
	}
	if reviewed {
		// The run must still be open: a ship of the session's own ends it,
		// and then there is nothing left for Waypoint to do.
		var open bool
		if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE id = ? AND ended_at IS NULL)`, r.runID.Int64).Scan(&open); err != nil {
			return err
		}
		if !open {
			return nil
		}
	}
	title := bp.Title
	if strings.TrimSpace(title) == "" {
		if err := s.db.R.QueryRowContext(ctx, `SELECT title FROM issues WHERE id = ?`, r.issueID).Scan(&title); err != nil {
			return err
		}
	}
	actor, why, err := s.shipAuthority(ctx, r, bp, &in)
	if err != nil {
		return err
	}
	if why != "" {
		return s.stopShip(ctx, r, in, why)
	}
	body := bp.Body
	// A review that ended with its findings undelivered did run.
	given := !strings.HasPrefix(in.why, heldReviewEnded)
	// The Haiku checks' confirmed problems that no reviewing model's
	// findings carry are said, with the audit that holds them.
	var panel string
	if !reviewed {
		if n, audit := s.panelConfirmed(ctx, r.id); n > 0 {
			panel = fmt.Sprintf(" Before the reviewing model failed, Waypoint's Haiku checks confirmed %s against the code; audit #%d holds them (`wp audit show %d`).", plural1(n, "problem"), audit, audit)
		}
	}
	in2 := OpenPRInput{
		Project: bp.Project, Repo: bp.Repo, Head: bp.Branch, Base: bp.Base,
		Title: title, Draft: bp.Draft, Ready: bp.Ready, Urgent: bp.Urgent,
		Issue: r.key, For: r.key, noSweep: true, BodyInputs: bp.BodyInputs,
	}
	if reviewed {
		in2.Review, in2.Body = ShipAuto, body
		if unchecked != "" {
			in2.Body = strings.TrimSpace(body + "\n\nWaypoint opened this: " + unchecked + ". The marks it didn't check go to the pull request's review gate.")
		}
	} else {
		in2.Review, in2.Reason = ShipSkip, in.why
		lead := "without an adversarial review"
		if ran {
			lead = "before its adversarial review was settled"
		}
		if !given {
			lead = "without the adversarial review's findings being given to the session"
		}
		in2.Body = strings.TrimSpace(body + "\n\nWaypoint opened this " + lead + ": " + in.why + "." + panel)
	}
	res, err := s.OpenPullRequest(ctx, actor, in2)
	if err != nil {
		return err
	}
	if res.Held != "" {
		if reviewed {
			// The ship the session would have run is held as well: another
			// review round was scheduled, or marks wait on the session.
			// Waypoint says nothing more; the round carries on.
			s.log.Info("autopilot: a reviewed ship was held", "round", r.id, "held", clipText(res.Held, 200))
			return nil
		}
		return errors.New(res.Held)
	}
	if res.Pull == nil {
		return errors.New("no pull request came back")
	}
	pr := res.Pull
	var notes []string
	armed, babysat := false, false
	if len(res.OtherTasks) > 0 {
		notes = append(notes, "that branch's pull request belongs to "+strings.Join(res.OtherTasks, ", ")+", so it wasn't armed")
	} else {
		if !bp.Draft && (pr.State != "draft" || pr.WipAuto) {
			if mr, err := s.MergePR(ctx, actor, pr.ID, MergeInput{WhenReady: true}); err != nil {
				notes = append(notes, "couldn't arm it: "+err.Error())
			} else if mr != nil && mr.ByHand {
				// Not an error, and not armed: a person merges it.
				notes = append(notes, "wasn't armed: "+strings.Join(mr.Reasons, "; "))
			} else {
				armed = true
			}
			if _, err := s.StartBabysit(ctx, actor, pr.ID, BabysitInput{Via: "ship"}); err != nil {
				notes = append(notes, "couldn't babysit it: "+err.Error())
			} else {
				babysat = true
			}
		}
		if reviewed && strings.TrimSpace(bp.Message) != "" && !res.Existing {
			if _, err := s.AddComment(ctx, actor, r.key, bp.Message); err != nil {
				notes = append(notes, "couldn't post its message on the task: "+err.Error())
			}
		}
		var status string
		// An unread status is not "not In Review": a task that is Done must not
		// be moved back on a failed read.
		statusErr := s.db.R.QueryRowContext(ctx, `SELECT s.name FROM issues i JOIN statuses s ON s.id = i.status_id WHERE i.id = ?`, r.issueID).Scan(&status)
		if statusErr != nil {
			notes = append(notes, "left the task's status alone: couldn't read it: "+statusErr.Error())
		}
		if statusErr == nil && status != "In Review" {
			next := "In Review"
			comment := fmt.Sprintf("No adversarial review ran before this pull request: %s. Waypoint opened %s so the task doesn't sit unshipped; CI and the review gate still apply.%s", in.why, pr.URL, panel)
			if ran {
				comment = fmt.Sprintf("The adversarial review wasn't settled before this pull request: %s. Waypoint opened %s so the task doesn't sit unshipped; what it found and nobody decided goes to the pull request's review gate, which still applies with CI.%s", in.why, pr.URL, panel)
			}
			if !given {
				comment = fmt.Sprintf("The adversarial review's findings weren't given to the session before this pull request: %s. Waypoint opened %s so the task doesn't sit unshipped; CI and the review gate still apply.%s", in.why, pr.URL, panel)
			}
			if reviewed {
				comment = fmt.Sprintf("The adversarial review left nothing for the session to do, so Waypoint opened %s; CI and the review gate still apply.", pr.URL)
				if unchecked != "" {
					comment = fmt.Sprintf("%s, so Waypoint opened %s anyway; the marks it didn't check go to the pull request's review gate, which still applies with CI.", unchecked, pr.URL)
				}
			}
			if _, err := s.UpdateIssue(ctx, actor, r.key, IssuePatch{Status: &next, ViaLink: pr.URL, Comment: comment}); err != nil {
				notes = append(notes, "left the task's status alone: "+err.Error())
			}
		}
	}
	if reviewed {
		if err := s.endRoundReviewed(ctx, r); err != nil {
			return err
		}
	}
	if err := s.endHeldRun(ctx, actor, r.issueID, in.agent); err != nil {
		return err
	}
	var text, head string
	if reviewed {
		head = r.key + " is in review: " + pr.Repo + "#" + strconv.Itoa(pr.Number)
		text = s.shippedText(r.key, pr, armed, babysat, res.Existing, bp.Summary, notes)
		if unchecked != "" {
			head = r.key + " is in review, its marks unchecked: " + pr.Repo + "#" + strconv.Itoa(pr.Number)
			text = "Waypoint: " + unchecked + ", so the marks you made stand unchecked and go to the pull request's review gate.\n\n" +
				strings.Replace(strings.Replace(text, " after its adversarial review left nothing for you to do", "", 1), " when its adversarial review left nothing for you to do", "", 1)
			s.carryAudits(ctx, r.id, pr.ID, pr.Number)
		}
		if in.session != 0 {
			if _, err := s.queueMessage(ctx, in.session, text, fmt.Sprintf("shipped:%d", r.id), messageFrom{}, true); err != nil {
				s.log.WarnContext(ctx, "autopilot: tell a session its pull request opened", "round", r.id, "err", err)
			}
		}
	} else {
		head = r.key + " opened without its adversarial review"
		text = fmt.Sprintf("%s's adversarial review didn't run (%s), so Waypoint opened %s#%d without it. CI and the review gate still apply.", r.key, in.why, pr.Repo, pr.Number)
		if ran {
			head = r.key + " opened before its adversarial review was settled"
			text = fmt.Sprintf("%s's adversarial review wasn't settled (%s), so Waypoint opened %s#%d anyway. What it found and nobody decided is carried to the pull request's review gate; CI and the gate still apply.", r.key, in.why, pr.Repo, pr.Number)
		}
		if !given {
			head = r.key + " opened without the review's findings given to its session"
			text = fmt.Sprintf("%s's adversarial review findings weren't given to the session (%s), so Waypoint opened %s#%d without them. What it found and nobody decided is carried to the pull request's review gate; CI and the gate still apply.", r.key, in.why, pr.Repo, pr.Number)
		}
		text += panel
		s.carryAudits(ctx, r.id, pr.ID, pr.Number)
		if len(notes) > 0 {
			text += " It " + strings.Join(notes, "; ") + "."
		}
	}
	if len(notes) > 0 {
		s.log.WarnContext(ctx, "autopilot: after opening a held pull request", "task", r.key, "notes", notes)
	}
	if res.Notice != "" {
		text += " " + res.Notice // what the open did instead of what was asked
	}
	if bp.Urgent && pr.Urgent {
		text += fmt.Sprintf(" Its wp ship marked it urgent, so it delays other CI runs to cut the line: `wp pr urgent %d --off` takes that off.", pr.Number)
	} else if pr.Urgent && !res.Existing {
		// wp ship didn't ask for it, so the task's tag did; no one reads this
		// open's notice.
		text += " " + taggedUrgentNotice(r.key, pr.Number)
	}
	s.notifyURL(ctx, in.userID, nil, NotifyAutopilot, head, text, "/agents?tab=autopilot")
	return nil
}

// shippedText is what Waypoint tells the session, and the person, after it
// opened the pull request the session's ship would have: the pull request,
// what was done to it, and the summary the ship left, filled in.
func (s *Service) shippedText(key string, pr *PRDetail, armed, babysat, existing bool, summary string, notes []string) string {
	var b strings.Builder
	if existing {
		fmt.Fprintf(&b, "%s#%d for %s was already open when its adversarial review left nothing for you to do: %s.", pr.Repo, pr.Number, key, pr.URL)
	} else {
		fmt.Fprintf(&b, "Waypoint opened %s#%d for %s after its adversarial review left nothing for you to do: %s.", pr.Repo, pr.Number, key, pr.URL)
	}
	switch {
	case armed && babysat:
		b.WriteString(" It is armed to merge when CI and reviews are green, and Waypoint babysits it.")
	case armed:
		b.WriteString(" It is armed to merge when CI and reviews are green.")
	case babysat:
		b.WriteString(" Waypoint babysits it.")
	}
	if len(notes) > 0 {
		b.WriteString(" It " + strings.Join(notes, "; ") + ".")
	}
	b.WriteString(" The task is In Review; there is nothing more to run.")
	if t := strings.TrimSpace(summary); t != "" {
		what := cmp(key, pr.Title)
		t = strings.NewReplacer("{task}", what, "{pr}", strconv.Itoa(pr.Number), "{url}", pr.URL).Replace(t)
		b.WriteString("\n\nThe summary you left with your ship:\n" + t)
	}
	return b.String()
}

// endRoundReviewed ends a round the server shipped for as reviewed: done,
// counted from its audit's findings, whatever it was (a running round whose
// session shipped and is checked; one that ended with nothing to do is
// already done).
func (s *Service) endRoundReviewed(ctx context.Context, r *roundRow) error {
	if r.state == RoundDone {
		return nil
	}
	found, fixed, rejected := 0, 0, 0
	body := "The adversarial review left nothing for the session to do; Waypoint opened the pull request."
	if ra, err := s.roundAuditOf(ctx, r.id); err == nil && ra.id != 0 {
		if a, err := s.auditRow(ctx, ra.id); err == nil && s.loadFindings(ctx, a) == nil {
			for _, f := range a.Findings {
				found++
				switch f.State {
				case "fixed":
					fixed++
				case "rejected", "dismissed":
					rejected++
				}
			}
			body = fmt.Sprintf("%s's audit #%d found %s: %d fixed, %d rejected. Waypoint opened the pull request.", cmp(a.Model, "another model"), a.ID, plural1(found, "finding"), fixed, rejected)
		}
	}
	now := s.nowMS()
	return s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE work_log SET state = 'done', body = ?, ended_at = coalesce(ended_at, ?), found = ?, fixed = ?, rejected = ?,
			read_state = ?, read_by = ?, read_at = ?, updated_at = ? WHERE id = ? AND state IN ('running', 'pending')`,
			body, now, found, fixed, rejected, readDone, readByRules, now, now, r.id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.emitWorkLog(ctx, t, r.issueID, r.id, RoundDone)
		}
		return nil
	})
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

// roundFacts says what a round's review did, from what was recorded: its
// audit (found how many, how many still undecided) and the check of the
// session's marks (agreed, disagrees, or didn't run). ran is whether any of
// it ran at all. The one place the texts of a round that ended short of a
// clean review get this from (WP-1370).
func (s *Service) roundFacts(ctx context.Context, round int64) (facts string, ran bool) {
	var parts []string
	if ra, err := s.roundAuditOf(ctx, round); err == nil && ra.id != 0 {
		if a, err := s.auditRow(ctx, ra.id); err == nil && a.Status == AuditDone && s.loadFindings(ctx, a) == nil {
			ran = true
			todo := len(s.roundTodo(ctx, a))
			parts = append(parts, fmt.Sprintf("its audit found %s, %d undecided", plural1(len(a.Findings), "thing"), todo))
		}
	}
	var id int64
	if s.db.R.QueryRowContext(ctx, `SELECT id FROM code_audits WHERE request_key = ?`, fmt.Sprintf(checkAuditKey, round)).Scan(&id) == nil {
		if a, err := s.auditRow(ctx, id); err == nil {
			switch todo, _ := s.checkTodo(ctx, a); {
			case a.Status == AuditQueued || a.Status == AuditRunning:
				parts = append(parts, "the check of its marks hadn't finished")
			case a.Status != AuditDone:
				parts = append(parts, "the check of its marks didn't run ("+a.Status+")")
			case len(todo) > 0:
				ran = true
				parts = append(parts, fmt.Sprintf("the check of its marks disagreed with %s", plural1(len(todo), "mark")))
			default:
				ran = true
				parts = append(parts, "the check of its marks agreed")
			}
		}
	}
	return strings.Join(parts, "; "), ran
}

// carryAudits makes the audits of a round the pull request's, so what they
// found and nobody decided holds its review gate (the gate reads the pull
// request's newest finished audit): the round's own and the check of its
// marks.
func (s *Service) carryAudits(ctx context.Context, round, prID int64, number int) {
	if ra, err := s.roundAuditOf(ctx, round); err == nil && ra.id != 0 {
		// Marks nobody checked are not decisions the gate can lean on: the
		// session's rejection of a critical or high finding stands only as a
		// claim, so it is open again, with what the session said kept in its note.
		if s.checkDidntRun(ctx, round) != "" {
			s.reopenUnverified(ctx, ra.id)
		}
		if err := s.handOverRoundAudit(ctx, ra.id, prID); err != nil {
			s.log.WarnContext(ctx, "carry a round's audit to its pull request", "round", round, "audit", ra.id, "err", err)
		}
	}
	var id int64
	if s.db.R.QueryRowContext(ctx, `SELECT id FROM code_audits WHERE request_key = ? AND status = ?`, fmt.Sprintf(checkAuditKey, round), AuditDone).Scan(&id) != nil {
		return
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `UPDATE code_audits SET purpose = '', pr_id = ?, pr_number = ? WHERE id = ? AND purpose = ?`, prID, number, id, AuditCheck)
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "carry a round's check to its pull request", "round", round, "audit", id, "err", err)
		return
	}
	s.log.InfoContext(ctx, "round's check carried to its pull request", "round", round, "audit", id, "pr", prID)
}

// reopenUnverified opens again the critical and high findings of an audit
// the session rejected and nothing checked, so they hold the pull request's
// merge until a person or its session decides them (WP-1370).
func (s *Service) reopenUnverified(ctx context.Context, auditID int64) {
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `UPDATE code_audit_findings SET state = 'open',
			note = trim('Rejected by the session, unchecked: ' || note), decided_at = NULL
			WHERE audit_id = ? AND state = 'rejected' AND severity IN ('critical', 'high')`, auditID)
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "reopen unchecked rejections", "audit", auditID, "err", err)
	}
}

// checkDidntRun says why a round's check of the session's marks gave no
// answer ("": it ran, or none was asked for): it failed, stopped at a limit,
// or was cancelled, by a person too, who is named when the audit says so.
func (s *Service) checkDidntRun(ctx context.Context, round int64) string {
	var id int64
	if s.db.R.QueryRowContext(ctx, `SELECT id FROM code_audits WHERE request_key = ?`, fmt.Sprintf(checkAuditKey, round)).Scan(&id) != nil {
		return ""
	}
	a, err := s.auditRow(ctx, id)
	if err != nil || a.Status == AuditQueued || a.Status == AuditRunning || a.Status == AuditDone {
		return ""
	}
	why := strings.TrimSuffix(strings.TrimSpace(a.Error), ".")
	return "the check of the marks didn't run: " + cmp(why, a.Status)
}
