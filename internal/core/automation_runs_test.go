package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- helpers ----

var (
	testActionsOnce sync.Once
	flakyMu         sync.Mutex
	flakyCalls      = map[string]int{}
)

// registerTestActions adds a long action that fails its first `fail` calls
// per key, and an instant one that counts.
func registerTestActions() {
	testActionsOnce.Do(func() {
		registerAction(ActionDef{Name: "test_flaky", Label: "Flaky", Group: "Flow", Long: true, Phrase: "flake",
			Params: []ParamDef{{Name: "key", Label: "Key", Type: PText}, {Name: "fail", Label: "Fail", Type: PNumber}, {Name: "stop_at", Label: "Stop at", Type: PNumber}},
			Run: func(c *StepCtx) error {
				key, fails := c.Str("key"), c.Int("fail", 0)
				flakyMu.Lock()
				flakyCalls[key]++
				n := flakyCalls[key]
				flakyMu.Unlock()
				c.Set("Calls", n)
				c.Set("Done", n >= c.Int("stop_at", 1))
				if n <= fails {
					return fmt.Errorf("flaky failure %d", n)
				}
				return nil
			}})
		registerAction(ActionDef{Name: "test_count", Label: "Count", Group: "Flow", Phrase: "count",
			Params: []ParamDef{{Name: "key", Label: "Key", Type: PText}},
			Run: func(c *StepCtx) error {
				flakyMu.Lock()
				flakyCalls[c.Str("key")]++
				n := flakyCalls[c.Str("key")]
				flakyMu.Unlock()
				c.Set("N", n)
				return nil
			}})
	})
}

// autoLLM answers the first scripted reply whose key the prompt contains,
// and keeps what it was asked.
type autoLLM struct {
	mu      sync.Mutex
	answers map[string]string
	prompts []string
	systems []string
}

func (l *autoLLM) Models(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	return []AIModel{{ID: "opencode/big-pickle", Free: true}}, nil
}

func (l *autoLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prompts, l.systems = append(l.prompts, prompt), append(l.systems, system)
	for k, v := range l.answers {
		if strings.Contains(prompt, k) {
			return json.RawMessage(v), "", nil
		}
	}
	return nil, "", &llmError{status: 404, body: "unknown model"}
}

func flakyCount(key string) int {
	flakyMu.Lock()
	defer flakyMu.Unlock()
	return flakyCalls[key]
}

func (e *env) addAuto(p *Project, name string, rule Rule) *Automation {
	e.t.Helper()
	a, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: &name, Rule: &rule})
	if err != nil {
		e.t.Fatalf("create automation %q: %v", name, err)
	}
	return a
}

func (e *env) runs(a *Automation) []*AutomationRun {
	e.t.Helper()
	list, err := e.s.ListAutomationRuns(e.ctx, e.admin, a.ID, 100, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

func (e *env) run(id int64) *AutomationRun {
	e.t.Helper()
	r, err := e.s.GetAutomationRun(e.ctx, e.admin, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func stepByID(r *AutomationRun, id string) *RunStep {
	for _, s := range r.Steps {
		if s.StepID == id {
			return s
		}
	}
	return nil
}

func (e *env) commentBodies(key string) []string {
	e.t.Helper()
	cs, err := e.s.ListComments(e.ctx, e.admin, key)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

func (e *env) manual(a *Automation, inputs map[string]any) *AutomationRun {
	e.t.Helper()
	r, err := e.s.RunAutomationNow(e.ctx, e.admin, a.ID, inputs, "manual")
	if err != nil {
		e.t.Fatalf("run %s: %v", a.Name, err)
	}
	return r
}

// ---- catalog ----

func TestAutomationCatalog(t *testing.T) {
	registerTestActions()
	c := BuildCatalog(true)
	have := map[string]bool{}
	for _, a := range c.Actions {
		have[a.Name] = true
		if a.Label == "" || a.Group == "" {
			t.Errorf("action %s lacks a label or group", a.Name)
		}
	}
	// Everything the flat list of actions could do is still defined.
	for _, n := range []string{"set_status", "set_parent_status", "reopen_parent", "set_children_status", "set_priority", "create_issue", "set_field", "assign",
		"add_assignee", "add_label", "remove_label", "comment", "notify", "gotify", "email", "webhook", "kestra", "start_timer", "stop_timer", "add_to_sprint", "set_agent",
		"set_workflow", "set_step", "create_subtasks", "review", "pr_comment", "react", "ask_model", "delay", "wait_for", "repeat"} {
		if !have[n] {
			t.Errorf("action %s is missing from the catalog", n)
		}
		if ActionTypes[n] == "" {
			t.Errorf("action %s has no documentation line", n)
		}
	}
	trig := map[string]TriggerDef{}
	for _, d := range c.Triggers {
		trig[d.Name] = d
	}
	for old := range map[string]string{"created": "", "status_changed": "", "assigned": "", "unassigned": "", "commented": "", "field_changed": "", "children_done": "", "reopened": "",
		"pr_opened": "", "pr_merged": "", "pr_closed": "", "linked_issue_closed": "", "step_changed": "", "due_soon": "", "stale": "", "schedule": "", "sprint_started": "",
		"sprint_closed": "", "pr_updated": "", "pr_command": ""} {
		if _, ok := trig[old]; !ok {
			t.Errorf("trigger %s is missing", old)
		}
	}
	// Triggers for what other parts will emit are registered already.
	for _, n := range []string{"session_waiting", "request_created", "request_decided", "request_waiting", "runner_offline", "pr_ci_failed", "pr_review_gate", "env_deployed",
		"repo_pushed", "files_changed", "glossary_changed", "design_changed", "project_created", "repo_attached", "timer_started", "automation_finished", "system_resources",
		"count_crossed", "manual", "webhook"} {
		if _, ok := trig[n]; !ok {
			t.Errorf("trigger %s is missing", n)
		}
	}
	if TriggerEvents["created"] != EvIssueCreated || TriggerEvents["manual"] != EvManual {
		t.Error("TriggerEvents isn't filled from the definitions")
	}
	if len(BuildCatalog(false).Triggers) >= len(c.Triggers) {
		t.Error("system triggers are offered outside admin automations")
	}
	if len(c.Presets) == 0 || len(c.Funcs) == 0 {
		t.Error("the catalog lacks presets or template functions")
	}
	for _, g := range []string{"Issues", "Counts", "System"} {
		found := false
		for _, name := range c.TriggerGroups {
			found = found || name == g
		}
		if !found {
			t.Errorf("trigger group %s is missing", g)
		}
	}
}

func TestAutomationValidationFromDefinitions(t *testing.T) {
	registerTestActions()
	step := func(a ...Action) []Action { return a }
	comment := Action{Type: "comment", Message: "x"}
	manual := Trigger{Event: "manual"}
	bad := map[string]Rule{
		"unknown trigger":     {Trigger: Trigger{Event: "nope"}, Actions: step(comment)},
		"bad schedule time":   {Trigger: Trigger{Event: "schedule", Every: "day", At: "9am"}, Actions: step(comment)},
		"bad schedule kind":   {Trigger: Trigger{Event: "schedule", Every: "fortnight", At: "09:00"}, Actions: step(comment)},
		"bad cron":            {Trigger: Trigger{Event: "schedule", Every: "cron", Extra: map[string]any{"cron": "61 * * * *"}}, Actions: step(comment)},
		"cron missing":        {Trigger: Trigger{Event: "schedule", Every: "cron"}, Actions: step(comment)},
		"stale days":          {Trigger: Trigger{Event: "stale", Days: 0}, Actions: step(comment)},
		"count op":            {Trigger: Trigger{Event: "count_crossed", Extra: map[string]any{"query": "is:open", "op": "!=", "n": 3}}, Actions: step(comment)},
		"count query":         {Trigger: Trigger{Event: "count_crossed", Extra: map[string]any{"query": "priority:", "op": ">=", "n": 3}}, Actions: step(comment)},
		"count n":             {Trigger: Trigger{Event: "count_crossed", Extra: map[string]any{"query": "is:open", "op": ">=", "n": -1}}, Actions: step(comment)},
		"unknown action":      {Trigger: manual, Actions: step(Action{Type: "teleport"})},
		"status missing":      {Trigger: manual, Actions: step(Action{Type: "set_status"})},
		"webhook url":         {Trigger: manual, Actions: step(Action{Type: "webhook", URL: "ftp://x"})},
		"react emoji":         {Trigger: Trigger{Event: "pr_updated"}, Actions: step(Action{Type: "react", Value: "poop"})},
		"review needs pr":     {Trigger: Trigger{Event: "created"}, Actions: step(Action{Type: "review"})},
		"duplicate ids":       {Trigger: manual, Actions: step(Action{Type: "comment", ID: "a"}, Action{Type: "comment", ID: "a"})},
		"bad id":              {Trigger: manual, Actions: step(Action{Type: "comment", ID: "Not Valid"})},
		"nested repeat":       {Trigger: manual, Actions: step(Action{Type: "repeat", Extra: map[string]any{"max": 2}, Steps: step(Action{Type: "repeat", Extra: map[string]any{"max": 2}, Steps: step(comment)})})},
		"empty repeat":        {Trigger: manual, Actions: step(Action{Type: "repeat", Extra: map[string]any{"max": 2}})},
		"repeat max":          {Trigger: manual, Actions: step(Action{Type: "repeat", Extra: map[string]any{"max": 99}, Steps: step(comment)})},
		"steps outside":       {Trigger: manual, Actions: step(Action{Type: "comment", Steps: step(comment)})},
		"retry count":         {Trigger: manual, Actions: step(Action{Type: "comment", OnError: "retry", Retries: 9})},
		"retry delay":         {Trigger: manual, Actions: step(Action{Type: "comment", OnError: "retry", Retries: 2, RetryDelay: "soon"})},
		"on_error":            {Trigger: manual, Actions: step(Action{Type: "comment", OnError: "explode"})},
		"only if template":    {Trigger: manual, Actions: step(Action{Type: "comment", OnlyIf: "{{"})},
		"message template":    {Trigger: manual, Actions: step(Action{Type: "comment", Message: "{{.Issue"})},
		"input name":          {Trigger: manual, Actions: step(comment), Inputs: []RuleInput{{Name: "Bad Name", Type: "text"}}},
		"input type":          {Trigger: manual, Actions: step(comment), Inputs: []RuleInput{{Name: "x", Type: "date"}}},
		"input select":        {Trigger: manual, Actions: step(comment), Inputs: []RuleInput{{Name: "x", Type: "select"}}},
		"concurrency":         {Trigger: manual, Actions: step(comment), Concurrency: "weird"},
		"time zone":           {Trigger: manual, Actions: step(comment), TimeZone: "Mars/Base"},
		"system trigger":      {Trigger: Trigger{Event: "system_resources"}, Actions: step(comment)},
		"wait_for unknown":    {Trigger: manual, Actions: step(Action{Type: "wait_for", Extra: map[string]any{"event": "no.such", "timeout": "1h"}})},
		"wait_for no timeout": {Trigger: manual, Actions: step(Action{Type: "wait_for", Extra: map[string]any{"event": "pr.ci"}})},
		"delay duration":      {Trigger: manual, Actions: step(Action{Type: "delay", Extra: map[string]any{"duration": "soon"}})},
		"delay too long":      {Trigger: manual, Actions: step(Action{Type: "delay", Extra: map[string]any{"duration": "90d"}})},
		"ask_model prompt":    {Trigger: manual, Actions: step(Action{Type: "ask_model"})},
		"no actions":          {Trigger: manual},
		"too many models":     {Trigger: manual, Actions: step(Action{Type: "ask_model", Extra: map[string]any{"prompt": "x"}, Models: make([]ChainModel, 11)})},
	}
	for name, r := range bad {
		r := r
		if err := r.validate(); err == nil || !IsInvalid(err) {
			t.Errorf("%s: accepted or not an invalid-input error: %v", name, err)
		}
	}
	good := map[string]Rule{
		"cron":    {Trigger: Trigger{Event: "schedule", Every: "cron", Extra: map[string]any{"cron": "*/15 9-17 * * mon-fri"}}, Actions: step(comment)},
		"count":   {Trigger: Trigger{Event: "count_crossed", Extra: map[string]any{"query": "is:open type:bug", "op": ">=", "n": 5}}, Actions: step(comment)},
		"created": {Trigger: Trigger{Event: "created", Extra: map[string]any{"by": "agent"}}, Actions: step(comment)},
		"steps": {Trigger: manual, Inputs: []RuleInput{{Name: "focus", Type: "text"}, {Name: "n", Type: "number", Default: "3"}},
			Concurrency: "queue", TimeZone: "Europe/London", AllowAgents: true,
			Actions: step(
				Action{Type: "ask_model", ID: "ask", Extra: map[string]any{"prompt": "Summarise {{.Inputs.focus}}"}, OnError: "retry", Retries: 2, RetryDelay: "30s"},
				Action{Type: "repeat", ID: "loop", Extra: map[string]any{"max": 3, "until": "{{.Steps.ask.OK}}"}, Steps: step(Action{Type: "delay", Extra: map[string]any{"duration": "5m"}}, comment)},
				Action{Type: "wait_for", Extra: map[string]any{"event": "pr.ci", "key": "pr:{{.Trigger.Data.pr}}", "where": "state=passed", "timeout": "2h"}, OnlyIf: "{{.Steps.ask.OK}}"},
			)},
	}
	for name, r := range good {
		r := r
		if err := r.validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	admin := Rule{Trigger: Trigger{Event: "system_resources"}, Actions: step(Action{Type: "notify", User: "admins", Message: "low"})}
	if err := admin.validateFor(true); err != nil {
		t.Errorf("admin automation with a system trigger: %v", err)
	}
}

// Rules saved before steps existed load, validate, run and save unchanged.
func TestOldRulesStillWork(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Old", "")
	oldRules := []string{
		`{"trigger":{"event":"status_changed","to":"in_progress"},"if":"is:unassigned","actions":[{"type":"assign","user":"actor"},{"type":"comment","message":"Started {{.Issue.Key}} by {{.Actor.Username}}"}]}`,
		`{"trigger":{"event":"schedule","every":"week","weekday":1,"at":"09:00"},"actions":[{"type":"create_issue","title":"Plan the week of {{.Date}}","due":"+2d"}]}`,
		`{"trigger":{"event":"pr_updated"},"pr":{"skip_labels":["dependencies"],"skip_heads":["release-please--*"]},"actions":[{"type":"review","models":[{"provider_id":1,"model":"opencode/muse-spark-*"}],"focus":"security","force":true,"max_findings":5}]}`,
		`{"trigger":{"event":"created"},"if":"priority:urgent","actions":[{"type":"gotify","user":"watchers","title":"T","message":"M","priority":8},{"type":"webhook","url":"https://example.com/hook","body":"{\"k\":1}"}]}`,
	}
	var ids []int64
	for n, text := range oldRules {
		var r Rule
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			t.Fatalf("rule %d: %v", n, err)
		}
		if err := r.validate(); err != nil {
			t.Fatalf("rule %d no longer validates: %v", n, err)
		}
		res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO automations(project_id, name, enabled, rule, position, created_at, updated_at) VALUES (?,?,1,?,?,0,0)`, p.ID, fmt.Sprintf("old %d", n), text, n+10)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
		// Saving what was loaded writes the same JSON back, key for key.
		back, _ := json.Marshal(r)
		var a, b map[string]any
		_ = json.Unmarshal([]byte(text), &a)
		_ = json.Unmarshal(back, &b)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("rule %d changed in a round trip:\n%s\n%s", n, text, back)
		}
	}
	// The first rule runs as it always did.
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "work"})
	if _, err := e.s.Transition(e.ctx, e.admin, i.Key, "In Progress", ""); err != nil {
		t.Fatal(err)
	}
	got := e.get(e.admin, i.Key)
	if len(got.Assignees) != 1 || got.Assignees[0].Username != "robin" {
		t.Errorf("old rule didn't assign: %+v", got.Assignees)
	}
	if cs := e.commentBodies(i.Key); len(cs) != 1 || cs[0] != "Started "+i.Key+" by robin" {
		t.Errorf("old rule comment = %q", cs)
	}
	// It left a run behind, which the old format never did.
	a, err := e.s.GetAutomation(e.ctx, e.admin, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if a.RunCount != 1 {
		t.Errorf("run_count = %d", a.RunCount)
	}
	runs := e.runs(a)
	if len(runs) != 1 || runs[0].Status != RunSucceeded {
		t.Fatalf("runs = %+v", runs)
	}
	r := e.run(runs[0].ID)
	if len(r.Steps) != 2 || r.Steps[0].StepID != "s1" || r.Steps[1].Type != "comment" {
		t.Errorf("steps = %+v", r.Steps)
	}
	// Unknown keys on a rule (a newer version's) are kept, not dropped.
	var withExtra Rule
	if err := json.Unmarshal([]byte(`{"trigger":{"event":"created","by":"agent","future":1},"actions":[{"type":"comment","message":"x","note_to_self":true}]}`), &withExtra); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(withExtra)
	if !strings.Contains(string(out), `"future":1`) || !strings.Contains(string(out), `"note_to_self":true`) || !strings.Contains(string(out), `"by":"agent"`) {
		t.Errorf("extra keys lost: %s", out)
	}
}

// ---- runs ----

func TestRunsRecordInstantAutomations(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Runs", "")
	a := e.addAuto(p, "Triage", Rule{Trigger: Trigger{Event: "created"}, If: "type:bug", Actions: []Action{
		{Type: "add_label", ID: "tag", Value: "triage"},
		{Type: "create_issue", ID: "followup", Title: "Follow up on {{.Issue.Key}}"},
		{Type: "comment", ID: "note", Message: "Filed {{.Steps.followup.Key}} ({{.Steps.tag.Status}}) via {{.Trigger.Name}} by {{.Trigger.By}}"},
		{Type: "comment", ID: "skipme", OnlyIf: "{{.Steps.tag.Failed}}", Message: "never"},
	}})
	bug := e.issue(e.admin, IssueInput{Project: p.Key, Type: "bug", Title: "Broken"})
	e.issue(e.admin, IssueInput{Project: p.Key, Type: "task", Title: "Not a bug"}) // the condition doesn't hold: no run
	runs := e.runs(a)
	if len(runs) != 1 {
		t.Fatalf("%d runs", len(runs))
	}
	r := e.run(runs[0].ID)
	if r.Status != RunSucceeded || r.Via != "event" || r.Trigger != "created" || r.Durable || r.FinishedAt == nil || r.Actor == nil || r.Actor.Username != "robin" {
		t.Errorf("run = %+v", r)
	}
	if !strings.Contains(r.Summary, bug.Key) {
		t.Errorf("summary = %q", r.Summary)
	}
	if len(r.Steps) != 4 {
		t.Fatalf("steps = %d", len(r.Steps))
	}
	if s := stepByID(r, "followup"); s.Status != "succeeded" || s.Outputs["Key"] == nil || s.Inputs["title"] != "Follow up on "+bug.Key {
		t.Errorf("create_issue step = %+v", s)
	}
	key := stepByID(r, "followup").Outputs["Key"].(string)
	want := "Filed " + key + " (succeeded) via created by person"
	if cs := e.commentBodies(bug.Key); len(cs) != 1 || cs[0] != want {
		t.Errorf("comment = %q, want %q", cs, want)
	}
	if s := stepByID(r, "skipme"); s.Status != "skipped" || s.Log != "only if was false" {
		t.Errorf("only-if step = %+v", s)
	}
	if r.Rule == nil || len(r.Rule.Actions) != 4 {
		t.Errorf("run lacks the rule it ran: %+v", r.Rule)
	}
	// The rule snapshot is the rule as it ran, even after an edit.
	edited := Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "add_label", Value: "other"}}}
	if _, err := e.s.UpdateAutomation(e.ctx, e.admin, a.ID, AutomationInput{Rule: &edited}); err != nil {
		t.Fatal(err)
	}
	if r2 := e.run(r.ID); len(r2.Rule.Actions) != 4 {
		t.Errorf("snapshot changed with the automation: %+v", r2.Rule)
	}

	// A failing step stops the run, records the error and is counted.
	f := e.addAuto(p, "Fails", Rule{Trigger: Trigger{Event: "assigned"}, Actions: []Action{
		{Type: "set_status", Status: "No Such Status"}, {Type: "comment", Message: "unreached"}}})
	i2 := e.issue(e.admin, IssueInput{Project: p.Key, Title: "assign me"})
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i2.Key, IssuePatch{Assignees: &[]string{"robin"}}); err != nil {
		t.Fatal(err)
	}
	fr := e.runs(f)
	if len(fr) != 1 || fr[0].Status != RunFailed || !strings.Contains(fr[0].Error, "set_status") {
		t.Fatalf("failed run = %+v", fr)
	}
	d := e.run(fr[0].ID)
	if len(d.Steps) != 1 || d.Steps[0].Status != "failed" || d.Steps[0].Error == "" {
		t.Errorf("failed steps = %+v", d.Steps)
	}
	if cur, _ := e.s.GetAutomation(e.ctx, e.admin, f.ID); cur.RunCount != 1 || cur.LastError == "" {
		t.Errorf("stats = %+v", cur)
	}
	// on_error: continue goes on past a failing step.
	c := e.addAuto(p, "Carries on", Rule{Trigger: Trigger{Event: "unassigned"}, Actions: []Action{
		{Type: "set_status", ID: "bad", Status: "No Such Status", OnError: "continue"},
		{Type: "comment", ID: "after", OnlyIf: "{{.Previous.Failed}}", Message: "recovered: {{.Steps.bad.Failed}}"}}})
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i2.Key, IssuePatch{Assignees: &[]string{}}); err != nil {
		t.Fatal(err)
	}
	cr := e.runs(c)
	if len(cr) != 1 || cr[0].Status != RunSucceeded {
		t.Fatalf("continue run = %+v", cr)
	}
	if cs := e.commentBodies(i2.Key); len(cs) != 1 || cs[0] != "recovered: true" {
		t.Errorf("comments = %q", cs)
	}
}

func TestOnlyIfPresetsAndTemplateFuncs(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Funcs", "")
	e.addAuto(p, "Funcs", Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{
		{Type: "comment", OnlyIf: `{{eq .Trigger.By "person"}}`, Message: `{{upper "ok"}} {{truncate 3 "abcdef"}} {{default "none" .Inputs.missing}} {{json .Trigger.Data}}`},
		{Type: "comment", OnlyIf: `{{eq .Trigger.By "agent"}}`, Message: "agent only"},
	}})
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "x"})
	cs := e.commentBodies(i.Key)
	if len(cs) != 1 || !strings.HasPrefix(cs[0], "OK abc… none ") {
		t.Fatalf("comments = %q", cs)
	}
	if got := truthy("<no value>") || truthy("") || truthy("false") || truthy(" 0 "); got {
		t.Error("falsy strings are truthy")
	}
	if !truthy("yes") || !truthy("true") {
		t.Error("truthy strings are falsy")
	}
}

func TestDurableRunWaitsAndResumes(t *testing.T) {
	registerTestActions()
	e := newEnv(t)
	p := e.project(e.admin, "Durable", "")
	a := e.addAuto(p, "Slow", Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{
		{Type: "comment", ID: "first", Message: "starting"},
		{Type: "delay", ID: "pause", Extra: map[string]any{"duration": "10m"}},
		{Type: "comment", ID: "second", Message: "waited {{.Steps.first.OK}}"},
	}})
	if !a.Durable {
		t.Fatal("a rule with a delay isn't durable")
	}
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "x"})
	// The request returned without waiting: the run is queued for the worker.
	runs := e.runs(a)
	if len(runs) != 1 || runs[0].Status != RunQueued || !runs[0].Durable {
		t.Fatalf("after the change: %+v", runs)
	}
	if len(e.commentBodies(i.Key)) != 0 {
		t.Fatal("a durable run's steps ran inside the request")
	}
	e.s.DrainAutomationRuns(e.ctx)
	r := e.run(runs[0].ID)
	if r.Status != RunWaiting {
		t.Fatalf("status = %s, want waiting", r.Status)
	}
	if cs := e.commentBodies(i.Key); len(cs) != 1 || cs[0] != "starting" {
		t.Fatalf("comments = %q", cs)
	}
	if s := stepByID(r, "pause"); s == nil || s.Status != "waiting" {
		t.Fatalf("delay step = %+v", s)
	}
	// Not yet: nothing happens a minute on.
	e.setClock(e.clockNow().Add(time.Minute))
	e.s.DrainAutomationRuns(e.ctx)
	if e.run(r.ID).Status != RunWaiting {
		t.Fatal("run resumed early")
	}
	// A restart in the meantime changes nothing: waits are in the database.
	s2 := New(e.s.db, Config{BaseURL: "https://wp.test"}, nil)
	s2.SetClock(e.clockNow)
	s2.resumeRuns(e.ctx)
	s2.DrainAutomationRuns(e.ctx)
	if e.run(r.ID).Status != RunWaiting {
		t.Fatal("run lost its wait over a restart")
	}
	e.setClock(e.clockNow().Add(10 * time.Minute))
	s2.DrainAutomationRuns(e.ctx)
	done := e.run(r.ID)
	if done.Status != RunSucceeded || done.FinishedAt == nil {
		t.Fatalf("status = %s (%s)", done.Status, done.Error)
	}
	if cs := e.commentBodies(i.Key); len(cs) != 2 || cs[1] != "waited true" {
		t.Errorf("comments = %q", cs)
	}
	// The first step ran once, not again on resume.
	if s := stepByID(done, "first"); s.Attempts != 1 {
		t.Errorf("first step attempts = %d", s.Attempts)
	}
}

// A run interrupted mid-step (the server stopped) is queued again at boot,
// keeps what finished, and runs the interrupted step again.
func TestDurableRunSurvivesARestartMidStep(t *testing.T) {
	registerTestActions()
	e := newEnv(t)
	p := e.project(e.admin, "Crash", "")
	a := e.addAuto(p, "Crashy", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "test_count", ID: "one", Extra: map[string]any{"key": "crash-one"}},
		{Type: "test_flaky", ID: "two", Extra: map[string]any{"key": "crash-two"}},
	}})
	run := e.manual(a, nil)
	// Claim it and stop after the first step, as a crash would: the run is
	// "running", step one finished, step two started and never ended.
	claimed, err := e.s.claimAutomationRun(e.ctx)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	now := e.s.now().UTC()
	if err := e.s.saveStep(e.ctx, run.ID, &RunStep{StepID: "one", Iteration: 1, Type: "test_count", Status: "succeeded", Attempts: 1, StartedAt: &now, FinishedAt: &now, Outputs: map[string]any{"N": 1}}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.saveStep(e.ctx, run.ID, &RunStep{StepID: "two", Iteration: 1, Position: 1, Type: "test_flaky", Status: "running", Attempts: 1, Long: true, StartedAt: &now}); err != nil {
		t.Fatal(err)
	}
	if got := e.run(run.ID).Status; got != RunRunning {
		t.Fatalf("status = %s", got)
	}
	s2 := New(e.s.db, Config{BaseURL: "https://wp.test"}, nil)
	s2.SetClock(e.clockNow)
	s2.resumeRuns(e.ctx)
	s2.DrainAutomationRuns(e.ctx)
	done := e.run(run.ID)
	if done.Status != RunSucceeded {
		t.Fatalf("status = %s (%s)", done.Status, done.Error)
	}
	if flakyCount("crash-one") != 0 {
		t.Error("a finished step ran again after the restart")
	}
	if flakyCount("crash-two") != 1 {
		t.Errorf("the interrupted step ran %d times", flakyCount("crash-two"))
	}
	if s := stepByID(done, "two"); s.Attempts != 1 || s.Status != "succeeded" {
		t.Errorf("interrupted step = %+v", s)
	}
}

func TestRetryOnError(t *testing.T) {
	registerTestActions()
	e := newEnv(t)
	p := e.project(e.admin, "Retry", "")
	a := e.addAuto(p, "Retries", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "test_flaky", ID: "flaky", Extra: map[string]any{"key": "retry-a", "fail": 2}, OnError: "retry", Retries: 3, RetryDelay: "2m"},
		{Type: "test_count", ID: "after", Extra: map[string]any{"key": "retry-after"}},
	}})
	run := e.manual(a, nil)
	e.s.DrainAutomationRuns(e.ctx)
	if r := e.run(run.ID); r.Status != RunWaiting || stepByID(r, "flaky").Attempts != 1 {
		t.Fatalf("after the first try: %s %+v", r.Status, stepByID(r, "flaky"))
	}
	e.setClock(e.clockNow().Add(2 * time.Minute))
	e.s.DrainAutomationRuns(e.ctx)
	if r := e.run(run.ID); r.Status != RunWaiting || stepByID(r, "flaky").Attempts != 2 {
		t.Fatalf("after the second try: %s %+v", r.Status, stepByID(r, "flaky"))
	}
	e.setClock(e.clockNow().Add(2 * time.Minute))
	e.s.DrainAutomationRuns(e.ctx)
	r := e.run(run.ID)
	if r.Status != RunSucceeded {
		t.Fatalf("status = %s (%s)", r.Status, r.Error)
	}
	if s := stepByID(r, "flaky"); s.Attempts != 3 || s.Status != "succeeded" || !strings.Contains(s.Log, "try 1 failed") || !strings.Contains(s.Log, "try 2 failed") {
		t.Errorf("retried step = %+v", s)
	}
	if flakyCount("retry-a") != 3 || flakyCount("retry-after") != 1 {
		t.Errorf("calls: flaky %d, after %d", flakyCount("retry-a"), flakyCount("retry-after"))
	}

	// Out of retries: the run fails.
	b := e.addAuto(p, "Gives up", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "test_flaky", ID: "flaky", Extra: map[string]any{"key": "retry-b", "fail": 9}, OnError: "retry", Retries: 1, RetryDelay: "1m"}}})
	run2 := e.manual(b, nil)
	e.s.DrainAutomationRuns(e.ctx)
	e.setClock(e.clockNow().Add(time.Minute))
	e.s.DrainAutomationRuns(e.ctx)
	r2 := e.run(run2.ID)
	if r2.Status != RunFailed || !strings.Contains(r2.Error, "flaky failure 2") || flakyCount("retry-b") != 2 {
		t.Errorf("exhausted retries: %s %q calls %d", r2.Status, r2.Error, flakyCount("retry-b"))
	}
}

func TestRepeatUntil(t *testing.T) {
	registerTestActions()
	e := newEnv(t)
	p := e.project(e.admin, "Repeat", "")
	a := e.addAuto(p, "Babysit", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "repeat", ID: "loop", Extra: map[string]any{"max": 5, "until": "{{.Steps.poll.Done}}"}, Steps: []Action{
			{Type: "test_flaky", ID: "poll", Extra: map[string]any{"key": "repeat-a", "stop_at": 3}},
			{Type: "delay", ID: "gap", Extra: map[string]any{"duration": "1m"}},
		}},
		{Type: "test_count", ID: "after", Extra: map[string]any{"key": "repeat-after"}},
	}})
	run := e.manual(a, nil)
	for range 10 {
		e.s.DrainAutomationRuns(e.ctx)
		if e.run(run.ID).Finished() {
			break
		}
		e.setClock(e.clockNow().Add(time.Minute))
	}
	r := e.run(run.ID)
	if r.Status != RunSucceeded {
		t.Fatalf("status = %s (%s)", r.Status, r.Error)
	}
	if flakyCount("repeat-a") != 3 {
		t.Errorf("the group ran %d times, want 3", flakyCount("repeat-a"))
	}
	var polls int
	for _, s := range r.Steps {
		if s.StepID == "poll" {
			polls++
			if s.Iteration != polls {
				t.Errorf("poll iteration %d recorded as %d", polls, s.Iteration)
			}
		}
	}
	if polls != 3 {
		t.Errorf("%d poll rows", polls)
	}
	loop := stepByID(r, "loop")
	if loop.Outputs["Iterations"] != float64(3) || loop.Outputs["Exhausted"] != false {
		t.Errorf("group outputs = %+v", loop.Outputs)
	}
	if flakyCount("repeat-after") != 1 {
		t.Error("the step after the group didn't run once")
	}

	// A condition that never holds stops at the maximum and says so.
	b := e.addAuto(p, "Never", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "repeat", ID: "loop", Extra: map[string]any{"max": 2, "until": "{{.Steps.poll.Failed}}"}, Steps: []Action{
			{Type: "test_count", ID: "poll", Extra: map[string]any{"key": "repeat-b"}}}},
		{Type: "notify", User: "admins", ID: "x", OnlyIf: "{{.Steps.loop.Exhausted}}", Message: "gave up"}}})
	rb := e.manual(b, nil)
	got := e.run(rb.ID)
	if got.Status != RunSucceeded || flakyCount("repeat-b") != 2 || stepByID(got, "loop").Outputs["Exhausted"] != true {
		t.Errorf("exhausted group: %s calls %d %+v", got.Status, flakyCount("repeat-b"), stepByID(got, "loop"))
	}
}

func TestWaitForEvent(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Waits", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "watched"})
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "other"})
	a := e.addAuto(p, "Waits", Rule{Trigger: Trigger{Event: "manual"}, Inputs: []RuleInput{{Name: "issue", Type: "text"}}, Actions: []Action{
		{Type: "wait_for", ID: "w", Extra: map[string]any{"event": EvIssueCommented, "key": "issue:{{.Inputs.issue}}", "timeout": "1h"}},
		{Type: "notify", User: "admins", Message: "saw {{.Steps.w.Event.Kind}} on {{.Steps.w.Event.IssueKey}}"},
	}})
	run := e.manual(a, map[string]any{"issue": i.Key})
	e.s.DrainAutomationRuns(e.ctx)
	if e.run(run.ID).Status != RunWaiting {
		t.Fatal("not waiting")
	}
	// An event about another issue doesn't satisfy it.
	if _, err := e.s.AddComment(e.ctx, e.admin, other.Key, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	e.s.DrainAutomationRuns(e.ctx)
	if e.run(run.ID).Status != RunWaiting {
		t.Fatal("an event about another issue resumed the run")
	}
	// The matching event queues the run in dispatch; the worker finishes it.
	if _, err := e.s.AddComment(e.ctx, e.admin, i.Key, "now"); err != nil {
		t.Fatal(err)
	}
	if got := e.run(run.ID).Status; got != RunQueued {
		t.Fatalf("after the event: %s", got)
	}
	e.s.DrainAutomationRuns(e.ctx)
	r := e.run(run.ID)
	if r.Status != RunSucceeded {
		t.Fatalf("status = %s (%s)", r.Status, r.Error)
	}
	w := stepByID(r, "w")
	if w.Outputs["TimedOut"] != false || w.Outputs["Event"].(map[string]any)["Kind"] != EvIssueCommented {
		t.Errorf("wait outputs = %+v", w.Outputs)
	}

	// Timing out fails the step, or carries on when asked.
	run2 := e.manual(a, map[string]any{"issue": i.Key})
	e.s.DrainAutomationRuns(e.ctx)
	e.setClock(e.clockNow().Add(2 * time.Hour))
	e.s.DrainAutomationRuns(e.ctx)
	r2 := e.run(run2.ID)
	if r2.Status != RunFailed || !strings.Contains(r2.Error, "timed out") {
		t.Errorf("timed out run = %s %q", r2.Status, r2.Error)
	}
	b := e.addAuto(p, "Carries on", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "wait_for", ID: "w", Extra: map[string]any{"event": EvIssueCommented, "key": "issue:" + i.Key, "timeout": "30m", "on_timeout": "continue"}},
		{Type: "notify", User: "admins", OnlyIf: "{{.Steps.w.TimedOut}}", Message: "nobody answered"}}})
	run3 := e.manual(b, nil)
	e.s.DrainAutomationRuns(e.ctx)
	e.setClock(e.clockNow().Add(time.Hour))
	e.s.DrainAutomationRuns(e.ctx)
	r3 := e.run(run3.ID)
	if r3.Status != RunSucceeded || stepByID(r3, "w").Outputs["TimedOut"] != true || stepByID(r3, "s2").Status != "succeeded" {
		t.Errorf("continue-on-timeout run = %s %+v", r3.Status, r3.Steps)
	}
	ns, _, _ := e.s.ListNotifications(e.ctx, e.admin, false, 20)
	var found bool
	for _, n := range ns {
		found = found || n.Body == "nobody answered"
	}
	if !found {
		t.Error("the notification after the timeout wasn't sent")
	}
	// Conditions on the event's data: where=.
	if !matchConditions("state=passed, verdict!=approved", &Event{Data: map[string]any{"state": "passed", "verdict": "commented"}}) ||
		matchConditions("state=passed", &Event{Data: map[string]any{"state": "failed"}}) ||
		matchConditions("verdict!=approved", &Event{Data: map[string]any{"verdict": "approved"}}) {
		t.Error("matchConditions is wrong")
	}
}

func TestRequestWaitingWatch(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Requests", "")
	a := e.addAuto(p, "Late", Rule{Trigger: Trigger{Event: "request_waiting", Extra: map[string]any{"after": "15m"}},
		Actions: []Action{{Type: "notify", User: "admins", Message: "request {{.Trigger.Data.request}} is waiting"}}})
	created := func(id string) {
		e.s.automate(e.ctx, SystemActor(), &Event{Kind: EvRequestCreated, ProjectID: p.ID, Data: map[string]any{"request": id, "kind": "question"}})
	}
	created("R-1")
	created("R-2")
	e.s.automate(e.ctx, SystemActor(), &Event{Kind: EvRequestUpdated, ProjectID: p.ID, Data: map[string]any{"request": "R-1", "decision": "answered"}})
	e.setClock(e.clockNow().Add(14 * time.Minute))
	e.s.ExpireAutomationWaits(e.ctx)
	if len(e.runs(a)) != 0 {
		t.Fatal("fired early")
	}
	e.setClock(e.clockNow().Add(2 * time.Minute))
	e.s.ExpireAutomationWaits(e.ctx)
	runs := e.runs(a)
	if len(runs) != 1 {
		t.Fatalf("%d runs; only the undecided request should fire", len(runs))
	}
	r := e.run(runs[0].ID)
	if r.Status != RunSucceeded || r.Event == nil || r.Event.Data["request"] != "R-2" {
		t.Errorf("run = %+v event %+v", r, r.Event)
	}
	// An update without a decision doesn't end a watch.
	created("R-3")
	e.s.automate(e.ctx, SystemActor(), &Event{Kind: EvRequestUpdated, ProjectID: p.ID, Data: map[string]any{"request": "R-3", "title": "renamed"}})
	e.setClock(e.clockNow().Add(20 * time.Minute))
	e.s.ExpireAutomationWaits(e.ctx)
	if len(e.runs(a)) != 2 {
		t.Errorf("%d runs after an update without a decision", len(e.runs(a)))
	}
}

func TestConcurrencyPolicies(t *testing.T) {
	registerTestActions()
	e := newEnv(t)
	p := e.project(e.admin, "Concurrency", "")
	slow := func(name, policy string) *Automation {
		return e.addAuto(p, name, Rule{Trigger: Trigger{Event: "manual"}, Concurrency: policy, Actions: []Action{
			{Type: "delay", Extra: map[string]any{"duration": "10m"}}, {Type: "test_count", Extra: map[string]any{"key": "conc-" + name}}}})
	}
	// parallel: both go.
	par := slow("par", "")
	e.manual(par, nil)
	e.manual(par, nil)
	e.s.DrainAutomationRuns(e.ctx)
	if n := len(e.runs(par)); n != 2 {
		t.Errorf("parallel: %d runs", n)
	}
	for _, r := range e.runs(par) {
		if r.Status != RunWaiting {
			t.Errorf("parallel run %d is %s", r.ID, r.Status)
		}
	}
	// skip: the second is recorded as skipped and does nothing.
	skip := slow("skip", "skip")
	first := e.manual(skip, nil)
	second := e.manual(skip, nil)
	if second.Status != RunSkipped || !strings.Contains(second.Summary, "already in progress") {
		t.Errorf("skip: second run = %s %q", second.Status, second.Summary)
	}
	e.s.DrainAutomationRuns(e.ctx)
	e.setClock(e.clockNow().Add(11 * time.Minute))
	e.s.DrainAutomationRuns(e.ctx)
	if e.run(first.ID).Status != RunSucceeded || flakyCount("conc-skip") != 1 {
		t.Errorf("skip: first = %s, calls %d", e.run(first.ID).Status, flakyCount("conc-skip"))
	}
	if cur, _ := e.s.GetAutomation(e.ctx, e.admin, skip.ID); cur.RunCount != 1 {
		t.Errorf("a skipped run counted: %d", cur.RunCount)
	}
	// queue: the second waits for the first to finish.
	queue := slow("queue", "queue")
	q1 := e.manual(queue, nil)
	q2 := e.manual(queue, nil)
	e.s.DrainAutomationRuns(e.ctx)
	if e.run(q1.ID).Status != RunWaiting || e.run(q2.ID).Status != RunQueued {
		t.Fatalf("queue: %s then %s", e.run(q1.ID).Status, e.run(q2.ID).Status)
	}
	e.setClock(e.clockNow().Add(11 * time.Minute))
	e.s.DrainAutomationRuns(e.ctx)
	if e.run(q1.ID).Status != RunSucceeded || e.run(q2.ID).Status != RunWaiting {
		t.Fatalf("queue after the first: %s then %s", e.run(q1.ID).Status, e.run(q2.ID).Status)
	}
	e.setClock(e.clockNow().Add(11 * time.Minute))
	e.s.DrainAutomationRuns(e.ctx)
	if e.run(q2.ID).Status != RunSucceeded {
		t.Errorf("queue: second = %s", e.run(q2.ID).Status)
	}
	// Schedules default to skip; everything else to parallel.
	if (&Rule{Trigger: Trigger{Event: "schedule"}}).concurrency() != "skip" || (&Rule{Trigger: Trigger{Event: "created"}}).concurrency() != "parallel" {
		t.Error("default concurrency")
	}
}

func TestCancelRun(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Cancel", "")
	a := e.addAuto(p, "Slow", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "delay", Extra: map[string]any{"duration": "1h"}}, {Type: "notify", User: "admins", Message: "never"}}})
	run := e.manual(a, nil)
	e.s.DrainAutomationRuns(e.ctx)
	got, err := e.s.CancelAutomationRun(e.ctx, e.admin, run.ID)
	if err != nil || got.Status != RunCancelled || got.FinishedAt == nil {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	e.setClock(e.clockNow().Add(2 * time.Hour))
	e.s.DrainAutomationRuns(e.ctx)
	r := e.run(run.ID)
	if r.Status != RunCancelled {
		t.Errorf("status after the wait would have ended = %s", r.Status)
	}
	if s := r.Steps[0]; s.Status != "cancelled" {
		t.Errorf("step = %+v", s)
	}
	ns, _, _ := e.s.ListNotifications(e.ctx, e.admin, false, 20)
	for _, n := range ns {
		if n.Body == "never" {
			t.Error("a cancelled run went on")
		}
	}
	// Cancelling a finished run changes nothing; a viewer can't cancel.
	if again, err := e.s.CancelAutomationRun(e.ctx, e.admin, run.ID); err != nil || again.Status != RunCancelled {
		t.Errorf("second cancel: %+v %v", again, err)
	}
	viewer := e.user("vic", false)
	e.s.SetMember(e.ctx, e.admin, p.Key, "vic", "viewer")
	run2 := e.manual(a, nil)
	if _, err := e.s.CancelAutomationRun(e.ctx, viewer, run2.ID); err == nil {
		t.Error("a viewer cancelled a run")
	}
}

func TestRunAgainAndPruning(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Again", "")
	a := e.addAuto(p, "Mark", Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "add_label", Value: "seen"}}})
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "x"})
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{RemLabels: []string{"seen"}}); err != nil {
		t.Fatal(err)
	}
	runs := e.runs(a)
	again, err := e.s.RerunAutomation(e.ctx, e.admin, runs[0].ID)
	if err != nil || again.Status != RunSucceeded || again.RerunOf == nil || *again.RerunOf != runs[0].ID || again.Via != "rerun" {
		t.Fatalf("rerun: %+v %v", again, err)
	}
	if got := e.get(e.admin, i.Key); len(got.Labels) != 1 {
		t.Errorf("rerun didn't repeat the effect: %+v", got.Labels)
	}
	// Only owners repeat an event's effects.
	sam := e.user("sam", false)
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	if _, err := e.s.RerunAutomation(e.ctx, sam, runs[0].ID); err == nil {
		t.Error("an editor repeated an event-triggered run")
	}

	// Retention: runs older than 90 days go, but the last 200 stay.
	old := e.clockNow().Add(-100 * 24 * time.Hour).UnixMilli()
	for n := range 205 {
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO automation_runs(automation_id, project_id, rule_id, status, created_at, finished_at)
			VALUES (?,?,(SELECT rule_id FROM automation_runs WHERE id = ?),'succeeded',?,?)`, a.ID, p.ID, runs[0].ID, old+int64(n), old+int64(n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.PruneAutomationRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	var total, oldLeft int
	_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM automation_runs WHERE automation_id = ?`, a.ID).Scan(&total)
	_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM automation_runs WHERE automation_id = ? AND finished_at < ?`, a.ID, e.clockNow().Add(-90*24*time.Hour).UnixMilli()).Scan(&oldLeft)
	// The 2 recent runs stay, and the newest 200 of the old ones.
	if total != 202 || oldLeft != 200 {
		t.Errorf("after pruning: %d runs, %d old", total, oldLeft)
	}
	// With few runs everything stays however old.
	b := e.addAuto(p, "Rare", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "comment", Message: "x"}}})
	rb := e.manual(b, nil)
	_, _ = e.s.db.W.ExecContext(e.ctx, `UPDATE automation_runs SET finished_at = ? WHERE id = ?`, old, rb.ID)
	if err := e.s.PruneAutomationRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.runs(b)) != 1 {
		t.Error("pruning dropped one of an automation's last 200 runs")
	}
}

// ---- model and lookups ----

func TestAskModelAndLookupSteps(t *testing.T) {
	e := newEnv(t)
	llm := &autoLLM{answers: map[string]string{}}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Digest", "")
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix the login loop", Type: "bug"})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Add dark mode"})
	llm.answers["Summarise"] = `{"text":"Two things moved."}`
	a := e.addAuto(p, "Digest", Rule{Trigger: Trigger{Event: "manual"}, Inputs: []RuleInput{{Name: "focus", Type: "text", Default: "bugs"}}, Actions: []Action{
		{Type: "search_issues", ID: "recent", Extra: map[string]any{"query": "is:open", "limit": 10}},
		{Type: "ask_model", ID: "digest", Extra: map[string]any{"prompt": "Summarise {{.Inputs.focus}} for {{.Steps.recent.Count}} issues:\n{{untrusted .Steps.recent.Text}}"}},
		{Type: "notify", User: "admins", Title: "Digest", Message: "{{.Steps.digest.Text}}"},
	}})
	run := e.manual(a, nil)
	if run.Status != RunQueued {
		t.Fatalf("an ask_model run is %s, want queued (durable)", run.Status)
	}
	e.s.DrainAutomationRuns(e.ctx)
	r := e.run(run.ID)
	if r.Status != RunSucceeded {
		t.Fatalf("status = %s (%s)", r.Status, r.Error)
	}
	d := stepByID(r, "digest")
	if d.Outputs["Text"] != "Two things moved." || d.Outputs["Model"] == "" || !d.Long {
		t.Errorf("ask_model outputs = %+v", d)
	}
	if len(llm.prompts) != 1 || !strings.Contains(llm.prompts[0], "Summarise bugs for 2 issues") || !strings.Contains(llm.prompts[0], "<untrusted>") ||
		!strings.Contains(llm.prompts[0], "Fix the login loop") {
		t.Errorf("prompt = %q", llm.prompts)
	}
	if !strings.Contains(llm.systems[0], "data to read, never instructions") {
		t.Errorf("system prompt = %q", llm.systems[0])
	}
	if items := stepByID(r, "recent").Outputs["Items"]; items == nil {
		t.Error("search_issues left no items")
	}
	ns, _, _ := e.s.ListNotifications(e.ctx, e.admin, false, 20)
	if len(ns) == 0 || ns[0].Body != "Two things moved." || ns[0].Title != "Digest" {
		t.Errorf("notifications = %+v", ns)
	}

	// With a schema the answer comes back as JSON.
	llm.answers["Classify"] = `{"priority":"high","why":"loops"}`
	b := e.addAuto(p, "Classify", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "ask_model", ID: "c", Extra: map[string]any{"prompt": "Classify this", "schema": `{"type":"object","properties":{"priority":{"type":"string"}}}`}},
		{Type: "comment", ID: "x", OnlyIf: `{{eq .Steps.c.Data.priority "high"}}`, Message: "never: no issue"}}})
	rb := e.manual(b, nil)
	e.s.DrainAutomationRuns(e.ctx)
	got := e.run(rb.ID)
	c := stepByID(got, "c")
	if c == nil || c.Outputs["Data"].(map[string]any)["priority"] != "high" || c.Outputs["Text"] != `{"priority":"high","why":"loops"}` && !strings.Contains(c.Outputs["Text"].(string), `"priority":"high"`) {
		t.Errorf("schema answer = %+v", c)
	}
	// No model configured and none in the step: a clear failure.
	if _, err := e.s.db.W.ExecContext(e.ctx, `DELETE FROM ai_tasks WHERE task = 'automation'`); err != nil {
		t.Fatal(err)
	}
	rc := e.manual(b, nil)
	e.s.DrainAutomationRuns(e.ctx)
	if r := e.run(rc.ID); r.Status != RunFailed || !strings.Contains(r.Error, "no model") {
		t.Errorf("no model: %s %q", r.Status, r.Error)
	}
}

func TestLongStepLimitPerProject(t *testing.T) {
	registerTestActions()
	e := newEnv(t)
	p := e.project(e.admin, "Limits", "")
	a := e.addAuto(p, "Many", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "test_flaky", Extra: map[string]any{"key": "limit"}}}})
	var failed int
	for range longStepsPerHour + 3 {
		r := e.manual(a, nil)
		e.s.DrainAutomationRuns(e.ctx)
		if e.run(r.ID).Status == RunFailed {
			failed++
		}
	}
	if failed != 3 {
		t.Errorf("%d runs hit the hourly limit, want 3", failed)
	}
	e.setClock(e.clockNow().Add(61 * time.Minute))
	r := e.manual(a, nil)
	e.s.DrainAutomationRuns(e.ctx)
	if e.run(r.ID).Status != RunSucceeded {
		t.Error("the limit didn't reset after an hour")
	}
}

func TestAutomationFinishedTrigger(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Chain", "")
	src := e.addAuto(p, "Source", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "set_status", Status: "No Such Status"}}})
	watch := e.addAuto(p, "Watcher", Rule{Trigger: Trigger{Event: "automation_finished", Extra: map[string]any{"automation": "Source", "status": "failed"}},
		Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Trigger.Data.name}} failed: {{.Trigger.Data.error}}"}}})
	// Another automation finishing doesn't count.
	other := e.addAuto(p, "Other", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "comment", Message: "x"}}})
	e.manual(other, nil)
	if len(e.runs(watch)) != 0 {
		t.Fatal("the watcher fired for another automation")
	}
	e.manual(src, nil)
	wr := e.runs(watch)
	if len(wr) != 1 || wr[0].Status != RunSucceeded {
		t.Fatalf("watcher runs = %+v", wr)
	}
	ns, _, _ := e.s.ListNotifications(e.ctx, e.admin, false, 5)
	if len(ns) == 0 || !strings.HasPrefix(ns[0].Body, "Source failed: ") {
		t.Errorf("notifications = %+v", ns)
	}
	// An automation that watches every automation doesn't trigger itself, and
	// two that watch each other stop at the depth limit.
	a := e.addAuto(p, "A", Rule{Trigger: Trigger{Event: "automation_finished"}, Actions: []Action{{Type: "add_label", Value: "x"}}})
	b := e.addAuto(p, "B", Rule{Trigger: Trigger{Event: "automation_finished"}, Actions: []Action{{Type: "add_label", Value: "x"}}})
	done := make(chan struct{})
	go func() { e.manual(other, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("automations watching each other never stopped")
	}
	if n := len(e.runs(a)) + len(e.runs(b)); n == 0 || n > 2*maxAutomationDepth {
		t.Errorf("ping-pong runs = %d", n)
	}
}

func TestEventTriggersFromWithinWaypoint(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Events", "sprints")
	n := func(name string, trig Trigger) *Automation {
		return e.addAuto(p, name, Rule{Trigger: trig, Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Trigger.Kind}} {{.Trigger.Data.name}}"}}})
	}
	sprint := n("sprint", Trigger{Event: "sprint_created"})
	timerOn := n("timer on", Trigger{Event: "timer_started"})
	timerOff := n("timer off", Trigger{Event: "timer_stopped"})
	gloss := n("glossary", Trigger{Event: "glossary_changed", Extra: map[string]any{"change": "added"}})
	glossEdit := n("glossary edit", Trigger{Event: "glossary_changed", Extra: map[string]any{"change": "changed"}})
	page := n("page", Trigger{Event: "page_created"})
	design := n("design", Trigger{Event: "design_changed", Extra: map[string]any{"state": "approved"}})
	// Admin automations see project creation and every project's events.
	global, err := e.s.CreateAdminAutomation(e.ctx, e.admin, AutomationInput{Name: str("all"), Rule: &Rule{Trigger: Trigger{Event: "project_created"},
		Actions: []Action{{Type: "notify", User: "admins", Message: "project {{.Trigger.Data.name}}"}}}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.s.CreateSprint(e.ctx, e.admin, p.Key, SprintInput{}); err != nil {
		t.Fatal(err)
	}
	if len(e.runs(sprint)) != 1 {
		t.Errorf("sprint created: %d runs", len(e.runs(sprint)))
	}
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "timed"})
	if _, err := e.s.StartTimer(e.ctx, e.admin, i.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.StopTimer(e.ctx, e.admin); err != nil {
		t.Fatal(err)
	}
	if len(e.runs(timerOn)) != 1 || len(e.runs(timerOff)) != 1 {
		t.Errorf("timers: %d started, %d stopped", len(e.runs(timerOn)), len(e.runs(timerOff)))
	}
	g, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: str("Claim"), Definition: str("A lease."), Project: str(p.Key)})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.runs(gloss)) != 1 || len(e.runs(glossEdit)) != 0 {
		t.Errorf("glossary added: %d / edited %d", len(e.runs(gloss)), len(e.runs(glossEdit)))
	}
	if _, err := e.s.UpdateGlossary(e.ctx, e.admin, g.ID, GlossaryInput{Definition: str("A two-hour lease.")}); err != nil {
		t.Fatal(err)
	}
	if len(e.runs(gloss)) != 1 || len(e.runs(glossEdit)) != 1 {
		t.Errorf("glossary edited: %d / %d", len(e.runs(gloss)), len(e.runs(glossEdit)))
	}
	if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Notes"), Project: Opt[string]{Set: true, Value: &p.Key}}); err != nil {
		t.Fatal(err)
	}
	if len(e.runs(page)) != 1 {
		t.Errorf("page created: %d runs", len(e.runs(page)))
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	note, err := e.s.CreateDesign(e.ctx, agent, DesignInput{Title: str("Idea"), Project: str(p.Key)})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.runs(design)) != 0 {
		t.Error("a proposal counted as approved")
	}
	if _, err := e.s.ApproveDesign(e.ctx, e.admin, note.ID, ""); err != nil {
		t.Fatal(err)
	}
	if len(e.runs(design)) != 1 {
		t.Errorf("design approved: %d runs", len(e.runs(design)))
	}
	p2 := e.project(e.admin, "Second", "")
	gr, err := e.s.ListAutomationRuns(e.ctx, e.admin, global.ID, 10, 0)
	if err != nil || len(gr) != 1 { // the first project was created before the automation
		t.Fatalf("project_created runs = %d (%v)", len(gr), err)
	}
	if gr[0].ProjectID != 0 || !strings.Contains(e.run(gr[0].ID).Event.Data["name"].(string), "Second") || p2.Key == "" {
		t.Errorf("project_created run = %+v", gr[0])
	}
}
