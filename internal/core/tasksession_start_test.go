package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Starting a task session from the queue (WP-71).

func TestTaskSessionPlacesAndStart(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	other := e.project(e.admin, "Other", "")
	is := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix the thing"})
	ag := agentActor(e.admin, "anthropic/claude-code")
	session := func(machine, cli string, project int64, mod func(*HookEvent)) {
		t.Helper()
		if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook(machine, cli, "SessionStart", mod)}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = nullif(?, 0) WHERE cli_id = ?`, project, cli); err != nil {
			t.Fatal(err)
		}
		e.setClock(e.clockNow().Add(time.Minute))
	}

	// No runner yet: nowhere to start, and saying so.
	session("box", "s1", p.ID, nil)
	if got, err := e.s.TaskSessionPlaces(e.ctx, e.admin); err != nil || len(got) != 0 {
		t.Fatalf("no runner: %v %v", got, err)
	}
	if _, err := e.s.StartTaskSession(e.ctx, e.admin, is.Key); !IsInvalid(err) || !strings.Contains(err.Error(), "wp start "+is.Key) {
		t.Fatalf("no runner: %v", err)
	}

	claude := func(r *RunnerRegistration) {
		r.CLIs, r.Features = []RunnerCLI{{Name: "claude"}}, []string{FeatureTaskKickoff}
	}
	e.runner(e.admin, "old", claude) // goes offline below
	session("old", "s2", p.ID, nil)  // newer than s1, on the offline runner's machine
	session("box", "s3", p.ID, func(h *HookEvent) { h.Worktree, h.Cwd = true, "/work/app/.claude/worktrees/x" })
	session("laptop", "s4", p.ID, nil)                                          // no runner there
	session("box", "s5", other.ID, func(h *HookEvent) { h.Cwd = "/elsewhere" }) // not allowed
	e.setClock(e.clockNow().Add(time.Hour))
	e.runner(e.admin, "box", claude)
	e.runner(e.admin, "nocli", nil)
	// A runner too old to take the task for the session isn't a place: it
	// would need the prompt WP-1037 removed.
	e.runner(e.admin, "laptop", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })

	places, err := e.s.TaskSessionPlaces(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(places) != 1 || places[0].ProjectKey != p.Key || places[0].Runner != "box" || !places[0].Online || places[0].Dir != "/work/app" {
		b, _ := json.Marshal(places)
		t.Fatalf("places = %s; want App on the online box, in its checkout (not the worktree)", b)
	}

	// A project without a connected repo: the session took one of its tasks.
	third := e.project(e.admin, "Third", "")
	t3 := e.issue(e.admin, IssueInput{Project: third.Key, Title: "Elsewhere"})
	session("box", "s6", 0, func(h *HookEvent) { h.Cwd = "/work/third" })
	if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = NULL WHERE cli_id = 's6'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO agent_runs(issue_id, agent, user_id, started_at, session_id)
		SELECT ?, 'anthropic/claude-code', user_id, 1, id FROM agent_sessions WHERE cli_id = 's6'`, t3.ID); err != nil {
		t.Fatal(err)
	}
	if places, _ = e.s.TaskSessionPlaces(e.ctx, e.admin); len(places) != 2 || places[0].ProjectKey != third.Key || places[0].Dir != "/work/third" {
		b, _ := json.Marshal(places)
		t.Fatalf("places = %s; want Third (from the task its session took) first", b)
	}

	st, err := e.s.StartTaskSession(e.ctx, e.admin, strings.ToLower(is.Key))
	if err != nil {
		t.Fatal(err)
	}
	if st.Key != is.Key || st.Job.Kind != JobSession || st.Job.State != "queued" || st.Place.Runner != "box" {
		t.Fatalf("start = %+v %+v", st.Job, st.Place)
	}
	job, err := e.s.GetRunnerJob(e.ctx, e.admin, st.Job.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	var in SessionJob
	if err := json.Unmarshal(job.Input, &in); err != nil {
		t.Fatal(err)
	}
	// The runner takes the task for the session and fills autopilot's
	// kickoff template with the answer (WP-1037): no prompt of its own.
	if in.Task != is.Key || in.Dir != "/work/app" || in.Prompt != "" || in.Kickoff != DefaultAutopilotPrompt || in.Title != "Fix the thing" || in.Name != is.Key+" Fix the thing" {
		t.Errorf("job input = %+v", in)
	}
	if job.Title != "Task session: "+is.Key+" Fix the thing" || job.Source != "manual" {
		t.Errorf("job = %q from %q", job.Title, job.Source)
	}

	// Agents never start things on a person's machine, and see no places.
	if _, err := e.s.StartTaskSession(e.ctx, ag, is.Key); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent start: %v", err)
	}
	if got, _ := e.s.TaskSessionPlaces(e.ctx, ag); len(got) != 0 {
		t.Errorf("agent places: %v", got)
	}
}

func TestSessionJobTaskIsValidated(t *testing.T) {
	raw := func(j SessionJob) json.RawMessage { b, _ := json.Marshal(j); return b }
	if _, _, _, err := checkRunnerInput(JobSession, raw(SessionJob{Dir: "/work", Prompt: "go", Task: "not a key"})); !IsInvalid(err) {
		t.Errorf("bad task: %v", err)
	}
	if _, _, _, err := checkRunnerInput(JobSession, raw(SessionJob{Dir: "/work", Prompt: "go", Name: "a\nb"})); !IsInvalid(err) {
		t.Errorf("bad name: %v", err)
	}
	n, title, out, err := checkRunnerInput(JobSession, raw(SessionJob{Dir: "/work", Prompt: "go", Task: "wp-3, WP-4"}))
	if err != nil || title != "Task session: WP-3,WP-4" || !strings.Contains(string(out), `"task":"WP-3,WP-4"`) || n.feature != "" {
		t.Errorf("task kept: %q %s %v", title, out, err)
	}
	// A kickoff (WP-1037) is for one task, without a prompt, and only for a
	// runner that knows how to take the task for the session.
	for name, j := range map[string]SessionJob{
		"two tasks":  {Dir: "/work", Kickoff: "{next}", Task: "WP-3,WP-4"},
		"no task":    {Dir: "/work", Kickoff: "{next}"},
		"and prompt": {Dir: "/work", Kickoff: "{next}", Task: "WP-3", Prompt: "go"},
	} {
		if _, _, _, err := checkRunnerInput(JobSession, raw(j)); !IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	n, _, _, err = checkRunnerInput(JobSession, raw(SessionJob{Dir: "/work", Kickoff: "{next}", Task: "WP-3", Title: "Fix"}))
	if err != nil || n.feature != FeatureTaskKickoff {
		t.Errorf("kickoff: %+v %v", n, err)
	}
	work := []string{"/work"}
	if runnerCanTake(&Runner{CLIs: []RunnerCLI{{Name: "claude"}}, AllowedDirs: work}, n) ||
		!runnerCanTake(&Runner{CLIs: []RunnerCLI{{Name: "claude"}}, AllowedDirs: work, Features: []string{FeatureTaskKickoff}}, n) {
		t.Error("only a runner with task_kickoff takes a kickoff")
	}
}

// A runner whose wp predates kickoffs never claims one (WP-1037): it would
// fail it, or start the session with no first prompt.
func TestOlderRunnerSkipsKickoff(t *testing.T) {
	e := newEnv(t)
	claude := []RunnerCLI{{Name: "claude"}}
	older := e.runner(e.admin, "older", func(r *RunnerRegistration) { r.CLIs = claude })
	newer := e.runner(e.admin, "newer", func(r *RunnerRegistration) { r.CLIs, r.Features = claude, []string{FeatureTaskKickoff} })
	raw, _ := json.Marshal(SessionJob{Dir: "/work", Kickoff: "{next}", Task: "WP-3", Title: "Fix"})
	j := e.enqueue(e.admin, RunnerJobInput{Kind: JobSession, Input: raw})
	if w := e.poll(e.admin, older, RunnerPoll{}); w.Job != nil {
		t.Fatalf("the older runner got job %d", w.Job.ID)
	}
	if w := e.poll(e.admin, newer, RunnerPoll{}); w.Job == nil || w.Job.ID != j.ID {
		t.Fatalf("the newer runner got %+v", w.Job)
	}
}
