package core

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/wql"
)

// View is a saved query, shown in the sidebar when pinned.
type View struct {
	ID         int64     `json:"id"`
	ProjectID  *int64    `json:"project_id"`
	ProjectKey string    `json:"project_key,omitempty"`
	Name       string    `json:"name"`
	Query      string    `json:"query"`
	Layout     string    `json:"layout"` // list, board
	Pinned     bool      `json:"pinned"`
	Position   int       `json:"position"`
	CreatedAt  time.Time `json:"created_at"`
}

// ViewInput creates or edits a view.
type ViewInput struct {
	Project *string `json:"project"`
	Name    *string `json:"name"`
	Query   *string `json:"query"`
	Layout  *string `json:"layout"`
	Pinned  *bool   `json:"pinned"`
}

// ListViews returns the actor's saved views.
func (s *Service) ListViews(ctx context.Context, actor *Actor) ([]View, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT v.id, v.project_id, coalesce(p.key, ''), v.name, v.query, v.layout, v.pinned, v.position, v.created_at
		FROM saved_views v LEFT JOIN projects p ON p.id = v.project_id WHERE v.user_id = ? ORDER BY v.position, v.id`, actor.ID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []View{}
	for rows.Next() {
		var v View
		var pid sql.NullInt64
		var c int64
		if err := rows.Scan(&v.ID, &pid, &v.ProjectKey, &v.Name, &v.Query, &v.Layout, &v.Pinned, &v.Position, &c); err != nil {
			return nil, err
		}
		v.ProjectID, v.CreatedAt = ptrInt(pid), msTime(c)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) validateView(in ViewInput) error {
	if in.Query != nil {
		if _, err := wql.Parse(*in.Query); err != nil {
			return invalid("%s", err.Error())
		}
	}
	if in.Layout != nil && *in.Layout != "list" && *in.Layout != "board" {
		return invalid("layout must be list or board")
	}
	return nil
}

// CreateView saves a query.
func (s *Service) CreateView(ctx context.Context, actor *Actor, in ViewInput) (*View, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if err := s.validateView(in); err != nil {
		return nil, err
	}
	name := deref(in.Name)
	if name == "" {
		return nil, invalid("view name is required")
	}
	var pid any
	if in.Project != nil && *in.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, *in.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		pid = p.ID
	}
	layout := "list"
	if in.Layout != nil {
		layout = *in.Layout
	}
	pinned := in.Pinned == nil || *in.Pinned
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO saved_views(user_id, project_id, name, query, layout, pinned, position, created_at)
		VALUES (?,?,?,?,?,?,(SELECT coalesce(max(position), 0) + 1 FROM saved_views WHERE user_id = ?),?)`,
		actor.ID(), pid, name, strings.TrimSpace(deref(in.Query)), layout, boolInt(pinned), actor.ID(), s.nowMS())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	views, err := s.ListViews(ctx, actor)
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		if v.ID == id {
			return &v, nil
		}
	}
	return nil, notFound("view")
}

// UpdateView edits a saved view.
func (s *Service) UpdateView(ctx context.Context, actor *Actor, id int64, in ViewInput) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	if err := s.validateView(in); err != nil {
		return err
	}
	var owner int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT user_id FROM saved_views WHERE id = ?`, id).Scan(&owner); err != nil || owner != actor.ID() {
		return notFound("view")
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE saved_views SET name = ? WHERE id = ?`, strings.TrimSpace(*in.Name), id); err != nil {
			return err
		}
	}
	if in.Query != nil {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE saved_views SET query = ? WHERE id = ?`, strings.TrimSpace(*in.Query), id); err != nil {
			return err
		}
	}
	if in.Layout != nil {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE saved_views SET layout = ? WHERE id = ?`, *in.Layout, id); err != nil {
			return err
		}
	}
	if in.Pinned != nil {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE saved_views SET pinned = ? WHERE id = ?`, boolInt(*in.Pinned), id); err != nil {
			return err
		}
	}
	return nil
}

// DeleteView removes a saved view.
func (s *Service) DeleteView(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	res, err := s.db.W.ExecContext(ctx, `DELETE FROM saved_views WHERE id = ? AND user_id = ?`, id, actor.ID())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("view")
	}
	return nil
}
