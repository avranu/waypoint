package core

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestJobTokenAgent(t *testing.T) {
	agent := func(j AgentJob) json.RawMessage { b, _ := json.Marshal(j); return b }
	auto := func(j AutopilotJob) json.RawMessage { b, _ := json.Marshal(j); return b }
	for _, c := range []struct {
		name, kind string
		input      json.RawMessage
		want       string
		ok         bool
	}{
		{"autopilot", JobAutopilot, auto(AutopilotJob{Agent: "anthropic/claude-opus-5-5"}), "anthropic/claude-opus-5-5", true},
		{"autopilot, no agent", JobAutopilot, auto(AutopilotJob{}), "anthropic/claude-code", true},
		{"edit_run", JobAgent, agent(AgentJob{CLI: "claude", Model: "opus[1m]", Permission: AgentEditRun}), "anthropic/opus", true},
		{"opencode", JobAgent, agent(AgentJob{CLI: "opencode", Model: "gateway/muse-spark-1.3", Permission: AgentEditRun}), "gateway/muse-spark-1.3", true},
		{"codex, no model", JobAgent, agent(AgentJob{CLI: "codex", Permission: AgentEditRun}), "openai/codex", true},
		{"edit", JobAgent, agent(AgentJob{CLI: "claude", Permission: AgentEdit}), "", false},
		{"confined", JobAgent, agent(AgentJob{CLI: "claude", Permission: AgentEditRun, Confine: true}), "", false},
		{"read only", JobAgent, agent(AgentJob{CLI: "claude", Permission: AgentEditRun, ReadOnly: true}), "", false},
		{"command", JobCommand, cmdInput("ls", "/work"), "", false},
	} {
		got, ok := jobTokenAgent(c.kind, c.input)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// A job that runs commands gets a token of its own when a runner claims it:
// an agent's, with only the job's permissions, out of the token list, and
// gone when the job ends (WP-418).
func TestJobTokenLifecycle(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "box", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	input := func(perm string) json.RawMessage {
		b, _ := json.Marshal(AgentJob{CLI: "claude", Model: "sonnet", Prompt: "hi", Repo: "https://x.test/a/b.git", Permission: perm})
		return b
	}
	plain := e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: input(AgentEdit)})
	w := e.poll(e.admin, r, RunnerPoll{})
	if w.Job == nil || w.Job.ID != plain.ID || w.Job.Token != "" {
		t.Fatalf("an agent that can't run commands got a token: %+v", w.Job)
	}
	job := e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: input(AgentEditRun)})
	w = e.poll(e.admin, r, RunnerPoll{})
	if w.Job == nil || w.Job.ID != job.ID || w.Job.Token == "" {
		t.Fatalf("an edit_run agent job got no token: %+v", w.Job)
	}
	a, err := e.s.TokenActor(e.ctx, w.Job.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsAgent() || a.Agent != "anthropic/sonnet" || a.RunnerJob != job.ID || a.User.ID != e.admin.User.ID {
		t.Errorf("job actor = %+v", a)
	}
	for _, p := range []Permission{PermRunnersRun, PermRequestsAnswer, PermAdmin, PermSessionsStart, PermIssuesDelete} {
		if a.need(p) == nil {
			t.Errorf("the job's token may %s", p)
		}
	}
	if a.need(PermTasksWork) != nil || a.need(PermIssuesComment) != nil {
		t.Error("the job's token can't do an agent's work")
	}
	if _, err := e.s.EnqueueRunnerJob(e.ctx, a, RunnerJobInput{Kind: JobCommand, Input: cmdInput("ls", "/work")}); err == nil {
		t.Error("a job's token queued a runner job")
	}
	toks, _ := e.s.ListAPITokens(e.ctx, e.admin)
	if slices.ContainsFunc(toks, func(k APIToken) bool { return strings.HasPrefix(k.Name, "runner job ") }) {
		t.Errorf("the job's token is in the token list: %+v", toks)
	}

	if _, err := e.s.FinishRunnerJob(e.ctx, e.admin, job.ID, RunnerFinish{State: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.TokenActor(e.ctx, w.Job.Token); err == nil {
		t.Errorf("the token outlived its job: %v", err)
	}
}

// A job's token reaches the job's project and its sub-projects only, as an
// editor at most, and never as an admin, even when its person is one
// (WP-418, WP-ADR28 §7.3).
func TestJobTokenScope(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	sub := e.project(e.admin, "Sub", "")
	if _, err := e.s.UpdateProject(e.ctx, e.admin, sub.Key, ProjectInput{Parent: Some(a.Key)}); err != nil {
		t.Fatal(err)
	}
	other := e.project(bob, "Bobs", "") // robin is only an admin there
	for _, p := range []*Project{a, b, sub, other} {
		e.issue(e.admin, IssueInput{Project: p.Key, Title: "work in " + p.Name})
	}
	diary, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Diary"), Category: str("note")})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Project{a, b} {
		if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Plan " + p.Name), Category: str("plan"), Project: Some(p.Key)}); err != nil {
			t.Fatal(err)
		}
	}
	r := e.runner(e.admin, "box", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	in, _ := json.Marshal(AgentJob{CLI: "claude", Prompt: "hi", Repo: "https://x.test/a/b.git", Permission: AgentEditRun})
	actor := func(project int64) *Actor {
		t.Helper()
		e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: in, ProjectID: project})
		w := e.poll(e.admin, r, RunnerPoll{})
		if w.Job == nil || w.Job.Token == "" {
			t.Fatalf("no job token: %+v", w.Job)
		}
		act, err := e.s.TokenActor(e.ctx, w.Job.Token)
		if err != nil {
			t.Fatal(err)
		}
		return act
	}
	keys := func(act *Actor) []string {
		var out []string
		for _, k := range e.search(act, "") {
			out = append(out, strings.SplitN(k, "-", 2)[0])
		}
		slices.Sort(out)
		return out
	}

	job := actor(a.ID)
	if job.isAdmin() || job.User.IsAdmin || !e.admin.User.IsAdmin {
		t.Error("a job's token is an admin, or its person stopped being one")
	}
	if got := keys(job); !slices.Equal(got, []string{a.Key, sub.Key}) {
		t.Errorf("the job's token finds issues in %v, want %s and %s", got, a.Key, sub.Key)
	}
	if _, err := e.s.GetIssue(e.ctx, job, b.Key+"-1"); err == nil {
		t.Error("the job's token opened an issue in another project")
	}
	e.get(job, sub.Key+"-1")
	if _, err := e.s.CreateIssue(e.ctx, job, IssueInput{Project: b.Key, Title: "elsewhere"}); err == nil {
		t.Error("the job's token filed an issue in another project")
	}
	e.issue(job, IssueInput{Project: a.Key, Title: "found while working"})
	if _, err := e.s.UpdateProject(e.ctx, job, a.Key, ProjectInput{Name: str("Renamed")}); err == nil {
		t.Error("the job's token changed its project's settings: it should be an editor at most")
	}
	ps, err := e.s.ListProjects(e.ctx, job, true)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.Key+":"+p.Role)
	}
	if !slices.Equal(names, []string{a.Key + ":editor", sub.Key + ":editor"}) {
		t.Errorf("the job's token lists projects %v", names)
	}
	pages, err := e.s.ListPages(e.ctx, job, PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].Title != "Plan Alpha" {
		t.Errorf("the job's token lists pages %+v, want only its project's", pages)
	}
	if _, err := e.s.GetPage(e.ctx, job, diary.ID); err == nil {
		t.Error("the job's token opened its person's personal page")
	}

	// An admin's job reaches a project the admin isn't a member of.
	if got := keys(actor(other.ID)); !slices.Equal(got, []string{other.Key}) {
		t.Errorf("a job in %s finds issues in %v", other.Key, got)
	}
	// A job for no project reaches none.
	if got := keys(actor(0)); len(got) != 0 {
		t.Errorf("a job for no project finds issues in %v", got)
	}
}

// A job's token lists its own session only while that has no project, as
// opening it does: once the session moves to a project outside the token's
// scope it is out of the list too (WP-418).
func TestJobTokenOwnSessionList(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	r := e.runner(e.admin, "box", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	in, _ := json.Marshal(AgentJob{CLI: "claude", Prompt: "hi", Repo: "https://x.test/a/b.git", Permission: AgentEditRun})
	e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: in, ProjectID: a.ID})
	w := e.poll(e.admin, r, RunnerPoll{})
	if w.Job == nil || w.Job.Token == "" {
		t.Fatalf("no job token: %+v", w.Job)
	}
	job, err := e.s.TokenActor(e.ctx, w.Job.Token)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.s.SessionHook(e.ctx, agentActor(e.admin, "anthropic/claude-code"), []HookEvent{hook("box", "cli-1", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	job.Session = res.Session
	listed := func() bool {
		for _, x := range sessionsFor(t, e, job, AgentSessionQuery{}) {
			if x.ID == res.Session {
				return true
			}
		}
		return false
	}
	_, getErr := e.s.GetAgentSession(e.ctx, job, res.Session)
	if !listed() || getErr != nil {
		t.Fatalf("the job's own session, no project: listed %v, open err %v", listed(), getErr)
	}
	if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ? WHERE id = ?`, b.ID, res.Session); err != nil {
		t.Fatal(err)
	}
	_, getErr = e.s.GetAgentSession(e.ctx, job, res.Session)
	if listed() || getErr == nil {
		t.Errorf("the job's own session in another project: listed %v, open err %v", listed(), getErr)
	}
}
