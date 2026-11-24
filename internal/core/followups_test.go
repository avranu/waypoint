package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func running(desc string) *HookPending {
	return &HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running", Description: desc}}, Crons: []PendingCron{}}
}

func nothingPending() *HookPending {
	return &HookPending{Tasks: []PendingTask{}, Crons: []PendingCron{}}
}

// fuEnv is an admin's agent session on the machine "box", in /work/app.
type fuEnv struct {
	*env
	ag  *Actor
	sid int64
}

func newFUEnv(t *testing.T) *fuEnv {
	e := newEnv(t)
	f := &fuEnv{env: e, ag: agentActor(e.admin, "anthropic/claude-code")}
	res := f.hook("SessionStart", func(h *HookEvent) { h.Source = "startup" })
	f.sid = res.Session
	f.hook("UserPromptSubmit", func(h *HookEvent) { h.Prompt = "build the parser" })
	return f
}

func (f *fuEnv) hook(event string, mod func(*HookEvent)) *HookResult {
	f.t.Helper()
	f.setClock(f.clockNow().Add(time.Second))
	res, err := f.s.SessionHook(f.ctx, f.ag, []HookEvent{hook("box", "cli-1", event, mod)})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

// stop is the agent stopping with what it still runs (nil: its CLI didn't say).
func (f *fuEnv) stop(last string, p *HookPending) *HookResult {
	f.t.Helper()
	return f.hook("Stop", func(h *HookEvent) { h.LastMessage, h.Pending = last, p })
}

func (f *fuEnv) session() *AgentSession {
	f.t.Helper()
	x, err := f.s.sessionByID(f.ctx, f.s.db.R, f.admin, f.sid)
	if err != nil {
		f.t.Fatal(err)
	}
	return x
}

func (f *fuEnv) work() {
	for range 4 {
		f.s.Wait()
		f.s.DrainAutomationRuns(f.ctx)
	}
	f.s.Wait()
}

func (f *fuEnv) queue(steps ...Action) *FollowUp {
	f.t.Helper()
	fu, err := f.s.SaveFollowUp(f.ctx, f.admin, f.sid, FollowUpInput{Steps: steps})
	if err != nil {
		f.t.Fatal(err)
	}
	return fu
}

func (f *fuEnv) followUp() *FollowUp {
	f.t.Helper()
	fu, err := f.s.GetFollowUp(f.ctx, f.admin, f.sid)
	if err != nil {
		f.t.Fatal(err)
	}
	return fu
}

func step(typ string, extra map[string]any) Action { return Action{Type: typ, Extra: extra} }

func TestStopTellsWhetherTheAgentFinished(t *testing.T) {
	f := newFUEnv(t)
	sub := f.s.Hub.Subscribe(f.admin.User.ID)
	defer f.s.Hub.Unsubscribe(sub)
	finished := func() int {
		n := 0
		for {
			select {
			case e := <-sub.C:
				if e.Kind == EvSessionFinished {
					n++
				}
			default:
				return n
			}
		}
	}

	f.stop("started the tests", running("go test ./..."))
	x := f.session()
	if x.State != SessionWorking || x.Pending != "a background task" || x.FinishedAt != nil || finished() != 0 {
		t.Fatalf("with a background task: state %s pending %q finished %v", x.State, x.Pending, x.FinishedAt)
	}
	f.stop("checking back later", &HookPending{Crons: []PendingCron{{Schedule: "in 20m"}, {Schedule: "*/5 * * * *", Recurring: true}}})
	if x = f.session(); x.Pending != "a wakeup, a recurring cron" || x.FinishedAt != nil {
		t.Fatalf("with a wakeup: %q", x.Pending)
	}
	f.stop("done", &HookPending{Tasks: []PendingTask{{Type: "shell", Status: "completed"}}})
	if x = f.session(); x.FinishedAt == nil || x.Pending != "" || x.State != SessionIdle || finished() != 1 {
		t.Fatalf("finished: %+v", x)
	}
	f.hook("UserPromptSubmit", func(h *HookEvent) { h.Prompt = "more" })
	if x = f.session(); x.FinishedAt != nil {
		t.Fatal("still finished after a prompt")
	}
	f.stop("done", nil)
	if x = f.session(); x.Pending != "?" || x.FinishedAt != nil || finished() != 0 {
		t.Fatalf("unknown: pending %q finished %v", x.Pending, x.FinishedAt)
	}

	asker := *f.ag
	asker.Session = f.sid
	r, _, err := f.s.CreateRequest(f.ctx, &asker, RequestInput{Kind: "action", Title: "Set the deploy secret"})
	if err != nil {
		t.Fatal(err)
	}
	f.stop("waiting on you", nothingPending())
	if x = f.session(); x.Pending != "requests" || x.FinishedAt != nil || finished() != 0 {
		t.Fatalf("held by a request: %q %v", x.Pending, x.FinishedAt)
	}
	if _, err := f.s.DecideRequest(f.ctx, f.admin, r.Ref, RequestDecision{Decision: "done"}); err != nil {
		t.Fatal(err)
	}
	f.s.Wait()
	if x = f.session(); x.FinishedAt == nil || x.Pending != "" {
		t.Fatalf("after the decision: %q %v", x.Pending, x.FinishedAt)
	}
	if finished() != 1 {
		t.Fatal("no session.finished after the request was decided")
	}
}

func TestFollowUpTellsTheAgentAtItsStop(t *testing.T) {
	f := newFUEnv(t)
	fu := f.queue(
		step("tell_agent", map[string]any{"message": "Now write the tests for {{.Trigger.Data.branch | default \"it\"}}."}),
		Action{Type: "notify", User: "actor", Message: "The agent wrote the tests: {{.Steps.s1.Answer}}", ID: "n"},
	)
	if fu.State != "armed" || len(fu.Steps) != 2 || !strings.Contains(fu.Held, "finish") {
		t.Fatalf("queued: %+v", fu)
	}
	if res := f.stop("waiting on the build", running("make")); res.Await != 0 {
		t.Fatal("a stop with background work set the follow-up off")
	}
	if fu = f.followUp(); fu.State != "armed" || fu.Held != "Still running a background task." {
		t.Fatalf("held: %+v", fu)
	}
	res := f.stop("the parser is done", nothingPending())
	if res.Await == 0 || res.Text != "" {
		t.Fatalf("finished stop: %+v", res)
	}
	f.work()
	got, err := f.s.AwaitMessage(f.ctx, f.ag, "cli-1", "box", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "Now write the tests for it.") || !strings.Contains(got.Text, "queued this for when you finished") {
		t.Fatalf("the hook got %q", got.Text)
	}
	f.work()
	if fu = f.followUp(); fu.State != "running" || fu.Run == nil || fu.Run.Steps[0].Status != "waiting" {
		t.Fatalf("while the agent works on it: %+v", fu)
	}
	if x := f.session(); x.FinishedAt != nil {
		t.Fatal("still finished after it was told something")
	}
	f.stop("tests written, all pass", nothingPending())
	f.work()
	fu = f.followUp()
	if fu.State != RunSucceeded || fu.Run.Steps[0].Outputs["Delivered"] != "stop" || fu.Run.Steps[0].Outputs["Answer"] != "tests written, all pass" {
		t.Fatalf("after: %+v %+v", fu, fu.Run.Steps)
	}
	notes, _, err := f.s.ListNotifications(f.ctx, f.admin, false, 20)
	if err != nil || len(notes) == 0 || !strings.Contains(notes[0].Body+notes[0].Title, "all pass") {
		t.Fatalf("notifications: %+v %v", notes, err)
	}
	f.hook("UserPromptSubmit", func(h *HookEvent) { h.Prompt = "thanks" })
	if res := f.stop("ok", nothingPending()); res.Await != 0 || res.Text != "" {
		t.Fatalf("a spent follow-up fired again: %+v", res)
	}
	list := sessionsFor(t, f.env, f.admin, AgentSessionQuery{})
	if list[0].FollowUp == nil || list[0].FollowUp.State != RunSucceeded || list[0].FollowUp.Steps != 2 {
		t.Fatalf("brief: %+v", list[0].FollowUp)
	}
	f.queue(Action{Type: "notify", User: "actor", Message: "again"})
	f.work()
	if fu = f.followUp(); fu.State != RunSucceeded || fu.Run.Via != "event" {
		t.Fatalf("re-armed on a finished session: %+v", fu)
	}
}

func TestTellAgentWaitsForTheNextStopWithoutARunner(t *testing.T) {
	f := newFUEnv(t)
	f.queue(step("tell_agent", map[string]any{"message": "Commit it.", "wait": false}))
	if _, err := f.s.RunFollowUpNow(f.ctx, f.admin, f.sid); err != nil {
		t.Fatal(err)
	}
	f.work()
	if fu := f.followUp(); fu.State != "running" {
		t.Fatalf("state %s", fu.State)
	}
	res := f.stop("halfway", running("go build"))
	if !strings.Contains(res.Text, "Commit it.") {
		t.Fatalf("the stop got %q", res.Text)
	}
	if x := f.session(); x.State != SessionWorking || x.FinishedAt != nil {
		t.Fatalf("after delivery: %s", x.State)
	}
	f.work()
	if fu := f.followUp(); fu.State != RunSucceeded {
		t.Fatalf("after delivery: %s", fu.State)
	}
}

func TestTellAgentThroughTheRunnersSocket(t *testing.T) {
	f := newFUEnv(t)
	box := f.runner(f.admin, "box", nil)
	f.queue(step("tell_agent", map[string]any{"message": "Open a PR."}))
	if _, err := f.s.RunFollowUpNow(f.ctx, f.admin, f.sid); err != nil {
		t.Fatal(err)
	}
	f.work()
	w := f.poll(f.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobMessage {
		t.Fatalf("job: %+v", w.Job)
	}
	var in MessageJob
	if err := json.Unmarshal(w.Job.Input, &in); err != nil || in.CLIID != "cli-1" || !strings.Contains(in.Body, "Open a PR.") {
		t.Fatalf("message job: %+v %v", in, err)
	}
	if res := f.stop("still going", running("x")); strings.Contains(res.Text, "Open a PR") {
		t.Fatal("delivered twice")
	}
	if _, err := f.s.FinishRunnerJob(f.ctx, f.admin, w.Job.ID, RunnerFinish{State: "done", Result: json.RawMessage(`{"delivered":true}`)}); err != nil {
		t.Fatal(err)
	}
	f.work()
	fu := f.followUp()
	if fu.Run.Steps[0].Status != "waiting" {
		t.Fatalf("step: %+v", fu.Run.Steps[0])
	}
	f.stop("PR opened", nothingPending())
	f.work()
	if fu = f.followUp(); fu.State != RunSucceeded || fu.Run.Steps[0].Outputs["Delivered"] != "socket" {
		t.Fatalf("after: %s %+v", fu.State, fu.Run.Steps[0].Outputs)
	}

	f.setClock(f.clockNow().Add(time.Minute)) // the last stop's hook has expired by now
	f.hook("UserPromptSubmit", func(h *HookEvent) { h.Prompt = "go" })
	f.queue(step("tell_agent", map[string]any{"message": "Now merge it.", "wait": false}))
	if _, err := f.s.RunFollowUpNow(f.ctx, f.admin, f.sid); err != nil {
		t.Fatal(err)
	}
	f.work()
	w = f.poll(f.admin, box, RunnerPoll{})
	if w.Job == nil {
		t.Fatal("no job")
	}
	f.s.FinishRunnerJob(f.ctx, f.admin, w.Job.ID, RunnerFinish{State: "failed", Error: "the session isn't running on this machine"})
	f.work()
	if res := f.stop("hm", nothingPending()); !strings.Contains(res.Text, "Now merge it.") {
		t.Fatalf("not delivered at the stop after the runner failed: %q", res.Text)
	}
}

func TestFollowUpStartsAFreshAgent(t *testing.T) {
	f := newFUEnv(t)
	box := f.runner(f.admin, "box", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	f.queue(step("start_session", map[string]any{"model": "opus", "prompt": "Review what {{.Trigger.Data.machine}}'s agent did, then build on it.", "permission_mode": "auto"}))
	if res := f.stop("haiku is done", nothingPending()); res.Await != 0 {
		t.Fatal("the hook was told to wait though no step talks to the agent")
	}
	f.work()
	w := f.poll(f.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobSession {
		t.Fatalf("job: %+v", w.Job)
	}
	var in SessionJob
	if err := json.Unmarshal(w.Job.Input, &in); err != nil || in.Dir != "/work/app" || in.Model != "opus" || in.PermissionMode != "auto" || in.Prompt != "Review what box's agent did, then build on it." {
		t.Fatalf("session job: %+v %v", in, err)
	}
	f.s.FinishRunnerJob(f.ctx, f.admin, w.Job.ID, RunnerFinish{State: "done", Result: json.RawMessage(`{"attach_id":"b50893f0"}`)})
	f.work()

	stranger, err := f.s.SessionHook(f.ctx, f.ag, []HookEvent{hook("box", "cli-x", "SessionStart", func(h *HookEvent) { h.SpawnedBy = "999" })})
	if err != nil {
		t.Fatal(err)
	}
	opus := agentActor(f.admin, "anthropic/claude-code")
	news, err := f.s.SessionHook(f.ctx, opus, []HookEvent{hook("box", "cli-2", "SessionStart", func(h *HookEvent) {
		h.SpawnedBy, h.Model = itoa64(w.Job.ID), "claude-opus-5-5"
	})})
	if err != nil {
		t.Fatal(err)
	}
	f.work()
	fu := f.followUp()
	if fu.State != "running" || fu.Run.Steps[0].Status != "waiting" {
		t.Fatalf("while the new agent works: %+v", fu.Run.Steps[0])
	}
	again, _ := f.s.SessionHook(f.ctx, opus, []HookEvent{hook("box", "cli-3", "SessionStart", func(h *HookEvent) { h.SpawnedBy = itoa64(w.Job.ID) })})
	var linked int
	f.s.db.R.QueryRow(`SELECT count(*) FROM agent_sessions WHERE spawned_by_job IS NOT NULL`).Scan(&linked)
	if linked != 1 || stranger.Session == news.Session || again.Session == news.Session {
		t.Fatalf("linked sessions: %d", linked)
	}
	if _, err := f.s.SessionHook(f.ctx, opus, []HookEvent{hook("box", "cli-2", "Stop", func(h *HookEvent) {
		h.LastMessage, h.Pending = "reviewed; three fixes pushed", nothingPending()
	})}); err != nil {
		t.Fatal(err)
	}
	f.work()
	fu = f.followUp()
	out := fu.Run.Steps[0].Outputs
	if fu.State != RunSucceeded || out["AttachID"] != "b50893f0" || out["Answer"] != "reviewed; three fixes pushed" || anyString(out["Session"]) != itoa64(news.Session) {
		t.Fatalf("after: %s %+v", fu.State, out)
	}
}

func TestStartSessionNeedsARunnerOnTheMachine(t *testing.T) {
	f := newFUEnv(t)
	f.runner(f.admin, "elsewhere", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	f.queue(step("start_session", map[string]any{"prompt": "go"}))
	f.stop("done", nothingPending())
	f.work()
	fu := f.followUp()
	if fu.State != RunFailed || !strings.Contains(fu.Run.Steps[0].Error, "no runner named box") {
		t.Fatalf("%s: %+v", fu.State, fu.Run.Steps[0])
	}
}

func TestFollowUpsAreTheSessionPersons(t *testing.T) {
	f := newFUEnv(t)
	bob := f.user("bob", false)
	p := f.project(f.admin, "App", "kanban")
	if err := f.s.SetMember(f.ctx, f.admin, p.Key, "bob", "owner"); err != nil {
		t.Fatal(err)
	}
	f.queue(Action{Type: "notify", User: "actor", Message: "done"})
	if _, err := f.s.GetFollowUp(f.ctx, bob, f.sid); err == nil {
		t.Fatal("bob saw the admin's follow-up")
	}
	if _, err := f.s.SaveFollowUp(f.ctx, f.ag, f.sid, FollowUpInput{Steps: []Action{{Type: "notify", User: "actor", Message: "x"}}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent queued steps: %v", err)
	}
	a, _ := f.s.followUpAutomation(f.ctx, f.s.db.R, f.sid)
	if _, err := f.s.GetAutomation(f.ctx, bob, a.ID); err == nil {
		t.Fatal("bob read it as an automation")
	}
	admins, _ := f.s.ListAdminAutomations(f.ctx, f.admin)
	for _, x := range admins {
		if x.ID == a.ID {
			t.Fatal("a follow-up is listed as an admin automation")
		}
	}
	if a.Admin {
		t.Fatal("a follow-up without a project reads as an admin automation")
	}
	if _, err := f.s.UpdateAutomation(f.ctx, f.admin, a.ID, AutomationInput{Rule: &Rule{Trigger: Trigger{Event: "manual"}, Actions: a.Rule.Actions}}); err == nil {
		t.Fatal("changed a follow-up's trigger")
	}
	if err := f.s.ClearFollowUp(f.ctx, f.admin, f.sid); err != nil {
		t.Fatal(err)
	}
	if fu := f.followUp(); fu.State != "none" || fu.Automation != 0 {
		t.Fatalf("after clear: %+v", fu)
	}
	if _, err := f.s.RunFollowUpNow(f.ctx, f.admin, f.sid); err == nil {
		t.Fatal("ran nothing")
	}
}

func TestFollowUpRunsAnAutomation(t *testing.T) {
	f := newFUEnv(t)
	p := f.project(f.admin, "Ops", "kanban")
	target := f.addAuto(p, "Nightly digest", Rule{Trigger: Trigger{Event: "manual"}, Inputs: []RuleInput{{Name: "days", Type: "number", Default: "7"}},
		Actions: []Action{{Type: "notify", User: "actor", Message: "digest for {{.Inputs.days}} days"}}})
	f.queue(step("run_automation", map[string]any{"automation": itoa64(target.ID), "inputs": `{"days": 3}`}))
	f.stop("done", nothingPending())
	f.work()
	fu := f.followUp()
	if fu.State != RunSucceeded || fu.Run.Steps[0].Outputs["Status"] != RunSucceeded {
		t.Fatalf("%s: %+v", fu.State, fu.Run.Steps)
	}
	runs, _ := f.s.ListAutomationRuns(f.ctx, f.admin, target.ID, 5, 0)
	if len(runs) != 1 || runs[0].Via != "automation" {
		t.Fatalf("target runs: %+v", runs)
	}
	notes, _, _ := f.s.ListNotifications(f.ctx, f.admin, false, 20)
	if len(notes) == 0 || !strings.Contains(notes[0].Body+notes[0].Title, "3 days") {
		t.Fatalf("notes: %+v", notes)
	}
}

func TestFollowUpRunsOncePerArming(t *testing.T) {
	f := newFUEnv(t)
	f.queue(Action{Type: "notify", User: "actor", Message: "one"})
	f.stop("done", nothingPending())
	f.work()
	run := f.followUp().Run
	f.hook("UserPromptSubmit", func(h *HookEvent) { h.Prompt = "more" })
	f.queue(Action{Type: "notify", User: "actor", Message: "two"})
	if _, err := f.s.RerunAutomation(f.ctx, f.admin, run.ID); err == nil {
		t.Fatal("ran a follow-up again with Run again")
	}
	f.stop("done", nothingPending())
	f.work()
	a, _ := f.s.followUpAutomation(f.ctx, f.s.db.R, f.sid)
	if runs, _ := f.s.ListAutomationRuns(f.ctx, f.admin, a.ID, 10, 0); len(runs) != 2 {
		t.Fatalf("%d runs for two armings", len(runs))
	}
}

func TestCancelledRunWithdrawsItsMessage(t *testing.T) {
	f := newFUEnv(t)
	f.queue(step("tell_agent", map[string]any{"message": "Deploy to prod.", "wait": false}))
	if _, err := f.s.RunFollowUpNow(f.ctx, f.admin, f.sid); err != nil {
		t.Fatal(err)
	}
	f.work()
	if _, err := f.s.CancelAutomationRun(f.ctx, f.admin, f.followUp().Run.ID); err != nil {
		t.Fatal(err)
	}
	if res := f.stop("hm", nothingPending()); strings.Contains(res.Text, "Deploy") {
		t.Fatalf("a cancelled run's message was delivered: %q", res.Text)
	}
}

func TestAFinishBeforeTheRunnerReportsCounts(t *testing.T) {
	f := newFUEnv(t)
	box := f.runner(f.admin, "box", nil)
	f.queue(step("tell_agent", map[string]any{"message": "Say ok."}))
	f.s.RunFollowUpNow(f.ctx, f.admin, f.sid)
	f.work()
	w := f.poll(f.admin, box, RunnerPoll{})
	if w.Job == nil {
		t.Fatal("no job")
	}
	f.hook("UserPromptSubmit", func(h *HookEvent) { h.Prompt = "Say ok." })
	f.stop("ok", nothingPending())
	f.s.FinishRunnerJob(f.ctx, f.admin, w.Job.ID, RunnerFinish{State: "done", Result: json.RawMessage(`{"delivered":true}`)})
	f.work()
	if fu := f.followUp(); fu.State != RunSucceeded || fu.Run.Steps[0].Outputs["Answer"] != "ok" {
		t.Fatalf("lost the finish: %s %+v", fu.State, fu.Run.Steps[0])
	}
}

func TestAgentsCantReachTheirPersonsFollowUps(t *testing.T) {
	f := newFUEnv(t)
	f.queue(step("tell_agent", map[string]any{"message": "SECRET PROMPT"}))
	f.s.RunFollowUpNow(f.ctx, f.admin, f.sid)
	f.work()
	fu := f.followUp()
	if _, err := f.s.GetAutomation(f.ctx, f.ag, fu.Automation); err == nil {
		t.Error("the agent read the follow-up")
	}
	if _, err := f.s.GetAutomationRun(f.ctx, f.ag, fu.Run.ID); err == nil {
		t.Error("the agent read its run")
	}
	if _, err := f.s.CancelAutomationRun(f.ctx, f.ag, fu.Run.ID); err == nil {
		t.Error("the agent cancelled its run")
	}
	if _, err := f.s.TestAutomation(f.ctx, f.ag, fu.Automation, TestInput{}); err == nil {
		t.Error("the agent tested it")
	}
	if l := sessionsFor(t, f.env, f.ag, AgentSessionQuery{}); l[0].FollowUp != nil {
		t.Error("the agent sees the brief")
	}
}

// wp followup finds "this session" by the CLI's id: only the person's own.
func TestCurrentAgentSessionIsYourOwn(t *testing.T) {
	f := newFUEnv(t)
	if x, err := f.s.CurrentAgentSession(f.ctx, f.admin, "cli-1", "box"); err != nil || x.ID != f.sid {
		t.Fatalf("own session: %v %v", x, err)
	}
	if _, err := f.s.CurrentAgentSession(f.ctx, f.admin, "cli-1", "elsewhere"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another machine: %v", err)
	}
	bob := f.user("bob", false)
	if _, err := f.s.CurrentAgentSession(f.ctx, bob, "cli-1", "box"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob found the admin's session: %v", err)
	}
	from := *f.admin
	from.Session = f.sid
	if x, err := f.s.CurrentAgentSession(f.ctx, &from, "", ""); err != nil || x.ID != f.sid {
		t.Fatalf("from the header: %v %v", x, err)
	}
	if _, err := f.s.CurrentAgentSession(f.ctx, f.admin, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no session named: %v", err)
	}
}

func TestAProjectlessFollowUpSeesOnlyItsPersonsEvents(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	secret := e.project(e.admin, "Secret", "kanban")
	ag := agentActor(bob, "anthropic/claude-code")
	res, _ := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("bobs", "b-1", "SessionStart", nil)})
	if _, err := e.s.SaveFollowUp(e.ctx, bob, res.Session, FollowUpInput{Steps: []Action{
		{Type: "wait_for", ID: "w", Extra: map[string]any{"event": "issue.created", "timeout": "1h"}},
		{Type: "notify", User: "actor", Message: "saw {{.Steps.w.Event.IssueKey}}"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RunFollowUpNow(e.ctx, bob, res.Session); err != nil {
		t.Fatal(err)
	}
	e.s.DrainAutomationRuns(e.ctx)
	if _, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: secret.Key, Title: "Layoffs"}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	e.s.DrainAutomationRuns(e.ctx)
	fu, _ := e.s.GetFollowUp(e.ctx, bob, res.Session)
	if fu.State != "running" {
		t.Fatalf("bob's follow-up saw an issue in a project he isn't in: %s", fu.State)
	}
}

func TestRunAutomationActsAsTheSaver(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	theirs := e.project(e.admin, "Secret", "kanban")
	target := e.addAuto(theirs, "Secret job", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "notify", User: "actor", Message: "x"}}})
	mine, err := e.s.CreateProject(e.ctx, bob, ProjectInput{Name: str("Bobs")})
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{step("run_automation", map[string]any{"automation": itoa64(target.ID), "wait": false})}}
	a, err := e.s.CreateAutomation(e.ctx, bob, mine.Key, AutomationInput{Name: str("Sneaky"), Rule: &rule})
	if err != nil {
		t.Fatal(err)
	}
	e.s.startRun(e.ctx, SystemActor(), a, &Event{Kind: EvManual, ProjectID: mine.ID, At: e.s.now()}, fireOpts{Via: "schedule"})
	e.s.DrainAutomationRuns(e.ctx)
	if runs, _ := e.s.ListAutomationRuns(e.ctx, e.admin, target.ID, 5, 0); len(runs) != 0 {
		t.Fatal("bob's automation ran an automation bob can't run")
	}
}

func TestASessionElsewhereCantClaimAFreshAgentJob(t *testing.T) {
	f := newFUEnv(t)
	box := f.runner(f.admin, "box", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	f.queue(step("start_session", map[string]any{"prompt": "go"}))
	f.stop("done", nothingPending())
	f.work()
	w := f.poll(f.admin, box, RunnerPoll{})
	forged, _ := f.s.SessionHook(f.ctx, f.ag, []HookEvent{hook("evil-laptop", "cli-evil", "SessionStart", func(h *HookEvent) { h.SpawnedBy = itoa64(w.Job.ID) })})
	var linked sql.NullInt64
	f.s.db.R.QueryRow(`SELECT spawned_by_job FROM agent_sessions WHERE id = ?`, forged.Session).Scan(&linked)
	if linked.Valid {
		t.Fatal("a session on another machine claimed the job")
	}
}

// A tell_agent step that isn't blocking waits in the session's batch
// (WP-209): until just before the agent's prompt cache cools.
func TestTellAgentThatCanWaitGoesWithTheBatch(t *testing.T) {
	f := newFUEnv(t)
	box := f.runner(f.admin, "box", nil)
	f.stop("done", nothingPending())
	f.setClock(f.clockNow().Add(time.Minute))
	f.queue(step("tell_agent", map[string]any{"message": "Tidy the branches.", "blocking": false, "wait": false}))
	f.work()
	if w := f.poll(f.admin, box, RunnerPoll{}); w.Job != nil {
		t.Fatalf("posted at once: %+v", w.Job)
	}
	f.setClock(f.clockNow().Add(cacheWarm))
	f.poll(f.admin, box, RunnerPoll{})
	f.s.CheckBabysits(f.ctx)
	w := f.poll(f.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobMessage || !strings.Contains(string(w.Job.Input), "Tidy the branches.") {
		t.Fatalf("job: %+v", w.Job)
	}
	if _, err := f.s.FinishRunnerJob(f.ctx, f.admin, w.Job.ID, RunnerFinish{State: "done", Result: json.RawMessage(`{"delivered":true}`)}); err != nil {
		t.Fatal(err)
	}
	f.work()
	if fu := f.followUp(); fu.State != RunSucceeded || fu.Run.Steps[0].Outputs["Delivered"] != "socket" {
		t.Fatalf("after: %s %+v", fu.State, fu.Run.Steps[0])
	}
}
