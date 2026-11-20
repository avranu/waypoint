package cilint

import (
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// RuleScriptNotExec: a script that a workflow step or Makefile recipe runs by
// path is committed without the executable bit, so the run dies with
// "Permission denied" (WP-950).
const RuleScriptNotExec = "ci-script-not-executable"

// ModeFunc returns the git index mode ("100755") of a repo-relative path, or
// "" when the path is not tracked. Callers fill it from `git ls-files -s`, so
// the rule itself reads no files and runs no commands.
type ModeFunc func(path string) string

// IsMakefilePath says whether a path is a Makefile or *.mk at the repo root.
func IsMakefilePath(p string) bool {
	p = filepath.ToSlash(p)
	if strings.Contains(p, "/") {
		return false
	}
	return p == "Makefile" || p == "makefile" || p == "GNUmakefile" || strings.HasSuffix(p, ".mk")
}

// cmdSplit cuts a shell line where a new command starts.
var cmdSplit = lazyre.New(`;|&&|\|\||\||\$\(|` + "`")

var shellKeywords = map[string]bool{"then": true, "do": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "!": true}

// ScriptModes finds commands run by repo path in a workflow's run blocks (name
// is a workflow path) or a Makefile's recipes (name is Makefile or *.mk) and
// reports each tracked file whose mode is not 100755.
func ScriptModes(name string, src []byte, mode ModeFunc) []Finding {
	name = filepath.ToSlash(name)
	var out []Finding
	seen := map[string]bool{}
	dir := ""
	check := func(line int, cmdline string) {
		paths, nd := scriptPaths(cmdline, dir)
		if !IsMakefilePath(name) {
			dir = nd // a run block is one shell, so cd carries to later lines
		}
		for _, p := range paths {
			m := mode(p)
			if m == "" || m == "100755" || m == "120000" || seen[p] {
				continue
			}
			seen[p] = true
			fix := "git update-index --chmod=+x " + p
			if strings.HasPrefix(p, ".forgejo/") {
				fix += " (files under .forgejo/ are protected: wp request run \"git update-index --chmod=+x " + p + " && git commit ...\", since change requests carry no modes)"
			}
			out = append(out, Finding{Rule: RuleScriptNotExec, File: name, Line: line,
				Msg: fmt.Sprintf("%s is run by path but its git mode is %s, not 100755, so it fails with Permission denied.", p, m),
				Fix: fix})
		}
	}
	switch {
	case IsMakefilePath(name):
		for i, l := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(l, "\t") {
				check(i+1, strings.TrimLeft(l, "\t @-+"))
			}
		}
	case IsWorkflowPath(name):
		var root yaml.Node
		if yaml.Unmarshal(src, &root) != nil {
			return nil
		}
		// wd is the directory a run block starts in: the step's
		// working-directory, else the nearest defaults.run.working-directory.
		var walk func(n *yaml.Node, wd string)
		walk = func(n *yaml.Node, wd string) {
			if n.Kind == yaml.MappingNode {
				if d, ok := workingDir(n); ok {
					wd = d
				}
				for i := 0; i+1 < len(n.Content); i += 2 {
					k, v := n.Content[i], n.Content[i+1]
					if k.Value == "run" && v.Kind == yaml.ScalarNode {
						dir = wd
						first := v.Line
						if v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
							first++
						}
						for j, l := range strings.Split(v.Value, "\n") {
							check(first+j, strings.TrimSpace(l))
						}
						continue
					}
					walk(v, wd)
				}
				return
			}
			for _, c := range n.Content {
				walk(c, wd)
			}
		}
		walk(&root, "")
	}
	return out
}

// unknownDir marks a directory the rule can't resolve (an expression, an
// absolute path, a cd it can't follow); paths run from it are not checked.
const unknownDir = "\x00"

// joinDir resolves a cd or working-directory target against dir.
func joinDir(dir, to string) string {
	to = strings.Trim(to, `"'`)
	if dir == unknownDir || to == "" || strings.ContainsAny(to, "$*?[{}()<>=~") || strings.HasPrefix(to, "/") || strings.HasPrefix(to, "-") {
		return unknownDir
	}
	d := path.Join(dir, to)
	if d == "." {
		return ""
	}
	if d == ".." || strings.HasPrefix(d, "../") {
		return unknownDir
	}
	return d
}

// workingDir returns the working-directory a mapping sets for the run blocks
// under it: a step's own key, or a workflow's or job's defaults.run. A
// relative one is from the repo root, never from an outer default.
func workingDir(n *yaml.Node) (string, bool) {
	if v := mapValue(n, "working-directory"); v != nil && v.Kind == yaml.ScalarNode {
		return joinDir("", v.Value), true
	}
	if v := mapValue(mapValue(mapValue(n, "defaults"), "run"), "working-directory"); v != nil && v.Kind == yaml.ScalarNode {
		return joinDir("", v.Value), true
	}
	return "", false
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// scriptPaths returns the repo-relative paths a shell line runs directly: the
// first word of each command, when it is ./x or contains a slash. Commands
// through an interpreter name the interpreter first, so they never match.
// dir is the directory (repo-relative, "" for the root, unknownDir when it
// can't be resolved) the line starts in; a `cd sub &&` moves later commands
// into sub. The final directory is
// returned so a run block can carry it to its next line.
func scriptPaths(line, dir string) ([]string, string) {
	var out []string
	for _, cmd := range cmdSplit.Split(line, -1) {
		f := strings.Fields(cmd)
		for len(f) > 0 && shellKeywords[f[0]] {
			f = f[1:]
		}
		if len(f) == 0 {
			continue
		}
		if f[0] == "cd" {
			if len(f) == 2 {
				dir = joinDir(dir, f[1])
			} else {
				dir = unknownDir
			}
			continue
		}
		if dir == unknownDir {
			continue
		}
		w := strings.TrimRight(strings.Trim(f[0], `"'`), `)"'`)
		if strings.ContainsAny(w, "$*?[{}()<>=") || strings.HasPrefix(w, "/") || strings.HasPrefix(w, "~") || !strings.Contains(w, "/") {
			continue
		}
		w = path.Clean(path.Join(dir, w))
		if w == "." || strings.HasPrefix(w, "../") {
			continue
		}
		out = append(out, w)
	}
	return out, dir
}

// ParseLsFiles turns `git ls-files -s` output ("100755 <sha> 0\tpath" per
// line) into a ModeFunc.
func ParseLsFiles(out string) ModeFunc {
	modes := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		meta, p, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		if f := strings.Fields(meta); len(f) > 0 {
			if strings.HasPrefix(p, `"`) { // git C-quotes unusual paths
				if u, err := strconv.Unquote(p); err == nil {
					p = u
				}
			}
			modes[p] = f[0]
		}
	}
	return func(p string) string { return modes[p] }
}
