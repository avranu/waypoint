package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The wait hook (WP-1571): after a Bash call that went to the background, the
// session is told how long the run is likely to take (when it is wp testsum
// or wp slot run, from the slot directory's history) and what it can do
// meanwhile: the findings of its review round with no verdict, the requests
// answered that it hasn't acted on. A foreground call is over by the time a
// PostToolUse hook runs, so it says nothing for one. The server says what is
// outstanding (POST /api/v1/sessions/outstanding, which also records that it
// told); with none, or with no answer, the hook prints only the ETA, if any.

// hookWait is the PostToolUse(Bash) hook's command.
const hookWait = "hook claude-wait"

// hookWaitTimeout is the hook's timeout in seconds in Claude Code's settings;
// its own call to the server is cut sooner (waitHTTPTimeout).
const hookWaitTimeout = 5

const waitHTTPTimeout = 3 * time.Second

// waitStarts says whether a Bash call went to the background: it was asked to
// (run_in_background), or Claude Code moved it there itself (the response
// carries a backgroundTaskId).
func waitStarts(toolInput, toolResponse json.RawMessage) bool {
	var in struct {
		Background bool `json:"run_in_background"`
	}
	if json.Unmarshal(toolInput, &in) == nil && in.Background {
		return true
	}
	return bashBackground(toolResponse) != ""
}

// shellSegments splits a command line into the words of each simple command,
// honouring quotes: separators (; & | newline parentheses) end a command
// unless quoted.
func shellSegments(s string) [][]string {
	var segs [][]string
	var words []string
	var cur strings.Builder
	have := false // a word is under way (even an empty quoted one)
	var quote rune
	flushWord := func() {
		if have {
			words = append(words, cur.String())
			cur.Reset()
			have = false
		}
	}
	flushSeg := func() {
		flushWord()
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' && i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, have = r, true
		case r == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
			have = true
		case r == ' ' || r == '\t':
			flushWord()
		case r == ';' || r == '&' || r == '|' || r == '\n' || r == '(' || r == ')':
			flushSeg()
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	flushSeg()
	return segs
}

// waitETACommand finds, in a Bash command line, a wp testsum or wp slot run
// with a command after "--" (alone, or after cd, env settings, time, nohup or
// exec): the slot it takes, the command as the lock file and the run record
// know it (slotCmdKey of the arguments), and the -n given (0: none).
func waitETACommand(command string) (name, cmd string, n int, ok bool) {
	for _, words := range shellSegments(command) {
		for len(words) > 0 && (strings.Contains(words[0], "=") || words[0] == "time" || words[0] == "nohup" || words[0] == "exec") {
			words = words[1:]
		}
		if len(words) < 2 || filepath.Base(words[0]) != "wp" {
			continue
		}
		args := words[1:] // os.Args[1:] of that wp
		dash := -1
		for i, a := range args {
			if a == "--" {
				dash = i
				break
			}
		}
		if dash < 0 || dash == len(args)-1 {
			continue
		}
		switch {
		case args[0] == "testsum":
			name = "tests"
		case len(args) > 2 && args[0] == "slot" && args[1] == "run" && validSlotName(args[2]):
			name = args[2]
			for i := 3; i < dash; i++ {
				if v, found := strings.CutPrefix(args[i], "-n"); found {
					v = strings.TrimPrefix(v, "=")
					if v == "" && i+1 < dash {
						v = args[i+1]
					}
					n, _ = strconv.Atoi(v)
				}
			}
		default:
			continue
		}
		return name, slotCmdKey(slotField(strings.Join(args, " "))), n, true
	}
	return "", "", 0, false
}

// waitETA is what a backgrounded wp testsum or wp slot run says about how
// long it will take, as the command itself would say it on stderr but where
// the agent can read it: "" without a run of that command to go by.
func waitETA(command, cwd string) string {
	name, cmd, n, ok := waitETACommand(command)
	if !ok {
		return ""
	}
	if n <= 0 {
		n = defaultSlots()
	}
	dir, err := slotDir()
	if err != nil {
		return ""
	}
	recs := slotRuns(dir)
	run, ok := slotMedianRun(recs, name, cmd, slotRepoKeyIn(cwd))
	if !ok {
		return ""
	}
	holders := slotHolders(dir, name)
	// The command was started a moment ago, so it may already hold a slot: that
	// is no one else's.
	for i, h := range holders {
		if h.Cmd == cmd && h.Elapsed < 15*time.Second {
			holders = append(holders[:i], holders[i+1:]...)
			break
		}
	}
	if len(holders) < n {
		return slotETAMessage(name, false, 0, run)
	}
	wait, ok := slotWaitETA(recs, name, holders, slotQueuedAhead(dir, name), run)
	if !ok {
		wait = -1
	}
	return slotETAMessage(name, true, wait, run)
}

// hookClaudeWait handles one PostToolUse event. Silent unless the call went to
// the background and there is an ETA or outstanding work to say; a server
// that fails means no outstanding work, never a failed call.
func hookClaudeWait(c *cli, stdin io.Reader, stdout io.Writer) {
	var in hookEventInput
	if json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in) != nil ||
		in.HookEventName != "PostToolUse" || in.ToolName != "Bash" || in.SessionID == "" || !waitStarts(in.ToolInput, in.ToolResponse) {
		return
	}
	var call struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(in.ToolInput, &call)
	text := waitETA(call.Command, in.Cwd)
	if c.server() != "" && c.token() != "" {
		c.session = in.SessionID
		c.hookTimeout = waitHTTPTimeout
		if b, err := c.call("POST", "/api/v1/sessions/outstanding", struct{}{}); err == nil {
			var o struct {
				Lines string `json:"lines"`
			}
			if json.Unmarshal(b, &o) == nil && strings.TrimSpace(o.Lines) != "" {
				if text != "" {
					text += "\n\n"
				}
				text += "Work of yours that doesn't need its result, to do while it runs:\n" + strings.TrimSpace(o.Lines)
			}
		}
	}
	if text == "" {
		return
	}
	_, _ = stdout.Write(hookContext("PostToolUse", text))
}
