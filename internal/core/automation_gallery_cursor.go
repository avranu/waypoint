package core

import (
	"fmt"
	"strings"
)

// Gallery entries for adversarial audits by another model (WP-305, WP-ADR18;
// since WP-348 through the audit step, WP-ADR20). The model reads the whole
// repository looking for defects; Waypoint keeps what it reports as an audit,
// and the daily card hands it to Claude, who checks each claim against the
// code before fixing anything.
//
// The cards name no model, so they get the audit default (WP-871): with
// The gateway set up its audit model, which is where Grok is reached and which
// has its own failover; on a Waypoint with no gateway, Grok 4.7 through
// cursor-agent on the person's runner. The daily card spreads the spend over
// the working days and stops quietly on a day the model is out of usage.
func init() {
	AutomationTemplates = append(AutomationTemplates, cursorTemplates...)
}

// auditLenses are the daily audit's focus, one per working day, so a week
// covers the codebase five ways.
var auditLenses = []struct{ day, name, what string }{
	{"Monday", "security", "security: missing or wrong access checks, injection (SQL, shell, path, template), secrets in code or logs, untrusted input used unchecked, unsafe defaults"},
	{"Tuesday", "correctness", "correctness: logic errors, wrong edge cases (empty, zero, one, many, unicode, time zones), races and shared state, wrong assumptions about inputs"},
	{"Wednesday", "failure handling", "failure handling: swallowed or misreported errors, partial writes, retries that repeat side effects, leaks of files, connections or goroutines, data that can be lost"},
	{"Thursday", "tests", "tests: important behaviour no test covers, tests that cannot fail or test the mock, flaky timing, assertions that check the wrong thing"},
	{"Friday", "dead weight", "dead weight and drift: dead code, duplicated logic that has diverged, comments and docs that contradict the code, names that mislead"},
}

// lensTemplate picks the day's lens from when the schedule fired; field is
// "name" or "what".
func lensTemplate(field string) string {
	var b strings.Builder
	for i, l := range auditLenses {
		v := l.name
		if field == "what" {
			v = l.what
		}
		if i > 0 {
			b.WriteString("{{else ")
		} else {
			b.WriteString("{{")
		}
		fmt.Fprintf(&b, `if eq (print .Trigger.At.Weekday) %q}}%s`, l.day, v)
	}
	b.WriteString("{{else}}")
	if field == "what" {
		b.WriteString("whatever is most likely to hurt the people who use or run this code")
	} else {
		b.WriteString("general")
	}
	b.WriteString("{{end}}")
	return b.String()
}

// auditSteps: one audit step (WP-348), which keeps the findings in Waypoint
// and, with fix, hands them to Claude as a low-priority task. The audit
// tells the saver itself when it fails; a model out of usage ends it
// quietly, since usage comes back by itself.
func auditSteps(target, ref, focus string, fix bool) []Action {
	return []Action{{Type: "audit", ID: "audit", OnError: "continue", Extra: map[string]any{"target": target, "ref": ref, "instructions": focus, "fix": fix}}}
}

// auditTemplateModel says which model a card's audit gets.
const auditTemplateModel = "The model is Grok 4.7 through cursor-agent on your runner, on your Cursor plan. " +
	"When Cursor can't run it (no runner with Cursor online, the plan out of usage, a failure), the model the LLM gateway picks for audits runs it instead, from a checkout your runner sends; " +
	"a Waypoint with no LLM gateway has Cursor alone."

var cursorTemplates = []AutomationTemplate{
	{
		Name: "Daily Grok audit, checked by Claude",
		Description: "Every working day at 12:30, another model audits the whole repository, one focus a day: security, correctness, failure handling, tests, dead weight. " +
			"Its findings become a low-priority task for Claude, who checks each against the code, fixes the real ones in one pull request and says why the rest are wrong. " +
			auditTemplateModel + " Stops for the day when the model is out of usage.",
		Rule: Rule{Trigger: Trigger{Event: "schedule", Every: "weekday", At: "12:30"}, Concurrency: "skip",
			Actions: auditSteps("repo", "", "Focus: "+lensTemplate("what")+".", true)},
	},
	{
		Name: "Weekly Grok audit of what landed",
		Description: "Every Monday at 09:00, another model audits everything that landed on the default branch in the last week, and keeps its findings under Audits. " +
			"A quiet week costs nothing: with no change to read, no model runs. Ask there for a task for Claude to check and fix the findings. " +
			auditTemplateModel,
		Rule: Rule{Trigger: Trigger{Event: "schedule", Every: "week", At: "09:00", Weekday: 1}, Concurrency: "skip",
			Actions: []Action{{Type: "audit", ID: "audit", OnError: "continue", Extra: map[string]any{"target": "recent", "since": "7d", "instructions": DefaultAuditInstructions}}}},
	},
	{
		Name: "Ask Grok to audit",
		Description: "Run by hand: another model audits the repository, or a branch of it, for what you name (a part of the code, a kind of problem, a big change). Its findings are kept under Audits, and become a task for Claude to check and fix only if you ask for one there. " +
			auditTemplateModel,
		Rule: Rule{Trigger: Trigger{Event: "manual"}, Concurrency: "queue",
			Inputs: []RuleInput{
				{Name: "focus", Label: "Look for", Type: "text", Default: DefaultAuditInstructions},
				{Name: "branch", Label: "Branch (empty: the whole repository)", Type: "text"},
			},
			Actions: []Action{{Type: "audit", ID: "audit", OnError: "continue", Extra: map[string]any{
				"target": "branch", "ref": "{{.Inputs.branch}}", "instructions": "{{.Inputs.focus}}"}}}},
	},
}
