package core

// Gallery entries that use steps (WP-D18): turn on as they are, or copy and
// edit. They sit in their own list so other parts can add theirs beside them.
func init() {
	AutomationTemplates = append(AutomationTemplates, stepTemplates...)
}

var stepTemplates = []AutomationTemplate{
	{
		Name:        "Tell me when an agent waits more than 15 minutes",
		Description: "An agent session that is still waiting for you after a quarter of an hour notifies the project's owners.",
		Rule: Rule{Trigger: Trigger{Event: "session_waiting"}, Actions: []Action{
			{Type: "wait_for", ID: "resumed", Extra: map[string]any{"event": EvSessionUpdated, "key": "session:{{.Trigger.Data.session}}", "where": "state!=waiting", "timeout": "15m", "on_timeout": "continue"}},
			{Type: "notify", User: "owners", ID: "tell", OnlyIf: "{{.Steps.resumed.TimedOut}}", Title: "An agent is waiting for you",
				Message: "{{.Trigger.Data.agent}} has been waiting for 15 minutes{{if .Trigger.Data.repo}} in {{.Trigger.Data.repo}}{{end}}."},
		}},
	},
	{
		Name:        "Tell me when a request waits more than 15 minutes",
		Description: "An agent's request that nobody has decided after 15 minutes notifies the project's owners.",
		Rule: Rule{Trigger: Trigger{Event: "request_waiting", Extra: map[string]any{"after": "15m"}}, Actions: []Action{
			{Type: "notify", User: "owners", Title: "A request is waiting for you", Message: "Request {{.Trigger.Data.code}} ({{.Trigger.Data.kind}}) has waited 15 minutes."},
		}},
	},
	{
		Name:        "Daily digest of what changed",
		Description: "Every weekday at 9:00 a model summarises the issues that changed in the last day and notifies the project's owners. Needs a model: set Automations in Admin → AI, or pick one in the step.",
		Rule: Rule{Trigger: Trigger{Event: "schedule", Every: "weekday", At: "09:00"}, Actions: []Action{
			{Type: "search_issues", ID: "recent", Extra: map[string]any{"query": "updated:>-1d", "limit": 40}},
			{Type: "ask_model", ID: "digest", OnlyIf: "{{if .Steps.recent.Count}}true{{end}}", Extra: map[string]any{
				"prompt": "Write a short digest (a few bullet points, plain language) of what changed in the last day in project {{.Project.Name}}. Group related issues, and name the keys.\n\nIssues that changed ({{.Steps.recent.Count}}):\n{{untrusted .Steps.recent.Text}}"}},
			{Type: "notify", User: "owners", OnlyIf: "{{.Steps.digest.OK}}", Title: "Daily digest", Message: "{{.Steps.digest.Text}}"},
		}},
	},
	{
		Name:        "Summarise recent work (run by hand)",
		Description: "A Run now button: asks how many days to look back, has a model summarise the issues finished in that time and notifies you.",
		Rule: Rule{Trigger: Trigger{Event: "manual"}, Inputs: []RuleInput{{Name: "days", Label: "Days to look back", Type: "number", Default: "7"}}, Actions: []Action{
			{Type: "search_issues", ID: "done", Extra: map[string]any{"query": "is:done updated:>-{{.Inputs.days}}d", "limit": 60}},
			{Type: "ask_model", ID: "summary", Extra: map[string]any{
				"prompt": "Summarise what was finished in the last {{.Inputs.days}} days in project {{.Project.Name}} as a short list of achievements.\n\n{{untrusted .Steps.done.Text}}"}},
			{Type: "notify", User: "actor", Title: "Recent work", Message: "{{.Steps.summary.Text}}"},
		}},
	},
	{
		Name:        "First look at new bugs",
		Description: "When a bug is reported, a model comments with questions to ask and where to look first. Needs a model (Admin → AI, Automations).",
		Rule: Rule{Trigger: Trigger{Event: "created"}, If: "type:bug", Actions: []Action{
			{Type: "ask_model", ID: "look", Extra: map[string]any{
				"prompt": "A bug was reported. In under 120 words, list what to find out first and where to look. Do not invent facts about the code.\n\nTitle: {{.Issue.Title}}\n{{untrusted .Issue.Description}}"}},
			{Type: "comment", Message: "First look (from a model, check before relying on it):\n\n{{.Steps.look.Text}}"},
		}},
	},
	{
		Name:        "Warn when open bugs pile up",
		Description: "Notifies the project's owners when there are 10 or more open bugs, once each time the count crosses.",
		Rule: Rule{Trigger: Trigger{Event: "count_crossed", Extra: map[string]any{"query": "is:open type:bug", "op": ">=", "n": 10}}, Actions: []Action{
			{Type: "notify", User: "owners", Title: "Open bugs are piling up", Message: "There are {{.Trigger.Data.count}} open bugs."},
		}},
	},
	{
		Name:        "Low disk space",
		Description: "Notifies the admins when free space on the data disk, the database's size or free memory crosses its limit (set the limits below).",
		Admin:       true,
		Rule: Rule{Trigger: Trigger{Event: "system_resources"}, Actions: []Action{
			{Type: "notify", User: "admins", Title: "Waypoint is running low on resources", Message: "{{.Trigger.Data.text}}"},
		}},
	},
	{
		Name:        "Backup failed",
		Description: "Notifies the admins when the daily database backup fails.",
		Admin:       true,
		Rule: Rule{Trigger: Trigger{Event: "backup_failed"}, Actions: []Action{
			{Type: "notify", User: "admins", Title: "The backup failed", Message: "{{.Trigger.Data.error}}"},
		}},
	},
	{
		Name:        "A runner went offline",
		Description: "Notifies the admins when a runner stops answering.",
		Admin:       true,
		Rule: Rule{Trigger: Trigger{Event: "runner_offline"}, Actions: []Action{
			{Type: "notify", User: "admins", Title: "A runner went offline", Message: "Runner {{.Trigger.Data.name}} went offline."},
		}},
	},
}
