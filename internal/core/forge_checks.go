package core

// Commit statuses and checks (WP-D21): Waypoint posts its own
// `waypoint/review` status, reads the statuses and check runs on a pull
// request's head to know whether CI passed, and keeps the tail of a failed
// job's log for a bot that fixes CI.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Check states, whatever the forge calls them.
const (
	CheckPending = "pending"
	CheckSuccess = "success"
	CheckFailed  = "failed"
)

// gateContext is the commit status Waypoint's review gate posts.
const gateContext = "waypoint/review"

// CommitCheck is one status or check run on a commit.
type CommitCheck struct {
	Name        string `json:"name"` // the status context, or the check run's name
	State       string `json:"state"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
	// Source: status, check_run or job (GitLab).
	Source string `json:"source,omitempty"`
	// JobID is what the forge reads a failed job's log by (GitHub check run
	// id, GitLab job id); empty when it has none.
	JobID string `json:"job_id,omitempty"`
	// ID and Created order several statuses of one context: the newest
	// (highest ID, then latest Created, RFC 3339) is the one that counts.
	ID      int64  `json:"-"`
	Created string `json:"-"`
	// Creator is the login of the account that posted a status; "" for a
	// check run or a GitLab pipeline job, or when the forge doesn't say
	// (Forgejo Actions' own statuses). Only Waypoint's own account's waypoint/* statuses count
	// as Waypoint's (WP-416).
	Creator string `json:"-"`
}

// forgeUser is the part of a forge's user object that names it.
type forgeUser struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

func (u *forgeUser) name() string {
	if u == nil {
		return ""
	}
	return cmp(u.Login, u.Username)
}

// CommitStatusPost is a status Waypoint sets on a commit.
type CommitStatusPost struct {
	Context     string
	State       string // pending, success, failure, error
	Description string
	TargetURL   string
}

// maxLogTail is how much of a failed job's log is kept.
const maxLogTail = 64 << 10

// ForgeChecks is the optional part of a ForgeClient that sets and reads
// commit statuses and CI logs.
type ForgeChecks interface {
	SetCommitStatus(ctx context.Context, f *Forge, repo, sha string, st CommitStatusPost) error
	// CommitChecks lists the latest status per context and, on GitHub, the
	// check runs, of a commit.
	CommitChecks(ctx context.Context, f *Forge, repo, sha string) ([]CommitCheck, error)
	// FailedLogs returns the tail (64 KB) of each failed check's job log,
	// by check name; checks the forge has no log for are left out.
	FailedLogs(ctx context.Context, f *Forge, repo, sha string, failed []CommitCheck) (map[string]string, error)
}

// checkState maps a forge's status, conclusion or job state to
// pending/success/failed.
func checkState(s string) string {
	switch strings.ToLower(s) {
	case "success", "neutral", "skipped", "passed", "warning", "manual", "ok":
		return CheckSuccess
	case "failure", "error", "failed", "canceled", "cancelled", "timed_out", "action_required", "stale", "startup_failure":
		return CheckFailed
	}
	return CheckPending // pending, running, queued, in_progress, created, waiting, …
}

func (c *httpForgeClient) SetCommitStatus(ctx context.Context, f *Forge, repo, sha string, st CommitStatusPost) error {
	if f.Kind == "gitlab" {
		state := map[string]string{"pending": "pending", "success": "success", "failure": "failed", "error": "failed"}[st.State]
		return c.do(ctx, f, http.MethodPost, fmt.Sprintf("%s/statuses/%s", glProject(repo), url.PathEscape(sha)), map[string]string{
			"state": state, "name": st.Context, "description": st.Description, "target_url": st.TargetURL}, nil)
	}
	return c.do(ctx, f, http.MethodPost, fmt.Sprintf("/repos/%s/statuses/%s", repo, url.PathEscape(sha)), map[string]string{
		"state": st.State, "context": st.Context, "description": st.Description, "target_url": st.TargetURL}, nil)
}

// newestPerContext keeps one check per name and source: the newest by ID,
// then Created, then position in the list. Forges can list every status of
// a context (Forgejo, GitLab retries), and an older "error" must not
// outvote a later "success" (WP-116). Order of first appearance is kept.
// Which run an Actions job's check comes from is settled before this, by
// the run that counts (fgRunChecks, WP-ADR48).
func newestPerContext(in []CommitCheck) []CommitCheck {
	type key struct{ name, source string }
	at := map[key]int{}
	out := make([]CommitCheck, 0, len(in))
	for _, c := range in {
		k := key{c.Name, c.Source}
		i, dup := at[k]
		if !dup {
			at[k] = len(out)
			out = append(out, c)
			continue
		}
		if o := out[i]; c.ID < o.ID || (c.ID == o.ID && c.Created < o.Created) {
			continue
		}
		out[i] = c
	}
	return out
}

// skippedCheck says whether a status is a job Actions skipped: Forgejo and
// Gitea post it as a success described "Has been skipped".
func skippedCheck(c CommitCheck) bool {
	return c.State == CheckSuccess && strings.HasPrefix(c.Description, "Has been skipped")
}

func (c *httpForgeClient) CommitChecks(ctx context.Context, f *Forge, repo, sha string) ([]CommitCheck, error) {
	all, err := c.commitChecks(ctx, f, repo, sha)
	return newestPerContext(all), err
}

// statusHistory is every job's newest status on a Forgejo or Gitea commit:
// a job (its target URL) keeps its newest, so a run's "Blocked by required
// conditions" doesn't outlive its "Has been skipped". The combined status
// lists only each context's newest status, which may be another run's
// (WP-424: a pull request merged on a failed full run hidden by an edit's skip).
func (c *httpForgeClient) statusHistory(ctx context.Context, f *Forge, repo, sha string) ([]CommitCheck, error) {
	type job struct{ context, url string }
	at := map[job]int{}
	var out []CommitCheck
	const per = 50
	for page := 1; page <= maxForgePages+1; page++ { // one past the cap: a full history ends on an empty page
		//wplint:ignore statuscreator fgRunChecks only follows these to a job in the run that counts, and the job's own state decides (its job-index check: WP-1339)
		var list []struct {
			ID          int64  `json:"id"`
			Context     string `json:"context"`
			CreatedAt   string `json:"created_at"`
			Status      string `json:"status"`
			State       string `json:"state"`
			TargetURL   string `json:"target_url"`
			Description string `json:"description"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/statuses?limit=%d&page=%d", repo, url.PathEscape(sha), per, page), nil, &list); err != nil {
			return nil, err
		}
		for _, s := range list {
			cc := CommitCheck{Name: s.Context, State: checkState(cmp(s.Status, s.State)), URL: s.TargetURL, Description: s.Description, Source: "status", ID: s.ID, Created: s.CreatedAt}
			k := job{s.Context, s.TargetURL}
			i, dup := at[k]
			switch {
			case !dup:
				at[k] = len(out)
				out = append(out, cc)
			case cc.ID > out[i].ID:
				out[i] = cc
			}
		}
		if len(list) == 0 {
			return out, nil // only an empty page ends it: a forge may cap pages below per
		}
	}
	return nil, fmt.Errorf("status history of %s@%s has more than %d pages", repo, sha, maxForgePages)
}

func (c *httpForgeClient) commitChecks(ctx context.Context, f *Forge, repo, sha string) ([]CommitCheck, error) {
	var out []CommitCheck
	switch f.Kind {
	case "gitlab":
		var list []struct {
			ID           int64      `json:"id"`
			Name         string     `json:"name"`
			Status       string     `json:"status"`
			TargetURL    string     `json:"target_url"`
			Description  string     `json:"description"`
			AllowFailure bool       `json:"allow_failure"`
			PipelineID   int64      `json:"pipeline_id"`
			Author       *forgeUser `json:"author"`
		}
		pipelines := map[int64]int64{}
		for page := 1; page <= maxForgePages; page++ {
			list = list[:0]
			if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/repository/commits/%s/statuses?per_page=100&page=%d", glProject(repo), url.PathEscape(sha), page), nil, &list); err != nil {
				return nil, err
			}
			for _, s := range list {
				st := checkState(s.Status)
				if st == CheckFailed && s.AllowFailure {
					st = CheckSuccess
				}
				out = append(out, CommitCheck{Name: s.Name, State: st, URL: s.TargetURL, Description: s.Description, Source: "job", JobID: strconv.FormatInt(s.ID, 10), ID: s.ID, Creator: s.Author.name()})
				pipelines[s.ID] = s.PipelineID
			}
			if len(list) < 100 {
				break
			}
		}
		c.gitlabJobAuthors(ctx, f, repo, out, pipelines)
		return out, nil
	}
	// Forgejo, Gitea and GitHub: the combined status lists the latest status
	// of each context.
	size, per := "limit", 50
	if f.Kind == "github" {
		size, per = "per_page", 100
	}
	for page := 1; page <= maxForgePages; page++ {
		var res struct {
			Statuses []struct {
				ID          int64      `json:"id"`
				Context     string     `json:"context"`
				CreatedAt   string     `json:"created_at"`
				State       string     `json:"state"`  // GitHub
				Status      string     `json:"status"` // Forgejo
				TargetURL   string     `json:"target_url"`
				Description string     `json:"description"`
				Creator     *forgeUser `json:"creator"` // Forgejo; GitHub's combined status has none
			} `json:"statuses"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/status?%s=%d&page=%d", repo, url.PathEscape(sha), size, per, page), nil, &res); err != nil {
			return nil, err
		}
		for _, s := range res.Statuses {
			out = append(out, CommitCheck{Name: s.Context, State: checkState(cmp(s.State, s.Status)), URL: s.TargetURL, Description: s.Description, Source: "status", ID: s.ID, Created: s.CreatedAt, Creator: s.Creator.name()})
		}
		if len(res.Statuses) < per {
			break
		}
	}
	if f.Kind != "github" {
		return c.fgRunChecks(ctx, f, repo, sha, out)
	}
	c.githubStatusCreators(ctx, f, repo, sha, out)
	for page := 1; page <= maxForgePages; page++ {
		var res struct {
			CheckRuns []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				HTMLURL    string `json:"html_url"`
				Output     struct {
					Title string `json:"title"`
				} `json:"output"`
			} `json:"check_runs"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100&page=%d", repo, url.PathEscape(sha), page), nil, &res); err != nil {
			return nil, err
		}
		for _, r := range res.CheckRuns {
			st := CheckPending
			if r.Status == "completed" {
				st = checkState(r.Conclusion)
			}
			out = append(out, CommitCheck{Name: r.Name, State: st, URL: r.HTMLURL, Description: r.Output.Title, Source: "check_run", JobID: strconv.FormatInt(r.ID, 10)})
		}
		if len(res.CheckRuns) < 100 {
			break
		}
	}
	return out, nil
}

// githubStatusCreators fills in who posted each waypoint/* status: GitHub's
// combined status doesn't say, its list of a commit's statuses (newest
// first) does. Only those are looked up, by id; a failed read leaves them
// unknown, which counts as not Waypoint's (WP-416).
func (c *httpForgeClient) githubStatusCreators(ctx context.Context, f *Forge, repo, sha string, checks []CommitCheck) {
	want := map[int64]int{}
	for i, ch := range checks {
		if ownCheck(ch.Name) && ch.ID != 0 {
			want[ch.ID] = i
		}
	}
	for page := 1; page <= maxForgePages && len(want) > 0; page++ {
		var list []struct {
			ID      int64      `json:"id"`
			Creator *forgeUser `json:"creator"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/statuses?per_page=100&page=%d", repo, url.PathEscape(sha), page), nil, &list); err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "read the creators of waypoint/* statuses", "repo", repo, "sha", sha, "err", err)
			}
			return
		}
		for _, st := range list {
			if i, ok := want[st.ID]; ok {
				checks[i].Creator = st.Creator.name()
				delete(want, st.ID)
			}
		}
		if len(list) < 100 {
			return
		}
	}
}

// gitlabJobAuthors clears the author of each waypoint/* entry that is a
// pipeline job (WP-416). GitLab lists a pipeline's jobs among a commit's
// statuses, and a job's author is the account that triggered the pipeline,
// not whoever wrote the job: a job named waypoint/review in a pipeline that
// Waypoint's own push started would pass for Waypoint's. Only a status
// posted through the statuses API is anyone's own, so each waypoint/* entry
// with an author is looked up: a job (the jobs API knows it) or a trigger
// job (among its pipeline's bridges) has no author. One that can't be read
// counts as not Waypoint's, as on GitHub (githubStatusCreators).
func (c *httpForgeClient) gitlabJobAuthors(ctx context.Context, f *Forge, repo string, checks []CommitCheck, pipelines map[int64]int64) {
	for i := range checks {
		ch := &checks[i]
		if !ownCheck(ch.Name) || ch.Creator == "" || ch.ID == 0 {
			continue
		}
		job, err := c.gitlabIsJob(ctx, f, repo, ch.ID, pipelines[ch.ID])
		if err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "tell a waypoint/* commit status from a pipeline job", "repo", repo, "id", ch.ID, "err", err)
		}
		if job || err != nil {
			ch.Creator = ""
		}
	}
}

func (c *httpForgeClient) gitlabIsJob(ctx context.Context, f *Forge, repo string, id, pipeline int64) (bool, error) {
	key := fmt.Sprintf("%d|%d", f.ID, id)
	if job, ok := c.glJobs.get(key); ok {
		return job, nil
	}
	var ae *forgeAPIError
	err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/jobs/%d", glProject(repo), id), nil, &struct{}{})
	switch {
	case err == nil:
		c.glJobs.put(key, true)
		return true, nil
	case !errors.As(err, &ae) || ae.status != http.StatusNotFound:
		return false, err
	}
	// Not a job: a trigger job (bridge) is looked up in its pipeline.
	if pipeline != 0 {
		for page := 1; page <= maxForgePages; page++ {
			var list []struct {
				ID int64 `json:"id"`
			}
			if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("%s/pipelines/%d/bridges?per_page=100&page=%d", glProject(repo), pipeline, page), nil, &list); err != nil {
				return false, err
			}
			for _, b := range list {
				if b.ID == id {
					c.glJobs.put(key, true)
					return true, nil
				}
			}
			if len(list) < 100 {
				break
			}
		}
	}
	c.glJobs.put(key, false)
	return false, nil
}

// glJobCache is a bounded memory of which GitLab statuses are jobs.
type glJobCache struct {
	mu sync.Mutex
	m  map[string]bool
}

func (g *glJobCache) get(key string) (job, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	job, ok = g.m[key]
	return job, ok
}

func (g *glJobCache) put(key string, job bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil || len(g.m) >= 4096 {
		g.m = map[string]bool{}
	}
	g.m[key] = job
}

// tail reads a plain-text answer and keeps its last keep bytes (logs are
// long and their end says what failed). Reads at most 32 MB.
func (c *httpForgeClient) tail(ctx context.Context, f *Forge, path string, keep int) (string, error) {
	return c.tailMarked(ctx, f, path, keep)
}

// tailMarked is tail that also keeps the first line matching each of marks
// when the cut drops it, so evidence like a step's failure line (WP-424) or
// go test's timeout panic (WP-923) outlives a long tail of later output.
func (c *httpForgeClient) tailMarked(ctx context.Context, f *Forge, path string, keep int, marks ...*lazyre.Regexp) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase(f)+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	if f.token != "" {
		switch f.Kind {
		case "github":
			req.Header.Set("Authorization", "Bearer "+f.token)
		case "gitlab":
			req.Header.Set("PRIVATE-TOKEN", f.token)
		default:
			req.Header.Set("Authorization", "token "+f.token)
		}
	}
	resp, err := c.client(ctx).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", &forgeAPIError{method: http.MethodGet, path: path, status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	buf := make([]byte, 0, keep*2)
	chunk := make([]byte, 32<<10)
	var total int64
	marked := make([]string, len(marks))
	for total < 32<<20 {
		n, err := resp.Body.Read(chunk)
		from := max(0, len(buf)-512) // what was read, and a line split by the read before it
		buf = append(buf, chunk[:n]...)
		total += int64(n)
		for i, mark := range marks {
			if marked[i] == "" {
				marked[i] = string(mark.Find(buf[from:]))
			}
		}
		if len(buf) > keep*2 {
			buf = append(buf[:0], buf[len(buf)-keep:]...)
		}
		if err != nil {
			break
		}
	}
	if len(buf) > keep {
		buf = buf[len(buf)-keep:]
	}
	s := strings.ToValidUTF8(string(buf), "")
	if total > int64(keep) {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:] // start on a whole line
		}
		s = "… (earlier output cut)\n" + s
		for i, mark := range marks {
			if marked[i] != "" && !mark.MatchString(s) {
				s += "\n" + marked[i] + " (kept from the cut output)\n"
			}
		}
	}
	return s, nil
}

// maxJobLog is how much of a job's log `wp pr log` reads: its last 8 MB.
const maxJobLog = 8 << 20

// ForgeJobLogs is the optional part of a ForgeClient that reads one
// check's whole job log (wp pr log, WP-132).
type ForgeJobLogs interface {
	JobLog(ctx context.Context, f *Forge, repo, sha string, check CommitCheck) (string, error)
}

func (c *httpForgeClient) FailedLogs(ctx context.Context, f *Forge, repo, sha string, failed []CommitCheck) (map[string]string, error) {
	out := map[string]string{}
	runs := &fgRuns{}
	for _, ch := range failed {
		log, err := c.jobLog(ctx, f, repo, sha, ch, maxLogTail, runs)
		if err == nil {
			out[ch.Name] = log
		} else if ctx.Err() != nil {
			return out, ctx.Err()
		}
	}
	return out, nil
}

func (c *httpForgeClient) JobLog(ctx context.Context, f *Forge, repo, sha string, check CommitCheck) (string, error) {
	return c.jobLog(ctx, f, repo, sha, check, maxJobLog, &fgRuns{})
}

// errNoJobLog: the check isn't a CI job the forge keeps a log of (another
// app's status, a check run that isn't an Actions job).
var errNoJobLog = fmt.Errorf("%w: this check has no job log on the forge", ErrNotFound)

// jobLog reads the last keep bytes of a check's job log.
func (c *httpForgeClient) jobLog(ctx context.Context, f *Forge, repo, sha string, ch CommitCheck, keep int, runs *fgRuns) (string, error) {
	switch f.Kind {
	case "github":
		// Only Actions jobs have logs; other apps' check runs answer 404.
		if ch.Source != "check_run" || ch.JobID == "" {
			return "", errNoJobLog
		}
		return c.tailMarked(ctx, f, fmt.Sprintf("/repos/%s/actions/jobs/%s/logs", repo, url.PathEscape(ch.JobID)), keep, logMarks...)
	case "gitlab":
		if ch.JobID == "" {
			return "", errNoJobLog
		}
		return c.tailMarked(ctx, f, fmt.Sprintf("%s/jobs/%s/trace", glProject(repo), url.PathEscape(ch.JobID)), keep, logMarks...)
	}
	repo, id, err := c.fgJobID(ctx, f, repo, sha, ch, runs)
	if err != nil {
		return "", err
	}
	return c.tailMarked(ctx, f, fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", repo, id), keep, logMarks...)
}

// fgActionsURL is where a Forgejo Actions status points: the run's NUMBER
// in the repo and the job's position in the run, not their ids.
var fgActionsURL = lazyre.New(`/([\w.-]+/[\w.-]+)/actions/runs/(\d+)(?:/jobs/(\d+))?`)

// fgRuns caches a commit's runs while one call reads several logs.
type fgRuns struct {
	list []fgRun
	read bool
}

type fgRun struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

type fgJob struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// fgJobID finds the id of the Forgejo Actions job a status is about. Its
// target URL names run number N and job index J: the run is the one with
// that number, the job the J-th of its jobs by id (as
// infrastructure/bin/opslib/forgejo.py's log_for_target_url does; checked
// against Forgejo 16 on acme/waypoint run 189). A status without such a URL
// is matched by name among the commit's failed runs: "ci / backend
// (pull_request)" is job "backend".
func (c *httpForgeClient) fgJobID(ctx context.Context, f *Forge, repo, sha string, ch CommitCheck, runs *fgRuns) (string, int64, error) {
	if m := fgActionsURL.FindStringSubmatch(ch.URL); m != nil {
		// Only this repo's runs: a status may point anywhere, and the
		// token may read repos the pull request's viewers can't.
		if !strings.EqualFold(m[1], repo) {
			return "", 0, fmt.Errorf("%w: the check's job is in %s, not %s", ErrNotFound, m[1], repo)
		}
		repo = m[1] // as the forge spells it
		index, _ := strconv.Atoi(m[3])
		var list struct {
			WorkflowRuns []fgRun `json:"workflow_runs"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs?run_number=%s&limit=1", repo, m[2]), nil, &list); err != nil {
			return "", 0, err
		}
		if len(list.WorkflowRuns) == 0 {
			return "", 0, fmt.Errorf("%w: no Actions run number %s", ErrNotFound, m[2])
		}
		var jobs []fgJob
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs", repo, list.WorkflowRuns[0].ID), nil, &jobs); err != nil {
			return "", 0, err
		}
		slices.SortFunc(jobs, func(a, b fgJob) int { return cmpInt64(a.ID, b.ID) })
		if index >= len(jobs) {
			return "", 0, fmt.Errorf("%w: run %s has %d jobs, no job %d", ErrNotFound, m[2], len(jobs), index)
		}
		return repo, jobs[index].ID, nil
	}
	if !runs.read {
		var list struct {
			WorkflowRuns []fgRun `json:"workflow_runs"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs?head_sha=%s&limit=50", repo, url.QueryEscape(sha)), nil, &list); err != nil {
			return "", 0, err
		}
		runs.list, runs.read = list.WorkflowRuns, true
	}
	for _, run := range runs.list {
		if checkState(run.Status) != CheckFailed {
			continue
		}
		var jobs []fgJob
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs", repo, run.ID), nil, &jobs); err != nil {
			return "", 0, err
		}
		for _, j := range jobs {
			if checkState(j.Status) == CheckFailed && (strings.Contains(ch.Name, " / "+j.Name+" (") || ch.Name == j.Name) {
				return repo, j.ID, nil
			}
		}
	}
	return "", 0, errNoJobLog
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
