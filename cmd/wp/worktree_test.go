package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBesideCheckout(t *testing.T) {
	root := "/projects/team/repo"
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/projects/team/repo-x", true},
		{"/projects/sp-activity", true},
		{"/projects/team/repo/.claude/worktrees/x", false},
		{"/projects/team/repo/.worktrees/x", false},
		{"/projects/team/worktrees/x", false},
		{"/tmp/claude/scratch/x", false},
		{"/home/me/wt/x", false},
		{"/projects/team/repo", false},
	} {
		if got := besideCheckout(root, tc.path); got != tc.want {
			t.Errorf("besideCheckout(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestMovedName(t *testing.T) {
	for in, want := range map[string]string{
		"/p/waypoint-wp402": "wp402",
		"/p/waypoint":       "waypoint",
		"/p/sp-activity":    "sp-activity",
		"/p/Waypoint PR 3":  "waypoint-pr-3",
	} {
		if got := movedName("/p/waypoint", in); got != want {
			t.Errorf("movedName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A git worktree add beside the main checkout is refused with the same
// command aimed under .claude/worktrees/; anywhere else, or with the
// override, it isn't.
func TestWorktreePlaceNudge(t *testing.T) {
	f := newTidyFixture(t)
	parent := filepath.Dir(f.repo)
	home := filepath.Join(f.repo, ".claude", "worktrees")
	other := t.TempDir()
	for _, tc := range []struct {
		cmd, dir string
		want     string
	}{
		{"git worktree add ../repo-x -b x", f.repo, "`git worktree add " + home + "/x -b x`"},
		{"git worktree add -b feat ../repo-feat origin/main", f.repo, "`git worktree add -b feat " + home + "/feat origin/main`"},
		{"cd /tmp && git -C " + f.repo + " worktree add ../repo-y", "/", "`git -C " + f.repo + " worktree add " + home + "/y`"},
		{"cd " + f.repo + " && git worktree add " + parent + "/elsewhere", "/", home + "/elsewhere"},
		{"bash --norc -c 'git worktree add ../repo-z'", f.repo, "worktree add " + home + "/z"},
		{"bash -o pipefail -c 'git worktree add ../repo-w'", f.repo, "worktree add " + home + "/w"},
		{"git worktree add .claude/worktrees/x", f.repo, ""},
		{"git worktree add " + other + "/x", f.repo, ""},
		{"WP_WORKTREE_ANYWHERE=1 git worktree add ../repo-x", f.repo, ""},
		{"echo git worktree add ../repo-x", f.repo, ""},
		{"git worktree list", f.repo, ""},
		{"git worktree add ../x", other, ""},
		{`sh -c "git worktree add ../repo-x"`, f.repo, home + "/x"},
		{"bash -lc 'git worktree add ../repo-x'", f.repo, home + "/x"},
		{"sudo git worktree add ../repo-x", f.repo, home + "/x"},
		{"command git worktree add ../repo-x", f.repo, home + "/x"},
		{"echo $(git worktree add ../repo-y)", f.repo, home + "/y"},
		{"git worktree move .claude/worktrees/z ../repo-z", f.repo, "`git worktree move .claude/worktrees/z " + home + "/z`"},
		{"git worktree move ../repo-z .claude/worktrees/z", f.repo, ""},
		{"git worktree add --orphan ../repo-w", f.repo, home + "/w"},
		{"export WP_WORKTREE_ANYWHERE=1; git worktree add ../repo-x", f.repo, ""},
		{"sudo WP_WORKTREE_ANYWHERE=1 git worktree add ../repo-x", f.repo, ""},
		{`sh -c "WP_WORKTREE_ANYWHERE=1 git worktree add ../repo-x"`, f.repo, ""},
	} {
		got := worktreePlaceNudge(tc.cmd, tc.dir)
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%q: refused: %s", tc.cmd, got)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("%q: want %q in %q", tc.cmd, tc.want, got)
		}
	}
	t.Setenv(worktreeAnywhere, "1")
	if got := worktreePlaceNudge("git worktree add ../repo-x", f.repo); got != "" {
		t.Errorf("%s=1 in the environment: refused: %s", worktreeAnywhere, got)
	}
}

// wp worktree NAME makes it under .claude/worktrees/ on worktree-NAME, keeps
// the folder out of git status, copies .worktreeinclude files and runs the
// setup; again, it says it is there.
func TestWorktreeAdd(t *testing.T) {
	f := newTidyFixture(t)
	write := func(rel, body string) {
		p := filepath.Join(f.repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", ".env\nsetup-ran\n")
	write(".worktreeinclude", ".env\n")
	write(".waypoint/worktree-setup", "echo \"${WAYPOINT_TOKEN:-none}\" > setup-ran\n")
	tgit(t, f.repo, "add", ".")
	tgit(t, f.repo, "commit", "-q", "-m", "setup")
	tgit(t, f.repo, "push", "-q", "origin", "main")
	write(".env", "SECRET=1\n")
	e := newEnv(t)
	t.Chdir(f.repo)
	t.Setenv("WP_TASK", "")
	t.Setenv("WAYPOINT_TOKEN", "secret-x")
	out := e.wp("worktree", "WP-12")
	wt := filepath.Join(f.repo, ".claude", "worktrees", "wp-12")
	if !strings.Contains(out, "cd "+wt) {
		t.Fatalf("output: %s", out)
	}
	if b := tgit(t, wt, "branch", "--show-current"); b != "worktree-wp-12" {
		t.Errorf("branch %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(wt, ".env")); err != nil || string(b) != "SECRET=1\n" {
		t.Errorf(".env not copied: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(wt, "setup-ran")); err != nil {
		t.Errorf("setup didn't run: %s", out)
	} else if strings.TrimSpace(string(b)) != "none" {
		t.Errorf("the setup saw WAYPOINT_TOKEN=%q (audit 939.1)", b)
	}
	if st := tgit(t, f.repo, "status", "--porcelain"); st != "" {
		t.Errorf("main checkout's status: %q", st)
	}
	if out := e.wp("worktree", "WP-12"); !strings.Contains(out, "Already a worktree") {
		t.Errorf("again: %s", out)
	}
	tgit(t, f.repo, "push", "-q", "origin", "main:feat")
	tgit(t, f.repo, "fetch", "-q", "origin")
	out = e.wp("worktree", "--branch", "feat", "--no-setup")
	if !strings.Contains(out, "tracking origin/feat") || !dirExists(filepath.Join(f.repo, ".claude", "worktrees", "feat")) {
		t.Errorf("--branch feat: %s", out)
	}
	tgit(t, f.repo, "branch", "worktree-foo")
	tgit(t, f.repo, "worktree", "add", "-q", "-b", "other", filepath.Join(f.repo, ".claude", "worktrees", "foo-2"))
	if out := e.wp("worktree", "foo"); !strings.Contains(out, "Already a worktree (other)") {
		t.Errorf("foo-2 reused: %s", out)
	}
	if out, code := e.run("worktree"); code == 0 {
		t.Errorf("no name, no WP_TASK: exit 0: %s", out)
	}
}

// move takes a quiet worktree from beside the checkout, uncommitted work
// and all, and leaves one a live session worked in.
func TestWorktreeMove(t *testing.T) {
	f := newTidyFixture(t)
	parent := filepath.Dir(f.repo)
	quiet, busy := filepath.Join(parent, "repo-quiet"), filepath.Join(parent, "repo-busy")
	tgit(t, f.repo, "worktree", "add", "-q", "-b", "quiet", quiet)
	tgit(t, f.repo, "worktree", "add", "-q", "-b", "busy", busy)
	if err := os.WriteFile(filepath.Join(quiet, "wip.txt"), []byte("wip"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{"sessions": []map[string]string{{"cwd": busy}}})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	e := newEnv(t)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "x")
	oldNow, oldCwds := worktreeNow, procCwds
	worktreeNow = func() time.Time { return time.Now().Add(48 * time.Hour) }
	procCwds = func() ([]string, error) { return []string{"/"}, nil }
	t.Cleanup(func() { worktreeNow, procCwds = oldNow, oldCwds })
	t.Chdir(f.repo)
	if out := e.wp("worktree", "ls"); !strings.Contains(out, "2 beside the main checkout") {
		t.Errorf("ls: %s", out)
	}
	moved := filepath.Join(f.repo, ".claude", "worktrees", "quiet")
	if out := e.wp("worktree", "move", "-n"); !strings.Contains(out, "would move "+quiet+" → "+moved) || !dirExists(quiet) {
		t.Errorf("dry run: %s", out)
	}
	if out := e.wp("tidy"); !strings.Contains(out, "2 kept worktree(s) sit beside the main checkout") {
		t.Errorf("tidy note: %s", out)
	}
	out := e.wp("worktree", "move")
	if !strings.Contains(out, "moved "+quiet) || !fileExists(filepath.Join(moved, "wip.txt")) {
		t.Errorf("quiet not moved with its work: %s", out)
	}
	if !strings.Contains(out, "kept "+busy+": a Waypoint session") || !dirExists(busy) {
		t.Errorf("busy: %s", out)
	}
	if b := tgit(t, moved, "branch", "--show-current"); b != "quiet" {
		t.Errorf("moved worktree's branch %q", b)
	}
	defer func() { worktreeNow = time.Now }()
	worktreeNow = time.Now
	fresh := filepath.Join(parent, "repo-fresh")
	tgit(t, f.repo, "worktree", "add", "-q", "-b", "fresh", fresh)
	if out := e.wp("worktree", "move", fresh); !strings.Contains(out, "kept "+fresh+": its HEAD moved") || !dirExists(fresh) {
		t.Errorf("recent: %s", out)
	}
	if out := e.wp("worktree", "move", moved); !strings.Contains(out, "kept "+moved+": already under") || !dirExists(moved) {
		t.Errorf("homed: %s", out)
	}
}

// Where processes' folders can't be read (no /proc), a dry run still says
// what would move; a move moves nothing and names the git commands
// (audit 939.3).
func TestWorktreeMoveBlind(t *testing.T) {
	f := newTidyFixture(t)
	quiet := filepath.Join(filepath.Dir(f.repo), "repo-quiet")
	tgit(t, f.repo, "worktree", "add", "-q", "-b", "quiet", quiet)
	e := newEnv(t)
	oldNow, oldCwds := worktreeNow, procCwds
	worktreeNow = func() time.Time { return time.Now().Add(48 * time.Hour) }
	procCwds = func() ([]string, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { worktreeNow, procCwds = oldNow, oldCwds })
	t.Chdir(f.repo)
	moved := filepath.Join(f.repo, ".claude", "worktrees", "quiet")
	if out := e.wp("worktree", "move", "-n"); !strings.Contains(out, "would move "+quiet+" → "+moved) || !strings.Contains(out, "can't see which folders") {
		t.Errorf("dry run: %s", out)
	}
	out, code := e.run("worktree", "move")
	if code == 0 || !strings.Contains(out, "worktree move "+quiet+" "+moved) || !dirExists(quiet) {
		t.Errorf("blind move (exit %d): %s", code, out)
	}
}

// A worktree's .git file is git's, not work (audit 939.6).
func TestChangedSinceSkipsGitFile(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-72 * time.Hour)
	for _, n := range []string{"a.txt", ".git"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(dir, "a.txt"), dir} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if changedSince(dir, time.Now().Add(-24*time.Hour)) {
		t.Error("a fresh .git file counted as a change")
	}
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !changedSince(dir, time.Now().Add(-24*time.Hour)) {
		t.Error("a changed file not seen")
	}
}

// check-ignore failing for real is an error, not "nothing to copy"
// (audit 939.7).
func TestIgnoredOfFails(t *testing.T) {
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	if _, err := ignoredOf(t.Context(), t.TempDir(), ".env\x00"); err == nil {
		t.Error("check-ignore outside a repository: no error")
	}
	f := newTidyFixture(t)
	if out, err := ignoredOf(t.Context(), f.repo, "nothing-ignored\x00"); err != nil || out != "" {
		t.Errorf("none ignored: %q %v", out, err)
	}
}
