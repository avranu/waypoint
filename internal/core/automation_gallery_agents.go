package core

// Gallery entries that run coding agents, open and merge pull requests and
// watch the agents' work (WP-D18, phase 2). Agent steps run on the runners of
// the person who turns the card on; each step names the CLI and model it
// uses, so changing the model is one dropdown.
func init() {
	AutomationTemplates = append(AutomationTemplates, agentTemplates...)
}

// museSpark is the free model the cards start with.
var museSpark = []any{map[string]any{"cli": "opencode", "model": "opencode/muse-spark-1.3-contributor-free"}}

const weeklyReviewPrompt = `Review this repository's code as a careful senior engineer would, and make a small number of real improvements.

- Look for bugs, missing error handling, security problems, dead code, unclear names and missing tests. Prefer a few changes that clearly matter over many that do not (at most 5).
- Make each change in this checkout, keep it focused, and leave the code building.
- Do not change CI configuration, lockfiles or anything that looks like a secret.
- Finish with a short list: each change, and why it is worth making. If nothing is worth changing, change nothing and say so.`

var agentTemplates = []AutomationTemplate{
	{
		Name: "Weekly code review into a PR",
		Description: "Every Tuesday at 12:00 a coding agent (opencode with Muse Spark 1.3, free) reviews the repository and commits suggested changes on a new branch. The rule opens a pull request, " +
			"fixes failing CI (up to 3 tries), waits for the review bots to have no open findings, merges it when everything is clean, and tells you. Runs on your runner: it needs one online with opencode signed in.",
		Rule: Rule{Trigger: Trigger{Event: "schedule", Every: "week", Weekday: 2, At: "12:00"}, Concurrency: "skip", Actions: []Action{
			{Type: "agent_task", ID: "review", Extra: map[string]any{"agents": museSpark, "permission": AgentEdit, "branch": "review/{{.Date}}", "push": true,
				"commit_message": "Suggested changes from the weekly code review", "prompt": weeklyReviewPrompt, "timeout": "45m"}},
			{Type: "open_pr", ID: "pr", OnlyIf: "{{if .Steps.review.Commits}}true{{end}}", Extra: map[string]any{
				"head": "{{.Steps.review.Branch}}", "title": "Weekly code review {{.Date}}", "body": "{{.Steps.review.Answer}}"}},
			{Type: "repeat", ID: "babysit", OnlyIf: "{{if .Steps.pr.Number}}true{{end}}", Extra: map[string]any{"max": 3, "until": "{{.Steps.ci.Passed}}"}, Steps: []Action{
				{Type: "wait_for", ID: "ci", Extra: map[string]any{"what": waitCI, "timeout": "2h", "on_timeout": "continue"}},
				{Type: "fix_ci", ID: "fix", OnlyIf: "{{not .Steps.ci.Passed}}", Extra: map[string]any{"agents": museSpark}},
			}},
			{Type: "wait_for", ID: "gate", OnlyIf: "{{.Steps.ci.Passed}}", Extra: map[string]any{"what": waitGate, "timeout": "6h", "on_timeout": "continue"}},
			{Type: "merge_pr", ID: "merge", OnlyIf: "{{.Steps.ci.Passed}}", Extra: map[string]any{"when_ready": true, "style": MergeSquash, "delete_branch": true, "timeout": "6h", "on_timeout": "continue"}},
			{Type: "notify", User: "owners", Title: "Weekly code review", Message: "{{if .Steps.merge.Merged}}The weekly review was merged: {{.Steps.pr.URL}}" +
				"{{else if .Steps.pr.URL}}The weekly review is open and needs you: {{.Steps.pr.URL}}{{else}}The weekly review found nothing worth changing.{{end}}"},
		}},
	},
	{
		Name:        "Ship merged PRs",
		Description: "When something is pushed to main, run a command on your runner (make deploy-remote). For repositories whose CI does not deploy. Added switched off: set the folder on your runner, then turn it on.",
		Off:         true,
		Rule: Rule{Trigger: Trigger{Event: "repo_pushed", Extra: map[string]any{"branch": "main"}}, Concurrency: "queue", Actions: []Action{
			{Type: "run_command", ID: "deploy", Extra: map[string]any{"command": "make deploy-remote", "dir": "/path/to/your/checkout", "timeout": "20m"}},
			{Type: "notify", User: "owners", Title: "Deployed", OnlyIf: "{{.Steps.deploy.OK}}", Message: "Shipped {{.Trigger.Data.after}} from {{.Trigger.Data.branch}}."},
		}},
	},
	{
		Name:        "Merge when ready",
		Description: "A pull request labelled automerge merges once its checks passed and the review gate is green. Waits up to a day, never merges one a person asked changes on.",
		Rule: Rule{Trigger: Trigger{Event: "pr_updated"}, PR: &PRFilter{Bots: true, Drafts: false}, Actions: []Action{
			{Type: "merge_pr", ID: "merge", OnlyIf: `{{contains "automerge" (join "," .PR.Labels)}}`, Extra: map[string]any{"when_ready": true, "style": MergeSquash, "delete_branch": true, "timeout": "24h", "on_timeout": "continue"}},
		}},
	},
	{
		Name:        "Retry flaky CI once",
		Description: "When CI fails on a pull request, run the failed checks again, once. If it fails again you are told. Works on GitHub, GitLab and Forgejo 17 or later; an older Forgejo cannot be asked to re-run, so there you are told straight away.",
		Rule: Rule{Trigger: Trigger{Event: "pr_ci_failed"}, Actions: []Action{
			{Type: "pr_label", ID: "mark", OnlyIf: `{{not (contains "ci-retried" (join "," .PR.Labels))}}`, Extra: map[string]any{"add": []any{"ci-retried"}}},
			{Type: "retry_ci", ID: "retry", OnlyIf: "{{.Steps.mark.OK}}", OnError: "continue"},
			{Type: "notify", User: "owners", Title: "CI failed", OnlyIf: "{{or .Steps.mark.Skipped .Steps.retry.Failed}}",
				Message: "CI failed on {{.PR.Repo}}#{{.PR.Number}} {{.PR.Title}}{{if .Steps.retry.Failed}} and couldn't be run again: {{.Steps.retry.Error}}{{else}} again after a retry{{end}}: {{.PR.URL}}"},
		}},
	},
	{
		Name:        "Tell me when an environment goes down",
		Description: "Notifies the project's owners when an environment's checks report it down.",
		Rule: Rule{Trigger: Trigger{Event: "env_down"}, Actions: []Action{
			{Type: "notify", User: "owners", Title: "{{.Trigger.Data.name}} is down", Message: "{{.Trigger.Data.name}} went down{{if .Trigger.Data.url}}: {{.Trigger.Data.url}}{{end}}."},
		}},
	},
	{
		Name:        "Tell me when an agent's question waits more than 30 minutes",
		Description: "A question an agent asked that nobody has answered after half an hour notifies the project's owners.",
		Rule: Rule{Trigger: Trigger{Event: "request_waiting", Extra: map[string]any{"after": "30m", "kind": RequestQuestion}}, Actions: []Action{
			{Type: "notify", User: "owners", Title: "An agent is waiting for your answer", Message: "{{.Trigger.Data.agent}} asked “{{.Trigger.Data.title}}” ({{.Trigger.Data.code}}) 30 minutes ago."},
		}},
	},
	{
		Name:        "Daily digest of agent work",
		Description: "Every morning a model (the Automations models in Admin → AI, free ones by default) summarises what the agents did, are doing and wait on you for, and notifies the project's owners.",
		Rule: Rule{Trigger: Trigger{Event: "schedule", Every: "day", At: "08:00"}, Actions: []Action{
			{Type: "agent_activity", ID: "activity", Extra: map[string]any{"days": 1}},
			{Type: "ask_model", ID: "digest", OnlyIf: "{{if or .Steps.activity.Done .Steps.activity.Doing .Steps.activity.Waiting}}true{{end}}", Extra: map[string]any{
				"prompt": "Write a short digest (at most six bullet points, plain language) of what the coding agents did in project {{.Project.Name}} in the last day, what they are doing, and anything waiting on a person. Name the keys.\n\n{{untrusted .Steps.activity.Text}}"}},
			{Type: "notify", User: "owners", Title: "Agents: yesterday in {{.Project.Name}}", OnlyIf: "{{.Steps.digest.OK}}", Message: "{{.Steps.digest.Text}}"},
		}},
	},
	{
		Name: "Fix failing CI on members' pull requests", Off: true,
		Description: "When CI fails on a pull request written by a member of this project (editor or owner, from a branch of the repository itself, never a fork), a coding agent reads the failed logs, fixes the code and pushes to the branch, up to 3 tries. " +
			"It never changes CI configuration, lockfiles or secrets, and you are told if it gives up. Runs on your runner (opencode with Muse Spark 1.3 by default). Added switched off.",
		Rule: Rule{Trigger: Trigger{Event: "pr_ci_failed"}, Actions: []Action{
			{Type: "pr_label", ID: "claim", OnlyIf: `{{not (contains "ci-fixing" (join "," .PR.Labels))}}`, Extra: map[string]any{"add": []any{"ci-fixing"}}},
			{Type: "repeat", ID: "fixes", OnlyIf: "{{.Steps.claim.OK}}", Extra: map[string]any{"max": 3, "until": "{{.Steps.ci.Passed}}"}, Steps: []Action{
				{Type: "fix_ci", ID: "fix", Extra: map[string]any{"agents": museSpark}},
				{Type: "wait_for", ID: "ci", Extra: map[string]any{"what": waitCI, "timeout": "2h", "on_timeout": "continue"}},
			}},
			{Type: "pr_label", ID: "release", OnlyIf: "{{.Steps.ci.Passed}}", Extra: map[string]any{"remove": []any{"ci-fixing"}}},
			{Type: "notify", User: "owners", Title: "CI is still failing", OnlyIf: "{{and .Steps.claim.OK (not .Steps.ci.Passed)}}",
				Message: "A coding agent tried to fix CI on {{.PR.Repo}}#{{.PR.Number}} {{.PR.Title}} and could not: {{.PR.URL}}"},
		}},
	},
	{
		Name: "Fix failing CI on agents' PRs",
		Description: "When CI fails on a pull request an agent wrote, a coding agent reads the failed logs, fixes the code and pushes to the branch, up to 3 tries. It never changes CI configuration, lockfiles or secrets, " +
			"and you are told if it gives up. Runs on your runner (opencode with Muse Spark 1.3 by default).",
		Rule: Rule{Trigger: Trigger{Event: "pr_ci_failed"}, Actions: []Action{
			{Type: "pr_label", ID: "claim", OnlyIf: `{{and .PR.Agent (not (contains "ci-fixing" (join "," .PR.Labels)))}}`, Extra: map[string]any{"add": []any{"ci-fixing"}}},
			{Type: "repeat", ID: "fixes", OnlyIf: "{{.Steps.claim.OK}}", Extra: map[string]any{"max": 3, "until": "{{.Steps.ci.Passed}}"}, Steps: []Action{
				{Type: "fix_ci", ID: "fix", Extra: map[string]any{"agents": museSpark}},
				{Type: "wait_for", ID: "ci", Extra: map[string]any{"what": waitCI, "timeout": "2h", "on_timeout": "continue"}},
			}},
			{Type: "pr_label", ID: "release", OnlyIf: "{{.Steps.ci.Passed}}", Extra: map[string]any{"remove": []any{"ci-fixing"}}},
			{Type: "notify", User: "owners", Title: "CI is still failing", OnlyIf: "{{and .Steps.claim.OK (not .Steps.ci.Passed)}}",
				Message: "An agent tried to fix CI on {{.PR.Repo}}#{{.PR.Number}} {{.PR.Title}} and could not: {{.PR.URL}}"},
		}},
	},
}
