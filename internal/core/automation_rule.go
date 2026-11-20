package core

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/wql"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Rule is an automation's logic: when Trigger happens and the issue matches
// If, run the steps in Actions (WP-D18). The JSON key stays "actions", so
// rules saved before steps existed load and run unchanged.
type Rule struct {
	Trigger Trigger `json:"trigger"`
	// If is a query the issue must match, e.g. "type:bug priority:>=high".
	If string `json:"if,omitempty"`
	// PR narrows pull request triggers (WP-D14).
	PR      *PRFilter `json:"pr,omitempty"`
	Actions []Action  `json:"actions"`
	// Inputs are asked for when someone runs the automation by hand.
	Inputs []RuleInput `json:"inputs,omitempty"`
	// Concurrency: parallel (the default), skip (do nothing while a run is
	// in progress; the default for schedules) or queue.
	Concurrency string `json:"concurrency,omitempty"`
	// TimeZone (an IANA name) is where schedules run; else the project
	// owner's.
	TimeZone string `json:"time_zone,omitempty"`
	// AllowAgents lets agents run the automation by hand (off by default).
	AllowAgents bool `json:"allow_agents,omitempty"`
}

// RuleInput is a value a manual run asks for: {{.Inputs.name}}.
type RuleInput struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Type     string   `json:"type"` // text, number, bool, select
	Default  string   `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required,omitempty"`
}

// Trigger says which event starts an automation. The fields below are the
// settings the first triggers had; every newer trigger keeps its settings in
// the same JSON object under its own names (see Extra).
type Trigger struct {
	// Event is one of TriggerEvents.
	Event string `json:"event"`
	// From/To filter status changes by status name or category.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Field filters field_changed (e.g. "priority", "due_date", "sprint").
	Field string `json:"field,omitempty"`
	// Days: for due_soon, fire this many days (0: on the day) before the due
	// date; for stale, after this many days without changes.
	Days int `json:"days,omitempty"`
	// Schedules: Every is day, weekday, week, month or cron, at At ("09:00")
	// in the automation's time zone; Weekday (0 Sunday) for week, MonthDay
	// (1-28) for month; the cron expression is Extra["cron"].
	Every    string `json:"every,omitempty"`
	At       string `json:"at,omitempty"`
	Weekday  int    `json:"weekday,omitempty"`
	MonthDay int    `json:"month_day,omitempty"`
	// Command filters pr_command: review (which "full review" also is);
	// empty is any.
	Command string `json:"command,omitempty"`
	// Extra holds the settings of newer triggers (by, branch, paths, query,
	// op, n, secret, …) by the names their definitions give them.
	Extra map[string]any `json:"-"`
}

// MarshalJSON writes Extra beside the fields.
func (t Trigger) MarshalJSON() ([]byte, error) {
	type plain Trigger
	return marshalFlat(plain(t), t.Extra)
}

// UnmarshalJSON collects unknown keys in Extra.
func (t *Trigger) UnmarshalJSON(b []byte) error {
	type plain Trigger
	var p plain
	if err := unmarshalFlat(b, &p, &p.Extra); err != nil {
		return err
	}
	*t = Trigger(p)
	return nil
}

// str reads a setting by name.
func (t Trigger) str(name string) string {
	switch name {
	case "event":
		return t.Event
	case "from":
		return t.From
	case "to":
		return t.To
	case "field":
		return t.Field
	case "every":
		return t.Every
	case "at":
		return t.At
	case "command":
		return t.Command
	}
	return anyString(t.Extra[name])
}

func (t Trigger) num(name string) float64 {
	switch name {
	case "days":
		return float64(t.Days)
	case "weekday":
		return float64(t.Weekday)
	case "month_day":
		return float64(t.MonthDay)
	}
	f, _ := anyFloat(t.Extra[name])
	return f
}

// TriggerEvents maps trigger names to event kinds; registerTrigger fills it.
var TriggerEvents = map[string]string{}

// Action is one step. Which settings matter depends on Type (see
// ActionTypes and the catalog); the fields before Steps are every step's.
type Action struct {
	Type string `json:"type"`
	// ID names the step so later steps can use its outputs:
	// {{.Steps.<id>.Text}}.
	ID string `json:"id,omitempty"`
	// OnlyIf is a template; the step runs only when it renders true.
	OnlyIf string `json:"only_if,omitempty"`
	// OnError: stop (the default), continue, or retry Retries times, waiting
	// RetryDelay between tries.
	OnError    string `json:"on_error,omitempty"`
	Retries    int    `json:"retries,omitempty"`
	RetryDelay string `json:"retry_delay,omitempty"`
	// Steps are the steps a repeat group runs again and again.
	Steps []Action `json:"steps,omitempty"`

	Status   string   `json:"status,omitempty"`
	Field    string   `json:"field,omitempty"`
	Value    string   `json:"value,omitempty"`
	User     string   `json:"user,omitempty"`
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message,omitempty"`
	URL      string   `json:"url,omitempty"`
	Token    string   `json:"token,omitempty"`
	Priority *int     `json:"priority,omitempty"` // Gotify, 0-10; default 5
	Titles   []string `json:"titles,omitempty"`
	Body     string   `json:"body,omitempty"`
	// create_issue
	Template  string   `json:"template,omitempty"`
	IssueType string   `json:"issue_type,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Due       string   `json:"due,omitempty"`
	// review: the models to try in order (empty: Admin → AI's review
	// models), extra instructions, review even a head already reviewed,
	// and the most findings to post.
	Models      []ChainModel `json:"models,omitempty"`
	Focus       string       `json:"focus,omitempty"`
	Force       bool         `json:"force,omitempty"`
	MaxFindings int          `json:"max_findings,omitempty"`
	// Extra holds the settings of newer actions (prompt, duration, kind, …).
	Extra map[string]any `json:"-"`
}

// MarshalJSON writes Extra beside the fields.
func (a Action) MarshalJSON() ([]byte, error) {
	type plain Action
	return marshalFlat(plain(a), a.Extra)
}

// UnmarshalJSON collects unknown keys in Extra.
func (a *Action) UnmarshalJSON(b []byte) error {
	type plain Action
	var p plain
	if err := unmarshalFlat(b, &p, &p.Extra); err != nil {
		return err
	}
	*a = Action(p)
	return nil
}

// ActionTypes documents what each action does, for agents; registerAction
// fills it.
var ActionTypes = map[string]string{}

var (
	atRE     = lazyre.New(`^([01]\d|2[0-3]):[0-5]\d$`)
	stepIDRE = lazyre.New(`^[a-z][a-z0-9_]{0,31}$`)
	inputRE  = lazyre.New(`^[a-z][a-z0-9_]{0,31}$`)
)

const (
	maxSteps       = 20 // per list
	maxStepsTotal  = 40
	maxRepeat      = 20
	maxRetries     = 5
	maxRetryDelay  = 24 * time.Hour
	maxInputs      = 10
	concurrencyDef = "parallel"
)

// stepID is the step's name: its own, or s1, s2 … by position (2.1, 2.2 …
// inside the repeat that is step 2; give a step an id to use its outputs).
func stepID(a Action, n int, parent string) string {
	if a.ID != "" {
		return a.ID
	}
	if parent != "" {
		return fmt.Sprintf("%s.%d", parent, n+1)
	}
	return fmt.Sprintf("s%d", n+1)
}

// validate checks a project automation's rule.
func (r *Rule) validate() error { return r.validateFor(false) }

// validateFor checks a rule; admin automations may use system triggers.
func (r *Rule) validateFor(admin bool) error {
	if err := r.Trigger.validateDef(admin); err != nil {
		return err
	}
	if r.If != "" {
		if _, err := wql.Parse(r.If); err != nil {
			return invalid("condition: %v", err)
		}
	}
	if err := r.PR.validate(); err != nil {
		return err
	}
	if len(r.Actions) == 0 {
		return invalid("an automation needs at least one action")
	}
	if len(r.Actions) > maxSteps {
		return invalid("an automation can have at most %d actions", maxSteps)
	}
	switch r.Concurrency {
	case "", "parallel", "skip", "queue":
	default:
		return invalid("concurrency must be parallel, skip or queue")
	}
	if r.TimeZone != "" {
		if _, err := time.LoadLocation(r.TimeZone); err != nil {
			return invalid("unknown time zone %q", r.TimeZone)
		}
	}
	if err := r.validateInputs(); err != nil {
		return err
	}
	ids := map[string]bool{}
	total := 0
	if err := r.validateSteps(r.Actions, "", ids, &total); err != nil {
		return err
	}
	if total > maxStepsTotal {
		return invalid("an automation can have at most %d steps in all", maxStepsTotal)
	}
	return nil
}

func (r *Rule) validateInputs() error {
	if len(r.Inputs) > maxInputs {
		return invalid("an automation can ask for at most %d inputs", maxInputs)
	}
	seen := map[string]bool{}
	for _, in := range r.Inputs {
		if !inputRE.MatchString(in.Name) {
			return invalid("input names are lowercase letters, digits and _, like focus_area")
		}
		if seen[in.Name] {
			return invalid("two inputs are called %q", in.Name)
		}
		seen[in.Name] = true
		switch in.Type {
		case "text", "number", "bool":
		case "select":
			if len(in.Options) == 0 {
				return invalid("input %q needs options to choose from", in.Name)
			}
			if in.Default != "" && !slices.Contains(in.Options, in.Default) {
				return invalid("input %q: the default isn't one of the options", in.Name)
			}
		default:
			return invalid("input %q: type must be text, number, bool or select", in.Name)
		}
		if in.Type == "number" && in.Default != "" {
			if _, ok := anyFloat(in.Default); !ok {
				return invalid("input %q: the default isn't a number", in.Name)
			}
		}
	}
	return nil
}

func (r *Rule) validateSteps(steps []Action, parent string, ids map[string]bool, total *int) error {
	for n, a := range steps {
		*total++
		if a.ID != "" && !stepIDRE.MatchString(a.ID) {
			return invalid("step ids are lowercase letters, digits and _, like review_pr")
		}
		id := stepID(a, n, parent)
		if ids[id] {
			return invalid("two steps have the id %q", id)
		}
		ids[id] = true
		if len(a.Titles) > 20 {
			return invalid("create_subtasks can create at most 20 issues")
		}
		if err := a.validateDef(r); err != nil {
			return err
		}
		if a.OnlyIf != "" {
			if _, err := newTemplate(a.OnlyIf); err != nil {
				return invalid("only if: %v", err)
			}
		}
		switch a.OnError {
		case "", "stop", "continue":
		case "retry":
			if a.Retries < 1 || a.Retries > maxRetries {
				return invalid("retry between 1 and %d times", maxRetries)
			}
			if a.RetryDelay != "" {
				d, err := ParseWaitDuration(a.RetryDelay)
				if err != nil || d < 0 || d > maxRetryDelay {
					return invalid("the retry delay must be a duration up to 24h")
				}
			}
		default:
			return invalid("on_error must be stop, continue or retry")
		}
		if a.Type == "repeat" {
			if parent != "" {
				return invalid("a repeat can't hold another repeat")
			}
			if len(a.Steps) == 0 {
				return invalid("a repeat needs steps to repeat")
			}
			if len(a.Steps) > maxSteps {
				return invalid("a repeat can hold at most %d steps", maxSteps)
			}
			if err := r.validateSteps(a.Steps, id, ids, total); err != nil {
				return err
			}
		} else if len(a.Steps) > 0 {
			return invalid("only a repeat holds steps")
		}
	}
	return nil
}

// flatSteps lists every step, repeats' children included.
func flatSteps(steps []Action) []Action {
	var out []Action
	for _, a := range steps {
		out = append(out, a)
		out = append(out, flatSteps(a.Steps)...)
	}
	return out
}

// isDurable: the rule has steps that take long, wait, repeat after a delay
// or retry, so a worker carries its runs rather than the request that
// triggered them.
func (r *Rule) isDurable() bool {
	steps := flatSteps(r.Actions)
	for n, a := range steps {
		if a.OnError == "retry" {
			return true
		}
		// A review that later steps follow waits for its bot run, so they
		// can use what it found (WP-D14).
		if a.Type == "review" && n < len(steps)-1 {
			return true
		}
		if d := actionDef(a.Type); d != nil && d.Long {
			return true
		}
	}
	return false
}

// concurrency is the policy a run uses: the rule's, else skip for schedules
// and parallel for everything else.
func (r *Rule) concurrency() string {
	if r.Concurrency != "" {
		return r.Concurrency
	}
	if r.Trigger.Event == "schedule" {
		return "skip"
	}
	return concurrencyDef
}

// inputValues checks the values given to a manual run against the rule's
// inputs, filling in defaults.
func (r *Rule) inputValues(given map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for k := range given {
		if !slices.ContainsFunc(r.Inputs, func(in RuleInput) bool { return in.Name == k }) {
			return nil, invalid("this automation has no input %q", k)
		}
	}
	for _, in := range r.Inputs {
		v, has := given[in.Name]
		if has && v != nil && anyString(v) == "" {
			has = false
		}
		if !has {
			if in.Required && in.Default == "" {
				return nil, invalid("%s is required", cmp(in.Label, in.Name))
			}
			v = in.Default
			if v == "" && in.Type != "bool" {
				out[in.Name] = ""
				if in.Type == "number" {
					out[in.Name] = nil
				}
				continue
			}
		}
		switch in.Type {
		case "number":
			f, ok := anyFloat(v)
			if !ok {
				return nil, invalid("%s must be a number", cmp(in.Label, in.Name))
			}
			out[in.Name] = f
		case "bool":
			out[in.Name] = anyBool(v)
		case "select":
			s := anyString(v)
			if !slices.Contains(in.Options, s) {
				return nil, invalid("%s must be one of %s", cmp(in.Label, in.Name), strings.Join(in.Options, ", "))
			}
			out[in.Name] = s
		default:
			s := anyString(v)
			if len(s) > 20_000 {
				return nil, invalid("%s is too long", cmp(in.Label, in.Name))
			}
			out[in.Name] = s
		}
	}
	return out, nil
}

// ruleJSON is the stored form of a rule.
func ruleJSON(r Rule) string {
	b, _ := json.Marshal(r)
	return string(b)
}
