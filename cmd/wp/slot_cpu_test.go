package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSlotCPUQuota(t *testing.T) {
	var warn bytes.Buffer
	old := slotCPUWarn
	slotCPUWarn = &warn
	t.Cleanup(func() { slotCPUWarn = old })
	for _, c := range []struct {
		env   string
		quota int
		on    bool
		warn  bool
	}{
		{"", 200, true, false}, {"300", 300, true, false}, {"150%", 150, true, false},
		{"off", 0, false, false}, {"OFF", 0, false, false},
		{"lots", 200, true, true}, {"-5", 200, true, true}, {"0", 200, true, true},
	} {
		warn.Reset()
		t.Setenv(slotCPUEnv, c.env)
		q, on := slotCPUQuota()
		if q != c.quota || on != c.on || (warn.Len() > 0) != c.warn {
			t.Errorf("%s=%q: quota %d on %v warn %q, want %d %v warn %v", slotCPUEnv, c.env, q, on, warn.String(), c.quota, c.on, c.warn)
		}
	}
}

func TestChooseCPUBound(t *testing.T) {
	p := func(goos string, cgroup, runner, bus bool) slotCPUProbes {
		return slotCPUProbes{goos: goos, cgroup: func() bool { return cgroup }, runnerJob: func() bool { return runner }, userBus: func() bool { return bus }}
	}
	for _, c := range []struct {
		name string
		p    slotCPUProbes
		on   bool
		want string
	}{
		{"off beats everything", p("linux", true, true, true), false, slotBoundNone},
		{"cgroup quota beats a runner job and the bus", p("linux", true, true, true), true, slotBoundCgroup},
		{"runner job beats the bus", p("linux", false, true, true), true, slotBoundRunner},
		{"bus on linux", p("linux", false, false, true), true, slotBoundScope},
		{"no bus on linux", p("linux", false, false, false), true, slotBoundEnv},
		{"macOS has no scope", p("darwin", false, false, true), true, slotBoundEnv},
		{"macOS has no cgroup", p("darwin", true, false, false), true, slotBoundEnv},
		{"windows", p("windows", false, false, true), true, slotBoundEnv},
		{"runner job off linux", p("darwin", false, true, false), true, slotBoundRunner},
	} {
		if got := chooseCPUBound(c.p, c.on); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInRunnerJob(t *testing.T) {
	t.Setenv(runnerJobEnv, "")
	t.Setenv(slotWaitsEnv, "")
	if inRunnerJob() {
		t.Error("plain shell is not a runner job")
	}
	t.Setenv(runnerJobEnv, "1")
	if !inRunnerJob() {
		t.Error("a Claude job is")
	}
	t.Setenv(runnerJobEnv, "")
	t.Setenv(slotWaitsEnv, "/tmp/waits")
	if !inRunnerJob() {
		t.Error("an agent job with a waits file is")
	}
}

func TestCgroupBounded(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(t.TempDir(), "cgroup")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setProc := func(s string) {
		t.Helper()
		if err := os.WriteFile(proc, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if cgroupBounded(filepath.Join(root, "missing"), root) {
		t.Error("no /proc file: not bounded")
	}
	setProc("0::/a/b/c.scope\n")
	write("a/b/c.scope/cpu.max", "max 100000\n")
	write("a/cpu.max", "max 100000\n")
	if cgroupBounded(proc, root) {
		t.Error("only max everywhere: not bounded")
	}
	write("a/cpu.max", "600000 100000\n")
	if !cgroupBounded(proc, root) {
		t.Error("a quota on an ancestor bounds it")
	}
	write("a/cpu.max", "max 100000\n")
	write("a/b/c.scope/cpu.max", "200000 100000\n")
	if !cgroupBounded(proc, root) {
		t.Error("a quota on its own cgroup bounds it")
	}
	setProc("12:cpu:/legacy\n0::/a/b/c.scope\n")
	if !cgroupBounded(proc, root) {
		t.Error("v2 line among v1 lines")
	}
	setProc("12:cpu:/legacy\n")
	if cgroupBounded(proc, root) {
		t.Error("cgroup v1 only: unknown, not bounded")
	}
}

func TestScopeArgv(t *testing.T) {
	got := scopeArgv("/usr/bin/systemd-run", 200, []string{"go", "test", "./x"})
	want := []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--slice=waypoint-tests.slice",
		"-p", "CPUQuota=200%", "--", "/bin/sh", "-c", slotScopeMarker, "sh", "go", "test", "./x"}
	if !slices.Equal(got, want) {
		t.Errorf("argv\n got %q\nwant %q", got, want)
	}
	if got := scopeArgv("s", 350, []string{"x"}); !slices.Contains(got, "CPUQuota=350%") {
		t.Errorf("quota not used: %q", got)
	}
}

func TestGoBoundEnv(t *testing.T) {
	has := func(env []string, kv string) bool { return slices.Contains(env, kv) }
	base := []string{"PATH=/bin", "HOME=/h"}
	got := goBoundEnv(base, 200)
	if !has(got, "GOMAXPROCS=2") || !has(got, "GOFLAGS=-p=2") || !has(got, "PATH=/bin") {
		t.Errorf("plain: %q", got)
	}
	if got := goBoundEnv(base, 50); !has(got, "GOMAXPROCS=1") || !has(got, "GOFLAGS=-p=1") {
		t.Errorf("under one core still gets one: %q", got)
	}
	got = goBoundEnv(append(slices.Clone(base), "GOMAXPROCS=6", "GOFLAGS=-p=5"), 200)
	if !has(got, "GOMAXPROCS=6") || !has(got, "GOFLAGS=-p=5") || has(got, "GOMAXPROCS=2") || has(got, "GOFLAGS=-p=2") {
		t.Errorf("the caller's settings were overridden: %q", got)
	}
	got = goBoundEnv(append(slices.Clone(base), "GOFLAGS=-mod=mod -p 7"), 200)
	if !has(got, "GOFLAGS=-mod=mod -p 7") {
		t.Errorf("a -p with a separate value is the caller's: %q", got)
	}
	got = goBoundEnv(append(slices.Clone(base), "GOFLAGS=-mod=mod -race"), 300)
	if !has(got, "GOFLAGS=-mod=mod -race -p=3") {
		t.Errorf("-p is added after the caller's other flags: %q", got)
	}
	n := 0
	for _, e := range got {
		if strings.HasPrefix(e, "GOFLAGS=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("GOFLAGS appears %d times: %q", n, got)
	}
}

// fakeSystemdRun puts a systemd-run on PATH that logs its arguments to the
// returned file and then, when ok, becomes the command after "--" as the
// real one does for --scope; otherwise it fails as it does without a bus.
// The scope has a cpu.max quota.
func fakeSystemdRun(t *testing.T, ok bool) (log string) {
	t.Helper()
	if !ok {
		return fakeSystemdScope(t, false, "")
	}
	return fakeSystemdScope(t, true, "200000 100000")
}

// fakeSystemdScope is fakeSystemdRun for a scope whose cgroup has the cpu.max
// content cpuMax ("": no such file). The scope's wrapper is run as written,
// with the cgroup files it reads pointed at a fixture.
func fakeSystemdScope(t *testing.T, ok bool, cpuMax string) (log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "args")
	root := filepath.Join(dir, "cg")
	if err := os.MkdirAll(filepath.Join(root, "scope"), 0o755); err != nil { // #nosec G301 -- a test fixture
		t.Fatal(err)
	}
	proc := filepath.Join(dir, "proc")
	if err := os.WriteFile(proc, []byte("0::/scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cpuMax != "" {
		if err := os.WriteFile(filepath.Join(root, "scope", "cpu.max"), []byte(cpuMax+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\necho \"$@\" > " + log + "\necho 'Failed to connect to bus' >&2\nexit 1\n"
	if ok {
		script = "#!/bin/sh\necho \"$@\" > " + log + "\nwhile [ \"$1\" != -- ]; do shift; done; shift\n" +
			"s=$(printf %s \"$3\" | sed -e 's#/sys/fs/cgroup#" + root + "#' -e 's#/proc/self/cgroup#" + proc + "#')\n" +
			"shift 3\nexec /bin/sh -c \"$s\" \"$@\"\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(script), 0o755); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func probesAre(t *testing.T, cgroup, runner, bus bool) {
	t.Helper()
	old := slotCPUProbeFn
	slotCPUProbeFn = func() slotCPUProbes {
		return slotCPUProbes{goos: "linux", cgroup: func() bool { return cgroup }, runnerJob: func() bool { return runner }, userBus: func() bool { return bus }}
	}
	t.Cleanup(func() { slotCPUProbeFn = old })
}

func runBound(t *testing.T, ctx context.Context, dir string, env []string, argv ...string) (code int, kind, out string) {
	t.Helper()
	var buf bytes.Buffer
	code, kind, err := runSlotBound(ctx, dir, env, argv, nil, &buf, &buf)
	if err != nil {
		t.Fatal(err)
	}
	return code, kind, buf.String()
}

func TestRunSlotBoundScope(t *testing.T) {
	log := fakeSystemdRun(t, true)
	probesAre(t, false, false, true)
	t.Setenv(slotCPUEnv, "250")
	dir := t.TempDir()
	// The test may itself run under an env bound, which sets GOMAXPROCS.
	env := slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "GOMAXPROCS=") })
	env = append(env, "WAYPOINT_SLOTS_HELD=tests=x@1", "MY_VAR=kept")
	code, kind, out := runBound(t, context.Background(), dir, env, "sh", "-c", "echo $MY_VAR $WAYPOINT_SLOTS_HELD; pwd; echo $GOMAXPROCS; exit 3")
	if code != 3 || kind != slotBoundScope {
		t.Fatalf("code %d kind %q out %q", code, kind, out)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if want := "kept tests=x@1\n" + real + "\n\n"; out != want {
		t.Errorf("env, working folder and Go's settings in the scope:\n got %q\nwant %q", out, want)
	}
	b, _ := os.ReadFile(log)
	for _, w := range []string{"--user", "--scope", "--collect", "--slice=waypoint-tests.slice", "CPUQuota=250%"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("systemd-run was given %q, want %q", b, w)
		}
	}
}

func TestRunSlotBoundScopeFailureRunsTheCommandOnce(t *testing.T) {
	fakeSystemdRun(t, false)
	probesAre(t, false, false, true)
	t.Setenv(slotCPUEnv, "200")
	count := filepath.Join(t.TempDir(), "count")
	code, kind, out := runBound(t, context.Background(), "", os.Environ(), "sh", "-c", "echo ran >> "+count+"; echo $GOMAXPROCS $GOFLAGS")
	if code != 0 || kind != slotBoundEnv {
		t.Fatalf("code %d kind %q out %q", code, kind, out)
	}
	if b, _ := os.ReadFile(count); string(b) != "ran\n" {
		t.Errorf("the command ran %q, want once", b)
	}
	if !strings.Contains(out, "2 ") || !strings.Contains(out, "-p=2") {
		t.Errorf("fell back without the env bound: %q", out)
	}
}

func TestRunSlotBoundCommandFailureIsNotAScopeFailure(t *testing.T) {
	fakeSystemdRun(t, true)
	probesAre(t, false, false, true)
	t.Setenv(slotCPUEnv, "200")
	count := filepath.Join(t.TempDir(), "count")
	code, kind, _ := runBound(t, context.Background(), "", os.Environ(), "sh", "-c", "echo ran >> "+count+"; exit 1")
	if code != 1 || kind != slotBoundScope {
		t.Errorf("code %d kind %q", code, kind)
	}
	if b, _ := os.ReadFile(count); string(b) != "ran\n" {
		t.Errorf("a command that exits 1 ran %q, want once and no fallback", b)
	}
}

func TestRunSlotBoundMissingCommandIsReportedAsBefore(t *testing.T) {
	fakeSystemdRun(t, true)
	probesAre(t, false, false, true)
	t.Setenv(slotCPUEnv, "200")
	var buf bytes.Buffer
	code, _, err := runSlotBound(context.Background(), "", os.Environ(), []string{"/nonexistent/wp-no-such-command"}, nil, &buf, &buf)
	if err == nil || code != 127 {
		t.Errorf("code %d err %v, want 127 and an error", code, err)
	}
}

func TestRunSlotBoundStopReachesTheCommandInTheScope(t *testing.T) {
	fakeSystemdRun(t, true)
	probesAre(t, false, false, true)
	t.Setenv(slotCPUEnv, "200")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	code, kind, _ := runBound(t, ctx, "", os.Environ(), "sleep", "30")
	if code != 128+15 || kind != slotBoundScope || time.Since(start) > 10*time.Second {
		t.Errorf("code %d kind %q after %v, want SIGTERM's 143 soon", code, kind, time.Since(start))
	}
}

func TestRunSlotBoundScopeWithoutQuotaIsRecordedAsNone(t *testing.T) {
	for _, c := range []struct{ name, cpuMax, kind string }{
		{"quota", "150000 100000", slotBoundScope},
		{"max", "max 100000", slotBoundNone},
		{"unreadable", "", slotBoundNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeSystemdScope(t, true, c.cpuMax)
			probesAre(t, false, false, true)
			t.Setenv(slotCPUEnv, "200")
			count := filepath.Join(t.TempDir(), "count")
			code, kind, _ := runBound(t, context.Background(), "", os.Environ(), "sh", "-c", "echo ran >> "+count+"; exit 4")
			if code != 4 || kind != c.kind {
				t.Errorf("code %d kind %q, want 4 and %q", code, kind, c.kind)
			}
			if b, _ := os.ReadFile(count); string(b) != "ran\n" {
				t.Errorf("the command ran %q, want once", b)
			}
		})
	}
}

func TestRunSlotBoundCancelledBeforeTheScopeStartsRunsNothing(t *testing.T) {
	fakeSystemdRun(t, false)
	probesAre(t, false, false, true)
	t.Setenv(slotCPUEnv, "200")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	count := filepath.Join(t.TempDir(), "count")
	code, _, _ := runBound(t, ctx, "", os.Environ(), "sh", "-c", "echo ran >> "+count)
	if code != 128+15 {
		t.Errorf("code %d, want 143", code)
	}
	if _, err := os.Stat(count); err == nil {
		t.Error("the command ran after the cancel")
	}
}

func TestRunSlotBoundKinds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOMAXPROCS=") && !strings.HasPrefix(e, "GOFLAGS=") {
			env = append(env, e)
		}
	}
	for _, c := range []struct {
		name                string
		cgroup, runner, bus bool
		quota, kind, seen   string
	}{
		{"cgroup adds nothing", true, false, true, "200", slotBoundCgroup, "[][]"},
		{"runner holds Go", false, true, true, "300", slotBoundRunner, "[3][-p=3]"},
		{"no bus holds Go", false, false, false, "200", slotBoundEnv, "[2][-p=2]"},
		{"off adds nothing", false, false, true, "off", slotBoundNone, "[][]"},
	} {
		probesAre(t, c.cgroup, c.runner, c.bus)
		t.Setenv(slotCPUEnv, c.quota)
		_, kind, out := runBound(t, context.Background(), "", env, "sh", "-c", `echo "[$GOMAXPROCS][$GOFLAGS]"`)
		if kind != c.kind || strings.TrimSpace(out) != c.seen {
			t.Errorf("%s: kind %q saw %q, want %q %q", c.name, kind, strings.TrimSpace(out), c.kind, c.seen)
		}
	}
}

func TestSlotRecordsTheBound(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqRepo(t)
	sqArgs(t)
	probesAre(t, false, false, false)
	t.Setenv(slotCPUEnv, "200")
	c, _, _ := sqCLI()
	var out, errb bytes.Buffer
	if code, err := c.runInSlotThen("tests", 2, []string{"sh", "-c", "sleep 0.5"}, &out, &errb, nil, nil); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	dir, _ := slotDir()
	recs := readWaitRecs(dir)
	if len(recs) != 1 || recs[0].Bound != slotBoundEnv {
		t.Errorf("records %+v, want one with bound %q", recs, slotBoundEnv)
	}
}

func TestSlotLsShowsTheBoundOfARunningHoldAndUnboundRuns(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqRepo(t)
	sqArgs(t)
	probesAre(t, false, false, false)
	t.Setenv(slotCPUEnv, "200")
	c, _, _ := sqCLI()
	started := make(chan struct{})
	stop := filepath.Join(t.TempDir(), "stop")
	done := make(chan struct{})
	go func() {
		defer close(done)
		var out, errb bytes.Buffer
		_, _ = c.runInSlotThen("tests", 2, []string{"sh", "-c", "while [ ! -e " + stop + " ]; do sleep 0.05; done"}, &out, &errb, func() { close(started) }, nil)
	}()
	<-started
	ctxLs, ls, _ := sqCLI()
	var got string
	for i := 0; i < 50 && !strings.Contains(got, "[cpu bound: "); i++ {
		ls.Reset()
		if err := cmdSlotLs(ctxLs, nil); err != nil {
			t.Fatal(err)
		}
		got = ls.String()
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(got, "[cpu bound: env]") {
		t.Errorf("a running hold shows its bound:\n%s", got)
	}
	if err := os.WriteFile(stop, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done

	dir, _ := slotDir()
	now := time.Now().Unix()
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/r/.git", Cmd: "a", Class: slotClassShort, At: now, Bound: slotBoundNone})
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/r/.git", Cmd: "b", Class: slotClassShort, At: now, Bound: slotBoundScope})
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/r/.git", Cmd: "c", Class: slotClassShort, At: now, GaveUp: true})
	ls.Reset()
	if err := cmdSlotLs(ctxLs, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ls.String(), "runs with no CPU bound (WAYPOINT_SLOT_CPU=off): 1 of 3") {
		t.Errorf("summary:\n%s", ls.String())
	}
}
