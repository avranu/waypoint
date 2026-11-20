package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// IssueTemplate is a reusable starting point for new issues in a project:
// type, title, description, labels, points and sub-tasks.
type IssueTemplate struct {
	ID          int64     `json:"id"`
	ProjectID   int64     `json:"project_id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Labels      []string  `json:"labels"`
	Points      *float64  `json:"points"`
	Subtasks    []string  `json:"subtasks"`
	CreatedAt   time.Time `json:"created_at"`
}

// TemplateInput creates or edits a template.
type TemplateInput struct {
	Name        *string      `json:"name"`
	Type        *string      `json:"type"`
	Title       *string      `json:"title"`
	Description *string      `json:"description"`
	Labels      *[]string    `json:"labels"`
	Points      Opt[float64] `json:"points"`
	Subtasks    *[]string    `json:"subtasks"`
}

const templateCols = `id, project_id, name, type, title, description, labels, points, subtasks, created_at`

func scanTemplate(sc interface{ Scan(...any) error }) (*IssueTemplate, error) {
	var t IssueTemplate
	var labels, subtasks string
	var pts sql.NullFloat64
	var c int64
	if err := sc.Scan(&t.ID, &t.ProjectID, &t.Name, &t.Type, &t.Title, &t.Description, &labels, &pts, &subtasks, &c); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(labels), &t.Labels); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(subtasks), &t.Subtasks); err != nil {
		return nil, err
	}
	if pts.Valid {
		t.Points = &pts.Float64
	}
	t.CreatedAt = msTime(c)
	return &t, nil
}

// ListTemplates returns a project's issue templates by name.
func (s *Service) ListTemplates(ctx context.Context, actor *Actor, projectRef string) ([]*IssueTemplate, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+templateCols+` FROM issue_templates WHERE project_id = ? ORDER BY lower(name)`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*IssueTemplate{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) templateByRef(ctx context.Context, q queryer, projectID int64, ref string) (*IssueTemplate, error) {
	where := `name = ? COLLATE NOCASE`
	if _, err := strconv.ParseInt(ref, 10, 64); err == nil {
		where = `(id = ? OR name = ? COLLATE NOCASE)` // an id first, then a name like "2024"
		ref2 := ref
		t, err := scanTemplate(q.QueryRowContext(ctx, `SELECT `+templateCols+` FROM issue_templates
			WHERE project_id = ? AND `+where+` ORDER BY CAST(id AS TEXT) = ? DESC LIMIT 1`, projectID, ref, ref2, ref))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFound("template " + ref)
		}
		return t, err
	}
	t, err := scanTemplate(q.QueryRowContext(ctx, `SELECT `+templateCols+` FROM issue_templates
		WHERE project_id = ? AND `+where+` LIMIT 1`, projectID, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("template " + ref)
	}
	return t, err
}

func (in *TemplateInput) apply(t *IssueTemplate) error {
	if in.Name != nil {
		t.Name = strings.TrimSpace(*in.Name)
	}
	if in.Type != nil {
		t.Type = strings.ToLower(strings.TrimSpace(*in.Type))
	}
	if in.Title != nil {
		t.Title = strings.TrimSpace(*in.Title)
	}
	if in.Description != nil {
		t.Description = *in.Description
	}
	if in.Labels != nil {
		t.Labels = *in.Labels
	}
	if in.Points.Set {
		t.Points = in.Points.Value
	}
	if in.Subtasks != nil {
		t.Subtasks = nil
		for _, st := range *in.Subtasks {
			if st = strings.TrimSpace(st); st != "" {
				t.Subtasks = append(t.Subtasks, st)
			}
		}
	}
	switch {
	case t.Name == "" || len(t.Name) > 80:
		return invalid("a template needs a name of up to 80 characters")
	case t.Type != "" && !slices.Contains(issueTypes, t.Type):
		return invalid("type must be one of %s", strings.Join(issueTypes, ", "))
	case utf8.RuneCountInString(t.Title) > maxTitleLen, len(t.Description) > maxDescriptionLen:
		return invalid("template text is too long")
	case len(t.Subtasks) > 50 || len(t.Labels) > 20:
		return invalid("a template can have at most 50 sub-tasks and 20 labels")
	}
	for _, st := range t.Subtasks {
		if utf8.RuneCountInString(st) > maxTitleLen {
			return invalid("sub-task titles can be at most %d characters", maxTitleLen)
		}
	}
	if t.Labels == nil {
		t.Labels = []string{}
	}
	if t.Subtasks == nil {
		t.Subtasks = []string{}
	}
	return nil
}

func (s *Service) saveTemplate(ctx context.Context, actor *Actor, projectRef string, id int64, in TemplateInput) (*IssueTemplate, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	t := &IssueTemplate{ProjectID: p.ID}
	if id != 0 {
		if t, err = s.templateByRef(ctx, s.db.R, p.ID, itoa64(id)); err != nil {
			return nil, err
		}
	}
	if err := in.apply(t); err != nil {
		return nil, err
	}
	labels, _ := json.Marshal(t.Labels)
	subtasks, _ := json.Marshal(t.Subtasks)
	var res sql.Result
	if id == 0 {
		res, err = s.db.W.ExecContext(ctx, `INSERT INTO issue_templates(project_id, name, type, title, description, labels, points, subtasks, created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, p.ID, t.Name, t.Type, t.Title, t.Description, string(labels), t.Points, string(subtasks), s.nowMS())
	} else {
		_, err = s.db.W.ExecContext(ctx, `UPDATE issue_templates SET name = ?, type = ?, title = ?, description = ?, labels = ?, points = ?, subtasks = ?
			WHERE id = ?`, t.Name, t.Type, t.Title, t.Description, string(labels), t.Points, string(subtasks), id)
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, invalid("a template called %q already exists", t.Name)
		}
		return nil, err
	}
	if id == 0 {
		if id, err = res.LastInsertId(); err != nil {
			return nil, err
		}
	}
	s.Hub.Publish(&Event{Kind: EvProjectUpdated, ProjectID: p.ID})
	return s.templateByRef(ctx, s.db.R, p.ID, itoa64(id))
}

// CreateTemplate adds an issue template to a project (editors).
func (s *Service) CreateTemplate(ctx context.Context, actor *Actor, projectRef string, in TemplateInput) (*IssueTemplate, error) {
	return s.saveTemplate(ctx, actor, projectRef, 0, in)
}

// UpdateTemplate edits an issue template (editors).
func (s *Service) UpdateTemplate(ctx context.Context, actor *Actor, projectRef string, id int64, in TemplateInput) (*IssueTemplate, error) {
	return s.saveTemplate(ctx, actor, projectRef, id, in)
}

// DeleteTemplate removes an issue template (editors).
func (s *Service) DeleteTemplate(ctx context.Context, actor *Actor, projectRef string, id int64) error {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return err
	}
	res, err := s.db.W.ExecContext(ctx, `DELETE FROM issue_templates WHERE id = ? AND project_id = ?`, id, p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("template")
	}
	s.Hub.Publish(&Event{Kind: EvProjectUpdated, ProjectID: p.ID})
	return nil
}

// fillFromTemplate completes an issue from its template: fields left empty
// take the template's values. It returns the sub-tasks to create.
func (s *Service) fillFromTemplate(ctx context.Context, actor *Actor, in *IssueInput) ([]string, error) {
	if in.Project == "" {
		return nil, invalid("project is required to use a template")
	}
	p, err := s.projectByRef(ctx, s.db.R, in.Project)
	if err != nil {
		return nil, err
	}
	// Don't say whether a project or template exists to someone outside it.
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, notFound("project " + in.Project)
	}
	t, err := s.templateByRef(ctx, s.db.R, p.ID, in.Template)
	if err != nil {
		return nil, err
	}
	if in.Type == "" {
		in.Type = t.Type
	}
	if strings.TrimSpace(in.Title) == "" {
		in.Title = t.Title
	}
	if in.Description == "" {
		in.Description = t.Description
	}
	if in.Points == nil {
		in.Points = t.Points
	}
	for _, l := range t.Labels {
		if !slices.ContainsFunc(in.Labels, func(x string) bool { return strings.EqualFold(x, l) }) {
			in.Labels = append(in.Labels, l)
		}
	}
	return t.Subtasks, nil
}
