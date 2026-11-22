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
// files a session under <config>/projects by the directory it ran in (the
// config directory is CLAUDE_CONFIG_DIR, else ~/.claude). It used to run with
// --no-session-persistence, so nothing was filed: a job's spend could not be
// attributed, and what a round did before it timed out could not be read. Now
// the session is kept, in the layout every other session uses, where `wp usage
// sync` (the runner's own timer, hooks, by hand) archives it (WP-1466) and
// Claude Code's retention (cleanupPeriodDays, 30 days unless settings.json in
// that config directory says otherwise) bounds the copy on this disk.
// keepTranscripts also writes a sidecar beside the transcripts, puts the main
// run's session id and the directory in the job's result (the link from the
// server's record of the job to its archived transcript) and sends the job's
// sessions to the server at once, before the checkout goes and however the job
// ended. An audit's helper runs (runner_helpers.go haikuRun) run in the same
// checkout, so their sessions are filed in the same project directory, listed
// in the sidecar's `sessions` and uploaded with it; `session_id` names the
// main run's session only.

// jobSidecarFile is the sidecar's name in the job's project directory. It is
// not a .jsonl file, so no transcript reader mistakes it for a session.
const jobSidecarFile = "wp-job.json"

// transcriptUploadLimit bounds the upload at the end of a job. A job that
// ends on its timeout or a cancel has no context left, so the upload has its
// own; the runner's timer sends what a slow or failed one leaves. Chosen, not
// measured: this assumes a job's sessions are a few MB (not checked), and the
// job's result and the runner's slot wait on the upload.
var transcriptUploadLimit = 2 * time.Minute

// transcriptBusyWait and transcriptBusyTries: another sync holds the lock
// (the timer's, a hook's), so look again a few times before leaving it to the
// timer. Chosen, not measured: it assumes a hook's sync pass normally ends
// within seconds (not checked), and the timer's is covered by the next timer
// pass. Tests shorten the wait.
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
	WPVersion string   `json:"wp_version"`
}

// withLink puts link (the session id, the directory) in the job's result.
func withLink(o outcome, link map[string]any) outcome {
	if len(link) == 0 {
		return o
	}
	switch m := o.Result.(type) {
	case nil:
		o.Result = link
	case map[string]any:
		for k, v := range link {
			m[k] = v
		}
	}
	return o
}

// jobProjectDir is the directory Claude Code filed the job's sessions under:
// <config>/projects/<the checkout's path with every character but letters and
// digits turned into "-">. The checkout's name (job<id>-<number>) is the same
// either way, and no other job's ends with it.
func jobProjectDir(configDir, work string) string {
	// ReadDir, not Glob: a [ in the config directory is a path, not a pattern
	projects := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return ""
	}
	suffix := "-" + filepath.Base(work)
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return filepath.Join(projects, e.Name())
		}
	}
	return ""
}

// jobTranscripts lists the .jsonl files in a job's project directory.
func jobTranscripts(proj string) []string {
	entries, _ := os.ReadDir(proj)
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, filepath.Join(proj, e.Name()))
		}
	}
	return out
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
		if drv != nil { // silent when the job ended before the CLI was built
			fmt.Fprintf(run.out, "No transcript found for this job (session id %q) under %s\n", drv.sessionID, filepath.Join(dir, "projects"))
			if drv.sessionID != "" { // no directory, so no sidecar, but the link to the session stands
				o = withLink(o, map[string]any{"session_id": drv.sessionID})
			}
		}
		return o
	}
	mains := jobTranscripts(proj)
	// the words the server will be told, unless a cancel comes during the upload (settled again below)
	o = settleOutcome(ctx, run.cancelled, o)
	meta := jobMeta{
		Job: run.id, Kind: run.kind, Title: run.title, CLI: in.CLI, Model: in.Model, Effort: in.Effort,
		Repo: in.Repo, Base: in.Base, Branch: in.Branch,
		Started: started.UTC().Format(time.RFC3339), Ended: time.Now().UTC().Format(time.RFC3339),
		State: o.State, Error: o.Error, Sessions: []string{}, Work: work, WPVersion: version,
	}
	if drv != nil {
		meta.SessionID = drv.sessionID
	}
	for _, m := range mains {
		meta.Sessions = append(meta.Sessions, strings.TrimSuffix(filepath.Base(m), ".jsonl"))
	}
	writeSidecar := func() {
		if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
			if err := os.WriteFile(filepath.Join(proj, jobSidecarFile), append(b, '\n'), 0o600); err != nil {
				fmt.Fprintf(run.out, "Couldn't write %s beside the transcripts: %v\n", jobSidecarFile, err)
			}
		}
	}
	writeSidecar()
	// A cancel can arrive while the upload runs: settle again, and make the
	// sidecar say what the job reports (locally only; it is not re-sent).
	resettle := func() {
		if n := settleOutcome(ctx, run.cancelled, o); n.State != o.State || n.Error != o.Error {
			o = n
			meta.State, meta.Error = o.State, o.Error
			writeSidecar()
			fmt.Fprintf(run.out, "The job was cancelled during the upload; %s on this machine now says %s.\n", jobSidecarFile, o.State)
		}
	}
	// The link from the job's record to its transcripts: the session's id when
	// claude named one, the directory when it holds a transcript.
	link := map[string]any{}
	if meta.SessionID != "" {
		link["session_id"] = meta.SessionID
	}
	if len(mains) > 0 {
		link["transcript_dir"] = proj
	}
	o = withLink(o, link)
	if len(mains) > 0 {
		fmt.Fprintf(run.out, "Transcripts kept in %s (%d session%s)\n", proj, len(mains), plural(len(mains)))
	}
	if drv != nil && (meta.SessionID == "" || len(mains) == 0) {
		fmt.Fprintf(run.out, "claude ran but filed no session id or no transcript for this job (session id %q, %d transcript%s in %s)\n", meta.SessionID, len(mains), plural(len(mains)), proj)
	}
	if len(mains) == 0 {
		return o
	}
	if p.opts.NoUsageSync {
		fmt.Fprintln(run.out, "Usage sync is off here: the transcripts are kept on this machine only and not uploaded.")
		return o
	}
	// Not the job's context: on a timeout or a cancel it is done.
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transcriptUploadLimit)
	defer cancel()
	// A person's cancel of this job does not cut the upload, but the runner
	// stopping does: its finish report must not wait for a slow server.
	if p.stopCtx != nil {
		defer context.AfterFunc(p.stopCtx, cancel)()
	}
	res, err := p.uploadTranscripts(uctx, dir, mains)
	resettle()
	const rest = "the next usage sync sends the rest."
	unfinished := err != nil || res.ArchiveError != "" || (res.Archive != nil && res.Archive.Stopped != "")
	switch {
	case p.stopCtx != nil && p.stopCtx.Err() != nil && unfinished:
		fmt.Fprintln(run.out, "The runner is stopping, so the transcript upload was cut short; a later usage sync sends the rest.")
	case err != nil:
		fmt.Fprintf(run.out, "The transcript upload did not finish (%v); %s\n", err, rest)
	case res.ArchiveError != "":
		fmt.Fprintf(run.out, "The usage counts were uploaded; the transcript archive upload did not finish cleanly (%s); %s\n", res.ArchiveError, rest)
	case res.Archive != nil && res.Archive.Stopped == archiveBusy:
		fmt.Fprintln(run.out, "The usage counts were uploaded; another transcript archive pass is running, and a later usage sync sends these transcripts.")
	case res.Archive != nil && res.Archive.Stopped == archiveNoneStop:
		fmt.Fprintf(run.out, "The usage counts were uploaded; %s, so the transcripts stay on this machine.\n", archiveNoneStop)
	case res.Archive != nil && res.Archive.Stopped != "":
		fmt.Fprintf(run.out, "The usage counts were uploaded; not all transcripts were sent (%s); %s\n", res.Archive.Stopped, rest)
	}
	return o
}

// uploadTranscripts sends the job's sessions and their sub-agents (usageSync
// with those files only, outdated transcripts and the archive pass's backlog
// left out), within ctx. It looks again while another sync holds the lock.
func (p *runnerProc) uploadTranscripts(ctx context.Context, dir string, mains []string) (*usageSyncResult, error) {
	var err error
	for try := 0; try < transcriptBusyTries; try++ {
		var res *usageSyncResult
		res, err = usageSync(p.c, usageSyncOptions{Dir: dir, Files: mains, Ctx: ctx, OnlyFiles: true})
		if !errors.Is(err, errUsageBusy) {
			return res, err
		}
		if try == transcriptBusyTries-1 {
			break // no wait after the last look
		}
		if !sleepCtx(ctx, transcriptBusyWait) {
			return nil, ctx.Err()
		}
	}
	return nil, err
}
