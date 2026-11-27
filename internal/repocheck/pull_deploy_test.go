package repocheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// deploy/pull-deploy.sh takes the forge and repo from its mirror's origin
// when its settings don't name them: only from a URL (scheme://host/owner/
// name). An ssh origin (git@host:owner/name) would make a forge URL that no
// API call reaches, so the deployer says what to set instead. Without its
// token it stops before git reaches the forge.
func TestPullDeployForgeFromOrigin(t *testing.T) {
	for _, tool := range []string{"bash", "git", "flock"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	script, err := filepath.Abs("../../deploy/pull-deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	for origin, want := range map[string]string{
		"https://git.example.net/robin/waypoint.git": "no " + "CONF/forge-token",
		"git@git.example.net:robin/waypoint.git":     "no forge: set FORGE_URL and REPO",
	} {
		dir := t.TempDir()
		conf, state := filepath.Join(dir, "conf"), filepath.Join(dir, "state")
		mirror := filepath.Join(state, "repo.git")
		for _, args := range [][]string{{"init", "-q", "--bare", mirror}, {"--git-dir", mirror, "remote", "add", "origin", origin}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		if err := os.MkdirAll(conf, 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", script)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "WAYPOINT_DEPLOYER_CONF=" + conf, "WAYPOINT_DEPLOYER_STATE=" + state,
			"WAYPOINT_DEPLOY_ROOT=" + dir, "WAYPOINT_DEPLOYER_BIN=" + dir}
		out, err := cmd.CombinedOutput()
		got := strings.ReplaceAll(string(out), conf, "CONF")
		if err == nil || !strings.Contains(got, want) || strings.Contains(got, "fatal:") {
			t.Errorf("origin %s: %v\n%s\nwant %q", origin, err, got, want)
		}
	}
}

// The deployer's one-run-at-a-time lock is held on fd 8. A child that outlives
// the run (git fetch can start a detached `git gc --auto`) must not inherit it,
// or it holds the lock after the run ends and the next run exits silently.
// Here a hook that git runs during the fetch starts a background sleep.
func TestPullDeployLockNotHeldByBackgroundChild(t *testing.T) {
	for _, tool := range []string{"bash", "git", "flock", "sleep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	script, err := filepath.Abs("../../deploy/pull-deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	conf, state, origin := filepath.Join(dir, "conf"), filepath.Join(dir, "state"), filepath.Join(dir, "origin.git")
	mirror := filepath.Join(state, "repo.git")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.net", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.net")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	work := filepath.Join(dir, "work")
	git("init", "-q", "-b", "main", work)
	git("-C", work, "commit", "-q", "--allow-empty", "-m", "one")
	git("clone", "-q", "--bare", work, origin)
	git("init", "-q", "--bare", mirror)
	git("--git-dir", mirror, "remote", "add", "origin", origin)
	for _, d := range []string{conf, filepath.Join(mirror, "hooks")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(conf, "forge-token"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\nsleep 5 >/dev/null 2>&1 &\n"
	if err := os.WriteFile(filepath.Join(mirror, "hooks", "reference-transaction"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "WAYPOINT_DEPLOYER_CONF=" + conf, "WAYPOINT_DEPLOYER_STATE=" + state,
		"WAYPOINT_DEPLOY_ROOT=" + dir, "WAYPOINT_DEPLOYER_BIN=" + dir, "FORGE_URL=" + dir, "REPO=origin"}
	out, _ := cmd.CombinedOutput() // the run may stop after the fetch; only the lock matters
	if ref, err := exec.Command("git", "--git-dir", mirror, "rev-parse", "--verify", "-q", "refs/heads/main").Output(); err != nil || len(ref) == 0 {
		t.Fatalf("the fetch did not happen, so the hook did not run: %v\n%s", err, out)
	}
	if out, err := exec.Command("flock", "-n", filepath.Join(state, ".lock"), "true").CombinedOutput(); err != nil {
		t.Errorf("the lock is still held after the run ended: %v %s", err, out)
	}
}
