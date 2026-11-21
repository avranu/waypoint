package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
