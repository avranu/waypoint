package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The shapes of the jobs a server sends (internal/core/contracts.go); the
// runner reads only what it needs.
type commandJob struct {
	Command  string            `json:"command"`
	Dir      string            `json:"dir"`
	Env      map[string]string `json:"env"`
	KeepKeys bool              `json:"keep_api_keys"`
}

type fileChange struct {
	Path    string `json:"path"`
	BaseSHA string `json:"base_sha"`
	Content string `json:"content"`
	Delete  bool   `json:"delete"`
}

type applyJob struct {
	Dir    string       `json:"dir"`
	Files  []fileChange `json:"files"`
	Commit string       `json:"commit"`
	Push   bool         `json:"push"`
}

type agentJob struct {
	CLI           string `json:"cli"`
	Model         string `json:"model"`
	Effort        string `json:"effort"` // claude's --effort; "" = its own
	Prompt        string `json:"prompt"`
	Repo          string `json:"repo"`
	Base          string `json:"base"`
	Branch        string `json:"branch"`
	Push          bool   `json:"push"`
	ReadOnly      bool   `json:"read_only"`
	Permission    string `json:"permission"` // read, edit (the default), edit_run
	JSONSchema    string `json:"json_schema"`
	CommitMessage string `json:"commit_message"`
	// ProtectPaths are globs of files the commits may not touch.
	ProtectPaths []string `json:"protect_paths"`
	// Target is the branch the work is going into, when it isn't the
	// default one: a protected file the same as it there was merged in.
	Target string `json:"target"`
	// Patch, for a job that runs no agent: changes to protected files held
	// back from a round, applied to Branch and pushed once checked.
	Patch string `json:"patch"`
	// KeepOnTimeout: when the agent runs out of time, the commits it had
	// made are still checked and pushed (salvage, WP-1597).
	KeepOnTimeout bool `json:"keep_on_timeout,omitempty"`
	// Confine: the prompt carries text others wrote (a pull request), so the
	// CLI must not be able to read beyond its checkout; a CLI that can't be
	// confined is refused (WP-6).
	Confine bool `json:"confine"`
	// DiffFrom, for an audit: the commit the change starts from. The diff
	// from it to Base goes into the prompt (WP-348).
	DiffFrom string `json:"diff_from"`
	// DiffSince (unix seconds): the change starts at Base's last
	// first-parent commit before then; DiffPaths and DiffExclude limit it
	// (WP-548).
	DiffSince   int64    `json:"diff_since"`
	DiffPaths   []string `json:"diff_paths"`
	DiffExclude []string `json:"diff_exclude"`
	// Helpers, for an audit: the Haiku checks run around its model in the
	// same checkout (runner_helpers.go, WP-1320).
	Helpers *auditHelpers `json:"helpers,omitempty"`
	// WorkTimeout (seconds) is how long the agent may work, not counting
	// the time its commands wait for a machine-wide slot; the claim's
	// timeout limits the whole run. 0: no separate limit (WP-1618).
	WorkTimeout int `json:"work_timeout"`
}

// perm is the permission level the job runs at: read-only (files can be
// read, nothing changes), edit (the default) or edit_run (it may also run
// commands such as the tests).
func (j agentJob) perm() string {
	switch {
	case j.ReadOnly || j.Permission == "read":
		return "read"
	case j.Permission == "edit_run":
		return "edit_run"
	}
	return "edit"
}

// outcome is how a job ended, as reported to the server.
type outcome struct {
	State    string // done, failed, cancelled
	ExitCode *int
	Result   any
	Error    string
}

func failed(format string, a ...any) outcome {
	return outcome{State: "failed", Error: fmt.Sprintf(format, a...)}
}

// runnerSelfEnv tells a command job where this runner's wp is and which
// directories it allows, so `wp kit sync --all` (the Kit page's Roll out)
// runs the same wp over the same folders.
func runnerSelfEnv(allowed []string) []string {
	var out []string
	if exe, err := os.Executable(); err == nil {
		out = append(out, "WAYPOINT_WP="+exe)
	}
	if len(allowed) > 0 {
		out = append(out, "WAYPOINT_RUNNER_ALLOW="+strings.Join(allowed, string(os.PathListSeparator)))
	}
	return out
}

// execCommand runs a shell command in an allowed directory, streaming its
// combined output, and kills its process group on timeout or cancel.
func (p *runnerProc) execCommand(ctx context.Context, run *jobRun, in commandJob) outcome {
	dir, err := resolveDir(in.Dir, p.allowed)
	if err != nil {
		return failed("%v", err)
	}
	if strings.TrimSpace(in.Command) == "" {
		return failed("no command")
	}
	ctx, cancel := context.WithTimeout(ctx, run.timeout)
	defer cancel()
	cmd := shellCommand(ctx, in.Command)
	cmd.Dir = dir
	cmd.Env = append(jobEnv(os.Environ(), in.Env, in.KeepKeys), runnerSelfEnv(p.allowed)...)
	cmd.Stdout, cmd.Stderr = run.out, run.out // one writer: output stays in order
	cmd.Stdin = nil
	setProcGroup(cmd)
	killOnCancel(cmd)
	return finishProcess(ctx, run, cmd, "command")
}

// finishProcess starts cmd and turns how it ended into an outcome, with the
// exit code and the end of the output.
func finishProcess(ctx context.Context, run *jobRun, cmd *exec.Cmd, what string) outcome {
	if err := cmd.Start(); err != nil {
		return failed("can't start the %s: %v", what, err)
	}
	err := cmd.Wait()
	procExited(cmd)
	return processOutcome(ctx, run, cmd, err, what)
}

// runProc is cmd.Run for a procCommand: when it ends, a cancelled command's
// delayed kill is settled.
func runProc(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	err := cmd.Wait()
	procExited(cmd)
	return err
}

// procCommand is a command that stops with ctx, in its own process group.
func procCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 G702 -- the CLIs and programs this runner drives; arguments are validated or follow --
	setProcGroup(cmd)
	killOnCancel(cmd)
	return cmd
}

func processOutcome(ctx context.Context, run *jobRun, cmd *exec.Cmd, err error, what string) outcome {
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	result := map[string]any{"exit_code": code}
	if run.out.Truncated() {
		result["truncated"] = true
		result["tail"] = run.out.Tail()
	}
	o := outcome{ExitCode: &code, Result: result}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		o.State, o.Error = "failed", fmt.Sprintf("The %s timed out after %s and was stopped.", what, run.timeout.Round(time.Second))
		result["timed_out"] = true
	case ctx.Err() != nil:
		o.State, o.Error = "cancelled", "Stopped."
	case err == nil:
		o.State = "done"
	default:
		var ee *exec.ExitError
		o.State = "failed"
		if errors.As(err, &ee) {
			o.Error = fmt.Sprintf("The %s exited with code %d.", what, code)
		} else {
			o.Error = fmt.Sprintf("The %s failed: %v", what, err)
		}
	}
	return o
}

var _ io.Writer = (*logSink)(nil)
