package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sqRepo makes a git repository and enters it.
func sqRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if out, err := exec.Command("git", "-C", d, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(d)
	return d
}

func sqArgs(t *testing.T) {
	t.Helper()
	old := slotOSArgs
	slotOSArgs = []string{"wp", "testsum", "--", "go", "test", "./x"}
	t.Cleanup(func() { slotOSArgs = old })
}

const sqCmd = "go test ./x"

func sqCLI() (*cli, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &cli{out: &out, err: &errb}, &out, &errb
}

// sqSeed writes n history records of secs seconds for the repo entered.
func sqSeed(t *testing.T, name, cmd string, secs int64, n int) {
	t.Helper()
	dir, _ := slotDir()
	_ = os.MkdirAll(dir, 0o700)
	repo := slotGit(context.Background(), "rev-parse", "--path-format=absolute", "--git-common-dir")
	for i := 0; i < n; i++ {
		appendWaitRec(dir, waitRec{Name: name, Repo: repo, Cmd: cmd, Class: slotClassShort, At: time.Now().Unix(), Secs: secs})
	}
}

// sqHeld returns the indexes of the slots of name that are locked.
func sqHeld(t *testing.T, name string, n int) []int {
	t.Helper()
	dir, _ := slotDir()
	var held []int
	for i := 0; i < n; i++ {
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%s.%d.lock", name, i)), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := slotTryLock(f); !ok {
			held = append(held, i)
		}
		_ = f.Close()
	}
	return held
}

type sqTake struct {
	release func()
	err     error
}

// sqGo takes a slot in the background from the current folder.
func sqGo(c *cli, name string, n int) chan sqTake {
	ch := make(chan sqTake, 1)
	go func() {
		_, rel, err := c.takeSlot(context.Background(), name, n)
		ch <- sqTake{rel, err}
	}()
	return ch
}

func sqNotYet(t *testing.T, ch chan sqTake, what string) {
	t.Helper()
	select {
	case r := <-ch:
		if r.release != nil {
			r.release()
		}
		t.Fatalf("%s got a slot while it should wait", what)
	case <-time.After(300 * time.Millisecond):
	}
}

func sqGot(t *testing.T, ch chan sqTake) func() {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.release
	case <-time.After(5 * time.Second):
		t.Fatal("never got a slot")
	}
	return nil
}

func TestSlotWorktreeRunsOneLongRunAtATime(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqRepo(t)
	sqArgs(t)
	sqSeed(t, "tests", sqCmd, 1800, 3)
	old := slotWaitNotice
	slotWaitNotice = 50 * time.Millisecond
	t.Cleanup(func() { slotWaitNotice = old })

	c1, _, _ := sqCLI()
	_, rel1, err := c1.takeSlot(context.Background(), "tests", 4)
	if err != nil {
		t.Fatal(err)
	}
	c2, _, errb := sqCLI()
	ch := sqGo(c2, "tests", 4)
	sqNotYet(t, ch, "second run of the same worktree")
	if h := sqHeld(t, "tests", 4); len(h) != 1 {
		t.Errorf("held slots %v, want exactly one", h)
	}
	dir, _ := slotDir()
	if b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("tests.%d.queue", os.Getpid()))); err != nil || !strings.Contains(string(b), `"behind_own":true`) {
		t.Errorf("queue file %q %v", b, err)
	}
	rel1()
	rel2 := sqGot(t, ch)
	notice := errb.String()
	if !strings.Contains(notice, fmt.Sprintf("this worktree's own earlier long run to finish (pid %d, running 0m: wp testsum -- go test ./x)", os.Getpid())) || !strings.Contains(notice, "WP-1604") {
		t.Errorf("notice %q", notice)
	}
	rel2()
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("tests.%d.queue", os.Getpid()))); !os.IsNotExist(err) {
		t.Errorf("queue file left behind: %v", err)
	}
}

func TestSlotDifferentWorktreesTakeTwoSlots(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqArgs(t)
	c, _, _ := sqCLI()
	sqRepo(t)
	_, r1, err := c.takeSlot(context.Background(), "tests", 4)
	if err != nil {
		t.Fatal(err)
	}
	sqRepo(t)
	ch := sqGo(c, "tests", 4)
	r2 := sqGot(t, ch)
	if h := sqHeld(t, "tests", 4); len(h) != 2 {
		t.Errorf("held slots %v, want two", h)
	}
	r1()
	r2()
}

func TestSlotLongRunsKeepToTheLowerHalf(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqArgs(t)
	c, _, _ := sqCLI()
	var rels []func()
	for i := 0; i < 2; i++ {
		sqRepo(t)
		sqSeed(t, "tests", sqCmd, 1800, 3)
		_, rel, err := c.takeSlot(context.Background(), "tests", 4)
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	if h := sqHeld(t, "tests", 4); len(h) != 2 || h[0] != 0 || h[1] != 1 {
		t.Fatalf("two long runs hold %v, want [0 1]", h)
	}
	sqRepo(t)
	sqSeed(t, "tests", sqCmd, 1800, 3)
	third := sqGo(c, "tests", 4)
	sqNotYet(t, third, "a third long run")
	sqRepo(t)
	short := sqGo(c, "tests", 4)
	relShort := sqGot(t, short)
	if h := sqHeld(t, "tests", 4); len(h) != 3 || h[2] != 3 {
		t.Errorf("a short run should take slot 3, held %v", h)
	}
	relShort()
	rels[0]()
	rels = append(rels[1:], sqGot(t, third))
	for _, r := range rels {
		r()
	}
}

func TestSlotLongAndShortShareTheOnlySlot(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqArgs(t)
	if got := slotOrder(slotClassLong, 1); len(got) != 1 || got[0] != 0 {
		t.Errorf("long order with 1 slot: %v", got)
	}
	if got := slotOrder(slotClassShort, 1); len(got) != 1 || got[0] != 0 {
		t.Errorf("short order with 1 slot: %v", got)
	}
	c, _, _ := sqCLI()
	sqRepo(t)
	sqSeed(t, "tests", sqCmd, 1800, 3)
	_, rel, err := c.takeSlot(context.Background(), "tests", 1)
	if err != nil {
		t.Fatal(err)
	}
	sqRepo(t)
	short := sqGo(c, "tests", 1)
	sqNotYet(t, short, "a short run with the only slot taken")
	rel()
	sqGot(t, short)()
	if got := slotOrder(slotClassLong, 4); len(got) != 2 {
		t.Errorf("long order with 4 slots: %v", got)
	}
	if got := slotOrder(slotClassShort, 4); len(got) != 4 || got[0] != 3 || got[3] != 0 {
		t.Errorf("short order with 4 slots: %v", got)
	}
}

func TestSlotClassify(t *testing.T) {
	r := func(secs int64) waitRec { return waitRec{Name: "tests", Repo: "/a/.git", Cmd: "c", Secs: secs} }
	long := int64(slotLongRun.Seconds())
	if got := classifyRun(nil, "tests", "/a/.git", "c"); got != slotClassShort {
		t.Errorf("unknown: %s", got)
	}
	// The latest five are long, long, 1, long, long: the median is long, though older ones were short.
	recs := []waitRec{r(1), r(1), r(1), r(long), r(long), r(1), r(long), r(long)}
	if got := classifyRun(recs, "tests", "/a/.git", "c"); got != slotClassLong {
		t.Errorf("median of the latest 5 = %s", got)
	}
	if got := classifyRun([]waitRec{r(long), r(long), r(1), r(1), r(1), r(1), r(long)}, "tests", "/a/.git", "c"); got != slotClassShort {
		t.Errorf("only the latest 5 count: %s", got)
	}
	if got := classifyRun([]waitRec{r(long), r(long), r(long)}, "tests", "/b/.git", "c"); got != slotClassShort {
		t.Errorf("another repo's history counted: %s", got)
	}
	if got := classifyRun([]waitRec{r(long)}, "tests", "/a/.git", "other"); got != slotClassShort {
		t.Errorf("another command's history counted: %s", got)
	}
	if got := classifyRun([]waitRec{r(long)}, "lint", "/a/.git", "c"); got != slotClassShort {
		t.Errorf("another name's history counted: %s", got)
	}
	if got := slotCmdOf([]string{"testsum", "--", "go", "test", "--", "x"}); got != "go test -- x" {
		t.Errorf("cmd key %q", got)
	}
	if got := slotCmdOf([]string{"slot", "run", "t"}); got != "slot run t" {
		t.Errorf("cmd key without -- %q", got)
	}
}

func TestSlotReleaseRecordsTheWait(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqRepo(t)
	sqArgs(t)
	c, _, _ := sqCLI()
	_, rel, err := c.takeSlot(context.Background(), "tests", 2)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := slotDir()
	if len(readWaitRecs(dir)) != 0 {
		t.Fatal("recorded before release")
	}
	rel()
	recs := readWaitRecs(dir)
	if len(recs) != 1 {
		t.Fatalf("records %v", recs)
	}
	got := recs[0]
	if got.Name != "tests" || got.Class != slotClassShort || got.Cmd != sqCmd || got.At == 0 || got.Wait < 0 || got.Secs < 0 || !strings.HasSuffix(got.Repo, "/.git") {
		t.Errorf("record %+v", got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, slotWaitsFile))
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"name", "repo", "cmd", "class", "at", "wait", "secs"} {
		if _, ok := m[k]; !ok {
			t.Errorf("no %q in %s", k, b)
		}
	}
}

func TestSlotWaitsLogIsTrimmed(t *testing.T) {
	slotEnv(t)
	dir, _ := slotDir()
	_ = os.MkdirAll(dir, 0o700)
	pad := strings.Repeat("x", 150)
	var buf bytes.Buffer
	for i := 0; i < 3500; i++ {
		b, _ := json.Marshal(waitRec{Name: "tests", Repo: "/r/.git", Cmd: pad, At: int64(i + 1)})
		buf.Write(append(b, '\n'))
	}
	if buf.Len() <= slotWaitsMaxSize {
		t.Fatalf("fixture too small: %d", buf.Len())
	}
	if err := os.WriteFile(filepath.Join(dir, slotWaitsFile), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/r/.git", Cmd: pad, At: 9999})
	recs := readWaitRecs(dir)
	if len(recs) != slotWaitsKeep || recs[len(recs)-1].At != 9999 || recs[0].At != 3500-int64(slotWaitsKeep)+2 {
		t.Errorf("kept %d records, first at %d, last at %d", len(recs), recs[0].At, recs[len(recs)-1].At)
	}
	// wp slot ls reads every *.lock here as a slot: the log's lock isn't one.
	if m, _ := filepath.Glob(filepath.Join(dir, "*.lock")); len(m) != 0 {
		t.Errorf("appending left %v, which wp slot ls would list as slots", m)
	}
}

func TestSlotLsShowsWaitersAndWaits(t *testing.T) {
	slotEnv(t)
	dir, _ := slotDir()
	_ = os.MkdirAll(dir, 0o700)
	now := time.Now()
	write := func(name string, v any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	live := fmt.Sprintf("tests.%d.queue", os.Getpid())
	write(live, queueRec{Since: now.Add(-12 * time.Minute).Unix(), WD: "/wd", Cmd: "testsum -- go test ./x", Class: "short"})
	write("tests.2147483646.queue", queueRec{Since: now.Unix(), WD: "/gone", Cmd: "dead", Class: "short"})
	write("lint.1.queue", queueRec{Since: now.Add(-3 * time.Minute).Unix(), WD: "/own", Cmd: "x", Class: "long", BehindOwn: true})
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/p/JobHunt/.git", Class: "short", At: now.Unix(), Wait: 4})
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/p/JobHunt/.git", Class: "short", At: now.Unix(), Wait: 70})
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/p/old/.git", Class: "long", At: now.Add(-48 * time.Hour).Unix(), Wait: 9})
	// a tree lock is not a slot lock line
	_ = os.WriteFile(filepath.Join(dir, "tree-abc.tests.treelock"), nil, 0o600)

	var out, errb bytes.Buffer
	if code := run([]string{"slot", "ls"}, &out, &errb); code != 0 {
		t.Fatalf("slot ls: %d %q", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		fmt.Sprintf("(short) pid %d in /wd: testsum -- go test ./x", os.Getpid()),
		"(behind its worktree's own run) pid 1 in /own: x",
		"waiting 12m",
		"waits over the last 24h (p50 / p95, runs):",
		"  JobHunt     short   4s / 1m10s (2)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("slot ls lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "/gone") || strings.Contains(s, "treelock") || strings.Contains(s, "old ") {
		t.Errorf("slot ls shows what it shouldn't:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "tests.2147483646.queue")); !os.IsNotExist(err) {
		t.Errorf("dead waiter file kept: %v", err)
	}
	if strings.Index(s, "pid 1 in /own") < strings.Index(s, "in /wd") {
		t.Errorf("waiters not oldest first:\n%s", s)
	}
}

// A short run from a worktree whose long run is holding goes straight to a slot.
func TestSlotShortRunSkipsItsWorktreesLongRun(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqRepo(t)
	sqSeed(t, "tests", "make check", 1800, 3)
	c, _, _ := sqCLI()
	_, long, err := c.takeSlotFor(context.Background(), "tests", 4, "make check")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan sqTake, 1)
	go func() {
		_, rel, err := c.takeSlotFor(context.Background(), "tests", 4, "go test ./x")
		ch <- sqTake{rel, err}
	}()
	short := sqGot(t, ch)
	if h := sqHeld(t, "tests", 4); len(h) != 2 || h[1] != 3 {
		t.Errorf("held %v, want the long run's slot and slot 3", h)
	}
	dir, _ := slotDir()
	if m, _ := filepath.Glob(filepath.Join(dir, "*.queue")); len(m) != 0 {
		t.Errorf("a short run that never waited left %v", m)
	}
	short()
	long()
}

// The key is the command, however wp was asked to run it.
func TestSlotKeyIsTheSameForTestsumAndShip(t *testing.T) {
	typed := []string{"go", "test", "./x"}
	if a, b := slotKey(typed), slotKey(goTestJSON(typed)); a != b {
		t.Errorf("typed %q, with -json %q", a, b)
	}
	if got := slotKey([]string{"make", "check"}); got != "make check" {
		t.Errorf("slotKey = %q", got)
	}
}

// A run whose WAYPOINT_SLOTS_HELD was lost waits on its own ancestor's tree
// lock forever unless it goes on to a slot.
func TestSlotLostHeldEnvDoesNotDeadlock(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WP_TEST_MAIN", "1")
	sqRepo(t)
	sqSeed(t, "tests", "env -u WAYPOINT_SLOTS_HELD "+bin+" slot run tests -n 4 -- echo x", 1800, 3)
	sqSeed(t, "tests", "echo x", 1800, 3)
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"slot", "run", "tests", "-n", "4", "--", "env", "-u", slotHeldEnv, bin, "slot", "run", "tests", "-n", "4", "--", "echo", "x"}, &out, &errb)
	}()
	select {
	case code := <-done:
		if code != 0 || !strings.Contains(out.String(), "x") {
			t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the inner run waited on its ancestor's worktree lock")
	}
}

func TestSlotGaveUpWaitIsRecorded(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	sqRepo(t)
	sqArgs(t)
	c, _, _ := sqCLI()
	_, rel, err := c.takeSlot(context.Background(), "tests", 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := c.takeSlot(ctx, "tests", 1); err == nil {
		t.Fatal("got a slot that was taken")
	}
	rel()
	dir, _ := slotDir()
	recs := readWaitRecs(dir)
	if len(recs) != 2 || !recs[0].GaveUp || recs[0].Secs != 0 || recs[1].GaveUp {
		t.Fatalf("records %+v", recs)
	}
	// Giving up says nothing about how long the command takes.
	long := waitRec{Name: "tests", Repo: recs[0].Repo, Cmd: sqCmd, Secs: 1800}
	gave := waitRec{Name: "tests", Repo: recs[0].Repo, Cmd: sqCmd, GaveUp: true}
	if got := classifyRun([]waitRec{long, long, gave, gave, gave}, "tests", recs[0].Repo, sqCmd); got != slotClassLong {
		t.Errorf("gave-up records counted: %s", got)
	}
	// They show in the 24h waits.
	var out bytes.Buffer
	(&cli{out: &out}).slotQueueLs(dir)
	if !strings.Contains(out.String(), "(2)") {
		t.Errorf("stats: %q", out.String())
	}
}

func TestSlotTrimLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, slotWaitsFile)
	if err := os.WriteFile(p, []byte("{}\n{}\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	trimWaits(p, 2)
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("directory holds %v", ents)
	}
	if b, _ := os.ReadFile(p); string(b) != "{}\n{}\n" {
		t.Errorf("trimmed to %q", b)
	}
}

// A hold of several runs (wp testsum --repeat) is recorded under its own key,
// so its total doesn't make a plain run of the same command classify long.
func TestSlotRepeatHoldDoesNotClassifyPlainRuns(t *testing.T) {
	argv := []string{"go", "test", "./x", "-run", "TestA"}
	plain, repeat := slotField(slotKey(argv)), slotField(slotKeyFor("wp testsum --repeat", argv))
	if plain == repeat {
		t.Fatalf("one key %q for a plain run and a repeat hold", plain)
	}
	long := int64(slotLongRun.Seconds()) * 2
	var recs []waitRec
	for i := 0; i < 5; i++ {
		recs = append(recs, waitRec{Name: "tests", Repo: "/a/.git", Cmd: repeat, Secs: long}, waitRec{Name: "tests", Repo: "/a/.git", Cmd: plain, Secs: 30})
	}
	if got := classifyRun(recs, "tests", "/a/.git", plain); got != slotClassShort {
		t.Errorf("plain run classified %s by repeat holds", got)
	}
	if got := classifyRun(recs, "tests", "/a/.git", repeat); got != slotClassLong {
		t.Errorf("repeat hold classified %s", got)
	}
}
