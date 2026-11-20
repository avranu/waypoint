package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// `wp runner` (WP-D17): this machine does work for Waypoint. It connects
// out to the server (so it works behind NAT), registers once, then
// long-polls for jobs: shell commands and file changes in the directories
// it was told to allow, and coding-agent CLIs run headless on a fresh
// checkout. It signs in as its owner and only their jobs reach it.

var runnerCmd = &command{
	name: "runner", group: "Setup", args: "[install|uninstall|status] [FLAGS]",
	summary: "run Waypoint's jobs on this machine",
	about: `Runs in the foreground, registers this machine and takes jobs from
Waypoint. Only jobs you started (or a rule you own) run on your runners.

  command   a shell command in an --allow directory
  apply     file changes in an --allow directory (refused if a file changed since)
  agent     Claude Code, Codex, opencode or Cursor, headless, on a fresh checkout
  review    an agent run with read-only tools

--allow is empty by default, so only checkout jobs run. Jobs get a trimmed
environment (PATH, HOME, locale, proxy, toolchains); API keys, SSH agent and
the runner's token stay behind. --env NAME passes one more.

It also uploads Claude Code usage and prepares queued tasks, and updates wp
between jobs to the server's version; the --no-* flags turn each off.
On Linux --max-jobs is a ceiling: it takes fewer while the machine is busy
(wp runner status shows how many and why).

Needs wp auth login first, as you. wp runner install starts it at login
(systemd user unit, launchd agent); wp setup does that and the hooks.`,
	examples: []string{
		"wp runner --allow ~/projects",
		"wp runner install --allow ~/projects --label gpu",
		"wp runner --name build-box --label fast --allow /srv/ci --max-jobs 4",
		"wp runner status",
	},
	run: cmdRunner,
}

func init() {
	runnerCmd.subs = []*command{
		{name: "install", args: "[FLAGS]", summary: "start the runner at login; rerun to change one setting (--allow none clears)",
			examples: []string{"wp runner install --allow ~/projects", "wp runner install --max-jobs 3"}, run: cmdRunnerInstall},
		{name: "uninstall", summary: "stop the runner and remove the login service",
			examples: []string{"wp runner uninstall"}, run: cmdRunnerUninstall},
		{name: "status", aliases: []string{"ls"}, summary: "show whether the runner is installed and running",
			examples: []string{"wp runner status"}, run: cmdRunnerStatus},
	}
}

// runnerOpts are the runner's settings, from flags.
type runnerOpts struct {
	Name   string
	Labels []string
	Allow  []string
	// Env names variables of the runner's environment its jobs get beyond
	// the allowlist (WP-418).
	Env      []string
	MaxJobs  int
	Once     bool
	PollWait float64 // seconds a poll is held; negative: the server's default (30)
	// NoUsageSync turns off the usage upload (WP-D20); UsageEvery and
	// UsageDir are for tests.
	NoUsageSync bool
	// NoPrep turns off preparing queued tasks for agents (WP-212).
	NoPrep bool
	// NoAutoUpdate keeps this wp as it is when the server's version differs.
	NoAutoUpdate bool
	UsageEvery   time.Duration
	UsageDir     string
	// For tests: repositories on this machine may be cloned, and files go here.
	AllowLocalRepos bool
	CacheDir        string
	// given is the flags on the command line, so install keeps the installed
	// runner's settings for the rest.
	given map[string]bool
}

// runnerFlags defines the flags of `wp runner` and (install) of
// `wp runner install`, which doesn't run anything itself.
func runnerFlags(c *cli, o *runnerOpts, install bool) func([]string) ([]string, error) {
	fs := c.flags()
	var labels, allow, env listFlag
	fs.StringVar(&o.Name, "name", "", "`NAME`: runner name (default: host name)")
	fs.Var(&labels, "label", "`LABEL`: label jobs can ask for (repeatable)")
	fs.Var(&allow, "allow", "`DIR`: directory (and below) command/apply jobs may use (repeatable)")
	fs.Var(&env, "env", "`NAME`: extra environment variable to pass to jobs (repeatable)")
	fs.IntVar(&o.MaxJobs, "max-jobs", 1, "`N`: most jobs at once")
	fs.BoolVar(&o.NoUsageSync, "no-usage-sync", false, "don't upload Claude Code usage")
	fs.BoolVar(&o.NoPrep, "no-prep", false, "don't prepare queued tasks")
	fs.BoolVar(&o.NoAutoUpdate, "no-auto-update", false, "don't update wp to the server's version")
	if !install {
		fs.BoolVar(&o.Once, "once", false, "take waiting jobs, then exit")
		fs.Float64Var(&o.PollWait, "wait", -1, "seconds a poll is held")
		hideFlags(fs, "wait", "once")
	}
	return func(args []string) ([]string, error) {
		pos, err := c.parse(fs, args)
		o.Labels, o.Allow, o.Env = labels, allow, env
		o.given = map[string]bool{}
		fs.Visit(func(f *flag.Flag) { o.given[f.Name] = true })
		return pos, err
	}
}

func cmdRunner(c *cli, args []string) error {
	var o runnerOpts
	parse := runnerFlags(c, &o, false)
	pos, err := parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unknown subcommand %q", pos[0])
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = runRunner(ctx, c, o)
	stop()
	var re *restartError
	if errors.As(err, &re) {
		return restartSelf(re.exe)
	}
	return err
}

// restartError ends the runner so it starts again on the wp at exe, which
// it just updated.
type restartError struct{ exe string }

func (e *restartError) Error() string { return "restart on the updated wp at " + e.exe }

// runRunner is the runner: register, poll, run jobs, until ctx ends.
func runRunner(ctx context.Context, c *cli, o runnerOpts) error {
	if c.token() == "" {
		return errors.New("not logged in: run wp auth login first (as yourself, not as an agent)")
	}
	c.agent = "none" // a runner acts as its owner
	if o.MaxJobs < 1 || o.MaxJobs > maxRunnerJobs {
		return c.usage("--max-jobs is between 1 and %d", maxRunnerJobs)
	}
	allowed, err := parseAllowed(o.Allow)
	if err != nil {
		return err
	}
	if err := passJobEnv(o.Env); err != nil {
		return c.usage("%v", err)
	}
	host, _ := os.Hostname()
	if o.Name == "" {
		o.Name = machineName() // what sessions report, so requests find this runner
	}
	if o.Name == "" {
		return c.usage("no host name found; give the runner a --name")
	}
	reportCtx, reportStop := context.WithCancel(context.Background())
	defer reportStop()
	p := &runnerProc{c: c, opts: o, allowed: allowed, host: host, jobs: map[int64]*activeJob{}, freed: make(chan struct{}, 1), reportCtx: reportCtx}
	return p.run(ctx, reportStop)
}

type runnerProc struct {
	c       *cli
	opts    runnerOpts
	allowed []string
	host    string
	id      int64

	// The server's version (each poll says it), and the last time this
	// runner tried to update to it.
	serverVersion  string
	updateTriedFor string
	updateTriedAt  time.Time

	reportCtx context.Context // ends when the runner has stopped waiting to report

	mu            sync.Mutex
	jobs          map[int64]*activeJob
	wg            sync.WaitGroup
	freed         chan struct{} // a job ended, or the governor allows more
	gov           *governor     // nil: up to MaxJobs whatever the load
	clis          []runnerCLI
	clisDirty     bool
	repoLocks     map[string]*sync.Mutex
	ocFormat      bool
	ocFormatKnown bool
	ocLogs        bool // opencode can print its own log (--print-logs)
	logMu         sync.Mutex
	sb            sandboxState // guarded by mu
}

type activeJob struct {
	id        int64
	lane      string // jobLane of its kind
	cancel    context.CancelFunc
	cancelled atomic.Bool
}

// jobRun is what an executor gets besides the input.
type jobRun struct {
	id      int64
	kind    string
	timeout time.Duration
	out     *logSink
	// token is the job's own Waypoint token (WP-418), for the jobs that run
	// wp; empty for the others, and from a server that doesn't mint them.
	token string
	// launch is the owner's session settings (WP-1265), for a job that
	// starts a session.
	launch *sessionPrefs
}

type claimedJob struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Title   string          `json:"title"`
	Input   json.RawMessage `json:"input"`
	Timeout int             `json:"timeout"`
	Token   string          `json:"token"`
	// Sandbox is the job's signed spec for the sandbox helper (WP-418).
	Sandbox *sandbox.Signed `json:"sandbox"`
	// Sessions are start specs for the job's Contained sessions, in the
	// order it starts them (the escape drill, WP-473).
	Sessions []*sandbox.Signed `json:"sessions"`
	// Lane is the lane the server took it in (WP-503); "" from an older
	// server: its kind's.
	Lane string `json:"lane"`
	// Launch is the owner's session settings, with a job that starts a
	// session (WP-1265); nil from an older server.
	Launch *sessionPrefs `json:"launch"`
}

type pollAnswer struct {
	Job           *claimedJob `json:"job"`
	Cancel        []int64     `json:"cancel"`
	ServerVersion string      `json:"server_version"`
	// Sandbox are helper actions a person asked for (WP-448).
	Sandbox []sandboxOp `json:"sandbox"`
}

var errRunnerRemoved = errors.New("this runner was removed in Waypoint")

func (p *runnerProc) logf(format string, a ...any) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	fmt.Fprintf(p.c.out, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// fatal: errors a retry won't fix (signed out, not allowed, removed).
func fatal(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.status == 400 || ae.status == 401 || ae.status == 403 || ae.status == 404)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// register tells the server what this machine is, retrying while it can't
// be reached.
func (p *runnerProc) register(ctx context.Context) error {
	if p.clis == nil {
		p.setCLIs(detectCLIsFn(ctx))
	}
	body := map[string]any{
		"name": p.opts.Name, "hostname": p.host, "os": runtime.GOOS, "arch": runtime.GOARCH, "version": version,
		"labels": orEmpty(p.opts.Labels), "allowed_dirs": toSlash(p.allowed), "clis": p.snapshotCLIs(), "max_jobs": p.opts.MaxJobs,
		// what this wp can do: "confine" = it limits a review's CLI to its
		// checkout; "autopilot" = it runs autopilot jobs (WP-ADR9)
		"features": runnerFeatureList(),
	}
	delay := time.Second
	for {
		var r struct {
			ID            int64  `json:"id"`
			ServerVersion string `json:"server_version"`
		}
		err := p.c.callInto(ctx, "POST", "/api/v1/runner/register", body, &r)
		if err == nil {
			p.id, p.serverVersion = r.ID, r.ServerVersion
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fatal(err) {
			return err
		}
		p.logf("Can't register with %s (%v); trying again in %s", p.c.server(), err, delay)
		if !sleepCtx(ctx, delay) {
			return ctx.Err()
		}
		delay = min(delay*2, time.Minute)
	}
}

func orEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func toSlash(l []string) []string {
	out := make([]string, len(l))
	for i, d := range l {
		out[i] = strings.ReplaceAll(d, `\`, "/")
	}
	return out
}

func (p *runnerProc) setCLIs(l []runnerCLI) {
	p.mu.Lock()
	p.clis, p.clisDirty = l, true
	p.mu.Unlock()
}

func (p *runnerProc) snapshotCLIs() []runnerCLI {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clisDirty = false
	return slices.Clone(p.clis)
}

// callInto is call that decodes the answer.
func (c *cli) callInto(ctx context.Context, method, path string, body, out any) error {
	b, err := c.callCtx(ctx, method, path, body)
	if err != nil {
		return err
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (p *runnerProc) activeIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]int64, 0, len(p.jobs))
	for id := range p.jobs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (p *runnerProc) poll(ctx context.Context, free int, lanes []string) (*pollAnswer, error) {
	body := map[string]any{"runner": p.id, "running": p.activeIDs(), "free": max(free, 0), "lanes": lanes}
	if p.gov != nil {
		heavy, light, _, _, gw := p.running()
		body["capacity"] = p.gov.report(time.Now(), heavy, light, gw)
	}
	switch {
	case p.opts.PollWait >= 0:
		body["wait"] = p.opts.PollWait
	case len(lanes) > 0 && (p.gov != nil || !slices.Contains(lanes, laneHeavy)):
		// A lane is open: a short poll, so the runner asks again soon after a
		// job ends or the load changes. The server reads the lanes once per
		// poll, so a long hold could claim a heavy job after the machine got
		// busy, or hold up the next heavy job while only the request,
		// message and gateway lanes are open (a poll that may take a job can't be ended
		// early, as a full runner's heartbeat can).
		body["wait"] = 5
	}
	p.mu.Lock()
	dirty := p.clisDirty
	p.mu.Unlock()
	if dirty {
		body["clis"] = p.snapshotCLIs()
	}
	sb, sbFailed := p.takeSandboxReport()
	if sb != nil {
		body["sandbox"] = sb
	}
	done, doneFailed := p.takeSandboxDone()
	if len(done) > 0 {
		body["sandbox_done"] = done
	}
	var a pollAnswer
	if err := p.c.callInto(ctx, "POST", "/api/v1/runner/poll", body, &a); err != nil {
		sbFailed()
		doneFailed()
		if dirty {
			p.mu.Lock()
			p.clisDirty = true
			p.mu.Unlock()
		}
		return nil, err
	}
	return &a, nil
}

// run is the runner's main loop.
func (p *runnerProc) run(ctx context.Context, reportStop context.CancelFunc) error {
	if err := p.register(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	p.logf("Runner %q is connected to %s (%d allowed %s, up to %d job%s at once)", p.opts.Name, p.c.server(), len(p.allowed), pluralWord(len(p.allowed), "directory", "directories"), p.opts.MaxJobs, pluralS(p.opts.MaxJobs))
	for _, cl := range p.snapshotCLIs() {
		state := "sign-in unknown"
		if cl.SignedIn != nil {
			state = map[bool]string{true: "signed in", false: "NOT signed in"}[*cl.SignedIn]
		}
		p.logf("  %s %s: %s", cl.Name, cl.Version, state)
	}
	go p.refreshCLIs(ctx)
	p.checkSandbox(ctx)
	go p.watchSandbox(ctx)
	if !p.opts.NoUsageSync && !p.opts.Once {
		go p.syncUsage(ctx)
	}
	if !p.opts.NoPrep && !p.opts.Once {
		go p.prepTasks(ctx)
	}
	if !p.opts.Once {
		go p.traceFixes(ctx)
		go p.proveFixes(ctx)
		go p.watchStalled(ctx)
	}
	if !p.opts.Once {
		p.reapInterruptedStarts(ctx) // before it takes a job: none of these can be this run's
	}
	if p.opts.MaxJobs > 1 {
		p.watchLoad(ctx)
	}

	delay := time.Second
	var err error
	for ctx.Err() == nil {
		// Between jobs, never during one: catch up with the server's wp.
		if p.updateDue() && len(p.activeIDs()) == 0 {
			if exe, ok := p.updateSelf(ctx); ok {
				err = &restartError{exe: exe}
				break
			}
		}
		lanes, free := p.slotsNow()
		// While full, the poll is only a heartbeat that listens for cancels;
		// it ends when a slot frees (it can't have taken a job, so ending it
		// loses nothing).
		pctx, pcancel := context.WithCancel(ctx)
		if free <= 0 {
			go func() {
				select {
				case <-p.freed:
					pcancel()
				case <-pctx.Done():
				}
			}()
		}
		var ans *pollAnswer
		ans, err = p.poll(pctx, free, lanes)
		slotFreed := pctx.Err() != nil && ctx.Err() == nil
		pcancel()
		if slotFreed {
			err = nil
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				err = nil
				break
			}
			if errors.Is(err, errRunnerRemoved) || isNotFound(err) {
				p.logf("%v. Stopping.", errRunnerRemoved)
				err = nil
				break
			}
			if fatal(err) {
				break
			}
			p.logf("Can't reach %s (%v); trying again in %s. Running jobs carry on.", p.c.server(), err, delay)
			sleepCtx(ctx, delay)
			delay = min(delay*2, time.Minute)
			err = nil
			continue
		}
		if delay > time.Second {
			p.logf("Reconnected.")
		}
		delay = time.Second
		p.serverVersion = ans.ServerVersion
		for _, id := range ans.Cancel {
			p.cancelJob(id)
		}
		if len(ans.Sandbox) > 0 {
			go p.runSandboxOps(ctx, ans.Sandbox)
		}
		if ans.Job != nil {
			p.start(ctx, ans.Job)
			continue
		}
		// Only a poll that asked for a job of any kind (the heavy and light
		// lanes; a request's job goes in its kind's lane when the request
		// lane is full) and got none means the queue is empty; a heartbeat
		// taken while full, or a poll for a request's jobs only, says
		// nothing, since a job may have finished since and the queue may
		// still hold more.
		if p.opts.Once && free > 0 && slices.Contains(lanes, laneHeavy) && slices.Contains(lanes, laneLight) && len(p.activeIDs()) == 0 {
			break
		}
		if len(ans.Cancel) > 0 {
			// A job is being stopped; the server repeats the cancel until it has.
			sleepCtx(ctx, 2*time.Second)
		}
	}
	if p.opts.Once && ctx.Err() == nil {
		p.waitJobs(0)
	}
	p.stopAll(reportStop)
	return err
}

// updateDue: the server runs another release than this wp, which is a
// release build too, and the runner may update itself (a failed try is
// repeated after 6 hours).
func (p *runnerProc) updateDue() bool {
	sv := p.serverVersion
	if p.opts.NoAutoUpdate || p.opts.Once || autoUpdateOff() || sv == version || !isRelease(sv) || !isRelease(version) {
		return false
	}
	return p.updateTriedFor != sv || time.Since(p.updateTriedAt) >= 6*time.Hour
}

// updateSelf replaces this wp with the server's, and says where it is if
// the runner should restart on it.
func (p *runnerProc) updateSelf(ctx context.Context) (string, bool) {
	sv := p.serverVersion
	p.updateTriedFor, p.updateTriedAt = sv, time.Now()
	p.logf("Waypoint runs %s and this runner wp %s: updating wp (--no-auto-update turns this off)", sv, version)
	got, exe, err := p.c.selfUpdate(ctx, sv)
	if err != nil {
		if ctx.Err() == nil {
			p.logf("Couldn't update wp: %v. Carrying on with %s; trying again in 6 hours.", err, version)
		}
		return "", false
	}
	p.logf("Updated %s to wp %s; restarting on it.", exe, got)
	return exe, true
}

func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == 404
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// refreshCLIs re-detects the agent CLIs every 10 minutes (people sign in
// and out); the next poll reports a change. A check that didn't finish keeps
// what was known, and every change of sign-in is logged (WP-501).
func (p *runnerProc) refreshCLIs(ctx context.Context) {
	for sleepCtx(ctx, 10*time.Minute) {
		p.refreshCLIsOnce(ctx)
	}
}

func (p *runnerProc) refreshCLIsOnce(ctx context.Context) {
	p.mu.Lock()
	prev := slices.Clone(p.clis)
	p.mu.Unlock()
	next := keepKnownSignIn(prev, detectCLIsFn(ctx))
	changed := !slices.EqualFunc(next, prev, func(a, b runnerCLI) bool {
		return a.Name == b.Name && a.Version == b.Version && a.Detail == b.Detail && (a.SignedIn == nil) == (b.SignedIn == nil) && (a.SignedIn == nil || *a.SignedIn == *b.SignedIn)
	})
	if !changed {
		return
	}
	for _, line := range signInChanges(prev, next) {
		p.logf("%s", line)
	}
	p.setCLIs(next)
}

// syncUsage uploads what Claude Code sessions used on this machine (WP-D20)
// when the runner starts and every 10 minutes after, as long as the machine
// has a Claude config directory (~/.claude), so the Usage tab fills in
// without a hook or a cron job. A sync that finds another one running (a
// hook's, or one by hand) leaves it to that one.
func (p *runnerProc) syncUsage(ctx context.Context) {
	every := p.opts.UsageEvery
	if every <= 0 {
		every = 10 * time.Minute
	}
	var lastErr string
	for {
		dir := claudeDir(p.opts.UsageDir)
		if fi, err := os.Stat(dir); dir != "" && err == nil && fi.IsDir() {
			res, err := usageSync(p.c, usageSyncOptions{Since: defaultSyncSince, Dir: dir, Inventory: true})
			switch {
			case errors.Is(err, errUsageBusy):
			case err != nil:
				if msg := err.Error(); msg != lastErr { // say it once, not every 10 minutes
					p.logf("Usage sync failed: %v", err)
					lastErr = msg
				}
			default:
				lastErr = ""
				if res.Accepted > 0 {
					p.logf("Uploaded usage from %s: %d new batch%s", dir, res.Accepted, map[bool]string{true: "", false: "es"}[res.Accepted == 1])
				}
			}
		}
		if !sleepCtx(ctx, every) {
			return
		}
	}
}

// start runs a claimed job in the background.
func (p *runnerProc) start(ctx context.Context, job *claimedJob) {
	jctx, cancel := context.WithCancel(ctx)
	aj := &activeJob{id: job.ID, cancel: cancel, lane: jobLane(job.Kind)}
	if slices.Contains([]string{laneHeavy, laneLight, laneRequest, laneMessage, laneGateway}, job.Lane) {
		aj.lane = job.Lane
	}
	p.mu.Lock()
	p.jobs[job.ID] = aj
	p.mu.Unlock()
	if p.gov != nil && aj.lane == laneHeavy {
		p.gov.heavyStarted(time.Now())
	}
	p.wg.Add(1)
	p.logf("Job %d (%s): %s", job.ID, job.Kind, job.Title)
	go func() {
		defer p.wg.Done()
		defer func() {
			p.mu.Lock()
			delete(p.jobs, job.ID)
			p.mu.Unlock()
			select {
			case p.freed <- struct{}{}:
			default:
			}
		}()
		p.runJob(jctx, aj, job)
	}()
}

func (p *runnerProc) cancelJob(id int64) {
	p.mu.Lock()
	aj := p.jobs[id]
	p.mu.Unlock()
	if aj != nil && !aj.cancelled.Swap(true) {
		p.logf("Job %d: Waypoint says to stop it (a person cancelled it, or it ended there)", id)
		aj.cancel()
	}
}

// waitJobs waits for the running jobs, at most d (0 = as long as it takes).
func (p *runnerProc) waitJobs(d time.Duration) {
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	if d == 0 {
		<-done
		return
	}
	select {
	case <-done:
	case <-time.After(d):
	}
}

// stopAll ends the jobs still running (they report that they were stopped)
// and gives the reports a few seconds to get through.
func (p *runnerProc) stopAll(reportStop context.CancelFunc) {
	p.mu.Lock()
	for _, aj := range p.jobs {
		aj.cancel()
	}
	n := len(p.jobs)
	p.mu.Unlock()
	if n > 0 {
		p.logf("Stopping %d running job%s", n, pluralS(n))
	}
	p.waitJobs(20 * time.Second)
	reportStop()
	p.wg.Wait()
}

// runJob carries out one job and reports how it ended.
func (p *runnerProc) runJob(ctx context.Context, aj *activeJob, job *claimedJob) {
	sink := newLogSink()
	timeout := time.Duration(job.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	p.ensureSandboxUsers(ctx, job)
	run := &jobRun{id: job.ID, kind: job.Kind, timeout: timeout, out: sink, token: job.Token, launch: job.Launch}
	var logs sync.WaitGroup
	logs.Add(1)
	go func() {
		defer logs.Done()
		sink.run(p.reportCtx, p.sendLog(job.ID), func() { p.cancelJob(job.ID) })
	}()
	o := p.execute(ctx, run, job)
	if aj.cancelled.Load() && o.State != "done" {
		o.State, o.Error = "cancelled", "Cancelled by a person."
	} else if ctx.Err() != nil && o.State == "cancelled" && o.Error == "Stopped." {
		o.Error = "The runner was stopped." // internal/core's runnerStoppedError: such an audit goes again
	}
	o = fitResult(o, resultLimit, sink)
	sink.close()
	logs.Wait()
	p.logf("Job %d: %s%s", job.ID, o.State, map[bool]string{true: " (" + o.Error + ")"}[o.Error != ""])
	p.finish(job.ID, o)
}

// execute dispatches on the kind; a panic in an executor fails the job.
func (p *runnerProc) execute(ctx context.Context, run *jobRun, job *claimedJob) (o outcome) {
	defer func() {
		if r := recover(); r != nil {
			o = failed("internal error in the runner: %v", r)
		}
	}()
	switch job.Kind {
	case "command":
		var in commandJob
		if err := json.Unmarshal(job.Input, &in); err != nil {
			return failed("bad job input: %v", err)
		}
		return p.execCommand(ctx, run, in)
	case "apply":
		var in applyJob
		if err := json.Unmarshal(job.Input, &in); err != nil {
			return failed("bad job input: %v", err)
		}
		return p.execApply(ctx, run, in)
	case "agent", "review", "audit":
		var in agentJob
		if err := json.Unmarshal(job.Input, &in); err != nil {
			return failed("bad job input: %v", err)
		}
		return p.execAgent(ctx, run, job.Kind, in)
	case "session":
		var in sessionJob
		if err := json.Unmarshal(job.Input, &in); err != nil {
			return failed("bad job input: %v", err)
		}
		return p.execSession(ctx, run, in)
	case "message":
		var in messageJob
		if err := json.Unmarshal(job.Input, &in); err != nil {
			return failed("bad job input: %v", err)
		}
		return p.execMessage(ctx, run, in)
	case "autopilot":
		var in autopilotJob
		if err := json.Unmarshal(job.Input, &in); err != nil {
			return failed("bad job input: %v", err)
		}
		return p.execAutopilot(ctx, run, in)
	case "autopilot_start":
		var in autopilotJob
		if err := json.Unmarshal(job.Input, &in); err != nil {
			return failed("bad job input: %v", err)
		}
		return p.execAutopilotStart(ctx, run, in)
	case "sandbox_drill":
		return p.execSandboxDrill(ctx, run, job)
	}
	return failed("this runner doesn't know jobs of kind %q (update wp)", job.Kind)
}

// sendLog uploads one batch of output.
func (p *runnerProc) sendLog(id int64) func(ctx context.Context, offset int64, text string) (bool, bool, error) {
	return func(ctx context.Context, offset int64, text string) (bool, bool, error) {
		var ack struct {
			Cancel    bool `json:"cancel"`
			Truncated bool `json:"truncated"`
		}
		err := p.c.callInto(ctx, "POST", fmt.Sprintf("/api/v1/runner/jobs/%d/log", id), map[string]any{"offset": offset, "text": text}, &ack)
		if isNotFound(err) {
			return true, true, nil // the job is gone: stop it and stop sending
		}
		return ack.Cancel, ack.Truncated, err
	}
}

// finish reports a job's end, again and again while the server can't be
// reached: a job outlives a server restart.
func (p *runnerProc) finish(id int64, o outcome) {
	body := map[string]any{"state": o.State, "error": o.Error}
	if o.ExitCode != nil {
		body["exit_code"] = *o.ExitCode
	}
	if o.Result != nil {
		body["result"] = o.Result
	}
	delay := 2 * time.Second
	reduced := false
	for {
		err := p.c.callInto(p.reportCtx, "POST", fmt.Sprintf("/api/v1/runner/jobs/%d/finish", id), body, nil)
		if err == nil {
			return
		}
		// The server says the result is too large (it measures what this
		// estimate missed): once, the helper runs go back as their kept
		// leads only, so the findings' work isn't lost.
		var ae *apiError
		if !reduced && errors.As(err, &ae) && ae.status == 400 && strings.Contains(ae.msg, core.RunnerResultTooLarge) {
			reduced = true
			if r, ok := reduceToKept(o); ok {
				p.logf("Job %d: the server found the result too large (%v); sending it again with the helper runs reduced to their kept leads", id, err)
				o = r
				body["result"] = o.Result
				continue
			}
		}
		if p.reportCtx.Err() != nil {
			p.logf("Job %d: couldn't report its end (%v); the server will expire it", id, err)
			return
		}
		if fatal(err) {
			p.logf("Job %d: the server won't take the report (%v)", id, err)
			return
		}
		p.logf("Job %d: can't report its end yet (%v); trying again in %s", id, err, delay)
		if !sleepCtx(p.reportCtx, delay) {
			return
		}
		delay = min(delay*2, 30*time.Second)
	}
}
