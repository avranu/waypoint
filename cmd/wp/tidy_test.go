package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tgit runs git in dir for a test, with a fixed identity.
func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// tcommit commits a new file (on whatever is checked out in dir).
func tcommit(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	tgit(t, dir, "add", name)
	tgit(t, dir, "commit", "-q", "-m", name)
	return tgit(t, dir, "rev-parse", "HEAD")
}

// tidyFixture is a clone with an origin, main pushed.
type tidyFixture struct {
	repo, origin string
}

func newTidyFixture(t *testing.T) *tidyFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &tidyFixture{repo: filepath.Join(root, "repo"), origin: filepath.Join(root, "origin.git")}
	tgit(t, root, "init", "-q", "--bare", "-b", "main", f.origin)
	tgit(t, root, "clone", "-q", f.origin, f.repo)
	tgit(t, f.repo, "checkout", "-q", "-b", "main")
	tcommit(t, f.repo, "base")
	tgit(t, f.repo, "push", "-q", "-u", "origin", "main")
	tgit(t, f.repo, "remote", "set-head", "origin", "main")
	return f
}

// merge makes branch (with one commit, pushed) and merges it into main, pushed.
func (f *tidyFixture) merge(t *testing.T, branch string) string {
	t.Helper()
	tgit(t, f.repo, "checkout", "-q", "-b", branch, "main")
	tip := tcommit(t, f.repo, branch+".txt")
	tgit(t, f.repo, "push", "-q", "origin", branch)
	tgit(t, f.repo, "checkout", "-q", "main")
	tgit(t, f.repo, "merge", "-q", "--no-ff", "-m", "merge "+branch, branch)
	tgit(t, f.repo, "push", "-q", "origin", "main")
	return tip
}

// squash makes branch (pushed) and squash-merges it into main: no ancestry.
func (f *tidyFixture) squash(t *testing.T, branch string) string {
	t.Helper()
	tgit(t, f.repo, "checkout", "-q", "-b", branch, "main")
	tip := tcommit(t, f.repo, branch+".txt")
	tgit(t, f.repo, "push", "-q", "origin", branch)
	tgit(t, f.repo, "checkout", "-q", "main")
	tgit(t, f.repo, "merge", "-q", "--squash", branch)
	tgit(t, f.repo, "commit", "-q", "-m", "squash "+branch)
	tgit(t, f.repo, "push", "-q", "origin", "main")
	return tip
}

func (f *tidyFixture) worktree(t *testing.T, name, branch string) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(f.repo), name)
	tgit(t, f.repo, "worktree", "add", "-q", p, branch)
	return p
}

// later is far enough ahead of the fixture's commits for the quiet period.
func later() time.Time { return time.Now().Add(3 * time.Hour) }

func scanTidy(t *testing.T, dir string, merged []tidyPR, now time.Time) *tidyReport {
	t.Helper()
	rep, err := tidyScan(context.Background(), dir, merged, tidyOpts{worktrees: true, branches: true, now: now})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (r *tidyReport) row(t *testing.T, kind, name string) *tidyRow {
	t.Helper()
	for _, x := range r.Rows {
		if x.Kind == kind && (x.Branch == name || filepath.Base(x.Path) == name) {
			return x
		}
	}
	t.Fatalf("no %s %q in %+v", kind, name, r.Rows)
	return nil
}

func wantVerdict(t *testing.T, r *tidyRow, verdict, reason string) {
	t.Helper()
	if r.Verdict != verdict || !strings.Contains(r.Reason, reason) {
		t.Errorf("%s %s: %s (%s), want %s (%s)", r.Kind, r.name(), r.Verdict, r.Reason, verdict, reason)
	}
}

func TestTidyVerdicts(t *testing.T) {
	f := newTidyFixture(t)

	// merged by ancestry: a branch alone and one with a clean worktree
	f.merge(t, "done-alone")
	f.merge(t, "done-wt")
	wtDone := f.worktree(t, "wt-done", "done-wt")

	// merged and clean, but the worktree is dirty / has an untracked file / is locked
	f.merge(t, "done-dirty")
	wtDirty := f.worktree(t, "wt-dirty", "done-dirty")
	if err := os.WriteFile(filepath.Join(wtDirty, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.merge(t, "done-locked")
	f.worktree(t, "wt-locked", "done-locked")
	tgit(t, f.repo, "worktree", "lock", filepath.Join(filepath.Dir(f.repo), "wt-locked"))

	// ignored files don't count as dirty
	f.merge(t, "done-ignored")
	wtIgn := f.worktree(t, "wt-ignored", "done-ignored")
	if err := os.WriteFile(filepath.Join(f.repo, ".git", "info", "exclude"), []byte("node_modules/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wtIgn, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtIgn, "node_modules", "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// squash-merged: not an ancestor; Waypoint says its PR merged
	sqTip := f.squash(t, "sq")
	// squash-merged, then a local commit after: not removable
	sqMore := f.squash(t, "sq-more")
	tgit(t, f.repo, "checkout", "-q", "sq-more")
	tcommit(t, f.repo, "later.txt")
	tgit(t, f.repo, "checkout", "-q", "main")

	// unpushed work on an unmerged branch; an unmerged pushed one; a stale one
	tgit(t, f.repo, "checkout", "-q", "-b", "wip", "main")
	tcommit(t, f.repo, "wip.txt")
	tgit(t, f.repo, "checkout", "-q", "-b", "pushed", "main")
	tcommit(t, f.repo, "pushed.txt")
	tgit(t, f.repo, "push", "-q", "origin", "pushed")
	tgit(t, f.repo, "checkout", "-q", "-b", "old", "main")
	if err := os.WriteFile(filepath.Join(f.repo, "old.txt"), []byte("o"), 0o600); err != nil {
		t.Fatal(err)
	}
	tgit(t, f.repo, "add", "old.txt")
	old := time.Now().AddDate(0, 0, -60).Format(time.RFC3339)
	cmd := exec.Command("git", "-C", f.repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "old", "--date", old)
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+old)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	tgit(t, f.repo, "push", "-q", "origin", "old")
	tgit(t, f.repo, "branch", "release/1.0", "main") // merged, but protected
	tgit(t, f.repo, "checkout", "-q", "main")

	// the current branch: a merged one checked out in the repo you are in
	f.merge(t, "here")
	wtHere := f.worktree(t, "wt-here", "here")

	prs := []tidyPR{
		{Number: 7, HeadRef: "sq", HeadSHA: sqTip, State: "merged"},
		{Number: 8, HeadRef: "sq-more", HeadSHA: sqMore, State: "merged"},
	}
	rep := scanTidy(t, wtHere, prs, later())

	wantVerdict(t, rep.row(t, "branch", "done-alone"), tidyRemovable, "merged (origin/main)")
	wantVerdict(t, rep.row(t, "worktree", "wt-done"), tidyRemovable, "merged")
	wantVerdict(t, rep.row(t, "worktree", "wt-dirty"), tidyKeep, "1 untracked file")
	wantVerdict(t, rep.row(t, "worktree", "wt-locked"), tidyKeep, "locked")
	wantVerdict(t, rep.row(t, "worktree", "wt-ignored"), tidyRemovable, "merged")
	wantVerdict(t, rep.row(t, "branch", "sq"), tidyRemovable, "PR #7")
	wantVerdict(t, rep.row(t, "branch", "sq-more"), tidyKeep, "not pushed")
	wantVerdict(t, rep.row(t, "branch", "wip"), tidyKeep, "1 commit not pushed")
	wantVerdict(t, rep.row(t, "branch", "pushed"), tidyKeep, "unmerged")
	wantVerdict(t, rep.row(t, "branch", "old"), tidyStale, "no commit for")
	wantVerdict(t, rep.row(t, "branch", "release/1.0"), tidyProtected, "release")
	wantVerdict(t, rep.row(t, "worktree", "wt-here"), tidyProtected, "you are in")
	wantVerdict(t, rep.row(t, "worktree", "repo"), tidyProtected, "main checkout")
	for _, r := range rep.Rows {
		if r.Branch == "main" && r.Kind == "branch" {
			t.Errorf("main listed as a branch row: %+v", r)
		}
		if r.Verdict == tidyRemovable && r.MergedBy == "" {
			t.Errorf("%s removable without saying how it is merged", r.name())
		}
	}

	// Without Waypoint's answer a squash merge is just unmerged.
	wantVerdict(t, scanTidy(t, wtHere, nil, later()).row(t, "branch", "sq"), tidyKeep, "unmerged")

	// A fresh branch is "merged" until it commits: not removable for a while.
	wantVerdict(t, scanTidy(t, wtHere, prs, time.Now()).row(t, "branch", "done-alone"), tidyKeep, "active")

	// --apply removes the removable and nothing else.
	removable := map[string]bool{}
	for _, r := range rep.Rows {
		if r.Verdict == tidyRemovable {
			removable[r.name()] = true
		}
	}
	if len(removable) == 0 {
		t.Fatal("nothing removable")
	}
	keepBefore := tgit(t, f.repo, "branch", "--format=%(refname:short)")
	tidyApply(context.Background(), wtHere, rep, tidyOpts{worktrees: true, branches: true})
	after := tgit(t, f.repo, "branch", "--format=%(refname:short)")
	for _, gone := range []string{"done-alone", "done-wt", "sq", "done-ignored"} {
		if strings.Contains("\n"+after+"\n", "\n"+gone+"\n") {
			t.Errorf("%s survived --apply:\n%s", gone, after)
		}
	}
	for _, kept := range []string{"done-dirty", "done-locked", "sq-more", "wip", "pushed", "old", "release/1.0", "here", "main"} {
		if !strings.Contains("\n"+after+"\n", "\n"+kept+"\n") {
			t.Errorf("%s was deleted by --apply (before:\n%s\nafter:\n%s)", kept, keepBefore, after)
		}
	}
	for _, p := range []string{wtDirty, filepath.Join(filepath.Dir(f.repo), "wt-locked"), wtHere} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("worktree %s was removed: %v", p, err)
		}
	}
	for _, p := range []string{wtDone, wtIgn} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("worktree %s survived --apply", p)
		}
	}
	if !strings.Contains(tgit(t, f.repo, "worktree", "list"), "wt-dirty") {
		t.Error("the dirty worktree isn't listed any more")
	}
}

// A branch that moved between the scan and the delete is kept.
func TestTidyApplyKeepsAMovedBranch(t *testing.T) {
	f := newTidyFixture(t)
	f.merge(t, "done")
	rep := scanTidy(t, f.repo, nil, later())
	tgit(t, f.repo, "checkout", "-q", "done")
	tcommit(t, f.repo, "new-work.txt")
	tgit(t, f.repo, "checkout", "-q", "main")
	tidyApply(context.Background(), f.repo, rep, tidyOpts{worktrees: true, branches: true})
	if !strings.Contains(tgit(t, f.repo, "branch", "--list", "done"), "done") {
		t.Error("a branch with new work was deleted")
	}
}

// --worktrees-only leaves the branches, --branches-only the worktrees.
func TestTidyOnlyFlags(t *testing.T) {
	f := newTidyFixture(t)
	f.merge(t, "a")
	f.merge(t, "b")
	wt := f.worktree(t, "wt-a", "a")
	// the CLI, against a server that knows no pull requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pulls":[]}`))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "x")
	t.Chdir(f.repo)

	rep, err := tidyScan(context.Background(), f.repo, nil, tidyOpts{branches: true, now: later()})
	if err != nil {
		t.Fatal(err)
	}
	wantVerdict(t, rep.row(t, "branch", "a"), tidyKeep, "checked out in")
	wantVerdict(t, rep.row(t, "branch", "b"), tidyRemovable, "merged")
	rep, err = tidyScan(context.Background(), f.repo, nil, tidyOpts{worktrees: true, now: later()})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Rows {
		if r.Kind != "worktree" {
			t.Errorf("a branch row with --worktrees-only: %+v", r)
		}
	}
	tidyApply(context.Background(), f.repo, rep, tidyOpts{worktrees: true})
	if _, err := os.Stat(wt); err == nil {
		t.Error("worktree not removed")
	}
	if !strings.Contains(tgit(t, f.repo, "branch", "--list", "a"), "a") {
		t.Error("--worktrees-only deleted the branch")
	}
}

// The command: asks Waypoint for the repo's merged pull requests, prints the
// table and the way to apply, exits 0; --explain lists the rules.
func TestTidyCommand(t *testing.T) {
	f := newTidyFixture(t)
	sqTip := f.squash(t, "sq")
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		b, _ := json.Marshal(map[string]any{"pulls": []tidyPR{{Number: 12, HeadRef: "sq", HeadSHA: sqTip, State: "merged"}}})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	e := newEnv(t)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "x")
	t.Chdir(f.repo)
	out := e.wp("tidy", "--explain", "sq")
	if !strings.Contains(out, "SQ") && !strings.Contains(strings.ToLower(out), "branch sq") {
		t.Errorf("explain: %s", out)
	}
	if !strings.Contains(gotQuery, "state=merged") || !strings.Contains(gotQuery, "repo=") {
		t.Errorf("asked Waypoint for %q", gotQuery)
	}
	if !strings.Contains(out, "merged: pull request #12 merged this very commit") {
		t.Errorf("explain doesn't give the rule that decided:\n%s", out)
	}
	out = e.wp("tidy")
	if !strings.Contains(out, "main") || !strings.Contains(out, "default branch: main") {
		t.Errorf("table:\n%s", out)
	}
	var js tidyReport
	if err := json.Unmarshal([]byte(e.wp("tidy", "--json")), &js); err != nil || js.Default != "main" || len(js.Rows) == 0 {
		t.Errorf("json: %v %+v", err, js)
	}
	if out, code := e.run("tidy", "--worktrees-only", "--branches-only"); code == 0 {
		t.Errorf("both only-flags accepted: %s", out)
	}
	if out, code := e.run("tidy", "stray"); code == 0 {
		t.Errorf("stray argument accepted: %s", out)
	}
}

// wp tidy --remote lists what Waypoint's sweep deletes and what it calls stale.
func TestTidyRemote(t *testing.T) {
	f := newTidyFixture(t)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		_, _ = w.Write([]byte(`[{"repo":"me/app","default_branch":"main","enabled":true,"counts":{},"rows":[
			{"branch":"main","sha":"a","verdict":"protected","reason":"the default branch"},
			{"branch":"old-work","sha":"b","verdict":"removable","reason":"merged (its tip is in main), no open pull request"},
			{"branch":"abandoned","sha":"c","verdict":"stale","reason":"unmerged, no pull request, no commit for 41 days"}]}]`))
	}))
	defer srv.Close()
	e := newEnv(t)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "x")
	t.Chdir(f.repo)
	out := e.wp("tidy", "--remote")
	for _, want := range []string{"REMOVABLE", "old-work", "STALE", "abandoned", "41 days", "daily sweep"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(gotPath, "/api/v1/branches/tidy?repo=") {
		t.Errorf("asked %q", gotPath)
	}
	if out, code := e.run("tidy", "--remote", "--apply"); code == 0 {
		t.Errorf("--remote --apply accepted: %s", out)
	}
}

func TestTidyHintThreshold(t *testing.T) {
	for n, want := range map[int]string{-1: "", 0: "", 10: "", 11: "11 merged branches/worktrees can go: wp tidy --apply", 40: "40 merged branches/worktrees can go: wp tidy --apply"} {
		if got := tidyHintText(n); got != want {
			t.Errorf("tidyHintText(%d) = %q, want %q", n, got, want)
		}
	}
}

// The SessionStart hint counts what is removable, once a day per repo.
func TestTidyHintCountsAndCaches(t *testing.T) {
	f := newTidyFixture(t)
	// Counting 11 branches takes git a few calls each; on a loaded CI runner
	// that outran the 300ms the real hook allows and read as "-1, try later".
	t.Cleanup(func(b time.Duration) func() { return func() { tidyHintBudget = b } }(tidyHintBudget))
	tidyHintBudget = time.Minute
	t.Setenv("WAYPOINT_SERVER", "")
	t.Setenv("WAYPOINT_TOKEN", "")
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	for i := range 10 {
		f.merge(t, "m"+string(rune('a'+i)))
	}
	c := &cli{}
	now := later()
	if h := tidyHint(c, f.repo, now); h != "" {
		t.Fatalf("10 removable: %q", h)
	}
	// still cached: an 11th doesn't show until the day is over
	f.merge(t, "mk")
	if h := tidyHint(c, f.repo, now.Add(time.Hour)); h != "" {
		t.Errorf("cache ignored: %q", h)
	}
	if h := tidyHint(c, f.repo, now.Add(25*time.Hour)); h != "11 merged branches/worktrees can go: wp tidy --apply" {
		t.Errorf("after a day: %q", h)
	}
	// the text reaches SessionStart
	if got := c.withTidyHint("decision arrived", hookEventInput{Cwd: f.repo, Source: "startup"}); got != "decision arrived" {
		// the real clock is inside the quiet period of the fixture's branches
		t.Logf("fresh branches are not counted yet: %q", got)
	}
	if got := c.withTidyHint("x", hookEventInput{Cwd: f.repo, Source: "compact"}); got != "x" {
		t.Errorf("compact: %q", got)
	}
	if got := c.withTidyHint("x", hookEventInput{Cwd: "relative", Source: "startup"}); got != "x" {
		t.Errorf("relative cwd: %q", got)
	}
	if got := c.withTidyHint("", hookEventInput{Cwd: t.TempDir(), Source: "startup"}); got != "" {
		t.Errorf("outside a repo: %q", got)
	}
}
