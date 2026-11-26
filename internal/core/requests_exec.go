package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core/udiff"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// runnerOnline says whether the person has a runner online on a machine
// (tests replace it).
var runnerOnline = func(ctx context.Context, s *Service, ownerID int64, machine string) bool {
	r := s.runnerOnMachine(ctx, ownerID, machine)
	return r != nil && r.Online
}

// runnerOnMachine is the person's runner on a session's machine: a runner is
// named after its host unless given another name (WP-D17).
func (s *Service) runnerOnMachine(ctx context.Context, ownerID int64, machine string) *Runner {
	all, err := s.ownerRunners(ctx, ownerID)
	if err != nil {
		return nil
	}
	var found *Runner
	for _, r := range all {
		if strings.EqualFold(r.Name, machine) && (found == nil || r.Online && !found.Online) {
			found = r
		}
	}
	return found
}

// enqueueRunnerJob queues a job (the contract's EnqueueRunnerJob; tests
// replace it). Set in init: queuing a job dispatches events, and dispatch can
// queue one (a failed run tells its agent, WP-170), an initialization cycle
// for a var's initializer.
var enqueueRunnerJob func(ctx context.Context, s *Service, a *Actor, in RunnerJobInput) (*RunnerJob, error)

func init() { enqueueRunnerJob = enqueueOnMachine }

// enqueueOnMachine: requests name the session's machine in Label; it picks
// the runner of that name, not a runner label.
func enqueueOnMachine(ctx context.Context, s *Service, a *Actor, in RunnerJobInput) (*RunnerJob, error) {
	if in.RunnerID == 0 && in.Label != "" {
		r := s.runnerOnMachine(ctx, in.OwnerID, in.Label)
		if r == nil {
			return nil, ErrNoRunner
		}
		in.RunnerID, in.Label = r.ID, ""
	}
	return s.EnqueueRunnerJob(ctx, a, in)
}

// lookupRunnerJob reads a finished job (its output, exit code and result):
// the finish event carries only its id and state (tests replace it).
var lookupRunnerJob = func(ctx context.Context, s *Service, id int64) (*RunnerJob, error) {
	var owner int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT owner_id FROM runner_jobs WHERE id = ?`, id).Scan(&owner); err != nil {
		return nil, err
	}
	return s.RunnerJobByID(ctx, &Actor{User: &User{ID: owner}}, id)
}

// noRunnerMessage is what a person reads when the machine has no runner.
func noRunnerMessage(machine string, forChange bool) string {
	if forChange {
		return fmt.Sprintf("No runner online on %s; connect one or open a PR instead", machine)
	}
	return fmt.Sprintf("No runner online on %s; connect one (wp runner) or run the command yourself", machine)
}

var slugRE = lazyre.New(`[^a-z0-9]+`)

func slug(s string, n int) string {
	s = strings.Trim(slugRE.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > n {
		s = strings.Trim(s[:n], "-")
	}
	return cmp(s, "change")
}

// applyChange does an approved change: on the person's runner in the
// session's worktree, or as a branch and pull request through the forge.
// Without a choice it takes the runner if one is online, else a pull
// request. It runs after the decision committed, and records how it ended.
func (s *Service) applyChange(ctx context.Context, actor *Actor, id int64, choice string) {
	list, err := s.loadRequests(ctx, actor, true, `r.id = ?`, id)
	if err != nil || len(list) == 0 {
		s.log.ErrorContext(ctx, "apply change: load", "request", id, "err", err)
		return
	}
	r := list[0]
	var files []FileChange
	for _, f := range r.files {
		files = append(files, FileChange{Path: f.Path, BaseSHA: f.BaseSHA, Content: f.Content, Delete: f.Delete})
	}
	var machine, cwd string
	if r.Session != nil {
		machine, cwd = r.Session.Machine, r.Session.Cwd
	}
	if changesNothing(r.files) {
		s.finishApply(ctx, actor, r, RequestApplied, "", noChangeResult(), "")
		return
	}
	outside := len(r.files) > 0 && r.files[0].Outside
	if outside {
		s.applyOutside(ctx, actor, r, files, machine, choice)
		return
	}
	canPR := false
	if r.projectID.Valid {
		role, rerr := s.projectRole(ctx, s.db.R, actor, r.projectID.Int64)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "projectid", r.projectID.Int64, "err", rerr)
			}
		}
		canPR = role >= RoleEditor
	}
	auto := choice == ""
	path := choice
	if auto {
		path = "pr"
		if cwd != "" && runnerOnline(ctx, s, actor.id(), machine) {
			path = "runner"
		}
	}
	fail := func(state, msg string, res RequestResult) {
		res.Reason = cmp(res.Reason, msg)
		s.finishApply(ctx, actor, r, state, path, res, msg)
	}
	if path == "runner" {
		if cwd == "" {
			if !auto || !canPR {
				fail(RequestFailed, "This request has no worktree to apply in (it didn't come from a session); open a PR instead", RequestResult{})
				return
			}
			path = "pr"
		} else {
			in := ApplyJob{Dir: cwd, Files: files, Commit: cmp(r.Message, r.Title)}
			b, _ := json.Marshal(in)
			job, err := enqueueRunnerJob(ctx, s, actor, RunnerJobInput{Kind: JobApply, OwnerID: actor.id(), Label: machine, Input: b, Source: "request_apply", SourceID: r.ID})
			switch {
			case err == nil:
				upd, _ := s.db.W.ExecContext(ctx, `UPDATE requests SET apply_path = 'runner', result = ?, updated_at = ? WHERE id = ? AND state = 'applying'`,
					mustJSON(RequestResult{Reason: fmt.Sprintf("job %d queued on %s", job.ID, machine)}), s.nowMS(), r.ID)
				if n, _ := upd.RowsAffected(); n > 0 {
					s.publishRequest(ctx, r, "applying")
				}
				return
			case errors.Is(err, ErrNoRunner) && auto && canPR:
				path = "pr"
			case errors.Is(err, ErrNoRunner):
				fail(RequestFailed, noRunnerMessage(machine, true), RequestResult{})
				return
			default:
				fail(RequestFailed, "The runner couldn't take the job: "+err.Error(), RequestResult{})
				return
			}
		}
	}
	if !canPR {
		fail(RequestFailed, "You need to edit the project to open a PR for this; connect a runner instead", RequestResult{})
		return
	}
	res, err := s.applyViaForge(ctx, r, files)
	var cc *ChangeConflict
	var np *prNotOpened
	switch {
	case err == nil:
		s.finishApply(ctx, actor, r, RequestApplied, "pr", *res, "")
	case errors.As(err, &np):
		s.log.WarnContext(ctx, "open the pull request for a change", "request", r.ID, "err", np.err)
		s.finishApply(ctx, actor, r, RequestFailed, "pr", *res, res.Reason)
	case errors.As(err, &cc):
		s.finishApply(ctx, actor, r, RequestConflict, "pr", RequestResult{Reason: cc.Error(), Conflicts: cc.Paths}, cc.Error())
	default:
		s.log.WarnContext(ctx, "apply change through the forge", "request", r.ID, "err", err)
		s.finishApply(ctx, actor, r, RequestFailed, "pr", RequestResult{}, "The forge refused: "+excerpt(err.Error(), 300))
	}
}

// applyOutside applies a change to files outside any git repository: only
// through a runner on the machine that asked, as a plain write.
func (s *Service) applyOutside(ctx context.Context, actor *Actor, r *Request, files []FileChange, machine, choice string) {
	fail := func(msg string) {
		s.finishApply(ctx, actor, r, RequestFailed, "runner", RequestResult{Reason: msg}, msg)
	}
	if choice == "pr" {
		fail("These files are outside any git repository, so there is no pull request to open; approve with the runner instead")
		return
	}
	if machine == "" {
		fail("This request doesn't say which machine asked (it didn't come from a session), so there is no runner to write the files on")
		return
	}
	dir := path.Dir(r.files[0].Path)
	for i := range files {
		files[i].Path = path.Base(files[i].Path)
	}
	b, _ := json.Marshal(ApplyJob{Dir: dir, Files: files})
	job, err := enqueueRunnerJob(ctx, s, actor, RunnerJobInput{Kind: JobApply, OwnerID: actor.id(), Label: machine, Input: b, Source: "request_apply", SourceID: r.ID})
	switch {
	case err == nil:
		upd, _ := s.db.W.ExecContext(ctx, `UPDATE requests SET apply_path = 'runner', result = ?, updated_at = ? WHERE id = ? AND state = 'applying'`,
			mustJSON(RequestResult{Reason: fmt.Sprintf("job %d queued on %s", job.ID, machine)}), s.nowMS(), r.ID)
		if n, _ := upd.RowsAffected(); n > 0 {
			s.publishRequest(ctx, r, "applying")
		}
	case errors.Is(err, ErrNoRunner):
		fail(fmt.Sprintf("No runner online on %s; connect one that is allowed to write in %s", machine, dir))
	default:
		fail("The runner couldn't take the job: " + err.Error())
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// prNotOpened: the files were committed to a branch but opening the pull
// request failed. applyViaForge returns it with the result describing the branch.
type prNotOpened struct{ err error }

func (e *prNotOpened) Error() string { return "could not open the pull request: " + e.err.Error() }
func (e *prNotOpened) Unwrap() error { return e.err }

// applyViaForge commits the files to a new branch of the project's repo and
// opens a pull request. A file that changed since the agent made its change
// is a conflict, and nothing is written.
func (s *Service) applyViaForge(ctx context.Context, r *Request, files []FileChange) (*RequestResult, error) {
	cl, ok := s.Forges.(ForgeCommitter)
	if !ok {
		return nil, errors.New("this forge client can't commit files")
	}
	if !r.projectID.Valid {
		return nil, errors.New("a pull request needs a project with the repository attached")
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, repo FROM project_repos WHERE project_id = ? ORDER BY id`, r.projectID.Int64)
	if err != nil {
		return nil, err
	}
	type pr struct {
		forge int64
		repo  string
	}
	var repos []pr
	for rows.Next() {
		var x pr
		if err := rows.Scan(&x.forge, &x.repo); err != nil {
			rows.Close()
			return nil, err
		}
		repos = append(repos, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var pick *pr
	for i, x := range repos {
		if r.Session != nil && strings.EqualFold(x.repo, r.Session.Repo) {
			pick = &repos[i]
		}
	}
	if pick == nil && len(repos) == 1 {
		pick = &repos[0]
	}
	if pick == nil {
		return nil, errors.New("no repository of the project matches the session's, so there is nowhere to open a PR")
	}
	f, err := s.forgeByID(ctx, pick.forge)
	if err != nil {
		return nil, err
	}
	if f.token == "" {
		return nil, errors.New("the forge has no API token; add one in Admin → Forges")
	}
	base, err := cl.DefaultBranch(ctx, f, pick.repo)
	if err != nil {
		return nil, err
	}
	var conflicts []string
	for _, fc := range files {
		cur, err := cl.FileBlobSHA(ctx, f, pick.repo, fc.Path, base)
		if err != nil {
			return nil, err
		}
		if cur != fc.BaseSHA {
			conflicts = append(conflicts, fc.Path)
		}
	}
	if len(conflicts) > 0 {
		return nil, &ChangeConflict{Paths: conflicts}
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%d", r.ID, s.nowMS()))
	branch := fmt.Sprintf("waypoint/%s-r%d-%s", slug(r.Title, 30), r.Number, hex.EncodeToString(sum[:])[:5])
	commit, err := cl.CommitFiles(ctx, f, pick.repo, CommitInput{Base: base, Branch: branch, Message: cmp(r.Message, r.Title), Files: files})
	if err != nil {
		return nil, err
	}
	body := r.Body
	if body != "" {
		body += "\n\n"
	}
	body += fmt.Sprintf("Requested by %s through Waypoint (%s) and approved there.", cmp(r.Agent, "an agent"), r.Ref)
	if base := s.BaseURL(); base != "" {
		body += "\n\n" + base + r.URL
	}
	p, err := cl.CreatePR(ctx, f, pick.repo, NewPR{Title: r.Title, Body: body, Head: branch, Base: base})
	if err != nil {
		return &RequestResult{Commit: commit.SHA, Branch: branch, Reason: "pushed the branch " + branch + " but could not open the PR: " + excerpt(err.Error(), 200)}, &prNotOpened{err}
	}
	return &RequestResult{Commit: commit.SHA, Branch: branch, PRURL: p.URL, PRTitle: p.Title, PRNumber: p.Number}, nil
}

// changesNothing is whether applying the files would alter none of them:
// each one is an edit whose content is already its base (WP-948).
func changesNothing(files []storedFile) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if f.Delete || f.BaseSHA == "" || udiff.GitBlobSHA(f.Content) != strings.ToLower(f.BaseSHA) {
			return false
		}
	}
	return true
}

func noChangeResult() RequestResult {
	return RequestResult{NoChange: true, Reason: "every file already has the requested content, so the change alters nothing."}
}

// finishApply records how applying ended and tells the person and the agent.
func (s *Service) finishApply(ctx context.Context, actor *Actor, r *Request, state, path string, res RequestResult, errText string) {
	err := s.write(ctx, actor, func(t *tx) error {
		n, err := t.ExecContext(ctx, `UPDATE requests SET state = ?, apply_path = ?, result = ?, error = ?, delivered_at = NULL, updated_at = ? WHERE id = ? AND state = 'applying'`,
			state, path, mustJSON(res), errText, s.nowMS(), r.ID)
		if err != nil {
			return err
		}
		if c, _ := n.RowsAffected(); c == 0 {
			return nil
		}
		e := requestEvent(EvRequestUpdated, r.ID, r.Ref, r.Number, r.Kind, state, r.userID, r.projectID, r.sessionID, r.Title, r.Agent)
		e.Data["decision"] = state
		t.emit(e)
		detail := state
		if res.NoChange {
			detail = "changed nothing: " + res.Reason
		} else if res.PRURL != "" {
			detail = "opened " + res.PRURL
		} else if res.Commit != "" {
			detail = "committed " + shortSHA(res.Commit)
		} else if errText != "" {
			detail += ": " + errText
		}
		t.audit(AuditEntry{Action: "request.apply", Category: AuditApproval, ProjectID: derefInt(ptrInt(r.projectID)), TargetType: "request", TargetID: r.ID,
			Target: r.Ref + " " + r.Title, URL: r.URL, Detail: detail, About: r.Agent})
		return nil
	})
	if err != nil {
		s.log.ErrorContext(ctx, "record how applying a change ended", "request", r.ID, "err", err)
	}
}

// RunCell runs a command cell on a runner the person owns (WP-D17): the one
// named like the session's machine. The person may edit the command first;
// each run is kept.
func (s *Service) RunCell(ctx context.Context, actor *Actor, ref string, n int, command string) (*Request, error) {
	if err := actor.need(PermRequestsRun); err != nil {
		return nil, err
	}
	if err := actor.requirePersonDirect("running a request's command"); err != nil {
		return nil, err
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	id, err := s.resolveRequestID(ctx, s.db.R, actor, ref)
	if err != nil {
		return nil, err
	}
	command = strings.TrimSpace(command)
	if len(command) > 8000 {
		return nil, invalid("a command can be at most 8000 characters")
	}
	var runID int64
	var dir, machine string
	err = s.write(ctx, actor, func(t *tx) error {
		r, err := s.loadRequestsTx(ctx, t, actor, id)
		if err != nil {
			return err
		}
		if !s.canDecide(ctx, actor, r, map[int64]int{}) {
			return ErrForbidden
		}
		rerun := r.State == RequestFailed && r.Kind == RequestCommand
		if r.State != RequestOpen && !rerun {
			return invalid("%s is already %s; commands can be run while it is open", r.Code, r.State)
		}
		if rerun {
			if _, err := t.ExecContext(ctx, `UPDATE requests SET state = ?, decision = '', decided_by = NULL, decided_at = NULL, delivered_at = NULL, error = '', updated_at = ? WHERE id = ?`,
				RequestOpen, s.nowMS(), r.ID); err != nil {
				return err
			}
		}
		var cellID int64
		var orig string
		err = t.QueryRowContext(ctx, `SELECT id, command, dir, machine FROM request_cells WHERE request_id = ? AND n = ?`, r.ID, n).Scan(&cellID, &orig, &dir, &machine)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("command " + strconv.Itoa(n))
		}
		if err != nil {
			return err
		}
		if command == "" {
			command = orig
		}
		if dir == "" && r.sessionID.Valid {
			_ = t.QueryRowContext(ctx, `SELECT cwd FROM agent_sessions WHERE id = ?`, r.sessionID.Int64).Scan(&dir)
		}
		res, err := t.ExecContext(ctx, `INSERT INTO request_cell_runs(cell_id, command, state, run_by, created_at) VALUES (?,?,'queued',?,?)`, cellID, command, actor.id(), s.nowMS())
		if err != nil {
			return err
		}
		runID, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		return nil, err
	}
	in := CommandJob{Command: command, Dir: dir}
	b, _ := json.Marshal(in)
	job, err := enqueueRunnerJob(ctx, s, actor, RunnerJobInput{Kind: JobCommand, OwnerID: actor.id(), Label: machine, Input: b, Source: "request_cell", SourceID: runID})
	if err != nil {
		msg := "The runner couldn't take the job: " + err.Error()
		if errors.Is(err, ErrNoRunner) {
			msg = noRunnerMessage(cmp(machine, "that machine"), false)
		}
		_, _ = s.db.W.ExecContext(ctx, `UPDATE request_cell_runs SET state = 'failed', error = ?, finished_at = ? WHERE id = ?`, msg, s.nowMS(), runID)
		s.publishRequestID(ctx, id, "ran")
		return nil, invalid("%s", msg)
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE request_cell_runs SET job_id = ?, state = ? WHERE id = ? AND state = 'queued'`, job.ID, cmp(job.State, "queued"), runID); err != nil {
		return nil, err
	}
	s.publishRequestID(ctx, id, "ran")
	return s.GetRequest(ctx, actor, strconv.FormatInt(id, 10), 0)
}

// requestJobFinished handles a finished runner job that was started for a
// request: a command cell's run, or an approved change.
func (s *Service) requestJobFinished(ctx context.Context, e *Event) {
	src, _ := e.Data["source"].(string)
	if src == "request_decision" {
		s.decisionJobFinished(ctx, e)
		return
	}
	if src != "request_cell" && src != "request_apply" {
		return
	}
	sid := dataInt(e, "source_id")
	jobID := dataInt(e, "job")
	state, _ := e.Data["state"].(string)
	job := &RunnerJob{ID: jobID, State: state}
	if lookupRunnerJob != nil && jobID != 0 {
		if j, err := lookupRunnerJob(ctx, s, jobID); err == nil && j != nil {
			job = j
		}
	}
	if out, ok := e.Data["output"].(string); ok && job.Output == "" {
		job.Output = out
	}
	if _, ok := e.Data["exit_code"]; ok && job.ExitCode == nil {
		c := int(dataInt(e, "exit_code"))
		job.ExitCode = &c
	}
	if msg, ok := e.Data["error"].(string); ok && job.Error == "" {
		job.Error = msg
	}
	switch src {
	case "request_cell":
		s.finishCellRun(ctx, sid, job)
	case "request_apply":
		s.finishApplyJob(ctx, sid, job)
	}
}

// requestJobStarted marks a command cell's run as running when its runner
// takes the job (the claim is the update with no output yet).
func (s *Service) requestJobStarted(ctx context.Context, e *Event) {
	if k, _ := e.Data["kind"].(string); k != JobCommand {
		return
	}
	if st, _ := e.Data["state"].(string); st != "running" || dataInt(e, "size") != 0 {
		return
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE request_cell_runs SET state = 'running' WHERE job_id = ? AND state = 'queued' AND finished_at IS NULL`, dataInt(e, "job"))
	if err != nil {
		s.log.ErrorContext(ctx, "mark a command as running", "job", dataInt(e, "job"), "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	var reqID int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT c.request_id FROM request_cell_runs r JOIN request_cells c ON c.id = r.cell_id WHERE r.job_id = ?`, dataInt(e, "job")).Scan(&reqID); err == nil {
		s.publishRequestID(ctx, reqID, "ran")
	}
}

func (s *Service) finishCellRun(ctx context.Context, runID int64, job *RunnerJob) {
	out, trunc := job.Output, 0
	if len(out) > maxCellOutput {
		out, trunc = "…(output cut)\n"+out[len(out)-maxCellOutput:], 1
	}
	now := s.nowMS()
	var code any
	if job.ExitCode != nil {
		code = *job.ExitCode
	}
	state, errText := job.State, job.Error
	switch state {
	case "done", "failed", "cancelled", "expired":
	default:
		state, errText = "failed", cmp(errText, "The runner didn't report how the command ended.")
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE request_cell_runs SET state = ?, exit_code = ?, output = ?, truncated = ?, error = ?, finished_at = ? WHERE id = ? AND finished_at IS NULL`,
		state, code, out, trunc, errText, now, runID)
	if err != nil {
		s.log.ErrorContext(ctx, "save a command's output", "run", runID, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	var reqID int64
	var runBy sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT c.request_id, r.run_by FROM request_cell_runs r JOIN request_cells c ON c.id = r.cell_id WHERE r.id = ?`, runID).Scan(&reqID, &runBy); err != nil {
		return
	}
	if job.ExitCode != nil && *job.ExitCode != 0 && (state == "done" || state == "failed") && s.returnFailedRun(ctx, reqID, runBy, *job.ExitCode) {
		return
	}
	s.publishRequestID(ctx, reqID, "ran")
}

// returnFailedRun sends an open command request back to the agent that asked
// when a run of it exits non-zero (WP-170): it is failed, decided by whoever
// ran it, and the agent is told with the output, at once when it sits idle,
// so it fixes what stopped the command instead of the person relaying it.
// Run again reopens it. It reports whether the request was sent back.
func (s *Service) returnFailedRun(ctx context.Context, reqID int64, runBy sql.NullInt64, code int) bool {
	var back bool
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		now := s.nowMS()
		res, err := t.ExecContext(ctx, `UPDATE requests SET state = ?, decision = 'ran', note = '', decided_by = ?, decided_at = ?, delivered_at = NULL, snoozed_until = NULL, error = ?, updated_at = ?
			WHERE id = ? AND state = ? AND kind = ?`, RequestFailed, runBy, now, fmt.Sprintf("exit %d", code), now, reqID, RequestOpen, RequestCommand)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		back = true
		r, err := s.loadRequestsTx(ctx, t, SystemActor(), reqID)
		if err != nil {
			return err
		}
		e := requestEvent(EvRequestUpdated, r.ID, r.Ref, r.Number, r.Kind, RequestFailed, r.userID, r.projectID, r.sessionID, r.Title, r.Agent)
		e.Data["decision"] = "ran"
		t.emit(e)
		return nil
	})
	if err != nil {
		s.log.ErrorContext(ctx, "send a failed command back to the agent", "request", reqID, "err", err)
		return false
	}
	if back {
		s.tellDecision(ctx, reqID)
	}
	return back
}

func (s *Service) finishApplyJob(ctx context.Context, reqID int64, job *RunnerJob) {
	list, err := s.loadRequests(ctx, SystemActor(), false, `r.id = ?`, reqID)
	if err != nil || len(list) == 0 {
		return
	}
	r := list[0]
	var raw struct {
		Commit    string `json:"commit"`
		Branch    string `json:"branch"`
		Conflict  bool   `json:"conflict"`
		Conflicts []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"conflicts"`
	}
	_ = json.Unmarshal(job.Result, &raw)
	res := RequestResult{Commit: raw.Commit, Branch: raw.Branch}
	var why []string
	for _, c := range raw.Conflicts {
		res.Conflicts = append(res.Conflicts, c.Path)
		why = append(why, c.Path+" ("+c.Reason+")")
	}
	switch {
	case job.State == "done" && raw.Commit == "" && changesNothing(r.files):
		res.NoChange, res.Reason = true, noChangeResult().Reason
		s.finishApply(ctx, SystemActor(), r, RequestApplied, "runner", res, "")
	case job.State == "done":
		s.finishApply(ctx, SystemActor(), r, RequestApplied, "runner", res, "")
	case raw.Conflict || len(raw.Conflicts) > 0:
		res.Reason = cmp(job.Error, "the files changed since the agent made the change")
		if len(why) > 0 {
			res.Reason = "Nothing was written: " + strings.Join(why, ", ") + " no longer match what the agent changed."
		}
		s.finishApply(ctx, SystemActor(), r, RequestConflict, "runner", res, res.Reason)
	default:
		s.finishApply(ctx, SystemActor(), r, RequestFailed, "runner", res, cmp(job.Error, "the runner's job "+cmp(job.State, "failed")))
	}
}

// publishRequest sends a request event to the people who can decide it.
func (s *Service) publishRequest(ctx context.Context, r *Request, decision string) {
	e := requestEvent(EvRequestUpdated, r.ID, r.Ref, r.Number, r.Kind, r.State, r.userID, r.projectID, r.sessionID, r.Title, r.Agent)
	e.Data["decision"] = decision
	s.Hub.Publish(e)
	s.fanOutToEditors(ctx, e, r.projectID)
}

func (s *Service) publishRequestID(ctx context.Context, id int64, decision string) {
	list, err := s.loadRequests(ctx, SystemActor(), false, `r.id = ?`, id)
	if err != nil || len(list) == 0 {
		return
	}
	s.publishRequest(ctx, list[0], decision)
}

// fanOutToEditors repeats a request event to editors of its project (the
// event itself goes to the person it is for): they may decide it too.
func (s *Service) fanOutToEditors(ctx context.Context, e *Event, project sql.NullInt64) {
	if !project.Valid {
		return
	}
	ids, err := queryIDs(ctx, s.db.R, `SELECT user_id FROM project_members WHERE project_id = ? AND role IN ('editor','owner') AND user_id != ?`, project.Int64, e.UserID)
	if err != nil {
		return
	}
	for _, uid := range ids {
		c := *e
		c.UserID = uid
		s.Hub.Publish(&c)
	}
}

// notifyRequestEvent is the part of dispatch that tells people about
// requests: editors see the live event; the person it is for is notified
// (in the app, and by web push) of a new one.
func (s *Service) notifyRequestEvent(ctx context.Context, e *Event) {
	proj := sql.NullInt64{Int64: e.ProjectID, Valid: e.ProjectID != 0}
	s.fanOutToEditors(ctx, e, proj)
	if e.Kind == EvRequestUpdated {
		if state, _ := e.Data["state"].(string); state != RequestOpen {
			if ref, _ := e.Data["ref"].(string); ref != "" {
				res, err := s.db.W.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE kind = ? AND url = ? AND read_at IS NULL`,
					s.nowMS(), NotifyRequest, "/requests/"+ref)
				if n, _ := res.RowsAffected(); err == nil && n > 0 {
					s.Hub.Publish(&Event{Kind: EvNotification, UserID: dataInt(e, "for"), Data: map[string]any{"read": true}})
				}
			}
		}
		return
	}
	if e.Kind != EvRequestCreated {
		return
	}
	user := dataInt(e, "for")
	ref, _ := e.Data["ref"].(string)
	title, _ := e.Data["title"].(string)
	agent, _ := e.Data["agent"].(string)
	kind, _ := e.Data["kind"].(string)
	code, _ := e.Data["code"].(string)
	s.notifyRequestCreated(ctx, user, ref, requestNumber(code), title, agent, kind)
}

// notifyRequestCreated tells the person a request is for about it.
func (s *Service) notifyRequestCreated(ctx context.Context, user int64, ref string, number int, title, agent, kind string) {
	code := fmt.Sprintf("R-%d", number)
	lead := map[string]string{
		RequestQuestion: "asks", RequestChange: "wants to change files", RequestCommand: "wants a command run",
		RequestPermission: "was refused", RequestAction: "needs you to do something",
	}[kind]
	who := cmp(shortAgent(agent), "An agent")
	s.notifyURL(ctx, user, nil, NotifyRequest, code+" "+title, who+" "+lead, "/requests/"+ref)
}

func requestNumber(code string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(code, "R-"))
	return n
}

// shortAgent is an agent identity as people say it: the model or tool.
func shortAgent(a string) string {
	if _, after, ok := strings.Cut(a, "/"); ok {
		return after
	}
	return a
}
