package core

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func (e *env) runner(a *Actor, name string, mod func(*RunnerRegistration)) *Runner {
	e.t.Helper()
	reg := RunnerRegistration{Name: name, Hostname: name, OS: "linux", Arch: "amd64", Version: "test", AllowedDirs: []string{"/work"}}
	if mod != nil {
		mod(&reg)
	}
	r, err := e.s.RegisterRunner(e.ctx, a, reg)
	if err != nil {
		e.t.Fatalf("register %s: %v", name, err)
	}
	return r
}

func cmdInput(command, dir string) json.RawMessage {
	b, _ := json.Marshal(CommandJob{Command: command, Dir: dir})
	return b
}

func (e *env) enqueue(a *Actor, in RunnerJobInput) *RunnerJob {
	e.t.Helper()
	j, err := e.s.EnqueueRunnerJob(e.ctx, a, in)
	if err != nil {
		e.t.Fatalf("enqueue: %v", err)
	}
	return j
}

func (e *env) poll(a *Actor, r *Runner, in RunnerPoll) *RunnerWork {
	e.t.Helper()
	in.Runner = r.ID
	if in.Wait == nil {
		zero := 0.0
		in.Wait = &zero
	}
	w, err := e.s.RunnerPoll(e.ctx, a, in)
	if err != nil {
		e.t.Fatalf("poll: %v", err)
	}
	return w
}

func TestRegisterRunnerIsIdempotentPerOwnerAndName(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	r1 := e.runner(e.admin, "laptop", func(r *RunnerRegistration) { r.Labels = []string{"gpu"} })
	r2 := e.runner(e.admin, "laptop", func(r *RunnerRegistration) { r.Labels = []string{"gpu", "mac"}; r.Version = "v2" })
	if r1.ID != r2.ID || r2.Version != "v2" || len(r2.Labels) != 2 {
		t.Errorf("same owner+name should update one runner: %+v %+v", r1, r2)
	}
	if r3 := e.runner(bob, "laptop", nil); r3.ID == r1.ID {
		t.Error("another person's runner of the same name is its own runner")
	}
	if !r2.Online || r2.LastSeenAt == nil {
		t.Errorf("registering should mark it online: %+v", r2)
	}
	for _, bad := range []func(*RunnerRegistration){
		func(r *RunnerRegistration) { r.AllowedDirs = []string{"relative/dir"} },
		func(r *RunnerRegistration) { r.AllowedDirs = []string{"/work/../etc"} },
		func(r *RunnerRegistration) { r.AllowedDirs = []string{"/"} },
		func(r *RunnerRegistration) { r.Labels = []string{"has space"} },
		func(r *RunnerRegistration) { r.Name = "  " },
	} {
		reg := RunnerRegistration{Name: "x", AllowedDirs: []string{"/work"}}
		bad(&reg)
		if _, err := e.s.RegisterRunner(e.ctx, e.admin, reg); !IsInvalid(err) {
			t.Errorf("accepted %+v: %v", reg, err)
		}
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-code"}
	if _, err := e.s.RegisterRunner(e.ctx, agent, RunnerRegistration{Name: "agentbox"}); err == nil {
		t.Error("an agent registered a runner")
	}
	ro := &Actor{User: e.admin.User, ReadOnly: true}
	if _, err := e.s.RegisterRunner(e.ctx, ro, RunnerRegistration{Name: "ro"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("read-only token: %v", err)
	}
}

func TestRunnerJobsRunOnlyOnTheirOwnersRunners(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	mine := e.runner(e.admin, "mine", nil)
	theirs := e.runner(bob, "theirs", nil)

	if _, err := e.s.EnqueueRunnerJob(e.ctx, e.admin, RunnerJobInput{Kind: JobCommand, RunnerID: theirs.ID, Input: cmdInput("true", "/work")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("job on another person's runner by id: %v", err)
	}
	if _, err := e.s.EnqueueRunnerJob(e.ctx, e.admin, RunnerJobInput{Kind: JobCommand, OwnerID: bob.User.ID, Input: cmdInput("true", "/work")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("job for another owner: %v", err)
	}
	auto := &Actor{User: e.admin.User, Automation: &AutomationRef{ID: 1, Name: "nightly"}}
	if _, err := e.s.EnqueueRunnerJob(e.ctx, auto, RunnerJobInput{Kind: JobCommand, OwnerID: bob.User.ID, Input: cmdInput("true", "/work"), Source: "automation_step", SourceID: 9}); !errors.Is(err, ErrForbidden) {
		t.Errorf("automation job on another person's runner: %v", err)
	}
	j := e.enqueue(bob, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work"), Source: "automation_step", SourceID: 9})
	if w := e.poll(e.admin, mine, RunnerPoll{}); w.Job != nil {
		t.Errorf("robin's runner took bob's job: %+v", w.Job)
	}
	w := e.poll(bob, theirs, RunnerPoll{})
	if w.Job == nil || w.Job.ID != j.ID || w.Job.Kind != JobCommand {
		t.Fatalf("bob's runner should take it: %+v", w)
	}
	if _, err := e.s.RunnerPoll(e.ctx, bob, RunnerPoll{Runner: mine.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("polling someone else's runner: %v", err)
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-code"}
	if _, err := e.s.EnqueueRunnerJob(e.ctx, agent, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent enqueued: %v", err)
	}
	carol := e.user("carol", false)
	if _, err := e.s.EnqueueRunnerJob(e.ctx, carol, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work")}); !errors.Is(err, ErrNoRunner) {
		t.Errorf("no runner: %v", err)
	}
	if _, err := e.s.EnqueueRunnerJob(e.ctx, e.admin, RunnerJobInput{Kind: JobCommand, Label: "gpu", Input: cmdInput("true", "/work")}); !errors.Is(err, ErrNoRunner) {
		t.Errorf("no runner with the label: %v", err)
	}
}

func TestRunnerJobInputIsChecked(t *testing.T) {
	e := newEnv(t)
	e.runner(e.admin, "mine", func(r *RunnerRegistration) {
		r.AllowedDirs = []string{"/work/proj"}
		r.CLIs = []RunnerCLI{{Name: "claude"}}
		r.Features = []string{FeatureConfine}
	})
	apply := func(f FileChange, dir string) json.RawMessage {
		b, _ := json.Marshal(ApplyJob{Dir: dir, Files: []FileChange{f}})
		return b
	}
	agent := func(a AgentJob) json.RawMessage { b, _ := json.Marshal(a); return b }
	for name, in := range map[string]RunnerJobInput{
		"dir outside the allowed ones":  {Kind: JobCommand, Input: cmdInput("ls", "/etc")},
		"dir with ..":                   {Kind: JobCommand, Input: cmdInput("ls", "/work/proj/../../etc")},
		"sibling with the same prefix":  {Kind: JobCommand, Input: cmdInput("ls", "/work/project")},
		"relative dir":                  {Kind: JobCommand, Input: cmdInput("ls", "proj")},
		"empty command":                 {Kind: JobCommand, Input: cmdInput("  ", "/work/proj")},
		"apply path escape":             {Kind: JobApply, Input: apply(FileChange{Path: "../x", Content: "y"}, "/work/proj")},
		"apply into .git":               {Kind: JobApply, Input: apply(FileChange{Path: ".git/hooks/pre-commit", Content: "y"}, "/work/proj")},
		"apply absolute path":           {Kind: JobApply, Input: apply(FileChange{Path: "/etc/passwd", Content: "y"}, "/work/proj")},
		"apply bad base":                {Kind: JobApply, Input: apply(FileChange{Path: "a", BaseSHA: "xyz", Content: "y"}, "/work/proj")},
		"unknown kind":                  {Kind: "shell", Input: cmdInput("ls", "/work/proj")},
		"timeout too long":              {Kind: JobCommand, Timeout: 3 * 3600, Input: cmdInput("ls", "/work/proj")},
		"unknown source":                {Kind: JobCommand, Source: "cron", Input: cmdInput("ls", "/work/proj")},
		"agent without a CLI installed": {Kind: JobAgent, Input: agent(AgentJob{CLI: "codex", Prompt: "hi", Repo: "https://x.test/a/b.git"})},
		"clone of a local path":         {Kind: JobAgent, Input: agent(AgentJob{CLI: "claude", Prompt: "hi", Repo: "/etc"})},
		"clone through ext::":           {Kind: JobAgent, Input: agent(AgentJob{CLI: "claude", Prompt: "hi", Repo: "ext::sh -c touch% /tmp/x"})},
		"option-looking branch":         {Kind: JobAgent, Input: agent(AgentJob{CLI: "claude", Prompt: "hi", Repo: "https://x.test/a/b.git", Branch: "--upload-pack=x"})},
		"branch with a //":              {Kind: JobAgent, Input: agent(AgentJob{CLI: "claude", Prompt: "hi", Repo: "https://x.test/a/b.git", Branch: "a//b"})},
		"branch with a .lock part":      {Kind: JobAgent, Input: agent(AgentJob{CLI: "claude", Prompt: "hi", Repo: "https://x.test/a/b.git", Branch: "a.lock/b"})},
		"branch with a hidden part":     {Kind: JobAgent, Input: agent(AgentJob{CLI: "claude", Prompt: "hi", Repo: "https://x.test/a/b.git", Branch: "a/.b"})},
		"read-only that pushes":         {Kind: JobAgent, Input: agent(AgentJob{CLI: "claude", Prompt: "hi", Repo: "https://x.test/a/b.git", ReadOnly: true, Push: true})},
	} {
		if _, err := e.s.EnqueueRunnerJob(e.ctx, e.admin, in); !IsInvalid(err) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("ls", "/work/proj/sub/")})
	e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: agent(AgentJob{CLI: "claude", Prompt: "hi", Repo: "git@git.example.com:a/b.git"})})
	rv := e.enqueue(e.admin, RunnerJobInput{Kind: JobReview, Input: agent(AgentJob{CLI: "claude", Prompt: "review", Repo: "https://x.test/a/b.git", Branch: "x", Push: true})})
	info, err := e.s.GetRunnerJob(e.ctx, e.admin, rv.ID, -1)
	if err != nil || !strings.Contains(string(info.Input), `"read_only":true`) || strings.Contains(string(info.Input), `"push":true`) {
		t.Errorf("review input = %s (%v)", info.Input, err)
	}
}

// An audit's job may run as long as the gateway's agent may, with its
// Haiku helpers (runnerAuditMaxTimeout);
// no other kind's may run past runnerMaxTimeout.
func TestAuditJobTimeoutReachesTheGatewayLimit(t *testing.T) {
	e := newEnv(t)
	e.runner(e.admin, "devhost", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}}
		r.Features = []string{FeatureAudit}
	})
	audit, _ := json.Marshal(AgentJob{CLI: "cursor", Prompt: "audit", Repo: "https://x.test/a/b.git"})
	e.enqueue(e.admin, RunnerJobInput{Kind: JobAudit, Timeout: runnerAuditMaxTimeout, Input: audit})
	if _, err := e.s.EnqueueRunnerJob(e.ctx, e.admin, RunnerJobInput{Kind: JobAudit, Timeout: runnerAuditMaxTimeout + 1, Input: audit}); !IsInvalid(err) {
		t.Errorf("an audit past %d seconds: %v", runnerAuditMaxTimeout, err)
	}
	if _, err := e.s.EnqueueRunnerJob(e.ctx, e.admin, RunnerJobInput{Kind: JobCommand, Timeout: runnerMaxTimeout + 1, Input: cmdInput("ls", "/work")}); !IsInvalid(err) {
		t.Errorf("a command past %d seconds: %v", runnerMaxTimeout, err)
	}
}

func TestClaimGoesInOrderAndHonoursLabelsAndCLIs(t *testing.T) {
	e := newEnv(t)
	plain := e.runner(e.admin, "plain", nil)
	gpu := e.runner(e.admin, "gpu", func(r *RunnerRegistration) {
		r.Labels = []string{"gpu"}
		no := false
		r.CLIs = []RunnerCLI{{Name: "claude", SignedIn: &no}, {Name: "codex"}}
	})
	agent := func(cli string) json.RawMessage {
		b, _ := json.Marshal(AgentJob{CLI: cli, Prompt: "hi", Repo: "https://x.test/a/b.git"})
		return b
	}
	first := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Label: "gpu", Input: cmdInput("train", "/work")})
	second := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("a", "/work")})
	third := e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: agent("codex")})
	if _, err := e.s.EnqueueRunnerJob(e.ctx, e.admin, RunnerJobInput{Kind: JobAgent, Input: agent("claude")}); !IsInvalid(err) {
		t.Errorf("claude is signed out everywhere: %v", err)
	}

	if w := e.poll(e.admin, plain, RunnerPoll{}); w.Job == nil || w.Job.ID != second.ID {
		t.Errorf("plain runner skips the gpu job and takes the next: %+v", w.Job)
	}
	if w := e.poll(e.admin, plain, RunnerPoll{}); w.Job != nil {
		t.Errorf("plain runner has no codex: %+v", w.Job)
	}
	if w := e.poll(e.admin, gpu, RunnerPoll{}); w.Job == nil || w.Job.ID != first.ID {
		t.Errorf("gpu runner takes the oldest it may: %+v", w.Job)
	}
	zero := 0
	if w := e.poll(e.admin, gpu, RunnerPoll{Free: &zero}); w.Job != nil {
		t.Errorf("a full runner took a job: %+v", w.Job)
	}
	if w := e.poll(e.admin, gpu, RunnerPoll{}); w.Job == nil || w.Job.ID != third.ID {
		t.Errorf("then the codex job: %+v", w.Job)
	}
	got, _ := e.s.GetRunnerJob(e.ctx, e.admin, first.ID, -1)
	if got.State != "running" || got.Runner != "gpu" || got.Attempts != 1 || got.StartedAt == nil {
		t.Errorf("claimed job = %+v", got)
	}
}

func TestClaimLooksPastOlderJobsTheRunnerCantTake(t *testing.T) {
	e := newEnv(t)
	plain := e.runner(e.admin, "plain", nil)
	e.runner(e.admin, "gpu", func(r *RunnerRegistration) { r.Labels = []string{"gpu"} })
	for range 105 {
		e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Label: "gpu", Input: cmdInput("train", "/work")})
	}
	mine := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("a", "/work")})
	if w := e.poll(e.admin, plain, RunnerPoll{}); w.Job == nil || w.Job.ID != mine.ID {
		t.Errorf("the plain runner never reaches its job behind 105 gpu jobs: %+v", w.Job)
	}
}

func TestClaimTakesAuditsAfterEverythingElse(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "devhost", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}}
		r.Features = []string{FeatureAudit}
	})
	audit, _ := json.Marshal(AgentJob{CLI: "cursor", Prompt: "audit", Repo: "https://x.test/a/b.git"})
	var audits []int64
	for range 3 {
		audits = append(audits, e.enqueue(e.admin, RunnerJobInput{Kind: JobAudit, Input: audit}).ID)
	}
	round := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make check", "/work")})
	var got []int64
	for range 5 {
		w := e.poll(e.admin, r, RunnerPoll{})
		if w.Job == nil {
			break
		}
		got = append(got, w.Job.ID)
	}
	if want := append([]int64{round.ID}, audits...); !slices.Equal(got, want) {
		t.Errorf("claimed %v, want the command first, then the audits oldest first: %v", got, want)
	}
}

func TestHeldPollUsesTheRunnerAsItIsNow(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	e.runner(e.admin, "other", nil)
	wait := 1.0
	got := make(chan *RunnerWork, 1)
	go func() {
		w, err := e.s.RunnerPoll(e.ctx, e.admin, RunnerPoll{Runner: r.ID, Wait: &wait})
		if err != nil {
			t.Error(err)
		}
		got <- w
	}()
	time.Sleep(150 * time.Millisecond)
	e.runner(e.admin, "mine", func(r *RunnerRegistration) { r.AllowedDirs = []string{"/elsewhere"} })
	e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work")})
	select {
	case w := <-got:
		if w.Job != nil {
			t.Errorf("a held poll claimed a job in a directory the runner no longer allows: %+v", w.Job)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never ended")
	}
}

func TestLongCLIReportDoesNotFailThePoll(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	clis := []RunnerCLI{{Name: "claude"}, {Name: "codex"}, {Name: "opencode"}, {Name: "claude"}, {Name: "mystery"}, {Name: "codex"}}
	e.poll(e.admin, r, RunnerPoll{CLIs: &clis})
	got, err := e.s.runnerByID(e.ctx, e.s.db.R, r.ID)
	if err != nil || len(got.CLIs) != 3 {
		t.Errorf("clis = %+v (%v)", got.CLIs, err)
	}
}

func TestPollIsHeldAndWokenByANewJob(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	got := make(chan *RunnerWork, 1)
	go func() {
		w, err := e.s.RunnerPoll(e.ctx, e.admin, RunnerPoll{Runner: r.ID})
		if err != nil {
			t.Error(err)
		}
		got <- w
	}()
	select {
	case <-got:
		t.Fatal("poll returned with nothing to do")
	case <-time.After(150 * time.Millisecond):
	}
	j := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work")})
	select {
	case w := <-got:
		if w.Job == nil || w.Job.ID != j.ID {
			t.Errorf("woken poll = %+v", w)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the held poll was not woken by the new job")
	}
	short := 0.1
	start := time.Now()
	w := e.poll(e.admin, r, RunnerPoll{Wait: &short})
	if w.Job != nil || time.Since(start) < 90*time.Millisecond {
		t.Errorf("short wait: %+v after %v", w.Job, time.Since(start))
	}
}

func TestLogFinishAndEvents(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	sub := e.s.Hub.Subscribe(e.admin.User.ID)
	defer e.s.Hub.Unsubscribe(sub)
	j := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make", "/work"), Source: "request_cell", SourceID: 42})
	e.poll(e.admin, r, RunnerPoll{})

	ack, err := e.s.AppendRunnerLog(e.ctx, e.admin, j.ID, 0, "hello\n")
	if err != nil || ack.Size != 6 || ack.Cancel {
		t.Fatalf("log: %+v %v", ack, err)
	}
	if ack, _ = e.s.AppendRunnerLog(e.ctx, e.admin, j.ID, 0, "hello\n"); ack.Size != 6 {
		t.Errorf("resent chunk: %+v", ack)
	}
	if ack, _ = e.s.AppendRunnerLog(e.ctx, e.admin, j.ID, 3, "lo\nworld\n"); ack.Size != 12 {
		t.Errorf("overlapping chunk: %+v", ack)
	}
	got, _ := e.s.GetRunnerJob(e.ctx, e.admin, j.ID, 0)
	if got.Log == nil || *got.Log != "hello\nworld\n" || got.LogSize != 12 {
		t.Errorf("log = %v size %d", got.Log, got.LogSize)
	}
	if tail, _ := e.s.GetRunnerJob(e.ctx, e.admin, j.ID, 6); *tail.Log != "world\n" {
		t.Errorf("tail = %q", *tail.Log)
	}
	bob := e.user("bob", false)
	if _, err := e.s.AppendRunnerLog(e.ctx, bob, j.ID, 0, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob wrote to robin's job: %v", err)
	}
	if _, err := e.s.FinishRunnerJob(e.ctx, bob, j.ID, RunnerFinish{State: "done"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob finished robin's job: %v", err)
	}

	zero := 0
	st, err := e.s.FinishRunnerJob(e.ctx, e.admin, j.ID, RunnerFinish{State: "done", ExitCode: &zero, Result: json.RawMessage(`{"exit_code":0}`)})
	if err != nil || st != "done" {
		t.Fatalf("finish: %q %v", st, err)
	}
	var finished *Event
	for len(sub.C) > 0 {
		if ev := <-sub.C; ev.Kind == EvRunnerJobFinished {
			finished = ev
		}
	}
	if finished == nil || finished.Data["job"] != j.ID || finished.Data["state"] != "done" || finished.Data["kind"] != JobCommand ||
		finished.Data["source"] != "request_cell" || finished.Data["source_id"] != int64(42) {
		t.Errorf("finished event = %+v", finished)
	}
	if st, _ := e.s.FinishRunnerJob(e.ctx, e.admin, j.ID, RunnerFinish{State: "failed"}); st != "done" {
		t.Errorf("late report changed the state to %q", st)
	}
	out, err := e.s.RunnerJobByID(e.ctx, e.admin, j.ID)
	if err != nil || out.State != "done" || out.Output != "hello\nworld\n" || string(out.Result) != `{"exit_code":0}` || *out.ExitCode != 0 {
		t.Errorf("job for other parts = %+v %v", out, err)
	}
	if ack, _ := e.s.AppendRunnerLog(e.ctx, e.admin, j.ID, 12, "more"); !ack.Cancel {
		t.Error("log after the end should say stop")
	}
}

func TestLogIsCappedAtOneMegabyte(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	j := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("yes", "/work")})
	e.poll(e.admin, r, RunnerPoll{})
	chunk := strings.Repeat("é", 1<<18)
	var off int64
	var ack *LogAck
	for range 4 {
		ack, _ = e.s.AppendRunnerLog(e.ctx, e.admin, j.ID, off, chunk)
		off += int64(len(chunk))
	}
	if !ack.Truncated || ack.Size < runnerLogCap || ack.Size > runnerLogCap+100 {
		t.Errorf("ack = %+v", ack)
	}
	got, _ := e.s.GetRunnerJob(e.ctx, e.admin, j.ID, 0)
	if !strings.HasSuffix(*got.Log, strings.TrimSpace(logTruncated)+"\n") || !strings.HasPrefix(*got.Log, "ééé") {
		t.Errorf("log ends %q", (*got.Log)[len(*got.Log)-40:])
	}
}

// A job Waypoint ended itself (an audit called off) that prints nothing is
// only stopped if the poll says so: its runner uploads no log to be told on
// (WP-870).
func TestPollStopsJobsWaypointEnded(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	silent := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("a", "/work")})
	other := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("b", "/work")})
	e.poll(e.admin, r, RunnerPoll{})
	e.poll(e.admin, r, RunnerPoll{Running: []int64{silent.ID}})
	both := []int64{silent.ID, other.ID}
	if w := e.poll(e.admin, r, RunnerPoll{Running: both}); len(w.Cancel) != 0 {
		t.Fatalf("nothing to stop yet: %+v", w.Cancel)
	}
	e.s.cancelStepJob(e.ctx, silent.ID, "The audit was cancelled.")
	if w := e.poll(e.admin, r, RunnerPoll{Running: both}); len(w.Cancel) != 0 {
		t.Errorf("told to stop a job that only just ended: %+v", w.Cancel)
	}
	e.setClock(e.clockNow().Add(runnerEndedGrace + time.Second))
	third := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("c", "/work")})
	w := e.poll(e.admin, r, RunnerPoll{Running: both})
	if len(w.Cancel) != 1 || w.Cancel[0] != silent.ID {
		t.Fatalf("the poll should stop the ended job, and only it: %+v", w.Cancel)
	}
	if w.Job != nil {
		t.Errorf("a poll that stops a job hands out none: %+v", w.Job)
	}
	bob := e.user("bob", false)
	theirs := e.runner(bob, "theirs", nil)
	if w := e.poll(bob, theirs, RunnerPoll{Running: both}); len(w.Cancel) != 0 {
		t.Errorf("a runner was told about a job that isn't its own: %+v", w.Cancel)
	}
	if st, _ := e.s.FinishRunnerJob(e.ctx, e.admin, silent.ID, RunnerFinish{State: "failed"}); st != "cancelled" {
		t.Errorf("state = %s", st)
	}
	if w := e.poll(e.admin, r, RunnerPoll{Running: []int64{other.ID}}); len(w.Cancel) != 0 || w.Job == nil || w.Job.ID != third.ID {
		t.Errorf("the next poll should stop nothing and take the waiting job: %+v", w)
	}
}

// The query that looks the listed ids up is built to their number: one id,
// the most it looks up, and more than that must all bind (audit 543.1).
func TestPollLooksUpAnyNumberOfRunningJobs(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	ended := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("a", "/work")})
	e.poll(e.admin, r, RunnerPoll{})
	e.s.cancelStepJob(e.ctx, ended.ID, "The audit was cancelled.")
	e.setClock(e.clockNow().Add(runnerEndedGrace + time.Second))
	for _, n := range []int{1, 2, runnerEndedMax - 1, runnerEndedMax, runnerEndedMax + 1, 3 * runnerEndedMax} {
		ids := []int64{ended.ID}
		for i := 1; i < n; i++ {
			ids = append(ids, ended.ID+int64(i))
		}
		if w := e.poll(e.admin, r, RunnerPoll{Running: ids}); len(w.Cancel) != 1 || w.Cancel[0] != ended.ID {
			t.Errorf("%d ids listed: cancel = %+v", n, w.Cancel)
		}
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	r := e.runner(e.admin, "mine", nil)
	first := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("a", "/work")})
	second := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("b", "/work")})
	e.poll(e.admin, r, RunnerPoll{})

	if _, err := e.s.CancelRunnerJob(e.ctx, bob, second.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob cancelled robin's job: %v", err)
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-code"}
	if _, err := e.s.CancelRunnerJob(e.ctx, agent, second.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent cancelled: %v", err)
	}
	j, err := e.s.CancelRunnerJob(e.ctx, e.admin, second.ID)
	if err != nil || j.State != "cancelled" {
		t.Fatalf("cancel waiting job: %+v %v", j, err)
	}
	j, err = e.s.CancelRunnerJob(e.ctx, e.admin, first.ID)
	if err != nil || j.State != "running" || !j.CancelRequested {
		t.Fatalf("cancel running job: %+v %v", j, err)
	}
	if w := e.poll(e.admin, r, RunnerPoll{}); len(w.Cancel) != 1 || w.Cancel[0] != first.ID {
		t.Errorf("poll should carry the cancel: %+v", w)
	}
	if ack, _ := e.s.AppendRunnerLog(e.ctx, e.admin, first.ID, 0, "x"); !ack.Cancel {
		t.Error("log answer should carry the cancel")
	}
	if st, _ := e.s.FinishRunnerJob(e.ctx, e.admin, first.ID, RunnerFinish{State: "failed"}); st != "cancelled" {
		t.Errorf("state = %s", st)
	}
	if _, err := e.s.CancelRunnerJob(e.ctx, e.admin, first.ID); !IsInvalid(err) {
		t.Errorf("cancelling a finished job: %v", err)
	}
	other := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("c", "/work")})
	e.poll(e.admin, r, RunnerPoll{})
	e.setClock(e.clockNow().Add(10 * time.Minute))
	if j, _ := e.s.CancelRunnerJob(e.ctx, e.admin, other.ID); j.State != "cancelled" {
		t.Errorf("job on an offline runner: %+v", j)
	}
}

func TestOnlineStatusAndExpiry(t *testing.T) {
	e := newEnv(t)
	sub := e.s.Hub.Subscribe(e.admin.User.ID)
	defer e.s.Hub.Unsubscribe(sub)
	status := func() (bool, bool) {
		for len(sub.C) > 0 {
			if ev := <-sub.C; ev.Kind == EvRunnerStatus {
				return ev.Data["online"].(bool), true
			}
		}
		return false, false
	}
	r := e.runner(e.admin, "mine", nil)
	if on, ok := status(); !ok || !on {
		t.Errorf("registering should announce online: %v %v", on, ok)
	}
	e.setClock(e.clockNow().Add(60 * time.Second))
	e.s.CheckRunners(e.ctx)
	if _, ok := status(); ok {
		t.Error("announced a change at 60 s")
	}
	e.setClock(e.clockNow().Add(40 * time.Second))
	e.s.CheckRunners(e.ctx)
	if on, ok := status(); !ok || on {
		t.Errorf("should have gone offline: %v %v", on, ok)
	}
	if got, _ := e.s.GetRunner(e.ctx, e.admin, r.ID); got.Online {
		t.Error("offline runner listed online")
	}
	e.s.CheckRunners(e.ctx)
	if _, ok := status(); ok {
		t.Error("announced offline twice")
	}
	e.poll(e.admin, r, RunnerPoll{})
	if on, ok := status(); !ok || !on {
		t.Errorf("poll should announce online: %v %v", on, ok)
	}

	running := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Timeout: 60, Input: cmdInput("sleep", "/work")})
	e.poll(e.admin, r, RunnerPoll{})
	waiting := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("later", "/work")})
	state := func(id int64) string {
		j, _ := e.s.GetRunnerJob(e.ctx, e.admin, id, -1)
		return j.State
	}
	e.setClock(e.clockNow().Add(60*time.Second + 4*time.Minute))
	e.s.CheckRunners(e.ctx)
	if state(running.ID) != "running" {
		t.Error("expired before timeout + 5 minutes")
	}
	e.setClock(e.clockNow().Add(2 * time.Minute))
	e.s.CheckRunners(e.ctx)
	if state(running.ID) != "expired" {
		t.Errorf("running job past its timeout: %s", state(running.ID))
	}
	if state(waiting.ID) != "queued" {
		t.Errorf("waiting job: %s", state(waiting.ID))
	}
	e.setClock(e.clockNow().Add(24 * time.Hour))
	e.s.CheckRunners(e.ctx)
	if state(waiting.ID) != "expired" {
		t.Errorf("job nobody took in a day: %s", state(waiting.ID))
	}
	n := 0
	for len(sub.C) > 0 {
		if ev := <-sub.C; ev.Kind == EvRunnerJobFinished && ev.Data["state"] == "expired" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("expired events = %d", n)
	}
}

func TestPollFailsJobsTheRunnerLost(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	kept := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("a", "/work")})
	lost := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("b", "/work")})
	e.poll(e.admin, r, RunnerPoll{})
	e.poll(e.admin, r, RunnerPoll{})
	e.poll(e.admin, r, RunnerPoll{Running: []int64{}})
	if j, _ := e.s.GetRunnerJob(e.ctx, e.admin, lost.ID, -1); j.State != "running" {
		t.Errorf("failed within the grace period: %s", j.State)
	}
	e.setClock(e.clockNow().Add(time.Minute))
	e.poll(e.admin, r, RunnerPoll{Running: []int64{kept.ID}})
	if j, _ := e.s.GetRunnerJob(e.ctx, e.admin, lost.ID, -1); j.State != "failed" || !strings.Contains(j.Error, "lost") {
		t.Errorf("lost job = %+v", j)
	}
	if j, _ := e.s.GetRunnerJob(e.ctx, e.admin, kept.ID, -1); j.State != "running" {
		t.Errorf("listed job = %s", j.State)
	}
}

func TestDeleteRunnerEndsItsJobs(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	r := e.runner(e.admin, "mine", nil)
	other := e.runner(e.admin, "other", nil)
	targeted := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, RunnerID: r.ID, Input: cmdInput("a", "/work")})
	running := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, RunnerID: r.ID, Input: cmdInput("b", "/work")})
	e.poll(e.admin, r, RunnerPoll{})
	any := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("c", "/work")})
	if err := e.s.DeleteRunner(e.ctx, bob, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob removed robin's runner: %v", err)
	}
	if err := e.s.DeleteRunner(e.ctx, e.admin, r.ID); err != nil {
		t.Fatal(err)
	}
	st := func(id int64) string { j, _ := e.s.GetRunnerJob(e.ctx, e.admin, id, -1); return j.State }
	if st(targeted.ID) != "failed" {
		t.Errorf("a job the runner was running = %s", st(targeted.ID))
	}
	if st(running.ID) != "cancelled" {
		t.Errorf("a job waiting for that runner = %s", st(running.ID))
	}
	if st(any.ID) != "queued" {
		t.Errorf("a job for any runner should wait for another: %s", st(any.ID))
	}
	if w := e.poll(e.admin, other, RunnerPoll{}); w.Job == nil || w.Job.ID != any.ID {
		t.Errorf("other runner = %+v", w.Job)
	}
	if _, err := e.s.GetRunner(e.ctx, e.admin, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("runner still there: %v", err)
	}
}

func TestListingAndAdminView(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	carol := e.user("carol", false)
	br := e.runner(bob, "bobs", nil)
	e.runner(carol, "carols", nil)
	bj := e.enqueue(bob, RunnerJobInput{Kind: JobCommand, Input: cmdInput("secret --token=abc", "/work"), Source: "manual"})
	e.poll(bob, br, RunnerPoll{})

	if rs, _ := e.s.ListRunners(e.ctx, bob, false); len(rs) != 1 || rs[0].Name != "bobs" || rs[0].Running != 1 {
		t.Errorf("bob's runners = %+v", rs)
	}
	if _, err := e.s.ListRunners(e.ctx, bob, true); !errors.Is(err, ErrForbidden) {
		t.Errorf("a member listed everyone's runners: %v", err)
	}
	if rs, _ := e.s.ListRunners(e.ctx, e.admin, true); len(rs) != 2 {
		t.Errorf("admin sees %d runners", len(rs))
	}
	if _, err := e.s.GetRunner(e.ctx, carol, br.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("carol read bob's runner: %v", err)
	}
	if _, err := e.s.GetRunnerJob(e.ctx, carol, bj.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("carol read bob's job: %v", err)
	}
	if js, _ := e.s.ListRunnerJobs(e.ctx, carol, RunnerJobFilter{}); len(js) != 0 {
		t.Errorf("carol sees %d jobs", len(js))
	}
	js, err := e.s.ListRunnerJobs(e.ctx, e.admin, RunnerJobFilter{All: true})
	if err != nil || len(js) != 1 || !js[0].Redacted || js[0].Title != "" {
		t.Errorf("admin list = %+v %v", js, err)
	}
	if d, _ := e.s.GetRunnerJob(e.ctx, e.admin, bj.ID, 0); !d.Redacted || len(d.Input) != 0 || d.Log != nil {
		t.Errorf("admin detail = %+v", d)
	}
	if _, err := e.s.ListRunnerJobs(e.ctx, bob, RunnerJobFilter{All: true}); !errors.Is(err, ErrForbidden) {
		t.Errorf("member asked for all jobs: %v", err)
	}
	own, _ := e.s.ListRunnerJobs(e.ctx, bob, RunnerJobFilter{RunnerID: br.ID, States: []string{"running"}, Source: "manual"})
	if len(own) != 1 || own[0].Title != "secret --token=abc" || own[0].CreatedBy == nil || own[0].CreatedBy.Username != "bob" {
		t.Errorf("bob's list = %+v", own)
	}
}

// A person's cancel keeps its reason when Waypoint cancels the job again: the
// reason is what stops a held review opening its pull request (WP-752).
func TestRunnerJobFirstCancelReasonStays(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	j := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("a", "/work")})
	e.poll(e.admin, r, RunnerPoll{})
	if _, err := e.s.CancelRunnerJob(e.ctx, e.admin, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.cancelRunnerJob(e.ctx, &Actor{User: e.admin.User}, j.ID, "No runner finished it within 12 minutes."); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FinishRunnerJob(e.ctx, e.admin, j.ID, RunnerFinish{State: "cancelled", Error: "killed"}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.s.GetRunnerJob(e.ctx, e.admin, j.ID, 0)
	if !strings.HasPrefix(got.Error, roundCancelledBy) {
		t.Fatalf("error = %q", got.Error)
	}
}

// A job's result is kept up to RunnerResultMax and refused over it with the
// words the runner reads to send less (WP-1320): an audit's result holds its
// helpers' runs, which passed the old 256 KB.
func TestFinishRunnerJobResultLimit(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "mine", nil)
	j := e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make", "/work")})
	e.poll(e.admin, r, RunnerPoll{})
	over := json.RawMessage(`{"pad":"` + strings.Repeat("x", RunnerResultMax) + `"}`)
	_, err := e.s.FinishRunnerJob(e.ctx, e.admin, j.ID, RunnerFinish{State: "done", Result: over})
	if !IsInvalid(err) || !strings.Contains(err.Error(), RunnerResultTooLarge) {
		t.Fatalf("a result over the most: %v", err)
	}
	if _, err := e.s.FinishRunnerJob(e.ctx, e.admin, j.ID, RunnerFinish{State: "done", Result: json.RawMessage(`{"pad": `)}); !IsInvalid(err) || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("a result that isn't JSON: %v", err)
	}
	mid := json.RawMessage(`{"pad":"` + strings.Repeat("x", 1<<20) + `"}`)
	if st, err := e.s.FinishRunnerJob(e.ctx, e.admin, j.ID, RunnerFinish{State: "done", Result: mid}); err != nil || st != "done" {
		t.Fatalf("a 1 MB result: %q %v", st, err)
	}
	got, err := e.s.GetRunnerJob(e.ctx, e.admin, j.ID, 0)
	if err != nil || len(got.Result) != len(mid) {
		t.Errorf("stored %d bytes, want %d (%v)", len(got.Result), len(mid), err)
	}
}
