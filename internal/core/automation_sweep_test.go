package core

import (
	"strings"
	"sync"
	"testing"
)

// Findings of the free-model sweep over the phase 1 automations code, each
// checked by hand and fixed where it was real.

func TestSkipPolicyHoldsUnderConcurrentStarts(t *testing.T) {
	registerTestActions()
	e := newEnv(t)
	p := e.project(e.admin, "Race", "")
	a := e.addAuto(p, "Race", Rule{Trigger: Trigger{Event: "manual"}, Concurrency: "skip", Actions: []Action{
		{Type: "delay", Extra: map[string]any{"duration": "10m"}}}})
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.s.RunAutomationNow(e.ctx, e.admin, a.ID, nil, "manual"); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	live, skipped := 0, 0
	for _, r := range e.runs(a) {
		switch r.Status {
		case RunSkipped:
			skipped++
		case RunQueued, RunRunning, RunWaiting:
			live++
		}
	}
	if live != 1 || skipped != 15 {
		t.Errorf("%d runs got in and %d were skipped, want 1 and 15", live, skipped)
	}
}

func TestQueuedRunsDoNotFinishWaits(t *testing.T) {
	registerTestActions()
	e := newEnv(t)
	p := e.project(e.admin, "Waits", "")
	a := e.addAuto(p, "Waits", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "wait_for", ID: "w", Extra: map[string]any{"event": EvAutomationRunFinished, "timeout": "1h"}}}})
	run := e.manual(a, nil)
	e.s.DrainAutomationRuns(e.ctx)
	if got := e.run(run.ID).Status; got != RunWaiting {
		t.Fatalf("status = %s", got)
	}
	for _, st := range []string{RunQueued, RunWaiting, RunSkipped} {
		e.s.matchWaits(e.ctx, &Event{Kind: EvAutomationRunFinished, ProjectID: p.ID, Data: map[string]any{"status": st}})
	}
	if got := e.run(run.ID).Status; got != RunWaiting {
		t.Fatalf("a run that was only queued resolved the wait: %s", got)
	}
	e.s.matchWaits(e.ctx, &Event{Kind: EvAutomationRunFinished, ProjectID: p.ID, Data: map[string]any{"status": RunSucceeded}})
	e.s.DrainAutomationRuns(e.ctx)
	if got := e.run(run.ID).Status; got != RunSucceeded {
		t.Errorf("a finished run didn't: %s", got)
	}
}

func TestCancelAnnouncesOnceAndRerunNeedsAnEnabledAutomation(t *testing.T) {
	registerTestActions()
	e := newEnv(t)
	p := e.project(e.admin, "Cancel", "")
	slow := e.addAuto(p, "Slow", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "delay", Extra: map[string]any{"duration": "10m"}}}})
	watcher := e.addAuto(p, "Watcher", Rule{Trigger: Trigger{Event: "automation_finished", Extra: map[string]any{"automation": "Slow"}},
		Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Trigger.Data.status}}"}}})
	run := e.manual(slow, nil)
	e.s.DrainAutomationRuns(e.ctx)
	for range 2 {
		if _, err := e.s.CancelAutomationRun(e.ctx, e.admin, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.runs(watcher)); n != 1 {
		t.Errorf("a cancelled run was announced %d times", n)
	}

	// A rule that fires on an event can't be rerun while it is off.
	reacts := e.addAuto(p, "Reacts", Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "add_label", Value: "x"}}})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "one"})
	runs := e.runs(reacts)
	if len(runs) != 1 {
		t.Fatalf("%d runs", len(runs))
	}
	off := false
	if _, err := e.s.UpdateAutomation(e.ctx, e.admin, reacts.ID, AutomationInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RerunAutomation(e.ctx, e.admin, runs[0].ID); err == nil || !strings.Contains(err.Error(), "turn the automation on") {
		t.Errorf("rerun of a disabled automation: %v", err)
	}
}

func TestDescribeStepNeedsNoPhrase(t *testing.T) {
	if got := describeStep(&ActionDef{Name: "bare"}, nil); got == "" {
		t.Error("an action with no phrase or label described as nothing")
	}
	if got := describeStep(&ActionDef{Name: "x", Phrase: "élan {a}"}, map[string]any{"a": "b"}); got != "Élan b" {
		t.Errorf("multi-byte first letter: %q", got)
	}
}

func TestCronNamesStayInTheirFields(t *testing.T) {
	for _, ok := range []string{"0 9 * jan-mar mon", "0 9 * * sun,sat", "0 9 * DEC FRI"} {
		if _, err := ParseCron(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"0 9 * sun *", "0 9 * * jan", "0 9 mon * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestOwnersRecipientsSkipDisabledUsers(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Owners", "")
	gone := e.user("gone", false)
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "gone", "owner"); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := e.s.UpdateUser(e.ctx, e.admin, gone.User.ID, UserPatch{Disabled: &on}); err != nil {
		t.Fatal(err)
	}
	a := e.addAuto(p, "Tell owners", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "notify", User: "owners", Title: "hello", Message: "m"}}})
	e.manual(a, nil)
	ns, _, _ := e.s.ListNotifications(e.ctx, gone, false, 5)
	if len(ns) != 0 {
		t.Errorf("a disabled owner was notified: %+v", ns)
	}
	ns, _, _ = e.s.ListNotifications(e.ctx, e.admin, false, 5)
	if len(ns) != 1 {
		t.Errorf("the owner wasn't: %+v", ns)
	}
}

func TestPRCommandMatchesWhateverTheCase(t *testing.T) {
	d := triggerDef("pr_command")
	ev := &Event{Kind: EvPRCommand, New: "Full Review"}
	if !d.Match(Trigger{Command: "review"}, ev) || d.Match(Trigger{Command: "merge"}, ev) {
		t.Error("full review should match a review trigger, and nothing else")
	}
}

func TestTestRunSaysWhenItsInputsAreWrong(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Dry", "")
	a := e.addAuto(p, "Dry", Rule{Trigger: Trigger{Event: "manual"}, Inputs: []RuleInput{{Name: "days", Type: "number", Default: "7"}},
		Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Inputs.days}}"}}})
	if _, err := e.s.TestAutomation(e.ctx, e.admin, a.ID, TestInput{Inputs: map[string]any{"days": "many"}}); err == nil || !strings.Contains(err.Error(), "must be a number") {
		t.Errorf("bad input: %v", err)
	}
	if _, err := e.s.TestAutomation(e.ctx, e.admin, a.ID, TestInput{}); err != nil {
		t.Errorf("no inputs tests with the defaults: %v", err)
	}
}
