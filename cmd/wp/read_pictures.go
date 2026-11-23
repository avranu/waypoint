package main

import (
	"os"
	"path/filepath"
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
//   - under a temp directory, in wp's own directories there: wp-shots (wp
//     shot) and wp-files (wp files: any task's attachments). wp shot --out
//     in the project is the project's; anywhere else, and a directory of
//     either name made by anything else, is left to Claude Code;
//   - under a temp directory, in a scratchpad of a Claude Code session
//     started at the top of one of the repository's checkouts
//     (claude-UID/PROJECT/SESSION/scratchpad, the path Claude Code names to
//     each session as its scratchpad).
//
// And only where no one else can swap it for another file before the Read
// (pathPrivate). Outside any repository only wp's temp directories are:
// Claude Code already lets a session read its own directory. Windows is left
// to Claude Code (ownedByMe).
func pictureReadAllowed(cwd, path string) bool {
	if !filepath.IsAbs(path) || !isPicture(path) {
		return false
	}
	// The file the Read will open, through links no one else can re-point
	// before it does (fixedPath): the temp directory and the user's projects
	// are often reached through one (macOS's /var is /private/var).
	p, ok := fixedPath(path)
	if !ok || !isPicture(p) { // a link to something else
		return false
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || !ownedByMe(fi) || !pathPrivate(p, fi) {
		return false
	}
	s := newGuardSite(cwd)
	if s.has && !s.repoHoldsHome() && s.ownRepo(p) {
		return true
	}
	for _, t := range guardTempDirs() {
		rel, ok := strings.CutPrefix(p, strings.TrimRight(realPath(t), "/")+"/")
		if !ok {
			continue
		}
		parts := strings.Split(rel, "/")
		if len(parts) >= 2 && (parts[0] == shotsDirName || parts[0] == filesDirName) {
			return true
		}
		if s.has && len(parts) >= 5 && parts[0] == "claude-"+strconv.Itoa(os.Getuid()) && parts[3] == "scratchpad" && s.ownsScratchpad(parts[1], cwd) {
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

// fixedPath is p with its links followed, and "true" when no one but the
// user and root can re-point any of them before the Read: each sits where
// pathPrivate says no one else can replace it. ".." is taken from the
// directory a link led to, as the Read will, not from the path as written.
// A path that doesn't exist, or loops, gives false.
func fixedPath(p string) (string, bool) {
	cur, rest := "/", strings.Split(p, "/")
	for links := 0; len(rest) > 0; {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur) // cur has no links in it
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if err != nil {
			return "", false
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 40 || !pathPrivate(next, fi) {
			return "", false
		}
		t, err := os.Readlink(next)
		if err != nil {
			return "", false
		}
		if filepath.IsAbs(t) {
			cur = "/"
		}
		rest = append(strings.Split(t, "/"), rest...)
	}
	return cur, true
}

// ownsScratchpad: dir, Claude Code's name for the directory a session started
// in (projectDirName), is the top of one of this session's checkouts, by the
// path it was reached through (cwd, as the hook was told it) or with its
// links followed. A session started in a subdirectory of one, this one
// included, is left to Claude Code: its name can't be told from a sibling
// project's (/x/app/web and /x/app-web are both -x-app-web).
func (s guardSite) ownsScratchpad(dir, cwd string) bool {
	for _, r := range s.checkouts() {
		if dir == projectDirName(r) || dir == projectDirName(realPath(r)) {
			return true
		}
		if filepath.IsAbs(cwd) && realPath(cwd) == realPath(r) && dir == projectDirName(filepath.Clean(cwd)) {
			return true
		}
	}
	return false
}

// checkouts are, in a repository, its top-level, the main checkout and every
// worktree git has registered, read from the common git directory without
// running git; outside one, none. Not the session's own directory: below
// the top-level it is a subdirectory (ownsScratchpad).
func (s guardSite) checkouts() []string {
	if !s.has {
		return nil
	}
	out := []string{s.repo.root}
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
