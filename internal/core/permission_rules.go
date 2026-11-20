package core

// Permission rules a person can add when approving a refused tool call
// (WP-233, WP-ADR114). Approving a permission request used to tell the
// agent only that the person allowed it: Claude Code's auto-mode classifier
// judged the retry afresh and often refused it again. Now the person can
// pick a narrow allow rule to add to the repo's .claude/settings.json, so
// the same command (and ones like it) skip the classifier in later sessions.
// Nothing else is remembered: an approval without a rule changes nothing.
//
// The rules offered are worked out here without a model: the exact command,
// and at most one prefix rule that stops before the first argument that
// looks like data (a path, a number, a quoted message). Programs that run
// arbitrary code, reach other machines or delete things only get exact
// rules: a prefix rule for them grants more than the call the person read.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// PermissionRule is an allow rule offered for a refused tool call, with
// what it covers in plain words.
type PermissionRule struct {
	Rule   string `json:"rule"`
	Covers string `json:"covers"`
}

// maxRuleLen bounds a rule: a settings file is no place for a script.
const maxRuleLen = 300

// riskyPrograms only get exact rules: with a wildcard they run any code,
// reach other machines, delete or change permissions, or run whatever a
// build file says. Claude Code drops broad interpreter rules in auto mode
// anyway; these go further.
var riskyPrograms = map[string]bool{}

func init() {
	for _, p := range strings.Fields(`sh bash zsh dash fish ksh csh tcsh python python2 python3 node deno bun ruby perl php lua
		npx pnpx bunx uvx env xargs sudo su doas eval exec command builtin source nohup timeout nice ionice time watch setsid
		ssh scp sftp rsync curl wget nc ncat netcat socat telnet ftp docker podman nerdctl kubectl helm terraform tofu
		ansible ansible-playbook pulumi rm rmdir unlink dd mkfs shred truncate chmod chown chgrp chattr find mv cp ln tee install
		make just task npm pnpm yarn pip pip3 pipx uv poetry cargo go gradle mvn bundle gem composer
		gh glab tea crontab systemctl service launchctl kill pkill killall reboot shutdown iptables nft ufw
		git awk gawk sed perl osascript open xdg-open
		vi vim nvim view ex rvim emacs ed expect script man less more tar zip unzip 7z gdb lldb strace ltrace valgrind
		busybox toybox tmux screen parallel flock chroot unshare nsenter firejail systemd-run at batch pkexec runuser
		setpriv stdbuf unbuffer taskset chrt prlimit entr ssh-agent dbus-run-session xvfb-run sqlite3 psql mysql
		gcc cc clang tclsh wish R Rscript julia java jshell dotnet swift irb elixir iex erl scala groovy pwsh
		cmake ninja meson bazel ant sbt mix rake pytest tox nox jest vitest mocha rspec phpunit
		g++ c++ clang++ tsc babel esbuild webpack vite rollup eslint prettier black ruff gofmt goimports rustfmt
		clang-format shfmt
		apt apt-get aptitude dpkg dnf yum rpm zypper pacman apk brew port snap flatpak nix conda mamba micromamba
		aws gcloud gsutil az doctl flyctl heroku vercel netlify wrangler
		sort uniq split csplit patch sponge yq openssl gzip gunzip bzip2 bunzip2 xz unxz zstd lz4 cpio
		rg ripgrep fd fdfind ack ag bat fzf`) {
		riskyPrograms[strings.ToLower(p)] = true
	}
}

var trailingDigitsRE = lazyre.New(`[0-9]+$`)

// isRiskyProgram: prog is in riskyPrograms under another name: a version
// (python3.11, ruby3, gcc-12), a variant after a dot (mkfs.ext4), a
// toolchain prefix (x86_64-linux-gnu-gcc) or a plugin (docker-compose,
// git-lfs). A name that only looks like one of them gets exact rules too,
// the safe side.
func isRiskyProgram(prog string) bool {
	name, _, _ := strings.Cut(strings.ToLower(prog), ".")
	if riskyPrograms[name] {
		return true
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' }) {
		if riskyPrograms[part] || riskyPrograms[trailingDigitsRE.ReplaceAllString(part, "")] {
			return true
		}
	}
	return false
}

// gitReadOnly are git subcommands a prefix rule may cover: they read the
// repository, and no option makes them run a command or write a file.
// Others that only read take one that does: fetch and ls-remote run
// --upload-pack and ext:: commands, grep -O runs a pager, and log, show,
// diff, blame, shortlog and rev-list write any file with --output (audit
// 940.1). Claude Code allows the read-only forms of those itself.
var gitReadOnly = map[string]bool{"status": true, "ls-files": true, "rev-parse": true, "describe": true,
	"merge-base": true, "cat-file": true, "check-ignore": true, "name-rev": true}

var (
	ruleWordRE = lazyre.New(`^[a-z][a-z0-9_-]*$`)         // a subcommand or plain word
	ruleFlagRE = lazyre.New(`^--?[a-z][a-z0-9-]*$`)       // a flag without a value
	ruleProgRE = lazyre.New(`^[A-Za-z][A-Za-z0-9_.+-]*$`) // a program on the PATH, no path
	mcpToolRE  = lazyre.New(`^mcp__[A-Za-z0-9_-]+__[A-Za-z0-9_-]+$`)
	// hostRE: a domain name. Its last label starts with a letter, so an
	// address (169.254.169.254, 127.1) is no host a rule names.
	hostRE = lazyre.New(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]([a-z0-9-]*[a-z0-9])?$`)
)

// permissionRules are the allow rules offered for a refused call, narrowest
// first; none when no rule can cover it safely.
func permissionRules(tool string, input json.RawMessage) []PermissionRule {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		in = nil
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	switch {
	case tool == "Bash":
		return bashRules(str("command"))
	case tool == "WebFetch":
		u, err := url.Parse(str("url"))
		if err != nil || u.Hostname() == "" {
			return nil
		}
		host := strings.ToLower(u.Hostname())
		if !hostRE.MatchString(host) {
			return nil
		}
		return []PermissionRule{{Rule: "WebFetch(domain:" + host + ")", Covers: "fetching any page on " + host}}
	case mcpToolRE.MatchString(tool):
		return []PermissionRule{{Rule: tool, Covers: "every call of this MCP tool, whatever its arguments"}}
	}
	return nil
}

// shellWord is one word of a command: its text without quotes, and whether
// it was written without any quoting.
type shellWord struct {
	text  string
	plain bool
	meta  bool // an unquoted > or a leading unquoted #, even in a word that has quotes too
}

// splitShell splits a command into its simple commands (at &&, ||, ; and |
// outside quotes) and each into words. ok is false for anything a rule
// can't be read against: unbalanced quotes, substitutions, heredocs,
// backgrounding, more than one line.
func splitShell(cmd string) (parts [][]shellWord, ok bool) {
	if strings.ContainsAny(cmd, "\n\r`") {
		return nil, false
	}
	var cur []shellWord
	var w strings.Builder
	inWord, plain, meta := false, true, false
	endWord := func() {
		if inWord {
			cur = append(cur, shellWord{text: w.String(), plain: plain, meta: meta})
		}
		w.Reset()
		inWord, plain, meta = false, true, false
	}
	endPart := func() bool {
		endWord()
		if len(cur) == 0 {
			return false
		}
		parts = append(parts, cur)
		cur = nil
		return true
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch c {
		case '\'', '"':
			j := strings.IndexByte(cmd[i+1:], c)
			if j < 0 {
				return nil, false
			}
			lit := cmd[i+1 : i+1+j]
			if c == '"' && (strings.Contains(lit, "$") || strings.Contains(lit, "\\")) {
				return nil, false // expansions inside double quotes
			}
			w.WriteString(lit)
			inWord, plain = true, false
			i += j + 1
		case '\\':
			return nil, false
		case '$':
			return nil, false // variables and substitutions: the rule can't see what runs
		case ' ', '\t':
			endWord()
		case ';':
			if !endPart() {
				return nil, false
			}
		case '&', '|':
			double := i+1 < len(cmd) && cmd[i+1] == c
			if c == '&' && !double {
				// "2>&1" is a redirect, read below; a lone & backgrounds.
				// The > must be the shell's, just before: a quoted '>'
				// followed by & is text, then backgrounding.
				if i > 0 && cmd[i-1] == '>' {
					w.WriteByte(c)
					continue
				}
				return nil, false
			}
			if double {
				i++
			}
			if !endPart() {
				return nil, false
			}
		case '(', ')', '{', '}', '<':
			return nil, false // subshells, groups, input redirects, process substitution
		default:
			if c == '>' || (c == '#' && !inWord) {
				meta = true
			}
			w.WriteByte(c)
			inWord = true
		}
	}
	if !endPart() {
		return nil, false
	}
	return parts, true
}

// bashRules: the exact command and at most one prefix rule.
func bashRules(cmd string) []PermissionRule {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || len("Bash()")+len(cmd) > maxRuleLen {
		return nil
	}
	parts, ok := splitShell(cmd)
	if !ok {
		return nil
	}
	// A cd into a folder first is how agents run a command elsewhere;
	// Claude Code judges it on its own. Any other chain needs one rule a
	// command, which a single approval shouldn't grow into.
	var words []shellWord
	for _, p := range parts {
		if p[0].text == "cd" && p[0].plain {
			continue
		}
		if words != nil {
			return nil
		}
		words = p
	}
	if len(words) == 0 {
		return nil
	}
	words = dropErrRedirect(words)
	if !ruleWordsOK(words) {
		return nil
	}
	prog := words[0].text
	if !words[0].plain || !ruleProgRE.MatchString(prog) {
		return nil // ./script, /usr/bin/x, VAR=value first, a quoted program
	}
	var out []PermissionRule
	exact := joinWords(words)
	if len(parts) == 1 {
		exact = cmd // as the agent wrote it, quotes and all, so the rule matches the retry
	}
	if exact != "" {
		out = append(out, PermissionRule{Rule: "Bash(" + exact + ")", Covers: "only this exact command"})
	}
	// The prefix: the program and the plain words and flags after it, up to
	// the first that looks like data.
	n := 1
	for n < len(words) && words[n].plain && (ruleWordRE.MatchString(words[n].text) || ruleFlagRE.MatchString(words[n].text)) {
		n++
	}
	switch {
	case n == len(words), n < 2:
		return out // nothing to widen, or only "prog *"
	case isRiskyProgram(prog) && (prog != "git" || !gitReadOnly[words[1].text]):
		return out
	}
	prefix := joinWords(words[:n])
	out = append(out, PermissionRule{Rule: "Bash(" + prefix + " *)",
		Covers: fmt.Sprintf("any “%s …” command, whatever options and files follow it (an option or a > redirect that writes a file too); not chained with other commands", prefix)})
	return out
}

// dropErrRedirect: a trailing 2>&1 is kept out of a rule's words: it
// changes only where errors are printed.
func dropErrRedirect(words []shellWord) []shellWord {
	if n := len(words); n > 1 && words[n-1].plain && words[n-1].text == "2>&1" {
		return words[:n-1]
	}
	return words
}

// ruleWordsOK: words a rule can be made of. Any other redirect writes a
// file the rule can't see, # starts a comment, and *, ( and ) can't be
// written inside a rule. Suggested and typed rules both pass it (WP-ADR114).
func ruleWordsOK(words []shellWord) bool {
	for _, w := range words {
		if w.meta || (w.plain && (strings.Contains(w.text, ">") || strings.HasPrefix(w.text, "#"))) {
			return false
		}
		if strings.ContainsAny(w.text, "*()") {
			return false
		}
	}
	return true
}

// joinWords writes words back as a command, quoting those that need it.
func joinWords(ws []shellWord) string {
	var b strings.Builder
	for i, w := range ws {
		if i > 0 {
			b.WriteByte(' ')
		}
		switch {
		case w.plain:
			b.WriteString(w.text)
		case !strings.Contains(w.text, "'"):
			b.WriteString("'" + w.text + "'")
		case !strings.ContainsAny(w.text, `"$\`+"`"):
			b.WriteString(`"` + w.text + `"`)
		default:
			return ""
		}
	}
	return b.String()
}

// checkPermissionRule checks a rule a person typed (or picked) for a refused
// call of tool: one narrow allow rule of the same tool.
func checkPermissionRule(tool, rule string) error {
	rule = strings.TrimSpace(rule)
	switch {
	case rule == "":
		return invalid("the rule is empty")
	case len(rule) > maxRuleLen:
		return invalid("a rule can be at most %d characters", maxRuleLen)
	case strings.ContainsAny(rule, "\n\r\x00"):
		return invalid("a rule is one line")
	}
	if mcpToolRE.MatchString(tool) {
		if rule != tool {
			return invalid("the rule for an MCP tool is its name, %s", tool)
		}
		return nil
	}
	name, body, ok := strings.Cut(rule, "(")
	if !ok || !strings.HasSuffix(body, ")") || name != tool {
		return invalid("the rule must be %s(…), for the tool that was refused", tool)
	}
	body = strings.TrimSuffix(body, ")")
	switch tool {
	case "WebFetch":
		host, ok := strings.CutPrefix(body, "domain:")
		if !ok || !hostRE.MatchString(host) {
			return invalid("a WebFetch rule is WebFetch(domain:HOST), one host with no wildcard")
		}
		return nil
	case "Bash":
	default:
		return invalid("Waypoint adds rules for Bash, WebFetch and MCP tools only; add others in the settings yourself")
	}
	// A wildcard follows a space or a colon: "wp ls*" would match "wp lsx" too.
	cmd, wild := strings.CutSuffix(body, ":*")
	if !wild {
		cmd, wild = strings.CutSuffix(body, " *")
	}
	cmd = strings.TrimSpace(cmd)
	if strings.Contains(cmd, "*") {
		return invalid("a wildcard may only end the rule")
	}
	parts, ok := splitShell(cmd)
	if !ok || len(parts) != 1 {
		return invalid("a rule covers one simple command: no $, quotes left open, chains, pipes or subshells")
	}
	words := parts[0]
	if !wild {
		words = dropErrRedirect(words)
	}
	if !ruleWordsOK(words) {
		return invalid("a rule can't hold a redirect other than a final 2>&1, a # comment, or *, ( or ) inside it")
	}
	if !words[0].plain || !ruleProgRE.MatchString(words[0].text) {
		return invalid("a rule names a program on the PATH, not a path")
	}
	if wild {
		switch {
		case len(words) < 2:
			return invalid("%s * would allow every %s command; name the subcommand too", words[0].text, words[0].text)
		case isRiskyProgram(words[0].text) && (words[0].text != "git" || !gitReadOnly[words[1].text]):
			return invalid("%s can run any code, reach other machines or delete things, so only an exact rule (no *) is added for it", words[0].text)
		}
	}
	return nil
}

var errRulePresent = errors.New("the rule is already in the file")

// addAllowRule adds rule to permissions.allow of a Claude Code settings
// file, written into the text so the rest of the file (its order, its
// layout, its comments-free JSON) stays as it was. content "" is a new file.
func addAllowRule(content, rule string) (string, error) {
	q := marshalNoEscape(rule)
	if strings.TrimSpace(content) == "" {
		return "{\n  \"permissions\": {\n    \"allow\": [\n      " + q + "\n    ]\n  }\n}\n", nil
	}
	sp, err := scanSettings(content)
	if err != nil {
		return "", err
	}
	if slices.Contains(sp.allow, rule) {
		return content, errRulePresent
	}
	unit := indentUnit(content)
	var out string
	switch {
	case sp.allowOpen >= 0 && len(sp.allow) > 0:
		at := sp.lastEnd
		if strings.Contains(content[sp.allowOpen:sp.allowClose], "\n") {
			out = content[:at] + ",\n" + lineIndent(content, at) + q + content[at:]
		} else {
			out = content[:at] + ", " + q + content[at:]
		}
	case sp.allowOpen >= 0: // an empty array
		ind := lineIndent(content, sp.allowOpen)
		out = content[:sp.allowOpen] + "\n" + ind + unit + q + "\n" + ind + content[sp.allowClose-1:]
	case sp.permOpen >= 0:
		ind := lineIndent(content, sp.permOpen) + unit
		entry := "\n" + ind + `"allow": [` + "\n" + ind + unit + q + "\n" + ind + "]"
		if sp.permMembers > 0 {
			entry += ","
		} else {
			entry += "\n" + lineIndent(content, sp.permOpen)
		}
		out = content[:sp.permOpen] + entry + content[sp.permOpen:]
	default:
		ind := lineIndent(content, sp.rootOpen) + unit
		entry := "\n" + ind + `"permissions": {` + "\n" + ind + unit + `"allow": [` + "\n" + ind + unit + unit + q + "\n" + ind + unit + "]\n" + ind + "}"
		if sp.rootMembers > 0 {
			entry += ","
		} else {
			entry += "\n" + lineIndent(content, sp.rootOpen)
		}
		out = content[:sp.rootOpen] + entry + content[sp.rootOpen:]
	}
	// What was written must read back as the same settings plus the rule.
	check, err := scanSettings(out)
	if err != nil || !slices.Contains(check.allow, rule) || len(check.allow) != len(sp.allow)+1 {
		return "", fmt.Errorf("adding the rule to the settings didn't give valid JSON with it in permissions.allow")
	}
	return out, nil
}

// settingsSpots are byte offsets in a settings file: just after the opening
// brace or bracket of the root, permissions and permissions.allow (-1 when
// absent), just after allow's closing bracket and its last element.
type settingsSpots struct {
	rootOpen, permOpen, allowOpen, allowClose, lastEnd int
	rootMembers, permMembers                           int
	allow                                              []string
}

func scanSettings(content string) (*settingsSpots, error) {
	sp := &settingsSpots{permOpen: -1, allowOpen: -1, allowClose: -1}
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber()
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return nil, invalid(".claude/settings.json isn't a JSON object")
	}
	sp.rootOpen = int(dec.InputOffset())
	bad := func() (*settingsSpots, error) { return nil, invalid(".claude/settings.json isn't valid JSON") }
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return bad()
		}
		sp.rootMembers++
		if k != "permissions" {
			if skipValue(dec) != nil {
				return bad()
			}
			continue
		}
		tok, err := dec.Token()
		if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
			return nil, invalid("permissions in .claude/settings.json isn't an object")
		}
		sp.permOpen = int(dec.InputOffset())
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return bad()
			}
			sp.permMembers++
			if k != "allow" {
				if skipValue(dec) != nil {
					return bad()
				}
				continue
			}
			tok, err := dec.Token()
			if d, ok := tok.(json.Delim); err != nil || !ok || d != '[' {
				return nil, invalid("permissions.allow in .claude/settings.json isn't a list")
			}
			sp.allowOpen = int(dec.InputOffset())
			for dec.More() {
				var v any
				if err := dec.Decode(&v); err != nil {
					return bad()
				}
				s, _ := v.(string)
				sp.allow = append(sp.allow, s)
				sp.lastEnd = int(dec.InputOffset())
			}
			if _, err := dec.Token(); err != nil {
				return bad()
			}
			sp.allowClose = int(dec.InputOffset())
		}
		if _, err := dec.Token(); err != nil {
			return bad()
		}
	}
	if _, err := dec.Token(); err != nil {
		return bad()
	}
	if _, err := dec.Token(); err == nil {
		return bad() // more after the object
	}
	return sp, nil
}

// skipValue reads one whole value.
func skipValue(dec *json.Decoder) error {
	var v json.RawMessage
	return dec.Decode(&v)
}

// lineIndent is the leading whitespace of the line holding offset at.
func lineIndent(s string, at int) string {
	start := strings.LastIndexByte(s[:at], '\n') + 1
	end := start
	for end < len(s) && (s[end] == ' ' || s[end] == '\t') {
		end++
	}
	return s[start:end]
}

// indentUnit is the file's indentation step: the first indented line's.
func indentUnit(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if ind := line[:len(line)-len(strings.TrimLeft(line, " \t"))]; ind != "" && len(ind) < len(line) {
			return ind
		}
	}
	return "  "
}

func marshalNoEscape(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSpace(b.String())
}
