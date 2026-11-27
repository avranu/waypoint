package core

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"git.jmann.me/claude/waypoint/internal/wql"
)

// Parameter types. The editor has a field for each; hand-made renderers
// (status pickers, the PR filter) stay as renderers for their type.
const (
	PText     = "text"
	PLong     = "long"
	PTemplate = "template"
	PNumber   = "number"
	PDuration = "duration"
	PSelect   = "select"
	PBool     = "bool"
	PUser     = "user"
	PStatus   = "status"
	PQuery    = "query"
	PRepo     = "repo"
	PGlob     = "glob"
	PGlobs    = "globs"
	PModels   = "models"
	PRunner   = "runner"
	PCron     = "cron"
	PJSON     = "json"
	PURL      = "url"
	PSecret   = "secret"
	PField    = "field"
	PTime     = "time"
	PWeekday  = "weekday"
	PList     = "list"
)

// ParamOption is one choice of a select parameter.
type ParamOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ParamWhen shows a parameter only while another has one of the values.
type ParamWhen struct {
	Param string   `json:"param"`
	In    []string `json:"in"`
}

// ParamDef is one setting of a trigger or action.
type ParamDef struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	// Needed: the editor asks for it, but rules saved without it still
	// load and run (the server never insisted on it).
	Needed      bool          `json:"needed,omitempty"`
	Default     any           `json:"default,omitempty"`
	Options     []ParamOption `json:"options,omitempty"`
	Min         *float64      `json:"min,omitempty"`
	Max         *float64      `json:"max,omitempty"`
	Help        string        `json:"help,omitempty"`
	Placeholder string        `json:"placeholder,omitempty"`
	// Secret values are hidden from run records and from people who can't
	// manage the automation.
	Secret bool `json:"secret,omitempty"`
	// Advanced settings sit behind "more options".
	Advanced bool       `json:"advanced,omitempty"`
	When     *ParamWhen `json:"when,omitempty"`
	// Runners: a models list may also name a coding-agent CLI on the rule
	// owner's runner (review and reply only; WP-6).
	Runners bool `json:"runners,omitempty"`
}

// OutputDef is a value a step leaves for later steps: {{.Steps.id.Name}}.
type OutputDef struct {
	Name string `json:"name"`
	Type string `json:"type"` // text, number, bool, list, object
	Doc  string `json:"doc,omitempty"`
}

// TriggerDef defines a trigger.
type TriggerDef struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Group string `json:"group"`
	// Phrase finishes "When …"; {param} shows that parameter in the sentence.
	Phrase string     `json:"phrase"`
	Doc    string     `json:"doc,omitempty"`
	Params []ParamDef `json:"params,omitempty"`
	// Event is the event kind this trigger listens for ("" for triggers that
	// start from a button or an incoming request).
	Event string `json:"event"`
	// Issue: the event is about an issue, so an If query and issue actions
	// work. PR: it is about a pull request.
	Issue bool `json:"issue,omitempty"`
	PR    bool `json:"pr,omitempty"`
	// Admin triggers only exist in admin automations.
	Admin bool `json:"admin,omitempty"`
	// Timed triggers are started by the scheduler, not by events.
	Timed bool `json:"timed,omitempty"`
	// Provides lists what templates can read beyond the common fields:
	// {{.Trigger.Data.session}} …
	Provides []OutputDef `json:"provides,omitempty"`
	// Match narrows an event of Event's kind to this trigger.
	Match func(tr Trigger, e *Event) bool `json:"-"`
	// Watch makes the trigger "an event that isn't followed by another in
	// time": the matching event starts a watch, a later event of kind Until
	// (about the same thing) ends it, and if none comes within the trigger's
	// After duration the automation runs.
	Watch *WatchDef `json:"watch,omitempty"`
	// Check validates what the parameters can't say alone.
	Check func(tr *Trigger) error `json:"-"`
}

// WatchDef describes a trigger that fires when an expected event is late.
type WatchDef struct {
	Until string                `json:"until"` // the event that ends the watch
	After string                `json:"after"` // the parameter holding how long to wait
	Match string                `json:"-"`     // conditions the ending event must meet (see matchConditions)
	Key   func(e *Event) string `json:"-"`     // what the ending event is about, as a wait key
}

// ActionDef defines an action: a step of an automation.
type ActionDef struct {
	Name   string     `json:"name"`
	Label  string     `json:"label"`
	Group  string     `json:"group"`
	Phrase string     `json:"phrase"` // "move it to {status}"
	Doc    string     `json:"doc,omitempty"`
	Params []ParamDef `json:"params,omitempty"`
	// Outputs are what later steps can use: {{.Steps.<id>.<Name>}}.
	Outputs []OutputDef `json:"outputs,omitempty"`
	// Long steps make a run durable: a worker carries it on, across
	// restarts, and they count against the project's hourly limit.
	Long bool `json:"long,omitempty"`
	// Runner: the step runs on a runner of the person who last saved the
	// automation (the editor says whose, and whether one is online).
	Runner bool `json:"runner,omitempty"`
	// Needs: "issue" when the action changes the trigger's issue, "pr" for
	// pull request triggers.
	Needs string `json:"needs,omitempty"`
	// Check validates what the parameters can't say alone.
	Check func(r *Rule, a *Action) error `json:"-"`
	// Run does the work. A long step that must wait calls c.Wait or c.Sleep
	// and returns its error: the run parks and calls Run again when the wait
	// ends, so Run must be safe to repeat.
	Run func(c *StepCtx) error `json:"-"`
	// Describe says what a dry run would do; the default names the action
	// and its settings.
	Describe func(c *StepCtx) string `json:"-"`
	// Doc line for ActionTypes (agents' reference).
	Summary string `json:"-"`
}

var (
	catalogMu    sync.RWMutex
	triggerDefs  = map[string]*TriggerDef{}
	actionDefs   = map[string]*ActionDef{}
	triggerOrder []string
	actionOrder  []string
)

func registerTrigger(d TriggerDef) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if _, dup := triggerDefs[d.Name]; dup {
		panic("duplicate trigger " + d.Name)
	}
	triggerDefs[d.Name] = &d
	triggerOrder = append(triggerOrder, d.Name)
	TriggerEvents[d.Name] = d.Event
}

func registerAction(d ActionDef) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if _, dup := actionDefs[d.Name]; dup {
		panic("duplicate action " + d.Name)
	}
	if d.Run == nil {
		panic("action " + d.Name + " has no Run")
	}
	actionDefs[d.Name] = &d
	actionOrder = append(actionOrder, d.Name)
	ActionTypes[d.Name] = cmp(d.Summary, d.Doc)
}

func triggerDef(name string) *TriggerDef {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	return triggerDefs[name]
}

func actionDef(name string) *ActionDef {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	return actionDefs[name]
}

// Preset is a ready-made "only if" for the editor; Template may contain
// {step}, which the editor fills with an earlier step's id.
type Preset struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Template string `json:"template"`
	Step     bool   `json:"step,omitempty"` // asks which earlier step
}

// onlyIfPresets are offered next to the template field.
var onlyIfPresets = []Preset{
	{ID: "previous_failed", Label: "The previous step failed", Template: "{{.Previous.Failed}}"},
	{ID: "previous_ok", Label: "The previous step succeeded", Template: "{{.Previous.OK}}"},
	{ID: "step_failed", Label: "A step failed", Template: "{{.Steps.{step}.Failed}}", Step: true},
	{ID: "step_ok", Label: "A step succeeded", Template: "{{.Steps.{step}.OK}}", Step: true},
	{ID: "by_person", Label: "A person started it", Template: `{{eq .Trigger.By "person"}}`},
	{ID: "by_agent", Label: "An agent started it", Template: `{{eq .Trigger.By "agent"}}`},
}

// RegisterPreset adds an "only if" preset; for actions that bring their own
// (findings at a severity, CI still failing).
func RegisterPreset(p Preset) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	onlyIfPresets = append(onlyIfPresets, p)
}

// CatalogGroup orders the editor's groups.
type CatalogGroup struct {
	Name string `json:"name"`
}

// Catalog is what the editor and agents read.
type Catalog struct {
	Triggers []TriggerDef `json:"triggers"`
	Actions  []ActionDef  `json:"actions"`
	// TriggerGroups and ActionGroups are in display order.
	TriggerGroups []string `json:"trigger_groups"`
	ActionGroups  []string `json:"action_groups"`
	Presets       []Preset `json:"presets"`
	// Funcs documents the template functions.
	Funcs []OutputDef `json:"funcs"`
}

var (
	triggerGroupOrder = []string{"Issues", "Counts", "Pull requests", "Code", "Deployments", "Agents", "Sprints", "Knowledge", "Projects", "Timers", "Automations", "Time", "Manual and incoming", "System"}
	actionGroupOrder  = []string{"Issue", "People", "Notify", "Integrations", "Pull request", "Agents", "Time", "AI", "Flow"}
)

// BuildCatalog lists the definitions, in the order they were registered
// within each group. Admin triggers are left out unless admin is set.
func BuildCatalog(admin bool) Catalog {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	c := Catalog{Presets: slices.Clone(onlyIfPresets), TriggerGroups: slices.Clone(triggerGroupOrder), ActionGroups: slices.Clone(actionGroupOrder)}
	for _, n := range triggerOrder {
		if d := triggerDefs[n]; !d.Admin || admin {
			c.Triggers = append(c.Triggers, *d)
		}
	}
	for _, n := range actionOrder {
		c.Actions = append(c.Actions, *actionDefs[n])
	}
	c.Funcs = append(c.Funcs, templateFuncDocs...)
	return c
}

// AutomationCatalog is BuildCatalog for this person, leaving out the
// actions the server isn't set up for (email, until Admin → Email is set).
func (s *Service) AutomationCatalog(ctx context.Context, actor *Actor) Catalog {
	c := BuildCatalog(actor.isAdmin())
	if !s.EmailConfigured(ctx) {
		c.Actions = slices.DeleteFunc(c.Actions, func(d ActionDef) bool { return d.Name == "email" })
	}
	return c
}

// flatKeys caches the JSON field names of a struct that also carries
// free-form parameters.
var flatKeys sync.Map

func knownKeys(t reflect.Type) map[string]bool {
	if m, ok := flatKeys.Load(t); ok {
		return m.(map[string]bool)
	}
	m := map[string]bool{}
	for i := range t.NumField() {
		tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			m[tag] = true
		}
	}
	flatKeys.Store(t, m)
	return m
}

// marshalFlat writes v's fields and extra's entries as one JSON object.
func marshalFlat(v any, extra map[string]any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return b, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, x := range extra {
		if _, ok := m[k]; ok {
			continue
		}
		xb, err := json.Marshal(x)
		if err != nil {
			return nil, err
		}
		m[k] = xb
	}
	return json.Marshal(m)
}

// unmarshalFlat reads v's fields and puts every other key in extra.
func unmarshalFlat(b []byte, v any, extra *map[string]any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	known := knownKeys(reflect.TypeOf(v).Elem())
	*extra = nil
	for k, raw := range m {
		if known[k] {
			continue
		}
		var x any
		if err := json.Unmarshal(raw, &x); err != nil {
			return err
		}
		if *extra == nil {
			*extra = map[string]any{}
		}
		(*extra)[k] = x
	}
	return nil
}

// paramMap is an object's settings by name, whichever struct field or free
// entry holds them.
func paramMap(v any, extra map[string]any) map[string]any {
	b, err := marshalFlat(v, extra)
	if err != nil {
		return map[string]any{}
	}
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func anyString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

func anyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func anyBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return truthy(x)
	case float64:
		return x != 0
	}
	return false
}

func anyStrings(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s := strings.TrimSpace(anyString(e)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		var out []string
		for _, l := range strings.FieldsFunc(x, func(r rune) bool { return r == '\n' || r == ',' }) {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	return nil
}

// truthy reads a rendered "only if": empty, false, 0, no and "<no value>"
// are false.
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "false", "0", "no", "off", "<no value>", "<nil>", "nil":
		return false
	}
	return true
}

// ParseWaitDuration reads 90s, 15m, 2h, 3d, 1w, or a bare number of seconds.
func ParseWaitDuration(v string) (time.Duration, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	var total time.Duration
	rest := v
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 || i == len(rest) {
			return 0, fmt.Errorf("%q is not a duration like 15m or 2h", v)
		}
		n, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration like 15m or 2h", v)
		}
		j := i
		for j < len(rest) && (rest[j] >= 'a' && rest[j] <= 'z') {
			j++
		}
		var unit time.Duration
		switch rest[i:j] {
		case "s", "sec", "secs", "second", "seconds":
			unit = time.Second
		case "m", "min", "mins", "minute", "minutes":
			unit = time.Minute
		case "h", "hr", "hrs", "hour", "hours":
			unit = time.Hour
		case "d", "day", "days":
			unit = 24 * time.Hour
		case "w", "week", "weeks":
			unit = 7 * 24 * time.Hour
		default:
			return 0, fmt.Errorf("%q is not a duration like 15m or 2h", v)
		}
		total += time.Duration(n * float64(unit))
		rest = rest[j:]
	}
	return total, nil
}

// validateParams checks values against definitions. kind and name only
// shape the messages ("set_status needs a status").
func validateParams(name string, defs []ParamDef, vals map[string]any) error {
	shown := func(d ParamDef) bool {
		if d.When == nil {
			return true
		}
		return slices.Contains(d.When.In, anyString(vals[d.When.Param]))
	}
	for _, d := range defs {
		if !shown(d) {
			continue
		}
		v, has := vals[d.Name]
		empty := !has || v == nil || (reflect.TypeOf(v).Kind() == reflect.String && strings.TrimSpace(v.(string)) == "") ||
			(reflect.TypeOf(v).Kind() == reflect.Slice && reflect.ValueOf(v).Len() == 0)
		if empty {
			if d.Required {
				return invalid("%s needs %s", name, article(strings.ToLower(d.Label)))
			}
			continue
		}
		if err := checkParam(name, d, v); err != nil {
			return err
		}
	}
	return nil
}

func article(w string) string {
	if w != "" && strings.ContainsRune("aeiou", rune(w[0])) {
		return "an " + w
	}
	return "a " + w
}

func checkParam(name string, d ParamDef, v any) error {
	bad := func(format string, args ...any) error {
		return invalid("%s: %s %s", name, strings.ToLower(d.Label), fmt.Sprintf(format, args...))
	}
	s := anyString(v)
	switch d.Type {
	case PText, PLong, PRepo, PRunner, PUser, PStatus, PField:
		if _, isStr := v.(string); !isStr && d.Type != PText {
			return bad("must be text")
		}
		if len(s) > 100_000 {
			return bad("is too long")
		}
	case PTemplate, PList:
		for _, tpl := range append([]string{s}, anyStrings2(v)...) {
			if tpl == "" {
				continue
			}
			if _, err := newTemplate(tpl); err != nil {
				return invalid("%s: template: %v", name, err)
			}
		}
	case PNumber:
		f, ok := anyFloat(v)
		if !ok {
			return bad("must be a number")
		}
		if d.Min != nil && f < *d.Min {
			return bad("must be at least %v", *d.Min)
		}
		if d.Max != nil && f > *d.Max {
			return bad("must be at most %v", *d.Max)
		}
	case PDuration:
		dur, err := ParseWaitDuration(s)
		if err != nil {
			return bad("must be a duration like 15m or 2h")
		}
		if dur < 0 {
			return bad("can't be negative")
		}
		if d.Min != nil && dur.Seconds() < *d.Min {
			return bad("must be at least %s", (time.Duration(*d.Min) * time.Second).String())
		}
		if d.Max != nil && dur.Seconds() > *d.Max {
			return bad("must be at most %s", (time.Duration(*d.Max) * time.Second).String())
		}
	case PSelect:
		if !slices.ContainsFunc(d.Options, func(o ParamOption) bool { return o.Value == s }) {
			vals := make([]string, len(d.Options))
			for i, o := range d.Options {
				vals[i] = o.Value
			}
			return bad("must be one of %s", strings.Join(vals, ", "))
		}
	case PBool:
		if _, ok := v.(bool); !ok {
			return bad("must be true or false")
		}
	case PQuery:
		if _, err := wql.Parse(s); err != nil {
			return invalid("%s: %s: %v", name, strings.ToLower(d.Label), err)
		}
	case PGlob:
		if _, err := newGlobSet([]string{s}); err != nil {
			return bad("isn't a valid pattern")
		}
	case PGlobs:
		if _, err := newGlobSet(anyStrings(v)); err != nil {
			return bad("has an invalid pattern")
		}
	case PURL:
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
			return invalid("%s needs an http(s) url", name)
		}
	case PCron:
		if _, err := ParseCron(s); err != nil {
			return invalid("%s: %v", name, err)
		}
	case PJSON:
		if _, err := newTemplate(s); err != nil {
			return invalid("%s: template: %v", name, err)
		}
	case PTime:
		if !atRE.MatchString(s) {
			return invalid("at must be a time like 09:00")
		}
	case PWeekday:
		f, ok := anyFloat(v)
		if !ok || f < 0 || f > 6 {
			return invalid("weekday must be 0 (Sunday) to 6")
		}
	case PModels:
		if ms, ok := v.([]any); ok && len(ms) > 10 {
			return invalid("%s can try at most 10 models", name)
		}
		if err := checkRunnerEntries(name, d, v); err != nil {
			return err
		}
	case PAgents:
		if _, err := agentChain(v); err != nil {
			return invalid("%s: %v", name, err)
		}
	}
	return nil
}

func anyStrings2(v any) []string {
	if l, ok := v.([]any); ok {
		return anyStrings(l)
	}
	if l, ok := v.([]string); ok {
		return l
	}
	return nil
}

// newTemplate parses a template with the automation functions.
func newTemplate(tpl string) (*template.Template, error) {
	return template.New("").Option("missingkey=zero").Funcs(templateFuncs).Parse(tpl)
}

// validateTrigger checks a trigger against its definition.
func (tr *Trigger) validateDef(admin bool) error {
	d := triggerDef(tr.Event)
	if d == nil {
		return invalid("unknown trigger %q", tr.Event)
	}
	if d.Admin && !admin {
		return invalid("%s only works in Admin → Automations", d.Label)
	}
	if err := validateParams(tr.Event, d.Params, paramMap(tr, nil)); err != nil {
		return err
	}
	if d.Check != nil {
		return d.Check(tr)
	}
	return nil
}

// validateDef checks a step (and the steps inside a repeat) against its
// definition.
func (a *Action) validateDef(r *Rule) error {
	d := actionDef(a.Type)
	if d == nil {
		return invalid("unknown action %q", a.Type)
	}
	if err := validateParams(a.Type, d.Params, paramMap(a, nil)); err != nil {
		return err
	}
	if d.Needs == "pr" {
		if td := triggerDef(r.Trigger.Event); td == nil || !td.PR {
			return invalid("%s needs a pull request trigger", a.Type)
		}
	}
	if d.Check != nil {
		if err := d.Check(r, a); err != nil {
			return err
		}
	}
	return nil
}

// checkRunnerEntries: a models list may name a coding-agent CLI only where
// the parameter says so, and it must be one a runner knows.
func checkRunnerEntries(name string, d ParamDef, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var chain []ChainModel
	if json.Unmarshal(b, &chain) != nil {
		return nil
	}
	for _, cm := range chain {
		switch {
		case cm.CLI == "":
		case !d.Runners:
			return invalid("%s: %s can't use a coding agent on a runner; only review and reply rules can", name, strings.ToLower(d.Label))
		case !slices.Contains(runnerCLIs, cm.CLI):
			return invalid("%s: the coding agent must be one of %s", name, strings.Join(reviewCLIs, ", "))
		case !slices.Contains(reviewCLIs, cm.CLI):
			return invalid("%s: %s", name, reviewCLIError(cm.CLI))
		case cm.Model != "" && !modelRE.MatchString(cm.Model):
			return invalid("%s: model %q isn't allowed", name, cm.Model)
		}
	}
	return nil
}
