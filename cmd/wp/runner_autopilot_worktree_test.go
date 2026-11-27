package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// As Claude Code 2.1.319 draws it.
const exitDialogKeep = "Exiting worktree session\nYou have 1 commit on worktree-wp-1. The branch will be deleted if you remove the worktree.\n\n❯ 1. Keep worktree    Stays at /r/.claude/worktrees/wp-1\n  2. Remove worktree  All changes and commits will be lost.\n\nEnter to confirm · Esc to cancel\n"

func TestKeepWorktreeDialog(t *testing.T) {
	for name, c := range map[string]struct {
		screen string
		want   bool
	}{
		"keep highlighted":   {exitDialogKeep, true},
		"no-break space":     {"Exiting worktree session\n❯\u00a01. Keep worktree\n", true},
		"with escapes":       {"\x1b[1mExiting worktree session\x1b[0m\n\x1b[36m❯ 1. Keep worktree\x1b[39m\n", true},
		"remove highlighted": {"Exiting worktree session\n  1. Keep worktree\n❯ 2. Remove worktree\n", false},
		"the input box":      {screenEmpty, false},
		"keep, no heading":   {"❯ 1. Keep worktree\n", false},
	} {
		if got := keepWorktreeDialog(c.screen); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestWorktreeName(t *testing.T) {
	f := newTidyFixture(t)
	ctx := context.Background()
	if got := worktreeName(ctx, f.repo, "APP-2"); got != "app-2" {
		t.Errorf("free: %s", got)
	}
	tgit(t, f.repo, "branch", autopilotWorktreeBranch("app-2"))
	if got := worktreeName(ctx, f.repo, "APP-2"); got != "app-2-2" {
		t.Errorf("leftover branch: %s", got)
	}
	if err := os.MkdirAll(filepath.Join(f.repo, autopilotWorktrees, "app-2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := worktreeName(ctx, f.repo, "APP-2"); got != "app-2" {
		t.Errorf("existing worktree: %s", got)
	}
}

func TestPlanWorktree(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	ctx := context.Background()
	f := newTidyFixture(t)
	p := newTestProc(t, filepath.Dir(f.repo))
	sub := filepath.Join(f.repo, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}

	rl := &relaunch{dir: sub, shell: "bash", shellDir: sub}
	if w := p.planWorktree(ctx, rl); w != "" || !rl.worktree || rl.root != f.repo || rl.dir != f.repo || rl.cdTo != f.repo || rl.ended != "" {
		t.Errorf("subfolder: %q %+v", w, rl)
	}
	rl = &relaunch{dir: sub, respawn: true}
	if w := p.planWorktree(ctx, rl); w != "" || !rl.worktree || rl.cdTo != "" {
		t.Errorf("respawn: %q %+v", w, rl)
	}
	wt := filepath.Join(f.repo, autopilotWorktrees, "app-1")
	tgit(t, f.repo, "worktree", "add", "-q", "-b", autopilotWorktreeBranch("app-1"), wt, "origin/main")
	rl = &relaunch{dir: wt, shell: "bash", shellDir: f.repo}
	if w := p.planWorktree(ctx, rl); w != "" || rl.ended != wt || rl.root != f.repo || rl.cdTo != "" {
		t.Errorf("from a worktree: %q %+v", w, rl)
	}
	tgit(t, f.repo, "branch", "mine", "origin/main")
	mine := f.worktree(t, "mine", "mine")
	rl = &relaunch{dir: mine, shell: "bash", shellDir: mine}
	if w := p.planWorktree(ctx, rl); w != "" || rl.ended != "" {
		t.Errorf("a person's worktree: %q %+v", w, rl)
	}

	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(`{"projects":{"/elsewhere":{"hasTrustDialogAccepted":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rl = &relaunch{dir: sub, shell: "bash", shellDir: sub}
	if w := p.planWorktree(ctx, rl); !strings.Contains(w, "trusted") || rl.worktree || rl.root != f.repo || rl.dir != sub {
		t.Errorf("untrusted: %q %+v", w, rl)
	}
	b := `{"projects":{"` + filepath.Dir(f.repo) + `":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
	rl = &relaunch{dir: sub, shell: "bash", shellDir: sub}
	if w := p.planWorktree(ctx, rl); w != "" || !rl.worktree {
		t.Errorf("trusted above: %q %+v", w, rl)
	}

	plain := t.TempDir()
	rl = &relaunch{dir: plain, shell: "bash"}
	if w := newTestProc(t, plain).planWorktree(ctx, rl); !strings.Contains(w, "git repository") || rl.worktree || rl.root != "" {
		t.Errorf("no repo: %q %+v", w, rl)
	}
}

func TestWorktreeSweep(t *testing.T) {
	type world struct {
		f  *tidyFixture
		p  *runnerProc
		rl *relaunch
		wt string
	}
	setup := func(t *testing.T) *world {
		old := procCwds
		procCwds = func() ([]string, error) { return nil, nil }
		t.Cleanup(func() { procCwds = old })
		f := newTidyFixture(t)
		wt := filepath.Join(f.repo, autopilotWorktrees, "app-1")
		tgit(t, f.repo, "worktree", "add", "-q", "-b", autopilotWorktreeBranch("app-1"), wt, "origin/main")
		return &world{f: f, p: newTestProc(t, filepath.Dir(f.repo)), wt: wt, rl: &relaunch{root: f.repo, ended: wt}}
	}
	sweep := func(w *world, held []autopilotHeld, spare string) []string {
		return w.p.sweepWorktrees(context.Background(), newRun(time.Minute), autopilotJob{Held: held}, w.rl, spare)
	}
	branch := autopilotWorktreeBranch("app-1")
	hasBranch := func(w *world) bool { return tgit(t, w.f.repo, "branch", "--list", branch) != "" }
	pushed := func(t *testing.T, w *world) {
		tcommit(t, w.wt, "work")
		tgit(t, w.wt, "push", "-q", "origin", branch)
	}
	stayed := func(t *testing.T, w *world, kept []string, want string) {
		t.Helper()
		if !dirExists(w.wt) {
			t.Fatal("the worktree was removed")
		}
		if len(kept) != 1 || !strings.Contains(kept[0], want) {
			t.Errorf("kept = %q, want %q", kept, want)
		}
		if again := sweep(w, nil, ""); len(again) != 0 && !strings.Contains(want, "held") {
			t.Errorf("reported twice: %q", again)
		}
	}

	t.Run("finished, clean and pushed: goes, its branch stays", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		if kept := sweep(w, nil, ""); len(kept) != 0 || dirExists(w.wt) || !hasBranch(w) {
			t.Errorf("kept %q, worktree there %v, branch there %v", kept, dirExists(w.wt), hasBranch(w))
		}
	})
	t.Run("finished and merged: goes with its branch", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		tgit(t, w.f.repo, "merge", "-q", "--no-ff", "-m", "merge", branch)
		tgit(t, w.f.repo, "push", "-q", "origin", "main")
		if kept := sweep(w, nil, ""); len(kept) != 0 || dirExists(w.wt) || hasBranch(w) {
			t.Errorf("kept %q, worktree there %v, branch there %v", kept, dirExists(w.wt), hasBranch(w))
		}
	})
	t.Run("unpushed commits of the ended session are pushed, then it goes (WP-843)", func(t *testing.T) {
		w := setup(t)
		t.Setenv("WP_NO_GIT", "")
		t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
		ps := &pushServer{tasks: []string{"APP-1"}}
		ps.start(t)
		head := tcommit(t, w.wt, "work")
		if kept := sweep(w, nil, ""); len(kept) != 0 || dirExists(w.wt) || !hasBranch(w) {
			t.Errorf("kept %q, worktree there %v, branch there %v", kept, dirExists(w.wt), hasBranch(w))
		}
		if got := tgit(t, w.f.origin, "rev-parse", "refs/heads/"+branch); got != head {
			t.Errorf("origin has %s, want %s", got, head)
		}
	})
	t.Run("unpushed commits stay, told once", func(t *testing.T) {
		w := setup(t)
		tcommit(t, w.wt, "work")
		stayed(t, w, sweep(w, nil, ""), "not pushed anywhere")
	})
	t.Run("uncommitted work stays, told once", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		if err := os.WriteFile(filepath.Join(w.wt, "draft"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		stayed(t, w, sweep(w, nil, ""), "untracked")
	})
	t.Run("a process in it", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		procCwds = func() ([]string, error) { return []string{filepath.Join(w.wt, "sub"), "/elsewhere"}, nil }
		stayed(t, w, sweep(w, nil, ""), "a process is running")
	})
	t.Run("processes can't be seen", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		procCwds = func() ([]string, error) { return nil, os.ErrNotExist }
		sweep(w, nil, "")
		if !dirExists(w.wt) {
			t.Fatal("the worktree was removed without a view of the processes")
		}
	})
	t.Run("what other sessions hold can't be read", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		w.p.sweepWorktrees(context.Background(), newRun(time.Minute), autopilotJob{HeldUnknown: true}, w.rl, "")
		if !dirExists(w.wt) {
			t.Fatal("the worktree was removed without knowing what other sessions hold")
		}
	})
	t.Run("the new session starts in a folder within it", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		if kept := sweep(w, nil, filepath.Join(w.wt, "sub")); len(kept) != 0 || !dirExists(w.wt) {
			t.Errorf("kept %q, there %v", kept, dirExists(w.wt))
		}
	})
	t.Run("held by key", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		stayed(t, w, sweep(w, []autopilotHeld{{Key: "APP-1"}}, ""), "APP-1 is still held")
	})
	t.Run("held by a quiet session's folder", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		stayed(t, w, sweep(w, []autopilotHeld{{Key: "APP-9", Cwd: w.wt}}, ""), "APP-9 is still held")
	})
	t.Run("the next task's stays, quietly", func(t *testing.T) {
		w := setup(t)
		pushed(t, w)
		if kept := sweep(w, nil, w.wt); len(kept) != 0 || !dirExists(w.wt) {
			t.Errorf("kept %q, there %v", kept, dirExists(w.wt))
		}
	})
	t.Run("another worktree just merged stays, quietly", func(t *testing.T) {
		w := setup(t)
		w.rl.ended = ""
		if kept := sweep(w, nil, ""); len(kept) != 0 || !dirExists(w.wt) {
			t.Errorf("kept %q, there %v", kept, dirExists(w.wt))
		}
	})
}

// An empty /proc (not mounted here) is no view of the processes, not a
// machine with none running.
func TestCwdsUnder(t *testing.T) {
	root := t.TempDir()
	if _, err := cwdsUnder(root); err == nil {
		t.Error("an empty /proc gave no error")
	}
	if _, err := cwdsUnder(filepath.Join(root, "missing")); err == nil {
		t.Error("a missing /proc gave no error")
	}
	if err := os.MkdirAll(filepath.Join(root, "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/work/here", filepath.Join(root, "7", "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "8"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := cwdsUnder(root); err != nil || len(got) != 1 || got[0] != "/work/here" {
		t.Errorf("%q %v", got, err)
	}
}

func TestRunWorktreeSetup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAYPOINT_TOKEN", "wpt_secret")
	ctx := context.Background()
	if w := runWorktreeSetup(ctx, newRun(time.Minute), dir, "s-1", "a"); w != "" {
		t.Errorf("no setup file: %q", w)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".waypoint"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, autopilotSetupFile)
	if err := os.WriteFile(script, []byte(`echo "$PWD ${WAYPOINT_TOKEN:-none}" > out`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := runWorktreeSetup(ctx, newRun(time.Minute), dir, "s-1", "a"); w != "" {
		t.Errorf("setup: %q", w)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "out"))
	if got := strings.TrimSpace(string(b)); !strings.HasSuffix(got, " none") || !strings.Contains(got, filepath.Base(dir)) {
		t.Errorf("ran as %q", got)
	}
	if err := os.WriteFile(script, []byte("exit 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := runWorktreeSetup(ctx, newRun(time.Minute), dir, "s-1", "a"); !strings.Contains(w, "failed") {
		t.Errorf("failed setup: %q", w)
	}
}
