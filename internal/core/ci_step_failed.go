package core

// A fast-failing workflow cancels its other jobs on the forge; only a job's log tells its own failure from a real stop, and check recording reads logs only for heads it still follows, so a scheduled pass reads each recent cancelled job's log once.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// stepFailedReads is how many cancelled jobs' logs a repo has read per
	// history pass (every ten minutes): a fail-fast run cancels a handful.
	stepFailedReads = 12
	// stepFailedSettle: a job's log may still be uploading just after the
	// forge marks it cancelled.
	stepFailedSettle = 2 * time.Minute
)

// forgeStepFailed is what reading a cancelled job's verdict needs: its run
// by number, the run's jobs (which attempt each holds), and whether a job's
// log shows one of its steps failed.
type forgeStepFailed interface {
	forgeRunJobs
	ActionJobStepFailed(ctx context.Context, f *Forge, repo string, jobID int64) (bool, error)
}

// forgeRunJobs finds a kept job (ci_jobs) on the forge: its run by number,
// then the run's jobs.
type forgeRunJobs interface {
	ActionRunNumber(ctx context.Context, f *Forge, repo string, n int64) (*ActionRun, error)
	ActionJobs(ctx context.Context, f *Forge, repo string, runID int64) ([]ActionJob, error)
}

// runJobs are the jobs of a repo's runs by number, each run listed once
// per pass.
type runJobs struct {
	cl   forgeRunJobs
	f    *Forge
	repo string
	runs map[int64][]ActionJob
}

func newRunJobs(cl forgeRunJobs, f *Forge, repo string) *runJobs {
	return &runJobs{cl: cl, f: f, repo: repo, runs: map[int64][]ActionJob{}}
}

// job is the job that ran task in run, named name. ok is false when the
// run or the job is gone, or the job's newest attempt is another task: the
// forge serves only the newest attempt's log, so a task a re-run replaced
// can't be read. err is the forge failing (the next pass tries again).
func (r *runJobs) job(ctx context.Context, run, task int64, name string) (ActionJob, bool, error) {
	jobs, seen := r.runs[run]
	if !seen {
		ar, err := r.cl.ActionRunNumber(ctx, r.f, r.repo, run)
		if err == nil {
			jobs, err = r.cl.ActionJobs(ctx, r.f, r.repo, ar.ID)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return ActionJob{}, false, err
		}
		r.runs[run] = jobs
	}
	for _, j := range jobs {
		if j.TaskID == task && strings.EqualFold(j.Name, name) {
			return j, true, nil
		}
	}
	return ActionJob{}, false, nil
}

// ActionJobStepFailed reads the tail of a job's log, keeping the step
// failure line however far back it is, and says whether it has one.
func (c *httpForgeClient) ActionJobStepFailed(ctx context.Context, f *Forge, repo string, jobID int64) (bool, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return false, ErrNotFound
	}
	log, err := c.tailMarked(ctx, f, fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", repo, jobID), maxLogTail, logMarks...)
	if err != nil {
		return false, err
	}
	return stepFailedLog(log), nil
}

// readStepFailed reads the verdicts of a repo's newest unread cancelled
// jobs of the doctor's window, at most stepFailedReads logs.
func (s *Service) readStepFailed(ctx context.Context, f *Forge, repo string) error {
	cl, ok := s.Forges.(forgeStepFailed)
	if !ok || f.token == "" {
		return nil
	}
	now := s.now()
	rows, err := s.db.R.QueryContext(ctx, `SELECT task_id, run_number, name FROM ci_jobs
		WHERE forge_id = ? AND repo = ? AND status = 'cancelled' AND step_failed IS NULL AND ended_at >= ? AND ended_at < ?
		ORDER BY ended_at DESC LIMIT ?`,
		f.ID, repo, now.Add(-2*ciHistoryWindow).UnixMilli(), now.Add(-stepFailedSettle).UnixMilli(), stepFailedReads)
	if err != nil {
		return err
	}
	type unread struct {
		task, run int64
		name      string
	}
	var list []unread
	for rows.Next() {
		var u unread
		if rows.Scan(&u.task, &u.run, &u.name) == nil {
			list = append(list, u)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	jobs := newRunJobs(cl, f, repo)
	for _, u := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j, ok, err := jobs.job(ctx, u.run, u.task, u.name)
		if err != nil {
			return err
		}
		verdict := -1
		if ok {
			failed, err := cl.ActionJobStepFailed(ctx, f, repo, j.ID)
			switch {
			case err == nil && failed:
				verdict = 1
			case err == nil:
				verdict = 0
			case !errors.Is(err, ErrNotFound):
				return err
			}
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE ci_jobs SET step_failed = ? WHERE forge_id = ? AND repo = ? AND task_id = ?`, verdict, f.ID, repo, u.task); err != nil {
			return err
		}
	}
	return nil
}

// stepFailedJobs adds to self the jobs of the window whose own log, read
// for ci_jobs, shows a step failed before the forge called them cancelled.
func (s *Service) stepFailedJobs(ctx context.Context, since time.Time, self map[string]bool) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, repo, sha, name FROM ci_jobs WHERE step_failed = 1 AND ended_at >= ?`, since.UnixMilli())
	if err != nil {
		s.log.Debug("read cancelled jobs' verdicts", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var forge int64
		var repo, sha, name string
		if rows.Scan(&forge, &repo, &sha, &name) == nil {
			self[selfStoppedKey(forge, repo, sha, name)] = true
		}
	}
}
