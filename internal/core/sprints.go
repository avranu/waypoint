package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sprint is a time-boxed set of issues.
type Sprint struct {
	ID              int64      `json:"id"`
	ProjectID       int64      `json:"project_id"`
	Name            string     `json:"name"`
	Goal            string     `json:"goal"`
	State           string     `json:"state"` // planned, active, closed
	StartDate       *string    `json:"start_date"`
	EndDate         *string    `json:"end_date"`
	StartedAt       *time.Time `json:"started_at"`
	ClosedAt        *time.Time `json:"closed_at"`
	CommittedPoints *float64   `json:"committed_points"`
	CompletedPoints *float64   `json:"completed_points"`
	IssueCount      int        `json:"issue_count"`
	OpenCount       int        `json:"open_count"`
	Points          float64    `json:"points"`
	DonePoints      float64    `json:"done_points"`
}

const sprintSelect = `SELECT s.id, s.project_id, s.name, s.goal, s.state, s.start_date, s.end_date, s.started_at, s.closed_at,
	s.committed_points, s.completed_points,
	(SELECT count(*) FROM issues i WHERE i.sprint_id = s.id),
	(SELECT count(*) FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.sprint_id = s.id AND st.category NOT IN ('done','canceled')),
	(SELECT coalesce(sum(i.points), 0) FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.sprint_id = s.id AND st.category != 'canceled'),
	(SELECT coalesce(sum(i.points), 0) FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.sprint_id = s.id AND st.category = 'done')
	FROM sprints s`

func scanSprint(sc interface{ Scan(...any) error }) (*Sprint, error) {
	var sp Sprint
	var start, end sql.NullString
	var started, closed sql.NullInt64
	var committed, completed sql.NullFloat64
	if err := sc.Scan(&sp.ID, &sp.ProjectID, &sp.Name, &sp.Goal, &sp.State, &start, &end, &started, &closed, &committed, &completed,
		&sp.IssueCount, &sp.OpenCount, &sp.Points, &sp.DonePoints); err != nil {
		return nil, err
	}
	sp.StartDate, sp.EndDate = ptrStr(start), ptrStr(end)
	sp.StartedAt, sp.ClosedAt = ptrTime(started), ptrTime(closed)
	sp.CommittedPoints, sp.CompletedPoints = ptrFloat(committed), ptrFloat(completed)
	return &sp, nil
}

func (s *Service) listSprints(ctx context.Context, q queryer, projectID int64, includeClosed bool) ([]*Sprint, error) {
	where := ` WHERE s.project_id = ? AND s.state != 'closed'`
	if includeClosed {
		where = ` WHERE s.project_id = ?`
	}
	rows, err := q.QueryContext(ctx, sprintSelect+where+` ORDER BY CASE s.state WHEN 'active' THEN 0 WHEN 'planned' THEN 1 ELSE 2 END,
		CASE WHEN s.state = 'closed' THEN -s.closed_at ELSE coalesce(s.start_date, '9999') END, s.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Sprint{}
	for rows.Next() {
		sp, err := scanSprint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// ListSprints returns a project's sprints, optionally including closed ones.
func (s *Service) ListSprints(ctx context.Context, actor *Actor, projectRef string, includeClosed bool) ([]*Sprint, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	return s.listSprints(ctx, s.db.R, p.ID, includeClosed)
}

func (s *Service) sprintByID(ctx context.Context, q queryer, id int64) (*Sprint, error) {
	sp, err := scanSprint(q.QueryRowContext(ctx, sprintSelect+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("sprint")
	}
	return sp, err
}

// GetSprint returns one sprint.
func (s *Service) GetSprint(ctx context.Context, actor *Actor, id int64) (*Sprint, error) {
	sp, err := s.sprintByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, sp.ProjectID, RoleViewer); err != nil {
		return nil, err
	}
	return sp, nil
}

// resolveSprint finds a sprint by id, name, "active"/"current" or "next".
func (s *Service) resolveSprint(ctx context.Context, q queryer, projectID int64, ref string) (*Sprint, error) {
	ref = strings.TrimSpace(ref)
	var where string
	var args []any
	switch strings.ToLower(ref) {
	case "active", "current":
		where, args = ` WHERE s.project_id = ? AND s.state = 'active'`, []any{projectID}
	case "next":
		where, args = ` WHERE s.project_id = ? AND s.state = 'planned' ORDER BY coalesce(s.start_date, '9999'), s.id LIMIT 1`, []any{projectID}
	default:
		where, args = ` WHERE s.project_id = ? AND (CAST(s.id AS TEXT) = ? OR s.name = ? COLLATE NOCASE) ORDER BY s.id DESC LIMIT 1`, []any{projectID, ref, ref}
	}
	sp, err := scanSprint(q.QueryRowContext(ctx, sprintSelect+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invalid("no sprint %q in this project", ref)
	}
	return sp, err
}

// SprintInput creates or edits a sprint.
type SprintInput struct {
	Name      *string     `json:"name"`
	Goal      *string     `json:"goal"`
	StartDate Opt[string] `json:"start_date"`
	EndDate   Opt[string] `json:"end_date"`
}

// CreateSprint plans a new sprint. Without dates it follows the last sprint.
func (s *Service) CreateSprint(ctx context.Context, actor *Actor, projectRef string, in SprintInput) (*Sprint, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		var count int
		var lastEnd sql.NullString
		if err := t.QueryRowContext(ctx, `SELECT count(*), max(end_date) FROM sprints WHERE project_id = ?`, p.ID).Scan(&count, &lastEnd); err != nil {
			return err
		}
		name := deref(in.Name)
		if name == "" {
			name = fmt.Sprintf("%s Sprint %d", p.Key, count+1)
		}
		loc := s.actorLocation(actor)
		start := s.now().In(loc)
		if lastEnd.Valid {
			if d, err := time.ParseInLocation(dateLayout, lastEnd.String, loc); err == nil && d.After(start.AddDate(0, 0, -1)) {
				start = d.AddDate(0, 0, 1)
			}
		}
		startS := start.Format(dateLayout)
		endS := start.AddDate(0, 0, p.Settings.SprintDays-1).Format(dateLayout)
		if in.StartDate.Set {
			v, err := s.parseDateFor(actor, deref(in.StartDate.Value))
			if err != nil {
				return err
			}
			startS = deref(v)
		}
		if in.EndDate.Set {
			v, err := s.parseDateFor(actor, deref(in.EndDate.Value))
			if err != nil {
				return err
			}
			endS = deref(v)
		}
		if startS != "" && endS != "" && endS < startS {
			return invalid("a sprint cannot end before it starts")
		}
		res, err := t.ExecContext(ctx, `INSERT INTO sprints(project_id, name, goal, start_date, end_date, created_at) VALUES (?,?,?,?,?,?)`,
			p.ID, name, deref(in.Goal), nullStr(&startS), nullStr(&endS), s.nowMS())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		t.emit(&Event{Kind: EvSprintCreated, ProjectID: p.ID, SprintID: id, New: name})
		t.emit(&Event{Kind: EvSprintUpdated, ProjectID: p.ID, SprintID: id})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.sprintByID(ctx, s.db.R, id)
}

// UpdateSprint edits a sprint's name, goal or dates.
func (s *Service) UpdateSprint(ctx context.Context, actor *Actor, id int64, in SprintInput) (*Sprint, error) {
	sp, err := s.sprintByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, sp.ProjectID, RoleEditor); err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
			if _, err := t.ExecContext(ctx, `UPDATE sprints SET name = ? WHERE id = ?`, strings.TrimSpace(*in.Name), id); err != nil {
				return err
			}
		}
		if in.Goal != nil {
			if _, err := t.ExecContext(ctx, `UPDATE sprints SET goal = ? WHERE id = ?`, *in.Goal, id); err != nil {
				return err
			}
		}
		for col, o := range map[string]Opt[string]{"start_date": in.StartDate, "end_date": in.EndDate} {
			if !o.Set {
				continue
			}
			v, err := s.parseDateFor(actor, deref(o.Value))
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE sprints SET `+col+` = ? WHERE id = ?`, nullStr(v), id); err != nil {
				return err
			}
		}
		var start, end sql.NullString
		if err := t.QueryRowContext(ctx, `SELECT start_date, end_date FROM sprints WHERE id = ?`, id).Scan(&start, &end); err != nil {
			return err
		}
		if start.Valid && end.Valid && end.String < start.String {
			return invalid("a sprint cannot end before it starts")
		}
		t.emit(&Event{Kind: EvSprintUpdated, ProjectID: sp.ProjectID, SprintID: id})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.sprintByID(ctx, s.db.R, id)
}

// StartSprint makes a planned sprint active, recording committed points.
func (s *Service) StartSprint(ctx context.Context, actor *Actor, id int64) (*Sprint, error) {
	sp, err := s.sprintByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, sp.ProjectID, RoleEditor); err != nil {
		return nil, err
	}
	if sp.State != "planned" {
		return nil, invalid("only a planned sprint can be started")
	}
	err = s.write(ctx, actor, func(t *tx) error {
		// Re-check inside the transaction: two starts can race.
		var state string
		var active int
		if err := t.QueryRowContext(ctx, `SELECT state, (SELECT count(*) FROM sprints WHERE project_id = ? AND state = 'active') FROM sprints WHERE id = ?`,
			sp.ProjectID, id).Scan(&state, &active); err != nil {
			return err
		}
		if state != "planned" {
			return invalid("only a planned sprint can be started")
		}
		if active > 0 {
			return invalid("close the active sprint before starting another")
		}
		now := s.now()
		today := now.In(s.actorLocation(actor)).Format(dateLayout)
		start := today
		if sp.StartDate != nil && *sp.StartDate < today {
			start = *sp.StartDate
		}
		end := sp.EndDate
		if end == nil || *end < start {
			p, err := s.projectByID(ctx, t, sp.ProjectID)
			if err != nil {
				return err
			}
			d, err := time.Parse(dateLayout, start)
			if err != nil {
				return err
			}
			e := d.AddDate(0, 0, p.Settings.SprintDays-1).Format(dateLayout)
			end = &e
		}
		// Anything still sitting in a backlog status joins the board, as a
		// normal status change (history, flow reports and automations see it).
		if todo, err := s.firstStatusIn(ctx, t, sp.ProjectID, CatTodo); err == nil {
			ids, err := queryIDs(ctx, t, `SELECT i.id FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.sprint_id = ? AND st.category = 'backlog'`, id)
			if err != nil {
				return err
			}
			for _, iid := range ids {
				i, err := s.issueByID(ctx, t, iid, false)
				if err != nil {
					return err
				}
				if err := s.applyPatch(ctx, t, actor, i, IssuePatch{Status: &todo.Name}); err != nil {
					return err
				}
			}
		}
		if _, err := t.ExecContext(ctx, `UPDATE sprints SET state = 'active', started_at = ?, start_date = ?, end_date = ?,
			committed_points = (SELECT coalesce(sum(points), 0) FROM issues WHERE sprint_id = ?) WHERE id = ?`, now.UnixMilli(), start, *end, id, id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvSprintStarted, ProjectID: sp.ProjectID, SprintID: id, New: sp.Name})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.sprintByID(ctx, s.db.R, id)
}

// CloseSprint completes the active sprint. Unfinished issues move to
// moveTo: "next" (the next planned sprint, created if needed), "backlog", or
// a sprint id/name.
func (s *Service) CloseSprint(ctx context.Context, actor *Actor, id int64, moveTo string) (*Sprint, error) {
	sp, err := s.sprintByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, sp.ProjectID, RoleEditor); err != nil {
		return nil, err
	}
	if sp.State != "active" {
		return nil, invalid("only the active sprint can be closed")
	}
	if moveTo == "" {
		moveTo = "backlog"
	}
	var target *int64
	if moveTo != "backlog" {
		next, err := s.resolveSprint(ctx, s.db.R, sp.ProjectID, moveTo)
		if err != nil && strings.EqualFold(moveTo, "next") {
			next, err = s.CreateSprint(ctx, actor, itoa64(sp.ProjectID), SprintInput{})
		}
		if err != nil {
			return nil, err
		}
		if next.ID == id || next.State == "closed" {
			return nil, invalid("unfinished work must move to the backlog or a planned sprint")
		}
		target = &next.ID
	}
	err = s.write(ctx, actor, func(t *tx) error {
		now := s.nowMS()
		res, err := t.ExecContext(ctx, `UPDATE sprints SET state = 'closed', closed_at = ?,
			completed_points = (SELECT coalesce(sum(i.points), 0) FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.sprint_id = ? AND st.category = 'done')
			WHERE id = ? AND state = 'active'`, now, id, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return invalid("only the active sprint can be closed")
		}
		newName := ""
		if target != nil {
			if err := t.QueryRowContext(ctx, `SELECT name FROM sprints WHERE id = ?`, *target).Scan(&newName); err != nil {
				return err
			}
		}
		open, err := queryIDs(ctx, t, `SELECT i.id FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.sprint_id = ? AND st.category NOT IN ('done','canceled')`, id)
		if err != nil {
			return err
		}
		for _, iid := range open {
			i, err := s.issueByID(ctx, t, iid, false)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE issues SET sprint_id = ?, updated_at = ? WHERE id = ?`, nullInt(target), now, iid); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "sprint", Old: sp.Name, New: newName,
				Data: map[string]any{"carried_over": true}})
		}
		t.emit(&Event{Kind: EvSprintClosed, ProjectID: sp.ProjectID, SprintID: id, New: sp.Name,
			Data: map[string]any{"carried_over": len(open)}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.sprintByID(ctx, s.db.R, id)
}

// DeleteSprint removes a sprint that is not active; its issues return to the backlog.
func (s *Service) DeleteSprint(ctx context.Context, actor *Actor, id int64) error {
	sp, err := s.sprintByID(ctx, s.db.R, id)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, sp.ProjectID, RoleEditor); err != nil {
		return err
	}
	if sp.State == "active" {
		return invalid("close the sprint before deleting it")
	}
	return s.write(ctx, actor, func(t *tx) error {
		if sp.State == "closed" {
			var n int
			if err := t.QueryRowContext(ctx, `SELECT count(*) FROM issues WHERE sprint_id = ?`, id).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return invalid("closed sprints with issues are kept for velocity and reports")
			}
		}
		if _, err := t.ExecContext(ctx, `UPDATE issues SET sprint_id = NULL WHERE sprint_id = ?`, id); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM sprints WHERE id = ?`, id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvSprintUpdated, ProjectID: sp.ProjectID, SprintID: id})
		return nil
	})
}
