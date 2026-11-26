package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// wpCommands maps what may be typed after /wp to the command it is. The
// server's (core.SessionCommand) are the ones wpServerCommand names.
var wpCommands = map[string]string{
	"next": "next",
	"stop": "stop", "pause": "stop", "done": "stop",
	"continue": "continue", "resume": "continue",
	"status": "status",
	"keep":   "keep",
	"review": "review", "audit": "review",
	"help": "help",
	"btw":  "btw",
	"exe":  "exe", "exec": "exe",
}

// wpServerCommand: the server answers cmd.
func wpServerCommand(cmd string) bool {
	switch cmd {
	case "next", "stop", "continue", "status", "keep", "review":
		return true
	}
	return false
}

// parseWpCommand reads a /wp prompt: the command ("" for an unknown one),
// the word typed for it, and what follows. ok is false for any other prompt.
func parseWpCommand(prompt string) (cmd, word, rest string, ok bool) {
	p := strings.TrimSpace(prompt)
	after, found := strings.CutPrefix(p, "/wp")
	if !found || (after != "" && !strings.ContainsAny(after[:1], " \t\n")) {
		return "", "", "", false
	}
	after = strings.TrimSpace(after)
	if after == "" {
		return "help", "", "", true
	}
	word, rest, _ = strings.Cut(after, " ")
	if i := strings.IndexAny(word, "\t\n"); i >= 0 {
		word, rest = after[:i], after[i+1:]
	}
	return wpCommands[strings.ToLower(word)], word, strings.TrimSpace(rest), true
}

// wpReviewKeyRE is a task key, as core.SessionReviewKey reads one.
var wpReviewKeyRE = lazyre.New(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

// wpReviewKey is the task key /wp review's rest starts with ("": none):
// the only thing typed after a server's command that goes to the server.
func wpReviewKey(rest string) string {
	f := strings.Fields(rest)
	if len(f) > 0 && wpReviewKeyRE.MatchString(f[0]) {
		return strings.ToUpper(f[0])
	}
	return ""
}

// isSessionCommand: the prompt is a /wp command or /keep, so it is no work
// on a task and nothing for the agent.
func isSessionCommand(prompt string) bool {
	if _, _, _, ok := parseWpCommand(prompt); ok {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(prompt), "/keep")
}

const wpHelp = `Waypoint session commands. None starts a turn: the agent reads the /wp line and its answer with its next prompt.
Typed while the agent works, one waits for the turn to end (send now runs it at once, but moves the agent's running
command to the background); a plain message instead reaches the agent at its next tool call.
  /wp next            start the next task now (autopilot's 1-minute pace, not its 20-minute wait)
  /wp stop            leave this session alone: no next task, review, keep-warm ping, compaction or news (also /wp pause, /wp done)
  /wp continue        undo /wp stop (also /wp resume)
  /wp status          what Waypoint will do with this session next
  /wp review [KEY]    this session's review round (or KEY's): queued or running, signs it is moving, and when it should finish (also /wp audit)
  /wp keep            keep the session from autopilot's next step once (same as /keep)
  /wp btw MESSAGE     a note for the agent's next prompt or tool result
  /wp exe "COMMAND"   run a command in this session's directory and see the end of its output; the agent reads it too (a long run: how it ended)`

// sessionNote is what /wp btw or /wp exe left for the agent: it rides on
// the session's next prompt or the agent's next tool result, so the agent
// knows it before its next decision, and no turn of its own is spent on it.
type sessionNote struct {
	At   time.Time `json:"at"`
	Text string    `json:"text,omitempty"` // /wp btw
	Exe  *exeRun   `json:"exe,omitempty"`  // /wp exe
}

// exeRun is a /wp exe command as the agent is told of it.
type exeRun struct {
	Command string `json:"command"`
	Dir     string `json:"dir"`
	Log     string `json:"log"`
	// Status is the file its exit code goes to when it ends ("": Windows,
	// where it isn't kept); Exit the code when it ended within the hook.
	Status string        `json:"status,omitempty"`
	Pid    int           `json:"pid,omitempty"`
	Exit   *int          `json:"exit,omitempty"`
	Took   time.Duration `json:"took,omitempty"`
}

// noteMaxAge is how long an undelivered note waits.
const noteMaxAge = 24 * time.Hour

// notesHead starts what the agent is handed, so it reads it as the
// person's aside and not as a new request.
const notesHead = "From the person, typed with /wp while you worked (for your information: no new task, and needing no reply of its own):"

// addNote keeps n for session's next prompt or tool result, after any
// already waiting.
func (c *cli) addNote(session string, n *sessionNote) error {
	n.At = time.Now()
	return c.updateState(func(s *State) {
		if s.Notes == nil {
			s.Notes = map[string][]*sessionNote{}
		}
		for id, list := range s.Notes {
			if len(list) == 0 || time.Since(list[len(list)-1].At) >= noteMaxAge {
				delete(s.Notes, id)
			}
		}
		s.Notes[session] = append(s.Notes[session], n)
	})
}

// takeNotes is what waits for session, as the agent reads it, and forgets
// it: "" if nothing does. transcript is the session's: where it shows
// Claude Code kept the stopped /wp prompt and Waypoint's answer for the
// agent's next request (keptStopSince), the note would only repeat them,
// so only a /wp exe run that outlived its hook is told again, as it ended.
func (c *cli) takeNotes(session, transcript string) string {
	if session == "" || len(c.state.Notes[session]) == 0 {
		return ""
	}
	var list []*sessionNote
	_ = c.updateState(func(s *State) {
		list = s.Notes[session]
		delete(s.Notes, session)
	})
	var parts []string
	for _, n := range list {
		if n == nil || time.Since(n.At) >= noteMaxAge {
			continue
		}
		ranOn := n.Exe != nil && n.Exe.Exit == nil
		if !ranOn && keptStopSince(transcript, n.At) {
			continue
		}
		switch {
		case n.Exe != nil:
			parts = append(parts, n.Exe.forAgent(n.At))
		case n.Text != "":
			parts = append(parts, "- A note: "+n.Text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return notesHead + "\n" + strings.Join(parts, "\n")
}

// keptTailBytes is how much of a transcript's end keptStopSince reads.
const keptTailBytes = 1 << 20

// keptStopSince: the transcript shows a prompt a hook stopped at or after
// at that Claude Code kept, so the prompt and the stop's reason go to the
// model with the session's next request (Claude Code 2.1.325, measured
// 2026-11-14; WP-ADR108). false when it can't be read: the note is then
// handed over itself, as a Claude Code that drops stopped prompts needs.
func keptStopSince(transcript string, at time.Time) bool {
	if !filepath.IsAbs(transcript) {
		return false
	}
	f, err := os.Open(transcript) // #nosec G304 -- the path Claude Code gave the hook
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return false
	}
	start := max(size-keptTailBytes, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return false
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	since := at.Add(-2 * time.Second)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"promptKept":true`)) {
			continue
		}
		var e struct {
			Timestamp time.Time `json:"timestamp"`
			Ending    *struct {
				Ending struct {
					Kind string `json:"kind"`
				} `json:"ending"`
				Kept bool `json:"promptKept"`
			} `json:"promptSubmitEnding"`
		}
		if json.Unmarshal(line, &e) == nil && e.Ending != nil && e.Ending.Kept &&
			e.Ending.Ending.Kind == "stopped_by_hook" && !e.Timestamp.Before(since) {
			return true
		}
	}
	return false
}

// exeAgentLines and exeAgentBytes bound the output the agent is handed of
// a /wp exe run; the log holds the rest.
const (
	exeAgentLines = 60
	exeAgentBytes = 6000
)

// forAgent tells the agent of the run: the command, where, its exit code
// as known now (a run that outlived the hook may have ended since), and
// the end of its output.
func (r *exeRun) forAgent(at time.Time) string {
	end := "still running when you were told (process " + strconv.Itoa(r.Pid) + ")"
	if r.Pid == 0 {
		end = "exit code unknown"
	}
	switch code, ok := r.exitCode(); {
	case r.Exit != nil:
		end = fmt.Sprintf("exit %d, took %s", *r.Exit, r.Took.Round(100*time.Millisecond))
	case ok:
		end = fmt.Sprintf("exit %d", code)
	}
	tail := exeTail(r.Log, exeAgentLines, exeAgentBytes)
	if tail == "" {
		tail = "(no output)"
	}
	return fmt.Sprintf("- They ran a command with /wp exe in %s at %s (%s):\n$ %s\n%s\n(whole output: %s)",
		r.Dir, at.Format("15:04"), end, r.Command, tail, r.Log)
}

// exitCode reads the code a run that outlived its hook left in its status
// file; false while it runs (or where none is kept).
func (r *exeRun) exitCode() (int, bool) {
	if r.Status == "" {
		return 0, false
	}
	raw, err := os.ReadFile(r.Status)
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return code, err == nil
}

// exeWait is how long /wp exe waits for its command before it leaves it
// running; within the hook's timeout (hookSlashTimeout).
var exeWait = 4 * time.Minute

// exeTail is how much of a command's output the person sees.
const (
	exeTailLines = 30
	exeTailBytes = 3000
)

// hookClaudeSlash handles UserPromptSubmit for /wp help, btw and exe, and
// hands what /wp btw and /wp exe left for the agent to any other prompt.
// Silent for everything else; local only, never an error.
func hookClaudeSlash(c *cli, stdin io.Reader, stdout io.Writer) {
	var in hookEventInput
	if err := json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in); err != nil || in.SessionID == "" {
		return
	}
	in.Prompt = core.UnwrapPasted(in.Prompt)
	p := strings.TrimSpace(in.Prompt)
	cmd, word, rest, ok := parseWpCommand(p)
	if !ok {
		if strings.EqualFold(p, "/keep") || core.IsKeepWarmPing(p) || p == "/compact" {
			return
		}
		if note := c.takeNotes(in.SessionID, in.TranscriptPath); note != "" {
			_, _ = stdout.Write(hookContext("UserPromptSubmit", note))
		}
		return
	}
	if wpServerCommand(cmd) {
		return
	}
	_, _ = stdout.Write(promptStop(c.slashAnswer(in, p, cmd, word, rest)))
}

// slashAnswer runs a local /wp command and is the line the person sees.
func (c *cli) slashAnswer(in hookEventInput, prompt, cmd, word, rest string) string {
	switch cmd {
	case "help":
		return wpHelp
	case "btw":
		if rest == "" {
			return "Waypoint: nothing noted; type /wp btw and the note."
		}
		if err := c.addNote(in.SessionID, &sessionNote{Text: rest}); err != nil {
			return "Waypoint: the note wasn't kept: " + err.Error()
		}
		return "Waypoint: noted; the agent reads it with its next prompt or tool result."
	case "exe":
		if c.isPastedPrompt(in.SessionID, prompt) || unattendedEnv() {
			return "Waypoint: /wp exe runs only what a person types, in a session a person drives; nothing ran."
		}
		return c.runExe(in, unquoteCommand(rest))
	}
	return fmt.Sprintf("Waypoint: there's no /wp %s.\n%s", word, wpHelp)
}

// unattendedEnv: the session was started for a task, a follow-up or a
// runner job, where no person types (guardActive, less a person's own
// session autopilot drives: typing there is the person's).
func unattendedEnv() bool {
	return os.Getenv("WP_TASK") != "" || os.Getenv("WAYPOINT_FOLLOWUP") != "" || os.Getenv("WAYPOINT_UNATTENDED") == "1"
}

// unquoteCommand takes off the quotes around a whole command
// (/wp exe "make check"), and leaves any other quoting alone.
func unquoteCommand(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] && !strings.ContainsRune(s[1:len(s)-1], rune(s[0])) {
		return s[1 : len(s)-1]
	}
	return s
}

// exeTold ends what /wp exe shows the person.
const exeTold = "The agent reads this with its next prompt or tool result."

// runExe runs command with the shell in the session's directory, its
// output to a log file, and waits up to exeWait: the exit code and the end
// of the output, or where it goes on running. The run waits for the
// agent's next prompt or tool result (sessionNote), so it knows what was
// done before it decides anything more; no turn is spent on it.
func (c *cli) runExe(in hookEventInput, command string) string {
	if command == "" {
		return `Waypoint: nothing to run; type /wp exe "COMMAND".`
	}
	dir := in.Cwd
	if !filepath.IsAbs(dir) {
		return "Waypoint: the session's directory isn't known, so nothing ran."
	}
	logPath, f, err := exeLog(c.dir, in.SessionID)
	if err != nil {
		return "Waypoint: nothing ran: " + err.Error()
	}
	defer func() { _ = f.Close() }()
	run := &exeRun{Command: command, Dir: dir, Log: logPath}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command) // #nosec G204 G702 -- the person typed it in their own session (/wp exe)
	} else {
		run.Status = logPath + ".exit"
		cmd = exec.Command("sh", "-c", `sh -c "$1"; c=$?; echo "$c" > "$2"; exit "$c"`, "wp-exe", command, run.Status) // #nosec G204 G702 -- as above
	}
	cmd.Dir, cmd.Stdout, cmd.Stderr, cmd.Stdin = dir, f, f, nil
	cmd.SysProcAttr = detachedProc()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return "Waypoint: it didn't start: " + err.Error()
	}
	run.Pid = cmd.Process.Pid
	told := func(line string) string {
		if err := c.addNote(in.SessionID, &sessionNote{Exe: run}); err != nil {
			return line + "\nThe agent won't be told of it: " + err.Error()
		}
		return line + "\n" + exeTold
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), exeWait)
	defer cancel()
	select {
	case err := <-done:
		code := 0
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			code = ee.ExitCode()
		case err != nil:
			return "Waypoint: " + err.Error()
		}
		took := time.Since(start).Round(100 * time.Millisecond)
		run.Exit, run.Took = &code, took
		head := fmt.Sprintf("$ %s  (exit %d, %s; whole output: %s)", command, code, took, logPath)
		return told(strings.TrimRight(head+"\n"+exeTail(logPath, exeTailLines, exeTailBytes), "\n"))
	case <-ctx.Done():
		return told(fmt.Sprintf("$ %s  (still running after %s as process %d; its output goes on to %s)\n%s",
			command, exeWait, cmd.Process.Pid, logPath, exeTail(logPath, exeTailLines, exeTailBytes)))
	}
}

// exeLog makes the log file for a /wp exe run, under the config directory
// (dir; "": wp's own).
func exeLog(dir, session string) (string, *os.File, error) {
	if dir == "" {
		d, err := configDir()
		if err != nil {
			return "", nil, err
		}
		dir = d
	}
	d := filepath.Join(dir, "exe")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", nil, err
	}
	pruneExeLogs(d)
	id := session
	if len(id) > 8 {
		id = id[:8]
	}
	path := filepath.Join(d, time.Now().Format("20060102-150405")+"-"+id+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- under wp's config directory
	return path, f, err
}

// pruneExeLogs removes /wp exe logs older than a week.
func pruneExeLogs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// exeTail is the end of the log: its last maxLines lines, at most
// maxBytes, marked when cut.
func exeTail(path string, maxLines, maxBytes int) string {
	raw, err := os.ReadFile(path) // #nosec G304 -- the log runExe wrote
	if err != nil {
		return ""
	}
	raw = bytes.TrimRight(raw, "\n")
	cut := false
	if len(raw) > maxBytes {
		raw, cut = raw[len(raw)-maxBytes:], true
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		}
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) > maxLines {
		lines, cut = lines[len(lines)-maxLines:], true
	}
	out := strings.Join(lines, "\n")
	if cut {
		out = "…\n" + out
	}
	return out
}
