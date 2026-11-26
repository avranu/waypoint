package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// scheduled is one scheduled job: what runs, how often and where
// (WP-ADR139). Each is counted and watched by name (job_runs.go), so a step
// that matters is its own job, not a line in a bigger one.
type scheduled struct {
	name    string
	every   time.Duration
	run     func(context.Context)
	lane    string
	atStart bool
}

// bg puts a job in a background lane of its own.
func bg(j scheduled) scheduled {
	j.lane = j.name
	return j
}

// Jobs that time themselves, declared with the schedule so one that stops
// is seen: the name, and the longest it goes between runs.
const (
	jobHealthName   = "job health"
	jobEnvUpkeep    = "environment upkeep"
	jobEnvChecks    = "environment checks"
	jobAssistants   = "assistants"
	jobGoalTraces   = "goal traces"
	goalTracesEvery = time.Hour
)

func selfTimedJobs() []scheduled {
	return []scheduled{
		{name: jobHealthName, every: jobHealthEvery},
		{name: jobEnvUpkeep, every: time.Minute},
		{name: jobEnvChecks, every: envTickInterval},
		{name: jobAssistants, every: assistantCheckEvery},
		{name: jobGoalTraces, every: goalTracesEvery},
		{name: healthWatchdogName, every: healthWatchdogEvery},
	}
}

// schedule is every job the scheduler runs. The order matters twice: jobs
// due together run in it, and so do the jobs of the server's start.
func (s *Service) schedule() []scheduled {
	const (
		hour      = time.Hour
		tenMin    = 10 * time.Minute
		fiveMin   = 5 * time.Minute
		halfMin   = 30 * time.Second
		minute    = time.Minute
		sinceLong = 30 * 24 * time.Hour
	)
	hourly := func(name string, run func(context.Context)) scheduled {
		return scheduled{name: name, every: hour, run: run, atStart: true}
	}
	once := func(name string, run func(context.Context)) scheduled {
		return bg(scheduled{name: name, run: run})
	}
	jobs := []scheduled{
		once("knowledge use backfill", s.BackfillKnowledgeUses),
		once("mention backfill", s.RunMentionBackfill),
		once("forge mention backfill", s.RunForgeMentionBackfill),
		once("review leftovers backfill", s.RunLeftoverBackfill),

		hourly("due reminders", s.errJob("due reminders", s.NotifyDueSoon)),
		hourly("agent run expiry", s.errJob("expire agent runs", s.expireRuns)),
		hourly("stranded tasks", s.errJob("stranded tasks", s.sweepStranded)),
		hourly("unpushed commits", s.errJob("unpushed commits", s.sweepUnpushed)),
		hourly("experiments", s.errJob("experiments", s.sweepExperiments)),
		hourly("request expiry", func(ctx context.Context) {
			if n, err := s.ExpireRequests(ctx); err != nil {
				s.log.WarnContext(ctx, "expire requests", "err", err)
			} else if n > 0 {
				s.log.InfoContext(ctx, "expired requests the agents moved on from", "count", n)
			}
		}),
		hourly("expired rows", func(ctx context.Context) {
			now := s.nowMS()
			for _, p := range []struct {
				table, where string
				before       int64
			}{
				{"device_logins", "expires_at < ?", now},
				{"sessions", "expires_at < ?", now},
				{"user_links", "expires_at < ?", now - sinceLong.Milliseconds()},
				{"notifications", "read_at IS NOT NULL AND created_at < ?", now - 365*24*hour.Milliseconds()},
			} {
				// #nosec G202 -- the table and condition are the fixed text above
				if _, err := s.db.W.ExecContext(ctx, `DELETE FROM `+p.table+` WHERE `+p.where, p.before); err != nil && ctx.Err() == nil {
					s.log.WarnContext(ctx, "prune expired rows", "table", p.table, "err", err)
				}
			}
		}),
		hourly("AI call pruning", s.pruneAICalls),
		hourly("automation run pruning", s.errJob("prune automation runs", s.PruneAutomationRuns)),
		hourly("webhook delivery pruning", s.errJob("prune webhook deliveries", s.pruneDeliveries)),
		hourly("estimates", s.sweepEstimates),
		hourly("stories", s.sweepStories),
		hourly("looks", s.sweepLooks),
		hourly("prep ahead", s.sweepPrepAhead),
		bg(hourly("prep scores", s.ScorePreps)),
		bg(hourly("collector health", s.CheckCollectorHealth)),
		hourly("safety drill", s.maybeSafetyDrill),
		hourly("sandbox drills", s.maybeSandboxDrills),
		hourly("goal recheck", func(context.Context) { s.goalsChanged() }),
		hourly("goal drift", s.startGoalDrift),
		bg(hourly("agent kit", s.RefreshKit)),
		bg(hourly("agent setup", s.RefreshAgentSetup)),
		bg(hourly("recurring mistakes", s.SweepMistakes)),
		{name: "quota guard", every: hour, run: s.RunQuotaGuard, lane: "quota guard", atStart: true},
		{name: "quota guard baselines", every: hour, run: s.refreshGuardBaselines, lane: "quota guard", atStart: true},
		hourly("CI pruning", s.pruneCI),
		bg(hourly("merged branches", s.SweepBranches)),
		bg(hourly("CI history backfill", s.BackfillCIHistory)),
		bg(hourly("CI history re-tier", s.RetierCIHistory)),
		bg(hourly("instruction history backfill", s.BackfillUsageMarks)),
	}
	if s.cfg.BackupDir != "" {
		jobs = append(jobs, hourly("backup", func(ctx context.Context) {
			if err := s.dailyBackup(ctx); err != nil {
				s.log.ErrorContext(ctx, "backup failed", "err", err)
				s.backupFailed(ctx, err)
			}
		}))
	}
	return append(jobs,
		scheduled{name: "timed automations", every: fiveMin, run: s.RunTimedAutomations, atStart: true},
		bg(scheduled{name: "review feedback", every: fiveMin, run: s.ReportReviewFeedback}),
		bg(scheduled{name: "prep trials", every: fiveMin, run: s.RunPrepTrials}),
		bg(scheduled{name: "session facts", every: fiveMin, run: s.RunSessionFacts}),

		scheduled{name: "runners", every: halfMin, run: s.CheckRunners, atStart: true},
		scheduled{name: "auto question notices", every: halfMin, run: s.NotifyAutoQuestions, atStart: true},
		scheduled{name: "front rounds", every: fiveMin, run: s.CheckFrontRounds},
		scheduled{name: "forge rechecks", every: halfMin, run: s.CheckMergeRechecks},

		once("pull requests", s.RefreshPullRequests),
		once("resume babysits", s.ResumeBabysits),

		scheduled{name: "forge sync", every: tenMin, run: s.SyncAllRepos},
		scheduled{name: "armed heads", every: tenMin, run: s.CheckArmedHeads},
		bg(scheduled{name: "CI history", every: tenMin, run: s.RecordCIHistory}),
		bg(scheduled{name: "CI health", every: tenMin, run: s.SweepCIHealth}),

		scheduled{name: "review gates", every: gateRecheck, run: s.CheckGates},
		scheduled{name: "pending checks", every: ciPollEvery, run: s.PollCI},

		scheduled{name: "system resources", every: minute, run: s.SampleResources},
		scheduled{name: "armed merges", every: minute, run: s.CheckArmedMerges},
		bg(scheduled{name: "babysits", every: minute, run: s.CheckBabysits}),
		scheduled{name: "stale babysits", every: minute, run: s.reviveStaleBabysits},
		scheduled{name: "merge lines", every: minute, run: func(ctx context.Context) {
			s.sweepLines(ctx)
			s.watchLines(ctx)
		}},
		scheduled{name: "batched news", every: minute, run: s.releaseDueBatches},
		scheduled{name: "audits", every: minute, run: s.CheckAudits},
		scheduled{name: "objectives", every: minute, run: s.CheckObjectives},
		scheduled{name: "request conditions", every: minute, run: s.CheckRequestWatches},
		scheduled{name: "hold conditions", every: minute, run: s.CheckHoldConditions},
		scheduled{name: "safety screens", every: minute, run: s.sweepScreens},
		scheduled{name: "quota guard watch", every: minute, run: s.RunGuardWatch},
		scheduled{name: "autopilot", every: minute, run: s.RunAutopilot},
		scheduled{name: "deferred offers", every: minute, run: s.deferredSweep},
		scheduled{name: "warm caches", every: minute, run: s.KeepCachesWarm},
		scheduled{name: "work logs", every: minute, run: s.RunWorkLog},
		scheduled{name: "PR descriptions", every: minute, run: s.RunPRBodies},
		scheduled{name: "fan-outs", every: minute, run: s.CheckFanouts},
		scheduled{name: "log secrets", every: minute, run: s.LoadLogSecrets},
	)
}

// checkSchedule refuses a table the scheduler can't run as written.
func checkSchedule(jobs []scheduled) error {
	names := map[string]bool{}
	lanes := map[string]scheduled{}
	for _, j := range append(slices.Clone(jobs), selfTimedJobs()...) {
		if j.name == "" || names[j.name] {
			return fmt.Errorf("scheduled job %q: the name is empty or used twice", j.name)
		}
		names[j.name] = true
		if j.every < 0 || (j.every == 0 && j.atStart) {
			return fmt.Errorf("scheduled job %q: every is how often, 0 for once at the start", j.name)
		}
	}
	for _, j := range jobs {
		if j.run == nil {
			return fmt.Errorf("scheduled job %q has nothing to run", j.name)
		}
		if j.lane == "" {
			continue
		}
		if first, ok := lanes[j.lane]; ok && (first.every != j.every || first.atStart != j.atStart) {
			return fmt.Errorf("scheduled jobs %q and %q share a lane and not a time", first.name, j.name)
		}
		if _, ok := lanes[j.lane]; !ok {
			lanes[j.lane] = j
		}
	}
	for _, j := range jobs {
		if _, ok := lanes[j.name]; ok && j.lane == "" {
			return fmt.Errorf("scheduled job %q is in the loop and has the name of a lane", j.name)
		}
	}
	return nil
}

// scheduler runs a schedule.
type scheduler struct {
	s    *Service
	jobs []scheduled

	mu   sync.Mutex
	busy map[string]*laneRun
}

type laneRun struct{ since time.Time }

// RunScheduler runs the scheduled jobs until ctx ends, and beside them the
// loops that time themselves: environment checks, the assistants' watch and
// the check of the jobs themselves.
func (s *Service) RunScheduler(ctx context.Context) {
	r := &scheduler{s: s, jobs: s.schedule(), busy: map[string]*laneRun{}}
	if err := checkSchedule(r.jobs); err != nil {
		panic(err)
	}
	for _, j := range r.jobs {
		s.jobs.declare(j.name, j.every, j.lane)
	}
	for _, j := range selfTimedJobs() {
		s.jobs.declare(j.name, j.every, j.name)
	}
	s.beginServerRun(ctx)
	context.AfterFunc(ctx, s.jobs.stop)
	s.goBG(func() { s.watchJobHealth(ctx) })
	s.goBG(func() { s.runHealthWatchdog(ctx) })
	r.fire(ctx, func(j scheduled) bool { return j.every == 0 || j.atStart }, 0)
	s.goBG(func() { s.RunEnvChecks(ctx) })
	s.goBG(func() { s.WatchAssistants(ctx) })

	var periods []time.Duration
	for _, j := range r.jobs {
		if j.every > 0 && !slices.Contains(periods, j.every) {
			periods = append(periods, j.every)
		}
	}
	cases := []reflect.SelectCase{{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())}}
	for _, p := range periods {
		t := time.NewTicker(p)
		defer t.Stop()
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(t.C)})
	}
	for {
		k, _, _ := reflect.Select(cases)
		if k == 0 {
			return
		}
		period := periods[k-1]
		r.fire(ctx, func(j scheduled) bool { return j.every == period }, loopJobPatience)
	}
}

// loopJobPatience is how long the loop waits for one of its own jobs before
// it goes on to the next and leaves that one running (WP-1944).
const loopJobPatience = 20 * time.Second

// fire runs the jobs that are due, in the table's order: the loop's own one
// after another, a background lane started the first time one of its jobs
// comes up. patience is how long the loop waits for one of its own jobs
// before leaving it running (loopJobPatience); 0 waits for it, as the
// server's start does, where the order matters.
func (r *scheduler) fire(ctx context.Context, due func(scheduled) bool, patience time.Duration) {
	started := map[string]bool{}
	for _, j := range r.jobs {
		if !due(j) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if j.lane == "" {
			r.runInLoop(ctx, j, patience)
			continue
		}
		if started[j.lane] {
			continue
		}
		started[j.lane] = true
		var lane []scheduled
		for _, o := range r.jobs {
			if o.lane == j.lane && due(o) {
				lane = append(lane, o)
			}
		}
		r.goLane(ctx, lane)
	}
}

// runInLoop runs one of the loop's jobs and waits for it up to patience. One
// still going then keeps its place in r.busy, as a background lane named
// after it does: its next due times are skipped and counted for as long as
// it goes on, and it counts as stuck as a lane does (jobTrouble). It is
// never started again beside itself, as a lane's job is (goLane): the loop's
// jobs were written to run one at a time. The wait ends with ctx too: the
// job itself is waited for at stop with the other background work (Wait).
func (r *scheduler) runInLoop(ctx context.Context, j scheduled, patience time.Duration) {
	run := r.claim([]scheduled{{name: j.name, every: j.every, lane: j.name}}, false)
	if run == nil {
		return
	}
	if patience <= 0 {
		// The loop waits for this job without limit: a stuck one holds
		// everything after it (jobTrouble).
		r.s.jobs.holdLoop(j.name)
		defer r.s.jobs.holdLoop("")
	}
	done := make(chan struct{})
	r.s.goBG(func() {
		defer close(done)
		defer r.release(j.name, run)
		r.s.job(ctx, j.name, j.every, j.run)
	})
	if patience <= 0 {
		<-done
		return
	}
	wait := time.NewTimer(patience)
	defer wait.Stop()
	select {
	case <-done:
	case <-wait.C:
	case <-ctx.Done():
	}
}

// claim takes a lane for a run, or returns nil and counts the lane's jobs as
// skipped when its last run is still going. With again, a run that has gone
// on past jobOverdue does not keep the lane: a new one starts beside it.
// Without, the lane is skipped for as long as the run goes on.
func (r *scheduler) claim(lane []scheduled, again bool) *laneRun {
	s := r.s
	name, now := lane[0].lane, s.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.busy[name]; cur != nil && (!again || now.Sub(cur.since) <= jobOverdue(lane[0].every)) {
		for _, j := range lane {
			s.jobs.skip(j.name, j.every, now)
		}
		return nil
	}
	run := &laneRun{since: now}
	r.busy[name] = run
	return run
}

func (r *scheduler) release(name string, run *laneRun) {
	r.mu.Lock()
	if r.busy[name] == run {
		delete(r.busy, name)
	}
	r.mu.Unlock()
}

// goLane runs a lane's jobs in the background, one after another. While its
// last run is still going the jobs are skipped, and counted as skipped: two
// runs of one job at once do the work twice. Once that run has gone on so
// long that it counts as stuck, a new one starts beside it: a run that
// hangs must not end the job until the next restart.
func (r *scheduler) goLane(ctx context.Context, lane []scheduled) {
	run := r.claim(lane, true)
	if run == nil {
		return
	}
	name := lane[0].lane
	r.s.goBG(func() {
		defer r.release(name, run)
		for _, j := range lane {
			if ctx.Err() != nil {
				return
			}
			r.s.job(ctx, j.name, j.every, j.run)
		}
	})
}

// jobHealthEvery is how often the jobs' counts are saved and checked.
const jobHealthEvery = time.Minute

// watchJobHealth checks the scheduled jobs each minute until ctx ends, then
// saves what was counted since.
func (s *Service) watchJobHealth(ctx context.Context) {
	t := time.NewTicker(jobHealthEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			s.saveJobHealth(save)
			s.stopServerRun(save)
			cancel()
			return
		case <-t.C:
			s.untracedJob(ctx, jobHealthName, jobHealthEvery, s.CheckJobHealth)
		}
	}
}

// healthWatchdogEvery is how often the watchdog looks at the health check.
const healthWatchdogEvery = 5 * time.Minute

// runHealthWatchdog looks at the health check every healthWatchdogEvery
// until ctx ends. It is started by RunScheduler beside the check, not by the
// scheduler's loop, so a job that holds the loop does not stop it.
func (s *Service) runHealthWatchdog(ctx context.Context) {
	t := time.NewTicker(healthWatchdogEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.markServerAlive(ctx)
			s.untracedJob(ctx, healthWatchdogName, healthWatchdogEvery, s.watchJobHealthCheck)
		}
	}
}

// dailyBackup writes waypoint-YYYY-MM-DD.db once a day and prunes old ones.
func (s *Service) dailyBackup(ctx context.Context) error {
	if err := os.MkdirAll(s.cfg.BackupDir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(s.cfg.BackupDir, "waypoint-"+s.now().UTC().Format(dateLayout)+".db")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	start := time.Now()
	if err := s.db.Backup(ctx, path); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "database backed up", "path", path, "ms", time.Since(start).Milliseconds())
	keep := s.cfg.BackupKeep
	if keep <= 0 {
		keep = 14
	}
	entries, err := os.ReadDir(s.cfg.BackupDir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "waypoint-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		if err := os.Remove(filepath.Join(s.cfg.BackupDir, names[0])); err != nil {
			s.log.WarnContext(ctx, "prune backup", "file", names[0], "err", err)
		}
		names = names[1:]
	}
	return nil
}
