package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// The wait hook (WP-1571): after a Bash call that starts something slow, the
// session is told what it can do meanwhile: the findings of its review round
// with no verdict, the requests answered that it hasn't acted on. The server
// says what is there (GET /api/v1/sessions/outstanding); with none, or with
// no answer, the hook prints nothing.

// hookWait is the PostToolUse(Bash) hook's command.
const hookWait = "hook claude-wait"

// hookWaitTimeout is the hook's timeout in seconds in Claude Code's settings;
// its own call to the server is cut sooner (waitHTTPTimeout).
const hookWaitTimeout = 5

const waitHTTPTimeout = 3 * time.Second

// waitCommands are the commands that take long: the words they start with.
var waitCommands = [][]string{{"wp", "testsum"}, {"wp", "slot", "run"}, {"wp", "pr", "wait"}, {"make", "check"}}

// waitStarts says whether a Bash call starts a wait: it runs in the
// background, or it is one of the slow commands (wp testsum, wp slot run, wp
// pr wait, make check), alone or after cd, env settings, time or a pipe.
func waitStarts(toolInput json.RawMessage) bool {
	var in struct {
		Command    string `json:"command"`
		Background bool   `json:"run_in_background"`
	}
	if json.Unmarshal(toolInput, &in) != nil {
		return false
	}
	if in.Background {
		return true
	}
	segs := strings.FieldsFunc(in.Command, func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' || r == '(' })
	for _, seg := range segs {
		words := strings.Fields(seg)
		for len(words) > 0 && (strings.Contains(words[0], "=") || words[0] == "time" || words[0] == "nohup" || words[0] == "exec") {
			words = words[1:]
		}
		if len(words) > 0 {
			words[0] = filepath.Base(words[0]) // ./wp, /usr/local/bin/wp
		}
		for _, w := range waitCommands {
			if len(words) >= len(w) && strings.Join(words[:len(w)], " ") == strings.Join(w, " ") {
				return true
			}
		}
	}
	return false
}

// hookClaudeWait handles one PostToolUse event. Silent unless the call starts
// a wait and the session has outstanding work; a server that fails means no
// context, never a failed call.
func hookClaudeWait(c *cli, stdin io.Reader, stdout io.Writer) {
	var in hookEventInput
	if json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in) != nil ||
		in.HookEventName != "PostToolUse" || in.ToolName != "Bash" || in.SessionID == "" || !waitStarts(in.ToolInput) {
		return
	}
	if c.server() == "" || c.token() == "" {
		return
	}
	c.session = in.SessionID
	c.hookTimeout = waitHTTPTimeout
	b, err := c.call("GET", "/api/v1/sessions/outstanding", nil)
	if err != nil {
		return
	}
	var o struct {
		Lines string `json:"lines"`
	}
	if json.Unmarshal(b, &o) != nil || strings.TrimSpace(o.Lines) == "" {
		return
	}
	_, _ = stdout.Write(hookContext("PostToolUse", "Something slow has started. Work of yours that doesn't need its result, to do while you wait:\n"+strings.TrimSpace(o.Lines)))
}
