package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The runner has the last word on where a job may act: it resolves symlinks and refuses "..".

// parseAllowed turns --allow values into absolute, symlink-resolved
// directories that exist. "~" is the home directory.
func parseAllowed(list []string) ([]string, error) {
	var out []string
	for _, d := range list {
		if d == "~" || strings.HasPrefix(d, "~/") || strings.HasPrefix(d, `~\`) {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			d = filepath.Join(home, d[1:])
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("--allow %s: %w", d, err)
		}
		// #nosec G703 -- the directory the person named with --allow
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("--allow %s is not a directory", d)
		}
		if filepath.Dir(real) == real {
			return nil, fmt.Errorf("--allow %s is the whole disk; allow a projects folder instead", d)
		}
		out = append(out, real)
	}
	return out, nil
}

// inside reports whether path is dir or below it, comparing as the file
// system would (case-insensitively on Windows and macOS).
func inside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// resolveDir checks a job's directory: absolute, no "..", an existing
// directory, and (once symlinks are resolved) inside an allowed one. It
// returns the resolved path to run in.
func resolveDir(dir string, allowed []string) (string, error) {
	dir = filepath.FromSlash(dir)
	if !filepath.IsAbs(dir) {
		return "", errors.New("the directory must be an absolute path")
	}
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if seg == ".." {
			return "", errors.New("the directory must not have .. in it")
		}
	}
	if len(allowed) == 0 {
		return "", errors.New("this runner allows no directories (start it with --allow DIR)")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("directory %s: %w", dir, err)
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	for _, a := range allowed {
		if inside(real, a) {
			return real, nil
		}
	}
	return "", fmt.Errorf("%s is outside the directories this runner allows", dir)
}

// withinDir resolves rel (a path from a job, slash separated) under root and
// refuses anything that would leave it: absolute paths, "..", and parents
// or targets that are symlinks pointing elsewhere. root must be resolved.
func withinDir(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, '\\') && runtime.GOOS != "windows" {
		return "", fmt.Errorf("%q is not a path inside the repository", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || seg == "" || seg == "." || strings.EqualFold(seg, ".git") {
			return "", fmt.Errorf("%q is not a path inside the repository", rel)
		}
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	if !inside(full, root) {
		return "", fmt.Errorf("%q is outside the repository", rel)
	}
	parent := filepath.Dir(full)
	for {
		if _, err := os.Lstat(parent); err == nil {
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	real, err := filepath.EvalSymlinks(parent)
	if err != nil || !inside(real, root) {
		return "", fmt.Errorf("%q leads outside the repository (a symbolic link?)", rel)
	}
	if fi, err := os.Lstat(full); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%q is a symbolic link; not writing through it", rel)
	}
	return full, nil
}
