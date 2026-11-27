package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// repoResolver names the repository a session worked in: owner/name from
// the origin remote, read from .git/config without running git (hundreds
// of sessions share a few directories). A directory with no remote, or
// one that has since been deleted (a worktree), falls back to the nearest
// parent that is a checkout, then to the directory's own name.
type repoResolver struct {
	mu    sync.Mutex
	cache map[string]string
}

func newRepoResolver() *repoResolver { return &repoResolver{cache: map[string]string{}} }

func (r *repoResolver) of(cwd string) string {
	if cwd == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.cache[cwd]; ok {
		return v
	}
	v := resolveRepo(cwd)
	r.cache[cwd] = v
	return v
}

func resolveRepo(cwd string) string {
	dir := filepath.Clean(cwd)
	for d := dir; ; d = filepath.Dir(d) {
		if cfg := gitConfigFor(d); cfg != "" {
			if remote := originURL(cfg); remote != "" {
				if repo, err := repoFromRemote(remote); err == nil {
					return repo
				}
			}
			return filepath.Base(d)
		}
		if p := filepath.Dir(d); p == d {
			break
		}
	}
	if isRunnerJobDir(dir) {
		return ""
	}
	return filepath.Base(dir)
}

// isRunnerJobDir reports whether dir is an ephemeral runner job worktree
// (…/waypoint/runner/work/job1234-56789) whose checkout has been removed, so
// we don't pollute repository reports with transient job directory names.
func isRunnerJobDir(dir string) bool {
	base := filepath.Base(dir)
	if !strings.HasPrefix(base, "job") {
		return false
	}
	rest, tail, ok := strings.Cut(base[3:], "-")
	if !ok || rest == "" || tail == "" {
		return false
	}
	for _, r := range rest + tail {
		if r < '0' || r > '9' {
			return false
		}
	}
	return strings.Contains(filepath.ToSlash(dir), "/runner/work/")
}

// gitConfigFor finds the config of the checkout rooted at dir ("" when
// dir isn't one); a worktree's .git file leads to the main repository's.
func gitConfigFor(dir string) string {
	g := filepath.Join(dir, ".git")
	fi, err := os.Stat(g)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		return filepath.Join(g, "config")
	}
	b, err := os.ReadFile(g) // #nosec G304 -- a .git file next to a transcript's working directory
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	if c, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil { // #nosec G304 G703 -- inside the worktree's git directory
		common := strings.TrimSpace(string(c))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
		return filepath.Join(common, "config")
	}
	return filepath.Join(gitdir, "config")
}

// originURL reads remote "origin"'s url (or the first remote's) from a git config.
func originURL(path string) string {
	f, err := os.Open(path) // #nosec G304 -- a git config of a checkout
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	var section, first string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(line)
			continue
		}
		if !strings.HasPrefix(section, "[remote ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "url" {
			continue
		}
		v = strings.TrimSpace(v)
		if section == `[remote "origin"]` {
			return v
		}
		if first == "" {
			first = v
		}
	}
	return first
}
