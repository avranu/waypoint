package core

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"text/template"
	"time"
)

// What templates in an automation can read:
//
//	{{.Issue}} {{.Project}} {{.Actor}} {{.Event}} {{.PR}} {{.URL}} {{.Date}}
//	{{.Comments}}              the issue's comments, oldest first (WP-449)
//	{{.Trigger.Data.session}}  the event's data (a webhook's JSON body)
//	{{.Trigger.By}}            person, agent, automation or system
//	{{.Steps.review.Text}}     an earlier step's outputs, by step id
//	{{.Steps.review.Failed}}   … and OK / Failed / Skipped / Error
//	{{.Previous}}              the step before this one, the same way
//	{{.Inputs.focus}}          what a manual run was given
//	{{.Run.ID}}                this run
type tplData struct {
	Issue   *Issue
	Project *Project
	Actor   UserRef
	Event   *Event
	URL     string
	Date    string // the day a scheduled rule fired (YYYY-MM-DD)
	PR      *PullRequest

	Trigger  *tplTrigger
	Steps    map[string]map[string]any
	Previous map[string]any
	Inputs   map[string]any
	Run      tplRun

	comments func() (string, error)
}

// Comments are the trigger issue's comments, oldest first, one per line as
// wp next hands them to an agent; "" without an issue or comments. Text
// people wrote: wrap it in untrusted before it reaches a model.
func (d tplData) Comments() (string, error) {
	if d.comments == nil {
		return "", nil
	}
	return d.comments()
}

// lazyComments reads an issue's comments once, as actor sees them.
func (s *Service) lazyComments(ctx context.Context, actor *Actor, i *Issue) func() (string, error) {
	var (
		once sync.Once
		text string
		err  error
	)
	return func() (string, error) {
		once.Do(func() {
			var cs []Comment
			if cs, err = s.ListComments(ctx, actor, i.Key); err == nil {
				text = TaskCommentLines(i.Key, cs)
			}
		})
		return text, err
	}
}

type tplRun struct {
	ID         int64
	Automation string
}

// tplTrigger is the event that started the run, as templates see it.
type tplTrigger struct {
	Name    string // the trigger: created, schedule, manual …
	Kind    string // the event kind
	Summary string
	By      string // person, agent, automation or system
	Via     string // event, schedule, manual, api, cli, webhook, rerun
	Actor   UserRef
	Old     string
	New     string
	Field   string
	Data    map[string]any
	At      time.Time
	PR      *PullRequest
}

// templateFuncs are available in every automation template.
var templateFuncs = template.FuncMap{
	"json": func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	},
	"untrusted": func(v any) string { return untrustedText(anyString(v)) },
	"truncate": func(n int, v any) string {
		r := []rune(anyString(v))
		if n < 0 || len(r) <= n {
			return string(r)
		}
		return string(r[:n]) + "…"
	},
	"lower": func(v any) string { return strings.ToLower(anyString(v)) },
	"upper": func(v any) string { return strings.ToUpper(anyString(v)) },
	"trim":  func(v any) string { return strings.TrimSpace(anyString(v)) },
	"join":  func(sep string, v any) string { return strings.Join(anyStrings(v), sep) },
	"default": func(def, v any) any {
		if anyString(v) == "" || anyString(v) == "<no value>" {
			return def
		}
		return v
	},
	"contains":  func(sub string, v any) bool { return strings.Contains(anyString(v), sub) },
	"hasPrefix": func(prefix string, v any) bool { return strings.HasPrefix(anyString(v), prefix) },
}

var templateFuncDocs = []OutputDef{
	{Name: "json", Type: "text", Doc: "{{json .Steps.x.Data}}: the value as JSON"},
	{Name: "untrusted", Type: "text", Doc: "{{untrusted .Trigger.Data.body}}: marks outside text as data for a model"},
	{Name: "truncate", Type: "text", Doc: "{{truncate 200 .Issue.Description}}"},
	{Name: "lower", Type: "text", Doc: "{{lower .Issue.Title}}"},
	{Name: "upper", Type: "text", Doc: "{{upper .Issue.Title}}"},
	{Name: "trim", Type: "text", Doc: "{{trim .Steps.x.Text}}"},
	{Name: "join", Type: "text", Doc: `{{join ", " .Steps.x.Items}}`},
	{Name: "default", Type: "text", Doc: `{{default "none" .Inputs.focus}}`},
	{Name: "contains", Type: "bool", Doc: `{{contains "urgent" .Issue.Title}}`},
	{Name: "hasPrefix", Type: "bool", Doc: `{{hasPrefix "fix" .Issue.Title}}`},
}

// untrustedText marks text as data for a model to read, never instructions.
func untrustedText(s string) string {
	return "<untrusted>\n" + untrusted(s) + "\n</untrusted>"
}

// noValue is what text/template prints for a missing map entry.
const noValue = "<no value>"

func renderTemplate(tpl string, d tplData) (string, error) {
	t, err := newTemplate(tpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return strings.ReplaceAll(b.String(), noValue, ""), nil
}

// renderOnlyIf reads a step's "only if".
func renderOnlyIf(tpl string, d tplData) (bool, error) {
	out, err := renderTemplate(tpl, d)
	if err != nil {
		return false, err
	}
	return truthy(out), nil
}
