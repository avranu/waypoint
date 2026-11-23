package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A wp ship held for a review round waits on the server for the round's next
// step and prints it, instead of ending the agent's turn so that each step
// costs a turn end, a reply and a typed prompt (WP-1669). AwaitShipNews is
// that wait. While one is open the round's audit and check news stays queued
// for it; when the wait goes away without taking it (the CLI was killed), the
// news is delivered the usual way.

// What a wait returns (ShipNews.Outcome).
const (
	ShipNewsFindings = "findings" // the round's news, taken: Text
	ShipNewsPull     = "pull"     // Waypoint opened the pull request: Pull
	ShipNewsEnded    = "ended"    // the round ended or was released: Text says why
	ShipNewsWaiting  = "waiting"  // nothing yet: ask again
	ShipNewsNone     = "none"     // the session has no round for the task
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
}

// shipWaits is the registry of waits, by session and task.
type shipWaits struct {
	mu sync.Mutex
	by map[int64]map[int64]*shipWaitEntry
}

type shipWaitEntry struct {
	open  int
	until time.Time // registered until, once the last call returned
}

// holds: a wp ship waits for m (audit or check news of its task).
func (w *shipWaits) holds(ctx context.Context, q queryer, now time.Time, session int64, m *SessionMessage) bool {
	w.mu.Lock()
	n := len(w.by[session])
	w.mu.Unlock()
	if n == 0 || (!strings.HasPrefix(m.stepKey, "audit:") && !strings.HasPrefix(m.stepKey, "check:")) {
		return false
	}
	task, bound := messageTask(ctx, q, m.stepKey)
	if !bound || task == 0 {
		return false
	}
	return w.held(session, task, now)
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
	w.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			e.open--
			e.until = s.now().Add(shipWaitLinger)
			w.mu.Unlock()
			time.AfterFunc(shipWaitLinger+time.Second, func() {
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

// AwaitShipNews holds a wp ship that was held for its task's review round
// (up to wait, at most prWaitMax) until the round has something for the
// agent: its audit or check news (taken, so nothing types it again), the pull
// request Waypoint opened, or the round's end. Neither in time: waiting.
func (s *Service) AwaitShipNews(ctx context.Context, actor *Actor, cliID, machine, taskKey string, wait time.Duration) (*ShipNews, error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return nil, ErrUnauthorized
	}
	session := s.SessionForHeader(ctx, actor.id(), cliID, machine)
	if session == 0 {
		return nil, notFound("session")
	}
	task, err := s.resolveIssueID(ctx, s.db.R, taskKey)
	if err != nil {
		return nil, err
	}
	// Reading the task is the access check (the session is the actor's: SessionForHeader).
	if _, err := s.sessionByID(ctx, s.db.R, actor, session); err != nil {
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
		if got, err := s.shipNewsNow(ctx, actor, session, task); err != nil || got != nil {
			return got, err
		}
		select {
		case <-ch:
		case <-recheck.C:
		case <-deadline.C:
			return &ShipNews{Outcome: ShipNewsWaiting}, nil
		case <-ctx.Done():
			return &ShipNews{Outcome: ShipNewsWaiting}, nil
		}
	}
}

// shipNewsNow is the answer if there is one: the round's news first, then
// what the round's state says.
func (s *Service) shipNewsNow(ctx context.Context, actor *Actor, session, task int64) (*ShipNews, error) {
	var taken []*SessionMessage
	err := s.write(ctx, actor, func(t *tx) error {
		x, err := scanSession(t.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, session), s.nowMS())
		if err != nil {
			return err
		}
		rows, err := t.QueryContext(ctx, `SELECT `+messageCols+` FROM session_messages m LEFT JOIN users u ON u.id = m.created_by
			WHERE m.session_id = ? AND m.state = 'queued' AND (m.step_key LIKE 'audit:%' OR m.step_key LIKE 'check:%') ORDER BY m.id`, session)
		if err != nil {
			return err
		}
		var mine []*SessionMessage
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				rows.Close()
				return err
			}
			if n, bound := messageTask(ctx, t, m.stepKey); bound && n == task {
				mine = append(mine, m)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		now := s.nowMS()
		for _, m := range mine {
			res, err := t.ExecContext(ctx, `UPDATE session_messages SET state = 'delivered', via = 'ship', delivered_at = ?, batch_until = NULL WHERE id = ? AND state = 'queued'`, now, m.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue // another path took it
			}
			m.State, m.Via = MessageDelivered, "ship"
			m.deliveredAt = sql.NullInt64{Int64: now, Valid: true}
			s.log.InfoContext(ctx, "session message delivered to a waiting ship", "message", m.ID, "session", session, "step_key", m.stepKey)
			t.emit(sessionEvent(EvSessionMessage, x, map[string]any{"message": m.ID, "via": "ship"}))
			taken = append(taken, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(taken) > 0 {
		s.publishSession(ctx, session)
		return &ShipNews{Outcome: ShipNewsFindings, Text: MessagesText(taken)}, nil
	}
	r, err := reviewRound(ctx, s.db.R, session, task)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return &ShipNews{Outcome: ShipNewsNone}, nil
	}
	in, err := s.roundInOf(ctx, r.id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return &ShipNews{Outcome: ShipNewsNone}, nil
		}
		return nil, err
	}
	if in.PROpen {
		if bp := roundBeforePR(ctx, s.db.R, r.id); bp != nil {
			p := &ShipPull{Repo: bp.Repo}
			err := s.db.R.QueryRowContext(ctx, `SELECT number, url, state FROM pull_requests WHERE repo = ? COLLATE NOCASE AND head_ref = ? AND state IN ('open', 'draft') ORDER BY id DESC LIMIT 1`, bp.Repo, bp.Branch).
				Scan(&p.Number, &p.URL, &p.State)
			if err == nil {
				return &ShipNews{Outcome: ShipNewsPull, Pull: p}, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
	}
	switch in.State {
	case RoundSkipped, RoundInterrupted, RoundFailed:
		why := in.Release
		if out := decideRound(in); why == "" {
			why = cmp(out.Told, out.Why)
		}
		return &ShipNews{Outcome: ShipNewsEnded, Text: fmt.Sprintf("The review round for this task ended (%s)%s. Nothing more is waiting on you here: Waypoint opens the pull request without the review where it can and tells the person.", in.State, colonIf(why))}, nil
	}
	return nil, nil
}

func colonIf(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return ": " + strings.TrimSpace(s)
}
