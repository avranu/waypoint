package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var roleNames = map[int]string{RoleNone: "", RoleViewer: "viewer", RoleEditor: "editor", RoleOwner: "owner"}

func roleFromName(n string) (int, bool) {
	for k, v := range roleNames {
		if v == n && k != RoleNone {
			return k, true
		}
	}
	return 0, false
}

// Status categories. Every status belongs to one; categories drive the
// board, the backlog, reports and "is this done?" logic.
const (
	CatBacklog    = "backlog"
	CatTodo       = "todo"
	CatInProgress = "in_progress"
	CatDone       = "done"
	CatCanceled   = "canceled"
)

var categories = []string{CatBacklog, CatTodo, CatInProgress, CatDone, CatCanceled}

func validCategory(c string) bool {
	for _, x := range categories {
		if x == c {
			return true
		}
	}
	return false
}

// isClosed reports whether a category counts as finished work.
func isClosed(cat string) bool { return cat == CatDone || cat == CatCanceled }

// ProjectSettings are per-project behaviors with sensible defaults.
type ProjectSettings struct {
	// Mode is "kanban" (continuous flow) or "sprints".
	Mode string `json:"mode"`
	// SprintDays is the default length of a new sprint.
	SprintDays int `json:"sprint_days"`
	// VelocityWeeks is how many recent weeks forecasts average over.
	VelocityWeeks int `json:"velocity_weeks"`
	// DefaultPoints is assumed for unestimated issues in forecasts; 0 means
	// use the average of estimated issues.
	DefaultPoints float64 `json:"default_points"`
	// DoneVisibleDays keeps recently finished issues on the kanban board.
	DoneVisibleDays int `json:"done_visible_days"`
	// DefaultStatusID is where new issues start when no status is given.
	DefaultStatusID int64 `json:"default_status_id"`
	// AutoEstimate asks the AI estimate model for points on new issues.
	AutoEstimate bool `json:"auto_estimate"`
	// AutoTitle has the AI title model write a short title for a new issue
	// whose title is long text, and keeps the text as the description (WP-67).
	AutoTitle bool `json:"auto_title"`
	// AutoPrep has runners and a free model prepare the tasks at the front
	// of the agent queue before an agent takes them (WP-212, WP-ADR8).
	AutoPrep bool `json:"auto_prep"`
	// AutoRelate links new issues to clearly related ones: the same title,
	// much the same words, or a dependency the text states (WP-296).
	AutoRelate bool `json:"auto_relate"`
	// AuditPRs has an audit (WP-ADR20) asked for when an agent's or an
	// editor's pull request opens and when it is marked ready at a head no
	// audit has read; its findings become the babysitter's work and the
	// worst hold the merge (WP-355).
	AuditPRs bool `json:"audit_prs"`
	// CIRunners: this project owns the CI runners (of the forges its repos
	// are on): a runner fault Waypoint can't re-run away is filed here as a
	// task for agents (WP-589).
	CIRunners bool `json:"ci_runners,omitempty"`
	// NoSharedSetup turns off Agent setup's automatic pull requests on the
	// project's repos (WP-ADR44 §6): the shared instructions aren't
	// published to them. Its repos are still read and shown.
	NoSharedSetup bool `json:"no_shared_setup,omitempty"`
	// NoAgentInstructions turns off agent instructions for the project's
	// sessions (ADR146): neither its own nor the ones for every project are
	// added to their context.
	NoAgentInstructions bool `json:"no_agent_instructions,omitempty"`
	// NoRecurringChecks stops Waypoint filing a task when a kind of mistake
	// keeps coming up here (WP-710).
	NoRecurringChecks bool `json:"no_recurring_checks,omitempty"`
	// AgentsPaused stops agents taking new work in the project (WP-D15);
	// work they hold stays theirs.
	AgentsPaused bool `json:"agents_paused"`
	// DevEnvURL is where this project's dev environments live, with {name}
	// for the environment's name: https://{name}.dev.example.com (WP-D19).
	DevEnvURL string `json:"dev_env_url,omitempty"`
	// AutopilotPrompt and AutopilotDone word autopilot in this project
	// (WP-308): the prompt it pastes into a session it starts on a task, and
	// the note at the Stop that finished one. Empty: the admins' wording
	// (Admin → AI), else the default.
	AutopilotPrompt string `json:"autopilot_prompt,omitempty"`
	AutopilotDone   string `json:"autopilot_done,omitempty"`
	// StalledJobs is when sessions in this project are told that a
	// background job stopped making progress (WP-1045); nil: the admins'
	// policy, else the default.
	StalledJobs *StallPolicy `json:"stalled_jobs,omitempty"`
}

func (p *ProjectSettings) validate() error {
	switch {
	case p.Mode != "" && p.Mode != "kanban" && p.Mode != "sprints":
		return invalid("mode must be kanban or sprints")
	case p.SprintDays < 0 || p.SprintDays > 90:
		return invalid("sprint length must be 1-90 days")
	case p.VelocityWeeks < 0 || p.VelocityWeeks > 52:
		return invalid("velocity window must be 1-52 weeks")
	case p.DoneVisibleDays < 0 || p.DoneVisibleDays > 365:
		return invalid("done issues can stay visible for 1-365 days")
	case p.DefaultPoints < 0 || p.DefaultPoints > 1000:
		return invalid("default points must be 0-1000")
	}
	for _, part := range []struct {
		v    *string
		what string
	}{{&p.AutopilotPrompt, "prompt"}, {&p.AutopilotDone, "note"}} {
		v, err := cleanAutopilotText(*part.v, part.what, "")
		if err != nil {
			return err
		}
		*part.v = v
	}
	if p.StalledJobs != nil {
		if err := p.StalledJobs.validate(); err != nil {
			return err
		}
	}
	return validDevEnvURL(p.DevEnvURL)
}

func (p *ProjectSettings) fill() {
	if p.Mode != "sprints" {
		p.Mode = "kanban"
	}
	if p.SprintDays <= 0 {
		p.SprintDays = 14
	}
	if p.VelocityWeeks <= 0 {
		p.VelocityWeeks = 6
	}
	if p.DoneVisibleDays <= 0 {
		p.DoneVisibleDays = 14
	}
}

// Project is a collection of issues with its own statuses, labels and style.
type Project struct {
	ID          int64           `json:"id"`
	Key         string          `json:"key"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Color       string          `json:"color"`
	Icon        string          `json:"icon"`
	Background  string          `json:"background"`
	Archived    bool            `json:"archived"`
	Settings    ProjectSettings `json:"settings"`
	// Look says who chose the icon, colour and background: auto (nobody yet),
	// generated (an AI model), custom (a person) or plain (reset to default).
	Look string `json:"look"`
	// ParentID nests projects (an organisation's repos under it, say).
	ParentID  *int64    `json:"parent_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Role is the actor's role: owner, editor, viewer, or "guest" when they
	// only see issues shared with them.
	Role       string `json:"role"`
	OpenIssues int    `json:"open_issues"`
	// ActiveAt is the last change to the project or any of its issues
	// (project lists only), for sorting by recent use.
	ActiveAt *time.Time `json:"active_at,omitempty"`
	// MatchedBy says how FindProjects matched it: key, name, repo,
	// repo_name, or sub-project (under a project that matched).
	MatchedBy string    `json:"matched_by,omitempty"`
	Statuses  []Status  `json:"statuses,omitempty"`
	Labels    []Label   `json:"labels,omitempty"`
	Members   []Member  `json:"members,omitempty"`
	Sprints   []*Sprint `json:"sprints,omitempty"`
}

// Status is a workflow state within a project.
type Status struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Color    string `json:"color"`
	Position int    `json:"position"`
}

// Label tags issues within a project.
type Label struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Member is a user's role in a project.
type Member struct {
	User UserRef `json:"user"`
	Role string  `json:"role"`
}

var keyRE = lazyre.New(`^[A-Z][A-Z0-9]{1,9}$`)

// SuggestKey derives a project key from a name: initials for multi-word
// names, otherwise the first letters.
func SuggestKey(name string) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var b strings.Builder
	if len(words) >= 2 {
		for _, w := range words {
			b.WriteRune(unicode.ToUpper([]rune(w)[0]))
			if b.Len() >= 5 {
				break
			}
		}
	} else if len(words) == 1 {
		for _, r := range words[0] {
			if r < 128 {
				b.WriteRune(unicode.ToUpper(r))
			}
			if b.Len() >= 4 {
				break
			}
		}
	}
	k := b.String()
	for len(k) > 0 && !unicode.IsLetter(rune(k[0])) {
		k = k[1:]
	}
	if len(k) < 2 {
		k = (k + "PRJ")[:3]
	}
	return k
}

const projectCols = `id, key, name, description, color, icon, background, archived, settings, created_at, updated_at, parent_id, look`

func scanProject(sc interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var settings string
	var c, u int64
	var parent sql.NullInt64
	if err := sc.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &p.Color, &p.Icon, &p.Background, &p.Archived, &settings, &c, &u, &parent, &p.Look); err != nil {
		return nil, err
	}
	p.ParentID = ptrInt(parent)
	_ = json.Unmarshal([]byte(settings), &p.Settings)
	p.Settings.fill()
	p.CreatedAt, p.UpdatedAt = msTime(c), msTime(u)
	return &p, nil
}

// DefaultTags are created in every new project (migration 0011 gave them
// to projects made before this existed).
var DefaultTags = []Label{{Name: "ui", Color: "#0ea5e9"}, {Name: "backend", Color: "#64748b"}}

// TagUse is a tag name and how many issues (open or closed) carry it.
type TagUse struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	Count int    `json:"count"`
}

// ListTags returns tag names used in the projects the actor belongs to,
// most used first, so pickers can suggest tags from other projects.
func (s *Service) ListTags(ctx context.Context, actor *Actor) ([]TagUse, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	member, margs := actor.MemberProjects(RoleViewer)
	// #nosec G202 -- member is placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `WITH mine AS (SELECT id FROM labels
			WHERE ? OR project_id IN `+member+`)
		SELECT l.name, min(l.color), (SELECT count(*) FROM issue_labels il WHERE il.label_id IN
			(SELECT m.id FROM mine m JOIN labels l2 ON l2.id = m.id WHERE lower(l2.name) = lower(l.name)))
		FROM labels l WHERE l.id IN (SELECT id FROM mine)
		GROUP BY lower(l.name) ORDER BY 3 DESC, lower(l.name) LIMIT 500`, append([]any{actor.IsAdmin()}, margs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagUse{}
	for rows.Next() {
		var t TagUse
		if err := rows.Scan(&t.Name, &t.Color, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// parentProject resolves a nesting request: a project the actor can see,
// which isn't the project itself or beneath it.
func (s *Service) parentProject(ctx context.Context, q queryer, actor *Actor, childID int64, ref Opt[string]) (*int64, error) {
	if !ref.Set || ref.Value == nil || strings.TrimSpace(*ref.Value) == "" {
		return nil, nil
	}
	parent, err := s.projectByRef(ctx, q, strings.TrimSpace(*ref.Value))
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, q, actor, parent.ID, RoleViewer); err != nil {
		return nil, err
	}
	if childID != 0 {
		var cyc int
		if err := q.QueryRowContext(ctx, `WITH RECURSIVE anc(id, parent_id) AS (SELECT id, parent_id FROM projects WHERE id = ?
			UNION SELECT x.id, x.parent_id FROM projects x JOIN anc a ON x.id = a.parent_id)
			SELECT count(*) FROM anc WHERE id = ?`, parent.ID, childID).Scan(&cyc); err != nil {
			return nil, err
		}
		if cyc > 0 {
			return nil, invalid("a project can't be nested under itself or one of its sub-projects")
		}
	}
	return &parent.ID, nil
}

// projectRole is the actor's role in a project; admins are owners everywhere.
func (s *Service) projectRole(ctx context.Context, q queryer, actor *Actor, projectID int64) (int, error) {
	if actor == nil || actor.User == nil {
		return RoleNone, nil
	}
	if actor.Scope != nil {
		return actor.Scope[projectID], nil
	}
	if actor.IsAdmin() {
		return RoleOwner, nil
	}
	var role string
	err := q.QueryRowContext(ctx, `SELECT role FROM project_members WHERE project_id = ? AND user_id = ?`, projectID, actor.ID()).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return RoleNone, nil
	}
	if err != nil {
		return RoleNone, err
	}
	r, _ := roleFromName(role)
	return r, nil
}

func (s *Service) hasShareInProject(ctx context.Context, q queryer, actor *Actor, projectID int64) bool {
	if actor.Scope != nil {
		return false
	}
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM issue_shares sh JOIN issues i ON i.id = sh.issue_id
		WHERE sh.user_id = ? AND i.project_id = ?`, actor.ID(), projectID).Scan(&n)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read has share in project", "projectID", projectID, "err", err)
		}
		return false
	}
	return n > 0
}

func (s *Service) requireProjectRole(ctx context.Context, q queryer, actor *Actor, projectID int64, min int) error {
	if min > RoleViewer {
		if err := actor.CanWrite(); err != nil {
			return err
		}
	}
	r, err := s.projectRole(ctx, q, actor, projectID)
	if err != nil {
		return err
	}
	if r == RoleNone {
		return notFound("project")
	}
	if r < min {
		return ErrForbidden
	}
	return nil
}

// projectByRef finds a project by key or numeric id.
// projectByRef finds a project by key (any case), id, name, or path of
// keys and names through its parents ("ACME/APP", "Harbor/Harbor").
// Access is the caller's to check.
func (s *Service) projectByRef(ctx context.Context, q queryer, ref string) (*Project, error) {
	ref = strings.TrimSpace(ref)
	p, err := scanProject(q.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects
		WHERE key = ? COLLATE NOCASE OR CAST(id AS TEXT) = ? LIMIT 1`, ref, ref))
	if !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id, key, name, parent_id FROM projects`)
	if err != nil {
		return nil, err
	}
	var all []*Project
	for rows.Next() {
		var p Project
		var parent sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Key, &p.Name, &parent); err != nil {
			rows.Close()
			return nil, err
		}
		p.ParentID = ptrInt(parent)
		all = append(all, &p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hits := matchProjectRef(all, ref)
	switch len(hits) {
	case 0:
		return nil, notFound("project " + ref)
	case 1:
		return s.projectByID(ctx, q, hits[0].ID)
	}
	var opts []string
	for _, h := range hits {
		opts = append(opts, h.Key)
	}
	return nil, invalid("%q could be %s; use the key", ref, strings.Join(opts, " or "))
}

// looseName lowercases a name and drops spaces and punctuation.
func looseName(v string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, v)
}

// projectNamed says whether one part of a reference names p: its key or
// name, any case (names also ignoring spaces and punctuation).
func projectNamed(p *Project, part string) bool {
	return strings.EqualFold(p.Key, part) || strings.EqualFold(p.Name, part) ||
		(looseName(part) != "" && looseName(p.Name) == looseName(part))
}

// matchProjectRef finds the projects ref names among all: by key, then
// name, then a path of keys or names through parents ("ORG/APP"). Exact
// keys win over names, so a key is never ambiguous.
func matchProjectRef(all []*Project, ref string) []*Project {
	ref = strings.Trim(strings.TrimSpace(ref), "/")
	var out []*Project
	for _, p := range all {
		if strings.EqualFold(p.Key, ref) {
			return []*Project{p}
		}
	}
	byID := map[int64]*Project{}
	for _, p := range all {
		byID[p.ID] = p
	}
	parts := strings.Split(ref, "/")
	for _, p := range all {
		cur, ok := p, true
		for i := len(parts) - 1; i >= 0 && ok; i-- {
			if cur == nil || !projectNamed(cur, strings.TrimSpace(parts[i])) {
				ok = false
				break
			}
			if i > 0 {
				if cur.ParentID == nil {
					ok = false
					break
				}
				cur = byID[*cur.ParentID]
			}
		}
		if ok {
			out = append(out, p)
		}
	}
	return out
}

func (s *Service) projectByID(ctx context.Context, q queryer, id int64) (*Project, error) {
	p, err := scanProject(q.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("project")
	}
	return p, err
}

// ListProjects returns projects the actor is a member of, plus projects
// where something has been shared with them.
func (s *Service) ListProjects(ctx context.Context, actor *Actor, includeArchived bool) ([]*Project, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	// #nosec G202 -- projectCols is a constant; values are bound
	q := `SELECT ` + prefixCols("p.", projectCols) + `,
		COALESCE(m.role, CASE WHEN ? THEN 'owner' ELSE 'guest' END),
		(SELECT count(*) FROM issues i JOIN statuses st ON st.id = i.status_id
		  WHERE i.project_id = p.id AND st.category NOT IN ('done','canceled')),
		max(p.updated_at, COALESCE((SELECT max(i.updated_at) FROM issues i WHERE i.project_id = p.id), 0))
		FROM projects p
		LEFT JOIN project_members m ON m.project_id = p.id AND m.user_id = ?
		WHERE (? OR m.user_id IS NOT NULL OR EXISTS (
			SELECT 1 FROM issue_shares sh JOIN issues i ON i.id = sh.issue_id WHERE sh.user_id = ? AND i.project_id = p.id))
		  AND (? OR p.archived = 0)
		ORDER BY p.archived, lower(p.name)`
	admin := actor.IsAdmin() || actor.Scope != nil
	rows, err := s.db.R.QueryContext(ctx, q, admin, actor.ID(), admin, actor.ID(), includeArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Project{}
	for rows.Next() {
		var p Project
		var settings string
		var c, u, active int64
		var parent sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &p.Color, &p.Icon, &p.Background, &p.Archived, &settings, &c, &u, &parent, &p.Look, &p.Role, &p.OpenIssues, &active); err != nil {
			return nil, err
		}
		p.ParentID = ptrInt(parent)
		at := msTime(active)
		p.ActiveAt = &at
		_ = json.Unmarshal([]byte(settings), &p.Settings)
		p.Settings.fill()
		p.CreatedAt, p.UpdatedAt = msTime(c), msTime(u)
		if actor.Scope != nil {
			if actor.Scope[p.ID] == RoleNone {
				continue
			}
			p.Role = roleNames[actor.Scope[p.ID]]
		}
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, p := range out {
		if p.Role == "guest" {
			if p.OpenIssues, err = s.guestOpenCount(ctx, actor, p.Key); err != nil {
				return nil, err
			}
			p.ActiveAt = &p.UpdatedAt
		}
	}
	return out, nil
}

// FindProjects resolves what someone calls a project: its key, its name,
// a repo attached to it (owner/name), or that repo's name alone. Only the
// best kind of match counts (key, then name, then repo, then repo name;
// names also match ignoring case, spaces and punctuation), so a project
// called "web" wins over a repo called web. Projects nested under a match
// follow it, marked "sub-project". repoOnly matches attached repos'
// owner/name and nothing else (for a checkout's remote).
func (s *Service) FindProjects(ctx context.Context, actor *Actor, ref string, repoOnly bool) ([]*Project, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return []*Project{}, nil
	}
	all, err := s.ListProjects(ctx, actor, false)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*Project{}
	for _, p := range all {
		byID[p.ID] = p
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT project_id, repo FROM project_repos`)
	if err != nil {
		return nil, err
	}
	repos := map[int64][]string{}
	for rows.Next() {
		var id int64
		var repo string
		if err := rows.Scan(&id, &repo); err != nil {
			rows.Close()
			return nil, err
		}
		repos[id] = append(repos[id], repo)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	loose := looseName
	path := matchProjectRef(all, ref)
	tiers := []struct {
		by    string
		match func(p *Project) bool
	}{
		{"key", func(p *Project) bool { return strings.EqualFold(p.Key, ref) }},
		{"name", func(p *Project) bool { return strings.EqualFold(p.Name, ref) }},
		{"repo", func(p *Project) bool {
			return slices.ContainsFunc(repos[p.ID], func(r string) bool { return strings.EqualFold(r, ref) })
		}},
		{"path", func(p *Project) bool { return strings.Contains(ref, "/") && slices.Contains(path, p) }},
		{"repo_name", func(p *Project) bool {
			return slices.ContainsFunc(repos[p.ID], func(r string) bool { _, n := repoNamespace(r); return strings.EqualFold(n, ref) })
		}},
		{"name", func(p *Project) bool { return loose(ref) != "" && loose(p.Name) == loose(ref) }},
		{"repo_name", func(p *Project) bool {
			return loose(ref) != "" && slices.ContainsFunc(repos[p.ID], func(r string) bool { _, n := repoNamespace(r); return loose(n) == loose(ref) })
		}},
	}
	if repoOnly {
		tiers = tiers[2:3]
	}
	var out []*Project
	for _, t := range tiers {
		for _, p := range all {
			if t.match(p) {
				p.MatchedBy = t.by
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			break
		}
	}
	seen := map[int64]bool{}
	for _, p := range out {
		seen[p.ID] = true
	}
	for i := 0; i < len(out); i++ {
		for _, p := range all {
			if p.ParentID != nil && *p.ParentID == out[i].ID && !seen[p.ID] {
				seen[p.ID] = true
				p.MatchedBy = "sub-project"
				out = append(out, p)
			}
		}
	}
	if out == nil {
		out = []*Project{}
	}
	return out, nil
}

// guestOpenCount counts only the open issues a guest's shares reach.
func (s *Service) guestOpenCount(ctx context.Context, actor *Actor, key string) (int, error) {
	res, err := s.Search(ctx, actor, SearchParams{Query: "is:open", Project: key, Limit: 1})
	if err != nil {
		return 0, err
	}
	return res.Total, nil
}

func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// GetProject returns a project with its statuses, labels, members and sprints.
func (s *Service) GetProject(ctx context.Context, actor *Actor, ref string) (*Project, error) {
	p, err := s.projectByRef(ctx, s.db.R, ref)
	if err != nil {
		return nil, err
	}
	role, err := s.projectRole(ctx, s.db.R, actor, p.ID)
	if err != nil {
		return nil, err
	}
	p.Role = roleNames[role]
	if role == RoleNone {
		if !s.hasShareInProject(ctx, s.db.R, actor, p.ID) {
			return nil, notFound("project " + ref)
		}
		p.Role = "guest"
	}
	if p.Statuses, err = s.statuses(ctx, s.db.R, p.ID); err != nil {
		return nil, err
	}
	if p.Labels, err = s.labels(ctx, s.db.R, p.ID); err != nil {
		return nil, err
	}
	if p.Sprints, err = s.listSprints(ctx, s.db.R, p.ID, false); err != nil {
		return nil, err
	}
	if role == RoleNone {
		p.Members = []Member{}
		p.OpenIssues, err = s.guestOpenCount(ctx, actor, p.Key)
		return p, err
	}
	if p.Members, err = s.members(ctx, s.db.R, p.ID); err != nil {
		return nil, err
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM issues i JOIN statuses st ON st.id = i.status_id
		WHERE i.project_id = ? AND st.category NOT IN ('done','canceled')`, p.ID).Scan(&p.OpenIssues); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) statuses(ctx context.Context, q queryer, projectID int64) ([]Status, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name, category, color, position FROM statuses WHERE project_id = ? ORDER BY position, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Status{}
	for rows.Next() {
		var st Status
		if err := rows.Scan(&st.ID, &st.Name, &st.Category, &st.Color, &st.Position); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Service) labels(ctx context.Context, q queryer, projectID int64) ([]Label, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name, color FROM labels WHERE project_id = ? ORDER BY lower(name)`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Label{}
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.Name, &l.Color); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Service) members(ctx context.Context, q queryer, projectID int64) ([]Member, error) {
	rows, err := q.QueryContext(ctx, `SELECT u.id, u.username, u.display_name, u.avatar_version, m.role
		FROM project_members m JOIN users u ON u.id = m.user_id WHERE m.project_id = ?
		ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'editor' THEN 1 ELSE 2 END, lower(u.display_name)`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		var av int64
		if err := rows.Scan(&m.User.ID, &m.User.Username, &m.User.DisplayName, &av, &m.Role); err != nil {
			return nil, err
		}
		if m.User.DisplayName == "" {
			m.User.DisplayName = m.User.Username
		}
		m.User.AvatarURL = avatarURL(m.User.ID, av)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProjectInput creates or edits a project.
type ProjectInput struct {
	Key         *string          `json:"key"`
	Name        *string          `json:"name"`
	Description *string          `json:"description"`
	Color       *string          `json:"color"`
	Icon        *string          `json:"icon"`
	Background  *string          `json:"background"`
	Archived    *bool            `json:"archived"`
	Settings    *ProjectSettings `json:"settings"`
	// Parent nests the project under another (a key); null or "" un-nests.
	Parent       Opt[string] `json:"parent"`
	settingsJSON json.RawMessage
}

func (in *ProjectInput) UnmarshalJSON(b []byte) error {
	type plain ProjectInput
	var raw struct {
		plain
		Settings json.RawMessage `json:"settings"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	*in = ProjectInput(raw.plain)
	if len(raw.Settings) > 0 && string(raw.Settings) != "null" {
		var st ProjectSettings
		if err := strictUnmarshal(raw.Settings, &st); err != nil {
			return fmt.Errorf("settings: %w", err)
		}
		in.Settings, in.settingsJSON = &st, raw.Settings
	}
	return nil
}

func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

var colorRE = lazyre.New(`^#[0-9a-fA-F]{6}$`)

// defaultStatuses are what every new project starts with.
var defaultStatuses = []Status{
	{Name: "Backlog", Category: CatBacklog, Color: "#94a3b8"},
	{Name: "To Do", Category: CatTodo, Color: "#64748b"},
	{Name: "In Progress", Category: CatInProgress, Color: "#3b82f6"},
	{Name: "In Review", Category: CatInProgress, Color: "#a855f7"},
	{Name: "Done", Category: CatDone, Color: "#22c55e"},
	{Name: "Won't Do", Category: CatCanceled, Color: "#78716c"},
}

// CreateProject makes a project with default statuses and automations; the
// creator becomes its owner.
func (s *Service) CreateProject(ctx context.Context, actor *Actor, in ProjectInput) (*Project, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return nil, invalid("project name is required")
	}
	name := strings.TrimSpace(*in.Name)
	key := ""
	if in.Key != nil {
		key = strings.ToUpper(strings.TrimSpace(*in.Key))
	}
	autoKey := key == ""
	if autoKey {
		key = SuggestKey(name)
	}
	if !keyRE.MatchString(key) {
		return nil, invalid("project key must be 2-10 capital letters or digits, starting with a letter")
	}
	color := ""
	if in.Color != nil && *in.Color != "" {
		if !colorRE.MatchString(*in.Color) {
			return nil, invalid("color must be #rrggbb")
		}
		color = *in.Color
	}
	look := LookAuto
	if color != "" || deref(in.Icon) != "" || deref(in.Background) != "" {
		look = LookCustom
	}
	settings := ProjectSettings{}
	if in.Settings != nil {
		settings = *in.Settings
	}
	if in.Settings == nil || (in.settingsJSON != nil && !hasJSONKey(in.settingsJSON, "auto_title")) {
		settings.AutoTitle = true
	}
	if in.Settings == nil || (in.settingsJSON != nil && !hasJSONKey(in.settingsJSON, "auto_prep")) {
		settings.AutoPrep = true
	}
	if err := settings.validate(); err != nil {
		return nil, err
	}
	settings.fill()
	var id int64
	err := s.write(ctx, actor, func(t *tx) error {
		now := s.nowMS()
		if autoKey {
			base := key
			for n := 2; ; n++ {
				var exists int
				if err := t.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE key = ?`, key).Scan(&exists); err != nil {
					return err
				}
				if exists == 0 {
					break
				}
				key = base + itoa(n)
			}
		}
		desc, icon, bg := deref(in.Description), deref(in.Icon), deref(in.Background)
		if color == "" {
			color = defaultLook(key).Color
		}
		if look == LookAuto {
			taken, err := takenLooks(ctx, t, 0)
			if err != nil {
				return err
			}
			l := pickLook(&Project{Key: key, Name: name, Description: desc}, taken, lookChoice{})
			if err := l.validate(); err != nil {
				return err
			}
			icon, color, bg, look = l.Icon, l.Color, l.Background, LookGenerated
		}
		var by any
		if actor.ID() != 0 {
			by = actor.ID()
		}
		parent, err := s.parentProject(ctx, t, actor, 0, in.Parent)
		if err != nil {
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO projects(key, name, description, color, icon, background, look, settings, created_by, created_at, updated_at, parent_id)
			VALUES (?,?,?,?,?,?,?,'{}',?,?,?,?)`, key, name, desc, color, icon, bg, look, by, now, now, nullInt(parent))
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return invalid("project key %s is already used", key)
			}
			return err
		}
		id, _ = res.LastInsertId()
		if actor.ID() != 0 {
			if _, err := t.ExecContext(ctx, `INSERT INTO project_members(project_id, user_id, role, created_at) VALUES (?,?,'owner',?)`, id, actor.ID(), now); err != nil {
				return err
			}
		}
		var todoID, backlogID int64
		for i, st := range defaultStatuses {
			res, err := t.ExecContext(ctx, `INSERT INTO statuses(project_id, name, category, color, position) VALUES (?,?,?,?,?)`, id, st.Name, st.Category, st.Color, i)
			if err != nil {
				return err
			}
			sid, _ := res.LastInsertId()
			switch st.Category {
			case CatTodo:
				todoID = sid
			case CatBacklog:
				backlogID = sid
			}
		}
		settings.DefaultStatusID = todoID
		if settings.Mode == "sprints" {
			settings.DefaultStatusID = backlogID
		}
		b, _ := json.Marshal(settings)
		if _, err := t.ExecContext(ctx, `UPDATE projects SET settings = ? WHERE id = ?`, string(b), id); err != nil {
			return err
		}
		for _, tag := range DefaultTags {
			if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO labels(project_id, name, color) VALUES (?,?,?)`, id, tag.Name, tag.Color); err != nil {
				return err
			}
		}
		if err := s.createDefaultAutomations(ctx, t, id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvProjectCreated, ProjectID: id, New: name, Data: map[string]any{"project": id, "key": key, "name": name}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetProject(ctx, actor, itoa64(id))
}

// UpdateProject edits a project's name, style, settings or archived state.
func (s *Service) UpdateProject(ctx context.Context, actor *Actor, ref string, in ProjectInput) (*Project, error) {
	p, err := s.projectByRef(ctx, s.db.R, ref)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return nil, err
	}
	sets, args := []string{}, []any{}
	var agentsPaused *bool
	autoEstimateOn := false
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			return nil, invalid("project name is required")
		}
		sets, args = append(sets, "name = ?"), append(args, strings.TrimSpace(*in.Name))
	}
	if in.Key != nil && !strings.EqualFold(*in.Key, p.Key) {
		k := strings.ToUpper(strings.TrimSpace(*in.Key))
		if !keyRE.MatchString(k) {
			return nil, invalid("project key must be 2-10 capital letters or digits, starting with a letter")
		}
		sets, args = append(sets, "key = ?"), append(args, k)
	}
	if in.Description != nil {
		sets, args = append(sets, "description = ?"), append(args, *in.Description)
	}
	lookChanged := false
	if in.Color != nil {
		if !colorRE.MatchString(*in.Color) {
			return nil, invalid("color must be #rrggbb")
		}
		sets, args = append(sets, "color = ?"), append(args, *in.Color)
		lookChanged = lookChanged || !strings.EqualFold(*in.Color, p.Color)
	}
	if in.Icon != nil {
		sets, args = append(sets, "icon = ?"), append(args, *in.Icon)
		lookChanged = lookChanged || *in.Icon != p.Icon
	}
	if in.Background != nil {
		if len(*in.Background) > 2000 {
			return nil, invalid("background is too long")
		}
		sets, args = append(sets, "background = ?"), append(args, *in.Background)
		lookChanged = lookChanged || *in.Background != p.Background
	}
	if lookChanged {
		sets, args = append(sets, "look = ?"), append(args, LookCustom)
	}
	if in.Archived != nil {
		sets, args = append(sets, "archived = ?"), append(args, boolInt(*in.Archived))
	}
	if in.Parent.Set {
		parent, err := s.parentProject(ctx, s.db.R, actor, p.ID, in.Parent)
		if err != nil {
			return nil, err
		}
		sets, args = append(sets, "parent_id = ?"), append(args, nullInt(parent))
	}
	if in.Settings != nil {
		st := *in.Settings
		if in.settingsJSON != nil {
			st = p.Settings
			if err := strictUnmarshal(in.settingsJSON, &st); err != nil {
				return nil, invalid("settings: %v", err)
			}
		}
		if st.Mode == "" {
			st.Mode = p.Settings.Mode
		}
		if st.SprintDays == 0 {
			st.SprintDays = p.Settings.SprintDays
		}
		if st.VelocityWeeks == 0 {
			st.VelocityWeeks = p.Settings.VelocityWeeks
		}
		if st.DoneVisibleDays == 0 {
			st.DoneVisibleDays = p.Settings.DoneVisibleDays
		}
		if err := st.validate(); err != nil {
			return nil, err
		}
		st.fill()
		if st.DefaultStatusID != 0 {
			var n int
			if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM statuses WHERE id = ? AND project_id = ?`, st.DefaultStatusID, p.ID).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, invalid("default status is not in this project")
			}
		} else {
			st.DefaultStatusID = p.Settings.DefaultStatusID
		}
		b, _ := json.Marshal(st)
		sets, args = append(sets, "settings = ?"), append(args, string(b))
		if st.AgentsPaused != p.Settings.AgentsPaused {
			agentsPaused = &st.AgentsPaused
		}
		autoEstimateOn = st.AutoEstimate && !p.Settings.AutoEstimate
	}
	if len(sets) > 0 {
		err := s.write(ctx, actor, func(t *tx) error {
			if in.Key != nil && !strings.EqualFold(*in.Key, p.Key) {
				if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO issue_aliases(project_key, number, issue_id)
					SELECT ?, number, id FROM issues WHERE project_id = ?`, p.Key, p.ID); err != nil {
					return err
				}
			}
			sets2, args2 := append(sets, "updated_at = ?"), append(args, s.nowMS(), p.ID)
			_, err := t.ExecContext(ctx, `UPDATE projects SET `+strings.Join(sets2, ", ")+` WHERE id = ?`, args2...)
			if err != nil && strings.Contains(err.Error(), "UNIQUE") {
				return invalid("that project key is already used")
			}
			if err == nil {
				ev := &Event{Kind: EvProjectUpdated, ProjectID: p.ID}
				if autoEstimateOn {
					ev.Field, ev.New = "auto_estimate", "on"
				}
				t.emit(ev)
				if agentsPaused != nil {
					action, detail := "project.agents_resumed", "agents take new work here again"
					if *agentsPaused {
						action, detail = "project.agents_paused", "agents take no new work here"
					}
					t.audit(AuditEntry{Action: action, Category: AuditControl, ProjectID: p.ID, TargetType: "project",
						TargetID: p.ID, Target: p.Name, Detail: detail})
				}
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return s.GetProject(ctx, actor, itoa64(p.ID))
}

// DeleteProject permanently removes a project and its issues (owner only).
// Its repos aren't attached again by automatic projects.
func (s *Service) DeleteProject(ctx context.Context, actor *Actor, ref string) error {
	p, err := s.projectByRef(ctx, s.db.R, ref)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		rows, err := t.QueryContext(ctx, `SELECT forge_id, repo, hook_id FROM project_repos WHERE project_id = ? AND hook_id != ''`, p.ID)
		if err != nil {
			return err
		}
		var hooks []*repoHookJob
		for rows.Next() {
			var forgeID int64
			j := &repoHookJob{}
			if err := rows.Scan(&forgeID, &j.repo, &j.hookID); err != nil {
				rows.Close()
				return err
			}
			if f, err := s.forgeByID(ctx, forgeID); err == nil {
				j.remove = f
				hooks = append(hooks, j)
			}
		}
		rows.Close()
		for _, j := range hooks {
			t.emit(withHint(&Event{Kind: EvRepoHook}, j))
		}
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO forge_repo_ignores(forge_id, repo, created_at)
			SELECT pr.forge_id, pr.repo, ? FROM project_repos pr WHERE pr.project_id = ? AND NOT EXISTS (
				SELECT 1 FROM project_repos o WHERE o.forge_id = pr.forge_id AND lower(o.repo) = lower(pr.repo) AND o.project_id != pr.project_id)`,
			s.nowMS(), p.ID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM issue_search WHERE rowid IN (SELECT id FROM issues WHERE project_id = ?)`, p.ID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM issues WHERE project_id = ?`, p.ID); err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, p.ID)
		return err
	})
}

// SetMember adds a user to a project or changes their role.
func (s *Service) SetMember(ctx context.Context, actor *Actor, projectRef, userRef, role string) error {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return err
	}
	if _, ok := roleFromName(role); !ok {
		return invalid("role must be owner, editor or viewer")
	}
	u, err := s.resolveUser(ctx, s.db.R, actor, userRef)
	if err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		if role != "owner" {
			if err := s.ensureAnotherOwner(ctx, t, p.ID, u.ID); err != nil {
				return err
			}
		}
		var was string
		_ = t.QueryRowContext(ctx, `SELECT role FROM project_members WHERE project_id = ? AND user_id = ?`, p.ID, u.ID).Scan(&was)
		if _, err := t.ExecContext(ctx, `INSERT INTO project_members(project_id, user_id, role, created_at) VALUES (?,?,?,?)
			ON CONFLICT(project_id, user_id) DO UPDATE SET role = excluded.role`, p.ID, u.ID, role, s.nowMS()); err != nil {
			return err
		}
		if was != role {
			detail := "added " + u.Name() + " as " + role
			if was != "" {
				detail = u.Name() + " is now " + role + " (was " + was + ")"
			}
			t.audit(AuditEntry{Action: "member.set", Category: AuditAccess, ProjectID: p.ID, TargetType: "user", TargetID: u.ID,
				Target: u.Name(), Detail: detail})
		}
		t.emit(&Event{Kind: EvProjectMember, ProjectID: p.ID, Field: "member", Old: u.Username, New: role, Data: map[string]any{"user_id": u.ID}})
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: p.ID})
		return nil
	})
}

func (s *Service) ensureAnotherOwner(ctx context.Context, t *tx, projectID, userID int64) error {
	var owners, isOwner int
	if err := t.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(user_id = ?), 0) FROM project_members WHERE project_id = ? AND role = 'owner'`, userID, projectID).Scan(&owners, &isOwner); err != nil {
		return err
	}
	if isOwner == 1 && owners == 1 {
		return invalid("a project needs at least one owner")
	}
	return nil
}

// RemoveMember removes a user from a project.
func (s *Service) RemoveMember(ctx context.Context, actor *Actor, projectRef, userRef string) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return err
	}
	u, err := s.resolveUser(ctx, s.db.R, actor, userRef)
	if err != nil {
		return err
	}
	if u.ID != actor.ID() {
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
			return err
		}
	}
	return s.write(ctx, actor, func(t *tx) error {
		if err := s.ensureAnotherOwner(ctx, t, p.ID, u.ID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM project_members WHERE project_id = ? AND user_id = ?`, p.ID, u.ID); err != nil {
			return err
		}
		detail := "removed " + u.Name()
		if u.ID == actor.ID() {
			detail = "left the project"
		}
		t.audit(AuditEntry{Action: "member.remove", Category: AuditAccess, ProjectID: p.ID, TargetType: "user", TargetID: u.ID,
			Target: u.Name(), Detail: detail})
		t.emit(&Event{Kind: EvProjectMember, ProjectID: p.ID, Field: "member", Old: u.Username, Data: map[string]any{"user_id": u.ID}})
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: p.ID})
		return nil
	})
}

// StatusInput creates or edits a status.
type StatusInput struct {
	Name     *string `json:"name"`
	Category *string `json:"category"`
	Color    *string `json:"color"`
	Position *int    `json:"position"`
}

// CreateStatus adds a status to a project.
func (s *Service) CreateStatus(ctx context.Context, actor *Actor, projectRef string, in StatusInput) (*Status, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return nil, err
	}
	name := deref(in.Name)
	if name == "" {
		return nil, invalid("status name is required")
	}
	cat := CatTodo
	if in.Category != nil {
		cat = *in.Category
	}
	if !validCategory(cat) {
		return nil, invalid("category must be one of %s", strings.Join(categories, ", "))
	}
	color := deref(in.Color)
	var st Status
	err = s.write(ctx, actor, func(t *tx) error {
		pos := 0
		if in.Position != nil {
			pos = *in.Position
			if _, err := t.ExecContext(ctx, `UPDATE statuses SET position = position + 1 WHERE project_id = ? AND position >= ?`, p.ID, pos); err != nil {
				return err
			}
		} else {
			if err := t.QueryRowContext(ctx, `SELECT coalesce(max(position), -1) + 1 FROM statuses WHERE project_id = ?`, p.ID).Scan(&pos); err != nil {
				return err
			}
		}
		res, err := t.ExecContext(ctx, `INSERT INTO statuses(project_id, name, category, color, position) VALUES (?,?,?,?,?)`, p.ID, name, cat, color, pos)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return invalid("a status named %q already exists", name)
			}
			return err
		}
		id, _ := res.LastInsertId()
		st = Status{ID: id, Name: name, Category: cat, Color: color, Position: pos}
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: p.ID})
		return nil
	})
	return &st, err
}

// UpdateStatus renames, recolors, recategorizes or moves a status.
func (s *Service) UpdateStatus(ctx context.Context, actor *Actor, statusID int64, in StatusInput) error {
	var projectID int64
	var oldPos int
	if err := s.db.R.QueryRowContext(ctx, `SELECT project_id, position FROM statuses WHERE id = ?`, statusID).Scan(&projectID, &oldPos); err != nil {
		return notFound("status")
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, projectID, RoleOwner); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		if in.Name != nil {
			if strings.TrimSpace(*in.Name) == "" {
				return invalid("status name is required")
			}
			if _, err := t.ExecContext(ctx, `UPDATE statuses SET name = ? WHERE id = ?`, strings.TrimSpace(*in.Name), statusID); err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					return invalid("a status with that name already exists")
				}
				return err
			}
		}
		if in.Category != nil {
			if !validCategory(*in.Category) {
				return invalid("category must be one of %s", strings.Join(categories, ", "))
			}
			if _, err := t.ExecContext(ctx, `UPDATE statuses SET category = ? WHERE id = ?`, *in.Category, statusID); err != nil {
				return err
			}
			now := s.nowMS()
			q, args := `UPDATE issues SET resolved_at = NULL WHERE status_id = ?`, []any{statusID}
			if isClosed(*in.Category) {
				q, args = `UPDATE issues SET resolved_at = coalesce(resolved_at, ?) WHERE status_id = ?`, []any{now, statusID}
			}
			if _, err := t.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
		if in.Color != nil {
			if _, err := t.ExecContext(ctx, `UPDATE statuses SET color = ? WHERE id = ?`, *in.Color, statusID); err != nil {
				return err
			}
		}
		if in.Position != nil && *in.Position != oldPos {
			if err := s.reorderStatus(ctx, t, projectID, statusID, *in.Position); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: projectID})
		return nil
	})
}

func (s *Service) reorderStatus(ctx context.Context, t *tx, projectID, statusID int64, pos int) error {
	sts, err := s.statuses(ctx, t, projectID)
	if err != nil {
		return err
	}
	ids := []int64{}
	for _, st := range sts {
		if st.ID != statusID {
			ids = append(ids, st.ID)
		}
	}
	pos = min(max(pos, 0), len(ids))
	ids = append(ids[:pos], append([]int64{statusID}, ids[pos:]...)...)
	for i, id := range ids {
		if _, err := t.ExecContext(ctx, `UPDATE statuses SET position = ? WHERE id = ?`, i, id); err != nil {
			return err
		}
	}
	return nil
}

// DeleteStatus removes a status, moving its issues to another status.
func (s *Service) DeleteStatus(ctx context.Context, actor *Actor, statusID, moveTo int64) error {
	var projectID int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT project_id FROM statuses WHERE id = ?`, statusID).Scan(&projectID); err != nil {
		return notFound("status")
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, projectID, RoleOwner); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		var n int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM statuses WHERE project_id = ?`, projectID).Scan(&n); err != nil {
			return err
		}
		if n <= 1 {
			return invalid("a project needs at least one status")
		}
		ids, err := queryIDs(ctx, t, `SELECT id FROM issues WHERE status_id = ?`, statusID)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			var target string
			err := t.QueryRowContext(ctx, `SELECT name FROM statuses WHERE id = ? AND project_id = ? AND id != ?`, moveTo, projectID, statusID).Scan(&target)
			if errors.Is(err, sql.ErrNoRows) {
				return invalid("choose another status in this project to move its %d issues to", len(ids))
			} else if err != nil {
				return err
			}
			for _, id := range ids {
				i, err := s.issueByID(ctx, t, id, false)
				if err != nil {
					return err
				}
				if err := s.applyPatch(ctx, t, actor, i, IssuePatch{Status: &target}); err != nil {
					return err
				}
			}
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM statuses WHERE id = ?`, statusID); err != nil {
			return err
		}
		p, err := s.projectByID(ctx, t, projectID)
		if err != nil {
			return err
		}
		if p.Settings.DefaultStatusID == statusID {
			var first int64
			if err := t.QueryRowContext(ctx, `SELECT id FROM statuses WHERE project_id = ? ORDER BY position LIMIT 1`, projectID).Scan(&first); err != nil {
				return err
			}
			p.Settings.DefaultStatusID = first
			b, err := json.Marshal(p.Settings)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE projects SET settings = ? WHERE id = ?`, string(b), projectID); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: projectID})
		return nil
	})
}

// LabelInput creates or edits a label.
type LabelInput struct {
	Name  *string `json:"name"`
	Color *string `json:"color"`
}

// CreateLabel adds a label to a project.
func (s *Service) CreateLabel(ctx context.Context, actor *Actor, projectRef string, in LabelInput) (*Label, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	name := deref(in.Name)
	if name == "" {
		return nil, invalid("label name is required")
	}
	color := "#64748b"
	if in.Color != nil && colorRE.MatchString(*in.Color) {
		color = *in.Color
	}
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO labels(project_id, name, color) VALUES (?,?,?)`, p.ID, name, color)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, invalid("label %q already exists", name)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	s.Hub.Publish(&Event{Kind: EvProjectUpdated, ProjectID: p.ID})
	return &Label{ID: id, Name: name, Color: color}, nil
}

// UpdateLabel renames or recolors a label.
func (s *Service) UpdateLabel(ctx context.Context, actor *Actor, labelID int64, in LabelInput) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	var projectID int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT project_id FROM labels WHERE id = ?`, labelID).Scan(&projectID); err != nil {
		return notFound("label")
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, projectID, RoleEditor); err != nil {
		return err
	}
	if in.Name != nil {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE labels SET name = ? WHERE id = ?`, strings.TrimSpace(*in.Name), labelID); err != nil {
			return invalid("a label with that name already exists")
		}
	}
	if in.Color != nil && colorRE.MatchString(*in.Color) {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE labels SET color = ? WHERE id = ?`, *in.Color, labelID); err != nil {
			return err
		}
	}
	s.Hub.Publish(&Event{Kind: EvProjectUpdated, ProjectID: projectID})
	return nil
}

// DeleteLabel removes a label from a project and its issues.
func (s *Service) DeleteLabel(ctx context.Context, actor *Actor, labelID int64) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	var projectID int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT project_id FROM labels WHERE id = ?`, labelID).Scan(&projectID); err != nil {
		return notFound("label")
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, projectID, RoleEditor); err != nil {
		return err
	}
	_, err := s.db.W.ExecContext(ctx, `DELETE FROM labels WHERE id = ?`, labelID)
	s.Hub.Publish(&Event{Kind: EvProjectUpdated, ProjectID: projectID})
	return err
}

func itoa(n int) string { return strconv.Itoa(n) }

// hasJSONKey reports whether a JSON object names key at its top level.
func hasJSONKey(obj json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(obj, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
