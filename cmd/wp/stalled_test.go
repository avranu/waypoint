package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stalled background jobs (WP-605).

const stallSess = "931f6a40-c5ae-4963-b39e-deb3754a9411"

var stallOut = "/tmp/claude-1000/-projects-x/" + stallSess + "/tasks/b1.output"

// The 11-05 job: it waits for a lint that had ended, and pgrep finds the
// job's own shell, whose command line holds the pattern.
const pgrepLoop = `while pgrep -f "golangci-lint run" >/dev/null; do sleep 5; done; git push`

type fakeFI struct {
	size int64
	mod  time.Time
}

func (f fakeFI) Name() string       { return "b1.output" }
func (f fakeFI) Size() int64        { return f.size }
func (f fakeFI) Mode() os.FileMode  { return 0o600 }
func (f fakeFI) ModTime() time.Time { return f.mod }
func (f fakeFI) IsDir() bool        { return false }
func (f fakeFI) Sys() any           { return nil }

// stallMachine is a fake machine: one Claude Code background job, looked at
// each minute.
type stallMachine struct {
	boot, start time.Time
	cmd         string
	out         fakeFI
	// minute is called at each look to say what runs under the job then.
	minute func(i int, now time.Time, root *procEntry) []*procEntry
	others []*procEntry
}

func ticksAt(boot, at time.Time) uint64 { return uint64(at.Sub(boot) / (time.Second / clockTicks)) }

// claudeShell is the job's shell as Claude Code starts it.
func claudeShell(cmd string) []string {
	q := strings.ReplaceAll(cmd, "'", `'"'"'`)
	return []string{"/bin/bash", "-c", "source /home/u/.claude/shell-snapshots/snapshot-bash-1.sh 2>/dev/null || true && export WAYPOINT_SESSION=x\n: && { \\builtin unalias -- 'which'; } >/dev/null 2>&1 || true && eval '" + q + "' < /dev/null && pwd -P >| /tmp/claude-4aa4-cwd"}
}

func (m *stallMachine) table(i int, now time.Time) *procTable {
	root := &procEntry{pid: 100, ppid: 1, pgrp: 100, comm: "bash", args: claudeShell(m.cmd), start: ticksAt(m.boot, m.start), outs: []string{stallOut}}
	t := &procTable{at: now, boot: m.boot, procs: map[int]*procEntry{}, kids: map[int][]int{}}
	add := func(p *procEntry) {
		t.procs[p.pid] = p
		t.kids[p.ppid] = append(t.kids[p.ppid], p.pid)
	}
	add(root)
	if m.minute != nil {
		for _, p := range m.minute(i, now, root) {
			add(p)
		}
	}
	for _, p := range m.others {
		add(p)
	}
	return t
}

// run looks each minute from the job's start for n minutes and returns the
// reports, by minute.
func (m *stallMachine) run(n int) map[int]stalledJob {
	w := newStallWatcher()
	got := map[int]stalledJob{}
	for i := 0; i <= n; i++ {
		now := m.start.Add(time.Duration(i) * time.Minute)
		for _, j := range w.look(m.table(i, now), func(string) (os.FileInfo, error) { return m.out, nil }) {
			got[i] = j
		}
	}
	return got
}

func newStallMachine(cmd string) *stallMachine {
	boot := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	start := boot.Add(8 * time.Hour)
	return &stallMachine{boot: boot, start: start, cmd: cmd, out: fakeFI{mod: start}}
}

// pollingChildren: a new sleep each look, the shell's reaped pgreps adding
// a little CPU.
func pollingChildren(m *stallMachine) func(int, time.Time, *procEntry) []*procEntry {
	return func(i int, now time.Time, root *procEntry) []*procEntry {
		root.cpu = uint64(i) * 12
		return []*procEntry{{pid: 1000 + i, ppid: 100, pgrp: 100, comm: "sleep", args: []string{"sleep", "5"}, start: ticksAt(m.boot, now.Add(-2*time.Second))}}
	}
}

func TestStalledPgrepLoop(t *testing.T) {
	m := newStallMachine(pgrepLoop)
	m.minute = pollingChildren(m)
	got := m.run(45)
	if len(got) != 1 {
		t.Fatalf("reports %v, want one", got)
	}
	j, ok := got[30]
	if !ok {
		t.Fatalf("reported at %v, want minute 30 (quiet since its start, reported once 30 minutes old)", got)
	}
	if j.Session != stallSess || j.Task != "b1" || j.PID != 100 || !j.Group || j.Quiet != 30 || j.Command != pgrepLoop {
		t.Errorf("job %+v", j)
	}
	ev := strings.Join(j.Evidence, "\n")
	for _, want := range []string{
		"Running 30 min; no output at all.",
		"nothing under it but shells and sleep",
		"A polling loop: sleep keeps coming back with a new pid.",
		"`pgrep -f 'golangci-lint run'` matches the job's own `bash -c` command line",
		"Nothing outside the job matches pgrep's pattern 'golangci-lint run'.",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("evidence lacks %q:\n%s", want, ev)
		}
	}
	if len(j.Running) != 2 || j.Running[0] != "pid 100: bash (the job's shell)" || j.Running[1] != "pid 1030: sleep 5" {
		t.Errorf("running %q", j.Running)
	}
}

// Something outside the job that matches is named: the loop may wait on it.
func TestStalledNamesWhatMatchesOutside(t *testing.T) {
	m := newStallMachine(pgrepLoop)
	m.minute = pollingChildren(m)
	m.others = []*procEntry{
		{pid: 555, ppid: 1, pgrp: 555, comm: "golangci-lint", args: []string{"golangci-lint", "run", "./..."}, mine: true},
		// Another user's isn't named: its command line isn't this user's to pass on.
		{pid: 556, ppid: 1, pgrp: 556, comm: "golangci-lint", args: []string{"golangci-lint", "run", "--secret"}},
	}
	j := m.run(30)[30]
	if ev := strings.Join(j.Evidence, "\n"); !strings.Contains(ev, "Outside the job, 'golangci-lint run' matches pid 555 golangci-lint run ./....") {
		t.Errorf("evidence:\n%s", ev)
	}
}

// A waiting loop in a script (`./wait.sh`): the script's shell shows the
// script's name, and its reads of each pgrep's output aren't work.
func TestStalledScriptLoop(t *testing.T) {
	m := newStallMachine("./wait.sh")
	poll := pollingChildren(m)
	m.minute = func(i int, now time.Time, root *procEntry) []*procEntry {
		script := &procEntry{pid: 150, ppid: 100, pgrp: 100, comm: "wait.sh", args: []string{"/bin/bash", "./wait.sh"}, start: ticksAt(m.boot, m.start),
			io: uint64(i) * 1 << 20, ioKnown: true}
		kids := poll(i, now, root)
		for _, k := range kids {
			k.ppid = 150
		}
		return append([]*procEntry{script}, kids...)
	}
	if got := m.run(30); len(got) != 1 {
		t.Errorf("reports %v, want one", got)
	}
}

// A process a finished command left running (`npm run dev &`) holds the
// output file, but isn't a job Claude Code runs: not reported.
func TestStalledSkipsLeftBehind(t *testing.T) {
	m := newStallMachine("unused")
	w := newStallWatcher()
	for i := 0; i <= 60; i++ {
		now := m.start.Add(time.Duration(i) * time.Minute)
		tb := &procTable{at: now, boot: m.boot, procs: map[int]*procEntry{
			300: {pid: 300, ppid: 1, pgrp: 300, comm: "node", args: []string{"node", "vite"}, start: ticksAt(m.boot, m.start), outs: []string{stallOut}},
		}, kids: map[int][]int{1: {300}}}
		if got := w.look(tb, func(string) (os.FileInfo, error) { return m.out, nil }); len(got) != 0 {
			t.Fatalf("minute %d: reported %+v", i, got)
		}
	}
}

func TestStalledNotReported(t *testing.T) {
	for name, c := range map[string]struct {
		cmd    string
		minute func(m *stallMachine) func(int, time.Time, *procEntry) []*procEntry
		out    func(m *stallMachine)
	}{
		"a lasting process working": {cmd: "go test ./...", minute: func(m *stallMachine) func(int, time.Time, *procEntry) []*procEntry {
			return func(i int, _ time.Time, _ *procEntry) []*procEntry {
				return []*procEntry{{pid: 200, ppid: 100, comm: "go", args: []string{"go", "test", "./..."}, start: ticksAt(m.boot, m.start), own: uint64(i) * 300, cpu: uint64(i) * 300}}
			}
		}},
		"a lasting process reading": {cmd: "rsync -a big/ host:big/", minute: func(m *stallMachine) func(int, time.Time, *procEntry) []*procEntry {
			return func(i int, _ time.Time, _ *procEntry) []*procEntry {
				return []*procEntry{{pid: 200, ppid: 100, comm: "rsync", start: ticksAt(m.boot, m.start), io: uint64(i) * 4096, ioKnown: true}}
			}
		}},
		"short-lived work": {cmd: "for f in *.png; do convert $f $f.webp; done", minute: func(m *stallMachine) func(int, time.Time, *procEntry) []*procEntry {
			return func(i int, now time.Time, root *procEntry) []*procEntry {
				root.cpu = uint64(i) * 1500 // 15 s a minute of reaped converts
				return []*procEntry{{pid: 1000 + i, ppid: 100, comm: "convert", start: ticksAt(m.boot, now.Add(-time.Second))}}
			}
		}},
		"writing output": {cmd: pgrepLoop, minute: pollingChildren, out: func(m *stallMachine) {
			m.out.size = 1 // grows below
		}},
		"waiting on a pull request": {cmd: "wp pr wait 412", minute: func(m *stallMachine) func(int, time.Time, *procEntry) []*procEntry {
			return func(int, time.Time, *procEntry) []*procEntry {
				return []*procEntry{{pid: 200, ppid: 100, comm: "wp", args: []string{"/home/u/go/bin/wp", "pr", "wait", "412"}, start: ticksAt(m.boot, m.start)}}
			}
		}},
		"under a 30m timeout":      {cmd: "timeout 30m sh -c 'until test -f done; do sleep 5; done'", minute: pollingChildren},
		"under a timeout after cd": {cmd: "cd /x && timeout --signal=KILL 45m make check", minute: pollingChildren},
	} {
		m := newStallMachine(c.cmd)
		m.minute = c.minute(m)
		if c.out != nil {
			c.out(m)
			inner := m.minute
			m.minute = func(i int, now time.Time, root *procEntry) []*procEntry {
				m.out = fakeFI{size: int64(i + 1), mod: now}
				return inner(i, now, root)
			}
		}
		if got := m.run(60); len(got) != 0 {
			t.Errorf("%s: reported %+v", name, got)
		}
	}
}

// A timeout of an hour or more, or one that bounds only part of the job,
// leaves it watched.
func TestStalledUnderLongOrPartTimeout(t *testing.T) {
	for _, cmd := range []string{
		"timeout 2h sh -c 'until test -f done; do sleep 5; done'",
		"timeout 60 make lint; until test -f done; do sleep 5; done",
	} {
		m := newStallMachine(cmd)
		m.minute = pollingChildren(m)
		if got := m.run(30); len(got) != 1 {
			t.Errorf("%s: reports %v, want one", cmd, got)
		}
	}
}

// Progress resets the quiet time; a job that ends is forgotten.
func TestStalledQuietCountsFromLastProgress(t *testing.T) {
	m := newStallMachine(pgrepLoop)
	poll := pollingChildren(m)
	m.minute = func(i int, now time.Time, root *procEntry) []*procEntry {
		if i == 25 {
			m.out = fakeFI{size: 10, mod: now}
		}
		return poll(i, now, root)
	}
	got := m.run(60)
	if _, ok := got[40]; !ok || len(got) != 1 {
		t.Errorf("reports at %v, want minute 40 (15 after its last output)", got)
	}
}

// A look that panics after marking one job (b1) and before the next (b2)
// undoes the mark: both are reported at the next look.
func TestStalledLookPanicUnmarks(t *testing.T) {
	m := newStallMachine(pgrepLoop)
	m.minute = pollingChildren(m)
	out2 := strings.Replace(stallOut, "b1.output", "b2.output", 1)
	m.others = []*procEntry{{pid: 200, ppid: 1, pgrp: 200, comm: "bash", args: claudeShell(pgrepLoop), start: ticksAt(m.boot, m.start), outs: []string{out2}}}
	w := newStallWatcher()
	got := map[int][]string{}
	for i := 0; i <= 31; i++ {
		now := m.start.Add(time.Duration(i) * time.Minute)
		stat := func(name string) (os.FileInfo, error) {
			if i == 30 && name == out2 {
				panic("odd /proc")
			}
			return m.out, nil
		}
		func() {
			defer func() { _ = recover() }()
			for _, j := range w.look(m.table(i, now), stat) {
				got[i] = append(got[i], j.Task)
			}
		}()
	}
	if len(got) != 1 || strings.Join(got[31], ",") != "b1,b2" {
		t.Errorf("reports %v, want b1 and b2 at minute 31", got)
	}
}

func TestTimeoutBound(t *testing.T) {
	for cmd, want := range map[string]time.Duration{
		"timeout 30m make check":                          30 * time.Minute,
		"timeout 90 make check":                           90 * time.Second,
		"timeout 1.5h ./deploy":                           90 * time.Minute,
		"timeout 2d ./soak":                               48 * time.Hour,
		"cd /x && timeout -k 5 --signal KILL 20m go test": 20 * time.Minute,
		"timeout 60 lint; git push":                       0,
		"timeout 60 lint | tee log":                       0,
		"make check":                                      0,
		"timeout 30m":                                     0,
		"timeout abc make":                                0,
		"> /tmp/out":                                      0,
		"cd /x && > f":                                    0,
		"timeout -s":                                      0, // a flag's value missing: no panic
		"timeout -k":                                      0,
		"timeout --signal":                                0,
	} {
		if got := timeoutBound(cmd); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
}

func TestJobCommand(t *testing.T) {
	root := &procEntry{args: claudeShell(`while pgrep -f "x y" >/dev/null; do sleep 5; done; git push`)}
	if got, ok := jobCommand(root); !ok || got != `while pgrep -f "x y" >/dev/null; do sleep 5; done; git push` {
		t.Errorf("got %q, %v", got, ok)
	}
	if got, ok := jobCommand(&procEntry{args: []string{"python3", "serve.py"}}); ok || got != "python3 serve.py" {
		t.Errorf("not a shell: %q, %v", got, ok)
	}
	if _, ok := jobCommand(&procEntry{args: []string{"/bin/bash", "-c", "make check"}}); ok {
		t.Error("a shell Claude Code didn't start: a job")
	}
}

func TestParseProcStat(t *testing.T) {
	// A command name with spaces and parentheses.
	p := parseProcStat("4242 (a) b (c)) S 100 4242 4242 0 -1 4194304 1 0 0 0 7 3 11 13 20 0 1 0 98765 1000 100 18446744073709551615")
	if p == nil || p.pid != 4242 || p.comm != "a) b (c)" || p.ppid != 100 || p.pgrp != 4242 || p.own != 10 || p.cpu != 34 || p.start != 98765 {
		t.Fatalf("%+v", p)
	}
	if parseProcStat("4242 (x) S 1") != nil || parseProcStat("garbage") != nil {
		t.Error("short or broken stat read")
	}
}

func TestReadProcTable(t *testing.T) {
	root := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "stat"), "cpu  1 2 3\nbtime 1793854800\nprocesses 9\n")
	stat := func(pid, ppid int, comm string) string {
		return fmt.Sprintf("%d (%s) S %d %d %d 0 -1 0 0 0 0 0 5 5 1 1 20 0 1 0 360000 0 0", pid, comm, ppid, pid, pid)
	}
	write(filepath.Join(root, "100", "stat"), stat(100, 1, "bash"))
	write(filepath.Join(root, "100", "cmdline"), strings.Join(claudeShell("make check"), "\x00")+"\x00")
	write(filepath.Join(root, "100", "io"), "rchar: 100\nwchar: 20\nsyscr: 5\n")
	if err := os.MkdirAll(filepath.Join(root, "100", "fd"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, fd := range []string{"1", "2"} {
		if err := os.Symlink(stallOut, filepath.Join(root, "100", "fd", fd)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/dev/null", filepath.Join(root, "100", "fd", "0")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "101", "stat"), stat(101, 100, "make"))
	write(filepath.Join(root, "self", "stat"), "not a pid")
	tb, err := readProcTable(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := tb.procs[100]
	if len(tb.procs) != 2 || p == nil || p.io != 120 || !p.ioKnown || len(p.outs) != 1 || p.outs[0] != stallOut || len(p.args) != 3 || p.cpu != 12 {
		t.Fatalf("%d procs, %+v", len(tb.procs), p)
	}
	if got := tb.startedAt(p); !got.Equal(time.Unix(1793854800+3600, 0)) {
		t.Errorf("started %v", got)
	}
	if kids := tb.kids[100]; len(kids) != 1 || kids[0] != 101 {
		t.Errorf("kids %v", kids)
	}
	jobs := findJobs(tb)
	if len(jobs) != 1 || jobs[0].session != stallSess || jobs[0].task != "b1" || jobs[0].root.pid != 100 {
		t.Errorf("jobs %+v", jobs)
	}
}

// A child holding the output too isn't a second job.
func TestFindJobsTakesTheTop(t *testing.T) {
	tb := &procTable{procs: map[int]*procEntry{
		100: {pid: 100, ppid: 1, outs: []string{stallOut}},
		101: {pid: 101, ppid: 100, outs: []string{stallOut}},
	}}
	if jobs := findJobs(tb); len(jobs) != 1 || jobs[0].root.pid != 100 {
		t.Errorf("jobs %+v", jobs)
	}
}

func TestPgrepSelfNudge(t *testing.T) {
	for cmd, want := range map[string]string{
		pgrepLoop:                                                "Write the pattern so it can't match itself, e.g. `pgrep -f '[g]olangci-lint run'`.",
		`pkill -f "vite dev"`:                                    "kills that shell",
		`kill $(pgrep -f "node server")`:                         "`pgrep -f 'node server'`",
		`pgrep -u u -fl node`:                                    "`pgrep -f 'node'`",
		`pgrep --full -- "make check" && echo running`:           "`pgrep -f 'make check'`",
		`while pgrep -f "[g]olangci-lint run"; do sleep 5; done`: "",
		`pgrep golangci-lint`:                                    "", // the name alone: no command line
		`pgrep -f "^node server"`:                                "", // anchored: the shell's line starts with bash
		`pgrep -f 'it'"'"'s here'`:                               "",
		`pgrep -xf 'node server.js'`:                             "", // exact: can't match the shell's line
		`pgrep -P 1 -f`:                                          "",
		`echo pgrep -f x`:                                        "",
		// Flag clusters: a flag that takes a value ends one.
		`pgrep -fu nobody "node server" | head`: "`pgrep -f 'node server'`",
		`pgrep -funobody "node server"`:         "`pgrep -f 'node server'`",
		`pgrep -f -O 60 "node server"`:          "`pgrep -f 'node server'`",
		`pkill -9 -f "node server"`:             "`pkill -f 'node server'`",
		`pkill -TERM -f "node server"`:          "`pkill -f 'node server'`",
		// Each alternative is bracketed.
		`pkill -f "vite|webpack"`:      "e.g. `pkill -f '[v]ite|[w]ebpack'`",
		`pkill -f "[v]ite|webpack"`:    "e.g. `pkill -f '[v]ite|[w]ebpack'`",
		`pgrep -f "go (test|vet) ./x"`: "", // its own text has "go (test", not "go test"
		`pgrep -f "[v]ite|[w]ebpack"`:  "",
		`pgrep -f -- "-+|webpack"`:     "put one letter of each alternative in brackets",
	} {
		got := pgrepSelfNudge(cmd)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", cmd, got, want)
		}
	}
}

func TestPgrepSelfHook(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	in, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": t.TempDir(),
		"tool_input": map[string]any{"command": pgrepLoop, "run_in_background": true}})
	var out bytes.Buffer
	hookClaudeGuard(&cli{}, bytes.NewReader(in), &out)
	if s := out.String(); !strings.Contains(s, `"permissionDecision":"deny"`) || !strings.Contains(s, "[g]olangci-lint run") {
		t.Errorf("output %q", s)
	}
}

// With a Waypoint that applies its stall policy (WP-1045): every job quiet
// 5 minutes, short timeouts included, again when Waypoint asks, and how its
// spells end.
func TestStalledPolicy2(t *testing.T) {
	// failed: the report before the job moved again failed (to be
	// retried); Waypoint heard of the spell earlier, so hears it end.
	for _, failed := range []bool{false, true} {
		stalledPolicy2(t, failed)
	}
}

func stalledPolicy2(t *testing.T, failed bool) {
	m := newStallMachine("timeout 30m sh -c 'until test -f done; do sleep 5; done'")
	poll := pollingChildren(m)
	m.minute = func(i int, now time.Time, root *procEntry) []*procEntry {
		if i == 20 {
			m.out = fakeFI{size: 10, mod: now}
		}
		return poll(i, now, root)
	}
	w := newStallWatcher()
	w.policy = 2
	var at []int
	var settled []string
	for i := 0; i <= 40; i++ {
		now := m.start.Add(time.Duration(i) * time.Minute)
		tab := m.table(i, now)
		if i == 40 {
			tab = &procTable{at: now, boot: m.boot, procs: map[int]*procEntry{}, kids: map[int][]int{}} // it ended
		}
		if failed && i == 20 {
			for _, sw := range w.jobs {
				w.unreport(stalledJob{Session: sw.session, Task: sw.task})
			}
		}
		jobs := w.look(tab, func(string) (os.FileInfo, error) { return m.out, nil })
		for _, j := range jobs {
			at = append(at, i)
			if j.Timeout != 30 {
				t.Errorf("minute %d: timeout %d", i, j.Timeout)
			}
			var ans stalledAnswer
			_ = json.Unmarshal(fmt.Appendf(nil, `{"jobs":[{"task":%q,"session":%q,"outcome":"quiet","again_in":600}],"policy":2}`, j.Task, j.Session), &ans)
			w.answered(ans, now, func(string, ...any) {})
		}
		for _, s := range w.settled {
			settled = append(settled, fmt.Sprintf("%d:%s", i, s.How))
		}
		w.settled = nil
	}
	if fmt.Sprint(at) != "[5 15 25 35]" {
		t.Errorf("failed %v: reports at %v, want 5, 15 (asked again in 10), 25 (5 after its output at 20), 35", failed, at)
	}
	if fmt.Sprint(settled) != "[20:progress 40:ended]" {
		t.Errorf("failed %v: settled %v", failed, settled)
	}
	// An older Waypoint's answer turns the runner back to its old floors.
	w.answered(stalledAnswer{}, m.start, func(string, ...any) {})
	if w.policy != 0 {
		t.Errorf("policy %d", w.policy)
	}
}
