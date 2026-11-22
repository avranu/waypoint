package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Pictures an agent may look at without being asked about (WP-1137). Claude
// Code asks before a Read outside the session's directories, and agents look
// at screenshots in the temp directory all the time: their own (wp shot,
// Playwright into the scratchpad), their task's and related tasks'
// attachments (wp files), and pictures anywhere in the project. The nudge
// hook answers "allow" for an image file in one of these places; anything
// else is left to Claude Code's own rules. Reads files, runs nothing.

// pictureReadAllowed: path is an image the session may look at unasked:
//   - in the session's repository or another worktree of it (its cwd when
//     it is in none);
//   - under a temp directory, a file of the user's in wp-shots (wp shot) or
//     wp-files (wp files: any task's attachments), or in a Claude Code
//     scratchpad of the same project (claude-UID/PROJECT/SESSION/scratchpad).
func pictureReadAllowed(cwd, path string) bool {
	if !filepath.IsAbs(path) || !shotExts[strings.ToLower(filepath.Ext(path))] {
		return false
	}
	p := realPath(path)
	if !shotExts[strings.ToLower(filepath.Ext(p))] { // a link to something else
		return false
	}
	s := newGuardSite(cwd)
	if s.has {
		if within(s.repo.root, p) {
			return true
		}
		if r, ok := findRepo(p); ok && r.common == s.repo.common {
			return true
		}
	} else if filepath.IsAbs(cwd) && within(s.cwd, p) {
		return true
	}
	for _, t := range guardTempDirs() {
		rel, ok := strings.CutPrefix(p, strings.TrimRight(realPath(t), "/")+"/")
		if !ok {
			continue
		}
		parts := strings.Split(rel, "/")
		switch {
		case len(parts) >= 2 && (parts[0] == "wp-shots" || parts[0] == "wp-files"):
		case len(parts) >= 5 && parts[0] == "claude-"+strconv.Itoa(os.Getuid()) && parts[3] == "scratchpad" && s.ownsScratchpad(parts[1]):
		default:
			continue
		}
		fi, err := os.Stat(p)
		return err == nil && fi.Mode().IsRegular() && ownedByMe(fi)
	}
	return false
}

// ownsScratchpad: dir, Claude Code's name for the directory a session started
// in, is this session's directory, its repository, or one inside the
// repository's main checkout (a worktree under it).
func (s guardSite) ownsScratchpad(dir string) bool {
	roots := []string{s.cwd}
	if s.has {
		roots = append(roots, s.repo.root)
		if filepath.Base(s.repo.common) == ".git" {
			roots = append(roots, filepath.Dir(s.repo.common))
		}
	}
	for _, r := range roots {
		m := claudeProjectDir(r)
		if dir == m || strings.HasPrefix(dir, m+"-") {
			return true
		}
	}
	return false
}

// claudeProjectDir is the name Claude Code gives a directory under its
// projects and temp directories: every character but a letter or digit
// becomes "-".
func claudeProjectDir(dir string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, dir)
}
