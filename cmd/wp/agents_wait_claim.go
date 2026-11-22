package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The wait claim (WP-1443): how `wp agents wait` learns which agent called it.

// claimWait records, from the guard hook once it has decided to let the call
// through, that a Bash call is about to run `wp agents wait`, and which agent
// made it: the command cannot tell, since a subagent's shell has its
// session's id but not its own. The command text is kept so the command can
// pick its own claim out (pickWaitClaim). It is picked up by that command
// within waitClaimTTL.
func (c *cli) claimWait(in hookEventInput) {
	if in.ToolName != "Bash" || in.SessionID == "" || !strings.Contains(string(in.ToolInput), "agents") {
		return
	}
	var ti struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &ti) != nil || !runsAgentsWait(ti.Command) {
		return
	}
	_ = c.updateStateWithin(hookCountWait(hookGuardTimeout), func(s *State) {
		s.pruneAgents(time.Now())
		s.WaitClaims = append(s.WaitClaims, waitClaim{Session: in.SessionID, Agent: in.AgentID, Command: strings.TrimSpace(ti.Command), At: time.Now()})
	})
}

// takeWaitClaim removes and returns the claim of session that is this
// command's: see pickWaitClaim. agent is "" for the main thread; found is
// false when there is no claim (the guard hook isn't installed, or didn't
// run).
func (c *cli) takeWaitClaim(session string) (agent string, found bool) {
	ancestors := ancestorCmdlines()
	_ = c.updateStateWithin(2*time.Second, func(s *State) {
		s.pruneAgents(time.Now())
		if i := pickWaitClaim(s.WaitClaims, session, ancestors); i >= 0 {
			agent, found = s.WaitClaims[i].Agent, true
			s.WaitClaims = append(s.WaitClaims[:i:i], s.WaitClaims[i+1:]...)
		}
	})
	return agent, found
}

// pickWaitClaim is the index in claims (oldest first) of the claim of session
// that belongs to the command whose ancestor processes' command lines are
// ancestors, or -1. Claude Code starts a Bash call as `bash -c '<command>'`,
// so the command text is in the command line of one of this command's
// ancestors (read from /proc, Linux only): of the claims whose text appears
// in one, the one with the longest text (a command that merely contains
// another's is not mistaken for it), then the oldest. With nothing readable or
// nothing matching it is the oldest claim of the session, as before. Two
// agents that run the identical command text at once can still swap their
// claims, and so wait for each other's children.
func pickWaitClaim(claims []waitClaim, session string, ancestors []string) int {
	best, oldest := -1, -1
	for i, w := range claims {
		if w.Session != session {
			continue
		}
		if oldest < 0 {
			oldest = i
		}
		if w.Command == "" {
			continue
		}
		for _, a := range ancestors {
			if strings.Contains(a, w.Command) {
				if best < 0 || len(w.Command) > len(claims[best].Command) {
					best = i
				}
				break
			}
		}
	}
	if best >= 0 {
		return best
	}
	return oldest
}

// ancestorCmdlines is the command lines (arguments joined by spaces) of this
// process's ancestors, nearest first, at most 8 deep; empty where /proc isn't
// there. Claude Code single-quotes the command into `eval '...'`, so each
// `'"'"'` (a quote inside it) is turned back into `'`. A variable so tests
// set it.
var ancestorCmdlines = func() []string {
	var out []string
	pid := os.Getppid()
	for depth := 0; depth < 8 && pid > 1; depth++ {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			break
		}
		out = append(out, strings.ReplaceAll(strings.ReplaceAll(string(b), "\x00", " "), `'"'"'`, `'`))
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			break
		}
		// pid (comm) state ppid ...: comm may hold spaces and parentheses.
		rest := string(st)
		k := strings.LastIndexByte(rest, ')')
		if k < 0 {
			break
		}
		f := strings.Fields(rest[k+1:])
		if len(f) < 2 {
			break
		}
		next, err := strconv.Atoi(f[1])
		if err != nil {
			break
		}
		pid = next
	}
	return out
}

// runsAgentsWait says whether cmd runs `wp agents wait` as a command: first in
// the command or after ; & | ( ) { a backquote or a newline, behind any
// number of VAR=value assignments and wrappers (timeout, env, nohup, time,
// exec, command, nice, stdbuf, setsid), or inside `bash -c '...'`. The words
// of `echo "wp agents wait"` or `grep wp agents wait f` are arguments, not a
// call, and do not count. Quotes are honoured, nothing is expanded.
func runsAgentsWait(cmd string) bool { return runsAgentsWaitAt(cmd, 0) }

func runsAgentsWaitAt(cmd string, depth int) bool {
	if len(cmd) > 20000 {
		return false
	}
	for _, words := range shellCommands(cmd) {
		if commandIsAgentsWait(words, depth) {
			return true
		}
	}
	return false
}

// shellCommands splits cmd into simple commands (lists of words, quotes
// removed) at the unquoted separators ; & | ( ) { } a backquote and newline;
// the & of a redirection (2>&1 >&2 <&3 &>f &>>f) is not one.
func shellCommands(cmd string) [][]string {
	var out [][]string
	var words []string
	var w strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, w.String())
			w.Reset()
			inWord = false
		}
	}
	endCmd := func() {
		endWord()
		if len(words) > 0 {
			out = append(out, words)
		}
		words = nil
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case '\\':
			if i+1 < len(rs) {
				i++
				w.WriteRune(rs[i])
				inWord = true
			}
		case '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			w.WriteString(string(rs[i+1 : j]))
			inWord = true
			i = j
		case '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) {
					j++
				}
				w.WriteRune(rs[j])
				j++
			}
			inWord = true
			i = j
		case ' ', '\t':
			endWord()
		case '&':
			// The & of a redirection (2>&1, >&2, <&3, &>f, &>>f) is part of the word.
			if (i > 0 && (rs[i-1] == '>' || rs[i-1] == '<')) || (i+1 < len(rs) && rs[i+1] == '>') {
				w.WriteRune(r)
				inWord = true
			} else {
				endCmd()
			}
		case ';', '|', '(', ')', '{', '}', '`', '\n', '\r':
			endCmd()
		default:
			w.WriteRune(r)
			inWord = true
		}
	}
	endCmd()
	return out
}

var (
	reDigitLed = regexp.MustCompile(`^\d`)
)

// wrapperValueOpts are the wrappers skipWrappers skips with their options,
// and for each the options whose value is the next word (audit #1372.9).
var wrapperValueOpts = map[string]map[string]bool{
	"timeout": {"-s": true, "--signal": true, "-k": true, "--kill-after": true},
	"nice":    {"-n": true, "--adjustment": true},
	"stdbuf":  {"-i": true, "-o": true, "-e": true, "--input": true, "--output": true, "--error": true},
	"env":     {"-u": true, "--unset": true, "-C": true, "--chdir": true},
}

// skipWrappers drops from the front of one simple command's words what only
// wraps the command: assignments; timeout, nice, stdbuf and env with their
// options; nohup, time, exec, command and setsid. It returns the words from
// the command itself (none: there was none), or, when that is `bash -c` or
// `sh -c`, the script it runs.
func skipWrappers(words []string) (rest []string, script string) {
	i := 0
	for i < len(words) {
		w := words[i]
		switch {
		case isAssignment(w):
			i++
		case wrapperValueOpts[w] != nil:
			i++
			for i < len(words) && (strings.HasPrefix(words[i], "-") || isAssignment(words[i]) ||
				(w != "env" && reDigitLed.MatchString(words[i]))) {
				if wrapperValueOpts[w][words[i]] {
					i++ // its value is the next word: timeout -s KILL, env -u NAME
				}
				i++
			}
		case w == "nohup" || w == "time" || w == "exec" || w == "command" || w == "setsid":
			i++
		case w == "bash" || w == "sh" || strings.HasSuffix(w, "/bash") || strings.HasSuffix(w, "/sh"):
			if i+2 < len(words) && words[i+1] == "-c" {
				return nil, words[i+2]
			}
			return nil, ""
		default:
			return words[i:], ""
		}
	}
	return nil, ""
}

// isWp says the word names the wp command.
func isWp(w string) bool { return w == "wp" || strings.HasSuffix(w, "/wp") }

// commandIsAgentsWait: words, one simple command, runs `wp agents wait`.
func commandIsAgentsWait(words []string, depth int) bool {
	rest, script := skipWrappers(words)
	if script != "" {
		return depth < 2 && runsAgentsWaitAt(script, depth+1)
	}
	return len(rest) > 2 && isWp(rest[0]) && rest[1] == "agents" && rest[2] == "wait"
}
