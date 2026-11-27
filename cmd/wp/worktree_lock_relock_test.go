package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stale lock is lifted only to remove the worktree: when the removal then
// fails (work appeared in it after the scan), the lock goes back as it was.
func TestTidyRelockWhenRemovalFails(t *testing.T) {
	f := newTidyFixture(t)
	ctx := context.Background()
	f.merge(t, "done")
	p := filepath.Join(filepath.Dir(f.repo), "done")
	tgit(t, f.repo, "worktree", "add", "-q", p, "done")
	reason := deadReason(t)
	tgit(t, f.repo, "worktree", "lock", "--reason", reason, p)
	o := tidyOpts{worktrees: true, now: later()}
	rep, err := tidyScan(ctx, f.repo, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	wantVerdict(t, rep.row(t, "worktree", "done"), tidyRemovable, "merged")
	if err := os.WriteFile(filepath.Join(p, "late.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tidyApply(ctx, f.repo, rep, o)
	if r := rep.row(t, "worktree", "done"); !strings.HasPrefix(r.Action, "failed") {
		t.Errorf("action: %q", r.Action)
	}
	w := mustRepo(t, f).find(p)
	if w == nil || !w.locked || w.lockReason != reason {
		t.Errorf("lock not put back: %+v", w)
	}
	if _, err := os.Stat(filepath.Join(p, "late.txt")); err != nil {
		t.Errorf("work lost: %v", err)
	}
}
