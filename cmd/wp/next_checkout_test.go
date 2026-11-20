package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// checkoutFixture is a clone of a local bare origin, two commits on main,
// with origin's main one commit further and a branch and a worktree named
// for WP-301. It returns the clone and the origin.
func checkoutFixture(t *testing.T) (clone, origin string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	root := t.TempDir()
	var seed string
	origin, seed, clone = filepath.Join(root, "origin.git"), filepath.Join(root, "seed"), filepath.Join(root, "app")
	git(t, root, "init", "-q", "--bare", "-b", "main", origin)
	git(t, root, "clone", "-q", origin, seed)
	commit := func(dir, file, msg string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(msg+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", file)
		git(t, dir, "commit", "-q", "-m", msg)
	}
	git(t, seed, "checkout", "-q", "-b", "main")
	commit(seed, "a.txt", "First commit")
	commit(seed, "b.txt", "Second commit")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", "origin", "main:wp-301-remote-only")
	git(t, root, "clone", "-q", origin, clone)
	commit(seed, "c.txt", "Third commit, only on origin")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, clone, "branch", "wp-3010-other") // a different task
	git(t, clone, "worktree", "add", "-q", "-b", "wp-301-next-context", filepath.Join(root, "app-wp301"))
	return clone, origin
}

func TestCheckoutReport(t *testing.T) {
	clone, _ := checkoutFixture(t)
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "new.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	before := checkoutReport(clone, "WP-301", nil)
	if strings.Contains(before, "Third commit") || !strings.Contains(before, "even with origin/main") {
		t.Errorf("before the fetch, origin/main is what the clone last saw:\n%s", before)
	}

	wait := startFetch(clone)
	if wait == nil {
		t.Fatal("no fetch in a work tree")
	}
	if err := wait(); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	out := checkoutReport(clone, "WP-301", nil)
	for _, want := range []string{
		"Checkout (just fetched):",
		clone + " on main, 1 behind origin/main",
		"Uncommitted (2 files):",
		"     M a.txt", // porcelain's leading space kept
		"    ?? new.txt",
		"Latest on origin/main:",
		"Third commit, only on origin",
		"Already named for WP-301: wp-301-next-context (worktree " + filepath.Join(filepath.Dir(clone), "app-wp301") + "), 1 behind origin/main",
		"Already named for WP-301: origin/wp-301-remote-only, 1 behind origin/main",
		"2 worktrees of this repo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "wp-3010") {
		t.Errorf("a branch for WP-3010 isn't one for WP-301:\n%s", out)
	}

	if got := checkoutReport(clone, "WP-301", errors.New("no answer from origin in 20s")); !strings.Contains(got, "Checkout (fetch failed: no answer from origin in 20s; remote branches as of the last fetch):") {
		t.Errorf("a failed fetch says so:\n%s", got)
	}
	if got := checkoutReport(t.TempDir(), "WP-301", nil); got != "" {
		t.Errorf("outside a work tree: %q", got)
	}
	if startFetch(t.TempDir()) != nil {
		t.Error("outside a work tree there is nothing to fetch")
	}
}

func TestCheckoutReportBranchAhead(t *testing.T) {
	clone, _ := checkoutFixture(t)
	git(t, clone, "checkout", "-q", "-b", "wp-301-here", "origin/main")
	if err := os.WriteFile(filepath.Join(clone, "d.txt"), []byte("d\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "add", "d.txt")
	git(t, clone, "commit", "-q", "-m", "Work")
	out := checkoutReport(clone, "WP-301", nil)
	for _, want := range []string{"on wp-301-here, 1 ahead of origin/main"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Already named for WP-301: wp-301-here") {
		t.Errorf("the branch here is on the first line, not repeated:\n%s", out)
	}
}

func TestKeyWordRE(t *testing.T) {
	re := keyWordRE("WP-301")
	for name, want := range map[string]bool{
		"wp-301": true, "wp-301-next": true, "wp301": true, "feat/WP-301_x": true, "origin/wp-301-a": true,
		"wp-3010": false, "xwp-301": false, "wp-30": false, "main": false,
	} {
		if got := re.MatchString(name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if keyWordRE("nokey") != nil {
		t.Error("not a key")
	}
}

// wp next prints the checkout after the task, fetched; --no-git leaves it out.
func TestNextPrintsCheckout(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_NO_BRIEF", "1")
	t.Setenv("WP_NO_GIT", "")
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, s.repo, "init", "-q", "--bare", "-b", "main", origin)
	commitFiles(t, s.repo, map[string]string{"README.md": "# App\n"})
	// A local origin: the scope is then named with -p.
	git(t, s.repo, "remote", "set-url", "origin", origin)
	git(t, s.repo, "push", "-q", "origin", "HEAD:main")
	p := s.project.Key

	mk := func(title string) string {
		i, err := s.svc.CreateIssue(context.Background(), s.me, core.IssueInput{Project: s.project.Key, Title: title, Agent: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		return i.Key
	}
	key := mk("Show the checkout")
	out := s.wp("next", key, "-p", p)
	for _, want := range []string{"Checkout (just fetched):", "Latest on origin/main:"} {
		if !strings.Contains(out, want) {
			t.Errorf("next lacks %q:\n%s", want, out)
		}
	}
	if out := s.wp("next", mk("Another"), "--no-git", "-p", p); strings.Contains(out, "Checkout") {
		t.Errorf("--no-git:\n%s", out)
	}
	if out := s.wp("next", "--peek", "-p", p); strings.Contains(out, "Checkout") {
		t.Errorf("--peek takes nothing, so no checkout:\n%s", out)
	}
}
