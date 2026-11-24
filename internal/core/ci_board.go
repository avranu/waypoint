package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ciBoardFresh    = 15 * time.Second
	ciHistoryFresh  = 2 * time.Minute
	ciHistoryWindow = 24 * time.Hour
	ciTaskPages     = 6
	ciRecentMax     = 200
	ciBucket        = 15 * time.Minute
	ciReadParallel  = 6
	ciPoolKey       = "ci_pool"
)

// CIBoard is the page (and wp ci status).
type CIBoard struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Summary     CISummary    `json:"summary"`
	Alerts      []CIAlert    `json:"alerts"`
	Pools       []CIPool     `json:"pools"`
	Runs        []CIRun      `json:"runs"`    // unfinished, the one finishing last first
	Running     []CIJob      `json:"running"` // longest running first
	Queued      []CIJob      `json:"queued"`  // in the order they should start
	Recent      []CIJob      `json:"recent"`  // finished in the last day, newest first
	Stats       []CIJobStats `json:"stats"`   // per job, most runner time first
	Repos       []CIRepo     `json:"repos"`
	Hosts       []CIHost     `json:"hosts"` // the CI hosts that report (WP-231)
	Timeline    []CIPoint    `json:"timeline"`
	CanEditPool bool         `json:"can_edit_pool"`
}

// CISummary answers "how busy is it and when will it be done".
type CISummary struct {
	Running  int `json:"running"`
	Waiting  int `json:"waiting"` // ready, waiting for a runner
	Blocked  int `json:"blocked"` // waiting for other jobs of their run
	Capacity int `json:"capacity"`
	// AllDoneAt is when everything running and queued should have
	// finished, with usual run times; AllDoneP90 with slow ones. Nil when
	// nothing is running or queued, or some job can never start.
	AllDoneAt     *time.Time `json:"all_done_at,omitempty"`
	AllDoneP90    *time.Time `json:"all_done_p90,omitempty"`
	Stuck         int        `json:"stuck"` // jobs that can't start: no runner offers their labels
	OldestWaiting *time.Time `json:"oldest_waiting,omitempty"`
	Finished24h   int        `json:"finished_24h"`
	Failed24h     int        `json:"failed_24h"`
	Cancelled24h  int        `json:"cancelled_24h"`
	WaitP50       int64      `json:"wait_p50"` // seconds, finished jobs of the last day
	WaitP90       int64      `json:"wait_p90"`
	// WaitP50_3h and WaitP90_3h are the waits of the jobs that started in
	// the last 3h (doctorRecent), from every job of the day rather than
	// the capped Recent list (WP-573).
	WaitP50_3h     int64   `json:"wait_p50_3h"`
	WaitP90_3h     int64   `json:"wait_p90_3h"`
	RunP50         int64   `json:"run_p50"`
	BusySeconds24h int64   `json:"busy_seconds_24h"`
	Utilization24h float64 `json:"utilization_24h"` // busy time over capacity × the time since Since
	// Since is where the "24h" figures start: a day ago, or later when the
	// jobs kept have a hole (Waypoint was down longer than its reads
	// reach), so they are never a part of the day taken for all of it.
	Since time.Time `json:"since"`
}

// CIAlert is something worth a look: error (CI is stuck), warn, info.
type CIAlert struct {
	Level string `json:"level"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
	key   string
}

// CIPool is the runners a forge's jobs share, as the ETAs model them.
type CIPool struct {
	ForgeID   int64          `json:"forge_id"`
	ForgeName string         `json:"forge_name"`
	Source    string         `json:"source"` // configured, runners, observed
	Runners   []CIPoolRunner `json:"runners"`
	// Forge is what the forge says of its runners; nil when it won't
	// (not a site administrator's token).
	Forge    []ActionRunner `json:"forge_runners"`
	Peak24h  int            `json:"peak_24h"` // most jobs running at once in the last day
	Capacity int            `json:"capacity"`
}

// CIRun is an unfinished run.
type CIRun struct {
	CIRef
	Jobs       int        `json:"jobs"`
	Done       int        `json:"done"`
	Failed     int        `json:"failed"`
	Running    int        `json:"running"`
	Waiting    int        `json:"waiting"`
	Status     string     `json:"status"`
	EstFinish  *time.Time `json:"est_finish,omitempty"`
	EstP90     *time.Time `json:"est_finish_p90,omitempty"`
	CantFinish bool       `json:"cant_finish,omitempty"`
}

// CIRef is what a job or run is of. Hidden: in a project the viewer can't
// see, so only its timing is shown.
type CIRef struct {
	ForgeID   int64     `json:"forge_id"`
	Repo      string    `json:"repo"`
	Project   string    `json:"project,omitempty"`
	Hidden    bool      `json:"hidden,omitempty"`
	Workflow  string    `json:"workflow"`
	RunID     int64     `json:"run_id"`
	RunNumber int64     `json:"run_number"`
	Title     string    `json:"title"`
	Ref       string    `json:"ref"`
	Event     string    `json:"event"`
	SHA       string    `json:"sha"`
	URL       string    `json:"url"`
	Created   time.Time `json:"created"`
	PR        *CIPRRef  `json:"pr,omitempty"`
}

// CIPRRef is the pull request a run is for.
type CIPRRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
}

// CIJob is one job: running, queued or finished.
type CIJob struct {
	CIRef
	Name     string     `json:"name"`
	JobID    int64      `json:"job_id,omitempty"`
	Status   string     `json:"status"` // waiting, blocked, running, success, failure, cancelled, skipped
	RunsOn   []string   `json:"runs_on,omitempty"`
	Needs    []string   `json:"needs,omitempty"`
	ReadyAt  *time.Time `json:"ready_at,omitempty"` // queued since (waiting) or since its run was made (blocked)
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	Wait     *int64     `json:"wait,omitempty"`     // seconds it waited for a runner
	Duration int64      `json:"duration,omitempty"` // seconds it ran (so far)
	Typical  int64      `json:"typical"`            // usual run time, seconds
	Slow     int64      `json:"slow"`               // 90th percentile
	// When it should start and end (queued and running jobs).
	EstStart  *time.Time `json:"est_start,omitempty"`
	EstFinish *time.Time `json:"est_finish,omitempty"`
	EstP90    *time.Time `json:"est_finish_p90,omitempty"`
	Overrun   bool       `json:"overrun,omitempty"`   // running past its slow time
	NoRunner  bool       `json:"no_runner,omitempty"` // no runner offers its labels
	Position  int        `json:"position,omitempty"`  // in the queue, from 1
	// Where it runs and what it used so far, when its host reports (WP-231).
	Host       string  `json:"host,omitempty"`
	CPUSeconds float64 `json:"cpu_seconds,omitempty"`
	MemPeak    int64   `json:"mem_peak,omitempty"`
}

// CIJobStats is how one job of a workflow fared in the last day.
type CIJobStats struct {
	Repo      string     `json:"repo"`
	Hidden    bool       `json:"hidden,omitempty"`
	Workflow  string     `json:"workflow"`
	Name      string     `json:"name"`
	Runs      int        `json:"runs"`
	Success   int        `json:"success"`
	Failure   int        `json:"failure"`
	Cancelled int        `json:"cancelled"`
	P50       int64      `json:"p50"`
	P90       int64      `json:"p90"`
	Max       int64      `json:"max"`
	WaitP50   int64      `json:"wait_p50"`
	Busy      int64      `json:"busy"` // runner seconds
	Last      string     `json:"last"`
	LastAt    *time.Time `json:"last_at,omitempty"`
	// What it used over the runs its hosts reported in the last two weeks
	// (WP-231): peak memory with its services, and CPU cores in use.
	UsageRuns int     `json:"usage_runs,omitempty"`
	MemP95    int64   `json:"mem_p95,omitempty"`
	MemMax    int64   `json:"mem_max,omitempty"`
	CoresP50  float64 `json:"cores_p50,omitempty"`
	CoresP95  float64 `json:"cores_p95,omitempty"`
}

// CIRepo is a repo the board read.
type CIRepo struct {
	ForgeID int64  `json:"forge_id"`
	Repo    string `json:"repo"`
	Project string `json:"project,omitempty"`
	Hidden  bool   `json:"hidden,omitempty"`
	Error   string `json:"error,omitempty"`
	Busy    int64  `json:"busy"` // runner seconds in the last day
	Jobs    int    `json:"jobs"` // finished in the last day
	Running int    `json:"running"`
	Queued  int    `json:"queued"`
}

// CIPoint is one slice of the last day: how many jobs ran and waited on
// average.
type CIPoint struct {
	At      time.Time `json:"at"`
	Running float64   `json:"running"`
	Waiting float64   `json:"waiting"`
}

// ciBoardCache keeps what was read, shared by everyone looking.
type ciBoardCache struct {
	mu       sync.Mutex
	at       time.Time
	snap     *ciSnapshot
	history  map[string]*ciHistory
	runners  map[int64]ciRunners
	stall    map[string]ciStall
	tests    map[string][]string
	building chan struct{}
}

type ciRunners struct {
	at   time.Time
	list []ActionRunner
	err  error
}

type ciHistory struct {
	at        time.Time
	tasks     []ActionTask
	runs      map[int64]ActionRun
	err       error
	kept      []ciKeptJob
	waits     map[int64]int64
	whole     time.Time
	draftRuns map[int64]bool
}

// tierOf is the tier of a task's run: a task doesn't say, and its title is
// the commit's subject (WP-849).
func (h *ciHistory) tierOf(t ActionTask) string {
	if r, ok := h.runs[t.RunNumber]; ok {
		return r.tier()
	}
	if h.draftRuns[t.RunNumber] {
		return ciDraftTier
	}
	return ""
}

// ciKeptJob is a finished job read back from ci_jobs.
type ciKeptJob struct {
	ActionTask
	tier string
	wait *int64
}

// ciRepoRef is an attached repo and the projects it's attached to.
type ciRepoRef struct {
	forge    *Forge
	repo     string
	projects []ciProject
}

type ciProject struct {
	id  int64
	key string
}

// ciRepoData is what was read of one repo.
type ciRepoData struct {
	ref     ciRepoRef
	active  []ActionRun
	jobs    map[int64][]ActionJob
	running map[int64]ActionTask
	hist    *ciHistory
	err     error
}

type ciSnapshot struct {
	at      time.Time
	repos   []*ciRepoData
	runners map[int64]ciRunners
	pools   map[int64][]CIPoolRunner
	prs     map[string]CIPRRef
}

// CIBoard reads (or reuses) what CI is doing and lays it out for actor.
func (s *Service) CIBoard(ctx context.Context, actor *Actor) (*CIBoard, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	snap, err := s.ciSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return s.layoutCIBoard(ctx, actor, snap), nil
}

// ciSnapshot returns a snapshot no older than ciBoardFresh; one reader
// builds it while the others wait.
func (s *Service) ciSnapshot(ctx context.Context) (*ciSnapshot, error) {
	c := &s.ciBoard
	for {
		c.mu.Lock()
		if c.snap != nil && s.now().Sub(c.at) < ciBoardFresh {
			snap := c.snap
			c.mu.Unlock()
			return snap, nil
		}
		if c.building == nil {
			c.building = make(chan struct{})
			c.mu.Unlock()
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			snap, err := s.readCI(rctx)
			cancel()
			if err == nil {
				s.notePoolSaturation(snap)
			}
			c.mu.Lock()
			if err == nil {
				c.snap, c.at = snap, s.now()
			}
			close(c.building)
			c.building = nil
			c.mu.Unlock()
			return snap, err
		}
		wait := c.building
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ciVisibleRepos are the CI repos actor can see (a project of theirs
// attaches it), by forge id|lower(repo).
func (s *Service) ciVisibleRepos(ctx context.Context, actor *Actor) (map[string]ciRepoRef, error) {
	repos, err := s.ciRepos(ctx)
	if err != nil {
		return nil, err
	}
	visible := map[string]ciRepoRef{}
	seen := map[int64]bool{}
	for _, r := range repos {
		for _, p := range r.projects {
			ok, known := seen[p.id]
			if !known {
				ok = s.requireProjectRole(ctx, s.db.R, actor, p.id, RoleViewer) == nil
				seen[p.id] = ok
			}
			if ok {
				visible[strconv.FormatInt(r.forge.ID, 10)+"|"+strings.ToLower(r.repo)] = r
				break
			}
		}
	}
	return visible, nil
}

// ciRepos lists the Forgejo repos attached to projects.
func (s *Service) ciRepos(ctx context.Context) ([]ciRepoRef, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.forge_id, r.repo, p.id, p.key FROM project_repos r
		JOIN forges f ON f.id = r.forge_id JOIN projects p ON p.id = r.project_id
		WHERE f.kind = 'forgejo' ORDER BY r.forge_id, lower(r.repo), p.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ciRepoRef
	at := map[string]int{}
	forges := map[int64]*Forge{}
	for rows.Next() {
		var fid, pid int64
		var repo, key string
		if err := rows.Scan(&fid, &repo, &pid, &key); err != nil {
			return nil, err
		}
		k := strconv.FormatInt(fid, 10) + "|" + strings.ToLower(repo)
		if i, ok := at[k]; ok {
			out[i].projects = append(out[i].projects, ciProject{pid, key})
			continue
		}
		if forges[fid] == nil {
			forges[fid] = &Forge{ID: fid}
		}
		at[k] = len(out)
		out = append(out, ciRepoRef{forge: forges[fid], repo: repo, projects: []ciProject{{pid, key}}})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, f := range forges {
		full, err := s.forgeByID(ctx, id)
		if err != nil {
			return nil, err
		}
		*f = *full
	}
	return out, nil
}

func (s *Service) readCI(ctx context.Context) (*ciSnapshot, error) {
	repos, err := s.ciRepos(ctx)
	if err != nil {
		return nil, err
	}
	snap := &ciSnapshot{at: s.now(), runners: map[int64]ciRunners{}, prs: map[string]CIPRRef{}}
	if snap.pools, err = s.ciPools(ctx); err != nil {
		return nil, err
	}
	cl, ok := s.Forges.(ForgeActions)
	if !ok {
		return snap, nil
	}
	sem := make(chan struct{}, ciReadParallel)
	var wg sync.WaitGroup
	snap.repos = make([]*ciRepoData, len(repos))
	for i, r := range repos {
		snap.repos[i] = &ciRepoData{ref: r}
		if r.forge.token == "" {
			snap.repos[i].err = errors.New("the forge has no token")
			continue
		}
		wg.Add(1)
		go func(d *ciRepoData) {
			defer wg.Done()
			s.readCIRepo(ctx, cl, d, sem)
		}(snap.repos[i])
	}
	forges := map[int64]*Forge{}
	for _, r := range repos {
		forges[r.forge.ID] = r.forge
	}
	for id, f := range forges {
		snap.runners[id] = s.ciForgeRunners(ctx, cl, f)
	}
	wg.Wait()
	s.ciPullRequests(ctx, snap)
	s.noteCITasks(ctx, snap)
	return snap, nil
}

func (s *Service) ciForgeRunners(ctx context.Context, cl ForgeActions, f *Forge) ciRunners {
	c := &s.ciBoard
	c.mu.Lock()
	r, ok := c.runners[f.ID]
	c.mu.Unlock()
	if ok && s.now().Sub(r.at) < ciHistoryFresh {
		return r
	}
	list, err := cl.ActionRunners(ctx, f)
	r = ciRunners{at: s.now(), list: list, err: err}
	c.mu.Lock()
	if c.runners == nil {
		c.runners = map[int64]ciRunners{}
	}
	c.runners[f.ID] = r
	c.mu.Unlock()
	return r
}

// readCIRepo reads a repo's unfinished runs and their jobs, its running
// tasks, and (when stale) its history.
func (s *Service) readCIRepo(ctx context.Context, cl ForgeActions, d *ciRepoData, sem chan struct{}) {
	f, repo := d.ref.forge, d.ref.repo
	call := func(fn func() error) error {
		sem <- struct{}{}
		defer func() { <-sem }()
		return fn()
	}
	err := call(func() (err error) {
		d.active, err = cl.ActionRuns(ctx, f, repo, []string{"waiting", "running", "blocked"}, 1, 50)
		return err
	})
	if noActions(err) {
		return
	}
	if err != nil {
		d.err = err
		return
	}
	d.active = slices.DeleteFunc(d.active, func(r ActionRun) bool { return !ciPending(r.Status) })
	d.jobs = map[int64][]ActionJob{}
	gone := map[int64]bool{}
	for _, r := range d.active {
		var jobs []ActionJob
		err := call(func() (err error) { jobs, err = cl.ActionJobs(ctx, f, repo, r.ID); return err })
		if noActions(err) {
			gone[r.ID] = true
			continue
		}
		if err != nil {
			d.err = err
			return
		}
		d.jobs[r.ID] = jobs
	}
	d.active = slices.DeleteFunc(d.active, func(r ActionRun) bool { return gone[r.ID] })
	var running []ActionTask
	err = call(func() (err error) {
		running, err = cl.ActionTasks(ctx, f, repo, []string{"running"}, 1, 50)
		return err
	})
	if noActions(err) {
		*d = ciRepoData{ref: d.ref}
		return
	}
	if err != nil {
		d.err = err
		return
	}
	d.running = map[int64]ActionTask{}
	for _, t := range running {
		d.running[t.ID] = t
	}
	d.hist = s.ciRepoHistory(ctx, cl, f, repo, call)
	if noActions(d.hist.err) {
		*d = ciRepoData{ref: d.ref}
		return
	}
	if d.hist.err != nil {
		d.err = d.hist.err
	}
}

func (s *Service) ciRepoHistory(ctx context.Context, cl ForgeActions, f *Forge, repo string, call func(func() error) error) *ciHistory {
	c := &s.ciBoard
	key := strconv.FormatInt(f.ID, 10) + "|" + strings.ToLower(repo)
	c.mu.Lock()
	h := c.history[key]
	c.mu.Unlock()
	if h != nil && h.err == nil && s.now().Sub(h.at) < ciHistoryFresh {
		return h
	}
	h = &ciHistory{at: s.now(), runs: map[int64]ActionRun{}}
	since := s.now().Add(-ciHistoryWindow)
	var oldest time.Time
	for page := 1; page <= ciTaskPages; page++ {
		var list []ActionTask
		if h.err = call(func() (err error) { list, err = cl.ActionTasks(ctx, f, repo, nil, page, 50); return err }); h.err != nil {
			break
		}
		h.tasks = append(h.tasks, list...)
		if len(list) < 50 {
			oldest = time.Time{}
			break
		}
		oldest = list[len(list)-1].Created
		if oldest.Before(since) {
			break
		}
	}
	var runs []ActionRun
	if h.err == nil {
		h.err = call(func() (err error) { runs, err = cl.ActionRuns(ctx, f, repo, nil, 1, 50); return err })
	}
	for _, r := range runs {
		h.runs[r.Index] = r
	}
	h.tasks = settleTaskEnds(h.tasks, h.runs)
	if h.err == nil {
		h.whole = oldest
		if s.recordCIHistory(ctx, f.ID, repo, h.tasks, h.runs) == nil {
			h.whole = s.noteCIWhole(ctx, f.ID, repo, oldest)
		}
		if drafts, err := s.keptDraftRuns(ctx, f.ID, repo, h.tasks, h.runs); err != nil {
			s.log.WarnContext(ctx, "read kept CI run tiers", "repo", repo, "err", err)
		} else {
			h.draftRuns = drafts
		}
		if err := s.keptCIJobs(ctx, f.ID, repo, since, h); err != nil {
			s.log.WarnContext(ctx, "read kept CI jobs", "repo", repo, "err", err)
			h.kept, h.whole = nil, oldest
		}
	}
	c.mu.Lock()
	if c.history == nil {
		c.history = map[string]*ciHistory{}
	}
	c.history[key] = h
	c.mu.Unlock()
	return h
}

// ciPullRequests finds the pull requests the runs are for, by head.
func (s *Service) ciPullRequests(ctx context.Context, snap *ciSnapshot) {
	since := s.now().Add(-2 * ciHistoryWindow).UnixMilli()
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, repo, number, url, title, head_sha FROM pull_requests
		WHERE head_sha != '' AND (state IN ('open','draft') OR updated_at > ?)`, since)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var fid int64
		var repo, sha string
		var pr CIPRRef
		if rows.Scan(&fid, &repo, &pr.Number, &pr.URL, &pr.Title, &sha) == nil {
			snap.prs[ciPRKey(fid, repo, sha)] = pr
		}
	}
}

func ciPRKey(forgeID int64, repo, sha string) string {
	return strconv.FormatInt(forgeID, 10) + "|" + strings.ToLower(repo) + "|" + strings.ToLower(sha)
}

// ciPending: a run or job that hasn't finished.
func ciPending(status string) bool {
	switch status {
	case "waiting", "running", "blocked", "unknown":
		return true
	}
	return false
}

// validTime: Forgejo sends an unset time as the Unix epoch.
func validTime(t time.Time) bool { return t.Year() > 2000 }

// noActions: the forge has no Actions for the repo (turned off, or a forge
// kind without them), which isn't a fault to report (WP-491).
func noActions(err error) bool {
	return err != nil && (errors.Is(err, ErrNotFound) || isStatus(err, http.StatusNotFound))
}

// ciTaskLongest is longer than any task runs: Forgejo stops a job at its
// timeout (3h by default) well before.
const ciTaskLongest = 6 * time.Hour

// settleTaskEnds gives finished tasks their real end. Forgejo has no stop
// time for a task, and its updated_at moves again long after the task ended
// (log upkeep a day later), so a task took hours and fell back into the last
// day (WP-491). A finished run's stop bounds its tasks; a task whose run
// isn't among those read and that seems to have run longer than any task
// can is dropped, its end unknown.
func settleTaskEnds(tasks []ActionTask, runs map[int64]ActionRun) []ActionTask {
	out := tasks[:0]
	for _, t := range tasks {
		if !ciPending(t.Status) && validTime(t.Started) {
			if r, ok := runs[t.RunNumber]; ok && !ciPending(r.Status) && validTime(r.Stopped) && !r.Stopped.Before(t.Started) {
				if t.Updated.After(r.Stopped) {
					t.Updated = r.Stopped
				}
			} else if !ok && t.Updated.Sub(t.Started) > ciTaskLongest {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// CIDayLabel names the span of a board's "24h" figures, which start at
// since: "last 24h", or how much shorter it is and why.
func CIDayLabel(since, now time.Time) string {
	if span := now.Sub(since); span < ciHistoryWindow-ciHistoryFresh {
		return "last " + ShortDuration(span) + " (no whole day kept)"
	}
	return "last 24h"
}

// ciDayJobs is a repo's jobs of the last day: the tasks read, each finished
// one with how long it waited for a runner, then those only kept. A task
// became ready when its run was made, or when the last task of its run
// before it ended (its needs, roughly); a task whose run wasn't read takes
// the wait kept with it.
func ciDayJobs(h *ciHistory) []ciKeptJob {
	byRun := map[int64][]ActionTask{}
	for _, t := range h.tasks {
		byRun[t.RunNumber] = append(byRun[t.RunNumber], t)
	}
	out := make([]ciKeptJob, 0, len(h.tasks)+len(h.kept))
	for _, t := range h.tasks {
		k := ciKeptJob{ActionTask: t, tier: h.tierOf(t)}
		if run, ok := h.runs[t.RunNumber]; ok && validTime(run.Created) {
			ready := run.Created
			for _, o := range byRun[t.RunNumber] {
				if o.ID != t.ID && !ciPending(o.Status) && o.Updated.Before(t.Created) && o.Updated.After(ready) {
					ready = o.Updated
				}
			}
			w := int64(max(0, t.Started.Sub(ready).Seconds()))
			k.wait = &w
		} else if w, ok := h.waits[t.ID]; ok {
			k.wait = &w
		}
		out = append(out, k)
	}
	return append(out, h.kept...)
}

func tptr(t time.Time) *time.Time {
	if !validTime(t) {
		return nil
	}
	return &t
}

// layoutCIBoard turns a snapshot into the board actor may see.
func (s *Service) layoutCIBoard(ctx context.Context, actor *Actor, snap *ciSnapshot) *CIBoard {
	now := snap.at
	from := now.Add(-ciHistoryWindow)
	for _, d := range snap.repos {
		if d.hist != nil && d.hist.whole.After(from) {
			from = d.hist.whole
		}
	}
	b := &CIBoard{GeneratedAt: now, Summary: CISummary{Since: from}, CanEditPool: actor.isAdmin() && !actor.IsAgent(),
		Alerts: []CIAlert{}, Runs: []CIRun{}, Running: []CIJob{}, Queued: []CIJob{}, Recent: []CIJob{}, Stats: []CIJobStats{}, Repos: []CIRepo{}}
	seen := map[int64]bool{}
	visible := func(ps []ciProject) (string, bool) {
		for _, p := range ps {
			ok, known := seen[p.id]
			if !known {
				ok = s.requireProjectRole(ctx, s.db.R, actor, p.id, RoleViewer) == nil
				seen[p.id] = ok
			}
			if ok {
				return p.key, true
			}
		}
		return "", false
	}

	durs := newCIDurations()
	type statAcc struct {
		CIJobStats
		forge       int64
		durs, waits []float64
	}
	stats := map[string]*statAcc{}
	var waits, waits3h, runTimes []float64
	from3h := now.Add(-doctorRecent)
	spans := map[int64][][2]time.Time{}
	var waitSpans [][2]time.Time
	var busySpans [][2]time.Time
	repoRow := make([]CIRepo, len(snap.repos))
	type known struct {
		d   *ciRepoData
		key string
		ok  bool
	}
	view := make([]known, len(snap.repos))
	for i, d := range snap.repos {
		key, ok := visible(d.ref.projects)
		view[i] = known{d, key, ok}
		repoRow[i] = CIRepo{ForgeID: d.ref.forge.ID, Repo: d.ref.repo, Project: key, Hidden: !ok}
		if d.err != nil {
			repoRow[i].Error = d.err.Error()
		}
		if d.hist == nil {
			continue
		}
		for _, dj := range ciDayJobs(d.hist) {
			t := dj.ActionTask
			if !validTime(t.Started) {
				continue
			}
			end := t.Updated
			if ciPending(t.Status) {
				end = now
			}
			if end.Before(from) {
				continue
			}
			spans[d.ref.forge.ID] = append(spans[d.ref.forge.ID], [2]time.Time{t.Started, end})
			busySpans = append(busySpans, [2]time.Time{t.Started, end})
			if ciPending(t.Status) {
				continue
			}
			dur := end.Sub(t.Started).Seconds()
			run, haveRun := d.hist.runs[t.RunNumber]
			wait := dj.wait
			if wait != nil {
				waitSpans = append(waitSpans, [2]time.Time{t.Started.Add(-time.Duration(*wait) * time.Second), t.Started})
			}
			if t.Status == "skipped" {
				continue
			}
			repoRow[i].Busy += int64(dur)
			repoRow[i].Jobs++
			k := ciJobKey(d.ref.repo, t.WorkflowID, t.Name)
			st := stats[k]
			if st == nil {
				st = &statAcc{CIJobStats: CIJobStats{Repo: d.ref.repo, Hidden: !ok, Workflow: t.WorkflowID, Name: t.Name}, forge: d.ref.forge.ID}
				stats[k] = st
			}
			st.Runs++
			st.Busy += int64(dur)
			st.Max = max(st.Max, int64(dur))
			if st.LastAt == nil || end.After(*st.LastAt) {
				st.Last, st.LastAt = t.Status, tptr(end)
			}
			switch t.Status {
			case "success":
				st.Success++
				st.durs = append(st.durs, dur)
				durs.add(d.ref.repo, t.WorkflowID, t.Name, dj.tier, dur)
			case "failure":
				st.Failure++
			case "cancelled":
				st.Cancelled++
			}
			if wait != nil {
				st.waits = append(st.waits, float64(*wait))
				waits = append(waits, float64(*wait))
				if !t.Started.Before(from3h) {
					waits3h = append(waits3h, float64(*wait))
				}
			}
			runTimes = append(runTimes, dur)
			b.Summary.Finished24h++
			switch t.Status {
			case "failure":
				b.Summary.Failed24h++
			case "cancelled":
				b.Summary.Cancelled24h++
			}
			j := CIJob{CIRef: s.ciRef(snap, d, ok, key, run, haveRun, t), Name: t.Name, Status: t.Status,
				Started: tptr(t.Started), Finished: tptr(end), Wait: wait, Duration: int64(dur)}
			b.Recent = append(b.Recent, j)
		}
	}
	usage, err := s.ciUsage(ctx, now.Add(-ciUsageWindow))
	if err != nil {
		s.log.WarnContext(ctx, "read CI job usage", "err", err)
	}
	for _, st := range stats {
		if u := usage[fmt.Sprintf("%d|%s", st.forge, ciJobKey(st.Repo, st.Workflow, st.Name))]; u != nil {
			st.UsageRuns, st.MemP95, st.MemMax, st.CoresP50, st.CoresP95 = u.Runs, u.MemP95, u.MemMax, u.CoresP50, u.CoresP95
		}
		if st.Hidden {
			st.Repo, st.Workflow = "", ""
		}
		if len(st.durs) > 0 {
			st.P50, st.P90 = int64(percentile(st.durs, 50)), int64(percentile(st.durs, 90))
		}
		if len(st.waits) > 0 {
			st.WaitP50 = int64(percentile(st.waits, 50))
		}
		b.Stats = append(b.Stats, st.CIJobStats)
	}
	slices.SortFunc(b.Stats, func(x, y CIJobStats) int { return cmpInt64(y.Busy, x.Busy) })
	slices.SortFunc(b.Recent, func(x, y CIJob) int { return y.Finished.Compare(*x.Finished) })
	if len(b.Recent) > ciRecentMax {
		b.Recent = b.Recent[:ciRecentMax]
	}
	if len(waits) > 0 {
		b.Summary.WaitP50, b.Summary.WaitP90 = int64(percentile(waits, 50)), int64(percentile(waits, 90))
	}
	if len(waits3h) > 0 {
		b.Summary.WaitP50_3h, b.Summary.WaitP90_3h = int64(percentile(waits3h, 50)), int64(percentile(waits3h, 90))
	}
	if len(runTimes) > 0 {
		b.Summary.RunP50 = int64(percentile(runTimes, 50))
	}

	s.keptDurations(ctx, durs, snap.repos)

	type pend struct {
		job   CIJob
		forge int64
		sim   int
		runIx int
	}
	var pending []pend
	runUse := s.ciRunningUse(ctx)
	sims := map[int64][]simJob{}
	sims90 := map[int64][]simJob{}
	for i, v := range view {
		d := v.d
		fid := d.ref.forge.ID
		for _, run := range d.active {
			jobs := d.jobs[run.ID]
			ref := s.ciRef(snap, d, v.ok, v.key, run, true, ActionTask{})
			runIx := len(b.Runs)
			cr := CIRun{CIRef: ref, Jobs: len(jobs), Status: run.Status}
			idx := map[int64]int{}
			for _, j := range jobs {
				switch {
				case j.Status == "running":
					cr.Running++
				case j.Status == "waiting" || j.Status == "blocked":
					cr.Waiting++
				case j.Status == "failure":
					cr.Done++
					cr.Failed++
				case !ciPending(j.Status):
					cr.Done++
				}
				if !ciPending(j.Status) || j.Status == "unknown" {
					continue
				}
				tier := run.tier()
				p50, p90 := durs.estimate(d.ref.repo, run.WorkflowID, j.Name, tier)
				cj := CIJob{CIRef: ref, Name: j.Name, JobID: j.ID, Status: j.Status, RunsOn: j.RunsOn, Needs: j.Needs,
					Typical: int64(p50.Seconds()), Slow: int64(p90.Seconds())}
				sj, sj90 := simJob{id: j.ID, runsOn: j.RunsOn}, simJob{id: j.ID, runsOn: j.RunsOn}
				if j.Status == "running" {
					started := now
					if t, ok := d.running[j.TaskID]; ok && validTime(t.Started) {
						started = t.Started
					}
					elapsed := now.Sub(started)
					if elapsed > p50 {
						p50, p90 = durs.estimateAfter(d.ref.repo, run.WorkflowID, j.Name, tier, elapsed)
						cj.Typical, cj.Slow = int64(p50.Seconds()), int64(p90.Seconds())
					}
					rem, over := remaining(elapsed, p50, p90)
					rem90 := rem
					if elapsed < p90 {
						rem90 = p90 - elapsed
					}
					cj.Started, cj.Duration, cj.Overrun = &started, int64(elapsed.Seconds()), over
					if u, ok := runUse[[2]int64{fid, j.TaskID}]; ok {
						cj.Host, cj.CPUSeconds, cj.MemPeak = u.host, round2(u.cpu), u.mem
					}
					sj.running, sj.end = true, now.Add(rem)
					sj90.running, sj90.end = true, now.Add(rem90)
					repoRow[i].Running++
				} else {
					cj.ReadyAt = tptr(jobReadyAt(d, run, j))
					sj.dur, sj90.dur = p50, p90
					repoRow[i].Queued++
				}
				idx[j.ID] = len(sims[fid])
				sims[fid] = append(sims[fid], sj)
				sims90[fid] = append(sims90[fid], sj90)
				pending = append(pending, pend{job: cj, forge: fid, sim: idx[j.ID], runIx: runIx})
			}
			for _, j := range jobs {
				si, ok := idx[j.ID]
				if !ok || len(j.Needs) == 0 || j.Status == "running" {
					continue
				}
				var deps []int
				for _, n := range j.Needs {
					found := false
					for _, o := range jobs {
						if oi, ok := idx[o.ID]; ok && o.ID != j.ID && strings.EqualFold(o.Name, n) {
							deps, found = append(deps, oi), true
						}
					}
					if !found && !ciNeedDone(jobs, n) {
						for _, o := range jobs {
							if oi, ok := idx[o.ID]; ok && o.ID != j.ID && len(o.Needs) == 0 {
								deps = append(deps, oi)
							}
						}
					}
				}
				sims[fid][si].deps = deps
				sims90[fid][si].deps = deps
			}
			b.Runs = append(b.Runs, cr)
		}
	}

	hosts, err := s.ciHosts(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "read CI hosts", "err", err)
	}
	b.Hosts = []CIHost{}
	forgeNames := map[int64]string{}
	for _, d := range snap.repos {
		forgeNames[d.ref.forge.ID] = d.ref.forge.Name
	}
	poolCap := 0
	for fid, name := range forgeNames {
		running := 0
		for _, sj := range sims[fid] {
			if sj.running {
				running++
			}
		}
		p := CIPool{ForgeID: fid, ForgeName: name, Peak24h: peakConcurrency(spans[fid])}
		if r := snap.runners[fid]; r.err == nil {
			p.Forge = r.list
		}
		observed := max(p.Peak24h, running, 1)
		p.Source, p.Runners = ciPoolModel(snap.pools[fid], p.Forge, observed)
		if rp := reportedPool(hosts[fid], snap.pools[fid], observed); rp != nil {
			p.Source, p.Runners = "reported", rp
		}
		b.Hosts = append(b.Hosts, hosts[fid]...)
		for _, r := range p.Runners {
			p.Capacity += r.Capacity
		}
		poolCap += p.Capacity
		simulate(now, p.Runners, sims[fid])
		simulate(now, p.Runners, sims90[fid])
		b.Pools = append(b.Pools, p)
	}
	slices.SortFunc(b.Pools, func(x, y CIPool) int { return cmpInt64(x.ForgeID, y.ForgeID) })
	b.Summary.Capacity = poolCap

	var allDone, allDone90 time.Time
	for _, p := range pending {
		sj, sj90 := sims[p.forge][p.sim], sims90[p.forge][p.sim]
		j := p.job
		run := &b.Runs[p.runIx]
		if sj.placed {
			j.EstFinish, j.EstP90 = &sj.finish, &sj90.finish
			if !sj.running {
				j.EstStart = &sj.start
			}
			allDone = later(allDone, sj.finish)
			allDone90 = later(allDone90, sj90.finish)
			if run.EstFinish == nil || sj.finish.After(*run.EstFinish) {
				run.EstFinish = &sj.finish
			}
			if run.EstP90 == nil || sj90.finish.After(*run.EstP90) {
				run.EstP90 = &sj90.finish
			}
		} else {
			run.CantFinish = true
			j.NoRunner = sj.noRunner
			b.Summary.Stuck++
		}
		switch j.Status {
		case "running":
			b.Summary.Running++
			b.Running = append(b.Running, j)
		default:
			if j.Status == "blocked" {
				b.Summary.Blocked++
			} else {
				b.Summary.Waiting++
				if j.ReadyAt != nil && (b.Summary.OldestWaiting == nil || j.ReadyAt.Before(*b.Summary.OldestWaiting)) {
					b.Summary.OldestWaiting = j.ReadyAt
				}
			}
			b.Queued = append(b.Queued, j)
		}
	}
	for i := range b.Runs {
		if b.Runs[i].CantFinish {
			b.Runs[i].EstFinish, b.Runs[i].EstP90 = nil, nil
		}
	}
	if !allDone.IsZero() && b.Summary.Stuck == 0 {
		b.Summary.AllDoneAt, b.Summary.AllDoneP90 = &allDone, &allDone90
	}
	slices.SortFunc(b.Running, func(x, y CIJob) int { return x.Started.Compare(*y.Started) })
	slices.SortStableFunc(b.Queued, func(x, y CIJob) int {
		switch {
		case x.EstStart == nil && y.EstStart == nil:
			return cmpInt64(x.JobID, y.JobID)
		case x.EstStart == nil:
			return 1
		case y.EstStart == nil:
			return -1
		}
		if c := x.EstStart.Compare(*y.EstStart); c != 0 {
			return c
		}
		return cmpInt64(x.JobID, y.JobID)
	})
	for i := range b.Queued {
		b.Queued[i].Position = i + 1
	}
	slices.SortStableFunc(b.Runs, func(x, y CIRun) int {
		switch {
		case x.EstFinish == nil && y.EstFinish == nil:
			return x.Created.Compare(y.Created)
		case x.EstFinish == nil:
			return -1
		case y.EstFinish == nil:
			return 1
		}
		return y.EstFinish.Compare(*x.EstFinish)
	})

	for _, sp := range busySpans {
		b.Summary.BusySeconds24h += int64(spanIn(sp, from, now).Seconds())
	}
	if span := now.Sub(from).Seconds(); poolCap > 0 && span > 0 {
		b.Summary.Utilization24h = busyShare(float64(b.Summary.BusySeconds24h) / (float64(poolCap) * span))
	}
	for _, j := range b.Queued {
		if j.ReadyAt != nil && j.Status == "waiting" {
			waitSpans = append(waitSpans, [2]time.Time{*j.ReadyAt, now})
		}
	}
	b.Timeline = ciTimeline(from, now, busySpans, waitSpans)
	shown := map[string]bool{}
	for _, r := range repoRow {
		if !r.Hidden {
			shown[fmt.Sprintf("%d:%s", r.ForgeID, strings.ToLower(r.Repo))] = true
		}
	}
	faults := runnerFaultAlerts(s.runnerFaults.since(now.Add(-runnerFaultWindow)), func(forgeID int64, repo string) bool {
		return shown[fmt.Sprintf("%d:%s", forgeID, strings.ToLower(repo))]
	})
	for i := range repoRow {
		if repoRow[i].Hidden {
			repoRow[i].Repo, repoRow[i].Error = "", ""
		}
	}
	b.Repos = repoRow
	slices.SortStableFunc(b.Repos, func(x, y CIRepo) int { return cmpInt64(y.Busy, x.Busy) })
	b.Alerts = append(faults, ciAlerts(b, snap)...)
	return b
}

// ciNeedDone: a need names a job of the run that already finished.
func ciNeedDone(jobs []ActionJob, need string) bool {
	return slices.ContainsFunc(jobs, func(o ActionJob) bool { return strings.EqualFold(o.Name, need) && !ciPending(o.Status) })
}

// ciRef describes the run a job is of; hidden ones keep only their timing.
func (s *Service) ciRef(snap *ciSnapshot, d *ciRepoData, ok bool, key string, run ActionRun, haveRun bool, t ActionTask) CIRef {
	r := CIRef{ForgeID: d.ref.forge.ID, Repo: d.ref.repo, Project: key, Workflow: t.WorkflowID, RunNumber: t.RunNumber,
		Title: t.Title, Ref: t.Branch, Event: t.Event, SHA: t.SHA, URL: t.URL}
	if haveRun {
		r.Workflow, r.RunID, r.RunNumber, r.Title, r.Ref, r.Event, r.SHA, r.Created =
			run.WorkflowID, run.ID, run.Index, run.Title, run.PrettyRef, run.Event, run.CommitSHA, run.Created
		if r.URL == "" {
			r.URL = run.HTMLURL
		}
	}
	if pr, found := snap.prs[ciPRKey(d.ref.forge.ID, d.ref.repo, r.SHA)]; found {
		r.PR = &pr
	}
	if !ok {
		return CIRef{ForgeID: r.ForgeID, Hidden: true, Workflow: r.Workflow, Event: r.Event, Created: r.Created}
	}
	return r
}

// jobReadyAt is when an unfinished job became ready for a runner: when its
// run was made, or when the last of its run's jobs before it ended (its
// needs).
func jobReadyAt(d *ciRepoData, run ActionRun, j ActionJob) time.Time {
	ready := run.Created
	if j.Status == "waiting" && len(j.Needs) > 0 && d.hist != nil {
		for _, t := range d.hist.tasks {
			if t.RunNumber == run.Index && !ciPending(t.Status) && t.Updated.After(ready) {
				ready = t.Updated
			}
		}
	}
	return ready
}

// ciPoolModel is the pool the ETAs use: the admin's, else the forge's
// online runners sharing the busiest moment's slots, else one runner that
// takes anything with that many.
func ciPoolModel(configured []CIPoolRunner, forge []ActionRunner, observed int) (string, []CIPoolRunner) {
	if len(configured) > 0 {
		return "configured", configured
	}
	var online []ActionRunner
	for _, r := range forge {
		if r.Status != "offline" {
			online = append(online, r)
		}
	}
	if len(online) > 0 {
		each := (observed + len(online) - 1) / len(online)
		out := make([]CIPoolRunner, len(online))
		for i, r := range online {
			out[i] = CIPoolRunner{Name: r.Name, Labels: r.Labels, Capacity: max(each, 1)}
		}
		return "runners", out
	}
	return "observed", []CIPoolRunner{{Name: "pool", Capacity: observed, Labels: nil}}
}

func spanIn(sp [2]time.Time, from, to time.Time) time.Duration {
	a, b := later(sp[0], from), sp[1]
	if b.After(to) {
		b = to
	}
	if b.Before(a) {
		return 0
	}
	return b.Sub(a)
}

// ciTimeline averages how many jobs ran and waited in each slice.
// busyShare is how busy a pool is, between 0 and 1. Busy time is measured
// against today's capacity: jobs that ran when more slots were up, or past a
// runner's slot count, add up to more than the pool holds now, and "130% busy"
// would read as a number about the pool when it is only that mismatch. The
// busy seconds beside it stay unclamped.
func busyShare(x float64) float64 { return min(max(x, 0), 1) }

func ciTimeline(from, to time.Time, busy, waiting [][2]time.Time) []CIPoint {
	start := from.Truncate(ciBucket)
	var out []CIPoint
	for at := start; at.Before(to); at = at.Add(ciBucket) {
		end := at.Add(ciBucket)
		if end.After(to) {
			end = to
		}
		width := end.Sub(at).Seconds()
		if width <= 0 {
			break
		}
		p := CIPoint{At: at}
		for _, sp := range busy {
			p.Running += spanIn(sp, at, end).Seconds() / width
		}
		for _, sp := range waiting {
			p.Waiting += spanIn(sp, at, end).Seconds() / width
		}
		out = append(out, p)
	}
	return out
}

// ciAlerts says what looks wrong.
func ciAlerts(b *CIBoard, snap *ciSnapshot) []CIAlert {
	out := []CIAlert{}
	now := b.GeneratedAt
	for _, p := range b.Pools {
		for _, r := range p.Forge {
			if r.Status == "offline" {
				out = append(out, CIAlert{Level: "warn", Title: fmt.Sprintf("Runner %s is offline", r.Name),
					Body: fmt.Sprintf("It offers %s; jobs that only it can take wait until it is back.", strings.Join(r.Labels, ", "))})
			}
		}
	}
	if b.Summary.Waiting > 0 && b.Summary.Running == 0 && b.Summary.OldestWaiting != nil && now.Sub(*b.Summary.OldestWaiting) > 2*time.Minute {
		out = append(out, CIAlert{Level: "error", Title: "Nothing is running, but jobs are waiting",
			Body: fmt.Sprintf("%d job%s wait, the oldest for %s. Are the runners up and connected?", b.Summary.Waiting, plural(b.Summary.Waiting), ShortDuration(now.Sub(*b.Summary.OldestWaiting)))})
	}
	labels := map[string]int{}
	for _, j := range b.Queued {
		if j.NoRunner {
			labels[strings.Join(j.RunsOn, ", ")]++
		}
	}
	for l, n := range labels {
		out = append(out, CIAlert{Level: "error", Title: fmt.Sprintf("%d job%s %s for runs-on %s, which no runner offers", n, plural(n), map[bool]string{true: "asks", false: "ask"}[n == 1], l),
			Body: "They wait forever. Change the workflow's runs-on, or the pool's labels."})
	}
	for _, j := range b.Running {
		if j.Overrun && j.Duration > 300 && float64(j.Duration) > 1.5*float64(max(j.Slow, 1)) {
			out = append(out, CIAlert{Level: "warn", Title: fmt.Sprintf("%s has run %s; it usually takes %s", ciJobLabel(j), ShortDuration(secs(float64(j.Duration))), ShortDuration(secs(float64(j.Typical)))),
				Body: "Stuck, or slower than it was? Its log says.", URL: j.URL})
		}
	}
	if b.Summary.OldestWaiting != nil && now.Sub(*b.Summary.OldestWaiting) > 30*time.Minute {
		out = append(out, CIAlert{Level: "info", Title: fmt.Sprintf("The oldest waiting job has waited %s", ShortDuration(now.Sub(*b.Summary.OldestWaiting)))})
	}
	out = append(out, ciHostAlerts(b.Hosts)...)
	for _, p := range b.Pools {
		if p.Source != "configured" && p.Source != "reported" {
			r := snap.runners[p.ForgeID]
			why := "the forge's runners, each given an even share of the busiest moment"
			if p.Source == "observed" {
				why = "one runner as busy as the busiest moment of the last day"
				if r.err != nil {
					why += " (" + r.err.Error() + ")"
				}
			}
			out = append(out, CIAlert{Level: "info", Title: "ETAs guess the runner pool of " + p.ForgeName,
				Body: "They model it as " + why + ". Set the pool's runners, labels and capacities for better ETAs."})
		}
	}
	for _, r := range b.Repos {
		if r.Error != "" && !r.Hidden {
			out = append(out, CIAlert{Level: "warn", Title: "Couldn't read Actions of " + r.Repo, Body: r.Error})
		}
	}
	return out
}

func ciJobLabel(j CIJob) string {
	if j.Hidden {
		return "A job of another project"
	}
	return j.Repo + " " + j.Name
}

// ShortDuration is 45s, 12m or 1h05m.
func ShortDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// ciPools reads the configured pools.
func (s *Service) ciPools(ctx context.Context) (map[int64][]CIPoolRunner, error) {
	var v string
	err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, ciPoolKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return map[int64][]CIPoolRunner{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[int64][]CIPoolRunner{}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return map[int64][]CIPoolRunner{}, nil //nolint:nilerr // a damaged setting is as good as none
	}
	return out, nil
}

// SetCIPool sets the runners of a forge's pool for the board's ETAs
// (admins, and their agents); none clears it. Hosts that report replace
// their entries (reportedPool).
func (s *Service) SetCIPool(ctx context.Context, actor *Actor, forgeID int64, runners []CIPoolRunner) error {
	if !actor.isAdmin() {
		return forbidden("only admins can set the CI runner pool")
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return err
	}
	clean := []CIPoolRunner{}
	for _, r := range runners {
		r.Name = strings.TrimSpace(r.Name)
		var labels []string
		for _, l := range r.Labels {
			if l = strings.TrimSpace(l); l != "" && !slices.Contains(labels, l) {
				labels = append(labels, l)
			}
		}
		r.Labels = labels
		if r.Name == "" {
			return invalid("every runner needs a name")
		}
		if r.Capacity < 1 || r.Capacity > 256 {
			return invalid("a runner's capacity is how many jobs it runs at once: 1 to 256")
		}
		clean = append(clean, r)
	}
	pools, err := s.ciPools(ctx)
	if err != nil {
		return err
	}
	if len(clean) == 0 {
		delete(pools, forgeID)
	} else {
		pools[forgeID] = clean
	}
	err = s.write(ctx, actor, func(t *tx) error {
		b, err := json.Marshal(pools)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, ciPoolKey, string(b)); err != nil {
			return err
		}
		detail := "cleared, so ETAs guess it"
		if len(clean) > 0 {
			var parts []string
			for _, r := range clean {
				parts = append(parts, fmt.Sprintf("%s ×%d [%s]", r.Name, r.Capacity, strings.Join(r.Labels, ",")))
			}
			detail = strings.Join(parts, "; ")
		}
		t.audit(AuditEntry{Action: "settings.ci_pool", Category: AuditSettings, TargetType: "setting",
			Target: "CI runner pool of " + f.Name, URL: "/ci", Detail: detail})
		return nil
	})
	if err != nil {
		return err
	}
	s.ciBoard.mu.Lock()
	s.ciBoard.snap = nil
	s.ciBoard.mu.Unlock()
	return nil
}
