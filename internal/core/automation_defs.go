package core

import (
	"slices"
	"strings"
)

func f64(v float64) *float64 { return &v }

func opts(pairs ...string) []ParamOption {
	out := make([]ParamOption, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, ParamOption{Value: pairs[i], Label: pairs[i+1]})
	}
	return out
}

// byParam narrows an issue trigger to changes made by people or by agents.
var byParam = ParamDef{Name: "by", Label: "Made by", Type: PSelect, Default: "any", Options: opts("any", "Anyone", "person", "A person", "agent", "An agent"),
	Help: "Only react to changes a person made, or only to those an agent made."}

var branchParam = ParamDef{Name: "branch", Label: "Branch", Type: PGlob, Placeholder: "main or release/*", Help: "A branch name or pattern; empty is any."}
var envParam = ParamDef{Name: "environment", Label: "Environment", Type: PGlob, Placeholder: "production", Help: "An environment name or pattern; empty is any."}

func dataStr(e *Event, key string) string { return anyString(e.Data[key]) }

// matchGlob: the value matches the glob, or the glob is empty.
func matchGlob(glob, v string) bool {
	if strings.TrimSpace(glob) == "" {
		return true
	}
	gs, err := newGlobSet([]string{glob})
	//wplint:ignore blankerr newGlobSet only parses the pattern: a malformed one is an answer (it matches nothing), not a failed read
	return err == nil && gs.match(v)
}

// eventTrigger registers a trigger that listens for an event kind and
// narrows it by data fields: want maps data keys to required values.
func eventTrigger(d TriggerDef, want map[string]string) {
	prev := d.Match
	d.Match = func(tr Trigger, e *Event) bool {
		for k, v := range want {
			if dataStr(e, k) != v {
				return false
			}
		}
		return prev == nil || prev(tr, e)
	}
	registerTrigger(d)
}

func init() {
	registerTrigger(TriggerDef{Name: "created", Label: "Issue created", Group: "Issues", Phrase: "an issue is created", Event: EvIssueCreated, Issue: true, Params: []ParamDef{byParam}})
	registerTrigger(TriggerDef{Name: "status_changed", Label: "Status changes", Group: "Issues", Phrase: "the status changes", Event: EvIssueStatus, Issue: true,
		Params: []ParamDef{
			{Name: "from", Label: "From", Type: PStatus, Help: "Status names or categories, comma separated; empty is any."},
			{Name: "to", Label: "To", Type: PStatus},
			byParam,
		},
		Match: func(tr Trigger, e *Event) bool {
			oc, _ := e.Data["old_category"].(string)
			nc, _ := e.Data["new_category"].(string)
			// A category matches only when the issue leaves (From) or enters (To) it; moving within one doesn't count.
			match := func(want, name, cat string) bool {
				if want == "" {
					return true
				}
				for _, w := range strings.Split(want, ",") {
					w = strings.TrimSpace(w)
					if strings.EqualFold(w, name) {
						return true
					}
					if strings.EqualFold(strings.ReplaceAll(w, " ", "_"), cat) && oc != nc {
						return true
					}
				}
				return false
			}
			return match(tr.From, e.Old, oc) && match(tr.To, e.New, nc)
		}})
	registerTrigger(TriggerDef{Name: "assigned", Label: "Someone is assigned", Group: "Issues", Phrase: "someone is assigned", Event: EvIssueAssigned, Issue: true})
	registerTrigger(TriggerDef{Name: "unassigned", Label: "Someone is unassigned", Group: "Issues", Phrase: "someone is unassigned", Event: EvIssueUnassigned, Issue: true})
	registerTrigger(TriggerDef{Name: "commented", Label: "Someone comments", Group: "Issues", Phrase: "someone comments", Event: EvIssueCommented, Issue: true, Params: []ParamDef{byParam}})
	registerTrigger(TriggerDef{Name: "field_changed", Label: "A field changes", Group: "Issues", Phrase: "a field changes", Event: EvIssueUpdated, Issue: true,
		Params: []ParamDef{{Name: "field", Label: "Field", Type: PField, Options: opts("title", "Title", "description", "Description", "type", "Type", "priority", "Priority", "points", "Points",
			"sprint", "Sprint", "parent", "Parent", "labels", "Tags", "start_date", "Start date", "due_date", "Due date"), Help: "Empty is any field."}},
		Match: func(tr Trigger, e *Event) bool { // Objective events are named by field, so the any-field trigger skips them.
			if tr.Field == "" {
				return e.Field != "objective"
			}
			return strings.EqualFold(tr.Field, e.Field)
		}})
	registerTrigger(TriggerDef{Name: "children_done", Label: "All sub-issues done", Group: "Issues", Phrase: "all sub-issues are done", Event: EvIssueChildrenDone, Issue: true})
	registerTrigger(TriggerDef{Name: "reopened", Label: "Issue reopened", Group: "Issues", Phrase: "an issue is reopened", Event: EvIssueReopened, Issue: true})
	registerTrigger(TriggerDef{Name: "step_changed", Label: "Step changes", Group: "Issues", Phrase: "the workflow step changes", Event: EvIssueStep, Issue: true,
		Params: []ParamDef{{Name: "from", Label: "From", Type: PText}, {Name: "to", Label: "To", Type: PText}},
		Match: func(tr Trigger, e *Event) bool {
			return (tr.From == "" || strings.EqualFold(tr.From, e.Old)) && (tr.To == "" || strings.EqualFold(tr.To, e.New))
		}})

	registerTrigger(TriggerDef{Name: "count_crossed", Label: "A count crosses a limit", Group: "Counts", Phrase: "the number of issues matching {query} is {op} {n}",
		Event: EvCountCrossed, Timed: true,
		Doc: "Checked half a minute after issues change and every five minutes; fires when the count crosses the limit, not while it stays there.",
		Params: []ParamDef{
			{Name: "query", Label: "Issues matching", Type: PQuery, Required: true, Placeholder: "is:open type:bug priority:>=high"},
			{Name: "op", Label: "Is", Type: PSelect, Required: true, Default: ">=", Options: opts(">=", "at least", "<=", "at most")},
			{Name: "n", Label: "Number", Type: PNumber, Required: true, Default: 10, Min: f64(0), Max: f64(1_000_000)},
		},
		Provides: []OutputDef{{Name: "Trigger.Data.count", Type: "number", Doc: "the count now"}, {Name: "Trigger.Data.n", Type: "number"}, {Name: "Trigger.Data.query", Type: "text"}}})

	registerTrigger(TriggerDef{Name: "pr_updated", Label: "Pull request opened or pushed to", Group: "Pull requests", Event: EvPRUpdated, PR: true,
		Phrase: "a pull request is opened, pushed to, marked ready or reopened",
		Params: []ParamDef{{Name: "on", Label: "Changes", Type: PSelect, Advanced: true, Default: "",
			Options: opts("", "Opened, pushed to, marked ready or reopened", "new", "Opened, marked ready or reopened, not pushes"),
			Help:    "Not pushes: once per pull request rather than once per commit, for a reviewer that is asked once and limits how often it reviews."}},
		Match: func(tr Trigger, e *Event) bool { return tr.str("on") != "new" || e.New != "pushed" }})
	registerTrigger(TriggerDef{Name: "pr_command", Label: "Review asked for", Group: "Pull requests", Event: EvPRCommand, PR: true,
		Phrase: "someone comments “@waypoint {command}” on a pull request",
		Params: []ParamDef{{Name: "command", Label: "Command", Type: PText, Placeholder: "review", Help: "Empty is any command."}},
		Match: func(tr Trigger, e *Event) bool {
			return tr.Command == "" || strings.EqualFold(tr.Command, e.New) || (strings.EqualFold(tr.Command, "review") && strings.EqualFold(e.New, "full review"))
		}})
	registerTrigger(TriggerDef{Name: "pr_comment", Label: "Reply to a bot", Group: "Pull requests", Event: EvPRComment, PR: true,
		Phrase: "a person replies in a bot's review thread"})
	registerTrigger(TriggerDef{Name: "pr_labeled", Label: "Pull request labelled", Group: "Pull requests", Event: EvPRLabeled, PR: true,
		Phrase:   "a pull request is labelled {label}",
		Params:   []ParamDef{{Name: "label", Label: "Label", Type: PText, Placeholder: "any label", Help: "Empty is any label."}},
		Provides: []OutputDef{{Name: "Trigger.Data.label", Type: "text", Doc: "the label added"}},
		Match: func(tr Trigger, e *Event) bool {
			l := strings.TrimSpace(tr.str("label"))
			return l == "" || strings.EqualFold(l, dataStr(e, "label"))
		}})
	registerTrigger(TriggerDef{Name: "review_requested", Label: "Review requested", Group: "Pull requests", Event: EvPRReviewRequested, PR: true,
		Phrase: "{who} is asked to review a pull request",
		Doc:    "Fires when the forge says someone was asked to review: GitHub and Forgejo name the reviewer, GitLab its reviewers. The bot account is the forge's bot token's account, else its token's.",
		Params: []ParamDef{
			{Name: "who", Label: "Who", Type: PSelect, Default: "bot", Options: opts("bot", "the bot account", "user", "this person", "any", "anyone")},
			{Name: "reviewer", Label: "Username", Type: PText, Needed: true, Placeholder: "ann", Help: "Their username on the forge.", When: &ParamWhen{Param: "who", In: []string{"user"}}},
		},
		Provides: []OutputDef{{Name: "Trigger.Data.reviewer", Type: "text", Doc: "who was asked"}, {Name: "Trigger.Data.bot", Type: "bool", Doc: "the bot account was asked"}},
		Match: func(tr Trigger, e *Event) bool {
			switch cmp(tr.str("who"), "bot") {
			case "any":
				return true
			case "user":
				r := strings.TrimSpace(tr.str("reviewer"))
				return r != "" && strings.EqualFold(r, dataStr(e, "reviewer"))
			}
			return anyBool(e.Data["bot"])
		}})
	registerTrigger(TriggerDef{Name: "pr_opened", Label: "Linked PR opened", Group: "Pull requests", Phrase: "a linked pull request opens", Event: EvExternalPROpened, Issue: true})
	registerTrigger(TriggerDef{Name: "pr_merged", Label: "Linked PR merged", Group: "Pull requests", Phrase: "a linked pull request is merged", Event: EvExternalPRMerged, Issue: true})
	registerTrigger(TriggerDef{Name: "pr_closed", Label: "Linked PR closed", Group: "Pull requests", Phrase: "a linked pull request is closed", Event: EvExternalPRClosed, Issue: true})
	registerTrigger(TriggerDef{Name: "pr_review", Label: "Review submitted", Group: "Pull requests", Phrase: "a review is submitted on a pull request", Event: EvPRReview, PR: true,
		Params: []ParamDef{{Name: "verdict", Label: "Verdict", Type: PSelect, Options: opts("", "Any", "approved", "Approved", "changes_requested", "Changes requested", "commented", "Commented")}},
		Match: func(tr Trigger, e *Event) bool {
			v := tr.str("verdict")
			return v == "" || dataStr(e, "verdict") == v
		}})
	eventTrigger(TriggerDef{Name: "pr_ci_passed", Label: "CI passed", Group: "Pull requests", Phrase: "CI passes on a pull request", Event: EvPRCI, PR: true}, map[string]string{"state": "passed"})
	eventTrigger(TriggerDef{Name: "pr_ci_failed", Label: "CI failed", Group: "Pull requests", Phrase: "CI fails on a pull request", Event: EvPRCI, PR: true,
		Provides: []OutputDef{{Name: "Trigger.Data.failed", Type: "list", Doc: "names of the failed checks"}}}, map[string]string{"state": "failed"})
	registerTrigger(TriggerDef{Name: "pr_review_gate", Label: "Review gate changes", Group: "Pull requests", Phrase: "a pull request's review gate becomes {state}", Event: EvPRReviewGate, PR: true,
		Params: []ParamDef{{Name: "state", Label: "State", Type: PSelect, Options: opts("", "Any", "success", "Green", "failure", "Red", "pending", "Pending")}},
		Match: func(tr Trigger, e *Event) bool {
			v := tr.str("state")
			return v == "" || dataStr(e, "state") == v
		}})

	registerTrigger(TriggerDef{Name: "repo_pushed", Label: "Pushed to a branch", Group: "Code", Phrase: "someone pushes to {branch}", Event: EvRepoPushed, Params: []ParamDef{branchParam},
		Match: func(tr Trigger, e *Event) bool { return matchGlob(tr.str("branch"), dataStr(e, "branch")) }})
	registerTrigger(TriggerDef{Name: "files_changed", Label: "Files changed", Group: "Code", Phrase: "a push changes files matching {paths}", Event: EvRepoPushed,
		Params: []ParamDef{{Name: "paths", Label: "Paths", Type: PGlobs, Required: true, Placeholder: "docs/**\nweb/src/**", Help: "One pattern per line."}, branchParam},
		Match: func(tr Trigger, e *Event) bool {
			if !matchGlob(tr.str("branch"), dataStr(e, "branch")) {
				return false
			}
			gs, err := newGlobSet(anyStrings(tr.Extra["paths"]))
			if err != nil {
				return false
			}
			return slices.ContainsFunc(anyStrings(e.Data["paths"]), gs.match)
		}})
	registerTrigger(TriggerDef{Name: "branch_created", Label: "Branch created", Group: "Code", Phrase: "a branch is created", Event: EvRepoPushed, Params: []ParamDef{branchParam},
		Match: func(tr Trigger, e *Event) bool {
			return anyBool(e.Data["created"]) && matchGlob(tr.str("branch"), dataStr(e, "branch"))
		}})
	registerTrigger(TriggerDef{Name: "release_published", Label: "Tag or release published", Group: "Code", Phrase: "a release is published", Event: EvRepoRelease})
	registerTrigger(TriggerDef{Name: "linked_issue_closed", Label: "Linked forge issue closed", Group: "Code", Phrase: "a linked forge issue is closed", Event: EvExternalIssueClose, Issue: true})

	registerTrigger(TriggerDef{Name: "env_deployed", Label: "Deployed", Group: "Deployments", Phrase: "something is deployed to {environment}", Event: EvEnvDeployed, Params: []ParamDef{envParam},
		Match: func(tr Trigger, e *Event) bool { return matchGlob(tr.str("environment"), dataStr(e, "name")) }})
	registerTrigger(TriggerDef{Name: "env_deploy_failed", Label: "Deploy failed", Group: "Deployments", Phrase: "a deploy to {environment} fails", Event: EvEnvDeployFailed, Params: []ParamDef{envParam},
		Match: func(tr Trigger, e *Event) bool { return matchGlob(tr.str("environment"), dataStr(e, "name")) }})
	registerTrigger(TriggerDef{Name: "env_down", Label: "Environment down", Group: "Deployments", Phrase: "{environment} goes down", Event: EvEnvStatus, Params: []ParamDef{envParam},
		Match: func(tr Trigger, e *Event) bool {
			return dataStr(e, "status") == "down" && matchGlob(tr.str("environment"), dataStr(e, "name"))
		}})
	registerTrigger(TriggerDef{Name: "env_up", Label: "Environment up", Group: "Deployments", Phrase: "{environment} comes back up", Event: EvEnvStatus, Params: []ParamDef{envParam},
		Match: func(tr Trigger, e *Event) bool {
			return dataStr(e, "status") == "up" && matchGlob(tr.str("environment"), dataStr(e, "name"))
		}})
	registerTrigger(TriggerDef{Name: "env_lagging", Label: "Not deployed in time", Group: "Deployments", Phrase: "{environment} still runs an older commit than its branch past its lag limit", Event: EvEnvLagging, Params: []ParamDef{envParam},
		Match: func(tr Trigger, e *Event) bool { return matchGlob(tr.str("environment"), dataStr(e, "name")) }})
	registerTrigger(TriggerDef{Name: "env_created", Label: "Environment created", Group: "Deployments", Phrase: "an environment is created", Event: EvEnvCreated})
	registerTrigger(TriggerDef{Name: "env_stopped", Label: "Environment stopped", Group: "Deployments", Phrase: "an environment is stopped", Event: EvEnvStopped})

	registerTrigger(TriggerDef{Name: "session_started", Label: "Agent session started", Group: "Agents", Phrase: "an agent session starts", Event: EvSessionStarted})
	registerTrigger(TriggerDef{Name: "session_ended", Label: "Agent session ended", Group: "Agents", Phrase: "an agent session ends", Event: EvSessionEnded})
	registerTrigger(TriggerDef{Name: followUpTrigger, Label: "An agent finishes", Group: "Agents", Phrase: "an agent finishes", Event: EvSessionFinished,
		Doc:      "The agent truly finished: no background task, cron or wakeup left that can wake it, and no open request (WP-D22).",
		Provides: []OutputDef{{Name: "Trigger.Data.session", Type: "text"}, {Name: "Trigger.Data.machine", Type: "text"}, {Name: "Trigger.Data.repo", Type: "text"}, {Name: "Trigger.Data.branch", Type: "text"}}})
	registerTrigger(TriggerDef{Name: "session_waiting", Label: "Agent waiting for you", Group: "Agents", Phrase: "an agent is waiting for you", Event: EvSessionWaiting,
		Provides: []OutputDef{{Name: "Trigger.Data.reason", Type: "text", Doc: "idle_prompt, permission_prompt …"}, {Name: "Trigger.Data.session", Type: "text"}}})
	requestKinds := []ParamOption{{Value: "", Label: "Any"}, {Value: "question", Label: "A question"}, {Value: "change", Label: "A change"}, {Value: "command", Label: "A command"},
		{Value: "permission", Label: "A permission"}, {Value: "action", Label: "An action"}}
	kindMatch := func(tr Trigger, e *Event) bool {
		k := tr.str("kind")
		return k == "" || dataStr(e, "kind") == k
	}
	registerTrigger(TriggerDef{Name: "request_created", Label: "Request created", Group: "Agents", Phrase: "an agent makes a request ({kind})", Event: EvRequestCreated,
		Params: []ParamDef{{Name: "kind", Label: "Kind", Type: PSelect, Options: requestKinds}}, Match: kindMatch})
	registerTrigger(TriggerDef{Name: "request_decided", Label: "Request decided", Group: "Agents", Phrase: "a request is decided", Event: EvRequestUpdated,
		Params: []ParamDef{{Name: "kind", Label: "Kind", Type: PSelect, Options: requestKinds}},
		Match:  func(tr Trigger, e *Event) bool { return dataStr(e, "decision") != "" && kindMatch(tr, e) }})
	registerTrigger(TriggerDef{Name: "request_waiting", Label: "Request waiting too long", Group: "Agents", Phrase: "a request has waited more than {after}", Event: EvRequestCreated,
		Doc:    "Starts when a request is created; runs if nobody has decided it after the time.",
		Params: []ParamDef{{Name: "after", Label: "Waits more than", Type: PDuration, Required: true, Default: "15m", Min: f64(60), Max: f64(30 * 24 * 3600)}, {Name: "kind", Label: "Kind", Type: PSelect, Options: requestKinds}},
		Match:  kindMatch,
		Watch: &WatchDef{Until: EvRequestUpdated, After: "after", Match: "decision!=", Key: func(e *Event) string {
			return "request:" + cmp(dataStr(e, "request"), dataStr(e, "code"))
		}}})

	registerTrigger(TriggerDef{Name: "sprint_created", Label: "Sprint created", Group: "Sprints", Phrase: "a sprint is created", Event: EvSprintCreated})
	registerTrigger(TriggerDef{Name: "sprint_started", Label: "Sprint started", Group: "Sprints", Phrase: "a sprint starts", Event: EvSprintStarted})
	registerTrigger(TriggerDef{Name: "sprint_closed", Label: "Sprint closed", Group: "Sprints", Phrase: "a sprint is closed", Event: EvSprintClosed})

	registerTrigger(TriggerDef{Name: "glossary_changed", Label: "Glossary term added or changed", Group: "Knowledge", Phrase: "a glossary term is {change}", Event: EvGlossaryChanged,
		Params: []ParamDef{{Name: "change", Label: "Change", Type: PSelect, Options: opts("", "Added or changed", "added", "Added", "changed", "Changed")}},
		Match: func(tr Trigger, e *Event) bool {
			switch tr.str("change") {
			case "added":
				return anyBool(e.Data["created"])
			case "changed":
				return !anyBool(e.Data["created"])
			}
			return true
		}})
	registerTrigger(TriggerDef{Name: "design_changed", Label: "Design note proposed or approved", Group: "Knowledge", Phrase: "a design note is {state}", Event: EvDesignChanged,
		Params: []ParamDef{{Name: "state", Label: "State", Type: PSelect, Options: opts("", "Any", "proposed", "Proposed", "approved", "Approved", "requested", "Written for a person's task")}},
		Match: func(tr Trigger, e *Event) bool {
			s := tr.str("state")
			return s == "" || dataStr(e, "state") == s
		}})
	registerTrigger(TriggerDef{Name: "page_created", Label: "Page created", Group: "Knowledge", Phrase: "a page is created", Event: EvPageCreated})
	registerTrigger(TriggerDef{Name: "page_updated", Label: "Page updated", Group: "Knowledge", Phrase: "a page is updated", Event: EvPageUpdated})

	registerTrigger(TriggerDef{Name: "project_created", Label: "Project created", Group: "Projects", Phrase: "a project is created", Event: EvProjectCreated, Admin: true})
	registerTrigger(TriggerDef{Name: "repo_attached", Label: "Repo attached", Group: "Projects", Phrase: "a repo is attached to a project", Event: EvRepoAttached})

	registerTrigger(TriggerDef{Name: "timer_started", Label: "Timer started", Group: "Timers", Phrase: "a timer is started", Event: EvTimerStarted, Issue: true})
	registerTrigger(TriggerDef{Name: "timer_stopped", Label: "Timer stopped", Group: "Timers", Phrase: "a timer is stopped", Event: EvTimerStopped, Issue: true})
	registerTrigger(TriggerDef{Name: "timer_paused", Label: "Timer paused", Group: "Timers", Phrase: "an agent's timer is paused", Event: EvTimerPaused, Issue: true})

	registerTrigger(TriggerDef{Name: "automation_finished", Label: "An automation's run finished", Group: "Automations", Phrase: "an automation's run finishes ({status})", Event: EvAutomationRunFinished,
		Params: []ParamDef{
			{Name: "automation", Label: "Automation", Type: PText, Help: "Its name or id; empty is any."},
			{Name: "status", Label: "Result", Type: PSelect, Options: opts("", "Any", "succeeded", "Succeeded", "failed", "Failed", "cancelled", "Cancelled")},
		},
		Provides: []OutputDef{{Name: "Trigger.Data.run", Type: "number"}, {Name: "Trigger.Data.name", Type: "text"}, {Name: "Trigger.Data.error", Type: "text"}},
		Match: func(tr Trigger, e *Event) bool {
			if st := tr.str("status"); st != "" && dataStr(e, "status") != st {
				return false
			}
			if st := dataStr(e, "status"); st != RunSucceeded && st != RunFailed && st != RunCancelled {
				return false
			}
			if want := tr.str("automation"); want != "" && !strings.EqualFold(want, dataStr(e, "name")) && want != dataStr(e, "automation") {
				return false
			}
			return true
		}})

	registerTrigger(TriggerDef{Name: "due_soon", Label: "Due date is near", Group: "Time", Phrase: "an issue is due", Event: EvDueSoon, Issue: true, Timed: true,
		Params: []ParamDef{{Name: "days", Label: "Days before", Type: PNumber, Default: 0, Min: f64(0), Max: f64(60), Help: "0 fires on the day."}},
		Check: func(tr *Trigger) error {
			if tr.Days < 0 || tr.Days > 60 {
				return invalid("due_soon needs days between 0 and 60")
			}
			return nil
		}})
	registerTrigger(TriggerDef{Name: "stale", Label: "Issue goes quiet", Group: "Time", Phrase: "an open issue goes quiet", Event: EvStale, Issue: true, Timed: true,
		Params: []ParamDef{{Name: "days", Label: "Days without changes", Type: PNumber, Default: 30, Min: f64(1), Max: f64(365)}},
		Check: func(tr *Trigger) error {
			if tr.Days < 1 || tr.Days > 365 {
				return invalid("stale needs days between 1 and 365")
			}
			return nil
		}})
	registerTrigger(TriggerDef{Name: "schedule", Label: "On a schedule", Group: "Time", Phrase: "on a schedule", Event: EvScheduled, Issue: true, Timed: true,
		Doc: "Every day, weekday, week or month at a time, or a cron expression, in the automation's time zone.",
		Params: []ParamDef{
			{Name: "every", Label: "Every", Type: PSelect, Required: true, Default: "day", Options: opts("day", "Day", "weekday", "Weekday", "week", "Week", "month", "Month", "cron", "Cron expression")},
			{Name: "at", Label: "At", Type: PTime, Default: "09:00", When: &ParamWhen{Param: "every", In: []string{"day", "weekday", "week", "month"}}},
			{Name: "weekday", Label: "On", Type: PWeekday, Default: 1, When: &ParamWhen{Param: "every", In: []string{"week"}}},
			{Name: "month_day", Label: "Day of the month", Type: PNumber, Default: 1, Min: f64(1), Max: f64(28), When: &ParamWhen{Param: "every", In: []string{"month"}}},
			{Name: "cron", Label: "Cron", Type: PCron, Required: true, Placeholder: "0 9 * * 1-5", Help: "minute hour day month weekday", When: &ParamWhen{Param: "every", In: []string{"cron"}}},
		},
		Check: func(tr *Trigger) error {
			switch tr.Every {
			case "day", "weekday":
			case "week":
				if tr.Weekday < 0 || tr.Weekday > 6 {
					return invalid("weekday must be 0 (Sunday) to 6")
				}
			case "month":
				if tr.MonthDay < 1 || tr.MonthDay > 28 {
					return invalid("month_day must be 1 to 28")
				}
			case "cron":
				return nil
			default:
				return invalid("every must be day, weekday, week, month or cron")
			}
			if !atRE.MatchString(tr.At) {
				return invalid("at must be a time like 09:00")
			}
			return nil
		}})

	registerTrigger(TriggerDef{Name: "manual", Label: "Run now", Group: "Manual and incoming", Phrase: "I press Run now", Event: EvManual,
		Doc:      "A button on the automation, wp automation run NAME, or POST /api/v1/automations/{id}/run. Add inputs to ask for values.",
		Provides: []OutputDef{{Name: "Inputs.<name>", Type: "text", Doc: "what the run was given"}}})
	registerTrigger(TriggerDef{Name: "webhook", Label: "Incoming webhook", Group: "Manual and incoming", Phrase: "a request arrives at the webhook address", Event: EvWebhookIn,
		Doc:      "A secret address per automation; POST JSON to it. The body is .Trigger.Data.",
		Params:   []ParamDef{{Name: "secret", Label: "Signing secret", Type: PSecret, Secret: true, Help: "Optional. When set, requests must carry X-Hub-Signature-256: sha256=HMAC of the body."}},
		Provides: []OutputDef{{Name: "Trigger.Data", Type: "object", Doc: "the JSON body"}}})

	registerTrigger(TriggerDef{Name: "system_resources", Label: "Disk, database or memory low", Group: "System", Phrase: "a system resource ({resource}) runs low", Event: EvSystemResources, Admin: true,
		Params: []ParamDef{{Name: "resource", Label: "Resource", Type: PSelect, Options: opts("", "Any", "disk", "Disk space", "database", "Database size", "memory", "Memory", "transcripts", "Transcript archive")}},
		Match: func(tr Trigger, e *Event) bool {
			r := tr.str("resource")
			return r == "" || dataStr(e, "resource") == r
		},
		Provides: []OutputDef{{Name: "Trigger.Data.resource", Type: "text"}, {Name: "Trigger.Data.free", Type: "number", Doc: "bytes"}, {Name: "Trigger.Data.total", Type: "number", Doc: "bytes"}, {Name: "Trigger.Data.text", Type: "text", Doc: "a sentence about it"}}})
	registerTrigger(TriggerDef{Name: "runner_offline", Label: "A runner goes offline", Group: "System", Phrase: "a runner goes offline", Event: EvRunnerStatus, Admin: true,
		Match: func(tr Trigger, e *Event) bool { return e.Data["online"] == false || dataStr(e, "online") == "false" }})
	registerTrigger(TriggerDef{Name: "backup_failed", Label: "A backup failed", Group: "System", Phrase: "the daily backup fails", Event: EvBackupFailed, Admin: true,
		Provides: []OutputDef{{Name: "Trigger.Data.error", Type: "text"}}})

	registerActions()
}
