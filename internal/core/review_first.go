package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
	// Message is the ship's -m, which wp ship posts on the task once the
	// pull request opens, and Summary its --summary (the reply to the
	// person, with {task} {pr} {url} still unfilled): the newest ship's
	// win, so a pull request Waypoint opens carries what wp ship would
	// have shown (WP-1370).
	Message string `json:"message,omitempty"`
	Summary string `json:"summary,omitempty"`
	// What Waypoint's own ship of this round has tried (shipTried): kept here
	// so a restart neither forgets it nor tells the person twice (WP-1370).
	ShipTries int    `json:"ship_tries,omitempty"`
	ShipFirst int64  `json:"ship_first,omitempty"`
	ShipNext  int64  `json:"ship_next,omitempty"`
	ShipTold  bool   `json:"ship_told,omitempty"`
	ShipErr   string `json:"ship_err,omitempty"`
	// BodyInputs are what a model rewords the body from (WP-55): a pull
	// request Waypoint opens keeps them as the direct open would. They go
	// and come with Body.
	BodyInputs *PRBodyInputs `json:"body_inputs,omitempty"`
	// TokenID is the API token the newest ship came with, so the pull
	// request Waypoint opens later has the authority that token still has
	// (none, if it was revoked): see shipAuthority (WP-1370).
	TokenID int64 `json:"token_id,omitempty"`
	// facts is what the ship's texts need of the agent (not stored).
	facts *shipFacts
}

// shipFacts says what a ship's two texts need of the agent, set by the code
// that writes each text (WP-1370): Held ends the ship with something for the
// agent to do unless its producer says it is only to be waited out
// (idleHeld), and a Notice is only news unless its producer sets noticeAsks
// (none does today: every notice says what was done instead). The CLI ends the turn by hook when nothing is
// asked, and never when something is; a producer that forgets to say gets
// the safe answer for Held (the agent is asked) and the quiet one for a
// Notice, which the table test over every producer pins. Nil-safe.
type shipFacts struct{ heldIdle, noticeAsks bool }

func (f *shipFacts) idleHeld() {
	if f != nil {
		f.heldIdle = true
	}
}

func (f *shipFacts) heldAsks(held string) bool { return held != "" && (f == nil || !f.heldIdle) }

func (f *shipFacts) noticeAsksAgent(notice string) bool {
	return notice != "" && f != nil && f.noticeAsks
}

// refreshBeforePR stores a newer ship's words on the round's before-PR
// record, so the newest ship's win when Waypoint opens the pull request
// (WP-1370). The title and body are replaced when the ship has them (a new
// body takes its body inputs with it); message and summary follow the
// ship's flags: nil is not given and keeps what the round holds, a given
// empty one clears it. The branch, base and flags stay as the round was
// scheduled with them. Best effort: a failure leaves the older words, which
// still open a pull request.
func (s *Service) refreshBeforePR(ctx context.Context, actor *Actor, id int64, fresh beforePR, message, summary *string) {
	old := roundBeforePR(ctx, s.db.R, id)
	if old == nil || old.Branch != fresh.Branch {
		return
	}
	next := *old
	if strings.TrimSpace(fresh.Title) != "" {
		next.Title = fresh.Title
	}
	if strings.TrimSpace(fresh.Body) != "" {
		next.Body, next.BodyInputs = fresh.Body, fresh.BodyInputs
	}
	if fresh.TokenID != 0 {
		next.TokenID = fresh.TokenID
	}
	if message != nil {
		next.Message = *message
	}
	if summary != nil {
		next.Summary = *summary
	}
	if reflect.DeepEqual(next, *old) {
		return
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return
	}
	err = s.write(ctx, actor, func(t *tx) error {
		_, err := t.ExecContext(ctx, `UPDATE work_log SET before_pr = ? WHERE id = ?`, string(raw), id)
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "review: refresh a round's pull request words", "round", id, "err", err)
	}
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
	ShipPR     = "pr"     // the same as ShipAuto, kept for older prompts and wps (WP-1370)
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
	if g.run, err = s.openRunIn(ctx, s.db.R, x.ID, g.issueID); err != nil || g.run != 0 {
		return g, err
	}
	if !take {
		g.no = g.key + " isn't under way in this session (wp next takes it)"
		return g, nil
	}
	if g.no, err = s.retakeForReview(ctx, actor, g); err != nil || g.no != "" {
		return g, err
	}
	if g.run, err = s.openRunIn(ctx, s.db.R, x.ID, g.issueID); err == nil && g.run == 0 {
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
func (s *Service) openRunIn(ctx context.Context, q queryer, session, issueID int64) (int64, error) {
	var run int64
	err := q.QueryRowContext(ctx, `SELECT id FROM agent_runs WHERE session_id = ? AND issue_id = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`,
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
	if intent == ShipPR {
		intent = ShipAuto // --pr is the default, kept for older prompts (WP-1370)
	}
	switch intent {
	case ShipAuto, ShipReview:
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
	var started bool // its prompt went in
	id, state, ended, started, err = s.roundOfShip(ctx, s.db.R, g.x.ID, g.issueID, g.run, head)
	// seen is the run's own newest round, for scheduleRound (0: none; the
	// round found may be an earlier run's the session took up again)
	var seen int64
	if e := s.db.R.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM work_log WHERE kind = 'review' AND run_id = ?`, g.run).Scan(&seen); e != nil {
		return false, "", "", e
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", "", err
	}
	bp := beforePR{Project: p.Key, Repo: repo, Branch: head, Base: base, Title: in.Title, Body: in.Body, BodyInputs: in.BodyInputs, TokenID: actor.TokenID, Message: deref(in.Message), Summary: deref(in.Summary), Draft: in.Draft, Ready: in.Ready, Urgent: in.Urgent && !existing}
	wait := fmt.Sprintf("Pushed %s. Waypoint reviews %s before its pull request and opens the pull request itself when the review leaves nothing for you to do; findings that need you come to this session. Nothing more to run: end your turn.", head, g.key)
	if existing {
		wait = fmt.Sprintf("Pushed %s. Waypoint reviews %s, and its open pull request doesn't merge before the review ends; findings that need you come to this session. Nothing more to run: end your turn.", head, g.key)
	}
	// Every round scheduled below is assessed and recorded (WP-692).
	var assessed *triageRow
	bp.facts = in.facts
	waitHeld := func() string { bp.facts.idleHeld(); return wait }
	assess := func() *triageRow {
		if assessed == nil {
			assessed = s.assessChange(ctx, actor, g, p.ID, in, repo, head, base, intent)
		}
		return assessed
	}
	schedule := func(replace int64, skip string) error {
		return s.scheduleRound(ctx, actor, g, bp, seen, replace, skip, assess())
	}
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
		case RoundPending, RoundRunning:
			// One under way is skipped on the record as well: the agent
			// opens the pull request past it, and until WP-1320's audit
			// nothing said so (the round stayed running, and its triage
			// row said it was reviewed).
			var moved bool
			err := s.write(ctx, actor, func(t *tx) error {
				var err error
				moved, err = s.setRound(ctx, t, id, []string{RoundPending, RoundRunning}, RoundSkipped, why)
				if err != nil || !moved {
					return err // (not moved: it ended just now)
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
		if err == nil && roundDecidable(state, started) && state != RoundRunning {
			// The round ran and ended; what the agent passes over is its
			// scoring. That is on the record too, when nothing decided it.
			s.noteUnscored(ctx, actor, g, id, in.Reason)
		}
		if err == nil && !in.noSweep {
			notice = strings.TrimSpace(notice + " " + s.skipRoundsElsewhere(ctx, actor, g.issueID, id, g.key, in.Reason))
		}
		return err == nil, "", notice, err
	case ShipAuto:
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
		if (state == RoundPending || state == RoundRunning) && id != 0 {
			// The newest ship's title, body and summary are the ones a pull
			// request Waypoint opens for the round carries (WP-1370).
			s.refreshBeforePR(ctx, actor, id, bp, in.Message, in.Summary)
		}
		if roundDecidable(state, started) {
			// This is its ship: new findings that score serious, or fixes
			// that look partial, get another round first, round after
			// round while they keep coming (WP-695, WP-1320). A round that
			// ended before its ship (the session stopped after it and
			// ships now) is decided the same: the decision is the ship's,
			// not the round's state's, and passed over there, stopping
			// once before wp ship opened the pull request on findings
			// nothing had scored. So is one given up after its prompt
			// went in ("no report came back"): this ship is that report,
			// late. One decided already answers at once.
			held, notice, err := s.reviewAgain(ctx, actor, g, bp, id, seen, head)
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
		// (WP-1089): that review is the change's round, whether the hook
		// recorded its reviewer's report or the agent attached one. It is
		// held to what any round is (reviewAgain): the session reports
		// what the review found, its marks are checked against the code,
		// and the findings are scored and may call for another round.
		// Until WP-1320's audit it stood for the round on the session's
		// word and the round was recorded as skipped. Not when the rules
		// skip the change anyway, when the round's audit already has
		// findings for the session (those are the round's: shipAudited),
		// or when the review found nothing and more was committed since.
		own, err := s.ownReviewFor(ctx, g)
		if err != nil {
			return false, "", "", err
		}
		ownStale := own.id != 0 && !own.covers(in.HeadTime)
		if own.id != 0 && !ownStale && (state == RoundPending || assess().decision.Action != triageSkip) {
			ready, err := s.roundAuditReady(ctx, id)
			if err != nil {
				return false, "", "", err
			}
			if !ready {
				var pending int64 // (id may be a round that is over)
				if state == RoundPending {
					pending = id
				}
				round, err := s.ownReviewRound(ctx, actor, g, bp, seen, pending, own, assess())
				if err != nil {
					return false, "", "", err
				}
				held, notice, err := s.reviewAgain(ctx, actor, g, bp, round, round, head)
				if err != nil || held != "" {
					return false, held, notice, err
				}
				return true, "", "", nil
			}
		}
		var auditNotice string
		if state == RoundPending {
			// The session may have read the round's audit itself (WP-919).
			done, n, err := s.shipAudited(ctx, actor, g, id)
			if err != nil {
				return false, "", "", err
			}
			if done {
				// The round ended with this ship, so this ship is the one
				// that decides it: the session's marks are checked and its
				// findings scored as any round's (reviewAgain).
				held, notice, err := s.reviewAgain(ctx, actor, g, bp, id, seen, head)
				if err != nil || held != "" {
					return false, held, notice, err
				}
				return true, "", n, nil
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
			notice = g.key + "'s adversarial review was interrupted before it reported, so it runs again and the pull request waits for it."
		} else if auditNotice != "" {
			notice = auditNotice
		} else if ownStale {
			notice = "The session's own review found nothing and more was committed since, so a review of what it didn't see comes before the pull request."
		}
		return false, waitHeld(), notice, nil
	}
	// ShipReview.
	switch {
	case state == RoundPending:
		return false, waitHeld(), "", nil
	case state == RoundRunning && !in.Force:
		bp.facts.idleHeld()
		return false, fmt.Sprintf("Pushed %s. %s's adversarial review is this one, under way. Nothing more to run: end your turn, and Waypoint opens the pull request itself when it leaves nothing for you to do.", head, g.key),
			"A review of this change is already under way. To start another after this turn anyway: `wp ship --review --force`.", nil
	case state == RoundRunning:
		notice = "The review under way is replaced: another starts after this turn."
		err = schedule(id, "")
	case state == RoundDone && !in.Force && in.HeadTime > 0 && (in.HeadTime+1)*1000 <= ended:
		// (Commit times are whole seconds: one in the round's last second
		// counts as since, so the doubt reviews again.)
		notice = "A review already finished on this change and nothing was committed since, so the pull request opens now. To review it again anyway: `wp ship --review --force`."
		if existing {
			notice = "A review already finished on this change and nothing was committed since; its pull request is open. To review it again anyway: `wp ship --review --force`."
		} else if held, n, err := s.reviewAgain(ctx, actor, g, bp, id, seen, head); err != nil || held != "" {
			// The finished round's findings decide whether another comes,
			// as at any ship that would open the pull request.
			return false, held, n, err
		}
		return true, "", notice, nil
	case state == RoundDone && in.Force:
		notice = "A review already finished on this change; another is scheduled, as asked."
		err = schedule(0, "")
	case state == RoundDone:
		notice = "A review already finished on this change; another is scheduled for what was committed since."
		err = schedule(0, "")
	default: // none yet, or one that never ran or broke off
		err = schedule(0, "")
	}
	if err != nil {
		return false, "", "", err
	}
	return false, waitHeld(), notice, nil
}

// shipAudited is a plain wp ship (or --pr, the same) of a task whose round is
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
		return false, "", nil // the held text says it: Waypoint opens the pull request when the audit leaves nothing for the session
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
		return false, fmt.Sprintf("%s's adversarial review is audit #%d, with %s still to decide:\n%s\nRead them (`wp audit show %d`), mark each (`wp audit mark %s fixed -m \"what you did\"` or `wp audit mark %s rejected -m \"why\"`), then run `wp ship`; or end your turn, and the review gives them to you.",
			g.key, a.ID, plural1(len(todo), "finding"), findingLines(todo), a.ID, todo[0].ID, todo[0].ID), nil
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
	return true, fmt.Sprintf("%s's adversarial review was audit #%d, and nothing in it is left to decide.", g.key, a.ID), nil
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
	if err := s.gateNote(ctx, actor, g, body); err != nil {
		s.log.WarnContext(ctx, "unreviewed pull request not recorded", "task", g.key, "err", err)
	}
	return fmt.Sprintf("%s's pull request opens without its adversarial review first: %s.", g.key, g.no)
}

// noteUnscored records in the task's work log that the agent opened the
// pull request past a finished round nothing had decided (wp ship
// --skip-review at the hold for its findings or their scoring): the round
// reads done, and without the note nothing says its findings were never
// scored. Not when the round was decided: then the skip passed over
// nothing.
func (s *Service) noteUnscored(ctx context.Context, actor *Actor, g *reviewGate, round int64, why string) {
	var decided int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM review_again_decisions WHERE round_id = ?`, round).Scan(&decided); err != nil || decided > 0 {
		return
	}
	body := fmt.Sprintf("wp ship --skip-review opened the pull request before the adversarial review round's findings were scored (work log entry %d), so nothing decided whether another round was due. The agent's reason: %s", round, strings.TrimSpace(why))
	if err := s.gateNote(ctx, actor, g, body); err != nil {
		s.log.WarnContext(ctx, "unscored review round not recorded", "task", g.key, "round", round, "err", err)
	}
}

// gateNote adds a note to the gate's task's work log, once per session
// and text.
func (s *Service) gateNote(ctx context.Context, actor *Actor, g *reviewGate, body string) error {
	return s.write(ctx, actor, func(t *tx) error {
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
}

// roundOfShip is the adversarial review round a ship of the branch decides,
// and a report of the review (reportRoundOf) lands on: the newest of the
// run's own, else the round of the same change from the task's run before
// this one in the session (roundBeforeResume). One locator, so a report
// can't land on a round the ship won't decide (WP-1320). Rounds of
// another session never count. branch "" is the branch of the session's
// newest round before a pull request of the task, for a caller that isn't
// told one (a report); id 0 (and sql.ErrNoRows) when there is none.
func (s *Service) roundOfShip(ctx context.Context, q queryer, session, issueID, run int64, branch string) (id int64, state string, ended int64, started bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT id, state, coalesce(ended_at, 0), started_at IS NOT NULL FROM work_log WHERE kind = 'review' AND run_id = ? ORDER BY id DESC LIMIT 1`, run).Scan(&id, &state, &ended, &started)
	if !errors.Is(err, sql.ErrNoRows) {
		return id, state, ended, started, err
	}
	if branch == "" {
		if err := q.QueryRowContext(ctx, `SELECT coalesce(`+roundBranchSQL("w")+`, '') FROM work_log w WHERE w.kind = 'review' AND w.session_id = ? AND w.issue_id = ? AND w.before_pr != '' ORDER BY w.id DESC LIMIT 1`,
			session, issueID).Scan(&branch); err != nil {
			return 0, "", 0, false, err
		}
	}
	return s.roundBeforeResume(ctx, q, session, issueID, run, branch)
}

// roundBeforeResume is the round of the same change from the task's run
// before this one in the session, when this run only took over from it: the
// session paused or its claim lapsed, and came back. Without it,
// the review the session already did is forgotten and wp ship asks for
// another. Only a run that stopped short of an end of its own counts (no
// run ended done or released in between), only a round of the same branch,
// and not one still pending: that one runs in no run, so a new one does.
func (s *Service) roundBeforeResume(ctx context.Context, q queryer, session, issueID, run int64, branch string) (id int64, state string, ended int64, started bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT w.id, w.state, coalesce(w.ended_at, 0), w.started_at IS NOT NULL FROM work_log w
		WHERE w.kind = 'review' AND w.session_id = ? AND w.issue_id = ? AND w.run_id < ? AND w.state != 'pending'
			AND w.before_pr != '' AND `+roundBranchSQL("w")+` = ?
			AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = w.issue_id AND r.id >= w.run_id AND r.id < ?
				AND (r.ended_at IS NULL OR r.outcome NOT IN ('paused', 'expired', 'timeout')))
		ORDER BY w.id DESC LIMIT 1`, session, issueID, run, branch, run).Scan(&id, &state, &ended, &started)
	return id, state, ended, started, err
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
	s.raced(ctx)
	return s.write(ctx, actor, func(t *tx) error {
		var latest int64
		if err := t.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM work_log WHERE kind = 'review' AND run_id = ?`, g.run).Scan(&latest); err != nil {
			return err
		}
		if latest != seen {
			return fmt.Errorf("another wp ship of %s was handling its adversarial review at the same moment and went first; end your turn, it carries the round on: %w", g.key, ErrConflict)
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
			return err
		}
		// The round's step is made now, due at once: its audit starts while the
		// turn that shipped is still ending, not a minute after the Stop
		// (WP-1370). The Stop only moves it (reviewFirstAtStop), and where the
		// Stop never reports, the watchdog makes it again.
		if g.x != nil {
			if err := s.putStep(ctx, t, g.x, g.pid, g.run, at, StepDue, "", "", at, id); err != nil {
				return err
			}
			s.log.InfoContext(ctx, "review step made at ship", "round", id, "run", g.run, "session", g.x.ID, "due", at)
		}
		return nil
	})
}

// roundAuditReady says whether a pending round's audit ended with
// findings the session hasn't been given yet: they are the round's.
func (s *Service) roundAuditReady(ctx context.Context, round int64) (bool, error) {
	if round == 0 {
		return false, nil
	}
	var ready bool
	err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM work_log w JOIN code_audits a ON a.id = w.audit_id JOIN code_audit_findings f ON f.audit_id = a.id
		WHERE w.id = ? AND w.state = 'pending' AND w.audit_note = '' AND a.status = ?)`, round, AuditDone).Scan(&ready)
	return ready, err
}

// ownReviewNote is the audit note of a round the session's own review
// stands as (ownReviewRound): no other model's findings are the round's.
const ownReviewNote = "the session ran its own adversarial review before the round started"

// ownReviewRound records the session's own review as the change's round
// before its pull request: a round that ran (done), new, or the pending
// one (its step stands down), with the triage that says the session
// reviewed it. An audit the pending round had asked for goes on as the
// pull request's (roundAuditOutlives), as when a round's reviewer ends
// before its audit. The round has no findings until the session reports
// them (ReportReview), and wp ship holds for that. seen is as for
// scheduleRound.
func (s *Service) ownReviewRound(ctx context.Context, actor *Actor, g *reviewGate, bp beforePR, seen, pending int64, own ownReview, tr *triageRow) (int64, error) {
	raw, _ := json.Marshal(bp)
	at := s.nowMS()
	how := "its reviewer's report is in the work log"
	if own.attached {
		how = "the agent attached its report by hand"
	}
	body := fmt.Sprintf("The session ran its own adversarial review of the change (%s: wp worklog %s), which stands as this round. Its findings are the session's to report (wp audit report), and are checked and scored from there.", how, g.key)
	id := pending
	s.raced(ctx)
	err := s.write(ctx, actor, func(t *tx) error {
		var latest int64
		if err := t.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM work_log WHERE kind = 'review' AND run_id = ?`, g.run).Scan(&latest); err != nil {
			return err
		}
		if latest != seen {
			return fmt.Errorf("another wp ship changed %s's adversarial review just now; run wp ship again to see where it stands: %w", g.key, ErrConflict)
		}
		note := ownReviewNote
		if id == 0 {
			var err error
			if id, err = s.newReviewRound(ctx, t, g.x, g.run, g.issueID, g.lim.reviewPrompt(), at); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE work_log SET before_pr = ? WHERE id = ?`, string(raw), id); err != nil {
				return err
			}
		} else {
			var audit sql.NullInt64
			if err := t.QueryRowContext(ctx, `SELECT w.audit_id FROM work_log w JOIN code_audits a ON a.id = w.audit_id
				WHERE w.id = ? AND w.audit_note = '' AND a.status IN (?, ?)`, id, AuditQueued, AuditRunning).Scan(&audit); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if audit.Valid {
				note = fmt.Sprintf("%saudit #%d was still going when the session's own review ended first; the audit goes on as the pull request's", roundAuditOutwaited, audit.Int64)
			}
		}
		res, err := t.ExecContext(ctx, `UPDATE work_log SET state = 'done', body = ?, started_at = coalesce(started_at, ?), ended_at = ?,
			audit_note = CASE WHEN audit_note = '' THEN ? ELSE audit_note END, read_state = ?, read_by = ?, read_at = ?, updated_at = ?
			WHERE id = ? AND kind = 'review' AND state = 'pending'`, body, at, at, note, readDone, readByRules, at, at, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%s's adversarial review started just now; run wp ship again: %w", g.key, ErrConflict)
		}
		s.emitWorkLog(ctx, t, g.issueID, id, RoundDone)
		if err := s.stopRoundStep(ctx, t, g.x.ID, id, "the session ran its own adversarial review"); err != nil {
			return err
		}
		// Reviewed, and by whom: the round's triage says so, unless it
		// already says why the change was reviewed (a holdout, a shadow
		// rule: what the rules' record is built from).
		var assessed bool
		if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM review_triage WHERE round_id = ?)`, id).Scan(&assessed); err != nil {
			return err
		}
		if assessed {
			_, err := t.ExecContext(ctx, `UPDATE review_triage SET why = ? WHERE round_id = ? AND action = ? AND why = ''`, triageWhyOwnReview, id, triageReview)
			return err
		}
		if tr == nil {
			return nil
		}
		row := *tr
		if row.decision.Action != triageReview || row.decision.Why == "" {
			row.decision.Action, row.decision.Why = triageReview, triageWhyOwnReview
		}
		if err := row.supersede(ctx, t, seen); err != nil {
			return err
		}
		return row.insert(ctx, t, id)
	})
	return id, err
}

// heldUnshipped says why the step's task, held for its review before the
// pull request, is still unshipped once its round is over ("": the step
// has no such round, the round isn't over, or the task shipped). It says
// what became of the round, so the person isn't told it didn't run when
// it did. release is set when the review never got its chance for a reason
// of Waypoint's own, so Waypoint opens the pull request (shipRound); it says
// why the review didn't run.
func (s *Service) heldUnshipped(ctx context.Context, st *stepRow) (why string, err error) {
	if !st.runID.Valid || !st.reviewID.Valid || roundBeforePR(ctx, s.db.R, st.reviewID.Int64) == nil {
		return "", nil
	}
	var ended sql.NullInt64
	var state, reason, key string
	var sessionID, issueID int64
	err = s.db.R.QueryRowContext(ctx, `SELECT r.ended_at, w.state, w.reason, p.key || '-' || i.number, coalesce(w.session_id, 0), w.issue_id FROM work_log w JOIN agent_runs r ON r.id = ?
		JOIN issues i ON i.id = r.issue_id JOIN projects p ON p.id = i.project_id WHERE w.id = ?`, st.runID.Int64, st.reviewID.Int64).Scan(&ended, &state, &reason, &key, &sessionID, &issueID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil || ended.Valid {
		return "", err
	}
	what := ""
	switch state {
	case RoundPending, RoundRunning:
		return "", nil
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
	return key + " isn't shipped yet: " + what, nil
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
	if again, err := askAgainAfterInterrupt(ctx, t, x.ID, issueID); err != nil {
		return err
	} else if !again {
		// Interrupted for the last time and the session just stopped: nobody
		// is left to ship it. The step is due at once, and its tick finds the
		// round over and the task unshipped (heldUnshipped), so Waypoint
		// opens the pull request (WP-1370).
		pid := x.projectID.Int64
		if !x.projectID.Valid {
			if err := t.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE id = ?`, issueID).Scan(&pid); err != nil {
				return err
			}
		}
		s.log.InfoContext(ctx, "review round interrupted for the last time: step made to release it", "round", id, "session", x.ID, "interrupts", reviewInterruptsMax)
		return s.putStep(ctx, t, x, pid, run, at, StepDue, "", "", at, id)
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
		live, _ := liveBackground(seen, since, at)
		going, err := s.checkGoing(ctx, t, id, at)
		if err != nil {
			return "", true, err
		}
		in := roundIn{State: state, Stopped: true, Subagents: len(live) > 0}
		if going {
			in.Check = checkRunning
		}
		// Its background work wakes it again; its ship waits on the check of
		// its marks, whose end is typed into it (tellAudit): the round goes
		// on then. Otherwise what it said is the round's report.
		if decideRound(in).Do != doReport {
			return "", true, nil
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
	// The step made at the ship is moved on: its audit may be under way
	// already, and a minute from this Stop is when the review is typed.
	var step int64
	var reason string
	err = t.QueryRowContext(ctx, `SELECT id, reason FROM autopilot_steps WHERE session_id = ? AND review_id = ? AND state IN (?, ?)`, x.ID, id, StepDue, StepWaiting).Scan(&step, &reason)
	if err == nil {
		if reason == WaitAudit {
			// the audit's own wait stands
			_, err = t.ExecContext(ctx, `UPDATE autopilot_steps SET stop_at = ?, updated_at = ? WHERE id = ?`, at, at, step)
		} else {
			_, err = t.ExecContext(ctx, `UPDATE autopilot_steps SET stop_at = ?, due_at = ?, updated_at = ? WHERE id = ?`, at, at+reviewStartDelay, at, step)
		}
		s.log.InfoContext(ctx, "review step moved by the Stop", "round", id, "step", step, "reason", reason, "due", at+reviewStartDelay)
		return note, true, err
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", true, err
	}
	s.log.InfoContext(ctx, "review step made by the Stop", "round", id, "run", run, "due", at+reviewStartDelay)
	return note, true, s.putStep(ctx, t, x, pid, run, at, StepDue, "", "", at+reviewStartDelay, id)
}
