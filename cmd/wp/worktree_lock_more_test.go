package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A lock written before this host last booted (or whose time can't be read)
// is never judged by this host's /proc, even with a pid that isn't running.
func TestUnlockStaleLockTime(t *testing.T) {
	f := newTidyFixture(t)
	ctx := context.Background()
	p := lockedWorktree(t, f, "wp-1", "worktree-wp-1", deadReason(t))
	common := filepath.Join(f.repo, ".git")
	if at := lockedAt(common, p); at.IsZero() || time.Since(at) > time.Minute {
		t.Fatalf("lockedAt: %v", at)
	}
	if !lockedAt(common, filepath.Join(f.repo, "nowhere")).IsZero() {
		t.Error("lockedAt answered for a path that isn't a worktree")
	}
	// Written "in 1970": an earlier boot, or another host.
	var lockFile string
	ents, _ := os.ReadDir(filepath.Join(common, "worktrees"))
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join(common, "worktrees", e.Name(), "locked")); err == nil {
			lockFile = filepath.Join(common, "worktrees", e.Name(), "locked")
		}
	}
	if err := os.Chtimes(lockFile, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := unlockStale(ctx, f.repo, p); err == nil || !strings.Contains(err.Error(), "before this host last booted") || !isLocked(t, f, p) {
		t.Errorf("unlocked a lock from before the boot: %v", err)
	}
	if gone, _ := lockOwner(deadReason(t), time.Time{}); gone {
		t.Error("a lock with no time was judged")
	}
	_, held := reclaimTask(ctx, mustRepo(t, f), "WP-1", f.repo)
	if held.blocked == nil || len(held.taken) != 0 {
		t.Errorf("reclaimTask took a lock from before the boot: %+v", held)
	}
}

func mustRepo(t *testing.T, f *tidyFixture) *worktreeRepo {
	t.Helper()
	r, err := worktreeRepoAt(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// makeWorktree (wp worktree) follows the same rule as wp next: a branch held
// by a lock whose owner is gone is taken over and reused; one held by a live
// owner is continued on a branch cut from it; a folder locked by a live owner
// isn't reused.
func TestMakeWorktreeLocks(t *testing.T) {
	ctx := context.Background()
	note := func(string, ...any) {}
	t.Run("branch held, owner gone", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "elsewhere", "wp-5", deadReason(t))
		m, err := makeWorktree(ctx, mustRepo(t, f), "wp-5", "wp-5", "", false, note)
		if err != nil || !m.branchOut || tidyReal(m.path) != tidyReal(p) || len(m.took) != 1 || m.cont != nil || isLocked(t, f, p) {
			t.Errorf("%+v %v locked=%v", m, err, isLocked(t, f, p))
		}
	})
	t.Run("branch held, owner alive", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "elsewhere", "wp-5", liveReason(t))
		tcommit(t, p, "earlier.txt")
		m, err := makeWorktree(ctx, mustRepo(t, f), "wp-5", "wp-5", "", false, note)
		if err != nil || m.cont == nil || m.cont.branch != "wp-5-2" || m.cont.from != "wp-5" || !strings.Contains(m.made, "wp-5-2") || m.branchOut {
			t.Fatalf("%+v %v", m, err)
		}
		if !isLocked(t, f, p) || tgit(t, f.repo, "rev-parse", "wp-5-2") != tgit(t, f.repo, "rev-parse", "wp-5") {
			t.Error("took the live lock, or the branch isn't cut from the held one")
		}
		if !strings.Contains(m.cont.explain(), "use wp-5-2 for the pull request") || !strings.Contains(m.cont.explain(), "maps to WP-5") {
			t.Errorf("explain: %s", m.cont.explain())
		}
	})
	t.Run("branch held, lock unreadable", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "elsewhere", "wp-5", "someone's note")
		m, err := makeWorktree(ctx, mustRepo(t, f), "wp-5", "wp-5", "", false, note)
		if err != nil || m.cont == nil || !isLocked(t, f, p) {
			t.Errorf("%+v %v", m, err)
		}
	})
	t.Run("folder locked by a live owner", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "wp-6", "worktree-wp-6", liveReason(t))
		m, err := makeWorktree(ctx, mustRepo(t, f), "wp-6", "", "", false, note)
		if err != nil || tidyReal(m.path) == tidyReal(p) || m.made == "" || !strings.HasPrefix(filepath.Base(m.path), "wp-6-") {
			t.Errorf("%+v %v", m, err)
		}
		if !isLocked(t, f, p) {
			t.Error("took the live lock")
		}
	})
	t.Run("folder locked, owner gone", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "wp-6", "worktree-wp-6", deadReason(t))
		m, err := makeWorktree(ctx, mustRepo(t, f), "wp-6", "", "", false, note)
		if err != nil || tidyReal(m.path) != tidyReal(p) || len(m.took) != 1 || isLocked(t, f, p) {
			t.Errorf("%+v %v", m, err)
		}
	})
	t.Run("branch held, folder gone, owner gone", func(t *testing.T) {
		f := newTidyFixture(t)
		p := lockedWorktree(t, f, "elsewhere", "wp-5", deadReason(t))
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
		m, err := makeWorktree(ctx, mustRepo(t, f), "wp-5", "wp-5", "", false, note)
		if err != nil || m.cont != nil || m.made == "" || !dirExists(m.path) {
			t.Errorf("%+v %v", m, err)
		}
	})
	t.Run("main checkout holds the branch: as before", func(t *testing.T) {
		f := newTidyFixture(t)
		m, err := makeWorktree(ctx, mustRepo(t, f), "x", "main", "", false, note)
		if err != nil || !m.branchOut || tidyReal(m.path) != tidyReal(f.repo) || m.cont != nil {
			t.Errorf("%+v %v", m, err)
		}
	})
}

// worktreeName passes a folder whose lock isn't shown gone to the next name.
func TestWorktreeNameLocked(t *testing.T) {
	f := newTidyFixture(t)
	ctx := context.Background()
	lockedWorktree(t, f, "app-1", "worktree-app-1", liveReason(t))
	if got := worktreeName(ctx, f.repo, "APP-1"); got != "app-1-2" {
		t.Errorf("live lock: %s", got)
	}
	lockedWorktree(t, f, "app-2", "worktree-app-2", "a note")
	if got := worktreeName(ctx, f.repo, "APP-2"); got != "app-2-2" {
		t.Errorf("unreadable lock: %s", got)
	}
	lockedWorktree(t, f, "app-3", "worktree-app-3", deadReason(t))
	if got := worktreeName(ctx, f.repo, "APP-3"); got != "app-3" {
		t.Errorf("stale lock: %s", got)
	}
	tgit(t, f.repo, "worktree", "add", "-q", "-b", "worktree-app-4", filepath.Join(f.repo, ".claude", "worktrees", "app-4"), "main")
	if got := worktreeName(ctx, f.repo, "APP-4"); got != "app-4" {
		t.Errorf("unlocked: %s", got)
	}
}

// Before autopilot starts a session, the task's stale-locked worktree is
// unlocked; a live one is left and named.
func TestFreeStaleLocks(t *testing.T) {
	f := newTidyFixture(t)
	ctx := context.Background()
	stale := lockedWorktree(t, f, "app-1", "worktree-app-1", deadReason(t))
	live := lockedWorktree(t, f, "app-2", "wp-app-2", liveReason(t))
	var said []string
	say := func(format string, a ...any) { said = append(said, fmt.Sprintf(format, a...)) }
	freeStaleLocks(ctx, f.repo, "APP-1", say)
	if isLocked(t, f, stale) || len(said) != 1 || !strings.Contains(said[0], "took over "+stale) {
		t.Errorf("stale: locked=%v said=%q", isLocked(t, f, stale), said)
	}
	said = nil
	freeStaleLocks(ctx, f.repo, "APP-2", say)
	if !isLocked(t, f, live) || len(said) != 1 || !strings.Contains(said[0], "wp-app-2 stays in "+live) || !strings.Contains(said[0], "still running") {
		t.Errorf("live: locked=%v said=%q", isLocked(t, f, live), said)
	}
}

// A tidy row for a worktree whose folder is gone names its branch, so
// wp tidy --explain BRANCH finds it, locked or not.
func TestTidyMissingFolderRowsNameBranch(t *testing.T) {
	f := newTidyFixture(t)
	ctx := context.Background()
	f.merge(t, "gone-free")
	free := filepath.Join(filepath.Dir(f.repo), "gone-free")
	tgit(t, f.repo, "worktree", "add", "-q", free, "gone-free")
	f.merge(t, "gone-locked")
	locked := filepath.Join(filepath.Dir(f.repo), "gone-locked")
	tgit(t, f.repo, "worktree", "add", "-q", locked, "gone-locked")
	tgit(t, f.repo, "worktree", "lock", "--reason", deadReason(t), locked)
	for _, p := range []string{free, locked} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := tidyScan(ctx, f.repo, nil, tidyOpts{worktrees: true, branches: true, now: later()})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gone-free", "gone-locked"} {
		found := false
		for _, r := range rep.Rows {
			if r.Kind == "worktree" && r.Branch == name && r.prunable {
				found = true
			}
		}
		if !found {
			t.Errorf("no worktree row with Branch %q: %+v", name, rep.Rows)
		}
	}
	// Applying prunes the records and never touches the branches (judged as branches).
	tidyApply(ctx, f.repo, rep, tidyOpts{worktrees: true, branches: true, now: later()})
	if out := tgit(t, f.repo, "worktree", "list", "--porcelain"); strings.Contains(out, "gone-") {
		t.Errorf("records left:\n%s", out)
	}
}

// Two agents start the same task: the second's worktree add loses to the
// first's, and it works in the one the first made instead of "working here".
func TestNextLostRace(t *testing.T) {
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
	oldFolder, oldCwds, oldHook := sessionFolder, procCwds, worktreeAddHook
	sessionFolder = func() string { return repo }
	procCwds = func() ([]string, error) { return []string{repo}, nil }
	t.Cleanup(func() { sessionFolder, procCwds, worktreeAddHook = oldFolder, oldCwds, oldHook })
	p := s.project.Key
	i, err := s.svc.CreateIssue(context.Background(), s.me, core.IssueInput{Project: p, Title: "Race", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	key := i.Key
	wt := filepath.Join(repo, ".claude", "worktrees", worktreeSlug(key))
	worktreeAddHook = func() { // the other agent got there first
		worktreeAddHook = func() {}
		git(t, s.repo, "worktree", "add", "-q", "-b", "worktree-"+worktreeSlug(key), wt, "origin/main")
	}
	out := s.wp("next", key, "-p", p)
	if strings.Contains(out, "failed") || !strings.Contains(out, key+" already has one: "+wt) || !strings.Contains(out, "Work there: cd "+wt) {
		t.Errorf("lost race:\n%s", out)
	}
}
