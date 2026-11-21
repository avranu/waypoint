package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// One question every caller that may advance a review round asks, and one
// answer (WP-1370): what does this round need, from what is recorded of it.
// The agent's ship, the settled check, the step, the sweep, the Stop and the
// session's end all ask decideRound, so none of them strands a round the
// others would have carried on, or ships one the others would have held.

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
	// Stopped, Subagents: the round's session just stopped (its turn ended),
	// with background agents of the review still going.
	Stopped, Subagents bool
	// Idle: the session went idle (autopilot's step saw it) and the round's Stop never came.
	Idle bool
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
	// doReleaseAfterRunEnded: the run ended with the round failed, skipped or
	// interrupted for Waypoint's own reason, and the branch has no pull request.
	// heldUnshipped, reviveHeldRounds, reaskHeldRound and reviewFirstAtStop all
	// need an open run, so nothing else would ever pick it up. WP-1325 writes
	// the action; advanceRound has the one call site to connect.
	doReleaseAfterRunEnded
)

type roundOut struct {
	Do    int
	For   string        // what a wait or a tell is for
	Until time.Duration // how much longer a wait or a tell may last
	Why   string        // why a release, an end or a carried ship
	To    string        // the state a doEnd ends the round in
}

// endOf: a round nothing will carry on, which ends in a state of its own
// (what sweepRounds used to decide). One whose check Waypoint is on, in an
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

// decideRound is the one decision. It never returns doNothing for an open
// run whose pull request isn't open, and no wait or tell outlasts its limit.
func decideRound(in roundIn) roundOut {
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
	if !in.RunOpen {
		if out, ok := endOf(in); ok {
			return out
		}
		switch in.State {
		case RoundFailed, RoundSkipped, RoundInterrupted:
			if in.Unshipped && !in.PROpen {
				return roundOut{Do: doReleaseAfterRunEnded, Why: "the review " + in.State + " and the run ended before its pull request opened"}
			}
		}
		return roundOut{Do: doNothing}
	}
	if in.PROpen {
		return roundOut{Do: doFinish}
	}
	if out, ok := endOf(in); ok {
		return out
	}
	left := func(limit time.Duration, since time.Duration) time.Duration { return max(limit-since, 0) }
	if in.Superseded {
		return roundOut{Do: doWait, For: "the review round its decision scheduled", Until: left(heldScreenWait, in.SinceChange)}
	}
	switch in.Check {
	case checkQueued, checkRunning:
		// expireChecks cancels a check at checkPatience; this bounds a check
		// that stays stuck anyway. The round's age plays no part: a check
		// still going is never "no report came back".
		if in.SinceChange > checkPatience+heldScreenWait {
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
		if in.SinceStart > reviewRunMax+autopilotNothingRetry {
			return roundOut{Do: doRelease, Why: "no report came back"}
		}
		return roundOut{Do: doWait, For: "the review's report", Until: left(reviewRunMax+autopilotNothingRetry, in.SinceStart)}
	case RoundPending:
		if in.SinceChange > heldScreenWait {
			return roundOut{Do: doRelease, Why: "the review didn't finish"}
		}
		return roundOut{Do: doWait, For: "the review", Until: left(heldScreenWait, in.SinceChange)}
	case RoundDone:
		switch {
		case in.Read == readFailed:
			return roundOut{Do: doRelease, Why: "Waypoint couldn't read the review's report"}
		case in.Read == readPending:
			if in.SinceChange > readWait {
				return roundOut{Do: doRelease, Why: "Waypoint couldn't read the review's report in time"}
			}
			return roundOut{Do: doWait, For: "Waypoint to read the review's report", Until: left(readWait, in.SinceChange)}
		case in.Audit == AuditQueued || in.Audit == AuditRunning:
			if in.SinceStart > reviewRunMax+autopilotNothingRetry {
				return roundOut{Do: doRelease, Why: "the review's audit never finished"}
			}
			return roundOut{Do: doWait, For: "the review's audit", Until: left(reviewRunMax+autopilotNothingRetry, in.SinceStart)}
		case in.Audit != AuditDone:
			return roundOut{Do: doRelease, Why: "the review's audit didn't finish (" + cmp(in.Audit, "none") + ")"}
		case in.Undecided > 0:
			if in.Session && in.SinceChange < heldScreenWait {
				return roundOut{Do: doTell, For: "the session to decide what the review found", Until: left(heldScreenWait, in.SinceChange)}
			}
			return roundOut{Do: doRelease, Why: fmt.Sprintf("%s the review found weren't decided in %d hours", plural1(in.Undecided, "thing"), int(heldScreenWait.Hours()))}
		case in.Check == checkDidnt:
			return roundOut{Do: doShipCarried, Why: "the check of its marks didn't run"}
		}
		return roundOut{Do: doShip}
	}
	// Skipped, failed, interrupted: the review didn't run to the end.
	return roundOut{Do: doRelease, Why: "the review " + in.State}
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
	started := nonzero(r.startedAt.Int64, r.createdAt)
	in.SinceStart = time.Duration(max(now-started, 0)) * time.Millisecond
	in.SinceChange = time.Duration(max(now-nonzero(r.ended.Int64, started), 0)) * time.Millisecond
	in.Read = r.readState
	if r.runID.Valid {
		if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE id = ? AND ended_at IS NULL)`, r.runID.Int64).Scan(&in.RunOpen); err != nil {
			return in, err
		}
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM review_again_decisions WHERE round_id = ? AND coalesce(next_round_id, 0) != 0)`, id).Scan(&in.Superseded); err != nil {
		return in, err
	}
	if bp := roundBeforePR(ctx, s.db.R, id); bp != nil {
		in.Unshipped = true
		if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pull_requests WHERE repo = ? COLLATE NOCASE AND head_ref = ? AND state IN ('open', 'draft'))`, bp.Repo, bp.Branch).Scan(&in.PROpen); err != nil {
			return in, err
		}
	}
	if r.sessionID.Valid && r.sessionID.Int64 != 0 {
		var live bool
		if s.db.R.QueryRowContext(ctx, `SELECT ended_at IS NULL FROM agent_sessions WHERE id = ?`, r.sessionID.Int64).Scan(&live) == nil {
			in.Session, in.SessionEnded = live, !live
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
	ra, err := s.roundAuditOf(ctx, id)
	if err != nil {
		return in, err
	}
	ids := []int64{ra.id}
	if rv, err := s.roundReviewer(ctx, id); err == nil && rv != nil {
		ids = append(ids, rv.ID)
	}
	for _, aid := range ids {
		if aid == 0 {
			continue
		}
		a, err := s.auditRow(ctx, aid)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return in, err
		}
		if aid == ra.id {
			in.Audit = a.Status
		}
		if a.Status != AuditDone {
			continue
		}
		if err := s.loadFindings(ctx, a); err != nil {
			return in, err
		}
		if a.readNothing() {
			continue
		}
		todo := len(s.roundTodo(ctx, a))
		in.Undecided += todo
		in.Decided += len(a.Findings) - todo
	}
	return in, nil
}

// advanceRound asks decideRound and does what it says for a round with a
// before-PR ship: the one place a server-side ship or release is decided.
// It returns the decision, so a caller can tell a wait from a ship.
func (s *Service) advanceRound(ctx context.Context, id int64) (roundOut, error) {
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
		s.setRoundNow(ctx, id, []string{in.State}, out.To, out.Why)
		return out, nil
	case doReleaseAfterRunEnded:
		// WP-1325 connects its release here (at the rebase): until then the
		// decision is made and logged, and nothing acts.
		s.log.WarnContext(ctx, "round left unshipped after its run ended", "round", id, "why", out.Why)
	case doShip, doShipCarried, doFinish:
		// doFinish: the pull request is open and the run isn't ended yet: the
		// ship picks up where the dead process left it (shipRound).
		err = s.shipReviewed(ctx, id)
	case doRelease:
		err = s.shipRound(ctx, shipRoundIn{round: id, why: out.Why})
		if err == nil && r != nil && r.state == RoundRunning {
			s.setRoundNow(ctx, id, []string{RoundRunning}, RoundFailed, out.Why)
			_, _ = s.db.W.ExecContext(ctx, `UPDATE session_messages SET state = 'cancelled' WHERE state = 'queued' AND step_key LIKE 'check:%' AND session_id = ?`, r.sessionID.Int64)
		}
	}
	if out.Do == doShip || out.Do == doShipCarried || out.Do == doRelease || out.Do == doFinish {
		// The try is recorded on the round whoever asked, so a retry's time and
		// the one notice to the person are the round's, not the caller's.
		key := ""
		if r != nil {
			key = r.key
		}
		s.shipTried(ctx, id, key, s.nowMS(), err)
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
	out, err := s.advanceRound(ctx, id)
	if errors.Is(err, ErrNotFound) && out.Do == doNothing {
		return roundOut{}, nil
	}
	return out, err
}
