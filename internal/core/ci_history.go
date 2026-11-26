package core

import (
	stdcmp "cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	ciBackfillKey     = "ci_history_backfill:"
	ciRetierKey       = "ci_history_retier:"
	ciBackfillPages   = 200
	ciRetierBatch     = 200
	ciHistoryMaxDays  = 730
	ciHistoryDefault  = 30
	ciHistoryCompare  = 7 * 24 * time.Hour
	ciHistoryJobsMax  = 25
	ciHistoryWeekFrom = 120
)

// ciHistoryBusy keeps two scheduled reads or backfills from overlapping.
var ciHistoryBusy, ciBackfillBusy, ciRetierBusy atomic.Bool

// recordCIHistory keeps the finished runs and tasks of a fresh read.
func (s *Service) recordCIHistory(ctx context.Context, forgeID int64, repo string, tasks []ActionTask, runs map[int64]ActionRun) error {
	errRuns := s.recordCIRuns(ctx, forgeID, repo, runs)
	if errRuns != nil {
		s.log.WarnContext(ctx, "record CI runs", "repo", repo, "err", errRuns)
	}
	errTasks := s.recordCITasks(ctx, forgeID, repo, tasks, runs)
	if errTasks != nil {
		s.log.WarnContext(ctx, "record CI jobs", "repo", repo, "err", errTasks)
	}
	return errors.Join(errRuns, errTasks)
}

func (s *Service) recordCIRuns(ctx context.Context, forgeID int64, repo string, runs map[int64]ActionRun) error {
	if len(runs) == 0 {
		return nil
	}
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range runs {
		if ciPending(r.Status) || !validTime(r.Created) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ci_runs(forge_id, repo, number, workflow, event, ref, sha, title, tier, status, created_at, started_at, stopped_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(forge_id, repo, number) DO UPDATE SET status = excluded.status, started_at = excluded.started_at, stopped_at = excluded.stopped_at`,
			forgeID, repo, r.Index, r.WorkflowID, r.Event, r.PrettyRef, r.CommitSHA, r.Title, r.tier(), r.Status,
			r.Created.UnixMilli(), msOrNil(r.Started), msOrNil(r.Stopped)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func msOrNil(t time.Time) any {
	if !validTime(t) {
		return nil
	}
	return t.UnixMilli()
}

// recordCITasks keeps the finished tasks not kept yet, each with how long
// it waited for a runner: from when it was ready (its run was made, or the
// last task of its run before it ended, as the board reckons) to its start.
// A run's creation comes from runs, else from ci_runs.
func (s *Service) recordCITasks(ctx context.Context, forgeID int64, repo string, tasks []ActionTask, runs map[int64]ActionRun) error {
	var done []ActionTask
	first := time.Time{}
	for _, t := range tasks {
		if ciPending(t.Status) || !validTime(t.Started) || !validTime(t.Updated) {
			continue
		}
		done = append(done, t)
		if first.IsZero() || t.Started.Before(first) {
			first = t.Started
		}
	}
	if len(done) == 0 {
		return nil
	}
	kept := map[int64]bool{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT task_id FROM ci_jobs WHERE forge_id = ? AND repo = ? AND started_at >= ?`, forgeID, repo, first.UnixMilli())
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			kept[id] = true
		}
	}
	rows.Close()
	done = slices.DeleteFunc(done, func(t ActionTask) bool { return kept[t.ID] })
	if len(done) == 0 {
		return nil
	}
	created := map[int64]time.Time{}
	for _, t := range done {
		if _, ok := created[t.RunNumber]; ok {
			continue
		}
		if r, ok := runs[t.RunNumber]; ok && validTime(r.Created) {
			created[t.RunNumber] = r.Created
			continue
		}
		var ms int64
		switch err := s.db.R.QueryRowContext(ctx, `SELECT created_at FROM ci_runs WHERE forge_id = ? AND repo = ? AND number = ?`, forgeID, repo, t.RunNumber).Scan(&ms); {
		case err == nil:
			created[t.RunNumber] = msTime(ms)
		case errors.Is(err, sql.ErrNoRows):
			created[t.RunNumber] = time.Time{}
		default:
			return err
		}
	}
	byRun := map[int64][]ActionTask{}
	for _, t := range tasks {
		byRun[t.RunNumber] = append(byRun[t.RunNumber], t)
	}
	drafts, err := s.keptDraftRuns(ctx, forgeID, repo, done, runs)
	if err != nil {
		return err
	}
	tiers := ciHistory{runs: runs, draftRuns: drafts}
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range done {
		var wait any
		if ready := created[t.RunNumber]; validTime(ready) {
			for _, o := range byRun[t.RunNumber] {
				if o.ID != t.ID && !ciPending(o.Status) && o.Updated.Before(t.Created) && o.Updated.After(ready) {
					ready = o.Updated
				}
			}
			wait = int64(max(0, t.Started.Sub(ready).Seconds()))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, workflow, name, event, ref, sha, tier, status, started_at, ended_at, wait_s)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			forgeID, repo, t.ID, t.RunNumber, t.WorkflowID, t.Name, t.Event, t.Branch, t.SHA, tiers.tierOf(t), t.Status,
			t.Started.UnixMilli(), max(t.Updated.UnixMilli(), t.Started.UnixMilli()), wait); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// keptDraftRuns finds which runs named by tasks but missing from runs are
// kept at the draft tier.
func (s *Service) keptDraftRuns(ctx context.Context, forgeID int64, repo string, tasks []ActionTask, runs map[int64]ActionRun) (map[int64]bool, error) {
	out := map[int64]bool{}
	var lo, hi int64
	for _, t := range tasks {
		if _, ok := runs[t.RunNumber]; ok {
			continue
		}
		if lo == 0 || t.RunNumber < lo {
			lo = t.RunNumber
		}
		hi = max(hi, t.RunNumber)
	}
	if hi == 0 {
		return out, nil
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT number FROM ci_runs WHERE forge_id = ? AND repo = ? AND number BETWEEN ? AND ? AND tier = ?`, forgeID, repo, lo, hi, ciDraftTier)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// RecordCIHistory reads each attached repo's recent runs and tasks, which
// keeps the finished ones, so the history has no holes while nobody looks
// at the CI page; then re-runs a base branch's runner-fault failures
// (WP-551) and keeps the test evidence of finished jobs (WP-704).
func (s *Service) RecordCIHistory(ctx context.Context) {
	cl, ok := s.Forges.(ForgeActions)
	if !ok {
		jobSkip(ctx, "the forge client cannot read CI runs")
		return
	}
	if !ciHistoryBusy.CompareAndSwap(false, true) {
		jobBusy(ctx, "the last pass is still going")
		return
	}
	defer ciHistoryBusy.Store(false)
	repos, err := s.ciRepos(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "CI history: list repos", "err", err)
		return
	}
	direct := func(fn func() error) error { return fn() }
	for _, r := range repos {
		if r.forge.token == "" || ctx.Err() != nil {
			continue
		}
		h := s.ciRepoHistory(ctx, cl, r.forge, r.repo, direct)
		if h.err != nil {
			if !errors.Is(h.err, ErrNotFound) {
				s.log.Debug("CI history: read", "repo", r.repo, "err", h.err)
			}
			continue
		}
		if err := s.readStepFailed(ctx, r.forge, r.repo); err != nil && ctx.Err() == nil {
			s.log.Debug("CI history: read cancelled jobs' logs", "repo", r.repo, "err", err)
		}
		s.checkBaseFaults(ctx, r.forge, r.repo, h.runs)
		if err := s.readCIEvidence(ctx, r.forge, r.repo); err != nil && ctx.Err() == nil {
			s.log.Debug("CI history: read jobs' evidence", "repo", r.repo, "err", err)
		}
		s.watchCILimits(ctx, r.forge, r.repo)
	}
}

// BackfillCIHistory reads, once per repo, every run and task the forge
// still lists (up to ciBackfillPages pages of each), so the history starts
// before Waypoint began keeping it. A repo whose read fails is tried again
// the next hour.
func (s *Service) BackfillCIHistory(ctx context.Context) {
	cl, ok := s.Forges.(ForgeActions)
	if !ok {
		jobSkip(ctx, "the forge client cannot read CI runs")
		return
	}
	if !ciBackfillBusy.CompareAndSwap(false, true) {
		jobBusy(ctx, "the last backfill is still going")
		return
	}
	defer ciBackfillBusy.Store(false)
	repos, err := s.ciRepos(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "CI history backfill: list repos", "err", err)
		return
	}
	for _, r := range repos {
		if r.forge.token == "" || ctx.Err() != nil {
			continue
		}
		key := ciBackfillKey + strconv.FormatInt(r.forge.ID, 10) + "|" + strings.ToLower(r.repo)
		var v string
		if s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, key).Scan(&v) == nil {
			continue
		}
		runs, tasks, err := s.ciBackfillRead(ctx, cl, r.forge, r.repo)
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.log.WarnContext(ctx, "CI history backfill", "repo", r.repo, "err", err)
			continue
		}
		if s.recordCIHistory(ctx, r.forge.ID, r.repo, tasks, runs) != nil {
			continue
		}
		var oldest time.Time
		if len(tasks) >= ciBackfillPages*50 {
			oldest = tasks[len(tasks)-1].Created
		}
		s.noteCIWhole(ctx, r.forge.ID, r.repo, oldest)
		if _, err := s.db.W.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, itoa64(s.nowMS())); err != nil {
			s.log.WarnContext(ctx, "best-effort write failed", "err", err)
		}
		s.log.Info("CI history backfilled", "repo", r.repo, "runs", len(runs), "tasks", len(tasks))
	}
}

func (s *Service) ciBackfillRead(ctx context.Context, cl ForgeActions, f *Forge, repo string) (map[int64]ActionRun, []ActionTask, error) {
	runs, err := ciAllRuns(ctx, cl, f, repo)
	if err != nil {
		return nil, nil, err
	}
	var tasks []ActionTask
	seen := map[int64]bool{}
	for page := 1; page <= ciBackfillPages; page++ {
		list, err := cl.ActionTasks(ctx, f, repo, nil, page, 50)
		if err != nil {
			return nil, nil, err
		}
		for _, t := range list {
			if !seen[t.ID] {
				seen[t.ID] = true
				tasks = append(tasks, t)
			}
		}
		if len(list) < 50 {
			break
		}
	}
	return runs, tasks, nil
}

// ciAllRuns reads every run the forge still lists, up to ciBackfillPages.
func ciAllRuns(ctx context.Context, cl ForgeActions, f *Forge, repo string) (map[int64]ActionRun, error) {
	runs := map[int64]ActionRun{}
	for page := 1; page <= ciBackfillPages; page++ {
		list, err := cl.ActionRuns(ctx, f, repo, nil, page, 50)
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			runs[r.Index] = r
		}
		if len(list) < 50 {
			break
		}
	}
	return runs, nil
}

// RetierCIHistory corrects, once per repo, the tier of what was kept before
// WP-849, when no run was ever called a draft. It asks the forge which of the
// runs it still lists were started for a draft pull request and marks those
// runs, their jobs and their tests' results. It only moves rows from full to
// draft, so a run the forge no longer lists stays as it was. A repo whose
// read fails is tried again the next hour.
func (s *Service) RetierCIHistory(ctx context.Context) {
	cl, ok := s.Forges.(ForgeActions)
	if !ok {
		jobSkip(ctx, "the forge client cannot read CI runs")
		return
	}
	if !ciRetierBusy.CompareAndSwap(false, true) {
		jobBusy(ctx, "the last re-tier is still going")
		return
	}
	defer ciRetierBusy.Store(false)
	repos, err := s.ciRepos(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "CI history re-tier: list repos", "err", err)
		return
	}
	for _, r := range repos {
		if r.forge.token == "" || ctx.Err() != nil {
			continue
		}
		key := ciRetierKey + strconv.FormatInt(r.forge.ID, 10) + "|" + strings.ToLower(r.repo)
		var v string
		if s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, key).Scan(&v) == nil {
			continue
		}
		runs, err := ciAllRuns(ctx, cl, r.forge, r.repo)
		if err != nil {
			s.log.WarnContext(ctx, "CI history re-tier: read runs", "repo", r.repo, "err", err)
			continue
		}
		var drafts []int64
		for n, run := range runs {
			if run.Draft {
				drafts = append(drafts, n)
			}
		}
		if err := s.markDraftRuns(ctx, r.forge.ID, r.repo, drafts, key); err != nil {
			s.log.WarnContext(ctx, "CI history re-tier", "repo", r.repo, "err", err)
			continue
		}
		s.log.Info("CI history re-tiered", "repo", r.repo, "runs", len(runs), "draft", len(drafts))
	}
}

// markDraftRuns moves a repo's kept runs to the draft tier, then notes that it
// was done. Other writers wait on each statement, so it isn't one transaction
// and a test's results are written a batch of tests at a time; a pass that
// stops part way is repeated whole, which changes only what's left.
func (s *Service) markDraftRuns(ctx context.Context, forgeID int64, repo string, numbers []int64, key string) error {
	if len(numbers) > 0 {
		list, err := json.Marshal(numbers)
		if err != nil {
			return err
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE ci_runs SET tier = ? WHERE tier = '' AND forge_id = ? AND repo = ?
			AND number IN (SELECT value FROM json_each(?))`, ciDraftTier, forgeID, repo, string(list)); err != nil {
			return err
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE ci_jobs SET tier = ? WHERE tier = '' AND forge_id = ? AND repo = ?
			AND run_number IN (SELECT value FROM json_each(?))`, ciDraftTier, forgeID, repo, string(list)); err != nil {
			return err
		}
		if err := s.markDraftTests(ctx, forgeID, repo); err != nil {
			return err
		}
	}
	_, err := s.db.W.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, itoa64(s.nowMS()))
	return err
}

// markDraftTests moves to the draft tier the results of a repo's tests in its
// draft jobs. Nothing indexes ci_test_runs by job, so it walks them by test;
// the + keeps SQLite from seeking every test and job pair instead.
func (s *Service) markDraftTests(ctx context.Context, forgeID int64, repo string) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM ci_tests WHERE forge_id = ? AND repo = ?`, forgeID, repo)
	if err != nil {
		return err
	}
	defer rows.Close()
	var tests []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		tests = append(tests, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for batch := range slices.Chunk(tests, ciRetierBatch) {
		list, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE ci_test_runs SET tier = ? WHERE tier = ''
			AND test_id IN (SELECT value FROM json_each(?))
			AND +task_id IN (SELECT task_id FROM ci_jobs WHERE forge_id = ? AND repo = ? AND tier = ?)`,
			ciDraftTier, string(list), forgeID, repo, ciDraftTier); err != nil {
			return err
		}
	}
	return nil
}

// CIHistoryInput picks what CIHistory covers: one repo or all, how many
// days back, and day or week buckets.
type CIHistoryInput struct {
	Repo   string
	Days   int
	Bucket string // day, week; '' picks by Days
}

// CIHistory is how CI fared over time (the CI page's history, wp ci
// history). Durations are seconds; a nil percentile had no samples.
type CIHistory struct {
	GeneratedAt time.Time          `json:"generated_at"`
	From        time.Time          `json:"from"`
	To          time.Time          `json:"to"`
	Bucket      string             `json:"bucket"`
	Days        int                `json:"days"`
	Repo        string             `json:"repo,omitempty"`
	Repos       []string           `json:"repos"`           // the repos you can see that have history
	Since       *time.Time         `json:"since,omitempty"` // the oldest job kept of those
	Points      []CIHistoryPoint   `json:"points"`
	Now         CIHistoryStats     `json:"now"`    // the last seven days
	Before      CIHistoryStats     `json:"before"` // the seven before them
	Jobs        []CIHistoryJobDiff `json:"jobs"`   // most runner time now first
}

// CIHistoryStats is one bucket's or period's numbers.
type CIHistoryStats struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Jobs      int       `json:"jobs"` // finished, not skipped
	Failed    int       `json:"failed"`
	Cancelled int       `json:"cancelled"`
	Busy      int64     `json:"busy"` // runner seconds
	WaitP50   *int64    `json:"wait_p50"`
	WaitP90   *int64    `json:"wait_p90"`
	// Full-tier runs (not the draft tier): how many finished, and the wall
	// time of the green ones, made to done, runner waits included.
	Runs   int    `json:"runs"`
	RunP50 *int64 `json:"run_p50"`
	RunP90 *int64 `json:"run_p90"`
	// Merges of the repos' pull requests, and the time each took in its
	// merge line: from its babysit's start (or opening), or the merge
	// before it when later, to its merge (as wp ci doctor reckons it).
	Merges   int    `json:"merges"`
	MergeP50 *int64 `json:"merge_p50"`
	// OpenMergeP50 is opened to merged, whole: what a pull request waited
	// from its opening, `wp pr flow` splits it by stage (WP-1910).
	OpenMergeP50 *int64 `json:"open_merge_p50"`
}

// CIHistoryPoint is one bucket.
type CIHistoryPoint struct {
	At time.Time `json:"at"`
	CIHistoryStats
}

// CIHistoryJobDiff is one job's full-tier numbers now and before.
type CIHistoryJobDiff struct {
	Repo         string `json:"repo"`
	Workflow     string `json:"workflow"`
	Name         string `json:"name"`
	Runs         int    `json:"runs"`
	Failed       int    `json:"failed"`
	P50          *int64 `json:"p50"`
	Busy         int64  `json:"busy"`
	BeforeRuns   int    `json:"before_runs"`
	BeforeFailed int    `json:"before_failed"`
	BeforeP50    *int64 `json:"before_p50"`
}

// ciStatsAcc gathers one bucket's samples.
type ciStatsAcc struct {
	st                       CIHistoryStats
	waits, runs, merge, open []float64
}

func (a *ciStatsAcc) done() CIHistoryStats {
	st := a.st
	st.WaitP50, st.WaitP90 = pctPtr(a.waits, 50), pctPtr(a.waits, 90)
	st.RunP50, st.RunP90 = pctPtr(a.runs, 50), pctPtr(a.runs, 90)
	st.MergeP50, st.OpenMergeP50 = pctPtr(a.merge, 50), pctPtr(a.open, 50)
	return st
}

func pctPtr(xs []float64, p float64) *int64 {
	if len(xs) == 0 {
		return nil
	}
	v := int64(percentile(xs, p))
	return &v
}

// ciBucketStart is the UTC day, or the Monday of its week, t falls in.
func ciBucketStart(t time.Time, bucket string) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if bucket == "week" {
		d = d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
	}
	return d
}

func ciBucketNext(t time.Time, bucket string) time.Time {
	if bucket == "week" {
		return t.AddDate(0, 0, 7)
	}
	return t.AddDate(0, 0, 1)
}

// CIHistory buckets the kept runs, jobs and merges of the repos actor can
// see.
func (s *Service) CIHistory(ctx context.Context, actor *Actor, in CIHistoryInput) (*CIHistory, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	days := in.Days
	if days <= 0 {
		days = ciHistoryDefault
	}
	if days > ciHistoryMaxDays {
		return nil, invalid("days: at most %d", ciHistoryMaxDays)
	}
	bucket := in.Bucket
	switch bucket {
	case "":
		bucket = "day"
		if days >= ciHistoryWeekFrom {
			bucket = "week"
		}
	case "day", "week":
	default:
		return nil, invalid("bucket: day or week")
	}
	visible, err := s.ciVisibleRepos(ctx, actor)
	if err != nil {
		return nil, err
	}
	want := func(forgeID int64, repo string) (string, bool) {
		r, ok := visible[strconv.FormatInt(forgeID, 10)+"|"+strings.ToLower(repo)]
		name := r.repo
		if !ok || (in.Repo != "" && !strings.EqualFold(in.Repo, name)) {
			return "", false
		}
		return name, true
	}

	now := s.now().UTC()
	h := &CIHistory{GeneratedAt: now, Bucket: bucket, Days: days, Repo: in.Repo, Repos: []string{}, Points: []CIHistoryPoint{}, Jobs: []CIHistoryJobDiff{}}
	h.From = ciBucketStart(now.AddDate(0, 0, -(days-1)), bucket)
	h.To = now
	nowFrom, beforeFrom := now.Add(-ciHistoryCompare), now.Add(-2*ciHistoryCompare)
	from := h.From
	if beforeFrom.Before(from) {
		from = beforeFrom
	}

	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, repo, min(started_at) FROM ci_jobs GROUP BY forge_id, repo`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fid, first int64
		var repo string
		if err := rows.Scan(&fid, &repo, &first); err != nil {
			rows.Close()
			return nil, err
		}
		r, ok := visible[strconv.FormatInt(fid, 10)+"|"+strings.ToLower(repo)]
		if !ok {
			continue
		}
		name := r.repo
		if !slices.Contains(h.Repos, name) {
			h.Repos = append(h.Repos, name)
		}
		if _, ok := want(fid, repo); ok && (h.Since == nil || msTime(first).Before(*h.Since)) {
			h.Since = tptr(msTime(first))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(h.Repos, func(a, b string) int { return stdcmp.Compare(strings.ToLower(a), strings.ToLower(b)) })

	var accs []*ciStatsAcc
	for t := h.From; t.Before(now); t = ciBucketNext(t, bucket) {
		end := ciBucketNext(t, bucket)
		if end.After(now) {
			end = now
		}
		accs = append(accs, &ciStatsAcc{st: CIHistoryStats{From: t, To: end}})
	}
	nowAcc := &ciStatsAcc{st: CIHistoryStats{From: nowFrom, To: now}}
	beforeAcc := &ciStatsAcc{st: CIHistoryStats{From: beforeFrom, To: nowFrom}}
	into := func(t time.Time) []*ciStatsAcc {
		var out []*ciStatsAcc
		if !t.Before(h.From) && t.Before(now) {
			i := 0
			if bucket == "day" {
				i = int(t.Sub(h.From) / (24 * time.Hour))
			} else {
				i = int(t.Sub(h.From) / (7 * 24 * time.Hour))
			}
			if i >= 0 && i < len(accs) {
				out = append(out, accs[i])
			}
		}
		switch {
		case !t.Before(nowFrom) && !t.After(now):
			out = append(out, nowAcc)
		case !t.Before(beforeFrom) && t.Before(nowFrom):
			out = append(out, beforeAcc)
		}
		return out
	}

	type jobAcc struct {
		CIHistoryJobDiff
		durs, before []float64
	}
	jobs := map[string]*jobAcc{}
	rows, err = s.db.R.QueryContext(ctx, `SELECT forge_id, repo, workflow, name, tier, status, started_at, ended_at, wait_s
		FROM ci_jobs WHERE started_at >= ? AND status != 'skipped'`, from.UnixMilli())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fid, started, ended int64
		var repo, workflow, name, tier, status string
		var wait sql.NullInt64
		if err := rows.Scan(&fid, &repo, &workflow, &name, &tier, &status, &started, &ended, &wait); err != nil {
			rows.Close()
			return nil, err
		}
		repoName, ok := want(fid, repo)
		if !ok {
			continue
		}
		start := msTime(started)
		dur := float64(max(0, ended-started)) / 1000
		for _, a := range into(start) {
			a.st.Jobs++
			a.st.Busy += int64(dur)
			switch status {
			case "failure":
				a.st.Failed++
			case "cancelled":
				a.st.Cancelled++
			}
			if wait.Valid {
				a.waits = append(a.waits, float64(wait.Int64))
			}
		}
		if tier != "" || start.Before(beforeFrom) {
			continue
		}
		k := ciJobKey(repoName, workflow, name)
		j := jobs[k]
		if j == nil {
			j = &jobAcc{CIHistoryJobDiff: CIHistoryJobDiff{Repo: repoName, Workflow: workflow, Name: name}}
			jobs[k] = j
		}
		if start.Before(nowFrom) {
			j.BeforeRuns++
			if status == "failure" {
				j.BeforeFailed++
			}
			if status == "success" {
				j.before = append(j.before, dur)
			}
			continue
		}
		j.Runs++
		j.Busy += int64(dur)
		if status == "failure" {
			j.Failed++
		}
		if status == "success" {
			j.durs = append(j.durs, dur)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.Runs == 0 {
			continue
		}
		j.P50, j.BeforeP50 = pctPtr(j.durs, 50), pctPtr(j.before, 50)
		h.Jobs = append(h.Jobs, j.CIHistoryJobDiff)
	}
	slices.SortFunc(h.Jobs, func(a, b CIHistoryJobDiff) int {
		return stdcmp.Or(cmpInt64(b.Busy, a.Busy), stdcmp.Compare(a.Repo+a.Workflow+a.Name, b.Repo+b.Workflow+b.Name))
	})
	if len(h.Jobs) > ciHistoryJobsMax {
		h.Jobs = h.Jobs[:ciHistoryJobsMax]
	}

	rows, err = s.db.R.QueryContext(ctx, `SELECT forge_id, repo, status, created_at, stopped_at FROM ci_runs
		WHERE created_at >= ? AND tier = '' AND status IN ('success', 'failure', 'cancelled')`, from.UnixMilli())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fid, created int64
		var stopped sql.NullInt64
		var repo, status string
		if err := rows.Scan(&fid, &repo, &status, &created, &stopped); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := want(fid, repo); !ok {
			continue
		}
		for _, a := range into(msTime(created)) {
			a.st.Runs++
			if status == "success" && stopped.Valid && stopped.Int64 > created {
				a.runs = append(a.runs, float64(stopped.Int64-created)/1000)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := s.ciHistoryMerges(ctx, from, want, into); err != nil {
		return nil, err
	}

	for i, a := range accs {
		h.Points = append(h.Points, CIHistoryPoint{At: accs[i].st.From, CIHistoryStats: a.done()})
	}
	h.Now, h.Before = nowAcc.done(), beforeAcc.done()
	return h, nil
}

func (s *Service) ciHistoryMerges(ctx context.Context, from time.Time, want func(int64, string) (string, bool), into func(time.Time) []*ciStatsAcc) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.forge_id, p.repo, p.base_ref, p.merged_at,
		  coalesce((SELECT min(b.created_at) FROM babysits b WHERE b.pr_id = p.id), 0), coalesce(p.opened_at, p.updated_at)
		FROM pull_requests p WHERE p.state = 'merged' AND p.merged_at >= ?
		ORDER BY p.merged_at`, from.Add(-7*24*time.Hour).UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	last := map[string]int64{}
	for rows.Next() {
		var fid, merged, start, opened int64
		var repo, base string
		if err := rows.Scan(&fid, &repo, &base, &merged, &start, &opened); err != nil {
			return err
		}
		if _, ok := want(fid, repo); !ok {
			continue
		}
		line := strconv.FormatInt(fid, 10) + "|" + strings.ToLower(repo) + "|" + base
		openedAt := opened
		begin := stdcmp.Or(start, opened)
		if prev, ok := last[line]; ok && prev > begin {
			begin = prev
		}
		last[line] = merged
		at := msTime(merged)
		if at.Before(from) {
			continue
		}
		for _, a := range into(at) {
			a.st.Merges++
			if merged > begin {
				a.merge = append(a.merge, float64(merged-begin)/1000)
			}
			if merged > openedAt {
				a.open = append(a.open, float64(merged-openedAt)/1000)
			}
		}
	}
	return rows.Err()
}

// ciWholeKey + forge id|repo in server_settings is "from|to" (ms; 0 for
// from: since the first job): the jobs kept of the repo have no hole
// between them, as far as the reads tell.
const ciWholeKey = "ci_history_whole:"

// noteCIWhole records a read that kept every finished job of the repo from
// oldest (zero: from the first) until now, and returns since when the kept
// jobs have no hole: a read that reaches back to the last one carries its
// start on, one that doesn't (Waypoint was down longer than the read
// reaches) starts over. Kept jobs a hole cuts off are left out of the
// board's last day rather than counted short.
func (s *Service) noteCIWhole(ctx context.Context, forgeID int64, repo string, oldest time.Time) time.Time {
	suffix := itoa64(forgeID) + "|" + strings.ToLower(repo)
	from := oldest
	var v string
	switch err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, ciWholeKey+suffix).Scan(&v); {
	case err == nil:
		if pf, pt, ok := parseCIWhole(v); ok && !pt.Before(oldest) && pf.Before(from) {
			from = pf
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		s.log.WarnContext(ctx, "read CI history coverage", "repo", repo, "err", err)
		return oldest
	}
	fromMS := int64(0)
	if !from.IsZero() {
		fromMS = from.UnixMilli()
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ciWholeKey+suffix, itoa64(fromMS)+"|"+itoa64(s.nowMS())); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
	return from
}

func parseCIWhole(v string) (from, to time.Time, ok bool) {
	a, b, found := strings.Cut(v, "|")
	f, errF := strconv.ParseInt(a, 10, 64)
	t, errT := strconv.ParseInt(b, 10, 64)
	if !found || errF != nil || errT != nil {
		return time.Time{}, time.Time{}, false
	}
	if f > 0 {
		from = msTime(f)
	}
	return from, msTime(t), true
}

// keptCIJobs reads the repo's kept jobs that ran in the last day into h:
// those its read left out as kept, and every one's wait.
func (s *Service) keptCIJobs(ctx context.Context, forgeID int64, repo string, since time.Time, h *ciHistory) error {
	live := make(map[int64]bool, len(h.tasks))
	for _, t := range h.tasks {
		live[t.ID] = true
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT task_id, run_number, workflow, name, event, ref, sha, tier, status, started_at, ended_at, wait_s
		FROM ci_jobs WHERE forge_id = ? AND repo = ? AND started_at >= ? AND ended_at >= ?`,
		forgeID, repo, since.Add(-ciTaskLongest).UnixMilli(), since.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	h.kept, h.waits = nil, map[int64]int64{}
	for rows.Next() {
		var k ciKeptJob
		var started, ended int64
		var wait sql.NullInt64
		if err := rows.Scan(&k.ID, &k.RunNumber, &k.WorkflowID, &k.Name, &k.Event, &k.Branch, &k.SHA, &k.tier, &k.Status, &started, &ended, &wait); err != nil {
			return err
		}
		if wait.Valid {
			h.waits[k.ID] = wait.Int64
			k.wait = &wait.Int64
		}
		if live[k.ID] {
			continue
		}
		k.Started, k.Updated = msTime(started), msTime(ended)
		k.Created = k.Started
		h.kept = append(h.kept, k)
	}
	return rows.Err()
}
