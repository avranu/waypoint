package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Pictures an agent may look at without being asked about (WP-1137). Claude
// Code asks before a Read outside the session's directory, and agents look
// at screenshots elsewhere all the time: their own (wp shot; Playwright into
// the scratchpad Claude Code gives each session), their task's and related
// tasks' attachments (wp files), and pictures in the project's other
// checkouts. The nudge hook answers "allow" for an image file in one of
// these places; anything else is left to Claude Code's own rules. Reads
// files, runs nothing.

const (
	shotsDirName = "wp-shots" // wp shot's pictures: under the temp directory, or under --out
	filesDirName = "wp-files" // wp files' downloads: under the temp directory, a folder per task
	// runnerJobEnv marks a runner's Claude Code job (claudeDriver): its
	// grants are the job's, and other jobs' checkouts and downloads share
	// its machine, so no picture is let through for it.
	runnerJobEnv = "WAYPOINT_RUNNER_JOB"
)

// pictureReadAllowed: path is an image file of the user's that the session
// may look at unasked:
//   - in the session's repository or another worktree of it, unless that
//     repository holds the home directory (dotfiles are not a project);
//   - in a wp-shots directory (wp shot, with or without --out);
//   - under a temp directory, in wp-files (wp files: any task's
//     attachments), or in a scratchpad of a Claude Code session started in
//     one of the repository's checkouts (claude-UID/PROJECT/SESSION/scratchpad,
//     the path Claude Code names to each session as its scratchpad).
//
// Outside any repository nothing is: Claude Code already lets a session read
// its own directory. Windows is left to Claude Code (ownedByMe).
func pictureReadAllowed(cwd, path string) bool {
	if !filepath.IsAbs(path) || !isPicture(path) {
		return false
	}
	// Only the exact file the Read will open: a path with a link in it could
	// be re-pointed between this check and the Read.
	p := realPath(path)
	if p != filepath.Clean(path) {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || !ownedByMe(fi) {
		return false
	}
	s := newGuardSite(cwd)
	if s.has && !s.repoHoldsHome() && s.ownRepo(p) {
		return true
	}
	if slices.Contains(strings.Split(filepath.Dir(p), "/"), shotsDirName) {
		return true
	}
	for _, t := range guardTempDirs() {
		rel, ok := strings.CutPrefix(p, strings.TrimRight(realPath(t), "/")+"/")
		if !ok {
			continue
		}
		parts := strings.Split(rel, "/")
		if len(parts) >= 2 && parts[0] == filesDirName {
			return true
		}
		if len(parts) >= 5 && parts[0] == "claude-"+strconv.Itoa(os.Getuid()) && parts[3] == "scratchpad" && s.ownsScratchpad(parts[1]) {
			return true
		}
	}
	return false
}

func isPicture(p string) bool { return shotExts[strings.ToLower(filepath.Ext(p))] }

// repoHoldsHome: the session's repository is the home directory or above it.
func (s guardSite) repoHoldsHome() bool {
	return s.home != "" && within(s.repo.root, realPath(s.home))
}

// ownsScratchpad: dir, Claude Code's name for the directory a session started
// in (projectDirName), is one of this session's checkouts. A session started
// in a subdirectory of one is left to Claude Code: its name can't be told
// from a sibling project's (/x/app/web and /x/app-web are both -x-app-web).
func (s guardSite) ownsScratchpad(dir string) bool {
	for _, r := range s.checkouts() {
		if dir == projectDirName(r) {
			return true
		}
	}
	return false
}

// checkouts are the session's directory and, in a repository, its top-level,
// the main checkout and every worktree git has registered, read from the
// common git directory without running git.
func (s guardSite) checkouts() []string {
	out := []string{s.cwd}
	if !s.has {
		return out
	}
	out = append(out, s.repo.root)
	if filepath.Base(s.repo.common) == ".git" {
		out = append(out, filepath.Dir(s.repo.common))
	}
	wts := filepath.Join(s.repo.common, "worktrees")
	ents, _ := os.ReadDir(wts)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(wts, e.Name(), "gitdir")) // #nosec G304 -- inside the repository's own git directory
		if err != nil {
			continue
		}
		g := strings.TrimSpace(string(b)) // the worktree's .git file
		if !filepath.IsAbs(g) {
			g = filepath.Join(wts, e.Name(), g)
		}
		out = append(out, filepath.Dir(filepath.Clean(g)))
	}
	return out
}
