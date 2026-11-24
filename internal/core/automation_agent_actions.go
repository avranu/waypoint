package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Jobs go to the runners of whoever last saved the automation; a rerun after a wait or restart finds its job by step key and never queues a second.

// PAgents: a chain of coding-agent CLIs and models, tried in order.
const PAgents = "agents"

// AgentModel is one entry of an agent chain: a CLI and the model it is
// asked to use (empty: the CLI's own default).
type AgentModel struct {
	CLI   string `json:"cli"`
	Model string `json:"model,omitempty"`
}

func (m AgentModel) String() string {
	if m.Model == "" {
		return m.CLI
	}
	return m.CLI + " " + m.Model
}

// agentChain reads the chain a step names.
func agentChain(v any) ([]AgentModel, error) {
	var list []any
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []any:
		list = x
	case []map[string]any:
		for _, m := range x {
			list = append(list, m)
		}
	default:
		return nil, invalid("agents must be a list of CLI and model")
	}
	var out []AgentModel
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, invalid("agents must be a list of CLI and model")
		}
		am := AgentModel{CLI: strings.TrimSpace(anyString(m["cli"])), Model: strings.TrimSpace(anyString(m["model"]))}
		if !slices.Contains(runnerCLIs, am.CLI) {
			return nil, invalid("agent cli must be one of %s", strings.Join(runnerCLIs, ", "))
		}
		if am.Model != "" && !modelRE.MatchString(am.Model) {
			return nil, invalid("model %q isn't allowed", am.Model)
		}
		out = append(out, am)
	}
	if len(out) > 6 {
		return nil, invalid("a chain can try at most 6 models")
	}
	return out, nil
}

func checkAgents(_ *Rule, a *Action) error {
	chain, err := agentChain(a.Extra["agents"])
	if err != nil {
		return err
	}
	if len(chain) == 0 {
		return invalid("%s needs a CLI and a model", a.Type)
	}
	return nil
}

// stepRepo is the repository a step works in.
type stepRepo struct {
	Forge *Forge
	Repo  string
}

// cloneURL is where the person's runner clones it from; their own git
// credentials do the signing in.
func (r *stepRepo) cloneURL() string {
	return strings.TrimRight(r.Forge.BaseURL, "/") + "/" + r.Repo + ".git"
}

// projectRepos lists the repositories attached to a project.
func (s *Service) projectRepoList(ctx context.Context, projectID int64) ([]stepRepo, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, repo FROM project_repos WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	type fr struct {
		forge int64
		repo  string
	}
	var list []fr
	for rows.Next() {
		var x fr
		if err := rows.Scan(&x.forge, &x.repo); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, x)
	}
	rows.Close()
	var out []stepRepo
	for _, x := range list {
		f, err := s.forgeByID(ctx, x.forge)
		if err != nil {
			return nil, err
		}
		out = append(out, stepRepo{Forge: f, Repo: x.repo})
	}
	return out, nil
}

// stepProject is the project a step acts in: the automation's, or for an
// admin automation the event's.
func (c *StepCtx) stepProject() int64 {
	return cmpID(c.Automation.ProjectID, c.Event.ProjectID)
}

// Repo finds the repository a step works on: the one named by the step's
// repo setting (it must be attached to the project), else the pull request
// the trigger is about, else the project's only repository.
func (c *StepCtx) Repo(param string) (*stepRepo, error) {
	name, err := c.Render(param)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	pid := c.stepProject()
	if pid == 0 {
		return nil, invalid("this step needs a project with a repository")
	}
	repos, err := c.S.projectRepoList(c.Ctx, pid)
	if err != nil {
		return nil, err
	}
	switch {
	case name != "":
		for i := range repos {
			if strings.EqualFold(repos[i].Repo, name) {
				return &repos[i], nil
			}
		}
		return nil, invalid("%s isn't a repository of this project", name)
	case c.data.PR != nil:
		for i := range repos {
			if repos[i].Forge.ID == c.data.PR.ForgeID && strings.EqualFold(repos[i].Repo, c.data.PR.Repo) {
				return &repos[i], nil
			}
		}
	}
	switch len(repos) {
	case 0:
		return nil, invalid("this project has no repository: attach one in its settings")
	case 1:
		return &repos[0], nil
	}
	return nil, invalid("this project has several repositories: say which one in the step")
}

// BoolDefault is a yes/no setting that is def when unset.
func (c *StepCtx) BoolDefault(name string, def bool) bool {
	v, ok := c.params[name]
	if !ok || v == nil || anyString(v) == "" {
		return def
	}
	return c.Bool(name)
}

// stepJob is a runner job a step queued, as the step reads it.
type stepJob struct {
	ID     int64
	State  string
	Exit   *int
	Result map[string]any
	Error  string
	Tail   string // the end of its output
}

func (j *stepJob) finished() bool {
	return slices.Contains([]string{"done", "failed", "cancelled", "expired"}, j.State)
}

const jobTailBytes = 4096

func (s *Service) readStepJob(ctx context.Context, where string, args ...any) (*stepJob, error) {
	var j stepJob
	var exit sql.NullInt64
	var result sql.NullString
	var tail sql.NullString
	// #nosec G202 -- where is one of two fixed conditions; values are bound
	err := s.db.R.QueryRowContext(ctx, `SELECT j.id, j.state, j.exit_code, j.result, j.error,
		(SELECT CAST(substr(CAST(l.text AS BLOB), -`+fmt.Sprint(jobTailBytes)+`) AS TEXT) FROM runner_job_logs l WHERE l.job_id = j.id)
		FROM runner_jobs j WHERE `+where, args...).Scan(&j.ID, &j.State, &exit, &result, &j.Error, &tail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if exit.Valid {
		c := int(exit.Int64)
		j.Exit = &c
	}
	if result.Valid {
		_ = json.Unmarshal([]byte(result.String), &j.Result)
	}
	j.Tail = strings.ToValidUTF8(tail.String, "")
	if !utf8.ValidString(j.Tail) {
		j.Tail = ""
	}
	return &j, nil
}

func (s *Service) stepJobByKey(ctx context.Context, runID int64, key string) (*stepJob, error) {
	return s.readStepJob(ctx, `j.source = 'automation_step' AND j.source_id = ? AND j.step_key != '' AND j.step_key = ?`, runID, key)
}

func (s *Service) stepJobByID(ctx context.Context, id int64) (*stepJob, error) {
	return s.readStepJob(ctx, `j.id = ?`, id)
}

// cancelStepJob stops a job whose step gave up waiting for it.
func (s *Service) cancelStepJob(ctx context.Context, id int64, why string) {
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		return t.finishRunnerJob(ctx, id, "cancelled", nil, nil, why)
	})
	if err != nil {
		s.log.WarnContext(ctx, "cancel an automation's runner job", "job", id, "err", err)
	}
	s.kickRunners()
}

// jobWaitMargin is how long past its own timeout a job may take to be
// finished: it can sit in the queue for a day before it expires.
const jobWaitMargin = runnerQueuedFor + 15*time.Minute

// RunnerJob queues a job for this step on the runners of the person who
// last saved the automation (or finds the one it queued already), parks the
// run until it finishes and returns it. slot numbers the jobs one step
// queues (a chain of models tries one after another).
func (c *StepCtx) RunnerJob(slot int, in RunnerJobInput) (*stepJob, error) {
	owner := automationRunnerOwner(c.Automation)
	if owner == 0 {
		return nil, invalid("this automation has no owner for its runners: an agent saved it, or it was never saved. Save it yourself to use your runners")
	}
	key := fmt.Sprintf("%s#%d#%d#%d", c.ID, c.Iteration, c.Attempt, slot)
	job, err := c.S.stepJobByKey(c.Ctx, c.Run.ID, key)
	if err != nil {
		return nil, err
	}
	if job == nil {
		in.OwnerID, in.Source, in.SourceID, in.StepKey = owner, "automation_step", c.Run.ID, key
		queued, err := c.S.EnqueueRunnerJob(c.Ctx, c.Actor, in)
		switch {
		case errors.Is(err, ErrNoRunner):
			return nil, fmt.Errorf("%w: %s has no runner%s to run this on: connect one with wp runner", ErrNoRunner, c.runnerOwnerName(owner), labelNote(in.Label))
		case err != nil:
			return nil, err
		}
		c.Logf("queued runner job %d: %s", queued.ID, c.S.BaseURL()+"/runner-jobs/"+itoa64(queued.ID))
		if job, err = c.S.stepJobByID(c.Ctx, queued.ID); err != nil {
			return nil, err
		}
	}
	c.Set("JobID", job.ID)
	c.Set("JobURL", c.S.BaseURL()+"/runner-jobs/"+itoa64(job.ID))
	if job.finished() {
		return job, nil
	}
	timeout := time.Duration(in.Timeout)*time.Second + jobWaitMargin
	_, timedOut, err := c.WaitSlot(slot, EvRunnerJobFinished, "job:"+itoa64(job.ID), "", timeout, func() (*Event, bool) {
		j, err := c.S.stepJobByID(c.Ctx, job.ID)
		if err != nil || j == nil || !j.finished() {
			return nil, false
		}
		return &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"job": job.ID, "state": j.State}}, true
	})
	if err != nil {
		return nil, err
	}
	if timedOut {
		c.S.cancelStepJob(c.Ctx, job.ID, "The automation stopped waiting for this job.")
		return nil, fmt.Errorf("the runner job %d didn't finish in time and was cancelled", job.ID)
	}
	if job, err = c.S.stepJobByID(c.Ctx, job.ID); err != nil {
		return nil, err
	}
	if job == nil || !job.finished() {
		return nil, errors.New("the runner job is gone")
	}
	return job, nil
}

func labelNote(label string) string {
	if label == "" {
		return ""
	}
	return " labelled " + label
}

func (c *StepCtx) runnerOwnerName(id int64) string {
	if u, err := c.S.userByID(c.Ctx, c.S.db.R, id); err == nil {
		return u.DisplayName
	}
	return "its owner"
}

// modelFailureWords in a failed job's message or output say the model, not
// the work, was the problem: a limit, an outage, a missing model or sign-in.
var modelFailureWords = []string{"rate limit", "rate-limit", "429", "529", "quota", "usage limit", "overloaded", "model not found", "unknown model",
	"model_not_found", "capacity", "insufficient", "credit balance", "billing", "isn't signed in", "not signed in", "ended without an answer",
	"too many requests", "temporarily unavailable", "context length", "context window", "reported an error", "resource_exhausted",
	"spend limit", "invalid model", "cannot use this model"}

// limitWords say a plan's or provider's usage limit stopped the job: what
// a subscription holds for the week or month is spent (Cursor's included
// pool, WP-305), or a rate limit. A step that runs again tomorrow can wait.
var limitWords = []string{"rate limit", "rate-limit", "rate_limit", "quota", "usage limit", "too many requests", "resource_exhausted", "spend limit",
	"credit balance", "out of usage", "hit your limit"}

// usageLimited: the job failed on a usage or rate limit.
func usageLimited(j *stepJob) bool {
	if j == nil || j.State != "failed" {
		return false
	}
	return limitText(j.Error + "\n" + lastBytes(j.Tail, 1500))
}

// notModelWords mark failures of the checkout or the push, which another
// model can't fix.
var notModelWords = []string{"can't get the repository", "pushing ", "protected files", "committing the agent", "git add failed", "can't find", "isn't installed", "timed out"}

// modelFailure: the job failed for a reason another model might avoid.
func modelFailure(j *stepJob) bool {
	text := strings.ToLower(j.Error + "\n" + lastBytes(j.Tail, 1500))
	for _, w := range notModelWords {
		if strings.Contains(strings.ToLower(j.Error), w) {
			return false
		}
	}
	return slices.ContainsFunc(modelFailureWords, func(w string) bool { return strings.Contains(text, w) })
}

func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// agentSpec is what an agent step asks of a runner.
type agentSpec struct {
	Chain      []AgentModel
	Prompt     string
	Repo       *stepRepo
	Base       string
	Branch     string
	Push       bool
	Permission string
	Message    string
	Label      string
	Timeout    time.Duration
	Protect    []string // paths the commits may not touch
}

// agentOutcome is a finished agent job as steps read it.
type agentOutcome struct {
	Answer  string
	Branch  string
	Commits []string
	Commit  string
	Files   []string
	Pushed  bool
	Used    AgentModel
	Job     *stepJob
}

// jobProject is the project an agent step's job works for: the repo's, or
// the automation's when no project has the repo.
func (c *StepCtx) jobProject(r *stepRepo) (int64, error) {
	if r != nil && r.Forge != nil {
		if pid := c.stepProject(); c.Automation != nil && pid != 0 {
			var n int
			if err := c.S.db.R.QueryRowContext(c.Ctx, `SELECT COUNT(*) FROM project_repos WHERE project_id = ? AND forge_id = ? AND repo = ? COLLATE NOCASE`, pid, r.Forge.ID, r.Repo).Scan(&n); err != nil {
				return 0, err
			}
			if n > 0 {
				return pid, nil
			}
		}
		id, err := c.S.attachedProject(c.Ctx, c.S.db.R, r.Forge.ID, r.Repo)
		if err != nil {
			return 0, err // On a lookup error do not fall back to the automation's project: the job would be filed under the wrong one.
		}
		if id != 0 {
			return id, nil
		}
	}
	if c.Automation != nil {
		return c.Automation.ProjectID, nil
	}
	return 0, nil
}

// RunAgent runs the spec on a runner, trying each model of the chain until
// one works, and leaves the step's outputs.
func (c *StepCtx) RunAgent(spec agentSpec) (*agentOutcome, error) {
	var lastErr error
	for i, m := range spec.Chain {
		in := AgentJob{CLI: m.CLI, Model: m.Model, Prompt: spec.Prompt, Repo: spec.Repo.cloneURL(), Base: spec.Base, Branch: spec.Branch,
			Push: spec.Push, Permission: spec.Permission, CommitMessage: spec.Message, ProtectPaths: spec.Protect}
		raw, _ := json.Marshal(in)
		projectID, err := c.jobProject(spec.Repo)
		if err != nil {
			return nil, err
		}
		job, err := c.RunnerJob(i, RunnerJobInput{Kind: JobAgent, Label: spec.Label, Input: raw, Timeout: int(spec.Timeout.Seconds()), ProjectID: projectID})
		if err != nil {
			return nil, err
		}
		out := agentResult(job)
		out.Used = m
		c.setAgentOutputs(out, job)
		if job.State == "done" {
			if bad := protectedTouched(spec.Protect, out.Files); len(bad) > 0 {
				return out, fmt.Errorf("the agent changed protected files (%s), so its commits are refused", strings.Join(bad, ", "))
			}
			return out, nil
		}
		lastErr = fmt.Errorf("%s %s: %s", m, job.State, cmp(job.Error, "no reason given"))
		if job.State == "failed" && i+1 < len(spec.Chain) && modelFailure(job) {
			c.Logf("%s; trying %s next", lastErr, spec.Chain[i+1])
			continue
		}
		return out, lastErr
	}
	return nil, cmpErr(lastErr, invalid("there is no model to try"))
}

func agentResult(job *stepJob) *agentOutcome {
	out := &agentOutcome{Job: job, Commits: []string{}, Files: []string{}}
	r := job.Result
	out.Answer = anyString(r["answer"])
	out.Branch = anyString(r["branch"])
	out.Commit = anyString(r["commit"])
	out.Pushed = anyBool(r["pushed"])
	out.Commits = append(out.Commits, anyStrings(r["commits"])...)
	out.Files = append(out.Files, anyFileStrings(r["files"])...)
	return out
}

func (c *StepCtx) setAgentOutputs(o *agentOutcome, job *stepJob) {
	c.Set("Answer", o.Answer)
	c.Set("Branch", o.Branch)
	c.Set("Commits", o.Commits)
	c.Set("Commit", o.Commit)
	c.Set("Files", o.Files)
	c.Set("Pushed", o.Pushed)
	c.Set("Model", o.Used.String())
	c.Set("Limited", usageLimited(job))
	if t, ok := job.Result["tokens"].(map[string]any); ok {
		c.Set("Tokens", map[string]any{"Input": t["input"], "Output": t["output"], "CacheRead": t["cache_read"], "Cost": t["cost_usd"]})
	}
	if s, ok := job.Result["structured"]; ok {
		c.Set("Data", s)
	}
}

// protectedTouched lists the files that match a protected path and are not
// exempted. A "!" glob exempts files from single-file protected globs (like
// **/.env.*). A directory glob (ending in "/**", like .github/**) protects
// what is under it whatever those say: only a "!" glob written for that
// directory lets anything out (**/secrets/**/*.go, WP-750).
func protectedTouched(globs, files []string) []string {
	if len(globs) == 0 || len(files) == 0 {
		return nil
	}
	var protect, exempt []string
	for _, g := range globs {
		if rest, ok := strings.CutPrefix(g, "!"); ok {
			exempt = append(exempt, rest)
		} else {
			protect = append(protect, g)
		}
	}
	type rule struct{ match, exempt globSet }
	rules := make([]rule, 0, len(protect))
	for _, g := range protect {
		kept := exempt
		if dir, ok := strings.CutSuffix(g, "**"); ok && strings.HasSuffix(dir, "/") {
			kept = slices.DeleteFunc(slices.Clone(exempt), func(e string) bool { return !strings.HasPrefix(e, dir) })
		}
		m, err := newGlobSet([]string{g})
		if err != nil {
			return nil
		}
		ex, err := newGlobSet(kept)
		if err != nil {
			return nil
		}
		rules = append(rules, rule{m, ex})
	}
	var bad []string
	for _, f := range files {
		if slices.ContainsFunc(rules, func(r rule) bool { return r.match.match(f) && !r.exempt.match(f) }) {
			bad = append(bad, f)
		}
	}
	return bad
}

var agentPermissionOptions = opts(AgentRead, "Read only (look, change nothing)", AgentEdit, "Edit files", AgentEditRun, "Edit files and run commands")

func init() {
	registerAction(ActionDef{Name: "agent_task", Label: "Have a coding agent do a task", Group: "Agents", Phrase: "have {agents} {permission}", Long: true, Runner: true,
		Doc: "Run a coding agent (Claude Code, Codex, opencode or Cursor) in a fresh checkout on a runner of the person who last saved the automation. `agents` is a chain of CLI and model tried in order when one fails for a model reason; `prompt` is what it is asked; `permission` read, edit or edit_run; `branch` makes a branch (and pushes it unless `push` is off). Outputs Answer, Branch, Commits, Commit, Files, Tokens, Model, Limited",
		Params: []ParamDef{
			{Name: "agents", Label: "CLI and model", Type: PAgents, Required: true, Help: "Tried in order: add a fallback for when a model is rate limited or down."},
			{Name: "permission", Label: "May", Type: PSelect, Default: AgentEdit, Options: agentPermissionOptions,
				Help: "Read only reviews; edit changes files; edit and run commands lets it run the tests."},
			{Name: "prompt", Label: "Task", Type: PTemplate, Required: true, Help: "What the agent is asked. Wrap outside text in {{untrusted …}}."},
			{Name: "repo", Label: "Repository", Type: PRepo, Advanced: true, Placeholder: "owner/name", Help: "Leave empty for the project's repository."},
			{Name: "base", Label: "Start from", Type: PTemplate, Placeholder: "main", Help: "A branch, tag or commit; empty is the repository's default branch."},
			{Name: "branch", Label: "Make the branch", Type: PTemplate, Placeholder: "review/{{.Date}}", Help: "Empty makes no branch: the agent works and answers, nothing is kept."},
			{Name: "push", Label: "Push the branch", Type: PBool, Default: true},
			{Name: "commit_message", Label: "Commit message", Type: PTemplate, Advanced: true, Help: "For changes the agent leaves uncommitted."},
			{Name: "label", Label: "Runner label", Type: PText, Advanced: true, Help: "Only runners with this label take it."},
			{Name: "timeout", Label: "Give up after", Type: PDuration, Default: "30m", Min: f64(60), Max: f64(runnerMaxTimeout), Advanced: true},
		},
		Outputs: []OutputDef{{Name: "Answer", Type: "text", Doc: "the agent's final message"}, {Name: "Branch", Type: "text", Doc: "the branch, when the agent committed anything"},
			{Name: "Commits", Type: "list", Doc: "commit ids it made"}, {Name: "Commit", Type: "text", Doc: "the last of them"}, {Name: "Files", Type: "list", Doc: "files it changed"},
			{Name: "Tokens", Type: "object", Doc: "Input, Output, CacheRead, Cost when the CLI says"}, {Name: "Model", Type: "text", Doc: "which CLI and model did it"},
			{Name: "Limited", Type: "bool", Doc: "a usage or rate limit stopped it (a plan's pool is spent)"},
			{Name: "JobURL", Type: "text", Doc: "the runner job, with its log"}},
		Check: func(r *Rule, a *Action) error {
			if err := checkAgents(r, a); err != nil {
				return err
			}
			if branch := anyString(a.Extra["branch"]); branch == "" && anyString(a.Extra["permission"]) != AgentRead && a.Extra["push"] == true {
				return invalid("agent_task: pushing needs a branch name")
			}
			return nil
		},
		Run: runAgentTask,
		Describe: func(c *StepCtx) string {
			chain, _ := agentChain(c.Param("agents"))
			p, _ := c.Prompt("prompt")
			return fmt.Sprintf("Would run %s on a runner of the person who last saved this: %s", chainText(chain), excerpt(p, 300))
		}})

	registerAction(ActionDef{Name: "run_command", Label: "Run a command on a runner", Group: "Agents", Phrase: "run {command}", Long: true, Runner: true,
		Doc: "Run a shell command on a runner of the person who last saved the automation, in `dir` (it must be within the runner's allowed directories). Outputs ExitCode, Output (the end of it), OK",
		Params: []ParamDef{
			{Name: "command", Label: "Command", Type: PTemplate, Required: true, Placeholder: "make deploy-remote"},
			{Name: "dir", Label: "In", Type: PText, Required: true, Placeholder: "/home/me/app", Help: "An absolute directory the runner allows (wp runner --allow)."},
			{Name: "timeout", Label: "Give up after", Type: PDuration, Default: "10m", Min: f64(10), Max: f64(runnerMaxTimeout), Advanced: true},
			{Name: "label", Label: "Runner label", Type: PText, Advanced: true, Help: "Only runners with this label take it."},
		},
		Outputs: []OutputDef{{Name: "ExitCode", Type: "number"}, {Name: "Output", Type: "text", Doc: "the end of the output"}, {Name: "OK", Type: "bool", Doc: "it exited 0"},
			{Name: "JobURL", Type: "text", Doc: "the runner job, with its full log"}},
		Run: runCommand,
		Describe: func(c *StepCtx) string {
			cmdText, _ := c.Render("command")
			return fmt.Sprintf("Would run %q in %s on a runner of the person who last saved this", excerpt(cmdText, 200), c.Str("dir"))
		}})
}

// chainText names a chain: "opencode muse-spark, then claude".
func chainText(chain []AgentModel) string {
	var parts []string
	for _, m := range chain {
		parts = append(parts, m.String())
	}
	return strings.Join(parts, ", then ")
}

func runAgentTask(c *StepCtx) error {
	chain, err := agentChain(c.Param("agents"))
	if err != nil {
		return err
	}
	if len(chain) == 0 {
		return invalid("agent_task needs a CLI and a model")
	}
	c.note("agents", chainText(chain))
	prompt, err := c.Prompt("prompt")
	if err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return invalid("the task came out empty")
	}
	repo, err := c.Repo("repo")
	if err != nil {
		return err
	}
	base, err := c.Render("base")
	if err != nil {
		return err
	}
	branch, err := c.Render("branch")
	if err != nil {
		return err
	}
	msg, err := c.Render("commit_message")
	if err != nil {
		return err
	}
	perm := cmp(c.Str("permission"), AgentEdit)
	timeout, err := c.Dur("timeout", 30*time.Minute)
	if err != nil {
		return err
	}
	spec := agentSpec{Chain: chain, Prompt: prompt, Repo: repo, Base: strings.TrimSpace(base), Permission: perm, Message: msg, Label: c.Str("label"), Timeout: timeout}
	if perm != AgentRead {
		spec.Branch = strings.TrimSpace(branch)
		spec.Push = spec.Branch != "" && c.BoolDefault("push", true)
	}
	_, err = c.RunAgent(spec)
	return err
}

// tailOutput is a job's output, for a step to carry on with.
func tailOutput(j *stepJob) string {
	if t, ok := j.Result["tail"].(string); ok && t != "" {
		return t
	}
	return j.Tail
}

func runCommand(c *StepCtx) error {
	command, err := c.Render("command")
	if err != nil {
		return err
	}
	if strings.TrimSpace(command) == "" {
		return invalid("the command came out empty")
	}
	dir, err := c.Render("dir")
	if err != nil {
		return err
	}
	timeout, err := c.Dur("timeout", 10*time.Minute)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(CommandJob{Command: command, Dir: strings.TrimSpace(dir)})
	job, err := c.RunnerJob(0, RunnerJobInput{Kind: JobCommand, Label: c.Str("label"), Input: raw, Timeout: int(timeout.Seconds())})
	if err != nil {
		return err
	}
	code := -1
	if job.Exit != nil {
		code = *job.Exit
	}
	c.Set("ExitCode", code)
	c.Set("Output", tailOutput(job))
	c.Set("OK", job.State == "done" && code == 0)
	if job.State != "done" {
		return fmt.Errorf("the command %s%s: %s", job.State, exitNote(code), cmp(job.Error, excerpt(tailOutput(job), 200)))
	}
	return nil
}

func exitNote(code int) string {
	if code < 0 {
		return ""
	}
	return fmt.Sprintf(" (exit %d)", code)
}

// anyFileStrings decodes reported file names as they are: unlike anyStrings it
// does not trim, since " foo bar" is a different path from "foo bar" and the
// protected-path check must see the name the runner saw.
func anyFileStrings(v any) []string {
	if s, ok := v.(string); ok {
		// Names are split on newlines only, as the runner sends them: commas and blanks are part of a name.
		var out []string
		for _, f := range strings.Split(s, "\n") {
			if f != "" {
				out = append(out, f)
			}
		}
		return out
	}
	l, ok := v.([]any)
	if !ok {
		return anyStrings(v)
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
