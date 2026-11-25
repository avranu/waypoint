package core

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// ForgeJobRerun is the optional part of a ForgeClient that re-runs a head's
// failed jobs and says which, for wp pr rerun (WP-1661).
type ForgeJobRerun interface {
	// RerunJobs re-runs the failed and cancelled jobs of the runs that count
	// on sha, or only the job called only (any case) when that is set. An
	// error that is a *RerunNoMatch says no failed job has that name.
	RerunJobs(ctx context.Context, f *Forge, repo, sha, only string) (*RerunResult, error)
	CanRetryChecks(ctx context.Context, f *Forge) (bool, error)
}

// RerunResult is what a re-run asked the forge to run again. Jobs is empty
// when nothing had failed; Runs counts what GitHub and GitLab re-ran, which
// name no jobs.
type RerunResult struct {
	Jobs   []string `json:"jobs"`
	Runs   int      `json:"runs,omitempty"`
	RunURL string   `json:"run_url,omitempty"`
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
	if ok, err := c.CanRetryChecks(ctx, f); err != nil {
		return nil, err
	} else if !ok {
		return nil, errForgejoNoRerun
	}
	var runs struct {
		Runs []ActionRun `json:"workflow_runs"`
	}
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs?head_sha=%s&limit=50", repo, url.QueryEscape(sha)), nil, &runs); err != nil {
		return nil, err
	}
	counting := countingRuns(runs.Runs)
	res := &RerunResult{}
	var failed []string
	for _, r := range runs.Runs {
		if cr, ok := counting[runGroup(r)]; !ok || cr.ID != r.ID || (r.Status != "failure" && r.Status != "cancelled") {
			continue
		}
		if only != "" { // the failed names, for a name that matches none
			jobs, err := c.ActionJobs(ctx, f, repo, r.ID)
			if err != nil {
				return res, err
			}
			for _, j := range jobs {
				if j.Status == "failure" || j.Status == "cancelled" {
					failed = append(failed, j.Name)
				}
			}
			if !slices.ContainsFunc(jobs, func(j ActionJob) bool {
				return strings.EqualFold(j.Name, only) && (j.Status == "failure" || j.Status == "cancelled")
			}) {
				continue
			}
		}
		names, err := c.rerunForgejoNamed(ctx, f, repo, r.ID, only)
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
			return res, nil
		}
		slices.Sort(failed)
		if slices.ContainsFunc(failed, func(n string) bool { return strings.EqualFold(n, only) }) {
			return res, fmt.Errorf("job %q needs another failed job, which Forgejo re-runs it with: run it without --job", only)
		}
		return res, &RerunNoMatch{Job: only, Failed: slices.Compact(failed)}
	}
	return res, nil
}
