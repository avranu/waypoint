package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// applyConflict is a file that changed since the change was made.
type applyConflict struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// execApply writes (or deletes) files in an allowed work tree. It writes
// nothing unless every file is still as the change expects: its current
// blob equals the job's base_sha (or the file doesn't exist, for ""). Then
// it stages the files, commits if asked and pushes if asked.
func (p *runnerProc) execApply(ctx context.Context, run *jobRun, in applyJob) outcome {
	dir, err := resolveDir(in.Dir, p.allowed)
	if err != nil {
		return failed("%v", err)
	}
	if len(in.Files) == 0 {
		return failed("no files to apply")
	}
	inRepo := false
	if v, err := p.git(ctx, dir, "rev-parse", "--is-inside-work-tree"); err == nil && v == "true" {
		inRepo = true
	}
	if (in.Commit != "" || in.Push) && !inRepo {
		return failed("%s is not a git repository, so it can't commit or push", in.Dir)
	}
	if in.Commit != "" {
		// The commit takes what is staged, so other staged work would ride along.
		if staged, err := p.git(ctx, dir, "diff", "--cached", "--name-only"); err != nil {
			return failed("%v", err)
		} else if staged != "" {
			return failed("the repository has staged changes; commit or unstage them first")
		}
	}

	type step struct {
		fileChange
		full    string
		exists  bool
		mode    os.FileMode
		current []byte
		skip    bool // already as wanted
	}
	var steps []*step
	var conflicts []applyConflict
	seen := map[string]bool{}
	for _, f := range in.Files {
		full, err := withinDir(dir, f.Path)
		if err != nil {
			return failed("%v", err)
		}
		if seen[full] {
			return failed("%s appears twice", f.Path)
		}
		seen[full] = true
		s := &step{fileChange: f, full: full, mode: 0o644}
		if fi, err := os.Lstat(full); err == nil {
			if !fi.Mode().IsRegular() {
				return failed("%s is not a regular file", f.Path)
			}
			s.exists, s.mode = true, fi.Mode().Perm()
			if s.current, err = os.ReadFile(full); err != nil { // #nosec G304 -- inside an allowed directory, checked by withinDir
				return failed("reading %s: %v", f.Path, err)
			}
		}
		switch {
		case f.Delete && !s.exists:
			if f.BaseSHA != "" {
				conflicts = append(conflicts, applyConflict{f.Path, "already deleted"})
			}
			s.skip = true
		case f.Delete && f.BaseSHA == "":
			conflicts = append(conflicts, applyConflict{f.Path, "no base to check the deletion against"})
		case !f.Delete && s.exists && bytes.Equal(s.current, []byte(f.Content)):
			s.skip = true // applied already (a retry)
		case f.BaseSHA == "" && s.exists:
			conflicts = append(conflicts, applyConflict{f.Path, "exists already"})
		case f.BaseSHA != "" && !s.exists:
			conflicts = append(conflicts, applyConflict{f.Path, "was deleted"})
		case f.BaseSHA != "":
			blob, err := p.git(ctx, dir, "hash-object", "--", full)
			if err != nil {
				return failed("%v", err)
			}
			if blob != f.BaseSHA {
				conflicts = append(conflicts, applyConflict{f.Path, "changed"})
			}
		}
		steps = append(steps, s)
	}
	if len(conflicts) > 0 {
		names := make([]string, len(conflicts))
		for i, c := range conflicts {
			names[i] = c.Path + " (" + c.Reason + ")"
			fmt.Fprintf(run.out, "conflict: %s %s\n", c.Path, c.Reason)
		}
		o := failed("Not applied, nothing was written: %s differ%s from what the change was made against.", strings.Join(names, ", "), map[bool]string{true: "s", false: ""}[len(names) == 1])
		o.Result = map[string]any{"conflicts": conflicts}
		return o
	}

	var written []string
	for _, s := range steps {
		if s.skip {
			if s.Delete || !s.exists {
				continue
			}
			written = append(written, s.Path) // already in place; still staged and committed
			continue
		}
		if s.Delete {
			if err := os.Remove(s.full); err != nil {
				return failed("deleting %s: %v", s.Path, err)
			}
			fmt.Fprintf(run.out, "deleted %s\n", s.Path)
		} else {
			if err := writeFileAtomic(s.full, []byte(s.Content), s.mode); err != nil {
				return failed("writing %s: %v", s.Path, err)
			}
			fmt.Fprintf(run.out, "wrote %s\n", s.Path)
		}
		written = append(written, s.Path)
	}
	result := map[string]any{"files": written}
	if inRepo && len(written) > 0 {
		args := append([]string{"add", "-A", "--"}, written...)
		if _, err := p.git(ctx, dir, args...); err != nil {
			o := failed("The files are written but git add failed: %v", err)
			o.Result = result
			return o
		}
	}
	if in.Commit != "" && len(written) > 0 && p.nothingStaged(ctx, dir) {
		// Already applied and committed (a retry): git commit would fail on nothing.
		fmt.Fprintf(run.out, "nothing to commit: the files are already as wanted\n")
	} else if in.Commit != "" && len(written) > 0 {
		sha, err := p.commit(ctx, dir, in.Commit, false)
		if err != nil {
			o := failed("The files are written and staged but the commit failed: %v", err)
			o.Result = result
			return o
		}
		result["commit"] = sha
		fmt.Fprintf(run.out, "committed %s\n", sha[:min(len(sha), 12)])
	}
	if in.Push && result["commit"] != nil {
		if err := p.push(ctx, dir); err != nil {
			o := failed("Committed %s but the push failed: %v", result["commit"], err)
			o.Result = result
			return o
		}
		result["pushed"] = true
		fmt.Fprintf(run.out, "pushed\n")
	}
	return outcome{State: "done", Result: result}
}

// nothingStaged reports whether the index matches HEAD. When git can't say
// it answers false, so the commit runs and fails loudly.
func (p *runnerProc) nothingStaged(ctx context.Context, dir string) bool {
	staged, err := p.git(ctx, dir, "diff", "--cached", "--name-only")
	return err == nil && staged == ""
}

// writeFileAtomic replaces path through a temporary file next to it, so an
// interrupted write never leaves half a file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- ordinary project directories
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wp-apply-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone already after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
