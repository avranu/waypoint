package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// IssueCategories are the kinds of work an issue can be. Optional, and
// separate from type (epic/story/task…), which is about size and hierarchy.
var IssueCategories = []string{"feature", "bugfix", "improvement", "refactor", "docs", "test", "research", "design", "ops", "security", "chore"}

var categoryAliases = map[string]string{
	"bug": "bugfix", "fix": "bugfix", "bugs": "bugfix",
	"enhancement": "improvement", "improve": "improvement", "perf": "improvement", "performance": "improvement",
	"documentation": "docs", "doc": "docs",
	"tests": "test", "testing": "test", "qa": "test",
	"infra": "ops", "infrastructure": "ops", "devops": "ops", "deploy": "ops",
	"sec": "security", "maintenance": "chore", "spike": "research", "ux": "design",
}

func normalizeIssueCategory(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if a, ok := categoryAliases[v]; ok {
		return a
	}
	return v
}

func checkIssueCategory(v string) (string, error) {
	v = normalizeIssueCategory(v)
	if v == "" || v == "none" || slices.Contains(IssueCategories, v) {
		if v == "none" {
			v = ""
		}
		return v, nil
	}
	return "", invalid("category must be one of %s", strings.Join(IssueCategories, ", "))
}

func lowerAll(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.ToLower(v)
	}
	return out
}

// normalizeAgent lowercases and drops separators, so "Haiku 5.5",
// "haiku-5-5" and "haiku55" compare equal.
func normalizeAgent(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', '.', '_', ' ':
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(s)))
}

// agentWords splits a spec into the words that must all appear.
func agentWords(spec string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(spec), func(r rune) bool { return r == ' ' || r == '/' || r == ',' }) {
		if w = normalizeAgent(w); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// Who may pick an issue up (WP-93). issue.agent says it:
//
//	""            Anyone: people and agents (the default)
//	"none"        Humans only
//	"any", a spec Agents: any agent, or the agents the spec matches (delegated)
//
// Anyone tasks are takeable by agents with next, in the one automatic order
// (being delegated counts for a little there, WP-997);
// only delegated work is taken without a person asking (runners draining
// the queue).
const (
	AgentAnyone = ""
	AgentHumans = "none"
	AgentAny    = "any"
)

// agentHumansOnly: the issue is for people, whatever anyone else says.
func agentHumansOnly(spec string) bool {
	return strings.EqualFold(strings.TrimSpace(spec), AgentHumans)
}

// agentDelegated: the issue was handed to agents on purpose ("any" or a spec).
func agentDelegated(spec string) bool { return spec != AgentAnyone && !agentHumansOnly(spec) }

// Delegated reports whether the issue was handed to agents on purpose
// (neither Anyone nor Humans only).
func (i *Issue) Delegated() bool { return agentDelegated(i.Agent) }

// issueAgentSpec is the spec agents are matched against: Anyone is open to
// any agent, Humans only to none.
func issueAgentSpec(spec string) string {
	switch {
	case agentHumansOnly(spec):
		return ""
	case spec == AgentAnyone:
		return AgentAny
	}
	return spec
}

// AgentLabel says who an issue's agent field lets take it, for text:
// "anyone", "humans only", "any agent" or the spec itself.
func AgentLabel(spec string) string {
	switch {
	case spec == AgentAnyone:
		return "anyone"
	case agentHumansOnly(spec):
		return "humans only"
	case strings.EqualFold(spec, AgentAny):
		return "any agent"
	}
	return spec
}

// sqlDelegated is the SQL for "handed to agents on purpose" on an issues row
// aliased i. Anything that read issue.agent as "not empty" means this.
const sqlDelegated = "(i.agent != '' AND i.agent != 'none')"

// sqlUnreviewed: text an agent wrote, or agent comments, that no person has
// reviewed (WP-D11).
const sqlUnreviewed = "((i.text_agent != '' AND i.reviewed_at IS NULL) OR EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id AND c.agent != '' AND c.reviewed_at IS NULL))"

// sqlApproved: a person wrote the task's text, or reviewed the agent's
// text, and it isn't outside text waiting for a person (the glossary's
// approved; WP-284). Agent comments don't unapprove a task.
const sqlApproved = "((i.text_agent = '' OR i.reviewed_at IS NOT NULL) AND i.outside = 0)"

// sqlPlanned: a published plan page names the task, or a free model's prep
// for it is ready (as plannedIssues reads them; WP-284).
const sqlPlanned = `(EXISTS (SELECT 1 FROM page_issues pgi JOIN pages pg ON pg.id = pgi.page_id WHERE pgi.issue_id = i.id AND pg.category = 'plan' AND pg.draft = 0)
	OR EXISTS (SELECT 1 FROM task_preps tp WHERE tp.issue_id = i.id AND tp.state = 'ready'))`

// AgentMatches reports whether an agent may take work for spec: "any"
// matches every agent, "" and "none" none; otherwise every word of the
// spec must be in the agent's identity ("haiku 5.5" matches
// "anthropic/claude-haiku-5-5").
func AgentMatches(spec, identity string) bool {
	spec = strings.TrimSpace(spec)
	id := normalizeAgent(strings.ReplaceAll(identity, "/", ""))
	if spec == "" || id == "" || strings.EqualFold(spec, AgentHumans) {
		return false
	}
	if strings.EqualFold(spec, "any") {
		return true
	}
	words := agentWords(spec)
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		if !strings.Contains(id, w) {
			return false
		}
	}
	return true
}

// checkAgentSpec normalizes an issue's agent field: empty or "anyone" is
// Anyone, "none" or "humans" is Humans only, "any" is any agent, and
// anything else is an agent spec.
func checkAgentSpec(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "", "anyone":
		return AgentAnyone, nil
	case "none", "human", "humans", "humans only":
		return AgentHumans, nil
	case "any":
		return AgentAny, nil
	}
	if len(v) > 100 {
		return "", invalid("agent can be at most 100 characters")
	}
	return v, nil
}

// WorkflowStep is one stage of a workflow.
type WorkflowStep struct {
	Name string `json:"name"`
	// Agent overrides the issue's agent spec for this step. "different"
	// means any matching agent except the one that did the previous step;
	// "same" means only that one.
	Agent        string `json:"agent,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

// Workflow is an ordered list of steps an issue moves through.
type Workflow struct {
	ID        int64          `json:"id"`
	ProjectID *int64         `json:"project_id"` // nil: available everywhere
	Name      string         `json:"name"`
	Steps     []WorkflowStep `json:"steps"`
	CreatedAt time.Time      `json:"created_at"`
}

// stepIndex is the position of a step by name, or -1.
func (w *Workflow) stepIndex(name string) int {
	return slices.IndexFunc(w.Steps, func(s WorkflowStep) bool { return strings.EqualFold(s.Name, name) })
}

// DefaultWorkflow is created once, for everyone, on first use.
var DefaultWorkflow = Workflow{Name: "Agent pipeline", Steps: []WorkflowStep{
	{Name: "plan", Instructions: "Read the issue and its related work. Write a short plan (approach, risks, files to touch) as a comment or a linked page. Don't change code yet."},
	{Name: "verify", Instructions: "Check the plan's claims against the code and docs; confirm each assumption holds. Correct the plan where it was wrong."},
	{Name: "implement", Instructions: "Make the change the plan describes, following the codebase's conventions. Keep the diff focused."},
	{Name: "review", Agent: "different", Instructions: "Review the change adversarially: look for bugs, missed cases and regressions, and demonstrate problems rather than guess. Record findings as comments; fix them or send the issue back a step."},
	{Name: "test", Instructions: "Run the tests and quality checks, add tests for what changed, and confirm the issue's acceptance criteria. Record the results."},
}}

const workflowCols = `id, project_id, name, steps, created_at`

func scanWorkflow(sc interface{ Scan(...any) error }) (*Workflow, error) {
	var w Workflow
	var pid sql.NullInt64
	var steps string
	var c int64
	if err := sc.Scan(&w.ID, &pid, &w.Name, &steps, &c); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(steps), &w.Steps); err != nil {
		return nil, err
	}
	w.ProjectID, w.CreatedAt = ptrInt(pid), msTime(c)
	return &w, nil
}

// ensureDefaultWorkflow creates the built-in workflow the first time it's
// needed. It is remembered, so deleting it sticks. Inside a write it uses
// that transaction (the single write connection can't be taken twice).
func (s *Service) ensureDefaultWorkflow(ctx context.Context, q queryer) error {
	var done int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM server_settings WHERE key = 'default_workflow'`).Scan(&done); err != nil || done > 0 {
		return err
	}
	steps, err := json.Marshal(DefaultWorkflow.Steps)
	if err != nil {
		return err
	}
	insert := func(t queryer) error {
		res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO server_settings(key, value) VALUES ('default_workflow', '1')`)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		_, err = t.ExecContext(ctx, `INSERT INTO workflows(project_id, name, steps, created_at) VALUES (NULL, ?, ?, ?)`,
			DefaultWorkflow.Name, string(steps), s.nowMS())
		return err
	}
	if t, ok := q.(*tx); ok {
		return insert(t)
	}
	return s.write(ctx, nil, func(t *tx) error { return insert(t) })
}

// ListWorkflows returns the workflows usable in a project (everyone's plus
// the project's own), or only everyone's when projectRef is empty.
func (s *Service) ListWorkflows(ctx context.Context, actor *Actor, projectRef string) ([]*Workflow, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if err := s.ensureDefaultWorkflow(ctx, s.db.R); err != nil {
		return nil, err
	}
	where, args := `project_id IS NULL`, []any{}
	if projectRef != "" {
		p, err := s.projectByRef(ctx, s.db.R, projectRef)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		where, args = `project_id IS NULL OR project_id = ?`, []any{p.ID}
	}
	// #nosec G202 -- where is one of two fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+workflowCols+` FROM workflows WHERE `+where+` ORDER BY project_id IS NOT NULL, lower(name)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Workflow{}
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Service) workflowByID(ctx context.Context, q queryer, id int64) (*Workflow, error) {
	w, err := scanWorkflow(q.QueryRowContext(ctx, `SELECT `+workflowCols+` FROM workflows WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("workflow")
	}
	return w, err
}

// workflowFor finds a workflow usable in a project by id or name.
func (s *Service) workflowFor(ctx context.Context, q queryer, projectID int64, ref string) (*Workflow, error) {
	if err := s.ensureDefaultWorkflow(ctx, q); err != nil {
		return nil, err
	}
	w, err := scanWorkflow(q.QueryRowContext(ctx, `SELECT `+workflowCols+` FROM workflows
		WHERE (project_id IS NULL OR project_id = ?) AND (CAST(id AS TEXT) = ? OR name = ? COLLATE NOCASE)
		ORDER BY CAST(id AS TEXT) = ? DESC, project_id IS NULL LIMIT 1`, projectID, ref, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("workflow " + ref)
	}
	return w, err
}

// WorkflowInput creates or edits a workflow.
type WorkflowInput struct {
	Name  *string         `json:"name"`
	Steps *[]WorkflowStep `json:"steps"`
}

func (in WorkflowInput) apply(w *Workflow) error {
	if in.Name != nil {
		w.Name = strings.TrimSpace(*in.Name)
	}
	if in.Steps != nil {
		w.Steps = nil
		for _, st := range *in.Steps {
			st.Name, st.Agent, st.Instructions = strings.TrimSpace(st.Name), strings.TrimSpace(st.Agent), strings.TrimSpace(st.Instructions)
			if l := strings.ToLower(st.Agent); l == "same" || l == "different" {
				st.Agent = l
			}
			if st.Name != "" {
				w.Steps = append(w.Steps, st)
			}
		}
	}
	if w.Name == "" || len(w.Name) > 80 {
		return invalid("a workflow needs a name of up to 80 characters")
	}
	if len(w.Steps) == 0 || len(w.Steps) > 20 {
		return invalid("a workflow needs 1 to 20 steps")
	}
	seen := map[string]bool{}
	for _, st := range w.Steps {
		switch {
		case len(st.Name) > 40:
			return invalid("step names can be at most 40 characters")
		case seen[strings.ToLower(st.Name)]:
			return invalid("step %q appears twice", st.Name)
		case len(st.Instructions) > 4000 || len(st.Agent) > 100:
			return invalid("step %q is too long", st.Name)
		}
		seen[strings.ToLower(st.Name)] = true
	}
	return nil
}

// requireWorkflowEdit: project owners edit their project's workflows;
// admins edit everyone's.
func (s *Service) requireWorkflowEdit(ctx context.Context, actor *Actor, projectID *int64) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	if projectID == nil {
		if !actor.isAdmin() {
			return invalid("only admins can change workflows shared by every project")
		}
		return nil
	}
	return s.requireProjectRole(ctx, s.db.R, actor, *projectID, RoleOwner)
}

// CreateWorkflow adds a workflow to a project, or for everyone when
// projectRef is empty (admins).
func (s *Service) CreateWorkflow(ctx context.Context, actor *Actor, projectRef string, in WorkflowInput) (*Workflow, error) {
	w := &Workflow{}
	if projectRef != "" {
		p, err := s.projectByRef(ctx, s.db.R, projectRef)
		if err != nil {
			return nil, err
		}
		w.ProjectID = &p.ID
	}
	if err := s.requireWorkflowEdit(ctx, actor, w.ProjectID); err != nil {
		return nil, err
	}
	if err := in.apply(w); err != nil {
		return nil, err
	}
	steps, err := json.Marshal(w.Steps)
	if err != nil {
		return nil, err
	}
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO workflows(project_id, name, steps, created_at) VALUES (?,?,?,?)`,
		nullInt(w.ProjectID), w.Name, string(steps), s.nowMS())
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, workflowAudit("workflow.create", w, fmt.Sprintf("%d steps", len(w.Steps))))
	return s.workflowByID(ctx, s.db.R, id)
}

// workflowAudit is a workflow change for the log.
func workflowAudit(action string, w *Workflow, detail string) AuditEntry {
	e := AuditEntry{Action: action, Category: AuditSettings, TargetType: "workflow", TargetID: w.ID, Target: w.Name, Detail: detail}
	if w.ProjectID != nil {
		e.ProjectID = *w.ProjectID
	}
	return e
}

// UpdateWorkflow edits a workflow. Issues on a step that no longer exists
// keep their step name until they move.
func (s *Service) UpdateWorkflow(ctx context.Context, actor *Actor, id int64, in WorkflowInput) (*Workflow, error) {
	w, err := s.workflowByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireWorkflowEdit(ctx, actor, w.ProjectID); err != nil {
		return nil, err
	}
	if err := in.apply(w); err != nil {
		return nil, err
	}
	steps, err := json.Marshal(w.Steps)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE workflows SET name = ?, steps = ? WHERE id = ?`, w.Name, string(steps), id); err != nil {
		return nil, err
	}
	names := make([]string, len(w.Steps))
	for n, st := range w.Steps {
		names[n] = st.Name
		if st.Agent != "" {
			names[n] += " (" + st.Agent + ")"
		}
	}
	s.audit(ctx, actor, workflowAudit("workflow.update", w, "steps: "+strings.Join(names, " → ")))
	return s.workflowByID(ctx, s.db.R, id)
}

// DeleteWorkflow removes a workflow; its issues leave it.
func (s *Service) DeleteWorkflow(ctx context.Context, actor *Actor, id int64) error {
	w, err := s.workflowByID(ctx, s.db.R, id)
	if err != nil {
		return err
	}
	if err := s.requireWorkflowEdit(ctx, actor, w.ProjectID); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE issues SET step = '' WHERE workflow_id = ?`, id); err != nil {
			return err
		}
		t.audit(workflowAudit("workflow.delete", w, "its issues left it"))
		_, err := t.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, id)
		return err
	})
}
