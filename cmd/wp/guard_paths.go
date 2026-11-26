package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// guardRepo is a repository found from a directory: its top-level and the git
// directory it shares with its worktrees.
type guardRepo struct {
	root   string
	common string
}

// findRepo is the repository dir is in, found by walking up to a .git.
func findRepo(dir string) (guardRepo, bool) {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		g := filepath.Join(d, ".git")
		if fi, err := os.Lstat(g); err == nil {
			if r, ok := repoAt(d, g, fi.IsDir()); ok {
				return r, true
			}
		}
		if d == filepath.Dir(d) {
			return guardRepo{}, false
		}
	}
}

// repoAt reads the .git at g (a directory, or a file that points at a
// worktree's directory) without running git.
func repoAt(root, g string, isDir bool) (guardRepo, bool) {
	gitdir := g
	if !isDir {
		b, err := os.ReadFile(g) // #nosec G304 -- a .git file found by walking up from the hook's cwd
		if err != nil {
			return guardRepo{}, false
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
		p, ok := strings.CutPrefix(line, "gitdir:")
		if !ok {
			return guardRepo{}, false
		}
		gitdir = strings.TrimSpace(p)
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(root, gitdir)
		}
	}
	common := gitdir
	if b, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil { // #nosec G304 G703 -- inside the git directory just found
		c := strings.TrimSpace(string(b))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitdir, c)
		}
		common = c
	}
	return guardRepo{root: realPath(root), common: realPath(common)}, true
}

// realPath is p with symbolic links resolved as far as it exists; the rest
// (a file about to be created) is kept as written.
func realPath(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for d := p; ; d = filepath.Dir(d) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(r, rest)
		}
		if d == filepath.Dir(d) {
			return p
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}

// within reports whether p is dir or below it.
func within(dir, p string) bool {
	if dir == "" {
		return false
	}
	return p == dir || strings.HasPrefix(p, strings.TrimRight(dir, "/")+"/")
}

// originHostOf is the host of the origin remote of the repository dir is in,
// read from its config; empty when there is none.
func originHostOf(dir string) string { return remoteHost(originURLOf(dir)) }

// originURLOf is the address of the origin remote of the repository dir is
// in, read from its config; empty when there is none.
func originURLOf(dir string) string {
	r, ok := findRepo(dir)
	if !ok {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(r.common, "config")) // #nosec G304 -- the repository's own config
	if err != nil {
		return ""
	}
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			in = strings.HasPrefix(l, `[remote "origin"]`)
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok && in && strings.TrimSpace(k) == "url" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// claudeHome is Claude Code's config directory.
func claudeHome() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Clean(d)
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".claude")
	}
	return ""
}

// loosensGuard: p is a file that would loosen the guard: any Claude Code
// settings file, the user's settings, or the hooks, commands, agents, skills
// and plugins of the user's Claude Code.
func loosensGuard(p string) bool {
	base := filepath.Base(p)
	if strings.HasPrefix(base, "settings") && strings.HasSuffix(base, ".json") {
		if filepath.Base(filepath.Dir(p)) == ".claude" || filepath.Dir(p) == claudeHome() {
			return true
		}
	}
	if s := os.Getenv("WP_CLAUDE_SETTINGS"); s != "" && p == filepath.Clean(s) {
		return true
	}
	if h, err := os.UserHomeDir(); err == nil && p == filepath.Join(h, ".claude.json") {
		return true
	}
	if ch := claudeHome(); ch != "" {
		for _, d := range []string{"hooks", "commands", "agents", "skills", "plugins"} {
			if within(filepath.Join(ch, d), p) {
				return true
			}
		}
	}
	return false
}

// isWaypointState: the CLI's state files: state.json holds the mark autopilot
// leaves on a session it drives (WP-277); wp itself writes it, never a
// command or the Write tool.
func isWaypointState(p string) bool {
	d, err := configDir()
	if err != nil || d == "" {
		return false
	}
	if abs, err := filepath.Abs(d); err == nil {
		d = abs
	}
	for _, f := range []string{"state.json", pauseFile, "guard_trips.json"} {
		if p == realPath(filepath.Join(d, f)) {
			return true
		}
	}
	return within(realPath(filepath.Join(d, "burn")), p)
}

// hostDirs are where an unattended session changes the host by writing.
var hostDirs = []string{"/etc", "/usr", "/opt", "/var/lib"}

func isHostPath(p string) bool {
	for _, d := range hostDirs {
		if within(d, p) {
			return true
		}
	}
	return false
}

// guardSite knows where a session may write: its repository, from the hook's
// cwd.
type guardSite struct {
	cwd  string
	home string
	repo guardRepo
	has  bool
}

func newGuardSite(cwd string) guardSite {
	s := guardSite{cwd: realPath(cwd)}
	s.home, _ = os.UserHomeDir()
	s.repo, s.has = findRepo(s.cwd)
	return s
}

// resolve is raw as an absolute, cleaned, link-resolved path, from dir; ""
// when it can't be known (it holds a substitution or an unknown variable).
func (s guardSite) resolve(dir, raw string) string {
	switch {
	case strings.Contains(raw, subPlaceholder):
		return ""
	case raw == "~" || strings.HasPrefix(raw, "~/"):
		raw = s.home + raw[1:]
	case strings.HasPrefix(raw, "$HOME"):
		raw = s.home + raw[len("$HOME"):]
	case strings.HasPrefix(raw, "${HOME}"):
		raw = s.home + raw[len("${HOME}"):]
	}
	if raw == "" || strings.Contains(raw, "$") {
		return ""
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(dir, raw)
	}
	return realPath(raw)
}

// foreign: p is inside a repository other than the session's.
func (s guardSite) foreign(p string) bool {
	if within(s.repo.root, p) {
		return false
	}
	r, ok := findRepo(p)
	return ok && (!s.has || r.common != s.repo.common) && !within(s.cwd, p)
}

// guardTempDirs are the scratch directories any session may write to; a
// variable so a test, whose own fixtures live in one, can narrow it.
var guardTempDirs = func() []string { return []string{os.TempDir(), "/tmp", "/var/tmp"} }

// ownRepo: p is in the session's repository or another worktree of it.
func (s guardSite) ownRepo(p string) bool {
	if !s.has {
		return false
	}
	if within(s.repo.root, p) {
		return true
	}
	r, ok := findRepo(p)
	return ok && r.common == s.repo.common
}

// confined: a write to p stays where the session may write.
func (s guardSite) confined(p string) bool {
	if s.has {
		if s.ownRepo(p) {
			return true
		}
	} else if within(s.cwd, p) {
		return true
	}
	for _, t := range guardTempDirs() {
		if within(realPath(t), p) {
			return true
		}
	}
	if ch := claudeHome(); ch != "" {
		if rel, ok := strings.CutPrefix(p, realPath(filepath.Join(ch, "projects"))+"/"); ok {
			parts := strings.Split(rel, "/")
			if len(parts) >= 3 && parts[1] == "memory" {
				return true
			}
		}
	}
	return false
}

// trackedClean says a file is the project's own: its text is what the
// default branch of origin holds at that path (origin/HEAD, else origin/main
// or origin/master), at that branch's tip or at the commit the checkout left
// it at (a worktree behind origin after a fetch). It compares text and reads
// nothing else of git's: a file the session wrote, edited or committed itself
// differs, whatever git status says of it (skip-worktree, ignored), and an
// ignored file holding origin's own text is the project's. A file outside a
// repository is not, nor is any file where no such branch is known. The name
// is given to git as a path, never as a pattern.
//
// This tells apart what a session wrote in the course of its work, and the
// guard refuses the git commands that set a remote-tracking ref by hand
// (forgesRemoteRef). It is no proof against a session that sets out to
// deceive the guard another way (a clean filter, a write under .git, a new
// URL for origin): such a session can as well put its commands in a
// Makefile, which runs unread. Containment holds that line (PL7).
func trackedClean(p string) bool {
	dir, base := filepath.Dir(p), filepath.Base(p)
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- git on the path of the script the command is about to run
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	have := git("hash-object", "--", base)
	if have == "" {
		return false
	}
	for _, ref := range []string{"refs/remotes/origin/HEAD", "refs/remotes/origin/main", "refs/remotes/origin/master"} {
		if git("rev-parse", "-q", "--verify", ref+"^{commit}") == "" {
			continue
		}
		if git("rev-parse", "-q", "--verify", ref+":./"+base) == have {
			return true
		}
		at := git("merge-base", "HEAD", ref)
		return at != "" && git("rev-parse", "-q", "--verify", at+":./"+base) == have
	}
	return false
}
