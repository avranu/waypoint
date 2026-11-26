package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/wql"
)

// AutomationTemplate is a ready-made rule offered in the UI.
type AutomationTemplate struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Rule        Rule   `json:"rule"`
	// Default templates are created, enabled, in every new project.
	Default bool `json:"default"`
	// Admin templates are for Admin → Automations; the rest are for projects.
	Admin bool `json:"admin,omitempty"`
	// Off templates are added switched off: they need a setting only the
	// person knows (a folder on their runner) before they can run.
	Off bool `json:"off,omitempty"`
}

// reviewFilter leaves out what llm-review left out: people's drafts (Waypoint's
// own are reviewed, WP-158), bots, dependency updates and release PRs.
var reviewFilter = &PRFilter{SkipLabels: []string{"dependencies", "autorelease: pending"}, SkipHeads: []string{"release-please--*"}}

// AutomationTemplates are the built-in rules.
var AutomationTemplates = []AutomationTemplate{
	{
		Name: "Close parent when all subtasks are done", Default: true,
		Description: "When the last open child of an issue is finished, move the parent to Done.",
		Rule:        Rule{Trigger: Trigger{Event: "children_done"}, Actions: []Action{{Type: "set_status", Status: "done"}}},
	},
	{
		Name: "Reopen parent when a subtask reopens", Default: true,
		Description: "If a child is reopened under a closed parent, move the parent back to In Progress.",
		Rule:        Rule{Trigger: Trigger{Event: "reopened"}, If: "has:parent", Actions: []Action{{Type: "reopen_parent", Status: "in_progress"}}},
	},
	{
		Name:        "Start my timer when I start work",
		Description: "Moving an issue into an in-progress status starts your timer on it.",
		Rule:        Rule{Trigger: Trigger{Event: "status_changed", To: "in_progress"}, Actions: []Action{{Type: "start_timer", User: "actor"}}},
	},
	{
		Name:        "Stop my timer when work stops",
		Description: "Moving an issue out of in-progress stops your timer if it is running on that issue.",
		Rule:        Rule{Trigger: Trigger{Event: "status_changed", From: "in_progress"}, Actions: []Action{{Type: "stop_timer", User: "actor"}}},
	},
	{
		Name:        "Assign to me when I start work",
		Description: "Starting an unassigned issue assigns it to you.",
		Rule:        Rule{Trigger: Trigger{Event: "status_changed", To: "in_progress"}, If: "is:unassigned", Actions: []Action{{Type: "assign", User: "actor"}}},
	},
	{
		Name: "Move to In Review when a PR opens", Default: true,
		Description: "A linked pull request being opened moves an open issue to In Review.",
		Rule:        Rule{Trigger: Trigger{Event: "pr_opened"}, If: "is:open", Actions: []Action{{Type: "set_status", Status: "In Review"}}},
	},
	{
		Name: "Move to Done when a PR merges", Default: true,
		Description: "A linked pull request being merged finishes the issue. wp done leaves a task In Review until its pull request merges and says the merge finishes it, so this rule is on in every project (WP-496).",
		Rule:        Rule{Trigger: Trigger{Event: "pr_merged"}, If: "is:open", Actions: []Action{{Type: "set_status", Status: "done"}}},
	},
	{
		Name:        "Push urgent issues to Gotify",
		Description: "New urgent issues notify their watchers on Gotify.",
		Rule: Rule{Trigger: Trigger{Event: "created"}, If: "priority:urgent", Actions: []Action{{Type: "notify", User: "watchers",
			Message: "Urgent: {{.Issue.Key}} {{.Issue.Title}}"}}},
	},
	{
		Name:        "Nudge work that has gone quiet",
		Description: "When an open issue hasn't changed for 30 days, remind its assignees.",
		Rule: Rule{Trigger: Trigger{Event: "stale", Days: 30}, If: "is:assigned",
			Actions: []Action{{Type: "notify", User: "assignees", Message: "No changes in a month. Close it, re-plan it or park it in the backlog."}}},
	},
	{
		Name:        "Weekly planning issue",
		Description: "Every Monday at 9:00, create a planning issue for the week.",
		Rule: Rule{Trigger: Trigger{Event: "schedule", Every: "week", Weekday: 1, At: "09:00"},
			Actions: []Action{{Type: "create_issue", Title: "Plan the week of {{.Date}}", Due: "+2d"}}},
	},
	{
		Name: "Review pull requests", Default: true,
		Description: "Each new pull request, and each push to one, gets a review from the review models (Admin → AI): one review with line comments, 👀 while it works and 👍 when it finds nothing. Not people's drafts, bots' or release PRs.",
		Rule:        Rule{Trigger: Trigger{Event: "pr_updated"}, PR: reviewFilter, Actions: []Action{{Type: "review"}}},
	},
	{
		Name: "Review when asked", Default: true,
		Description: "\"@waypoint review\" in a pull request comment reviews it now; \"@waypoint full review\" reviews it again even if this head was reviewed.",
		Rule:        Rule{Trigger: Trigger{Event: "pr_command", Command: "review"}, Actions: []Action{{Type: "review"}}},
	},
	{
		Name: "Answer replies", Default: true,
		Description: "When a person replies in a thread of one of the bot's findings, the bot answers once: it stands by the finding with evidence, withdraws it, or agrees it is minor. It never argues a thread a person resolved.",
		Rule:        Rule{Trigger: Trigger{Event: "pr_comment"}, Actions: []Action{{Type: "reply"}}},
	},
	{
		Name:        "Ask CodeRabbit to review",
		Description: "Each new pull request, or draft marked ready, gets \"@coderabbitai review\" from the bot account, once, not on pushes. For repos a mirror copies to GitHub, where CodeRabbit reviews the twin: forge-sync passes the command on when the bot account is one of its command_authors. Added on, by migration, to projects with a repo on a forge that has a mirror account.",
		Rule: Rule{Trigger: Trigger{Event: "pr_updated", Extra: map[string]any{"on": "new"}}, PR: reviewFilter,
			Actions: []Action{{Type: "pr_comment", Message: askCodeRabbit}}},
	},
	{
		Name:        "Security review of pull requests",
		Description: "A second review of each pull request that looks only for security problems: injection, auth, secrets, unsafe input.",
		Rule: Rule{Trigger: Trigger{Event: "pr_updated"}, PR: reviewFilter, Actions: []Action{{Type: "review", Force: true, MaxFindings: 5,
			Focus: "Look only for security problems: injection, broken authentication or authorisation, leaked secrets, unsafe handling of untrusted input, SSRF, path traversal."}}},
	},
	{
		Name: "Review with a second opinion", Off: true,
		Description: "A second reviewer, a different free model (opencode's Beam, as a coding agent on your runner), reads each pull request alongside the review models; a finding both raise is marked as raised by two reviewers, and each finding is checked against the code before it is posted. Added switched off; turn off \"Review pull requests\" first, or both post. Needs a runner with opencode signed in, and only members' pull requests reach it.",
		Rule: Rule{Trigger: Trigger{Event: "pr_updated"}, PR: reviewFilter, Actions: []Action{{Type: "review",
			Extra: map[string]any{"second_models": []any{map[string]any{"cli": "opencode", "model": "opencode/beam-free"}}}}}},
	},
	{
		Name:        "Tell me about serious review findings",
		Description: "Reviews each pull request, waits for the review, and notifies the project's owners when it found a P1 or worse. It replaces Review pull requests: turn that one off, or each push gets two reviews.",
		Rule: Rule{Trigger: Trigger{Event: "pr_updated"}, PR: reviewFilter, Actions: []Action{
			{Type: "review", ID: "review"},
			{Type: "notify", User: "owners", OnlyIf: `{{or .Steps.review.P0 .Steps.review.P1}}`,
				Message: "{{.Steps.review.Worst}} on {{.PR.Repo}}#{{.PR.Number}} {{.PR.Title}}: {{.Steps.review.Summary}} {{.Steps.review.URL}}"},
		}},
	},
}

func (s *Service) createDefaultAutomations(ctx context.Context, t *tx, projectID int64) error {
	now := s.nowMS()
	for n, tpl := range AutomationTemplates {
		if !tpl.Default {
			continue
		}
		b, _ := json.Marshal(tpl.Rule)
		if _, err := t.ExecContext(ctx, `INSERT INTO automations(project_id, name, enabled, rule, position, created_at, updated_at) VALUES (?,?,1,?,?,?,?)`,
			projectID, tpl.Name, string(b), n, now, now); err != nil {
			return err
		}
	}
	return nil
}

// matches reports whether an event starts an automation with this trigger.
func (tr Trigger) matches(e *Event) bool {
	d := triggerDef(tr.Event)
	if d == nil || d.Event == "" || d.Event != e.Kind {
		return false
	}
	if by := tr.str("by"); by != "" && by != "any" && eventBy(e) != by {
		return false
	}
	return d.Match == nil || d.Match(tr, e)
}

// automationBudget caps the actions one user change can set off, across
// every rule it triggers directly or through other rules.
const automationBudget = 50

// issueMatches checks one issue against a query. "me" is the triggering
// user; visibility is not applied.
func (s *Service) issueMatches(ctx context.Context, user *User, issueID int64, query string) (bool, error) {
	q, err := wql.Parse(query)
	if err != nil {
		return false, err
	}
	u := *user
	u.IsAdmin = true
	c, err := s.compileQuery(ctx, &Actor{User: &u}, q, 0)
	if err != nil {
		return false, err
	}
	c.add("i.id = ?", issueID)
	sqlText, args := c.sql(`SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id LEFT JOIN sprints sp ON sp.id = i.sprint_id`)
	var n int
	err = s.db.R.QueryRowContext(ctx, sqlText, args...).Scan(&n)
	return n > 0, err
}

// runAction does one of the original actions (the issue, notification and
// pull request ones); newer actions have their own function in their
// definition. The step context carries the template data, so these see
// {{.Steps}}, {{.Trigger}} and {{.Inputs}} too.
func (s *Service) runAction(c *StepCtx) error {
	ctx, actor, a, act, e := c.Ctx, c.Actor, c.Automation, *c.Step, c.Event
	data := c.Data()
	issue := data.Issue
	projectID := cmpID(a.ProjectID, e.ProjectID)
	render := func(tpl, fallback string) (string, error) {
		if tpl == "" {
			tpl = fallback
		}
		return renderTemplate(tpl, data)
	}
	needIssue := func() error {
		if issue == nil {
			return invalid("this trigger has no issue")
		}
		return nil
	}
	switch act.Type {
	case "reply":
		findingID := dataInt(e, "finding_id")
		if findingID == 0 {
			return nil
		}
		commentID, _ := e.Data["comment_id"].(string)
		var autoID *int64
		if a.ID > 0 {
			autoID = &a.ID
		}
		_, err := s.enqueueBotRun(ctx, a.ProjectID, autoID, data.PR, "reply", reviewOptions{Models: act.Models, FindingID: findingID}, e.New, commentID)
		return err
	case "pr_comment", "react":
		f, err := s.forgeByID(ctx, data.PR.ForgeID)
		if err != nil {
			return err
		}
		cl, ok := s.Forges.(ForgePRs)
		if !ok {
			return invalid("this forge can't be commented on")
		}
		if act.Type == "react" {
			_, err = cl.React(ctx, botForge(f), data.PR.Repo, data.PR.Number, "", act.Value)
			return err
		}
		msg, err := render(act.Message, "")
		if err != nil || strings.TrimSpace(msg) == "" {
			return cmpErr(err, invalid("pr_comment needs a message"))
		}
		// "Ask CodeRabbit to review" is not repeated on a head it was asked on
		// (WP-1808, WP-1732): "once" holds across a draft marked ready twice.
		// Other comments post as they always did.
		if msg == askCodeRabbit {
			asked, err := s.askedOnHead(ctx, data.PR.ID, a.ID, data.PR.HeadSHA, msg)
			if err != nil {
				return err
			}
			if asked {
				c.note("skipped", "already asked on "+short(data.PR.HeadSHA))
				s.log.InfoContext(ctx, "not asking again on this head", "pr", data.PR.ID, "head", short(data.PR.HeadSHA), "automation", a.ID)
				return nil
			}
		}
		c, err := cl.PostPRComment(ctx, botForge(f), data.PR.Repo, data.PR.Number, msg)
		if err == nil {
			s.noteOwnComment(ctx, data.PR.ID, c, a.ID, data.PR.HeadSHA, msg)
		}
		return err
	case "set_status":
		if err := needIssue(); err != nil {
			return err
		}
		st := act.Status
		patch := IssuePatch{Status: &st}
		if e.Kind == EvExternalPROpened {
			patch.ViaLink = e.New
		}
		if e.Kind == EvExternalPRMerged {
			patch.mergedLink = e.New
		}
		_, err := s.UpdateIssue(ctx, actor, itoa64(issue.ID), patch)
		return err
	case "set_parent_status", "reopen_parent":
		if err := needIssue(); err != nil {
			return err
		}
		if issue.ParentID == nil {
			return nil
		}
		parent, err := s.issueByID(ctx, s.db.R, *issue.ParentID, false)
		if err != nil {
			return err
		}
		if act.Type == "reopen_parent" && !parent.Closed() {
			return nil
		}
		st := act.Status
		if st == "" {
			st = CatInProgress
		}
		_, err = s.UpdateIssue(ctx, actor, itoa64(parent.ID), IssuePatch{Status: &st})
		return err
	case "set_children_status":
		if err := needIssue(); err != nil {
			return err
		}
		rows, err := s.db.R.QueryContext(ctx, `SELECT c.id FROM issues c JOIN statuses st ON st.id = c.status_id WHERE c.parent_id = ? AND st.category NOT IN ('done','canceled')`, issue.ID)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, itoa64(id))
		}
		rows.Close()
		if len(ids) == 0 {
			return nil
		}
		st := act.Status
		_, err = s.BulkUpdate(ctx, actor, ids, IssuePatch{Status: &st}, false)
		return err
	case "set_priority":
		if err := needIssue(); err != nil {
			return err
		}
		_, err := s.UpdateIssue(ctx, actor, itoa64(issue.ID), IssuePatch{Priority: act.Value})
		return err
	case "set_field":
		if err := needIssue(); err != nil {
			return err
		}
		var p IssuePatch
		v := act.Value
		switch act.Field {
		case "points":
			if v == "" {
				p.Points = Null[float64]()
			} else {
				var f float64
				if _, err := fmt.Sscan(v, &f); err != nil {
					return invalid("points must be a number")
				}
				p.Points = Some(f)
			}
		case "due_date":
			p.DueDate = Some(v)
		case "start_date":
			p.StartDate = Some(v)
		case "type":
			p.Type = &v
		case "sprint":
			p.Sprint = Some(v)
		case "title":
			t, err := render(v, "")
			if err != nil {
				return err
			}
			p.Title = &t
		case "priority":
			p.Priority = v
		default:
			return invalid("set_field can't set %q", act.Field)
		}
		_, err := s.UpdateIssue(ctx, actor, itoa64(issue.ID), p)
		return err
	case "assign", "add_assignee":
		if err := needIssue(); err != nil {
			return err
		}
		who := s.actionUser(issue, actor, act.User)
		if who == "" {
			return nil
		}
		var p IssuePatch
		if act.Type == "assign" {
			list := []string{}
			if who != "" && who != "none" {
				list = []string{who}
			}
			p.Assignees = &list
		} else if who != "" && who != "none" {
			p.AddAssign = []string{who}
		}
		_, err := s.UpdateIssue(ctx, actor, itoa64(issue.ID), p)
		return err
	case "add_label", "remove_label":
		if err := needIssue(); err != nil {
			return err
		}
		p := IssuePatch{AddLabels: []string{act.Value}}
		if act.Type == "remove_label" {
			p = IssuePatch{RemLabels: []string{act.Value}}
		}
		_, err := s.UpdateIssue(ctx, actor, itoa64(issue.ID), p)
		return err
	case "comment":
		if err := needIssue(); err != nil {
			return err
		}
		body, err := render(act.Message, "")
		if err != nil || strings.TrimSpace(body) == "" {
			return err
		}
		_, err = s.AddComment(ctx, actor, itoa64(issue.ID), body)
		return err
	case "notify":
		msg, err := render(act.Message, eventText(e))
		if err != nil {
			return err
		}
		title := a.Name
		if issue != nil {
			title = issue.Key + " " + issue.Title
		}
		if act.Title != "" {
			if title, err = render(act.Title, ""); err != nil {
				return err
			}
		}
		for _, uid := range s.actionUserIDs(ctx, issue, actor, act.User, projectID) {
			s.notify(ctx, uid, e, NotifyAutomated, title, msg)
		}
		return nil
	case "gotify":
		msg, err := render(act.Message, eventText(e))
		if err != nil {
			return err
		}
		title, err := render(act.Title, "{{if .Issue}}{{.Issue.Key}} {{.Issue.Title}}{{else}}Waypoint{{end}}")
		if err != nil {
			return err
		}
		prio := 5
		if act.Priority != nil {
			prio = min(max(*act.Priority, 0), 10)
		}
		if act.URL != "" && act.Token != "" {
			url, token := act.URL, act.Token
			s.goBG(func() {
				if err := s.SendGotify(context.Background(), url, token, title, msg, prio, data.URL); err != nil {
					s.log.WarnContext(ctx, "automation gotify failed", "automation", a.Name, "err", err)
				}
			})
			return nil
		}
		for _, uid := range s.actionUserIDs(ctx, issue, actor, cmp(act.User, "assignees"), projectID) {
			u, err := s.userByID(ctx, s.db.R, uid)
			if err != nil || u.Disabled || u.Prefs.GotifyURL == "" || u.Prefs.GotifyToken == "" {
				continue
			}
			if issue != nil && !s.canSee(ctx, s.db.R, &Actor{User: u}, issue.ID, issue.ProjectID) {
				continue
			}
			s.goBG(func() {
				if err := s.SendGotify(context.Background(), u.Prefs.GotifyURL, u.Prefs.GotifyToken, title, msg, prio, data.URL); err != nil {
					s.log.WarnContext(ctx, "automation gotify failed", "automation", a.Name, "err", err)
				}
			})
		}
		return nil
	case "email":
		es, err := s.emailSettings(ctx)
		if err != nil {
			return err
		}
		if !es.Configured {
			return errEmailOff
		}
		msg, err := render(act.Message, eventText(e))
		if err != nil {
			return err
		}
		subject, err := render(act.Title, "{{if .Issue}}{{.Issue.Key}} {{.Issue.Title}}{{else}}Waypoint{{end}}")
		if err != nil {
			return err
		}
		to, err := parseRecipients(anyString(act.Extra["to"]))
		if err != nil {
			return err
		}
		if act.User != "" {
			for _, uid := range s.actionUserIDs(ctx, issue, actor, act.User, projectID) {
				u, err := s.userByID(ctx, s.db.R, uid)
				if err != nil || u.Disabled || u.Email == "" {
					continue
				}
				if issue != nil && !s.canSee(ctx, s.db.R, &Actor{User: u}, issue.ID, issue.ProjectID) {
					continue
				}
				to = append(to, u.Email)
			}
		}
		slices.Sort(to)
		for _, addr := range slices.Compact(to) {
			s.goBG(func() {
				if err := sendEmail(context.Background(), es, addr, subject, msg, data.URL); err != nil {
					s.log.WarnContext(ctx, "automation email failed", "automation", a.Name, "err", err)
				}
			})
		}
		return nil
	case "webhook", "kestra":
		body, err := render(act.Body, "")
		if err != nil {
			return err
		}
		var payload []byte
		if strings.TrimSpace(body) != "" {
			if !json.Valid([]byte(body)) {
				return invalid("webhook body is not valid JSON after templating")
			}
			payload = []byte(body)
		} else {
			payload, _ = json.Marshal(s.webhookPayload(e, issue))
		}
		url := act.URL
		s.goBG(func() {
			if _, err := s.postJSON(context.Background(), url, "", e.Kind, payload, newToken("")[:20]); err != nil {
				s.log.WarnContext(ctx, "automation webhook failed", "automation", a.Name, "err", err)
			}
		})
		return nil
	case "start_timer":
		if err := needIssue(); err != nil {
			return err
		}
		u, err := s.resolveUser(ctx, s.db.R, actor, cmp(s.actionUser(issue, actor, act.User), "actor"))
		if err != nil {
			return err
		}
		if u.ID == 0 {
			return nil
		}
		_, err = s.StartTimer(ctx, &Actor{User: u, Automation: a.ref(), Depth: actor.Depth}, itoa64(issue.ID), "")
		return err
	case "stop_timer":
		if err := needIssue(); err != nil {
			return err
		}
		u, err := s.resolveUser(ctx, s.db.R, actor, cmp(s.actionUser(issue, actor, act.User), "actor"))
		if err != nil || u.ID == 0 {
			return err
		}
		ua := &Actor{User: u, Automation: a.ref(), Depth: actor.Depth}
		running, err := s.RunningTimer(ctx, ua)
		if err != nil || running == nil || running.Issue == nil || running.Issue.ID != issue.ID {
			return err
		}
		_, err = s.StopTimer(ctx, ua)
		return err
	case "add_to_sprint":
		if err := needIssue(); err != nil {
			return err
		}
		_, err := s.UpdateIssue(ctx, actor, itoa64(issue.ID), IssuePatch{Sprint: Some(act.Value)})
		return err
	case "set_agent":
		if err := needIssue(); err != nil {
			return err
		}
		v := act.Value
		_, err := s.UpdateIssue(ctx, actor, itoa64(issue.ID), IssuePatch{Agent: &v})
		return err
	case "set_workflow":
		if err := needIssue(); err != nil {
			return err
		}
		p := IssuePatch{Workflow: Some(act.Value)}
		if strings.EqualFold(act.Value, "none") || act.Value == "" {
			p.Workflow = Null[string]()
		}
		_, err := s.UpdateIssue(ctx, actor, itoa64(issue.ID), p)
		return err
	case "set_step":
		if err := needIssue(); err != nil {
			return err
		}
		step := act.Value
		if strings.EqualFold(step, "next") {
			if issue.WorkflowID == nil {
				return invalid("the issue isn't on a workflow")
			}
			w, err := s.workflowByID(ctx, s.db.R, *issue.WorkflowID)
			if err != nil {
				return err
			}
			n := w.stepIndex(issue.Step)
			if n < 0 || n == len(w.Steps)-1 {
				return nil
			}
			step = w.Steps[n+1].Name
		}
		_, err := s.UpdateIssue(ctx, actor, itoa64(issue.ID), IssuePatch{Step: &step})
		return err
	case "create_issue":
		title, err := render(act.Title, "")
		if err != nil {
			return err
		}
		if strings.TrimSpace(title) == "" && act.Template == "" {
			return invalid("create_issue needs a title or a template")
		}
		body, err := render(act.Body, "")
		if err != nil {
			return err
		}
		project := itoa64(projectID)
		if p := anyString(act.Extra["project"]); p != "" {
			project = p
		}
		if project == "0" {
			return invalid("create_issue needs a project: name one in the step")
		}
		in := IssueInput{Project: project, Title: title, Description: body, Type: act.IssueType, Labels: act.Labels, DueDate: act.Due, Template: act.Template,
			Agent: anyString(act.Extra["agent"])}
		if p := anyString(act.Extra["issue_priority"]); p != "" {
			in.Priority = p
		}
		if who := s.actionUser(issue, actor, act.User); who != "" && who != "none" {
			in.Assignees = []string{who}
		}
		writer := actor
		writtenBy, err := render(anyString(act.Extra["written_by"]), "")
		if err != nil {
			return err
		}
		if w := strings.Join(strings.Fields(writtenBy), "/"); w != "" {
			if len(w) > 100 {
				return invalid("written_by can be at most 100 characters")
			}
			as := *actor
			as.TextAgent = w
			writer = &as
		}
		made, err := s.CreateIssue(ctx, writer, in)
		if err != nil {
			return err
		}
		c.Set("Key", made.Key)
		c.Set("URL", s.BaseURL()+"/issue/"+made.Key)
		return nil
	case "create_subtasks":
		if err := needIssue(); err != nil {
			return err
		}
		for _, t := range act.Titles {
			title, err := render(t, "")
			if err != nil {
				return err
			}
			if strings.TrimSpace(title) == "" {
				continue
			}
			if _, err := s.CreateIssue(ctx, actor, IssueInput{Parent: itoa64(issue.ID), Type: "subtask", Title: title}); err != nil {
				return err
			}
		}
		return nil
	}
	return invalid("unknown action %q", act.Type)
}

// actionUser resolves "actor"/"reporter" to a username for single-user actions.
func (s *Service) actionUser(issue *Issue, actor *Actor, who string) string {
	switch strings.ToLower(who) {
	case "", "actor", "me":
		if actor.User != nil && actor.User.ID != 0 {
			return actor.User.Username
		}
		return ""
	case "reporter":
		if issue != nil && issue.Reporter != nil {
			return issue.Reporter.Username
		}
		return ""
	}
	return who
}

// actionUserIDs resolves a user group for notify-style actions.
func (s *Service) actionUserIDs(ctx context.Context, issue *Issue, actor *Actor, who string, projectID int64) []int64 {
	switch strings.ToLower(who) {
	case "assignees":
		if issue == nil {
			return nil
		}
		var ids []int64
		for _, a := range issue.Assignees {
			ids = append(ids, a.ID)
		}
		return ids
	case "watchers":
		if issue == nil {
			return nil
		}
		return s.watchers(ctx, issue.ID)
	case "admins":
		return s.userIDsWhere(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0`)
	case "owners":
		if projectID == 0 {
			return nil
		}
		return s.userIDsWhere(ctx, `SELECT m.user_id FROM project_members m JOIN users u ON u.id = m.user_id WHERE m.project_id = ? AND m.role = 'owner' AND u.disabled = 0`, projectID)
	}
	name := s.actionUser(issue, actor, who)
	if name == "" {
		return nil
	}
	u, err := s.userByName(ctx, s.db.R, name)
	if err != nil {
		return nil
	}
	return []int64{u.ID}
}

// userIDsWhere runs a query that selects user ids.
func (s *Service) userIDsWhere(ctx context.Context, query string, args ...any) []int64 {
	rows, err := s.db.R.QueryContext(ctx, query, args...)
	if err != nil {
		s.log.WarnContext(ctx, "read who to tell", "err", err)
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
