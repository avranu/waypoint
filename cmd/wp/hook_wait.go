package main

import (
	"encoding/json"
	"io"
	"regexp"
	"slices"
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

// waitETACommand finds, in a Bash command line, a wp testsum or wp slot run
// with a command after "--" (alone, or after cd, assignments or wrappers such
// as timeout, nice, nohup, env, or inside bash -c): the slot it takes, the
// command as the run record knows it (slotRunKey of its arguments, without
// redirections) and the -n given (0: none). A testsum with --repeat or --base
// is not one run of the command, and has no ETA.
func waitETACommand(command string) (name, cmd string, n int, ok bool) {
	return waitETACommandAt(command, 0)
}

func waitETACommandAt(command string, depth int) (name, cmd string, n int, ok bool) {
	if len(command) > 20000 {
		return "", "", 0, false
	}
	for _, words := range shellCommands(command) {
		rest, script := skipWrappers(words)
		if script != "" {
			if depth < 2 {
				if name, cmd, n, ok = waitETACommandAt(script, depth+1); ok {
					return name, cmd, n, true
				}
			}
			continue
		}
		if len(rest) < 2 || !isWp(rest[0]) {
			continue
		}
		args := dropRedirections(rest[1:]) // os.Args[1:] of that wp
		dash := slices.Index(args, "--")
		if dash < 0 || dash == len(args)-1 {
			continue
		}
		switch {
		case args[0] == "testsum":
			name = "tests"
			for _, a := range args[1:dash] {
				if f, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "="); f == "repeat" || f == "base" {
					name = ""
				}
			}
			if name == "" {
				continue
			}
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
		return name, slotRunKey(append([]string{"wp"}, args...), nil), n, true
	}
	return "", "", 0, false
}

// reRedirect matches a redirection word: an optional fd (or &), > >> or <,
// an optional & and the target, which is empty when it is the next word.
var reRedirect = regexp.MustCompile(`^(\d*|&)(>>?|<)&?(.*)$`)

// dropRedirections removes redirections (2>&1, >log, > log, >>log,
// 2>/dev/null, &>log, <in) from words.
func dropRedirections(words []string) []string {
	var out []string
	for i := 0; i < len(words); i++ {
		if m := reRedirect.FindStringSubmatch(words[i]); m != nil {
			if m[3] == "" {
				i++ // the target is the next word
			}
			continue
		}
		out = append(out, words[i])
	}
	return out
}

// waitETA is what a backgrounded wp testsum or wp slot run says about how
// long it will take, as the command itself would say it on stderr but where
// the agent can read it: "" without a run of that command to go by. session is
// the one that made the call: its processes are the ones that are the command
// itself.
func waitETA(command, cwd, session string) string {
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
	// is no one else's. It is the holder of the same command that this session's
	// process started; where that can't be read, one that began just now.
	for i, h := range holders {
		if h.Cmd != cmd {
			continue
		}
		own := h.Elapsed < 15*time.Second
		if s := slotPidSession(h.Pid); s != "" {
			own = s == session
		}
		if own {
			holders = append(holders[:i], holders[i+1:]...)
			break
		}
	}
	if len(holders) < n {
		return slotETAMessage(name, false, 0, run, true)
	}
	return slotQueuedETA(recs, name, holders, slotQueuedAheadOf(dir, name, session), run, true)
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
	text := waitETA(call.Command, in.Cwd, in.SessionID)
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
				text += "Work of yours to do while it runs (leave anything that needs its result):\n" + strings.TrimSpace(o.Lines)
			}
		}
	}
	if text == "" {
		return
	}
	_, _ = stdout.Write(hookContext("PostToolUse", text))
}
