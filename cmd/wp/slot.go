package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// slotWaitNotice is how long run waits before saying it is waiting.
var slotWaitNotice = 5 * time.Second

// slotPoll is how often a waiting run retries the locks.
var slotPoll = 100 * time.Millisecond

var slotCmd = &command{
	name: "slot", group: "Code and CI", args: "run NAME [-n N] [--all] -- CMD… | ls | send | replay",
	summary: "run a command once a per-machine slot is free",
	about: `A per-host semaphore so concurrent test suites take turns. run waits for
a free slot of NAME, runs CMD holding it and exits with CMD's code. N
defaults to half the cores. Runs whose last five (same repo and command) took
5 minutes or more are long: they may only use the lower half of the slots, so
short runs (the rest) never queue behind them, and a worktree runs one long
run of a NAME at a time (a second waits behind its first; short runs from it
go straight to a slot). ls shows the slots, who waits (and whether behind its
own worktree) and the waits of the last 24h, including runs that gave up.
Each finished hold and each wait that gave up is also
sent to the server (not when WAYPOINT_SLOT_SEND=off or no server is set), by
the next run in the background or by send; replay runs the recorded holds
through the policy again, and through other settings (WP-1646).
A nested run of the same NAME reuses the slot it
is in. A run of tests drops credential-named variables, as wp testsum does
(--keep-env NAME passes one through; WP-1065). Each successful run's time is
kept (runs.jsonl in the slot directory); a run that has to wait, and again
one that starts, says on stderr when it expects to finish, from the median
of the command's recent runs, and says nothing without any (WP-1571).

In the tests slot a go test of a whole heavy package or ./... with no -run,
in a repository with .waypoint/heavy-tests, runs the tests beside what you
changed since the base instead, and says so first, with the command that runs
everything. --all, or WP_TEST_ALL=1, runs it whole, as does a flag that names a
file (-coverprofile, -o, -cpuprofile, -trace, …). Such a whole run is long
from its first run. Runs of the two kinds keep separate times (WP-1877).

Waiters are served first come, first served: a run tries a slot only when no
earlier waiter that may use it is still waiting (WP-1876).

Each run of a command through the slot (run, testsum, ship --check) gets one
CPU bound, WAYPOINT_SLOT_CPU percent of one core (default 200; off for none),
so a go test in a slot can't take every core:
  cgroup  the process already sits under a cpu.max quota (a sandbox): nothing added
  runner  a runner job: its load governor paces it; Go is held to the quota
  scope   a systemd user scope (systemd-run --user) with CPUQuota in
          waypoint-tests.slice (a scope whose cgroup shows no quota counts as
          none); needs a user bus (XDG_RUNTIME_DIR)
  env     elsewhere: GOMAXPROCS and GOFLAGS -p set, unless you set them. This
          binds Go builds and tests only, not other programs the command runs.
  none    WAYPOINT_SLOT_CPU=off
ls shows the bound of each running hold and how many runs of the last 24h had none.`,
	examples: []string{"wp slot run tests -- go test -run '^TestSlot' ./cmd/wp", "wp slot ls"},
}

func init() {
	slotCmd.subs = []*command{
		{name: "run", args: "NAME [-n N] [--all] -- CMD…", summary: "wait for a free slot, run CMD, pass its exit code through",
			run: cmdSlotRun},
		{name: "send", summary: "send the recorded slot holds to the server now",
			run: cmdSlotSend},
		{name: "replay", args: "[--days 7] [--threshold 5m] [--long-share 0.5] [--slots N] [--machine NAME] [--local]",
			summary: "replay recorded slot holds under the policy and under another one", run: cmdSlotReplay},
		{name: "ls", aliases: []string{"list"}, summary: "show the slots, who holds and awaits them, and recent waits",
			run: cmdSlotLs},
	}
}

func slotDir() (string, error) {
	if d := os.Getenv("WAYPOINT_SLOT_DIR"); d != "" {
		return filepath.Abs(d)
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "waypoint", "slots"), nil
}

// defaultSlots is how many runs share a slot name: half the cores. The CPU
// quota of a run (slot_cpu.go) does not change it: replaying the recorded
// holds with fewer slots made waits much longer, and changing both at once
// would hide what the quota does (WP-1876).
func defaultSlots() int { return max(1, runtime.NumCPU()/2) }

// slotHold is a held slot.
type slotHold struct {
	f      *os.File
	idx    int
	since  int64
	ctxKey string
}

// write puts the holder line in the lock file: pid, start, folder, arguments,
// session, run key and the CPU bound the run has ("" until known).
func (h *slotHold) write(bound string) {
	wd, _ := os.Getwd()
	_ = h.f.Truncate(0)
	_, _ = h.f.WriteAt([]byte(fmt.Sprintf("%d\t%d\t%s\t%s\t%s\t%s\t%s\n", os.Getpid(), h.since, slotField(wd), slotField(strings.Join(os.Args[1:], " ")), slotField(slotSession()), slotField(h.ctxKey), bound)), 0)
}

func (h *slotHold) release() { _ = h.f.Close() }

// acquireSlot waits for one of n slots called name. notify gets called once
// if the wait passes slotWaitNotice. Inside an agent job the wait is noted
// for the runner (WP-1618): its work clock leaves slot waits out.
func acquireSlot(ctx context.Context, name string, n int, notify func()) (*slotHold, error) {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	return acquireSlotIn(ctx, name, order, notify, nil)
}

// acquireSlotIn is acquireSlot trying only the slots in order, in that order.
// firstMiss, when not nil, is called once when the first pass finds none free.
func acquireSlotIn(ctx context.Context, name string, order []int, notify, firstMiss func()) (*slotHold, error) {
	defer noteSlotWait(name)()
	dir, err := slotDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	start := time.Now()
	notified := false
	me := slotArrival(ctx, start)
	// Registered before the first try, so a run that comes later can't take a
	// slot this one may use (WP-1876).
	remove := slotAddWaiterFor(dir, name, me, order)
	defer remove()
	missed := false
	for {
		slotTouchWaiter(dir, name)
		blocked := slotBlockedSlots(dir, name, me.UnixNano())
		for _, i := range order {
			if blocked(i) {
				continue
			}
			f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%s.%d.lock", name, i)), os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- name is validated
			if err != nil {
				return nil, err
			}
			ok, err := slotTryLock(f)
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			if ok {
				h := &slotHold{f: f, idx: i, since: time.Now().Unix(), ctxKey: slotCtxKey(ctx)}
				h.write(slotBoundOf(ctx))
				return h, nil
			}
			_ = f.Close()
		}
		if !missed {
			missed = true
			if firstMiss != nil {
				firstMiss()
			}
		}
		if !notified && time.Since(start) > slotWaitNotice {
			notified = true
			if notify != nil {
				notify()
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(slotPoll):
		}
	}
}

type slotCtxKeyT struct{}

type (
	slotArrivalKeyT struct{}
	slotBoundKeyT   struct{}
)

// slotArrival is when the run asking for the slot came: the time its caller
// gave on ctx (a run that waited behind its worktree's long run came before
// that wait), else now.
func slotArrival(ctx context.Context, now time.Time) time.Time {
	if t, ok := ctx.Value(slotArrivalKeyT{}).(time.Time); ok {
		return t
	}
	return now
}

// slotBoundOf is the CPU bound the run on ctx is about to have, "" when not told.
func slotBoundOf(ctx context.Context) string {
	k, _ := ctx.Value(slotBoundKeyT{}).(string)
	return k
}

// slotCtxKey is the run key (slotRunKey) the slot is taken for, "" when none.
func slotCtxKey(ctx context.Context) string {
	k, _ := ctx.Value(slotCtxKeyT{}).(string)
	return k
}

// slotField flattens s onto one tab-free line of at most 200 bytes, so a
// lock file always parses as four fields.
func slotField(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func validSlotName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// splitDashes splits args at the first "--": flags before it are wp's, what
// follows is a command whose own flags must not be parsed.
func splitDashes(args []string) (before, cmd []string, found bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

// slotHeldEnv names the slots the current process tree already holds,
// comma-separated, so a nested run of the same NAME doesn't wait on itself.
const slotHeldEnv = "WAYPOINT_SLOTS_HELD"

// Each entry is NAME=DIR@PID (PID is the wp holding the slot), so a child that points WAYPOINT_SLOT_DIR somewhere
// else still has to take that directory's slot.
func slotHeld(name string) bool {
	dir, err := slotDir()
	if err != nil {
		return false
	}
	want := name + "=" + url.PathEscape(dir)
	for _, h := range strings.Split(os.Getenv(slotHeldEnv), ",") {
		base, pidStr, ok := strings.Cut(h, "@")
		if !ok || base != want {
			continue
		}
		if pid, err := strconv.Atoi(pidStr); err == nil && pid > 0 && slotPidAlive(pid) {
			return true
		}
	}
	return false
}

// runInSlotDo holds a slot of NAME while do runs, and records the hold and
// (when do exits 0) the run's time under cmd, the key its history is kept by
// ("" when this process tree already held the slot). started is called once
// the slot is held, before do.
func (c *cli) runInSlotDo(name string, n int, run slotRun, cmd string, started func(), do func(ctx context.Context, env []string) (int, error)) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bound, _ := slotBoundKind()
	// ran is the bound the command had, told by runSlotBound as soon as it
	// is known (a scope that can't start falls back to the env bound), and
	// passed on to the lock file once the slot is held.
	ran := &slotRan{}
	ctx = context.WithValue(ctx, slotRanKeyT{}, ran)
	taken, err := c.takeSlotRun(context.WithValue(ctx, slotBoundKeyT{}, bound), name, n, run, cmd)
	if err != nil {
		return 0, err
	}
	env, setExit := taken.env, taken.setExit
	ran.set = taken.setBound
	defer taken.release()
	dir, derr := slotDir()
	if cmd != "" && derr == nil {
		if note := slotRunningNote(dir, name, cmd); note != "" {
			_, _ = fmt.Fprintln(c.err, note)
		}
	}
	if started != nil {
		started()
	}
	begun := time.Now()
	code, err := do(ctx, env)
	if ran.kind != "" {
		taken.setBound(ran.kind)
	}
	if err == nil {
		setExit(code)
	}
	if err == nil && code == 0 && cmd != "" && derr == nil {
		slotRecordRun(dir, slotRunRec{Name: name, Cmd: cmd, Repo: slotRepoKey(), At: time.Now().Unix(), Secs: time.Since(begun).Seconds()})
	}
	return code, err
}

// runSlotCmd runs argv with env and returns its exit code (128+N when a
// signal ended it). A stop on ctx is passed on as SIGTERM and the command is
// let finish, so a slot stays held exactly as long as the command runs.
func runSlotCmd(ctx context.Context, env, argv []string, in io.Reader, out, errw io.Writer) (int, error) {
	return runSlotCmdIn(ctx, "", env, argv, in, out, errw)
}

// runSlotCmdIn is runSlotCmd in folder dir ("" for this one), under the CPU
// bound slotBoundKind picks (slot_cpu.go).
func runSlotCmdIn(ctx context.Context, dir string, env, argv []string, in io.Reader, out, errw io.Writer) (int, error) {
	code, _, err := runSlotBound(ctx, dir, env, argv, in, out, errw)
	return code, err
}

// runSlotPlain runs argv as it is, without a scope.
func runSlotPlain(ctx context.Context, dir string, env, argv []string, in io.Reader, out, errw io.Writer) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 G702 -- running the command is the point
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = in
	cmd.Stdout, cmd.Stderr = out, errw
	if err := cmd.Start(); err != nil {
		return 127, fmt.Errorf("%s: %w", argv[0], err)
	}
	return waitSlotCmd(ctx, cmd)
}

// waitSlotCmd waits for a started cmd and returns its exit code (128+N when
// a signal ended it). A stop on ctx is passed on as SIGTERM and the command
// is let finish, so a slot stays held exactly as long as the command runs.
func waitSlotCmd(ctx context.Context, cmd *exec.Cmd) (int, error) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	return 1, err
}

func cmdSlotRun(c *cli, args []string) error {
	before, argv, found := splitDashes(args)
	fs := c.flags()
	n := fs.Int("n", 0, "slots to share (default: half the cores)")
	all := fs.Bool("all", false, "with the tests slot, run a whole-suite go test whole: without it the slot runs the tests beside your changes (WP-1877)")
	var keep keepEnvFlag
	fs.Var(&keep, "keep-env", "with the tests slot, pass the credential-named variable `NAME` through (repeatable)")
	pos, err := c.parse(fs, before)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !found || len(argv) == 0 {
		return c.usage("need NAME and a command after --")
	}
	if !validSlotName(pos[0]) {
		return c.usage("NAME is letters, digits, - and _")
	}
	if *n < 0 {
		return c.usage("-n must be positive")
	}
	if *n == 0 {
		*n = defaultSlots()
	}
	if len(keep) > 0 && pos[0] != "tests" {
		return c.usage("--keep-env is for the tests slot; other slots run with the whole environment")
	}
	var envf func([]string) []string
	var dropped []string
	if pos[0] == "tests" {
		envf = func(env []string) []string {
			env, dropped = testEnv(env, keep)
			return env
		}
	}
	code, err := c.runInSlotWith(pos[0], *n, argv, c.out, c.err, nil, envf, slotOpts{all: *all, who: "wp slot", allCmd: "wp slot run tests --all --"})
	if err != nil {
		return err
	}
	if code != 0 {
		if note := withheldNote(dropped); note != "" {
			_, _ = fmt.Fprintln(c.err, note)
		}
		return exitCode(code)
	}
	return nil
}

func cmdSlotLs(c *cli, args []string) error {
	fs := c.flags()
	if pos, err := c.parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return c.usage("wp slot ls takes no arguments")
	}
	dir, err := slotDir()
	if err != nil {
		return err
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.lock"))
	sort.Strings(files)
	if len(files) == 0 {
		if c.slotQueueLs(dir) {
			return nil
		}
		_, _ = fmt.Fprintf(c.out, "no slots in use yet (%d per name by default; %s)\n", defaultSlots(), dir)
		return nil
	}
	for _, p := range files {
		f, err := os.OpenFile(p, os.O_RDWR, 0o600) // #nosec G304 -- from our own directory
		if err != nil {
			continue
		}
		label := strings.TrimSuffix(filepath.Base(p), ".lock")
		ok, err := slotTryLock(f)
		if err == nil && ok {
			_, _ = fmt.Fprintf(c.out, "%-12s free\n", label)
			_ = f.Close()
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(f, 4096))
		_ = f.Close()
		parts := strings.SplitN(strings.TrimSpace(string(b)), "\t", 7)
		if len(parts) < 4 {
			_, _ = fmt.Fprintf(c.out, "%-12s held\n", label)
			continue
		}
		since := ""
		if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			since = time.Since(time.Unix(ts, 0)).Round(time.Second).String()
		}
		bound := ""
		if len(parts) == 7 && parts[6] != "" {
			bound = " [cpu bound: " + parts[6] + "]"
		}
		_, _ = fmt.Fprintf(c.out, "%-12s held pid %s for %s in %s: wp %s%s\n", label, parts[0], since, parts[2], parts[3], bound)
	}
	c.slotQueueLs(dir)
	return nil
}

// takeSlot waits for a slot of NAME unless this process tree already holds
// one, and returns the environment for commands run in it (marking the slot
// held, so a nested run doesn't wait on itself) and what lets it go.
func (c *cli) takeSlot(ctx context.Context, name string, n int) (env []string, release func(), err error) {
	return c.takeSlotFor(ctx, name, n, slotRunOfArgs(slotOSArgs[1:]))
}

// takeSlotFor is takeSlot for a run, whose key the wait history is kept by
// (so one command has one key however wp was asked to run it).
func (c *cli) takeSlotFor(ctx context.Context, name string, n int, run slotRun) (env []string, release func(), err error) {
	env, release, _, err = c.takeSlotExit(ctx, name, n, run, "")
	return env, release, err
}

// takeSlotExit is takeSlotFor that also returns setExit, which tells the hold
// what the command run in the slot exited with before release (WP-1646). A
// caller that doesn't call it leaves the exit unknown. While it waits it also
// says when it expects to finish if it knows how long etaCmd (slotCmdKey)
// runs; "" says nothing (WP-1571).
func (c *cli) takeSlotExit(ctx context.Context, name string, n int, run slotRun, etaCmd string) (env []string, release func(), setExit func(int), err error) {
	t, err := c.takeSlotRun(ctx, name, n, run, etaCmd)
	if err != nil {
		return nil, nil, nil, err
	}
	return t.env, t.release, t.setExit, nil
}

// slotTaken is a slot taken (or one this process tree already holds) and what
// the run in it tells the hold: setExit what the command exited with, setBound
// the CPU bound it ran under (slot_cpu.go).
type slotTaken struct {
	env      []string
	release  func()
	setExit  func(int)
	setBound func(string)
}

func (c *cli) takeSlotRun(ctx context.Context, name string, n int, run slotRun, etaCmd string) (slotTaken, error) {
	env := os.Environ()
	if slotHeld(name) {
		return slotTaken{env, func() {}, func(int) {}, func(string) {}}, nil
	}
	hold, err := c.acquireQueued(ctx, name, n, run, etaCmd)
	if err != nil {
		return slotTaken{}, err
	}
	held := name
	if dir, err := slotDir(); err == nil {
		held += "=" + url.PathEscape(dir)
	}
	held += "@" + strconv.Itoa(os.Getpid())
	if prev := os.Getenv(slotHeldEnv); prev != "" {
		held = prev + "," + held
	}
	kept := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, slotHeldEnv+"=") {
			kept = append(kept, e)
		}
	}
	return slotTaken{append(kept, slotHeldEnv+"="+held), hold.release, hold.setExit, hold.setBound}, nil
}
