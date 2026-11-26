package transcript

import (
	"path"
	"regexp"
	"strings"
)

// ShellEditKind is the kind of the per-hour shell-edit count the client
// uploads in tool_hours (tool "Bash"): calls = the Bash calls that wrote a
// file of the work. The server reads the same kind back.
const ShellEditKind = "shell_edit"

// ShellNoneKind is the kind of the per-hour count of Bash calls the client
// read whole and found no write of the work in. With ShellEditKind and
// ShellUnknownKind it accounts for every Bash call the client classified, so
// calls with none of the three (synced by an older client) stay uncounted.
const ShellNoneKind = "shell_none"

// ShellUnknownKind is the kind of the per-hour count of Bash calls the client
// could not read (a command or script too long): calls = those calls. They
// are neither edits nor known to be none, so readers add them to the calls
// not counted.
const ShellUnknownKind = "shell_unknown"

// ShellMaxCommand is the longest command the reader reads; a longer one is
// unknown, to the client's count as to the server's.
const ShellMaxCommand = shellMaxCommand

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
	cwd = ShellPosixDir(cwd)
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

var shellDriveRE = regexp.MustCompile(`^([A-Za-z]):[\\/]`)

// ShellPosixDir is a working directory as the shell of the session names it:
// a Windows one (C:\Users\x\repo) is /c/Users/x/repo to the bash Claude
// Code runs there, and any other is itself.
func ShellPosixDir(cwd string) string {
	if m := shellDriveRE.FindStringSubmatch(cwd); m != nil {
		return "/" + strings.ToLower(m[1]) + "/" + strings.TrimLeft(strings.ReplaceAll(cwd[2:], `\`, "/"), "/")
	}
	return cwd
}

func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// shellHomeRE is a home directory by where operating systems put them.
var shellHomeRE = regexp.MustCompile(`^(/home/[^/]+|(?:/[a-z])?/Users/[^/]+|/root)(/|$)`)

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

// shellScratchDirs are the directories scratch files live in, on Linux and
// macOS: nothing under them is a file of the work.
var shellScratchDirs = []string{"/tmp", "/var/tmp", "/dev", "/private/tmp", "/private/var/tmp", "/var/folders", "/private/var/folders"}

// workPath says a cleaned absolute path is a file of the work as far as the
// session can tell: in a directory it has worked in, or one of cd (the
// directory the command cd's to and the one the call runs in), and not
// scratch, not Claude Code's own directory.
func (s *ShellSession) workPath(p string, cd ...string) bool {
	for _, d := range shellScratchDirs {
		if under(p, d) {
			return false
		}
	}
	// A directory the session itself worked in is the work even under a cache
	// directory (the runner checks repositories out in ~/.cache); only Claude
	// Code's own directory stays out.
	claude := shellInClaudeDir(p)
	for _, r := range append(cd, s.Roots...) {
		if r != "" && under(p, r) && (!claude || strings.Contains(r, "/.cache/") && !strings.Contains(p[len(r):], "/.cache/")) {
			return true
		}
	}
	return false
}

// Edit says a Bash command wrote a file that is the work: a redirect, tee,
// sed -i, formatter, git apply or a script that writes, to a path that is
// not scratch (see workPath). A file curl saves with -o counts like any other
// write. A Windows working directory is read as bash there names it (see
// ShellPosixDir).
//
// known is false when the reading cannot say no: a write to a path it cannot
// place (one built from a substitution or from a variable the line does not
// set, a relative one after a cd the line does not spell out or in a working
// directory that is no path, one under a home it could not infer); a command
// too long to read, with more commands or deeper nesting than are read, or
// one that runs a script too long to read whole, whose paths it could not
// follow, or that the session no longer remembers. The answer is then
// "unknown", not "no". A file named by $(mktemp) or under $TMPDIR is scratch.
func (s *ShellSession) Edit(cmd, cwd string) (wrote, known bool) {
	if len(cmd) > shellMaxCommand {
		return false, false
	}
	cwd = ShellPosixDir(cwd)
	s.Note(cwd)
	vars := map[string]string{}
	if h := ShellHomeOf(cwd); h != "" {
		vars["HOME"] = h
	}
	vars["TMPDIR"] = "/tmp" // scratch wherever it is
	cmds, _ := ReadShell(cmd, cwd, vars, &s.Scripts, false)
	known = true
	for i := range cmds {
		known = known && !cmds[i].Unread
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
			p := WritePlace(cmds, i, w)
			if p == "$HOME" || strings.HasPrefix(p, "$HOME/") {
				known = false // a home this reading could not infer: anywhere
				continue
			}
			if p == "" {
				continue
			}
			if p[0] == '$' || p[0] == '`' {
				known = false // a path only the shell that ran it knew: anywhere
				continue
			}
			if !strings.HasPrefix(p, "/") {
				if dir != "" && !strings.HasPrefix(dir, "/") {
					known = false // after a cd not spelled out, or in a working directory that is no path: anywhere
				}
				if dir == ShellNowhere || !strings.HasPrefix(dir, "/") {
					continue
				}
				p = path.Join(dir, p)
			}
			// The call's own directory counts though the session remembers no
			// more directories (shellMaxRoots).
			here := ""
			if strings.HasPrefix(cwd, "/") {
				here = path.Clean(cwd)
			}
			if s.workPath(path.Clean(p), cd, here) {
				return true, true
			}
		}
	}
	return false, known
}

// WritePlace is where a write lands, as the line spells it: its path, or for
// one whose files the command does not name (Path "*": git apply, patch,
// find ... -exec, xargs) the directory they are under: the write's own root
// (patch -d DIR, find DIR -exec), else the root of the find that feeds the
// command through a pipe, else "." (the command's directory).
func WritePlace(cmds []ShellCommand, i int, w ShellWrite) string {
	if w.Path != "*" {
		return w.Path
	}
	if w.Root != "" {
		return w.Root
	}
	if r := pipedFindRoot(cmds, i); r != "" {
		return r // find ROOT ... | xargs sed -i: the files are the find's
	}
	return "."
}

// pipedFindRoot is where the find feeding command i through a pipe looks
// ("" when no find feeds it): its first argument that is not an option, or
// "." when it names none. The find may be several commands back along the
// pipe (find … | grep -v x | xargs sed -i).
func pipedFindRoot(cmds []ShellCommand, i int) string {
	for ; i > 0 && cmds[i].PipeIn; i-- {
		for i > 1 && cmds[i-1].Sub {
			i-- // a substitution's commands stand before the command they are in
		}
		if f := cmds[i-1]; len(f.Words) > 0 && path.Base(f.Words[0]) == "find" {
			for _, a := range f.Words[1:] {
				if strings.HasPrefix(a, "-") || strings.ContainsAny(a, "()!") {
					break
				}
				return a
			}
			return "."
		}
	}
	return ""
}
