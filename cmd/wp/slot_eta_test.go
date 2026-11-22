package main

import (
	"bytes"
	"encoding/json"
	"os"
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
	h = append(h, recs("tests", "make check", "/r", 30, 40, 20, 30, 50, 30, 20, 40, 30, 30)...) // the latest ten: median 30
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
	// A holder whose command has no history: unknown.
	if _, ok := slotWaitETA(h, "tests", []slotHolder{{Cmd: "pytest", Elapsed: min2(1)}}, 0, min2(30)); ok {
		t.Error("an estimate from a holder nobody has timed")
	}
}

func TestSlotETAMessage(t *testing.T) {
	// Queued: wait plus run, padded by 20% and rounded to minutes: (20+30)*1.2 = 60.
	got := slotETAMessage("tests", true, min2(20), min2(30))
	want := "The test is queued, and is expected to complete in 60 minutes. If you have other work to do in the meantime, handle it while you wait. Otherwise, end your turn and you'll be notified when the test concludes."
	if got != want {
		t.Errorf("queued:\n%s\nwant:\n%s", got, want)
	}
	if got := slotETAMessage("tests", false, 0, min2(10)); !strings.HasPrefix(got, "The test is running, and is expected to complete in 12 minutes.") {
		t.Errorf("running: %s", got)
	}
	if got := slotETAMessage("build", false, 0, min2(10)); !strings.HasPrefix(got, "The command is running,") || !strings.Contains(got, "when the command concludes") {
		t.Errorf("another slot's command: %s", got)
	}
	if got := slotETAMessage("tests", false, 0, min2(1)); !strings.Contains(got, "in 1 minute.") {
		t.Errorf("singular: %s", got)
	}
	// Queued with no way to tell the wait: only the run is said.
	if got := slotETAMessage("tests", true, -1, min2(10)); !strings.Contains(got, "queued") || !strings.Contains(got, "once it starts") || !strings.Contains(got, "12 minutes") {
		t.Errorf("unknown wait: %s", got)
	}
	// Nothing to say: no run time, or under half a minute in all.
	if got := slotETAMessage("tests", true, min2(5), 0); got != "" {
		t.Errorf("no run history: %q", got)
	}
	if got := slotETAMessage("tests", false, 0, 5*time.Second); got != "" {
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
	if got := eb.String(); !strings.Contains(got, "The test is queued, and is expected to complete in 60 minutes.") {
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
	// Alive and earlier; alive and later; dead and earlier.
	_ = os.WriteFile(filepath.Join(dir, "tests."+strconv.Itoa(os.Getppid())+".wait"), []byte(strconv.FormatInt(now-1e9, 10)), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "tests.1.wait"), []byte(strconv.FormatInt(now+1e12, 10)), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "tests.4194999.wait"), []byte(strconv.FormatInt(now-2e9, 10)), 0o600)
	remove := slotAddWaiter(dir, "tests")
	defer remove()
	if got := slotQueuedAhead(dir, "tests"); got != 1 {
		t.Errorf("ahead = %d, want 1", got)
	}
}
