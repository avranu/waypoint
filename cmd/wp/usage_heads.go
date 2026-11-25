package main

import (
	"path"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Command heads (WP-D20): what a Bash call ran, without its arguments. A
// chain "cd x && git commit -m msg | tee log" is three heads: cd, git commit,
// tee. Arguments never leave the machine this way; only the program and, for
// tools whose first word picks what they do, that subcommand.

// shellWords splits a command line into segments (at && || ; | & and
// newlines) of words with quotes removed. It follows the shell closely
// enough for heads, not for running anything: quoted strings and $(…) stay
// inside their word, here-document bodies and comments are skipped,
// redirections are dropped.
func shellWords(cmd string) [][]string {
	var segs [][]string
	var cur []string
	var word strings.Builder
	inWord := false
	var heredocs []string
	flushWord := func() {
		if inWord {
			cur = append(cur, word.String())
			word.Reset()
			inWord = false
		}
	}
	endSeg := func() {
		flushWord()
		if len(cur) > 0 {
			segs = append(segs, cur)
		}
		cur = nil
	}
	r := []rune(cmd)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			flushWord()
		case c == '\n':
			endSeg()
			for len(heredocs) > 0 { // skip the body of each here-document
				delim := heredocs[0]
				heredocs = heredocs[1:]
				for i+1 < len(r) {
					j := i + 1
					for j < len(r) && r[j] != '\n' {
						j++
					}
					line := strings.TrimSpace(string(r[i+1 : j]))
					i = j
					if line == delim || j >= len(r) {
						break
					}
				}
			}
		case c == ';':
			endSeg()
		case c == '|':
			endSeg()
			if i+1 < len(r) && (r[i+1] == '|' || r[i+1] == '&') {
				i++
			}
		case c == '&':
			if i+1 < len(r) && r[i+1] == '&' {
				endSeg()
				i++
			} else if i+1 < len(r) && r[i+1] == '>' || inWord && strings.HasSuffix(word.String(), ">") || i > 0 && r[i-1] == '>' {
				word.WriteRune(c) // 2>&1, &>file
				inWord = true
			} else {
				endSeg()
			}
		case c == '#' && !inWord:
			for i < len(r) && r[i] != '\n' {
				i++
			}
			i--
		case c == ')' || (c == '}' && !inWord):
			endSeg() // a subshell or group closing
		case (c == '(' || c == '{') && !inWord:
			// opening one: not a word of its own
		case c == '\\':
			if i+1 < len(r) {
				i++
				word.WriteRune(r[i])
				inWord = true
			}
		case c == '\'':
			inWord = true
			for i++; i < len(r) && r[i] != '\''; i++ {
				word.WriteRune(r[i])
			}
		case c == '"':
			inWord = true
			for i++; i < len(r) && r[i] != '"'; i++ {
				if r[i] == '\\' && i+1 < len(r) {
					i++
				}
				word.WriteRune(r[i])
			}
		case c == '`':
			inWord = true
			word.WriteRune(c)
			for i++; i < len(r) && r[i] != '`'; i++ {
				word.WriteRune(r[i])
			}
			word.WriteRune('`')
		case c == '$' && i+1 < len(r) && r[i+1] == '(':
			inWord = true
			depth := 0
			for ; i < len(r); i++ {
				word.WriteRune(r[i])
				if r[i] == '(' {
					depth++
				} else if r[i] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
		case c == '<' && i+1 < len(r) && r[i+1] == '<' && (i+2 >= len(r) || r[i+2] != '<'):
			// here-document: remember the delimiter, drop the operator
			j := i + 2
			if j < len(r) && r[j] == '-' {
				j++
			}
			for j < len(r) && (r[j] == ' ' || r[j] == '\t') {
				j++
			}
			k := j
			for k < len(r) && !strings.ContainsRune(" \t\n;|&)", r[k]) {
				k++
			}
			heredocs = append(heredocs, strings.Trim(string(r[j:k]), `'"\`))
			i = k - 1
			flushWord()
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	endSeg()
	return segs
}

var (
	assignRE   = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)
	redirectRE = lazyre.New(`^(\d*|&)(>>?|<)(&\d*|.*)$`)
	subcmdRE   = lazyre.New(`^[a-z][a-z0-9:_.-]*$`)
)

// controlWords start or end shell constructs; the command inside follows.
var skipWords = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true, "!": true, "time": true, "nohup": true,
	"command": true, "exec": true, "builtin": true, "setsid": true, "coproc": true}

// dropSegment: these words alone don't run anything.
var dropWords = map[string]bool{"fi": true, "done": true, "esac": true, "for": true, "case": true, "select": true, "function": true, "in": true, "}": true, "{": true, "[[": true, "[": true, "break": true, "continue": true}

// subcommandTools take a subcommand as their first word that is not a flag.
// valueFlags are the flags that take a value, which must be skipped.
var subcommandTools = map[string]map[string]bool{
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

var shells = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ash": true, "ksh": true}

const maxHeads = 12

// CommandHeads returns the heads of a shell command line, in order, without
// arguments or duplicates within the call.
func CommandHeads(cmd string) []string {
	var heads []string
	seen := map[string]bool{}
	add := func(h string) {
		if h != "" && !seen[h] && len(heads) < maxHeads {
			seen[h] = true
			heads = append(heads, h)
		}
	}
	walkCommands(cmd, 0, func(words []string) { add(headOf(words)) })
	return heads
}

// walkCommands calls fn with the words of each command a line runs, going
// into `bash -c SCRIPT`.
func walkCommands(cmd string, depth int, fn func(words []string)) {
	for _, seg := range shellWords(cmd) {
		words := stripWords(seg)
		if len(words) == 0 {
			continue
		}
		if depth < 3 && shells[path.Base(words[0])] {
			if inner, ok := shellDashC(words[1:]); ok {
				walkCommands(inner, depth+1, fn)
				continue
			}
		}
		fn(words)
	}
}

// shellDashC finds the script of `bash -c SCRIPT` (also -lc, -ic…).
func shellDashC(args []string) (string, bool) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args) {
			return args[i+1], true
		}
		if !strings.HasPrefix(a, "-") {
			return "", false
		}
	}
	return "", false
}

// stripWords drops redirections, assignments, control words and wrappers
// from the front of a segment, leaving the program and its arguments.
func stripWords(seg []string) []string {
	var words []string
	for i := 0; i < len(seg); i++ {
		w := seg[i]
		if redirectRE.MatchString(w) && w != "" {
			// "2>file" is one word; a bare ">" takes the next word as its target
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
		case assignRE.MatchString(w), skipWords[w]:
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
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || assignRE.MatchString(words[0])) {
				flag := words[0]
				words = words[1:]
				if wrapperValueFlags[base][flag] && len(words) > 0 {
					words = words[1:]
				}
			}
			for ; n > 0 && len(words) > 0; n-- { // timeout 30 cmd
				words = words[1:]
			}
		}
	}
	return nil
}

// headOf names a command: its program, plus the subcommand for tools that have them.
func headOf(words []string) string {
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
	flags, ok := subcommandTools[strings.TrimSuffix(name, ".exe")]
	if !ok {
		if strings.HasPrefix(name, "python") {
			// python -m pytest: the module is what ran
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
			if flags[a] { // a flag taking a value: git -C dir status
				i++
			}
		case subcmdRE.MatchString(a):
			sub := a
			if name == "docker" && sub == "compose" || name == "kubectl" && sub == "rollout" {
				// one level more says what ran: docker compose up
				for _, b := range rest[i+1:] {
					if !strings.HasPrefix(b, "-") && subcmdRE.MatchString(b) {
						sub += " " + b
					}
					break
				}
			}
			return name + " " + sub
		default:
			return name // a path or value, not a subcommand
		}
	}
	return name
}

// secretRE finds credentials in text that may leave the machine (an
// excerpt of a refused command, say).
// It takes NAME=value and name: value where the name holds a secret word
// (AWS_SECRET_ACCESS_KEY=…), a flag with its value after a space
// (--password hunter2, --token abc) and Bearer values. After a name that
// holds a secret, an auth scheme word (Basic, Token, Negotiate, NTLM, Bearer)
// and the credential after it go together; Digest's parameters are
// digestAuthRE's.
var secretRE = lazyre.New(`(?i)([\w-]*(?:token|secret|passw(?:or)?d|api[_-]?key|apikey|access[_-]?key|private[_-]?key|authorization|credential)[\w-]*["']?\s*[=:]\s*|--(?:token|secret|password|passwd|api[_-]?key|auth|access[_-]?key)\s+|bearer\s+)(["']?)(?:(?:bearer|basic|token|negotiate|ntlm)\s+)?[^\s"']{4,}`)
var tokenLikeRE = lazyre.New(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{22,}|glpat-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|AIza[A-Za-z0-9_-]{35,}|npm_[A-Za-z0-9]{36,}|pypi-[A-Za-z0-9_-]{16,}|xapp-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,})`)

// urlPasswordRE finds the password in a URL's user part
// (postgres://admin:pw@db/app).
// digestAuthRE takes a Digest header's whole value (several quoted
// parameters, one of them the response) to the end of the line or a closing
// single quote.
var digestAuthRE = lazyre.New(`(?i)(\bauthorization["']?\s*[=:]\s*["']?digest\s+)[^\r\n']+`)

// schemeAuthRE takes a scheme word and the credential after it in an
// Authorization value (ApiKey, AWS4-HMAC-SHA256, one not invented yet), so a
// scheme secretRE does not list leaves no credential behind.
var schemeAuthRE = lazyre.New(`(?i)(\bauthorization["']?\s*[=:]\s*["']?)[\w-]+\s+[^\s"']{4,}`)

// pemKeyRE blanks a PEM private key's body between its BEGIN and END lines
// (RSA, EC, OPENSSH, ENCRYPTED or plain), and to the end of the text when the
// END line is missing (a key cut off by a limit). The body may hold real or
// escaped newlines.
var pemKeyRE = lazyre.New(`(-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----)(?s:.*?)(-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)
var urlPasswordRE = lazyre.New(`(://[^\s/:@]+:)[^\s/@]+@`)

// urlUserRE finds a URL user part that is not followed by a password, or
// only by one redact has blanked (:***@) or left empty (:@), where the user
// may be the token (https://TOKEN@host, https://TOKEN:x-oauth-basic@host).
// Of a user part of 8 characters or more, redact blanks one that is 20
// characters or more, or holds a digit and at least one other character that
// is not a digit; a shorter part with no digit, or all digits, stays (git@,
// deploy@, a number).
var urlUserRE = lazyre.New(`://[^\s/:@?#]{8,}(?::\*\*\*|:)?@`)

func blankURLUser(m string) string {
	u := m[len("://") : len(m)-1]
	rest := ""
	if i := strings.IndexByte(u, ':'); i >= 0 {
		u, rest = u[:i], u[i:]
	}
	if len(u) < 20 && (!strings.ContainsAny(u, "0123456789") || strings.Trim(u, "0123456789") == "") {
		return m
	}
	return "://***" + rest + "@"
}

// redactStep is one pattern of redact and what replaces a match of it; sub
// is the match and its groups, as FindStringSubmatch gives them.
type redactStep struct {
	re   *lazyre.Regexp
	repl func(sub []string) string
}

// redactSteps are redact's patterns after pemKeyRE, in the order they run.
var redactSteps = []redactStep{
	{urlPasswordRE, func(sub []string) string { return sub[1] + "***@" }},
	{urlUserRE, func(sub []string) string { return blankURLUser(sub[0]) }},
	{digestAuthRE, func(sub []string) string { return sub[1] + "***" }},
	{schemeAuthRE, func(sub []string) string { return sub[1] + "***" }},
	{secretRE, func(sub []string) string { return sub[1] + sub[2] + "***" }},
	{tokenLikeRE, func([]string) string { return "***" }},
}

// replaceStep replaces every match of st in s; fix, when set, may change a
// match's replacement given the matched text.
func replaceStep(s string, st redactStep, fix func(m, r string) string) string {
	idx := st.re.FindAllStringSubmatchIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, ix := range idx {
		sub := make([]string, len(ix)/2)
		for g := range sub {
			if ix[2*g] >= 0 {
				sub[g] = s[ix[2*g]:ix[2*g+1]]
			}
		}
		r := st.repl(sub)
		if fix != nil {
			r = fix(sub[0], r)
		}
		b.WriteString(s[last:ix[0]])
		b.WriteString(r)
		last = ix[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// redact hides credentials in text that may leave the machine. Redact
// before cutting, so a cut never leaves part of a token.
func redact(s string) string {
	s = pemKeyRE.ReplaceAllString(s, "${1}***${2}")
	for _, st := range redactSteps {
		s = replaceStep(s, st, nil)
	}
	return s
}
