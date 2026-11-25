package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Hold is a task's hold.
type Hold struct {
	At time.Time `json:"at"`
	// Until is when it ends by itself; nil: when someone lifts it.
	Until *time.Time `json:"until,omitempty"`
	// When is what it waits for besides that date (WP-ADR75); Waits says it in words.
	When  *HoldWhen `json:"-"`
	Waits string    `json:"waits,omitempty"`
	Note  string    `json:"note,omitempty"`
	// By is the person who placed it (an agent's person, when Agent is set).
	By       string `json:"by,omitempty"`
	Agent    string `json:"agent,omitempty"`
	byPerson bool
}

// on reports whether the hold still holds at now.
func (h *Hold) on(now time.Time) bool {
	return h != nil && (h.When != nil || h.Until == nil || h.Until.After(now))
}

// HoldInput puts a task on hold. Until is a date (2026-11-01, friday, +3d:
// the start of that day where the actor is), an RFC 3339 time, or empty
// (or "indefinitely") for a hold someone lifts.
//
// After, Requests, Reviewed and Person say what else it waits for
// (WP-ADR75): tasks to close (keys), requests to be settled (R-12,
// KEY:R-12), a person's review of it, a person lifting it. All of them
// must be met. KeepWaits leaves what the hold waits for as it is, for an
// edit of only its date and note.
type HoldInput struct {
	Until    string   `json:"until"`
	Note     string   `json:"note"`
	After    []string `json:"after,omitempty"`
	Requests []string `json:"requests,omitempty"`
	Reviewed bool     `json:"reviewed,omitempty"`
	Person   bool     `json:"person,omitempty"`

	KeepWaits bool `json:"keep_waits,omitempty"`
}

// holdUntil is when a hold given as until ends, in ms; nil for one
// someone lifts.
func (s *Service) holdUntil(actor *Actor, until string) (*int64, error) {
	v := strings.TrimSpace(until)
	switch strings.ToLower(v) {
	case "", "indefinitely", "indefinite", "forever", "lifted":
		return nil, nil
	}
	now := s.now()
	var end time.Time
	if t, err := time.Parse(time.RFC3339, strings.ToUpper(v)); err == nil {
		end = t
	} else {
		day, err := s.parseDateFor(actor, v)
		if err != nil || day == nil {
			return nil, invalid("can't understand the hold's end %q (try 2026-11-01, friday, +3d, 2026-11-01T09:00:00Z, or indefinitely)", v)
		}
		if end, err = time.ParseInLocation(dateLayout, *day, s.actorLocation(actor)); err != nil {
			return nil, err
		}
	}
	if !end.After(now) {
		return nil, invalid("%s has already passed; a hold ends in the future", v)
	}
	ms := end.UnixMilli()
	return &ms, nil
}

// holdRefusal is why no one may take a task on hold, or nil.
func (s *Service) holdRefusal(actor *Actor, i *Issue) error {
	h := i.Hold
	if !h.on(s.now()) {
		return nil
	}
	return invalid("%s is on hold %s; it can't be taken until then (wp hold %s --off lifts it)", i.Key, holdSpan(h, s.actorLocation(actor)), i.Key)
}

// holdSpan says how long a hold lasts, with its note: "until 2026-11-01
// (the gateway's quota refreshes)".
func holdSpan(h *Hold, loc *time.Location) string {
	span := holdWaits(h, loc)
	if h.Note != "" {
		span += " (" + h.Note + ")"
	}
	return span
}

// holdWaits is holdSpan without the note: "until 2026-11-01 and WP-1 is done".
func holdWaits(h *Hold, loc *time.Location) string {
	var parts []string
	if h.Until != nil {
		u := h.Until.In(loc)
		if u.Hour() == 0 && u.Minute() == 0 {
			parts = append(parts, u.Format(dateLayout))
		} else {
			parts = append(parts, u.Format("2006-01-02 15:04 MST"))
		}
	}
	if w := h.When.text(); w != "" {
		parts = append(parts, w)
	}
	if len(parts) == 0 {
		return "until someone lifts it"
	}
	return "until " + strings.Join(parts, " and ")
}

// SetHold puts a task on hold, or changes the hold it is on. An agent
// working on it lets it go. A hold a person placed is theirs: agents
// (and anything run in their sessions) can't change it.
func (s *Service) SetHold(ctx context.Context, actor *Actor, ref string, in HoldInput) (*IssueDetail, error) {
	until, err := s.holdUntil(actor, in.Until)
	if err != nil {
		return nil, err
	}
	var key string
	err = s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if i.Closed() {
			return invalid("%s is already %s; there is nothing to hold", i.Key, i.Status.Name)
		}
		if err := s.personsHold(actor, i); err != nil {
			return err
		}
		when, err := s.holdWhenFor(ctx, t, actor, i, in)
		if err != nil {
			return err
		}
		if in.KeepWaits && when == nil && i.Hold.on(s.now()) {
			when = i.Hold.When
		}
		key = i.Key
		return s.placeHold(ctx, t, actor, i, until, when, in.Note)
	})
	if err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, actor, key)
}

// holdCleared is the SET clause that takes a task off hold.
const holdCleared = `hold_at = NULL, hold_until = NULL, hold_note = '', hold_by = NULL, hold_agent = '', hold_person = 0, hold_when = ''`

// LiftHold takes a task off hold, so agents can take it again.
func (s *Service) LiftHold(ctx context.Context, actor *Actor, ref string) (*IssueDetail, error) {
	var key string
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if !i.Hold.on(s.now()) {
			if !actor.HidesDrafts() {
				if ended, err := s.endReleaseWait(ctx, t, i.ID); err != nil || ended {
					key = i.Key
					return err
				}
			}
			return invalid("%s isn't on hold", i.Key)
		}
		if err := s.personsHold(actor, i); err != nil {
			return err
		}
		key = i.Key
		if _, err := t.ExecContext(ctx, `UPDATE issues SET `+holdCleared+`, updated_at = ? WHERE id = ?`, s.nowMS(), i.ID); err != nil {
			return err
		}
		if !actor.HidesDrafts() {
			if _, err := s.endReleaseWait(ctx, t, i.ID); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "hold",
			Old: holdSpan(i.Hold, time.UTC), New: "lifted"})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, actor, key)
}

// personsHold refuses an agent changing a hold a person placed.
func (s *Service) personsHold(actor *Actor, i *Issue) error {
	if i.Hold.on(s.now()) && i.Hold.byPerson && actor.HidesDrafts() {
		if i.Hold.When != nil && i.Hold.When.Person {
			return forbidden("%s waits for a person; only a person can change or lift that hold", i.Key)
		}
		return forbidden("%s was put on hold by %s; only a person can change or lift that hold", i.Key, cmp(i.Hold.By, "a person"))
	}
	return nil
}

// placeHold records the hold, inside a write, and ends any agent's claim
// on the task, lapsed or not: it isn't anyone's to work on until the hold
// ends. A claim ended that way counts as given back, so autopilot moves on
// (WP-ADR75).
func (s *Service) placeHold(ctx context.Context, t *tx, actor *Actor, i *Issue, until *int64, when *HoldWhen, note string) error {
	now := s.nowMS()
	note = strings.TrimSpace(note)
	raw := ""
	if !when.empty() {
		b, err := json.Marshal(when)
		if err != nil {
			return err
		}
		raw = string(b)
	} else {
		when = nil
	}
	byPerson := !actor.HidesDrafts() || (when != nil && when.Person)
	if _, err := t.ExecContext(ctx, `UPDATE issues SET hold_at = ?, hold_until = ?, hold_note = ?, hold_by = ?, hold_agent = ?, hold_person = ?, hold_when = ?,
		updated_at = ? WHERE id = ?`, now, nullInt(until), note, nullUser(actor.id()), actor.agentName(), byPerson, raw, now, i.ID); err != nil {
		return err
	}
	h := &Hold{Note: note, When: when}
	if until != nil {
		u := msTime(*until)
		h.Until = &u
	}
	t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "hold", New: holdSpan(h, time.UTC)})
	var agent string
	switch err := t.QueryRowContext(ctx, `SELECT claimed_by FROM issues WHERE id = ? AND claimed_by != ''`, i.ID).Scan(&agent); {
	case err == sql.ErrNoRows:
		return nil
	case err != nil:
		return err
	}
	if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ?`, i.ID); err != nil {
		return err
	}
	if err := s.endRun(ctx, t, i.ID, agent, "released", nil); err != nil {
		return err
	}
	return s.recordRelease(ctx, t, i.ID, agent, nil)
}
