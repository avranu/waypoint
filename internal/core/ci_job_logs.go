package core

// Any CI job's log (WP-315): `wp ci log` reads the logs of a commit's or a
// run's Forgejo Actions jobs through Waypoint, which holds the forge token.
// `wp pr log` (ci_logs.go) only reads a pull request's head checks; a
// push to main has no pull request, so its jobs (the ones that decide
// whether a merge is released) could not be looked at by an agent at all.
// The extraction is the same: the failing part, or all of it with full.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// ForgeActionLogs is the optional part of a ForgeClient that finds a
// repo's runs by commit or number and reads a job's log by id.
type ForgeActionLogs interface {
	ForgeActions
	// ActionRunsAt lists the runs of a commit, newest first; sha may be an
	// abbreviation.
	ActionRunsAt(ctx context.Context, f *Forge, repo, sha string) ([]ActionRun, error)
	// ActionRunNumber is the run with this number in the repo, or ErrNotFound.
	ActionRunNumber(ctx context.Context, f *Forge, repo string, n int64) (*ActionRun, error)
	// ActionJobLog is the last maxJobLog bytes of a job's log.
	ActionJobLog(ctx context.Context, f *Forge, repo string, jobID int64) (string, error)
}

// runsAtPages bounds how far back an abbreviated commit is looked for: 4
// pages of 50 runs.
const runsAtPages = 4

// fullSHARunPages bounds the pages read for one full commit id (500 runs).
const fullSHARunPages = 10

func (c *httpForgeClient) ActionRunsAt(ctx context.Context, f *Forge, repo, sha string) ([]ActionRun, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return nil, ErrNotFound
	}
	if len(sha) == 40 {
		// Every page: a head with 50 newer skipped runs keeps an older
		// failed run on page two, and the run that counts needs them all.
		var out []ActionRun
		for page := 1; ; page++ {
			if page > fullSHARunPages {
				return nil, fmt.Errorf("more than %d runs on commit %s in %s", fullSHARunPages*50, sha, repo)
			}
			var res struct {
				Runs []ActionRun `json:"workflow_runs"`
			}
			if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs?head_sha=%s&page=%d&limit=50", repo, url.QueryEscape(sha), page), nil, &res); err != nil {
				return nil, err
			}
			out = append(out, res.Runs...)
			if len(res.Runs) < 50 {
				return out, nil
			}
		}
	}
	var out []ActionRun
	for page := 1; page <= runsAtPages; page++ {
		var res struct {
			Runs []ActionRun `json:"workflow_runs"`
		}
		if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs?page=%d&limit=50", repo, page), nil, &res); err != nil {
			return nil, err
		}
		for _, r := range res.Runs {
			if strings.HasPrefix(strings.ToLower(r.CommitSHA), sha) {
				out = append(out, r)
			}
		}
		if len(res.Runs) < 50 {
			break
		}
	}
	return out, nil
}

func (c *httpForgeClient) ActionRunNumber(ctx context.Context, f *Forge, repo string, n int64) (*ActionRun, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return nil, ErrNotFound
	}
	var res struct {
		Runs []ActionRun `json:"workflow_runs"`
	}
	if err := c.do(ctx, f, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs?run_number=%d&limit=1", repo, n), nil, &res); err != nil {
		return nil, err
	}
	if len(res.Runs) == 0 {
		return nil, fmt.Errorf("%w: no Actions run number %d in %s", ErrNotFound, n, repo)
	}
	return &res.Runs[0], nil
}

func (c *httpForgeClient) ActionJobLog(ctx context.Context, f *Forge, repo string, jobID int64) (string, error) {
	if f.Kind == "github" || f.Kind == "gitlab" {
		return "", ErrNotFound
	}
	return c.tail(ctx, f, fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", repo, jobID), maxJobLog)
}

// CILogInput says which jobs to read: the runs of a commit (SHA, full or
// abbreviated) or one run (Run, its number as `wp ci status` shows it), and
// of those the jobs named Job or with it in their name (any state); none
// named: the failed ones. Full: all of each log, not its failing part.
type CILogInput struct {
	Repo string
	SHA  string
	Run  int64
	Job  string
	Full bool
}

// CILogs are the jobs of the runs asked for.
type CILogs struct {
	Repo string     `json:"repo"`
	Runs []CIRunLog `json:"runs"`
}

// CIRunLog is one run and its jobs; only the picked jobs carry a log.
type CIRunLog struct {
	RunNumber int64      `json:"run_number"`
	Workflow  string     `json:"workflow"`
	Title     string     `json:"title"`
	Event     string     `json:"event"`
	Ref       string     `json:"ref"`
	SHA       string     `json:"sha"`
	Status    string     `json:"status"`
	URL       string     `json:"url,omitempty"`
	Jobs      []CIJobLog `json:"jobs"`
}

// CIJobLog is a job of a run: the forge's status of it, and (picked) its log.
type CIJobLog struct {
	JobID  int64  `json:"job_id"`
	Status string `json:"status"` // the forge's word: success, failure, running, skipped, …
	Picked bool   `json:"picked"`
	CheckLog
}

var reHexSHA = lazyre.New(`^[0-9a-f]{4,40}$`)

// CILogs reads the logs of CI jobs in an attached Forgejo repo the actor
// can see. A repo in a project the actor can't see answers like a repo that
// isn't attached.
func (s *Service) CILogs(ctx context.Context, actor *Actor, in CILogInput) (*CILogs, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	repo := strings.TrimSpace(in.Repo)
	sha := strings.ToLower(strings.TrimSpace(in.SHA))
	switch {
	case repo == "" || !strings.Contains(repo, "/"):
		return nil, invalid("name the repo as OWNER/NAME")
	case sha == "" && in.Run <= 0:
		return nil, invalid("give a commit (SHA) or a run number")
	case sha != "" && in.Run > 0:
		return nil, invalid("give a commit or a run number, not both")
	case sha != "" && !reHexSHA.MatchString(sha):
		return nil, invalid("%q isn't a commit SHA (4 to 40 hex digits)", in.SHA)
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.forge_id, r.repo, r.project_id FROM project_repos r JOIN forges f ON f.id = r.forge_id
		WHERE f.kind = 'forgejo' AND r.repo = ? COLLATE NOCASE ORDER BY r.id`, repo)
	if err != nil {
		return nil, err
	}
	var forgeID int64
	found := false
	for rows.Next() {
		var fid, pid int64
		var name string
		if err := rows.Scan(&fid, &name, &pid); err != nil {
			rows.Close()
			return nil, err
		}
		if !found && s.requireProjectRole(ctx, s.db.R, actor, pid, RoleViewer) == nil {
			forgeID, repo, found = fid, name, true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no Forgejo repo %q attached to a project you can see: %w", repo, ErrNotFound)
	}
	cl, ok := s.Forges.(ForgeActionLogs)
	if !ok {
		return nil, errors.New("this forge client can't read Actions logs")
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return nil, err
	}
	if f.token == "" {
		return nil, errors.New("the forge has no API token, so Waypoint can't read its logs")
	}

	var runs []ActionRun
	if in.Run > 0 {
		r, err := cl.ActionRunNumber(ctx, f, repo, in.Run)
		if err != nil {
			return nil, fmt.Errorf("run %d: %s: %w", in.Run, logError(err), ErrNotFound)
		}
		runs = []ActionRun{*r}
	} else {
		runs, err = cl.ActionRunsAt(ctx, f, repo, sha)
		if err != nil {
			return nil, fmt.Errorf("find the runs of %s: %s", sha, logError(err))
		}
		if len(runs) == 0 {
			hint := ""
			if len(sha) < 40 {
				hint = " (an abbreviated SHA is looked for among the repo's last 200 runs; give all 40 digits or --run)"
			}
			return nil, fmt.Errorf("no Actions run for %s in %s%s: %w", sha, repo, hint, ErrNotFound)
		}
	}

	out := &CILogs{Repo: repo, Runs: []CIRunLog{}}
	job := strings.ToLower(strings.TrimSpace(in.Job))
	reads := 0
	for _, r := range runs {
		rl := CIRunLog{RunNumber: r.Index, Workflow: r.WorkflowID, Title: r.Title, Event: r.Event, Ref: r.PrettyRef, SHA: r.CommitSHA,
			Status: r.Status, URL: r.HTMLURL, Jobs: []CIJobLog{}}
		jobs, err := cl.ActionJobs(ctx, f, repo, r.ID)
		if err != nil {
			return nil, fmt.Errorf("list run %d's jobs: %s", r.Index, logError(err))
		}
		for _, j := range jobs {
			jl := CIJobLog{JobID: j.ID, Status: j.Status, CheckLog: CheckLog{Name: j.Name, State: checkState(j.Status), URL: r.HTMLURL}}
			switch {
			case job != "":
				jl.Picked = strings.Contains(strings.ToLower(j.Name), job)
			default:
				jl.Picked = jl.State == CheckFailed && j.Status != "skipped"
			}
			if jl.Picked && reads < maxLogChecks {
				reads++
				log, why := cl.ActionJobLog(ctx, f, repo, j.ID)
				if why != nil {
					jl.Error = logError(why)
				}
				if log != "" {
					jl.From = "forge"
					if in.Full {
						lines := cleanLog(log)
						jl.Log = strings.Join(lines, "\n") + "\n"
						jl.Lines, jl.Shown = len(lines), len(lines)
					} else {
						jl.LogExcerpt = failingExcerpt(log, 0)
					}
				}
			} else if jl.Picked {
				jl.Error = fmt.Sprintf("not read: %d logs are the most one call reads; name the job with --job", maxLogChecks)
			}
			rl.Jobs = append(rl.Jobs, jl)
		}
		out.Runs = append(out.Runs, rl)
	}
	if job != "" {
		matched := false
		for _, r := range out.Runs {
			for _, j := range r.Jobs {
				matched = matched || j.Picked
			}
		}
		if !matched {
			return nil, fmt.Errorf("no job %q in the runs asked for (their jobs: %s): %w", in.Job, jobNames(out), ErrNotFound)
		}
	}
	return out, nil
}

func jobNames(l *CILogs) string {
	var names []string
	seen := map[string]bool{}
	for _, r := range l.Runs {
		for _, j := range r.Jobs {
			if !seen[j.Name] {
				seen[j.Name] = true
				names = append(names, j.Name)
			}
		}
	}
	return strings.Join(names, ", ")
}
