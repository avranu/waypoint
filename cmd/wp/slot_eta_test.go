package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func recs(name, cmd, repo string, mins ...float64) []slotRunRec {
	var out []slotRunRec
	for i, m := range mins {
		out = append(out, slotRunRec{Name: name, Cmd: cmd, Repo: repo, At: int64(1000 + i), Secs: m * 60})
	}
	return out
}

func min2(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }

func TestSlotMedianRunUsesRecentRunsOfThisCommandHere(t *testing.T) {
	h := append(recs("tests", "make check", "/r", 100, 100, 100), recs("tests", "make check", "/other", 1)...)
	h = append(h, recs("tests", "go vet", "/r", 2)...)
	// The latest ten, an even count: the middle pair is 20 and 40, so 30. Without
	// the window the three 100s before them would make it 40; one middle value
	// alone would be 20 or 40.
	h = append(h, recs("tests", "make check", "/r", 10, 10, 10, 10, 20, 40, 40, 40, 40, 40)...)
	if got, ok := slotMedianRun(h, "tests", "make check", "/r"); !ok || got != min2(30) {
		t.Errorf("median = %v %v, want 30m", got, ok)
	}
	if _, ok := slotMedianRun(h, "tests", "make check", "/nowhere"); ok {
		t.Error("an estimate for a repo with no runs")
	}
	if _, ok := slotMedianRun(nil, "tests", "make check", "/r"); ok {
		t.Error("an estimate with no history")
	}
}

func TestSlotWaitETA(t *testing.T) {
	h := recs("tests", "make check", "/r", 30, 30, 30)
	holders := []slotHolder{{Cmd: "make check", Elapsed: min2(10)}}
	// The one holder has 20 of its usual 30 minutes left.
	if w, ok := slotWaitETA(h, "tests", holders, 0, min2(30)); !ok || w != min2(20) {
		t.Errorf("one holder, nobody ahead: %v %v", w, ok)
	}
	// Someone queued ahead takes the slot when it frees and holds it 30 minutes.
	if w, ok := slotWaitETA(h, "tests", holders, 1, min2(30)); !ok || w != min2(50) {
		t.Errorf("one holder, one ahead: %v %v", w, ok)
	}
	// Two holders: the earlier to finish frees a slot first.
	two := []slotHolder{{Cmd: "make check", Elapsed: min2(25)}, {Cmd: "make check", Elapsed: min2(5)}}
	if w, ok := slotWaitETA(h, "tests", two, 0, min2(30)); !ok || w != min2(5) {
		t.Errorf("two holders: %v %v", w, ok)
	}
	// A holder past its usual time is about to end.
	late := []slotHolder{{Cmd: "make check", Elapsed: min2(45)}}
	if w, ok := slotWaitETA(h, "tests", late, 0, min2(30)); !ok || w != 0 {
		t.Errorf("a late holder: %v %v", w, ok)
	}
	// Two queued ahead each take the slot in turn.
	if w, ok := slotWaitETA(h, "tests", holders, 2, min2(30)); !ok || w != min2(80) {
		t.Errorf("one holder, two ahead: %v %v", w, ok)
	}
	// No holders: nothing to estimate from.
	if _, ok := slotWaitETA(h, "tests", nil, 0, min2(30)); ok {
		t.Error("an estimate with no holders")
	}
	// A holder whose command has no history: unknown.
	if _, ok := slotWaitETA(h, "tests", []slotHolder{{Cmd: "pytest", Elapsed: min2(1)}}, 0, min2(30)); ok {
		t.Error("an estimate from a holder nobody has timed")
	}
}

func TestSlotETAMessage(t *testing.T) {
	// Queued: wait plus run, padded by 20% and rounded to minutes: (20+30)*1.2 = 60.
	got := slotETAMessage("tests", true, min2(20), min2(30), true)
	want := "The test is queued, and is expected to complete in 60 minutes. If you have other work to do in the meantime, handle it while you wait. Otherwise, end your turn and you'll be notified when the test concludes."
	if got != want {
		t.Errorf("queued:\n%s\nwant:\n%s", got, want)
	}
	if got := slotETAMessage("tests", false, 0, min2(10), true); !strings.HasPrefix(got, "The test is running, and is expected to complete in 12 minutes.") {
		t.Errorf("running: %s", got)
	}
	if got := slotETAMessage("build", false, 0, min2(10), true); !strings.HasPrefix(got, "The command is running,") || !strings.Contains(got, "when the command concludes") {
		t.Errorf("another slot's command: %s", got)
	}
	if got := slotETAMessage("tests", false, 0, min2(1), true); !strings.Contains(got, "in 1 minute.") {
		t.Errorf("singular: %s", got)
	}
	// Queued with no way to tell the wait: only the run is said.
	if got := slotETAMessage("tests", true, -1, min2(10), true); !strings.Contains(got, "queued") || !strings.Contains(got, "once it starts") || !strings.Contains(got, "12 minutes") {
		t.Errorf("unknown wait: %s", got)
	}
	// Without the advice (the stderr of a foreground run, read after the run
	// ends) only the ETA sentence is said.
	if got := slotETAMessage("tests", true, min2(20), min2(30), false); got != "The test is queued, and is expected to complete in 60 minutes." {
		t.Errorf("no advice: %q", got)
	}
	// Nothing to say: no run time, or under half a minute in all.
	if got := slotETAMessage("tests", true, min2(5), 0, true); got != "" {
		t.Errorf("no run history: %q", got)
	}
	if got := slotETAMessage("tests", false, 0, 5*time.Second, true); got != "" {
		t.Errorf("a few seconds: %q", got)
	}
}

// A run records how long it took; the next run of the same command says how
// long it expects; and a command with no history says nothing.
func TestSlotRunSaysItsETAFromTheRecordedRuns(t *testing.T) {
	slotEnv(t)
	dir, _ := slotDir()
	args := []string{"slot", "run", "tests", "-n", "1", "--", "sh", "-c", "true"}

	var out, errb bytes.Buffer
	if code := run(args, &out, &errb); code != 0 || strings.Contains(errb.String(), "expected to complete") {
		t.Fatalf("first run: %d %q", code, errb.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "runs.jsonl"))
	if err != nil || strings.Count(string(b), "\n") != 1 {
		t.Fatalf("the run wasn't recorded once: %q %v", b, err)
	}
	var rec slotRunRec
	if err := json.Unmarshal(b, &rec); err != nil || rec.Name != "tests" || rec.Cmd != "sh -c true" || rec.Secs < 0 || rec.Secs > 30 {
		t.Fatalf("record = %+v %v", rec, err)
	}

	// Runs that took ten minutes: the next says twelve.
	seeded := ""
	for range 3 {
		rec.Secs = 600
		line, _ := json.Marshal(rec)
		seeded += string(line) + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "runs.jsonl"), []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := run(args, &out, &errb); code != 0 || !strings.Contains(errb.String(), "The test is running, and is expected to complete in 12 minutes.") {
		t.Fatalf("second run: %d %q", code, errb.String())
	}
	if strings.Contains(errb.String(), "end your turn") {
		t.Errorf("a foreground run's stderr gave the advice for a backgrounded one: %q", errb.String())
	}
	// A failed run is not a run to expect.
	if code := run([]string{"slot", "run", "tests", "-n", "1", "--", "sh", "-c", "exit 3"}, &out, &errb); code != 3 {
		t.Fatal(code)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "runs.jsonl"))
	if strings.Contains(string(b), "exit 3") {
		t.Errorf("a failed run was recorded:\n%s", b)
	}
}

// A run that has to wait says when it expects to finish, from the holder's
// time so far and its own usual time.
func TestSlotRunQueuedSaysItsETA(t *testing.T) {
	slotEnv(t)
	oldNotice, oldPoll := slotWaitNotice, slotPoll
	slotWaitNotice, slotPoll = 20*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { slotWaitNotice, slotPoll = oldNotice, oldPoll })
	dir, _ := slotDir()

	// The holder started 10 minutes ago on the same command, which takes 30.
	hold, err := acquireSlot(t.Context(), "tests", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "tests.0.lock")
	f, _ := os.OpenFile(lock, os.O_RDWR, 0o600)
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte("1\t"+strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)+"\t/r\tslot run tests -- sh -c true\n"), 0)
	_ = f.Close()
	hist := recs("tests", "sh -c true", slotRepoKey(), 30, 30, 30)
	var lines []string
	for _, r := range hist {
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(filepath.Join(dir, "runs.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	eb := &lockedBuf{}
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"slot", "run", "tests", "-n", "1", "--", "sh", "-c", "true"}, &out, eb)
	}()
	deadline := time.After(10 * time.Second)
	for !strings.Contains(eb.String(), "queued") {
		select {
		case <-deadline:
			t.Fatalf("no ETA while queued: %q", eb.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	hold.release()
	<-done
	// (20 left + 30 run) * 1.2 = 60
	if got := eb.String(); !strings.Contains(got, "The test is queued, and is expected to complete in 60 minutes.") || strings.Contains(got, "end your turn") {
		t.Errorf("stderr:\n%s", got)
	}
}

// lockedBuf is a buffer a goroutine writes while the test reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestSlotQueuedAheadCountsLiveEarlierWaiters(t *testing.T) {
	slotEnv(t)
	dir, _ := slotDir()
	_ = os.MkdirAll(dir, 0o700)
	now := time.Now().UnixNano()
	// Waiting and earlier; waiting and later; dead and earlier; and the file
	// a waiter killed with SIGKILL left, whose pid a live process has now
	// (pid 1): it holds no lock, so it is no one's (audit #1382.2).
	writeWaiter(t, dir, os.Getppid(), now-1e9)
	writeWaiter(t, dir, os.Getpid()+1, now+1e12)
	_ = os.WriteFile(filepath.Join(dir, "tests.4194999.wait"), []byte(strconv.FormatInt(now-2e9, 10)), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "tests.1.wait"), []byte(strconv.FormatInt(now-3e9, 10)), 0o600)
	remove := slotAddWaiter(dir, "tests")
	defer remove()
	if got := slotQueuedAhead(dir, "tests"); got != 1 {
		t.Errorf("ahead = %d, want 1", got)
	}
	for _, pid := range []string{"1", "4194999"} {
		if _, err := os.Stat(filepath.Join(dir, "tests."+pid+".wait")); !os.IsNotExist(err) {
			t.Errorf("the left-over waiter file of %s is still there: %v", pid, err)
		}
	}
}

// stubPidSessions makes slotPidSession read from m (pid to session id).
func stubPidSessions(t *testing.T, m map[int]string) {
	t.Helper()
	old := slotPidSession
	slotPidSession = func(pid int) string { return m[pid] }
	t.Cleanup(func() { slotPidSession = old })
}

// writeWaiter is a waiter of pid that began at ts, waiting (it holds the
// file's lock) until the test ends.
func writeWaiter(t *testing.T, dir string, pid int, ts int64) {
	writeWaiterAs(t, dir, pid, ts, "")
}

// writeWaiterAs is writeWaiter recording the waiter's session.
func writeWaiterAs(t *testing.T, dir string, pid int, ts int64, session string) {
	t.Helper()
	p := filepath.Join(dir, "tests."+strconv.Itoa(pid)+".wait")
	line := strconv.FormatInt(ts, 10)
	if session != "" {
		line += "\t" + session
	}
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0o600) // #nosec G304 -- the test's own directory
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := slotTryLock(f); !ok || err != nil {
		t.Fatalf("lock %s: %v %v", p, ok, err)
	}
	t.Cleanup(func() { _ = f.Close() })
}

func TestSlotQueuedAheadTieBreaksOnPid(t *testing.T) {
	slotEnv(t)
	dir, _ := slotDir()
	_ = os.MkdirAll(dir, 0o700)
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skip("no sleep:", err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	const ts = int64(5e18)
	writeWaiter(t, dir, os.Getpid(), ts)
	writeWaiter(t, dir, 1, ts)
	writeWaiter(t, dir, child.Process.Pid, ts)
	// Began at the same instant: the lower pid is ahead.
	want := 1
	if child.Process.Pid < os.Getpid() {
		want = 2
	}
	if got := slotQueuedAhead(dir, "tests"); got != want {
		t.Errorf("ahead = %d, want %d (pids %d, %d)", got, want, os.Getpid(), child.Process.Pid)
	}
}

func TestSlotQueuedAheadForASession(t *testing.T) {
	slotEnv(t)
	dir, _ := slotDir()
	_ = os.MkdirAll(dir, 0o700)
	now := time.Now().UnixNano()
	own, other := os.Getppid(), 1
	stubPidSessions(t, map[int]string{own: "s1", other: "s2", os.Getpid(): "s2"})
	writeWaiter(t, dir, own, now-60e9)
	// The command's own waiter is not ahead of it, and with nobody else: 0.
	if got := slotQueuedAheadOf(dir, "tests", "s1"); got != 0 {
		t.Errorf("own waiter counted: %d", got)
	}
	// One that began before it is ahead; one that began after is not.
	writeWaiter(t, dir, other, now-300e9)
	writeWaiter(t, dir, os.Getpid(), now-10e9)
	if got := slotQueuedAheadOf(dir, "tests", "s1"); got != 1 {
		t.Errorf("ahead = %d, want 1", got)
	}
	// No waiter of the session yet (it has not started to wait): both are ahead.
	if got := slotQueuedAheadOf(dir, "tests", "s9"); got != 3 {
		t.Errorf("a session with no waiter: ahead = %d, want 3", got)
	}
}

// A run that fails to get a slot at once is a waiter from then on, not only
// once it says it is waiting.
func TestSlotWaiterIsRegisteredWhenTheWaitBegins(t *testing.T) {
	slotEnv(t)
	oldNotice, oldPoll := slotWaitNotice, slotPoll
	slotWaitNotice, slotPoll = time.Hour, 10*time.Millisecond
	t.Cleanup(func() { slotWaitNotice, slotPoll = oldNotice, oldPoll })
	dir, _ := slotDir()
	first, err := acquireSlot(t.Context(), "tests", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *slotHold, 1)
	go func() {
		h, _ := acquireSlot(t.Context(), "tests", 1, nil)
		got <- h
	}()
	// Someone else (as another session's process) counts it ahead, long before
	// slotWaitNotice.
	stubPidSessions(t, map[int]string{os.Getpid(): "waiter"})
	deadline := time.After(5 * time.Second)
	for slotQueuedAheadOf(dir, "tests", "newcomer") != 1 {
		select {
		case <-deadline:
			t.Fatal("the waiter is not visible before slotWaitNotice")
		case <-time.After(10 * time.Millisecond):
		}
	}
	first.release()
	h := <-got
	h.release()
	if files, _ := filepath.Glob(filepath.Join(dir, "*.wait")); len(files) != 0 {
		t.Errorf("waiter files left: %v", files)
	}
}

// A waiter and a holder record the session they run in, so the wait hook
// knows its own where a process's session can't be read (darwin, audit
// #1382.1).
func TestSlotWaiterAndHolderRecordTheirSession(t *testing.T) {
	slotEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-x")
	stubPidSessions(t, map[int]string{})
	dir, _ := slotDir()
	h, err := acquireSlot(t.Context(), "tests", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.release()
	if hs := slotHolders(dir, "tests"); len(hs) != 1 || hs[0].Session != "sess-x" {
		t.Errorf("holders = %+v", hs)
	}
	remove := slotAddWaiter(dir, "tests")
	defer remove()
	b, _ := os.ReadFile(slotWaiterPath(dir, "tests"))
	if !strings.HasSuffix(strings.TrimSpace(string(b)), "\tsess-x") {
		t.Errorf("waiter file = %q", b)
	}
	// Another process asking for sess-x's command counts the waiter as its
	// own, old as it is; for another session it is ahead.
	stubPidSessions(t, map[int]string{})
	if got := slotQueuedAheadOf(dir, "tests", "sess-x"); got != 0 {
		t.Errorf("own waiter ahead: %d", got)
	}
	if got := slotQueuedAheadOf(dir, "tests", "sess-y"); got != 1 {
		t.Errorf("another session's waiter: ahead = %d, want 1", got)
	}
}

// A waiter whose wait is cancelled leaves no waiter file (audit #1372.25).
func TestSlotWaiterCancelledLeavesNoFile(t *testing.T) {
	slotEnv(t)
	oldPoll := slotPoll
	slotPoll = 10 * time.Millisecond
	t.Cleanup(func() { slotPoll = oldPoll })
	dir, _ := slotDir()
	first, err := acquireSlot(t.Context(), "tests", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := acquireSlot(ctx, "tests", 1, nil)
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for files, _ := filepath.Glob(filepath.Join(dir, "*.wait")); len(files) == 0; files, _ = filepath.Glob(filepath.Join(dir, "*.wait")) {
		select {
		case <-deadline:
			t.Fatal("no waiter file")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.wait")); len(files) != 0 {
		t.Errorf("waiter files left: %v", files)
	}
}

// Without the record's lock a run is still appended, but the record isn't
// trimmed: a trim then could drop a line another process appends
// (audit #1372.10).
func TestSlotRecordRunWithoutTheLockDoesntTrim(t *testing.T) {
	dir := t.TempDir()
	old := fileLock
	fileLock = func(*os.File) (func(), error) { return nil, errors.New("no lock") }
	t.Cleanup(func() { fileLock = old })
	pad := strings.Repeat("x", 2000)
	var sb strings.Builder
	for i := range slotRunsKeep + 50 {
		b, _ := json.Marshal(slotRunRec{Name: "tests", Cmd: pad, At: int64(i), Secs: 1})
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, slotRunsFile), []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	slotRecordRun(dir, slotRunRec{Name: "tests", Cmd: "new", At: 9999, Secs: 1})
	if got := len(slotRuns(dir)); got != slotRunsKeep+51 {
		t.Errorf("runs = %d, want %d", got, slotRunsKeep+51)
	}
}

func TestSlotRunsSkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	good := `{"name":"tests","cmd":"a","repo":"/r","at":1,"secs":3}`
	body := good + "\nnot json\n" + `{"name":"tests","cmd":"b","repo":"/r","at":2,"secs":-4}` + "\n" + good + "\n"
	if err := os.WriteFile(filepath.Join(dir, slotRunsFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := slotRuns(dir); len(got) != 2 || got[0].Cmd != "a" || got[1].Cmd != "a" {
		t.Errorf("slotRuns = %+v", got)
	}
}

func TestSlotRecordRunTrimsByRename(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, slotRunsFile)
	var sb strings.Builder
	for i := range 3000 {
		b, _ := json.Marshal(slotRunRec{Name: "tests", Cmd: "c" + strconv.Itoa(i) + strings.Repeat("x", 100), Repo: "/r", At: int64(i), Secs: 1})
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	slotRecordRun(dir, slotRunRec{Name: "tests", Cmd: "last", Repo: "/r", At: 9999, Secs: 1})
	got := slotRuns(dir)
	if len(got) != slotRunsKeep || got[len(got)-1].Cmd != "last" {
		t.Fatalf("kept %d lines, last %+v", len(got), got[len(got)-1])
	}
	if after, _ := os.Stat(p); os.SameFile(before, after) {
		t.Error("the file was rewritten in place")
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(files) != 0 {
		t.Errorf("temp files left: %v", files)
	}
}

// A run is recorded under the record's lock, so a trim (read, then rename)
// by one process can't drop a line another appends meanwhile (audit #1357.2):
// with the lock held elsewhere, the append waits for it.
func TestSlotRecordRunWaitsForTheRecordLock(t *testing.T) {
	dir := t.TempDir()
	lf, err := os.OpenFile(filepath.Join(dir, slotRunsLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lf.Close() }()
	unlock, err := fileLock(lf)
	if err != nil {
		t.Skip("no flock here:", err)
	}
	done := make(chan struct{})
	go func() {
		slotRecordRun(dir, slotRunRec{Name: "tests", Cmd: "c", Repo: "/r", At: 1, Secs: 1})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("recorded a run while another process held the record's lock")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run was never recorded after the lock was released")
	}
	if got := slotRuns(dir); len(got) != 1 {
		t.Fatalf("runs = %+v", got)
	}
}

func TestSlotRunKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		os   []string
		argv []string
		want string
	}{
		{"testsum", []string{"wp", "testsum", "--", "make", "check"}, nil, "make check"},
		{"a dashes word inside one argument", []string{"wp", "ship", "WP-1", "--check", "cargo test -- --nocapture"}, []string{"cargo", "test", "--", "--nocapture"}, "cargo test -- --nocapture"},
		{"ship check of go test", []string{"wp", "ship", "WP-1", "--check", "go test ./..."}, []string{"go", "test", "-json", "./..."}, "go test ./..."},
		{"testsum without dashes", []string{"wp", "testsum", "go", "test", "./..."}, []string{"go", "test", "-json", "./..."}, "go test ./..."},
		{"slot run", []string{"wp", "slot", "run", "tests", "-n", "1", "--", "sh", "-c", "true"}, []string{"sh", "-c", "true"}, "sh -c true"},
	} {
		if got := slotRunKey(tc.os, tc.argv); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if got, _, _, _ := waitETACommand("wp testsum -- make check"); got != "tests" {
		t.Fatal(got)
	}
	_, cmd, _, _ := waitETACommand("wp testsum -- make check")
	if want := slotRunKey([]string{"wp", "testsum", "--", "make", "check"}, nil); cmd != want {
		t.Errorf("hook key %q, command key %q", cmd, want)
	}
}
