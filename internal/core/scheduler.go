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
	name string
	// every is how often it runs; 0 runs it once, when the server starts.
	every time.Duration
	run   func(context.Context)
	// lane says where it runs. "" is the scheduler's loop: jobs due
	// together run in the table's order, each waiting for the one before,
	// so a slow one holds the rest. Anything else runs in the background,
	// one after another with the jobs of the same lane, for work a slow
	// forge or gateway must not let hold the loop.
	lane string
	// atStart also runs a periodic job when the server starts.
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
	goalTracesEvery = time.Hour // the hourly recheck starts them if no change did
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
	// hourly is a job that runs each hour and when the server starts.
	hourly := func(name string, run func(context.Context)) scheduled {
		return scheduled{name: name, every: hour, run: run, atStart: true}
	}
	once := func(name string, run func(context.Context)) scheduled {
		return bg(scheduled{name: name, run: run})
	}
	jobs := []scheduled{
		// Once, when the server starts, beside everything else.
		once("knowledge use backfill", s.BackfillKnowledgeUses),   // text written before use was recorded (WP-D15)
		once("mention backfill", s.RunMentionBackfill),            // issue text names issues, pages and terms without [[ ]] (WP-101)
		once("forge mention backfill", s.RunForgeMentionBackfill), // forge text linked every key it named before WP-379
		once("review leftovers backfill", s.RunLeftoverBackfill),  // findings open when their pull request merged (WP-367)

		hourly("due reminders", s.errJob("due reminders", s.NotifyDueSoon)),
		hourly("agent run expiry", s.errJob("expire agent runs", s.expireRuns)),
		hourly("stranded tasks", s.errJob("stranded tasks", s.sweepStranded)),     // WP-567
		hourly("unpushed commits", s.errJob("unpushed commits", s.sweepUnpushed)), // WP-599
		hourly("experiments", s.errJob("experiments", s.sweepExperiments)),        // WP-254
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
				{"notifications", "read_at IS NOT NULL AND created_at < ?", now - 365*24*hour.Milliseconds()}, // a year of read ones is kept
			} {
				// #nosec G202 -- the table and condition are the fixed text above
				if _, err := s.db.W.ExecContext(ctx, `DELETE FROM `+p.table+` WHERE `+p.where, p.before); err != nil && ctx.Err() == nil {
					s.log.WarnContext(ctx, "prune expired rows", "table", p.table, "err", err)
				}
			}
		}),
		hourly("AI call pruning", s.pruneAICalls), // WP-375
		hourly("automation run pruning", s.errJob("prune automation runs", s.PruneAutomationRuns)),
		hourly("webhook delivery pruning", s.errJob("prune webhook deliveries", s.pruneDeliveries)),
		hourly("estimates", s.sweepEstimates),
		hourly("stories", s.sweepStories), // WP-190
		hourly("looks", s.sweepLooks),
		hourly("prep ahead", s.sweepPrepAhead),                 // WP-906
		bg(hourly("prep scores", s.ScorePreps)),                // WP-906
		bg(hourly("collector health", s.CheckCollectorHealth)), // WP-999
		hourly("safety drill", s.maybeSafetyDrill),             // weekly (WP-278)
		hourly("sandbox drills", s.maybeSandboxDrills),         // weekly and after a helper update (WP-473)
		// goal traces, in case an event that changes them was missed (WP-382)
		hourly("goal recheck", func(context.Context) { s.goalsChanged() }),
		hourly("goal drift", s.startGoalDrift),            // weekly, in the background (WP-382)
		bg(hourly("agent kit", s.RefreshKit)),             // in case a push to the kit was missed
		bg(hourly("agent setup", s.RefreshAgentSetup)),    // WP-ADR44
		bg(hourly("recurring mistakes", s.SweepMistakes)), // WP-710
		// daily per person (WP-932); the baselines after it, then it prunes
		{name: "quota guard", every: hour, run: s.RunQuotaGuard, lane: "quota guard", atStart: true},
		{name: "quota guard baselines", every: hour, run: s.refreshGuardBaselines, lane: "quota guard", atStart: true},
		hourly("CI pruning", s.pruneCI),
		bg(hourly("merged branches", s.SweepBranches)),         // on the forges, daily per repo (WP-140)
		bg(hourly("CI history backfill", s.BackfillCIHistory)), // each repo's older CI runs, once (WP-490)
		bg(hourly("CI history re-tier", s.RetierCIHistory)),    // WP-849
		bg(hourly("instruction history backfill", s.BackfillUsageMarks)),
	}
	if s.cfg.BackupDir != "" {
		jobs = append(jobs, hourly("backup", func(ctx context.Context) { // one file a day
			if err := s.dailyBackup(ctx); err != nil {
				s.log.ErrorContext(ctx, "backup failed", "err", err)
				s.backupFailed(ctx, err)
			}
		}))
	}
	return append(jobs,
		scheduled{name: "timed automations", every: fiveMin, run: s.RunTimedAutomations, atStart: true},
		// how review findings fared, to the gateway (WP-514)
		bg(scheduled{name: "review feedback", every: fiveMin, run: s.ReportReviewFeedback}),
		bg(scheduled{name: "prep trials", every: fiveMin, run: s.RunPrepTrials}), // surplus only (WP-906)

		scheduled{name: "runners", every: halfMin, run: s.CheckRunners, atStart: true},
		scheduled{name: "auto question notices", every: halfMin, run: s.NotifyAutoQuestions, atStart: true}, // at start: after a restart (WP-96)
		scheduled{name: "forge rechecks", every: halfMin, run: s.CheckMergeRechecks},                        // a forge still checking (WP-138)

		// Pull requests merged or closed while Waypoint was down or before
		// it listened: forges can be slow.
		once("pull requests", s.RefreshPullRequests),
		once("resume babysits", s.ResumeBabysits), // a fixed Waypoint may settle what it gave up on (WP-431)

		scheduled{name: "forge sync", every: tenMin, run: s.SyncAllRepos},
		scheduled{name: "armed heads", every: tenMin, run: s.CheckArmedHeads},    // missed push webhooks (WP-163)
		bg(scheduled{name: "CI history", every: tenMin, run: s.RecordCIHistory}), // finished CI runs and jobs (WP-490)
		bg(scheduled{name: "CI health", every: tenMin, run: s.SweepCIHealth}),

		scheduled{name: "review gates", every: gateRecheck, run: s.CheckGates}, // failing review gates (WP-D21)
		scheduled{name: "pending checks", every: ciPollEvery, run: s.PollCI},

		scheduled{name: "system resources", every: minute, run: s.SampleResources},
		scheduled{name: "armed merges", every: minute, run: s.CheckArmedMerges},          // reviews settled, quiet over (WP-133)
		scheduled{name: "babysits", every: minute, run: s.CheckBabysits},                 // WP-134
		scheduled{name: "audits", every: minute, run: s.CheckAudits},                     // WP-348
		scheduled{name: "request conditions", every: minute, run: s.CheckRequestWatches}, // WP-126
		scheduled{name: "hold conditions", every: minute, run: s.CheckHoldConditions},    // WP-ADR75
		scheduled{name: "safety screens", every: minute, run: s.sweepScreens},            // WP-ADR10
		scheduled{name: "quota guard watch", every: minute, run: s.RunGuardWatch},        // holds before autopilot starts (WP-932)
		scheduled{name: "autopilot", every: minute, run: s.RunAutopilot},                 // WP-194
		scheduled{name: "warm caches", every: minute, run: s.KeepCachesWarm},             // WP-192
		scheduled{name: "work logs", every: minute, run: s.RunWorkLog},                   // WP-285
		scheduled{name: "PR descriptions", every: minute, run: s.RunPRBodies},            // WP-55
		scheduled{name: "fan-outs", every: minute, run: s.CheckFanouts},                  // WP-148
		scheduled{name: "log secrets", every: minute, run: s.LoadLogSecrets},             // secrets added since, blanked in the log (WP-375)
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
		// A lane runs as one: its jobs are due together or not at all.
		if first, ok := lanes[j.lane]; ok && (first.every != j.every || first.atStart != j.atStart) {
			return fmt.Errorf("scheduled jobs %q and %q share a lane and not a time", first.name, j.name)
		}
		if _, ok := lanes[j.lane]; !ok {
			lanes[j.lane] = j
		}
	}
	return nil
}

// scheduler runs a schedule.
type scheduler struct {
	s    *Service
	jobs []scheduled

	mu sync.Mutex
	// busy is the lanes with a run going: the lane's name to that run.
	busy map[string]*laneRun
}

type laneRun struct{ since time.Time }

// RunScheduler runs the scheduled jobs until ctx ends, and beside them the
// loops that time themselves: environment checks, the assistants' watch and
// the check of the jobs themselves.
func (s *Service) RunScheduler(ctx context.Context) {
	r := &scheduler{s: s, jobs: s.schedule(), busy: map[string]*laneRun{}}
	if err := checkSchedule(r.jobs); err != nil {
		panic(err) // a programming error, caught by the schedule's test
	}
	for _, j := range r.jobs {
		s.jobs.declare(j.name, j.every, j.lane)
	}
	for _, j := range selfTimedJobs() {
		s.jobs.declare(j.name, j.every, j.name)
	}
	s.beginServerRun(ctx)
	// What fails once the server is told to stop fails because of that.
	context.AfterFunc(ctx, s.jobs.stop)
	// Each on a timer and a goroutine of its own, started here and not by
	// the loop below: they are what say the loop is stuck, and the watchdog
	// says the check is.
	s.goBG(func() { s.watchJobHealth(ctx) })
	s.goBG(func() { s.runHealthWatchdog(ctx) })
	r.fire(ctx, func(j scheduled) bool { return j.every == 0 || j.atStart })
	s.goBG(func() { s.RunEnvChecks(ctx) })
	s.goBG(func() { s.WatchAssistants(ctx) })

	// One ticker for each period, as there was before the table: a tick is
	// dropped, not queued, while the loop is busy.
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
		r.fire(ctx, func(j scheduled) bool { return j.every == period })
	}
}

// fire runs the jobs that are due, in the table's order: the loop's own one
// after another, a background lane started the first time one of its jobs
// comes up.
func (r *scheduler) fire(ctx context.Context, due func(scheduled) bool) {
	started := map[string]bool{}
	for _, j := range r.jobs {
		if !due(j) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if j.lane == "" {
			r.s.job(ctx, j.name, j.every, j.run)
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

// goLane runs a lane's jobs in the background, one after another. While its
// last run is still going the jobs are skipped, and counted as skipped: two
// runs of one job at once do the work twice. Once that run has gone on so
// long that it counts as stuck, a new one starts beside it: a run that
// hangs must not end the job until the next restart.
func (r *scheduler) goLane(ctx context.Context, lane []scheduled) {
	s := r.s
	name, now := lane[0].lane, s.now()
	r.mu.Lock()
	if cur := r.busy[name]; cur != nil && now.Sub(cur.since) <= jobOverdue(lane[0].every) {
		r.mu.Unlock()
		for _, j := range lane {
			s.jobs.skip(j.name, j.every, now)
		}
		return
	}
	run := &laneRun{since: now}
	r.busy[name] = run
	r.mu.Unlock()
	s.goBG(func() {
		defer func() {
			r.mu.Lock()
			if r.busy[name] == run {
				delete(r.busy, name)
			}
			r.mu.Unlock()
		}()
		for _, j := range lane {
			if ctx.Err() != nil {
				return
			}
			s.job(ctx, j.name, j.every, j.run)
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
			// The server is stopping: what the last minute counted is kept.
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
