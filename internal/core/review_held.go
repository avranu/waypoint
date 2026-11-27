package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
//     request anyway (shipRound), says so on the task, and the session moves
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
			AND `+movedOnAt+` = 0
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
			continue
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

// shipReviewed does what the session's wp ship would have done once its
// adversarial review left nothing for the session to do: the pull request
// opens as the round's session, armed and babysat, the task moves to In
// Review, the run ends, the person is told and the session gets one passive
// message with the result (WP-1370). The round ends as reviewed. Safe to
// call again and from several places: one pull request, one message.
func (s *Service) shipReviewed(ctx context.Context, roundID int64) error {
	return s.shipRound(ctx, shipRoundIn{round: roundID})
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
	ended   bool
}

// shipRun is what one ship of a round has read and decided as it goes, so
// its steps share it.
type shipRun struct {
	r                *roundRow
	bp               *beforePR
	in               shipRoundIn
	reviewed         bool
	ran, given       bool
	unchecked, panel string
	title            string
	actor            *Actor
	res              *OpenPRResult
	notes            []string
	armed, babysat   bool
	pin              string
}

// shipRound is the one server-side ship of a round's branch: a release (the
// review couldn't run) and shipReviewed (it ran, and left nothing to do).
// Its steps: read the round (shipRead), act as whoever may still ship it and
// refuse what isn't Waypoint's to take (shipAuthorize), open the pull request
// (shipOpen), arm, babysit and move the task (shipArm), record the end of the
// round and its run (shipRecord), and tell the session and the person
// (shipTell).
func (s *Service) shipRound(ctx context.Context, in shipRoundIn) error {
	defer s.shipLocks.lock(in.round)()
	sh, err := s.shipRead(ctx, in)
	if err != nil || sh == nil {
		return err
	}
	if done, err := s.shipAuthorize(ctx, sh); err != nil || done {
		return err
	}
	if done, err := s.shipOpen(ctx, sh); err != nil || done {
		return err
	}
	if len(sh.res.OtherTasks) > 0 {
		sh.notes = append(sh.notes, "that branch's pull request belongs to "+strings.Join(sh.res.OtherTasks, ", ")+", so it wasn't armed")
	} else {
		s.shipArm(ctx, sh)
	}
	if err := s.shipRecord(ctx, sh); err != nil {
		return err
	}
	s.shipTell(ctx, sh)
	return nil
}

// shipRead reads the round and what the ship is told from it; nil (and no
// error) when there is nothing left for Waypoint to ship.
func (s *Service) shipRead(ctx context.Context, in shipRoundIn) (*shipRun, error) {
	r, err := roundByID(ctx, s.db.R, in.round)
	if err != nil {
		return nil, err
	}
	bp := roundBeforePR(ctx, s.db.R, in.round)
	if r == nil || bp == nil {
		return nil, errors.New("its review round is gone")
	}
	if bp.ShipDone {
		return nil, nil
	}
	sh := &shipRun{r: r, bp: bp, in: in}
	reviewed := in.why == ""
	unchecked := ""
	if reviewed {
		unchecked = s.uncheckedOf(ctx, r, true)
	}
	facts, ran := "", false
	if !reviewed {
		facts, ran = s.roundFacts(ctx, r)
		if facts != "" {
			in.why += " (" + facts + ")"
		}
	}
	if in.session == 0 {
		in.session = r.sessionID.Int64
	}
	if in.session != 0 {
		var live bool
		if err := s.db.R.QueryRowContext(ctx, `SELECT ended_at IS NULL FROM agent_sessions WHERE id = ?`, in.session).Scan(&live); err == nil {
			in.ended = !live
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	in.agent = s.shipAgent(ctx, r, in.agent, in.session)
	if in.userID == 0 {
		if err := s.db.R.QueryRowContext(ctx, `SELECT user_id FROM agent_sessions WHERE id = ?`, in.session).Scan(&in.userID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	sh.reviewed, sh.unchecked, sh.ran = reviewed, unchecked, ran
	sh.in = in
	if reviewed {
		// The run must still be open: a ship of the session's own ends it,
		// and then there is nothing left for Waypoint to do.
		open, err := roundRunOpen(ctx, s.db.R, r.id)
		if err != nil {
			return nil, err
		}
		if !open {
			return nil, nil
		}
	}
	title := bp.Title
	if strings.TrimSpace(title) == "" {
		if err := s.db.R.QueryRowContext(ctx, `SELECT title FROM issues WHERE id = ?`, r.issueID).Scan(&title); err != nil {
			return nil, err
		}
	}
	sh.title = title
	sh.given = !strings.HasPrefix(in.why, heldReviewEnded)
	if !reviewed {
		if n, audit := s.panelConfirmed(ctx, r.id); n > 0 {
			sh.panel = fmt.Sprintf(" Before the reviewing model failed, Waypoint's Haiku checks confirmed %s against the code; audit #%d holds them (`wp audit show %d`).", plural1(n, "problem"), audit, audit)
		}
	}
	return sh, nil
}

// shipAuthorize finds who the ship acts as and refuses what Waypoint may
// not ship: the authority gone (the round ends with why), a pull request
// open from the branch that isn't the task's, a branch that moved after the
// review read it. done: nothing more is shipped.
func (s *Service) shipAuthorize(ctx context.Context, sh *shipRun) (done bool, err error) {
	r, bp, in, reviewed := sh.r, sh.bp, sh.in, sh.reviewed
	actor, why, err := s.shipAuthority(ctx, r, bp, &in)
	sh.in, sh.actor = in, actor
	if err != nil {
		return true, err
	}
	if why != "" {
		return true, s.stopShip(ctx, r, in, why)
	}
	if id := s.openPROfBranch(ctx, bp); id != 0 {
		var repo string
		var number int
		if err := s.db.R.QueryRowContext(ctx, `SELECT repo, number FROM pull_requests WHERE id = ?`, id).Scan(&repo, &number); err != nil {
			return true, err
		}
		keys, err := s.pullTaskKeys(ctx, repo, number)
		if err != nil {
			return true, err
		}
		if len(keys) == 0 {
			var by, url string
			var unlinked bool
			if s.db.R.QueryRowContext(ctx, `SELECT p.agent, p.url, EXISTS (SELECT 1 FROM issue_unlinks u WHERE u.issue_id = ? AND u.url = p.url) FROM pull_requests p WHERE p.id = ?`, r.issueID, id).Scan(&by, &url, &unlinked) == nil && by != "" && by == in.agent && !unlinked {
				if _, lerr := s.AddLink(ctx, actor, r.key, url, ""); lerr == nil {
					keys = []string{r.key}
				} else {
					s.log.WarnContext(ctx, "link a pull request Waypoint opened", "round", r.id, "pr", id, "err", lerr)
				}
			}
		}
		if !slices.ContainsFunc(keys, func(k string) bool { return strings.EqualFold(k, r.key) }) {
			owner := "isn't linked to any task"
			if len(keys) > 0 {
				owner = "is linked to " + strings.Join(keys, ", ")
			}
			return true, s.stopShip(ctx, r, in, fmt.Sprintf("%s#%d is already open from %s and %s, not to %s, so Waypoint didn't arm or babysit it or take it over", repo, number, bp.Branch, owner, r.key))
		}
	}
	if reviewed {
		if moved, err := s.branchMovedSinceReview(ctx, actor, r, bp); err != nil {
			return true, err
		} else if moved {
			return true, s.startOverAfterMove(ctx, r, in, bp, "")
		}
	}
	return false, nil
}

// startOverAfterMove ends the round the way a ship that pushed more ends it
// (roundRestarted, which no release follows) and tells the person: the next
// ship asks for a review of what is pushed now. opened is the pull request
// that was opened at the wrong commit, if one was (it is left unarmed).
func (s *Service) startOverAfterMove(ctx context.Context, r *roundRow, in shipRoundIn, bp *beforePR, opened string) error {
	s.setRoundNow(ctx, r.id, []string{r.state}, RoundInterrupted, roundRestarted)
	what := "so Waypoint opened no pull request for what the review didn't see."
	if opened != "" {
		what = "so " + opened + " is at a commit the review didn't see; Waypoint left it unarmed."
	}
	s.notifyURL(ctx, in.userID, nil, NotifyAutopilot, r.key+": its branch moved after its review",
		fmt.Sprintf("%s was pushed to after %s's adversarial review read it, %s Run `wp ship` again and the review starts over on what is pushed now.", bp.Branch, r.key, what), "/agents?tab=autopilot")
	return nil
}

// reviewedHead is the commit a round's review read: its audit's ToSHA when it
// reported one, else the commit the round was shipped at ("" when neither).
func (s *Service) reviewedHead(ctx context.Context, r *roundRow, bp *beforePR) string {
	read := bp.HeadSHA
	if ra, err := s.roundAuditOf(ctx, r.id); err == nil && ra.id != 0 && ra.note == "" {
		if a, err := s.auditRow(ctx, ra.id); err == nil && a != nil && a.ToSHA != "" {
			read = a.ToSHA
		}
	}
	return read
}

// shipOpen opens the pull request as the ship's actor, or records that it
// was opened and stopped, or held. done: nothing more is shipped.
func (s *Service) shipOpen(ctx context.Context, sh *shipRun) (done bool, err error) {
	r, bp, in, reviewed, actor := sh.r, sh.bp, sh.in, sh.reviewed, sh.actor
	title, unchecked, given, panel, ran := sh.title, sh.unchecked, sh.given, sh.panel, sh.ran
	body := bp.Body
	base, stackedOn := bp.Base, []string(nil)
	if bp.Stacked != "" {
		base, stackedOn = bp.Asked, []string{bp.Stacked}
	}
	in2 := OpenPRInput{
		Project: bp.Project, Repo: bp.Repo, Head: bp.Branch, Base: base, StackedOn: stackedOn,
		Title: title, Draft: bp.Draft, Ready: bp.Ready, Urgent: bp.Urgent,
		Issue: r.key, For: r.key, Also: bp.Also, noSweep: true, byWaypoint: true, BodyInputs: bp.BodyInputs,
	}
	if reviewed {
		in2.Review, in2.Body = ShipAuto, body
		if unchecked != "" {
			in2.Body = strings.TrimSpace(body + "\n\n" + uncheckedLines(unchecked).Body)
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
	again := in
	again.user = nil
	in2.stillMay = func(ctx context.Context) string {
		_, why, err := s.shipAuthority(ctx, r, bp, &again)
		if err != nil {
			return "its authority couldn't be read: " + err.Error()
		}
		return why
	}
	res, err := s.OpenPullRequest(ctx, actor, in2)
	if err != nil {
		return true, err
	}
	sh.pin = s.reviewedHead(ctx, r, bp)
	if reviewed && res.Pull != nil && res.Held == "" && res.Stopped == "" && sh.pin != "" && !sameSHA(res.Pull.HeadSHA, sh.pin) {
		return true, s.startOverAfterMove(ctx, r, in, bp, fmt.Sprintf("%s#%d", res.Pull.Repo, res.Pull.Number))
	}
	if res.Stopped != "" && res.Pull != nil {
		body := fmt.Sprintf("Waypoint opened %s#%d, then stopped: %s. It is recorded, not armed and not watched; the task was left as it is.", res.Pull.Repo, res.Pull.Number, res.Stopped)
		return true, s.stopShipWith(ctx, r, in, body, "pull request opened, then stopped", res.Stopped)
	}
	if res.Held != "" {
		return true, s.heldOutcome(ctx, r.id, reviewed, res.Held)
	}
	if res.Pull == nil {
		return true, errors.New("no pull request came back")
	}
	sh.res = res
	return false, nil
}

// shipArm arms the pull request, babysits it, posts the ship's message on
// the task and moves it to In Review, each only where it applies; what
// couldn't be done is kept in notes for the texts.
func (s *Service) shipArm(ctx context.Context, sh *shipRun) {
	r, bp, in, reviewed, actor, res := sh.r, sh.bp, sh.in, sh.reviewed, sh.actor, sh.res
	unchecked, given, panel, ran := sh.unchecked, sh.given, sh.panel, sh.ran
	pr := res.Pull
	var notes []string
	armed, babysat := false, false
	if len(res.OtherTasks) > 0 {
		notes = append(notes, "that branch's pull request belongs to "+strings.Join(res.OtherTasks, ", ")+", so it wasn't armed")
	} else {
		if !bp.Draft && (pr.State != "draft" || pr.WipAuto) {
			if mr, err := s.MergePR(ctx, actor, pr.ID, MergeInput{WhenReady: true, Head: sh.pin}); err != nil {
				notes = append(notes, "couldn't arm it: "+err.Error())
			} else if mr != nil && mr.ByHand {
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
		var resolved bool
		statusErr := s.db.R.QueryRowContext(ctx, `SELECT s.name, i.resolved_at IS NOT NULL FROM issues i JOIN statuses s ON s.id = i.status_id WHERE i.id = ?`, r.issueID).Scan(&status, &resolved)
		if statusErr != nil {
			notes = append(notes, "left the task's status alone: couldn't read it: "+statusErr.Error())
		}
		if statusErr == nil && !resolved && status != "In Review" {
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
				if res.Existing {
					comment = fmt.Sprintf("The adversarial review left nothing for the session to do; %s was already open, so Waypoint moved the task on; CI and the review gate still apply.", pr.URL)
				}
				if unchecked != "" {
					comment = uncheckedLines(unchecked).Comment(pr.URL, res.Existing)
				}
			}
			if _, err := s.UpdateIssue(ctx, actor, r.key, IssuePatch{Status: &next, ViaLink: pr.URL, Comment: comment}); err != nil {
				notes = append(notes, "left the task's status alone: "+err.Error())
			}
		}
		for _, k := range res.AlsoLinked {
			i, _, err := s.getIssueFor(ctx, s.db.R, actor, k, RoleEditor, false)
			if err != nil || i.Closed() || i.Status.Name == "In Review" {
				continue
			}
			next := "In Review"
			if _, err := s.UpdateIssue(ctx, actor, k, IssuePatch{Status: &next, ViaLink: pr.URL, Comment: "Shipped with " + r.key + " in " + pr.URL + "; CI and the review gate still apply."}); err != nil {
				notes = append(notes, "left "+k+"'s status alone: "+err.Error())
			}
		}
	}
	sh.notes, sh.armed, sh.babysat = notes, armed, babysat
}

// shipRecord ends the round (when the review ran) and the held run.
func (s *Service) shipRecord(ctx context.Context, sh *shipRun) error {
	r, in, reviewed, actor, res := sh.r, sh.in, sh.reviewed, sh.actor, sh.res
	unchecked := sh.unchecked
	if reviewed {
		if err := s.endRoundReviewed(ctx, r, unchecked, res.Existing); err != nil {
			return err
		}
	}
	head, text, _ := s.shipWords(ctx, sh)
	return s.endHeldRunWith(ctx, actor, r.issueID, in.agent, func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE work_log SET before_pr = json_set(before_pr, '$.ship_done', json('true')) WHERE id = ? AND before_pr != '' AND coalesce(json_extract(before_pr, '$.ship_done'), 0) = 0`, r.id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 || in.userID == 0 {
			return nil
		}
		_, err = t.storeNotice(ctx, in.userID, NotifyAutopilot, head, text, "/agents?tab=autopilot")
		return err
	})
}

// shipWords is what the person is told (and the session, when it is still
// there) of what was opened and what was done to it.
func (s *Service) shipWords(ctx context.Context, sh *shipRun) (head, text, session string) {
	r, bp, in, reviewed, res := sh.r, sh.bp, sh.in, sh.reviewed, sh.res
	unchecked, given, panel, ran := sh.unchecked, sh.given, sh.panel, sh.ran
	notes, armed, babysat := sh.notes, sh.armed, sh.babysat
	pr := res.Pull
	if reviewed {
		head = r.key + " is in review: " + pr.Repo + "#" + strconv.Itoa(pr.Number)
		text = s.shippedText(r.key, pr, armed, babysat, res.Existing, bp.Summary, notes)
		if unchecked != "" {
			head = r.key + " is in review, its marks unchecked: " + pr.Repo + "#" + strconv.Itoa(pr.Number)
			text = uncheckedLines(unchecked).Session + "\n\n" +
				strings.Replace(strings.Replace(text, " after its adversarial review left nothing for you to do", "", 1), " when its adversarial review left nothing for you to do", "", 1)
		}
		session = text
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
		if len(notes) > 0 {
			text += " It " + strings.Join(notes, "; ") + "."
		}
	}
	if len(notes) > 0 {
		s.log.WarnContext(ctx, "autopilot: after opening a held pull request", "task", r.key, "notes", notes)
	}
	if res.Notice != "" {
		text += " " + res.Notice
	}
	if bp.Urgent && pr.Urgent {
		text += fmt.Sprintf(" Its wp ship marked it urgent, so it delays other CI runs to cut the line: `wp pr urgent %d --off` takes that off.", pr.Number)
	} else if pr.Urgent && !res.Existing && !strings.Contains(res.Notice, taggedUrgentNotice(r.key, pr.Number)) {
		text += " " + taggedUrgentNotice(r.key, pr.Number)
	}
	return head, text, session
}

// shipTell tells the session, when it is still there, what was opened and
// what was done to it; the person's notice is stored by shipRecord.
func (s *Service) shipTell(ctx context.Context, sh *shipRun) {
	r, in, reviewed, res := sh.r, sh.in, sh.reviewed, sh.res
	pr := res.Pull
	if reviewed && sh.unchecked != "" || !reviewed {
		s.carryAudits(ctx, r.id, pr.ID, pr.Number)
	}
	if !reviewed {
		return
	}
	_, _, text := s.shipWords(ctx, sh)
	if in.session != 0 && !in.ended {
		if _, err := s.queueMessage(ctx, in.session, text, fmt.Sprintf("shipped:%d", r.id), messageFrom{}, true); err != nil {
			s.log.WarnContext(ctx, "autopilot: tell a session its pull request opened", "round", r.id, "err", err)
		}
	}
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
func (s *Service) endRoundReviewed(ctx context.Context, r *roundRow, unchecked string, existing bool) error {
	if r.state == RoundDone {
		return s.write(ctx, SystemActor(), func(t *tx) error { return s.endShippedStep(ctx, t, r.id) })
	}
	found, fixed, rejected := 0, 0, 0
	opened := "Waypoint opened the pull request."
	if existing {
		opened = "The pull request was already open."
	}
	body := "The adversarial review left nothing for the session to do. " + opened
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
			body = fmt.Sprintf("%s's audit #%d found %s: %d fixed, %d rejected. %s", cmp(a.Model, "another model"), a.ID, plural1(found, "finding"), fixed, rejected, opened)
		}
	}
	if unchecked != "" {
		body += " " + uncheckedLines(unchecked).EndReason
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
		return s.endShippedStep(ctx, t, r.id)
	})
}

// endShippedStep ends the autopilot step carrying a round that has shipped
// (none: nothing to do).
func (s *Service) endShippedStep(ctx context.Context, t *tx, round int64) error {
	var session int64
	err := t.QueryRowContext(ctx, `SELECT session_id FROM autopilot_steps WHERE review_id = ? AND state IN (?, ?, ?) ORDER BY id DESC LIMIT 1`,
		round, StepDue, StepWaiting, StepStarting).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.stopStep(ctx, t, session, "shipped", "Waypoint opened the pull request after the adversarial review")
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
	return s.endHeldRunWith(ctx, actor, issueID, agent, nil)
}

// endHeldRunWith is endHeldRun with more written in the same transaction.
func (s *Service) endHeldRunWith(ctx context.Context, actor *Actor, issueID int64, agent string, also func(t *tx) error) error {
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
		if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`, issueID, agent); err != nil {
			return err
		}
		if also != nil {
			return also(t)
		}
		return nil
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
			continue
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
func (s *Service) roundFacts(ctx context.Context, r *roundRow) (facts string, ran bool) {
	round := r.id
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
			todo, todoErr := s.checkTodo(ctx, a)
			switch {
			case a.Status == AuditQueued || a.Status == AuditRunning:
				parts = append(parts, "the check of its marks hadn't finished")
			case a.Status != AuditDone:
				parts = append(parts, "the check of its marks didn't run ("+cmp(strings.TrimSuffix(strings.TrimSpace(a.Error), "."), a.Status)+")")
			case todoErr != nil:
				parts = append(parts, "the check of its marks couldn't be read")
			case len(todo) > 0:
				ran = true
				parts = append(parts, fmt.Sprintf("the check of its marks disagreed with %s", plural1(len(todo), "mark")))
			default:
				ran = true
				parts = append(parts, "the check of its marks agreed")
			}
		}
	}
	if un := s.uncheckedOf(ctx, r, false); un != "" {
		parts = append(parts, un)
	}
	return strings.Join(parts, "; "), ran
}

// factsOfRound is roundFacts for a round by id ("": unknown or nothing recorded).
func (s *Service) factsOfRound(ctx context.Context, round int64) (facts string, ran bool) {
	r, err := roundByID(ctx, s.db.R, round)
	if err != nil || r == nil {
		return "", false
	}
	return s.roundFacts(ctx, r)
}

// loopRounds is the round and the rounds of its loop before it (the run's
// earlier rounds of the same change), oldest first.
func (s *Service) loopRounds(ctx context.Context, r *roundRow) []int64 {
	loop, err := s.againLoopOf(ctx, r.id, r.runID.Int64)
	if err != nil {
		s.log.WarnContext(ctx, "read a round's earlier rounds", "round", r.id, "err", err)
		return []int64{r.id}
	}
	if loop == nil {
		return []int64{r.id}
	}
	return append(slices.Clone(loop.earlier), r.id)
}

// uncheckedOf says why marks of the round, or of an earlier round of its
// loop, stand unchecked ("": none do): the round's own check, or one of an
// earlier round that didn't run (own: the round's own check counts). One
// reason once, however many rounds share it.
func (s *Service) uncheckedOf(ctx context.Context, r *roundRow, own bool) string {
	var whys []string
	for _, id := range s.loopRounds(ctx, r) {
		if id == r.id && !own {
			continue
		}
		if why := s.checkDidntRun(ctx, id); why != "" && !slices.Contains(whys, why) {
			whys = append(whys, why)
		}
	}
	return strings.Join(whys, "; and ")
}

// release lines: what every text of a ship that carries marks nobody checked
// says, from the one reason (uncheckedOf): the pull request's description, the
// session's message, the task's comment and the round's end.
type release struct{ Body, Session, EndReason, because string }

func uncheckedLines(unchecked string) release {
	return release{
		Body:      "Waypoint opened this: " + unchecked + ". The marks it didn't check go to the pull request's review gate.",
		Session:   "Waypoint: " + unchecked + ", so the marks you made stand unchecked and go to the pull request's review gate.",
		because:   unchecked,
		EndReason: "Its marks stand unchecked (" + unchecked + ") and go to the pull request's review gate.",
	}
}

// Comment is the task's comment on the move to In Review.
func (l release) Comment(url string, existing bool) string {
	if existing {
		return fmt.Sprintf("%s; %s was already open, so Waypoint moved the task on; the marks it didn't check go to the pull request's review gate, which still applies with CI.", l.because, url)
	}
	return fmt.Sprintf("%s, so Waypoint opened %s anyway; the marks it didn't check go to the pull request's review gate, which still applies with CI.", l.because, url)
}

// carryAudits makes the audits of a round the pull request's, so what they
// found and nobody decided holds its review gate (the gate reads the pull
// request's newest finished audit): the round's own and the check of its
// marks.
func (s *Service) carryAudits(ctx context.Context, round, prID int64, number int) {
	if r, _ := roundByID(ctx, s.db.R, round); r != nil {
		for _, e := range s.loopRounds(ctx, r) {
			if e == round || s.checkDidntRun(ctx, e) == "" {
				continue
			}
			if ra, err := s.roundAuditOf(ctx, e); err == nil && ra.id != 0 {
				s.reopenUnverified(ctx, ra.id)
				if err := s.handOverRoundAudit(ctx, ra.id, prID); err != nil {
					s.log.WarnContext(ctx, "carry an earlier round's audit to its pull request", "round", e, "audit", ra.id, "err", err)
				}
			}
		}
	}
	if ra, err := s.roundAuditOf(ctx, round); err == nil && ra.id != 0 {
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
		res, err := t.ExecContext(ctx, `UPDATE code_audits SET purpose = '', pr_id = ?, pr_number = ? WHERE id = ? AND purpose = ?`, prID, number, id, AuditCheck)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			var projectID int64
			if err := t.QueryRowContext(ctx, `SELECT project_id FROM code_audits WHERE id = ?`, id).Scan(&projectID); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvCodeAudit, ProjectID: projectID, Field: "pr", Data: map[string]any{"audit": id, "pr_id": prID}})
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "carry a round's check to its pull request", "round", round, "audit", id, "err", err)
		return
	}
	s.log.InfoContext(ctx, "round's check carried to its pull request", "round", round, "audit", id, "pr", prID)
}

// uncheckedNote starts the note of a rejection nobody checked.
const uncheckedNote = "Rejected by the session, unchecked: "

// reopenUnverified opens again the critical and high findings of an audit
// the session rejected and nothing checked, so they hold the pull request's
// merge until a person or its session decides them, and marks the medium and
// low ones as unchecked, to be listed (WP-1370).
func (s *Service) reopenUnverified(ctx context.Context, auditID int64) {
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE code_audit_findings SET state = 'open',
			note = trim(? || note), decided_at = NULL,
			evidence = '', decided_model = '', decided_effort = '', decided_strength = ''
			WHERE audit_id = ? AND state = 'rejected' AND severity IN ('critical', 'high')`, uncheckedNote, auditID)
		if err != nil {
			return err
		}
		reopened, _ := res.RowsAffected()
		res, err = t.ExecContext(ctx, `UPDATE code_audit_findings SET note = trim(? || note)
			WHERE audit_id = ? AND state = 'rejected' AND severity IN ('medium', 'low') AND note NOT LIKE ?`, uncheckedNote, auditID, uncheckedNote+"%")
		if err != nil {
			return err
		}
		marked, _ := res.RowsAffected()
		if reopened+marked == 0 {
			return nil
		}
		var projectID int64
		if err := t.QueryRowContext(ctx, `SELECT project_id FROM code_audits WHERE id = ?`, auditID).Scan(&projectID); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvCodeAudit, ProjectID: projectID, Field: "marks", Data: map[string]any{"audit": auditID, "reopened": reopened}})
		return nil
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
		var why string
		if s.db.R.QueryRowContext(ctx, `SELECT coalesce(json_extract(before_pr, '$.check_err'), '') FROM work_log WHERE id = ? AND before_pr != ''`, round).Scan(&why) != nil || why == "" {
			return ""
		}
		return "the check of the marks didn't run: couldn't be asked: " + strings.TrimSuffix(why, ".")
	}
	a, err := s.auditRow(ctx, id)
	if err != nil || a.Status == AuditQueued || a.Status == AuditRunning || a.Status == AuditDone {
		return ""
	}
	why := strings.TrimSuffix(strings.TrimSpace(a.Error), ".")
	return "the check of the marks didn't run: " + cmp(why, a.Status)
}

// openPROfBranch is the open pull request from a round's branch (0: none).
func (s *Service) openPROfBranch(ctx context.Context, bp *beforePR) int64 {
	var id int64
	err := s.db.R.QueryRowContext(ctx, `SELECT id FROM pull_requests WHERE repo = ? COLLATE NOCASE AND head_ref = ? AND state IN ('open', 'draft') AND (head_repo = '' OR head_repo = repo COLLATE NOCASE) ORDER BY id DESC LIMIT 1`, bp.Repo, bp.Branch).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.WarnContext(ctx, "read the pull request open from a branch", "repo", bp.Repo, "branch", bp.Branch, "err", err)
	}
	return id
}

// pullTaskKeys are the keys of the tasks a pull request is linked to,
// whoever may see them: what decides whether it is the task's to arm. A
// link counts by the pull request's address (a task linked by wp ship or by
// hand) or as the forge's own pull link.
func (s *Service) pullTaskKeys(ctx context.Context, repo string, number int) ([]string, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT p.key || '-' || i.number FROM external_links el
		JOIN issues i ON i.id = el.issue_id JOIN projects p ON p.id = i.project_id
		WHERE (el.kind = 'pull' AND el.repo = ? COLLATE NOCASE AND el.number = ?)
		   OR el.url IN (SELECT pr.url FROM pull_requests pr WHERE pr.repo = ? COLLATE NOCASE AND pr.number = ?)
		ORDER BY 1`, repo, number, repo, number)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// branchMovedSinceReview says whether the forge's branch is at another
// commit than the one the round's review read: its audit's ToSHA when it
// reported one, else the commit the round was shipped at. A round with
// neither, or a forge that can't read branches, is not asked. A forge that
// can't say where the branch is leaves the answer unknown, and that is an
// error: shipRound's caller tries again (shipTried), and nothing opens
// meanwhile.
func (s *Service) branchMovedSinceReview(ctx context.Context, actor *Actor, r *roundRow, bp *beforePR) (bool, error) {
	read := s.reviewedHead(ctx, r, bp)
	if read == "" {
		return false, fmt.Errorf("waypoint can't tell which commit %s's review read, so it didn't open a pull request for %s", r.key, bp.Branch)
	}
	cl, ok := s.Forges.(ForgeKitReader)
	if !ok {
		return false, fmt.Errorf("this forge client can't say where %s is, so Waypoint didn't open its pull request after a review", bp.Branch)
	}
	repos, err := s.ListProjectRepos(ctx, actor, bp.Project)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(repos, func(pr ProjectRepo) bool { return strings.EqualFold(pr.Repo, bp.Repo) })
	if i < 0 {
		return false, nil
	}
	f, err := s.forgeByID(ctx, repos[i].ForgeID)
	if err != nil {
		return false, err
	}
	fctx, cancel := context.WithTimeout(forgePatient(ctx), openPRBudget)
	defer cancel()
	tip, err := cl.BranchHead(fctx, f, bp.Repo, bp.Branch)
	if errors.Is(err, ErrNotFound) {
		return false, fmt.Errorf("%s isn't on the forge any more, so Waypoint didn't open its pull request: %w", bp.Branch, err)
	}
	if err != nil || tip == "" {
		return false, fmt.Errorf("the forge couldn't say the tip of %s, so Waypoint didn't open its pull request: %v", bp.Branch, cmp(errString(err), "no commit"))
	}
	return !strings.EqualFold(tip, read), nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// heldCarries says a round's held ship leaves the round waiting on something
// that has a deadline (decideRound's wait or tell): then nothing is lost by
// not trying again until that changes.
func (s *Service) heldCarries(ctx context.Context, round int64) bool {
	in, err := s.roundInOf(ctx, round)
	if err != nil {
		return false
	}
	switch out := decideRound(in); out.Do {
	case doWait, doTell:
		return true
	}
	return false
}

// heldOutcome is what a ship held at the pull request's opening comes to
// (WP-1370). It is a success only when the round now waits on something with
// a deadline (decideRound: a wait or a tell): another review round was
// scheduled (the round is superseded and waits on it), the check of the marks
// started, or the marks wait on the session. Any other hold, of a round that
// was reviewed or not, is a failed try with its text as the error, so the
// back-off, the give-up and the person's notice apply: nothing logs and
// returns nil.
func (s *Service) heldOutcome(ctx context.Context, round int64, reviewed bool, held string) error {
	if reviewed && s.heldCarries(ctx, round) {
		s.log.Info("autopilot: a reviewed ship was held, and the round waits", "round", round, "held", clipText(held, 200))
		return nil
	}
	return errors.New(held)
}
