package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A job's transcripts outlive it. The agent, review and audit jobs run
// Claude Code in a checkout that is removed when the job ends, and Claude Code
// files a session under ~/.claude/projects by the directory it ran in. It
// used to run with --no-session-persistence, so nothing was filed: a job's
// spend could not be attributed, and what a round did before it timed out
// could not be read. Now the session is kept, in the layout every other
// session uses, where `wp usage sync` (the runner's own timer, hooks, by hand)
// archives it (WP-1466) and Claude Code's retention (cleanupPeriodDays, 30
// days unless ~/.claude/settings.json says otherwise) bounds the copy on this
// disk. keepTranscripts also writes a sidecar beside the transcripts, puts the
// session's id and directory in the job's result (the link from the server's
// record of the job to its archived transcript) and sends them to the server
// at once, before the checkout goes and however the job ended.

// jobSidecarFile is the sidecar's name in the job's project directory. It is
// not a .jsonl file, so no transcript reader mistakes it for a session.
const jobSidecarFile = "wp-job.json"

// transcriptUploadLimit bounds the upload at the end of a job. A job that
// ends on its timeout or a cancel has no context left, so the upload has its
// own; the runner's timer sends what a slow or failed one leaves.
var transcriptUploadLimit = 2 * time.Minute

// transcriptBusyWait and transcriptBusyTries: another sync holds the lock
// (the timer's, a hook's), so look again a few times before leaving it to the
// timer. Tests shorten the wait.
var (
	transcriptBusyWait  = 5 * time.Second
	transcriptBusyTries = 6
)

// jobMeta is what the sidecar says about the job a transcript belongs to,
// since the transcript itself holds neither the job's id nor how it ended.
type jobMeta struct {
	Job       int64    `json:"job"`
	Kind      string   `json:"kind"`
	Title     string   `json:"title,omitempty"`
	CLI       string   `json:"cli"`
	Model     string   `json:"model,omitempty"`
	Effort    string   `json:"effort,omitempty"`
	Repo      string   `json:"repo,omitempty"`
	Base      string   `json:"base,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Started   string   `json:"started"`
	Ended     string   `json:"ended"`
	State     string   `json:"state"`
	Error     string   `json:"error,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Sessions  []string `json:"sessions"`
	Work      string   `json:"work"`
}

// jobProjectDir is the directory Claude Code filed the job's sessions under:
// <config>/projects/<the checkout's path with every character but letters and
// digits turned into "-">. The checkout's name (job<id>-<number>) is the same
// either way, and no other job's ends with it.
func jobProjectDir(configDir, work string) string {
	matches, _ := filepath.Glob(filepath.Join(configDir, "projects", "*-"+filepath.Base(work)))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// keepTranscripts is what a claude job does as it ends, before its checkout
// is removed. It returns o with the session's id and directory in the result.
// It never fails the job: a transcript it can't keep is logged and the job's
// outcome stands.
func (p *runnerProc) keepTranscripts(ctx context.Context, run *jobRun, in agentJob, work string, started time.Time, drv *cliDriver, o outcome) outcome {
	if in.CLI != "claude" {
		return o
	}
	dir := claudeDir(p.opts.UsageDir)
	if dir == "" {
		return o
	}
	proj := jobProjectDir(dir, work)
	if proj == "" {
		return o // nothing was filed: the job ended before claude started one
	}
	mains, _ := filepath.Glob(filepath.Join(proj, "*.jsonl"))
	meta := jobMeta{
		Job: run.id, Kind: run.kind, Title: run.title, CLI: in.CLI, Model: in.Model, Effort: in.Effort,
		Repo: in.Repo, Base: in.Base, Branch: in.Branch,
		Started: started.UTC().Format(time.RFC3339), Ended: time.Now().UTC().Format(time.RFC3339),
		State: o.State, Error: o.Error, Sessions: []string{}, Work: work,
	}
	if drv != nil {
		meta.SessionID = drv.sessionID
	}
	for _, m := range mains {
		meta.Sessions = append(meta.Sessions, strings.TrimSuffix(filepath.Base(m), ".jsonl"))
	}
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join(proj, jobSidecarFile), append(b, '\n'), 0o600); err != nil {
			fmt.Fprintf(run.out, "Couldn't write %s beside the transcripts: %v\n", jobSidecarFile, err)
		}
	}
	// The link from the job's record to its transcripts.
	switch m := o.Result.(type) {
	case nil:
		o.Result = map[string]any{"session_id": meta.SessionID, "transcript_dir": proj}
	case map[string]any:
		m["session_id"], m["transcript_dir"] = meta.SessionID, proj
	}
	fmt.Fprintf(run.out, "Transcripts kept in %s (%d session%s)\n", proj, len(mains), map[bool]string{true: "", false: "s"}[len(mains) == 1])
	if p.opts.NoUsageSync || len(mains) == 0 {
		return o
	}
	// Not the job's context: on a timeout or a cancel it is done.
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transcriptUploadLimit)
	defer cancel()
	if err := p.uploadTranscripts(uctx, dir, mains); err != nil {
		fmt.Fprintf(run.out, "Transcripts not uploaded yet (%v); the runner's next usage sync sends them.\n", err)
	}
	return o
}

// uploadTranscripts sends the job's sessions and their sub-agents (usageSync
// with those files only, archive pass included).
func (p *runnerProc) uploadTranscripts(ctx context.Context, dir string, mains []string) error {
	var err error
	for try := 0; try < transcriptBusyTries; try++ {
		var res *usageSyncResult
		res, err = usageSync(p.c, usageSyncOptions{Dir: dir, Files: mains})
		if err == nil {
			if res.ArchiveError != "" {
				return errors.New(res.ArchiveError)
			}
			return nil
		}
		if !errors.Is(err, errUsageBusy) {
			return err
		}
		if !sleepCtx(ctx, transcriptBusyWait) {
			return ctx.Err()
		}
	}
	return err
}
