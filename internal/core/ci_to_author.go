package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// authorHandBack is how long a failed check handed to the session that wrote
// the pull request waits for that session's new commit before babysitting
// sends a round as it always did. Not measured: a fix, a local run and a ship
// take about this long (WP-1878).
const authorHandBack = 30 * time.Minute

// authorFixingWait is what a babysit says while the author has its failure.
const authorFixingWait = "the session that wrote it is fixing its failed checks"

// Outcomes of a hand-off, besides pending (empty).
const (
	handoffFixed    = "fixed"     // a new head arrived
	handoffFellBack = "fell_back" // a round took it
	handoffCleared  = "cleared"   // its checks passed on the same head
)

// authorHandoff is one head whose failed checks went to the author.
type authorHandoff struct {
	Head    string   `json:"head"`
	At      int64    `json:"at"`
	Checks  []string `json:"checks"`
	Outcome string   `json:"outcome,omitempty"`
}

func (b *Babysit) handoffList() []authorHandoff {
	var l []authorHandoff
	_ = json.Unmarshal([]byte(b.handoffs), &l)
	return l
}

func (s *Service) saveHandoffs(ctx context.Context, b *Babysit, l []authorHandoff) bool {
	raw, err := json.Marshal(l)
	if err != nil {
		return false
	}
	if s.setBabysit(ctx, b, map[string]any{"author_handoffs": string(raw)}) != nil {
		return false
	}
	b.handoffs = string(raw)
	return true
}

// ciNewsKey is the step key of a head's failure news: one message for each
// pull request and head, so it is stored once.
func ciNewsKey(pr int64, head string) string { return "ci:" + itoa64(pr) + ":" + head }

// authorWaiting says whether the session that shipped the pull request is in
// a wait for its checks (or was within ciWaitWindow) and still running.
func (s *Service) authorWaiting(ctx context.Context, b *Babysit, pr *PullRequest) bool {
	return b.sessionID != 0 && s.shipWaits.checksHeld(b.sessionID, pr.ID, s.now()) && s.sessionLive(ctx, b.sessionID)
}

func (s *Service) sessionLive(ctx context.Context, session int64) bool {
	var live bool
	return s.db.R.QueryRowContext(ctx, `SELECT ended_at IS NULL FROM agent_sessions WHERE id = ?`, session).Scan(&live) == nil && live
}

// handToAuthor is the decision babysitting makes before it sends a round for
// failed checks: when the session that shipped the pull request waits in wp
// ship, it takes the failure, once for each head. It reports whether the
// failure is the author's now (no round is due). A hand-off past
// authorHandBack, or whose session ended, falls back to a round (WP-1878).
func (s *Service) handToAuthor(ctx context.Context, b *Babysit, pr *PullRequest, failed []string) bool {
	list := b.handoffList()
	for i, h := range list {
		if !sameSHA(h.Head, pr.HeadSHA) {
			continue
		}
		if h.Outcome != "" {
			return false
		}
		if s.now().Sub(msTime(h.At)) >= authorHandBack || !s.sessionLive(ctx, b.sessionID) {
			s.closeHandoff(ctx, b, pr, i, handoffFellBack)
			return false
		}
		s.setState(ctx, b, pr, BabysitWatching, authorFixingWait)
		return true
	}
	if !s.authorWaiting(ctx, b, pr) {
		return false
	}
	text := authorNewsText(pr, failed)
	if _, err := s.queueMessage(ctx, b.sessionID, text, ciNewsKey(pr.ID, pr.HeadSHA), messageFrom{}, true); err != nil {
		s.log.WarnContext(ctx, "hand failed checks to the session that wrote them", "pr", pr.ID, "err", err)
		return false
	}
	if !s.saveHandoffs(ctx, b, append(list, authorHandoff{Head: pr.HeadSHA, At: s.nowMS(), Checks: failed})) {
		s.cancelCINews(ctx, pr.ID, pr.HeadSHA)
		return false
	}
	s.setState(ctx, b, pr, BabysitWatching, authorFixingWait)
	return true
}

// authorNewsText is what the author is told: which pull request, the failed
// checks and the command that prints each one's failing part.
func authorNewsText(pr *PullRequest, failed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s#%d failed its checks on %s: %s.\n", pr.Repo, pr.Number, short(pr.HeadSHA), strings.Join(failed, ", "))
	b.WriteString("Read the failing part of each with:\n")
	for _, n := range failed {
		if n == "checks" {
			fmt.Fprintf(&b, "  wp pr log %d\n", pr.Number)
			continue
		}
		fmt.Fprintf(&b, "  wp pr log %d --check %q\n", pr.Number, n)
	}
	fmt.Fprintf(&b, "You wrote this change, so fix it, commit, and run `wp ship` again. If no new commit arrives in %d minutes, Waypoint's babysitting sends a round to fix it.", int(authorHandBack.Minutes()))
	return b.String()
}

// closeHandoff records how a hand-off ended, and withdraws its news if the
// author never took it.
func (s *Service) closeHandoff(ctx context.Context, b *Babysit, pr *PullRequest, i int, outcome string) {
	list := b.handoffList()
	if i >= len(list) || list[i].Outcome != "" {
		return
	}
	list[i].Outcome = outcome
	s.saveHandoffs(ctx, b, list)
	s.cancelCINews(ctx, pr.ID, list[i].Head)
}

func (s *Service) cancelCINews(ctx context.Context, pr int64, head string) {
	if _, err := s.writeRows(ctx, `UPDATE session_messages SET state = 'cancelled' WHERE step_key = ? AND state = 'queued'`, ciNewsKey(pr, head)); err != nil {
		s.log.WarnContext(ctx, "withdraw failed-check news", "pr", pr, "err", err)
	}
}

// settleHandoffs ends the pending hand-offs that aren't for the head being
// looked at (the author pushed: fixed), and, with outcome, the one for it.
func (s *Service) settleHandoffs(ctx context.Context, b *Babysit, pr *PullRequest, outcome string) {
	for i, h := range b.handoffList() {
		if h.Outcome != "" {
			continue
		}
		switch {
		case !sameSHA(h.Head, pr.HeadSHA):
			s.closeHandoff(ctx, b, pr, i, handoffFixed)
		case outcome != "":
			s.closeHandoff(ctx, b, pr, i, outcome)
		}
	}
}

// handoffCounts are the hand-offs begun since the time: all, those a new head
// fixed and those a round took.
func handoffCounts(raw string, since time.Time) (handed, fixed, fellBack int) {
	var l []authorHandoff
	_ = json.Unmarshal([]byte(raw), &l)
	for _, h := range l {
		if msTime(h.At).Before(since) {
			continue
		}
		handed++
		switch h.Outcome {
		case handoffFixed:
			fixed++
		case handoffFellBack:
			fellBack++
		}
	}
	return
}

// AwaitShipChecks holds a wp ship that waits on a pull request it shipped
// (up to wait, at most shipNewsMax) until the head's first checks have an
// outcome: they passed, they failed and babysitting handed the failure to this
// session (its news, delivered once the client acks it: AckShipNews), or
// something else took the pull request. While it waits, babysitting hands this
// session a failed check instead of sending a round (WP-1878). Neither in
// time: waiting, with what is still going.
func (s *Service) AwaitShipChecks(ctx context.Context, actor *Actor, cliID, machine, taskKey string, prID int64, head string, wait time.Duration) (*ShipNews, error) {
	session, task, err := s.shipWaitTarget(ctx, actor, cliID, machine, taskKey)
	if err != nil {
		return nil, err
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if _, err := s.prAuthority(ctx, actor, pr, RoleViewer); err != nil {
		return nil, err
	}
	wait = min(max(wait, 0), shipNewsMax)
	end := s.openWait(session, task, prID)
	finished := true
	defer func() { end(!finished) }()
	ch := s.stopWaits.addShip(session)
	defer s.stopWaits.removeShip(session, ch)
	woken, done := s.prWait.add(prID)
	defer done()
	s.scheduleMergeRecheck(ctx, prID)
	if head != "" && !sameSHA(pr.HeadSHA, head) {
		s.prWaitRefresh(ctx, pr)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	recheck := time.NewTicker(prWaitRecheck)
	defer recheck.Stop()
	for {
		got, err := s.shipChecksNow(ctx, session, task, prID, head)
		if err != nil {
			return nil, err
		}
		if got.Outcome != ShipNewsWaiting {
			// The client acks the failure next: keep the news held until it has.
			finished = got.Outcome != ShipNewsChecksFailed
			s.logShipNews(ctx, session, task, got)
			return got, nil
		}
		select {
		case <-ch:
		case <-woken:
		case <-recheck.C:
			if pr, err := s.prByID(ctx, s.db.R, prID); err == nil {
				s.prWaitRefresh(ctx, pr)
			}
		case <-deadline.C:
			finished = false
			s.logShipNews(ctx, session, task, got)
			return got, nil
		case <-ctx.Done():
			finished = false
			return nil, ctx.Err()
		}
	}
}

// shipChecksNow is where the wait stands: the failure handed to the session
// first, then what the head's checks say.
func (s *Service) shipChecksNow(ctx context.Context, session, task, prID int64, head string) (*ShipNews, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+messageCols+` FROM session_messages m LEFT JOIN users u ON u.id = m.created_by
		WHERE m.session_id = ? AND m.state = 'queued' AND m.step_key LIKE ? ORDER BY m.id`, session, "ci:"+itoa64(prID)+":%")
	if err != nil {
		return nil, err
	}
	var found []*SessionMessage
	var ids []int64
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		found = append(found, m)
		ids = append(ids, m.ID)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(found) > 0 {
		return &ShipNews{Outcome: ShipNewsChecksFailed, Checks: true, Text: MessagesText(found), IDs: ids}, nil
	}
	w, err := s.prWaitState(ctx, prID, head)
	if err != nil {
		return nil, err
	}
	at := fmt.Sprintf("%s#%d", w.Repo, w.Number)
	over := func(text string) *ShipNews {
		return &ShipNews{Outcome: ShipNewsChecksOver, Checks: true, Text: text}
	}
	switch w.Verdict {
	case PRWaitMerged:
		return &ShipNews{Outcome: ShipNewsChecksPassed, Checks: true, Text: at + " merged. Nothing more to do."}, nil
	case PRWaitClosed:
		return over(at + " was closed without merging."), nil
	case PRWaitFailed:
		if why := s.babysitHasIt(ctx, prID, w.HeadSHA); why != "" {
			return over(fmt.Sprintf("%s failed its checks (%s), and %s.", at, w.Why, why)), nil
		}
		return &ShipNews{Outcome: ShipNewsWaiting, Checks: true, Text: at + " has failed checks: " + w.Why + "; Waypoint is deciding who fixes them."}, nil
	}
	if w.CI == CIPassed && (head == "" || sameSHA(w.HeadSHA, head)) {
		return &ShipNews{Outcome: ShipNewsChecksPassed, Checks: true, Text: at + ": its checks passed. " + checksStanding(w)}, nil
	}
	if w.Verdict == PRWaitConflict {
		return over(fmt.Sprintf("%s has a conflict with %s; Waypoint's babysitting takes it from here.", at, cmp(w.BaseRef, "its base"))), nil
	}
	text := at + ": " + cmp(w.Why, "waiting for its checks") + "."
	return &ShipNews{Outcome: ShipNewsWaiting, Checks: true, Text: text}, nil
}

// checksStanding says where a pull request with passing checks stands.
func checksStanding(w *PRWait) string {
	switch {
	case w.Verdict == PRWaitReady && w.Armed:
		return "It is ready and armed, so it merges when its turn comes."
	case w.Verdict == PRWaitReady:
		return "It is ready to merge."
	case w.Why != "":
		return "It still waits on: " + w.Why + "."
	case len(w.Readiness.Reasons) > 0:
		return "It still waits on: " + strings.Join(w.Readiness.Reasons, "; ") + "."
	}
	return ""
}

// babysitHasIt says, for a pull request whose checks failed on head, why
// no hand-off to the author is coming: babysitting is not on it, a round has
// it, or it waits on a person ("" when a hand-off may still come).
func (s *Service) babysitHasIt(ctx context.Context, prID int64, head string) string {
	b, err := s.activeBabysit(ctx, s.db.R, prID)
	switch {
	case err != nil:
		return ""
	case b == nil:
		return "nobody is babysitting it: `wp pr babysit` starts that"
	}
	for _, h := range b.handoffList() {
		if sameSHA(h.Head, head) && h.Outcome == handoffFellBack {
			return "Waypoint's babysitting sent a round to fix them"
		}
	}
	switch {
	case b.JobID != 0 && sameSHA(b.RoundSHA, head):
		return "Waypoint's babysitting sent a round to fix them"
	case b.State == BabysitNeedsPerson, b.State == BabysitResting, b.State == BabysitGaveUp:
		return "Waypoint's babysitting handed it to a person"
	}
	return ""
}
