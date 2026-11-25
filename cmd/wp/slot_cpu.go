package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// A run in the tests slot gets exactly one CPU bound (WP-ADR161, WP-1876): the
// slot counts runs, but one go test uses every core.
const (
	// slotCPUEnv is the quota of a run as a percent of one core, or "off".
	slotCPUEnv = "WAYPOINT_SLOT_CPU"
	// slotCPUDefault is that quota unset: two cores.
	slotCPUDefault = 200
	// slotCPUWeight is the scope's CPUWeight. It equals the sandbox slice's
	// (sandbox_install.go: CPUWeight=50), so a test run weighs what a
	// sandboxed session does when they compete.
	slotCPUWeight = 50
	// slotCPUSlice holds every scope this makes, so they share one weight.
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
// process already sits under, a runner job (its governor paces jobs, and it
// has no user bus), a systemd user scope, the environment.
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
	st, err := os.Stat(filepath.Join(rt, "bus"))
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
		if b, err := os.ReadFile(filepath.Join(root, p, "cpu.max")); err == nil { // #nosec G304 -- a cgroup file
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
// started, closes fd 3 so the command doesn't have it, and becomes the
// command. systemd-run exits 1 when it can't start the scope, as a command
// may, so without it the two can't be told apart.
const slotScopeMarker = `printf x >&3; exec 3>&-; exec "$@"`

// scopeArgv is the command line that runs argv in a systemd user scope with
// the quota (percent); bin is systemd-run.
func scopeArgv(bin string, quota int, argv []string) []string {
	out := []string{bin, "--user", "--scope", "--quiet", "--collect", "--slice=" + slotCPUSlice,
		"-p", "CPUQuota=" + strconv.Itoa(quota) + "%", "-p", "CPUWeight=" + strconv.Itoa(slotCPUWeight),
		"--", "/bin/sh", "-c", slotScopeMarker, "sh"}
	return append(out, argv...)
}

// goBoundEnv is env with Go held to the cores of the quota: GOMAXPROCS and a
// -p in GOFLAGS, each only when the caller hasn't set one. It binds Go and
// nothing else.
func goBoundEnv(env []string, quota int) []string {
	k := strconv.Itoa(max(1, quota/100))
	procs, flags := false, ""
	out := make([]string, 0, len(env)+2)
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "GOMAXPROCS="):
			procs = true
		case strings.HasPrefix(e, "GOFLAGS="):
			flags = strings.TrimPrefix(e, "GOFLAGS=")
			continue
		}
		out = append(out, e)
	}
	if !procs {
		out = append(out, "GOMAXPROCS="+k)
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
	if flags != "" {
		out = append(out, "GOFLAGS="+flags)
	}
	return out
}

// runSlotBound runs argv in dir ("" for this one) with env under the one CPU
// bound slotBoundKind picks, and returns its exit code and the bound it had.
// A scope that can't start falls back to the env bound; the command runs once.
func runSlotBound(ctx context.Context, dir string, env, argv []string, in io.Reader, out, errw io.Writer) (int, string, error) {
	kind, quota := slotBoundKind()
	if kind == slotBoundScope {
		if code, started, err := runInScope(ctx, dir, env, argv, quota, in, out, errw); started {
			return code, kind, err
		}
		kind = slotBoundEnv
	}
	if kind == slotBoundEnv || kind == slotBoundRunner {
		env = goBoundEnv(env, quota)
	}
	code, err := runSlotPlain(ctx, dir, env, argv, in, out, errw)
	return code, kind, err
}

// runInScope runs argv in a systemd user scope. started false says the scope
// did not start and argv did not run.
func runInScope(ctx context.Context, dir string, env, argv []string, quota int, in io.Reader, out, errw io.Writer) (code int, started bool, err error) {
	bin, lerr := exec.LookPath("systemd-run")
	if lerr != nil {
		return 0, false, nil
	}
	if _, lerr := exec.LookPath(argv[0]); lerr != nil {
		return 0, false, nil // the plain run says what is missing
	}
	r, w, perr := os.Pipe()
	if perr != nil {
		return 0, false, nil
	}
	defer func() { _ = r.Close() }()
	full := scopeArgv(bin, quota, argv)
	cmd := exec.Command(full[0], full[1:]...) // #nosec G204 G702 -- running the command is the point
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, env, in, out, errw
	cmd.ExtraFiles = []*os.File{w}
	serr := cmd.Start()
	_ = w.Close()
	if serr != nil {
		return 0, false, nil
	}
	code, err = waitSlotCmd(ctx, cmd)
	_ = r.SetReadDeadline(time.Now().Add(time.Second))
	b, _ := io.ReadAll(r)
	return code, len(b) > 0, err
}
