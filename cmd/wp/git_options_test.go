package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitShim puts a git on PATH that only logs what it was asked, and returns
// the log's path: a refused value must leave it absent.
func gitShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "ran")
	shim := "#!/bin/sh\necho \"$@\" >> " + log + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil { // #nosec G306 -- a test shim must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func noGitRan(t *testing.T, log string) {
	t.Helper()
	if b, err := os.ReadFile(log); err == nil {
		t.Errorf("git ran: %s", b)
	}
}

var optionLike = []string{"--output=/tmp/x", "--upload-pack=touch /tmp/y", "-p", "main\nx"}

// A value a person, a forge or a file supplies that git would read as an
// option never reaches git (WP-1370): --branch and --from of wp worktree.
func TestWorktreeAddRefusesAnOptionAsBranchOrFrom(t *testing.T) {
	log := gitShim(t)
	for _, flag := range []string{"--branch", "--from"} {
		for _, bad := range optionLike {
			c := &cli{err: &bytes.Buffer{}, out: &bytes.Buffer{}}
			if err := cmdWorktreeAdd(c, []string{flag + "=" + bad, "some-name"}); err == nil {
				t.Errorf("%s=%q was accepted", flag, bad)
			}
		}
	}
	noGitRan(t, log)
}

// A remote named like an option in the checkout's config isn't fetched.
func TestFetchRemoteSkipsAnOptionNamedRemote(t *testing.T) {
	dir := gitRepo(t, "https://example.invalid/r.git")
	tgit(t, dir, "config", "remote.--upload-pack=touch /tmp/z.url", "https://example.invalid/o.git")
	if got := fetchRemote(dir); got != "origin" {
		t.Errorf("remote = %q, want origin", got)
	}
	tgit(t, dir, "remote", "remove", "origin")
	if got := fetchRemote(dir); got != "" {
		t.Errorf("only an option-named remote: got %q, want none", got)
	}
}

// The runner checks a job's target like its base and branch.
func TestRunnerRefusesAnOptionAsTarget(t *testing.T) {
	p := &runnerProc{}
	for _, bad := range optionLike {
		if err := p.checkJobRefs(context.Background(), agentJob{Target: bad}); err == nil {
			t.Errorf("target %q was accepted", bad)
		}
	}
}

// wp code risk --diff BASE and the pull request's refs from the forge, the
// commit a round started from and the trees a test run recorded: refused
// before git is given them.
func TestOptionLikeRefsNeverReachGitInTheirReaders(t *testing.T) {
	log := gitShim(t)
	for _, bad := range optionLike {
		if _, err := diffFiles(t.TempDir(), bad); err == nil {
			t.Errorf("diffFiles base %q was accepted", bad)
		}
		if got := filesBetween(bad, "HEAD"); got != -1 {
			t.Errorf("filesBetween(%q) = %d", bad, got)
		}
		if got := objectiveBase(&cli{err: &bytes.Buffer{}}, &objective{BaseSHA: bad, Measure: "time"}, ""); got != "" {
			t.Errorf("objectiveBase(%q) = %q", bad, got)
		}
	}
	noGitRan(t, log)
}

// A ref named like an option can exist (git update-ref makes one): tidy
// deletes it with the name after "--", and reads its reflog by full name.
func TestTidyDeletesABranchNamedLikeAnOption(t *testing.T) {
	f := newTidyFixture(t)
	tgit(t, f.repo, "update-ref", "refs/heads/-x", "main")
	rep := scanTidy(t, f.repo, nil, later())
	tidyApply(context.Background(), f.repo, rep, tidyOpts{worktrees: true, branches: true})
	if strings.Contains(tgit(t, f.repo, "for-each-ref", "refs/heads/-x"), "-x") {
		t.Error("the branch named -x is still there")
	}
}

// makeWorktree's callers pass a branch the server or a stored plan named
// (wp next's continuation, a revive): it is refused before any git runs.
func TestMakeWorktreeRefusesAnOptionAsBranchOrFrom(t *testing.T) {
	log := gitShim(t)
	repo := &worktreeRepo{root: t.TempDir(), common: t.TempDir()}
	for _, bad := range optionLike {
		if _, err := makeWorktree(context.Background(), repo, "slug", bad, "", false, func(string, ...any) {}); err == nil {
			t.Errorf("branch %q was accepted", bad)
		}
		if _, err := makeWorktree(context.Background(), repo, "slug", "", bad, false, func(string, ...any) {}); err == nil {
			t.Errorf("from %q was accepted", bad)
		}
	}
	noGitRan(t, log)
}

// A pull request's refs come from the forge: a head or base named like an
// option is never fetched, and the base is refused.
func TestPRFilesNeverGivesGitAForgeRefThatIsAnOption(t *testing.T) {
	log := gitShim(t)
	for _, bad := range optionLike {
		if _, err := prFilesOf(t.TempDir(), 5, &prDetail{HeadRef: bad, BaseRef: bad}); err == nil {
			t.Errorf("a pull request with head and base %q was read", bad)
		}
	}
	if b, err := os.ReadFile(log); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.Contains(l, "--output") || strings.Contains(l, "--upload-pack") || strings.Contains(l, " -p") || strings.Contains(l, "main") {
				t.Errorf("git was given a forge ref that is an option: %s", l)
			}
		}
	}
}

// With the head fetched, only the base is left to refuse: a git that answers
// every command must still never be given it.
func TestPRFilesRefusesABaseThatIsAnOptionOnceTheHeadIsFetched(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran")
	shim := "#!/bin/sh\necho \"$@\" >> " + log + "\necho abc123\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil { // #nosec G306 -- a test shim must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, bad := range optionLike {
		if _, err := prFilesOf(t.TempDir(), 5, &prDetail{HeadRef: "topic", BaseRef: bad}); err == nil {
			t.Errorf("a pull request based on %q was read", bad)
		}
	}
	b, _ := os.ReadFile(log)
	for _, line := range strings.Split(string(b), "\n") {
		for _, arg := range strings.Fields(line) {
			for _, bad := range optionLike[:3] {
				if arg == strings.SplitN(bad, "\n", 2)[0] || strings.TrimPrefix(arg, "origin/") == bad {
					t.Errorf("git was given the base %q: %s", bad, line)
				}
			}
		}
	}
}

// --base reaches git only as a branch: one that is an option gives no base.
func TestBaseRefAndMergeBaseNeverGiveGitAnOption(t *testing.T) {
	log := gitShim(t)
	for _, bad := range optionLike {
		if got := baseRef(bad); got != "" {
			t.Errorf("baseRef(%q) = %q", bad, got)
		}
		(&cli{err: &bytes.Buffer{}}).mergeBaseIfForkedTwice("origin/" + bad)
	}
	if b, err := os.ReadFile(log); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.HasPrefix(l, "fetch") {
				t.Errorf("git fetched a base that is an option: %s", l)
			}
			if strings.Contains(l, "rev-parse") && strings.Contains(l, "--output") {
				t.Errorf("git read a base that is an option: %s", l)
			}
		}
	}
}

// The kickoff a runner composes for a session job is noted to the server
// before the session is started with it as its argv prompt.
func TestRunnerSessionKickoffIsNotedAsTypedStart(t *testing.T) {
	var mu sync.Mutex
	var noted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/api/v1/sessions/typed-start" {
			mu.Lock()
			noted += string(b)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "t")
	old := runWpNext
	t.Cleanup(func() { runWpNext = old })
	runWpNext = func(context.Context, string, []string, string, string) (string, error) { return "THE-ANSWER", nil }
	fakeCLI(t, "claude", fakeClaudeBG)
	dir := t.TempDir()
	p := newTestProc(t, dir)
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	p.c = c
	p.clis = []runnerCLI{{Name: "claude"}}
	o := p.execSession(context.Background(), newRun(time.Minute), sessionJob{Dir: dir, Task: "APP-1", Kickoff: "KICKOFF-TEMPLATE"})
	if o.State != "done" {
		t.Fatalf("outcome = %+v", o)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(noted, "KICKOFF-TEMPLATE") {
		t.Errorf("the kickoff was not noted to the server: %q", noted)
	}
}
