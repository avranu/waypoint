package core

import (
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// newSweepEnv: apEnv with a runner that can start sessions, and the person's
// session known to work in /work/app in the project (a task done there).
func newSweepEnv(t *testing.T) *apEnv {
	a := newAPEnv(t)
	a.personsOwn()
	a.runner(a.admin, "box", func(r *RunnerRegistration) {
		r.Features = []string{FeatureAutopilot, FeatureAutopilotStart}
		r.CLIs = []RunnerCLI{{Name: "claude"}}
	})
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Worked here", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *apEnv) ownStep() *stepRow {
	a.t.Helper()
	st, err := a.s.lastOwnStart(a.ctx, a.admin.User.ID, a.p.ID)
	if err != nil {
		a.t.Fatal(err)
	}
	return st
}

func (a *apEnv) ownJobs() int {
	n := 0
	for _, j := range a.jobs {
		if j.Kind == JobAutopilotStart {
			n++
		}
	}
	return n
}

func (a *apEnv) ownDone(n int, st *stepRow, res AutopilotJobResult) {
	a.t.Helper()
	raw, _ := json.Marshal(res)
	a.res[int64(n)] = RunnerJob{Kind: JobAutopilotStart, State: "done", Result: raw}
	a.s.autopilotJobFinished(a.ctx, &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"job": int64(n), "source": autopilotSource, "source_id": st.id}})
}

// ownSession answers the n-th job with a new session and says it is at work.
func (a *apEnv) ownSession(n int, cli string) *Actor {
	a.t.Helper()
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", cli, "SessionStart", func(h *HookEvent) { h.Source = "startup"; h.Cwd = "/work/app/.claude/worktrees/x"; h.Worktree = true })})
	if err != nil {
		a.t.Fatal(err)
	}
	ag := *a.ag
	ag.Session = res.Session
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET issue_id = ? WHERE id = ?`, a.ownStep().nextIssue.Int64, res.Session); err != nil {
		a.t.Fatal(err)
	}
	a.ownDone(n, a.ownStep(), AutopilotJobResult{Outcome: "started", NewCLI: cli, Via: "start"})
	var next sql.NullInt64
	if err := a.s.db.R.QueryRow(`SELECT next_session FROM autopilot_steps WHERE id = ?`, a.ownStep().id).Scan(&next); err != nil || !next.Valid {
		a.t.Fatalf("after the job: %+v, %v", a.ownStep(), err)
	}
	return &ag
}

func (a *apEnv) running() []string {
	a.t.Helper()
	got, err := a.s.autopilotRunning(a.ctx, a.s.db.R, a.admin.User.ID, a.p.ID, 0, true, a.clockNow().UnixMilli())
	if err != nil {
		a.t.Fatal(err)
	}
	return got
}

// A ready task and room: a session of its own, one job, the task reserved,
// the step holding the place while it starts.
func TestSweepStartsASessionOfItsOwn(t *testing.T) {
	a := newSweepEnv(t)
	next := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Next", Agent: "claude"})
	a.tick(time.Minute)
	if a.ownJobs() != 1 || a.jobs[0].RunnerID == 0 || a.jobs[0].OwnerID != a.admin.User.ID {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.Key != next.Key || in.Dir != "/work/app" || in.Step == 0 {
		t.Errorf("input = %+v", in)
	}
	if d := a.get(a.admin, next.Key); d.ClaimedBy != a.ag.Agent {
		t.Errorf("the task isn't reserved: %q", d.ClaimedBy)
	}
	if st := a.ownStep(); st.state != StepStarting || st.sessionID != 0 {
		t.Fatalf("step = %+v", st)
	}
	a.tick(time.Minute)
	if a.ownJobs() != 1 {
		t.Fatalf("a second job while the first is out: %d", a.ownJobs())
	}
	steps, err := a.s.autopilotSteps(a.ctx, a.admin.User.ID, a.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 || steps[0].Session != nil || steps[0].Next == nil || steps[0].Next.Key != next.Key {
		t.Fatalf("the step as listed: %+v", steps)
	}
}

// The limit holds once a session is running; one that has gone idle with
// nothing left leaves room. At most the limit, however many sweeps.
func TestSweepKeepsToTheLimit(t *testing.T) {
	a := newSweepEnv(t)
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "One", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Two", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Three", Agent: "claude"})
	a.tick(time.Minute)
	if a.ownJobs() != 1 {
		t.Fatalf("jobs %d", a.ownJobs())
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	own := a.ownSession(1, "own-1")
	if _, err := a.s.AgentTake(a.ctx, own, in.Key, ""); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		a.tick(time.Minute)
		if a.ownJobs() != 1 {
			t.Fatalf("sweep %d started another: %d jobs, running %v", i, a.ownJobs(), a.running())
		}
	}
	if got := a.running(); len(got) != 1 {
		t.Fatalf("running = %v", got)
	}
	a.setLimits(AutopilotLimits{AtOnce: ptrOf(2)})
	a.tick(time.Minute)
	if a.ownJobs() != 2 {
		t.Fatalf("at_once 2: %d jobs", a.ownJobs())
	}
	a.tick(time.Minute)
	if a.ownJobs() != 2 {
		t.Fatalf("over the limit of 2: %d jobs", a.ownJobs())
	}
}

// Two checks at once: the claim is one transaction, so one of them wins.
func TestSweepClaimRace(t *testing.T) {
	a := newSweepEnv(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := a.s.claimOwnStart(a.ctx, a.admin.User.ID, a.p.ID, nil, a.clockNow().UnixMilli(), nil)
			if err != nil {
				t.Error(err)
			}
			if st != nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d claims won, want 1", won)
	}
}

// A start being made fills the limit for a finished autopilot session too,
// and the finished one, waiting to move on, doesn't deadlock with the sweep:
// the sweep counts it, and it doesn't count itself.
func TestFinishedSessionAndSweepShareTheLimit(t *testing.T) {
	a := newSweepEnv(t)
	x := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "One", Agent: "claude"})
	a.tick(time.Minute)
	if a.ownJobs() != 1 {
		t.Fatalf("jobs %d", a.ownJobs())
	}
	own := a.ownSession(1, "own-1")
	if _, err := a.s.AgentTake(a.ctx, own, x.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, own, x.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.SessionHook(a.ctx, own, []HookEvent{hook("box", "own-1", "Stop", func(h *HookEvent) {
		h.Cwd = "/work/app/.claude/worktrees/x"
		h.LastMessage = "Done."
		h.Pending = &HookPending{Tasks: []PendingTask{}, Crons: []PendingCron{}}
	})}); err != nil {
		t.Fatal(err)
	}
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Two", Agent: "claude"})
	before := len(a.jobs)
	a.tick(2 * time.Minute)
	if len(a.jobs) != before+1 || a.jobs[before].Kind != JobAutopilot {
		t.Fatalf("the finished session should move on and the sweep leave it: %+v", a.jobs[before:])
	}
}

func (a *apEnv) atOnce() []string {
	a.t.Helper()
	got, err := a.s.atOnceCount(a.ctx, a.s.db.R, a.admin.User.ID, a.p.ID, 0, true, a.clockNow().UnixMilli())
	if err != nil {
		a.t.Fatal(err)
	}
	return got
}

// A person's session at work on a task counts towards at_once (WP-1079),
// though not as autopilot's own; once it has shipped, it doesn't, even with
// the session still open.
func TestSweepCountsAPersonsTask(t *testing.T) {
	a := newSweepEnv(t)
	busy := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "A person's", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, busy.Key, ""); err != nil {
		t.Fatal(err)
	}
	if got := a.running(); len(got) != 0 {
		t.Fatalf("a person's session counted as autopilot's: %v", got)
	}
	if got := a.atOnce(); len(got) != 1 || !strings.HasPrefix(got[0], busy.Key+": your session") {
		t.Fatalf("at_once counts %v", got)
	}
	a.tick(time.Minute)
	if a.ownJobs() != 0 {
		t.Fatalf("started beside a person's task: %d jobs", a.ownJobs())
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, busy.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if got := a.atOnce(); len(got) != 0 {
		t.Fatalf("a shipped task still counts: %v", got)
	}
	a.tick(time.Minute)
	if a.ownJobs() != 1 {
		t.Fatalf("jobs %d", a.ownJobs())
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.Key == busy.Key {
		t.Error("took a person's task")
	}
}

// A person's session quiet for longer than the idle span stops counting,
// though it still holds its task; heard from again, it counts again. Its
// task in another project, or a session of someone else's, never counts.
func TestSweepIgnoresQuietAndOtherSessions(t *testing.T) {
	a := newSweepEnv(t)
	busy := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "A person's", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, busy.Key, ""); err != nil {
		t.Fatal(err)
	}
	if got := a.atOnce(); len(got) != 1 {
		t.Fatalf("at work: %v", got)
	}
	held, err := a.s.db.W.Exec(`INSERT INTO work_log(kind, session_id, run_id, issue_id, state, prompt, before_pr, created_at, updated_at)
		SELECT 'review', session_id, id, issue_id, 'pending', '', 'pr', ?, ? FROM agent_runs WHERE ended_at IS NULL`, a.clockNow().UnixMilli(), a.clockNow().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if got := a.atOnce(); len(got) != 0 {
		t.Fatalf("held for its review: %v", got)
	}
	round, _ := held.LastInsertId()
	if _, err := a.s.db.W.Exec(`DELETE FROM work_log WHERE id = ?`, round); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(sessionIdleAfter + time.Minute))
	if got := a.atOnce(); len(got) != 0 {
		t.Fatalf("quiet for %v: %v", sessionIdleAfter+time.Minute, got)
	}
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "go on" })}); err != nil {
		t.Fatal(err)
	}
	if got := a.atOnce(); len(got) != 1 {
		t.Fatalf("heard from again: %v", got)
	}
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", "cli-far", "SessionStart", func(h *HookEvent) { h.Source = "startup"; h.Cwd = "/work/far" })})
	if err != nil {
		t.Fatal(err)
	}
	b := *a.ag
	b.Session = res.Session
	elsewhere := a.project(a.admin, "Other", "")
	far := a.issue(a.admin, IssueInput{Project: elsewhere.Key, Title: "Elsewhere", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, &b, far.Key, ""); err != nil {
		t.Fatal(err)
	}
	if got := a.atOnce(); len(got) != 1 || !strings.HasPrefix(got[0], busy.Key+":") {
		t.Fatalf("a task in another project counted: %v", got)
	}
}

// A session autopilot is starting, that has taken its task before the job
// answers, or one bound to no step yet that the person typed in, counts
// once, as autopilot's (WP-1079 review).
func TestAtOnceCountsAStartingSessionOnce(t *testing.T) {
	a := newSweepEnv(t)
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Next", Agent: "claude"})
	a.tick(time.Minute)
	st := a.ownStep()
	if a.ownJobs() != 1 || st.state != StepStarting || !st.nextIssue.Valid {
		t.Fatalf("jobs %d, step %+v", a.ownJobs(), st)
	}
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", "own-1", "SessionStart", func(h *HookEvent) {
		h.Source = "startup"
		h.Cwd = "/work/app/.claude/worktrees/x"
		h.Worktree = true
	})})
	if err != nil {
		t.Fatal(err)
	}
	own := *a.ag
	own.Session = res.Session
	var key string
	if err := a.s.db.R.QueryRow(`SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, st.nextIssue.Int64).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.AgentTake(a.ctx, &own, key, ""); err != nil {
		t.Fatal(err)
	}
	if got := a.atOnce(); len(got) != 1 {
		t.Fatalf("starting, task taken: %v", got)
	}
	a.ownDone(1, st, AutopilotJobResult{Outcome: "started", Via: "start"})
	if got := a.atOnce(); len(got) != 1 {
		t.Fatalf("started, unbound: %v", got)
	}
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET typed_at = ? WHERE id = ?`, a.clockNow().UnixMilli()+1, own.Session); err != nil {
		t.Fatal(err)
	}
	if got := a.atOnce(); len(got) != 1 {
		t.Fatalf("typed in, just started: %v", got)
	}
	a.setClock(a.clockNow().Add(HandoffReserve + time.Minute))
	if _, err := a.s.SessionHook(a.ctx, &own, []HookEvent{hook("box", "own-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "go on" })}); err != nil {
		t.Fatal(err)
	}
	if got := a.atOnce(); len(got) != 1 || !strings.Contains(got[0], "your session") {
		t.Fatalf("typed in, past the reserve: %v", got)
	}
}

// Off, and at_once 0, start nothing.
func TestSweepOffStartsNothing(t *testing.T) {
	a := newSweepEnv(t)
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.setLimits(AutopilotLimits{AtOnce: ptrOf(0)})
	a.tick(time.Minute)
	if len(a.jobs) != 0 {
		t.Fatalf("at_once 0: %+v", a.jobs)
	}
	a.setLimits(AutopilotLimits{AtOnce: ptrOf(1)})
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false)}); err != nil {
		t.Fatal(err)
	}
	a.tick(time.Minute)
	if len(a.jobs) != 0 {
		t.Fatalf("autopilot off: %+v", a.jobs)
	}
	if st, err := a.s.claimOwnStart(a.ctx, a.admin.User.ID, a.p.ID, nil, a.clockNow().UnixMilli(), nil); err != nil || st != nil {
		t.Fatalf("claim with autopilot off: %+v, %v", st, err)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.ag, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(true)}); err == nil {
		t.Fatal("an agent turned autopilot on")
	}
}

// Turning it off while a start is out ends the step and gives the task back.
func TestSweepTurnedOffMidStart(t *testing.T) {
	a := newSweepEnv(t)
	next := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.tick(time.Minute)
	if a.ownJobs() != 1 {
		t.Fatalf("jobs %d", a.ownJobs())
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Enabled: ptrOf(false)}); err != nil {
		t.Fatal(err)
	}
	if st := a.ownStep(); st.state != StepStopped {
		t.Fatalf("step = %+v", st)
	}
	if d := a.get(a.admin, next.Key); d.ClaimedBy != "" {
		t.Errorf("still reserved for %q", d.ClaimedBy)
	}
}

// Every other limit still applies.
func TestSweepKeepsTheOtherLimits(t *testing.T) {
	spend := func(a *apEnv) {
		hour := a.clockNow().UnixMilli() - a.clockNow().UnixMilli()%3_600_000
		if _, err := a.s.db.W.Exec(`INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, output) VALUES (?,?,?,?,?,?,?)`,
			a.admin.User.ID, hour, "box", "s", "", "claude-opus-5-5", 5_000_000); err != nil {
			t.Fatal(err)
		}
	}
	for name, set := range map[string]func(*apEnv){
		"hourly spend": spend,
		"hours": func(a *apEnv) {
			at := a.clockNow().Add(3 * time.Hour).UTC()
			a.setLimits(AutopilotLimits{Hours: ptrOf(at.Format("15:04") + "-" + at.Add(time.Hour).Format("15:04"))})
		},
		"agent pause": func(a *apEnv) {
			if _, err := a.s.db.W.Exec(`INSERT INTO agent_pauses(user_id, spec, created_at) VALUES (?, 'anthropic', ?)`, a.admin.User.ID, a.clockNow().UnixMilli()); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := newSweepEnv(t)
			a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
			set(a)
			a.tick(time.Minute)
			if len(a.jobs) != 0 {
				t.Fatalf("started anyway: %+v", a.jobs)
			}
		})
	}
	t.Run("per day and spacing", func(t *testing.T) {
		a := newSweepEnv(t)
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "One", Agent: "claude"})
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Two", Agent: "claude"})
		a.setLimits(AutopilotLimits{SpacingMinutes: ptrOf(30), AtOnce: ptrOf(3)})
		a.tick(time.Minute)
		a.ownSession(1, "own-1")
		a.tick(time.Minute)
		if a.ownJobs() != 1 {
			t.Fatalf("spacing: %d jobs", a.ownJobs())
		}
		a.tick(31 * time.Minute)
		if a.ownJobs() != 2 {
			t.Fatalf("after the spacing: %d jobs", a.ownJobs())
		}
		a.ownSession(2, "own-2")
		a.setLimits(AutopilotLimits{SpacingMinutes: ptrOf(0), PerDay: ptrOf(2), AtOnce: ptrOf(3)})
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Three", Agent: "claude"})
		a.tick(time.Minute)
		if a.ownJobs() != 2 {
			t.Fatalf("per day: %d jobs", a.ownJobs())
		}
	})
}

// A failed start gives the task back and waits longer each time.
func TestSweepBacksOff(t *testing.T) {
	a := newSweepEnv(t)
	next := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.tick(time.Minute)
	a.ownDone(1, a.ownStep(), AutopilotJobResult{Outcome: "cleared", Detail: "no session showed up"})
	st := a.ownStep()
	if st.state != StepFailed || !strings.Contains(st.detail, "no session showed up") {
		t.Fatalf("step = %+v", st)
	}
	if d := a.get(a.admin, next.Key); d.ClaimedBy != "" {
		t.Errorf("still reserved for %q", d.ClaimedBy)
	}
	a.tick(time.Minute)
	if a.ownJobs() != 1 {
		t.Fatalf("no wait after a failure: %d jobs", a.ownJobs())
	}
	a.tick(5 * time.Minute)
	if a.ownJobs() != 2 {
		t.Fatalf("after the wait: %d jobs", a.ownJobs())
	}
	a.ownDone(2, a.ownStep(), AutopilotJobResult{Outcome: "held", Reason: "no_worktree", Detail: "not trusted"})
	if st := a.ownStep(); st.state != StepFailed || st.reason != "no_worktree" || st.attempts != 2 {
		t.Fatalf("step = %+v", st)
	}
	a.tick(6 * time.Minute)
	if a.ownJobs() != 2 {
		t.Fatal("the second wait isn't longer")
	}
	a.tick(5 * time.Minute)
	if a.ownJobs() != 3 {
		t.Fatalf("after the longer wait: %d jobs", a.ownJobs())
	}
}

// A person's Stop ends the start, gives the task back and holds off an hour.
func TestSweepStop(t *testing.T) {
	a := newSweepEnv(t)
	next := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.tick(time.Minute)
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, a.ownStep().id, "stop"); err != nil {
		t.Fatal(err)
	}
	if d := a.get(a.admin, next.Key); d.ClaimedBy != "" {
		t.Errorf("still reserved for %q", d.ClaimedBy)
	}
	a.tick(10 * time.Minute)
	if a.ownJobs() != 1 {
		t.Fatalf("started again right after a stop: %d", a.ownJobs())
	}
	a.tick(time.Hour)
	if a.ownJobs() != 2 {
		t.Fatalf("after an hour: %d", a.ownJobs())
	}
}

// No runner that can start one: one waiting step, told once, and it goes
// when the work does.
func TestSweepNeedsARunner(t *testing.T) {
	a := newSweepEnv(t)
	a.runner(a.admin, "box", func(r *RunnerRegistration) {
		r.Features = []string{FeatureAutopilot}
		r.CLIs = []RunnerCLI{{Name: "claude"}}
	})
	i := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.tick(time.Minute)
	a.tick(time.Minute)
	st := a.ownStep()
	if st == nil || st.state != StepWaiting || st.reason != WaitRunner || len(a.jobs) != 0 {
		t.Fatalf("step = %+v, jobs %d", st, len(a.jobs))
	}
	var steps, told int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM autopilot_steps WHERE session_id IS NULL`).Scan(&steps)
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ?`, a.admin.User.ID, NotifyAutopilot).Scan(&told)
	if steps != 1 || told != 1 {
		t.Errorf("%d steps, told %d times", steps, told)
	}
	if _, err := a.s.UpdateIssue(a.ctx, a.admin, i.Key, IssuePatch{Status: ptrOf("Done")}); err != nil {
		t.Fatal(err)
	}
	a.tick(time.Minute)
	if st := a.ownStep(); st != nil {
		t.Fatalf("nothing to start, yet: %+v", st)
	}
}

// A session that isn't in tmux can't be moved on: that step ends, not waits,
// and the sweep starts the next task in a session of its own.
func TestNoTmuxEndsAndTheSweepStarts(t *testing.T) {
	a := newSweepEnv(t)
	a.autopilotStarted(a.session)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	a.tick(61 * time.Second)
	if len(a.jobs) != 1 || a.jobs[0].Kind != JobAutopilot {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	a.jobDone(1, AutopilotJobResult{Outcome: "held", Reason: WaitNoTmux, Detail: "not in tmux"})
	if st := a.step(); st.state != StepStopped || st.reason != WaitNoTmux {
		t.Fatalf("step = %+v", st)
	}
	a.tick(time.Minute)
	if a.ownJobs() != 1 {
		t.Fatalf("own jobs %d", a.ownJobs())
	}
}

// The job kind is real: its input is checked, and only a runner with the
// feature gets it.
func TestAutopilotStartJobKind(t *testing.T) {
	raw, _ := json.Marshal(AutopilotJob{Dir: "/work/app", Key: "APP-1", Step: 1})
	n, _, _, err := checkRunnerInput(JobAutopilotStart, raw)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Online: true, Features: []string{FeatureAutopilot}, CLIs: []RunnerCLI{{Name: "claude"}}, AllowedDirs: []string{"/work"}}
	n.cli = "claude"
	if runnerCanTake(r, n) {
		t.Error("a runner without the feature can take it")
	}
	r.Features = append(r.Features, FeatureAutopilotStart)
	if !runnerCanTake(r, n) {
		t.Error("a runner with the feature can't take it")
	}
	if agent, ok := jobTokenAgent(JobAutopilotStart, raw); !ok || agent == "" {
		t.Error("no token for the job")
	}
}

// The runner's question before it starts anything works for a step with no
// session, and says no once the step was stopped.
func TestOwnStartStepWanted(t *testing.T) {
	a := newSweepEnv(t)
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.tick(time.Minute)
	st := a.ownStep()
	if got, err := a.s.AutopilotStepWanted(a.ctx, a.admin, st.id, st.jobID.Int64); err != nil || !got {
		t.Fatalf("wanted = %v, %v", got, err)
	}
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, st.id, "stop"); err != nil {
		t.Fatal(err)
	}
	if got, err := a.s.AutopilotStepWanted(a.ctx, a.admin, st.id, st.jobID.Int64); err != nil || got {
		t.Fatalf("after a stop: wanted = %v, %v", got, err)
	}
}

// WP-799's review (audit 365): a lapsed reservation that someone else has
// since taken isn't theirs to lose when the start fails.
func TestOwnStartLeavesANewHolderAlone(t *testing.T) {
	a := newSweepEnv(t)
	next := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.tick(time.Minute)
	if _, err := a.s.db.W.Exec(`UPDATE issues SET claimed_by = 'openai/codex' WHERE id = ?`, next.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, a.ownStep().id, "stop"); err != nil {
		t.Fatal(err)
	}
	if d := a.get(a.admin, next.Key); d.ClaimedBy != "openai/codex" {
		t.Errorf("claimed by %q", d.ClaimedBy)
	}
}

// Stopped too late: the session started anyway, so it counts.
func TestOwnStartStoppedTooLateStillCounts(t *testing.T) {
	a := newSweepEnv(t)
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready too", Agent: "claude"})
	a.tick(time.Minute)
	st := a.ownStep()
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, st.id, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", "late-1", "SessionStart", func(h *HookEvent) { h.Source = "startup"; h.Cwd = "/work/app/.claude/worktrees/x"; h.Worktree = true })}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET issue_id = ? WHERE cli_id = 'late-1'`, st.nextIssue.Int64); err != nil {
		t.Fatal(err)
	}
	a.ownDone(1, st, AutopilotJobResult{Outcome: "started", NewCLI: "late-1", Via: "start"})
	var next sql.NullInt64
	if err := a.s.db.R.QueryRow(`SELECT next_session FROM autopilot_steps WHERE id = ?`, st.id).Scan(&next); err != nil || !next.Valid {
		t.Fatalf("next_session = %+v, %v", next, err)
	}
	if got := a.running(); len(got) != 1 {
		t.Errorf("running = %v", got)
	}
}

// A session Waypoint hadn't heard of when its job finished still counts
// once it has done a run on its task.
func TestOwnStartBoundLaterToItsSession(t *testing.T) {
	a := newSweepEnv(t)
	next := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.tick(time.Minute)
	st := a.ownStep()
	a.ownDone(1, st, AutopilotJobResult{Outcome: "started", NewCLI: "unheard-of", Via: "start"})
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", "unheard-of", "SessionStart", func(h *HookEvent) {
		h.Source, h.Worktree = "startup", true
		h.Cwd = "/work/app/.claude/worktrees/x"
		h.Worktree = true
	})})
	if err != nil {
		t.Fatal(err)
	}
	ag := *a.ag
	ag.Session = res.Session
	a.setClock(a.clockNow().Add(time.Second))
	if _, err := a.s.AgentTake(a.ctx, &ag, next.Key, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.s.bindOwnSessions(a.ctx, a.admin.User.ID, a.p.ID, a.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var bound sql.NullInt64
	if err := a.s.db.R.QueryRow(`SELECT next_session FROM autopilot_steps WHERE id = ?`, st.id).Scan(&bound); err != nil || !bound.Valid {
		t.Fatalf("next_session = %+v, %v", bound, err)
	}
}

// The per-day limit holds at the claim, and a claim counts as a start.
func TestOwnClaimKeepsThePace(t *testing.T) {
	a := newSweepEnv(t)
	a.setLimits(AutopilotLimits{PerDay: ptrOf(1)})
	now := a.clockNow().UnixMilli()
	claimed, err := a.s.claimOwnStart(a.ctx, a.admin.User.ID, a.p.ID, nil, now, nil)
	if err != nil || claimed == nil {
		t.Fatalf("first claim: %+v, %v", claimed, err)
	}
	_, lim, err := a.s.autopilotFor(a.ctx, a.s.db.R, a.admin.User.ID, a.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wait, _, _, err := a.s.autopilotPace(a.ctx, a.s.db.R, a.admin.User.ID, a.p.ID, lim, now, false); err != nil || wait == "" {
		t.Errorf("a claim doesn't count as today's start: %q, %v", wait, err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE autopilot_steps SET state = ? WHERE id = ?`, StepStopped, claimed.id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE autopilot_steps SET state = ?, next_issue = (SELECT min(id) FROM issues) WHERE id = ?`, StepStarted, claimed.id); err != nil {
		t.Fatal(err)
	}
	if again, err := a.s.claimOwnStart(a.ctx, a.admin.User.ID, a.p.ID, nil, now, nil); err != nil || again != nil {
		t.Errorf("claimed past the day's limit: %+v, %v", again, err)
	}
}

// A person's own worktree session that started after the step, on another
// task, isn't the autopilot's, even when the job reports a start.
func TestOwnStartStoppedTooLateIgnoresOtherSessions(t *testing.T) {
	a := newSweepEnv(t)
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Ready", Agent: "claude"})
	a.tick(time.Minute)
	st := a.ownStep()
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, st.id, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", "late-1", "SessionStart", func(h *HookEvent) { h.Source = "startup"; h.Cwd = "/work/app/.claude/worktrees/x"; h.Worktree = true })}); err != nil {
		t.Fatal(err)
	}
	a.ownDone(1, st, AutopilotJobResult{Outcome: "started", NewCLI: "late-1", Via: "start"})
	var next sql.NullInt64
	if err := a.s.db.R.QueryRow(`SELECT next_session FROM autopilot_steps WHERE id = ?`, st.id).Scan(&next); err != nil || next.Valid {
		t.Fatalf("next_session = %+v, %v", next, err)
	}
}
