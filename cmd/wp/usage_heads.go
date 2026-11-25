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
var tokenLikeRE = lazyre.New(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|glpat-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,})`)

// urlPasswordRE finds the password in a URL's user part
// (postgres://admin:pw@db/app).
// digestAuthRE takes a Digest header's whole value (several quoted
// parameters, one of them the response) to the end of the line or a closing
// single quote.
var digestAuthRE = lazyre.New(`(?i)(\bauthorization["']?\s*[=:]\s*["']?digest\s+)[^\n']+`)

// schemeAuthRE takes a scheme word and the credential after it in an
// Authorization value (ApiKey, AWS4-HMAC-SHA256, one not invented yet), so a
// scheme secretRE does not list leaves no credential behind.
var schemeAuthRE = lazyre.New(`(?i)(\bauthorization["']?\s*[=:]\s*["']?)[\w-]+\s+[^\s"']{4,}`)

// pemKeyRE blanks a PEM private key's body between its BEGIN and END lines
// (RSA, EC, OPENSSH, ENCRYPTED or plain), and to the end of the text when the
// END line is missing (a key cut off by a limit). The body may hold real or
// escaped newlines.
var pemKeyRE = lazyre.New(`(-----BEGIN [A-Z ]*PRIVATE KEY-----)(?s:.*?)(-----END [A-Z ]*PRIVATE KEY-----|\z)`)
var urlPasswordRE = lazyre.New(`(://[^\s/:@]+:)[^\s/@]+@`)

// redact hides credentials in text that may leave the machine. Redact
// before cutting, so a cut never leaves part of a token.
func redact(s string) string {
	s = pemKeyRE.ReplaceAllString(s, "${1}***${2}")
	s = urlPasswordRE.ReplaceAllString(s, "${1}***@")
	s = digestAuthRE.ReplaceAllString(s, "${1}***")
	s = schemeAuthRE.ReplaceAllString(s, "${1}***")
	s = secretRE.ReplaceAllString(s, "${1}${2}***")
	return tokenLikeRE.ReplaceAllString(s, "***")
}
