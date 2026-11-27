package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

// Automation is a rule: when Trigger happens and the issue matches If, run
// the steps in Actions. A project's automations see that project's events;
// an admin automation (ProjectID 0) sees every project's and the system's.
type Automation struct {
	ID        int64      `json:"id"`
	ProjectID int64      `json:"project_id"` // 0 for an admin automation
	Admin     bool       `json:"admin"`
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	Rule      Rule       `json:"rule"`
	RunCount  int        `json:"run_count"`
	LastRunAt *time.Time `json:"last_run_at"`
	LastError string     `json:"last_error"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// HookURL is the address of an incoming-webhook automation; only the
	// people who manage it see it.
	HookURL string `json:"hook_url,omitempty"`
	// Durable: its runs are carried by a worker (long steps).
	Durable bool `json:"durable"`
	// SavedBy is the person who last saved it (WP-D18): the owner of the
	// runners its steps may use. Nil when an agent saved it, until a person
	// saves it again.
	SavedBy *UserRef `json:"saved_by"`
	// Runners says what that person's runners look like to an automation
	// with runner steps; nil for the others.
	Runners *AutomationRunners `json:"runners,omitempty"`
	// SessionID: a follow-up (WP-D22), bound to an agent session; only the
	// session's person sees or runs it, and no project list shows it.
	SessionID int64 `json:"session_id,omitempty"`
	// RuleAgent is the agent that wrote the current rule ("agent" when it
	// wasn't named); empty when a person did (WP-560).
	RuleAgent string `json:"rule_agent,omitempty"`
	// Risk is its provenance risk (WP-ADR41) when it isn't low.
	Risk *ProvenanceRisk `json:"risk,omitempty"`

	createdBy int64
	savedBy   int64
	hookToken string
}

func scanAutomation(sc interface{ Scan(...any) error }) (*Automation, error) {
	var a Automation
	var rule string
	var last sql.NullInt64
	var c, u int64
	if err := sc.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Enabled, &rule, &a.RunCount, &last, &a.LastError, &c, &u, &a.createdBy, &a.hookToken, &a.savedBy, &a.SessionID, &a.RuleAgent); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(rule), &a.Rule); err != nil {
		return nil, err
	}
	a.LastRunAt, a.CreatedAt, a.UpdatedAt = ptrTime(last), msTime(c), msTime(u)
	a.Admin = a.ProjectID == 0 && a.SessionID == 0
	a.Durable = a.Rule.isDurable()
	if r := automationRisk(&a); r.Level != RiskLow {
		a.Risk = r
	}
	return &a, nil
}

const automationCols = `id, coalesce(project_id, 0), name, enabled, rule, run_count, last_run_at, last_error, created_at, updated_at, coalesce(created_by, 0), hook_token, coalesce(saved_by, 0), coalesce(session_id, 0), rule_agent`

// forOwners adds what only the people who manage an automation see.
func (s *Service) forOwners(a *Automation) {
	if a.hookToken != "" {
		a.HookURL = s.hookURL(a.hookToken)
	}
}

// ListAutomations returns a project's automations.
func (s *Service) ListAutomations(ctx context.Context, actor *Actor, projectRef string) ([]*Automation, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	list, err := s.automations(ctx, s.db.R, p.ID, false)
	if err != nil {
		return nil, err
	}
	owner := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner) == nil
	for _, a := range list {
		s.fillOwner(ctx, a)
		if owner {
			s.forOwners(a)
		} else {
			redactRule(&a.Rule)
		}
	}
	return list, nil
}

// ListAdminAutomations returns the admin automations (admins only).
func (s *Service) ListAdminAutomations(ctx context.Context, actor *Actor) ([]*Automation, error) {
	if err := s.requireAdminHuman(actor); err != nil {
		return nil, err
	}
	list, err := s.adminAutomations(ctx, false)
	for _, a := range list {
		s.fillOwner(ctx, a)
		s.forOwners(a)
	}
	return list, err
}

// GetAutomation returns one automation the actor may see.
func (s *Service) GetAutomation(ctx context.Context, actor *Actor, id int64) (*Automation, error) {
	a, err := s.automationFor(ctx, actor, id, RoleViewer)
	if err != nil {
		return nil, err
	}
	if a.ProjectID == 0 {
		if err := actor.requireHuman("admin automations"); err != nil {
			return nil, err
		}
	}
	s.fillOwner(ctx, a)
	if a.ProjectID == 0 || a.SessionID != 0 || s.requireProjectRole(ctx, s.db.R, actor, a.ProjectID, RoleOwner) == nil {
		s.forOwners(a)
	} else {
		redactRule(&a.Rule)
	}
	return a, nil
}

// redactURL keeps a URL's scheme and host, enough to say where it goes.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(hidden)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

func (s *Service) automations(ctx context.Context, q queryer, projectID int64, enabledOnly bool) ([]*Automation, error) {
	where := "project_id = ? AND session_id IS NULL"
	if enabledOnly {
		where += " AND enabled = 1"
	}
	rows, err := q.QueryContext(ctx, `SELECT `+automationCols+` FROM automations WHERE `+where+` ORDER BY position, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Automation{}
	for rows.Next() {
		a, err := scanAutomation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AutomationInput creates or edits an automation.
type AutomationInput struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
	Rule    *Rule   `json:"rule"`
}

// CreateAutomation adds a rule to a project (owners only).
func (s *Service) CreateAutomation(ctx context.Context, actor *Actor, projectRef string, in AutomationInput) (*Automation, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return nil, err
	}
	return s.createAutomation(ctx, actor, p.ID, in)
}

// CreateAdminAutomation adds an automation outside any project: it sees every
// project's events and the system's. Admins (people) only.
func (s *Service) CreateAdminAutomation(ctx context.Context, actor *Actor, in AutomationInput) (*Automation, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if err := s.requireAdminHuman(actor); err != nil {
		return nil, err
	}
	return s.createAutomation(ctx, actor, 0, in)
}

func (s *Service) requireAdminHuman(actor *Actor) error {
	if !actor.isAdmin() {
		return forbidden("admin automations are for admins")
	}
	return actor.requireHuman("admin automations")
}

func (s *Service) createAutomation(ctx context.Context, actor *Actor, projectID int64, in AutomationInput) (*Automation, error) {
	if in.Rule == nil {
		return nil, invalid("rule is required")
	}
	if err := in.Rule.validateFor(projectID == 0); err != nil {
		return nil, err
	}
	name := deref(in.Name)
	if name == "" {
		name = "When " + strings.ReplaceAll(in.Rule.Trigger.Event, "_", " ")
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	hook := ""
	if in.Rule.Trigger.Event == "webhook" {
		hook = newToken("h")
	}
	now := s.nowMS()
	var pid any
	if projectID != 0 {
		pid = projectID
	}
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO automations(project_id, name, enabled, rule, position, created_by, saved_by, created_at, updated_at, hook_token, rule_agent)
		VALUES (?,?,?,?,(SELECT coalesce(max(position), 0) + 1 FROM automations WHERE project_id IS ?),?,?,?,?,?,?)`,
		pid, name, boolInt(enabled), ruleJSON(*in.Rule), pid, nullUser(actor.id()), saverOf(actor), now, now, hook, ruleAgentOf(actor))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	detail := "turned on"
	if !enabled {
		detail = "added, off"
	}
	s.audit(ctx, actor, AuditEntry{Action: "automation.create", Category: AuditSettings, ProjectID: projectID, TargetType: "automation",
		TargetID: id, Target: name, Detail: detail})
	a, err := scanAutomation(s.db.W.QueryRowContext(ctx, `SELECT `+automationCols+` FROM automations WHERE id = ?`, id))
	if err == nil {
		s.fillOwner(ctx, a)
		s.forOwners(a)
	}
	return a, err
}

// saverOf is what an automation records as its saver: the person acting, or
// nobody when an agent is, so an agent can't point an automation at a
// person's runners.
func saverOf(actor *Actor) any {
	if actor.IsAgent() {
		return nil
	}
	return nullUser(actor.id())
}

// ruleAgentOf is what an automation records as its rule's author when
// actor saves a rule: the agent, or "" for a person.
func ruleAgentOf(actor *Actor) string {
	if !actor.IsAgent() {
		return ""
	}
	return cmp(actor.agentName(), "agent")
}

// automationForManage loads an automation the actor may edit: a project's
// owners, or admins (people) for admin automations.
func (s *Service) automationForManage(ctx context.Context, actor *Actor, id int64) (*Automation, error) {
	a, err := s.automationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.SessionID != 0 {
		if !a.ownedBy(actor) {
			return nil, notFound("automation")
		}
		return a, actor.requireHuman("follow-ups")
	}
	if a.ProjectID == 0 {
		if !actor.isAdmin() {
			return nil, notFound("automation")
		}
		if err := actor.requireHuman("admin automations"); err != nil {
			return nil, err
		}
		return a, nil
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, a.ProjectID, RoleOwner); err != nil {
		return nil, err
	}
	return a, nil
}

// UpdateAutomation edits or enables/disables an automation.
func (s *Service) UpdateAutomation(ctx context.Context, actor *Actor, id int64, in AutomationInput) (*Automation, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	a, err := s.automationForManage(ctx, actor, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, notFound("automation")
		}
		return nil, err
	}
	was, oldName := a.Enabled, a.Name
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		a.Name = strings.TrimSpace(*in.Name)
	}
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	hook := a.hookToken
	if in.Rule != nil {
		if err := in.Rule.validateFor(a.Admin); err != nil {
			return nil, err
		}
		if a.SessionID != 0 && in.Rule.Trigger.Event != followUpTrigger {
			return nil, invalid("a follow-up runs when its agent finishes; its trigger can't change")
		}
		a.Rule = *in.Rule
		a.RuleAgent = ruleAgentOf(actor)
		switch {
		case a.Rule.Trigger.Event != "webhook":
			hook = ""
		case hook == "":
			hook = newToken("h")
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE automations SET name = ?, enabled = ?, rule = ?, hook_token = ?, saved_by = ?, rule_agent = ?, updated_at = ? WHERE id = ?`,
		a.Name, boolInt(a.Enabled), ruleJSON(a.Rule), hook, saverOf(actor), a.RuleAgent, s.nowMS(), id); err != nil {
		return nil, err
	}
	var changes []string
	if in.Rule != nil {
		changes = append(changes, "edited the rule")
	}
	if a.Name != oldName {
		changes = append(changes, "renamed it from “"+oldName+"”")
	}
	if a.Enabled != was {
		changes = append(changes, map[bool]string{true: "turned it on", false: "turned it off"}[a.Enabled])
	}
	if len(changes) > 0 {
		s.audit(ctx, actor, AuditEntry{Action: "automation.update", Category: AuditSettings, ProjectID: a.ProjectID, TargetType: "automation",
			TargetID: id, Target: a.Name, Detail: strings.Join(changes, ", ")})
	}
	out, err := scanAutomation(s.db.W.QueryRowContext(ctx, `SELECT `+automationCols+` FROM automations WHERE id = ?`, id))
	if err == nil {
		s.fillOwner(ctx, out)
		s.forOwners(out)
	}
	return out, err
}

// DeleteAutomation removes an automation.
func (s *Service) DeleteAutomation(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	a, err := s.automationForManage(ctx, actor, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("automation")
		}
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE session_messages SET state = 'cancelled' WHERE state IN ('queued','sending')
			AND step_key != '' AND CAST(substr(step_key, 1, instr(step_key, ':') - 1) AS INTEGER) IN (SELECT id FROM automation_runs WHERE automation_id = ?)`, id); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM automations WHERE id = ?`, id); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "automation.delete", Category: AuditSettings, ProjectID: a.ProjectID, TargetType: "automation",
			TargetID: id, Target: a.Name, Detail: "deleted the rule"})
		return nil
	})
}
