package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// worktreeAnywhere, set before git on the command line, lets a worktree go
// beside the main checkout when a person asked for that place.
const worktreeAnywhere = "WP_WORKTREE_ANYWHERE"

// worktreePlaceNudge refuses a `git worktree add` (or `move`) that would put
// the worktree beside the main checkout (WP-563): in the folder where people
// keep their projects, which agents' ../repo-x worktrees littered. It names
// the same command aimed under the repo's .claude/worktrees/. It looks
// inside wrappers (sudo, env, command…), `sh -c` scripts and $( )
// substitutions, and reads only the files under .git: it runs before every
// Bash call. WP_WORKTREE_ANYWHERE=1 before git, exported earlier in the
// command, or in the environment Claude Code runs in lets it through. Empty
// when there is nothing to say.
func worktreePlaceNudge(cmd, cwd string) string {
	if !strings.Contains(cmd, "worktree") || len(cmd) > 20000 {
		return ""
	}
	return worktreePlaceIn(parseShell(cmd), cwd, anywhereValue(os.Getenv(worktreeAnywhere)), 0)
}

func anywhereValue(v string) bool { return v != "" && v != "0" }

// anywhereSet: the assignment w sets WP_WORKTREE_ANYWHERE on.
func anywhereSet(w string) bool {
	k, v, _ := strings.Cut(w, "=")
	return k == worktreeAnywhere && anywhereValue(v)
}

func worktreePlaceIn(cmds []*guardCmd, dir string, anywhere bool, depth int) string {
	if depth > 4 {
		return ""
	}
	for _, c := range cmds {
		for _, sub := range c.subs {
			if t := worktreePlaceIn(sub, dir, anywhere, depth+1); t != "" {
				return t
			}
		}
		words, here := worktreeUnwrap(c.words)
		here = here || anywhere
		if len(words) == 0 {
			continue
		}
		switch path.Base(words[0]) {
		case "cd", "pushd":
			if len(words) > 1 && !strings.HasPrefix(words[1], "-") {
				dir = absFrom(dir, words[1])
			}
			continue
		case "export":
			for _, w := range words[1:] {
				anywhere = anywhere || anywhereSet(w)
			}
			continue
		case "sh", "bash", "zsh", "dash", "ksh":
			if script := shellScriptArg(words[1:]); script != "" {
				if t := worktreePlaceIn(parseShell(script), dir, here, depth+1); t != "" {
					return t
				}
			}
			continue
		case "git":
		default:
			continue
		}
		if here {
			continue
		}
		if t := worktreePlaceGit(words[1:], dir); t != "" {
			return t
		}
	}
	return ""
}

// worktreePlaceGit is the refusal for one git command (its arguments after
// git), run in dir; empty when it doesn't place a worktree beside the
// checkout.
func worktreePlaceGit(args []string, dir string) string {
	gdir, pre, rest := gitGlobals(args, dir)
	if len(rest) < 2 || rest[0] != "worktree" {
		return ""
	}
	var i int
	switch rest[1] {
	case "add":
		i = worktreeAddPathArg(rest[2:])
	case "move":
		i = worktreeMoveDestArg(rest[2:])
	default:
		return ""
	}
	if i < 0 {
		return ""
	}
	target := absFrom(gdir, rest[2+i])
	r, ok := findRepo(gdir)
	if !ok || filepath.Base(r.common) != ".git" {
		return ""
	}
	root := filepath.Dir(r.common)
	if !besideCheckout(root, realPath(target)) {
		return ""
	}
	dst := filepath.Join(root, autopilotWorktrees, movedName(root, target))
	fixed := append([]string{"git"}, pre...)
	fixed = append(fixed, rest[:2]...)
	fixed = append(fixed, rest[2:2+i]...)
	fixed = append(fixed, dst)
	fixed = append(fixed, rest[3+i:]...)
	or := fmt.Sprintf("`wp worktree %s` (it also copies .env and the like and runs the repo's setup)", filepath.Base(dst))
	if rest[1] == "move" {
		or = "`wp worktree move` for the ones already beside it"
	}
	return fmt.Sprintf("Waypoint (WP-563): that puts a worktree beside the main checkout, in %s, where people keep their projects. "+
		"Keep it under the repo: `%s`, or %s. "+
		"If a person asked for that place, put %s=1 before git.",
		filepath.Dir(realPath(target)), shellJoin(fixed), or, worktreeAnywhere)
}

// worktreeUnwrap strips assignments and wrappers (env, sudo, command…) off
// a command's words; anywhere: one of the assignments was
// WP_WORKTREE_ANYWHERE=1.
func worktreeUnwrap(words []string) (rest []string, anywhere bool) {
	for range 8 {
		for len(words) > 0 && isAssignment(words[0]) {
			anywhere = anywhere || anywhereSet(words[0])
			words = words[1:]
		}
		if len(words) == 0 {
			return nil, anywhere
		}
		args := words[1:]
		switch path.Base(words[0]) {
		case "env":
			words = dropFlags(args, "-u", "--unset", "-C", "--chdir", "-S", "--split-string")
		case "sudo", "doas":
			words = dropFlags(args, "-u", "-g", "-C", "-D", "-h", "-p", "-r", "-t", "-T", "-U", "--user", "--group")
		case "command", "builtin", "nohup", "setsid", "time":
			words = dropFlags(args)
		case "exec":
			words = dropFlags(args, "-a")
		case "nice":
			words = dropFlags(args, "-n")
		case "timeout":
			if words = dropFlags(args, "-s", "-k", "--signal", "--kill-after"); len(words) > 0 {
				words = words[1:]
			}
		default:
			return words, anywhere
		}
	}
	return words, anywhere
}

// shellScriptArg is the script a shell is given with -c (or -lc, -ec…);
// empty when there is none.
func shellScriptArg(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "+o" || a == "-O" || a == "+O":
			i++
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.Contains(a[1:], "c") && i+1 < len(args) {
				return args[i+1]
			}
		default:
			return ""
		}
	}
	return ""
}

// absFrom is p made absolute against dir (~ is the home folder).
func absFrom(dir, p string) string {
	if rest, ok := strings.CutPrefix(p, "~"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// gitGlobals reads git's options before its subcommand: the folder it runs
// in (-C), the options themselves, and the subcommand with what follows.
func gitGlobals(args []string, dir string) (gdir string, pre, rest []string) {
	gdir = dir
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" && i+1 < len(args):
			gdir = absFrom(gdir, args[i+1])
			pre = append(pre, a, args[i+1])
			i++
		case a == "-c" && i+1 < len(args):
			pre = append(pre, a, args[i+1])
			i++
		case strings.HasPrefix(a, "-"):
			pre = append(pre, a)
		default:
			return gdir, pre, args[i:]
		}
	}
	return gdir, pre, nil
}

// worktreeMoveDestArg is the index of the new path among git worktree
// move's arguments; -1 when there is none.
func worktreeMoveDestArg(args []string) int {
	seen := 0
	for i, a := range args {
		if a == "--" || strings.HasPrefix(a, "-") {
			continue
		}
		if seen++; seen == 2 {
			return i
		}
	}
	return -1
}

// worktreeAddPathArg is the index of the path among git worktree add's
// arguments; -1 when there is none.
func worktreeAddPathArg(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return i + 1
			}
			return -1
		case a == "-b" || a == "-B" || a == "--reason":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return i
		}
	}
	return -1
}
