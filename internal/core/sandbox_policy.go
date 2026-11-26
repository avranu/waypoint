package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sandbox levels, strongest first (WP-ADR28 decision 2).
const (
	SandboxVM        = "vm"
	SandboxContained = "contained"
	SandboxTool      = "tool"
	SandboxOff       = "off"
)

// sandboxRank orders the levels by strength.
var sandboxRank = map[string]int{SandboxOff: 0, SandboxTool: 1, SandboxContained: 2, SandboxVM: 3}

// NotifySandbox is the notification kind of a sandbox policy change.
const NotifySandbox = "sandbox"

// EvSandboxPolicy: a project's or machine's sandbox policy changed. Data:
// scope (project, machine), target (id), name, changes ([{field, old,
// new}]), loosens, reason, runner_owner (machine only).
const EvSandboxPolicy = "sandbox.policy_changed"

// sandboxReauthFor is how long a password check lets a person loosen the
// sandbox.
const sandboxReauthFor = 10 * time.Minute

var (
	// ErrSandboxUI refuses every actor but a person in the browser.
	ErrSandboxUI = fmt.Errorf("sandbox settings are changed in the Waypoint UI: %w", ErrForbidden)
	// ErrReauth asks the person for their password again; the web app
	// shows a password prompt and retries.
	ErrReauth = fmt.Errorf("enter your password again to loosen the sandbox: %w", ErrForbidden)
)

// inBrowser says whether the actor is a person in the Waypoint UI: a
// browser session, not a token, MCP, an agent or an automation.
func (a *Actor) inBrowser() bool {
	return a.Person() && a.SessionHash != "" && !a.ReadOnly
}

// Reauth checks the person's password again and records it on their
// browser session, so they may loosen the sandbox for the next ten
// minutes. It returns when that ends.
func (s *Service) Reauth(ctx context.Context, actor *Actor, password string) (time.Time, error) {
	if !actor.inBrowser() {
		return time.Time{}, fmt.Errorf("re-entering your password is done in the Waypoint UI: %w", ErrForbidden)
	}
	u, err := s.userByID(ctx, s.db.R, actor.ID())
	if err != nil {
		return time.Time{}, err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return time.Time{}, invalid("wrong password")
	}
	now := s.nowMS()
	err = s.write(ctx, actor, func(t *tx) error {
		_, err := t.ExecContext(ctx, `UPDATE sessions SET reauth_at = ? WHERE token_hash = ? AND user_id = ?`, now, actor.SessionHash, u.ID)
		return err
	})
	return msTime(now).Add(sandboxReauthFor), err
}

// ReauthUntil is when the actor's last password check stops counting, if
// it still counts.
func (s *Service) ReauthUntil(ctx context.Context, actor *Actor) *time.Time {
	if !actor.inBrowser() {
		return nil
	}
	var at sql.NullInt64
	if s.db.R.QueryRowContext(ctx, `SELECT reauth_at FROM sessions WHERE token_hash = ? AND user_id = ?`, actor.SessionHash, actor.ID()).Scan(&at) != nil || !at.Valid {
		return nil
	}
	until := msTime(at.Int64).Add(sandboxReauthFor)
	if !until.After(s.now()) {
		return nil
	}
	return &until
}

// sandboxWriter refuses anyone but a person in the browser, and a change
// that loosens without a recent password check or a reason.
func (s *Service) sandboxWriter(ctx context.Context, actor *Actor, loosens bool, reason string) error {
	if !actor.inBrowser() {
		return ErrSandboxUI
	}
	if !loosens {
		return nil
	}
	if s.ReauthUntil(ctx, actor) == nil {
		return ErrReauth
	}
	if strings.TrimSpace(reason) == "" {
		return invalid("say why you're loosening the sandbox")
	}
	return nil
}

// cleanSandboxLevel checks a level; "" means the default.
func cleanSandboxLevel(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if _, ok := sandboxRank[v]; ok || v == "" {
		return v, nil
	}
	return "", invalid("sandbox level must be vm, contained, tool or off (or empty for the default)")
}

// levelLoosens says whether changing a level from old to new may weaken
// the sandbox. "" is a default that may be anything, so moving to it may
// weaken and only VM is surely as strong as it.
func levelLoosens(old, new string) bool {
	if old == new {
		return false
	}
	oldMax, newMin := sandboxRank[SandboxVM], sandboxRank[SandboxOff]
	if old != "" {
		oldMax = sandboxRank[old]
	}
	if new != "" {
		newMin = sandboxRank[new]
	}
	return newMin < oldMax
}

// SandboxChange is one field of a policy change, as logged.
type SandboxChange struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	User       *UserRef  `json:"user,omitempty"`
	Scope      string    `json:"scope"` // project, machine
	TargetID   int64     `json:"target_id"`
	TargetName string    `json:"target_name"`
	Field      string    `json:"field"`
	Old        string    `json:"old"`
	New        string    `json:"new"`
	Loosens    bool      `json:"loosens"`
	Reason     string    `json:"reason,omitempty"`
	Reverts    *int64    `json:"reverts,omitempty"`
	// Current: the field still holds New, so Revert puts Old back.
	Current bool `json:"current"`
}

// sandboxEdit is one field a write changes.
type sandboxEdit struct {
	field, old, new string
	loosens         bool
}

func loosensAny(edits []sandboxEdit) bool {
	for _, e := range edits {
		if e.loosens {
			return true
		}
	}
	return false
}

// logSandbox records a write's edits and emits the event that notifies.
func (t *tx) logSandbox(ctx context.Context, actor *Actor, scope string, target int64, name string, edits []sandboxEdit, reason string, reverts *int64, owner int64) error {
	now := t.s.nowMS()
	changes := make([]map[string]any, 0, len(edits))
	for _, e := range edits {
		if _, err := t.ExecContext(ctx, `INSERT INTO sandbox_policy_log(at, user_id, scope, target_id, target_name, field, old_value, new_value, loosens, reason, reverts)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, now, actor.ID(), scope, target, name, e.field, e.old, e.new, e.loosens, excerpt(strings.TrimSpace(reason), 500), reverts); err != nil {
			return err
		}
		changes = append(changes, map[string]any{"field": e.field, "old": e.old, "new": e.new})
	}
	ev := &Event{Kind: EvSandboxPolicy, Data: map[string]any{"scope": scope, "target": target, "name": name, "changes": changes,
		"loosens": loosensAny(edits), "reason": strings.TrimSpace(reason)}}
	if scope == "project" {
		ev.ProjectID = target
	} else {
		ev.AdminOnly = true
		ev.Data["runner_owner"] = owner
	}
	t.emit(ev)
	return nil
}

// SandboxPolicy is a project's sandbox levels.
type SandboxPolicy struct {
	Project string `json:"project"`
	// Level is for every session in the project; "" follows the machine's
	// default.
	Level string `json:"level"`
	// InteractiveLevel is for sessions a person starts; "" means Level.
	InteractiveLevel string     `json:"interactive_level"`
	UpdatedBy        *UserRef   `json:"updated_by,omitempty"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
	// CanEdit: the actor owns the project and is in the browser.
	CanEdit bool `json:"can_edit"`
}

// SandboxPolicyInput changes a project's levels; nil leaves one as it is.
type SandboxPolicyInput struct {
	Level            *string `json:"level"`
	InteractiveLevel *string `json:"interactive_level"`
	Reason           string  `json:"reason"`
}

// ProjectSandbox is a project's sandbox policy, for anyone who can see the
// project.
func (s *Service) ProjectSandbox(ctx context.Context, actor *Actor, ref string) (*SandboxPolicy, error) {
	p, err := s.projectByRef(ctx, s.db.R, ref)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	pol, err := s.projectSandbox(ctx, s.db.R, p)
	if err != nil {
		return nil, err
	}
	if actor.inBrowser() {
		r, rerr := s.projectRole(ctx, s.db.R, actor, p.ID)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "p", p.ID, "err", rerr)
			}
		}
		pol.CanEdit = r >= RoleOwner
	}
	return pol, nil
}

func (s *Service) projectSandbox(ctx context.Context, q queryer, p *Project) (*SandboxPolicy, error) {
	pol := &SandboxPolicy{Project: p.Key}
	var by sql.NullInt64
	var at sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT level, interactive_level, updated_by, updated_at FROM sandbox_projects WHERE project_id = ?`, p.ID).
		Scan(&pol.Level, &pol.InteractiveLevel, &by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return pol, nil
	}
	if err != nil {
		return nil, err
	}
	pol.UpdatedBy = s.userRefByID(ctx, q, by)
	if at.Valid {
		t := msTime(at.Int64)
		pol.UpdatedAt = &t
	}
	return pol, nil
}

// SetProjectSandbox changes a project's sandbox levels: the project's
// owners (and admins), in the browser.
func (s *Service) SetProjectSandbox(ctx context.Context, actor *Actor, ref string, in SandboxPolicyInput) (*SandboxPolicy, error) {
	return s.setProjectSandbox(ctx, actor, ref, in, nil)
}

func (s *Service) setProjectSandbox(ctx context.Context, actor *Actor, ref string, in SandboxPolicyInput, reverts *int64) (*SandboxPolicy, error) {
	if !actor.inBrowser() {
		return nil, ErrSandboxUI
	}
	p, err := s.projectByRef(ctx, s.db.R, ref)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		cur, err := s.projectSandbox(ctx, t, p)
		if err != nil {
			return err
		}
		var edits []sandboxEdit
		for _, f := range []struct {
			name string
			in   *string
			cur  string
		}{{"level", in.Level, cur.Level}, {"interactive_level", in.InteractiveLevel, cur.InteractiveLevel}} {
			if f.in == nil {
				continue
			}
			v, err := cleanSandboxLevel(*f.in)
			if err != nil {
				return err
			}
			if v != f.cur {
				edits = append(edits, sandboxEdit{f.name, f.cur, v, levelLoosens(f.cur, v)})
			}
		}
		if len(edits) == 0 {
			return nil
		}
		if err := s.sandboxWriter(ctx, actor, loosensAny(edits), in.Reason); err != nil {
			return err
		}
		next := *cur
		for _, e := range edits {
			if e.field == "level" {
				next.Level = e.new
			} else {
				next.InteractiveLevel = e.new
			}
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO sandbox_projects(project_id, level, interactive_level, updated_by, updated_at) VALUES (?,?,?,?,?)
			ON CONFLICT(project_id) DO UPDATE SET level = excluded.level, interactive_level = excluded.interactive_level, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			p.ID, next.Level, next.InteractiveLevel, actor.ID(), s.nowMS()); err != nil {
			return err
		}
		return t.logSandbox(ctx, actor, "project", p.ID, p.Key, edits, in.Reason, reverts, 0)
	})
	if err != nil {
		return nil, err
	}
	return s.ProjectSandbox(ctx, actor, p.Key)
}

// SandboxMachine is a runner machine's sandbox settings.
type SandboxMachine struct {
	Runner *Runner `json:"runner"`
	// DefaultLevel is for projects that don't set one; "" is Waypoint's
	// default (VM where the machine has KVM, else Contained).
	DefaultLevel string `json:"default_level"`
	// Paused is the kill switch: sessions on the machine run as they did
	// before sandboxing.
	Paused    bool       `json:"paused"`
	UpdatedBy *UserRef   `json:"updated_by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	CanEdit   bool       `json:"can_edit"`
	// Helper is what the machine's sandbox helper last reported; nil when
	// its runner never said (an older wp).
	Helper *SandboxHelper `json:"helper,omitempty"`
	// Users is what Waypoint knows of each machine user the helper
	// reported, by name (WP-448).
	Users map[string]*SandboxUserInfo `json:"users,omitempty"`
	// Ops are the latest helper actions people asked for, newest first.
	Ops []*SandboxOp `json:"ops"`
	// Drill is the machine's last escape drill (WP-473), nil before its first.
	Drill *SandboxDrill `json:"drill,omitempty"`
	// DrillBlocked says why a drill can't run on the machine now, "" when
	// one can.
	DrillBlocked string `json:"drill_blocked,omitempty"`
}

// SandboxMachineInput changes a machine's settings; nil leaves one as it is.
type SandboxMachineInput struct {
	DefaultLevel *string `json:"default_level"`
	Paused       *bool   `json:"paused"`
	Reason       string  `json:"reason"`
}

// SandboxMachines lists the runner machines the actor may see: an admin's
// are all of them, anyone else's their own.
func (s *Service) SandboxMachines(ctx context.Context, actor *Actor) ([]*SandboxMachine, error) {
	if err := actor.Need(PermRunnersRead); err != nil {
		return nil, err
	}
	runners, err := s.ListRunners(ctx, actor, actor.IsAdmin())
	if err != nil {
		return nil, err
	}
	out := make([]*SandboxMachine, 0, len(runners))
	for _, r := range runners {
		m, err := s.sandboxMachine(ctx, s.db.R, r)
		if err != nil {
			return nil, err
		}
		m.CanEdit = actor.inBrowser() && (actor.IsAdmin() || r.Owner.ID == actor.ID())
		out = append(out, m)
	}
	return out, nil
}

func (s *Service) sandboxMachine(ctx context.Context, q queryer, r *Runner) (*SandboxMachine, error) {
	m := &SandboxMachine{Runner: r}
	var err error
	if m.Helper, err = s.sandboxHelper(ctx, q, r.ID); err != nil {
		return nil, err
	}
	if m.Helper != nil && m.Helper.Status != nil {
		if m.Users, err = s.sandboxUserInfo(ctx, q, r.ID, m.Helper.Status.Users); err != nil {
			return nil, err
		}
	}
	if m.Drill, err = s.lastSandboxDrill(ctx, q, r.ID, true); err != nil {
		return nil, err
	}
	if _, m.DrillBlocked, err = s.drillReady(ctx, q, r); err != nil {
		return nil, err
	}
	if m.Ops, err = s.sandboxOps(ctx, q, r.ID, 0); err != nil {
		return nil, err
	}
	var by, at sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT default_level, paused, updated_by, updated_at FROM sandbox_machines WHERE runner_id = ?`, r.ID).
		Scan(&m.DefaultLevel, &m.Paused, &by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	m.UpdatedBy = s.userRefByID(ctx, q, by)
	if at.Valid {
		t := msTime(at.Int64)
		m.UpdatedAt = &t
	}
	return m, nil
}

// SetSandboxMachine changes a machine's default level or pauses its
// sandboxing: an admin or the runner's owner, in the browser.
func (s *Service) SetSandboxMachine(ctx context.Context, actor *Actor, runnerID int64, in SandboxMachineInput) (*SandboxMachine, error) {
	return s.setSandboxMachine(ctx, actor, runnerID, in, nil)
}

func (s *Service) setSandboxMachine(ctx context.Context, actor *Actor, runnerID int64, in SandboxMachineInput, reverts *int64) (*SandboxMachine, error) {
	if !actor.inBrowser() {
		return nil, ErrSandboxUI
	}
	r, err := s.runnerByID(ctx, s.db.R, runnerID)
	if err != nil {
		return nil, err
	}
	if !actor.IsAdmin() && r.Owner.ID != actor.ID() {
		return nil, notFound("runner")
	}
	var m *SandboxMachine
	err = s.write(ctx, actor, func(t *tx) error {
		cur, err := s.sandboxMachine(ctx, t, r)
		if err != nil {
			return err
		}
		var edits []sandboxEdit
		next := *cur
		if in.DefaultLevel != nil {
			v, err := cleanSandboxLevel(*in.DefaultLevel)
			if err != nil {
				return err
			}
			if v != cur.DefaultLevel {
				edits = append(edits, sandboxEdit{"default_level", cur.DefaultLevel, v, levelLoosens(cur.DefaultLevel, v)})
				next.DefaultLevel = v
			}
		}
		if in.Paused != nil && *in.Paused != cur.Paused {
			edits = append(edits, sandboxEdit{"paused", boolWord(cur.Paused), boolWord(*in.Paused), *in.Paused})
			next.Paused = *in.Paused
		}
		if len(edits) > 0 {
			if err := s.sandboxWriter(ctx, actor, loosensAny(edits), in.Reason); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `INSERT INTO sandbox_machines(runner_id, default_level, paused, updated_by, updated_at) VALUES (?,?,?,?,?)
				ON CONFLICT(runner_id) DO UPDATE SET default_level = excluded.default_level, paused = excluded.paused, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
				r.ID, next.DefaultLevel, next.Paused, actor.ID(), s.nowMS()); err != nil {
				return err
			}
			if err := t.logSandbox(ctx, actor, "machine", r.ID, r.Name, edits, in.Reason, reverts, r.Owner.ID); err != nil {
				return err
			}
		}
		m, err = s.sandboxMachine(ctx, t, r)
		return err
	})
	if err != nil {
		return nil, err
	}
	m.CanEdit = true
	return m, nil
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// SandboxLogQuery narrows the policy log.
type SandboxLogQuery struct {
	Project string // a project key: its changes only
	Runner  int64  // a runner: its changes only
	Limit   int
}

// SandboxLog is the policy log, newest first: an admin sees every change,
// anyone else the changes to projects they can see and to their own
// runners.
func (s *Service) SandboxLog(ctx context.Context, actor *Actor, q SandboxLogQuery) ([]*SandboxChange, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	conds, args := []string{"1=1"}, []any{}
	if q.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		conds, args = append(conds, "scope = 'project' AND target_id = ?"), append(args, p.ID)
	} else if q.Runner != 0 {
		conds, args = append(conds, "scope = 'machine' AND target_id = ?"), append(args, q.Runner)
	}
	if !actor.IsAdmin() {
		set, sargs := actor.MemberProjects(RoleViewer)
		conds = append(conds, "((scope = 'project' AND target_id IN "+set+") OR (scope = 'machine' AND target_id IN (SELECT id FROM runners WHERE owner_id = ?)))")
		args = append(append(args, sargs...), actor.ID())
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// #nosec G202 -- conds are fixed clauses; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, at, user_id, scope, target_id, target_name, field, old_value, new_value, loosens, reason, reverts
		FROM sandbox_policy_log WHERE `+strings.Join(conds, " AND ")+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SandboxChange{}
	var users []sql.NullInt64
	for rows.Next() {
		c := &SandboxChange{}
		var at int64
		var uid, reverts sql.NullInt64
		if err := rows.Scan(&c.ID, &at, &uid, &c.Scope, &c.TargetID, &c.TargetName, &c.Field, &c.Old, &c.New, &c.Loosens, &c.Reason, &reverts); err != nil {
			return nil, err
		}
		c.At = msTime(at)
		if reverts.Valid {
			c.Reverts = &reverts.Int64
		}
		users = append(users, uid)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, c := range out {
		c.User = s.userRefByID(ctx, s.db.R, users[i])
		c.Current = s.sandboxFieldValue(ctx, s.db.R, c.Scope, c.TargetID, c.Field) == c.New
	}
	return out, nil
}

// sandboxFieldValue is a policy field's value now ("" for a project or
// machine that is gone or never set).
func (s *Service) sandboxFieldValue(ctx context.Context, q queryer, scope string, target int64, field string) string {
	var v string
	var query string
	switch {
	case scope == "project" && field == "level":
		query = `SELECT level FROM sandbox_projects WHERE project_id = ?`
	case scope == "project" && field == "interactive_level":
		query = `SELECT interactive_level FROM sandbox_projects WHERE project_id = ?`
	case scope == "machine" && field == "default_level":
		query = `SELECT default_level FROM sandbox_machines WHERE runner_id = ?`
	case scope == "machine" && field == "paused":
		query = `SELECT CASE paused WHEN 1 THEN 'yes' ELSE 'no' END FROM sandbox_machines WHERE runner_id = ?`
	default:
		return ""
	}
	if err := q.QueryRowContext(ctx, query, target).Scan(&v); err != nil && field == "paused" {
		return "no"
	}
	return v
}

// RevertSandboxChange puts a logged field back to what it was before the
// change, if it still holds what the change set. It is a change like any
// other: in the browser, and with a password check and a reason if putting
// it back loosens the sandbox.
func (s *Service) RevertSandboxChange(ctx context.Context, actor *Actor, id int64, reason string) error {
	if !actor.inBrowser() {
		return ErrSandboxUI
	}
	var c SandboxChange
	err := s.db.R.QueryRowContext(ctx, `SELECT scope, target_id, field, old_value, new_value FROM sandbox_policy_log WHERE id = ?`, id).
		Scan(&c.Scope, &c.TargetID, &c.Field, &c.Old, &c.New)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound("sandbox change")
	}
	if err != nil {
		return err
	}
	if s.sandboxFieldValue(ctx, s.db.R, c.Scope, c.TargetID, c.Field) != c.New {
		return fmt.Errorf("%s has changed since; set it directly: %w", c.Field, ErrConflict)
	}
	old := c.Old
	switch c.Scope {
	case "project":
		var key string
		if err := s.db.R.QueryRowContext(ctx, `SELECT key FROM projects WHERE id = ?`, c.TargetID).Scan(&key); err != nil {
			return notFound("project")
		}
		in := SandboxPolicyInput{Reason: reason}
		if c.Field == "level" {
			in.Level = &old
		} else {
			in.InteractiveLevel = &old
		}
		_, err = s.setProjectSandbox(ctx, actor, key, in, &id)
	case "machine":
		in := SandboxMachineInput{Reason: reason}
		if c.Field == "default_level" {
			in.DefaultLevel = &old
		} else {
			paused := old == "yes"
			in.Paused = &paused
		}
		_, err = s.setSandboxMachine(ctx, actor, c.TargetID, in, &id)
	}
	return err
}

// notifySandboxPolicy tells the people who answer for a project's or
// machine's sandbox that someone changed it: the project's owners or the
// runner's owner, and the admins, but not the person who did it.
func (s *Service) notifySandboxPolicy(ctx context.Context, actor *Actor, e *Event) {
	scope, _ := e.Data["scope"].(string)
	name, _ := e.Data["name"].(string)
	loosens, _ := e.Data["loosens"].(bool)
	var parts []string
	if list, ok := e.Data["changes"].([]map[string]any); ok {
		for _, c := range list {
			parts = append(parts, fmt.Sprintf("%s %s → %s", c["field"], sandboxWord(c["old"]), sandboxWord(c["new"])))
		}
	}
	verb := "changed"
	if loosens {
		verb = "loosened"
	}
	who := "Someone"
	if actor != nil && actor.User != nil {
		who = actor.User.Name()
	}
	body := who + ": " + strings.Join(parts, ", ") + "."
	if r, _ := e.Data["reason"].(string); r != "" {
		body += " Reason: " + r
	}
	var ids []int64
	var title, link string
	var owner int64
	if scope == "project" {
		title = "Sandbox " + verb + " in " + name
		link = "/p/" + name + "/settings?tab=sandbox"
		ids = s.userIDsWhere(ctx, `SELECT user_id FROM project_members WHERE project_id = ? AND role = 'owner'`, e.ProjectID)
	} else {
		title = "Sandbox " + verb + " on " + name
		link = "/admin?tab=runners"
		owner = dataInt(e, "runner_owner")
		if owner != 0 && owner != actor.ID() {
			s.notifyURL(ctx, owner, nil, NotifySandbox, title, body, "/settings?tab=runners")
		}
	}
	ids = append(ids, s.userIDsWhere(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0`)...)
	seen := map[int64]bool{actor.ID(): true, 0: true, owner: true}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			s.notifyURL(ctx, id, nil, NotifySandbox, title, body, link)
		}
	}
}

func sandboxWord(v any) string {
	if s, _ := v.(string); s != "" {
		return s
	}
	return "default"
}
