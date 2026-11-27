package transcript

// The command-head reader the usage report (cmd/wp) and the write recogniser share.

import (
	"path"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var (
	AssignRE   = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)
	redirectRE = lazyre.New(`^(\d*|&)(>>?|<)(&\d*|.*)$`)
	subcmdRE   = lazyre.New(`^[a-z][a-z0-9:_.-]*$`)
)

// controlWords start or end shell constructs; the command inside follows.
var skipWords = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true, "!": true, "time": true, "nohup": true,
	"command": true, "exec": true, "builtin": true, "setsid": true, "coproc": true}

// dropSegment: these words alone don't run anything.
var dropWords = map[string]bool{"fi": true, "done": true, "esac": true, "for": true, "case": true, "select": true, "function": true, "in": true, "}": true, "{": true, "[[": true, "[": true, "break": true, "continue": true}

// SubcommandTools take a subcommand as their first word that is not a flag.
// valueFlags are the flags that take a value, which must be skipped.
var SubcommandTools = map[string]map[string]bool{
	"git": {"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true}, "go": {"-C": true},
	"npm": {"--prefix": true, "-w": true, "--workspace": true}, "pnpm": {"-C": true, "--dir": true, "--filter": true, "-F": true}, "yarn": {"--cwd": true},
	"npx": nil, "bun": nil, "deno": nil, "make": {"-C": true, "-f": true, "-j": true, "-I": true}, "gmake": nil,
	"docker": {"-H": true, "--host": true, "--context": true, "-c": true}, "docker-compose": {"-f": true, "-p": true, "--project-name": true, "--file": true},
	"podman": nil, "kubectl": {"-n": true, "--namespace": true, "--context": true, "--kubeconfig": true, "-s": true}, "k3s": nil, "helm": {"-n": true, "--namespace": true, "--kube-context": true, "--kubeconfig": true},
	"wp": {"-p": true, "--project": true, "--agent": true}, "gh": {"-R": true, "--repo": true}, "glab": {"-R": true, "--repo": true}, "cargo": {"--manifest-path": true, "-p": true},
	"rustup": nil, "pip": nil, "pip3": nil, "poetry": nil, "uv": {"--directory": true, "-p": true}, "brew": nil, "apt": nil, "apt-get": nil,
	"dnf": nil, "yum": nil, "pacman": nil, "snap": nil, "systemctl": {"-H": true, "--host": true}, "journalctl": nil, "terraform": {"-chdir": true}, "tofu": nil,
	"aws": {"--profile": true, "--region": true}, "gcloud": {"--project": true}, "az": nil, "argocd": nil, "flux": nil, "kustomize": nil,
	"claude": nil, "ansible-playbook": nil, "ufw": nil, "ip": nil, "bundle": nil, "gem": nil, "rake": nil, "composer": nil, "dotnet": nil, "mvn": nil, "gradle": nil,
	"ssh-keygen": nil, "git-lfs": nil, "tmux": nil, "sqlite3": nil, "psql": nil,
}

// wrappers run what follows them; the head is that program. A value is how
// many leading arguments (not flags) they take before it.
var wrapperArgs = map[string]int{"sudo": 0, "doas": 0, "env": 0, "nice": 0, "ionice": 0, "timeout": 1, "stdbuf": 0, "chrt": 0, "unbuffer": 0, "caffeinate": 0}

// wrapperValueFlags take a value: sudo -u root cmd.
var wrapperValueFlags = map[string]map[string]bool{
	"sudo": {"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-r": true, "-t": true, "-T": true, "-U": true, "-D": true},
	"doas": {"-u": true, "-C": true}, "nice": {"-n": true}, "ionice": {"-c": true, "-n": true, "-p": true},
	"timeout": {"-s": true, "-k": true}, "env": {"-u": true, "-C": true, "-S": true}, "stdbuf": {"-i": true, "-o": true, "-e": true},
}

var Shells = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ash": true, "ksh": true}

// ShellDashC finds the script of `bash -c SCRIPT` (also -lc, -ic, -ce…): the
// first word after the shell's options when one of them is -c. Options that
// take a value (-o pipefail, -eo pipefail, +O extglob, --rcfile FILE) are read
// past, so `bash -euo pipefail -c SCRIPT` is read as the guard reads it.
func ShellDashC(args []string) (string, bool) {
	dashC := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if dashC && i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case a == "--rcfile" || a == "--init-file":
			i++
		case strings.HasPrefix(a, "--"):
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'c':
					dashC = dashC || a[0] == '-'
				case 'o', 'O':
					if j == len(a)-1 {
						i++ // its value is the next word
					}
				}
			}
		default:
			if dashC {
				return a, true
			}
			return "", false
		}
	}
	return "", false
}

// StripWords drops redirections, assignments, control words and wrappers
// from the front of a segment, leaving the program and its arguments.
func StripWords(seg []string) []string {
	var words []string
	for i := 0; i < len(seg); i++ {
		w := seg[i]
		if redirectRE.MatchString(w) && w != "" {
			if m := redirectRE.FindStringSubmatch(w); m[3] == "" && i+1 < len(seg) {
				i++
			}
			continue
		}
		words = append(words, w)
	}
	for len(words) > 0 {
		w := words[0]
		switch {
		case AssignRE.MatchString(w), skipWords[w]:
			words = words[1:]
		case dropWords[w]:
			return nil
		default:
			base := path.Base(w)
			n, isWrapper := wrapperArgs[base]
			if !isWrapper {
				return words
			}
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || AssignRE.MatchString(words[0])) {
				flag := words[0]
				words = words[1:]
				if wrapperValueFlags[base][flag] && len(words) > 0 {
					words = words[1:]
				}
			}
			for ; n > 0 && len(words) > 0; n-- {
				words = words[1:]
			}
		}
	}
	return nil
}

// HeadOf names a command: its program, plus the subcommand for tools that have them.
func HeadOf(words []string) string {
	prog := words[0]
	switch {
	case (strings.HasPrefix(prog, "$") || strings.HasPrefix(prog, "`")) && !strings.Contains(prog, "/"):
		return "(variable)"
	case strings.ContainsAny(prog, "=*?[") && !strings.Contains(prog, "/"):
		return ""
	}
	name := path.Base(prog)
	if name == "." || name == "/" || name == "" {
		return ""
	}
	if len(name) > 60 {
		name = name[:60]
	}
	flags, ok := SubcommandTools[strings.TrimSuffix(name, ".exe")]
	if !ok {
		if strings.HasPrefix(name, "python") {
			for i := 1; i+1 < len(words); i++ {
				if words[i] == "-m" && subcmdRE.MatchString(words[i+1]) {
					return "python -m " + words[i+1]
				}
				if !strings.HasPrefix(words[i], "-") {
					break
				}
			}
			return "python"
		}
		return name
	}
	rest := words[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			return name
		case strings.HasPrefix(a, "-"):
			if flags[a] {
				i++
			}
		case subcmdRE.MatchString(a):
			sub := a
			if name == "docker" && sub == "compose" || name == "kubectl" && sub == "rollout" {
				for _, b := range rest[i+1:] {
					if !strings.HasPrefix(b, "-") && subcmdRE.MatchString(b) {
						sub += " " + b
					}
					break
				}
			}
			return name + " " + sub
		default:
			return name
		}
	}
	return name
}
