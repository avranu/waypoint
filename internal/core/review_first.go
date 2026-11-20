package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Reviews before the pull request (WP-688, WP-ADR60). When the person's
// autopilot settings ask for adversarial reviews (whether autopilot is on
// or off: the switch only starts tasks, WP-1007), wp ship of a task in one of
// their sessions pushes the branch, but Waypoint holds the pull request:
// it records the task's review round, with the branch (before_pr), and
// tells the session to end its turn. At that Stop the round is due in a
// minute (reviewStartDelay); Grok audits the branch, and the round types
// another model's findings, or the round's own prompt, which ends with wp ship
// --pr. That finds the round under way and opens the pull request; the
// Stop after it ends the round with its report and autopilot moves on as
// before.
//
// wp ship says what it wants (ShipReview, ShipPR, ShipSkip, or ShipAuto
// for whichever fits), and the answer is the same for the same intent and
// state: where the intent can't be had, the obvious next step is taken
// and a notice says what happened and how to override it.
//
// Anything the gate can't see through (gateFor) opens the pull request at
// once, and its round, if any, runs after it as before; where autopilot
// wanted the review, the agent is told why and the task's work log keeps
// it (WP-803).

// reviewStartDelay: a review starts a minute after the Stop that asks for
// it, whatever the wait for the next task (WP-689).
const reviewStartDelay = 60 * 1000

// beforePR is where a round before its pull request reviews.
type beforePR struct {
	Project string `json:"project"`
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	// What wp ship asked for, so Waypoint can open it if the review can't
	// run and the session never ships (WP-752). Urgent is only for a pull
	// request not open yet: one already open is labelled by the held ship
	// itself (WP-774).
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
	Ready  bool   `json:"ready,omitempty"`
	Urgent bool   `json:"urgent,omitempty"`
}

// roundBeforePR is the round's branch when it runs before its pull
// request (nil: after it).
func roundBeforePR(ctx context.Context, q queryer, id int64) *beforePR {
	var raw string
	if q.QueryRowContext(ctx, `SELECT before_pr FROM work_log WHERE id = ?`, id).Scan(&raw) != nil || raw == "" {
		return nil
	}
	var b beforePR
	if json.Unmarshal([]byte(raw), &b) != nil || b.Branch == "" {
		return nil
	}
	return &b
}

// heldUrgent is true when the session's newest review of the task was held
// by a wp ship --urgent of this branch: the ship that opens the pull request
// after the review doesn't have to say it again (WP-543). Only a pull
// request not open yet asks, so taking the label off later stays off.
func (s *Service) heldUrgent(ctx context.Context, actor *Actor, task, branch string) bool {
	if actor == nil || actor.Session == 0 || strings.TrimSpace(task) == "" {
		return false
	}
	issueID, err := s.resolveIssueID(ctx, s.db.R, task)
	if err != nil {
		return false
	}
	var round int64
	if s.db.R.QueryRowContext(ctx, `SELECT w.id FROM work_log w JOIN agent_runs r ON r.id = w.run_id
		WHERE w.kind = 'review' AND w.session_id = ? AND w.issue_id = ? AND r.ended_at IS NULL ORDER BY w.id DESC LIMIT 1`, actor.Session, issueID).Scan(&round) != nil {
		return false
	}
	bp := roundBeforePR(ctx, s.db.R, round)
	return bp != nil && bp.Urgent && bp.Branch == branch
}

// Ship intents: what wp ship says about the task's adversarial review
// before its pull request (WP-688, WP-ADR60).
const (
	ShipAuto   = "auto"   // whichever fits: the review if none ran yet, else the pull request
	ShipReview = "review" // the review, before (or on) the pull request
	ShipPR     = "pr"     // the pull request: the review ran, or runs first
	ShipSkip   = "skip"   // the pull request without the review; OpenPRInput.Reason says why
)

// reviewGate is the task's run that autopilot reviews before its pull
// request, or why it can't (no).
type reviewGate struct {
	x            *AgentSession
	run, issueID int64
	pid          int64 // the session's project, for its autopilot settings
	taskPID      int64 // the task's own, for what's recorded about the task
	key          string
	lim          AutopilotLimits
	no           string
	// wanted: autopilot reviews before pull requests in this session, so a
	// no is a review it wanted and didn't get (WP-803).
	wanted bool
}

// gateFor finds the run a ship's review belongs to. The gate holds only
// what autopilot can see through: an agent session of the person's (not a
// task session) with reviews on in its project, a runner on its
// machine that types reviews, and the task's run open in it. take: with no
// run open (the claim lapsed, the run was released or ended, or the task
// was never taken here), the session takes the task again, as wp ship's
// finish would a moment later, so its review still comes before the pull
// request (WP-803). Without take, no run open is a no. asked: the ship asked
// for the review (wp ship --review), so the review setting doesn't decide.
func (s *Service) gateFor(ctx context.Context, actor *Actor, task string, take, asked bool) (*reviewGate, error) {
	g := &reviewGate{}
	task = strings.TrimSpace(task)
	if actor == nil || actor.User == nil || actor.Session == 0 {
		g.no = "this isn't an agent session"
		return g, nil
	}
	if task == "" {
		g.no = "no task is named or held"
		return g, nil
	}
	x, err := scanSession(s.db.R.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, actor.Session), s.nowMS())
	if errors.Is(err, sql.ErrNoRows) {
		g.no = "this isn't an agent session"
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case x.Kind == SessionTask:
		g.no = "a task session isn't reviewed by autopilot"
	case x.EndedAt != nil:
		g.no = "this session has ended"
	case x.userID != actor.User.ID:
		g.no = "this session isn't yours"
	}
	if g.no != "" {
		return g, nil
	}
	g.x = x
	if g.issueID, err = s.resolveIssueID(ctx, s.db.R, task); err != nil {
		g.no = task + " isn't a task here"
		return g, nil //nolint:nilerr // a task it can't name has no round
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT i.project_id, pj.key || '-' || i.number FROM issues i JOIN projects pj ON pj.id = i.project_id WHERE i.id = ?`,
		g.issueID).Scan(&g.taskPID, &g.key); err != nil {
		return nil, err
	}
	g.pid = g.taskPID
	if x.projectID.Valid {
		g.pid = x.projectID.Int64
	}
	// Autopilot's switch is about starting the next task, not about the
	// task shipping now: its review follows the review setting alone, and a
	// wp ship --review asks for one whatever the setting (WP-1007).
	_, lim, err := s.autopilotFor(ctx, s.db.R, x.userID, g.pid)
	if err != nil {
		return nil, err
	}
	g.lim = lim
	if !asked && !valueOf(lim.Review) {
		g.no = "your reviews before pull requests are off here"
		return g, nil
	}
	g.wanted = true
	if r := s.runnerOnMachine(ctx, x.userID, x.Machine); r == nil || !r.Online || !slices.Contains(r.Features, FeatureAutopilot) || !slices.Contains(r.Features, FeatureAutopilotReview) {
		g.no = "no runner online on this machine types reviews"
		return g, nil
	}
	if g.run, err = s.openRunIn(ctx, x.ID, g.issueID); err != nil || g.run != 0 {
		return g, err
	}
	if !take {
		g.no = g.key + " isn't under way in this session (wp next takes it)"
		return g, nil
	}
	if g.no, err = s.retakeForReview(ctx, actor, g); err != nil || g.no != "" {
		return g, err
	}
	if g.run, err = s.openRunIn(ctx, x.ID, g.issueID); err == nil && g.run == 0 {
		g.no = g.key + " isn't under way in this session (wp next takes it)"
	}
	return g, err
}

// skipReviewStep is why a step stands down for wp ship --skip-review.
const skipReviewStep = "the agent skipped the review with wp ship --skip-review"

// skipRoundsElsewhere marks skipped the task's review rounds still pending
// in the person's sessions (except: the round this ship already handled, 0
// for none), and stands their steps down. A wp ship --skip-review
// from a session that doesn't hold the task (another session shipping for
// a held one, a person's terminal) skips the task's review wherever it
// waits. Without this the round waited on in its own session until
// something else ended it, and its record named that instead: a /clear
// read as "you cleared the session" though the review had been skipped.
// The notice says when a skipped round's audit goes on (key names the task).
// A failed write is logged, not returned: the pull request the agent asked
// for opens either way.
func (s *Service) skipRoundsElsewhere(ctx context.Context, actor *Actor, issueID, except int64, key, reason string) string {
	if issueID == 0 || actor.id() == 0 {
		return ""
	}
	why := "the agent skipped it: " + strings.TrimSpace(reason)
	var skipped []int64
	err := s.write(ctx, actor, func(t *tx) error {
		skipped = skipped[:0]
		rows, err := t.QueryContext(ctx, `SELECT w.id, w.session_id FROM work_log w JOIN agent_sessions x ON x.id = w.session_id
			WHERE w.kind = 'review' AND w.issue_id = ? AND w.state = ? AND w.id != ? AND x.user_id = ? ORDER BY w.id`,
			issueID, RoundPending, except, actor.id())
		if err != nil {
			return err
		}
		type pending struct{ round, session int64 }
		var list []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.round, &p.session); err != nil {
				rows.Close()
				return err
			}
			list = append(list, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range list {
			moved, err := s.setRound(ctx, t, p.round, []string{RoundPending}, RoundSkipped, why)
			if err != nil {
				return err
			}
			if !moved {
				continue // it started just now: the review is running
			}
			if err := s.stopRoundStep(ctx, t, p.session, p.round, skipReviewStep); err != nil {
				return err
			}
			if err := s.recordAgentSkip(ctx, t, issueID, p.round); err != nil {
				return err
			}
			skipped = append(skipped, p.round)
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "review skip: other sessions' rounds not skipped", "task", key, "err", err)
		return ""
	}
	for _, id := range skipped {
		if s.roundAuditLives(ctx, id) {
			return "The other model's audit for " + key + "'s review goes on without the round: its findings go to the pull request, and a high or critical one holds the merge until it's fixed or dismissed."
		}
	}
	return ""
}

// recordAgentSkip records in triage that the agent skipped the round's
// review: its triage row, which had said review, now says skip; a round
// scheduled with no assessment gets one, with the branch its wp ship
// named and no facts about the change (this ship's are another session's).
func (s *Service) recordAgentSkip(ctx context.Context, t *tx, issueID, round int64) error {
	res, err := t.ExecContext(ctx, `UPDATE review_triage SET action = ?, why = ? WHERE round_id = ?`, triageSkip, triageWhyAgentSkip, round)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	tr := triageRow{issueID: issueID, features: triageFeatures(nil), at: s.nowMS()}
	if err := t.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE id = ?`, issueID).Scan(&tr.projectID); err != nil {
		return err
	}
	if bp := roundBeforePR(ctx, t, round); bp != nil {
		tr.repo, tr.head, tr.base = bp.Repo, bp.Branch, bp.Base
	}
	tr.decision = decideTriage(tr.features, "", ShipSkip, 0)
	return tr.insert(ctx, t, round)
}

// openRunIn is the task's run open in the session (0: none).
func (s *Service) openRunIn(ctx context.Context, session, issueID int64) (int64, error) {
	var run int64
	err := s.db.R.QueryRowContext(ctx, `SELECT id FROM agent_runs WHERE session_id = ? AND issue_id = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`,
		session, issueID).Scan(&run)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return run, err
}

// retakeForReview takes the task in the session again for its review
// before the pull request, as wp next KEY would; wp ship's finish takes it
// anyway once the pull request is open. It says why it couldn't ("":
// taken). Another live session holding the task keeps it.
func (s *Service) retakeForReview(ctx context.Context, actor *Actor, g *reviewGate) (string, error) {
	id := actor.agentName()
	if id == "" {
		return g.key + " isn't under way in this session (wp next takes it)", nil
	}
	if where := s.heldElsewhere(ctx, actor, g.issueID, id); where != "" {
		return g.key + " is under way elsewhere: " + where, nil
	}
	err := s.claimIssue(ctx, actor, g.issueID, id, true)
	switch {
	case errors.Is(err, errAlreadyClaimed):
		return g.key + " was just taken by another agent", nil
	case IsInvalid(err) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden):
		return "this session can't take it again: " + err.Error(), nil
	case err != nil:
		// The gate was read-only before WP-803: a failed take opens the
		// pull request rather than failing the ship.
		s.log.WarnContext(ctx, "review gate: taking the task again failed", "task", g.key, "err", err)
		return "taking it again in this session failed: " + err.Error(), nil
	}
	return "", nil
}

// ReviewsFirst says whether wp ship can schedule the task's adversarial
// review in this session, before its pull request (WP-770).
func (s *Service) ReviewsFirst(ctx context.Context, actor *Actor, task string) bool {
	g, err := s.gateFor(ctx, actor, task, false, false)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read reviews first", "task", task, "err", err)
		}
		return false
	}
	return g.no == ""
}

// reviewFirst decides what wp ship's intent (in.Review) does about the
// task's adversarial review as its pull request is about to open, or, when
// existing, is already open. open: go on with the pull request; else held
// is what to tell the agent, the branch pushed and no pull request opened.
// notice goes before either: what happened that the intent didn't say, and
// how to get the other thing.
func (s *Service) reviewFirst(ctx context.Context, actor *Actor, p *Project, in OpenPRInput, repo, head, base string, existing bool) (open bool, held, notice string, err error) {
	intent := cmp(in.Review, ShipAuto)
	switch intent {
	case ShipAuto, ShipPR, ShipReview:
		if existing && intent != ShipReview {
			return true, "", "", nil
		}
	case ShipSkip:
		if strings.TrimSpace(in.Reason) == "" {
			return false, "", "", invalid("say why the review is skipped")
		}
	default:
		return false, "", "", invalid("review must be auto, review, pr or skip")
	}
	g, err := s.gateFor(ctx, actor, cmp(in.For, in.Issue), intent != ShipSkip, intent == ShipReview)
	if err != nil {
		return false, "", "", err
	}
	if g.no != "" {
		// The pull request goes ahead: holding it for a review the session
		// would do itself costs the session's tokens and ships nothing
		// sooner (WP-1007).
		switch {
		case intent == ShipSkip:
			issueID := g.issueID
			if issueID == 0 {
				// No such task is an answer; a failed read is logged, and the
				// pull request opens either way (as skipRoundsElsewhere's write).
				var rerr error
				if issueID, rerr = s.resolveIssueID(ctx, s.db.R, cmp(in.For, in.Issue)); readFault(rerr) && !missingOrDenied(rerr) {
					s.log.WarnContext(ctx, "skip review: find the task whose waiting rounds end", "err", rerr)
				}
			}
			if in.noSweep {
				return true, "", "", nil
			}
			return true, "", s.skipRoundsElsewhere(ctx, actor, issueID, 0, cmp(in.For, in.Issue), in.Reason), nil
		case intent == ShipReview && existing:
			return true, "", "No adversarial review can be scheduled here: " + g.no + "; the pull request stays open.", nil
		case intent == ShipReview:
			return true, "", "No adversarial review can be scheduled here: " + g.no + ", so the pull request opens without it.", nil
		case g.wanted && g.issueID != 0 && s.ownReviewed(ctx, g, in.HeadTime):
			return true, "", "", nil // the session reviewed it itself (WP-1089)
		case g.wanted:
			return true, "", s.noteUnreviewed(ctx, actor, g), nil
		}
		return true, "", "", nil
	}
	var id, ended int64
	var state string
	err = s.db.R.QueryRowContext(ctx, `SELECT id, state, coalesce(ended_at, 0) FROM work_log WHERE kind = 'review' AND run_id = ? ORDER BY id DESC LIMIT 1`, g.run).Scan(&id, &state, &ended)
	seen := id // the run's own newest round, for scheduleRound
	if errors.Is(err, sql.ErrNoRows) {
		id, state, ended, err = s.roundBeforeResume(ctx, g, head)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", "", err
	}
	bp := beforePR{Project: p.Key, Repo: repo, Branch: head, Base: base, Title: in.Title, Body: in.Body, Draft: in.Draft, Ready: in.Ready, Urgent: in.Urgent && !existing}
	wait := fmt.Sprintf("Pushed %s; its pull request opens after %s's adversarial review. End your turn now: the review starts after it, and Waypoint gives you its findings here.", head, g.key)
	if existing {
		wait = fmt.Sprintf("Pushed %s; %s's adversarial review comes next, and its open pull request doesn't merge before the review ends. End your turn now: the review starts after it, and Waypoint gives you its findings here.", head, g.key)
	}
	// Every round scheduled below is assessed and recorded (WP-692).
	var assessed *triageRow
	assess := func() *triageRow {
		if assessed == nil {
			assessed = s.assessChange(ctx, actor, g, p.ID, in, repo, head, base, intent)
		}
		return assessed
	}
	schedule := func(replace int64, skip string) error {
		return s.scheduleRound(ctx, actor, g, bp, seen, replace, skip, assess())
	}
	const cancel = " To cancel it: `wp ship --skip-review \"WHY\"`."
	// skipRound records the review skipped (why; triageWhy for its triage
	// row): a new round, skipped, when there is none, else the pending one
	// marked skipped and its step stood down. Its notice says what becomes
	// of an audit still running for it.
	skipRound := func(why, triageWhy, stepWhy string) (string, error) {
		switch state {
		case "":
			tr := *assess()
			tr.decision.Action, tr.decision.Why = triageSkip, triageWhy
			return "", s.scheduleRound(ctx, actor, g, bp, seen, 0, why, &tr)
		case RoundPending:
			var moved bool
			err := s.write(ctx, actor, func(t *tx) error {
				var err error
				moved, err = s.setRound(ctx, t, id, []string{RoundPending}, RoundSkipped, why)
				if err != nil || !moved {
					return err // (not moved: it started just now, and the review is running)
				}
				if err := s.stopRoundStep(ctx, t, g.x.ID, id, stepWhy); err != nil {
					return err
				}
				// Triage had said review; the record says what happened.
				res, err := t.ExecContext(ctx, `UPDATE review_triage SET action = ?, why = ? WHERE round_id = ?`, triageSkip, triageWhy, id)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					return nil
				}
				// The round was scheduled with no assessment: record one now.
				tr := *assess()
				tr.decision.Action, tr.decision.Why = triageSkip, triageWhy
				return tr.insert(ctx, t, id)
			})
			if err == nil && moved && s.roundAuditLives(ctx, id) {
				return "The other model's audit for " + g.key + "'s review goes on without the round: its findings go to the pull request, and a high or critical one holds the merge until it's fixed or dismissed.", nil
			}
			return "", err
		}
		return "", nil
	}
	switch intent {
	case ShipSkip:
		notice, err = skipRound("the agent skipped it: "+strings.TrimSpace(in.Reason), triageWhyAgentSkip, skipReviewStep)
		if err == nil && !in.noSweep {
			notice = strings.TrimSpace(notice + " " + s.skipRoundsElsewhere(ctx, actor, g.issueID, id, g.key, in.Reason))
		}
		return err == nil, "", notice, err
	case ShipAuto, ShipPR:
		reasked := false
		if state == RoundSkipped {
			// A rule's skip holds for the change it read. This ship has no
			// pull request from it (one failed to open, or it's another
			// branch), so what's shipped now is assessed for itself.
			byRule, err := s.skippedByRule(ctx, id)
			if err != nil {
				return false, "", "", err
			}
			if byRule {
				state = ""
			}
		}
		if state == RoundRunning {
			// This is its ship: new findings that score serious, or fixes
			// that look partial, get another round first, round after
			// round while they keep coming (WP-695, WP-1320).
			held, notice, err := s.reviewAgain(ctx, actor, g, bp, id, head)
			if err != nil || held != "" {
				return false, held, notice, err
			}
		}
		if state == RoundInterrupted {
			// Taken from the session before it reported: the review didn't
			// happen, so it runs again before the pull request opens
			// (WP-823), unless it keeps being interrupted.
			again, err := askAgainAfterInterrupt(ctx, s.db.R, g.x.ID, g.issueID)
			if err != nil {
				return false, "", "", err
			}
			if !again {
				return true, "", fmt.Sprintf("%s's adversarial review was interrupted %d times before it reported, so the pull request goes ahead without it. To review it anyway: `wp ship --review`.", g.key, reviewInterruptsMax), nil
			}
			state, reasked = "", true
		}
		if state != "" && state != RoundPending {
			return true, "", "", nil // under way (this is its ship), or over
		}
		// The session reviewed the change itself since its last round
		// (WP-1089): that review stands for the round, and the pull request
		// opens, unless it found nothing and more was committed since. A
		// report attached by hand is the agent's word: its skip.
		own, err := s.ownReviewFor(ctx, g)
		if err != nil {
			return false, "", "", err
		}
		ownStale := own.id != 0 && !own.covers(in.HeadTime)
		if own.id != 0 && !ownStale {
			why, triageWhy := ownReviewWhy(g.key), triageWhyOwnReview
			if own.attached {
				why, triageWhy = ownReviewAttachedWhy(g.key), triageWhyAgentSkip
			}
			notice, err := skipRound(why, triageWhy, "the session ran its own adversarial review")
			return err == nil, "", notice, err
		}
		var auditNotice string
		if state == RoundPending {
			// The session may have read the round's audit itself (WP-919).
			done, n, err := s.shipAudited(ctx, actor, g, id)
			if err != nil || done {
				return done, "", n, err
			}
			auditNotice = n
		}
		if state == "" {
			if tr := assess(); tr.decision.Action == triageSkip {
				if err := schedule(0, tr.roundReason()); err != nil {
					return false, "", "", err
				}
				return true, "", tr.notice(), nil
			}
			if err := schedule(0, ""); err != nil {
				return false, "", "", err
			}
		}
		if reasked {
			notice = g.key + "'s adversarial review was interrupted before it reported, so it runs again and the pull request waits for it. To skip the review: `wp ship --skip-review \"WHY\"`."
		} else if auditNotice != "" {
			notice = auditNotice
		} else if ownStale {
			notice = "The session's own review found nothing and more was committed since, so a review of what it didn't see comes before the pull request. To skip the review: `wp ship --skip-review \"WHY\"`."
		} else if intent == ShipPR {
			notice = "The task's adversarial review comes before its pull request, so the pull request waits for it. To skip the review: `wp ship --skip-review \"WHY\"`."
		}
		return false, wait, notice, nil
	}
	// ShipReview.
	switch {
	case state == RoundPending:
		return false, wait, "", nil
	case state == RoundRunning && !in.Force:
		return false, fmt.Sprintf("Pushed %s. %s's adversarial review is this one, under way: when it's done, run `wp ship --pr`.", head, g.key),
			"A review of this change is already under way. To start another after this turn anyway: `wp ship --review --force`.", nil
	case state == RoundRunning:
		notice = "The review under way is replaced: another starts after this turn." + cancel
		err = schedule(id, "")
	case state == RoundDone && !in.Force && in.HeadTime > 0 && (in.HeadTime+1)*1000 <= ended:
		// (Commit times are whole seconds: one in the round's last second
		// counts as since, so the doubt reviews again.)
		notice = "A review already finished on this change and nothing was committed since, so the pull request opens now. To review it again anyway: `wp ship --review --force`."
		if existing {
			notice = "A review already finished on this change and nothing was committed since; its pull request is open. To review it again anyway: `wp ship --review --force`."
		}
		return true, "", notice, nil
	case state == RoundDone && in.Force:
		notice = "A review already finished on this change; another is scheduled, as asked." + cancel
		err = schedule(0, "")
	case state == RoundDone:
		notice = "A review already finished on this change; another is scheduled for what was committed since." + cancel
		err = schedule(0, "")
	default: // none yet, or one that never ran or broke off
		err = schedule(0, "")
	}
	if err != nil {
		return false, "", "", err
	}
	return false, wait, notice, nil
}

// shipAudited is wp ship --pr (or a plain wp ship) of a task whose round is
// still pending while its audit is done (WP-919). The session read the audit
// itself (wp audit show, wp audit mark) instead of ending its turn for the
// round's prompt, and that prompt can't go in while the session is busy, so
// a round that waited for it held the pull request for as long as the
// session kept working. Once nothing in the audit is left to decide, the ship
// is the round's report: the round ends done with the audit's findings as
// what it found, a step about to type its prompt stands down, and the pull
// request opens. Findings still to decide hold it, and the notice names them
// and how to decide them here; so does an audit still under way.
func (s *Service) shipAudited(ctx context.Context, actor *Actor, g *reviewGate, roundID int64) (bool, string, error) {
	ra, err := s.roundAuditOf(ctx, roundID)
	if err != nil || ra.id == 0 || ra.note != "" {
		// No audit yet, or one the round gave up on: the session reviews on
		// the round's own prompt.
		return false, "", err
	}
	a, err := s.auditRow(ctx, ra.id)
	if errors.Is(err, ErrNotFound) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	switch a.Status {
	case AuditQueued, AuditRunning:
		return false, fmt.Sprintf("%s's adversarial review is audit #%d, still under way (`wp audit show %d`), so the pull request waits for it. To skip the review: `wp ship --skip-review \"WHY\"`.", g.key, a.ID, a.ID), nil
	case AuditDone:
	default:
		return false, "", nil // the round falls back or asks another model: planReview's to decide
	}
	if err := s.loadFindings(ctx, a); err != nil {
		return false, "", err
	}
	if a.readNothing() {
		return false, "", nil // nothing was read, so the round's own prompt reviews
	}
	if todo := s.roundTodo(ctx, a); len(todo) > 0 {
		ids := make([]string, len(todo))
		for i, f := range todo {
			ids[i] = f.ID
		}
		return false, fmt.Sprintf("%s's adversarial review is audit #%d, with %s still to decide: %s. Read them (`wp audit show %d`), mark each (`wp audit mark %s fixed -m \"what you did\"` or `wp audit mark %s rejected -m \"why\"`), then run `wp ship --pr` again; or end your turn, and the review gives them to you.",
			g.key, a.ID, plural1(len(todo), "finding"), strings.Join(ids, ", "), a.ID, ids[0], ids[0]), nil
	}
	items := make([]WorkIssue, 0, len(a.Findings))
	fixed, rejected := 0, 0
	for _, f := range a.Findings {
		out := "open"
		switch f.State {
		case "fixed":
			out = "fixed"
			fixed++
		case "rejected":
			out = "rejected"
			rejected++
		}
		items = append(items, WorkIssue{Claim: f.Title, File: f.Path, Outcome: out})
	}
	raw, _ := json.Marshal(cleanIssues(items))
	who := cmp(a.Model, "Grok")
	body := fmt.Sprintf("%s audited the change (audit #%d: %s/audits/%d) and found nothing to fix; the session shipped it with wp ship.", who, a.ID, s.BaseURL(), a.ID)
	if n := len(a.Findings); n > 0 {
		// Findings already handed to a babysitter are not the session's to
		// claim: say only what its own marks were.
		body = fmt.Sprintf("%s audited the change (audit #%d: %s/audits/%d) and found %d. The session marked %d fixed and %d rejected itself and shipped it with wp ship.",
			who, a.ID, s.BaseURL(), a.ID, n, fixed, rejected)
	}
	now := s.nowMS()
	ended := false
	err = s.write(ctx, actor, func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE work_log SET state = 'done', body = ?, started_at = coalesce(started_at, ?), ended_at = ?, found = ?, fixed = ?, rejected = ?, items = ?,
			read_state = ?, read_by = ?, read_at = ?, updated_at = ? WHERE id = ? AND kind = 'review' AND state = 'pending'`,
			body, now, now, len(a.Findings), fixed, rejected, string(raw), readDone, readByRules, now, now, roundID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // its prompt went in just now: the round is under way
		}
		ended = true
		s.emitWorkLog(ctx, t, g.issueID, roundID, RoundDone)
		return s.stopRoundStep(ctx, t, g.x.ID, roundID, "the session took its review's audit itself and shipped")
	})
	if err != nil || !ended {
		return false, "", err
	}
	return true, fmt.Sprintf("%s's adversarial review was audit #%d, and nothing in it is left to decide, so the pull request opens.", g.key, a.ID), nil
}

// stopRoundStep stops the session's autopilot step when it is about the
// round, which ended outside it (the agent's own wp ship): a job queued to
// type the round's prompt then finds itself unwanted, and the session's next
// Stop decides afresh, as after a background task's report.
func (s *Service) stopRoundStep(ctx context.Context, t *tx, sessionID, roundID int64, detail string) error {
	var n int
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE session_id = ? AND review_id = ? AND state IN (?, ?, ?)`,
		sessionID, roundID, StepDue, StepWaiting, StepStarting).Scan(&n); err != nil || n == 0 {
		return err
	}
	return s.stopStep(ctx, t, sessionID, "working", detail)
}

// noteUnreviewed records in the task's work log why its pull request opens
// without the adversarial review autopilot wanted before it, and says so
// to the agent (WP-803). The same reason in the same session is recorded
// once. A record that fails doesn't hold the pull request back.
func (s *Service) noteUnreviewed(ctx context.Context, actor *Actor, g *reviewGate) string {
	body := fmt.Sprintf("wp ship went on to the pull request without the adversarial review first: %s.", g.no)
	err := s.write(ctx, actor, func(t *tx) error {
		var seen bool
		if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM work_log WHERE issue_id = ? AND kind = 'note' AND session_id = ? AND body = ?)`,
			g.issueID, g.x.ID, body).Scan(&seen); err != nil || seen {
			return err
		}
		now := s.nowMS()
		var uid any
		if actor.id() != 0 {
			uid = actor.id()
		}
		res, err := t.ExecContext(ctx, `INSERT INTO work_log(issue_id, kind, session_id, user_id, agent, body, created_at, updated_at) VALUES (?, 'note', ?, ?, ?, ?, ?, ?)`,
			g.issueID, g.x.ID, uid, actor.agentName(), body, now, now)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		t.emit(&Event{Kind: EvIssueWorkLog, ProjectID: g.taskPID, IssueID: g.issueID, IssueKey: g.key, New: WorkNote, Data: map[string]any{"entry": id, "state": WorkNote}})
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "unreviewed pull request not recorded", "task", g.key, "err", err)
	}
	return fmt.Sprintf("%s's pull request opens without its adversarial review first: %s.", g.key, g.no)
}

// roundBeforeResume is the round of the same change from the task's run
// before this one in the session, when this run only took over from it: the
// session paused or its claim lapsed, and came back. Without it,
// the review the session already did is forgotten and wp ship --pr asks for
// another. Only a run that stopped short of an end of its own counts (no
// run ended done or released in between), only a round of the same branch,
// and not one still pending: that one runs in no run, so a new one does.
func (s *Service) roundBeforeResume(ctx context.Context, g *reviewGate, branch string) (id int64, state string, ended int64, err error) {
	err = s.db.R.QueryRowContext(ctx, `SELECT w.id, w.state, coalesce(w.ended_at, 0) FROM work_log w
		WHERE w.kind = 'review' AND w.session_id = ? AND w.issue_id = ? AND w.run_id < ? AND w.state != 'pending'
			AND w.before_pr != '' AND json_extract(w.before_pr, '$.branch') = ?
			AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = w.issue_id AND r.id >= w.run_id AND r.id < ?
				AND (r.ended_at IS NULL OR r.outcome NOT IN ('paused', 'expired', 'timeout')))
		ORDER BY w.id DESC LIMIT 1`, g.x.ID, g.issueID, g.run, branch, g.run).Scan(&id, &state, &ended)
	return id, state, ended, err
}

// scheduleRound records the run's round before its pull request: pending,
// so the session's next Stop makes it due, or, with skip, skipped for that
// reason (so no round runs after the pull request either). replace is a
// round under way that the new one replaces. seen is the run's newest
// round when the ship looked (0: none): if another ship recorded one since,
// nothing is, so two at once don't schedule two reviews. tr is the round's
// triage (WP-692), recorded with it.
func (s *Service) scheduleRound(ctx context.Context, actor *Actor, g *reviewGate, bp beforePR, seen, replace int64, skip string, tr *triageRow) error {
	raw, _ := json.Marshal(bp)
	at := s.nowMS()
	return s.write(ctx, actor, func(t *tx) error {
		var latest int64
		if err := t.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM work_log WHERE kind = 'review' AND run_id = ?`, g.run).Scan(&latest); err != nil {
			return err
		}
		if latest != seen {
			return fmt.Errorf("another wp ship changed %s's adversarial review just now; run wp ship again to see where it stands: %w", g.key, ErrConflict)
		}
		if _, err := s.setRound(ctx, t, replace, []string{RoundRunning}, RoundInterrupted, roundReplaced); err != nil {
			return err
		}
		id, err := s.newReviewRound(ctx, t, g.x, g.run, g.issueID, g.lim.reviewPrompt(), at)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE work_log SET before_pr = ? WHERE id = ?`, string(raw), id); err != nil {
			return err
		}
		if tr != nil {
			if err := tr.supersede(ctx, t, seen); err != nil {
				return err
			}
			if err := tr.insert(ctx, t, id); err != nil {
				return err
			}
		}
		if skip != "" {
			_, err = s.setRound(ctx, t, id, []string{RoundPending}, RoundSkipped, skip)
		}
		return err
	})
}

// heldUnshipped says why the step's task, held for its review before the
// pull request, is still unshipped once its round is over ("": the step
// has no such round, the round isn't over, or the task shipped). It says
// what became of the round, so the person isn't told it didn't run when
// it did. release is set when the review never got its chance for a reason
// of Waypoint's own, so Waypoint opens the pull request (shipHeld); it says
// why the review didn't run.
func (s *Service) heldUnshipped(ctx context.Context, st *stepRow) (why, release string, err error) {
	if !st.runID.Valid || !st.reviewID.Valid || roundBeforePR(ctx, s.db.R, st.reviewID.Int64) == nil {
		return "", "", nil
	}
	var ended sql.NullInt64
	var state, reason, key string
	err = s.db.R.QueryRowContext(ctx, `SELECT r.ended_at, w.state, w.reason, p.key || '-' || i.number FROM work_log w JOIN agent_runs r ON r.id = ?
		JOIN issues i ON i.id = r.issue_id JOIN projects p ON p.id = i.project_id WHERE w.id = ?`, st.runID.Int64, st.reviewID.Int64).Scan(&ended, &state, &reason, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil || ended.Valid {
		return "", "", err
	}
	what := ""
	switch state {
	case RoundPending, RoundRunning:
		return "", "", nil
	case RoundDone:
		what = "its adversarial review ran, but the session didn't run wp ship after it"
	case RoundSkipped:
		what = "its adversarial review was skipped"
	case RoundFailed:
		what = "its adversarial review didn't finish"
	case RoundInterrupted:
		what = "its adversarial review was interrupted"
	default:
		what = "its adversarial review is " + state
	}
	reason = strings.TrimSpace(reason)
	if reason != "" && state != RoundDone {
		what += " (" + reason + ")"
	}
	if (state == RoundSkipped || state == RoundFailed) && !strings.HasPrefix(reason, roundCancelledBy) {
		release = cmp(reason, "its adversarial review didn't run")
	}
	return key + " isn't shipped yet: " + what, release, nil
}

// roundAuditLives: the round's audit hasn't failed or been cancelled, so
// once the round has ended without it, it goes to the pull request
// (handOverRoundAudits, WP-1059).
func (s *Service) roundAuditLives(ctx context.Context, roundID int64) bool {
	var ok bool
	err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM work_log w JOIN code_audits a ON a.id = w.audit_id
		WHERE w.id = ? AND a.purpose = ? AND a.status IN ('queued', 'running', 'done'))`, roundID, AuditReview).Scan(&ok)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read round audit lives", "roundID", roundID, "err", err)
		}
		return false
	}
	return ok
}

// reaskHeldRound asks again for a review held before its pull request that
// was taken from the session before it reported (a person's prompt
// mid-review): otherwise nothing runs it, and the pull request waits for a
// review that never comes (WP-823). The new round is the same change's, so
// it takes over the old one's triage. The Stop then makes it due as any
// held round, a minute later, and a person still at the session holds it
// (WP-816).
func (s *Service) reaskHeldRound(ctx context.Context, t *tx, x *AgentSession, at int64) error {
	var id, run, issueID int64
	var state, prompt, bp string
	err := t.QueryRowContext(ctx, `SELECT w.id, w.run_id, w.issue_id, w.state, w.prompt, w.before_pr FROM work_log w JOIN agent_runs r ON r.id = w.run_id
		WHERE w.kind = 'review' AND w.session_id = ? AND r.ended_at IS NULL AND r.session_id = w.session_id
		ORDER BY w.id DESC LIMIT 1`, x.ID).Scan(&id, &run, &issueID, &state, &prompt, &bp)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (state != RoundInterrupted || bp == "")) {
		return nil
	}
	if err != nil {
		return err
	}
	// The session has gone on to another task since: asked for now, the
	// review would take that task's Stop (reviveHeldRounds leaves it too).
	var moved bool
	if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE session_id = ? AND id > ? AND issue_id != ?)`,
		x.ID, run, issueID).Scan(&moved); err != nil || moved {
		return err
	}
	if again, err := askAgainAfterInterrupt(ctx, t, x.ID, issueID); err != nil || !again {
		return err
	}
	next, err := s.newReviewRound(ctx, t, x, run, issueID, prompt, at)
	if err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `UPDATE work_log SET before_pr = ? WHERE id = ?`, bp, next); err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `UPDATE review_triage SET round_id = ? WHERE round_id = ?`, next, id)
	return err
}

// reviewFirstAtStop handles a Stop while the session's task waits for its
// review before the pull request: the round is due in a minute, or, run
// and stopped short of wp ship, it ends with this Stop as its report. It
// says whether it handled the Stop.
func (s *Service) reviewFirstAtStop(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent, seen map[string]int64, at int64) (string, bool, error) {
	if err := s.reaskHeldRound(ctx, t, x, at); err != nil {
		return "", false, err
	}
	var id, run, issueID, since int64
	var state, key string
	err := t.QueryRowContext(ctx, `SELECT w.id, w.run_id, w.issue_id, w.state, pj.key || '-' || i.number, coalesce(w.started_at, w.created_at) FROM work_log w
		JOIN agent_runs r ON r.id = w.run_id JOIN issues i ON i.id = w.issue_id JOIN projects pj ON pj.id = i.project_id
		WHERE w.kind = 'review' AND w.session_id = ? AND w.before_pr != '' AND w.state IN ('pending', 'running') AND r.ended_at IS NULL
		ORDER BY w.id DESC LIMIT 1`, x.ID).Scan(&id, &run, &issueID, &state, &key, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	pid := x.projectID.Int64
	if !x.projectID.Valid {
		if err := t.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE id = ?`, issueID).Scan(&pid); err != nil {
			return "", false, err
		}
	}
	if state == RoundRunning {
		if live, _ := liveBackground(seen, since, at); len(live) > 0 {
			return "", true, nil // its background work wakes it again
		}
		// Its ship waits on the check of its marks, whose end is typed
		// into it (tellAudit): the round goes on then.
		if going, err := s.checkGoing(ctx, t, id, at); err != nil || going {
			return "", true, err
		}
		// It stopped without shipping: what it said is the round's report,
		// and the task is the session's (or its person's) to finish.
		if _, err := s.roundAtStop(ctx, t, x, issueID, ev.LastMessage, at); err != nil {
			return "", true, err
		}
		return "", true, s.stopStep(ctx, t, x.ID, "working", key+" isn't shipped: the session stopped after its adversarial review without wp ship")
	}
	// Autopilot off, at the ship or since, doesn't drop the round: the task
	// is shipped, and the switch is about the next one (heldOwed, WP-1007).
	on, _, err := s.autopilotFor(ctx, t, x.userID, pid)
	if err != nil {
		return "", true, err
	}
	// Background work doesn't hold it: a session at its prompt can be typed
	// into whatever runs behind it, and a ship asked for the review now
	// (WP-1363).
	note := fmt.Sprintf("Autopilot: %s's adversarial review comes next, before its pull request. Type `keep` to keep the session.", key)
	if !on {
		note = fmt.Sprintf("%s's adversarial review comes next, before its pull request. Autopilot is off here, so no task starts after it. Type `keep` to keep the session.", key)
	}
	return note, true, s.putStep(ctx, t, x, pid, run, at, StepDue, "", "", at+reviewStartDelay, id)
}
