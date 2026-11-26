package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// What a wait returns (ShipNews.Outcome).
const (
	ShipNewsFindings = "findings"
	ShipNewsPull     = "pull"
	ShipNewsEnded    = "ended"
	ShipNewsWaiting  = "waiting"
	ShipNewsNone     = "none"
	// What a wait for a pull request's checks returns (AwaitShipChecks).
	ShipNewsChecksPassed = "checks_passed"
	ShipNewsChecksFailed = "checks_failed"
	ShipNewsChecksOver   = "checks_over"
)

const (
	// shipNewsMax is the longest one wait is held.
	shipNewsMax = prWaitMax
	// shipNewsRecheck is how often a wait reads the round's state again, for
	// what no message announces (the pull request opening).
	shipNewsRecheck = 25 * time.Second
	// shipWaitLinger keeps a wait registered after its call returned, so the
	// client's reconnect gap doesn't lose the news it queued for.
	shipWaitLinger = 15 * time.Second
)

// ShipPull is the pull request a round's end opened.
type ShipPull struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
}

// ShipNews is what wp ship waited for.
type ShipNews struct {
	Outcome string    `json:"outcome"`
	Text    string    `json:"text,omitempty"`
	Pull    *ShipPull `json:"pull,omitempty"`
	// IDs are the messages of Text: still queued until AckShipNews.
	IDs []int64 `json:"ids,omitempty"`
	// Checks marks the answer of a wait for checks (AwaitShipChecks): a
	// server that predates it never sets it, which is how wp ship knows to
	// stop at "opened" (WP-1878).
	Checks bool `json:"checks,omitempty"`
}

// shipWaits is the registry of waits, by session and task.
type shipWaits struct {
	mu sync.Mutex
	by map[int64]map[int64]*shipWaitEntry
}

type shipWaitEntry struct {
	open  int
	until time.Time
	// checks: a wait for the checks of the pull request pr is open or was
	// (AwaitShipChecks), and its news goes to the wait; only such a wait lets
	// the babysit hand a failed check to the session instead of a round.
	checks bool
	pr     int64
	// checksOpen counts the checks calls open now, and checksUntil is how
	// long the last one's hold lasts: the review wait of the same task
	// neither is a checks wait nor keeps one alive.
	checksOpen  int
	checksUntil time.Time
	// since is when the checks wait first opened: what the bound on a head
	// whose checks Waypoint can't read counts from.
	since time.Time
	head  string // the head the wait asked about last
}

// holder says, for one session's messages, whether a wp ship waits for each
// (audit or check news of its task). An audit's or check's task is looked up
// once, however many of its messages are asked about.
func (w *shipWaits) holder(ctx context.Context, q queryer, now time.Time, session int64) func(m *SessionMessage) bool {
	w.mu.Lock()
	n := len(w.by[session])
	w.mu.Unlock()
	if n == 0 {
		return func(*SessionMessage) bool { return false }
	}
	tasks := map[string]int64{}
	return func(m *SessionMessage) bool {
		if !shipNewsKey(m.stepKey) {
			return false
		}
		kind, id := stepKeySource(m.stepKey)
		if kind == "ci" {
			pr, _ := strconv.ParseInt(id, 10, 64)
			return w.checksHeld(session, pr, now)
		}
		task, ok := tasks[kind+":"+id]
		if !ok {
			if t, bound := messageTask(ctx, q, m.stepKey); bound {
				task = t
			}
			tasks[kind+":"+id] = task
		}
		return task != 0 && w.held(session, task, now)
	}
}

// checksHeld says whether a wait for the checks of the pull request is open
// for the session, or ended within ciWaitWindow.
func (w *shipWaits) checksHeld(session, pr int64, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.by[session] {
		if e.checks && e.pr == pr && (e.checksOpen > 0 || now.Before(e.checksUntil)) {
			return true
		}
	}
	return false
}

// checksSince is when the session's wait for the pull request's checks began
// (the zero time when none is registered).
func (w *shipWaits) checksSince(session, pr int64) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.by[session] {
		if e.checks && e.pr == pr {
			return e.since
		}
	}
	return time.Time{}
}

// noteHead restarts the bound for a wait that now asks about another head:
// the new head's checks haven't had their time yet.
func (w *shipWaits) noteHead(session, pr int64, head string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.by[session] {
		if e.checks && e.pr == pr && e.head != head {
			e.head, e.since = head, now
		}
	}
}

func (w *shipWaits) held(session, task int64, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.by[session][task]
	return e != nil && (e.open > 0 || now.Before(e.until))
}

// openShipWait registers a wait of the session on the task's round; the
// returned func ends the call's hold, which lasts shipWaitLinger more.
func (s *Service) openShipWait(session, task int64) func() {
	end := s.openWait(session, task, 0)
	return func() { end(true) }
}

// ciWaitWindow is how long a wait for checks still counts as waiting after
// its call returned: the client asks again at once, and its retries back off
// by seconds (WP-1878).
const ciWaitWindow = time.Minute

// openWait registers a wait as openShipWait does; a pr other than 0 makes it
// a wait for that pull request's checks. The returned func ends the call's
// hold: it lingers (ciWaitWindow for checks) when the client is expected to
// ask again, and ends at once when the wait is over.
func (s *Service) openWait(session, task, pr int64) func(linger bool) {
	w := &s.shipWaits
	w.mu.Lock()
	if w.by == nil {
		w.by = map[int64]map[int64]*shipWaitEntry{}
	}
	if w.by[session] == nil {
		w.by[session] = map[int64]*shipWaitEntry{}
	}
	e := w.by[session][task]
	if e == nil {
		e = &shipWaitEntry{}
		w.by[session][task] = e
	}
	e.open++
	if pr != 0 {
		if !e.checks || e.pr != pr {
			e.since = s.now()
		}
		e.checks, e.pr = true, pr
		e.checksOpen++
	}
	w.mu.Unlock()
	var once sync.Once
	return func(linger bool) {
		once.Do(func() {
			hold := shipWaitLinger
			if pr != 0 {
				hold = ciWaitWindow
			}
			if !linger {
				hold = 0
			}
			w.mu.Lock()
			e.open--
			e.until = s.now().Add(hold)
			if pr != 0 {
				e.checksOpen--
				e.checksUntil = e.until
			} else if e.checksUntil.After(e.until) {
				e.until = e.checksUntil
			}
			w.mu.Unlock()
			time.AfterFunc(hold+time.Second, func() {
				s.goBG(func() { s.shipWaitLapsed(context.Background(), session) })
			})
		})
	}
}

// shipWaitLapsed drops the session's registrations that ran out and, if any
// did, delivers what they kept queued the normal way, so nothing is stranded.
func (s *Service) shipWaitLapsed(ctx context.Context, session int64) {
	w := &s.shipWaits
	w.mu.Lock()
	lapsed := false
	now := s.now()
	for task, e := range w.by[session] {
		if e.open == 0 && !now.Before(e.until) {
			delete(w.by[session], task)
			lapsed = true
		}
	}
	if len(w.by[session]) == 0 {
		delete(w.by, session)
	}
	w.mu.Unlock()
	if lapsed {
		s.releaseBatch(ctx, session)
	}
}

func (w *stopWaits) wakeShip(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.ship[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (w *stopWaits) addShip(id int64) chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ship == nil {
		w.ship = map[int64][]chan struct{}{}
	}
	ch := make(chan struct{}, 1)
	w.ship[id] = append(w.ship[id], ch)
	return ch
}

func (w *stopWaits) removeShip(id int64, ch chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := w.ship[id]
	for n, c := range list {
		if c == ch {
			list = append(list[:n], list[n+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(w.ship, id)
	} else {
		w.ship[id] = list
	}
}

func (s *Service) logShipNews(ctx context.Context, session, task int64, got *ShipNews) {
	outcome := "error"
	if got != nil {
		outcome = got.Outcome
	}
	s.log.InfoContext(context.WithoutCancel(ctx), "ship news returned", "session", session, "task", task, "outcome", outcome)
}

// shipWaitTarget resolves the session and task of a wait and checks the actor may.
func (s *Service) shipWaitTarget(ctx context.Context, actor *Actor, cliID, machine, taskKey string) (session, task int64, err error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return 0, 0, ErrUnauthorized
	}
	session = s.SessionForHeader(ctx, actor.id(), cliID, machine)
	if session == 0 {
		return 0, 0, notFound("session")
	}
	if _, err := s.sessionByID(ctx, s.db.R, actor, session); err != nil {
		return 0, 0, err
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, taskKey, RoleViewer, false)
	if err != nil {
		return 0, 0, err
	}
	return session, i.ID, nil
}

// AwaitShipNews holds a wp ship that was held for its task's review round
// (up to wait, at most prWaitMax) until the round has something for the
// agent: its audit or check news (delivered once the client acks it:
// AckShipNews), the pull request Waypoint shipped, or the round's end. Neither in time: waiting.
func (s *Service) AwaitShipNews(ctx context.Context, actor *Actor, cliID, machine, taskKey string, wait time.Duration) (*ShipNews, error) {
	session, task, err := s.shipWaitTarget(ctx, actor, cliID, machine, taskKey)
	if err != nil {
		return nil, err
	}
	wait = min(max(wait, 0), shipNewsMax)
	defer s.openShipWait(session, task)()
	ch := s.stopWaits.addShip(session)
	defer s.stopWaits.removeShip(session, ch)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	recheck := time.NewTicker(shipNewsRecheck)
	defer recheck.Stop()
	for {
		if got, err := s.shipNewsNow(ctx, session, task); err != nil || got != nil {
			s.logShipNews(ctx, session, task, got)
			return got, err
		}
		select {
		case <-ch:
		case <-recheck.C:
		case <-deadline.C:
			s.logShipNews(ctx, session, task, &ShipNews{Outcome: ShipNewsWaiting})
			return &ShipNews{Outcome: ShipNewsWaiting}, nil
		case <-ctx.Done():
			s.logShipNews(ctx, session, task, &ShipNews{Outcome: ShipNewsWaiting})
			return &ShipNews{Outcome: ShipNewsWaiting}, nil
		}
	}
}

// shipNewsNow is the answer if there is one: the round's news first (read,
// not taken: AckShipNews delivers it once the client has it), then what the
// round's state says.
func (s *Service) shipNewsNow(ctx context.Context, session, task int64) (*ShipNews, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+messageCols+` FROM session_messages m LEFT JOIN users u ON u.id = m.created_by
		WHERE m.session_id = ? AND m.state = 'queued' AND (m.step_key LIKE 'audit:%' OR m.step_key LIKE 'check:%') ORDER BY m.id`, session)
	if err != nil {
		return nil, err
	}
	var mine []*SessionMessage
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		mine = append(mine, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var found []*SessionMessage
	var ids []int64
	for _, m := range mine {
		if n, bound := messageTask(ctx, s.db.R, m.stepKey); bound && n == task {
			found = append(found, m)
			ids = append(ids, m.ID)
		}
	}
	if len(found) > 0 {
		return &ShipNews{Outcome: ShipNewsFindings, Text: MessagesText(found), IDs: ids}, nil
	}
	r, err := reviewRound(ctx, s.db.R, session, task)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return &ShipNews{Outcome: ShipNewsNone}, nil
	}
	bp := roundBeforePR(ctx, s.db.R, r.id)
	if bp == nil {
		return &ShipNews{Outcome: ShipNewsNone}, nil
	}
	if bp.ShipDone {
		p := &ShipPull{Repo: bp.Repo}
		err := s.db.R.QueryRowContext(ctx, `SELECT number, url, state FROM pull_requests WHERE repo = ? COLLATE NOCASE AND head_ref = ? AND (head_repo = '' OR head_repo = repo COLLATE NOCASE) ORDER BY id DESC LIMIT 1`, bp.Repo, bp.Branch).
			Scan(&p.Number, &p.URL, &p.State)
		switch {
		case err == nil:
			if p.State == "open" || p.State == "draft" {
				return &ShipNews{Outcome: ShipNewsPull, Pull: p, Text: fmt.Sprintf("The review ended and Waypoint shipped the pull request: PR %d (%s): %s", p.Number, p.State, p.URL)}, nil
			}
			return &ShipNews{Outcome: ShipNewsEnded, Text: fmt.Sprintf("Waypoint's review round for this task is over, and its pull request, PR %d, is %s. Nothing more is waiting on you here.", p.Number, p.State)}, nil
		case errors.Is(err, sql.ErrNoRows):
			return &ShipNews{Outcome: ShipNewsEnded, Text: fmt.Sprintf("Waypoint's review round for this task is over and it shipped the round, but no pull request from %s in %s is recorded. Nothing more is waiting on you here.", bp.Branch, bp.Repo)}, nil
		default:
			return nil, err
		}
	}
	in, err := s.roundInOf(ctx, r.id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return &ShipNews{Outcome: ShipNewsNone}, nil
		}
		return nil, err
	}
	ended := func(why string) *ShipNews {
		return &ShipNews{Outcome: ShipNewsEnded, Text: fmt.Sprintf("Waypoint's review round for this task is over (%s)%s. Nothing more is waiting on you here.", in.State, colonIf(why))}
	}
	if bp.ShipStop != "" {
		return ended(bp.ShipStop), nil
	}
	switch out := decideRound(in); {
	case out.Do == doSettle || out.Do == doHold:
		return ended(out.Why), nil
	case out.Do == doNothing:
		return ended(out.Settled), nil
	case out.Do == doEnd && out.Told != "":
		return ended(out.Told), nil
	}
	return nil, nil
}

// AckShipNews says the client has printed the findings AwaitShipNews returned:
// they are delivered (via 'ship') so nothing types them again. Only what a
// ship's wait returns can be acked (the task's queued audit and check news,
// and the session's failed-check news); any other id is left as it is.
func (s *Service) AckShipNews(ctx context.Context, actor *Actor, cliID, machine, taskKey string, ids []int64) error {
	session, task, err := s.shipWaitTarget(ctx, actor, cliID, machine, taskKey)
	if err != nil {
		return err
	}
	defer s.openShipWait(session, task)()
	delivered := false
	err = s.write(ctx, actor, func(t *tx) error {
		x, err := scanSession(t.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, session), s.nowMS())
		if err != nil {
			return err
		}
		now := s.nowMS()
		for _, id := range ids {
			var key string
			if t.QueryRowContext(ctx, `SELECT step_key FROM session_messages WHERE id = ? AND session_id = ? AND state = 'queued'`, id, session).Scan(&key) != nil {
				continue
			}
			if !shipNewsKey(key) {
				continue
			}
			if !strings.HasPrefix(key, "ci:") {
				// A ci: key (AwaitShipChecks) is bound by this session and its
				// exact step key, not by task: a pull request may link to
				// several.
				if of, bound := messageTask(ctx, t, key); !bound || of != task {
					continue
				}
			}
			res, err := t.ExecContext(ctx, `UPDATE session_messages SET state = 'delivered', via = 'ship', delivered_at = ?, batch_until = NULL WHERE id = ? AND session_id = ? AND state = 'queued'`, now, id, session)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			delivered = true
			s.log.InfoContext(ctx, "session message delivered to a waiting ship", "message", id, "session", session, "step_key", key)
			t.emit(sessionEvent(EvSessionMessage, x, map[string]any{"message": id, "via": "ship"}))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if delivered {
		s.publishSession(ctx, session)
	}
	return nil
}

// shipNewsKey: the step key is news a wp ship's wait delivers: an audit's or
// the check of a session's marks (the review's wait), or the failed checks of
// its pull request (the checks' wait).
func shipNewsKey(key string) bool {
	return strings.HasPrefix(key, "audit:") || strings.HasPrefix(key, "check:") || strings.HasPrefix(key, "ci:")
}

func colonIf(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return ": " + strings.TrimSpace(s)
}
