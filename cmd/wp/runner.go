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

It also uploads Claude Code usage and prepares queued tasks, and keeps wp at
the server's version: the new binary is put in place at once and the runner
restarts on it when no job is running (after 15 minutes of waiting it takes no
new jobs until the running ones finish); the --no-* flags turn each off.
On Linux --max-jobs is a ceiling: it takes fewer while the machine is busy
(wp runner status shows how many and why). A job that has waited over a
minute for a test slot doesn't count toward it, one such job at a time.

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
	given           map[string]bool
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
	fs.BoolVar(&o.NoAutoUpdate, "no-auto-update", false, "keep this wp version: no update or restart to the server's")
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
	c.agent = "none"
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
	defer setUpdateOptOut(o)()
	host, _ := os.Hostname()
	if o.Name == "" {
		o.Name = machineName()
	}
	if o.Name == "" {
		return c.usage("no host name found; give the runner a --name")
	}
	reportCtx, reportStop := context.WithCancel(context.Background())
	defer reportStop()
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	p := &runnerProc{c: c, opts: o, allowed: allowed, host: host, jobs: map[int64]*activeJob{}, freed: make(chan struct{}, 1), reportCtx: reportCtx, stopCtx: stopCtx, stopCancel: stopCancel}
	return p.run(ctx, reportStop)
}

type runnerProc struct {
	c       *cli
	opts    runnerOpts
	allowed []string
	host    string
	id      int64

	serverVersion  string
	updateTriedFor string
	updateTriedAt  time.Time
	// Another wp holds the shared record's claim on (or failed) the server's
	// release: this runner doesn't download it before then (real time, as
	// the record is).
	updateHeldFor     string
	updateHeldUntil   time.Time
	updateHeldByClaim bool // the hold is a download in flight, not a failure
	// This runner's last failure it recorded in the shared record (real
	// time, after the record's): a failure there no newer is its own, which
	// its own backoff governs.
	failedFor string
	failedAt  time.Time
	claimErr  string // the last error claiming a try met, logged once
	// The wp on disk is already this release (updatedExe, whose SHA-256 is
	// updatedSum) while this process still runs another one: empty when the
	// disk holds this process's own release, as far as the runner knows. The
	// runner learns it from its own download or from looking at the disk.
	// updatedAt is when a restart first became pending: a newer release
	// that replaces the one noted doesn't start the drain's wait again.
	updatedTo, updatedExe, updatedSum string
	updatedAt                         time.Time
	diskLookedAt                      time.Time // the last look at the disk's release
	drainLoggedFor                    string
	draining                          bool             // takes no new jobs until idle, then restarts
	now                               func() time.Time // tests swap it; nil: runnerClock
	updating                          bool             // a download runs beside the loop
	updateDone                        chan updateResult
	updateCancel                      context.CancelFunc                                                  // ends the download beside the loop
	downloadFn                        func(ctx context.Context, want string) (got, exe string, err error) // nil: c.selfUpdate

	reportCtx  context.Context
	stopCtx    context.Context
	stopCancel context.CancelFunc

	mu            sync.Mutex
	jobs          map[int64]*activeJob
	wg            sync.WaitGroup
	freed         chan struct{}
	gov           *governor
	clis          []runnerCLI
	clisDirty     bool
	repoLocks     map[string]*sync.Mutex
	ocFormat      bool
	ocFormatKnown bool
	ocLogs        bool
	logMu         sync.Mutex
	sb            sandboxState
}

type activeJob struct {
	id            int64
	lane          string
	cancel        context.CancelFunc
	cancelled     atomic.Bool
	slotWaitSince time.Time
}

// jobRun is what an executor gets besides the input.
type jobRun struct {
	id        int64
	kind      string
	title     string
	timeout   time.Duration
	out       *logSink
	token     string
	launch    *sessionPrefs
	cancelled *atomic.Bool
	job       *activeJob
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
		p.reapInterruptedStarts(ctx)
	}
	if p.opts.MaxJobs > 1 {
		p.watchLoad(ctx)
	}

	defer p.stopUpdate()
	delay := time.Second
	var err error
	for ctx.Err() == nil {
		if exe := p.updateStep(ctx); exe != "" {
			err = &restartError{exe: exe}
			break
		}
		lanes, free := p.lanesNow()
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
		if p.opts.Once && free > 0 && slices.Contains(lanes, laneHeavy) && slices.Contains(lanes, laneLight) && len(p.activeIDs()) == 0 {
			break
		}
		if len(ans.Cancel) > 0 {
			sleepCtx(ctx, 2*time.Second)
		}
	}
	if p.opts.Once && ctx.Err() == nil {
		p.waitJobs(0)
	}
	p.stopAll(reportStop)
	return err
}

// runnerDrainAfter is how long a runner whose wp on disk is the new release
// keeps taking jobs while it waits for idle, before it stops taking them.
const runnerDrainAfter = 15 * time.Minute

// runnerClock is the runner's time source (tests move it).
var runnerClock = time.Now

func (p *runnerProc) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return runnerClock()
}

// mayUpdate: this runner follows the server's release (no opt-out, not
// --once) and both it and the server are release builds.
func (p *runnerProc) mayUpdate() bool {
	return !p.opts.NoAutoUpdate && !p.opts.Once && !autoUpdateOff() && isRelease(p.serverVersion) && isRelease(version)
}

// updateAllowed: mayUpdate, and the server runs another release than this
// wp (older too: wp follows the server, so a rollback reaches runners).
func (p *runnerProc) updateAllowed() bool {
	return p.mayUpdate() && p.serverVersion != version
}

// diskBehind: as far as the runner knows, the wp on disk isn't the server's
// release. That is so when the server runs another release than this
// process and no download put it there, and also when the server is back on
// this process's release after one put another there.
func (p *runnerProc) diskBehind() bool {
	if p.updatedTo == "" {
		return p.serverVersion != version
	}
	return p.updatedTo != p.serverVersion
}

// updateDue: mayUpdate and the disk is behind, outside this runner's own
// backoff for that release (a failed try is repeated after 6 hours) and
// any other wp's claim or failure in the shared record.
func (p *runnerProc) updateDue() bool {
	sv := p.serverVersion
	return p.mayUpdate() && p.diskBehind() &&
		(p.updateTriedFor != sv || p.clock().Sub(p.updateTriedAt) >= p.retryEvery(sv)) &&
		!p.heldBack(sv)
}

// putBackRetryEvery is how soon a failed try to put this process's release
// back on disk is repeated: until it is, the runner's jobs, hooks and new wp
// processes run the release the server rolled away from (1847.1).
const putBackRetryEvery = 15 * time.Minute

// retryEvery is the backoff after a failed try for release sv: 6 hours,
// but putBackRetryEvery when sv is this process's release and the disk holds
// another.
func (p *runnerProc) retryEvery(sv string) time.Duration {
	if sv == version && p.updatedTo != "" && p.updatedTo != sv {
		return putBackRetryEvery
	}
	return updateRetryEvery
}

// heldBack: another wp's claim on release sv, or its failure, holds this
// runner's download of it back now.
func (p *runnerProc) heldBack(sv string) bool {
	return p.updateHeldFor == sv && time.Now().Before(p.updateHeldUntil)
}

// updateResult is what a download that ran beside the poll loop came to.
type updateResult struct {
	sv, got, exe string
	err          error
	interrupted  bool // the runner was stopping: not a failure
}

// updateDownloadLimit bounds a runner's download, as wp upgrade's is: the
// runner waits for one in flight before it restarts.
const updateDownloadLimit = 10 * time.Minute

// diskLookEvery is how often a runner whose disk is behind, but which
// doesn't download now, runs the wp on disk to ask its release.
const diskLookEvery = time.Minute

// updateHoldRecheck is how often a runner held back by another wp's claim
// reads the shared record again: a claim given back (its download cut
// short) or replaced by a result holds it no longer than this (c6971). A
// var: tests shorten it.
var updateHoldRecheck = time.Minute

// updateStep is the runner's update logic, once per loop. It replaces the
// binary on disk as soon as the server runs another release, with jobs
// running too: that download runs beside the loop, which goes on polling
// and applying cancels, and a later pass collects it. It returns the path to
// restart on once idle. A runner whose restart has been pending longer than
// runnerDrainAfter sets p.draining: lanesNow then asks for no jobs, until
// it restarts.
//
// What it decides on is the wp on disk, not only what its own downloads
// did: while a download runs beside the loop it doesn't restart (that
// download may yet replace the binary); while it doesn't download (its
// backoff, another wp's claim) it looks at the disk, which another wp may
// have updated; a server back on this process's release has the disk put
// back to it; and it restarts only on the file it noted, unchanged.
//
// Whether it drains is decided on every pass that doesn't restart, whatever
// that pass did (started or waited for a download, its own or another
// wp's, or was held back): no pass lets a poll take a job while a restart
// is pending past the wait (1788.1, c6972).
func (p *runnerProc) updateStep(ctx context.Context) (restart string) {
	p.collectUpdate()
	if !p.mayUpdate() {
		p.draining = false
		return ""
	}
	restart = p.restartNow(ctx, true)
	p.draining = restart == "" && p.drainDue()
	return restart
}

// restartNow does what updateStep does but drain: it downloads, looks at
// the disk, and returns the path to restart on when the runner is idle and
// the disk holds the release to restart on. A file found changed since it
// was noted (another wp replaced it) is decided on afresh once, at once.
func (p *runnerProc) restartNow(ctx context.Context, again bool) string {
	sv := p.serverVersion
	if p.updating {
		return ""
	}
	if p.diskBehind() {
		if p.updateDue() && p.beginUpdate(sv) {
			p.startDownload(ctx, sv) // idle, here: decided on below at once
		} else {
			p.lookAtDisk(ctx)
		}
		if p.updating {
			return ""
		}
	}
	ready := p.updatedTo
	// The wp on disk is ready to restart on when it is the server's, or when
	// the server's isn't due (a failed try, inside its backoff, or another
	// wp's claim) and the disk holds another release than this process's
	// while the server doesn't run this process's either. While another wp's
	// download of the server's runs, the runner waits for it as for its own.
	inFlight := p.updateHeldByClaim && p.heldBack(sv)
	if ready == "" || (ready != sv && (sv == version || inFlight || p.updateDue())) {
		return ""
	}
	if len(p.activeIDs()) > 0 {
		return ""
	}
	if !p.diskStill() {
		if again {
			return p.restartNow(ctx, false)
		}
		return ""
	}
	p.logf("Restarting on wp %s at %s.", ready, p.updatedExe)
	return p.updatedExe
}

// restartPending: this runner restarts once it can. The disk holds another
// release than this process's, as far as it knows, and the server doesn't
// run this process's: whatever a download in flight (this runner's or
// another wp's) puts there, the runner restarts on that or on what is there.
func (p *runnerProc) restartPending() bool {
	return p.updatedTo != "" && p.serverVersion != version
}

// drainDue: a restart has been pending since longer than runnerDrainAfter
// (across newer releases: one that replaces another doesn't start the wait
// again). It logs the drain once per release on disk.
func (p *runnerProc) drainDue() bool {
	if !p.restartPending() || p.clock().Sub(p.updatedAt) < runnerDrainAfter {
		return false
	}
	if ready := p.updatedTo; p.drainLoggedFor != ready {
		p.drainLoggedFor = ready
		if active := len(p.activeIDs()); active > 0 {
			p.logf("Waiting to restart on wp %s since %s: taking no new jobs until the %d running finish, then restarting on wp %s.", ready, p.updatedAt.Format("15:04"), active, ready)
		} else {
			p.logf("Waiting to restart since %s: taking no new jobs until the download of wp %s ends, then restarting.", p.updatedAt.Format("15:04"), p.serverVersion)
		}
	}
	return true
}

// diskStill says whether the wp at updatedExe is still the file noted as
// release updatedTo (its SHA-256 unchanged). When it isn't (another wp put
// another release there, a server rollback the runner didn't see), the
// runner forgets it and decides afresh: it looks at the disk and may
// download the server's again at once (c6980).
func (p *runnerProc) diskStill() bool {
	if sum, err := fileSHA(p.updatedExe); err == nil && sum == p.updatedSum {
		return true
	}
	p.logf("The wp at %s is no longer the wp %s this runner noted there; looking again.", p.updatedExe, p.updatedTo)
	if p.updateTriedFor == p.updatedTo {
		p.updateTriedFor = "" // it got there: no failure to back off from
	}
	p.updatedTo, p.updatedExe, p.updatedSum, p.drainLoggedFor = "", "", "", ""
	p.diskLookedAt = time.Time{}
	return false
}

// startDownload downloads sv, claimed: beside the loop when jobs run, else
// here (nothing to poll for meanwhile). Either way updateStep's restart and
// drain decision follows, on the pass this one ends in or a later one.
func (p *runnerProc) startDownload(ctx context.Context, sv string) {
	if len(p.activeIDs()) > 0 {
		if p.updateDone == nil {
			p.updateDone = make(chan updateResult, 1)
		}
		dctx, cancel := context.WithCancel(ctx)
		p.updating, p.updateCancel = true, cancel
		go func() { p.updateDone <- p.download(dctx, sv) }()
		return
	}
	res := p.download(ctx, sv)
	p.finishUpdate(res)
	p.tookDownload(res)
}

// onDisk notes that the wp at exe, whose SHA-256 is sum, is release v, and
// says whether it is another release than this process's (one to restart
// on). The drain's wait starts when a restart first becomes pending.
func (p *runnerProc) onDisk(v, exe, sum string) (other bool) {
	if v == version {
		p.updatedTo, p.updatedExe, p.updatedSum, p.drainLoggedFor = "", "", "", ""
		return false
	}
	if p.updatedTo == "" {
		p.updatedAt = p.clock()
	}
	p.updatedTo, p.updatedExe, p.updatedSum = v, exe, sum
	return true
}

// lookAtDisk looks at the wp on disk (once a minute at most) to learn
// whether it is the server's release: another wp (a job's, a hook's wp
// upgrade --auto) may have put it there while this runner's own try failed
// or was held back. Only a file whose SHA-256 is the one the server's
// SHA256SUMS names is run to ask its release, and so restarted on (1788.2).
func (p *runnerProc) lookAtDisk(ctx context.Context) {
	if !p.diskLookedAt.IsZero() && p.clock().Sub(p.diskLookedAt) < diskLookEvery {
		return
	}
	p.diskLookedAt = p.clock()
	exe, err := selfExe()
	if err != nil {
		return
	}
	on := p.serverVersion
	if p.updatedTo == on || (p.updatedTo == "" && on == version) {
		return
	}
	sum, err := p.c.cliSum(ctx)
	if err != nil || !isServersRelease(ctx, exe, sum, on) {
		return
	}
	if p.onDisk(on, exe, sum) {
		p.logf("Another wp put wp %s at %s; this runner restarts on it when idle.", on, exe)
	}
}

// collectUpdate takes the result of a download that ran beside the loop.
func (p *runnerProc) collectUpdate() {
	if !p.updating {
		return
	}
	select {
	case res := <-p.updateDone:
		p.endDownload(res)
		p.tookDownload(res)
	default:
	}
}

// tookDownload notes what a finished download put on disk.
func (p *runnerProc) tookDownload(res updateResult) {
	if res.err != nil {
		return
	}
	var sum string
	if res.sv != version {
		sum, _ = fileSHA(res.exe) // "": diskStill takes it as changed
	}
	if !p.onDisk(res.sv, res.exe, sum) {
		p.logf("Put wp %s back at %s, the release Waypoint runs again.", res.sv, res.exe)
		return
	}
	if n := len(p.activeIDs()); n == 0 {
		p.logf("Updated %s to wp %s.", res.exe, res.sv) // the restart, if it is the one, logs next
	} else {
		p.logf("Updated %s to wp %s; this runner restarts on it when its %d job%s %s.", res.exe, res.sv, n, pluralS(n), pluralWord(n, "finishes", "finish"))
	}
}

// endDownload closes the books on a download that ran beside the loop.
func (p *runnerProc) endDownload(res updateResult) {
	p.updating = false
	if p.updateCancel != nil {
		p.updateCancel()
		p.updateCancel = nil
	}
	p.finishUpdate(res)
}

// beginUpdate claims the try for sv in the shared record, unless another wp
// holds a fresh claim (on any release, as claimAutoUpdate's due says: the
// SessionStart hook's daily one names none, and wp upgrade --auto downloads
// what the server serves when it runs) or failed sv inside its backoff (a
// runner that restarted keeps that backoff too, and a failure newer than
// this runner's own holds even once its own backoff is over: c6982): then
// it says false and the runner waits that out. A claim is looked at again
// every updateHoldRecheck, so one given back holds no longer (c6971). A
// record it couldn't read, or a claim it couldn't save, holds it back as
// long too: another wp's claim may be there unseen, and its own isn't there
// for others to see (c6981). A claimed try is noted here (a failed one is
// repeated after 6 hours).
func (p *runnerProc) beginUpdate(sv string) bool {
	var held time.Time
	claimed := false // held by a download in flight, not a failure
	backoff := p.retryEvery(sv)
	// The poll loop's goroutine waits here: no longer than a command's claim
	// does (c6997). A lock it misses holds it back updateHoldRecheck.
	err := p.c.updateStateWithin(claimWait, func(s *State) {
		switch u := s.Update; {
		case claimLive(u):
			held, claimed = u.Checked.Add(updateClaimFresh), true
			return
		case u != nil && u.Server == sv && strings.HasPrefix(u.Result, "failed") && time.Since(u.Checked) < backoff && !p.ownFailure(u):
			held = u.Checked.Add(backoff)
			return
		}
		s.Update = &updateCheck{Checked: time.Now(), Server: sv, Result: "started"}
	})
	if err != nil {
		if msg := err.Error(); p.claimErr != msg {
			p.claimErr = msg
			p.logf("Couldn't claim the update to wp %s in the shared state (%v); trying again every %s.", sv, err, updateHoldRecheck)
		}
		p.updateHeldFor, p.updateHeldUntil, p.updateHeldByClaim = sv, time.Now().Add(updateHoldRecheck), false
		return false
	}
	p.claimErr = ""
	if !held.IsZero() {
		if look := time.Now().Add(updateHoldRecheck); claimed && look.Before(held) {
			held = look // held again then, if still claimed
		}
		p.updateHeldFor, p.updateHeldUntil, p.updateHeldByClaim = sv, held, claimed
		return false
	}
	p.updateTriedFor, p.updateTriedAt = sv, p.clock()
	p.logf("Waypoint runs %s and this runner wp %s: updating wp (--no-auto-update turns this off)", sv, version)
	return true
}

// ownFailure: the shared record's failure u is this runner's own (none newer
// was recorded since), whose backoff the runner's own clock governs.
func (p *runnerProc) ownFailure(u *updateCheck) bool {
	return p.failedFor == u.Server && !u.Checked.After(p.failedAt)
}

// download puts the server's wp over this one. It only downloads: the loop's
// goroutine does the logging and the record.
func (p *runnerProc) download(ctx context.Context, sv string) updateResult {
	fn := p.downloadFn
	if fn == nil {
		fn = p.c.selfUpdate
	}
	dctx, cancel := context.WithTimeout(ctx, updateDownloadLimit)
	defer cancel()
	got, exe, err := fn(dctx, sv)
	return updateResult{sv: sv, got: got, exe: exe, err: err, interrupted: err != nil && ctx.Err() != nil}
}

// finishUpdate records how a download ended. One cut short by the runner
// stopping is no failure: it only gives up its claim.
func (p *runnerProc) finishUpdate(res updateResult) {
	if res.interrupted {
		p.c.releaseUpdateClaim(res.sv)
		return
	}
	if res.err != nil {
		again := "6 hours"
		if p.retryEvery(res.sv) == putBackRetryEvery {
			again = "15 minutes"
		}
		p.logf("Couldn't update wp: %v. Carrying on with %s; trying again in %s.", res.err, version, again)
		p.c.recordUpdate(res.sv, "failed: "+res.err.Error(), false)
		p.failedFor, p.failedAt = res.sv, time.Now()
		return
	}
	p.c.recordUpdate(res.sv, "updated from "+version+" to "+res.got, false)
}

// lanesNow is slotsNow, except that a draining runner has no lane or slot
// open: its poll is then only the heartbeat that listens for cancels and
// ends when a job finishes.
func (p *runnerProc) lanesNow() ([]string, int) {
	if p.draining {
		return []string{}, 0
	}
	return p.slotsNow()
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
				if msg := err.Error(); msg != lastErr {
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
			p.wake()
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
// and gives the reports a few seconds to get through. A job's transcript
// upload at its end ignores its cancel, so the stop also ends those (stopCtx):
// otherwise one could outlast the wait and the finish report would be lost.
func (p *runnerProc) stopAll(reportStop context.CancelFunc) {
	if p.stopCancel != nil {
		p.stopCancel()
	}
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

// settleOutcome words how a job that was stopped ended: cancelled by a person
// (cancelled), or the runner was stopped (ctx). keepTranscripts settles before
// it writes the sidecar kept beside a job's transcripts and again after the
// upload, so the sidecar says what the server is told even when a cancel
// comes during the upload.
func settleOutcome(ctx context.Context, cancelled *atomic.Bool, o outcome) outcome {
	if cancelled != nil && cancelled.Load() && o.State != "done" {
		o.State, o.Error = "cancelled", "Cancelled by a person."
	} else if ctx.Err() != nil && o.State == "cancelled" && o.Error == "Stopped." {
		o.Error = "The runner was stopped."
	}
	return o
}

// runJob carries out one job and reports how it ended.
func (p *runnerProc) runJob(ctx context.Context, aj *activeJob, job *claimedJob) {
	sink := newLogSink()
	timeout := time.Duration(job.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	p.ensureSandboxUsers(ctx, job)
	run := &jobRun{id: job.ID, kind: job.Kind, title: job.Title, timeout: timeout, out: sink, token: job.Token, launch: job.Launch, cancelled: &aj.cancelled, job: aj}
	var logs sync.WaitGroup
	logs.Add(1)
	go func() {
		defer logs.Done()
		sink.run(p.reportCtx, p.sendLog(job.ID), func() { p.cancelJob(job.ID) })
	}()
	o := p.execute(ctx, run, job)
	o = settleOutcome(ctx, &aj.cancelled, o)
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
	case "objective":
		var in core.ObjectiveJob
		if err := json.Unmarshal(job.Input, &in); err != nil {
			return failed("bad job input: %v", err)
		}
		return p.execObjective(ctx, run, in)
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
			return true, true, nil
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

// stopUpdate runs when the loop ends: a download still running is cut short
// and waited for, so the process doesn't leave in the middle of putting the
// binary in place (on Windows, between its renames), and the claim goes only
// once nothing of this runner's touches the binary. One cut short gives up
// its claim, and wp commands may try; one that got there is recorded.
func (p *runnerProc) stopUpdate() {
	if !p.updating {
		return
	}
	p.updateCancel()
	select {
	case res := <-p.updateDone:
		p.endDownload(res)
	case <-time.After(stopUpdateWait):
		// Stuck past its context: leave the claim, which goes stale.
	}
}

// stopUpdateWait bounds how long a stopping runner waits for its cancelled
// download to return.
const stopUpdateWait = time.Minute

// setUpdateOptOut puts the update opt-out in this process's environment when
// the runner doesn't update (--no-auto-update, --once): every child gets it
// through jobEnvKeeps, so its wp stays as it is too. The returned func puts
// the environment back, so it lasts no longer than the runner.
func setUpdateOptOut(o runnerOpts) (restore func()) {
	if !o.NoAutoUpdate && !o.Once {
		return func() {}
	}
	was, had := os.LookupEnv(updateOptOutVar)
	_ = os.Setenv(updateOptOutVar, "1")
	return func() {
		if had {
			_ = os.Setenv(updateOptOutVar, was)
		} else {
			_ = os.Unsetenv(updateOptOutVar)
		}
	}
}
