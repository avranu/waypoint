package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A run in the tests slot gets exactly one CPU bound (WP-ADR161, WP-1876): the
// slot counts runs, but one go test uses every core.
const (
	// slotCPUEnv is the quota of a run as a percent of one core, or "off".
	slotCPUEnv = "WAYPOINT_SLOT_CPU"
	// slotCPUDefault is that quota unset: two cores. It is a starting value
	// that has not been measured against run times or waits; set it with
	// WAYPOINT_SLOT_CPU.
	slotCPUDefault = 200
	// slotCPUSlice holds every scope this makes.
	slotCPUSlice = "waypoint-tests.slice"
)

// The kinds of bound a run had (waitRec.Bound).
const (
	slotBoundCgroup = "cgroup" // its own cgroup already has a cpu.max quota
	slotBoundRunner = "runner" // a runner job: the load governor paces it
	slotBoundScope  = "scope"  // a systemd user scope with CPUQuota
	slotBoundEnv    = "env"    // GOMAXPROCS and GOFLAGS -p: Go only
	slotBoundNone   = "none"   // WAYPOINT_SLOT_CPU=off
)

// slotCPUWarn is where a bad WAYPOINT_SLOT_CPU is said.
var slotCPUWarn io.Writer = os.Stderr

// slotCPUQuota is the quota in percent of one core; on false means off. A
// value that is not a number is said once on stderr and the default is used:
// the run is not refused over a setting.
func slotCPUQuota() (quota int, on bool) {
	v := strings.TrimSpace(os.Getenv(slotCPUEnv))
	switch strings.ToLower(v) {
	case "":
		return slotCPUDefault, true
	case "off":
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
	if err != nil || n <= 0 {
		_, _ = fmt.Fprintf(slotCPUWarn, "wp slot: %s=%q is not a percent of one core (for example 200) or off; using %d\n", slotCPUEnv, v, slotCPUDefault)
		return slotCPUDefault, true
	}
	return n, true
}

// slotCPUProbes are the facts the choice of bound depends on, so tests can
// give them.
type slotCPUProbes struct {
	goos      string
	cgroup    func() bool // the process's cgroup is limited by cpu.max
	runnerJob func() bool
	userBus   func() bool // systemd-run is there and a user bus is reachable
}

// slotCPUProbeFn is where the probes come from; a variable for the tests.
var slotCPUProbeFn = realSlotCPUProbes

func realSlotCPUProbes() slotCPUProbes {
	return slotCPUProbes{goos: runtime.GOOS, cgroup: ownCgroupBounded, runnerJob: inRunnerJob, userBus: userBusReachable}
}

// chooseCPUBound is the kind of bound a run gets, in order: off, a quota the
// process already sits under, a runner job (the runner's governor already
// paces its jobs, so a scope would only add a second limit), a systemd user
// scope, the environment. The runner branch comes before the user-bus probe
// for that reason, whether or not the runner has a bus.
func chooseCPUBound(p slotCPUProbes, on bool) string {
	switch {
	case !on:
		return slotBoundNone
	case p.goos == "linux" && p.cgroup():
		return slotBoundCgroup
	case p.runnerJob():
		return slotBoundRunner
	case p.goos == "linux" && p.userBus():
		return slotBoundScope
	}
	return slotBoundEnv
}

// slotBoundKind is the bound a run started now gets and its quota.
func slotBoundKind() (kind string, quota int) {
	q, on := slotCPUQuota()
	return chooseCPUBound(slotCPUProbeFn(), on), q
}

// inRunnerJob says whether this is a command of a runner's job: the
// runner marks a Claude Code job (runnerJobEnv) and gives an agent job its
// slot-waits file (slotWaitsEnv).
func inRunnerJob() bool {
	return os.Getenv(runnerJobEnv) != "" || os.Getenv(slotWaitsEnv) != ""
}

// userBusReachable says whether systemd-run can reach a user bus: it is on
// PATH and XDG_RUNTIME_DIR has the bus socket.
func userBusReachable() bool {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return false
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(rt, "bus")) // #nosec G703 -- the user's own runtime folder, only whether the socket is there
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// ownCgroupBounded says whether the process's cgroup, or one above it, has a
// cpu.max quota (cgroup v2).
func ownCgroupBounded() bool {
	return cgroupBounded("/proc/self/cgroup", "/sys/fs/cgroup")
}

// cgroupBounded reads the process's cgroup from procCgroup and walks up from
// it under root looking for a cpu.max whose quota is not "max".
func cgroupBounded(procCgroup, root string) bool {
	b, err := os.ReadFile(procCgroup) // #nosec G304 -- /proc/self/cgroup
	if err != nil {
		return false
	}
	rel := ""
	for _, l := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			rel = p
		}
	}
	if rel == "" {
		return false
	}
	for p := filepath.Clean("/" + rel); ; p = filepath.Dir(p) {
		if b, err := os.ReadFile(filepath.Join(root, p, "cpu.max")); err == nil { // #nosec G304 G703 -- a cgroup file
			if f := strings.Fields(string(b)); len(f) > 0 && f[0] != "max" {
				return true
			}
		}
		if p == "/" {
			return false
		}
	}
}

// slotScopeMarker runs first in the scope: it says on fd 3 that the scope
// started, "x" when the scope's cgroup has a cpu.max quota and "n" when it
// has none (unreadable counts as none), closes fd 3 so the command doesn't
// have it, and becomes the command. A scope with no quota bounds nothing, so
// there it first sets the Go limits runInScope handed it (slotNoQuota…), and
// in either case drops them. systemd-run exits 1 when it can't start the
// scope, as a command may, so without the marker the two can't be told apart.
const slotScopeMarker = `q=; { read q _ < /sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)/cpu.max; } 2>/dev/null; m=n; [ -n "$q" ] && [ "$q" != max ] && m=x; if [ $m = n ]; then [ -n "${WP_SLOT_NOQUOTA_GOMAXPROCS:-}" ] && export GOMAXPROCS="$WP_SLOT_NOQUOTA_GOMAXPROCS"; [ -n "${WP_SLOT_NOQUOTA_GOFLAGS:-}" ] && export GOFLAGS="$WP_SLOT_NOQUOTA_GOFLAGS"; fi; unset WP_SLOT_NOQUOTA_GOMAXPROCS WP_SLOT_NOQUOTA_GOFLAGS; printf %s "$m" >&3; exec 3>&-; exec "$@"`

// slotNoQuotaPrefix names the variables that carry the Go limits into a
// scope, for slotScopeMarker to apply should the scope have no quota.
const slotNoQuotaPrefix = "WP_SLOT_NOQUOTA_"

// noQuotaEnv is env (nil: this process's) with what goBoundEnv would add to
// it carried beside, under slotNoQuotaPrefix.
func noQuotaEnv(env []string, quota int) []string {
	if env == nil {
		env = os.Environ()
	}
	out := slices.Clone(env)
	for _, e := range goBoundEnv(env, quota, slotSavedGoFlagsFn) {
		if (strings.HasPrefix(e, "GOMAXPROCS=") || strings.HasPrefix(e, "GOFLAGS=")) && !slices.Contains(env, e) {
			out = append(out, slotNoQuotaPrefix+e)
		}
	}
	return out
}

// scopeArgv is the command line that runs argv in a systemd user scope with
// the quota (percent); bin is systemd-run.
func scopeArgv(bin string, quota int, argv []string) []string {
	out := []string{bin, "--user", "--scope", "--quiet", "--collect", "--slice=" + slotCPUSlice,
		"-p", "CPUQuota=" + strconv.Itoa(quota) + "%",
		"--", "/bin/sh", "-c", slotScopeMarker, "sh"}
	return append(out, argv...)
}

// slotSavedGoFlagsFn is what go env GOFLAGS prints, the flags saved with go
// env -w; a variable for the tests.
var slotSavedGoFlagsFn = savedGoFlags

// savedGoFlags asks go for its GOFLAGS setting; "" when go is not found or
// fails, which leaves the bound as it was without this.
func savedGoFlags() string {
	bin, err := exec.LookPath("go")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "env", "GOFLAGS").Output() // #nosec G204 -- a fixed argument
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// goBoundEnv is env with Go held to the cores of the quota: GOMAXPROCS and a
// -p in GOFLAGS, each only when the caller hasn't set one. It binds Go and
// nothing else. An environment GOFLAGS overrides the flags saved with go env
// -w, so when the environment has none the -p is added to the saved ones
// (saved, nil for none), and left out when they have a -p of their own.
func goBoundEnv(env []string, quota int, saved func() string) []string {
	k := strconv.Itoa(max(1, quota/100))
	procs, flags, fromEnv := false, "", false
	out := make([]string, 0, len(env)+2)
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "GOMAXPROCS="):
			procs = true
		case strings.HasPrefix(e, "GOFLAGS="):
			flags, fromEnv = strings.TrimPrefix(e, "GOFLAGS="), true
			continue
		}
		out = append(out, e)
	}
	if !procs {
		out = append(out, "GOMAXPROCS="+k)
	}
	if !fromEnv && saved != nil {
		flags = saved()
	}
	hasP := false
	for _, f := range strings.Fields(flags) {
		if f == "-p" || f == "--p" || strings.HasPrefix(f, "-p=") || strings.HasPrefix(f, "--p=") {
			hasP = true
		}
	}
	if !hasP {
		flags = strings.TrimSpace(flags + " -p=" + k)
	}
	if flags != "" && (fromEnv || !hasP) {
		out = append(out, "GOFLAGS="+flags)
	}
	return out
}

// runSlotBound runs argv in dir ("" for this one) with env under the one CPU
// bound slotBoundKind picks, and returns its exit code and the bound it had.
// A scope that can't start falls back to the env bound; the command runs once.
func runSlotBound(ctx context.Context, dir string, env, argv []string, in io.Reader, out, errw io.Writer) (int, string, error) {
	kind, quota := slotBoundKind()
	tell := func() {
		if p, ok := ctx.Value(slotRanKeyT{}).(*slotRan); ok {
			p.kind = kind
			if p.set != nil {
				p.set(kind)
			}
		}
	}
	if kind == slotBoundScope {
		code, started, _, err := runInScope(ctx, dir, env, argv, quota, in, out, errw, func(bounded bool) {
			// The marker says the scope started: from here the command runs
			// under it, and the lock file says so before its work begins.
			if !bounded {
				kind = slotBoundEnv // no quota: the marker set the Go limits
			}
			tell()
		})
		if started {
			return code, kind, err
		}
		if ctx.Err() != nil {
			// Cancelled before the scope started: the next command does not run.
			return 128 + int(syscall.SIGTERM), kind, nil
		}
		kind = slotBoundEnv
	}
	if kind == slotBoundEnv || kind == slotBoundRunner {
		env = goBoundEnv(env, quota, slotSavedGoFlagsFn)
	}
	tell()
	code, err := runSlotPlain(ctx, dir, env, argv, in, out, errw)
	return code, kind, err
}

// slotRanKeyT is the context key of a *slotRan runSlotBound tells the bound
// it runs under, for a hold whose commands are run by its caller (runInSlotDo).
type slotRanKeyT struct{}

// slotRan is the bound the command ran under, and set (when not nil) is told
// it the moment it is known, before the command's work starts, so the lock
// file wp slot ls reads is right for the whole run.
type slotRan struct {
	kind string
	set  func(string)
}

// runInScope runs argv in a systemd user scope. started false says the scope
// did not start and argv did not run; bounded says the scope has a quota.
// onStart (when not nil) is called with bounded as soon as the marker says the
// scope started, while the command runs.
func runInScope(ctx context.Context, dir string, env, argv []string, quota int, in io.Reader, out, errw io.Writer, onStart func(bounded bool)) (code int, started, bounded bool, err error) {
	bin, lerr := exec.LookPath("systemd-run")
	if lerr != nil {
		return 0, false, false, nil
	}
	if _, lerr := exec.LookPath(argv[0]); lerr != nil {
		return 0, false, false, nil // the plain run says what is missing
	}
	r, w, perr := os.Pipe()
	if perr != nil {
		return 0, false, false, nil
	}
	defer func() { _ = r.Close() }()
	full := scopeArgv(bin, quota, argv)
	cmd := exec.Command(full[0], full[1:]...) // #nosec G204 G702 -- running the command is the point
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, noQuotaEnv(env, quota), in, out, errw
	cmd.ExtraFiles = []*os.File{w}
	serr := cmd.Start()
	_ = w.Close()
	if serr != nil {
		return 0, false, false, nil
	}
	// The marker writes one byte and closes its end before the command
	// starts, so the read ends then, not when the command does.
	var b []byte
	read := make(chan struct{})
	go func() {
		defer close(read)
		b, _ = io.ReadAll(r)
		if len(b) > 0 && onStart != nil {
			onStart(b[0] == 'x')
		}
	}()
	code, err = waitSlotCmd(ctx, cmd)
	_ = r.SetReadDeadline(time.Now().Add(time.Second))
	<-read
	return code, len(b) > 0, len(b) > 0 && b[0] == 'x', err
}
