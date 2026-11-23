package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Follow-up jobs (WP-D22): start a fresh background Claude Code session where
// an agent worked, and post a prompt to a running session's inbox socket.

type sessionJob struct {
	Dir            string `json:"dir"`
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	PermissionMode string `json:"permission_mode"`
	Effort         string `json:"effort"`
	// Task binds the new session to tasks (WP_TASK=KEY[,KEY…]): a task session (WP-34).
	Task string `json:"task"`
	// Name is the session's name (claude attach takes it).
	Name string `json:"name"`
	// Kickoff, instead of Prompt: the project's kickoff template; the
	// runner takes Task for the session and fills it with the answer
	// (WP-1037). Title is the task's, for {title}.
	Kickoff string `json:"kickoff"`
	Title   string `json:"title"`
}

type messageJob struct {
	CLIID string `json:"cli_id"`
	Body  string `json:"body"`
	// IfIdle: post only when Claude Code says the session is idle (a
	// keep-warm ping, WP-192).
	IfIdle bool `json:"if_idle"`
	// Compact: type /compact into its tmux pane instead (WP-192).
	Compact bool `json:"compact"`
	// Type: type Body into its tmux pane, as autopilot does, where it runs
	// in one; the inbox otherwise (WP-534). Claude Code adds a long note
	// about peers to what comes through the inbox.
	Type bool `json:"type"`
}

// bgIDRE finds the id `claude --bg` prints: "backgrounded · b50893f0".
var bgIDRE = lazyre.New(`backgrounded\W+([0-9a-f]{6,36})`)

// execSession runs `claude --bg` in an allowed directory. The new session's
// hooks see WAYPOINT_FOLLOWUP (this job) and link it in Waypoint.
func (p *runnerProc) execSession(ctx context.Context, run *jobRun, in sessionJob) outcome {
	dir, err := resolveDir(in.Dir, p.allowed)
	if err != nil {
		return failed("%v", err)
	}
	tasks := splitKeys(in.Task)
	kickoff := in.Kickoff != "" && strings.TrimSpace(in.Prompt) == ""
	if kickoff && len(tasks) != 1 {
		return failed("a kickoff needs one task")
	}
	if !kickoff && strings.TrimSpace(in.Prompt) == "" {
		return failed("no prompt")
	}
	if c := p.cliInfo("claude"); c == nil {
		return failed("Claude Code isn't installed on this runner")
	}
	// The launcher wp start uses too (WP-34).
	l := taskLaunch{bg: true, prompt: in.Prompt, model: in.Model, permissionMode: in.PermissionMode, effort: in.Effort, tasks: tasks, name: in.Name}
	run.launch.forDir(dir).fill(&l)
	ctx, cancel := context.WithTimeout(ctx, run.timeout)
	defer cancel()
	env := l.sessionEnv(bgSessionEnv(os.Environ()))
	if kickoff {
		// Like autopilot's take (WP-418): wp next runs as the job's own
		// token and server, not the runner machine's config. The session
		// itself keeps env.
		kenv := slices.Clip(env)
		if run.token != "" {
			kenv = append(kenv, "WAYPOINT_SERVER="+p.c.server(), "WAYPOINT_TOKEN="+run.token)
		}
		l.prompt = startKickoff(ctx, dir, kenv, tasks[0], in.Title, in.Kickoff)
	}
	args := l.claudeArgs()
	cmd := procCommand(ctx, cliBinaries["claude"], args...)
	cmd.Dir = dir
	cmd.Env = append(env, "WAYPOINT_FOLLOWUP="+strconv.FormatInt(run.id, 10))
	tail := &tailBuffer{}
	w := &teeWriter{a: run.out, b: tail}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = w, w, nil
	o := finishProcess(ctx, run, cmd, "claude --bg")
	if o.State != "done" {
		if strings.Contains(tail.String(), "not trusted") {
			o.Error = "Claude Code doesn't trust " + dir + " yet: run claude there once and accept the prompt."
		}
		return o
	}
	result, _ := o.Result.(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	if m := bgIDRE.FindStringSubmatch(tail.String()); m != nil {
		result["attach_id"] = m[1]
	}
	o.Result = result
	return o
}

// bgSessionEnv is the runner's environment for a new session: jobEnv's,
// which already leaves out what would tie it to a Claude Code session the
// runner was started from (WAYPOINT_SESSION, CLAUDECODE, CLAUDE_CODE_*).
func bgSessionEnv(base []string) []string {
	env := jobEnv(base, nil, false)
	// The session is the person's own: its hooks need their config directory.
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, "WAYPOINT_CONFIG_DIR") {
			env = append(env, kv)
		}
	}
	return env
}

// ccSession is what Claude Code writes about a running session in
// ~/.claude/sessions/<pid>.json.
type ccSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Socket    string `json:"messagingSocketPath"`
	Status    string `json:"status"`
	ProcStart string `json:"procStart"`
	Tmux      string `json:"tmux"` // "session:@window.%pane" when it runs in tmux
}

// claudeConfigDir is Claude Code's config directory.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// findCCSession finds the running Claude Code session with that id. A file
// without a live process (pid 0 included) is left over from one that ended.
func findCCSession(dir, id string) (*ccSession, error) {
	files, err := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- Claude Code's own session registry
		if err != nil {
			continue
		}
		var s ccSession
		if json.Unmarshal(b, &s) != nil || s.SessionID != id {
			continue
		}
		if s.PID <= 0 || !processAlive(s.PID) {
			continue
		}
		return &s, nil
	}
	return nil, errors.New("the session isn't running on this machine (it was closed, or it runs under another user)")
}

// peerKeyRE is a key file Claude Code leaves beside a session's pid file for
// the peers that may post to its inbox: <pid>.<64 hex>.key.
var peerKeyRE = lazyre.New(`^(\d+)\.[0-9a-f]{64}\.key$`)

// ccPeerKey is a key file's content.
type ccPeerKey struct {
	Token     string `json:"peerToken"`
	ProcStart string `json:"procStart"`
}

// findPeerToken reads the token a session takes from peers, from its key
// file in the session registry; "" when there is none. A key left by an
// earlier process with the same pid (another procStart) doesn't count.
func findPeerToken(dir string, s *ccSession) string {
	if s.PID <= 0 {
		return ""
	}
	pid := strconv.Itoa(s.PID)
	files, _ := filepath.Glob(filepath.Join(dir, "sessions", pid+".*.key"))
	token, newest := "", time.Time{}
	for _, f := range files {
		if m := peerKeyRE.FindStringSubmatch(filepath.Base(f)); m == nil || m[1] != pid {
			continue
		}
		fi, err := os.Stat(f)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > 64<<10 {
			continue
		}
		b, err := os.ReadFile(f) // #nosec G304 -- Claude Code's own session registry
		if err != nil {
			continue
		}
		var k ccPeerKey
		if json.Unmarshal(b, &k) != nil || strings.TrimSpace(k.Token) == "" {
			continue
		}
		if s.ProcStart != "" && k.ProcStart != "" && k.ProcStart != s.ProcStart {
			continue
		}
		if token == "" || fi.ModTime().After(newest) {
			token, newest = k.Token, fi.ModTime()
		}
	}
	return token
}

// inboxLines frames a message for a session's inbox: one JSON object per
// line, the auth line first when there is a token (required on Windows,
// optional elsewhere).
func inboxLines(token, body string) [][]byte {
	var lines [][]byte
	if token != "" {
		auth, _ := json.Marshal(struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}{"auth", token})
		lines = append(lines, append(auth, '\n'))
	}
	msg, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": body}})
	return append(lines, append(msg, '\n'))
}

// execMessage posts a prompt to a running session's inbox (a unix socket; a
// named pipe on Windows): an idle session starts a turn with it, a busy one
// reads it between tool calls.
// With if_idle a session that isn't idle is left alone; the job is done, and
// its result says nothing was posted.
func (p *runnerProc) execMessage(ctx context.Context, run *jobRun, in messageJob) outcome {
	if in.Compact {
		return p.execCompact(ctx, run, in)
	}
	if strings.TrimSpace(in.Body) == "" {
		return failed("no text")
	}
	dir := claudeConfigDir()
	s, err := findCCSession(dir, in.CLIID)
	if err != nil {
		return failed("%v", err)
	}
	if in.IfIdle && !atPrompt(s.Status) {
		return heldMessage(run, "the session is %s, not idle: nothing posted", cmpStr(s.Status, "in an unknown state"))
	}
	if in.Type && s.Tmux != "" {
		return p.execTypeBody(ctx, run, in, s)
	}
	if s.Socket == "" {
		return failed("the session has no inbox socket (Claude Code too old, or messaging off)")
	}
	token := findPeerToken(dir, s)
	if token == "" && inboxNeedsAuth {
		return failed("the session published no peer key (%s), which its inbox needs here", filepath.Join(dir, "sessions", strconv.Itoa(s.PID)+".*.key"))
	}
	reply, err := postInbox(ctx, s.Socket, inboxLines(token, in.Body))
	if err != nil {
		return failed("%v", err)
	}
	auth := ""
	if token != "" {
		auth = ", authenticated"
	}
	fmt.Fprintf(run.out, "posted %d bytes to session %s (%s%s)\n", len(in.Body), in.CLIID, cmpStr(s.Status, "status unknown"), auth)
	if reply != "" {
		fmt.Fprintf(run.out, "the session answered: %s\n", truncRunes(reply, 500))
	}
	return outcome{State: "done", Result: map[string]any{"delivered": true, "status": s.Status, "reply": truncRunes(reply, 500)}}
}

// heldMessage: a job done without posting anything, and why.
func heldMessage(run *jobRun, format string, a ...any) outcome {
	d := fmt.Sprintf(format, a...)
	fmt.Fprintf(run.out, "held: %s\n", d)
	return outcome{State: "done", Result: map[string]any{"delivered": false, "held": d}}
}

// execCompact types /compact into an idle session's tmux pane, as a person
// would (slash commands don't run through the inbox socket), when nothing is
// typed there; the checks are autopilot's (WP-ADR9).
func (p *runnerProc) execCompact(ctx context.Context, run *jobRun, in messageJob) outcome {
	s, err := findCCSession(claudeConfigDir(), in.CLIID)
	if err != nil {
		return heldMessage(run, "%v", err)
	}
	socket, pane, held := idlePane(ctx, run, s)
	if held != nil {
		return *held
	}
	if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "-l", "/compact"); err != nil {
		return failed("%v", err)
	}
	if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "Enter"); err != nil {
		return failed("%v", err)
	}
	fmt.Fprintf(run.out, "typed /compact into session %s (tmux pane %s)\n", in.CLIID, pane)
	return outcome{State: "done", Result: map[string]any{"delivered": true, "compact": true}}
}

// execTypeBody pastes Body into an idle session's tmux pane and sends it,
// when nothing is typed there (WP-534), the way autopilot gives a prompt.
func (p *runnerProc) execTypeBody(ctx context.Context, run *jobRun, in messageJob, s *ccSession) outcome {
	socket, pane, held := idlePane(ctx, run, s)
	if held != nil {
		return *held
	}
	buf := fmt.Sprintf("wp-message-%d", run.id) // one per job: two jobs never paste each other's
	if _, err := runTmux(ctx, socket, []byte(in.Body), "load-buffer", "-b", buf, "-"); err != nil {
		return failed("the text couldn't be loaded: %v", err)
	}
	if _, err := runTmux(ctx, socket, nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", pane); err != nil {
		return failed("the text couldn't be pasted: %v", err)
	}
	sleepCtx(ctx, 400*time.Millisecond) // let it take the paste in before Enter
	if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "Enter"); err != nil {
		return failed("the text was pasted and not sent (press Enter there): %v", err)
	}
	fmt.Fprintf(run.out, "typed %d bytes into session %s (tmux pane %s)\n", len(in.Body), in.CLIID, pane)
	return outcome{State: "done", Result: map[string]any{"delivered": true, "status": s.Status, "typed": true}}
}

// idlePane finds the tmux pane of session s, when s is idle with its input
// box on the screen and empty; held says why not.
func idlePane(ctx context.Context, run *jobRun, s *ccSession) (socket, pane string, held *outcome) {
	hold := func(format string, a ...any) (string, string, *outcome) {
		o := heldMessage(run, format, a...)
		return "", "", &o
	}
	if !atPrompt(s.Status) {
		return hold("the session is %s, not idle", cmpStr(s.Status, "in an unknown state"))
	}
	if s.Tmux == "" {
		return hold("it doesn't run inside tmux, so nothing can be typed into it")
	}
	pane = s.Tmux[strings.LastIndex(s.Tmux, ".")+1:]
	if !strings.HasPrefix(pane, "%") {
		return hold("Claude Code names no tmux pane for it (%q)", s.Tmux)
	}
	socket = tmuxSocketOf(s.PID)
	screen, err := runTmux(ctx, socket, nil, "capture-pane", "-p", "-e", "-t", pane)
	if err != nil {
		return hold("can't read its tmux pane: %v", err)
	}
	if draft, found := promptInput(screen); !found {
		return hold("its input box isn't on the screen (a dialog or a scrolled pane?)")
	} else if draft != "" {
		return hold("someone has typed into its input box (%q)", truncRunes(draft, 40))
	}
	return socket, pane, nil
}
