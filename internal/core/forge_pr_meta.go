package core

// Labels and reviewers on a pull request, for automations (WP-D18): the
// pr_label and request_reviewers actions. GitHub, Forgejo and GitLab differ
// in what they take (names or ids), so each does what its API needs.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// ForgePRMeta is the optional part of a ForgeClient that changes a pull
// request's labels and asks people to review it.
type ForgePRMeta interface {
	// ChangePRLabels adds and removes labels by name; a label that doesn't
	// exist yet is created, one that isn't on the pull request is ignored.
	ChangePRLabels(ctx context.Context, f *Forge, repo string, number int, add, remove []string) error
	// RequestReviewers asks users (forge logins) to review.
	RequestReviewers(ctx context.Context, f *Forge, repo string, number int, users []string) error
}

// defaultLabelColor is the colour of a label Waypoint creates (Forgejo
// needs one).
const defaultLabelColor = "ededed"

// madeLabels are the labels Waypoint makes with a colour and description of
// their own; any other gets defaultLabelColor and no description.
var madeLabels = map[string]struct{ color, description string }{
	// infrastructure's bin/ci-urgent.py makes it the same way.
	CIUrgentLabel: {"#d93f0b", "CI runs on the urgent lane, ahead of the queue (infrastructure docs/runbooks/forgejo-runner.md)"},
}

// fjLabel is a label as Forgejo and Gitea list it.
type fjLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func fjLabelNamed(list []fjLabel, name string) *fjLabel {
	for i := range list {
		if strings.EqualFold(list[i].Name, name) {
			return &list[i]
		}
	}
	return nil
}

// fjLabelIDs are the ids of the repo's labels by these names, making the
// ones it doesn't have yet (Forgejo and Gitea take ids, not names).
func (c *httpForgeClient) fjLabelIDs(ctx context.Context, f *Forge, repo string, names []string) ([]int64, error) {
	var repoLabels []fjLabel
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/labels?limit=200", repo), nil, &repoLabels); err != nil {
		return nil, err
	}
	var ids []int64
	for _, name := range names {
		l := fjLabelNamed(repoLabels, name)
		if l == nil {
			body := map[string]string{"name": name, "color": defaultLabelColor}
			if m, ok := madeLabels[name]; ok {
				body["color"], body["description"] = m.color, m.description
			}
			var made fjLabel
			if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/labels", repo), body, &made); err != nil {
				return nil, fmt.Errorf("make the label %q: %w", name, err)
			}
			l = &made
		}
		ids = append(ids, l.ID)
	}
	return ids, nil
}

func (c *httpForgeClient) ChangePRLabels(ctx context.Context, f *Forge, repo string, number int, add, remove []string) error {
	switch f.Kind {
	case "gitlab":
		body := map[string]string{}
		if len(add) > 0 {
			body["add_labels"] = strings.Join(add, ",")
		}
		if len(remove) > 0 {
			body["remove_labels"] = strings.Join(remove, ",")
		}
		return c.do(ctx, f, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d", glProject(repo), number), body, nil)
	case "github":
		if len(add) > 0 {
			if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number), map[string]any{"labels": add}, nil); err != nil {
				return err
			}
		}
		for _, l := range remove {
			err := c.do(ctx, f, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, url.PathEscape(l)), nil, nil)
			if err != nil && !isStatus(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
	// Forgejo and Gitea take label ids.
	if len(add) > 0 {
		ids, err := c.fjLabelIDs(ctx, f, repo, add)
		if err != nil {
			return err
		}
		if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number), map[string]any{"labels": ids}, nil); err != nil {
			return err
		}
	}
	if len(remove) > 0 {
		var on []fjLabel
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number), nil, &on); err != nil {
			return err
		}
		for _, name := range remove {
			if l := fjLabelNamed(on, name); l != nil {
				if err := c.do(ctx, f, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/%d/labels/%d", repo, number, l.ID), nil, nil); err != nil && !isStatus(err, http.StatusNotFound) {
					return err
				}
			}
		}
	}
	return nil
}

func (c *httpForgeClient) RequestReviewers(ctx context.Context, f *Forge, repo string, number int, users []string) error {
	if len(users) == 0 {
		return nil
	}
	if f.Kind != "gitlab" {
		return c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/requested_reviewers", repo, number), map[string]any{"reviewers": users}, nil)
	}
	// GitLab sets the whole list, by user id.
	var mr struct {
		Reviewers []struct {
			ID int64 `json:"id"`
		} `json:"reviewers"`
	}
	path := fmt.Sprintf("%s/merge_requests/%d", glProject(repo), number)
	if err := c.do(ctx, f, http.MethodGet, path, nil, &mr); err != nil {
		return err
	}
	ids := []int64{}
	for _, r := range mr.Reviewers {
		ids = append(ids, r.ID)
	}
	for _, name := range users {
		var found []struct {
			ID int64 `json:"id"`
		}
		if err := c.do(ctx, f, http.MethodGet, "/users?username="+url.QueryEscape(name), nil, &found); err != nil {
			return err
		}
		if len(found) == 0 {
			return fmt.Errorf("%w: GitLab has no user %q", ErrNotFound, name)
		}
		if !slices.Contains(ids, found[0].ID) {
			ids = append(ids, found[0].ID)
		}
	}
	return c.do(ctx, f, http.MethodPut, path, map[string]any{"reviewer_ids": ids}, nil)
}

// ---- re-running CI ----

// ForgeCIRetry is the optional part of a ForgeClient that re-runs the
// failed checks of a commit. Forgejo has an API for it from 17 on.
type ForgeCIRetry interface {
	// RetryChecks re-runs what failed on sha and says how many runs (or
	// Forgejo jobs) it asked to run again.
	RetryChecks(ctx context.Context, f *Forge, repo, sha string) (int, error)
	// CanRetryChecks says whether the forge can be asked to.
	CanRetryChecks(ctx context.Context, f *Forge) (bool, error)
}

// errForgejoNoRerun: Forgejo before 17 can't be asked to re-run jobs.
var errForgejoNoRerun = fmt.Errorf("%w: this Forgejo can't be asked to run CI again (its API can from Forgejo 17 on); push a commit or use its Re-run button", ErrUnsupported)

func (c *httpForgeClient) CanRetryChecks(ctx context.Context, f *Forge) (bool, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return true, nil
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, f, http.MethodGet, "/version", nil, &v); err != nil {
		return false, err
	}
	return forgejoMajor(v.Version) >= 17, nil
}

// forgejoMajor: 17 of "17.0.1+gitea-1.22.0" (0 when it isn't a number).
func forgejoMajor(v string) int {
	n := 0
	for _, r := range strings.TrimPrefix(v, "v") {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// retryForgejo re-runs the failed and cancelled jobs of the runs that count
// on sha (countingRuns, WP-ADR48): a run a newer one replaced is never re-run
// (WP-591). Forgejo re-runs a job with the jobs that need it, so a failed job
// that one of these reopened is left to that re-run.
func (c *httpForgeClient) retryForgejo(ctx context.Context, f *Forge, repo, sha string) (int, error) {
	ok, err := c.CanRetryChecks(ctx, f)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errForgejoNoRerun
	}
	var runs struct {
		Runs []ActionRun `json:"workflow_runs"`
	}
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs?head_sha=%s&limit=50", repo, url.QueryEscape(sha)), nil, &runs); err != nil {
		return 0, err
	}
	counting := countingRuns(runs.Runs)
	n := 0
	for _, r := range runs.Runs { // in the forge's order, each counting run once
		if cr, ok := counting[runGroup(r)]; !ok || cr.ID != r.ID {
			continue
		}
		// A fail-fast run ends "cancelled": its first failure cancelled the
		// rest. Its failed and cancelled jobs both go again.
		if r.Status != "failure" && r.Status != "cancelled" {
			continue
		}
		m, err := c.rerunForgejoJobs(ctx, f, repo, r.ID)
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ForgeRunRetry is the optional part of a ForgeClient that re-runs the
// failed and cancelled jobs of one Actions run (WP-551): a base branch's
// push run, without touching the other runs of its commit.
type ForgeRunRetry interface {
	RetryRun(ctx context.Context, f *Forge, repo string, runID int64) (int, error)
	CanRetryChecks(ctx context.Context, f *Forge) (bool, error)
}

func (c *httpForgeClient) RetryRun(ctx context.Context, f *Forge, repo string, runID int64) (int, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return 0, fmt.Errorf("%w: re-running one Actions run by id is Forgejo's", ErrUnsupported)
	}
	return c.rerunForgejoJobs(ctx, f, repo, runID)
}

// rerunForgejoJobs re-runs a run's failed and cancelled jobs and says how
// many it asked to run again.
func (c *httpForgeClient) rerunForgejoJobs(ctx context.Context, f *Forge, repo string, runID int64) (int, error) {
	jobs, err := c.ActionJobs(ctx, f, repo, runID)
	if err != nil {
		return 0, err
	}
	n := 0
	posted := map[int64]bool{}
	for {
		j, ok := nextRerun(jobs, posted)
		if !ok {
			break
		}
		if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/actions/jobs/%d/rerun", repo, j.ID), nil, nil); err != nil {
			return n, err
		}
		posted[j.ID] = true
		n++
		// The re-run reopened the job's dependents. needs names their
		// workflow keys, which the API doesn't give (a matrix job's name
		// is not its key), so the jobs are read again rather than
		// matched: re-running a reopened job is refused.
		if jobs, err = c.ActionJobs(ctx, f, repo, runID); err != nil {
			return n, err
		}
	}
	return n, nil
}

// nextRerun picks the next failed or cancelled job of a run to re-run,
// leaving one that needs another such job (by a name that is also its key)
// to that one's re-run.
func nextRerun(jobs []ActionJob, posted map[int64]bool) (ActionJob, bool) {
	redo := func(j ActionJob) bool { return j.Status == "failure" || j.Status == "cancelled" }
	failed := map[string]bool{}
	for _, j := range jobs {
		if redo(j) {
			failed[strings.ToLower(j.Name)] = true
		}
	}
	for _, j := range jobs {
		if redo(j) && !posted[j.ID] && !slices.ContainsFunc(j.Needs, func(need string) bool { return failed[strings.ToLower(need)] }) {
			return j, true
		}
	}
	return ActionJob{}, false
}

func (c *httpForgeClient) RetryChecks(ctx context.Context, f *Forge, repo, sha string) (int, error) {
	switch f.Kind {
	case "github":
		var runs struct {
			Runs []struct {
				ID         int64  `json:"id"`
				Conclusion string `json:"conclusion"`
			} `json:"workflow_runs"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs?head_sha=%s&per_page=50", repo, url.QueryEscape(sha)), nil, &runs); err != nil {
			return 0, err
		}
		n := 0
		for _, r := range runs.Runs {
			if r.Conclusion != "failure" && r.Conclusion != "timed_out" {
				continue
			}
			if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/actions/runs/%d/rerun-failed-jobs", repo, r.ID), nil, nil); err != nil {
				return n, err
			}
			n++
		}
		return n, nil
	case "gitlab":
		var pipes []struct {
			ID int64 `json:"id"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/pipelines?sha=%s&status=failed&per_page=20", glProject(repo), url.QueryEscape(sha)), nil, &pipes); err != nil {
			return 0, err
		}
		n := 0
		for _, p := range pipes {
			if err := c.do(ctx, f, http.MethodPost, fmt.Sprintf("%s/pipelines/%d/retry", glProject(repo), p.ID), nil, nil); err != nil {
				return n, err
			}
			n++
		}
		return n, nil
	}
	return c.retryForgejo(ctx, f, repo, sha)
}
