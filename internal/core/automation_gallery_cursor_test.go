package core

import (
	"strings"
	"testing"
	"time"
)

// The daily Grok audit (WP-305, through the audit step since WP-348): Grok
// reads the repository on the saver's runner, read-only; what it finds is
// kept as an audit and becomes a task for Claude that counts as Grok's
// unreviewed text; a spent Cursor pool ends the day quietly and any other
// failure is told.
func TestDailyGrokAuditBecomesATaskForClaude(t *testing.T) {
	ae := newAgentEnv(t)
	card := ae.addAuto(ae.p, "Grok", templateRule(t, "Daily Grok audit, checked by Claude"))
	day := func(d int) *AutomationRun {
		t.Helper()
		ae.setClock(time.Date(2026, 11, d, 12, 31, 0, 0, time.UTC))
		ae.runnerSeen(ae.box, true) // it kept polling
		ae.s.RunTimedAutomations(ae.ctx)
		ae.work()
		runs := ae.env.runs(card)
		return runs[0]
	}

	// Monday: security.
	run := day(2)
	claim := ae.take()
	in := agentJobOf(t, claim)
	if claim.Kind != JobAudit || in.CLI != "cursor" || in.Model != "grok-4.7-high" || in.Permission != AgentRead || in.Branch != "" || in.JSONSchema == "" ||
		!strings.Contains(in.Prompt, "Focus: security: missing or wrong access checks") {
		t.Fatalf("audit job = %s %+v", claim.Kind, in)
	}
	ae.finish(claim.ID, "done", auditResult("One hole.", map[string]any{"title": "The export handler skips the project check", "path": "internal/api/export.go",
		"line": 42, "severity": "high", "problem": "Anyone exports any board."}), "")
	ae.work()
	got := ae.runStatus(run)
	step := stepByID(got, "audit")
	key, _ := step.Outputs["FixIssue"].(string)
	if got.Status != RunSucceeded || key == "" || step.Outputs["Findings"] != float64(1) && step.Outputs["Findings"] != 1 {
		t.Fatalf("run = %s (%s), audit step %+v", got.Status, got.Error, step)
	}
	task, err := ae.s.GetIssue(ae.ctx, ae.admin, key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(task.Title, "Check audit #") || task.Agent != "claude" || task.TextAgent != "cursor/grok-4.7-high" || !task.Unreviewed ||
		PriorityName(task.Priority) != "low" || len(task.Labels) != 1 || task.Labels[0].Name != "audit" {
		t.Errorf("task = %q agent %q text %q unreviewed %v priority %d labels %+v", task.Title, task.Agent, task.TextAgent, task.Unreviewed, task.Priority, task.Labels)
	}
	// Grok's text waits for the safety screen before an agent may take it.
	if task.Safety == nil || task.Safety.Verdict != "pending" {
		t.Errorf("safety = %+v", task.Safety)
	}
	if !strings.Contains(task.Description, "<untrusted>") || !strings.Contains(task.Description, "another model's claims") ||
		!strings.Contains(task.Description, "The export handler skips the project check") {
		t.Errorf("description = %s", task.Description)
	}
	if ns, _, _ := ae.s.ListNotifications(ae.ctx, ae.admin, false, 5); len(ns) != 0 {
		t.Errorf("findings handed to Claude needn't be told: %+v", ns)
	}

	// Tuesday: nothing found, so no task.
	run = day(3)
	claim = ae.take()
	if in := agentJobOf(t, claim); !strings.Contains(in.Prompt, "Focus: correctness") {
		t.Fatalf("Tuesday's focus: %s", in.Prompt)
	}
	ae.finish(claim.ID, "done", auditResult("Nothing.", nil...), "")
	ae.work()
	if got := ae.runStatus(run); got.Status != RunSucceeded || stepByID(got, "audit").Outputs["FixIssue"] != "" {
		t.Fatalf("Tuesday: %s, audit step %+v", got.Status, stepByID(got, "audit"))
	}

	// Wednesday: the pool is spent. Both entries of the chain say so, and
	// nobody is told.
	run = day(4)
	for range 2 {
		claim = ae.take()
		ae.finish(claim.ID, "failed", map[string]any{}, "cursor-agent reported an error: You've hit your usage limit for Cursor models")
		ae.work()
	}
	got = ae.runStatus(run)
	if a := stepByID(got, "audit"); a.Status != "failed" || a.Outputs["Limited"] != true {
		t.Fatalf("Wednesday's audit = %+v", a)
	}
	if ns, _, _ := ae.s.ListNotifications(ae.ctx, ae.admin, false, 5); len(ns) != 0 {
		t.Errorf("a spent pool shouldn't tell anyone: %+v", ns)
	}

	// Thursday: another failure is told.
	day(5)
	claim = ae.take()
	ae.finish(claim.ID, "failed", map[string]any{}, "can't get the repository: authentication failed")
	ae.work()
	ns, _, _ := ae.s.ListNotifications(ae.ctx, ae.admin, false, 5)
	if len(ns) == 0 || ns[0].Kind != NotifyAudit || !strings.Contains(ns[0].Body, "can't get the repository") {
		t.Errorf("notification = %+v", ns)
	}
}

// Asked by hand, Grok audits a branch for what it is told to look for, and
// nothing goes to Claude unless asked.
func TestAskGrokToAuditABranch(t *testing.T) {
	ae := newAgentEnv(t)
	card := ae.addAuto(ae.p, "Ask Grok", templateRule(t, "Ask Grok to audit"))
	run, err := ae.s.RunAutomationNow(ae.ctx, ae.admin, card.ID, map[string]any{"focus": "races in the merge queue", "branch": "wp-222-queue"}, "")
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	in := agentJobOf(t, claim)
	if in.Base != "wp-222-queue" || in.DiffFrom != "main" || in.Permission != AgentRead || !strings.Contains(in.Prompt, "What to look for: races in the merge queue") {
		t.Fatalf("audit job = %+v", in)
	}
	ae.finish(claim.ID, "done", auditResult("", map[string]any{"title": "Two merges can take the same slot", "severity": "high", "problem": "x"}), "")
	ae.work()
	got := ae.runStatus(run)
	step := stepByID(got, "audit")
	if got.Status != RunSucceeded || step.Outputs["FixIssue"] != "" {
		t.Fatalf("run %s (%s): %+v", got.Status, got.Error, step)
	}
	id, _ := step.Outputs["AuditID"].(float64)
	a, err := ae.s.GetAudit(ae.ctx, ae.admin, int64(id))
	if err != nil || a.Title != "me/app branch wp-222-queue (against main)" || a.Counts.Total != 1 || a.Automation != "Ask Grok" {
		t.Fatalf("audit = %+v %v", a, err)
	}
	// Told, since nobody else will act on them.
	if ns, _, _ := ae.s.ListNotifications(ae.ctx, ae.admin, false, 5); len(ns) == 0 || ns[0].Kind != NotifyAudit {
		t.Errorf("notifications = %+v", ns)
	}
}

// create_issue's written_by makes the issue an agent's unreviewed text, and
// agent delegates it.
func TestCreateIssueWrittenByAnAgent(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Written", "")
	a := e.addAuto(p, "File", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "create_issue", ID: "mine", Title: "Mine"},
		{Type: "create_issue", ID: "theirs", Title: "Theirs", Extra: map[string]any{"written_by": "cursor grok-4.7", "agent": "claude", "issue_priority": "high"}},
	}})
	run, err := e.s.RunAutomationNow(e.ctx, e.admin, a.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	run = e.run(run.ID)
	get := func(step string) *IssueDetail {
		t.Helper()
		key, _ := stepByID(run, step).Outputs["Key"].(string)
		i, err := e.s.GetIssue(e.ctx, e.admin, key)
		if err != nil {
			t.Fatalf("%s: %v (%+v)", step, err, run.Steps)
		}
		return i
	}
	if i := get("mine"); i.TextAgent != "" || i.Unreviewed {
		t.Errorf("the saver's own issue = text %q unreviewed %v", i.TextAgent, i.Unreviewed)
	}
	if i := get("theirs"); i.TextAgent != "cursor/grok-4.7" || !i.Unreviewed || i.Agent != "claude" || PriorityName(i.Priority) != "high" {
		t.Errorf("the agent's issue = text %q unreviewed %v agent %q priority %d", i.TextAgent, i.Unreviewed, i.Agent, i.Priority)
	}
}

// cursor-agent refuses a model it doesn't offer with "Cannot use this
// model: …" and exit 1; the chain tries its next model.
func TestCursorRefusedModelIsModelFailure(t *testing.T) {
	j := &stepJob{State: "failed", Error: "exit status 1", Tail: "Cannot use this model: grok-4.7[effort=high,fast=false]. Available models: auto, grok-4.7-high"}
	if !modelFailure(j) {
		t.Fatal("a refused model should let the chain try the next one")
	}
}
