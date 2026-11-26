package main

import (
	"path"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
	trscript "git.jmann.me/claude/waypoint/internal/transcript"
)

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
			for len(heredocs) > 0 {
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
				word.WriteRune(c)
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
			endSeg()
		case (c == '(' || c == '{') && !inWord:
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
	assignRE        = trscript.AssignRE
	subcommandTools = trscript.SubcommandTools
	shells          = trscript.Shells
)

const maxHeads = 12

func stripWords(seg []string) []string     { return trscript.StripWords(seg) }
func headOf(words []string) string         { return trscript.HeadOf(words) }
func shellDashC(a []string) (string, bool) { return trscript.ShellDashC(a) }

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
// escaped newlines. A PGP BLOCK is a pattern of its own, so its END line
// doesn't end another key early (audit 1816.1). pemKeyRepl is what replaces
// a match.
var pemKeyRE = lazyre.New(`(-----BEGIN [A-Z ]*PRIVATE KEY-----)(?s:.*?)(-----END [A-Z ]*PRIVATE KEY-----|\z)|(-----BEGIN [A-Z ]*PRIVATE KEY BLOCK-----)(?s:.*?)(-----END [A-Z ]*PRIVATE KEY BLOCK-----|\z)`)
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

const pemKeyRepl = "${1}${3}***${2}${4}"

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
	s = pemKeyRE.ReplaceAllString(s, pemKeyRepl)
	for _, st := range redactSteps {
		s = replaceStep(s, st, nil)
	}
	return s
}
