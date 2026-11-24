package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// baseTmpPrefix names the checkouts, so a later run can sweep the ones a
// killed run left.
const baseTmpPrefix = "wp-base-"

// baseRefError is a --base that names nothing to check out: a wrong call
// (exit 2), not a failed command.
type baseRefError string

func (e baseRefError) Error() string { return string(e) }

// baseWorktree checks ref out, detached, in a temporary worktree and
// returns the folder to run in (the current folder's place in it), how to
// remove it, and the commit.
func baseWorktree(ref string) (dir string, cleanup func(), sha string, err error) {
	if sha, err = baseCommit(ref); err != nil {
		return "", nil, "", err
	}
	prefix, err := localGit("rev-parse", "--show-prefix")
	if err != nil {
		return "", nil, "", err
	}
	sweepBaseWorktrees()
	// The pid in the name lets a later run remove a checkout whose wp bench
	// was killed before it could.
	root, err := os.MkdirTemp("", baseTmpPrefix+strconv.Itoa(os.Getpid())+"-")
	if err != nil {
		return "", nil, "", err
	}
	if _, err := localGit("worktree", "add", "--detach", "--quiet", root, sha); err != nil {
		_ = os.RemoveAll(root)
		return "", nil, "", err
	}
	cleanup = func() {
		// Our own temporary checkout: nothing in it is anyone's work.
		_, _ = localGit("worktree", "remove", "--force", root)
		_ = os.RemoveAll(root)
		_, _ = localGit("worktree", "prune")
	}
	return filepath.Join(root, filepath.FromSlash(prefix)), cleanup, sha, nil
}

// sweepBaseWorktrees removes base checkouts left by a wp bench that was
// killed (SIGKILL, OOM) before its cleanup ran: ours by name, its pid gone.
func sweepBaseWorktrees() {
	out, err := localGit("worktree", "list", "--porcelain")
	if err != nil {
		return
	}
	for _, l := range strings.Split(out, "\n") {
		path, ok := strings.CutPrefix(l, "worktree ")
		if !ok {
			continue
		}
		rest, ok := strings.CutPrefix(filepath.Base(path), baseTmpPrefix)
		if !ok || !sameDir(filepath.Dir(path), os.TempDir()) {
			continue
		}
		pidText, _, _ := strings.Cut(rest, "-")
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 || pid == os.Getpid() || processAlive(pid) {
			continue
		}
		_, _ = localGit("worktree", "remove", "--force", path)
		_ = os.RemoveAll(path)
	}
	_, _ = localGit("worktree", "prune")
}

// sameDir: a and b are one folder once symlinks are followed. git reports a
// worktree by its real path, and a TMPDIR is often a link (macOS's
// /var/folders is under /private).
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// baseCommit resolves ref to a commit, or says why it can't (a wrong call).
func baseCommit(ref string) (string, error) {
	if strings.HasPrefix(ref, "-") {
		return "", baseRefError(fmt.Sprintf("--base %q: not a commit", ref))
	}
	if _, err := localGit("rev-parse", "--show-prefix"); err != nil {
		return "", baseRefError("--base needs a git repository; run it inside one")
	}
	sha, err := localGit("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || sha == "" {
		return "", baseRefError(fmt.Sprintf("--base %q: no such commit here (git fetch first?)", ref))
	}
	return sha, nil
}
