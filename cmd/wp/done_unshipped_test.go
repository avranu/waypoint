//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Finishing with nothing to ship but files left uncommitted refuses;
// --no-ship finishes anyway. Commits not pushed go to shipping, whichever
// name ran it (WP-608). A pushed branch that isn't the task's finishes.
func TestDoneRefusesUnshippedCode(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("WP_TASK", "")
	first, second := s.task("First"), s.task("Second")

	s.wp("next", first, "--no-brief")
	if err := os.WriteFile(filepath.Join(s.repo, "fix.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := s.run("done", first, "-m", "x")
	if code == 0 || !strings.Contains(out, "has 1 uncommitted file. To ship it, stage") || !strings.Contains(out, "--no-ship") {
		t.Fatalf("uncommitted: exit %d\n%s", code, out)
	}
	if out := s.wp("show", first); strings.Contains(out, "Done") {
		t.Errorf("refused, yet finished:\n%s", out)
	}

	// Committed but not pushed (a remote has the main branch, not this
	// one): wp done ships it. This repo has no origin to open a pull
	// request against, so shipping stops at the base branch.
	bare := t.TempDir()
	git(t, bare, "init", "-q", "--bare")
	git(t, s.repo, "remote", "add", "pub", bare)
	git(t, s.repo, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, s.repo, "push", "-q", "pub", "HEAD:refs/heads/main")
	git(t, s.repo, "fetch", "-q", "pub")
	git(t, s.repo, "add", "fix.go")
	git(t, s.repo, "commit", "-q", "-m", "fix")
	if out, code := s.run("done", first, "-m", "x"); code == 0 || !strings.Contains(out, "default branch") {
		t.Fatalf("unpushed went to shipping? exit %d\n%s", code, out)
	}
	s.wp("done", first, "-m", "not this task's", "--no-ship")

	// Pushed: nothing left here, so wp done finishes.
	git(t, s.repo, "push", "-q", "-u", "pub", "HEAD:refs/heads/fix")
	s.wp("next", second, "--no-brief")
	s.wp("done", second, "-m", "reviewed")
}

func TestNamesKey(t *testing.T) {
	for _, c := range []struct {
		text, key string
		want      bool
	}{
		{"wp-608-one-finish", "WP-608", true},
		{"feature/WP-608", "WP-608", true},
		{"wp-6080-x", "WP-608", false},
		{"xwp-608", "WP-608", false},
		{"fix-wp-6080-and-wp-608", "WP-608", true},
		{"main", "WP-608", false},
		{"Fix the redirect (WP-608)", "WP-608", true},
		{"Fix the redirect (WP-6080)", "WP-608", false},
	} {
		if got := namesKey(c.text, c.key); got != c.want {
			t.Errorf("namesKey(%q, %q) = %v", c.text, c.key, got)
		}
	}
}
