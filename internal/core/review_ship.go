package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Waypoint runs the ship the session would have run whenever the run can go
// on without the session (WP-1370): after a round whose audit left nothing
// to do (shipReviewed, called from planFromFindings' step), and after a
// check of the session's marks that found none of them wrong (here). A check
// that found marks wrong is the session's to answer, with all its findings
// in one message (tellCheck).

// checkRound is the round a check audit belongs to (0: not a round's check).
func (s *Service) checkRound(ctx context.Context, a *CodeAudit) int64 {
	var key string
	if s.db.R.QueryRowContext(ctx, `SELECT request_key FROM code_audits WHERE id = ?`, a.ID).Scan(&key) != nil {
		return 0
	}
	var round int64
	if _, err := fmt.Sscanf(key, checkAuditKey, &round); err != nil {
		return 0
	}
	return round
}

// onCheckSettled: a round's check of its marks settled; when it leaves
// nothing for the session to mark, the round goes on without it, off the
// request path. A failed open is retried by shipSettledRounds.
func (s *Service) onCheckSettled(ctx context.Context, e *Event) {
	if e.Kind != EvCodeAudit || e.Field != "settled" {
		return
	}
	id, _ := e.Data["audit"].(int64)
	if id == 0 {
		return
	}
	s.goBG(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shipBudget)
		defer cancel()
		s.continueAfterCheck(ctx, id)
	})
}

// continueAfterCheck carries a round on after its check ended with nothing
// for the session to mark: the check agreed (no findings), or couldn't say
// (cancelled by expireChecks, failed, at a limit). The ship the session
// would have run runs here: it opens the pull request, or schedules the next
// round when the findings call for one.
func (s *Service) continueAfterCheck(ctx context.Context, auditID int64) {
	a, err := s.auditRow(ctx, auditID)
	if err != nil || a.Purpose != AuditCheck {
		return
	}
	round := s.checkRound(ctx, a)
	if round == 0 {
		return
	}
	// What the round needs now, from what is recorded (decideRound): a check
	// that agreed or didn't run ships it, one nobody can take is released with
	// its marks carried, one a live session can take waits for it (tellCheck).
	r, err := roundByID(ctx, s.db.R, round)
	if err != nil || r == nil || r.state != RoundRunning {
		return // a Done round is the sweep's (shipSettledRounds)
	}
	out, err := s.advanceRound(ctx, round)
	s.log.InfoContext(ctx, "round advanced after its check", "round", round, "audit", auditID, "do", out.Do, "why", out.Why, "err", err)
	if err != nil {
		s.log.WarnContext(ctx, "autopilot: carry a round on after its check", "round", round, "audit", auditID, "err", err)
	}
}

// checkTodo is what a finished check found wrong that the session hasn't
// decided: none for one that failed or was cancelled.
func (s *Service) checkTodo(ctx context.Context, a *CodeAudit) ([]CodeAuditFinding, error) {
	if a.Status != AuditDone {
		return nil, nil
	}
	if a.Findings == nil {
		if err := s.loadFindings(ctx, a); err != nil {
			return nil, err
		}
	}
	var todo []CodeAuditFinding
	for _, f := range a.Findings {
		if findingToDo(f) {
			todo = append(todo, f)
		}
	}
	return todo, nil
}

// tellCheck tells the session what its round's check found wrong: all of it
// in one message, which waits on the session like a ship would (blocking).
// A check that found nothing, or ended without an answer, tells no one:
// Waypoint carries the round on (continueAfterCheck).
func (s *Service) tellCheck(ctx context.Context, a *CodeAudit) {
	todo, err := s.checkTodo(ctx, a)
	if err != nil || len(todo) == 0 {
		return
	}
	// Only a round in a session that can take it is the session's to answer
	// (decideRound says so, for a running round and for a done one: a ship
	// ends its round Done while the check runs); any other waits on Waypoint:
	// continueAfterCheck releases a running round at once, the sweep a done
	// one, each opening its pull request with the marks carried and telling
	// the person.
	round := s.checkRound(ctx, a)
	r, err := roundByID(ctx, s.db.R, round)
	if err != nil || r == nil || (r.state != RoundRunning && r.state != RoundDone) || a.sessionID == 0 {
		s.log.InfoContext(ctx, "check news not sent to a session", "audit", a.ID, "round", round, "session", a.sessionID)
		return
	}
	if in, err := s.roundInOf(ctx, round); err != nil || decideRound(in).Do != doTell {
		s.log.InfoContext(ctx, "check news not sent: the round doesn't wait on a session", "audit", a.ID, "round", round, "session", a.sessionID, "err", err)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Waypoint's check of your marks on %s's adversarial review (audit #%d) disagrees with %s. These are another model's claims: read the code before you accept one.\n\n", r.key, a.ID, plural1(len(todo), "mark"))
	if facts, _ := s.roundFacts(ctx, r); facts != "" {
		fmt.Fprintf(&b, "Where the round stands: %s.\n\n", facts)
	}
	var claims strings.Builder
	for _, f := range todo {
		at := f.Path
		if f.Line > 0 {
			at = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		fmt.Fprintf(&claims, "[%s] %s, %s: %s\nWhat the check says is wrong: %s\n\n", f.ID, f.Severity, cmp(at, "(no file)"), clipText(strings.TrimSpace(f.Title), 200), clipText(strings.TrimSpace(f.Problem), checkProblemMax))
	}
	b.WriteString("<untrusted>\n" + untrusted(strings.TrimRight(claims.String(), "\n")) + "\n</untrusted>\n\n")
	fmt.Fprintf(&b, "Mark each one (`wp audit mark %s fixed -m \"what you did\"`, or `wp audit mark %s rejected -m \"why your mark stands\"`), then run `wp ship --summary \"what you fixed, filed and rejected\"` once. Nothing else is needed after that.\n%s/audits/%d",
		todo[0].ID, todo[0].ID, s.BaseURL(), a.ID)
	s.messageSession(ctx, a, b.String(), fmt.Sprintf("check:%d:%s", a.ID, a.Status))
	s.log.InfoContext(ctx, "check news queued for a session", "audit", a.ID, "round", round, "session", a.sessionID, "task", r.key, "marks", len(todo))
}

// checkProblemMax clips what the check says is wrong with one mark (as the
// text says it does: "read the audit page for all of it").
const checkProblemMax = 4000

// shipBackoff is how long to wait after the n-th failed try: quick at first,
// then heldShipRetry for as long as it lasts.
func shipBackoff(n int) time.Duration {
	steps := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}
	if n <= len(steps) {
		return steps[max(n, 1)-1]
	}
	return heldShipRetry
}

// forgeSaid is an error that carries a forge's own words, made fit to put in a
// notice or a step's reason: on one line, bounded, and with any tag that
// would pass it off as ours removed.
func forgeSaid(err error) string {
	if err == nil {
		return ""
	}
	if cause := forgeSetupFault(err); cause != "" {
		return clipText(strings.Join(strings.Fields(cause), " "), 400) // ours, not the forge's words
	}
	return forgeWords(err.Error(), 300)
}

// forgeWords is a forge's own text made fit to put in a note, a notice or a
// message: on one line, bounded, and with any tag that would pass it off as
// ours removed.
func forgeWords(s string, most int) string {
	return clipText(untrusted(strings.Join(strings.Fields(s), " ")), most)
}

// shipWait is how long to wait after the n-th failed try of a round's ship:
// the backoff, or what the forge asked for (Retry-After, up to six hours) if
// longer, and a little more, by the round, so rounds that failed together
// don't all try again in the same second.
func shipWait(round int64, n int, err error) time.Duration {
	wait := shipBackoff(n)
	var ae *forgeAPIError
	if errors.As(err, &ae) && ae.retryAfter > wait {
		wait = min(ae.retryAfter, 6*time.Hour)
	}
	return wait + wait/100*time.Duration(round%10)
}

// scanIDs reads every row of ids, or fails: a row that can't be read is
// not a round to skip (WP-1407).
func scanIDs(rows *sql.Rows) ([]int64, error) {
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// shipSettledRounds is the sweep behind shipReviewed: a round that is due a
// ship of Waypoint's and has none (the first try failed, the server
// restarted, the forge was down) tries again, quick at first, then every
// heldShipRetry, for as long as it takes: the round waits on Waypoint, not
// on anyone, so it is never given up on. The person is told once, with the
// error, when it has failed for heldShipRetry. A round the step carries is
// its step's (advanceStep), unless the step has stopped.
func (s *Service) shipSettledRounds(ctx context.Context) {
	now := s.nowMS()
	// Two kinds of round: one in an open run that nothing carries (or whose
	// check settled), and one whose run ended (or whose session ended or moved
	// on) with the branch unshipped, done or not. The second kind is
	// pre-filtered the way decideRound decides it (newest round, no pull
	// request linked, in the window) so the batch isn't spent on rounds it
	// leaves alone; decideRound is the one that says.
	rows, err := s.db.R.QueryContext(ctx, `SELECT w.id FROM work_log w JOIN agent_runs r ON r.id = w.run_id
		JOIN agent_sessions x ON x.id = w.session_id JOIN issues i ON i.id = w.issue_id
		WHERE w.kind = 'review' AND w.before_pr != '' AND (
			(r.ended_at IS NULL AND w.state IN ('pending', 'running', 'done') AND (
				NOT EXISTS (SELECT 1 FROM autopilot_steps st WHERE st.review_id = w.id AND st.state IN ('due', 'waiting', 'starting'))
				OR (w.state = 'running' AND EXISTS (SELECT 1 FROM code_audits c WHERE c.request_key = 'review-round:' || w.id || ':check'
					AND c.settled_at IS NOT NULL))))
			OR (w.state IN ('skipped', 'failed', 'interrupted', 'done') AND i.resolved_at IS NULL
				AND (r.ended_at IS NOT NULL OR x.ended_at IS NOT NULL
					OR EXISTS (SELECT 1 FROM agent_runs o WHERE o.session_id = w.session_id AND o.id > w.run_id AND o.issue_id != w.issue_id))
				AND w.id = (SELECT max(m.id) FROM work_log m WHERE m.kind = 'review' AND m.issue_id = w.issue_id)
				AND NOT EXISTS (SELECT 1 FROM external_links l WHERE l.issue_id = w.issue_id AND l.kind = 'pull' AND NOT `+unapprovedOutsidePullSQL("l.url")+`)
				AND (max(w.updated_at, coalesce(r.ended_at, 0), coalesce(x.ended_at, 0),
					coalesce((SELECT min(o.started_at) FROM agent_runs o WHERE o.session_id = w.session_id AND o.id > w.run_id AND o.issue_id != w.issue_id), 0)) > ?
					OR coalesce(json_extract(w.before_pr, '$.ship_first'), 0) > 0)))
		ORDER BY w.id DESC LIMIT 50`, now-strandedShipWindow.Milliseconds())
	if err != nil {
		s.log.WarnContext(ctx, "rounds due a ship of Waypoint's", "err", err)
		return
	}
	ids, err := scanIDs(rows)
	if err != nil {
		s.log.WarnContext(ctx, "rounds due a ship of Waypoint's: read them", "err", err)
		return
	}
	for _, id := range ids {
		r, err := roundByID(ctx, s.db.R, id)
		if err != nil || r == nil {
			continue
		}
		in, err := s.roundInOf(ctx, id)
		if err != nil {
			continue
		}
		out := decideRound(in)
		if out.Do != doShip && out.Do != doShipCarried && out.Do != doRelease && out.Do != doFinish && out.Do != doReleaseAfterRunEnded && out.Do != doSettle && (out.Do != doEnd || (!in.ShipSpent && out.Told == "")) {
			continue
		}
		if out.Do != doEnd && !s.shipDue(ctx, id, now) {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, shipBudget)
		_, err = s.advanceRound(rctx, id)
		cancel()
		if err != nil {
			s.log.WarnContext(ctx, "autopilot: advance a settled round", "round", id, "task", r.key, "err", err)
		}
	}
}

// shipDue says whether a round's next try of the ship is now. The tries are
// recorded on the round (before_pr), so a restart neither forgets them nor
// tries at once, and the person isn't told twice.
func (s *Service) shipDue(ctx context.Context, round, now int64) bool {
	bp := roundBeforePR(ctx, s.db.R, round)
	return bp == nil || now >= bp.ShipNext
}

// shipTried records a try of the ship of Waypoint's on the round: the next
// one's time, the error, and, after heldShipRetry of failing, the one notice
// to the person (its flag is recorded with the rest, so a restart doesn't
// send it again).
func (s *Service) shipTried(ctx context.Context, round int64, key string, now int64, err error) {
	// The record of a try outlives the try's deadline: it is what says the
	// ship ran out of time.
	ctx = context.WithoutCancel(ctx)
	var tries int
	// What the round did, as it was recorded: the notice says no more of the
	// review than that (it may not have run at all).
	did, _ := s.factsOfRound(ctx, round)
	if did == "" {
		did = "no result of its review is recorded"
	}
	merr := s.write(ctx, SystemActor(), func(t *tx) error {
		bp := roundBeforePR(ctx, t, round)
		if bp == nil {
			return nil
		}
		next := *bp
		tell := false
		if err == nil {
			next.ShipTries, next.ShipFirst, next.ShipNext, next.ShipTold, next.ShipErr, next.ShipRefused = 0, 0, 0, false, "", false
		} else {
			if next.ShipFirst == 0 {
				next.ShipFirst = now
			}
			next.ShipTries++
			next.ShipNext = now + shipWait(round, next.ShipTries, err).Milliseconds()
			next.ShipErr = clipText(err.Error(), 500)
			next.ShipRefused = shipRefused(err)
			if !next.ShipTold && now-next.ShipFirst >= heldShipRetry.Milliseconds() {
				next.ShipTold, tell = true, true
			}
		}
		tries = next.ShipTries
		if next == *bp {
			return nil
		}
		raw, e := json.Marshal(next)
		if e != nil {
			return e
		}
		if _, e := t.ExecContext(ctx, `UPDATE work_log SET before_pr = ? WHERE id = ?`, string(raw), round); e != nil {
			return e
		}
		if !tell {
			return nil
		}
		// The notice is stored by the write that sets the flag: both or
		// neither, and dispatch sends it on.
		var userID int64
		if t.QueryRowContext(ctx, `SELECT coalesce(user_id, 0) FROM agent_runs WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, round).Scan(&userID) != nil || userID == 0 {
			return nil
		}
		_, e = t.storeNotice(ctx, userID, NotifyAutopilot, "Waypoint can't open "+key+"'s pull request",
			fmt.Sprintf("%s's adversarial review round has ended (%s), and Waypoint has tried %d times over %d minutes to open its pull request: %s. It keeps trying every %d minutes.", key, did, next.ShipTries, int(heldShipRetry.Minutes()), forgeSaid(err), int(heldShipRetry.Minutes())),
			"/agents?tab=autopilot")
		return e
	})
	if merr != nil {
		s.log.WarnContext(ctx, "autopilot: record a try of the ship", "round", round, "err", merr)
	}
	if err == nil {
		s.log.InfoContext(ctx, "server-side ship done", "round", round, "task", key)
		return
	}
	s.log.WarnContext(ctx, "autopilot: open a reviewed pull request", "round", round, "task", key, "try", tries, "err", err)
}

// shipBudget bounds one server ship of a round (the sweep's, and the one a
// settled check starts): every forge call and write in it.
var shipBudget = 5 * time.Minute

// shipGiveUp is how long Waypoint keeps trying a failing ship before it ends
// the round with the last error.
const shipGiveUp = 24 * time.Hour

// shipRefused says a failure of the ship won't pass by being tried again: the
// forge refused the request (not authorised, not found, not valid), or what
// the ship needs is gone.
func shipRefused(err error) bool {
	if forgeSetupFault(err) != "" {
		return true
	}
	var ae *forgeAPIError
	if errors.As(err, &ae) {
		return forgeRefusalStatus(ae.status) && !ae.limited // a rate limit passes by waiting
	}
	return errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound)
}

// shipEnds ends a round whose ship failed for good (shipRefused), or has
// failed for shipGiveUp, saying why and telling the person (stopShipWith):
// it says whether it did. The error is the last one the ship met.
func (s *Service) shipEnds(ctx context.Context, id int64, err error, last bool) bool {
	ctx = context.WithoutCancel(ctx)
	bp := roundBeforePR(ctx, s.db.R, id)
	if bp == nil || bp.ShipStop != "" {
		return false
	}
	refused := shipRefused(err) || bp.ShipRefused
	spent := bp.ShipFirst != 0 && s.nowMS()-bp.ShipFirst >= shipGiveUp.Milliseconds()
	if !refused && !spent && !last {
		return false
	}
	r, rerr := roundByID(ctx, s.db.R, id)
	if rerr != nil || r == nil {
		return false
	}
	in := s.endingShip(ctx, r)
	why := forgeSaid(err)
	body := "Waypoint did not open the pull request: the forge refused it: " + why + "."
	title := "pull request not opened"
	if !refused && spent {
		body = fmt.Sprintf("Waypoint gave up opening the pull request after trying for %d hours (%d tries). The last error: %s.", int(shipGiveUp.Hours()), bp.ShipTries, why)
		title = "pull request not opened: gave up"
	} else if !refused {
		// The round's window closed before the give-up did: say what
		// happened, not a give-up it wasn't.
		body = fmt.Sprintf("Waypoint stopped trying to open the pull request: the round ended after %d tries. The last error: %s.", bp.ShipTries, why)
		title = "pull request not opened: stopped trying"
	}
	if serr := s.stopShipWith(ctx, r, in, body, title, why); serr != nil {
		s.log.WarnContext(ctx, "autopilot: end a round whose ship failed", "round", id, "err", serr)
		return false
	}
	return true
}

// endingShip is who a round's end acts for: its session, the user it belongs
// to and the agent its run was.
func (s *Service) endingShip(ctx context.Context, r *roundRow) shipRoundIn {
	in := shipRoundIn{round: r.id, session: r.sessionID.Int64}
	in.agent = s.shipAgent(ctx, r, "", in.session)
	_ = s.db.R.QueryRowContext(ctx, `SELECT user_id FROM agent_sessions WHERE id = ?`, in.session).Scan(&in.userID)
	return in
}

// endUnreviewed ends a round that has no review (decideRound: Told): nothing
// is opened, and the round, the person's notice and the release of the run
// and the claim are one write (stopShipWith), so wp ship starts again.
func (s *Service) endUnreviewed(ctx context.Context, id int64, r *roundRow, out roundOut) error {
	ctx = context.WithoutCancel(ctx)
	if r == nil {
		return nil
	}
	if bp := roundBeforePR(ctx, s.db.R, id); bp != nil && bp.ShipStop != "" {
		return nil
	}
	return s.stopShipWith(ctx, r, s.endingShip(ctx, r), out.Told, "pull request not opened: no review recorded", out.Why)
}
