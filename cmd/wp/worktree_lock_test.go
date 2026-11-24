package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// deadReason is the lock reason of an agent that has exited: a real child
// that ran to the end, with a start time that is not its own.
func deadReason(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=NoSuchTest")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("claude agent agent-dead (pid %d start 1)", cmd.ProcessState.Pid())
}

// liveReason is the lock reason of an agent that runs: this test process, as
// /proc records it.
func liveReason(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	e, found, err := readLockProc(os.Getpid())
	if err != nil || !found {
		t.Fatalf("own process: %v %v", found, err)
	}
	return fmt.Sprintf("claude agent agent-live (pid %d start %d)", os.Getpid(), e.start)
}

func TestLockOwner(t *testing.T) {
	old := lockProcLookup
	t.Cleanup(func() { lockProcLookup = old })
	now := time.Now()
	oldClock := lockClock
	lockClock = func() (time.Time, time.Duration, error) { return now, 1000 * time.Second, nil }
	t.Cleanup(func() { lockClock = oldClock })
	boot := now.Add(-1000 * time.Second)
	procs := map[int]lockProc{10: {start: 500}, 11: {start: 500, zombie: true}, 13: {start: 90000}}
	lockProcLookup = func(pid int) (lockProc, bool, error) {
		if pid == 99 {
			return lockProc{}, false, fmt.Errorf("no /proc")
		}
		e, ok := procs[pid]
		return e, ok, nil
	}
	for name, c := range map[string]struct {
		reason string
		at     time.Time
		gone   bool
		why    string
	}{
		"no reason":                          {"", time.Time{}, false, "records no owner"},
		"free text":                          {"WP-999 rework in progress (session waypoint-c5)", time.Time{}, false, "names no process"},
		"pid not running":                    {"claude agent a (pid 12 start 500)", time.Time{}, true, "no longer runs"},
		"claude --worktree's form":           {"claude session lk (pid 12 start 500)", time.Time{}, true, "no longer runs"},
		"zombie":                             {"claude agent a (pid 11 start 500)", time.Time{}, true, "has exited"},
		"pid reused":                         {"claude agent a (pid 10 start 400)", time.Time{}, true, "another process"},
		"same process, running":              {"claude agent a (pid 10 start 500)", time.Time{}, false, "still running"},
		"no view of /proc":                   {"claude agent a (pid 99 start 500)", time.Time{}, false, "can't tell"},
		"pid zero":                           {"claude agent a (pid 0 start 500)", time.Time{}, false, "names no process"},
		"age says nothing":                   {"claude agent a (pid 10 start 500) from last year", time.Time{}, false, "names no process"},
		"trailing newline, alive":            {"claude agent a (pid 10 start 500)\n", time.Time{}, false, "still running"},
		"start later than uptime":            {"claude agent a (pid 12 start 200000)", time.Time{}, false, "later than this host has been up"},
		"written before this boot":           {"claude agent a (pid 12 start 500)", boot.Add(-time.Hour), false, "before this host last booted"},
		"written before the process started": {"claude agent a (pid 13 start 90000)", boot.Add(100 * time.Second), false, "before that process started"},
		"written after it started, gone":     {"claude agent a (pid 12 start 90000)", boot.Add(901 * time.Second), true, "no longer runs"},
		"lock time unknown":                  {"claude agent a (pid 12 start 500)", time.Unix(1, 0).Add(-1), false, "before this host last booted"},
	} {
		at := c.at
		if at.IsZero() {
			at = now
		}
		gone, why := lockOwner(c.reason, at)
		if gone != c.gone || !strings.Contains(why, c.why) {
			t.Errorf("%s: gone=%v (%s), want %v (%s)", name, gone, why, c.gone, c.why)
		}
	}
}

// The real /proc: a child that ran to the end is gone, this process is not,
// and a pid with another start time is a different process.
func TestLockOwnerReal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	if gone, why := lockOwner(deadReason(t), time.Now()); !gone {
		t.Errorf("exited child not gone: %s", why)
	}
	if gone, why := lockOwner(liveReason(t), time.Now()); gone {
		t.Errorf("this process gone: %s", why)
	}
	if gone, why := lockOwner(fmt.Sprintf("claude agent a (pid %d start 1)", os.Getpid()), time.Now()); !gone || !strings.Contains(why, "another process") {
		t.Errorf("reused pid not gone: %v %s", gone, why)
	}
}

func lockedWorktree(t *testing.T, f *tidyFixture, name, branch, reason string) string {
	t.Helper()
	p := filepath.Join(f.repo, ".claude", "worktrees", name)
	tgit(t, f.repo, "worktree", "add", "-q", "-b", branch, p, "main")
	args := []string{"worktree", "lock"}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	tgit(t, f.repo, append(args, p)...)
	return p
}

func isLocked(t *testing.T, f *tidyFixture, p string) bool {
	t.Helper()
	repo, err := worktreeRepoAt(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	w := repo.find(p)
	return w != nil && w.locked
}

// unlockStale unlocks only a lock whose owner is gone, and two at once both
// end with it unlocked (the second finds nothing to do).
func TestUnlockStale(t *testing.T) {
	f := newTidyFixture(t)
	ctx := context.Background()
	dead := lockedWorktree(t, f, "wp-1", "worktree-wp-1", deadReason(t))
	live := lockedWorktree(t, f, "wp-2", "worktree-wp-2", liveReason(t))
	text := lockedWorktree(t, f, "wp-3", "worktree-wp-3", "WP-999 rework in progress")
	bare := lockedWorktree(t, f, "wp-4", "worktree-wp-4", "")
	for _, p := range []string{live, text, bare} {
		if _, err := unlockStale(ctx, f.repo, p); err == nil || !isLocked(t, f, p) {
			t.Errorf("%s: unlocked a lock whose owner isn't shown gone (err %v)", filepath.Base(p), err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = unlockStale(ctx, f.repo, dead)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d: %v", i, err)
		}
	}
	if isLocked(t, f, dead) {
		t.Error("still locked after the race")
	}
	if _, err := unlockStale(ctx, f.repo, dead); err != nil {
		t.Errorf("already unlocked: %v", err)
	}
}

// reclaimTask: stale locks go; live ones, unreadable ones and the main
// checkout block; a stale lock on a worktree whose folder is gone frees its
// branch; one record pruning would drop with another's is not pruned.
func TestReclaimTask(t *testing.T) {
	ctx := context.Background()
	reclaim := func(f *tidyFixture, key string) (lockedFor, *worktreeRepo) {
		repo, err := worktreeRepoAt(ctx, f.repo)
		if err != nil {
			t.Fatal(err)
		}
		repo, held := reclaimTask(ctx, repo, key, f.repo)
		return held, repo
	}
	t.Run("stale", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "wp-1", "worktree-wp-1", deadReason(t))
		held, _ := reclaim(f, "WP-1")
		if len(held.taken) != 1 || held.blocked != nil || isLocked(t, f, p) {
			t.Errorf("%+v locked=%v", held, isLocked(t, f, p))
		}
	})
	t.Run("alive", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "wp-1", "worktree-wp-1", liveReason(t))
		held, _ := reclaim(f, "WP-1")
		if held.blocked == nil || !strings.Contains(held.why, "still running") || !isLocked(t, f, p) {
			t.Errorf("%+v", held)
		}
	})
	t.Run("another task's lock is none of its business", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "wp-2", "worktree-wp-2", deadReason(t))
		held, _ := reclaim(f, "WP-1")
		if held.blocked != nil || len(held.taken) != 0 || !isLocked(t, f, p) {
			t.Errorf("%+v", held)
		}
	})
	t.Run("main checkout holds the branch", func(t *testing.T) {
		f := newTidyFixture(t)
		tgit(t, f.repo, "checkout", "-q", "-b", "wp-1")
		other := filepath.Join(filepath.Dir(f.repo), "elsewhere")
		tgit(t, f.repo, "worktree", "add", "-q", "--detach", other, "main")
		repo, err := worktreeRepoAt(ctx, f.repo)
		if err != nil {
			t.Fatal(err)
		}
		_, held := reclaimTask(ctx, repo, "WP-1", other)
		if held.blocked == nil || !held.blocked.main || !strings.Contains(held.why, "main checkout") {
			t.Errorf("%+v", held)
		}
		if _, held := reclaimTask(ctx, repo, "WP-1", f.repo); held.blocked != nil {
			t.Errorf("in the main checkout: %+v", held)
		}
	})
	t.Run("folder gone, lock stale", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "wp-1", "wp-1", deadReason(t))
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
		held, _ := reclaim(f, "WP-1")
		if held.revive != "wp-1" || held.blocked != nil {
			t.Fatalf("%+v", held)
		}
		tgit(t, f.repo, "worktree", "add", "-q", p, "wp-1")
	})
	t.Run("folder gone, lock alive", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "wp-1", "wp-1", liveReason(t))
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
		held, _ := reclaim(f, "WP-1")
		if held.blocked == nil || held.revive != "" || !isLocked(t, f, p) {
			t.Errorf("%+v", held)
		}
	})
	t.Run("folder gone, but another missing worktree outside the repo's own folder", func(t *testing.T) {
		f := newTidyFixture(t)
		away := filepath.Join(t.TempDir(), "away")
		tgit(t, f.repo, "worktree", "add", "-q", "-b", "away", away, "main")
		if err := os.RemoveAll(away); err != nil {
			t.Fatal(err)
		}
		p := lockedWorktree(t, f, "wp-1", "wp-1", deadReason(t))
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
		held, repo := reclaim(f, "WP-1")
		if held.revive != "" || held.blocked == nil || !strings.Contains(held.why, "another missing") {
			t.Errorf("%+v", held)
		}
		if repo.find(away) == nil {
			t.Error("pruned the record of a folder that may only be unmounted")
		}
	})
}

// wp tidy: a merged, clean worktree whose lock's owner is gone goes like an
// unlocked one; every other locked one stays, and a lock is put back when the
// removal fails.
func TestTidyStaleLock(t *testing.T) {
	f := newTidyFixture(t)
	ctx := context.Background()
	mk := func(name, reason string) string {
		f.merge(t, name)
		p := filepath.Join(filepath.Dir(f.repo), name)
		tgit(t, f.repo, "worktree", "add", "-q", p, name)
		args := []string{"worktree", "lock"}
		if reason != "" {
			args = append(args, "--reason", reason)
		}
		tgit(t, f.repo, append(args, p)...)
		return p
	}
	stale := mk("stale", deadReason(t))
	live := mk("live", liveReason(t))
	text := mk("text", "WP-999 rework in progress")
	plain := mk("plain", "")
	dirty := mk("dirty", deadReason(t))
	if err := os.WriteFile(filepath.Join(dirty, "wip.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.merge(t, "gone")
	gone := filepath.Join(filepath.Dir(f.repo), "gone")
	tgit(t, f.repo, "worktree", "add", "-q", gone, "gone")
	tgit(t, f.repo, "worktree", "lock", "--reason", deadReason(t), gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	o := tidyOpts{worktrees: true, branches: true, now: later()}
	rep, err := tidyScan(ctx, f.repo, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	wantVerdict(t, rep.row(t, "worktree", "stale"), tidyRemovable, "merged")
	wantVerdict(t, rep.row(t, "worktree", "live"), tidyKeep, "locked")
	wantVerdict(t, rep.row(t, "worktree", "text"), tidyKeep, "locked")
	wantVerdict(t, rep.row(t, "worktree", "plain"), tidyKeep, "locked")
	wantVerdict(t, rep.row(t, "worktree", "dirty"), tidyKeep, "1 untracked file")
	wantVerdict(t, rep.row(t, "worktree", "gone"), tidyRemovable, "directory is gone")
	tidyApply(ctx, f.repo, rep, o)
	for p, want := range map[string]bool{stale: false, gone: false, live: true, text: true, plain: true, dirty: true} {
		if got := dirExists(p); got != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(p), got, want)
		}
	}
	repo, err := worktreeRepoAt(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if repo.find(gone) != nil || repo.find(stale) != nil {
		t.Errorf("registered still: %+v", repo.wts)
	}
	if w := repo.find(dirty); w == nil || !w.locked || !strings.Contains(w.lockReason, "pid") {
		t.Errorf("dirty one lost its lock: %+v", w)
	}
}

// The end to end of the report: an agent taking WP-N finds the task's worktree
// locked by an agent that is gone, and works on the task's own branch.
func TestNextLockedWorktree(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_NO_BRIEF", "1")
	t.Setenv("WP_NO_GIT", "")
	t.Setenv(noWorktreeEnv, "")
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, s.repo, "init", "-q", "--bare", "-b", "main", origin)
	commitFiles(t, s.repo, map[string]string{"README.md": "# App\n"})
	git(t, s.repo, "remote", "set-url", "origin", origin)
	git(t, s.repo, "push", "-q", "origin", "HEAD:main")
	git(t, s.repo, "fetch", "-q", "origin")
	git(t, s.repo, "remote", "set-head", "origin", "main")
	repo := tidyReal(s.repo)
	home := filepath.Join(repo, ".claude", "worktrees")
	oldFolder, oldCwds := sessionFolder, procCwds
	sessionFolder = func() string { return repo }
	procCwds = func() ([]string, error) { return []string{repo}, nil }
	t.Cleanup(func() { sessionFolder, procCwds = oldFolder, oldCwds })
	p := s.project.Key
	mk := func(title string) string {
		i, err := s.svc.CreateIssue(context.Background(), s.me, core.IssueInput{Project: p, Title: title, Agent: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		return i.Key
	}
	lock := func(wt, reason string) { git(t, s.repo, "worktree", "lock", "--reason", reason, wt) }
	earlier := func(key, branch, reason string) string {
		wt := filepath.Join(home, worktreeSlug(key))
		git(t, s.repo, "worktree", "add", "-q", "-b", branch, wt, "origin/main")
		commitFiles(t, wt, map[string]string{"earlier.txt": "work"})
		lock(wt, reason)
		return wt
	}
	listed := func(wt string) *tidyWorktree {
		r, err := worktreeRepoAt(context.Background(), s.repo)
		if err != nil {
			t.Fatal(err)
		}
		return r.find(wt)
	}

	k1 := mk("Stale and clean")
	wt1 := earlier(k1, "worktree-"+worktreeSlug(k1), deadReason(t))
	out := s.wp("next", k1, "-p", p)
	for _, want := range []string{"Took over " + wt1, "agent that locked it is gone", k1 + " already has one: " + wt1, "Work there: cd " + wt1} {
		if !strings.Contains(out, want) {
			t.Errorf("stale, clean: lacks %q:\n%s", want, out)
		}
	}
	if w := listed(wt1); w == nil || w.locked {
		t.Errorf("still locked: %+v", w)
	}

	k2 := mk("Stale and dirty")
	wt2 := earlier(k2, "wp-"+strings.ToLower(k2), deadReason(t))
	if err := os.WriteFile(filepath.Join(wt2, "wip.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = s.wp("next", k2, "-p", p)
	if !strings.Contains(out, k2+" already has one: "+wt2) || !strings.Contains(out, "It holds 1 uncommitted file, 1 commit not on origin/main: the task's own work, left in place.") {
		t.Errorf("stale, dirty:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(wt2, "wip.txt")); err != nil {
		t.Errorf("work gone: %v", err)
	}

	// Cut branches carry the task key in their name, which is how Waypoint maps a branch to its task.
	k3 := mk("Alive")
	b3 := "wp-" + strings.ToLower(k3)
	wt3 := earlier(k3, b3, liveReason(t))
	out = s.wp("next", k3, "-p", p)
	for _, want := range []string{b3 + " (" + k3 + "'s earlier work) is checked out in " + wt3, "still running", "continues from " + b3 + "'s last commit on " + b3 + "-2", "use " + b3 + "-2 for the pull request"} {
		if !strings.Contains(out, want) {
			t.Errorf("alive: lacks %q:\n%s", want, out)
		}
	}
	if w := listed(wt3); w == nil || !w.locked {
		t.Errorf("took a live agent's worktree: %+v", w)
	}
	if got := git(t, s.repo, "rev-parse", b3+"-2"); got != git(t, s.repo, "rev-parse", b3) {
		t.Errorf("%s-2 isn't cut from %s", b3, b3)
	}
	if m := branchKeyRE.FindStringSubmatch(b3 + "-2"); m == nil || !strings.EqualFold(m[1], k3) {
		t.Errorf("%s-2 doesn't map to %s: %v", b3, k3, m)
	}

	k4 := mk("Unknown owner")
	b4 := "wp-" + strings.ToLower(k4)
	wt4 := earlier(k4, b4, "WP-999 rework in progress (session waypoint-c5)")
	out = s.wp("next", k4, "-p", p)
	if !strings.Contains(out, "names no process wp can check") || !strings.Contains(out, "on "+b4+"-2") {
		t.Errorf("unknown owner:\n%s", out)
	}
	if w := listed(wt4); w == nil || !w.locked {
		t.Errorf("took a lock it couldn't judge: %+v", w)
	}

	k5 := mk("Folder gone")
	b5 := "wp-" + strings.ToLower(k5)
	wt5 := earlier(k5, b5, deadReason(t))
	if err := os.RemoveAll(wt5); err != nil {
		t.Fatal(err)
	}
	out = s.wp("next", k5, "-p", p)
	if !strings.Contains(out, "Took over "+wt5) || !strings.Contains(out, "on branch "+b5) {
		t.Errorf("folder gone:\n%s", out)
	}
	if got := git(t, wt5, "rev-parse", "--abbrev-ref", "HEAD"); got != b5 {
		t.Errorf("revived on %s, not %s", got, b5)
	}
	if !fileExists(filepath.Join(wt5, "earlier.txt")) {
		t.Error("the branch's commit is missing")
	}

}
