package main

import (
	"encoding/json"
	"path"
	"slices"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// How agents use wp (WP-618): compact counters per wp command, added up by
// the hour, so "how are agents doing with wp?" no longer takes mining the
// transcripts. Nothing here carries a prompt, an output or an argument:
// only command names from wp's own command list, words that have the shape
// of a flag name, and a fixed set of error classes and labels.

// Row kinds. The server keeps to this list.
const (
	usageCLIFile           = "file"            // a transcript began (a session or a subagent)
	usageCLIBash           = "bash"            // a Bash call
	usageCLIWP             = "wp"              // head: a wp command run
	usageCLIHelp           = "help"            // head: its help was looked up
	usageCLIFlag           = "flag"            // head; detail: a flag name it was given
	usageCLIError          = "error"           // head; detail: the error's class
	usageCLIUnknownFlag    = "unknown_flag"    // head; detail: the flag it doesn't have
	usageCLIUnknownCommand = "unknown_command" // head; detail: the command that doesn't exist
	usageCLIFollow         = "follow"          // head; detail: what ran within the next calls
	usageCLISignal         = "signal"          // detail: one of the signals below
)

// Error classes.
const (
	cliErrUnknownFlag    = "unknown_flag"
	cliErrUnknownCommand = "unknown_command"
	cliErrAmbiguousCode  = "ambiguous_code"
	cliErrUsage          = "usage"
)

// Signals: habits of a Bash call that wp is meant to replace.
const (
	cliSignalCurlForge = "curl_forge" // curl against a forge's API
	cliSignalSleep     = "sleep"
	cliSignalSleepCI   = "sleep_ci" // a sleep in a call running wp pr or wp ci, or next to one
	cliSignalGhTea     = "gh_tea"
)

const (
	// usageCLIVersion: raise it to read every transcript again for these rows.
	usageCLIVersion = 1
	// cliFollowCalls: how many Bash calls after a wp command count as what
	// followed it. Other tools don't use it up.
	cliFollowCalls = 5
	cliKeepPending = 12
	cliMaxInCall   = 8
	cliMaxFlags    = 12
	cliResultRunes = 4000
	// cliGenericLines: "wp: …" is what every failure starts with and a task
	// body can quote one, so these count near the start of the output only.
	cliGenericLines = 5
	cliNewBranch    = "git new branch/worktree"
)

// usageCLIRow is one counter of one hour of one transcript's session.
type usageCLIRow struct {
	Hour    int64  `json:"h"`
	Session string `json:"s"`
	Repo    string `json:"repo,omitempty"`
	Model   string `json:"model,omitempty"`
	Agent   string `json:"agent"` // main, sub
	Kind    string `json:"kind"`
	Head    string `json:"head,omitempty"`
	Detail  string `json:"detail,omitempty"`
	N       int64  `json:"n"`
}

type usageCLIKey struct {
	hour               int64
	model              string
	kind, head, detail string
}

// usageCLIPending is a wp command waiting to see what comes after it.
type usageCLIPending struct {
	Head  string   `json:"h"`
	Hour  int64    `json:"t"`
	Model string   `json:"m,omitempty"`
	Left  int      `json:"l"`
	Seen  []string `json:"s,omitempty"`
}

// cliInvocation is one wp command on a line.
type cliInvocation struct {
	Head  string
	Help  bool
	Flags []string
}

// cliShellCall is what one Bash call ran.
type cliShellCall struct {
	WP      []cliInvocation
	Follow  []string // the labels it counts as, when it follows a wp command
	Signals []string
	Sleeps  bool
	CI      bool // ran wp pr or wp ci
}

// wpValueFlags are the flags, valid anywhere, whose value is the next word.
var wpValueFlags = subcommandTools["wp"]

var (
	cliFlagRE    = lazyre.New(`^--?[A-Za-z][A-Za-z0-9-]{0,29}$`)
	cliCommandRE = lazyre.New(`^[a-z][a-z0-9-]{0,29}$`)
)

// cliFlagName is a word's flag name, cut at its value, spelled the way help
// spells it. "" when the word doesn't have the shape of one, which keeps out
// most values that happen to start with a dash.
func cliFlagName(word string) string {
	if i := strings.IndexByte(word, '='); i > 0 {
		word = word[:i]
	}
	if !cliFlagRE.MatchString(word) {
		return ""
	}
	name := strings.TrimLeft(word, "-")
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

// parseWPInvocation reads the words of a wp command against wp's own command
// list, so a head is always a real command's path (or a single word in the
// place of a command: the command that doesn't exist).
func parseWPInvocation(words []string) cliInvocation {
	inv := cliInvocation{Head: "wp"}
	var cur *command
	descending := true
	seen := map[string]bool{}
	args := words[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if len(a) > 1 && a[0] == '-' {
			if a == "-h" || a == "--help" || a == "-help" {
				inv.Help = true
				continue
			}
			if name := cliFlagName(a); name != "" && !seen[name] && len(inv.Flags) < cliMaxFlags {
				seen[name] = true
				inv.Flags = append(inv.Flags, name)
			}
			if wpValueFlags[a] {
				i++
			}
			continue
		}
		if !descending {
			continue
		}
		if a == "help" {
			inv.Help = true
			continue
		}
		var next *command
		if cur == nil {
			next = findCommand(a)
		} else {
			next = cur.sub(a)
		}
		switch {
		case next != nil:
			cur = next
			inv.Head = "wp " + cur.path()
		case cur == nil && cliCommandRE.MatchString(a):
			inv.Head = "wp " + a
			descending = false
		default:
			descending = false // an argument of the command found so far
		}
	}
	return inv
}

// followDropped: heads that say nothing about what the agent did next.
// Reading around (grep, cat, ls) follows everything, in most runs; it drowned
// out the steps that differ from one command to the next.
var followDropped = map[string]bool{"cd": true, "echo": true, "true": true, "export": true, "set": true, "source": true, "test": true, "(variable)": true,
	"head": true, "tail": true, "wc": true, "sort": true, "uniq": true, "cut": true, "tee": true, "tr": true, "xargs": true, "jq": true, "awk": true, "sed": true,
	"grep": true, "rg": true, "cat": true, "ls": true, "find": true, "python": true, "pwd": true, "date": true, "mkdir": true, "rm": true, "cp": true, "mv": true,
	"printf": true, "nl": true, "which": true, "basename": true, "dirname": true, "stat": true, "du": true}

// isNewBranch: one label for the several ways of starting a branch.
func isNewBranch(head string, words []string) bool {
	has := func(want ...string) bool {
		return slices.ContainsFunc(words, func(w string) bool { return slices.Contains(want, w) })
	}
	switch head {
	case "git worktree":
		return has("add")
	case "git switch":
		return has("-c", "-C", "--create", "--force-create")
	case "git checkout":
		return has("-b", "-B")
	case "git branch":
		// `git branch NAME [START]`: a bare word, no listing or deleting flag
		i := slices.Index(words, "branch")
		return i >= 0 && i+1 < len(words) && !strings.HasPrefix(words[i+1], "-")
	}
	return false
}

// forgeAPI: the words of a curl that show it talks to a forge's API.
var forgeAPI = []string{"/api/v1/repos", "/api/v4/projects", "api.github.com"}

func hitsForgeAPI(words []string) bool {
	return slices.ContainsFunc(words, func(w string) bool {
		return slices.ContainsFunc(forgeAPI, func(api string) bool { return strings.Contains(w, api) })
	})
}

// readCLIShell reads one Bash call's command line.
func readCLIShell(cmd string) *cliShellCall {
	call := &cliShellCall{}
	label := func(l string) {
		if l != "" && !slices.Contains(call.Follow, l) && len(call.Follow) < maxHeads {
			call.Follow = append(call.Follow, l)
		}
	}
	var ghTea, curlForge bool
	walkCommands(cmd, 0, func(words []string) {
		prog := path.Base(words[0])
		if prog == "wp" {
			if len(call.WP) == cliMaxInCall {
				return
			}
			inv := parseWPInvocation(words)
			call.WP = append(call.WP, inv)
			call.CI = call.CI || strings.HasPrefix(inv.Head+" ", "wp pr ") || strings.HasPrefix(inv.Head+" ", "wp ci ")
			if inv.Help {
				label(inv.Head + " -h")
			} else {
				label(inv.Head)
			}
			return
		}
		call.Sleeps = call.Sleeps || prog == "sleep"
		ghTea = ghTea || prog == "gh" || prog == "tea"
		curlForge = curlForge || prog == "curl" && hitsForgeAPI(words[1:])
		head := headOf(words)
		switch {
		case head == "" || followDropped[head]:
		case isNewBranch(head, words):
			label(cliNewBranch)
		default:
			label(head)
		}
	})
	if curlForge {
		call.Signals = append(call.Signals, cliSignalCurlForge)
	}
	if call.Sleeps {
		call.Signals = append(call.Signals, cliSignalSleep)
	}
	if ghTea {
		call.Signals = append(call.Signals, cliSignalGhTea)
	}
	return call
}

func (p *usageParser) addCLI(hour int64, model, kind, head, detail string, n int64) {
	p.cli[usageCLIKey{hour, model, kind, head, detail}] += n
}

// noteCLI counts one new tool call: what it ran, and what it was for the wp
// commands before it.
func (p *usageParser) noteCLI(call *usageOpenCall) {
	sh := call.shell
	if sh == nil {
		return
	}
	p.addCLI(call.Hour, call.Model, usageCLIBash, "", "", 1)
	for _, s := range sh.Signals {
		p.addCLI(call.Hour, call.Model, usageCLISignal, "", s, 1)
	}
	p.noteCLISleep(call)
	var kept []usageCLIPending
	for _, w := range p.st.CLIPending {
		for _, l := range sh.Follow {
			if !slices.Contains(w.Seen, l) {
				w.Seen = append(w.Seen, l)
				p.addCLI(w.Hour, w.Model, usageCLIFollow, w.Head, l, 1)
			}
		}
		if w.Left--; w.Left > 0 {
			kept = append(kept, w)
		}
	}
	for _, inv := range sh.WP {
		p.addCLI(call.Hour, call.Model, usageCLIWP, inv.Head, "", 1)
		for _, f := range inv.Flags {
			p.addCLI(call.Hour, call.Model, usageCLIFlag, inv.Head, f, 1)
		}
		if inv.Help {
			p.addCLI(call.Hour, call.Model, usageCLIHelp, inv.Head, "", 1)
			continue
		}
		if !slices.Contains(call.WP, inv.Head) {
			call.WP = append(call.WP, inv.Head)
		}
		kept = append(kept, usageCLIPending{Head: inv.Head, Hour: call.Hour, Model: call.Model, Left: cliFollowCalls})
	}
	p.st.CLIPending = kept[max(0, len(kept)-cliKeepPending):]
}

// noteCLISleep counts a sleep beside wp pr or wp ci, in the same Bash call or
// the one before or after it: what polling CI looks like. Page X11 measured
// it this way, and a sleep on its own line is most of it.
func (p *usageParser) noteCLISleep(call *usageOpenCall) {
	sh, st := call.shell, p.st
	switch {
	case sh.Sleeps && (sh.CI || st.CLILastCI):
		p.addCLI(call.Hour, call.Model, usageCLISignal, "", cliSignalSleepCI, 1)
		st.CLISleepHour, st.CLISleepModel = 0, ""
	case sh.Sleeps:
		st.CLISleepHour, st.CLISleepModel = call.Hour, call.Model
	default:
		if sh.CI && st.CLISleepHour != 0 {
			p.addCLI(st.CLISleepHour, st.CLISleepModel, usageCLISignal, "", cliSignalSleepCI, 1)
		}
		st.CLISleepHour, st.CLISleepModel = 0, ""
	}
	st.CLILastCI = sh.CI
}

// noteCLIResult files the usage errors in the output of a call that ran wp.
// The exit code can't say: a pipe hides it, and most agents pipe.
func (p *usageParser) noteCLIResult(call *usageOpenCall, content json.RawMessage) {
	if len(call.WP) == 0 {
		return
	}
	for _, e := range classifyCLIErrors(resultText(content, cliResultRunes), call.WP) {
		p.addCLI(call.Hour, call.Model, usageCLIError, e.head, e.class, 1)
		switch e.class {
		case cliErrUnknownFlag:
			p.addCLI(call.Hour, call.Model, usageCLIUnknownFlag, e.head, e.detail, 1)
		case cliErrUnknownCommand:
			p.addCLI(call.Hour, call.Model, usageCLIUnknownCommand, e.head, e.detail, 1)
		}
	}
}

type cliError struct{ head, class, detail string }

var (
	cliErrLineRE    = lazyre.New(`^wp((?: [a-z][a-z0-9-]*)*): (.+)$`)
	cliUndefinedRE  = lazyre.New(`^flag provided but not defined: (-\S+)$`)
	cliNoCommandRE  = lazyre.New(`^(?:unknown (?:sub)?command|no command|no [a-z-]+ command) "([^"]+)"`)
	cliAmbiguousRE  = lazyre.New(`could be [A-Z0-9]+:|is in several projects; say which`)
	cliGenericUsage = lazyre.New(`^(?:can't tell the test format|which project\?)`)
)

// classifyCLIErrors finds the usage errors in a result. Only a whole line
// that starts the way wp starts its own counts, and it has to name a command
// the call ran and, when it's the only command, come near the top: a task
// body quoting one doesn't count. At most one of each per call. A runtime failure (not found, a refused push) isn't a usage error.
func classifyCLIErrors(text string, heads []string) []cliError {
	var out []cliError
	add := func(e cliError) {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		m := cliErrLineRE.FindStringSubmatch(strings.TrimRight(line, "\r "))
		if m == nil {
			continue
		}
		generic, head, msg := m[1] == "", "wp"+m[1], m[2]
		if !generic && !slices.Contains(heads, head) {
			continue
		}
		if !generic && len(heads) == 1 && i >= cliGenericLines {
			continue // wp show of a task that quotes this very error
		}
		if cm := cliNoCommandRE.FindStringSubmatch(msg); cm != nil {
			full := head + " " + cm[1]
			if !cliCommandRE.MatchString(cm[1]) {
				continue // wp glossary --help: a look at the help, not a name
			}
			if generic {
				if !slices.Contains(heads, full) {
					continue
				}
				head = full
			}
			add(cliError{head: head, class: cliErrUnknownCommand, detail: full})
			continue
		}
		if um := cliUndefinedRE.FindStringSubmatch(msg); um != nil {
			if name := cliFlagName(um[1]); name != "" && !generic {
				add(cliError{head: head, class: cliErrUnknownFlag, detail: name})
			}
			continue
		}
		if generic {
			if i >= cliGenericLines {
				continue
			}
			head = heads[0]
		}
		switch {
		case cliAmbiguousRE.MatchString(msg):
			add(cliError{head: head, class: cliErrAmbiguousCode})
		case generic && cliGenericUsage.MatchString(msg):
			add(cliError{head: head, class: cliErrUsage})
		case !generic && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "usage: wp"):
			add(cliError{head: head, class: cliErrUsage})
		}
	}
	return out
}

// cliRows are the run's counters, in a fixed order.
func (p *usageParser) cliRows(repo string) []usageCLIRow {
	if len(p.cli) == 0 {
		return nil
	}
	agent := "sub"
	if p.f.Main {
		agent = "main"
	}
	rows := make([]usageCLIRow, 0, len(p.cli))
	for k, n := range p.cli {
		rows = append(rows, usageCLIRow{Hour: k.hour, Session: p.f.Session, Repo: repo, Model: k.model, Agent: agent, Kind: k.kind, Head: k.head, Detail: k.detail, N: n})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Hour != b.Hour {
			return a.Hour < b.Hour
		}
		return a.Kind+"\x00"+a.Head+"\x00"+a.Detail+"\x00"+a.Model < b.Kind+"\x00"+b.Head+"\x00"+b.Detail+"\x00"+b.Model
	})
	return rows
}
