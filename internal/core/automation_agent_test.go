package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// ---- a forge that opens pull requests, and a runner that is driven by hand ----

func (g *shipForge) DefaultBranch(ctx context.Context, f *Forge, repo string) (string, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	switch g.defaultBranch {
	case "":
		return "main", nil
	case "-":
		return "", ErrUnavailable
	}
	return g.defaultBranch, nil
}

func (g *shipForge) FileBlobSHA(ctx context.Context, f *Forge, repo, path, ref string) (string, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	if g.union.blobs {
		return "blob-" + ref, nil
	}
	return "", nil
}

func (g *shipForge) CommitFiles(ctx context.Context, f *Forge, repo string, in CommitInput) (*ForgeCommit, error) {
	g.sm.Lock()
	fn := g.commitFn
	g.sm.Unlock()
	if fn != nil {
		return fn(in)
	}
	return nil, errors.New("the fake forge doesn't commit files")
}

func (g *shipForge) CreatePR(ctx context.Context, f *Forge, repo string, in NewPR) (*ForgePR, error) {
	g.sm.Lock()
	if g.createErr != nil {
		defer g.sm.Unlock()
		return nil, g.createErr
	}
	n := 101 + len(g.created)
	g.created = append(g.created, in)
	sha := cmp(g.heads[in.Head], fmt.Sprintf("%07dpr", n))
	g.sm.Unlock()
	state := "open"
	if in.Draft {
		state = "draft"
	}
	pr := ForgePR{Number: n, Title: in.Title, Body: in.Body, URL: fmt.Sprintf("https://git.example.com/%s/pulls/%d", repo, n), State: state,
		Author: cmp(g.createAuthor, "wp-bot"), BaseRef: in.Base, HeadRef: in.Head, HeadSHA: sha, Labels: slices.Clone(in.Labels)}
	g.setPR(f, repo, pr, sampleDiff)
	if g.afterCreate != nil {
		if err := g.afterCreate(pr); err != nil {
			return nil, err
		}
	}
	out := pr
	return &out, nil
}

func (g *shipForge) ChangePRLabels(ctx context.Context, f *Forge, repo string, number int, add, remove []string) error {
	g.sm.Lock()
	for _, l := range add {
		g.labelOps = append(g.labelOps, "+"+l)
	}
	for _, l := range remove {
		g.labelOps = append(g.labelOps, "-"+l)
	}
	g.sm.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if pr := g.prs[mk(f, repo, number)]; pr != nil {
		for _, l := range add {
			if !slices.Contains(pr.Labels, l) {
				pr.Labels = append(pr.Labels, l)
			}
		}
		pr.Labels = slices.DeleteFunc(pr.Labels, func(l string) bool { return slices.Contains(remove, l) })
	}
	return nil
}

func (g *shipForge) RequestReviewers(ctx context.Context, f *Forge, repo string, number int, users []string) error {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.reviewers = append(g.reviewers, users)
	return nil
}

func (g *shipForge) RetryChecks(ctx context.Context, f *Forge, repo, sha string) (int, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.retries = append(g.retries, sha)
	if g.retryNone {
		return 0, nil
	}
	return 1, nil
}

func (g *shipForge) CanRetryChecks(ctx context.Context, f *Forge) (bool, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	return !g.noRetry, nil
}

// pushTo moves a pull request's head, as a push to its branch does.
func (g *shipForge) pushTo(f *Forge, repo string, number int, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prs[mk(f, repo, number)].HeadSHA = sha
	g.diffs[sha] = sampleDiff
}

func (g *shipForge) snapshot() (created []NewPR, labelOps []string, reviewers [][]string, retries []string) {
	g.sm.Lock()
	defer g.sm.Unlock()
	return slices.Clone(g.created), slices.Clone(g.labelOps), slices.Clone(g.reviewers), slices.Clone(g.retries)
}

// agentEnv is the pull request environment with one of the admin's runners.
type agentEnv struct {
	*shipEnv
	box *Runner
}

func newAgentEnv(t *testing.T) *agentEnv {
	se := newShipEnv(t, "forgejo")
	box := se.runner(se.admin, "box", func(r *RunnerRegistration) {
		r.AllowedDirs = []string{"/work"}
		r.CLIs = []RunnerCLI{{Name: "opencode"}, {Name: "claude"}, {Name: "cursor"}}
		r.Features = []string{FeatureConfine, FeatureAudit}
	})
	return &agentEnv{shipEnv: se, box: box}
}

// work carries out what is queued: durable runs, and the background work
// (CI reads, merges) they set off.
func (a *agentEnv) work() {
	a.t.Helper()
	for range 4 {
		a.s.Wait()
		a.s.DrainAutomationRuns(a.ctx)
	}
	a.s.Wait()
}

// rpoll is a runner asking for work (shipEnv's own poll reads CI).
func (a *agentEnv) rpoll(actor *Actor, r *Runner) *RunnerWork {
	a.t.Helper()
	return a.env.poll(actor, r, RunnerPoll{})
}

// take is the runner taking the next job.
func (a *agentEnv) take() *RunnerClaim {
	a.t.Helper()
	w := a.rpoll(a.admin, a.box)
	if w.Job == nil {
		a.t.Fatal("no job was queued for the runner")
	}
	return w.Job
}

// finish is the runner reporting a job's end.
func (a *agentEnv) finish(id int64, state string, result map[string]any, errText string) {
	a.t.Helper()
	raw, _ := json.Marshal(result)
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, id, RunnerFinish{State: state, Result: raw, Error: errText}); err != nil {
		a.t.Fatal(err)
	}
}

func agentJobOf(t *testing.T, c *RunnerClaim) AgentJob {
	t.Helper()
	var j AgentJob
	if err := json.Unmarshal(c.Input, &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func museChain(models ...string) []any {
	var out []any
	for _, m := range models {
		cli, model, _ := strings.Cut(m, " ")
		out = append(out, map[string]any{"cli": cli, "model": model})
	}
	return out
}

func (a *agentEnv) runStatus(r *AutomationRun) *AutomationRun {
	a.t.Helper()
	return a.run(r.ID)
}

// ---- ownership ----

func TestAutomationsRecordWhoSavedThem(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	p := e.project(e.admin, "Owners", "")
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "bob", "owner"); err != nil {
		t.Fatal(err)
	}
	rule := Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "run_command", Extra: map[string]any{"command": "true", "dir": "/work"}}}}
	a := e.addAuto(p, "Runs a command", rule)
	if a.SavedBy == nil || a.SavedBy.ID != e.admin.User.ID {
		t.Fatalf("created by robin, saved by %+v", a.SavedBy)
	}
	// Anyone who saves it takes it over, even to turn it on or off.
	off := false
	a2, err := e.s.UpdateAutomation(e.ctx, bob, a.ID, AutomationInput{Enabled: &off})
	if err != nil || a2.SavedBy == nil || a2.SavedBy.ID != bob.User.ID {
		t.Fatalf("after bob turned it off: %+v %v", a2.SavedBy, err)
	}
	if got := automationRunnerOwner(&Automation{savedBy: bob.User.ID}); got != bob.User.ID {
		t.Errorf("runner owner = %d", got)
	}
	// The editor is told whose runners it uses, and whether one is online.
	if a2.Runners == nil || a2.Runners.Owner.ID != bob.User.ID || a2.Runners.Total != 0 || a2.Runners.Online != 0 {
		t.Errorf("runners = %+v", a2.Runners)
	}
	e.runner(bob, "bobs", nil)
	got, err := e.s.GetAutomation(e.ctx, e.admin, a.ID)
	if err != nil || got.Runners == nil || got.Runners.Total != 1 || got.Runners.Online != 1 {
		t.Errorf("with a runner: %+v %v", got.Runners, err)
	}
	// An automation without runner steps says nothing about runners.
	plain := e.addAuto(p, "Plain", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "notify", User: "owners", Message: "hi"}}})
	if plain.Runners != nil {
		t.Errorf("plain automation has runners: %+v", plain.Runners)
	}
	// An agent saving one names nobody, so its runner steps are refused until a person saves it.
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-code"}
	b, err := e.s.CreateAutomation(e.ctx, agent, p.Key, AutomationInput{Name: str("By an agent"), Rule: &rule})
	if err != nil {
		t.Fatal(err)
	}
	if b.SavedBy != nil || automationRunnerOwner(b) != 0 {
		t.Errorf("an agent became an owner: %+v", b.SavedBy)
	}
	if b.Runners == nil || b.Runners.Owner != nil {
		t.Errorf("runners of an agent's automation = %+v", b.Runners)
	}
}

func TestAutomationsReachOnlyTheSaversRunners(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	mine := e.runner(e.admin, "mine", nil)
	theirs := e.runner(bob, "theirs", nil)
	job := func(owner int64) RunnerJobInput {
		return RunnerJobInput{Kind: JobCommand, OwnerID: owner, Input: cmdInput("true", "/work"), Source: "automation_step", SourceID: 9}
	}
	// The automation was saved by robin: her runners, not bob's.
	auto := &Actor{User: &User{ID: 0, Username: "system"}, Automation: &Automation{ID: 1, Name: "nightly", savedBy: e.admin.User.ID}}
	if _, err := e.s.EnqueueRunnerJob(e.ctx, auto, job(bob.User.ID)); !errors.Is(err, ErrForbidden) {
		t.Errorf("an automation saved by robin used bob's runner: %v", err)
	}
	j, err := e.s.EnqueueRunnerJob(e.ctx, auto, job(e.admin.User.ID))
	if err != nil {
		t.Fatalf("robin's automation on robin's runner: %v", err)
	}
	if w := e.poll(bob, theirs, RunnerPoll{}); w.Job != nil {
		t.Errorf("bob's runner took robin's job: %+v", w.Job)
	}
	if w := e.poll(e.admin, mine, RunnerPoll{}); w.Job == nil || w.Job.ID != j.ID {
		t.Errorf("robin's runner should take it: %+v", w.Job)
	}
	// Run as bob (he triggered it), it still goes to robin's runner, never his.
	asBob := &Actor{User: bob.User, Automation: auto.Automation}
	if _, err := e.s.EnqueueRunnerJob(e.ctx, asBob, job(bob.User.ID)); !errors.Is(err, ErrForbidden) {
		t.Errorf("triggering the automation gave bob's runner to it: %v", err)
	}
	// Saved by nobody: no runner at all.
	orphan := &Actor{User: bob.User, Automation: &Automation{ID: 2, Name: "orphan"}}
	if _, err := e.s.EnqueueRunnerJob(e.ctx, orphan, job(bob.User.ID)); !errors.Is(err, ErrForbidden) {
		t.Errorf("an automation nobody saved used a runner: %v", err)
	}
}

// ---- agent_task ----

func TestAgentTaskRunsOnARunnerAndWaitsForIt(t *testing.T) {
	ae := newAgentEnv(t)
	bob := ae.user("bob", false)
	bobBox := ae.runner(bob, "bobbox", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "opencode"}} })
	a := ae.addAuto(ae.p, "Review", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "agent_task", ID: "review", Extra: map[string]any{
		"agents": museChain("opencode opencode/muse-spark-1.3-contributor-free"), "permission": AgentEdit, "branch": "review/{{.Date}}",
		"prompt": "Review {{.Project.Name}}", "commit_message": "Suggestions",
	}}, {Type: "notify", User: "admins", Message: "{{.Steps.review.Answer}} on {{.Steps.review.Branch}}"}}})
	run := ae.manual(a, nil)
	ae.work()
	if r := ae.runStatus(run); r.Status != RunWaiting {
		t.Fatalf("status = %s (%s)", r.Status, r.Error)
	}
	// The job went to the person who saved the automation, never to bob's runner.
	if w := ae.rpoll(bob, bobBox); w.Job != nil {
		t.Fatalf("bob's runner took the job: %+v", w.Job)
	}
	claim := ae.take()
	in := agentJobOf(t, claim)
	if claim.Kind != JobAgent || in.CLI != "opencode" || in.Model != "opencode/muse-spark-1.3-contributor-free" || in.Prompt != "Review App" ||
		in.Repo != "https://git.example.com/me/app.git" || in.Branch != "review/2026-10-05" || !in.Push || in.Permission != AgentEdit || in.CommitMessage != "Suggestions" {
		t.Fatalf("job input = %+v", in)
	}
	// Running the step again (a restart) doesn't queue a second job.
	ae.s.resumeRuns(ae.ctx)
	ae.work()
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatalf("a second job was queued: %+v", w.Job)
	}
	ae.finish(claim.ID, "done", map[string]any{"answer": "Two fixes.", "branch": "review/2026-10-05", "commits": []string{"c1", "c2"}, "commit": "c2", "pushed": true,
		"files": []string{"a.go"}, "tokens": map[string]any{"input": 100, "output": 20}}, "")
	ae.work()
	r := ae.runStatus(run)
	if r.Status != RunSucceeded {
		t.Fatalf("status = %s (%s)", r.Status, r.Error)
	}
	st := stepByID(r, "review")
	if st.Outputs["Answer"] != "Two fixes." || st.Outputs["Branch"] != "review/2026-10-05" || st.Outputs["Commit"] != "c2" || st.Outputs["Pushed"] != true ||
		len(st.Outputs["Commits"].([]any)) != 2 || st.Outputs["Tokens"].(map[string]any)["Output"] != 20.0 || st.Outputs["Model"] != "opencode opencode/muse-spark-1.3-contributor-free" {
		t.Errorf("outputs = %+v", st.Outputs)
	}
	ns, _, _ := ae.s.ListNotifications(ae.ctx, ae.admin, false, 5)
	if len(ns) == 0 || ns[0].Body != "Two fixes. on review/2026-10-05" {
		t.Errorf("notification = %+v", ns)
	}
}

func TestAgentTaskModelChainAndPermissions(t *testing.T) {
	ae := newAgentEnv(t)
	a := ae.addAuto(ae.p, "Chain", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "agent_task", ID: "t", Extra: map[string]any{
		"agents": museChain("opencode opencode/free-model", "claude sonnet"), "permission": AgentRead, "prompt": "Look around",
	}}}})
	run := ae.manual(a, nil)
	ae.work()
	first := ae.take()
	if in := agentJobOf(t, first); in.CLI != "opencode" || in.Model != "opencode/free-model" || in.Permission != AgentRead || !in.ReadOnly || in.Branch != "" || in.Push {
		t.Fatalf("first job = %+v", in)
	}
	// A rate limit is the model's fault: the next model is tried.
	ae.finish(first.ID, "failed", nil, "opencode reported an error: rate limit exceeded (429)")
	ae.work()
	second := ae.take()
	if in := agentJobOf(t, second); in.CLI != "claude" || in.Model != "sonnet" {
		t.Fatalf("second job = %+v", in)
	}
	ae.finish(second.ID, "done", map[string]any{"answer": "Nothing to change."}, "")
	ae.work()
	r := ae.runStatus(run)
	st := stepByID(r, "t")
	if r.Status != RunSucceeded || st.Outputs["Model"] != "claude sonnet" || st.Outputs["Answer"] != "Nothing to change." || !strings.Contains(st.Log, "trying claude sonnet next") {
		t.Fatalf("run = %s %q outputs %+v log %q", r.Status, r.Error, st.Outputs, st.Log)
	}

	// A failure that isn't the model's (the repository couldn't be fetched) stops there.
	run2 := ae.manual(a, nil)
	ae.work()
	j := ae.take()
	ae.finish(j.ID, "failed", nil, "can't get the repository: authentication failed")
	ae.work()
	r2 := ae.runStatus(run2)
	if r2.Status != RunFailed || !strings.Contains(r2.Error, "authentication failed") {
		t.Fatalf("run 2 = %s %q", r2.Status, r2.Error)
	}
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Errorf("the next model was tried after a git failure: %+v", w.Job)
	}

	// Edit and run commands is passed on, and a chain needs a valid CLI.
	b := ae.addAuto(ae.p, "Fixes", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "agent_task", Extra: map[string]any{
		"agents": museChain("claude sonnet"), "permission": AgentEditRun, "prompt": "Fix the tests", "branch": "fix/x", "push": false,
	}}}})
	ae.manual(b, nil)
	ae.work()
	if in := agentJobOf(t, ae.take()); in.Permission != AgentEditRun || in.Push || in.Branch != "fix/x" {
		t.Errorf("edit+run job = %+v", in)
	}
	for name, extra := range map[string]map[string]any{
		"no agents":   {"prompt": "x"},
		"unknown cli": {"agents": museChain("vim x"), "prompt": "x"},
		"no prompt":   {"agents": museChain("claude x")},
		"bad perm":    {"agents": museChain("claude x"), "prompt": "x", "permission": "root"},
	} {
		rule := Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "agent_task", Extra: extra}}}
		if _, err := ae.s.CreateAutomation(ae.ctx, ae.admin, ae.p.Key, AutomationInput{Name: str(name), Rule: &rule}); !IsInvalid(err) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

func TestAgentTaskWithoutARunnerFailsClearly(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "NoRunner", "")
	e.s.Forges = newShipForge()
	f := e.forge("forgejo", "https://git.example.com", false)
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "me/app", false); err != nil {
		t.Fatal(err)
	}
	a := e.addAuto(p, "Alone", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "agent_task", Extra: map[string]any{"agents": museChain("claude x"), "prompt": "hi"}}}})
	run := e.manual(a, nil)
	e.s.DrainAutomationRuns(e.ctx)
	r := e.run(run.ID)
	if r.Status != RunFailed || !strings.Contains(r.Error, "robin has no runner") {
		t.Errorf("run = %s %q", r.Status, r.Error)
	}
}

func TestAgentTaskTimesOutAndCancelsTheJob(t *testing.T) {
	ae := newAgentEnv(t)
	a := ae.addAuto(ae.p, "Slow", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "agent_task", ID: "t", Extra: map[string]any{
		"agents": museChain("claude x"), "prompt": "hi", "timeout": "10m"}}}})
	run := ae.manual(a, nil)
	ae.work()
	claim := ae.take()
	// The runner never reports: after the job's time and the day a job may sit, the step gives up.
	ae.setClock(ae.clockNow().Add(26 * time.Hour))
	ae.work()
	r := ae.runStatus(run)
	if r.Status != RunFailed || !strings.Contains(r.Error, "didn't finish in time") {
		t.Fatalf("run = %s %q", r.Status, r.Error)
	}
	info, err := ae.s.GetRunnerJob(ae.ctx, ae.admin, claim.ID, -1)
	if err != nil || info.State != "cancelled" {
		t.Errorf("job = %+v %v", info, err)
	}
}

// ---- run_command ----

func TestRunCommandStep(t *testing.T) {
	ae := newAgentEnv(t)
	a := ae.addAuto(ae.p, "Deploy", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "run_command", ID: "deploy", OnError: "continue", Extra: map[string]any{"command": "make deploy-remote", "dir": "/work/app"}},
		{Type: "notify", User: "admins", Message: "exit {{.Steps.deploy.ExitCode}}: {{.Steps.deploy.Output}}"},
	}})
	run := ae.manual(a, nil)
	ae.work()
	claim := ae.take()
	var cj CommandJob
	if err := json.Unmarshal(claim.Input, &cj); err != nil || claim.Kind != JobCommand || cj.Command != "make deploy-remote" || cj.Dir != "/work/app" {
		t.Fatalf("job = %+v %+v", claim, cj)
	}
	if _, err := ae.s.AppendRunnerLog(ae.ctx, ae.admin, claim.ID, 0, "building\nboom\n"); err != nil {
		t.Fatal(err)
	}
	three := 3
	if _, err := ae.s.FinishRunnerJob(ae.ctx, ae.admin, claim.ID, RunnerFinish{State: "failed", ExitCode: &three, Error: "exit status 3"}); err != nil {
		t.Fatal(err)
	}
	ae.work()
	r := ae.runStatus(run)
	st := stepByID(r, "deploy")
	// The step failed but the run carried on, with the exit code and the end of the output.
	if r.Status != RunSucceeded || st.Status != "failed" || st.Outputs["ExitCode"] != 3.0 || st.Outputs["OK"] != false || st.Outputs["Output"] != "building\nboom\n" {
		t.Fatalf("run = %s %q, step %+v", r.Status, r.Error, st)
	}
	ns, _, _ := ae.s.ListNotifications(ae.ctx, ae.admin, false, 5)
	if len(ns) == 0 || !strings.HasPrefix(ns[0].Body, "exit 3: building") {
		t.Errorf("notification = %+v", ns)
	}

	// A directory the runner doesn't allow is refused, saying so.
	b := ae.addAuto(ae.p, "Elsewhere", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "run_command", Extra: map[string]any{"command": "ls", "dir": "/etc"}}}})
	run2 := ae.manual(b, nil)
	ae.work()
	r2 := ae.runStatus(run2)
	if r2.Status != RunFailed || !strings.Contains(r2.Error, "no runner allows /etc") {
		t.Errorf("outside the allowed dirs: %s %q", r2.Status, r2.Error)
	}
	// Success sets OK.
	c := ae.addAuto(ae.p, "Fine", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{{Type: "run_command", ID: "x", Extra: map[string]any{"command": "true", "dir": "/work"}}}})
	run3 := ae.manual(c, nil)
	ae.work()
	zero := 0
	j := ae.take()
	if _, err := ae.s.FinishRunnerJob(ae.ctx, ae.admin, j.ID, RunnerFinish{State: "done", ExitCode: &zero}); err != nil {
		t.Fatal(err)
	}
	ae.work()
	if st := stepByID(ae.runStatus(run3), "x"); st.Status != "succeeded" || st.Outputs["OK"] != true {
		t.Errorf("success step = %+v", st)
	}
}

func stepIter(r *AutomationRun, id string, iter int) *RunStep {
	for _, s := range r.Steps {
		if s.StepID == id && s.Iteration == iter {
			return s
		}
	}
	return nil
}

// Robin's example, from the gallery card: every Tuesday at 12:00 an agent on
// a free model reviews the repository and commits suggestions to a branch;
// the rule opens a pull request, finds CI failing, has an agent fix it, waits
// for the review bot and merges.
func TestWeeklyReviewIntoAPullRequest(t *testing.T) {
	ae := newAgentEnv(t)
	ae.gate("P2")
	ae.policy(MergePolicyInput{AutoMerge: ptr(false), ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(0)}) // the rule merges, not the setting
	ae.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	card := ae.addAuto(ae.p, "Weekly code review into a PR", templateRule(t, "Weekly code review into a PR"))
	if card.Runners == nil || card.Runners.Owner.ID != ae.admin.User.ID || card.Runners.Online != 1 {
		t.Fatalf("the card should say whose runners it uses: %+v", card.Runners)
	}

	// Tuesday, just after 12:00.
	ae.setClock(time.Date(2026, 10, 6, 12, 1, 0, 0, time.UTC))
	ae.s.RunTimedAutomations(ae.ctx)
	ae.work()
	runs := ae.env.runs(card)
	if len(runs) != 1 {
		t.Fatalf("%d runs", len(runs))
	}
	run := runs[0]

	// 1. The agent reviews on a branch, on the model the card names.
	claim := ae.take()
	in := agentJobOf(t, claim)
	if in.CLI != "opencode" || in.Model != "opencode/muse-spark-1.3-contributor-free" || in.Permission != AgentEdit || in.Branch != "review/2026-10-06" || !in.Push ||
		!strings.Contains(in.Prompt, "careful senior engineer") {
		t.Fatalf("review job = %+v", in)
	}
	const reviewHead, fixedHead = "aaaaaaa1111", "bbbbbbb2222"
	ae.sg.heads["review/2026-10-06"] = reviewHead
	ae.sg.logs[ciBackend] = "--- FAIL: TestParse\n    parse_test.go:40: unexpected token\n"
	ae.sg.setChecks(reviewHead, CommitCheck{Name: ciBackend, State: CheckFailed, URL: "https://ci.example.com/1"}, CommitCheck{Name: ciWeb, State: CheckSuccess})
	ae.finish(claim.ID, "done", map[string]any{"answer": "Tightened two error paths.", "branch": "review/2026-10-06", "commits": []string{"c1"}, "commit": "c1", "pushed": true, "files": []string{"parse.go", "util.go"}}, "")
	ae.work()

	// 2. A pull request is open and CI fails on it, so a second agent is asked to fix it.
	created, _, _, _ := ae.sg.snapshot()
	if len(created) != 1 || created[0].Head != "review/2026-10-06" || created[0].Title != "Weekly code review 2026-10-06" || !strings.Contains(created[0].Body, "Tightened two error paths.") {
		t.Fatalf("pull request = %+v", created)
	}
	if got := ae.runStatus(run); got.Status != RunWaiting {
		t.Fatalf("after opening the pull request: %s (%s)", got.Status, got.Error)
	}
	fixClaim := ae.take()
	fin := agentJobOf(t, fixClaim)
	if fin.Permission != AgentEditRun || fin.Base != "review/2026-10-06" || fin.Branch != "review/2026-10-06" || !fin.Push || fin.Model != "opencode/muse-spark-1.3-contributor-free" ||
		!strings.Contains(fin.Prompt, "unexpected token") || !slices.Contains(fin.ProtectPaths, ".forgejo/**") {
		t.Fatalf("fix job = %+v", fin)
	}
	// It pushes a fix; CI passes on the new head.
	ae.sg.pushTo(ae.f, "me/app", 101, fixedHead)
	ae.sg.setChecks(fixedHead, CommitCheck{Name: ciBackend, State: CheckSuccess}, CommitCheck{Name: ciWeb, State: CheckSuccess})
	ae.finish(fixClaim.ID, "done", map[string]any{"answer": "The parser rejected valid input.", "commits": []string{"c2"}, "commit": "c2", "pushed": true, "files": []string{"parse.go"}}, "")
	ae.work()

	// 3. The review bot looks at the new head; its gate goes green and the rule merges.
	ae.drain()
	ae.work()
	got := ae.runStatus(run)
	if got.Status != RunSucceeded {
		for _, s := range got.Steps {
			t.Logf("step %s#%d %s %s %s", s.StepID, s.Iteration, s.Type, s.Status, s.Error)
		}
		t.Fatalf("run = %s (%s)", got.Status, got.Error)
	}
	if ci1, ci2 := stepIter(got, "ci", 1), stepIter(got, "ci", 2); ci1.Outputs["Passed"] != false || ci2 == nil || ci2.Outputs["Passed"] != true {
		t.Errorf("CI steps: %+v / %+v", ci1.Outputs, ci2)
	}
	if fx1, fx2 := stepIter(got, "fix", 1), stepIter(got, "fix", 2); fx1.Outputs["Fixed"] != true || fx2 == nil || fx2.Status != "skipped" {
		t.Errorf("fix steps: %+v / %+v", fx1, fx2)
	}
	if loop := stepByID(got, "babysit"); loop.Outputs["Iterations"] != 2.0 || loop.Outputs["Exhausted"] != false {
		t.Errorf("loop = %+v", loop.Outputs)
	}
	ms := ae.sg.merged()
	if len(ms) != 1 || ms[0].Style != MergeSquash || !ms[0].DeleteBranch || ms[0].HeadSHA != fixedHead {
		t.Fatalf("merges = %+v", ms)
	}
	if pr := ae.pr(101); pr.State != "merged" {
		t.Errorf("pull request = %+v", pr)
	}
	ns, _, _ := ae.s.ListNotifications(ae.ctx, ae.admin, false, 5)
	if len(ns) == 0 || ns[0].Title != "Weekly code review" || !strings.Contains(ns[0].Body, "was merged: https://git.example.com/me/app/pulls/101") {
		t.Errorf("notification = %+v", ns)
	}
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Errorf("a stray job: %+v", w.Job)
	}
}
