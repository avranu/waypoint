package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// HoldWhen is what a hold waits for. References are stored by id, checked
// against what the placer may see when the hold is placed.
type HoldWhen struct {
	Tasks    []HoldRef `json:"tasks,omitempty"`
	Requests []HoldRef `json:"requests,omitempty"`
	Reviewed bool      `json:"reviewed,omitempty"`
	Person   bool      `json:"person,omitempty"`
}

// HoldRef names a task or request a hold waits for.
type HoldRef struct {
	ID  int64  `json:"id"`
	Ref string `json:"ref"`
}

func (w *HoldWhen) empty() bool {
	return w == nil || (len(w.Tasks) == 0 && len(w.Requests) == 0 && !w.Reviewed && !w.Person)
}

// text says what it waits for: "WP-1 is done and R-146 is answered".
func (w *HoldWhen) text() string {
	if w.empty() {
		return ""
	}
	var parts []string
	for _, r := range w.Tasks {
		parts = append(parts, r.Ref+" is done")
	}
	for _, r := range w.Requests {
		parts = append(parts, r.Ref+" is answered")
	}
	if w.Reviewed {
		parts = append(parts, "a person reviews it")
	}
	if w.Person {
		parts = append(parts, "a person lifts it")
	}
	return strings.Join(parts, " and ")
}

// parseHoldWhen reads the stored column; nil when there is nothing in it.
func parseHoldWhen(raw string) *HoldWhen {
	if raw == "" {
		return nil
	}
	w := &HoldWhen{}
	if err := json.Unmarshal([]byte(raw), w); err != nil || w.empty() {
		return nil
	}
	return w
}

// conditional: the input says what to wait for besides a date.
func (in HoldInput) conditional() bool {
	return len(in.After) > 0 || len(in.Requests) > 0 || in.Reviewed || in.Person
}

// holdWhenFor resolves the conditions of a hold on i, inside its write.
func (s *Service) holdWhenFor(ctx context.Context, t *tx, actor *Actor, i *Issue, in HoldInput) (*HoldWhen, error) {
	if !in.conditional() {
		return nil, nil
	}
	w := &HoldWhen{Reviewed: in.Reviewed, Person: in.Person}
	seen := map[int64]bool{}
	for _, ref := range splitRefs(in.After) {
		o, _, err := s.getIssueFor(ctx, t, actor, ref, RoleViewer, false)
		if err != nil {
			return nil, err
		}
		if o.ID == i.ID {
			return nil, invalid("%s can't wait for itself", i.Key)
		}
		switch loop, err := holdLoop(ctx, t, o.ID, i.ID); {
		case err != nil:
			return nil, err
		case loop:
			return nil, invalid("%s already waits for %s, so neither could ever come back; wait for something else or lift that hold first", o.Key, i.Key)
		}
		if !seen[o.ID] {
			seen[o.ID] = true
			w.Tasks = append(w.Tasks, HoldRef{o.ID, o.Key})
		}
	}
	seen = map[int64]bool{}
	for _, ref := range splitRefs(in.Requests) {
		id, err := s.resolveRequestID(ctx, t, actor, ref)
		if err != nil {
			return nil, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		var key sql.NullString
		var number int
		if err := t.QueryRowContext(ctx, `SELECT p.key, r.number FROM requests r LEFT JOIN projects p ON p.id = r.project_id WHERE r.id = ?`, id).Scan(&key, &number); err != nil {
			return nil, err
		}
		w.Requests = append(w.Requests, HoldRef{id, requestRef(key.String, number)})
	}
	if w.Reviewed {
		waiting, err := reviewWaiting(ctx, t, i.ID)
		if err != nil {
			return nil, err
		}
		if !waiting {
			w.Reviewed, w.Person = false, true
		}
	}
	return w, nil
}

// holdLoop: the hold on from, or on a task it waits for, waits for target.
func holdLoop(ctx context.Context, q queryer, from, target int64) (bool, error) {
	seen := map[int64]bool{}
	for next := []int64{from}; len(next) > 0; {
		id := next[len(next)-1]
		next = next[:len(next)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		var raw string
		err := q.QueryRowContext(ctx, `SELECT hold_when FROM issues WHERE id = ? AND hold_at IS NOT NULL`, id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		if w := parseHoldWhen(raw); w != nil {
			for _, r := range w.Tasks {
				if r.ID == target {
					return true, nil
				}
				next = append(next, r.ID)
			}
		}
	}
	return false, nil
}

// splitRefs flattens references given as lists, "A,B" or "A B".
func splitRefs(refs []string) []string {
	var out []string
	for _, r := range refs {
		out = append(out, strings.FieldsFunc(r, func(c rune) bool { return c == ',' || c == ' ' })...)
	}
	return out
}

// reviewWaiting: an agent wrote something on the task a person hasn't reviewed.
func reviewWaiting(ctx context.Context, q queryer, issueID int64) (bool, error) {
	var waiting bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE id = ? AND text_agent != '' AND reviewed_at IS NULL)
		OR EXISTS(SELECT 1 FROM comments WHERE issue_id = ? AND agent != '' AND reviewed_at IS NULL)`, issueID, issueID).Scan(&waiting)
	return waiting, err
}

// CheckHoldConditions lifts the holds whose conditions are met. A hold only
// a person can lift has nothing to check.
func (s *Service) CheckHoldConditions(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, hold_at, hold_until, hold_when FROM issues WHERE hold_when != '' AND hold_at IS NOT NULL AND coalesce(json_extract(hold_when, '$.person'), 0) = 0 ORDER BY id`)
	if err != nil {
		s.log.WarnContext(ctx, "list hold conditions", "err", err)
		return
	}
	type held struct {
		id, at int64
		until  sql.NullInt64
		raw    string
	}
	var list []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.at, &h.until, &h.raw); err != nil {
			_ = rows.Close()
			s.log.WarnContext(ctx, "read a hold condition", "err", err)
			return
		}
		list = append(list, h)
	}
	_ = rows.Close()
	now := s.nowMS()
	for _, h := range list {
		w := parseHoldWhen(h.raw)
		if w == nil {
			continue
		}
		switch changed, err := s.personForExpired(ctx, h.id, h.at, h.raw, w); {
		case err != nil:
			s.log.WarnContext(ctx, "hand an expired wait to a person", "issue", h.id, "err", err)
			continue
		case changed:
			continue
		}
		if h.until.Valid && h.until.Int64 > now {
			continue
		}
		met, why, err := s.holdMet(ctx, h.id, h.at, w)
		if err != nil {
			s.log.WarnContext(ctx, "check a hold condition", "issue", h.id, "err", err)
			continue
		}
		if !met {
			continue
		}
		if err := s.liftMetHold(ctx, h.id, h.at, h.raw, why); err != nil {
			s.log.WarnContext(ctx, "lift a hold", "issue", h.id, "err", err)
		}
	}
}

// holdMet says whether every condition is met, and how they were.
func (s *Service) holdMet(ctx context.Context, issueID, holdAt int64, w *HoldWhen) (bool, string, error) {
	var why []string
	for _, r := range w.Tasks {
		var open bool
		err := s.db.R.QueryRowContext(ctx, `SELECT st.category NOT IN ('done','canceled') FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.id = ?`, r.ID).Scan(&open)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			why = append(why, r.Ref+" is gone")
			continue
		case err != nil:
			return false, "", err
		case open:
			return false, "", nil
		}
		why = append(why, r.Ref+" closed")
	}
	for _, r := range w.Requests {
		var state string
		err := s.db.R.QueryRowContext(ctx, `SELECT state FROM requests WHERE id = ?`, r.ID).Scan(&state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			why = append(why, r.Ref+" is gone")
			continue
		case err != nil:
			return false, "", err
		case state == RequestOpen, state == RequestApplying, state == RequestExpired:
			return false, "", nil
		}
		why = append(why, r.Ref+" is "+state)
	}
	if w.Reviewed {
		var reviewed bool
		if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE id = ? AND (coalesce(reviewed_at, 0) >= ? OR coalesce(close_reviewed_at, 0) >= ?))
			OR EXISTS(SELECT 1 FROM comments WHERE issue_id = ? AND coalesce(reviewed_at, 0) >= ?)`, issueID, holdAt, holdAt, issueID, holdAt).Scan(&reviewed); err != nil {
			return false, "", err
		}
		if !reviewed {
			return false, "", nil
		}
		why = append(why, "a person reviewed it")
	}
	return true, strings.Join(why, ", "), nil
}

// personForExpired: a request the hold waits for expired, which hook and
// automatic requests do when the session moves on, so no answer is coming.
// As with --reviewed when nothing awaits review, a person lifts it instead.
// True when it changed the hold.
func (s *Service) personForExpired(ctx context.Context, issueID, holdAt int64, raw string, w *HoldWhen) (bool, error) {
	var keep []HoldRef
	var expired []string
	for _, r := range w.Requests {
		var state string
		err := s.db.R.QueryRowContext(ctx, `SELECT state FROM requests WHERE id = ?`, r.ID).Scan(&state)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if state == RequestExpired {
			expired = append(expired, r.Ref)
		} else {
			keep = append(keep, r)
		}
	}
	if len(expired) == 0 {
		return false, nil
	}
	next := *w
	next.Requests, next.Person = keep, true
	b, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	return true, s.write(ctx, SystemActor(), func(t *tx) error {
		i, err := s.issueByID(ctx, t, issueID, false)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		note := strings.Join(expired, ", ") + " expired unanswered"
		if i.Hold != nil && i.Hold.Note != "" {
			note = i.Hold.Note + "; " + note
		}
		res, err := t.ExecContext(ctx, `UPDATE issues SET hold_when = ?, hold_person = 1, hold_note = ?, updated_at = ? WHERE id = ? AND hold_at = ? AND hold_when = ?`, string(b), note, s.nowMS(), issueID, holdAt, raw)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 || i.Hold == nil {
			return nil
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "hold",
			Old: holdSpan(i.Hold, time.UTC), New: holdSpan(&Hold{Until: i.Hold.Until, When: &next, Note: note}, time.UTC)})
		return nil
	})
}

// liftMetHold takes the hold off unless it changed since it was read. Every
// change to a hold sets hold_at, so a new date with the same conditions counts.
func (s *Service) liftMetHold(ctx context.Context, issueID, holdAt int64, raw, why string) error {
	return s.write(ctx, SystemActor(), func(t *tx) error {
		i, err := s.issueByID(ctx, t, issueID, false)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		res, err := t.ExecContext(ctx, `UPDATE issues SET `+holdCleared+`, updated_at = ? WHERE id = ? AND hold_at = ? AND hold_when = ?`, s.nowMS(), issueID, holdAt, raw)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "hold",
			Old: holdSpan(i.Hold, time.UTC), New: "lifted: " + why})
		return nil
	})
}
