package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The age comes from field 22 of /proc/self/stat, counted from the last ")":
// a comm with spaces and parentheses must not shift it.
func TestAgeFromProc(t *testing.T) {
	tail := " S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 1000 20 21"
	for _, comm := range []string{"(wp)", "(a b)", "(x) 1 2 (y)", "(w)p)"} {
		got, ok := ageFromProc("4242 "+comm+tail+"\n", "12.50 99.00\n")
		if !ok || got != 2500*time.Millisecond {
			t.Errorf("comm %s: age %v ok=%v, want 2.5s", comm, got, ok)
		}
	}
	for _, bad := range [][2]string{{"no parens", "1 1"}, {"1 (wp) S 1 2", "1 1"}, {"1 (wp)" + tail, ""}, {"1 (wp)" + tail, "x y"}, {"1 (wp)" + tail, "5.0 5.0"}} {
		if d, ok := ageFromProc(bad[0], bad[1]); ok {
			t.Errorf("%q / %q gave %v", bad[0], bad[1], d)
		}
	}
	if runtimeAge, ok := procAge(); ok && (runtimeAge < 0 || runtimeAge > time.Hour) {
		t.Errorf("this test process is %v old?", runtimeAge)
	}
}

func setAge(t *testing.T, d time.Duration) {
	t.Helper()
	old := processAge
	processAge = func() time.Duration { return d }
	t.Cleanup(func() { processAge = old })
}

func TestHookLate(t *testing.T) {
	setAge(t, 1400*time.Millisecond)
	if hookLate(hookLateAfter(hookNudgeTimeout)) {
		t.Error("1.4 s into a 2 s hook is not late")
	}
	setAge(t, 1600*time.Millisecond)
	if !hookLate(hookLateAfter(hookNudgeTimeout)) {
		t.Error("1.6 s into a 2 s hook is late")
	}
	if hookLate(hookLateAfter(hookGateTimeout)) {
		t.Error("1.6 s into a 10 s hook is not late")
	}
}

func lateRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return repo
}

// A late nudge run still returns the chain note (a cancelled run's output is
// discarded at no cost) but marks it shown only on the lateTellsMax-th try:
// told on three late calls, then silent.
func TestChainNudgeLateTellsThenMarks(t *testing.T) {
	repo := lateRepo(t)
	c := &cli{dir: t.TempDir()}
	call := func(step string) bool {
		return c.chainNudge(&hookEventInput{SessionID: "late"}, repo, step, true)
	}
	setAge(t, 1600*time.Millisecond)
	for _, s := range []string{stepFind, stepLook, stepLook, stepLook} {
		if call(s) {
			t.Fatal("told before the chain was long enough")
		}
	}
	for n := 1; n <= lateTellsMax; n++ {
		if !call(stepFind) {
			t.Fatalf("late call %d did not tell", n)
		}
		st := &chainState{}
		if err := readJSON(c.chainPath("late", ""), st); err != nil {
			t.Fatal(err)
		}
		if st.Done != (n == lateTellsMax) {
			t.Errorf("after late tell %d: Done = %v", n, st.Done)
		}
	}
	if call(stepLook) {
		t.Error("told after being marked")
	}
}

// An in-time run tells and marks at once.
func TestChainNudgeInTimeMarks(t *testing.T) {
	repo := lateRepo(t)
	c := &cli{dir: t.TempDir()}
	setAge(t, 50*time.Millisecond)
	told := 0
	for _, s := range []string{stepFind, stepLook, stepLook, stepLook, stepFind, stepLook, stepLook} {
		if c.chainNudge(&hookEventInput{SessionID: "x"}, repo, s, true) {
			told++
		}
	}
	if told != 1 {
		t.Errorf("told %d times", told)
	}
}

func TestHookTimeLeft(t *testing.T) {
	limit := hookLateAfter(hookNudgeTimeout)
	setAge(t, 1400*time.Millisecond)
	if got := hookTimeLeft(limit); got != 100*time.Millisecond {
		t.Errorf("left = %v", got)
	}
	setAge(t, 3*time.Second)
	if got := hookTimeLeft(limit); got != 0 {
		t.Errorf("left = %v, want 0", got)
	}
}

// A held chain lock must not carry the nudge past the late point, nor count.
func TestChainNudgeBoundedLock(t *testing.T) {
	repo := lateRepo(t)
	c := &cli{dir: t.TempDir()}
	path := c.chainPath("held", "")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	setAge(t, 1400*time.Millisecond)
	start := time.Now()
	for i := 0; i < 6; i++ {
		if c.chainNudge(&hookEventInput{SessionID: "held"}, repo, stepFind, true) {
			t.Fatal("told without the lock")
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("6 calls waited %v on the lock", d)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a count was saved without the lock")
	}
}

// A late nudge run tells a pause but marks it only on the lateTellsMax-th try;
// the SessionStart hook's limit is wider, a run with no limit marks at once.
func TestPauseTellLate(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	c.savePause(json.RawMessage(testPause))
	setAge(t, 1900*time.Millisecond)
	late := hookLateAfter(hookNudgeTimeout)
	for n := 1; n <= lateTellsMax; n++ {
		if got := c.pauseTell("s1", late); got == "" {
			t.Fatalf("late run %d did not tell", n)
		}
		_, told := c.readPause().Told["s1"]
		if told != (n == lateTellsMax) {
			t.Errorf("after late run %d: marked = %v", n, told)
		}
	}
	if got := c.pauseTell("s1", late); got != "" {
		t.Errorf("told after the mark: %q", got)
	}
	c.savePause(json.RawMessage(`{"id":4,"spec":"any","note":"n","created_at":"2026-11-05T09:00:00Z"}`))
	if got := c.pauseTell("s2", 0); got == "" {
		t.Error("a hook with no limit did not tell")
	}
	if _, told := c.readPause().Told["s2"]; !told {
		t.Error("a run with no limit did not mark")
	}
	setAge(t, 10*time.Millisecond)
	if got := c.pauseTell("s3", late); got == "" {
		t.Error("an in-time run did not tell")
	}
	if _, told := c.readPause().Told["s3"]; !told {
		t.Error("an in-time run did not mark")
	}
	if got := c.pauseTell("s3", late); got != "" {
		t.Errorf("told twice: %q", got)
	}
}

// A new pause forgets the late counts of the old one.
func TestPauseTellLateCountResets(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	c.savePause(json.RawMessage(testPause))
	setAge(t, 1900*time.Millisecond)
	late := hookLateAfter(hookNudgeTimeout)
	c.pauseTell("s1", late)
	c.pauseTell("s1", late)
	c.savePause(json.RawMessage(`{"id":9,"spec":"any","note":"n","created_at":"2026-11-05T09:00:00Z"}`))
	if n := len(c.readPause().LateTold); n != 0 {
		t.Errorf("late counts survived a new pause: %d", n)
	}
}

// A held pause lock must not carry the nudge past the late point, nor mark.
func TestPauseTellBoundedLock(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	c.savePause(json.RawMessage(testPause))
	if err := os.WriteFile(c.pausePath()+".lock", []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	setAge(t, 1400*time.Millisecond)
	start := time.Now()
	if got := c.pauseTell("s1", hookLateAfter(hookNudgeTimeout)); got != "" {
		t.Errorf("told without the lock: %q", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("waited %v on the lock", d)
	}
	p := c.readPause()
	if _, told := p.Told["s1"]; told || len(p.LateTold) != 0 {
		t.Errorf("marked without the lock: %+v", p)
	}
}

func TestLockFileWithinZeroFailsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lockFileWithin(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := lockFileWithin(path, 0); err == nil {
		t.Error("took a held lock")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Error("a zero wait waited")
	}
	unlock()
	if u, err := lockFileWithin(path, 0); err != nil {
		t.Errorf("free lock refused: %v", err)
	} else {
		u()
	}
}

// If /proc (or the OS's own source) fails, the age is time since init.
func TestAgeFallback(t *testing.T) {
	old, oldStart := procAgeFunc, startedAt
	t.Cleanup(func() { procAgeFunc, startedAt = old, oldStart })
	procAgeFunc = func() (time.Duration, bool) { return 0, false }
	startedAt = time.Now().Add(-3 * time.Second)
	if d := osProcessAge(); d < 3*time.Second || d > 10*time.Second {
		t.Errorf("fallback age %v", d)
	}
	procAgeFunc = func() (time.Duration, bool) { return 7 * time.Second, true }
	if d := osProcessAge(); d != 7*time.Second {
		t.Errorf("age %v, want the OS's 7s", d)
	}
}

// With nothing to tell, the nudge path must not wait on the pause lock.
func TestPauseTellNeedsNoLockWhenNothingToTell(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	lock := c.pausePath() + ".lock"
	if err := os.WriteFile(lock, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if got := c.pauseTell("s1", 0); got != "" {
		t.Errorf("told with no pause: %q", got)
	}
	var sp sessionPause
	if err := json.Unmarshal([]byte(testPause), &sp); err != nil {
		t.Fatal(err)
	}
	c.writePause(pauseCopy{Pause: &sp, Checked: time.Now(), Told: map[string]time.Time{"s1": time.Now()}})
	if got := c.pauseTell("s1", 0); got != "" {
		t.Errorf("told twice: %q", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("waited %v on the lock", d)
	}
	if b, _ := os.ReadFile(lock); string(b) != "held" {
		t.Errorf("lock file touched: %q", b)
	}
}

// SessionStart has its own limit (10 s): a run 8 s in is late and tells
// without marking; a run 2 s in marks at once.
func TestPauseTellSessionStartLate(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	c.savePause(json.RawMessage(testPause))
	limit := hookLateAfter(hookSessionStartTimeout)
	setAge(t, 8*time.Second)
	if got := c.pauseTell("s1", limit); got == "" {
		t.Fatal("a late SessionStart did not tell")
	}
	if _, told := c.readPause().Told["s1"]; told {
		t.Error("a late SessionStart marked the pause told")
	}
	setAge(t, 2*time.Second)
	if got := c.pauseTell("s1", limit); got == "" {
		t.Fatal("an in-time SessionStart did not tell")
	}
	if _, told := c.readPause().Told["s1"]; !told {
		t.Error("an in-time SessionStart did not mark")
	}
}

// holdStateLock makes a fresh state.lock in dir, as a held lock looks.
func holdStateLock(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "state.lock"), []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The counter write after a hook's output must not carry the run past its
// timeout: with state.lock held the deny and the note are still out in time.
func TestCounterAfterOutputBounded(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GUARD", "")
	t.Setenv("HOME", t.TempDir())
	sleepPoll := map[string]any{"command": "until gh pr checks 12 | grep -q pass; do sleep 20; done"}
	for _, tc := range []struct {
		name string
		h    func(*cli, io.Reader, io.Writer)
		in   map[string]any
		want string
	}{
		{"guard", hookClaudeGuard, map[string]any{"command": pgrepLoop}, `"permissionDecision":"deny"`},
		{"nudge", hookClaudeNudge, sleepPoll, "additionalContext"},
	} {
		late := 1800 * time.Millisecond
		if tc.name == "guard" {
			late = 8 * time.Second
		}
		for _, age := range []time.Duration{0, late} {
			t.Run(tc.name+"/"+age.String(), func(t *testing.T) {
				setAge(t, age)
				c := &cli{dir: t.TempDir()}
				holdStateLock(t, c.dir)
				limit := 1500 * time.Millisecond
				if age > 0 {
					limit = 300 * time.Millisecond
				}
				start := time.Now()
				out := runHook(tc.h, c, t.TempDir(), tc.in)
				if d := time.Since(start); d > limit {
					t.Errorf("returned after %v, want under %v", d, limit)
				}
				if !strings.Contains(out, tc.want) {
					t.Errorf("output %q, want %q", out, tc.want)
				}
			})
		}
	}
}

func TestHookCountWait(t *testing.T) {
	setAge(t, 0)
	if got := hookCountWait(hookNudgeTimeout); got != 500*time.Millisecond {
		t.Errorf("fresh nudge = %v, want 500ms", got)
	}
	setAge(t, 1300*time.Millisecond)
	if got := hookCountWait(hookNudgeTimeout); got != 200*time.Millisecond {
		t.Errorf("1.3 s in = %v, want 200ms", got)
	}
	setAge(t, 1800*time.Millisecond)
	if got := hookCountWait(hookNudgeTimeout); got != 0 {
		t.Errorf("late = %v, want 0", got)
	}
}

// The lock wait is the time left before the late point and, past it, the
// floor. TestChainNudgeLateFloor widens the floor, so its shipped value is
// held here: two of lockFileWithin's 25 ms sleeps, which is three tries.
func TestHookLockWait(t *testing.T) {
	late := hookLateAfter(hookNudgeTimeout)
	setAge(t, 0)
	if got := hookLockWait(late); got != late {
		t.Errorf("fresh = %v, want %v", got, late)
	}
	setAge(t, 1800*time.Millisecond)
	if got := hookLockWait(late); got != 50*time.Millisecond {
		t.Errorf("late = %v, want the 50ms floor", got)
	}
}

// A chain lock held for a moment past the late point costs a short wait, not
// the count.
func TestChainNudgeLateFloor(t *testing.T) {
	repo := lateRepo(t)
	c := &cli{dir: t.TempDir()}
	path := c.chainPath("brief", "")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	setAge(t, 1800*time.Millisecond)
	// Widen the floor so a slow runner delaying the release can't end the wait
	// first; the lock is still held when chainNudge starts, and the wait ends
	// when it is removed.
	old := hookLockFloor
	hookLockFloor = 5 * time.Second
	t.Cleanup(func() { hookLockFloor = old })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		_ = os.Remove(path + ".lock")
	}()
	c.chainNudge(&hookEventInput{SessionID: "brief"}, repo, stepFind, false)
	wg.Wait()
	st := &chainState{}
	if err := readJSON(path, st); err != nil || st.Finds != 1 {
		t.Errorf("chain = %+v, err %v: the late step was not counted", st, err)
	}
}
