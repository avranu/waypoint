package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// decideRound is the one table for what a review round needs, from what is
// recorded of it (WP-1370). Asked by: advanceRound (the step, the sweep, the
// settled check and the agent's ship all carry a round out through it),
// shipSettledRounds (the sweep), decideIdle (the step's idle session), the Stop (reviewFirstAtStop and
// roundAtStop, which ask whether the session's last message is the report),
// sweepRounds (which rounds end), tellCheck (whether a session is waited on)
// and, with ShipAsks, the session's own ship (roundDecidable). What does not
// go through it: the ship's intent (skip, review, force) and the scheduling
// of a new round (reviewFirst), a step's own bookkeeping about a round
// (typing its prompt, a runner that is gone, the pace) and the ending of a
// step's round when the step stops (endStepRound). Those act on a round's
// state as mechanics and decide nothing about its fate.

// Checks, as the decision sees them.
const (
	checkAbsent    = ""
	checkQueued    = "queued"
	checkRunning   = "running"
	checkAgreed    = "agreed"
	checkDisagreed = "disagreed"
	checkDidnt     = "didn't run" // failed, hit a limit, or was cancelled
)

// roundIn is every fact the decision rests on.
type roundIn struct {
	State      string // RoundPending ... RoundFailed
	Read       string // readPending, readDone, readFailed
	Audit      string // "" none, or an audit status
	Undecided  int    // findings of the round's audits nobody decided
	Decided    int
	Check      string // check*
	CheckMarks int    // marks the check disagrees with
	Session    bool   // a live session could take what Waypoint tells it
	RunOpen    bool
	PROpen     bool
	// Superseded: the round's decision scheduled another review round, which
	// is what the branch waits on now; the round is not shipped again.
	Superseded bool
	// Unshipped: the round is before a pull request, the branch is recorded as
	// pushed and no pull request is open for it.
	Unshipped bool
	// Carried: a step carries the round, or the branch is before its pull
	// request and the round's session is live in the open run: someone is on it.
	Carried bool
	// SessionEnded: the round's session has ended.
	SessionEnded bool
	// ShipAsks: the session's own `wp ship` is asking what it means for the
	// round (reviewFirst, the report). Started: the round's prompt went in.
	ShipAsks, Started bool
	// ShipAudited: the session's plain `wp ship` reaches a round still
	// pending whose audit it may have read itself (WP-919): the facts are
	// State, Audit (its status), ReadNothing (the audit read nothing, so the
	// round's own prompt reviews) and Undecided (findings left to decide).
	ShipAudited, ReadNothing bool
	// Stopped, Subagents: the round's session just stopped (its turn ended),
	// with background agents of the review still going.
	Stopped, Subagents bool
	// Release: why a round that didn't run to its end is released (Waypoint
	// opens the pull request without it); "" when a person's decision stands
	// or the session is asked again.
	Release string
	// What leaves a round that is before a pull request alone once its run
	// has ended (WP-1325): the task is resolved
	// or has its pull request linked, the pull request that existed when the
	// round was scheduled was merged or closed since (PRGone: the person's
	// decision, so no other is opened), a newer round of the task (or its
	// decision's next round) carries the branch, or another session holds
	// the task now. SinceEnd is how long ago the latest of the round's end,
	// its run's, its session's and the session's move to another task was.
	Resolved, Linked, PRGone, Newer, Taken bool
	// Halted: Waypoint stopped shipping the round for good and told a person
	// (beforePR.ShipStop).
	Halted   bool
	SinceEnd time.Duration
	// ShipSpent: Waypoint's own ship of this ended round has failed for
	// shipGiveUp, counted from its first try, and nothing stopped it yet.
	ShipSpent bool
	// Idle: the session went idle (autopilot's step saw it) and the round's Stop never came.
	Idle bool
	// What a review of the round is, read once by roundInOf (WP-1370): Audit
	// is the status of the audit whose findings are the round's (the session's
	// own report of its review, else the reviewer's: AuditID, OwnID), ReadNothing
	// says it read nothing, HandedOver that the round's slow audit went on as the
	// pull request's and holds the merge (stakesNoClaude), Decisions how many
	// review-again decisions the round has. A pull request opens only on a
	// review that read something, or one handed over (reviewed).
	Own, HandedOver bool
	AuditID, OwnID  int64
	Decisions       int
	// ShipOpens asks the one question reviewAgain used to answer from its own
	// reads: a ship (the session's, or the server's) is about to open the
	// round's pull request, so is the round reviewed?
	ShipOpens bool
	// Ages: since the round started, and since the thing it waits on last changed
	// (the check settled, the round ended, the audit ended).
	SinceStart, SinceChange time.Duration
}

// outcome kinds.
const (
	doNothing     = iota // only when the run is closed
	doWait               // for something that will change, by a time
	doTell               // the session, who has until a time
	doShip               // reviewed: Waypoint opens the pull request
	doShipCarried        // opened with unverified marks carried to its gate
	doRelease            // opened without the review, with what it found carried
	doFinish             // the pull request is open: end the round and the run
	doEnd                // the round ends with a state of its own (To), Waypoint ships nothing
	doReport             // the session's stop is the round's report
	doHold               // a person's decision (or a new ask) stands: Waypoint ships nothing, the step stops and says why
	// doReleaseAfterRunEnded: the run ended (or its session did) with the round
	// done, failed, skipped or interrupted for Waypoint's own reason, and the
	// branch has no pull request. heldUnshipped, reviveHeldRounds,
	// reaskHeldRound and reviewFirstAtStop all need an open run, so nothing
	// else would ever pick it up: advanceRound opens it (releaseEnded), inside
	// the window and with the back-off the round's ship tries keep.
	doReleaseAfterRunEnded
	// doSettle: the pull request that existed when the round was scheduled was
	// merged or closed while the run is open: nothing is opened, the round
	// ends and the run with it.
	doSettle
)

type roundOut struct {
	Do    int
	For   string        // what a wait or a tell is for
	Until time.Duration // how much longer a wait or a tell may last
	Why   string        // why a release, an end or a carried ship
	To    string        // the state a doEnd ends the round in
	// Settled says why a doNothing leaves a round alone for good: the only
	// answer that names no one to act later. Every doNothing has one.
	Settled string
	// Need says what a tell asks of the session (needReport, needMarks), and
	// Told, on a doEnd, that the end is told to the person with these words:
	// the round had no review, so nothing was opened.
	Need, Told string
}

// What a tell asks of the session.
const (
	needReport = "report" // its review isn't recorded: wp audit report
	needMarks  = "marks"  // findings of the review are still to decide
)

// reviewed says the round has a review a pull request may open on (WP-1370):
// one that read something (the reviewer's audit done and not read-nothing, or
// the session's own report of it), or one handed over to the pull request
// (stakesNoClaude), whose audit holds the merge, or one that a decision was recorded on
// (it passed this very question). An audit that failed, was
// cancelled, ran over its limit or read nothing is no review.
func (in roundIn) reviewed() bool {
	return in.Own || in.HandedOver || in.Decisions > 0 || (in.Audit == AuditDone && !in.ReadNothing)
}

// noReviewAnswer answers for a round with no review (ok) and says nothing (not
// ok) for one that has one. A review still going is waited on, to a limit; one
// that is none is the session's to report, to a deadline; and a round whose
// session is gone, or whose deadline passed, ends and tells the person once.
// The server never opens a pull request for it.
func noReviewAnswer(in roundIn) (roundOut, bool) {
	if in.reviewed() {
		return roundOut{}, false
	}
	noReview := func(why string) roundOut {
		return roundOut{Do: doEnd, To: RoundFailed, Why: why, Told: "No review of this round was recorded (" + why + "), so Waypoint opened nothing. Run `wp ship` again to start a new review round."}
	}
	if in.Audit == AuditQueued || in.Audit == AuditRunning {
		if in.SinceStart >= reviewRunMax+autopilotNothingRetry {
			return noReview("its review never finished"), true
		}
		return roundOut{Do: doWait, For: "the review's audit", Until: max(reviewRunMax+autopilotNothingRetry-in.SinceStart, 0)}, true
	}
	if in.Session && in.SinceChange < heldScreenWait {
		return roundOut{Do: doTell, Need: needReport, For: "the session's report of its review", Until: max(heldScreenWait-in.SinceChange, 0)}, true
	}
	why := "the session had no report of it by the deadline"
	if !in.Session {
		why = "the session that was to report it is gone"
	}
	return noReview(why), true
}

// decideOpens is decideRound's answer when a ship is about to open the
// round's pull request (ShipOpens): reviewAgain asks it, and the Done and ended
// cases of decideRound ask the same gate. It decides whether this round may
// open; whether another review round comes first is reviewAgain's policy
// (the check of marks, the score, the next round), asked of a round that is
// doShip here.
func decideOpens(in roundIn) roundOut {
	if out, ok := noReviewAnswer(in); ok {
		return out
	}
	if in.Undecided > 0 {
		return undecidedAnswer(in, needMarks)
	}
	return roundOut{Do: doShip}
}

// undecidedAnswer is the one answer for a round whose findings are not all
// decided, so a ship's question and the sweep agree on the same facts: while
// a session is there and the wait isn't over, the session is told to decide
// them; past the wait, or with no session, the round is released with its
// findings carried. need is what the tell asks (needMarks for a ship).
func undecidedAnswer(in roundIn, need string) roundOut {
	if in.Session && in.SinceChange < heldScreenWait {
		return roundOut{Do: doTell, Need: need, For: "the session to decide what the review found", Until: max(heldScreenWait-in.SinceChange, 0)}
	}
	return roundOut{Do: doRelease, Why: fmt.Sprintf("%s the review found weren't decided in %d hours", plural1(in.Undecided, "thing"), int(heldScreenWait.Hours()))}
}

// strandedShipWindow is how long after a round ended, or after its run or
// session ended or moved on if that was later, Waypoint keeps trying to open
// the pull request it left unopened.
const strandedShipWindow = 24 * time.Hour

// endOf: a round nothing will carry on, which ends in a state of its own
// (sweepRounds carries it out). One whose check Waypoint is on, in an
// open run, is Waypoint's: it ships or releases, it isn't ended here.
func endOf(in roundIn) (roundOut, bool) {
	switch {
	case in.State == RoundRunning && in.RunOpen && in.Check != checkAbsent:
		return roundOut{}, false
	case in.Unshipped && !in.RunOpen && in.State == RoundPending:
		return roundOut{Do: doEnd, To: RoundSkipped, Why: "the run ended before the review started"}, true
	case in.Unshipped && !in.RunOpen && in.State == RoundRunning:
		return roundOut{Do: doEnd, To: RoundInterrupted, Why: "the run ended during the review"}, true
	case in.State == RoundPending && !in.Carried:
		return roundOut{Do: doEnd, To: RoundSkipped, Why: "autopilot moved on without it"}, true
	case in.State == RoundRunning && in.Idle:
		return roundOut{Do: doEnd, To: RoundFailed, Why: "no report came back: the session went idle without one"}, true
	case in.State == RoundRunning && in.SessionEnded:
		return roundOut{Do: doEnd, To: RoundInterrupted, Why: "the session ended during the review"}, true
	case in.State == RoundRunning && in.SinceStart > reviewRunMax+autopilotNothingRetry && (!in.Unshipped || !in.RunOpen):
		return roundOut{Do: doEnd, To: RoundFailed, Why: fmt.Sprintf("no report came back within %d hours", int(reviewRunMax.Hours()))}, true
	}
	return roundOut{}, false
}

// readWait is how long a round's report may stay unread before Waypoint
// stops waiting on it.
const readWait = heldScreenWait

// decideRound is the one decision of whether a round's pull request opens,
// and the one reader of a round's review (roundInOf). It never returns
// doNothing for an open run whose pull request isn't open, and no wait or tell
// outlasts its limit. A round with no recorded review never answers a ship
// (R1, WP-1370). The boundary with reviewAgain: decideRound decides whether
// the round may open (and, for the session's ship, ShipOpens, what holds it);
// reviewAgain keeps the review-again policy past that gate (the check of
// the marks, the score, scheduling round N+1) and renders decideRound's holds.
func decideRound(in roundIn) roundOut {
	if in.ShipOpens {
		return decideOpens(in)
	}
	if in.ShipAsks {
		// The session's ship answers a round under way (the ship is its
		// report), one that ended (at a Stop, or by the session reading its
		// audit) and one given up after its prompt went in ("no report came
		// back"): the session was asked, and a ship from it is the answer,
		// late. A round that never started reviewed nothing.
		if in.State == RoundRunning || in.State == RoundDone || (in.State == RoundFailed && in.Started) {
			return roundOut{Do: doReport}
		}
		return roundOut{Do: doNothing, Settled: "a ship from the session is no report of a round that isn't under way or never started"}
	}
	if in.ShipAudited {
		// The ship is the round's report only when the round hasn't started
		// and its audit is done, read something, and leaves nothing to decide.
		switch {
		case in.State != RoundPending:
			return roundOut{Do: doNothing, Settled: "only a round still pending is ended by the session's reading of its audit"}
		case in.Audit != AuditDone:
			return roundOut{Do: doNothing, Settled: "its audit is none, under way or didn't finish: the round's own prompt or its step decides"}
		case in.ReadNothing:
			return roundOut{Do: doNothing, Settled: "its audit read nothing, so the round's own prompt reviews"}
		case in.Undecided > 0:
			return roundOut{Do: doTell, For: "the session to decide what the audit found", Until: heldScreenWait}
		}
		return roundOut{Do: doReport}
	}
	if in.Stopped && in.State == RoundRunning {
		// The session stopped: its last message is the report, unless the
		// review's own background agents or its check still go (they wake it).
		switch {
		case in.Subagents:
			return roundOut{Do: doWait, For: "the review's background agents", Until: readWait}
		case in.Check == checkQueued || in.Check == checkRunning:
			return roundOut{Do: doWait, For: "the check of the marks", Until: checkPatience}
		}
		return roundOut{Do: doReport}
	}
	if in.RunOpen && in.Unshipped && in.PRGone && !in.PROpen {
		return roundOut{Do: doSettle, Why: "its pull request was merged or closed while the review was under way"}
	}
	if !in.RunOpen {
		if out, ok := endOf(in); ok {
			return out
		}
		return decideEnded(in)
	}
	if in.PROpen {
		return roundOut{Do: doFinish}
	}
	if out, ok := endOf(in); ok {
		return out
	}
	left := func(limit time.Duration, since time.Duration) time.Duration { return max(limit-since, 0) }
	if in.Superseded {
		if in.SinceChange >= heldScreenWait {
			// A wait with no time left that nothing acts on would hold the
			// round for ever: it is shipped, with what it found carried.
			return roundOut{Do: doShipCarried, Why: "the review round its decision scheduled never settled"}
		}
		return roundOut{Do: doWait, For: "the review round its decision scheduled", Until: left(heldScreenWait, in.SinceChange)}
	}
	switch in.Check {
	case checkQueued, checkRunning:
		// expireChecks cancels a check at checkPatience; this bounds a check
		// that stays stuck anyway. The round's age plays no part: a check
		// still going is never "no report came back".
		if in.SinceChange >= checkPatience+heldScreenWait {
			return roundOut{Do: doShipCarried, Why: "the check of its marks never settled"}
		}
		return roundOut{Do: doWait, For: "the check of the marks", Until: left(checkPatience+heldScreenWait, in.SinceChange)}
	case checkDisagreed:
		if in.Session && in.SinceChange < heldScreenWait {
			return roundOut{Do: doTell, For: "the session to decide the marks the check disagrees with", Until: left(heldScreenWait, in.SinceChange)}
		}
		if !in.Session {
			return roundOut{Do: doRelease, Why: fmt.Sprintf("the check of its marks disagreed with %s, and no session was left to take it up", plural1(in.CheckMarks, "mark"))}
		}
		return roundOut{Do: doRelease, Why: fmt.Sprintf("the check of its marks disagreed with %s, and the session didn't take it up in %d hours", plural1(in.CheckMarks, "mark"), int(heldScreenWait.Hours()))}
	}
	switch in.State {
	case RoundRunning:
		if in.Check == checkAgreed {
			return roundOut{Do: doShip}
		}
		if in.Check == checkDidnt {
			return roundOut{Do: doShipCarried, Why: "the check of its marks didn't run"}
		}
		// No check: the session's own ship is in flight.
		if in.SinceStart >= reviewRunMax+autopilotNothingRetry {
			return roundOut{Do: doRelease, Why: "no report came back"}
		}
		return roundOut{Do: doWait, For: "the review's report", Until: left(reviewRunMax+autopilotNothingRetry, in.SinceStart)}
	case RoundPending:
		if in.SinceChange >= heldScreenWait {
			return roundOut{Do: doRelease, Why: "the review didn't finish"}
		}
		return roundOut{Do: doWait, For: "the review", Until: left(heldScreenWait, in.SinceChange)}
	case RoundDone:
		if out, ok := noReviewAnswer(in); ok && in.Unshipped {
			return out
		}
		switch {
		case in.Read == readFailed:
			return roundOut{Do: doRelease, Why: "Waypoint couldn't read the review's report"}
		case in.Read == readPending:
			if in.SinceChange >= readWait {
				return roundOut{Do: doRelease, Why: "Waypoint couldn't read the review's report in time"}
			}
			return roundOut{Do: doWait, For: "Waypoint to read the review's report", Until: left(readWait, in.SinceChange)}
		case in.Undecided > 0:
			return undecidedAnswer(in, "")
		case in.Check == checkDidnt:
			return roundOut{Do: doShipCarried, Why: "the check of its marks didn't run"}
		}
		return roundOut{Do: doShip}
	}
	// Skipped, failed, interrupted: the review didn't run to the end.
	if in.Release == "" {
		return roundOut{Do: doHold, Why: "the review " + in.State}
	}
	return roundOut{Do: doRelease, Why: in.Release}
}

// decideEnded: a round whose run has ended and that no end of its own
// applies to. Waypoint opens the pull request it left, unless something
// leaves it alone for good (Settled says what).
func decideEnded(in roundIn) roundOut {
	left := func(why string) roundOut { return roundOut{Do: doNothing, Settled: why} }
	switch {
	case !in.Unshipped:
		return left("the round isn't before a pull request")
	case in.Halted:
		return left("Waypoint stopped shipping it and told a person")
	case in.PROpen:
		return left("its pull request is open")
	case in.PRGone:
		return left("its pull request was merged or closed: a person's decision")
	case in.Linked:
		return left("the task has its pull request")
	case in.Resolved:
		return left("the task is resolved")
	case in.Superseded:
		return left("a later round carries the branch")
	case in.Newer:
		return left("a newer round carries the task")
	case in.Taken:
		return left(takenWhy)
	case in.ShipSpent:
		return roundOut{Do: doEnd, To: RoundFailed, Why: "Waypoint gave up opening its pull request"}
	case in.SinceEnd > strandedShipWindow:
		return left("it ended longer ago than Waypoint keeps trying")
	}
	switch in.State {
	case RoundDone:
		if out, ok := noReviewAnswer(in); ok {
			return out
		}
		return roundOut{Do: doReleaseAfterRunEnded, Why: "its adversarial review finished and the run ended before its pull request opened"}
	case RoundFailed, RoundSkipped, RoundInterrupted:
		if in.Release == "" {
			return left("a person's decision, or a new ask for the review, stands")
		}
		return roundOut{Do: doReleaseAfterRunEnded, Why: in.Release + "; the run ended before its pull request opened"}
	}
	return left("the round ends in its own state first")
}

// roundInOf reads the facts decideRound needs.
func (s *Service) roundInOf(ctx context.Context, id int64) (roundIn, error) {
	var in roundIn
	r, err := roundByID(ctx, s.db.R, id)
	if err != nil || r == nil {
		return in, errOr(err, ErrNotFound)
	}
	now := s.nowMS()
	in.State = r.state
	in.Started = r.startedAt.Valid
	started := nonzero(r.startedAt.Int64, r.createdAt)
	in.SinceStart = time.Duration(max(now-started, 0)) * time.Millisecond
	in.SinceChange = time.Duration(max(now-nonzero(r.ended.Int64, started), 0)) * time.Millisecond
	in.Read = r.readState
	if in.RunOpen, err = roundRunOpen(ctx, s.db.R, id); err != nil {
		return in, err
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM review_again_decisions WHERE round_id = ? AND coalesce(next_round_id, 0) != 0)`, id).Scan(&in.Superseded); err != nil {
		return in, err
	}
	if bp := roundBeforePR(ctx, s.db.R, id); bp != nil {
		in.Unshipped = true
		in.Halted = bp.ShipStop != ""
		if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pull_requests WHERE repo = ? COLLATE NOCASE AND head_ref = ? AND state IN ('open', 'draft') AND (head_repo = '' OR head_repo = repo COLLATE NOCASE))`, bp.Repo, bp.Branch).Scan(&in.PROpen); err != nil {
			return in, err
		}
		// A pull request that existed when the round was scheduled and was
		// merged or closed since is a person's to have decided; one closed
		// before the round is an earlier ship's, and doesn't stop this one.
		if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pull_requests WHERE repo = ? COLLATE NOCASE AND head_ref = ? AND state IN ('merged', 'closed')
			AND created_at <= ? AND updated_at >= ?)`, bp.Repo, bp.Branch, r.createdAt, r.createdAt).Scan(&in.PRGone); err != nil {
			return in, err
		}
		if err := s.endedFacts(ctx, r, &in); err != nil {
			return in, err
		}
	}
	if r.sessionID.Valid && r.sessionID.Int64 != 0 {
		var live bool
		if s.db.R.QueryRowContext(ctx, `SELECT ended_at IS NULL FROM agent_sessions WHERE id = ?`, r.sessionID.Int64).Scan(&live) == nil {
			in.Session, in.SessionEnded = live, !live
		}
	}
	if in.State == RoundSkipped || in.State == RoundFailed || in.State == RoundInterrupted {
		if in.Release, err = s.releaseWhy(ctx, r, !in.RunOpen); err != nil {
			return in, err
		}
	}
	var stepLive bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM autopilot_steps WHERE review_id = ? AND state IN ('due', 'waiting', 'starting'))`, id).Scan(&stepLive); err != nil {
		return in, err
	}
	in.Carried = stepLive || (in.Unshipped && in.Session && in.RunOpen)
	var cid, checkSession int64
	switch err := s.db.R.QueryRowContext(ctx, `SELECT id FROM code_audits WHERE request_key = ?`, fmt.Sprintf(checkAuditKey, id)).Scan(&cid); {
	case err == nil:
		a, err := s.auditRow(ctx, cid)
		if err != nil {
			return in, err
		}
		checkSession = a.sessionID
		switch todo, err := s.checkTodo(ctx, a); {
		case err != nil:
			return in, err
		case a.Status == AuditQueued:
			in.Check = checkQueued
		case a.Status == AuditRunning:
			in.Check = checkRunning
		case a.Status != AuditDone:
			in.Check = checkDidnt
		case len(todo) > 0:
			in.Check, in.CheckMarks = checkDisagreed, len(todo)
		default:
			in.Check = checkAgreed
		}
		var asked, settled int64
		_ = s.db.R.QueryRowContext(ctx, `SELECT created_at, coalesce(settled_at, 0) FROM code_audits WHERE id = ?`, cid).Scan(&asked, &settled)
		since := settled
		if in.Check == checkQueued || in.Check == checkRunning || settled == 0 {
			since = asked
		}
		if since > 0 {
			in.SinceChange = time.Duration(max(now-since, 0)) * time.Millisecond
		}
	case !errors.Is(err, sql.ErrNoRows):
		return in, err
	}
	if cid != 0 && checkSession == 0 {
		in.Session = false // the check has no session to tell
	} else if cid != 0 && checkSession != r.sessionID.Int64 {
		var live bool
		in.Session = s.db.R.QueryRowContext(ctx, `SELECT ended_at IS NULL FROM agent_sessions WHERE id = ?`, checkSession).Scan(&live) == nil && live
	}
	if err := s.reviewFacts(ctx, id, &in); err != nil {
		return in, err
	}
	return in, nil
}

// reviewFacts reads, once and for every decider, what a review of the round
// is (roundIn: Audit, ReadNothing, Own, HandedOver, AuditID, OwnID,
// Decisions, Undecided, Decided): the audit whose findings are the round's is
// the session's own report of the review it ran, else the reviewer's
// (againAuditSQL); a reviewer still going is the round's until it ends.
func (s *Service) reviewFacts(ctx context.Context, id int64, in *roundIn) error {
	var own, chosen int64
	var note string
	if err := s.db.R.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM review_again_decisions WHERE round_id = w.id), w.audit_note,
			coalesce(`+againOwnSQL+`, 0), coalesce(`+againReviewerSQL+`, 0) FROM work_log w WHERE w.id = ?`, id).Scan(&in.Decisions, &note, &own, &chosen); err != nil {
		return err
	}
	in.OwnID, in.Own = own, own != 0
	in.AuditID = own
	if in.AuditID == 0 {
		in.AuditID = chosen
	}
	if in.AuditID == 0 {
		// The reviewer that replaced a given-up audit, still going: its
		// findings are the round's when it ends.
		if rv, err := s.roundReviewer(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		} else if rv != nil && (rv.Status == AuditQueued || rv.Status == AuditRunning) {
			in.AuditID = rv.ID
		}
	}
	in.HandedOver = in.AuditID == 0 && strings.Contains(note, stakesNoClaude)
	// The findings that are the round's are those of the audit it chose, as
	// reviewAgain has always counted them: no other audit's decide it.
	ids := []int64{in.AuditID}
	seen := map[int64]bool{}
	for _, aid := range ids {
		if aid == 0 || seen[aid] {
			continue
		}
		seen[aid] = true
		a, err := s.auditRow(ctx, aid)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if aid == in.AuditID {
			in.Audit = a.Status
		}
		if a.Status != AuditDone {
			continue
		}
		if err := s.loadFindings(ctx, a); err != nil {
			return err
		}
		if a.readNothing() {
			if aid == in.AuditID {
				in.ReadNothing = true
			}
			continue
		}
		todo := len(s.roundTodo(ctx, a))
		in.Undecided += todo
		in.Decided += len(a.Findings) - todo
	}
	return nil
}

// roundRunOpen: the round's own run is open, or its session holds the task
// in another open run. A session that took the task again after its claim
// expired carries the round in that run, as if the first had never ended
// (WP-1828): the round waits on its marks and ships by the session's ship,
// and is not released behind a session still working.
func roundRunOpen(ctx context.Context, q queryer, round int64) (open bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM work_log w JOIN agent_runs o ON o.ended_at IS NULL
		AND (o.id = w.run_id OR (o.issue_id = w.issue_id AND o.session_id = w.session_id))
		WHERE w.id = ? AND w.run_id IS NOT NULL)`, round).Scan(&open)
	return open, err
}

// takenRun is the open run o that makes round w Taken: one on its task that
// is neither the round's own run nor a run of the round's session
// (roundRunOpen has those). A run with no session is somebody else's work,
// and a round with no session compares runs alone.
const takenRun = `o.issue_id = w.issue_id AND o.ended_at IS NULL AND o.id != coalesce(w.run_id, 0)
	AND (w.session_id IS NULL OR o.session_id IS NOT w.session_id)`

// laterRunEnd is when the last later run on round w's task ended: a session
// that held the task after the round and left without a ship. The round's
// window counts from there too, so a hold that outlasts the window doesn't
// drop the round unseen (shipSettledRounds filters by the same time).
const laterRunEnd = `coalesce((SELECT max(o.ended_at) FROM agent_runs o WHERE o.issue_id = w.issue_id AND o.id > w.run_id), 0)`

// takenWhy is decideEnded's reason for a Taken round.
const takenWhy = "another session holds the task"

// takenText is what a ship is told of a Taken round: who holds the task, and
// what the caller can do about it.
func (s *Service) takenText(ctx context.Context, r *roundRow) string {
	var cwd, machine, agent, key string
	var seen int64
	var sess sql.NullInt64
	err := s.db.R.QueryRowContext(ctx, `SELECT o.session_id, o.agent, coalesce(x.cwd, ''), coalesce(x.machine, ''), coalesce(x.last_seen_at, 0), p.key || '-' || i.number
		FROM work_log w JOIN issues i ON i.id = w.issue_id JOIN projects p ON p.id = i.project_id
		JOIN agent_runs o ON `+takenRun+` LEFT JOIN agent_sessions x ON x.id = o.session_id
		WHERE w.id = ? ORDER BY o.id DESC LIMIT 1`, r.id).Scan(&sess, &agent, &cwd, &machine, &seen, &key)
	who := takenWhy
	switch {
	case err != nil:
		// The holder left between the decision and this read: the next ask says what Waypoint does.
		return fmt.Sprintf("Waypoint's review round for this task is over (%s): %s.", r.state, takenWhy)
	case !sess.Valid:
		who = "a run of " + agent + " with no session holds it"
	default:
		who = sessionHoldsText("another session", cwd, machine, seen, s.nowMS())
	}
	return fmt.Sprintf("Waypoint's review round for this task is over (%s): %s, so Waypoint opens no pull request for this round. That session's own `wp ship %s` ships the task. To ship it from this session instead, run `wp ship %s --review`: it starts a new review round.",
		r.state, who, key, key)
}

// endedFacts reads what decides a round with no end of its own once its run
// has ended (roundIn: Resolved ... SinceEnd), and whether its session moved
// on to another task. roundInOf reads it for every round before a pull
// request, whether its run is open or not.
func (s *Service) endedFacts(ctx context.Context, r *roundRow, in *roundIn) error {
	now := s.nowMS()
	var updated, runEnd, sessEnd, moved, heldEnd int64
	// moved: the session's first run on another task after the run that
	// carries the round (its newest open one on this task, else the round's).
	err := s.db.R.QueryRowContext(ctx, `SELECT w.updated_at, coalesce((SELECT ended_at FROM agent_runs WHERE id = w.run_id), 0),
			coalesce((SELECT ended_at FROM agent_sessions WHERE id = w.session_id), 0),
			coalesce((SELECT min(o.started_at) FROM agent_runs o WHERE o.session_id = w.session_id AND o.issue_id != w.issue_id AND o.id > coalesce(
				(SELECT max(h.id) FROM agent_runs h WHERE h.issue_id = w.issue_id AND h.session_id = w.session_id AND h.ended_at IS NULL), w.run_id)), 0),
			`+laterRunEnd+`,
			i.resolved_at IS NOT NULL,
			EXISTS (SELECT 1 FROM external_links l WHERE l.issue_id = w.issue_id AND l.kind = 'pull'),
			w.id != (SELECT max(m.id) FROM work_log m WHERE m.kind = 'review' AND m.issue_id = w.issue_id),
			EXISTS (SELECT 1 FROM agent_runs o WHERE `+takenRun+`)
		FROM work_log w JOIN issues i ON i.id = w.issue_id WHERE w.id = ?`, r.id).Scan(&updated, &runEnd, &sessEnd, &moved, &heldEnd, &in.Resolved, &in.Linked, &in.Newer, &in.Taken)
	if err != nil {
		return err
	}
	in.SinceEnd = time.Duration(max(now-max(updated, runEnd, sessEnd, moved, heldEnd), 0)) * time.Millisecond
	// A ship that has been failing counts its window from its first try:
	// the end of the run says nothing of how long Waypoint has kept at it.
	if bp := roundBeforePR(ctx, s.db.R, r.id); bp != nil && bp.ShipFirst != 0 && bp.ShipStop == "" {
		in.SinceEnd = time.Duration(max(now-bp.ShipFirst, 0)) * time.Millisecond
		in.ShipSpent = in.SinceEnd >= shipGiveUp // however the last try left the record: a ship ever tried is ended, and told, never dropped
	}
	// A session that went on to another task has left the round: its run is
	// over for it. A round still pending or running is the session's own
	// Stop's for heldStopLost first; one that has an end state has nothing
	// left to wait for.
	if moved > 0 && in.RunOpen {
		ended := in.State == RoundDone || in.State == RoundSkipped || in.State == RoundFailed || in.State == RoundInterrupted
		if ended || time.Duration(max(now-moved, 0))*time.Millisecond > heldStopLost {
			in.RunOpen = false
		}
	}
	return nil
}

// releaseEnded is the release of a round whose run ended
// (doReleaseAfterRunEnded): the pull request opens without the review, or
// with what it found carried to its gate, as it would have in an open run.
func (s *Service) releaseEnded(ctx context.Context, id int64, out roundOut) error {
	return s.shipRound(ctx, shipRoundIn{round: id, why: out.Why + "; its session had ended or moved on by then", ended: true})
}

// settleRound ends a round whose pull request was merged or closed while the
// run was open (doSettle), and the run with it: nothing is opened.
func (s *Service) settleRound(ctx context.Context, id int64, r *roundRow, out roundOut) error {
	if r == nil {
		return nil
	}
	if r.state == RoundPending || r.state == RoundRunning {
		s.setRoundNow(ctx, id, []string{r.state}, RoundSkipped, out.Why)
	}
	return s.endHeldRun(ctx, SystemActor(), r.issueID, s.shipAgent(ctx, r, "", r.sessionID.Int64))
}

// advanceRound asks decideRound and does what it says for a round with a
// before-PR ship: the one place a server-side ship or release is decided.
// It returns the decision, so a caller can tell a wait from a ship.
func (s *Service) advanceRound(ctx context.Context, id int64) (roundOut, error) {
	// No ship waits on a forge for ever, whoever asked for it.
	ctx, cancel := context.WithTimeout(ctx, shipBudget)
	defer cancel()
	in, err := s.roundInOf(ctx, id)
	if err != nil {
		return roundOut{}, err
	}
	out := decideRound(in)
	r, _ := roundByID(ctx, s.db.R, id)
	if !in.Unshipped && out.Do != doEnd {
		return roundOut{Do: doNothing}, nil // a round that isn't before a pull request ships nothing
	}
	switch out.Do {
	case doEnd:
		if out.Told != "" {
			// No review was recorded: nothing is opened, the person is told
			// once, by the write that ends the round, and wp ship works again.
			err = s.endUnreviewed(ctx, id, r, out)
			return out, err
		}
		// A round the window ended while its ship kept failing says what the
		// ship met, and tells the person.
		if bp := roundBeforePR(ctx, s.db.R, id); bp != nil && bp.ShipFirst != 0 && bp.ShipStop == "" {
			why := bp.ShipErr
			if why == "" {
				why = "its last try left no error"
			}
			if s.shipEnds(ctx, id, errors.New(why), true) {
				return out, nil
			}
		}
		s.setRoundNow(ctx, id, []string{in.State}, out.To, out.Why)
		return out, nil
	case doSettle:
		err = s.settleRound(ctx, id, r, out)
		return out, err
	case doReleaseAfterRunEnded:
		err = s.releaseEnded(ctx, id, out)
	case doShip, doShipCarried, doFinish:
		// doFinish: the pull request is open and the run isn't ended yet: the
		// ship picks up where the dead process left it (shipRound).
		err = s.shipReviewed(ctx, id)
	case doRelease:
		err = s.shipRound(ctx, shipRoundIn{round: id, why: out.Why})
		if err == nil && r != nil && r.state == RoundRunning {
			s.setRoundNow(ctx, id, []string{RoundRunning}, RoundFailed, out.Why)
			// The check's news is no use to a round that ended: dropped, and
			// a failure to drop it is said, not swallowed.
			if _, cerr := s.writeRows(ctx, `UPDATE session_messages SET state = 'cancelled' WHERE state = 'queued' AND step_key LIKE 'check:%' AND session_id = ?`, r.sessionID.Int64); cerr != nil {
				s.log.WarnContext(ctx, "drop the check news of a released round", "round", id, "session", r.sessionID.Int64, "err", cerr)
			}
		}
	}
	if out.Do == doShip || out.Do == doShipCarried || out.Do == doRelease || out.Do == doFinish || out.Do == doReleaseAfterRunEnded {
		// The try is recorded on the round whoever asked, so a retry's time and
		// the one notice to the person are the round's, not the caller's.
		key := ""
		if r != nil {
			key = r.key
		}
		s.shipTried(ctx, id, key, s.nowMS(), err)
		if err != nil && s.shipEnds(ctx, id, err, false) {
			err = nil
		}
	}
	return out, err
}

func errOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func nonzero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// decideIdle is the decision for a round whose session went idle without
// its Stop coming (autopilot's step): a check Waypoint is on is waited for;
// otherwise the round ends failed.
func (s *Service) decideIdle(ctx context.Context, id int64) (roundOut, error) {
	in, err := s.roundInOf(ctx, id)
	if err != nil {
		return roundOut{}, err
	}
	in.Idle = true
	return decideRound(in), nil
}

// shipOwed: Waypoint ships (or releases) the round now, for the callers that
// carry a round without being the sweep (autopilot's step): it advances it as
// decideRound says, and returns the decision and the ship's error.
func (s *Service) shipOwed(ctx context.Context, id int64) (roundOut, error) {
	if id == 0 {
		return roundOut{}, nil
	}
	// A ship that failed isn't tried again before its backoff is up, whoever
	// asks: the same gate as the sweep's, unless its time is spent and the
	// round only has to end (WP-1370).
	if bp := roundBeforePR(ctx, s.db.R, id); bp != nil && bp.ShipErr != "" && bp.ShipStop == "" && !s.shipDue(ctx, id, s.nowMS()) {
		if in, err := s.roundInOf(ctx, id); err != nil || !in.ShipSpent {
			return roundOut{}, err
		}
	}
	out, err := s.advanceRound(ctx, id)
	if errors.Is(err, ErrNotFound) && out.Do == doNothing {
		return roundOut{}, nil
	}
	return out, err
}

// releaseWhy: why a round that ended without running its review is
// released, which is also what the pull request and the person are told. A
// person's cancel is a decision, not a failure, and a round replaced or
// asked for again isn't released; one interrupted for the last time is.
func (s *Service) releaseWhy(ctx context.Context, r *roundRow, runEnded bool) (string, error) {
	var reason string
	if err := s.db.R.QueryRowContext(ctx, `SELECT reason FROM work_log WHERE id = ?`, r.id).Scan(&reason); err != nil {
		return "", err
	}
	reason = strings.TrimSpace(reason)
	if reason == roundReplaced || reason == roundRestarted {
		return "", nil // another round took the branch on
	}
	switch r.state {
	case RoundSkipped, RoundFailed:
		if !strings.HasPrefix(reason, roundCancelledBy) {
			return cmp(reason, "its adversarial review didn't run"), nil
		}
	case RoundInterrupted:
		// Interrupted for the last time, the round isn't asked for again
		// (askAgainAfterInterrupt): the session's stop that follows is a stop
		// with nobody left to ship, and Waypoint ships.
		again, err := askAgainAfterInterrupt(ctx, s.db.R, nonzero(r.sessionID.Int64, 0), r.issueID)
		if err != nil {
			return "", err
		}
		if !again {
			return fmt.Sprintf("the session was interrupted %d times before its adversarial review reported, so it isn't asked again", reviewInterruptsMax), nil
		}
		if runEnded {
			// Asked again only by a session that is there to be asked: the
			// run is over, so nobody is, and Waypoint ships.
			return "its adversarial review was interrupted and its run ended", nil
		}
	}
	return "", nil
}
