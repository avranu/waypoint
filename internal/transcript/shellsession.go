package transcript

import (
	"path"
	"regexp"
	"strings"
)

// ShellEditKind is the kind of the per-hour shell-edit count the client
// uploads in tool_hours: one row (tool "Bash") for every hour a session ran
// Bash, with calls = the Bash calls that wrote a file of the work. The server
// reads the same kind back.
const ShellEditKind = "shell_edit"

// shellMaxRoots bounds the directories a session remembers working in.
const shellMaxRoots = 256

// ShellSession is what the server remembers of one transcript file while it
// reads its Bash calls: the directories the session has worked in, and the
// scripts it kept in files. Shell edits are judged against them, because the
// server cannot see the disk the session ran on but does see each record's
// working directory.
type ShellSession struct {
	Roots   []string
	Scripts ShellScripts
}

var shellWorktreeRE = regexp.MustCompile(`^(.*?)/(\.claude/worktrees|\.worktrees)/([^/]+)(/.*)?$`)

// Note records a working directory the session was seen in: the directory,
// and for one inside a worktree (X/.claude/worktrees/NAME or X/.worktrees/NAME)
// the worktree's root and the main checkout X, since a session in a worktree
// names the main checkout's files by the same paths.
func (s *ShellSession) Note(cwd string) {
	if cwd == "" || !strings.HasPrefix(cwd, "/") {
		return
	}
	cwd = path.Clean(cwd)
	s.add(cwd)
	if m := shellWorktreeRE.FindStringSubmatch(cwd); m != nil {
		s.add(m[1] + "/" + m[2] + "/" + m[3])
		s.add(m[1])
	}
}

func (s *ShellSession) add(dir string) {
	if dir == "" || dir == "/" || len(s.Roots) >= shellMaxRoots {
		return
	}
	for _, r := range s.Roots {
		if r == dir {
			return
		}
	}
	s.Roots = append(s.Roots, dir)
}

// KeepWrite remembers a script the session saved with the Write tool, to
// match with a later command that runs it.
func (s *ShellSession) KeepWrite(file, cwd, content string) {
	s.Scripts.Keep(ShellAt(file, cwd), content)
}

func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// shellHomeRE is a home directory by where operating systems put them.
var shellHomeRE = regexp.MustCompile(`^(/home/[^/]+|/Users/[^/]+|/root)(/|$)`)

// ShellHomeOf infers the home directory of whoever ran a session from its
// working directory; "" when the directory is not under a home.
func ShellHomeOf(cwd string) string {
	if m := shellHomeRE.FindStringSubmatch(cwd); m != nil {
		return m[1]
	}
	return ""
}

// shellInClaudeDir says a path is Claude Code's own (its settings, plans and
// memory under ~/.claude, other than the worktrees it keeps there) or a
// cache, which hold no file of the work.
func shellInClaudeDir(p string) bool {
	if strings.Contains(p, "/.cache/") {
		return true
	}
	if m := shellHomeRE.FindStringSubmatch(p); m != nil {
		if rest, ok := strings.CutPrefix(p[len(m[1]):], "/.claude/"); ok {
			return !strings.HasPrefix(rest, "worktrees/")
		}
	}
	return false
}

// workPath says a cleaned absolute path is a file of the work as far as the
// session can tell: in a directory it has worked in or the command cd's to, and not scratch, not
// Claude Code's own directory.
func (s *ShellSession) workPath(p, cd string) bool {
	if under(p, "/tmp") || under(p, "/var/tmp") || under(p, "/dev") {
		return false
	}
	// A directory the session itself worked in is the work even under a cache
	// directory (the runner checks repositories out in ~/.cache); only Claude
	// Code's own directory stays out.
	claude := shellInClaudeDir(p)
	for _, r := range append([]string{cd}, s.Roots...) {
		if r != "" && under(p, r) && (!claude || strings.Contains(r, "/.cache/") && !strings.Contains(p[len(r):], "/.cache/")) {
			return true
		}
	}
	return false
}

// Edit says a Bash command wrote a file that is the work: a redirect, tee,
// sed -i, formatter, git apply or a script that writes, to a path that is
// not scratch (see workPath). A path built from a variable or command
// substitution does not count, nor does a relative one after a cd the line
// does not spell out; nor does a variable set in an earlier Bash call, which
// is truly unknown: environment variables do not persist between Claude
// Code's Bash calls (only the working directory does). A file a tool saves
// with -o is not a write here. known is false when the command is too long to
// read (the answer is then "unknown", not "no").
func (s *ShellSession) Edit(cmd, cwd string) (wrote, known bool) {
	if len(cmd) > shellMaxCommand {
		return false, false
	}
	s.Note(cwd)
	vars := map[string]string{}
	if h := ShellHomeOf(cwd); h != "" {
		vars["HOME"] = h
	}
	cmds, _ := ReadShell(cmd, cwd, vars, &s.Scripts, false)
	for i := range cmds {
		dir := cmds[i].Dir
		if dir == "" {
			dir = cwd
		}
		// A directory the command itself cd's into by an absolute path is where
		// the work is, though no record had it as a working directory (a sibling
		// checkout). Scratch and Claude's own directory are still excluded.
		cd := ""
		if cmds[i].Dir != "" && strings.HasPrefix(cmds[i].Dir, "/") && cmds[i].Dir != ShellNowhere {
			cd = path.Clean(cmds[i].Dir)
		}
		for _, w := range cmds[i].Wrote {
			if w.Kind == "output" {
				continue
			}
			p := w.Path
			if p == "*" { // git apply, patch, find ... -exec: the files are not named
				p = "."
				if w.Root != "" {
					p = w.Root
				} else if r := pipedFindRoot(cmds, i); r != "" {
					p = r // find ROOT ... | xargs sed -i: the files are the find's
				}
			}
			if p == "" || p[0] == '$' || p[0] == '`' {
				continue
			}
			if !strings.HasPrefix(p, "/") {
				if dir == ShellNowhere || !strings.HasPrefix(dir, "/") {
					continue
				}
				p = path.Join(dir, p)
			}
			if s.workPath(path.Clean(p), cd) {
				return true, true
			}
		}
	}
	return false, true
}

// pipedFindRoot is where the find feeding command i through a pipe looks
// ("" when command i is not fed by a find): its first argument that is not
// an option, or "." when it names none.
func pipedFindRoot(cmds []ShellCommand, i int) string {
	if i == 0 || !cmds[i].PipeIn || len(cmds[i-1].Words) == 0 || path.Base(cmds[i-1].Words[0]) != "find" {
		return ""
	}
	for _, a := range cmds[i-1].Words[1:] {
		if strings.HasPrefix(a, "-") || strings.ContainsAny(a, "()!") {
			break
		}
		return a
	}
	return "."
}
