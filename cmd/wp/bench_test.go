package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBenchTargetDecidesTheExitCode(t *testing.T) {
	slotEnv(t)
	tempRepo(t)
	var out, errb bytes.Buffer
	if code := run([]string{"bench", "-n", "3", "--target", "10s", "--", "sh", "-c", "true"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, want 0\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "target wall 10.0s: met") {
		t.Errorf("no met line:\n%s", out.String())
	}
	out.Reset()
	if code := run([]string{"bench", "-n", "2", "--target", "1ms", "--", "sh", "-c", "sleep 0.05"}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "not met") {
		t.Errorf("no not-met line:\n%s", out.String())
	}
}

func TestBenchFailingCommandShowsItsTailAndCode(t *testing.T) {
	slotEnv(t)
	tempRepo(t)
	var out, errb bytes.Buffer
	// go test fails with 1, which must not read as "target not met".
	if code := run([]string{"bench", "--target", "1h", "--", "sh", "-c", "echo boom; exit 1"}, &out, &errb); code != benchFailedCode {
		t.Fatalf("exit %d, want %d\n%s%s", code, benchFailedCode, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "boom") || !strings.Contains(errb.String(), "exit 1") {
		t.Errorf("tail or code missing:\n%s", errb.String())
	}
	errb.Reset()
	if code := run([]string{"bench", "--", "no-such-command-wp-bench"}, &out, &errb); code != benchFailedCode {
		t.Fatalf("missing command: exit %d, want %d\n%s", code, benchFailedCode, errb.String())
	}
}

func TestBenchBaseRunsTheOtherTreeAndKeepsResults(t *testing.T) {
	slotEnv(t)
	dir := tempRepo(t)
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, "sub/d.txt", "0.08\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "slow")
	write(t, "sub/d.txt", "0.001\n") // this tree, uncommitted: faster
	t.Chdir(filepath.Join(dir, "sub"))

	var out, errb bytes.Buffer
	args := []string{"bench", "--json", "-n", "4", "--base", "HEAD", "--setup", "touch built", "--", "sh", "-c", "test -f built && sleep $(cat d.txt)"}
	if code := run(args, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	var res benchResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.Base == nil || res.Change == nil {
		t.Fatalf("no base side: %s", out.String())
	}
	if res.Head.Wall.Median >= res.Base.Wall.Median || res.Change.Wall >= 0 {
		t.Errorf("this tree (sleep 1ms) should beat the base (sleep 80ms): %+v vs %+v", res.Head.Wall, res.Base.Wall)
	}
	if res.Change.WallP == nil || *res.Change.WallP > 0.05 {
		t.Errorf("wall p = %v, want < 0.05 for 4 runs a side that never overlap", res.Change.WallP)
	}
	if wt := gitDo(t, "worktree", "list"); strings.Count(wt, "\n") > 1 {
		t.Errorf("temporary worktree left behind:\n%s", wt)
	}

	recs := readBenchRecords(benchPath())
	if len(recs) != 1 || recs[0].BaseRef != "HEAD" || recs[0].Tree == "" || recs[0].Tree == recs[0].BaseTree {
		t.Fatalf("kept %+v", recs)
	}
	out.Reset()
	if code := run([]string{"bench", "-n", "1", "--setup", "touch built", "--", "sh", "-c", "test -f built && sleep $(cat d.txt)"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "last kept for this command") {
		t.Errorf("no history line:\n%s", out.String())
	}
}

// With a base, each round starts with the tree the last one ended with,
// warm-up included: B H | H B | B H, so neither tree always runs first.
func TestBenchTreesTakeTurnsEveryRound(t *testing.T) {
	slotEnv(t)
	tempRepo(t)
	write(t, "side", "base\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "one")
	write(t, "side", "head\n")
	log := filepath.Join(t.TempDir(), "order")
	var out, errb bytes.Buffer
	if code := run([]string{"bench", "-n", "4", "--base", "HEAD", "--", "sh", "-c", "cat side >> " + log}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(b))
	want := []string{"base", "head", "head", "base", "base", "head", "head", "base", "base", "head"}
	if !slices.Equal(got, want) {
		t.Errorf("order %q, want %q", got, want)
	}
}

func TestBenchUsage(t *testing.T) {
	slotEnv(t)
	for _, args := range [][]string{
		{"bench"},
		{"bench", "true"},
		{"bench", "-n", "0", "--", "true"},
		{"bench", "--by", "rss", "--", "true"},
		{"bench", "--target", "0s", "--", "true"},
		{"bench", "--target", "-1s", "--", "true"},
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2 (usage)\n%s", args, code, errb.String())
		}
	}
}

func TestMannWhitneyP(t *testing.T) {
	p := mannWhitneyP([]float64{1, 2, 3, 4, 5}, []float64{6, 7, 8, 9, 10})
	if p == nil || math.Abs(*p-0.0122) > 0.001 {
		t.Errorf("separated samples: p = %v, want about 0.0122", p)
	}
	p = mannWhitneyP([]float64{1, 3, 5, 7}, []float64{2, 4, 6, 8})
	if p == nil || *p < 0.5 {
		t.Errorf("interleaved samples: p = %v, want large", p)
	}
	if p := mannWhitneyP([]float64{2, 2}, []float64{2, 2}); p != nil {
		t.Errorf("all equal: p = %v, want none", *p)
	}
	if p := mannWhitneyP(nil, []float64{1}); p != nil {
		t.Errorf("empty side: p = %v, want none", *p)
	}
}

func TestBenchStatsAndHistory(t *testing.T) {
	s := benchStats([]benchSample{{Wall: 4}, {Wall: 1}, {Wall: 3}, {Wall: 2}}, func(x benchSample) float64 { return x.Wall })
	if s.Median != 2.5 || s.Low != 1.75 || s.High != 3.25 {
		t.Errorf("stats %+v", s)
	}
	recs := []benchRecord{
		{Command: "a", Wall: 3, CPU: 1, Tree: "t1"},
		{Command: "b", Wall: 1, CPU: 1, Tree: "t2"},
		{Command: "a", Wall: 2, CPU: 5, Tree: "t3"},
		{Command: "a", Wall: 4, CPU: 2, Tree: "t4"},
		{Command: "a", Wall: 0.1, CPU: 0.1, Tree: "t5", Dir: "other"},
		{Command: "a", Wall: 0.1, CPU: 0.1, Tree: "t6", Setup: "make"},
	}
	a := benchRecord{Command: "a"}
	prev, best := benchHistory(recs, a, "wall")
	if prev.Tree != "t4" || best.Tree != "t3" {
		t.Errorf("wall: prev %s best %s (another folder or --setup is another workload)", prev.Tree, best.Tree)
	}
	if _, best := benchHistory(recs, a, "cpu"); best.Tree != "t1" {
		t.Errorf("cpu best %s", best.Tree)
	}
	if prev, _ := benchHistory(recs, benchRecord{Command: "a", Dir: "other"}, "wall"); prev == nil || prev.Tree != "t5" {
		t.Errorf("same folder not matched: %+v", prev)
	}
	if prev, best := benchHistory(recs, benchRecord{Command: "c"}, "wall"); prev != nil || best != nil {
		t.Errorf("unknown command has history")
	}
}

func TestShellJoin(t *testing.T) {
	if got := shellJoin([]string{"echo", "a b", "", "it's", "-run=TestX", "./x"}); got != `echo 'a b' '' 'it'\''s' -run=TestX ./x` {
		t.Errorf("got %s", got)
	}
}

func TestBenchStopKillsTheCommandsChildren(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		// The grandchild sleep holds the output pipe; a stop must not wait for it.
		_, _, err := benchExec(ctx, t.TempDir(), os.Environ(), []string{"sh", "-c", "sleep 30; echo done"}, nil)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("a stopped run reported no error")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("stop took %s", d)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("benchExec still waiting on the grandchild after a stop")
	}
}

func TestBenchSweepsKilledRunsWorktrees(t *testing.T) {
	tempRepo(t)
	write(t, "a.txt", "x\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "one")
	// A pid that can't be running: a wp bench killed before its cleanup.
	dead := filepath.Join(os.TempDir(), baseTmpPrefix+"2147483646-test")
	gitDo(t, "worktree", "add", "--detach", "--quiet", dead, "HEAD")
	t.Cleanup(func() { _ = os.RemoveAll(dead) })
	sweepBaseWorktrees()
	if wt := gitDo(t, "worktree", "list"); strings.Contains(wt, dead) {
		t.Errorf("dead run's worktree kept:\n%s", wt)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("dead run's folder kept: %v", err)
	}
}

func TestBenchChangeTextSaysWhenTooFewRuns(t *testing.T) {
	p := 0.0809
	if got := changeText(-90, &p, 3); !strings.Contains(got, "90% faster (p=0.08; 3 runs a side can't tell a change from noise, -n 4 or more can)") {
		t.Errorf("3 runs: %s", got)
	}
	p = 0.3
	if got := changeText(5, &p, 5); got != "5% slower (p=0.30, could be noise)" {
		t.Errorf("5 runs: %s", got)
	}
}

func TestBenchBadBaseIsAUsageError(t *testing.T) {
	slotEnv(t)
	tempRepo(t)
	write(t, "a.txt", "x\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "one")
	for _, base := range []string{"no-such-ref", "-x"} {
		var out, errb bytes.Buffer
		if code := run([]string{"bench", "--base=" + base, "--", "true"}, &out, &errb); code != 2 {
			t.Errorf("--base %s: exit %d, want 2\n%s", base, code, errb.String())
		}
	}
}

func TestBenchSweepFollowsASymlinkedTMPDIR(t *testing.T) {
	tempRepo(t)
	write(t, "a.txt", "x\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "one")
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "tmp")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	t.Setenv("TMPDIR", link) // as macOS's /var/folders → /private/var/folders
	dead := filepath.Join(link, baseTmpPrefix+"2147483646-test")
	gitDo(t, "worktree", "add", "--detach", "--quiet", dead, "HEAD")
	sweepBaseWorktrees()
	if wt := gitDo(t, "worktree", "list"); strings.Contains(wt, baseTmpPrefix) {
		t.Errorf("dead run's worktree kept:\n%s", wt)
	}
}

// runningProcess is processAlive, except that a killed process nobody has
// reaped yet (a zombie, as under a container's PID 1 that never waits) is dead.
func runningProcess(pid int) bool {
	if !processAlive(pid) {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	if i := strings.LastIndex(string(b), ") "); i >= 0 && i+2 < len(b) {
		return b[i+2] != 'Z'
	}
	return true
}

func TestBenchKillsWhatTheCommandLeftRunning(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	code, _, err := benchExec(context.Background(), dir, os.Environ(), []string{"sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $! > " + pidFile}, nil)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	b, _ := os.ReadFile(pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); runningProcess(pid); {
		if time.Now().After(end) {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
			t.Fatal("the background sleep outlived the run")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBenchFlagsATreeThatMovedDuringTheRuns(t *testing.T) {
	slotEnv(t)
	tempRepo(t)
	write(t, "a.txt", "x\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "one")
	var out, errb bytes.Buffer
	// --setup's own output isn't an edit; a file written during the timed runs is.
	if code := run([]string{"bench", "-n", "2", "--setup", "touch built", "--", "sh", "-c", "test -f built && date +%s%N > stamp"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "changed during the timed runs") {
		t.Errorf("no moved notice:\n%s", out.String())
	}
	recs := readBenchRecords(benchPath())
	if len(recs) != 1 || !recs[0].Moved || len(benchToSend(recs, recs[0].Tree)) != 0 {
		t.Errorf("kept %+v", recs)
	}
	out.Reset()
	if code := run([]string{"bench", "-n", "2", "--setup", "touch built2", "--", "true"}, &out, &errb); code != 0 || strings.Contains(out.String(), "changed during") {
		t.Errorf("setup output counted as an edit (exit %d):\n%s", code, out.String())
	}
}

// git failing to read the tree, before the timed runs or after them, says
// nothing about whether files moved: the result isn't kept, rather than
// kept as moved or against a tree that wasn't timed.
func TestBenchKeepsNothingWhenATreeReadFails(t *testing.T) {
	t.Cleanup(func() { benchTree = workingTree })
	for _, fail := range []int{1, 2} {
		slotEnv(t)
		tempRepo(t)
		write(t, "a.txt", "x\n")
		gitDo(t, "add", "-A")
		gitDo(t, "commit", "-q", "-m", "one")
		reads := 0
		benchTree = func() string {
			if reads++; reads == fail {
				return ""
			}
			return workingTree()
		}
		var out, errb bytes.Buffer
		code := run([]string{"bench", "-n", "2", "--", "true"}, &out, &errb)
		if code != 0 || strings.Contains(out.String(), "changed during") {
			t.Errorf("read %d failing: exit %d\n%s%s", fail, code, out.String(), errb.String())
		}
		if recs := readBenchRecords(benchPath()); len(recs) != 0 {
			t.Errorf("read %d failing: kept %+v", fail, recs)
		}
	}
}
