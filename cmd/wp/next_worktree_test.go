package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// An agent taking a task in the main checkout gets a worktree of its own
// under .claude/worktrees/, the claim moves there, and finished worktrees
// there go (not one a process is in); a clean worktree stays the place for
// the next task; one holding other work outside the session's folder is
// only named; --no-worktree stays put (WP-200).
func TestNextOwnWorktree(t *testing.T) {
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

	done, busy := filepath.Join(home, "done"), filepath.Join(home, "busy")
	for _, p := range []string{done, busy} {
		b := filepath.Base(p)
		git(t, s.repo, "worktree", "add", "-q", "-b", b, p, "origin/main")
		git(t, s.repo, "push", "-q", "origin", b)
	}
	away := filepath.Join(t.TempDir(), "away")
	git(t, s.repo, "worktree", "add", "-q", "-b", "away", away, "origin/main")
	git(t, s.repo, "push", "-q", "origin", "away")
	if err := os.RemoveAll(away); err != nil {
		t.Fatal(err)
	}
	folder := repo
	oldFolder, oldCwds, oldNow := sessionFolder, procCwds, worktreeNow
	sessionFolder = func() string { return folder }
	busyLink := filepath.Join(t.TempDir(), "busy-link")
	if err := os.Symlink(busy, busyLink); err != nil {
		t.Fatal(err)
	}
	procCwds = func() ([]string, error) { return []string{busyLink}, nil }
	worktreeNow = func() time.Time { return time.Now().Add(3 * time.Hour) }
	t.Cleanup(func() { sessionFolder, procCwds, worktreeNow = oldFolder, oldCwds, oldNow })

	p := s.project.Key
	mk := func(title string) string {
		i, err := s.svc.CreateIssue(context.Background(), s.me, core.IssueInput{Project: p, Title: title, Agent: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		return i.Key
	}
	key := mk("Own worktree")
	slug := worktreeSlug(key)
	wt := filepath.Join(home, slug)
	out := s.wp("next", key, "-p", p)
	for _, want := range []string{
		"Worktree of its own: this is the main checkout",
		"Made " + wt + " on new branch worktree-" + slug + " from origin/main",
		"Work there: cd " + wt,
		"Removed 1 finished worktree (merged or their sessions over; clean; all pushed; a merged one's branch too): done",
		wt + " on worktree-" + slug + " (a linked worktree), even with origin/main",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("next lacks %q:\n%s", want, out)
		}
	}
	if dirExists(done) || !dirExists(busy) {
		t.Errorf("done should be gone (%v), busy kept (%v)", dirExists(done), dirExists(busy))
	}
	if list := git(t, s.repo, "worktree", "list", "--porcelain"); !strings.Contains(list, "away") {
		t.Errorf("the missing worktree outside .claude/worktrees/ was pruned:\n%s", list)
	}
	c, err := newCLI(&strings.Builder{}, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, cl := c.state.claimFor(wt); cl == nil || cl.Key != key {
		t.Errorf("claim not moved to %s: %+v", wt, cl)
	}
	if cl := c.state.Claims[repo]; cl != nil {
		t.Errorf("claim left in the main checkout: %+v", cl)
	}

	t.Chdir(wt)
	folder = wt
	if out := s.wp("next", mk("Next one"), "-p", p); strings.Contains(out, "\nWorktree") || !strings.Contains(out, wt+" on worktree-"+slug) {
		t.Errorf("clean worktree:\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	third := mk("Third")
	out = s.wp("next", third, "-p", p)
	if !strings.Contains(out, "this checkout holds "+key+"'s work (1 uncommitted file)") || !strings.Contains(out, "`wp worktree "+third+"`") {
		t.Errorf("busy worktree, outside the folder:\n%s", out)
	}
	if dirExists(filepath.Join(home, worktreeSlug(third))) {
		t.Error("made a worktree outside the session's folder")
	}

	t.Chdir(s.repo)
	folder = repo
	if out := s.wp("next", mk("Here"), "--no-worktree", "-p", p); strings.Contains(out, "\nWorktree") || !strings.Contains(out, repo+" on ") {
		t.Errorf("--no-worktree:\n%s", out)
	}
}

// Moving a claim takes the tasks just taken (all of a batch) and leaves the
// tasks held before where they were; a claim already in the worktree keeps
// its tasks too (WP-200).
func TestMoveClaim(t *testing.T) {
	main, wt := filepath.Join(t.TempDir(), "app"), filepath.Join(t.TempDir(), "wt")
	c := &cli{}
	c.state.Claims = map[string]*claim{
		main: {Key: "AP-3", Also: []string{"AP-4", "AP-1"}, Session: "s1"},
		wt:   {Key: "AP-9"},
	}
	c.moveClaim(main, wt, []string{"AP-3", "AP-4"})
	if got := c.state.Claims[wt]; got == nil || got.Key != "AP-3" || !slices.Equal(got.Also, []string{"AP-4", "AP-9"}) || got.Session != "s1" {
		t.Errorf("worktree's claim: %+v", got)
	}
	if got := c.state.Claims[main]; got == nil || got.Key != "AP-1" || len(got.Also) != 0 {
		t.Errorf("main's claim: %+v", got)
	}
	c.moveClaim(main, wt, []string{"AP-1"})
	if got := c.state.Claims[main]; got != nil {
		t.Errorf("main's claim should be gone: %+v", got)
	}
	if got := c.state.Claims[wt]; got == nil || got.Key != "AP-1" || !slices.Equal(got.Also, []string{"AP-3", "AP-4", "AP-9"}) {
		t.Errorf("worktree's claim after the second move: %+v", got)
	}
}
