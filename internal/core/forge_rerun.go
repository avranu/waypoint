package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
)

// ForgeJobRerun is the optional part of a ForgeClient that re-runs a head's
// failed jobs and says which, for wp pr rerun (WP-1661).
type ForgeJobRerun interface {
	// RerunJobs re-runs the failed and cancelled jobs of the run that counts
	// for each workflow on sha, or only the job called only (any case) when
	// that is set. GitHub and GitLab take no name, and re-run what
	// RetryChecks does there (their failed runs, whole). An error that is a
	// *RerunNoMatch says no failed job has that name; one that is
	// errTooManyRuns comes with what was re-run among the newest runs, older
	// ones not read. The caller asks CanRetryChecks first; RerunJobs doesn't.
	RerunJobs(ctx context.Context, f *Forge, repo, sha, only string) (*RerunResult, error)
	CanRetryChecks(ctx context.Context, f *Forge) (bool, error)
}

// RerunResult is what a re-run asked the forge to run again. Jobs is empty
// when nothing had failed; Runs counts what GitHub and GitLab re-ran, which
// name no jobs. Active counts the runs that count on the head and haven't
// finished (Forgejo): a failed job in one can be re-run when it ends.
type RerunResult struct {
	Jobs   []string `json:"jobs"`
	Runs   int      `json:"runs,omitempty"`
	RunURL string   `json:"run_url,omitempty"`
	Active int      `json:"active,omitempty"`
}

// RerunNoMatch: no failed job of the head is called Job. Failed lists the
// ones that are.
type RerunNoMatch struct {
	Job    string
	Failed []string
}

func (e *RerunNoMatch) Error() string {
	return fmt.Sprintf("no failed job is called %q", e.Job)
}

func (c *httpForgeClient) RerunJobs(ctx context.Context, f *Forge, repo, sha, only string) (*RerunResult, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		if only != "" {
			return nil, fmt.Errorf("%w: this forge re-runs a head's failed jobs together, not one by name", ErrUnsupported)
		}
		n, err := c.RetryChecks(ctx, f, repo, sha)
		return &RerunResult{Runs: n}, err
	}
	redo, active, unread := c.forgejoRunsToRerun(ctx, f, repo, sha)
	if unread != nil && !errors.Is(unread, errTooManyRuns) {
		return nil, unread
	}
	res := &RerunResult{Active: active}
	var failed []string
	for _, r := range redo {
		var jobs []ActionJob // read here for a named job, and picked from as read
		if only != "" {      // the failed names, for a name that matches none
			var err error
			if jobs, err = c.ActionJobs(ctx, f, repo, r.ID); err != nil {
				return res, err
			}
			for _, j := range jobs {
				if actionJobFailed(j) {
					failed = append(failed, j.Name)
				}
			}
			if !slices.ContainsFunc(jobs, func(j ActionJob) bool { return strings.EqualFold(j.Name, only) && actionJobFailed(j) }) {
				continue
			}
		}
		names, err := c.rerunForgejoNamed(ctx, f, repo, r.ID, only, jobs)
		res.Jobs = append(res.Jobs, names...)
		if len(names) > 0 {
			res.RunURL = r.HTMLURL
		}
		if err != nil {
			return res, err
		}
	}
	if only != "" && len(res.Jobs) == 0 {
		if len(failed) == 0 {
			return res, unread
		}
		slices.Sort(failed)
		if slices.ContainsFunc(failed, func(n string) bool { return strings.EqualFold(n, only) }) {
			return res, kernel.Invalid("job %q needs another failed job, which Forgejo re-runs it with: run it without --job", only)
		}
		return res, &RerunNoMatch{Job: only, Failed: slices.Compact(failed)}
	}
	return res, unread
}

// forgejoRunsToRerun is the runs of sha (every page) that a re-run runs
// again (rerunnableRuns), and how many that count haven't finished. On a
// commit with more runs than are read, the newest are judged and the error
// says older ones weren't (errTooManyRuns): a failed run that counts among
// them would not be re-run, so the answer is not "nothing failed".
func (c *httpForgeClient) forgejoRunsToRerun(ctx context.Context, f *Forge, repo, sha string) (redo []ActionRun, active int, err error) {
	runs, err := c.ActionRunsAt(ctx, f, repo, sha)
	if err != nil && !errors.Is(err, errTooManyRuns) {
		return nil, 0, err
	}
	redo, active = rerunnableRuns(runs)
	return redo, active, err
}

// rerunnableRuns is the runs that count for their workflow (countingRuns,
// WP-ADR48) and ended failed or cancelled, in the order given, and how many
// that count haven't finished: a run a newer one replaced is never re-run
// (WP-591). A fail-fast run ends "cancelled": its first failure cancelled the
// rest, and its failed and cancelled jobs both go again. The re-run and the
// evidence kept before it (keepFailedEvidence) both choose with it.
func rerunnableRuns(runs []ActionRun) (redo []ActionRun, active int) {
	counting := countingRuns(runs)
	for _, r := range runs {
		if cr, ok := counting[runGroup(r)]; !ok || cr.ID != r.ID {
			continue
		}
		switch r.Status {
		case "failure", "cancelled":
			redo = append(redo, r)
		case "success", "skipped":
		default:
			active++
		}
	}
	return redo, active
}

// actionJobFailed: a job ended failed or cancelled, which a re-run runs again.
func actionJobFailed(j ActionJob) bool { return j.Status == "failure" || j.Status == "cancelled" }
