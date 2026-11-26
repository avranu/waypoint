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

const (
	snapshotsPerSession = 100
	dropsIDMax          = 64
	dropReportKeep      = 30 * 24 * time.Hour
	observedPerSession  = 400
	snapshotMaxBytes    = 64 << 10
	dropKeyMax          = 80
)

// HookSnapshot is what the CLI observed on the machine. Pointers: nil is
// NULL, a value the source did not carry.
type HookSnapshot struct {
	Harness        *string `json:"harness,omitempty"`
	Entrypoint     *string `json:"entrypoint,omitempty"`
	HarnessVersion *string `json:"harness_version,omitempty"`
	WPVersion      *string `json:"wp_version,omitempty"`
	Model          *string `json:"model,omitempty"`
	Effort         *string `json:"effort,omitempty"`
	PermissionMode *string `json:"permission_mode,omitempty"`
	// Requested at launch: the claude process's own arguments.
	RequestedModel          *string `json:"requested_model,omitempty"`
	RequestedEffort         *string `json:"requested_effort,omitempty"`
	RequestedPermissionMode *string `json:"requested_permission_mode,omitempty"`

	GitHead     *string `json:"git_head,omitempty"`
	GitBranch   *string `json:"git_branch,omitempty"`
	GitWorktree *bool   `json:"git_worktree,omitempty"`

	MemoryFiles      *int64 `json:"memory_files,omitempty"`
	MemoryBytes      *int64 `json:"memory_bytes,omitempty"`
	MemoryIndexBytes *int64 `json:"memory_index_bytes,omitempty"`

	// Inventory: settings files (exists, bytes, sha256, selected keys),
	// instruction files (path, bytes), allow-listed environment, hook events,
	// skills, MCP servers and plugins by name. No secret values.
	Inventory json.RawMessage `json:"inventory,omitempty"`
	// Sources: field -> where it was observed ("hook:SessionStart", "env:NAME",
	// "disk:PATH", "process:argv").
	Sources map[string]string `json:"sources,omitempty"`
	// Missing: field -> why it is NULL.
	Missing map[string]string `json:"missing,omitempty"`
	BuildMS *int64            `json:"build_ms,omitempty"`
}

// SessionSnapshot is a stored row.
type SessionSnapshot struct {
	ID            int64     `json:"id"`
	At            time.Time `json:"at"`
	TriggerEvent  string    `json:"trigger_event"`
	TriggerSource *string   `json:"trigger_source"`
	HookSnapshot
}

// ObservedChange is one row of the observed timeline.
type ObservedChange struct {
	At     time.Time       `json:"at"`
	Kind   string          `json:"kind"`
	Value  string          `json:"value"`
	Prev   *string         `json:"prev"`
	Source string          `json:"source"`
	Detail json.RawMessage `json:"detail"`
}

// HookDrop is a count of hook data that was dropped.
type HookDrop struct {
	User    UserRef   `json:"user"`
	Machine string    `json:"machine"`
	Reason  string    `json:"reason"`
	Event   string    `json:"event"`
	Dropped int64     `json:"dropped"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
}

func nstr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nint(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nbool(p *bool) any {
	if p == nil {
		return nil
	}
	return boolInt(*p)
}

// recordObserved stores what an event shows the session to be running with:
// a snapshot, a changed model/effort/permission mode, a loaded instruction
// file, a config change; and counts what the CLI's outbox dropped.
func (s *Service) recordObserved(ctx context.Context, t *tx, actor *Actor, x *AgentSession, ev *HookEvent) error {
	at := s.hookTime(ev)
	machine := cmp(strings.TrimSpace(ev.Machine), "unknown")
	if err := s.countDrops(ctx, t, actor, machine, at, ev.Drops, ev.DropsID); err != nil {
		return err
	}
	src := "hook:" + ev.Event
	if ev.Source != "" {
		src += ":" + ev.Source
	}
	switch ev.Event {
	case "SessionStart", "PostModelSwitch":
		if ev.Model != "" {
			detail := map[string]string{}
			if ev.Event == "PostModelSwitch" {
				if ev.FromModel != "" {
					detail["from_model"] = clipRunes(ev.FromModel, 1000)
				}
				if ev.RequestedModel != "" {
					detail["requested_model"] = clipRunes(ev.RequestedModel, 1000)
				}
			}
			if err := s.observeValue(ctx, t, x.ID, at, "model", ev.Model, src, detail); err != nil {
				return err
			}
		}
	}
	if ev.PermissionMode != "" {
		if err := s.observeValue(ctx, t, x.ID, at, "permission_mode", ev.PermissionMode, src, nil); err != nil {
			return err
		}
	}
	if ev.Effort != "" {
		if err := s.observeValue(ctx, t, x.ID, at, "effort", ev.Effort, src, nil); err != nil {
			return err
		}
	}
	if (ev.Event == "InstructionsLoaded" || ev.Event == "ConfigChange") && ev.File != "" {
		kind, detail := "instruction_file", map[string]string{"memory_type": ev.MemoryType, "load_reason": ev.LoadReason}
		if ev.Event == "ConfigChange" {
			kind, detail = "config_change", map[string]string{"config": ev.Source}
		}
		if err := s.observeAlways(ctx, t, actor, machine, x.ID, at, kind, clipRunes(ev.File, 1000), src, detail); err != nil {
			return err
		}
	}
	if ev.Snapshot != nil {
		return s.storeSnapshot(ctx, t, actor, machine, x.ID, at, ev)
	}
	return nil
}

// countDrops adds the CLI's counts of what it dropped. The key is
// "reason/event"; a malformed key is counted as such rather than skipped. A
// report with an id already applied (the client sent it again after a lost
// reply) adds nothing; an empty or over-long id is counted every time, as
// older clients are.
func (s *Service) countDrops(ctx context.Context, t *tx, actor *Actor, machine string, at int64, drops map[string]int64, id string) error {
	if len(drops) == 0 {
		return nil
	}
	if id != "" && len(id) <= dropsIDMax {
		if _, err := t.ExecContext(ctx, `DELETE FROM hook_drop_reports WHERE at < ?`, at-dropReportKeep.Milliseconds()); err != nil {
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO hook_drop_reports(user_id, report_id, at) VALUES (?,?,?) ON CONFLICT DO NOTHING`, actor.ID(), id, at)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return nil
		}
	}
	for k, n := range drops {
		if n <= 0 {
			continue
		}
		reason, event, ok := strings.Cut(k, "/")
		if !ok || len(k) > dropKeyMax {
			reason, event = "malformed_key", "?"
		}
		if err := s.addDrop(ctx, t, actor, machine, reason, event, n, at); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) addDrop(ctx context.Context, t *tx, actor *Actor, machine, reason, event string, n, at int64) error {
	return addDropRow(ctx, t, actor.ID(), machine, reason, event, n, at)
}

// addDropRow adds n to the count of what was dropped for a person's machine.
// The server's own drops (a trim, a cap, a skipped row, a withheld prompt) and
// the CLI's outbox drops are counted the same way, and shown by `wp server
// status` and in the "session data" collector (WP-1470, WP-ADR145 point 5).
func addDropRow(ctx context.Context, q queryer, uid int64, machine, reason, event string, n, at int64) error {
	if n <= 0 {
		return nil
	}
	_, err := q.ExecContext(ctx, `INSERT INTO hook_drops(user_id, machine, reason, event, dropped, first_at, last_at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(user_id, machine, reason, event) DO UPDATE SET dropped = dropped + excluded.dropped, last_at = max(last_at, excluded.last_at)`,
		uid, machine, reason, event, n, at, at)
	return err
}

// Reasons the server counts in hook_drops for data it did not keep.
const (
	DropSessionEventsTrim  = "session_events_trim"
	DropEvidenceRunsCap    = "evidence_cap"
	DropPromptTextWithheld = "prompt_text_withheld"
	DropUnregisteredWrite  = "unregistered_session"
)

// observeValue puts a change into the session's timeline of that kind. The
// baseline is the latest row at or before the event's time (ties by id), so an
// event that arrives late is placed where it belongs: equal to the baseline,
// nothing is written; otherwise a row is added with the baseline as prev, and
// the next row after it is repaired (deleted if it now says the same, else
// its prev becomes this value).
func (s *Service) observeValue(ctx context.Context, t *tx, sid, at int64, kind, value, source string, detail map[string]string) error {
	var prev sql.NullString
	err := t.QueryRowContext(ctx, `SELECT value FROM session_observed WHERE session_id = ? AND kind = ? AND at <= ? ORDER BY at DESC, id DESC LIMIT 1`, sid, kind, at).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if prev.Valid && prev.String == value {
		return nil
	}
	d, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("observed detail: %w", err)
	}
	if detail == nil {
		d = []byte("{}")
	}
	res, err := t.ExecContext(ctx, `INSERT INTO session_observed(session_id, at, kind, value, prev, source, detail) VALUES (?,?,?,?,?,?,?)`,
		sid, at, kind, value, prev, source, string(d))
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	var nid int64
	var nval string
	err = t.QueryRowContext(ctx, `SELECT id, value FROM session_observed WHERE session_id = ? AND kind = ? AND (at > ? OR (at = ? AND id > ?)) ORDER BY at, id LIMIT 1`,
		sid, kind, at, at, id).Scan(&nid, &nval)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if nval == value {
		_, err = t.ExecContext(ctx, `DELETE FROM session_observed WHERE id = ?`, nid)
		return err
	}
	_, err = t.ExecContext(ctx, `UPDATE session_observed SET prev = ? WHERE id = ?`, value, nid)
	return err
}

// observeAlways adds a row each time, up to observedPerSession a session; the
// rest are counted as dropped.
func (s *Service) observeAlways(ctx context.Context, t *tx, actor *Actor, machine string, sid, at int64, kind, value, source string, detail map[string]string) error {
	var n int64
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM session_observed WHERE session_id = ? AND kind = ?`, sid, kind).Scan(&n); err != nil {
		return err
	}
	if n >= observedPerSession {
		return s.addDrop(ctx, t, actor, machine, "server_cap", kind, 1, at)
	}
	d, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("observed detail: %w", err)
	}
	_, err = t.ExecContext(ctx, `INSERT INTO session_observed(session_id, at, kind, value, source, detail) VALUES (?,?,?,?,?,?)`,
		sid, at, kind, value, source, string(d))
	return err
}

func (s *Service) storeSnapshot(ctx context.Context, t *tx, actor *Actor, machine string, sid, at int64, ev *HookEvent) error {
	sn := ev.Snapshot
	inv := string(sn.Inventory)
	if inv == "" {
		inv = "{}"
	}
	missing := map[string]string{}
	for k, v := range sn.Missing {
		missing[k] = v
	}
	if len(inv) > snapshotMaxBytes || !json.Valid([]byte(inv)) {
		if err := s.addDrop(ctx, t, actor, machine, "snapshot_inventory_unusable", ev.Event, 1, at); err != nil {
			return err
		}
		inv = "{}"
		missing["inventory"] = "too large or not JSON; dropped by the server"
	}
	sources := sn.Sources
	if sources == nil {
		sources = map[string]string{}
	}
	srcJSON, err := json.Marshal(sources)
	if err != nil {
		return fmt.Errorf("snapshot sources: %w", err)
	}
	missJSON, err := json.Marshal(missing)
	if err != nil {
		return fmt.Errorf("snapshot missing: %w", err)
	}
	var trig any
	if ev.Source != "" {
		trig = ev.Source
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO session_snapshots(session_id, at, trigger_event, trigger_source, harness, entrypoint, harness_version, wp_version,
		model, effort, permission_mode, requested_model, requested_effort, requested_permission_mode, git_head, git_branch, git_worktree,
		memory_files, memory_bytes, memory_index_bytes, inventory, sources, missing, build_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sid, at, ev.Event, trig, nstr(sn.Harness), nstr(sn.Entrypoint), nstr(sn.HarnessVersion), nstr(sn.WPVersion),
		nstr(sn.Model), nstr(sn.Effort), nstr(sn.PermissionMode), nstr(sn.RequestedModel), nstr(sn.RequestedEffort), nstr(sn.RequestedPermissionMode),
		nstr(sn.GitHead), nstr(sn.GitBranch), nbool(sn.GitWorktree), nint(sn.MemoryFiles), nint(sn.MemoryBytes), nint(sn.MemoryIndexBytes),
		inv, string(srcJSON), string(missJSON), nint(sn.BuildMS)); err != nil {
		return err
	}
	res, err := t.ExecContext(ctx, `DELETE FROM session_snapshots WHERE session_id = ? AND id NOT IN (
		SELECT min(id) FROM session_snapshots WHERE session_id = ?
		UNION SELECT id FROM (SELECT id FROM session_snapshots WHERE session_id = ? ORDER BY id DESC LIMIT ?))`, sid, sid, sid, snapshotsPerSession-1)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("snapshot trim: %w", err)
	} else if n > 0 {
		return s.addDrop(ctx, t, actor, machine, "server_cap", "snapshot", n, at)
	}
	return nil
}

// ownSession finds a session its owner or an admin may read the machine
// details of (absolute paths, settings hashes, inventory): like prompts, they
// are not shown to a colleague who can see the session. A colleague who can
// see it is refused; one who can't gets not found.
func (s *Service) ownSession(ctx context.Context, actor *Actor, sessionID int64) error {
	x, err := s.sessionByID(ctx, s.db.R, actor, sessionID)
	if err != nil {
		return err
	}
	if x.userID != actor.ID() && !actor.IsAdmin() {
		return forbidden("a session's snapshots and observed changes are for its owner and admins")
	}
	return nil
}

// SessionSnapshots is the session's snapshots, oldest first. Owner and admins
// only.
func (s *Service) SessionSnapshots(ctx context.Context, actor *Actor, sessionID int64) ([]SessionSnapshot, error) {
	if err := s.ownSession(ctx, actor, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, at, trigger_event, trigger_source, harness, entrypoint, harness_version, wp_version, model, effort, permission_mode,
		requested_model, requested_effort, requested_permission_mode, git_head, git_branch, git_worktree, memory_files, memory_bytes, memory_index_bytes,
		inventory, sources, missing, build_ms FROM session_snapshots WHERE session_id = ? ORDER BY at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []SessionSnapshot{}
	for rows.Next() {
		var r SessionSnapshot
		var at int64
		var wt, mf, mb, mi, bm sql.NullInt64
		var h, e, hv, wv, m, ef, pm, rm, re, rp, gh, gb, ts sql.NullString
		var inv, src, miss string
		if err := rows.Scan(&r.ID, &at, &r.TriggerEvent, &ts, &h, &e, &hv, &wv, &m, &ef, &pm, &rm, &re, &rp, &gh, &gb, &wt, &mf, &mb, &mi, &inv, &src, &miss, &bm); err != nil {
			return nil, err
		}
		r.At = msTime(at)
		r.TriggerSource = ptrStr(ts)
		r.Harness, r.Entrypoint, r.HarnessVersion, r.WPVersion = ptrStr(h), ptrStr(e), ptrStr(hv), ptrStr(wv)
		r.Model, r.Effort, r.PermissionMode = ptrStr(m), ptrStr(ef), ptrStr(pm)
		r.RequestedModel, r.RequestedEffort, r.RequestedPermissionMode = ptrStr(rm), ptrStr(re), ptrStr(rp)
		r.GitHead, r.GitBranch = ptrStr(gh), ptrStr(gb)
		if wt.Valid {
			b := wt.Int64 != 0
			r.GitWorktree = &b
		}
		r.MemoryFiles, r.MemoryBytes, r.MemoryIndexBytes, r.BuildMS = ptrInt(mf), ptrInt(mb), ptrInt(mi), ptrInt(bm)
		r.Inventory = json.RawMessage(inv)
		if err := json.Unmarshal([]byte(src), &r.Sources); err != nil {
			return nil, fmt.Errorf("snapshot %d sources: %w", r.ID, err)
		}
		if err := json.Unmarshal([]byte(miss), &r.Missing); err != nil {
			return nil, fmt.Errorf("snapshot %d missing: %w", r.ID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionObserved is the session's observed changes, oldest first. Owner and
// admins only.
func (s *Service) SessionObserved(ctx context.Context, actor *Actor, sessionID int64) ([]ObservedChange, error) {
	if err := s.ownSession(ctx, actor, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT at, kind, value, prev, source, detail FROM session_observed WHERE session_id = ? ORDER BY at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ObservedChange{}
	for rows.Next() {
		var c ObservedChange
		var at int64
		var prev sql.NullString
		var d string
		if err := rows.Scan(&at, &c.Kind, &c.Value, &prev, &c.Source, &d); err != nil {
			return nil, err
		}
		c.At, c.Prev, c.Detail = msTime(at), ptrStr(prev), json.RawMessage(d)
		out = append(out, c)
	}
	return out, rows.Err()
}

// HookDrops is the counts of hook data dropped, for every person and machine
// (server status; admins only).
func (s *Service) HookDrops(ctx context.Context, actor *Actor) ([]HookDrop, error) {
	if err := actor.canReadServer(); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT h.user_id, coalesce(u.username, ''), coalesce(u.display_name, ''), h.machine, h.reason, h.event, sum(h.dropped), min(h.first_at), max(h.last_at)
		FROM hook_drops h LEFT JOIN users u ON u.id = h.user_id GROUP BY h.user_id, h.machine, h.reason, h.event ORDER BY 7 DESC, 1, 4`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HookDrop{}
	for rows.Next() {
		var d HookDrop
		var f, l int64
		if err := rows.Scan(&d.User.ID, &d.User.Username, &d.User.DisplayName, &d.Machine, &d.Reason, &d.Event, &d.Dropped, &f, &l); err != nil {
			return nil, err
		}
		d.First, d.Last = msTime(f), msTime(l)
		out = append(out, d)
	}
	return out, rows.Err()
}
