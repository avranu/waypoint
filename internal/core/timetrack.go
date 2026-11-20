package core

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// TimeEntry is a span of tracked work, optionally on an issue.
type TimeEntry struct {
	ID         int64     `json:"id"`
	User       UserRef   `json:"user"`
	Issue      *IssueRef `json:"issue"`
	ProjectID  *int64    `json:"project_id"`
	ProjectKey string    `json:"project_key,omitempty"`
	Note       string    `json:"note"`
	// Agent is the agent tracking this time on its person's behalf; agents'
	// timers run beside their person's own.
	Agent     string     `json:"agent,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	// Seconds is the duration so far; it grows while the timer runs.
	Seconds int64 `json:"seconds"`
	Running bool  `json:"running"`
}

const timeSelect = `SELECT t.id, t.note, t.agent, t.started_at, t.ended_at, t.project_id, coalesce(p.key, ''),
	i.id, coalesce(ip.key || '-' || i.number, ''), coalesce(i.title, ''), coalesce(i.type, ''),
	u.id, u.username, u.display_name, u.avatar_version
	FROM time_entries t
	JOIN users u ON u.id = t.user_id
	LEFT JOIN issues i ON i.id = t.issue_id
	LEFT JOIN projects ip ON ip.id = i.project_id
	LEFT JOIN projects p ON p.id = t.project_id`

func (s *Service) scanTimeEntries(rows *sql.Rows) ([]TimeEntry, error) {
	defer rows.Close()
	now := s.nowMS()
	out := []TimeEntry{}
	for rows.Next() {
		var e TimeEntry
		var started int64
		var ended, pid, iid, av sql.NullInt64
		var ikey, ititle, itype string
		if err := rows.Scan(&e.ID, &e.Note, &e.Agent, &started, &ended, &pid, &e.ProjectKey, &iid, &ikey, &ititle, &itype,
			&e.User.ID, &e.User.Username, &e.User.DisplayName, &av); err != nil {
			return nil, err
		}
		e.User.AvatarURL = avatarURL(e.User.ID, av.Int64)
		e.StartedAt, e.EndedAt, e.ProjectID = msTime(started), ptrTime(ended), ptrInt(pid)
		if iid.Valid {
			e.Issue = &IssueRef{ID: iid.Int64, Key: ikey, Title: ititle, Type: itype}
		}
		end := now
		if ended.Valid {
			end = ended.Int64
		} else {
			e.Running = true
		}
		e.Seconds = max(0, (end-started)/1000)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) timeEntry(ctx context.Context, q queryer, id int64) (*TimeEntry, error) {
	rows, err := q.QueryContext(ctx, timeSelect+` WHERE t.id = ?`, id)
	if err != nil {
		return nil, err
	}
	list, err := s.scanTimeEntries(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("time entry")
	}
	return &list[0], nil
}

// AgentTimerLimit is how long an agent's timer may run: one still running
// after this is stopped (agents don't notice a forgotten timer).
const AgentTimerLimit = 24 * time.Hour

// RunningTimer returns the actor's running timer, or nil. An agent has its
// own timer, apart from its person's.
func (s *Service) RunningTimer(ctx context.Context, actor *Actor) (*TimeEntry, error) {
	rows, err := s.db.R.QueryContext(ctx, timeSelect+` WHERE t.user_id = ? AND t.agent = ? AND t.ended_at IS NULL`, actor.id(), actor.agentName())
	if err != nil {
		return nil, err
	}
	list, err := s.scanTimeEntries(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// StartTimer starts tracking time, stopping any running timer first (an
// agent's timer and its person's are separate). issueRef may be empty to
// track time without an issue.
func (s *Service) StartTimer(ctx context.Context, actor *Actor, issueRef, note string) (*TimeEntry, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	var id int64
	agent := actor.agentName()
	err := s.write(ctx, actor, func(t *tx) error {
		var issueID, projectID any
		if issueRef != "" {
			i, _, err := s.getIssueFor(ctx, t, actor, issueRef, RoleViewer, false)
			if err != nil {
				return err
			}
			// Restarting the timer on the same issue is a no-op.
			var running sql.NullInt64
			if err := t.QueryRowContext(ctx, `SELECT id FROM time_entries WHERE user_id = ? AND agent = ? AND ended_at IS NULL AND issue_id = ?`, actor.id(), agent, i.ID).Scan(&running); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if running.Valid && note == "" {
				id = running.Int64
				return nil
			}
			issueID, projectID = i.ID, i.ProjectID
		}
		now := s.nowMS()
		if _, err := t.ExecContext(ctx, `UPDATE time_entries SET ended_at = ? WHERE user_id = ? AND agent = ? AND ended_at IS NULL`, now, actor.id(), agent); err != nil {
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO time_entries(user_id, agent, issue_id, project_id, note, started_at, created_at) VALUES (?,?,?,?,?,?,?)`,
			actor.id(), agent, issueID, projectID, strings.TrimSpace(note), now, now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		t.emit(&Event{Kind: EvTimer, UserID: actor.id()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.timerEvent(ctx, actor, EvTimerStarted, id)
}

// timerEvent returns a timer's entry and lets automations know what
// happened to it (WP-D18).
func (s *Service) timerEvent(ctx context.Context, actor *Actor, kind string, id int64) (*TimeEntry, error) {
	entry, err := s.timeEntry(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	e := &Event{Kind: kind, Data: map[string]any{"entry": id, "user": entry.User.Username, "note": entry.Note, "seconds": entry.Seconds}}
	if entry.ProjectID != nil {
		e.ProjectID = *entry.ProjectID
	}
	if entry.Issue != nil {
		e.IssueID, e.IssueKey = entry.Issue.ID, entry.Issue.Key
		if e.ProjectID == 0 {
			_ = s.db.R.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE id = ?`, entry.Issue.ID).Scan(&e.ProjectID)
		}
	}
	s.automate(ctx, actor, e)
	return entry, nil
}

// StopTimer stops the actor's running timer, if any.
func (s *Service) StopTimer(ctx context.Context, actor *Actor) (*TimeEntry, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	var id int64
	err := s.write(ctx, actor, func(t *tx) error {
		err := t.QueryRowContext(ctx, `SELECT id FROM time_entries WHERE user_id = ? AND agent = ? AND ended_at IS NULL`, actor.id(), actor.agentName()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, `UPDATE time_entries SET ended_at = ? WHERE id = ?`, s.nowMS(), id)
		t.emit(&Event{Kind: EvTimer, UserID: actor.id()})
		return err
	})
	if err != nil || id == 0 {
		return nil, err
	}
	return s.timerEvent(ctx, actor, EvTimerStopped, id)
}

// TimeEntryInput creates or edits an entry. Give Started plus either Ended
// or Duration ("1h30m", "45m", "1.5h", "1:30").
type TimeEntryInput struct {
	Issue     *string `json:"issue"`
	Note      *string `json:"note"`
	StartedAt *string `json:"started_at"`
	EndedAt   *string `json:"ended_at"`
	Duration  *string `json:"duration"`
}

var clockDurRE = lazyre.New(`^(\d+):(\d{1,2})$`)

// ParseDuration accepts Go durations, plain minutes, and h:mm.
func ParseDuration(v string) (time.Duration, error) {
	v = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(v)), " ", "")
	if m := clockDurRE.FindStringSubmatch(v); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		return time.Duration(h)*time.Hour + time.Duration(mi)*time.Minute, nil
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(n * float64(time.Minute)), nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d, nil
	}
	return 0, invalid("can't understand duration %q (try 1h30m, 45m, 1:30)", v)
}

func parseTimestamp(v string) (int64, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return 0, invalid("timestamps must be RFC 3339, like 2026-10-11T09:30:00Z")
	}
	return t.UnixMilli(), nil
}

// CreateTimeEntry records time manually.
func (s *Service) CreateTimeEntry(ctx context.Context, actor *Actor, in TimeEntryInput) (*TimeEntry, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	var id int64
	err := s.write(ctx, actor, func(t *tx) error {
		var issueID, projectID any
		if in.Issue != nil && *in.Issue != "" {
			i, _, err := s.getIssueFor(ctx, t, actor, *in.Issue, RoleViewer, false)
			if err != nil {
				return err
			}
			issueID, projectID = i.ID, i.ProjectID
		}
		start, end, err := s.entrySpan(in, nil, nil)
		if err != nil {
			return err
		}
		if end == nil {
			return invalid("give an end time or a duration")
		}
		res, err := t.ExecContext(ctx, `INSERT INTO time_entries(user_id, agent, issue_id, project_id, note, started_at, ended_at, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			actor.id(), actor.agentName(), issueID, projectID, deref(in.Note), start, *end, s.nowMS())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		t.emit(&Event{Kind: EvTimer, UserID: actor.id()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.timeEntry(ctx, s.db.R, id)
}

func (s *Service) entrySpan(in TimeEntryInput, curStart, curEnd *int64) (int64, *int64, error) {
	var start int64
	switch {
	case in.StartedAt != nil:
		v, err := parseTimestamp(*in.StartedAt)
		if err != nil {
			return 0, nil, err
		}
		start = v
	case curStart != nil:
		start = *curStart
	}
	end := curEnd
	if in.EndedAt != nil {
		v, err := parseTimestamp(*in.EndedAt)
		if err != nil {
			return 0, nil, err
		}
		end = &v
	}
	if in.Duration != nil {
		d, err := ParseDuration(*in.Duration)
		if err != nil {
			return 0, nil, err
		}
		if d <= 0 || d > 24*time.Hour {
			return 0, nil, invalid("duration must be between 1 minute and 24 hours")
		}
		if in.StartedAt == nil && curStart == nil {
			// Duration alone: it just ended.
			e := s.nowMS()
			start = e - d.Milliseconds()
			end = &e
		} else {
			e := start + d.Milliseconds()
			end = &e
		}
	}
	if start == 0 {
		return 0, nil, invalid("give a start time or a duration")
	}
	if end != nil && *end < start {
		return 0, nil, invalid("an entry cannot end before it starts")
	}
	if end == nil && start > s.nowMS()+60_000 {
		return 0, nil, invalid("a running timer can't start in the future")
	}
	return start, end, nil
}

// UpdateTimeEntry edits one of the actor's entries.
func (s *Service) UpdateTimeEntry(ctx context.Context, actor *Actor, id int64, in TimeEntryInput) (*TimeEntry, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	err := s.write(ctx, actor, func(t *tx) error {
		var uid, start int64
		var end sql.NullInt64
		if err := t.QueryRowContext(ctx, `SELECT user_id, started_at, ended_at FROM time_entries WHERE id = ?`, id).Scan(&uid, &start, &end); err != nil {
			return notFound("time entry")
		}
		if uid != actor.id() {
			return ErrForbidden
		}
		newStart, newEnd, err := s.entrySpan(in, &start, ptrInt(end))
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE time_entries SET started_at = ?, ended_at = ? WHERE id = ?`, newStart, nullInt(newEnd), id); err != nil {
			return err
		}
		if in.Note != nil {
			note := strings.TrimSpace(*in.Note)
			if len(note) > maxNoteLen {
				return invalid("note is too long")
			}
			if _, err := t.ExecContext(ctx, `UPDATE time_entries SET note = ? WHERE id = ?`, note, id); err != nil {
				return err
			}
		}
		if in.Issue != nil {
			if *in.Issue == "" {
				if _, err := t.ExecContext(ctx, `UPDATE time_entries SET issue_id = NULL, project_id = NULL WHERE id = ?`, id); err != nil {
					return err
				}
			} else {
				i, _, err := s.getIssueFor(ctx, t, actor, *in.Issue, RoleViewer, false)
				if err != nil {
					return err
				}
				if _, err := t.ExecContext(ctx, `UPDATE time_entries SET issue_id = ?, project_id = ? WHERE id = ?`, i.ID, i.ProjectID, id); err != nil {
					return err
				}
			}
		}
		t.emit(&Event{Kind: EvTimer, UserID: actor.id()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.timeEntry(ctx, s.db.R, id)
}

// DeleteTimeEntry removes one of the actor's entries.
func (s *Service) DeleteTimeEntry(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	res, err := s.db.W.ExecContext(ctx, `DELETE FROM time_entries WHERE id = ? AND user_id = ?`, id, actor.id())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("time entry")
	}
	s.Hub.Publish(&Event{Kind: EvTimer, UserID: actor.id()})
	return nil
}

const maxNoteLen = 1000

// TimeQuery filters time entries.
type TimeQuery struct {
	From    *time.Time
	To      *time.Time
	Issue   string // entries on an issue (by anyone, if the actor can see it)
	Project string // entries in a project (members see everyone's)
	Limit   int
	all     bool // reports: no limit (the date range bounds it)
}

// ListTimeEntries returns entries, newest first. Without Issue or Project it
// returns the actor's own entries.
func (s *Service) ListTimeEntries(ctx context.Context, actor *Actor, q TimeQuery) ([]TimeEntry, error) {
	where, args := []string{}, []any{}
	switch {
	case q.Issue != "":
		i, _, err := s.getIssueFor(ctx, s.db.R, actor, q.Issue, RoleViewer, false)
		if err != nil {
			return nil, err
		}
		where, args = append(where, "t.issue_id = ?"), append(args, i.ID)
	case q.Project != "":
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		where, args = append(where, "t.project_id = ?"), append(args, p.ID)
	default:
		where, args = append(where, "t.user_id = ?"), append(args, actor.id())
	}
	if q.From != nil {
		where, args = append(where, "coalesce(t.ended_at, ?) >= ?"), append(args, s.nowMS(), q.From.UnixMilli())
	}
	if q.To != nil {
		where, args = append(where, "t.started_at < ?"), append(args, q.To.UnixMilli())
	}
	switch {
	case q.all:
		q.Limit = -1 // SQLite: no limit
	case q.Limit <= 0:
		q.Limit = 500
	case q.Limit > 2000:
		q.Limit = 2000
	}
	rows, err := s.db.R.QueryContext(ctx, timeSelect+` WHERE `+strings.Join(where, " AND ")+` ORDER BY t.started_at DESC LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	return s.scanTimeEntries(rows)
}

// stopLongAgentTimers ends agents' timers that ran past AgentTimerLimit, at
// the limit; the scheduler calls it.
func (s *Service) stopLongAgentTimers(ctx context.Context) error {
	limit := AgentTimerLimit.Milliseconds()
	_, err := s.db.W.ExecContext(ctx, `UPDATE time_entries SET ended_at = started_at + ?
		WHERE agent != '' AND ended_at IS NULL AND started_at < ?`, limit, s.nowMS()-limit)
	return err
}
