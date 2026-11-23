package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Test slot holds (WP-1646): each wp that runs tests in a per-machine slot
// sends what every completed hold looked like (the class it was given and
// why, who held and awaited the slots when it queued, how long it waited and
// held), so the slot policy (WP-ADR150) can be checked and simulated.

const (
	// SlotHoldsMax is the most holds one request may carry.
	SlotHoldsMax = 500
	// SlotHoldsReadMax is the most holds one read returns (and its default).
	SlotHoldsReadMax = 20000
	slotListMax      = 64
	slotDurMaxMS     = int64(30 * 24 * time.Hour / time.Millisecond)
)

// EvSlotHoldsRecorded: a machine reported its slot holds. It goes live to
// the owner only and is quiet: not recorded, and it sets nothing off.
const EvSlotHoldsRecorded = "slot.holds_recorded"

// SlotHolder is a slot that was held when a run queued.
type SlotHolder struct {
	Slot  int   `json:"slot"`
	PID   int   `json:"pid"`
	Since int64 `json:"since"` // unix seconds
}

// SlotWaiter is a run that was already waiting when another queued.
type SlotWaiter struct {
	PID       int    `json:"pid"`
	Since     int64  `json:"since"`
	Class     string `json:"class"`
	BehindOwn bool   `json:"behind_own,omitempty"`
}

// SlotHold is one completed hold of a test slot, or one wait that gave up.
type SlotHold struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Repo          string       `json:"repo,omitempty"`
	Tree          string       `json:"tree,omitempty"`
	Cmd           string       `json:"cmd,omitempty"`
	Class         string       `json:"class"`
	MedianSecs    *float64     `json:"median_secs,omitempty"`
	ThresholdSecs int64        `json:"threshold_secs"`
	Runs          int          `json:"runs"`
	Policy        string       `json:"policy,omitempty"`
	Slots         int          `json:"slots"`
	LongSlots     int          `json:"long_slots"`
	Slot          *int         `json:"slot,omitempty"`
	QueuedAt      int64        `json:"queued_at"` // ms
	WaitMS        int64        `json:"wait_ms"`
	TreeWaitMS    int64        `json:"tree_wait_ms,omitempty"`
	HoldMS        int64        `json:"hold_ms"`
	GaveUp        bool         `json:"gave_up,omitempty"`
	Exit          *int         `json:"exit,omitempty"`
	Holders       []SlotHolder `json:"holders"`
	Waiters       []SlotWaiter `json:"waiters"`
	// Set on reads only.
	User    string `json:"user,omitempty"`
	Machine string `json:"machine,omitempty"`
}

// SlotHoldsIn is a batch of holds from one machine, with what the machine
// says of its outbox. The counts come apart from the batch so a machine whose
// batches are refused still shows.
type SlotHoldsIn struct {
	Machine string
	Pending int64 // holds in the outbox, this batch included
	Oldest  int64 // queued_at (ms) of the oldest of them
	Dropped int64 // the machine's total dropped from a full outbox
	Holds   []SlotHold
}

// SlotHoldsResult counts what became of a batch.
type SlotHoldsResult struct {
	Stored    int `json:"stored"`
	Duplicate int `json:"duplicate"`
	Rejected  int `json:"rejected"`
}

// slotWriter is who may send holds: a person's account, through a token that
// may write. A runner job's or scoped token counts too, as in usage ingest:
// the holds are the machine's, not a task's, and a job runs tests in slots.
func slotWriter(actor *Actor) (int64, error) {
	uid, err := usageUser(actor)
	if err != nil {
		return 0, err
	}
	if err := actor.canWrite(); err != nil {
		return 0, err
	}
	return uid, nil
}

// slotHoldRow validates h and returns its cleaned form (ok false: refused).
func slotHoldRow(h SlotHold, now int64) (SlotHold, []byte, []byte, bool) {
	if utf8.RuneCountInString(strings.TrimSpace(h.ID)) > 64 {
		return h, nil, nil, false // cutting an id could merge two holds into one
	}
	h.ID, h.Name = clip(h.ID, 64), clip(h.Name, 100)
	h.Repo, h.Tree, h.Cmd = clip(h.Repo, 1024), clip(h.Tree, 1024), clip(h.Cmd, 4000)
	h.Policy = clip(h.Policy, 60)
	switch {
	case h.ID == "", h.Name == "", h.Class != "short" && h.Class != "long":
		return h, nil, nil, false
	case h.QueuedAt <= 0, h.QueuedAt > now+int64(24*time.Hour/time.Millisecond):
		return h, nil, nil, false
	case h.WaitMS < 0, h.TreeWaitMS < 0, h.HoldMS < 0, h.ThresholdSecs < 0, h.Runs < 0, h.Slots < 0, h.LongSlots < 0:
		return h, nil, nil, false
	case h.WaitMS > slotDurMaxMS, h.TreeWaitMS > slotDurMaxMS, h.HoldMS > slotDurMaxMS:
		return h, nil, nil, false
	case h.Slot != nil && *h.Slot < 0, h.MedianSecs != nil && *h.MedianSecs < 0:
		return h, nil, nil, false
	case len(h.Holders) > slotListMax, len(h.Waiters) > slotListMax:
		return h, nil, nil, false
	}
	for i := range h.Waiters {
		h.Waiters[i].Class = clip(h.Waiters[i].Class, 10)
	}
	if h.Holders == nil {
		h.Holders = []SlotHolder{}
	}
	if h.Waiters == nil {
		h.Waiters = []SlotWaiter{}
	}
	hb, err1 := json.Marshal(h.Holders)
	wb, err2 := json.Marshal(h.Waiters)
	return h, hb, wb, err1 == nil && err2 == nil
}

// RecordSlotHolds stores a machine's holds. The machine's own state (what it
// says is pending, dropped) is written first and kept even when the batch is
// refused as too big. A hold whose id is already stored counts as a
// duplicate, so a resend is safe; a malformed one is counted as rejected and
// the rest are kept.
//
// Every report, a refused one too, emits a quiet EvSlotHoldsRecorded: holds
// are bulk telemetry, so nothing is notified, hooked or kept in history; a
// machine whose holds stop arriving is told by collector health.
func (s *Service) RecordSlotHolds(ctx context.Context, actor *Actor, in SlotHoldsIn) (*SlotHoldsResult, error) {
	uid, err := slotWriter(actor)
	if err != nil {
		return nil, err
	}
	in.Machine = clip(in.Machine, 100)
	if in.Machine == "" {
		return nil, invalid("machine is required")
	}
	in.Pending, in.Dropped = max(in.Pending, 0), max(in.Dropped, 0)
	now := s.nowMS()
	res := &SlotHoldsResult{}
	tooMany := len(in.Holds) > SlotHoldsMax
	err = s.write(ctx, actor, func(t *tx) error {
		*res = SlotHoldsResult{}
		// Waiting since the oldest hold the machine still has (its own clock,
		// never later than now), so a stall is as old as the data that waits.
		// With no usable oldest, a machine already waiting keeps its earlier
		// since (it is not newer than it was), else it is now.
		since, usable := now, in.Oldest > 0 && in.Oldest < now
		if usable {
			since = in.Oldest
		}
		keep := in.Pending > 0 && !usable
		if _, err := t.ExecContext(ctx, `INSERT INTO slot_machines(user_id, machine, pending, pending_since, dropped, last_report_at) VALUES (?,?,?,?,?,?)
			ON CONFLICT(user_id, machine) DO UPDATE SET pending = excluded.pending,
				pending_since = CASE WHEN ? THEN COALESCE(slot_machines.pending_since, excluded.pending_since) ELSE excluded.pending_since END,
				dropped = excluded.dropped, last_report_at = excluded.last_report_at`,
			uid, in.Machine, in.Pending, nullIf(in.Pending > 0, since), in.Dropped, now, keep); err != nil {
			return err
		}
		if tooMany {
			t.emit(slotHoldsEvent(uid, in.Machine, in.Pending, res, true))
			return nil
		}
		for _, h := range in.Holds {
			h, hb, wb, ok := slotHoldRow(h, now)
			if !ok {
				res.Rejected++
				continue
			}
			var median, slot, exit any
			if h.MedianSecs != nil {
				median = *h.MedianSecs
			}
			if h.Slot != nil {
				slot = *h.Slot
			}
			if h.Exit != nil {
				exit = *h.Exit
			}
			r, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO slot_holds(user_id, machine, hold_id, name, repo, tree, cmd, class, median_secs,
					threshold_secs, runs, policy, slots, long_slots, slot, queued_at, wait_ms, tree_wait_ms, hold_ms, gave_up, exit_code,
					holders, waiters, received_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				uid, in.Machine, h.ID, h.Name, h.Repo, h.Tree, h.Cmd, h.Class, median, h.ThresholdSecs, h.Runs, h.Policy, h.Slots, h.LongSlots,
				slot, h.QueuedAt, h.WaitMS, h.TreeWaitMS, h.HoldMS, boolInt(h.GaveUp), exit, string(hb), string(wb), now)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n > 0 {
				res.Stored++
			} else {
				res.Duplicate++
			}
		}
		left := max(0, in.Pending-int64(res.Stored+res.Duplicate+res.Rejected))
		_, err := t.ExecContext(ctx, `UPDATE slot_machines SET pending = ?, pending_since = CASE WHEN ? = 0 THEN NULL ELSE pending_since END,
				rejected = rejected + ?, last_stored_at = CASE WHEN ? > 0 THEN ? ELSE last_stored_at END WHERE user_id = ? AND machine = ?`,
			left, left, res.Rejected, res.Stored, now, uid, in.Machine)
		if err != nil {
			return err
		}
		t.emit(slotHoldsEvent(uid, in.Machine, left, res, false))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if tooMany {
		return nil, invalid("at most %d holds in one request", SlotHoldsMax)
	}
	return res, nil
}

// slotHoldsEvent is the quiet event for one machine's report.
func slotHoldsEvent(uid int64, machine string, pending int64, res *SlotHoldsResult, refused bool) *Event {
	return &Event{Kind: EvSlotHoldsRecorded, UserID: uid, quiet: true, Data: map[string]any{
		"machine": machine, "pending": pending, "stored": res.Stored, "duplicate": res.Duplicate,
		"rejected": res.Rejected, "refused": refused,
	}}
}

// nullIf is v when ok, else NULL.
func nullIf(ok bool, v any) any {
	if ok {
		return v
	}
	return nil
}

// SlotHoldsFilter says which holds to read.
type SlotHoldsFilter struct {
	Since, Until time.Time // by queued_at; zero: open
	Machine      string    // "": every machine in scope
	User         string    // "": you; admins: a username, or "all"
	Limit        int
}

// SlotHolds reads holds oldest first, as usage machines are read: your own,
// and an admin may name another person or "all". Over Limit holds in the
// window it keeps the newest Limit, still oldest first.
func (s *Service) SlotHolds(ctx context.Context, actor *Actor, f SlotHoldsFilter) ([]SlotHold, error) {
	uids, everyone, _, err := s.usageWho(ctx, actor, f.User)
	if err != nil {
		return nil, err
	}
	where, args := []string{"1"}, []any{}
	if !everyone {
		where, args = []string{"h.user_id = ?"}, []any{uids[0]}
	}
	if !f.Since.IsZero() {
		where, args = append(where, "h.queued_at >= ?"), append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		where, args = append(where, "h.queued_at < ?"), append(args, f.Until.UnixMilli())
	}
	if m := strings.TrimSpace(f.Machine); m != "" {
		where, args = append(where, "h.machine = ?"), append(args, m)
	}
	limit := f.Limit
	if limit <= 0 || limit > SlotHoldsReadMax {
		limit = SlotHoldsReadMax
	}
	args = append(args, limit)
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT * FROM (SELECT u.username, h.machine, h.hold_id, h.name, h.repo, h.tree, h.cmd, h.class, h.median_secs,
			h.threshold_secs, h.runs, h.policy, h.slots, h.long_slots, h.slot, h.queued_at, h.wait_ms, h.tree_wait_ms, h.hold_ms, h.gave_up,
			h.exit_code, h.holders, h.waiters, h.id AS rid
		FROM slot_holds h JOIN users u ON u.id = h.user_id WHERE `+strings.Join(where, " AND ")+` ORDER BY h.queued_at DESC, h.id DESC LIMIT ?) ORDER BY queued_at, rid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SlotHold{}
	for rows.Next() {
		var h SlotHold
		var median sql.NullFloat64
		var slot, exit sql.NullInt64
		var gave int
		var hb, wb string
		var rid int64
		if err := rows.Scan(&h.User, &h.Machine, &h.ID, &h.Name, &h.Repo, &h.Tree, &h.Cmd, &h.Class, &median, &h.ThresholdSecs, &h.Runs,
			&h.Policy, &h.Slots, &h.LongSlots, &slot, &h.QueuedAt, &h.WaitMS, &h.TreeWaitMS, &h.HoldMS, &gave, &exit, &hb, &wb, &rid); err != nil {
			return nil, err
		}
		if median.Valid {
			h.MedianSecs = &median.Float64
		}
		if slot.Valid {
			v := int(slot.Int64)
			h.Slot = &v
		}
		if exit.Valid {
			v := int(exit.Int64)
			h.Exit = &v
		}
		h.GaveUp = gave == 1
		h.Holders, h.Waiters = []SlotHolder{}, []SlotWaiter{}
		_ = json.Unmarshal([]byte(hb), &h.Holders)
		_ = json.Unmarshal([]byte(wb), &h.Waiters)
		out = append(out, h)
	}
	return out, rows.Err()
}
