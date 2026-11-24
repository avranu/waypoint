package core

// Field shapes follow Forgejo 16 (services/convert/action.go, convert.go, v16.0.5); other versions may differ.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ActionRun is one workflow run.
type ActionRun struct {
	ID         int64     `json:"id"`
	Index      int64     `json:"index_in_repo"`
	Title      string    `json:"title"`
	WorkflowID string    `json:"workflow_id"`
	PrettyRef  string    `json:"prettyref"`
	CommitSHA  string    `json:"commit_sha"`
	Event      string    `json:"trigger_event"`
	Status     string    `json:"status"`
	Created    time.Time `json:"created"`
	Started    time.Time `json:"started"`
	Stopped    time.Time `json:"stopped"`
	HTMLURL    string    `json:"html_url"`
	Draft      runDraft  `json:"event_payload"`
}

// runDraft says a run was started for a draft pull request (WP-849). A run's
// title is its commit's subject, which says nothing of the kind. The event
// payload has the pull request's title when the event fired, the same value
// .forgejo/actions/tier goes by, and it's tested the way that step does: a
// title the forge calls a draft by another prefix (draftTitle) still ran in
// full. The payload is the whole webhook body, so only this answer is kept
// of it. A payload that can't be read is a full run,
// as is a push's, which has no pull request.
type runDraft bool

func (d *runDraft) UnmarshalJSON(b []byte) error {
	var raw string
	if json.Unmarshal(b, &raw) != nil || raw == "" {
		return nil
	}
	var p struct {
		PullRequest struct {
			Title string `json:"title"`
		} `json:"pull_request"`
	}
	if json.Unmarshal([]byte(raw), &p) == nil {
		*d = runDraft(strings.HasPrefix(p.PullRequest.Title, "WIP:"))
	}
	return nil
}

// ActionJob is one job of a run. Needs are the YAML keys of the jobs it
// waits for; Forgejo doesn't say which key is which job, so they are
// matched by name (a job's name defaults to its key).
type ActionJob struct {
	ID     int64    `json:"id"`
	RunID  int64    `json:"run_id"`
	Name   string   `json:"name"`
	Needs  []string `json:"needs"`
	RunsOn []string `json:"runs_on"`
	TaskID int64    `json:"task_id"`
	Status string   `json:"status"`
}

// ActionTask is one attempt of a job on a runner. Forgejo has no stop
// time in the API: a finished task's updated_at is when it ended.
type ActionTask struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"` // the job's
	RunNumber  int64     `json:"run_number"`
	WorkflowID string    `json:"workflow_id"`
	Event      string    `json:"event"`
	Branch     string    `json:"head_branch"`
	SHA        string    `json:"head_sha"`
	Title      string    `json:"display_title"`
	Status     string    `json:"status"`
	URL        string    `json:"url"`
	Created    time.Time `json:"created_at"`
	Updated    time.Time `json:"updated_at"`
	Started    time.Time `json:"run_started_at"`
}

// ActionRunner is a runner Forgejo knows; Status is offline, idle or active.
type ActionRunner struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Labels  []string `json:"labels"`
	Version string   `json:"version"`
}

// ForgeActions is the optional part of a ForgeClient that reads Forgejo
// Actions (the CI board, WP-225).
type ForgeActions interface {
	// ActionRuns lists a page of a repo's runs, newest first; statuses
	// narrow it.
	ActionRuns(ctx context.Context, f *Forge, repo string, statuses []string, page, limit int) ([]ActionRun, error)
	ActionJobs(ctx context.Context, f *Forge, repo string, runID int64) ([]ActionJob, error)
	// ActionTasks lists a page of a repo's tasks, newest first.
	ActionTasks(ctx context.Context, f *Forge, repo string, statuses []string, page, limit int) ([]ActionTask, error)
	// ActionRunners lists every runner; errRunnersHidden when the token
	// isn't a site administrator's.
	ActionRunners(ctx context.Context, f *Forge) ([]ActionRunner, error)
}

// errRunnersHidden: Forgejo lists global runners to site administrators only.
var errRunnersHidden = errors.New("the forge lists runners only to a site administrator's token")

func statusQuery(statuses []string) string {
	var b strings.Builder
	for _, s := range statuses {
		b.WriteString("&status=" + url.QueryEscape(s))
	}
	return b.String()
}

func (c *httpForgeClient) ActionRuns(ctx context.Context, f *Forge, repo string, statuses []string, page, limit int) ([]ActionRun, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return nil, ErrNotFound
	}
	var res struct {
		Runs []ActionRun `json:"workflow_runs"`
	}
	err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs?page=%d&limit=%d%s", repo, page, limit, statusQuery(statuses)), nil, &res)
	return res.Runs, err
}

func (c *httpForgeClient) ActionJobs(ctx context.Context, f *Forge, repo string, runID int64) ([]ActionJob, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return nil, ErrNotFound
	}
	var jobs []ActionJob
	err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs", repo, runID), nil, &jobs)
	return jobs, err
}

func (c *httpForgeClient) ActionTasks(ctx context.Context, f *Forge, repo string, statuses []string, page, limit int) ([]ActionTask, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return nil, ErrNotFound
	}
	var res struct {
		Tasks []ActionTask `json:"workflow_runs"` // sic: Forgejo's ActionTaskResponse
	}
	err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/tasks?page=%d&limit=%d%s", repo, page, limit, statusQuery(statuses)), nil, &res)
	return res.Tasks, err
}

func (c *httpForgeClient) ActionRunners(ctx context.Context, f *Forge) ([]ActionRunner, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return nil, ErrNotFound
	}
	var out []ActionRunner
	for page := 1; page <= maxForgePages; page++ {
		var list []ActionRunner
		err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/admin/actions/runners?page=%d&limit=50", page), nil, &list)
		var fe *forgeAPIError
		if errors.As(err, &fe) && (fe.status == http.StatusUnauthorized || fe.status == http.StatusForbidden) {
			return nil, errRunnersHidden
		}
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
		if len(list) < 50 {
			break
		}
	}
	return out, nil
}
