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
	"unicode"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	runnerLogCap     = 1 << 20
	runnerLogEmit    = time.Second
	runnerQueuedMax  = 200
	runnerMaxTimeout = 2 * 3600
	// runnerAuditMaxTimeout: an audit's job may run longer, for the gateway's
	// agent (gatewayAuditTimeout); its models read a repository slowly. The
	// runner's Haiku helpers around the model add to it (WP-1320).
	runnerAuditMaxTimeout = gatewayAuditTimeout + helpersMaxExtra
	runnerInputMax        = 4 << 20
)

var runnerJobSources = []string{"request_apply", "request_cell", "request_decision", "automation_step", "bot_run", "babysit", "session_message", "session_batch", "manual", "kit_rollout", "autopilot", autopilotReviewSource, "audit", cacheWarmSource, reviewCompactSource, prepAgentSource, babysitHeldSource}

// default timeouts, seconds
var runnerTimeouts = map[string]int{JobCommand: 600, JobApply: 300, JobAgent: 1800, JobReview: 900, JobSession: 120, JobMessage: 60, JobAutopilot: 900, JobAutopilotStart: 900, JobAudit: 2700, JobObjective: objectiveJobTimeout}

var (
	envKeyRE = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)
	gitRefRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._/@{}^~+-]{0,199}$`)
	branchRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	// A model name, with the settings some CLIs take in brackets
	// (opus[1m]); it goes in as an argument of its own, never through a
	// shell.
	modelRE    = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,99}(\[[A-Za-z0-9._:=,-]{1,100}\])?$`)
	blobRE     = lazyre.New(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	scpCloneRE = lazyre.New(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s\x00-\x1f]+$`)
	// cliSessionRE: a CLI's session id (Claude Code's are UUIDs).
	cliSessionRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)
)

// validBranch: a name `git check-ref-format --branch` accepts and git can't
// take for an option (the first character is alphanumeric).
func validBranch(b string) bool {
	if !branchRE.MatchString(b) || strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "/.") || strings.HasSuffix(b, ".") {
		return false
	}
	for _, part := range strings.Split(b, "/") {
		if part == "" || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// validCloneURL: https, http, ssh or scp-style; never a local path or a git
// transport that runs commands (ext::, file:).
func validCloneURL(u string) bool {
	if u == "" || len(u) > 500 || strings.HasPrefix(u, "-") || strings.ContainsAny(u, " \t\r\n\x00") {
		return false
	}
	for _, p := range []string{"https://", "http://", "ssh://"} {
		if strings.HasPrefix(u, p) && len(u) > len(p) {
			return true
		}
	}
	return scpCloneRE.MatchString(u)
}

// relPath: a file path inside a repository, written relative to it.
func relPath(p string) bool {
	if p == "" || len(p) > 1000 || strings.ContainsAny(p, "\x00\\") || strings.HasPrefix(p, "/") || winDrive.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" || strings.EqualFold(seg, ".git") {
			return false
		}
	}
	return true
}

// checkRunnerInput validates a job's input and returns what a runner needs
// to take it, a one-line title and the input as it is stored.
func checkRunnerInput(kind string, raw json.RawMessage) (jobNeeds, string, json.RawMessage, error) {
	n := jobNeeds{kind: kind}
	var v any
	var title string
	if len(raw) == 0 {
		return n, "", nil, invalid("the job has no input")
	}
	switch kind {
	case JobCommand:
		var in CommandJob
		if err := json.Unmarshal(raw, &in); err != nil {
			return n, "", nil, invalid("command input: %v", err)
		}
		if strings.TrimSpace(in.Command) == "" || len(in.Command) > 32<<10 {
			return n, "", nil, invalid("a command job needs a command (up to 32 KB)")
		}
		dir, ok := cleanDir(in.Dir)
		if !ok {
			return n, "", nil, invalid("a command job needs an absolute directory without ..")
		}
		if len(in.Env) > 50 {
			return n, "", nil, invalid("at most 50 environment variables")
		}
		for k, val := range in.Env {
			if !envKeyRE.MatchString(k) || strings.HasPrefix(strings.ToUpper(k), "WAYPOINT_") || len(val) > 4096 {
				return n, "", nil, invalid("environment variable %q isn't allowed", k)
			}
		}
		in.Dir, n.dir, v = dir, dir, in
		title = excerpt(strings.SplitN(strings.TrimSpace(in.Command), "\n", 2)[0], 100)
	case JobApply:
		var in ApplyJob
		if err := json.Unmarshal(raw, &in); err != nil {
			return n, "", nil, invalid("apply input: %v", err)
		}
		dir, ok := cleanDir(in.Dir)
		if !ok {
			return n, "", nil, invalid("an apply job needs an absolute directory without ..")
		}
		if len(in.Files) == 0 || len(in.Files) > 500 {
			return n, "", nil, invalid("an apply job changes between 1 and 500 files")
		}
		if len(in.Commit) > 4096 {
			return n, "", nil, invalid("the commit message can be at most 4 KB")
		}
		seen := map[string]bool{}
		total := 0
		for _, f := range in.Files {
			if !relPath(f.Path) || seen[f.Path] {
				return n, "", nil, invalid("file path %q isn't a unique path inside the repository", f.Path)
			}
			seen[f.Path] = true
			if f.BaseSHA != "" && !blobRE.MatchString(f.BaseSHA) {
				return n, "", nil, invalid("base_sha of %s isn't a git blob id", f.Path)
			}
			if f.Delete && f.Content != "" {
				return n, "", nil, invalid("%s is deleted and written at once", f.Path)
			}
			total += len(f.Content)
		}
		if total > runnerInputMax {
			return n, "", nil, invalid("an apply job can carry at most 4 MB of content")
		}
		in.Dir, n.dir, v = dir, dir, in
		title = fmt.Sprintf("Apply %d file%s in %s", len(in.Files), plural(len(in.Files)), dir[strings.LastIndex(dir, "/")+1:])
	case JobAgent, JobReview, JobAudit:
		var in AgentJob
		if err := json.Unmarshal(raw, &in); err != nil {
			return n, "", nil, invalid("agent input: %v", err)
		}
		gateway := kind == JobAudit && in.CLI == GatewayCLI
		patch := in.Patch != ""
		if patch && (kind != JobAgent || len(in.Patch) > 512<<10 || !in.Push || in.Branch == "" || in.Base != in.Branch) {
			return n, "", nil, invalid("a patch job pushes a patch of up to 512 KB to the branch it starts from")
		}
		if patch {
			in.CLI, in.Prompt, n.patch = "claude", "Apply a held patch.", true
		}
		if !gateway && !slices.Contains(runnerCLIs, in.CLI) {
			return n, "", nil, invalid("cli must be one of %s", strings.Join(runnerCLIs, ", "))
		}
		if gateway && in.Model == "" {
			return n, "", nil, invalid("an audit through the gateway names the gateway's model: gateway/audit")
		}
		if strings.TrimSpace(in.Prompt) == "" || len(in.Prompt) > 200<<10 {
			return n, "", nil, invalid("an agent job needs a prompt (up to 200 KB)")
		}
		if !validCloneURL(in.Repo) {
			return n, "", nil, invalid("repo must be an https, http or ssh clone URL")
		}
		if in.Base != "" && !gitRefRE.MatchString(in.Base) {
			return n, "", nil, invalid("base %q isn't a branch, tag or commit", in.Base)
		}
		if in.Branch != "" && !validBranch(in.Branch) {
			return n, "", nil, invalid("branch %q isn't a valid branch name", in.Branch)
		}
		if in.Model != "" && !modelRE.MatchString(in.Model) {
			return n, "", nil, invalid("model %q isn't allowed", in.Model)
		}
		if !slices.Contains(sessionEfforts, in.Effort) {
			return n, "", nil, invalid("effort must be one of %s", strings.Join(sessionEfforts[1:], ", "))
		}
		if in.JSONSchema != "" && (len(in.JSONSchema) > 64<<10 || !json.Valid([]byte(in.JSONSchema))) {
			return n, "", nil, invalid("json_schema must be JSON, up to 64 KB")
		}
		if kind == JobReview {
			in.ReadOnly, in.Push, in.Branch, in.Permission = true, false, "", AgentRead
			in.Confine = true
		}
		if kind == JobAudit {
			in.ReadOnly, in.Push, in.Branch, in.Permission = true, false, "", AgentRead
			in.Confine = slices.Contains(reviewCLIs, in.CLI)
			if gateway {
				var err error
				if in.GatewayExclude, err = cleanGatewayExclude(in.GatewayExclude); err != nil {
					return n, "", nil, err
				}
				if in.GatewayWeakest != "" && !gatewayWeakestRE.MatchString(in.GatewayWeakest) {
					return n, "", nil, invalid("%q isn't a model name", in.GatewayWeakest)
				}
				if !slices.Contains([]string{"", "normal", prioritySurplus}, in.GatewayPriority) {
					return n, "", nil, invalid("gateway priority must be normal or surplus")
				}
				if !slices.Contains([]string{"", "fallback", "never"}, in.GatewayPaid) {
					return n, "", nil, invalid("gateway paid must be fallback or never")
				}
				if in.GatewayArm != "" && !gatewayArmRE.MatchString(in.GatewayArm) {
					return n, "", nil, invalid("%q isn't an arm label", in.GatewayArm)
				}
			} else {
				in.GatewayExclude, in.GatewayWeakest, in.GatewayPriority, in.GatewayPaid, in.GatewayArm = nil, "", "", "", ""
			}
			if in.DiffFrom != "" && !gitRefRE.MatchString(in.DiffFrom) {
				return n, "", nil, invalid("diff_from %q isn't a branch, tag or commit", in.DiffFrom)
			}
			if in.DiffSince < 0 || (in.DiffSince > 0 && in.DiffFrom != "") {
				return n, "", nil, invalid("diff_since is a time (unix seconds), instead of diff_from")
			}
			var err error
			if in.DiffPaths, err = cleanAuditPaths(in.DiffPaths); err != nil {
				return n, "", nil, err
			}
			if in.DiffExclude, err = cleanAuditPaths(in.DiffExclude); err != nil {
				return n, "", nil, err
			}
		} else {
			in.DiffFrom, in.DiffSince, in.DiffPaths, in.DiffExclude, in.GatewayExclude, in.GatewayWeakest = "", 0, nil, nil, nil, ""
			in.GatewayPriority, in.GatewayPaid, in.GatewayArm = "", "", ""
		}
		switch in.Permission {
		case "", AgentRead, AgentEdit, AgentEditRun:
		default:
			return n, "", nil, invalid("permission must be read, edit or edit_run")
		}
		if in.ReadOnly && in.Permission != "" && in.Permission != AgentRead {
			return n, "", nil, invalid("a read-only job can't have %s permission", in.Permission)
		}
		if in.ReadOnly || in.Permission == AgentRead {
			in.ReadOnly, in.Permission = true, AgentRead
		} else if in.Permission == "" {
			in.Permission = AgentEdit
		}
		if in.ReadOnly && (in.Push || in.Branch != "") {
			return n, "", nil, invalid("a read-only job can't push or make a branch")
		}
		if in.Push && in.Branch == "" {
			return n, "", nil, invalid("pushing needs a branch to push")
		}
		if len(in.ProtectPaths) > 100 {
			return n, "", nil, invalid("at most 100 protected paths")
		}
		for _, g := range in.ProtectPaths {
			if len(g) > 200 || strings.ContainsAny(g, "\x00\r\n") {
				return n, "", nil, invalid("protected path %q isn't a pattern", g)
			}
		}
		if len(in.CommitMessage) > 4096 {
			return n, "", nil, invalid("the commit message can be at most 4 KB")
		}
		n.cli, v = in.CLI, in
		if patch {
			n.cli = ""
		}
		title = in.CLI + ": " + excerpt(in.Prompt, 90)
		if patch {
			title = "Apply held protected-file changes to " + in.Branch
		}
		switch kind {
		case JobReview:
			title = "Review with " + title
		case JobAudit:
			title = "Audit with " + title
		}
	case JobSession:
		var in SessionJob
		if err := json.Unmarshal(raw, &in); err != nil {
			return n, "", nil, invalid("session input: %v", err)
		}
		dir, ok := cleanDir(in.Dir)
		if !ok {
			return n, "", nil, invalid("a session job needs an absolute directory without ..")
		}
		if in.Kickoff != "" {
			if in.Prompt != "" || len(splitTaskKeys(in.Task)) != 1 || len(in.Kickoff) > maxAutopilotText || len(in.Title) > 400 {
				return n, "", nil, invalid("a kickoff session job needs one task, a template of up to %d characters and no prompt", maxAutopilotText)
			}
			n.feature = FeatureTaskKickoff
		} else if strings.TrimSpace(in.Prompt) == "" || len(in.Prompt) > 200<<10 {
			return n, "", nil, invalid("a session job needs a prompt (up to 200 KB)")
		}
		if in.Model != "" && !modelRE.MatchString(in.Model) {
			return n, "", nil, invalid("model %q isn't allowed", in.Model)
		}
		if !slices.Contains(sessionPermissionModes, in.PermissionMode) {
			return n, "", nil, invalid("permission mode must be one of %s", strings.Join(sessionPermissionModes[1:], ", "))
		}
		if !slices.Contains(sessionEfforts, in.Effort) {
			return n, "", nil, invalid("effort must be one of %s", strings.Join(sessionEfforts[1:], ", "))
		}
		if in.Task != "" {
			keys := splitTaskKeys(in.Task)
			if len(keys) == 0 {
				return n, "", nil, invalid("task %q isn't an issue key", in.Task)
			}
			in.Task = strings.Join(keys, ",")
		}
		if len(in.Name) > 200 || strings.ContainsFunc(in.Name, unicode.IsControl) {
			return n, "", nil, invalid("the session name can be at most 200 characters on one line")
		}
		in.Dir, n.dir, n.cli, v = dir, dir, "claude", in
		title = "New session: " + excerpt(in.Prompt, 90)
		if in.Task != "" {
			title = "Task session: " + cmp(strings.TrimSpace(in.Name), in.Task)
		}
	case JobMessage:
		var in MessageJob
		if err := json.Unmarshal(raw, &in); err != nil {
			return n, "", nil, invalid("message input: %v", err)
		}
		if !cliSessionRE.MatchString(in.CLIID) {
			return n, "", nil, invalid("a message job needs the session's id")
		}
		if in.Compact {
			if in.Body != "" {
				return n, "", nil, invalid("a compact job takes no text")
			}
			in.IfIdle = true
		} else if strings.TrimSpace(in.Body) == "" || len(in.Body) > 200<<10 {
			return n, "", nil, invalid("a message job needs text (up to 200 KB)")
		}
		v = in
		title = "Tell session " + excerpt(in.CLIID, 8) + ": " + excerpt(in.Body, 80)
		if in.Compact {
			title = "Compact session " + excerpt(in.CLIID, 8)
		}
	case JobAutopilot:
		var in AutopilotJob
		if err := json.Unmarshal(raw, &in); err != nil {
			return n, "", nil, invalid("autopilot input: %v", err)
		}
		if !cliSessionRE.MatchString(in.CLIID) {
			return n, "", nil, invalid("an autopilot job needs the session's id")
		}
		dir, ok := cleanDir(in.Dir)
		if !ok {
			return n, "", nil, invalid("an autopilot job needs the session's directory")
		}
		review := in.ReviewOf != ""
		switch {
		case review && (in.Key != "" || !taskKeyRE.MatchString(in.ReviewOf)):
			return n, "", nil, invalid("an autopilot review job needs the finished task's key, and no task to start")
		case review && (strings.TrimSpace(in.Review) == "" || utf8.RuneCountInString(in.Review) > maxReviewJobPrompt ||
			strings.ContainsFunc(in.Review, func(r rune) bool { return r != '\n' && unicode.IsControl(r) })):
			return n, "", nil, invalid("an autopilot review job needs its prompt (up to %d characters)", maxReviewJobPrompt)
		case !review && (!taskKeyRE.MatchString(in.Key) || in.Review != ""):
			return n, "", nil, invalid("an autopilot job needs the task's key")
		case in.Done != "" && !taskKeyRE.MatchString(in.Done):
			return n, "", nil, invalid("an autopilot job needs the task's key")
		}
		if len(in.Agent) > 200 || len(in.Title) > 2000 || strings.ContainsFunc(in.Agent+in.Title, unicode.IsControl) {
			return n, "", nil, invalid("the agent or title is too long")
		}
		in.Dir, n.dir, n.cli, v = dir, dir, "claude", in
		title = "Autopilot: " + in.Key + " in session " + excerpt(in.CLIID, 8)
		if review {
			title = "Autopilot: review " + in.ReviewOf + " in session " + excerpt(in.CLIID, 8)
		}
	case JobAutopilotStart:
		var in AutopilotJob
		if err := json.Unmarshal(raw, &in); err != nil {
			return n, "", nil, invalid("autopilot input: %v", err)
		}
		dir, ok := cleanDir(in.Dir)
		switch {
		case !ok:
			return n, "", nil, invalid("an autopilot start needs the checkout's directory")
		case !taskKeyRE.MatchString(in.Key):
			return n, "", nil, invalid("an autopilot start needs the task's key")
		case in.CLIID != "" || in.Done != "" || in.ReviewOf != "" || in.Review != "":
			return n, "", nil, invalid("an autopilot start has no session to move on")
		case len(in.Agent) > 200 || len(in.Title) > 2000 || strings.ContainsFunc(in.Agent+in.Title, unicode.IsControl):
			return n, "", nil, invalid("the agent or title is too long")
		}
		in.Dir, n.dir, n.cli, v = dir, dir, "claude", in
		title = "Autopilot: start a session for " + in.Key
	default:
		return n, "", nil, invalid("kind must be command, apply, agent, review, audit, session, message, autopilot or autopilot_start")
	}
	b, _ := json.Marshal(v)
	return n, title, b, nil
}

// RunnerJobInfo is a job as the web and the API show it.
type RunnerJobInfo struct {
	ID              int64      `json:"id"`
	Kind            string     `json:"kind"`
	Title           string     `json:"title"`
	State           string     `json:"state"`
	OwnerID         int64      `json:"owner_id"`
	CreatedBy       *UserRef   `json:"created_by"`
	RunnerID        *int64     `json:"runner_id"`
	Runner          string     `json:"runner,omitempty"`
	TargetRunnerID  *int64     `json:"target_runner_id,omitempty"`
	Label           string     `json:"label,omitempty"`
	Timeout         int        `json:"timeout"`
	Attempts        int        `json:"attempts"`
	CancelRequested bool       `json:"cancel_requested,omitempty"`
	ExitCode        *int       `json:"exit_code"`
	Error           string     `json:"error,omitempty"`
	LogSize         int64      `json:"log_size"`
	Source          string     `json:"source,omitempty"`
	SourceID        int64      `json:"source_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	// Detail only (and only for the job's owner and creator):
	Input   json.RawMessage `json:"input,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Log     *string         `json:"log,omitempty"`      // from byte LogFrom
	LogFrom int64           `json:"log_from,omitempty"` //
	LogEnd  int64           `json:"log_end,omitempty"`  // the byte after Log: where the next read starts
	Waiting string          `json:"waiting,omitempty"`  // queued: what it's waiting for
	// Gateway: a running audit through the gateway, how its task goes there
	// (status and tokens so far; WP-1064). Only with the job's full view
	// and its log.
	Gateway *GatewayTaskView `json:"gateway,omitempty"`
	// Redacted: an admin sees that the job exists, not what it does.
	Redacted bool `json:"redacted,omitempty"`
	needs    jobNeeds
}

const jobCols = `j.id, j.kind, j.title, j.state, j.owner_id, j.created_by, cu.username, cu.display_name, j.runner_id, j.runner_name, j.target_runner_id, j.label, j.timeout, j.attempts,
	j.cancel_requested, j.exit_code, j.error, j.log_size, j.source, j.source_id, j.created_at, j.started_at, j.finished_at, j.dir, j.cli, ` + jobFeatureColJ

type jobScanner interface{ Scan(...any) error }

func scanJob(row jobScanner, extra ...any) (*RunnerJobInfo, error) {
	var j RunnerJobInfo
	var creator, runner, target, exit, started, finished sql.NullInt64
	var cuName, cuDisp sql.NullString
	var created int64
	var cancel bool
	dest := []any{&j.ID, &j.Kind, &j.Title, &j.State, &j.OwnerID, &creator, &cuName, &cuDisp, &runner, &j.Runner, &target, &j.Label, &j.Timeout, &j.Attempts,
		&cancel, &exit, &j.Error, &j.LogSize, &j.Source, &j.SourceID, &created, &started, &finished, &j.needs.dir, &j.needs.cli, &j.needs.feature}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	j.needs.kind = j.Kind
	j.needs.label = j.Label
	j.needs.source = j.Source
	j.CancelRequested = cancel
	if creator.Valid {
		j.CreatedBy = &UserRef{ID: creator.Int64, Username: cuName.String, DisplayName: cuDisp.String}
		if j.CreatedBy.DisplayName == "" {
			j.CreatedBy.DisplayName = cuName.String
		}
	}
	if runner.Valid {
		j.RunnerID = &runner.Int64
	}
	if target.Valid {
		j.TargetRunnerID = &target.Int64
	}
	if exit.Valid {
		c := int(exit.Int64)
		j.ExitCode = &c
	}
	j.CreatedAt = msTime(created)
	if started.Valid {
		t := msTime(started.Int64)
		j.StartedAt = &t
	}
	if finished.Valid {
		t := msTime(finished.Int64)
		j.FinishedAt = &t
	}
	return &j, nil
}

// EnqueueRunnerJob queues work for a runner and returns it; the job's end
// emits EvRunnerJobFinished. A job runs only on a runner of the person
// asking (or, for an automation, the person it acts for), so one person's
// machine and subscriptions never serve another's work; an agent can't
// queue one at all, because what runs on a machine is a person's decision.
// ErrNoRunner means that person has no runner (with the label).
func (s *Service) EnqueueRunnerJob(ctx context.Context, a *Actor, in RunnerJobInput) (*RunnerJob, error) {
	if err := a.need(PermRunnersRun); err != nil {
		return nil, err
	}
	if err := a.canWrite(); err != nil {
		return nil, err
	}
	if a.IsAgent() && a.Automation == nil {
		return nil, fmt.Errorf("agents can't run things on a person's machine: %w", ErrForbidden)
	}
	maxTimeout := runnerMaxTimeout
	if in.Kind == JobAudit {
		maxTimeout = runnerAuditMaxTimeout
	}
	if in.Timeout < 0 || in.Timeout > maxTimeout {
		return nil, invalid("timeout is at most %d seconds", maxTimeout)
	}
	if in.Source != "" && !slices.Contains(runnerJobSources, in.Source) {
		return nil, invalid("source must be one of %s", strings.Join(runnerJobSources, ", "))
	}
	if len(in.Label) > 40 || (in.Label != "" && !labelRE.MatchString(in.Label)) {
		return nil, invalid("label %q isn't valid", in.Label)
	}
	owner := in.OwnerID
	var candidates []*Runner
	if in.RunnerID != 0 {
		r, err := s.runnerByID(ctx, s.db.R, in.RunnerID)
		if err != nil {
			return nil, err
		}
		if owner != 0 && owner != r.Owner.ID {
			return nil, invalid("that runner belongs to someone else")
		}
		owner, candidates = r.Owner.ID, []*Runner{r}
	}
	if owner == 0 {
		owner = a.id()
	}
	if owner == 0 {
		return nil, invalid("say whose runner the job is for")
	}
	switch {
	case a.Automation != nil:
		if automationRunnerOwner(a.Automation) != owner {
			return nil, fmt.Errorf("an automation runs jobs only on the runners of the person who last saved it: %w", ErrForbidden)
		}
	case a.User.ID != 0 && owner != a.User.ID:
		return nil, fmt.Errorf("a job runs only on your own runners: %w", ErrForbidden)
	}
	if candidates == nil {
		all, err := s.ownerRunners(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, r := range all {
			if in.Label == "" || slices.Contains(r.Labels, in.Label) {
				candidates = append(candidates, r)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoRunner
	}
	needs, title, input, err := checkRunnerInput(in.Kind, in.Input)
	if err != nil {
		return nil, err
	}
	if needs.patch && (a.User == nil || a.User.ID != 0 || a.IsAgent() || a.Automation != nil || in.Source != babysitHeldSource) {
		return nil, fmt.Errorf("only Waypoint queues a job that pushes a held patch: %w", ErrForbidden)
	}
	needs.label = in.Label
	if !slices.ContainsFunc(candidates, func(r *Runner) bool { return runnerCanTake(r, needs) }) {
		switch {
		case needs.dir != "" && !slices.ContainsFunc(candidates, func(r *Runner) bool { return dirAllowed(needs.dir, r.AllowedDirs) }):
			return nil, invalid("no runner allows %s (start the runner with --allow)", needs.dir)
		case (needs.kind == JobReview || needs.kind == JobAudit) && slices.ContainsFunc(candidates, func(r *Runner) bool {
			n := needs
			n.kind = ""
			return runnerCanTake(r, n)
		}):
			if needs.kind == JobAudit {
				return nil, invalid("%s", oldAuditRunnerWhy(candidates, needs.cli))
			}
			n := needs
			n.kind = ""
			return nil, invalid("%s", oldRunnerWhy(candidates, n))
		case needs.cli != "":
			return nil, invalid("no runner has %s installed and signed in", needs.cli)
		}
		return nil, ErrNoRunner
	}
	timeout := in.Timeout
	if timeout == 0 {
		timeout = runnerTimeouts[in.Kind]
	}
	var target any
	if in.RunnerID != 0 {
		target = in.RunnerID
	}
	var creator any
	if a.User.ID != 0 {
		creator = a.User.ID
	}
	var id int64
	err = s.write(ctx, a, func(t *tx) error {
		var queued int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM runner_jobs WHERE owner_id = ? AND state = 'queued'`, owner).Scan(&queued); err != nil {
			return err
		}
		if queued >= runnerQueuedMax {
			return invalid("%d jobs are already waiting for a runner", queued)
		}
		var project any
		if in.ProjectID != 0 {
			project = in.ProjectID
		}
		res, err := t.ExecContext(ctx, `INSERT INTO runner_jobs(owner_id, created_by, target_runner_id, label, kind, title, dir, cli, input, timeout, source, source_id, step_key, project_id, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, owner, creator, target, in.Label, in.Kind, title, needs.dir, needs.cli, string(input), timeout, in.Source, in.SourceID, in.StepKey, project, s.nowMS())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		if _, err := t.ExecContext(ctx, `INSERT INTO runner_job_logs(job_id) VALUES (?)`, id); err != nil {
			return err
		}
		t.emit(jobUpdated(&RunnerClaim{ID: id, Kind: in.Kind}, "queued", in.RunnerID, owner, 0))
		if in.Source == "manual" {
			t.audit(AuditEntry{Action: "runner.run", Category: AuditControl, TargetType: "runner_job", TargetID: id, Target: title, Detail: "ran a " + in.Kind + " job on their runner: " + title})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.kickRunners()
	return &RunnerJob{ID: id, Kind: in.Kind, State: "queued", Source: in.Source, SourceID: in.SourceID}, nil
}

func (s *Service) ownerRunners(ctx context.Context, owner int64) ([]*Runner, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+runnerCols+` FROM runners r JOIN users u ON u.id = r.owner_id WHERE r.owner_id = ? ORDER BY r.id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Runner
	for rows.Next() {
		r, err := s.scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// jobAccess: the owner and the person who queued a job see all of it; an
// admin sees that it exists.
func jobAccess(a *Actor, owner int64, creator *UserRef) (full, ok bool) {
	if a == nil || a.User == nil {
		return false, false
	}
	if a.User.ID == owner || (creator != nil && creator.ID == a.User.ID) {
		return true, true
	}
	return false, a.isAdmin()
}

func (j *RunnerJobInfo) redact() {
	j.Redacted = true
	j.Title, j.Error, j.Label, j.Runner = "", "", "", ""
	j.Input, j.Result, j.Log = nil, nil, nil
}

// GetRunnerJob returns a job. logFrom >= 0 includes the output from that
// byte on (the page asks for the rest of what it has).
func (s *Service) GetRunnerJob(ctx context.Context, actor *Actor, id, logFrom int64) (*RunnerJobInfo, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var input string
	var result sql.NullString
	j, err := scanJob(s.db.R.QueryRowContext(ctx, `SELECT `+jobCols+`, j.input, j.result FROM runner_jobs j LEFT JOIN users cu ON cu.id = j.created_by WHERE j.id = ?`, id), &input, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("job")
	} else if err != nil {
		return nil, err
	}
	full, ok := jobAccess(actor, j.OwnerID, j.CreatedBy)
	if !ok {
		return nil, notFound("job")
	}
	if !full {
		j.redact()
		return j, nil
	}
	j.Input = json.RawMessage(input)
	if result.Valid {
		j.Result = json.RawMessage(result.String)
	}
	if logFrom >= 0 && j.LogSize > logFrom {
		var text string
		if err := s.db.R.QueryRowContext(ctx, `SELECT CAST(substr(CAST(text AS BLOB), ?) AS TEXT) FROM runner_job_logs WHERE job_id = ?`, logFrom+1, id).Scan(&text); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		j.Log, j.LogFrom, j.LogEnd = &text, logFrom, logFrom+int64(len(text))
	} else if logFrom >= 0 {
		empty := ""
		j.Log, j.LogFrom, j.LogEnd = &empty, logFrom, logFrom
	}
	if j.State == "queued" {
		j.Waiting = s.queuedWhy(ctx, j)
	}
	if logFrom >= 0 && j.State == "running" && j.Kind == JobAudit && j.needs.cli == GatewayCLI {
		j.Gateway = s.gatewayProgress(ctx, id)
	}
	return j, nil
}

// RunnerJobByID is a job in the shape other parts of Waypoint read, with
// its output.
func (s *Service) RunnerJobByID(ctx context.Context, actor *Actor, id int64) (*RunnerJob, error) {
	j, err := s.GetRunnerJob(ctx, actor, id, 0)
	if err != nil {
		return nil, err
	}
	out := &RunnerJob{ID: j.ID, Kind: j.Kind, State: j.State, RunnerID: j.RunnerID, ExitCode: j.ExitCode, Result: j.Result, Error: j.Error, Source: j.Source, SourceID: j.SourceID, StartedAt: j.StartedAt, Input: j.Input}
	if j.Log != nil {
		out.Output = *j.Log
	}
	return out, nil
}

// RunnerJobFilter narrows ListRunnerJobs.
type RunnerJobFilter struct {
	RunnerID int64
	States   []string
	Source   string
	SourceID int64
	All      bool // admins: everyone's
	Limit    int
	Before   int64 // ids below this
}

// ListRunnerJobs lists jobs newest first: those that run on the caller's
// runners or that the caller queued.
func (s *Service) ListRunnerJobs(ctx context.Context, actor *Actor, f RunnerJobFilter) ([]*RunnerJobInfo, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var where []string
	var args []any
	if f.All {
		if !actor.isAdmin() {
			return nil, ErrForbidden
		}
	} else {
		where, args = append(where, `(j.owner_id = ? OR j.created_by = ?)`), append(args, actor.User.ID, actor.User.ID)
	}
	if f.RunnerID != 0 {
		where, args = append(where, `j.runner_id = ?`), append(args, f.RunnerID)
	}
	if len(f.States) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(f.States)), ",")
		where = append(where, `j.state IN (`+marks+`)`)
		for _, st := range f.States {
			args = append(args, st)
		}
	}
	if f.Source != "" {
		where, args = append(where, `j.source = ?`), append(args, f.Source)
	}
	if f.SourceID != 0 {
		where, args = append(where, `j.source_id = ?`), append(args, f.SourceID)
	}
	if f.Before > 0 {
		where, args = append(where, `j.id < ?`), append(args, f.Before)
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	q := `SELECT ` + jobCols + ` FROM runner_jobs j LEFT JOIN users cu ON cu.id = j.created_by`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	rows, err := s.db.R.QueryContext(ctx, q+` ORDER BY j.id DESC LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RunnerJobInfo{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		if full, _ := jobAccess(actor, j.OwnerID, j.CreatedBy); !full {
			j.redact()
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// runnerJobFor loads a job a runner reports on, which must be the caller's.
func (s *Service) runnerJobFor(ctx context.Context, q queryer, actor *Actor, id int64) (owner int64, state string, size int64, cancel bool, kind string, err error) {
	err = q.QueryRowContext(ctx, `SELECT owner_id, state, log_size, cancel_requested, kind FROM runner_jobs WHERE id = ?`, id).Scan(&owner, &state, &size, &cancel, &kind)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != actor.User.ID) {
		return 0, "", 0, false, "", notFound("job")
	}
	return
}

// LogAck answers a runner's log upload.
type LogAck struct {
	Size      int64 `json:"size"`      // bytes the server holds
	Cancel    bool  `json:"cancel"`    // a person asked to stop the job
	Truncated bool  `json:"truncated"` // the 1 MB cap was reached; more output is dropped
}

const logTruncated = "\n[output truncated at 1 MB]\n"

// AppendRunnerLog adds output a runner streams. offset is where text starts
// in the runner's output, so a chunk sent again after a lost answer isn't
// added twice. Output beyond 1 MB is dropped.
func (s *Service) AppendRunnerLog(ctx context.Context, actor *Actor, id, offset int64, text string) (*LogAck, error) {
	if err := runnerActor(actor); err != nil {
		return nil, err
	}
	var ack LogAck
	var kind string
	var emit bool
	err := s.write(ctx, actor, func(t *tx) error {
		_, state, size, cancel, k, err := s.runnerJobFor(ctx, t, actor, id)
		if err != nil {
			return err
		}
		kind, ack.Size, ack.Cancel = k, size, cancel
		if state != "running" {
			ack.Cancel = true
			return nil
		}
		if offset >= 0 && offset < size {
			skip := size - offset
			if skip >= int64(len(text)) {
				text = ""
			} else {
				text = text[skip:]
			}
		}
		if size >= runnerLogCap {
			ack.Truncated = true
			if size > runnerLogCap || text == "" {
				return nil
			}
			text = logTruncated
		} else if room := runnerLogCap - size; int64(len(text)) > room {
			text = text[:room]
			for len(text) > 0 && !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
			text += logTruncated
			ack.Truncated = true
		}
		text = strings.ToValidUTF8(text, "�")
		if text == "" {
			return nil
		}
		if _, err := t.ExecContext(ctx, `UPDATE runner_job_logs SET text = text || ? WHERE job_id = ?`, text, id); err != nil {
			return err
		}
		ack.Size = size + int64(len(text))
		if _, err := t.ExecContext(ctx, `UPDATE runner_jobs SET log_size = ? WHERE id = ?`, ack.Size, id); err != nil {
			return err
		}
		emit = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if emit {
		s.liveJobUpdate(actor.User.ID, id, kind, "running", ack.Size)
	}
	return &ack, nil
}

// liveJobUpdate tells the owner's open pages a job's output grew, at most
// once a second per job (live only; pages ask for the new bytes).
func (s *Service) liveJobUpdate(owner, id int64, kind, state string, size int64) {
	s.runners.mu.Lock()
	if s.runners.lastEmit == nil {
		s.runners.lastEmit = map[int64]time.Time{}
	}
	//wplint:ignore clock a real-time throttle on live log pushes; on a frozen test clock every push after the first would be dropped
	now := time.Now()
	if now.Sub(s.runners.lastEmit[id]) < runnerLogEmit {
		s.runners.mu.Unlock()
		return
	}
	s.runners.lastEmit[id] = now
	s.runners.mu.Unlock()
	s.Hub.Publish(&Event{Kind: EvRunnerJobUpdated, UserID: owner, At: s.now().UTC(), Data: map[string]any{"job": id, "kind": kind, "state": state, "size": size}})
}

// RunnerFinish is how a runner reports a job's end.
type RunnerFinish struct {
	State    string          `json:"state"` // done, failed, cancelled
	ExitCode *int            `json:"exit_code"`
	Result   json.RawMessage `json:"result"`
	Error    string          `json:"error"`
}

const (
	// RunnerResultMax is the most a job's result may be (bytes). An audit's
	// holds its findings and every helper run with its leads, cut leads and
	// evidence (WP-1320): live results passed 256 KB, which the runner took
	// as fatal and lost the work. The API reads a finish body up to this and
	// a margin (RunnerFinishBodyMax); a proxy needs a body limit above it.
	RunnerResultMax = 2 << 20
	// RunnerFinishBodyMax is the most a finish request's body may be: the
	// result and what else it carries (the state, the error).
	RunnerFinishBodyMax = RunnerResultMax + 64<<10
	// RunnerResultTooLarge starts the error for a result over the most; the
	// runner reads it to send the result again with less in it.
	RunnerResultTooLarge = "the result is too large"
)

// FinishRunnerJob records how a job ended. A job that already ended
// (cancelled, expired) stays as it is; the answer says what it is.
func (s *Service) FinishRunnerJob(ctx context.Context, actor *Actor, id int64, in RunnerFinish) (string, error) {
	if err := runnerActor(actor); err != nil {
		return "", err
	}
	if !slices.Contains([]string{"done", "failed", "cancelled"}, in.State) {
		return "", invalid("state must be done, failed or cancelled")
	}
	if len(in.Result) > RunnerResultMax {
		return "", invalid("%s: %d bytes, the most is %d (2 MB)", RunnerResultTooLarge, len(in.Result), RunnerResultMax)
	}
	if len(in.Result) > 0 && !json.Valid(in.Result) {
		return "", invalid("result must be JSON")
	}
	state := in.State
	err := s.write(ctx, actor, func(t *tx) error {
		_, cur, _, cancel, _, err := s.runnerJobFor(ctx, t, actor, id)
		if err != nil {
			return err
		}
		if cur != "running" {
			state = cur
			return nil
		}
		if cancel && state == "failed" {
			state = "cancelled"
		}
		msg := excerpt(in.Error, 2000)
		if state == "cancelled" {
			var why string
			_ = t.QueryRowContext(ctx, `SELECT error FROM runner_jobs WHERE id = ?`, id).Scan(&why)
			msg = cmp(why, msg)
		}
		return t.finishRunnerJob(ctx, id, state, in.ExitCode, in.Result, msg)
	})
	return state, err
}

// finishRunnerJob ends a queued or running job and announces it.
func (t *tx) finishRunnerJob(ctx context.Context, id int64, state string, exit *int, result json.RawMessage, msg string) error {
	var res any
	if len(result) > 0 {
		res = string(result)
	}
	var owner, sid, size int64
	var kind, source string
	var runner sql.NullInt64
	err := t.QueryRowContext(ctx, `UPDATE runner_jobs SET state = ?, exit_code = ?, result = ?, error = ?, finished_at = ?, cancel_requested = 0
		WHERE id = ? AND state IN ('queued','running') RETURNING owner_id, kind, source, source_id, runner_id, log_size`,
		state, exit, res, msg, t.s.nowMS(), id).Scan(&owner, &kind, &source, &sid, &runner, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := t.dropJobTokens(ctx, id); err != nil {
		return err
	}
	if source == sandboxDrillSource && sid != 0 {
		if err := t.finishSandboxDrill(ctx, sid, state, result, msg); err != nil {
			return err
		}
	}
	data := map[string]any{"job": id, "kind": kind, "state": state, "source": source, "source_id": sid}
	fin := &Event{Kind: EvRunnerJobFinished, UserID: owner, Data: data}
	if source == "automation_step" && sid != 0 {
		var pid sql.NullInt64
		_ = t.QueryRowContext(ctx, `SELECT project_id FROM automation_runs WHERE id = ?`, sid).Scan(&pid)
		fin.ProjectID = pid.Int64
	}
	t.emit(fin)
	upd := map[string]any{"job": id, "kind": kind, "state": state, "size": size, "runner": runner.Int64}
	t.emit(&Event{Kind: EvRunnerJobUpdated, UserID: owner, Data: upd})
	t.s.runners.mu.Lock()
	delete(t.s.runners.lastEmit, id)
	t.s.runners.mu.Unlock()
	return nil
}

// CancelRunnerJob stops a job: a waiting one is cancelled at once; a running
// one is told to stop by its runner's next poll or log upload (at once when
// the runner has gone). Only people can: the job's creator or its runner's
// owner.
func (s *Service) CancelRunnerJob(ctx context.Context, actor *Actor, id int64) (*RunnerJobInfo, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if actor.IsAgent() || actor.Automation != nil {
		return nil, fmt.Errorf("only people cancel runner jobs: %w", ErrForbidden)
	}
	j, err := s.GetRunnerJob(ctx, actor, id, -1)
	if err != nil {
		return nil, err
	}
	if full, _ := jobAccess(actor, j.OwnerID, j.CreatedBy); !full {
		return nil, ErrForbidden
	}
	return s.cancelRunnerJob(ctx, actor, id, "Cancelled by "+actor.User.Name()+".")
}

// cancelRunnerJob cancels a job for the reason given, which becomes its
// error; a running job keeps it until its runner reports back. actor is
// whose job it is: Waypoint cancelling one its runner never took says so
// in why, so nobody reads it as the person's doing (WP-752).
func (s *Service) cancelRunnerJob(ctx context.Context, actor *Actor, id int64, why string) (*RunnerJobInfo, error) {
	j, err := s.GetRunnerJob(ctx, actor, id, -1)
	if err != nil {
		return nil, err
	}
	if j.State != "queued" && j.State != "running" {
		return nil, invalid("this job has already finished")
	}
	err = s.write(ctx, actor, func(t *tx) error {
		offline := false
		if j.State == "running" && j.RunnerID != nil {
			var seen sql.NullInt64
			_ = t.QueryRowContext(ctx, `SELECT last_seen_at FROM runners WHERE id = ?`, *j.RunnerID).Scan(&seen)
			offline = !seen.Valid || s.nowMS()-seen.Int64 >= runnerOnlineFor.Milliseconds()
		}
		if j.State == "queued" || offline || j.RunnerID == nil {
			if err := t.finishRunnerJob(ctx, id, "cancelled", nil, nil, why); err != nil {
				return err
			}
		} else if _, err := t.ExecContext(ctx, `UPDATE runner_jobs SET error = CASE cancel_requested WHEN 1 THEN error ELSE ? END, cancel_requested = 1 WHERE id = ? AND state = 'running'`, why, id); err != nil {
			return err
		} else {
			t.emit(&Event{Kind: EvRunnerJobUpdated, UserID: j.OwnerID, Data: map[string]any{"job": id, "kind": j.Kind, "state": "running", "size": j.LogSize, "cancel": true}})
		}
		t.audit(AuditEntry{Action: "runner.cancel", Category: AuditControl, TargetType: "runner_job", TargetID: id, Target: j.Title, Detail: "cancelled a " + j.Kind + " job: " + j.Title + " (" + why + ")"})
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.kickRunners()
	return s.GetRunnerJob(ctx, actor, id, -1)
}
