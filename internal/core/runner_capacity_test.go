package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPollTakesOnlyTheLanesItAsksFor(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "devhost", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}}
		r.Features = []string{FeatureAudit}
	})
	audit, _ := json.Marshal(AgentJob{CLI: "cursor", Prompt: "audit", Repo: "https://x.test/a/b.git"})
	cmd := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make check", "/work")})
	au := e.enqueue(e.admin, RunnerJobInput{Kind: JobAudit, Input: audit})

	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{}}); w.Job != nil {
		t.Fatalf("no lanes took job %d", w.Job.ID)
	}
	// Busy: light jobs only, so the audit goes ahead of the older command.
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneLight}}); w.Job == nil || w.Job.ID != au.ID {
		t.Fatalf("light lane took %+v, want the audit %d", w.Job, au.ID)
	}
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneLight}}); w.Job != nil {
		t.Fatalf("light lane took the command %d", w.Job.ID)
	}
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneHeavy, LaneLight}}); w.Job == nil || w.Job.ID != cmd.ID {
		t.Fatalf("both lanes took %+v, want the command %d", w.Job, cmd.ID)
	}
}

func TestRunnerShowsItsLastCapacityReport(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "devhost", nil)
	busy := time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC)
	e.poll(e.admin, r, RunnerPoll{Capacity: &RunnerCapacity{Limit: 9, Max: 6, Level: "busy", Reason: "load 1.8 per CPU\n" + strings.Repeat("x", 300), CPUs: 8, Load: 1.8, MemFree: 0.7, BusyAt: []time.Time{busy}, Heavy: 2}})
	got, err := e.s.GetRunner(e.ctx, e.admin, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Capacity
	if c == nil || c.Level != "busy" || c.Limit != 6 || c.Max != 6 || c.Heavy != 2 || len(c.BusyAt) != 1 || !c.BusyAt[0].Equal(busy) {
		t.Fatalf("capacity = %+v, want the report, limit kept within max", c)
	}
	if strings.Contains(c.Reason, "\n") || len(c.Reason) > 200 {
		t.Errorf("reason not cleaned: %q", c.Reason)
	}
	// A report of a level it doesn't know is dropped; the last one stays.
	e.poll(e.admin, r, RunnerPoll{Capacity: &RunnerCapacity{Level: "melting"}})
	if got, _ = e.s.GetRunner(e.ctx, e.admin, r.ID); got.Capacity == nil || got.Capacity.Level != "busy" {
		t.Errorf("capacity after a bad report = %+v", got.Capacity)
	}
}

// A request's jobs have a lane of their own (WP-503); a runner that doesn't
// ask for it takes them in their kind's lane.
func TestRequestJobsHaveTheirOwnLane(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "devhost", func(r *RunnerRegistration) { r.Features = []string{FeatureRequestLane} })
	agent := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make check", "/work"), Source: "babysit"})
	run := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("sqlite3 x.db", "/work"), Source: "request_cell"})
	change := e.enqueue(e.admin, RunnerJobInput{Kind: JobApply, Input: func() json.RawMessage {
		b, _ := json.Marshal(ApplyJob{Dir: "/work", Files: []FileChange{{Path: "a.txt", Content: "a"}}})
		return b
	}(), Source: "request_apply"})

	// Overloaded with a job running: only the request lane.
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneRequest}}); w.Job == nil || w.Job.ID != run.ID || w.Job.Lane != LaneRequest {
		t.Fatalf("request lane took %+v, want the command %d in the request lane", w.Job, run.ID)
	}
	// The heavy lane (an older runner, or the request lane full) takes a
	// request's job too, counted as heavy; the older agent job goes first.
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneHeavy}}); w.Job == nil || w.Job.ID != agent.ID || w.Job.Lane != LaneHeavy {
		t.Fatalf("heavy lane took %+v, want the agent job %d", w.Job, agent.ID)
	}
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneHeavy, LaneLight}}); w.Job == nil || w.Job.ID != change.ID || w.Job.Lane != LaneHeavy {
		t.Fatalf("heavy and light lanes took %+v, want the change %d as heavy", w.Job, change.ID)
	}
}

// An inbox message (a review's compaction) has a lane of its own (WP-886):
// two hung audits kept a review's /compact queued for 38 minutes, and
// neither the governor, audits, builds nor approved commands may hold one.
func TestInboxMessagesHaveTheirOwnLane(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "devhost", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}}
		r.Features = []string{FeatureAudit, FeatureRequestLane, FeatureMessageLane}
	})
	audit, _ := json.Marshal(AgentJob{CLI: "cursor", Prompt: "audit", Repo: "https://x.test/a/b.git"})
	au := e.enqueue(e.admin, RunnerJobInput{Kind: JobAudit, Input: audit})
	run := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("sqlite3 x.db", "/work"), Source: "request_cell"})
	compact, _ := json.Marshal(MessageJob{CLIID: "abc", Compact: true})
	msg := e.enqueue(e.admin, RunnerJobInput{Kind: JobMessage, Input: compact, Source: reviewCompactSource, SourceID: 7})

	// Every other lane open: none of them takes the message.
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneRequest}}); w.Job == nil || w.Job.ID != run.ID {
		t.Fatalf("request lane took %+v, want the command %d", w.Job, run.ID)
	}
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneHeavy, LaneLight, LaneRequest}}); w.Job == nil || w.Job.ID != au.ID {
		t.Fatalf("heavy, light and request lanes took %+v, want the audit %d", w.Job, au.ID)
	}
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneHeavy, LaneLight, LaneRequest}}); w.Job != nil {
		t.Fatalf("heavy, light and request lanes took job %d (%s)", w.Job.ID, w.Job.Kind)
	}
	if got := e.s.jobWaiting(e.ctx, msg.ID); got != "devhost is online and should take it within seconds. This job is next." {
		t.Errorf("message waits: %q", got)
	}
	// Overloaded, the request lane full: the message lane is still open.
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneMessage}}); w.Job == nil || w.Job.ID != msg.ID || w.Job.Lane != LaneMessage {
		t.Fatalf("message lane took %+v, want the compaction %d in the message lane", w.Job, msg.ID)
	}

	// A runner whose wp has only the request lane (WP-503) takes it there;
	// one older still, as a light job.
	if l, ok := claimLane(&Runner{Features: []string{FeatureRequestLane}}, jobNeeds{kind: JobMessage, source: cacheWarmSource}, []string{LaneRequest}); !ok || l != LaneRequest {
		t.Errorf("request-lane runner: lane %q, %v", l, ok)
	}
	if l, ok := claimLane(&Runner{}, jobNeeds{kind: JobMessage, source: cacheWarmSource}, nil); !ok || l != LaneLight {
		t.Errorf("older runner: lane %q, %v", l, ok)
	}
	// A full message lane doesn't spill into another: it frees in seconds.
	if _, ok := claimLane(&Runner{Features: []string{FeatureRequestLane, FeatureMessageLane}}, jobNeeds{kind: JobMessage, source: reviewCompactSource}, []string{LaneHeavy, LaneLight, LaneRequest}); ok {
		t.Error("a message was taken outside its lane")
	}
}

// The job that types a review round's prompt into its session runs in the
// message lane (WP-1134): in the heavy lane it queued behind babysit agents
// until the stuck sweep cancelled it, and the round ended unreviewed. The job
// that moves a session on to its next task stays heavy.
func TestReviewPromptJobsUseTheMessageLane(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "devhost", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "claude"}}
		r.AllowedDirs = []string{"/work"}
		r.Features = []string{FeatureAutopilot, FeatureRequestLane, FeatureMessageLane}
	})
	restart, _ := json.Marshal(AutopilotJob{CLIID: "abc", Dir: "/work", Key: "WP-2"})
	next := e.enqueue(e.admin, RunnerJobInput{Kind: JobAutopilot, Input: restart, Source: autopilotSource, SourceID: 1})
	review, _ := json.Marshal(AutopilotJob{CLIID: "abc", Dir: "/work", ReviewOf: "WP-1", Review: "review it"})
	rev := e.enqueue(e.admin, RunnerJobInput{Kind: JobAutopilot, Input: review, Source: autopilotReviewSource, SourceID: 2})

	// Heavy lane full: the review prompt still goes out.
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneMessage}}); w.Job == nil || w.Job.ID != rev.ID || w.Job.Lane != LaneMessage {
		t.Fatalf("message lane took %+v, want the review prompt %d", w.Job, rev.ID)
	}
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneMessage}}); w.Job != nil {
		t.Fatalf("message lane took job %d (%s)", w.Job.ID, w.Job.Kind)
	}
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneHeavy}}); w.Job == nil || w.Job.ID != next.ID || w.Job.Lane != LaneHeavy {
		t.Fatalf("heavy lane took %+v, want the restart %d", w.Job, next.ID)
	}
	// A runner whose wp has no message lane takes it as before.
	if l, ok := claimLane(&Runner{Features: []string{FeatureAutopilot}}, jobNeeds{kind: JobAutopilot, source: autopilotReviewSource}, nil); !ok || l != LaneHeavy {
		t.Errorf("older runner: lane %q, %v", l, ok)
	}
}

func TestListJobs(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"babysit round"}, "a babysit round"},
		{[]string{"audit", "babysit round", "audit"}, "2 audits and a babysit round"},
		{[]string{"audit", "review", "agent job", "kit rollout", "kit rollout"}, "an audit, a review and 3 other jobs"},
	} {
		if got := listJobs(c.in); got != c.want {
			t.Errorf("listJobs(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A queued job says which runner holds it up, why, and what is ahead of it
// (WP-503).
func TestQueuedJobSaysWhatItWaitsFor(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "devhost", func(r *RunnerRegistration) {
		r.Features = []string{FeatureRequestLane}
		r.MaxJobs = 3
	})
	why := func(id int64) string {
		t.Helper()
		j, err := e.s.GetRunnerJob(e.ctx, e.admin, id, -1)
		if err != nil {
			t.Fatal(err)
		}
		return j.Waiting
	}
	round := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make check", "/work"), Source: "babysit"})
	if got := why(round.ID); got != "devhost is online and should take it within seconds. This job is next." {
		t.Errorf("idle runner: %q", got)
	}
	over := &RunnerCapacity{Limit: 1, Max: 3, Level: "overloaded", Reason: "tasks waited for a CPU 58% of the time", Heavy: 1}
	e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneHeavy, LaneLight, LaneRequest}, Capacity: over})
	step := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work"), Source: "autopilot"})
	later := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work"), Source: "autopilot"})
	want := "devhost is overloaded (tasks waited for a CPU 58% of the time), so it runs one job at a time; it is running a babysit round."
	if got := why(step.ID); got != want+" This job is next." {
		t.Errorf("overloaded: %q", got)
	}
	if got := why(later.ID); got != want+" One job is ahead of this one." {
		t.Errorf("overloaded, second in line: %q", got)
	}
	// A request's job waits only for the other ones.
	a := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work"), Source: "request_cell"})
	b := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work"), Source: "request_cell"})
	for range 2 {
		e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneRequest}})
	}
	state := func(id int64) string {
		t.Helper()
		j, err := e.s.GetRunnerJob(e.ctx, e.admin, id, -1)
		if err != nil {
			t.Fatal(err)
		}
		return j.State
	}
	if state(a.ID) != "running" || state(b.ID) != "running" {
		t.Fatalf("request jobs %s, %s", state(a.ID), state(b.ID))
	}
	c := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work"), Source: "request_cell"})
	if got := why(c.ID); got != "devhost runs 2 approved commands, changes or session messages at once beside its other jobs; it is running 2 approved commands. This job is next." {
		t.Errorf("request lane full: %q", got)
	}
	// Busy, at the heavy limit.
	e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneLight}, Capacity: &RunnerCapacity{Limit: 1, Max: 3, Level: "busy", Reason: "load 1.8 per CPU", Heavy: 1}})
	if got := why(step.ID); got != "devhost takes 1 heavy job at once while it is busy (load 1.8 per CPU); it is running a babysit round. This job is next." {
		t.Errorf("busy: %q", got)
	}
}

func TestClaimLaneOfAnOlderRunner(t *testing.T) {
	// No lanes in the poll: a request's job is counted in its kind's lane.
	if l, ok := claimLane(&Runner{}, jobNeeds{kind: JobCommand, source: "request_cell"}, nil); !ok || l != LaneHeavy {
		t.Errorf("older runner: lane %q, %v", l, ok)
	}
	if l, ok := claimLane(&Runner{Features: []string{FeatureRequestLane}}, jobNeeds{kind: JobCommand, source: "request_cell"}, []string{LaneRequest}); !ok || l != LaneRequest {
		t.Errorf("request lane: lane %q, %v", l, ok)
	}
}

// An audit through the gateway has a lane of its own (WP-1064): two of them
// held the light lane of a --max-jobs 3 runner for hours while the machine
// sat calm and two more audits queued behind them. The runners together
// take no more of them than the gateway runs at once.
func TestGatewayAuditsHaveTheirOwnLane(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "devhost", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}}
		r.Features = []string{FeatureAudit, FeatureRequestLane, FeatureMessageLane, FeatureGatewayAudit, FeatureGatewayLane}
	})
	gw, _ := json.Marshal(AgentJob{CLI: GatewayCLI, Model: gatewayAuditModel, Prompt: "audit", Repo: "https://x.test/a/b.git"})
	cur, _ := json.Marshal(AgentJob{CLI: "cursor", Prompt: "audit", Repo: "https://x.test/a/b.git"})
	var gws []*RunnerJob
	for range GatewayTasksInFlight + 1 {
		gws = append(gws, e.enqueue(e.admin, RunnerJobInput{Kind: JobAudit, Input: gw}))
	}
	local := e.enqueue(e.admin, RunnerJobInput{Kind: JobAudit, Input: cur})

	// The light lane takes the audit a CLI on the machine runs, not the
	// gateway's.
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneHeavy, LaneLight}}); w.Job == nil || w.Job.ID != local.ID {
		t.Fatalf("heavy and light lanes took %+v, want the cursor audit %d", w.Job, local.ID)
	}
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneHeavy, LaneLight}}); w.Job != nil {
		t.Fatalf("heavy and light lanes took job %d", w.Job.ID)
	}
	// The gateway lane, with the machine overloaded (no other lane asked
	// for), takes as many as the gateway runs and then holds the rest.
	for i := range GatewayTasksInFlight {
		if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneGateway}}); w.Job == nil || w.Job.ID != gws[i].ID || w.Job.Lane != LaneGateway {
			t.Fatalf("gateway lane, poll %d: took %+v, want %d in the gateway lane", i, w.Job, gws[i].ID)
		}
	}
	last := gws[GatewayTasksInFlight].ID
	if w := e.poll(e.admin, r, RunnerPoll{Lanes: []string{LaneGateway}}); w.Job != nil {
		t.Fatalf("took audit %d past the gateway's %d at once", w.Job.ID, GatewayTasksInFlight)
	}
	if got := e.s.jobWaiting(e.ctx, last); !strings.Contains(got, "The gateway runs 6 of Waypoint's audits at once and 6 are running") {
		t.Errorf("waits: %q", got)
	}
	// Another runner (an older wp, light lane only) doesn't get past it either.
	old := e.runner(e.admin, "oldhost", func(r *RunnerRegistration) {
		r.Features = []string{FeatureAudit, FeatureGatewayAudit}
	})
	if w := e.poll(e.admin, old, RunnerPoll{Lanes: []string{LaneHeavy, LaneLight}}); w.Job != nil {
		t.Fatalf("older runner took job %d past the gateway's limit", w.Job.ID)
	}
	// One ends: the next goes.
	if _, err := e.s.FinishRunnerJob(e.ctx, e.admin, gws[0].ID, RunnerFinish{State: "failed", Error: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if w := e.poll(e.admin, old, RunnerPoll{Lanes: []string{LaneHeavy, LaneLight}}); w.Job == nil || w.Job.ID != last || w.Job.Lane != LaneLight {
		t.Fatalf("older runner took %+v, want %d in the light lane", w.Job, last)
	}

	if l, ok := claimLane(&Runner{Features: []string{FeatureGatewayAudit}}, jobNeeds{kind: JobAudit, cli: GatewayCLI}, []string{LaneLight}); !ok || l != LaneLight {
		t.Errorf("runner without the gateway lane: lane %q, %v", l, ok)
	}
	if _, ok := claimLane(&Runner{Features: []string{FeatureGatewayAudit, FeatureGatewayLane}}, jobNeeds{kind: JobAudit, cli: GatewayCLI}, []string{LaneHeavy, LaneLight}); ok {
		t.Error("an audit through the gateway was taken outside its lane")
	}
}
