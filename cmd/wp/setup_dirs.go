package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// The directories wp setup offers the runner by default: the top-level
// folders where Claude Code has worked on this computer (~/code,
// /projects), from the session folders under ~/.claude/projects. Never the
// whole disk, the home directory, a folder that holds it, system or
// temporary folders, or hidden ones.

// claudeWorkRoots lists those folders, the most used first.
func claudeWorkRoots(claudeDir, home string) []string {
	projects := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return nil
	}
	if home != "" {
		if real, err := filepath.EvalSymlinks(home); err == nil {
			home = real
		}
	}
	count := map[string]int{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cwd := transcriptCwd(filepath.Join(projects, e.Name()))
		if cwd == "" {
			cwd = decodeProjectDir(e.Name())
		}
		root := workRoot(cwd, home)
		if root == "" {
			continue
		}
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			continue
		}
		count[root]++
	}
	roots := make([]string, 0, len(count))
	for r := range count {
		roots = append(roots, r)
	}
	slices.SortFunc(roots, func(a, b string) int {
		if count[a] != count[b] {
			return count[b] - count[a]
		}
		return strings.Compare(a, b)
	})
	return roots
}

// transcriptCwd is the working directory a session folder's newest
// transcripts record (each line of a Claude Code transcript has its cwd).
func transcriptCwd(dir string) string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	type file struct {
		path string
		mod  int64
	}
	var list []file
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
			list = append(list, file{f, fi.ModTime().UnixNano()})
		}
	}
	slices.SortFunc(list, func(a, b file) int { return cmp.Compare(b.mod, a.mod) })
	for i, f := range list {
		if i == 3 {
			break
		}
		if cwd := firstCwd(f.path); cwd != "" {
			return cwd
		}
	}
	return ""
}

// firstCwd reads the first cwd in a transcript's first lines.
func firstCwd(path string) string {
	fh, err := os.Open(path) // #nosec G304 -- a transcript under ~/.claude/projects
	if err != nil {
		return ""
	}
	defer func() { _ = fh.Close() }()
	br := bufio.NewReader(io.LimitReader(fh, 8<<20))
	for range 200 {
		line, err := br.ReadBytes('\n')
		var entry struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &entry) == nil && filepath.IsAbs(entry.Cwd) {
			return entry.Cwd
		}
		if err != nil {
			break
		}
	}
	return ""
}

// decodeProjectDir turns a session folder's name back into a path, as far
// as it exists. Claude Code names it after the working directory with
// every / (and . and _) made a -, so "-home-me-my-app" is /home/me/my-app or
// /home/me/my/app: the file system decides. It returns the deepest folder
// that exists along the way ("" on Windows, where names differ).
func decodeProjectDir(name string) string {
	if runtime.GOOS == "windows" || !strings.HasPrefix(name, "-") {
		return ""
	}
	toks := strings.Split(name[1:], "-")
	best, bestDepth, budget := "", 0, 5000
	var walk func(dir string, depth int, toks []string) bool
	walk = func(dir string, depth int, toks []string) bool {
		if depth > bestDepth {
			best, bestDepth = dir, depth
		}
		if len(toks) == 0 {
			return true
		}
		for k := 1; k <= len(toks) && k <= 6; k++ {
			for _, seg := range segVariants(toks[:k]) {
				if budget--; budget < 0 {
					return false
				}
				if seg == "" || strings.Trim(seg, ".") == "" {
					continue
				}
				p := filepath.Join(dir, seg)
				if fi, err := os.Stat(p); err == nil && fi.IsDir() && walk(p, depth+1, toks[k:]) {
					return true
				}
			}
		}
		return false
	}
	walk(string(filepath.Separator), 0, toks)
	return best
}

// segVariants joins tokens with each of -, . and _ between them.
func segVariants(toks []string) []string {
	out := []string{toks[0]}
	for _, t := range toks[1:] {
		next := make([]string, 0, len(out)*3)
		for _, s := range out {
			for _, sep := range []string{"-", ".", "_"} {
				next = append(next, s+sep+t)
			}
		}
		out = next
	}
	return out
}

// Folders a work root is never made of.
var (
	systemDirs = map[string]bool{
		"tmp": true, "var": true, "etc": true, "usr": true, "bin": true, "sbin": true, "lib": true, "lib32": true,
		"lib64": true, "libx32": true, "proc": true, "sys": true, "dev": true, "run": true, "boot": true, "root": true,
		"private": true, "system": true, "library": true, "applications": true, "cores": true, "nix": true, "snap": true,
		"windows": true, "program files": true, "program files (x86)": true, "programdata": true, "users": true,
		"home": true, "lost+found": true,
	}
	// mountDirs hold drives or packages: the root is one level further down.
	mountDirs = map[string]bool{"mnt": true, "media": true, "volumes": true, "opt": true}
	// homeSkip are folders in the home directory that aren't for projects.
	homeSkip = map[string]bool{"library": true, "appdata": true, "application data": true, "downloads": true, "desktop": true}
)

// workRoot is the top-level folder of cwd that a runner may be allowed:
// ~/X for a folder in the home directory, else /X (or /mnt/X), or "".
func workRoot(cwd, home string) string {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	cwd = filepath.Clean(cwd)
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	sep := string(filepath.Separator)
	if home != "" && inside(cwd, home) {
		rel, err := filepath.Rel(home, cwd)
		if err != nil || rel == "." {
			return ""
		}
		first := strings.Split(rel, sep)[0]
		if strings.HasPrefix(first, ".") || homeSkip[strings.ToLower(first)] {
			return ""
		}
		return filepath.Join(home, first)
	}
	vol := filepath.VolumeName(cwd)
	parts := strings.Split(strings.Trim(cwd[len(vol):], `/\`), sep)
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	first := strings.ToLower(parts[0])
	if strings.HasPrefix(first, ".") || systemDirs[first] {
		return ""
	}
	n := 1
	if mountDirs[first] {
		if len(parts) < 2 || strings.HasPrefix(parts[1], ".") {
			return ""
		}
		n = 2
	}
	root := vol + sep + filepath.Join(parts[:n]...)
	if tmp := os.TempDir(); tmp != "" && inside(root, tmp) {
		return ""
	}
	if home != "" && inside(home, root) {
		return "" // it holds the home directory
	}
	return root
}

// tildePath writes a path in the home directory as ~/….
func tildePath(p, home string) string {
	if home == "" || !inside(p, home) {
		return p
	}
	rel, err := filepath.Rel(home, p)
	if err != nil {
		return p
	}
	if rel == "." {
		return "~"
	}
	return "~" + string(filepath.Separator) + rel
}
