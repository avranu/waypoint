package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// automationFor loads an automation the actor may act on at the role:
// project automations by project role, admin automations by being an admin,
// follow-ups by being the session's person.
func (s *Service) automationFor(ctx context.Context, actor *Actor, id int64, min int) (*Automation, error) {
	a, err := s.automationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.SessionID != 0 {
		if !a.ownedBy(actor) {
			return nil, notFound("automation")
		}
		return a, nil
	}
	if a.ProjectID == 0 {
		if !actor.IsAdmin() {
			return nil, notFound("automation")
		}
		return a, nil
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, a.ProjectID, min); err != nil {
		return nil, err
	}
	return a, nil
}

// ListAutomationRuns lists an automation's runs, newest first; before is the
// id to continue from (0 for the newest).
func (s *Service) ListAutomationRuns(ctx context.Context, actor *Actor, automationID int64, limit int, before int64) ([]*AutomationRun, error) {
	a, err := s.automationFor(ctx, actor, automationID, RoleEditor)
	if err != nil {
		return nil, err
	}
	if a.ProjectID == 0 {
		if err := actor.RequireHuman("admin automations"); err != nil {
			return nil, err
		}
	}
	limit = min(max(limit, 1), 100)
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+autoRunCols+` FROM automation_runs r LEFT JOIN automations a ON a.id = r.automation_id
		WHERE r.automation_id = ? AND r.id < ? ORDER BY r.id DESC LIMIT ?`, automationID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AutomationRun{}
	for rows.Next() {
		r, err := s.scanAutomationRun(ctx, rows)
		if err != nil {
			return nil, err
		}
		r.Event = nil
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetAutomationRun returns a run with its steps and the rule it ran.
func (s *Service) GetAutomationRun(ctx context.Context, actor *Actor, id int64) (*AutomationRun, error) {
	r, err := s.runByID(ctx, id)
	if err != nil {
		return nil, err
	}
	a, err := s.automationFor(ctx, actor, r.AutomationID, RoleEditor)
	if err != nil {
		return nil, notFound("run")
	}
	if a.ProjectID == 0 {
		if err := actor.RequireHuman("admin automations"); err != nil {
			return nil, err
		}
	}
	if r.Steps, err = s.runSteps(ctx, id); err != nil {
		return nil, err
	}
	if rule, err := s.runRule(ctx, r.ruleID); err == nil {
		if a.ProjectID != 0 && s.requireProjectRole(ctx, s.db.R, actor, a.ProjectID, RoleOwner) != nil {
			redactRule(rule)
		}
		r.Rule = rule
	}
	return r, nil
}

// redactRule hides credentials from people who can't manage the automation.
func redactRule(r *Rule) {
	for n := range r.Actions {
		redactAction(&r.Actions[n])
	}
	delete(r.Trigger.Extra, "secret")
}

func redactAction(act *Action) {
	act.Token = ""
	act.URL = redactURL(act.URL)
	for n := range act.Steps {
		redactAction(&act.Steps[n])
	}
	if d := actionDef(act.Type); d != nil {
		for _, p := range d.Params {
			if p.Secret {
				delete(act.Extra, p.Name)
			}
		}
	}
}

// RunAutomationNow runs a manual automation, with the inputs it asks for.
// Agents may only run automations that allow it.
func (s *Service) RunAutomationNow(ctx context.Context, actor *Actor, id int64, inputs map[string]any, via string) (*AutomationRun, error) {
	if err := actor.Need(PermAutomationsManage); err != nil {
		return nil, err
	}
	return s.runManual(ctx, actor, id, inputs, via, 0)
}

func (s *Service) runManual(ctx context.Context, actor *Actor, id int64, inputs map[string]any, via string, rerunOf int64) (*AutomationRun, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	a, err := s.automationFor(ctx, actor, id, RoleEditor)
	if err != nil {
		return nil, err
	}
	if a.Rule.Trigger.Event != "manual" {
		return nil, invalid("this automation has no manual trigger")
	}
	if actor.IsAgent() && !a.Rule.AllowAgents {
		return nil, forbidden("this automation doesn't allow agents to run it")
	}
	if !a.Enabled {
		return nil, invalid("turn the automation on to run it")
	}
	vals, err := a.Rule.inputValues(inputs)
	if err != nil {
		return nil, err
	}
	ref := actor.User.Ref()
	who := actor.User.DisplayName
	by := "person"
	if actor.IsAgent() {
		who, by = actor.AgentName(), "agent"
	}
	e := &Event{Kind: EvManual, ProjectID: a.ProjectID, Actor: &ref, At: s.now().UTC(), Data: map[string]any{"inputs": vals, "summary": "Run by " + who}}
	run, err := s.startRun(ctx, actor, a, e, fireOpts{Via: cmp(via, "manual"), Inputs: vals, Agent: actor.AgentName(), By: by, RerunOf: rerunOf})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, AuditEntry{Action: "automation.run", Category: AuditSettings, ProjectID: a.ProjectID, TargetType: "automation", TargetID: id, Target: a.Name, Detail: "ran it by hand"})
	return run, nil
}

// RerunAutomation starts another run from a run's event and inputs, with the
// automation as it is now.
func (s *Service) RerunAutomation(ctx context.Context, actor *Actor, runID int64) (*AutomationRun, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	old, err := s.runByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	a, err := s.automationFor(ctx, actor, old.AutomationID, RoleEditor)
	if err != nil {
		return nil, notFound("run")
	}
	if a.SessionID != 0 {
		return nil, invalid("a follow-up runs again with Run now on its session")
	}
	if a.Rule.Trigger.Event == "manual" {
		return s.runManual(ctx, actor, a.ID, old.Inputs, "rerun", old.ID)
	}
	if a.ProjectID != 0 {
		if err := s.requireProjectRole(ctx, s.db.R, actor, a.ProjectID, RoleOwner); err != nil {
			return nil, err
		}
	}
	if actor.IsAgent() && !a.Rule.AllowAgents {
		return nil, forbidden("this automation doesn't allow agents to run it")
	}
	if !a.Enabled {
		return nil, invalid("turn the automation on to run it")
	}
	if old.Event == nil {
		return nil, invalid("this run has no event to repeat")
	}
	e := *old.Event
	e.At = s.now().UTC()
	return s.startRun(ctx, actor, a, &e, fireOpts{Via: "rerun", Inputs: old.Inputs, RerunOf: old.ID, Summary: old.Summary, Agent: actor.AgentName()})
}

// CancelAutomationRun stops a run that has not finished: a step in progress
// is interrupted, waits are dropped.
func (s *Service) CancelAutomationRun(ctx context.Context, actor *Actor, runID int64) (*AutomationRun, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	r, err := s.runByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	a, err := s.automationFor(ctx, actor, r.AutomationID, RoleEditor)
	if err != nil {
		return nil, notFound("run")
	}
	if r.Finished() {
		return r, nil
	}
	now := s.nowMS()
	res, err := s.db.W.ExecContext(ctx, `UPDATE automation_runs SET status = 'cancelled', finished_at = ?, error = 'Cancelled' WHERE id = ? AND status IN ('queued','running','waiting')`, now, runID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, _ = s.db.W.ExecContext(ctx, `UPDATE automation_waits SET resolved_at = ?, outcome = 'cancelled' WHERE run_id = ? AND resolved_at IS NULL`, now, runID)
		_, _ = s.db.W.ExecContext(ctx, `UPDATE automation_run_steps SET status = 'cancelled', finished_at = ? WHERE run_id = ? AND status IN ('running','waiting')`, now, runID)
		s.cancelRunMessages(ctx, runID)
		s.autoRuns.init()
		s.autoRuns.mu.Lock()
		cancel := s.autoRuns.cancels[runID]
		s.autoRuns.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		s.audit(ctx, actor, AuditEntry{Action: "automation.cancel", Category: AuditSettings, ProjectID: a.ProjectID, TargetType: "automation", TargetID: a.ID, Target: a.Name, Detail: "cancelled a run"})
		run, err := s.runByID(ctx, runID)
		if err == nil {
			s.runFinished(ctx, a, run)
		}
		return run, err
	}
	return s.runByID(ctx, runID)
}

// TestInput picks what a test runs against.
type TestInput struct {
	Inputs map[string]any `json:"inputs"`
	// Data stands in for a webhook's body.
	Data map[string]any `json:"data"`
}

// TestResult is what a dry run found: what each step would do.
type TestResult struct {
	Summary string     `json:"summary"`
	Event   *Event     `json:"event"`
	Sample  bool       `json:"sample"` // no recent event matched; sample data stood in
	Note    string     `json:"note,omitempty"`
	Steps   []*RunStep `json:"steps"`
	Error   string     `json:"error,omitempty"`
}

// TestAutomation walks the steps against a recent event the trigger matches,
// changing nothing: every step says what it would do.
func (s *Service) TestAutomation(ctx context.Context, actor *Actor, id int64, in TestInput) (*TestResult, error) {
	a, err := s.automationFor(ctx, actor, id, RoleOwner)
	if err != nil {
		return nil, err
	}
	e, sample, note := s.sampleEvent(ctx, actor, a, in)
	vals, err := a.Rule.inputValues(in.Inputs)
	if err != nil && len(in.Inputs) > 0 {
		return nil, err
	}
	if err != nil {
		vals, _ = (&Rule{Inputs: a.Rule.Inputs}).inputValues(nil)
		if vals == nil {
			vals = map[string]any{}
		}
	}
	n := automationBudget
	run := &AutomationRun{AutomationID: a.ID, Automation: a.Name, ProjectID: a.ProjectID, Inputs: vals, Event: e}
	auto := &Actor{User: actor.User, Automation: a, Depth: 1, Budget: &n}
	x := &execution{s: s, run: run, a: a, rule: &a.Rule, ev: e, actor: auto, dry: true,
		trig:  s.triggerView(a, e, "test", eventBy(e), actor.User.Ref()),
		steps: map[string]map[string]any{}, prev: map[string]any{}, rows: map[string]*RunStep{}}
	res := &TestResult{Summary: eventSummary(e), Event: e, Sample: sample, Note: note}
	if err := x.walk(ctx); err != nil {
		res.Error = excerpt(err.Error(), 500)
	}
	res.Steps = x.order
	if res.Steps == nil {
		res.Steps = []*RunStep{}
	}
	return res, nil
}

// sampleEvent finds a recent event the automation's trigger matches (and
// whose issue passes its condition), or makes one up.
func (s *Service) sampleEvent(ctx context.Context, actor *Actor, a *Automation, in TestInput) (*Event, bool, string) {
	d := triggerDef(a.Rule.Trigger.Event)
	now := s.now().UTC()
	if d == nil {
		return &Event{Kind: "", At: now}, true, ""
	}
	switch d.Event {
	case EvManual:
		return &Event{Kind: EvManual, ProjectID: a.ProjectID, At: now, Data: map[string]any{"summary": "Run by hand"}}, false, "A manual run starts from a button; this is a test of it."
	case EvWebhookIn:
		data := in.Data
		if data == nil {
			data = map[string]any{}
		}
		return &Event{Kind: EvWebhookIn, ProjectID: a.ProjectID, At: now, Data: data}, true, "Add sample data to see how templates read the body."
	}
	if persisted[d.Event] {
		where, args := `e.kind = ?`, []any{d.Event}
		if a.ProjectID != 0 {
			where += ` AND e.project_id = ?`
			args = append(args, a.ProjectID)
		}
		// #nosec G202 -- where is built from fixed conditions in this function; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT e.id, coalesce(e.project_id, 0), coalesce(e.issue_id, 0), coalesce(p.key || '-' || i.number, ''), coalesce(e.actor_id, 0), e.kind, e.field, e.old_value, e.new_value, e.data, e.created_at
			FROM events e LEFT JOIN issues i ON i.id = e.issue_id LEFT JOIN projects p ON p.id = i.project_id WHERE `+where+` ORDER BY e.id DESC LIMIT 100`, args...)
		if err != nil {
			s.log.WarnContext(ctx, "find a sample event", "err", err)
		} else {
			var found []*Event
			for rows.Next() {
				var e Event
				var actorID, at int64
				var data string
				if err := rows.Scan(&e.ID, &e.ProjectID, &e.IssueID, &e.IssueKey, &actorID, &e.Kind, &e.Field, &e.Old, &e.New, &data, &at); err != nil {
					continue
				}
				e.At = msTime(at)
				_ = json.Unmarshal([]byte(data), &e.Data)
				if actorID != 0 {
					if u, err := s.userByID(ctx, s.db.R, actorID); err == nil {
						r := u.Ref()
						e.Actor = &r
					}
				}
				found = append(found, &e)
			}
			rows.Close()
			for _, e := range found {
				if !a.Rule.Trigger.matches(e) {
					continue
				}
				if e.IssueID != 0 && a.Rule.If != "" {
					if ok, err := s.issueMatches(ctx, actor.User, e.IssueID, a.Rule.If); err != nil || !ok {
						continue
					}
				}
				return e, false, "A recent event this automation would have run for."
			}
		}
	}
	e := &Event{Kind: d.Event, ProjectID: a.ProjectID, At: now, Data: map[string]any{}}
	if d.Issue || d.Timed {
		var id int64
		var key string
		q, args := `SELECT i.id, p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id ORDER BY i.updated_at DESC LIMIT 1`, []any{}
		if a.ProjectID != 0 {
			q, args = `SELECT i.id, p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.project_id = ? ORDER BY i.updated_at DESC LIMIT 1`, []any{a.ProjectID}
		}
		if s.db.R.QueryRowContext(ctx, q, args...).Scan(&id, &key) == nil {
			e.IssueID, e.IssueKey = id, key
			if e.ProjectID == 0 {
				_ = s.db.R.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE id = ?`, id).Scan(&e.ProjectID)
			}
		}
	}
	if d.PR {
		var id int64
		if s.db.R.QueryRowContext(ctx, `SELECT id FROM pull_requests ORDER BY updated_at DESC LIMIT 1`).Scan(&id) == nil {
			e.Data["pr_id"] = id
		}
	}
	return e, true, "Nothing like this has happened yet, so sample data stands in."
}

// HookURL is the address an incoming-webhook automation listens on.
func (s *Service) hookURL(token string) string {
	return s.BaseURL() + "/hooks/automations/" + token
}

// RegenerateAutomationHook replaces a webhook automation's secret address.
func (s *Service) RegenerateAutomationHook(ctx context.Context, actor *Actor, id int64) (*Automation, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	a, err := s.automationForManage(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if a.Rule.Trigger.Event != "webhook" {
		return nil, invalid("this automation has no webhook trigger")
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE automations SET hook_token = ?, updated_at = ? WHERE id = ?`, newToken("h"), s.nowMS(), id); err != nil {
		return nil, err
	}
	s.audit(ctx, actor, AuditEntry{Action: "automation.update", Category: AuditSettings, ProjectID: a.ProjectID, TargetType: "automation", TargetID: id, Target: a.Name, Detail: "made a new webhook address"})
	return s.GetAutomation(ctx, actor, id)
}

// ErrBadSignature: the body's signature didn't match the automation's secret.
var ErrBadSignature = errors.New("bad signature")

// ErrNoHook: nothing listens at that address.
var ErrNoHook = errors.New("no such webhook")

// RunAutomationWebhook starts the automation behind a webhook address. The
// body (JSON, else text) is available to templates as .Trigger.Data. The run
// acts as the person who made the automation.
func (s *Service) RunAutomationWebhook(ctx context.Context, token string, body []byte, signature string) (*AutomationRun, error) {
	if len(token) < 16 {
		return nil, ErrNoHook
	}
	a, err := scanAutomation(s.db.R.QueryRowContext(ctx, `SELECT `+automationCols+` FROM automations WHERE hook_token = ?`, token))
	if err != nil || a.Rule.Trigger.Event != "webhook" || !a.Enabled {
		return nil, ErrNoHook
	}
	if secret := a.Rule.Trigger.str("secret"); secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		got := strings.TrimPrefix(strings.TrimSpace(signature), "sha256=")
		if !hmac.Equal([]byte(strings.ToLower(got)), []byte(want)) {
			return nil, ErrBadSignature
		}
	}
	data := map[string]any{}
	var parsed any
	if len(strings.TrimSpace(string(body))) > 0 {
		if json.Unmarshal(body, &parsed) == nil {
			if m, ok := parsed.(map[string]any); ok {
				data = m
			} else {
				data["body"] = parsed
			}
		} else {
			data["body"] = string(body)
		}
	}
	actor := SystemActor()
	if a.createdBy != 0 {
		if u, err := s.userByID(ctx, s.db.R, a.createdBy); err == nil && !u.Disabled {
			actor = &Actor{User: u}
		}
	}
	e := &Event{Kind: EvWebhookIn, ProjectID: a.ProjectID, At: s.now().UTC(), Data: data}
	return s.startRun(ctx, actor, a, e, fireOpts{Via: "webhook", By: "system", Summary: "Incoming webhook"})
}

// NextCronRuns lists the next n times a cron expression fires in a time zone.
func (s *Service) NextCronRuns(expr, tz string, n int) ([]time.Time, error) {
	c, err := ParseCron(expr)
	if err != nil {
		return nil, invalid("%v", err)
	}
	loc := time.UTC
	if tz != "" {
		if loc, err = time.LoadLocation(tz); err != nil {
			return nil, invalid("unknown time zone %q", tz)
		}
	}
	n = min(max(n, 1), 20)
	out := make([]time.Time, 0, min(n, 20))
	t := s.now()
	for range n {
		t = c.Next(t.In(loc))
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out, nil
}
