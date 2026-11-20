package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Stop and a SessionStart report the checkout the session is in; a stop
// also the worktrees of its tasks (WP-567).
func TestHookCheckouts(t *testing.T) {
	clone, _ := checkoutFixture(t)
	wt := filepath.Join(filepath.Dir(clone), "app-wp301")

	got := hookCheckouts(clone, nil)
	if len(got) != 1 || got[0].Top != clone || got[0].Branch != "main" || got[0].FileCount != 0 || got[0].Unpushed != 0 || !got[0].Default {
		t.Fatalf("clean: %+v", got)
	}
	if got := hookCheckouts(clone, []string{"WP-301"}); len(got) != 2 || got[1].Top != wt || got[1].Branch != "wp-301-next-context" || got[1].Default {
		t.Errorf("the task's clean worktree: %+v", got)
	}
	if got := hookCheckouts(clone, []string{"WP-30"}); len(got) != 1 {
		t.Errorf("WP-30 isn't WP-301: %+v", got)
	}

	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "d.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "d.txt")
	git(t, wt, "commit", "-q", "-m", "Not pushed")
	if err := os.WriteFile(filepath.Join(wt, "e.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := hookCheckouts(clone, nil); len(got) != 1 || got[0].Top != clone {
		t.Errorf("a start looks at its own checkout only: %+v", got)
	}
	got = hookCheckouts(clone, []string{"WP-301"})
	if len(got) != 2 {
		t.Fatalf("dirty worktree: %+v", got)
	}
	w := got[1]
	if w.Top != wt || w.FileCount != 2 || w.Unpushed != 1 || strings.Join(trimLines(w.Files), ",") != "M a.txt,?? e.txt" {
		t.Errorf("dirty worktree: %+v", w)
	}
	if hookCheckouts("relative", nil) != nil || hookCheckouts(t.TempDir(), nil) != nil {
		t.Error("no checkout: want nil")
	}
}

// A remote that was never fetched has no remote-tracking branches: the
// checkout's own history isn't "unpushed" work (WP-579).
func TestHookCheckoutsUnfetchedRemote(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-q", "-m", "First")
	git(t, dir, "remote", "add", "origin", filepath.Join(dir, "nowhere.git"))
	got := hookCheckouts(dir, nil)
	if len(got) != 1 || got[0].Unpushed != 0 || got[0].Default {
		t.Errorf("unfetched remote: %+v", got)
	}
}

func trimLines(l []string) []string {
	out := make([]string, len(l))
	for n, s := range l {
		out[n] = strings.TrimSpace(s)
	}
	return out
}

func TestBranchCarries(t *testing.T) {
	for _, c := range []struct {
		branch string
		keys   []string
		want   bool
	}{
		{"wp-437-packet-triage", nil, false},
		{"feature/WP437", []string{"WP-437"}, true},
		{"wp-437-packet-triage", []string{"WP-437"}, true},
		{"wp-4370-x", []string{"WP-437"}, false},
		{"wp-437-x", []string{"LIB-4"}, false},
		{"main", []string{}, false},
	} {
		if got := branchCarries(c.branch, c.keys); got != c.want {
			t.Errorf("branchCarries(%q, %q) = %v", c.branch, c.keys, got)
		}
	}
}
