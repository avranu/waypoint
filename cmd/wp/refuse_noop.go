package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The no-op poll (WP-1443): a Bash call that only prints a literal or
// succeeds (`echo waiting`) does nothing but cost a turn, with the whole
// context read again. An agent that has been told not to end its turn (Claude
// Code asks a subagent for its report the moment it does) answers by running
// one of these over and over. It is refused, and the text says what to do in
// the agent's situation.

var reSleepArg = regexp.MustCompile(`^\d+(\.\d+)?[smhd]?$`)

// pureNoop says whether cmd does nothing but print literal text or succeed:
// only echo, printf, true and `:` (and sleeps beside them), joined by ; or &&.
// It is strict: any variable, substitution, redirection, pipe, glob, escape,
// other command or newline makes it a real command, and `sleep` alone is the
// sleep rules' business.
func pureNoop(cmd string) bool {
	if len(cmd) > 500 {
		return false
	}
	segs, ok := splitLiteral(strings.TrimSpace(cmd))
	if !ok {
		return false
	}
	printed := 0
	for _, w := range segs {
		if len(w) == 0 {
			continue // `a;;b` or a trailing `;`
		}
		switch w[0] {
		case "echo", "printf", "true", ":":
			printed++
		case "sleep":
			if len(w) != 2 || !reSleepArg.MatchString(w[1]) {
				return false
			}
		default:
			return false
		}
	}
	return printed > 0
}

// splitLiteral splits cmd at ; and && into segments of words, quotes removed,
// and reports false for anything a shell would expand or run.
func splitLiteral(cmd string) (segs [][]string, ok bool) {
	var words []string
	var word strings.Builder
	inWord := false
	flushWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	flushSeg := func() {
		flushWord()
		segs = append(segs, words)
		words = nil
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j >= len(rs) {
				return nil, false
			}
			word.WriteString(string(rs[i+1 : j]))
			inWord = true
			i = j
		case '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if strings.ContainsRune("$`\\!", rs[j]) {
					return nil, false
				}
				j++
			}
			if j >= len(rs) {
				return nil, false
			}
			word.WriteString(string(rs[i+1 : j]))
			inWord = true
			i = j
		case ';':
			flushSeg()
		case '&':
			if i+1 < len(rs) && rs[i+1] == '&' {
				flushSeg()
				i++
				continue
			}
			return nil, false
		case ' ', '\t':
			flushWord()
		case '$', '`', '|', '<', '>', '(', ')', '\\', '\n', '\r', '{', '}', '*', '?', '[', ']', '~', '#', '!':
			return nil, false
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	flushSeg()
	return segs, true
}

// noopRefusalText is the refusal for a no-op poll by the agent that made the
// call, or "" when the agent has nothing it could wait for (then the call is
// let through: a lone `echo done` is not a poll). Otherwise it is chosen by
// what the agent could do instead: wait for agents it started (a background
// subagent runs `wp agents wait` in the background and ends its turn; a
// foreground one waits in the foreground), or wait for a background command of
// the session. Each states the reason, and none names Monitor, which does not
// keep a subagent's run open. c may be nil.
func (c *cli) noopRefusalText(in hookEventInput) string {
	const why = "This command does nothing, and each call re-reads your whole context. "
	children := c.hasLiveChildren(in.SessionID, in.AgentID)
	if !children && !bgTaskRunning(in.SessionID) {
		return ""
	}
	who := c.callerOf(in.SessionID, in.AgentID)
	if children {
		switch who {
		case callerBg:
			return why + "Your agents are still running and you are told when each finishes: run `wp agents wait` with run_in_background: true and end your turn."
		case callerFg:
			return why + "Your agents are still running: run `wp agents wait` in the foreground with the Bash timeout set to 600000; it blocks until they have all finished. Ending your turn instead ends your run and stops them."
		}
		return why + "The agents you started are still running; you are told when each finishes."
	}
	switch who {
	case callerBg:
		return why + "A background command is still running: end your turn and you are woken when it exits."
	case callerFg:
		return why + "A background command is still running: wait for it in ONE foreground Bash call with the timeout set to 600000 (`timeout 590 bash -c 'until CHECK; do sleep 5; done'`). Ending your turn instead ends your run and stops it."
	}
	return why + "A background command is still running; you are told when it exits."
}

// hasLiveChildren says whether agent (empty: the main thread) of session
// has started an agent that is still running.
// A nil c has none.
func (c *cli) hasLiveChildren(session, agent string) bool {
	if c == nil || session == "" {
		return false
	}
	s := c.loadState()
	now := time.Now()
	return len(s.childrenOf(session, agent, now)) > 0 || s.pendingStarts(session, agent, now) > 0
}

// bgTaskRunning says whether a background command of the session is still
// running: one of its task output files, newest first (at most three), is
// held open by a process. A variable so tests set it.
var bgTaskRunning = func(session string) bool {
	if session == "" {
		return false
	}
	files, _ := filepath.Glob(filepath.Join(os.TempDir(), "claude-*", "*", session, "tasks", "*.output"))
	type f struct {
		path string
		mod  int64
	}
	var fs []f
	for _, p := range files {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 { // a symlink is a sub-agent's transcript
			continue
		}
		fs = append(fs, f{p, fi.ModTime().UnixNano()})
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod > fs[j].mod })
	for i, x := range fs {
		if i == 3 {
			break
		}
		if heldOpen(x.path) {
			return true
		}
	}
	return false
}
