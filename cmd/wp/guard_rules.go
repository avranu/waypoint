package main

import (
	"bufio"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
	trscript "git.jmann.me/claude/waypoint/internal/transcript"
)

// Rule ids, as they appear in guard.log.
const (
	ruleRoot     = "root"
	ruleInstall  = "install"
	ruleWebCode  = "web-code"
	ruleDownload = "download"
	ruleRemote   = "remote"
	ruleHost     = "host"
	ruleGuard    = "guard"
	ruleRepo     = "repo"
	// A change sent to one of the repo's environments (WP-749).
	ruleEnvChange = "env-change"
)

// guardHit is a refused call.
type guardHit struct {
	rule   string
	reason string
	what   string
	ask    bool
	way    string
}

func hit(rule, reason string, words ...string) *guardHit {
	return &guardHit{rule: rule, reason: reason, what: strings.Join(words, " ")}
}

// guard is one call's check. dir follows `cd` through the command; lost is
// set once a `cd` goes where the guard can't tell ($DIR, -).
type guard struct {
	site      guardSite
	dir       string
	server    string
	origin    func() string
	orig      *string
	lost      bool
	installs  bool
	inherit   bool
	contained bool
	envHosts  func() []string
	open      []bool
}

// walled are the rules a Contained sandbox enforces itself (PL7 §3): it has
// no root (NoNewPrivileges, no capabilities), sees no other repository and
// none of the host's files, writes only its workspace and its own /tmp, and
// reaches the network only through the session broker's allowlist, so an
// install, a download or a fetch gets only what the project's policy allows
// and lands inside the unit. The guard rule stays: the session's own hooks
// and settings, and what marks it unattended, are what keep the guard on.
// WebSearch stays refused: it runs at Anthropic, outside the broker.
var walled = map[string]bool{
	ruleRoot: true, ruleInstall: true, ruleWebCode: true, ruleDownload: true,
	ruleRemote: true, ruleHost: true, ruleRepo: true,
}

// keep drops a hit the sandbox's walls make moot, so the check goes on to
// what they don't cover.
func (g *guard) keep(h *guardHit) *guardHit {
	if h != nil && g.contained && walled[h.rule] {
		return nil
	}
	return h
}

func (g *guard) originHost() string {
	if g.orig == nil {
		h := ""
		if g.origin != nil {
			h = g.origin()
		}
		g.orig = &h
	}
	return *g.orig
}

// hostOK: a download from host is fine: this machine, the Waypoint server,
// or the host of the repo's origin.
func (g *guard) hostOK(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	switch {
	case host == "localhost", host == "::1", host == "0.0.0.0", strings.HasSuffix(host, ".localhost"), strings.HasPrefix(host, "127."):
		return true
	case host == "":
		return false
	case host == g.server:
		return true
	}
	return host == g.originHost()
}

// bashHit checks a Bash command; nil lets it through.
func (g *guard) bashHit(command string) *guardHit {
	return g.script(parseShell(command), 0)
}

const guardMaxDepth = 5

// script checks a list of simple commands.
func (g *guard) script(cmds []*guardCmd, depth int) *guardHit {
	if depth > guardMaxDepth {
		return nil
	}
	fetch := false
	for i, c := range cmds {
		first := i == 0
		if !c.PipeIn {
			fetch = false
		}
		if !c.AndIn && (!g.inherit || !first) {
			g.installs = false
		}
		for _, sub := range c.Subs {
			if h := g.child(sub, depth+1); h != nil {
				return h
			}
		}
		rest, h := g.unwrap(c.Words)
		if h != nil {
			return h
		}
		var name string
		var args []string
		if len(rest) > 0 {
			name, args = path.Base(rest[0]), rest[1:]
		}
		if isFetcher(name) {
			for _, w := range c.Words[:len(c.Words)-len(rest)] {
				if h := fetchHomeHit(w); h != nil {
					return h
				}
			}
		}
		prev := g.open
		if n := len(c.Words) - len(args); len(args) > 0 && len(c.Open) == len(c.Words) {
			g.open = c.Open[n:]
		} else {
			g.open = nil
		}
		h = g.simple(c, name, args, depth)
		g.open = prev
		if h != nil {
			return h
		}
		switch {
		case isFetcher(name):
			fetch = true
		case fetch && c.PipeIn && readsStdinAsCode(name, args):
			if h := g.keep(hit(ruleWebCode, "running code downloaded from the web", "… |", name)); h != nil {
				return h
			}
		}
	}
	return nil
}

// child checks commands a child shell runs ($( ), bash -c, a heredoc
// script, find -exec): its cd and installs don't carry back to this one.
func (g *guard) child(cmds []*guardCmd, depth int) *guardHit {
	dir, lost, installs := g.dir, g.lost, g.installs
	inherit := g.inherit
	g.inherit = true
	defer func() { g.dir, g.lost, g.installs, g.inherit = dir, lost, installs, inherit }()
	return g.script(cmds, depth)
}

var (
	fetchers     = map[string]bool{"curl": true, "wget": true, "fetch": true, "aria2c": true, "http": true, "https": true, "xh": true, "lwp-request": true, "lwp-download": true}
	codeRunners  = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "eval": true, "source": true, ".": true, "node": true, "perl": true, "ruby": true, "php": true}
	shellNames   = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true}
	rePython     = lazyre.New(`^python[0-9.]*$`)
	rePip        = lazyre.New(`^pip[0-9.]*$`)
	reAssign     = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*=`)
	reVersioned  = lazyre.New(`^(@[A-Za-z0-9._-]+/)?[A-Za-z0-9._-]+@[A-Za-z0-9._^~<>=*+-]+$`)
	reDaemonJSON = lazyre.New(`(^|/)daemon\.json$`)
	guardMarkers = map[string]bool{"WP_TASK": true, "WAYPOINT_FOLLOWUP": true, "WAYPOINT_UNATTENDED": true, "WP_GUARD": true}
)

func isFetcher(name string) bool { return fetchers[name] }

// hasFetcher: a command of cmds, or of the substitutions in them, downloads.
func (g *guard) hasFetcher(cmds []*guardCmd) bool {
	for _, c := range cmds {
		rest, _ := g.unwrap(c.Words)
		if len(rest) > 0 && isFetcher(path.Base(rest[0])) {
			return true
		}
		for _, sub := range c.Subs {
			if g.hasFetcher(sub) {
				return true
			}
		}
	}
	return false
}

// runsSub: the code a command runs is a substitution: sh -c "$(…)", bash <(…),
// eval "$(…)", source <(…).
func runsSub(name string, args []string) bool {
	if name == "eval" {
		return strings.Contains(strings.Join(args, " "), subPlaceholder)
	}
	pos := scanArgs(args, "").pos
	return len(pos) > 0 && strings.Contains(pos[0], subPlaceholder)
}

// readsStdinAsCode: the interpreter would run what is piped into it (no
// script, no -c): `curl … | sh`, not `curl … | python3 -m json.tool`.
func readsStdinAsCode(name string, args []string) bool {
	inline := ""
	switch {
	case shellNames[name]:
		inline = "c"
	case rePython.MatchString(name):
		inline = "cm"
	case name == "node" || name == "deno":
		inline = "ep"
	case name == "perl":
		inline = "eEnp"
	case name == "ruby":
		inline = "e"
	case name == "php":
		inline = "r"
	default:
		return false
	}
	sc := scanArgs(args, "")
	for _, l := range inline {
		if sc.flags["-"+string(l)] {
			return false
		}
	}
	if sc.flags["--eval"] || sc.flags["--print"] {
		return false
	}
	for _, p := range sc.pos {
		if p != "-" {
			return false
		}
	}
	return true
}

func isAssignment(w string) bool { return reAssign.MatchString(w) }

// assignHit checks NAME=value given to a command or exported.
func (g *guard) assignHit(w string) *guardHit {
	name, val, _ := strings.Cut(w, "=")
	switch {
	case name == "DOCKER_HOST" && val != "":
		return hit(ruleRemote, "pointing Docker at another machine", w)
	case name == "WP_GUARD":
		return hit(ruleGuard, "changing the guard", w)
	case guardMarkers[name] && (val == "" || val == "0" || val == "false"):
		return hit(ruleGuard, "clearing what marks the session as unattended", w)
	case strings.EqualFold(name, "npm_config_package") || strings.EqualFold(name, "npm_config_yes") && val != "false":
		return hit(ruleInstall, "installing software", w)
	case fetchSettingsVar(name):
		return hit(ruleDownload, "pointing fetches at settings or a proxy the guard can't read", w)
	}
	return nil
}

// fetchSettingsVar: a variable that makes curl, wget, httpie or xh read
// settings from elsewhere (a body, a method, more addresses) or send
// everything through a proxy (WP-749 review). no_proxy only narrows.
func fetchSettingsVar(name string) bool {
	l := strings.ToLower(name)
	switch l {
	case "curl_home", "wgetrc", "httpie_config_dir", "xh_config_dir":
		return true
	}
	return strings.HasSuffix(l, "_proxy") && l != "no_proxy"
}

// fetchHomeHit: HOME or XDG_CONFIG_HOME set for a fetch, or exported, which
// moves where it reads its settings from (fetchSettingsVar).
func fetchHomeHit(w string) *guardHit {
	if !isAssignment(w) {
		return nil
	}
	if name, _, _ := strings.Cut(w, "="); name == "HOME" || name == "XDG_CONFIG_HOME" {
		return hit(ruleDownload, "pointing fetches at settings the guard can't read", w)
	}
	return nil
}

// unwrap strips assignments and wrappers off a command's words and returns
// the command proper.
func (g *guard) unwrap(words []string) ([]string, *guardHit) {
	for range 16 {
		for len(words) > 0 && isAssignment(words[0]) {
			if h := g.keep(g.assignHit(words[0])); h != nil {
				return nil, h
			}
			words = words[1:]
		}
		if len(words) == 0 {
			return nil, nil
		}
		args := words[1:]
		switch path.Base(words[0]) {
		case "nohup", "builtin", "setsid", "time":
			words = dropFlags(args)
		case "exec":
			words = dropFlags(args, "-a")
		case "command":
			if sc := scanArgs(args, ""); sc.flags["-v"] || sc.flags["-V"] {
				return nil, nil
			}
			words = dropFlags(args)
		case "nice":
			words = dropFlags(args, "-n")
		case "ionice":
			words = dropFlags(args, "-c", "-n", "-p")
		case "stdbuf":
			words = dropFlags(args, "-i", "-o", "-e")
		case "timeout":
			words = dropFlags(args, "-s", "-k", "--signal", "--kill-after")
			if len(words) > 0 {
				words = words[1:]
			}
		case "xargs":
			words = dropFlags(args, "-n", "-I", "-L", "-P", "-s", "-d", "-E", "-a", "--max-args", "--max-procs", "--delimiter", "--arg-file")
		case "env":
			rest, h := g.unwrapEnv(args)
			if h != nil {
				return nil, h
			}
			words = rest
		default:
			return words, nil
		}
	}
	return words, nil
}

// dropFlags removes leading flags (and the values of those named in valued).
func dropFlags(args []string, valued ...string) []string {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" {
		w := args[i]
		i++
		if w == "--" {
			break
		}
		for _, v := range valued {
			if w == v && i < len(args) {
				i++
				break
			}
		}
	}
	return args[i:]
}

// unwrapEnv reads env's options and assignments; env -i and env -u of a
// marker are refused.
func (g *guard) unwrapEnv(args []string) ([]string, *guardHit) {
	i := 0
	for i < len(args) {
		w := args[i]
		switch {
		case w == "-i" || w == "--ignore-environment" || w == "-":
			return nil, hit(ruleGuard, "clearing what marks the session as unattended", "env", w)
		case w == "-u" || w == "--unset":
			if i+1 < len(args) && guardMarkers[args[i+1]] {
				return nil, hit(ruleGuard, "unsetting what marks the session as unattended", "env -u", args[i+1])
			}
			i += 2
		case strings.HasPrefix(w, "--unset="):
			if guardMarkers[strings.TrimPrefix(w, "--unset=")] {
				return nil, hit(ruleGuard, "unsetting what marks the session as unattended", "env", w)
			}
			i++
		case w == "-C" || w == "--chdir" || w == "-S" || w == "--split-string":
			i += 2
		case strings.HasPrefix(w, "-"):
			i++
		case isAssignment(w):
			if h := g.keep(g.assignHit(w)); h != nil {
				return nil, h
			}
			i++
		default:
			return args[i:], nil
		}
	}
	return nil, nil
}

// argScan is a command line split into positional words and flags.
type argScan struct {
	pos   []string
	flags map[string]bool
	val   map[string][]string
}

// values are the values given to any of the flags named.
func (sc argScan) values(flags ...string) []string {
	var out []string
	for _, f := range flags {
		out = append(out, sc.val[f]...)
	}
	return out
}

// scanArgs splits args. shortVal are the single-letter flags that take a
// value (their cluster may carry it: -XPOST), longVal the long ones.
func scanArgs(args []string, shortVal string, longVal ...string) argScan {
	sc := argScan{flags: map[string]bool{}, val: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			sc.pos = append(sc.pos, args[i+1:]...)
			return sc
		case strings.HasPrefix(a, "--"):
			name, v, has := strings.Cut(a, "=")
			sc.flags[name] = true
			if has {
				sc.val[name] = append(sc.val[name], v)
				continue
			}
			for _, l := range longVal {
				if l == name && i+1 < len(args) {
					i++
					sc.val[name] = append(sc.val[name], args[i])
					break
				}
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				f := "-" + string(a[j])
				sc.flags[f] = true
				if strings.IndexByte(shortVal, a[j]) >= 0 {
					if j+1 < len(a) {
						sc.val[f] = append(sc.val[f], a[j+1:])
					} else if i+1 < len(args) {
						i++
						sc.val[f] = append(sc.val[f], args[i])
					}
					break
				}
			}
		default:
			sc.pos = append(sc.pos, a)
		}
	}
	return sc
}

// simple checks one simple command whose name and arguments are given.
func (g *guard) simple(c *guardCmd, name string, args []string, depth int) *guardHit {
	if h := g.writes(c, name, args); h != nil {
		return h
	}
	if name == "" {
		return nil
	}
	if shellNames[name] && c.Heredoc != "" {
		if h := g.child(parseShell(c.Heredoc), depth+1); h != nil {
			return h
		}
	}
	if (codeRunners[name] || rePython.MatchString(name)) && runsSub(name, args) {
		for _, sub := range c.Subs {
			if g.hasFetcher(sub) {
				if h := g.keep(hit(ruleWebCode, "running code downloaded from the web", name, "$(curl …)")); h != nil {
					return h
				}
			}
		}
	}
	if name == "wp" {
		return g.keep(wpHit(args))
	}
	for _, rule := range []func(string, []string) *guardHit{rootHit, g.installHit, remoteHit, hostHit, g.looseHit, g.downloadHit} {
		if h := g.keep(rule(name, args)); h != nil {
			return h
		}
	}
	switch {
	case shellNames[name]:
		return g.shellBody(args, depth)
	case name == "eval":
		return g.script(parseShell(strings.Join(args, " ")), depth+1)
	case name == "find":
		return g.findExec(args, depth)
	case name == "cd" || name == "pushd":
		g.chdir(args)
	case name == "git":
		return g.keep(g.gitHit(args))
	}
	return nil
}

// shellBody checks the script given to `bash -c`.
func (g *guard) shellBody(args []string, depth int) *guardHit {
	for i, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args) {
			return g.child(parseShell(args[i+1]), depth+1)
		}
	}
	return nil
}

// findExec checks the commands find -exec runs.
func (g *guard) findExec(args []string, depth int) *guardHit {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(args) && args[j] != ";" && args[j] != "+" {
				j++
			}
			c := &guardCmd{Words: args[i+1 : j]}
			if len(g.open) == len(args) {
				c.Open = g.open[i+1 : j]
			}
			if h := g.child([]*guardCmd{c}, depth+1); h != nil {
				return h
			}
			i = j
		}
	}
	return nil
}

func (g *guard) chdir(args []string) {
	g.installs = false
	sc := scanArgs(args, "")
	target := g.site.home
	if len(sc.pos) > 0 {
		target = sc.pos[0]
	}
	if target == "-" {
		g.lost = true
		return
	}
	if p := g.site.resolve(g.dir, target); p != "" {
		g.dir = p
	} else {
		g.lost = true
	}
}

// stateWhy is the refusal for writing the CLI's state file, or feeding its
// session hook a made-up prompt (WP-277).
const stateWhy = "changing Waypoint's local state, which marks a session autopilot drives as unattended and holds the quota guard's counts and stops"

// wpHit: the things wp's arguments can do to the guard: remove its hooks, or
// run the session hook by hand, which could clear autopilot's mark.
func wpHit(args []string) *guardHit {
	if len(args) > 0 && args[0] == "hook" {
		for _, a := range args[1:] {
			if a == "--uninstall" || a == "-uninstall" {
				return hit(ruleGuard, "removing Waypoint's hooks, which include the guard", "wp hook", a)
			}
			if a == strings.TrimPrefix(hookEvent, "hook ") {
				return hit(ruleGuard, stateWhy, "wp hook", a)
			}
		}
	}
	return nil
}

// wpWriteTargets are the files a wp command writes where it's told to:
// wp env read|get -o FILE (Go flags: -o F, -o=F, --o F, --o=F), checked
// like curl -o's.
func wpWriteTargets(args []string) []string {
	i := -1
	for k := 0; k+1 < len(args); k++ {
		if args[k] == "env" && (args[k+1] == "read" || args[k+1] == "get") {
			i = k
			break
		}
	}
	if i < 0 {
		return nil
	}
	var out []string
	rest := args[i+2:]
	for j := 0; j < len(rest); j++ {
		a := rest[j]
		if a == "--" {
			break
		}
		switch {
		case a == "-o" || a == "--o":
			if j+1 < len(rest) {
				j++
				out = append(out, rest[j])
			}
		case strings.HasPrefix(a, "-o=") || strings.HasPrefix(a, "--o="):
			_, v, _ := strings.Cut(a, "=")
			out = append(out, v)
		}
	}
	return out
}

func rootHit(name string, args []string) *guardHit {
	switch name {
	case "sudo", "su", "doas", "pkexec", "run0", "sudoedit":
		return hit(ruleRoot, "running as root", name, firstWords(args, 3))
	}
	return nil
}

func firstWords(args []string, n int) string {
	if len(args) > n {
		args = args[:n]
	}
	return strings.Join(args, " ")
}

// readOnlyVerbs are what a system package manager does without changing anything.
var readOnlyVerbs = map[string]bool{"list": true, "show": true, "search": true, "policy": true, "info": true, "depends": true, "rdepends": true,
	"version": true, "--version": true, "-v": true, "-h": true, "--help": true, "help": true, "outdated": true, "deps": true, "config": true, "--prefix": true}

// installHit: software installed from outside the repository.
func (g *guard) installHit(name string, args []string) *guardHit {
	const reason = "installing software"
	h := func() *guardHit { return hit(ruleInstall, reason, name, firstWords(args, 3)) }
	first := ""
	if sc := scanArgs(args, ""); len(sc.pos) > 0 {
		first = sc.pos[0]
	}
	switch name {
	case "apt", "apt-get", "aptitude", "dnf", "dnf5", "microdnf", "yum", "zypper", "brew", "snap", "flatpak", "port":
		if len(args) > 0 && !readOnlyVerbs[first] && !readOnlyVerbs[args[0]] {
			return h()
		}
	case "dpkg":
		for _, a := range args {
			if a == "-i" || a == "--install" || a == "-R" || a == "--unpack" || a == "--recursive" {
				return h()
			}
		}
	case "rpm":
		for _, a := range args {
			if (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && !strings.Contains(a, "q") && strings.ContainsAny(a, "iUF")) || a == "--install" || a == "--upgrade" {
				return h()
			}
		}
	case "apk":
		if first == "add" || first == "upgrade" || first == "fix" {
			return h()
		}
	case "pacman", "yay", "paru":
		for _, a := range args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 1 && strings.ContainsRune("SUR", rune(a[1])) {
				return h()
			}
		}
	case "nix-env":
		if sc := scanArgs(args, ""); !sc.flags["-q"] && !sc.flags["--query"] {
			return h()
		}
	case "nix":
		if first == "profile" && contains(args, "install") {
			return h()
		}
	case "pipx":
		if first != "" && first != "list" {
			return h()
		}
	case "uvx", "bunx":
		return h()
	case "uv":
		if len(args) >= 2 && (args[0] == "pip" && args[1] == "install" || args[0] == "tool" && (args[1] == "install" || args[1] == "run")) || first == "add" {
			return h()
		}
	case "poetry", "pdm", "rye":
		if first == "add" {
			return h()
		}
	case "conda", "mamba", "micromamba":
		if first == "install" || first == "create" || first == "update" {
			return h()
		}
	case "npm", "pnpm", "yarn", "bun":
		if pmInstalls(name, args) {
			return h()
		}
		if lockfileInstall(name, args) {
			g.installs = true
		}
		if rest, where, ok := pmExec(name, args); ok {
			verb := " exec"
			if name == "bun" {
				verb = " x"
			}
			return g.npxHit(name+verb, append(where, rest...))
		}
	case "npx":
		return g.npxHit(name, args)
	case "go":
		if first == "install" || first == "get" {
			return h()
		}
		if first == "run" || first == "tool" {
			for _, p := range scanArgs(args, "").pos[1:] {
				if strings.Contains(p, "@") {
					return h()
				}
			}
		}
	case "cargo":
		if first == "install" || first == "add" || first == "binstall" {
			return h()
		}
	case "gem":
		if first == "install" || first == "update" {
			return h()
		}
	case "composer":
		if first == "require" || first == "global" {
			return h()
		}
	case "bundle":
		if first == "add" {
			return h()
		}
	case "docker", "podman":
		sc := scanArgs(args, "")
		if len(sc.pos) > 0 && sc.pos[0] == "pull" || len(sc.pos) > 1 && sc.pos[0] == "image" && sc.pos[1] == "pull" || len(sc.pos) > 1 && sc.pos[0] == "compose" && sc.pos[1] == "pull" {
			return h()
		}
	default:
		if rePip.MatchString(name) && first == "install" && pipAddsPackages(args[1:]) {
			return h()
		}
		if rePython.MatchString(name) {
			for i, a := range args {
				if a == "-m" && i+2 < len(args) && args[i+1] == "pip" && args[i+2] == "install" && pipAddsPackages(args[i+3:]) || a == "-m" && i+1 < len(args) && args[i+1] == "uv" {
					return h()
				}
			}
		}
	}
	return nil
}

// pipAddsPackages: a pip install names packages, not only the repo's own
// requirement files and local checkouts (-r FILE, -c FILE, -e ., .), which
// are the project's own like a bare npm install.
func pipAddsPackages(args []string) bool {
	local := func(p string) bool { return p == "." || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") }
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-r" || a == "--requirement" || a == "-c" || a == "--constraint":
			i++
		case strings.HasPrefix(a, "--requirement=") || strings.HasPrefix(a, "--constraint=") || strings.HasPrefix(a, "-r") && len(a) > 2:
		case a == "-e" || a == "--editable":
			if i++; i < len(args) && !local(args[i]) {
				return true
			}
		case strings.HasPrefix(a, "-"):
		case !local(a):
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// pmInstalls: an npm-style install of a package, or a global one. A bare
// install (from the lockfile) is the project's own.
func pmInstalls(name string, args []string) bool {
	sc := scanArgs(args, "wC", "--prefix", "--workspace", "--cwd", "--filter", "--dir", "--registry", "--cache", "--userconfig", "--loglevel", "--tag")
	global := sc.flags["-g"] || sc.flags["--global"] || contains(sc.val["--location"], "global")
	verb, pkgs := "", []string(nil)
	if len(sc.pos) > 0 {
		verb, pkgs = sc.pos[0], sc.pos[1:]
	}
	switch verb {
	case "install", "i", "add", "isntall", "in", "ins":
		return len(pkgs) > 0 || global || verb == "add"
	case "update", "upgrade", "up", "uninstall", "remove", "rm", "link", "ln":
		return global
	case "dlx", "create":
		return true
	case "init", "innit":
		return name == "npm" && len(pkgs) > 0
	case "global":
		return name == "yarn"
	}
	return false
}

// pmValueFlags are a package manager's long options that take a value.
var pmValueFlags = []string{"--prefix", "--workspace", "--cwd", "--filter", "--dir", "--registry", "--cache", "--userconfig", "--loglevel", "--tag", "--call"}

// pmExec: the arguments after `npm exec` (npm x, bun x), which work like
// npx's, and the options before the verb that say where it looks (--prefix,
// -C, --workspace, -w) or what it fetches (--package, --yes), in any
// spelling npm takes (npmOpt). pnpm exec and yarn exec only run what is
// installed, so they never fetch (WP-856). A word after an option the guard
// doesn't know may be its value (npm --otp 123 exec X), so the verb is
// looked for past it; exec or x there is taken as the verb, which is the
// safe reading. bun's options before x are its runtime's, not npm's: of
// them bun x honors only --no-install (its -p is --print, and it ignores
// --cwd), so that is the one passed on.
func pmExec(name string, args []string) (rest, where []string, ok bool) {
	if name != "npm" && name != "bun" {
		return nil, nil, false
	}
	value := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		maybeValue := value
		value = false
		switch {
		case a == "--":
			return nil, nil, false
		case a == "exec" || a == "x":
			return args[i+1:], where, true
		case name == "bun" && strings.HasPrefix(a, "-") && a != "-":
			if a == "--no-install" {
				where = append(where, a)
			}
			value = !strings.Contains(a, "=")
		case strings.HasPrefix(a, "-") && a != "-":
			o := readNpmOpt(a, "npm")
			if o.follow {
				where = append(where, a)
			}
			switch {
			case o.takes && !o.hasVal && i+1 < len(args):
				i++
				if o.follow {
					where = append(where, args[i])
				}
			case o.known:
			case strings.HasPrefix(a, "--"):
				value = !o.hasVal && !npmSwitches[a] && !strings.HasPrefix(a, "--no-")
			}
		case !maybeValue:
			return nil, nil, false
		}
	}
	return nil, nil, false
}

// npmOptLetters are npm's one-letter options and the full names of those
// the guard follows; a trailing = marks one that takes a value.
var npmOptLetters = map[byte]string{'a': "", 'c': "call=", 'd': "", 'f': "", 'g': "", 'h': "", 'l': "", 'm': "message=", 'n': "no",
	'p': "parseable", 'q': "", 's': "", 'v': "", 'w': "workspace=", 'y': "yes", 'B': "", 'C': "prefix=", 'D': "", 'E': "",
	'H': "", 'L': "location=", 'O': "", 'P': "", 'S': "", '?': ""}

// npmOptNames are the spellings npm takes for the long options the guard
// follows: the name, a prefix no other option has (--ye, --prefi), and the
// shorthands of more than one letter that would otherwise read as letters
// run together (-ws is --workspaces, not -w -s).
var npmOptNames = map[string]string{"package": "package=", "yes": "yes", "ye": "yes", "no": "no", "no-y": "no", "no-ye": "no",
	"no-yes": "no", "prefix": "prefix=", "prefi": "prefix=", "workspace": "workspace=", "call": "call=",
	"ws": "", "dd": "", "ddd": "", "reg": "registry="}

// npmOpt is an option word as npm reads it.
type npmOpt struct {
	names  []string
	val    string
	hasVal bool
	takes  bool
	follow bool
	known  bool
	split  bool
}

// readNpmOpt reads an option word the way npm's parser does: one dash or
// two, a name in any spelling npmOptNames has, --no-NAME (for an option
// with a value, NAME itself), or one-letter options alone or run together
// (-yC DIR), the value going to the last. cli is whose reading: "npm",
// "npx" or "bunx". npx reads -p as --package; npm reads it as --parseable
// (npm -p evil exec tsc runs npm's command "evil", which doesn't exist).
// bun x knows only -p/--package and --no-install and skips every other
// option without a value (bun x --prefix aa bb runs aa), so for it every
// other option word is a switch.
func readNpmOpt(a string, cli string) npmOpt {
	var o npmOpt
	key := strings.TrimLeft(a, "-")
	key, o.val, o.hasVal = strings.Cut(key, "=")
	if cli == "bunx" {
		o.known = true
		switch key {
		case "p", "package":
			o.names, o.takes, o.follow = []string{"package"}, true, true
		case "no-install":
			o.names, o.follow = []string{"no"}, true
		}
		return o
	}
	npx := cli == "npx"
	name, ok := npmOptNames[key]
	if !ok && strings.HasPrefix(key, "no-") {
		if n, isOpt := npmOptNames[strings.TrimPrefix(key, "no-")]; isOpt && strings.HasSuffix(n, "=") {
			name, ok = n, true
		}
	}
	if npx && key == "no-install" {
		name, ok = "no", true
	}
	if npx && key == "n" {
		name, ok = "removed=", true
	}
	var parts []string
	switch {
	case ok:
		parts = []string{name}
	case key != "" && strings.Trim(key, "acdfghlmnpqsvwyBCDEHLOPS?") == "":
		for i := 0; i < len(key); i++ {
			l := npmOptLetters[key[i]]
			if npx && key[i] == 'p' {
				l = "package="
			}
			parts = append(parts, l)
		}
	default:
		return o
	}
	o.known = true
	for i, p := range parts {
		n, takes := strings.CutSuffix(p, "=")
		if i == len(parts)-1 {
			o.takes = takes
		} else if takes {
			o.split = true
		}
		switch n {
		case "package", "yes", "no", "prefix", "workspace", "call":
			o.names = append(o.names, n)
			o.follow = true
		}
	}
	return o
}

// npmSwitches are npm's common options that take no value.
var npmSwitches = map[string]bool{"--silent": true, "--quiet": true, "--offline": true, "--prefer-offline": true, "--prefer-online": true,
	"--json": true, "--verbose": true, "--global": true, "--ignore-scripts": true, "--workspaces": true, "--include-workspace-root": true,
	"--if-present": true, "--dry-run": true, "--force": true, "--yes": true, "--no": true, "--color": true}

// lockfileInstall: npm ci, a bare npm install or a bare yarn, which install
// the lockfile's packages here and nothing else.
func lockfileInstall(name string, args []string) bool {
	if name == "yarn" && len(args) == 0 {
		return true
	}
	if len(args) == 0 || args[0] != "ci" && args[0] != "install" && args[0] != "i" {
		return false
	}
	for _, a := range args[1:] {
		switch a {
		case "--frozen-lockfile", "--immutable", "--no-audit", "--no-fund", "--prefer-offline", "--silent", "--quiet", "--ignore-scripts":
		default:
			return false
		}
	}
	return true
}

// npxHit: npx (or npm exec) would download what it runs without asking.
// Its own options end at the command's name: what follows is the command's
// (tsc -p ., vitest -p 4), not npx's -p PACKAGE (WP-856). With no terminal
// npx assumes --yes, so a command no node_modules on the way up has is a
// package of that name fetched from the registry: npx tsc in a checkout with
// no node_modules runs the package "tsc", which is not TypeScript.
func (g *guard) npxHit(name string, args []string) *guardHit {
	h := hit(ruleInstall, "installing software", name, firstWords(args, 3))
	cmd, prefix, no := "", "", false
	var workspaces []string
	cli := map[string]string{"npm exec": "npm", "bun x": "bunx"}[name]
	if cli == "" {
		cli = "npx"
	}
	for i := 0; i < len(args) && cmd == ""; i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				cmd = args[i+1]
			}
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			cmd = a
			break
		}
		o := readNpmOpt(a, cli)
		val := o.val
		if o.takes && !o.hasVal && i+1 < len(args) {
			i++
			val = args[i]
		}
		if !o.known {
			flag, _, hasVal := strings.Cut(a, "=")
			switch {
			case contains(pmValueFlags, flag) && !hasVal:
				i++
			case !strings.HasPrefix(a, "--") && strings.ContainsAny(a[1:], "yp"):
				return h
			}
			continue
		}
		if o.split && o.follow {
			return h
		}
		for j, n := range o.names {
			last := j == len(o.names)-1
			off := last && o.hasVal && val == "false"
			switch {
			case n == "package", n == "yes" && !off, n == "no" && off:
				return h
			case n == "yes", n == "no":
				no = true
			case n == "prefix" && last:
				prefix = val
			case n == "workspace" && last:
				workspaces = append(workspaces, val)
			}
		}
	}
	switch {
	case cmd == "":
		return nil
	case reVersioned.MatchString(cmd):
		return h
	case no || g.lost || g.installs || strings.ContainsAny(cmd, "$`") || strings.Contains(cmd, subPlaceholder) ||
		strings.HasPrefix(cmd, ".") || strings.HasPrefix(cmd, "~") || filepath.IsAbs(cmd):
		return nil
	}
	dir := g.dir
	if prefix != "" {
		if dir = g.site.resolve(g.dir, prefix); dir == "" {
			return nil
		}
	}
	if npmLocal(dir, cmd) {
		return nil
	}
	for _, w := range workspaces {
		wd := g.site.resolve(dir, w)
		if fi, err := os.Stat(wd); wd == "" || err != nil || !fi.IsDir() || npmLocal(wd, cmd) {
			return nil
		}
	}
	h.way = "There is no node_modules/.bin/" + cmd + " here, so " + name + " would download a package named `" + cmd +
		"` from the registry. Install the repository's own packages first (`npm ci` runs unattended), then run it again"
	return h
}

// npmLocal: cmd is installed in a node_modules from dir up, where npx looks
// for it: a bin of that name, or a package (npx @scope/tool).
func npmLocal(dir, cmd string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		nm := filepath.Join(d, "node_modules")
		if fi, err := os.Stat(filepath.Join(nm, ".bin", cmd)); err == nil && !fi.IsDir() {
			return true
		}
		if _, err := os.Stat(filepath.Join(nm, cmd, "package.json")); err == nil {
			return true
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
}

// remoteHit: reaching another machine.
func remoteHit(name string, args []string) *guardHit {
	h := func(what string) *guardHit { return hit(ruleRemote, "reaching another machine", name, what) }
	switch name {
	case "ssh", "scp", "sftp", "mosh", "mosh-client", "telnet", "ssh-copy-id", "ssh-keyscan", "sshpass",
		"kubectl", "helm", "k9s", "ansible", "ansible-playbook", "talosctl", "virsh", "ipmitool":
		return h(firstWords(args, 3))
	case "rsync":
		sc := scanArgs(args, "e", "--rsh", "--exclude", "--include", "--filter", "--port")
		for _, p := range sc.pos {
			if strings.HasPrefix(p, "rsync://") || (!strings.HasPrefix(p, "/") && !strings.HasPrefix(p, ".") && strings.Contains(p, ":") && !strings.Contains(p[:strings.Index(p, ":")], "/")) {
				return h(p)
			}
		}
	case "nc", "ncat", "netcat", "nmap":
		sc := scanArgs(args, "psw")
		if sc.flags["-l"] {
			return nil
		}
		for _, p := range sc.pos {
			if !isDigits(p) && !localHost(p) {
				return h(p)
			}
		}
	case "socat":
		for _, a := range args {
			if m := reSocatHost.FindStringSubmatch(a); m != nil && !localHost(m[2]) {
				return h(a)
			}
		}
	case "docker", "podman", "docker-compose":
		sc := scanArgs(globalDockerArgs(args), "H", "--host", "--context", "--connection", "--url")
		if sc.flags["-H"] || sc.flags["--host"] || sc.flags["--context"] || sc.flags["-c"] || sc.flags["--connection"] || sc.flags["--remote"] || sc.flags["-r"] || sc.flags["--url"] {
			return h("-H/--host/--context")
		}
		if len(args) >= 2 && args[0] == "context" && args[1] == "use" {
			return h("context use")
		}
	}
	return nil
}

var reSocatHost = lazyre.New(`(?i)^(tcp|udp|sctp|openssl|ssl)[46]?(-connect)?:([^:,]+)`)

func localHost(h string) bool {
	h = strings.ToLower(h)
	return h == "localhost" || h == "::1" || h == "0.0.0.0" || strings.HasPrefix(h, "127.")
}

// globalDockerArgs are the options before docker's subcommand.
func globalDockerArgs(args []string) []string {
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			return args[:i]
		}
		switch args[i] {
		case "-H", "--host", "--context", "-c", "--connection", "-l", "--log-level", "--config":
			i++
		}
	}
	return args
}

// hostReadOnly are what systemctl-like tools do without changing anything.
var hostReadOnly = map[string]bool{"status": true, "is-active": true, "is-enabled": true, "is-failed": true, "show": true, "cat": true,
	"list-units": true, "list-timers": true, "list-unit-files": true, "list-sockets": true, "list-dependencies": true,
	"list": true, "print": true, "blame": true, "version": true, "--version": true, "--status-all": true}

// hostHit: changing the machine itself.
func hostHit(name string, args []string) *guardHit {
	h := func() *guardHit { return hit(ruleHost, "changing the host", name, firstWords(args, 3)) }
	sc := scanArgs(args, "HMnotp", "--host", "--machine", "--type", "--lines", "--output", "--property")
	switch name {
	case "systemctl", "launchctl":
		if len(sc.pos) == 0 || !hostReadOnly[sc.pos[0]] {
			return h()
		}
	case "service":
		if len(sc.pos) >= 2 && hostReadOnly[sc.pos[1]] || sc.flags["--status-all"] {
			return nil
		}
		return h()
	case "systemd-run":
		return h()
	case "hostnamectl", "timedatectl", "localectl":
		if len(sc.pos) > 0 && strings.HasPrefix(sc.pos[0], "set") {
			return h()
		}
	case "crontab":
		cs := scanArgs(args, "u")
		if !cs.flags["-l"] || len(cs.pos) > 0 || cs.flags["-e"] || cs.flags["-r"] {
			return h()
		}
	case "useradd", "usermod", "userdel", "groupadd", "groupmod", "groupdel", "passwd", "chpasswd", "adduser", "deluser", "addgroup", "delgroup", "chage", "visudo", "vipw", "newusers":
		return h()
	case "iptables", "ip6tables", "iptables-restore", "ip6tables-restore", "nft", "ufw", "firewall-cmd":
		return h()
	case "mount":
		if len(args) > 0 {
			return h()
		}
	case "umount", "modprobe", "insmod", "rmmod", "reboot", "shutdown", "poweroff", "halt", "telinit", "swapon", "swapoff", "losetup", "fdisk", "parted", "mkfs", "update-grub", "grub-install":
		return h()
	case "sysctl":
		if sc.flags["-w"] || sc.flags["--write"] {
			return h()
		}
		for _, p := range sc.pos {
			if strings.Contains(p, "=") {
				return h()
			}
		}
	}
	if strings.HasPrefix(name, "mkfs.") {
		return h()
	}
	return nil
}

// looseHit: weakening the guard, or starting what it doesn't watch.
func (g *guard) looseHit(name string, args []string) *guardHit {
	switch name {
	case "unset":
		for _, a := range args {
			if guardMarkers[a] {
				return hit(ruleGuard, "unsetting what marks the session as unattended", "unset", a)
			}
		}
	case "export", "declare", "typeset", "readonly", "local":
		for _, a := range args {
			if isAssignment(a) {
				if h := g.assignHit(a); h != nil {
					return h
				}
				if h := fetchHomeHit(a); h != nil && name == "export" {
					return h
				}
			}
		}
		for _, a := range args {
			if guardMarkers[a] && contains(args, "-n") {
				return hit(ruleGuard, "unexporting what marks the session as unattended", name, a)
			}
		}
	case "claude":
		if len(args) == 1 && (args[0] == "--version" || args[0] == "-v" || args[0] == "-h" || args[0] == "--help") {
			return nil
		}
		return hit(ruleGuard, "starting another agent, which the guard does not watch", name, firstWords(args, 3))
	case "codex", "cursor-agent", "gemini", "aider", "goose", "opencode":
		return hit(ruleGuard, "starting another agent, which the guard does not watch (llm-task is fine)", name, firstWords(args, 3))
	case "git":
		for i, a := range args {
			if strings.HasPrefix(strings.ToLower(a), "core.hookspath") && (strings.Contains(a, "=") || i+1 < len(args)) {
				return hit(ruleGuard, "moving git's hooks", "git", a)
			}
		}
	}
	switch name {
	case "grep", "rg", "egrep", "fgrep", "ag", "echo", "printf", "git", "sed", "awk", "cat":
	default:
		for _, a := range args {
			if strings.HasPrefix(a, "--dangerously-skip-permissions") || strings.HasPrefix(a, "--allow-dangerously-skip-permissions") {
				return hit(ruleGuard, "skipping permission checks", name, a)
			}
		}
	}
	for _, a := range args {
		if reDaemonJSON.MatchString(a) {
			return hit(ruleHost, "touching the Docker daemon's configuration", name, a)
		}
	}
	return nil
}

// downloadHit: fetching from a host that isn't this machine, the Waypoint
// server or the repo's forge; or sending a change to one of the repo's
// environments, whose hosts are otherwise fine to read from (WP-749).
func (g *guard) downloadHit(name string, args []string) *guardHit {
	var urls []string
	switch name {
	case "curl", "fetch":
		sc := scanArgs(args, curlValShort, curlValLong...)
		urls = append(sc.pos, sc.val["--url"]...)
	case "wget":
		sc := scanArgs(args, wgetValShort, wgetValLong...)
		urls = sc.pos
	case "aria2c":
		sc := scanArgs(args, "dosjxkUi", "--dir", "--out", "--header", "--input-file", "--user-agent", "--max-connection-per-server", "--split", "--all-proxy")
		urls = sc.pos
	case "http", "https", "xh":
		sc := scanArgs(args, httpValShort, httpValLong...)
		pos := sc.pos
		if len(pos) > 1 && httpMethod(pos[0]) {
			pos = pos[1:]
		}
		if len(pos) > 0 {
			urls = pos[:1]
		}
	default:
		return nil
	}
	for _, u := range urls {
		if u == "" || strings.HasPrefix(u, "file://") {
			continue
		}
		if !strings.Contains(u, "://") && !hostish(u) {
			continue
		}
		host := g.urlHost(u)
		switch {
		case g.hostOK(host):
		case g.envHost(envHostPort(name, u, host)):
			bad := envChange(name, args)
			switch {
			case bad != "":
			case !envScheme(name, u):
				bad = "not http:// or https://"
			case slices.Contains(g.open, true):
				bad = "an argument the shell expands: " + args[slices.Index(g.open, true)]
			default:
				if f := fetchConfig(name); f != "" {
					bad = "settings in " + f
				}
			}
			if bad != "" {
				h := hit(ruleEnvChange, "sending more than a plain read ("+cut(bad, 60)+") to "+host+
					", one of the project's environments, which agents may only read from with GET, HEAD or OPTIONS", name, cut(u, 80))
				if bad != "not http:// or https://" {
					h.way = envReadWay
				}
				return h
			}
		default:
			shown := host
			if host == "" {
				shown = "an address the guard can't read (" + cut(u, 60) + ")"
			}
			h := hit(ruleDownload, "fetching from "+shown+", which is not "+allowedHostsText, name, cut(u, 80))
			if o := envOrigin(name, u); o != "" && host != "" {
				h.way = envVouchWay(host, o)
			}
			return h
		}
	}
	return nil
}

// The options of the fetch tools that take a value, so their values aren't
// read as addresses or other options.
var (
	curlValShort = "oHdXuAebcFTwmxKrEzYyUCDQ"
	curlValLong  = []string{"--output", "--header", "--data", "--data-raw", "--data-binary", "--data-urlencode", "--data-ascii", "--request", "--user",
		"--user-agent", "--referer", "--cookie", "--cookie-jar", "--form", "--upload-file", "--write-out", "--max-time", "--connect-timeout", "--proxy", "--config",
		"--range", "--cert", "--key", "--cacert", "--capath", "--retry", "--retry-delay", "--retry-max-time", "--json", "--limit-rate", "--max-filesize",
		"--oauth2-bearer", "--time-cond", "--dump-header", "--stderr", "--interface", "--url-query", "--resolve", "--connect-to", "--proxy-user", "--netrc-file",
		"--form-string", "--trace", "--trace-ascii", "--libcurl", "--etag-save", "--etag-compare", "--hsts", "--alt-svc", "--max-redirs"}
	wgetValShort = "oOaPeUTtwQiBlADIX"
	wgetValLong  = []string{"--output-document", "--output-file", "--append-output", "--directory-prefix", "--execute", "--user-agent", "--timeout",
		"--tries", "--wait", "--limit-rate", "--input-file", "--base", "--header", "--post-data", "--post-file", "--user", "--password", "--http-user", "--http-password",
		"--referer", "--load-cookies", "--save-cookies", "--bind-address", "--ca-certificate", "--certificate", "--private-key", "--accept", "--reject",
		"--method", "--body-data", "--body-file", "--max-redirect"}
	httpValShort = "AaoHpsP"
	httpValLong  = []string{"--auth", "--auth-type", "--output", "--session", "--session-read-only", "--timeout", "--proxy", "--verify", "--cert", "--cert-key",
		"--pretty", "--style", "--print", "--history-print", "--max-redirects"}
)

// hostish: a bare argument that curl would take for an address.
func hostish(u string) bool {
	if strings.HasPrefix(u, "$") {
		return true
	}
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") || strings.HasPrefix(u, "~") || strings.HasPrefix(u, "@") || strings.HasPrefix(u, "-") {
		return false
	}
	h := u
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	return strings.Contains(h, ".") || strings.Contains(h, ":") || h == "localhost"
}

// urlHost is the host of an address as written in a command; "" when it
// can't be known. $WAYPOINT_SERVER stands for the server's host.
func (g *guard) urlHost(raw string) string {
	r := raw
	if i := strings.Index(r, "://"); i >= 0 {
		r = r[i+3:]
	}
	for _, v := range []string{"$WAYPOINT_SERVER", "${WAYPOINT_SERVER}", "$WP_SERVER", "${WP_SERVER}"} {
		if strings.HasPrefix(r, v) {
			return g.server
		}
	}
	if i := strings.IndexAny(r, "/?#"); i >= 0 {
		r = r[:i]
	}
	if i := strings.LastIndex(r, "@"); i >= 0 {
		r = r[i+1:]
	}
	if strings.HasPrefix(r, "[") {
		if i := strings.Index(r, "]"); i > 0 {
			return strings.ToLower(r[1:i])
		}
	}
	if i := strings.Index(r, ":"); i >= 0 {
		r = r[:i]
	}
	if r == "" || strings.Contains(r, "$") || strings.Contains(r, subPlaceholder) {
		return ""
	}
	return strings.ToLower(r)
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// gitWriteVerbs change a repository.
var gitWriteVerbs = map[string]bool{"commit": true, "push": true, "add": true, "reset": true, "checkout": true, "switch": true, "merge": true, "rebase": true,
	"tag": true, "cherry-pick": true, "revert": true, "stash": true, "rm": true, "mv": true, "restore": true, "clean": true, "am": true, "apply": true, "pull": true}

// gitHit: a git command that changes a repository other than the session's,
// by cd, -C, --git-dir or --work-tree.
func (g *guard) gitHit(args []string) *guardHit {
	dir := g.dir
	verb := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" && i+1 < len(args):
			i++
			if p := g.site.resolve(dir, args[i]); p != "" {
				dir = p
			}
		case a == "-c" || a == "--namespace" || a == "--exec-path":
			i++
		case strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree=") || (a == "--git-dir" || a == "--work-tree") && i+1 < len(args):
			_, v, eq := strings.Cut(a, "=")
			if !eq { // --git-dir DIR: the value is the next word, not the verb
				i++
				v = args[i]
			}
			if p := g.site.resolve(dir, v); p != "" {
				dir = p
				if strings.HasPrefix(a, "--git-dir") {
					dir = filepath.Dir(p)
				}
			}
		case strings.HasPrefix(a, "-"):
		default:
			verb = a
		}
		if verb != "" {
			break
		}
	}
	if !gitWriteVerbs[verb] || !g.site.foreign(dir) {
		return nil
	}
	h := hit(ruleRepo, "changing a repository other than this session's", "git", dir, verb)
	h.ask = true
	return h
}

// writes checks where a command writes: redirects and the files the usual
// writers name.
func (g *guard) writes(c *guardCmd, name string, args []string) *guardHit {
	targets := append([]string(nil), c.Redirs...)
	name, args = runnerCommand(name, args) // npx prettier --write X writes X as prettier does
	targets = append(targets, writeTargets(name, args)...)
	targets = append(targets, g.patchTargets(c, name, args)...)
	targets = append(targets, scriptTargets(c, name, args)...)
	for _, t := range targets {
		p := g.site.resolve(g.dir, t)
		if p == "" || strings.HasPrefix(p, "/dev/") {
			continue
		}
		if h := g.keep(g.writeHit(p)); h != nil {
			return h
		}
	}
	return nil
}

// writeHit checks one path a command writes.
func (g *guard) writeHit(p string) *guardHit {
	switch {
	case reDaemonJSON.MatchString(p):
		return hit(ruleHost, "touching the Docker daemon's configuration", "write", p)
	case loosensGuard(p):
		return hit(ruleGuard, "changing Claude Code's settings, hooks or skills", "write", p)
	case isWaypointState(p):
		return hit(ruleGuard, stateWhy, "write", p)
	case isHostPath(p):
		return hit(ruleHost, "writing into the host's system directories", "write", p)
	case g.site.foreign(p):
		h := hit(ruleRepo, "writing into a repository other than this session's", "write", p)
		h.ask = true
		return h
	}
	return nil
}

// patchValueFlags are patch's options that take a value in the next word, so
// that word is neither the file patched nor the patch.
var patchValueFlags = map[string]bool{"-p": true, "--strip": true, "-F": true, "--fuzz": true, "-D": true, "--ifdef": true,
	"-B": true, "--prefix": true, "-Y": true, "--basename-prefix": true, "-z": true, "--suffix": true, "-V": true,
	"--version-control": true, "-g": true, "--get": true, "--reject-format": true, "--quoting-style": true, "--read-only": true}

// patchTargets are the files `git apply`, `git am` or `patch` writes. The
// patch names most of them, not the command line: it is read, whole, from
// every file the command names, from one fed with <, or from a heredoc.
// patch also writes the file it is told to patch (its first file argument),
// its output (-o) and its reject file (-r). A patch that arrives through a
// pipe (git diff | git apply), from something that is not a regular file, or
// from a file that is not there yet cannot be read, and is let through. A
// run that only checks or lists (--check, --stat, --dry-run) writes nothing,
// unless git is told to apply as well (--check --apply).
func (g *guard) patchTargets(c *guardCmd, name string, args []string) []string {
	dir := g.dir
	switch name {
	case "git":
		verb := -1
		for i := 0; i < len(args) && verb < 0; i++ {
			switch a := args[i]; {
			case a == "-C" && i+1 < len(args):
				i++
				if p := g.site.resolve(dir, args[i]); p != "" {
					dir = p
				}
			case a == "--work-tree" && i+1 < len(args): // git writes under it, as gitHit reads it
				i++
				if p := g.site.resolve(dir, args[i]); p != "" {
					dir = p
				}
			case strings.HasPrefix(a, "--work-tree="):
				if p := g.site.resolve(dir, a[len("--work-tree="):]); p != "" {
					dir = p
				}
			case trscript.GitValueFlags(a): // -c, --git-dir …: the next word is a value, not the verb
				i++
			case strings.HasPrefix(a, "-"):
			default:
				verb = i
			}
		}
		if verb < 0 || args[verb] != "apply" && args[verb] != "am" {
			return nil
		}
		args = args[verb+1:]
	case "patch":
	default:
		return nil
	}
	var files, wrote []string // patches to read; files the command line itself names as written
	cd := func(to string) {
		if p := g.site.resolve(dir, to); p != "" {
			dir = p
		}
	}
	first, lists, applies := true, false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		isPatch := name == "patch"
		next := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case a == "--check" || a == "--stat" || a == "--numstat" || a == "--summary" || a == "--dry-run":
			lists = true
		case !isPatch && a == "--apply":
			applies = true // git apply --check --apply: lists and applies
		case isPatch && (a == "-i" || a == "--input"):
			if v, ok := next(); ok {
				files = append(files, v)
			}
		case a == "--directory" || isPatch && a == "-d":
			if v, ok := next(); ok {
				cd(v)
			}
		case isPatch && (a == "-o" || a == "--output" || a == "-r" || a == "--reject-file"):
			if v, ok := next(); ok {
				wrote = append(wrote, v)
			}
		case isPatch && patchValueFlags[a]:
			i++
		case strings.HasPrefix(a, "--input="):
			files = append(files, a[len("--input="):])
		case strings.HasPrefix(a, "--directory="):
			cd(a[len("--directory="):])
		case isPatch && strings.HasPrefix(a, "--output="):
			wrote = append(wrote, a[len("--output="):])
		case isPatch && strings.HasPrefix(a, "--reject-file="):
			wrote = append(wrote, a[len("--reject-file="):])
		case isPatch && strings.HasPrefix(a, "-d") && len(a) > 2:
			cd(a[2:])
		case isPatch && strings.HasPrefix(a, "-i") && len(a) > 2:
			files = append(files, a[2:])
		case isPatch && (strings.HasPrefix(a, "-o") || strings.HasPrefix(a, "-r")) && len(a) > 2:
			wrote = append(wrote, a[2:])
		case isPatch && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 2:
			// Short options run together (-Ni FILE, -Nifix.diff, -so OUT): the
			// first letter that takes a value takes the rest of the word, or
			// the next word when nothing follows.
			for j := 1; j < len(a); j++ {
				if !strings.ContainsRune("iordpDBYzgFV", rune(a[j])) {
					continue
				}
				v := a[j+1:]
				if v == "" {
					v, _ = next()
				}
				switch a[j] {
				case 'i':
					files = append(files, v)
				case 'd':
					cd(v)
				case 'o', 'r':
					wrote = append(wrote, v)
				}
				break
			}
		case strings.HasPrefix(a, "-"):
		default:
			if isPatch && first {
				wrote = append(wrote, a) // patch FILE [PATCH]: the file patched
			}
			first = false
			files = append(files, a) // git: a patch; patch: read too, in case it is the patch
		}
	}
	if lists && !applies {
		return nil
	}
	files = append(files, c.Ins...)
	var out []string
	add := func(f string) {
		if !filepath.IsAbs(f) {
			f = filepath.Join(dir, f)
		}
		out = append(out, f)
	}
	for _, f := range wrote {
		add(f)
	}
	patchFilesIn(strings.NewReader(c.Heredoc), add)
	read := map[string]bool{}
	for _, f := range files {
		for _, from := range slices.Compact([]string{dir, g.dir}) { // after -C or -d, or as the shell opens a < file
			p := g.site.resolve(from, f)
			if p == "" || read[p] {
				continue
			}
			read[p] = true
			if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
				continue
			}
			if fh, err := os.Open(p); err == nil { // #nosec G304 G703 -- the patch the command is about to apply, read to see what it changes
				patchFilesIn(fh, add)
				_ = fh.Close()
			}
		}
	}
	return out
}

// patchFiles are the files a patch's headers name: see patchFilesIn.
func patchFiles(text string) []string {
	var out []string
	patchFilesIn(strings.NewReader(text), func(f string) { out = append(out, f) })
	return out
}

// patchFilesIn reads a patch to its end and gives add each file its headers
// name (---, +++, diff --git, rename and copy lines), as written and with
// its first directory dropped: git writes a/ and b/ before a path and -p1
// strips it, and the guard does not know which -p the command uses. It holds
// one line at a time, so the patch's size is no limit; a line longer than
// the buffer is read by its start, which is where a header is.
func patchFilesIn(r io.Reader, add func(string)) {
	seen := map[string]bool{}
	name := func(f string) {
		f, _, _ = strings.Cut(f, "\t")
		f = strings.Trim(strings.TrimSpace(f), `"`)
		if f == "" || f == "/dev/null" {
			return
		}
		for _, v := range []string{f, f[strings.IndexByte(f, '/')+1:]} {
			if v != "" && !seen[v] {
				seen[v] = true
				add(v)
			}
		}
	}
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		b, more, err := br.ReadLine()
		if err != nil {
			return
		}
		line := strings.TrimRight(string(b), "\r")
		for more && err == nil {
			_, more, err = br.ReadLine()
		}
		for _, pre := range []string{"+++ ", "--- ", "rename to ", "rename from ", "copy to "} {
			if f, ok := strings.CutPrefix(line, pre); ok {
				name(f)
			}
		}
		if f, ok := strings.CutPrefix(line, "diff --git "); ok {
			if i := strings.LastIndex(f, " b/"); i >= 0 {
				name(f[:i])
				name(f[i+1:])
			}
		}
	}
}

// scriptTargets are the files a script given to an interpreter (python3 -c,
// node -e, a heredoc on its input) opens for writing, as far as the script
// spells them: the same reading the usage report's write recogniser makes
// (internal/transcript). A path the script builds at run time is not seen.
func scriptTargets(c *guardCmd, name string, args []string) []string {
	// One rule with the recogniser for what runs a script and where it is.
	script := trscript.InlineScript(append([]string{name}, args...), c.Heredoc)
	if script == "" {
		return nil
	}
	return trscript.ScriptWrites(script)
}

// writeTargets are the paths a command writes, as far as its name and
// arguments say.
func writeTargets(name string, args []string) []string {
	switch name {
	case "tee":
		return scanArgs(args, "").pos
	case "cp", "install", "ln", "rsync", "mv":
		sc := scanArgs(args, "tSt", "--target-directory", "--suffix")
		if t := append(sc.val["-t"], sc.val["--target-directory"]...); len(t) > 0 {
			return t
		}
		if name == "mv" {
			return sc.pos
		}
		if len(sc.pos) >= 2 {
			return sc.pos[len(sc.pos)-1:]
		}
	case "sed":
		sc := scanArgs(args, "ef", "--expression", "--file")
		inplace := sc.flags["--in-place"]
		for f := range sc.flags {
			if strings.HasPrefix(f, "--in-place") || f == "-i" {
				inplace = true
			}
		}
		if !inplace {
			return nil
		}
		if len(sc.val["-e"])+len(sc.val["-f"])+len(sc.val["--expression"])+len(sc.val["--file"]) == 0 && len(sc.pos) > 0 {
			return sc.pos[1:]
		}
		return sc.pos
	case "perl":
		return trscript.PerlInPlaceFiles(args) // one rule with the usage report's recogniser
	case "gofmt", "goimports", "prettier":
		return trscript.FormatterFiles(args)
	case "dd":
		var out []string
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "of="); ok {
				out = append(out, v)
			}
		}
		return out
	case "chmod", "chown", "chgrp":
		sc := scanArgs(args, "", "--reference")
		if len(sc.pos) > 1 {
			return sc.pos[1:]
		}
	case "touch", "mkdir", "rm", "rmdir", "unlink", "shred", "truncate": // patch: see patchTargets, which tells the file patched from the patch read
		return scanArgs(args, "sr").pos
	case "curl":
		return scanArgs(args, curlValShort, curlValLong...).values("-o", "--output", "-D", "--dump-header", "-c", "--cookie-jar", "--stderr", "--trace",
			"--trace-ascii", "--libcurl", "--etag-save", "--hsts", "--alt-svc")
	case "wget":
		return scanArgs(args, wgetValShort, wgetValLong...).values("-O", "--output-document", "-P", "--directory-prefix", "-o", "--output-file", "-a",
			"--append-output", "--save-cookies")
	case "http", "https", "xh":
		return scanArgs(args, httpValShort, httpValLong...).values("-o", "--output")
	case "wp":
		return wpWriteTargets(args)
	case "tar":
		sc := scanArgs(args, "C", "--directory")
		return append(sc.val["-C"], sc.val["--directory"]...)
	case "unzip":
		return scanArgs(args, "d").val["-d"]
	}
	return nil
}

// runnerCommand reads through a package runner (npx, bunx, npm exec, pnpm
// exec or dlx, yarn exec or dlx, a bare pnpm BIN or yarn BIN, bun x) to the program it
// runs and its arguments, which the write rules then judge as if run
// directly. Its own options, and the values of those that take one, are
// skipped; a command that is not a runner comes back unchanged.
func runnerCommand(name string, args []string) (string, []string) {
	for range 4 {
		var rest []string
		runner := name
		switch name {
		case "npx", "bunx":
			rest = args
		case "npm", "bun":
			r, _, ok := pmExec(name, args)
			if !ok {
				return name, args
			}
			rest = r
		case "pnpm", "yarn":
			rest = dropFlags(args, "-C", "--dir", "--cwd", "--filter", "-F")
			if len(rest) == 0 {
				return name, args
			}
			switch rest[0] {
			case "exec", "dlx":
				rest = rest[1:]
			case "run", "install", "i", "add", "remove", "rm", "up", "update", "upgrade", "init", "link", "unlink", "publish", "pack",
				"test", "start", "build", "cache", "config", "info", "why", "list", "ls", "audit", "outdated", "global", "create", "set", "node", "workspace", "workspaces":
				return name, args
			}
		default:
			return name, args
		}
		// The runner's own options: -p/--package and the like take a value,
		// -c runs a shell string (left as it is).
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") && rest[0] != "-" {
			w := rest[0]
			rest = rest[1:]
			switch {
			case w == "--":
				w = ""
			case w == "-c" || w == "--call":
				return name, args
			case !strings.Contains(w, "=") && (w == "--package" || w == "--prefix" || w == "--workspace" || w == "--cache" || w == "--registry" ||
				!strings.HasPrefix(w, "--") && strings.HasSuffix(w, "p") || w == "-w"):
				if len(rest) > 0 {
					rest = rest[1:]
				}
			}
			if w == "" {
				break
			}
		}
		if len(rest) == 0 {
			return name, args
		}
		name, args = path.Base(rest[0]), rest[1:]
		if len(args) > 0 && args[0] == "--" && (runner == "npm" || runner == "bun") {
			args = args[1:] // npm exec CMD -- ARGS: the -- only ends npm's options
		}
	}
	return name, args
}
