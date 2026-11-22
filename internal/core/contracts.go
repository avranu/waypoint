package core

import (
	"encoding/json"
	"errors"
)

// Shared names for WP-D16 to WP-D21, so the parts built in parallel agree.
// Each event's Data keys are listed beside it; automations (WP-D18) read them
// as {{.Trigger.Data.key}}.
const (
	// Agent sessions (WP-D16). Data: session (id), machine, repo, branch, agent.
	EvSessionStarted = "session.started"
	EvSessionEnded   = "session.ended"
	EvSessionWaiting = "session.waiting" // + reason: idle_prompt, permission_prompt, …
	EvSessionUpdated = "session.updated" // state, title or last update changed (live only)
	// The agent truly finished (WP-D22): a Stop with no background tasks,
	// crons or wakeups, nothing delivered and no open request. + at (ms).
	EvSessionFinished = "session.finished"
	// A queued message reached the agent (WP-D22, live and for waits). Data:
	// session, message (id), via: stop, socket, start.
	EvSessionMessage = "session.message"

	// A person lifted a pause (WP-146): what waited for their idle sessions
	// is posted to them. Data: user (id).
	EvAgentsResumed = "agents.resumed"

	// An account was deleted. Data: user (id). Seen by admins only; what
	// belongs to the account outside the database goes in dispatch (the
	// transcript archive's files, WP-1466).
	EvUserDeleted = "user.deleted"

	// Requests (WP-D16). Data: request (id), code (R-12), kind, state, session.
	EvRequestCreated = "request.created"
	EvRequestUpdated = "request.updated" // + decision: answered, approved, denied, done, dismissed, applied, failed, ran (a failed run sent it back)
	// A person picked an allow rule from a refused call (WP-232, WP-233).
	// Data: refusal (id), rule, repo, session.
	EvRefusalAllowed = "refusal.allowed"

	// Runners (WP-D17). Data: runner (id), name, owner (user id).
	EvRunnerStatus = "runner.status" // + online: true/false
	// Data: job (id), kind, state, source (request_cell, automation_step, bot_run), source_id.
	EvRunnerJobFinished = "runner.job_finished"
	EvRunnerJobUpdated  = "runner.job_updated" // state change or new log (live only)

	// Automation runs (WP-D18). Data: automation (id), run (id), status.
	EvAutomationRunFinished = "automation.run_finished"

	// Pull requests, CI and code (WP-D21). Data for pr.*: pr (id), repo, number, sha.
	EvPRCI         = "pr.ci"          // + state: passed, failed; failed: [check names]
	EvPRReviewGate = "pr.review_gate" // + state: pending, success, failure; open: n
	EvPRReview     = "pr.review"      // a review was submitted: + reviewer, verdict, bot
	// WP-D14 phase 5: a label was added (+ label), someone was asked to
	// review (+ reviewer, bot: the forge's bot account was asked).
	EvPRLabeled         = "pr.labeled"
	EvPRReviewRequested = "pr.review_requested"
	// Data: repo, branch, before, after, pusher, paths: [changed files], created: bool.
	EvRepoPushed  = "repo.pushed"
	EvRepoRelease = "repo.release" // + tag, name, url

	// Environments (WP-D19). Data: environment (id), name, kind, url.
	EvEnvDeployed     = "env.deployed" // + deployment (id), commit, branch, version, status
	EvEnvDeployFailed = "env.deploy_failed"
	EvEnvStatus       = "env.status" // + status: up, down, degraded; previous
	EvEnvCreated      = "env.created"
	EvEnvStopped      = "env.stopped"
	// An environment still runs an older commit than its branch's head
	// longer than its lag limit (WP-199). + commit, head, branch, behind_m, since.
	EvEnvLagging = "env.lagging"

	// Knowledge and projects, for automation triggers (WP-D18).
	EvGlossaryChanged = "glossary.changed" // + term (id), name, created: bool
	EvDesignChanged   = "design.changed"   // + note (id), state: proposed, approved, requested (written for a person's task, WP-ADR96)
	EvProjectCreated  = "project.created"
	EvRepoAttached    = "project.repo_attached" // + repo, forge

	// System (admin automations). Data: resource: disk, database, memory, transcripts; free, total.
	EvSystemResources = "system.resources_low"
)

// Runner jobs (WP-D17): work done on a person's machine by `wp runner`.
const (
	JobCommand = "command" // run a shell command in an allowed directory
	JobApply   = "apply"   // write files in an allowed worktree, optionally commit
	JobAgent   = "agent"   // a coding-agent CLI headless in a fresh checkout
	JobReview  = "review"  // a PR review on a subscription CLI (WP-D14 phase 3)
	JobSession = "session" // start a background Claude Code session in an allowed directory (WP-D22)
	JobMessage = "message" // post a prompt to a running session's inbox socket (WP-D22)
	JobAudit   = "audit"   // a read-only audit that reports findings (WP-348, WP-ADR20)
)

// Permission levels of an agent job.
const (
	AgentRead    = "read"
	AgentEdit    = "edit"
	AgentEditRun = "edit_run"
)

// RunnerJobInput asks for work on a runner. Exactly one of RunnerID or
// (OwnerID [+ Label]) picks where it may run; the job only runs on runners
// owned by the person who asked (or the automation's owner).
type RunnerJobInput struct {
	Kind     string          `json:"kind"`
	RunnerID int64           `json:"runner_id,omitempty"` // a specific runner
	OwnerID  int64           `json:"owner_id,omitempty"`  // any online runner of this user
	Label    string          `json:"label,omitempty"`     // … that has this label
	Input    json.RawMessage `json:"input"`               // CommandJob, ApplyJob, AgentJob
	Source   string          `json:"source,omitempty"`    // request_cell, automation_step, bot_run
	SourceID int64           `json:"source_id,omitempty"`
	Timeout  int             `json:"timeout,omitempty"` // seconds; default per kind
	// StepKey names the automation step (and try) that queued the job, so the
	// step finds it again if it runs twice (WP-D18).
	StepKey string `json:"-"`
	// ProjectID is the project the job works for: a job's own token reaches
	// it and its sub-projects only (WP-418). Set by Waypoint, never by a
	// caller of the API.
	ProjectID int64 `json:"-"`
}

// CommandJob is the input of a command job.
type CommandJob struct {
	Command  string            `json:"command"`
	Dir      string            `json:"dir"`
	Env      map[string]string `json:"env,omitempty"`
	KeepKeys bool              `json:"keep_api_keys,omitempty"` // keep ANTHROPIC_API_KEY etc.
}

// SessionJob is the input of a session job (WP-D22): start a background
// Claude Code session (`claude --bg`) in Dir with the prompt. The runner sets
// WAYPOINT_FOLLOWUP to the job's id so the session's hooks link it.
type SessionJob struct {
	Dir            string `json:"dir"`
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"` // default, acceptEdits, auto, plan
	Effort         string `json:"effort,omitempty"`
	// Task binds the new session to tasks (WP_TASK=KEY[,KEY…]): a task
	// session (WP-34). Name is its Claude Code session name (claude attach
	// takes it). Runners older than these ignore them.
	Task string `json:"task,omitempty"`
	Name string `json:"name,omitempty"`
	// Kickoff, for a session on one task (WP-1037): the prompt is made the
	// way autopilot makes its own. The runner runs wp next for the task as
	// the session will, and Kickoff is the template its answer fills
	// ({next}, {key}, {title}, {done}: AutopilotJob.Prompt's). Prompt is then
	// empty; only a runner with FeatureTaskKickoff gets such a job. Title is
	// the task's, for {title}.
	Kickoff string `json:"kickoff,omitempty"`
	Title   string `json:"title,omitempty"`
}

// MessageJob is the input of a message job (WP-D22): post Body to the
// inbox socket of the running Claude Code session CLIID on the runner's
// machine, which starts a turn with it if the session is idle.
type MessageJob struct {
	CLIID string `json:"cli_id"`
	Body  string `json:"body"`
	// IfIdle: post only if Claude Code says the session is idle; a busy one
	// is left alone (a keep-warm ping, WP-192). The job is done either way;
	// its result says whether it was posted.
	IfIdle bool `json:"if_idle,omitempty"`
	// Compact: no Body; type /compact into the session's tmux pane, if it is
	// idle with nothing typed there (WP-192). Slash commands don't run
	// through the inbox socket.
	Compact bool `json:"compact,omitempty"`
	// Type: type Body into the session's tmux pane where it runs in one,
	// with Compact's checks; the inbox otherwise (WP-534). What comes
	// through the inbox gets Claude Code's long note about peers.
	Type bool `json:"type,omitempty"`
}

// Session permission modes a session job may start with.
var sessionPermissionModes = []string{"", "default", "acceptEdits", "auto", "plan"}

// Effort levels a session job may start with.
var sessionEfforts = []string{"", "low", "medium", "high", "xhigh", "max"}

// FileChange is one file an apply job (or a forge commit) writes.
type FileChange struct {
	Path    string `json:"path"`
	BaseSHA string `json:"base_sha,omitempty"` // git blob of the file the change was made against; "" = new file
	Content string `json:"content,omitempty"`
	Delete  bool   `json:"delete,omitempty"`
}

// ApplyJob is the input of an apply job.
type ApplyJob struct {
	Dir    string       `json:"dir"`
	Files  []FileChange `json:"files"`
	Commit string       `json:"commit,omitempty"` // commit message; "" = leave uncommitted
	Push   bool         `json:"push,omitempty"`
}

// AgentJob is the input of an agent (or review) job.
type AgentJob struct {
	CLI   string `json:"cli"`             // claude, codex, opencode, cursor; gateway for an audit the gateway runs
	Model string `json:"model,omitempty"` // passed to the CLI
	// Effort is the reasoning effort the CLI runs at (low … max; "" = the
	// CLI's own). Only claude takes it; other CLIs ignore it.
	Effort   string `json:"effort,omitempty"`
	Prompt   string `json:"prompt"`
	Repo     string `json:"repo"`             // clone URL
	Base     string `json:"base,omitempty"`   // ref to start from; default branch if empty
	Branch   string `json:"branch,omitempty"` // create this branch
	Push     bool   `json:"push,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
	// Permission is what the agent may do in its checkout: AgentRead (look,
	// change nothing), AgentEdit (the default: change files) or AgentEditRun
	// (change files and run commands such as the tests). ReadOnly is the
	// older way to say AgentRead.
	Permission string `json:"permission,omitempty"`
	JSONSchema string `json:"json_schema,omitempty"`
	// ProtectPaths are globs of files the job's commits may not touch (CI
	// config, lockfiles, secrets): the runner refuses to push commits that do.
	ProtectPaths []string `json:"protect_paths,omitempty"`
	// Target is the branch the work is going into: a protected file the same
	// as it there came from merging that branch, and isn't the job's change.
	Target string `json:"target,omitempty"`
	// CommitMessage is for changes the CLI leaves uncommitted ("Agent changes" if empty).
	CommitMessage string `json:"commit_message,omitempty"`
	// Patch makes it a job that runs no agent (no CLI or prompt): the patch
	// of protected-file changes a round held back, applied to Branch (which
	// is Base) and pushed. Only a check of the change or a person's approval
	// sends one (WP-ADR138).
	Patch string `json:"patch,omitempty"`
	// WorkTimeout, in seconds, is how long the agent may work, not counting
	// the time its commands wait for a machine slot (wp slot, wp testsum);
	// the job's timeout stays the limit on the whole run, waits included.
	// 0: the job's timeout is the only limit (WP-1618).
	WorkTimeout int `json:"work_timeout,omitempty"`
	// KeepOnTimeout: an agent that runs out of time has the commits it had
	// made checked and pushed all the same; the job still fails (WP-1597).
	KeepOnTimeout bool `json:"keep_on_timeout,omitempty"`
	// Confine marks a prompt that carries text others wrote (a pull request):
	// the runner confines the CLI to its checkout, or refuses a CLI it can't
	// confine. Always set for review jobs.
	Confine bool `json:"confine,omitempty"`
	// DiffFrom, for an audit, is the commit (or ref) the change under audit
	// starts from: the runner puts the diff from it to Base in the prompt,
	// so the model reads the change and has the whole checkout around it.
	DiffFrom string `json:"diff_from,omitempty"`
	// DiffSince, for an audit of recent changes (unix seconds): the change
	// starts at Base's last first-parent commit before then (WP-548).
	DiffSince int64 `json:"diff_since,omitempty"`
	// DiffPaths and DiffExclude, for an audit: the paths (git pathspecs, no
	// magic) the diff is limited to and leaves out.
	DiffPaths   []string `json:"diff_paths,omitempty"`
	DiffExclude []string `json:"diff_exclude,omitempty"`
	// GatewayExclude, for an audit the LLM gateway runs (CLI
	// GatewayCLI): globs of the models it must not run on, which are the
	// family of the model that wrote the code (WP-617).
	GatewayExclude []string `json:"gateway_exclude,omitempty"`
	// GatewayWeakest, for such an audit: the weakest model the gateway may
	// run it on, its quality bar (WP-916); "": none.
	GatewayWeakest string `json:"gateway_weakest,omitempty"`
	// GatewayPriority, GatewayPaid and GatewayArm, for such a job: its
	// routing.priority and routing.paid, and the arm it is labelled with at
	// the gateway; "": the gateway's defaults for an audit (free models
	// first, a paid one as the fallback). A prep trial is surplus, never
	// paid (WP-250, WP-ADR104).
	GatewayPriority string `json:"gateway_priority,omitempty"`
	GatewayPaid     string `json:"gateway_paid,omitempty"`
	GatewayArm      string `json:"gateway_arm,omitempty"`
	// Helpers, for an audit: the Haiku checks the runner runs around its
	// model in the same checkout (WP-1320, review_helpers.go); nil: none.
	Helpers *AuditHelpers `json:"helpers,omitempty"`
}

// AuditHelpers are the Haiku checks around an audit's model: Panel, checks
// run before it, whose leads (pruned) are added to its prompt, with
// Context (the task, the person's words, the goals) in theirs; and Verify,
// a check of each of its findings against the code after it. Model is the
// helpers' model on CLI (claude/haiku).
type AuditHelpers struct {
	Panel   []PanelCheck `json:"panel,omitempty"`
	Context string       `json:"context,omitempty"`
	Verify  bool         `json:"verify,omitempty"`
	CLI     string       `json:"cli"`
	Model   string       `json:"model"`
	// Prior are done, unanswered panel runs of the round's reviewer audits, from an attempt
	// that failed, with the range each read (Row: its record). The runner
	// takes a run whose range is its own as it is and runs the other
	// checks. Set when a runner claims the job (runners.go's
	// withPriorPanel, WP-1481), never in the stored input or on the audit.
	Prior []AuditHelperRun `json:"prior,omitempty"`
}

// PanelCheck is one of the panel's checks: its name and what it asks.
type PanelCheck struct {
	Name string `json:"name"`
	Ask  string `json:"ask"`
}

// RunnerJob is a job's state as other parts see it.
type RunnerJob struct {
	ID       int64           `json:"id"`
	Kind     string          `json:"kind"`
	State    string          `json:"state"` // queued, running, done, failed, cancelled, expired
	RunnerID *int64          `json:"runner_id,omitempty"`
	ExitCode *int            `json:"exit_code,omitempty"`
	Output   string          `json:"output,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"` // kind-specific: {"commit":…,"branch":…,"answer":…}
	Error    string          `json:"error,omitempty"`
	Source   string          `json:"source,omitempty"`
	SourceID int64           `json:"source_id,omitempty"`
	Input    json.RawMessage `json:"-"` // what it was queued with, for the code that queued it
}

// ErrNoRunner: no runner of the right owner (and label) is registered.
var ErrNoRunner = errors.New("no runner")
