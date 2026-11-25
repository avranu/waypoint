package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// apEnv is a person's interactive session in a project with autopilot on,
// a runner on its machine, and runner jobs caught instead of run.
type apEnv struct {
	*taskEnv
	p    *Project
	jobs []RunnerJobInput
	res  map[int64]RunnerJob
}

func newAPEnv(t *testing.T) *apEnv {
	te := newTaskEnv(t, "")
	a := &apEnv{taskEnv: te, p: te.project(te.admin, "App", ""), res: map[int64]RunnerJob{}}
	te.runner(te.admin, "box", func(r *RunnerRegistration) {
		r.Features = []string{FeatureAutopilot}
		r.CLIs = []RunnerCLI{{Name: "claude"}}
	})
	oldEnq, oldLook := enqueueRunnerJob, lookupRunnerJob
	enqueueRunnerJob = func(ctx context.Context, s *Service, ac *Actor, in RunnerJobInput) (*RunnerJob, error) {
		if in.Source == reviewCompactSource {
			return oldEnq(ctx, s, ac, in)
		}
		a.jobs = append(a.jobs, in)
		if err := s.write(ctx, ac, func(t *tx) error {
			return s.recordHandedOver(ctx, t, in.Kind, in.Input, in.Source, in.Timeout, in.OwnerID)
		}); err != nil {
			return nil, err
		}
		return &RunnerJob{ID: int64(len(a.jobs)), Kind: in.Kind, State: "queued", Source: in.Source, SourceID: in.SourceID}, nil
	}
	lookupRunnerJob = func(ctx context.Context, s *Service, id int64) (*RunnerJob, error) {
		j := a.res[id]
		j.ID = id
		if id >= 1 && id <= int64(len(a.jobs)) {
			j.Input = a.jobs[id-1].Input
		}
		return &j, nil
	}
	t.Cleanup(func() { enqueueRunnerJob, lookupRunnerJob = oldEnq, oldLook })
	if _, err := te.s.SetAutopilot(te.ctx, te.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(true)}); err != nil {
		t.Fatal(err)
	}
	a.autopilotStarted(a.session)
	return a
}

// autopilotStarted records that an earlier step of autopilot's started the
// session, as the runner's report does: autopilot moves on only sessions it
// started (WP-1319).
func (a *apEnv) autopilotStarted(session int64) {
	a.t.Helper()
	now := a.clockNow().UnixMilli()
	res, err := a.s.db.W.Exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, state, started_at, last_seen_at, ended_at)
		VALUES (?, 'box', ?, 'ended', ?, ?, ?)`, a.admin.User.ID, fmt.Sprintf("before-%d-%d", session, a.bornN()), now-1, now, now)
	if err != nil {
		a.t.Fatal(err)
	}
	prev, _ := res.LastInsertId()
	if _, err := a.s.db.W.Exec(`INSERT INTO autopilot_steps (user_id, project_id, session_id, stop_at, state, due_at, next_session, next_session_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, a.admin.User.ID, a.p.ID, prev, now, StepStarted, now, session, now, now, now); err != nil {
		a.t.Fatal(err)
	}
}

// bornN numbers the stand-in sessions autopilotStarted makes.
func (a *apEnv) bornN() int {
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM agent_sessions WHERE cli_id LIKE 'before-%'`).Scan(&n)
	return n
}

// finish takes and finishes the task in the session.
func (a *apEnv) finish(key string) {
	a.t.Helper()
	if _, err := a.s.AgentTake(a.ctx, a.ag, key, ""); err != nil {
		a.t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	res, err := a.s.AgentDone(a.ctx, a.ag, key, AgentDoneInput{Comment: "done"})
	if err != nil {
		a.t.Fatal(err)
	}
	if !res.Autopilot {
		a.t.Errorf("wp done in an autopilot session says %q", res.Message)
	}
}

// stopWith reports a Stop with that last message and background work.
func (a *apEnv) stopWith(msg string, tasks ...PendingTask) *HookResult {
	a.t.Helper()
	a.setClock(a.clockNow().Add(10 * time.Second))
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "Stop", func(h *HookEvent) {
		h.LastMessage = msg
		h.Pending = &HookPending{Tasks: append([]PendingTask{}, tasks...), Crons: []PendingCron{}}
	})})
	if err != nil {
		a.t.Fatal(err)
	}
	return res
}

func (a *apEnv) step() *stepRow {
	a.t.Helper()
	st, err := scanStep(a.s.db.R.QueryRow(`SELECT `+stepCols+` FROM autopilot_steps WHERE session_id = ?`, a.session))
	if err != nil {
		a.t.Fatalf("no autopilot step: %v", err)
	}
	return st
}

func (a *apEnv) tick(d time.Duration) {
	a.s.Wait()
	a.setClock(a.clockNow().Add(d))
	_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = ?`, a.clockNow().UnixMilli())
	a.s.RunAutopilot(a.ctx)
}

func (a *apEnv) jobDone(n int, res AutopilotJobResult) {
	raw, _ := json.Marshal(res)
	a.res[int64(n)] = RunnerJob{State: "done", Result: raw}
	src := autopilotSource
	if n >= 1 && n <= len(a.jobs) {
		src = a.jobs[n-1].Source
	}
	a.s.autopilotJobFinished(a.ctx, &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"job": int64(n), "source": src, "source_id": a.step().id}})
}

// A finished session is due after the grace; the tick checks the limits,
// reserves the next task and queues the runner's job; the job's answer
// ends the step and marks the summary as one autopilot moved on from.
func TestAutopilotMovesAFinishedSessionOn(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	res := a.stopWith("Shipped the first one.")
	if !strings.Contains(res.Note, "Autopilot: "+first.Key+" is done") || !strings.Contains(res.Note, "about a minute") {
		t.Errorf("note = %q", res.Note)
	}
	if st := a.step(); st.state != StepDue || st.dueAt != a.clockNow().UnixMilli()+60_000 {
		t.Fatalf("step = %+v", st)
	}
	a.tick(30 * time.Second)
	if len(a.jobs) != 0 {
		t.Fatal("moved on inside the grace period")
	}
	a.tick(31 * time.Second)
	if len(a.jobs) != 1 || a.jobs[0].Kind != JobAutopilot || a.jobs[0].Label != "box" || a.jobs[0].ProjectID != a.p.ID {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.Key != second.Key || in.Done != first.Key || in.CLIID != a.cli || in.Agent != "anthropic/claude-opus-5-5" || in.Dir != "/work/app" {
		t.Errorf("job input = %+v", in)
	}
	if d := a.get(a.admin, second.Key); d.ClaimedBy != "anthropic/claude-opus-5-5" {
		t.Errorf("the next task isn't reserved: %q", d.ClaimedBy)
	}
	if st := a.step(); st.state != StepStarting {
		t.Fatalf("step = %s", st.state)
	}
	a.tick(time.Minute)
	if len(a.jobs) != 1 {
		t.Fatal("queued a second job while the first ran")
	}
	if _, err := a.s.SessionHook(a.ctx, a.admin, []HookEvent{hook("box", a.cli, "SessionEnd", func(h *HookEvent) { h.EndReason = "prompt_input_exit" })}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.SessionHook(a.ctx, a.admin, []HookEvent{hook("box", "cli-2", "SessionStart", func(h *HookEvent) { h.Source = "startup" })}); err != nil {
		t.Fatal(err)
	}
	a.jobDone(1, AutopilotJobResult{Outcome: "started", Via: "restart", NewCLI: "cli-2"})
	st := a.step()
	if st.state != StepStarted || st.detail != second.Key+" started in the restarted session" {
		t.Fatalf("after the job: %+v", st)
	}
	var auto int
	_ = a.s.db.R.QueryRow(`SELECT autopilot FROM task_summaries WHERE session_id = ?`, a.session).Scan(&auto)
	if auto != 1 {
		t.Error("the summary isn't marked as one autopilot moved on from")
	}
	if _, err := a.s.AutopilotOverview(a.ctx, a.ag); err != nil {
		t.Errorf("an agent reading autopilot: %v", err)
	}
	o, err := a.s.AutopilotOverview(a.ctx, a.admin)
	if err != nil {
		t.Fatal(err)
	}
	var steps []*AutopilotStep
	for _, p := range o.Projects {
		if p.ProjectKey == a.p.Key {
			steps = slices.DeleteFunc(p.Steps, func(s *AutopilotStep) bool { return s.Session == nil || s.Next == nil })
		}
	}
	if len(steps) != 1 || steps[0].Next == nil || steps[0].Next.Key != second.Key || steps[0].NextSession == 0 || o.Unreviewed != 1 {
		t.Errorf("overview steps = %+v, unreviewed %d", steps, o.Unreviewed)
	}
}

// A person who types in the grace period keeps the session, for good: it is
// theirs from then on (WP-1319).
func TestAutopilotLeavesAPersonTheirSession(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	a.record(TypedKickoff, AutopilotPromptMark+" started this session for APP-9.")
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "UserPromptSubmit", func(h *HookEvent) { h.Prompt = AutopilotPromptMark + " started this session for APP-9." })}); err != nil {
		t.Fatal(err)
	}
	if st := a.step(); st.state != StepDue {
		t.Fatalf("autopilot's prompt took the session: %+v", st)
	}
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "Also fix the typo" })}); err != nil {
		t.Fatal(err)
	}
	if st := a.step(); st.state != StepStopped || st.reason != "prompted" {
		t.Fatalf("after a prompt: %+v", st)
	}
	a.tick(2 * time.Minute)
	if len(a.jobs) != 0 {
		t.Fatal("moved a session its person was using")
	}
	if res := a.stopWith("Fixed the typo."); strings.Contains(res.Note, "Autopilot") {
		t.Errorf("note = %q", res.Note)
	}
	a.tick(30 * time.Minute)
	if len(a.jobs) != 0 {
		t.Fatalf("moved on a session its person took: jobs = %d", len(a.jobs))
	}
}

// keep, typed while autopilot is about to move a session on, stops it with
// no turn: the hook says to stop the prompt, nothing is recorded as a
// prompt and the session stays idle. With nothing about to happen it is an
// ordinary prompt.
func TestAutopilotKeep(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	if _, err := a.s.storeMessage(a.ctx, a.session, "news for the next turn", "test:news", messageFrom{by: a.admin.User.ID}, true); err != nil {
		t.Fatal(err)
	}
	prompt := func(p string) *HookResult {
		t.Helper()
		res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "UserPromptSubmit", func(h *HookEvent) { h.Prompt = p })})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := prompt(" Keep ")
	if res.Block != AutopilotKeptLine || res.Text != "" || len(res.Messages) != 0 {
		t.Fatalf("keep = %+v", res)
	}
	if st := a.step(); st.state != StepStopped || st.reason != "kept" {
		t.Fatalf("after keep: %+v", st)
	}
	var prompts int
	var state string
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM session_events WHERE session_id = ? AND kind = 'prompt'`, a.session).Scan(&prompts)
	_ = a.s.db.R.QueryRow(`SELECT state FROM agent_sessions WHERE id = ?`, a.session).Scan(&state)
	if prompts != 0 || state == SessionWorking {
		t.Errorf("keep was a turn: %d prompts, state %s", prompts, state)
	}
	a.tick(2 * time.Minute)
	if len(a.jobs) != 0 {
		t.Fatal("moved a kept session on")
	}
	if res := prompt("keep"); res.Block != "" {
		t.Errorf("keep with nothing about to happen was dropped: %+v", res)
	}
	if res := prompt(" /KEEP "); res.Block != AutopilotNothingToKeepLine {
		t.Errorf("/keep with nothing about to happen = %+v", res)
	}
}

// Turning autopilot on, changing a limit or starting a step now is for a
// person directly, not from an agent's shell or session, even with their
// token; turning it off is for anyone.
func TestAutopilotOnIsForPeopleDirectly(t *testing.T) {
	a := newAPEnv(t)
	inShell := *a.admin
	inShell.AgentEnv = "anthropic/claude-code"
	inSession := *a.admin
	inSession.Session = a.session
	for name, ac := range map[string]*Actor{"agent": a.ag, "agent's shell": &inShell, "agent session": &inSession} {
		if _, err := a.s.SetAutopilot(a.ctx, ac, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(true), Merge: true}); err == nil {
			t.Errorf("%s turned autopilot on", name)
		}
		if _, err := a.s.SetAutopilot(a.ctx, ac, "", AutopilotSettingInput{Merge: true, Limits: AutopilotLimits{WeekMax: ptrOf(100.0)}}); err == nil {
			t.Errorf("%s raised a limit", name)
		}
	}
	if _, err := a.s.SetAutopilot(a.ctx, &inShell, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
		t.Errorf("turning it off from an agent's shell: %v", err)
	}
}

// Background work started after the task holds the session; work from
// before it, or running for hours, is stale and doesn't.
func TestAutopilotStaleBackgroundWork(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	waiter := PendingTask{Type: "shell", Status: "running", Description: "wp pr wait 12"}
	a.stopWith("Working; CI is running.", waiter)
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Done.", waiter)
	if st := a.step(); st.state != StepDue {
		t.Fatalf("a waiter from before the done held it: %+v", st)
	}
	tests := PendingTask{Type: "shell", Status: "running", Description: "make check"}
	a.stopWith("Checking one more thing.", waiter, tests)
	st := a.step()
	if st.state != StepWaiting || st.reason != WaitBackground || !strings.Contains(st.detail, "make check") || strings.Contains(st.detail, "wp pr wait") {
		t.Fatalf("live work after the done: %+v", st)
	}
	if st.dueAt != a.clockNow().UnixMilli()+autopilotStaleAfter.Milliseconds() {
		t.Errorf("looks again at %d, want when it goes stale", st.dueAt)
	}
	a.tick(autopilotStaleAfter + time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("still held by stale work: %+v", a.step())
	}
	if got, _, _ := a.s.autopilotHold(a.ctx, a.s.db.R, a.session, false, nil, 0, a.clockNow().UnixMilli()); got != WaitUnknown {
		t.Errorf("unknown pending = %q", got)
	}
}

// The limits pause it and say so once; it goes on once they drop.
func TestAutopilotLimits(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	hour := a.clockNow().UnixMilli() - a.clockNow().UnixMilli()%3_600_000
	if _, err := a.s.db.W.Exec(`INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, output) VALUES (?,?,?,?,?,?,?)`,
		a.admin.User.ID, hour, "box", "s", "", "claude-opus-5-5", 5_000_000); err != nil {
		t.Fatal(err)
	}
	a.tick(2 * time.Minute)
	st := a.step()
	if st.state != StepWaiting || st.reason != PauseHourly || len(a.jobs) != 0 {
		t.Fatalf("over the hourly limit: %+v, jobs %d", st, len(a.jobs))
	}
	a.tick(6 * time.Minute)
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ?`, a.admin.User.ID, NotifyAutopilot).Scan(&n)
	if n != 1 {
		t.Errorf("notified %d times, want once", n)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true, Limits: AutopilotLimits{MaxHourlyUSD: ptrOf(500.0)}}); err != nil {
		t.Fatal(err)
	}
	reset := a.clockNow().Add(5*24*time.Hour + 12*time.Hour)
	if _, err := a.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
		a.admin.User.ID, a.clockNow().UnixMilli(), "seven_day", 80.0, reset.UnixMilli(), "box"); err != nil {
		t.Fatal(err)
	}
	a.tick(6 * time.Minute)
	if st := a.step(); st.reason != PauseWeek || !strings.Contains(st.detail, "ahead of an even pace") {
		t.Fatalf("far ahead of the week's pace: %+v", st)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Limits: AutopilotLimits{WeekAhead: ptrOf(0.0)}}); err != nil {
		t.Fatal(err)
	}
	a.tick(16 * time.Minute)
	if len(a.jobs) != 1 {
		t.Fatalf("with the week's limit off: %+v", a.step())
	}

	m := &AutopilotMeter{HourlyUSD: 1, WeekPercent: ptrOf(92.0), WeekEven: ptrOf(85.0), WeekResetsAt: ptrOf(a.clockNow().Add(25 * time.Hour))}
	if why, _ := autopilotPaused(m, waypointAutopilotDefaults(), a.clockNow()); pauseReason(why) != PauseWeekMax {
		t.Errorf("92%% of the week, a day before it resets = %q", why)
	}
	m.WeekEven, m.WeekResetsAt = ptrOf(93.0), ptrOf(a.clockNow().Add(18*time.Hour))
	if why, _ := autopilotPaused(m, waypointAutopilotDefaults(), a.clockNow()); why != "" {
		t.Errorf("92%% of the week, 18 hours before it resets = %q", why)
	}
	m.WeekPercent = ptrOf(96.0)
	if why, _ := autopilotPaused(m, waypointAutopilotDefaults(), a.clockNow()); pauseReason(why) != PauseWeekMax || !strings.Contains(why, "95%") {
		t.Errorf("96%% of the week, 18 hours before it resets = %q", why)
	}
	for _, left := range []time.Duration{12 * time.Hour, time.Minute} {
		m.WeekPercent, m.WeekResetsAt = ptrOf(99.5), ptrOf(a.clockNow().Add(left))
		if why, _ := autopilotPaused(m, waypointAutopilotDefaults(), a.clockNow()); why != "" {
			t.Errorf("99.5%% of the week, %s before it resets = %q", left, why)
		}
	}
	five, at := 92.0, a.clockNow().Add(time.Hour)
	m = &AutopilotMeter{FiveHour: &five, FiveResetsAt: &at}
	if why, recheck := autopilotPaused(m, waypointAutopilotDefaults(), a.clockNow()); pauseReason(why) != PauseFiveHour || !recheck.After(at) {
		t.Errorf("92%% of five hours = %q, recheck %v", why, recheck)
	}
}

// The person can turn the quota limits off for a while: what waits on one
// goes on at the next tick, the hourly limit still holds, and the overview
// says what the quota would have paused for.
func TestAutopilotQuotaLimitsOff(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	if _, err := a.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine) VALUES (?,?,?,?,?,?)`,
		a.admin.User.ID, a.clockNow().UnixMilli(), "seven_day", 91.0, a.clockNow().Add(3*24*time.Hour).UnixMilli(), "box"); err != nil {
		t.Fatal(err)
	}
	a.tick(2 * time.Minute)
	if st := a.step(); st.state != StepWaiting || st.reason != PauseWeekMax {
		t.Fatalf("91%% of the week: %+v", st)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true, QuotaOffUntil: ptrOf(a.clockNow().Add(time.Hour))}); err == nil {
		t.Error("turned the quota limits off for one project")
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Merge: true, QuotaOffUntil: ptrOf(a.clockNow().Add(8 * 24 * time.Hour))}); err == nil {
		t.Error("turned the quota limits off for over a week")
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.ag, "", AutopilotSettingInput{Merge: true, QuotaOffUntil: ptrOf(a.clockNow().Add(time.Hour))}); err == nil {
		t.Error("an agent turned the quota limits off")
	}
	o, err := a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Merge: true, QuotaOffUntil: ptrOf(a.clockNow().Add(time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	if o.Meter.PausedBecause != "" || pauseReason(o.Meter.QuotaHeld) != PauseWeekMax || o.Meter.QuotaOffUntil == nil {
		t.Errorf("with the quota limits off the meter says %+v", o.Meter)
	}
	offAt := func() (at int64) {
		_ = a.s.db.R.QueryRow(`SELECT quota_off_at FROM autopilot_settings WHERE user_id = ? AND project_id IS NULL`, a.admin.User.ID).Scan(&at)
		return at
	}
	offFrom := a.clockNow().UnixMilli()
	if offAt() != offFrom {
		t.Errorf("quota_off_at %d, want %d", offAt(), offFrom)
	}
	a.setClock(a.clockNow().Add(time.Second))
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Merge: true, QuotaOffUntil: ptrOf(a.clockNow().Add(2 * time.Hour))}); err != nil || offAt() != offFrom {
		t.Errorf("extended: quota_off_at %d %v", offAt(), err)
	}
	a.tick(time.Minute)
	if len(a.jobs) != 1 {
		t.Fatalf("with the quota limits off: %+v", a.step())
	}
	m := &AutopilotMeter{HourlyUSD: 100, WeekPercent: ptrOf(95.0), WeekEven: ptrOf(50.0), QuotaOffUntil: ptrOf(a.clockNow().Add(time.Hour))}
	if why, _ := autopilotPaused(m, waypointAutopilotDefaults(), a.clockNow()); pauseReason(why) != PauseHourly {
		t.Errorf("over the hourly limit with the quota limits off = %q", why)
	}
	m.HourlyUSD = 1
	if why, _ := autopilotPaused(m, waypointAutopilotDefaults(), a.clockNow().Add(2*time.Hour)); pauseReason(why) != PauseWeekMax {
		t.Errorf("once the quota limits are back on = %q", why)
	}
	o, err = a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Merge: true, QuotaOffUntil: ptrOf(time.Time{})})
	if err != nil {
		t.Fatal(err)
	}
	if o.Meter.QuotaOffUntil != nil || pauseReason(o.Meter.PausedBecause) != PauseWeekMax {
		t.Errorf("with the quota limits back on the meter says %+v", o.Meter)
	}
}

// A ready task that builds on the session's unmerged pull request waits;
// an unrelated one goes first; after three hours it goes anyway.
func TestAutopilotMergeWait(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.finish(first.Key)
	child := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Builds on it", Agent: "claude", Priority: "urgent", Parent: first.Key})
	if _, err := a.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		first.ID, "forgejo", "pull", "https://git.example/acme/app/pulls/7", "acme/app", 7, "open", 1); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Shipped as #7.")
	a.tick(2 * time.Minute)
	if st := a.step(); st.state != StepWaiting || st.reason != WaitMerge || !strings.Contains(st.detail, "acme/app#7") {
		t.Fatalf("only a related task ready: %+v", st)
	}
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Unrelated", Agent: "claude"})
	a.tick(6 * time.Minute)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || in.Key != other.Key {
		t.Fatalf("the unrelated task should go first: %+v", a.jobs)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true, Limits: AutopilotLimits{Merge: ptrOf(MergeWaitNever)}}); err != nil {
		t.Fatal(err)
	}
	a.jobDone(1, AutopilotJobResult{Outcome: "held", Reason: "draft", Detail: "someone typed"})
	if st := a.step(); st.state != StepWaiting || st.reason != WaitScreen {
		t.Fatalf("held: %+v", st)
	}
	if d := a.get(a.admin, other.Key); d.ClaimedBy != "" {
		t.Errorf("a held job kept its reservation: %q", d.ClaimedBy)
	}
	a.tick(3 * time.Minute)
	_ = json.Unmarshal(a.jobs[len(a.jobs)-1].Input, &in)
	if in.Key != child.Key {
		t.Errorf("merge never: picked %s, want the urgent %s", in.Key, child.Key)
	}
}

// Task sessions hand off themselves (WP-97), and a project without
// autopilot is left alone; turning it off stops a step.
func TestAutopilotOnlyWhereItIsOn(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false), Merge: true}); err != nil {
		t.Fatal(err)
	}
	if st := a.step(); st.state != StepStopped || st.reason != "off" {
		t.Fatalf("after turning it off: %+v", st)
	}
	a.tick(5 * time.Minute)
	if len(a.jobs) != 0 {
		t.Fatal("moved on with autopilot off")
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.ag, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(true)}); err == nil {
		t.Error("an agent turned autopilot on")
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Limits: AutopilotLimits{Merge: ptrOf("sometimes")}}); !IsInvalid(err) {
		t.Errorf("a bad merge policy: %v", err)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Enabled: ptrOf(true)}); !IsInvalid(err) {
		t.Errorf("enabled without a project: %v", err)
	}

	tt := newTaskEnv(t, "APP-1")
	var kind string
	_ = tt.s.db.R.QueryRow(`SELECT kind FROM agent_sessions WHERE id = ?`, tt.session).Scan(&kind)
	if kind != SessionTask {
		t.Fatalf("kind = %q", kind)
	}
	x, _ := scanSession(tt.s.db.R.QueryRow(`SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, tt.session), tt.clockNow().UnixMilli())
	var note string
	err := tt.s.write(tt.ctx, tt.ag, func(t *tx) error {
		var err error
		note, err = tt.s.autopilotAtStop(tt.ctx, t, x, &HookEvent{Event: "Stop"})
		return err
	})
	if note != "" || err != nil {
		t.Errorf("a task session: %q %v", note, err)
	}
}

// SessionStart tells an agent in an autopilot project how it ends.
func TestAutopilotSessionNote(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ? WHERE id = ?`, a.p.ID, a.session); err != nil {
		t.Fatal(err)
	}
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "Autopilot is on.") {
		t.Errorf("SessionStart text = %q", res.Text)
	}
}

// A job no runner took is cancelled once it has sat a while, and the step
// tries again; after three failed jobs it gives up and says so.
func TestAutopilotUnsticksAJobNobodyTook(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	for try := 1; try <= autopilotJobTries; try++ {
		a.tick(6 * time.Minute)
		if len(a.jobs) != try || a.step().state != StepStarting {
			t.Fatalf("try %d: jobs %d, step %+v", try, len(a.jobs), a.step())
		}
		a.res[int64(try)] = RunnerJob{State: "running"}
		a.setClock(a.clockNow().Add(autopilotJobStuck + time.Minute))
		a.res[int64(try)] = RunnerJob{State: "cancelled", Error: "Cancelled"}
		a.s.unstickSteps(a.ctx)
	}
	if st := a.step(); st.state != StepFailed || st.reason != WaitJob {
		t.Fatalf("after %d stuck jobs: %+v", autopilotJobTries, st)
	}
}

// Asks in a summary that the trailing-question rule doesn't catch get their
// own request.
func TestSummaryAsksFiledWhenNothingElseIs(t *testing.T) {
	te := newTaskEnv(t, "")
	p := te.project(te.admin, "App", "")
	a := te.issue(te.admin, IssueInput{Project: p.Key, Title: "First", Agent: "claude"})
	if _, err := te.s.AgentTake(te.ctx, te.ag, a.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := te.s.AgentDone(te.ctx, te.ag, a.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	te.setClock(te.clockNow().Add(time.Minute))
	msg := "Done.\n\n**Waiting on you:**\n- Rotate the forge token before Friday\n\nCI is green and the PR is armed."
	if _, err := te.s.SessionHook(te.ctx, te.ag, []HookEvent{hook("box", te.cli, "Stop", func(h *HookEvent) { h.LastMessage = msg })}); err != nil {
		t.Fatal(err)
	}
	var title, body string
	if err := te.s.db.R.QueryRow(`SELECT title, body FROM requests WHERE id = (SELECT request_id FROM task_summaries)`).Scan(&title, &body); err != nil {
		t.Fatal(err)
	}
	if title != a.Key+": Rotate the forge token before Friday" || !strings.Contains(body, "filed no request") {
		t.Errorf("filed %q: %q", title, body)
	}
}

// A prompt, Stop or Off while the job is on its way stops the step: the job
// is told it isn't wanted and the task goes back. A tick working from an
// older reading never revives a stopped step. Busy screens don't count as
// failed tries.
func TestAutopilotStopsWhileStarting(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	a.tick(2 * time.Minute)
	st := a.step()
	if st.state != StepStarting {
		t.Fatalf("step = %+v", st)
	}
	if ok, _ := a.s.AutopilotStepWanted(a.ctx, a.admin, st.id, st.jobID.Int64); !ok {
		t.Fatal("the job isn't wanted before anything happened")
	}
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "wait, one more thing" })}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.s.AutopilotStepWanted(a.ctx, a.admin, st.id, st.jobID.Int64); ok {
		t.Error("the job is still wanted after the person prompted")
	}
	if d := a.get(a.admin, second.Key); d.ClaimedBy != "" {
		t.Errorf("the reservation stayed: %q", d.ClaimedBy)
	}
	a.jobDone(1, AutopilotJobResult{Outcome: "unwanted"})
	if st := a.step(); st.state != StepStopped {
		t.Fatalf("after the unwanted answer: %+v", st)
	}
	old := *st
	old.state = StepDue
	_, _ = a.s.db.W.Exec(`UPDATE runners SET last_seen_at = 0`)
	if err := a.s.advanceStep(a.ctx, &old); err != nil {
		t.Fatal(err)
	}
	if st := a.step(); st.state != StepStopped {
		t.Fatalf("a stale advance revived the step: %+v", st)
	}

	a.setClock(a.clockNow().Add(time.Second))
	a.autopilotStarted(a.session)
	a.stopWith("Done again.")
	a.tick(autopilotIdleHold + time.Minute)
	for i := 0; i < autopilotJobTries+1; i++ {
		if i > 0 {
			a.tick(3 * time.Minute)
		}
		st := a.step()
		if st.state != StepStarting {
			t.Fatalf("round %d: %+v", i, st)
		}
		a.jobDone(len(a.jobs), AutopilotJobResult{Outcome: "held", Reason: "draft", Detail: "a draft"})
	}
	if st := a.step(); st.state != StepWaiting || st.attempts != 0 {
		t.Errorf("after held answers: %+v", st)
	}
}

// A restart whose new session came up but never got its prompt asks the
// person to give that session the task, not to start Claude Code.
func TestAutopilotRestartedWithoutItsPrompt(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Shipped the first one.")
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	a.jobDone(1, AutopilotJobResult{Outcome: "cleared", Via: "restart", NewCLI: "cli-2", Detail: "it wasn't ready for its prompt within 1m30s"})
	if st := a.step(); st.state != StepFailed || st.detail != "restarted the session, but it wasn't ready for its prompt within 1m30s" {
		t.Fatalf("step = %+v", st)
	}
}

// finishIn takes and finishes the task in the session, taking d, with
// that comment.
func (a *apEnv) finishIn(key string, d time.Duration, comment string) {
	a.t.Helper()
	if _, err := a.s.AgentTake(a.ctx, a.ag, key, ""); err != nil {
		a.t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(d))
	if _, err := a.s.AgentDone(a.ctx, a.ag, key, AgentDoneInput{Comment: comment}); err != nil {
		a.t.Fatal(err)
	}
}

// moveOn lets the finished session's step start the next task in a
// restarted session, and works there from then on. It returns the task.
func (a *apEnv) moveOn() string {
	a.t.Helper()
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	return a.moveOnStarted()
}

// moveOnStarted is moveOn once the step is starting.
func (a *apEnv) moveOnStarted() string {
	a.t.Helper()
	if st := a.step(); st.state != StepStarting {
		a.t.Fatalf("not starting: %+v", st)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[len(a.jobs)-1].Input, &in)
	cli := fmt.Sprintf("cli-%d", len(a.jobs)+1)
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", cli, "SessionStart", func(h *HookEvent) { h.Source = "startup" })})
	if err != nil {
		a.t.Fatal(err)
	}
	a.jobDone(len(a.jobs), AutopilotJobResult{Outcome: "started", Via: "restart", NewCLI: cli})
	a.cli, a.session, a.ag.Session = cli, res.Session, res.Session
	return in.Key
}

// A task an agent finished is never started again, however the queue
// offers it; the person hears once.
func TestAutopilotNeverRepeatsAFinishedTask(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.finish(first.Key)
	if _, err := a.s.db.W.Exec(`UPDATE issues SET status_id = (SELECT id FROM statuses WHERE project_id = ? AND category = 'todo' LIMIT 1), resolved_at = NULL WHERE id = ?`, a.p.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	if st := a.step(); len(a.jobs) != 0 || st.state != StepWaiting || st.reason != WaitNothing || st.notified != WaitRepeat {
		t.Fatalf("offered the finished task again: jobs %d, step %+v", len(a.jobs), st)
	}
	var body string
	_ = a.s.db.R.QueryRow(`SELECT body FROM notifications WHERE user_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, a.admin.User.ID, NotifyAutopilot).Scan(&body)
	if !strings.Contains(body, first.Key) || !strings.Contains(body, "already finished") {
		t.Errorf("notification = %q", body)
	}
	if _, err := a.s.UpdateIssue(a.ctx, a.admin, first.Key, IssuePatch{Status: ptrOf(CatBacklog)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.UpdateIssue(a.ctx, a.admin, first.Key, IssuePatch{Status: ptrOf(CatTodo)}); err != nil {
		t.Fatal(err)
	}
	a.tick(5 * time.Minute)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || in.Key != first.Key {
		t.Fatalf("after it was sent back: %+v", a.jobs)
	}
}

// Tasks churned through, three in a row (each finished within minutes, no
// pull request, and nothing to do by the agent's word), turn autopilot off
// in the project until the person turns it back on. Quick tasks that ship
// a pull request, or that say nothing suspicious, don't count.
func TestAutopilotTurnsItselfOffOnChurn(t *testing.T) {
	a := newAPEnv(t)
	for n := range 10 {
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: fmt.Sprintf("Task %d", n), Agent: "claude"})
	}
	a.finish(a.p.Key + "-1")
	for n := range 3 {
		key := a.moveOn()
		i := a.get(a.admin, key)
		if _, err := a.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			i.ID, "forgejo", "pull", fmt.Sprintf("https://git.example/acme/app/pulls/%d", n+1), "acme/app", n+1, "open", a.clockNow().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		a.finishIn(key, time.Minute, "Nothing to do here, already fixed.")
	}
	a.finishIn(a.moveOn(), time.Minute, "Answered on the task.")
	for range 2 {
		a.finishIn(a.moveOn(), time.Minute, "This was already done in #40; closing.")
	}
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	if st := a.step(); st.state != StepStarting {
		t.Fatalf("two churned tasks after a real one stopped it: %+v", st)
	}
	key := a.moveOnStarted()
	a.finishIn(key, time.Minute, "No changes needed: it is already in place.")
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	st := a.step()
	if st.state != StepStopped || st.reason != StopLoop || !strings.Contains(st.detail, "looked needless") || !strings.Contains(st.detail, `it said "already done"`) {
		t.Fatalf("after three churned tasks: %+v", st)
	}
	if on, _, _ := a.s.autopilotFor(a.ctx, a.s.db.R, a.admin.User.ID, a.p.ID); on {
		t.Error("autopilot is still on")
	}
	var title string
	_ = a.s.db.R.QueryRow(`SELECT title FROM notifications WHERE user_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, a.admin.User.ID, NotifyAutopilot).Scan(&title)
	if title != "Autopilot turned itself off in "+a.p.Key {
		t.Errorf("notification = %q", title)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(true)}); err != nil {
		t.Fatal(err)
	}
	if a.moveOn() == "" {
		t.Fatal("no task after turning it back on")
	}
}

// Work that takes longer still stops at DefaultAutopilotPerHour an hour, and
// goes on as the hour rolls.
func TestAutopilotStartsAtMostSoManyAnHour(t *testing.T) {
	a := newAPEnv(t)
	for n := range DefaultAutopilotPerHour + 2 {
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: fmt.Sprintf("Task %d", n), Agent: "claude"})
	}
	a.finish(a.p.Key + "-1")
	var first int64
	for n := range DefaultAutopilotPerHour {
		key := a.moveOn()
		if n == 0 {
			first = a.clockNow().UnixMilli()
		}
		a.finishIn(key, autopilotQuickRun+time.Second, "This was already done.")
	}
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	st := a.step()
	if len(a.jobs) != DefaultAutopilotPerHour || st.state != StepWaiting || st.reason != WaitRate || st.dueAt != first+time.Hour.Milliseconds() {
		t.Fatalf("after %d starts: jobs %d, step %+v (want due %d)", DefaultAutopilotPerHour, len(a.jobs), st, first+time.Hour.Milliseconds())
	}
	a.setClock(time.UnixMilli(st.dueAt))
	a.tick(time.Second)
	if st := a.step(); st.state != StepStarting {
		t.Fatalf("after the hour: %+v", st)
	}
}

// With nothing ready because the safety screen can't read the tasks (the AI
// key's day is spent), the Stop doesn't promise a start, and the step says
// why and tells the person once.
func TestAutopilotSaysWhyNothingIsReady(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.s.LLM = &fakeLLM{answer: `{"nonsense": true}`}
	if err := a.s.EnsureDefaultAI(a.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	a.s.bg.Wait()
	writer := agentActor(a.admin, "anthropic/claude-opus-5-5")
	a.issue(writer, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.s.bg.Wait()
	a.s.parkAI(1, nextUTCMidnight(a.clockNow()))
	a.finish(first.Key)
	res := a.stopWith("Shipped it.")
	want := "1 task waits for the safety screen (the AI key's daily request limit is used up; it resets at 00:00 UTC)"
	if !strings.Contains(res.Note, "No task is ready yet: "+want) || strings.Contains(res.Note, "about a minute") {
		t.Errorf("note = %q", res.Note)
	}
	a.tick(61 * time.Second)
	st := a.step()
	if len(a.jobs) != 0 || st.state != StepWaiting || st.reason != WaitNothing || !strings.HasSuffix(st.detail, ": "+want) {
		t.Fatalf("jobs %d, step %+v", len(a.jobs), st)
	}
	a.tick(5 * time.Minute)
	var n int
	var body string
	_ = a.s.db.R.QueryRow(`SELECT count(*), max(body) FROM notifications WHERE user_id = ? AND kind = ?`, a.admin.User.ID, NotifyAutopilot).Scan(&n, &body)
	if n != 1 || !strings.Contains(body, want) {
		t.Errorf("%d notifications: %q", n, body)
	}
}

// A session that finished its task but holds another has no step: the
// next task doesn't start there, and its Stop says why rather than
// nothing (WP-720).
func TestAutopilotSaysASessionHoldsAnotherTask(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other", Agent: "claude"})
	a.finish(first.Key)
	if _, err := a.s.AgentTake(a.ctx, a.ag, other.Key, ""); err != nil {
		t.Fatal(err)
	}
	res := a.stopWith("Shipped the first one.")
	if want := "Autopilot: " + first.Key + " is done, but this session still holds " + other.Key; !strings.HasPrefix(res.Note, want) {
		t.Errorf("note = %q, want %q…", res.Note, want)
	}
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE session_id = ? AND state IN (?, ?, ?)`, a.session, StepDue, StepWaiting, StepStarting).Scan(&n)
	if n != 0 {
		t.Errorf("%d live steps for a session holding a task", n)
	}
}

// A session that holds the epic its finished task came from still gets the
// task's review round: the epic's run isn't another task (WP-730). The
// epic is taken before its sub-task exists, so it opens a run.
func TestAutopilotReviewsASubTaskWhileTheEpicIsHeld(t *testing.T) {
	a := reviewAPEnv(t)
	epic := a.issue(a.admin, IssueInput{Project: a.p.Key, Type: "epic", Title: "Epic", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, epic.Key, ""); err != nil {
		t.Fatal(err)
	}
	child := a.issue(a.admin, IssueInput{Project: a.p.Key, Parent: epic.Key, Title: "Slice", Agent: "claude"})
	a.setClock(a.clockNow().Add(time.Minute))
	a.finish(child.Key)
	a.linkPR(child.ID, 7, "aaa")
	res := a.stopWith("Shipped as #7.")
	if !strings.Contains(res.Note, "Its adversarial review comes next") {
		t.Errorf("note = %q", res.Note)
	}
	st := a.step()
	if st.state != StepDue || !st.reviewID.Valid {
		t.Fatalf("step = %+v", st)
	}
	if r := a.round(st.reviewID.Int64); r.state != RoundPending {
		t.Errorf("round = %+v", r)
	}
}

// The job names what other sessions on the machine still have, so the
// runner's worktree sweep leaves it alone (WP-ADR57).
func TestAutopilotJobNamesWorkOtherSessionsHold(t *testing.T) {
	a := newAPEnv(t)
	a.setLimits(AutopilotLimits{AtOnce: ptrOf(0)})
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other", Agent: "claude", Priority: "high"})
	elsewhere := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Elsewhere", Agent: "claude", Priority: "high"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Next", Agent: "claude"})
	a.finish(first.Key)
	take := func(machine, cli, cwd, key string) {
		res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook(machine, cli, "SessionStart", func(h *HookEvent) { h.Cwd = cwd })})
		if err != nil {
			t.Fatal(err)
		}
		ag := *a.ag
		ag.Session = res.Session
		if _, err := a.s.AgentTake(a.ctx, &ag, key, ""); err != nil {
			t.Fatal(err)
		}
	}
	take("box", "cli-other", "/work/app/.claude/worktrees/other", other.Key)
	take("laptop", "cli-away", "/home/me/app", elsewhere.Key)
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if len(in.Held) != 1 || in.Held[0].Key != other.Key || in.Held[0].Cwd != "/work/app/.claude/worktrees/other" {
		t.Errorf("held = %+v", in.Held)
	}
}

// A hold that can't be read makes the whole list unknown: a shorter list
// would let the sweep remove the worktree of work still held (WP-ADR57).
func TestAutopilotHeldIsUnknownWhenARowCantBeRead(t *testing.T) {
	for name, issueID := range map[string]any{"its task can't be found": 999999, "the row can't be scanned": "garbage"} {
		t.Run(name, func(t *testing.T) {
			a := newAPEnv(t)
			a.setLimits(AutopilotLimits{AtOnce: ptrOf(0)})
			first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
			other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other", Agent: "claude", Priority: "high"})
			a.finish(first.Key)
			res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", "cli-other", "SessionStart", func(h *HookEvent) { h.Cwd = "/work/other" })})
			if err != nil {
				t.Fatal(err)
			}
			ag := *a.ag
			ag.Session = res.Session
			if _, err := a.s.AgentTake(a.ctx, &ag, other.Key, ""); err != nil {
				t.Fatal(err)
			}
			if held, err := a.s.autopilotHeld(a.ctx, &AgentSession{Machine: "box"}); err != nil || len(held) == 0 {
				t.Fatalf("before: %+v %v", held, err)
			}
			conn, err := a.s.db.W.Conn(a.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`PRAGMA foreign_keys = OFF`, nil},
				{`UPDATE agent_runs SET issue_id = ? WHERE issue_id = ? AND ended_at IS NULL`, []any{issueID, other.ID}},
				{`PRAGMA foreign_keys = ON`, nil},
			} {
				if _, err := conn.ExecContext(a.ctx, q.sql, q.args...); err != nil {
					t.Fatal(err)
				}
			}
			if held, err := a.s.autopilotHeld(a.ctx, &AgentSession{Machine: "box"}); err == nil {
				t.Errorf("held = %+v, no error", held)
			}
		})
	}
}

// Worktrees the runner left in place reach the person, whatever became of
// the start (WP-ADR57).
func TestAutopilotTellsWhichWorktreesStayed(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	a.jobDone(1, AutopilotJobResult{Outcome: "started", Via: "restart", NewCLI: "cli-2", Kept: []string{"/work/app/.claude/worktrees/x (worktree-x): 1 commit not pushed anywhere"}})
	var title, body string
	_ = a.s.db.R.QueryRow(`SELECT title, body FROM notifications WHERE user_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, a.admin.User.ID, NotifyAutopilot).Scan(&title, &body)
	if title != "Autopilot left a worktree in place" || !strings.Contains(body, "1 commit not pushed anywhere") || !strings.Contains(body, "nothing is lost") {
		t.Errorf("%q: %q", title, body)
	}
}

// Work the runner found that nothing has shipped reaches the person (WP-ADR74).
func TestAutopilotTellsUnshippedWork(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	a.jobDone(1, AutopilotJobResult{Outcome: "started", Via: "restart", NewCLI: "cli-2", Unshipped: []string{"stash@{0} [WP-4]: On x: y"}})
	var title, body string
	_ = a.s.db.R.QueryRow(`SELECT title, body FROM notifications WHERE user_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, a.admin.User.ID, NotifyAutopilot).Scan(&title, &body)
	if title != "1 piece of work isn't shipped" || !strings.Contains(body, "stash@{0} [WP-4]: On x: y") || !strings.Contains(body, "nothing is lost") {
		t.Errorf("%q: %q", title, body)
	}
}

// A background task reporting back to a finished session (WP-700): no
// person's prompt, so the next task isn't held for the person and the
// session doesn't read as attended. Only a job already about to type into
// the session stands down, since the session is busy again.
func TestAutopilotHarnessPromptIsNoPersons(t *testing.T) {
	const note = "<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>"
	setup := func(t *testing.T) (*apEnv, *IssueDetail) {
		a := newAPEnv(t)
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
		a.finish(first.Key)
		return a, first
	}
	typedAt := func(a *apEnv) sql.NullInt64 {
		var at sql.NullInt64
		if err := a.s.db.R.QueryRow(`SELECT typed_at FROM agent_sessions WHERE id = ?`, a.session).Scan(&at); err != nil {
			a.t.Fatal(err)
		}
		return at
	}

	t.Run("after the task, before its Stop", func(t *testing.T) {
		a, _ := setup(t)
		a.prompt(note)
		a.stopWith("All done.")
		if st := a.step(); st.state != StepDue || st.reason == WaitPrompted {
			t.Fatalf("step = %+v", st)
		}
		if at := typedAt(a); at.Valid {
			t.Fatalf("typed_at = %v", at)
		}
	})

	t.Run("while the step is due", func(t *testing.T) {
		a, _ := setup(t)
		a.stopWith("All done.")
		a.prompt(note)
		a.stopWith("Nothing more.")
		a.tick(2 * time.Minute)
		if len(a.jobs) != 1 {
			t.Fatalf("jobs = %+v", a.jobs)
		}
	})

	t.Run("the job is out", func(t *testing.T) {
		a, _ := setup(t)
		a.stopWith("All done.")
		a.tick(61 * time.Second)
		if st := a.step(); st.state != StepStarting {
			t.Fatalf("step = %+v", st)
		}
		a.prompt(note)
		if ok, err := a.s.AutopilotStepWanted(a.ctx, a.admin, a.step().id, a.step().jobID.Int64); err != nil || ok {
			t.Fatalf("wanted = %v, %v", ok, err)
		}
	})

	t.Run("a person's prompt still counts", func(t *testing.T) {
		a, _ := setup(t)
		a.prompt("and fix the docs")
		if res := a.stopWith("All done."); strings.Contains(res.Note, "Autopilot") {
			t.Fatalf("note = %q", res.Note)
		}
		if at := typedAt(a); !at.Valid {
			t.Fatal("the person's prompt left typed_at unset")
		}
	})
}

// One call turns autopilot off in every project it was on for.
func TestTurnOffAutopilotEverywhere(t *testing.T) {
	a := newAPEnv(t)
	other := a.project(a.admin, "Other", "")
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, other.Key, AutopilotSettingInput{Enabled: ptrOf(true), Merge: true}); err != nil {
		t.Fatal(err)
	}
	o, err := a.s.TurnOffAutopilot(a.ctx, a.admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range o.Projects {
		if p.Enabled {
			t.Errorf("autopilot is still on in %s", p.ProjectKey)
		}
	}
}
