package main

import (
	"path"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Risky commands (WP-647, under WP-100): shell commands an agent ran that
// delete, discard or overwrite what can't be had back, or that reach
// production. Plain patterns over the words shellWords splits out, no
// model; `wp usage sync` sends each one found (redacted, clipped) with
// whether it ran, so a person can audit them under Agents → Audit. Every
// other command still leaves the machine as its head alone.
//
// Levels: high reaches production, another machine, a cluster or a disk,
// rewrites shared history, or deletes outside any checkout (Waypoint tells
// the person when one ran); medium loses local work or state that a person
// might want back.

// riskyHit is one risky command in a shell line.
type riskyHit struct {
	Kind    string `json:"k"`           // delete, discard, force_push, push_main, branch_delete, history, sql, cluster, deploy, service, docker, disk, power, permissions, remote_script
	Level   string `json:"l"`           // high, medium
	Label   string `json:"d"`           // what it does, in a few words: "force push to main"
	Segment string `json:"s,omitempty"` // the command, as its words
	Command string `json:"c,omitempty"` // the whole shell line, redacted, as sent (WP-860)
}

const (
	riskHigh   = "high"
	riskMedium = "medium"
)

// localKinds stay at their level on another machine: files and checkouts
// there are no more production than here.
var localKinds = map[string]bool{"delete": true, "discard": true, "branch_delete": true, "push_main": true, "force_push": true, "history": true}

// riskyMaxDepth bounds bash -c and ssh nesting.
const riskyMaxDepth = 3

// riskyCommand classifies a shell line. The weightiest hit wins (high over
// medium, then the first); nil when nothing in it is risky.
func riskyCommand(cmd string) *riskyHit {
	var best *riskyHit
	riskyWalk(cmd, 0, false, func(h riskyHit) {
		if best == nil || h.Level == riskHigh && best.Level != riskHigh {
			c := h
			best = &c
		}
	})
	if best == nil && pipedScript.MatchString(cmd) {
		best = &riskyHit{Kind: "remote_script", Level: riskMedium, Label: "runs a downloaded script", Segment: pipedScript.FindString(cmd)}
	}
	return best
}

// sqlClients run SQL; their whole command line (here-documents included)
// is read for destructive statements.
var sqlClients = map[string]bool{"sqlite3": true, "psql": true, "mysql": true, "mariadb": true, "clickhouse-client": true, "duckdb": true}

var (
	sqlDropRE      = lazyre.New(`(?i)\bdrop\s+(database|schema)\b`)
	sqlTableRE     = lazyre.New(`(?i)\b(drop\s+table|truncate(\s+table)?\s+\w)`)
	sqlDeleteRE    = lazyre.New(`(?i)\bdelete\s+from\s+[\w."]+\s*(;|\z|"|')`)
	sqlUpdateAllRE = lazyre.New(`(?i)\bupdate\s+[\w."]+\s+set\s+[^;]*(;|\z)`)
	sqlLocalRE     = lazyre.New(`\b(localhost|127\.0\.0\.1|::1)\b|\.(db|sqlite3?)\b`)
	sqlWhereRE     = lazyre.New(`(?i)\bwhere\b`)
)

func riskyWalk(cmd string, depth int, remote bool, hit func(riskyHit)) {
	lower := strings.ToLower(cmd)
	sqlSeen := false
	vars := map[string]string{}
	for _, seg := range shellWords(cmd) {
		noteVars(seg, vars)
		root := false
		for _, w := range seg {
			if assignRE.MatchString(w) {
				continue
			}
			b := path.Base(w)
			root = b == "sudo" || b == "doas" || b == "pkexec"
			break
		}
		words := stripWords(seg)
		if len(words) == 0 {
			continue
		}
		prog := strings.TrimSuffix(path.Base(words[0]), ".exe")
		args := words[1:]
		if depth < riskyMaxDepth {
			if shells[prog] {
				if inner, ok := shellDashC(args); ok {
					riskyWalk(inner, depth+1, remote, hit)
					continue
				}
			}
			if prog == "ssh" {
				if inner := sshCommand(args); inner != "" {
					riskyWalk(inner, depth+1, true, hit)
				}
				continue
			}
		}
		h, ok := riskySegment(prog, args, root, vars)
		if !ok && sqlClients[prog] && !sqlSeen {
			sqlSeen = true
			h, ok = riskySQL(lower)
		}
		if !ok {
			continue
		}
		if remote {
			h.Label += " on another machine"
			if !localKinds[h.Kind] {
				h.Level = riskHigh // a server's services, data or containers
			}
		}
		h.Segment = strings.Join(words, " ")
		hit(h)
	}
}

// sshCommand is the command `ssh [opts] host CMD…` runs there, "" for a login.
func sshCommand(args []string) string {
	valued := "BbcDEeFIiJLlmOoPpQRSWw" // ssh's flags that take a value
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) >= 2 {
			if len(a) == 2 && strings.ContainsRune(valued, rune(a[1])) {
				i++
			}
			continue
		}
		return strings.Join(args[i+1:], " ")
	}
	return ""
}

func riskySQL(lower string) (riskyHit, bool) {
	switch {
	case sqlDropRE.MatchString(lower) && sqlLocalRE.MatchString(lower):
		return riskyHit{Kind: "sql", Level: riskMedium, Label: "drops a local database"}, true
	case sqlDropRE.MatchString(lower):
		return riskyHit{Kind: "sql", Level: riskHigh, Label: "drops a database"}, true
	case sqlTableRE.MatchString(lower):
		return riskyHit{Kind: "sql", Level: riskMedium, Label: "drops or empties a table"}, true
	case sqlDeleteRE.MatchString(lower):
		return riskyHit{Kind: "sql", Level: riskMedium, Label: "deletes every row of a table"}, true
	}
	for _, m := range sqlUpdateAllRE.FindAllString(lower, -1) {
		if !sqlWhereRE.MatchString(m) {
			return riskyHit{Kind: "sql", Level: riskMedium, Label: "updates every row of a table"}, true
		}
	}
	return riskyHit{}, false
}

// flags splits arguments into short-flag letters, long flags and the rest.
func flags(args []string) (short string, long map[string]bool, rest []string) {
	long = map[string]bool{}
	for i, a := range args {
		switch {
		case a == "--":
			return short, long, append(rest, args[i+1:]...)
		case strings.HasPrefix(a, "--"):
			k, _, _ := strings.Cut(a[2:], "=")
			long[k] = true
		case strings.HasPrefix(a, "-") && len(a) > 1:
			short += a[1:]
		default:
			rest = append(rest, a)
		}
	}
	return short, long, rest
}

func riskySegment(prog string, args []string, root bool, vars map[string]string) (riskyHit, bool) {
	switch prog {
	case "rm":
		return riskyRm(args, root, vars)
	case "git":
		return riskyGit(args)
	case "find":
		if slices.Contains(args, "-delete") {
			allScratch, n := true, 0
			for _, a := range args {
				if strings.HasPrefix(a, "-") {
					break
				}
				n++
				if c := path.Clean(expandVars(a, vars)); !scratchPath(c) && !strings.Contains(c, "/.cache/") && !strings.HasSuffix(c, "/.cache") {
					allScratch = false
				}
			}
			if n > 0 && allScratch {
				return riskyHit{}, false // emptying a cache or scratch space
			}
			return riskyHit{Kind: "delete", Level: riskMedium, Label: "find -delete"}, true
		}
	case "shred", "wipefs", "mkfs", "fdisk", "sfdisk", "parted", "mkswap":
		return riskyHit{Kind: "disk", Level: riskHigh, Label: prog}, true
	case "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=/dev/") && !strings.HasPrefix(a, "of=/dev/null") {
				return riskyHit{Kind: "disk", Level: riskHigh, Label: "dd onto a device"}, true
			}
		}
	case "shutdown", "reboot", "halt", "poweroff":
		return riskyHit{Kind: "power", Level: riskHigh, Label: prog}, true
	case "kubectl", "k3s":
		return riskyKubectl(prog, args)
	case "helm":
		if sub := firstWord(args, subcommandTools["helm"]); sub == "uninstall" || sub == "delete" || sub == "rollback" {
			return riskyHit{Kind: "cluster", Level: riskHigh, Label: "helm " + sub}, true
		}
	case "terraform", "tofu":
		if sub := firstWord(args, subcommandTools["terraform"]); sub == "destroy" {
			return riskyHit{Kind: "cluster", Level: riskHigh, Label: prog + " destroy"}, true
		}
	case "make", "gmake":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && (strings.Contains(a, "deploy") || strings.Contains(a, "rollback")) &&
				(strings.Contains(a, "remote") || strings.Contains(a, "prod")) {
				return riskyHit{Kind: "deploy", Level: riskHigh, Label: "make " + a}, true
			}
		}
	case "systemctl":
		sub := firstWord(args, subcommandTools["systemctl"])
		switch sub {
		case "stop", "disable", "mask":
			return riskyHit{Kind: "service", Level: riskMedium, Label: "systemctl " + sub}, true
		}
	case "docker", "podman":
		return riskyDocker(prog, args)
	case "chmod", "chown":
		short, long, rest := flags(args)
		if strings.ContainsAny(short, "R") || long["recursive"] {
			for _, p := range rest {
				if systemPath(p) {
					return riskyHit{Kind: "permissions", Level: riskHigh, Label: prog + " -R on " + p}, true
				}
			}
			if prog == "chmod" && len(rest) > 0 && strings.HasSuffix(rest[0], "777") {
				return riskyHit{Kind: "permissions", Level: riskMedium, Label: "chmod -R 777"}, true
			}
		}
	}
	return riskyHit{}, false
}

func firstWord(args []string, valued map[string]bool) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if valued[a] {
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// systemPath: a path outside any checkout or scratch space, whose loss a
// machine notices: /, home, /etc, /usr, /var (not /var/tmp)…
func systemPath(p string) bool {
	p = strings.Trim(p, `"'`)
	switch p {
	case "/", "/*", "~", "~/", "~/*", "$HOME", "$HOME/", "${HOME}", "$HOME/*":
		return true
	}
	if !strings.HasPrefix(p, "/") {
		return false
	}
	clean := path.Clean(p)
	for _, safe := range []string{"/tmp", "/var/tmp", "/dev/shm", "/run/user"} {
		if clean == safe || strings.HasPrefix(clean, safe+"/") {
			return false
		}
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	switch parts[0] {
	case "etc", "usr", "bin", "sbin", "lib", "lib64", "boot", "root", "opt", "srv", "var":
		return true
	case "home", "Users", "projects", "mnt", "media":
		return len(parts) <= 2 // a home, a whole repo or disk, not something inside one
	}
	return false
}

func riskyRm(args []string, root bool, vars map[string]string) (riskyHit, bool) {
	short, long, rest := flags(args)
	if !strings.ContainsAny(short, "rR") && !long["recursive"] {
		return riskyHit{}, false
	}
	for _, p := range rest {
		if systemPath(expandVars(p, vars)) {
			return riskyHit{Kind: "delete", Level: riskHigh, Label: "recursive delete of " + strings.Trim(p, `"'`)}, true
		}
	}
	scratch := len(rest) > 0
	for _, p := range rest {
		c := path.Clean(strings.Trim(expandVars(p, vars), `"'`))
		if !scratchPath(c) && !rebuildable(c) {
			scratch = false
		}
	}
	if scratch && !root {
		return riskyHit{}, false // a scratch or build directory: what it held is made again
	}
	label := "recursive delete"
	if root {
		label += " as root"
	}
	if len(rest) > 0 {
		label += " of " + strings.Trim(rest[0], `"'`)
	}
	return riskyHit{Kind: "delete", Level: riskMedium, Label: label}, true
}

// scratchPath: under a temporary directory.
func scratchPath(p string) bool {
	for _, t := range []string{"/tmp", "/var/tmp", "/dev/shm", "$TMPDIR", "${TMPDIR}"} {
		if p == t || strings.HasPrefix(p, t+"/") {
			return true
		}
	}
	return false
}

// noteVars remembers NAME=value assignments standing alone in a segment
// (S=/tmp/x; export S=/tmp/x), so a later rm -rf "$S/y" can be read; a
// value from mktemp is a temporary directory.
func noteVars(seg []string, vars map[string]string) {
	words := seg
	if len(words) > 0 && (words[0] == "export" || words[0] == "local" || words[0] == "readonly" || words[0] == "declare") {
		words = words[1:]
	}
	for _, w := range words {
		if !assignRE.MatchString(w) {
			return
		}
	}
	for _, w := range words {
		name, val, _ := strings.Cut(w, "=")
		name = strings.TrimSuffix(name, "+")
		if strings.HasPrefix(val, "$(mktemp") || strings.HasPrefix(val, "`mktemp") {
			val = "/tmp/mktemp"
		}
		vars[name] = expandVars(val, vars)
	}
}

var varRE = lazyre.New(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// expandVars puts known variables' values into a word.
func expandVars(w string, vars map[string]string) string {
	if !strings.Contains(w, "$") || len(vars) == 0 {
		return w
	}
	return varRE.ReplaceAllStringFunc(w, func(m string) string {
		if v, ok := vars[varRE.FindStringSubmatch(m)[1]]; ok {
			return v
		}
		return m
	})
}

// rebuildable: build output and caches, made again by the next build.
func rebuildable(p string) bool {
	switch path.Base(p) {
	case "node_modules", "dist", "build", "target", ".next", ".cache", "__pycache__", ".pytest_cache", "coverage", ".turbo", ".vite",
		"test-results", "playwright-report", ".mypy_cache", ".ruff_cache":
		for _, home := range []string{"~/", "$HOME/", "${HOME}/"} {
			if strings.HasPrefix(p, home) {
				p = "/~/" + p[len(home):]
			}
		}
		for _, h := range []string{"/home/", "/Users/"} {
			if strings.HasPrefix(p, h) {
				if _, after, ok := strings.Cut(p[len(h):], "/"); ok {
					p = "/~/" + after
				}
			}
		}
		return !strings.HasPrefix(p, "/") || strings.Count(p, "/") > 2 // inside a project, not ~/build
	}
	return false
}

var protectedBranch = lazyre.New(`^(\+?(refs/heads/)?|.*:(refs/heads/)?)(main|master|production|prod|release[\w./-]*|trunk|develop)$`)

func riskyGit(args []string) (riskyHit, bool) {
	sub := firstWord(args, subcommandTools["git"])
	i := 0
	for i < len(args) && args[i] != sub {
		i++
	}
	if i >= len(args) {
		return riskyHit{}, false
	}
	rest := args[i+1:]
	short, long, pos := flags(rest)
	switch sub {
	case "push":
		force := strings.Contains(short, "f") || long["force"] || long["force-with-lease"] || long["force-if-includes"] || long["mirror"]
		deleting := strings.Contains(short, "d") || long["delete"] || long["prune"]
		main := false
		for j, p := range pos {
			if j == 0 && len(pos) > 1 {
				continue // the remote
			}
			if strings.HasPrefix(p, "+") {
				force = true
			}
			if strings.HasPrefix(p, ":") && len(p) > 1 {
				deleting = true
			}
			if protectedBranch.MatchString(p) {
				main = true
			}
		}
		switch {
		case force && main:
			return riskyHit{Kind: "force_push", Level: riskHigh, Label: "force push to a protected branch"}, true
		case deleting && main:
			return riskyHit{Kind: "branch_delete", Level: riskHigh, Label: "deletes a protected branch on the remote"}, true
		case force && long["mirror"]:
			return riskyHit{Kind: "force_push", Level: riskHigh, Label: "mirror push"}, true
		case force:
			label := "force push"
			if long["force-with-lease"] && !strings.Contains(short, "f") && !long["force"] {
				label = "force push with lease"
			}
			return riskyHit{Kind: "force_push", Level: riskMedium, Label: label}, true
		case deleting:
			return riskyHit{Kind: "branch_delete", Level: riskMedium, Label: "deletes a branch on the remote"}, true
		case main:
			return riskyHit{Kind: "push_main", Level: riskMedium, Label: "push straight to a protected branch"}, true
		}
	case "reset":
		if long["hard"] || long["merge"] || long["keep"] {
			return riskyHit{Kind: "discard", Level: riskMedium, Label: "git reset --hard"}, true
		}
	case "clean":
		if (strings.ContainsAny(short, "f") || long["force"]) && (!strings.Contains(short, "X") || strings.Contains(short, "x")) {
			return riskyHit{Kind: "discard", Level: riskMedium, Label: "git clean -f"}, true // -X alone: only ignored files, made again by a build
		}
	case "checkout", "restore":
		if sub == "checkout" && (strings.Contains(short, "f") || long["force"]) {
			return riskyHit{Kind: "discard", Level: riskMedium, Label: "git checkout --force"}, true
		}
		if sub == "restore" && (long["staged"] && !long["worktree"] && !strings.Contains(short, "W")) {
			return riskyHit{}, false
		}
		for j, a := range rest {
			if a == "--" || sub == "restore" || sub == "checkout" {
				tail := rest[j:]
				if a == "--" {
					tail = rest[j+1:]
				}
				for _, p := range tail {
					if p == "." || p == ":/" || p == "*" {
						return riskyHit{Kind: "discard", Level: riskMedium, Label: "git " + sub + " of the whole tree"}, true
					}
				}
				break
			}
		}
	case "branch":
		if strings.Contains(short, "D") || (strings.Contains(short, "d") && long["force"]) {
			return riskyHit{Kind: "branch_delete", Level: riskMedium, Label: "git branch -D"}, true
		}
	case "stash":
		if len(pos) > 0 && (pos[0] == "drop" || pos[0] == "clear") {
			return riskyHit{Kind: "discard", Level: riskMedium, Label: "git stash " + pos[0]}, true
		}
	case "worktree":
		if len(pos) > 0 && pos[0] == "remove" && (strings.Contains(short, "f") || long["force"]) {
			return riskyHit{Kind: "discard", Level: riskMedium, Label: "git worktree remove --force"}, true
		}
	case "filter-branch", "filter-repo":
		return riskyHit{Kind: "history", Level: riskHigh, Label: "git " + sub}, true
	case "update-ref":
		if strings.Contains(short, "d") {
			return riskyHit{Kind: "branch_delete", Level: riskMedium, Label: "git update-ref -d"}, true
		}
	case "reflog":
		if len(pos) > 0 && (pos[0] == "expire" || pos[0] == "delete") {
			return riskyHit{Kind: "history", Level: riskMedium, Label: "git reflog " + pos[0]}, true
		}
	}
	return riskyHit{}, false
}

func riskyKubectl(prog string, args []string) (riskyHit, bool) {
	if prog == "k3s" {
		if len(args) == 0 || args[0] != "kubectl" {
			return riskyHit{}, false
		}
		args = args[1:]
	}
	sub := firstWord(args, subcommandTools["kubectl"])
	switch sub {
	case "delete", "drain", "cordon":
		return riskyHit{Kind: "cluster", Level: riskHigh, Label: "kubectl " + sub}, true
	case "scale":
		for i, a := range args {
			if a == "--replicas=0" || (a == "--replicas" && i+1 < len(args) && args[i+1] == "0") {
				return riskyHit{Kind: "cluster", Level: riskHigh, Label: "kubectl scale to 0"}, true
			}
		}
	case "rollout":
		for _, a := range args {
			switch a {
			case "undo":
				return riskyHit{Kind: "deploy", Level: riskHigh, Label: "kubectl rollout undo"}, true
			case "restart":
				return riskyHit{Kind: "deploy", Level: riskMedium, Label: "kubectl rollout restart"}, true
			}
		}
	}
	return riskyHit{}, false
}

// composeArgs removes the value-taking flags of docker compose (and their values).
func composeArgs(args []string) []string {
	valued := subcommandTools["docker-compose"]
	var out []string
	for i := 0; i < len(args); i++ {
		if valued[args[i]] {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func riskyDocker(prog string, args []string) (riskyHit, bool) {
	sub := firstWord(args, subcommandTools["docker"])
	var pos []string
	for i, a := range args {
		if a == sub {
			rest := args[i+1:]
			if sub == "compose" { // compose's own -f/-p take a value; drop them so the verb is first
				rest = composeArgs(rest)
			}
			_, _, pos = flags(rest)
			break
		}
	}
	short, long, _ := flags(args)
	switch {
	case sub == "volume" && len(pos) > 0 && (pos[0] == "prune" || pos[0] == "rm"):
		return riskyHit{Kind: "docker", Level: riskHigh, Label: prog + " volume " + pos[0]}, true // volumes hold databases
	case sub == "system" && len(pos) > 0 && pos[0] == "prune":
		if long["volumes"] {
			return riskyHit{Kind: "docker", Level: riskHigh, Label: prog + " system prune --volumes"}, true
		}
		return riskyHit{Kind: "docker", Level: riskMedium, Label: prog + " system prune"}, true
	case sub == "compose" && len(pos) > 0 && pos[0] == "down" && (long["volumes"] || strings.Contains(short, "v")):
		return riskyHit{Kind: "docker", Level: riskHigh, Label: prog + " compose down --volumes"}, true
	}
	return riskyHit{}, false
}

// pipedScript: curl … | sh runs whatever the server sends.
var pipedScript = lazyre.New(`\b(curl|wget)\b[^|;&]*\|\s*(sudo\s+)?(ba|z|da)?sh\b`)
