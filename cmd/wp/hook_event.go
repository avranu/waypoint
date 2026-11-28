package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/codeindex"
	"git.jmann.me/claude/waypoint/internal/core"
)

const (
	hookHTTPTimeout = 3 * time.Second
	endHTTPTimeout  = 1200 * time.Millisecond
	maxStopAwait    = 8
	outboxMax       = 200
	outboxMaxAge    = 24 * time.Hour
)

// hookEventInput is what Claude Code gives a hook on stdin.
type hookEventInput struct {
	SessionID       string          `json:"session_id"`
	TranscriptPath  string          `json:"transcript_path"`
	Cwd             string          `json:"cwd"`
	HookEventName   string          `json:"hook_event_name"`
	Source          string          `json:"source"`
	Model           string          `json:"model"`
	Prompt          string          `json:"prompt"`
	SessionTitle    string          `json:"session_title"`
	NotifType       string          `json:"notification_type"`
	Message         string          `json:"message"`
	StopHookActive  bool            `json:"stop_hook_active"`
	LastMessage     string          `json:"last_assistant_message"`
	ToolName        string          `json:"tool_name"`
	ToolInput       json.RawMessage `json:"tool_input"`
	ToolUseID       string          `json:"tool_use_id"`
	ToolResponse    json.RawMessage `json:"tool_response"`
	Reason          string          `json:"reason"`
	AgentID         string          `json:"agent_id"`
	AgentType       string          `json:"agent_type"`
	AgentTranscript string          `json:"agent_transcript_path"` // SubagentStop
	// What the session runs with (WP-1468). Observed in live 2.1.330 payloads
	// for SessionStart, UserPromptSubmit, Stop and tool events; PostCompact,
	// ConfigChange, InstructionsLoaded and PostModelSwitch and their fields are
	// from the hooks reference, https://code.claude.com/docs/en/hooks.md, not
	// yet captured live (WP-1486).
	PermissionMode string          `json:"permission_mode"` // observed on UserPromptSubmit and Stop, not SessionStart
	EffortRaw      json.RawMessage `json:"effort"`          // {"level": "low"}: observed on PreToolUse, PostToolUse, Stop, SubagentStop; absent on UserPromptSubmit and SessionStart
	Trigger        string          `json:"trigger"`         // PreCompact, PostCompact: manual or auto
	FilePath       string          `json:"file_path"`       // InstructionsLoaded, ConfigChange
	MemoryType     string          `json:"memory_type"`     // InstructionsLoaded
	LoadReason     string          `json:"load_reason"`     // InstructionsLoaded
	FromModel      string          `json:"from_model"`      // PostModelSwitch
	ToModel        string          `json:"to_model"`        // PostModelSwitch
	RequestedModel string          `json:"requested_model"` // PostModelSwitch
	// Stop: what can still wake the agent (WP-D22). Absent from Claude Code
	// versions that don't say.
	BackgroundTasks json.RawMessage `json:"background_tasks"`
	SessionCrons    json.RawMessage `json:"session_crons"`
	// Harness names the non-Claude harness when invoked via an adapter
	// (e.g. "antigravity"); empty defaults to "claude".
	Harness string `json:"-"`
}

// effortLevel is the effort level of a payload: its `effort.level`, "" when
// the payload has none or says it differently (never an error: a payload
// this CLI can't read in one field must still be read in the rest).
func (in hookEventInput) effortLevel() string {
	var e struct {
		Level string `json:"level"`
	}
	if json.Unmarshal(in.EffortRaw, &e) != nil {
		return ""
	}
	return e.Level
}

// effortPresent: the payload has an effort field at all (not absent, not null).
func (in hookEventInput) effortPresent() bool {
	raw := strings.TrimSpace(string(in.EffortRaw))
	return raw != "" && raw != "null"
}

// sessionEvent is core.HookEvent as the CLI sends it (the CLI imports
// internal/core, but sends only these fields).
type sessionEvent struct {
	// PromptSum: UserPromptSubmit: the digest of the whole prompt (core.PromptSum),
	// for the server to tell what Waypoint typed from what a person did
	// (WP-1370). Not sent under --no-prompts: see TypedID.
	PromptSum string `json:"prompt_sum,omitempty"`
	// TypedID: under --no-prompts, the id of the pending record the hook matched
	// itself (matchTyped); in place of the digest and the text, which stay here.
	TypedID int64 `json:"typed_id,omitempty"`
	// Key: set on UserPromptSubmit only: one id per hook invocation, made once
	// and kept in the outbox, so every attempt to post this event carries it
	// and the server answers a repeat as it answered the first (WP-1370).
	Key              string           `json:"key,omitempty"`
	Event            string           `json:"event"`
	At               int64            `json:"at,omitempty"`
	SessionID        string           `json:"session_id"`
	Machine          string           `json:"machine"`
	CLI              string           `json:"cli,omitempty"`
	Cwd              string           `json:"cwd,omitempty"`
	Repo             string           `json:"repo,omitempty"`
	Branch           string           `json:"branch,omitempty"`
	Worktree         bool             `json:"worktree,omitempty"`
	Model            string           `json:"model,omitempty"`
	Source           string           `json:"source,omitempty"`
	Prompt           string           `json:"prompt,omitempty"`
	PromptWithheld   bool             `json:"prompt_withheld,omitempty"` // a typed prompt whose text --no-prompts left out
	Title            string           `json:"title,omitempty"`
	NotificationType string           `json:"notification_type,omitempty"`
	Message          string           `json:"message,omitempty"`
	LastMessage      string           `json:"last_message,omitempty"`
	StopHookActive   bool             `json:"stop_hook_active,omitempty"`
	InputTokens      int64            `json:"input_tokens,omitempty"`
	OutputTokens     int64            `json:"output_tokens,omitempty"`
	Tool             string           `json:"tool,omitempty"`
	ToolUseID        string           `json:"tool_use_id,omitempty"`
	ToolInput        json.RawMessage  `json:"tool_input,omitempty"`
	ToolResponse     json.RawMessage  `json:"tool_response,omitempty"`
	Reason           string           `json:"reason,omitempty"`
	AgentID          string           `json:"agent_id,omitempty"`
	AgentType        string           `json:"agent_type,omitempty"`
	AgentDescription string           `json:"agent_description,omitempty"` // SubagentStop of an adversarial review (WP-1089)
	AgentModel       string           `json:"agent_model,omitempty"`
	AgentPrompt      string           `json:"agent_prompt,omitempty"`
	AgentReport      string           `json:"agent_report,omitempty"`
	EndReason        string           `json:"end_reason,omitempty"`
	Pending          *hookPending     `json:"pending,omitempty"`
	SpawnedBy        string           `json:"spawned_by,omitempty"`
	Task             string           `json:"task,omitempty"`           // WP_TASK: the tasks a launcher bound the session to (WP-34)
	ContextTokens    int64            `json:"context_tokens,omitempty"` // the context its last call carried
	CacheTTL         string           `json:"cache_ttl,omitempty"`      // Stop: the prompt-cache tier its last calls wrote (WP-192)
	Checkouts        []hookCheckout   `json:"checkouts,omitempty"`      // Stop, SessionStart: the git state of its checkouts (WP-567)
	PermissionMode   string           `json:"permission_mode,omitempty"`
	Effort           string           `json:"effort,omitempty"`
	FromModel        string           `json:"from_model,omitempty"`
	RequestedModel   string           `json:"requested_model,omitempty"`
	File             string           `json:"file,omitempty"`
	MemoryType       string           `json:"memory_type,omitempty"`
	LoadReason       string           `json:"load_reason,omitempty"`
	Snapshot         *hookSnapshot    `json:"snapshot,omitempty"` // SessionStart, PostCompact, ConfigChange (WP-1468)
	Drops            map[string]int64 `json:"drops,omitempty"`    // what the outbox dropped since the last send, "reason/event"
	DropsID          string           `json:"drops_id,omitempty"` // names that report, so the server counts it once
}

// hookPending is core.HookPending: the background tasks, crons and wakeups
// a Stop reports.
type hookPending struct {
	Tasks []pendingTask `json:"tasks"`
	Crons []pendingCron `json:"crons"`
}

type pendingTask struct {
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
}

type pendingCron struct {
	Schedule  string `json:"schedule,omitempty"`
	Recurring bool   `json:"recurring,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
}

// stopPending reads what a Stop says still runs; nil when it says nothing
// (an older Claude Code), so Waypoint can't call the agent finished.
//
// Its text is a person's: a cron's prompt is what they typed and a task's
// description can quote it, so both get the redaction the prompt does, and
// under --no-prompts a cron's prompt text is not sent at all (WP-1470).
func stopPending(in hookEventInput, known []string, noPrompts bool) *hookPending {
	if len(in.BackgroundTasks) == 0 && len(in.SessionCrons) == 0 {
		return nil
	}
	p := &hookPending{Tasks: []pendingTask{}, Crons: []pendingCron{}}
	if json.Unmarshal(in.BackgroundTasks, &p.Tasks) != nil || p.Tasks == nil {
		p.Tasks = []pendingTask{}
	}
	if json.Unmarshal(in.SessionCrons, &p.Crons) != nil || p.Crons == nil {
		p.Crons = []pendingCron{}
	}
	for n := range p.Tasks {
		d, _, _ := redactTextCounted(p.Tasks[n].Description, known)
		p.Tasks[n].Description = truncRunes(d, 200)
	}
	for n := range p.Crons {
		if noPrompts {
			p.Crons[n].Prompt = ""
			continue
		}
		d, _, _ := redactTextCounted(p.Crons[n].Prompt, known)
		p.Crons[n].Prompt = truncRunes(d, 200)
	}
	return p
}

// queuedEvent is an event waiting for the server.
type queuedEvent struct {
	Event sessionEvent `json:"event"`
}

// cwdSession is the agent session that started in a directory.
type cwdSession struct {
	Session string    `json:"session"`
	Machine string    `json:"machine"`
	At      time.Time `json:"at"`
	// Pid and PidStart name the harness process that started the session
	// (PidStart is its start time, so a reused pid isn't taken for it): the
	// session is alive while that process is. Entries from before they were
	// kept have neither, and are held to their age alone.
	Pid      int    `json:"pid,omitempty"`
	PidStart string `json:"pid_start,omitempty"`
	// Cwd is the directory the session started in. State.Cwds is keyed by
	// session; an entry from before that (Cwd empty) is keyed by its directory.
	Cwd string `json:"cwd,omitempty"`
}

// alive says whether the session's harness process still runs (true when
// the entry doesn't say, or the system can't tell).
func (cs *cwdSession) alive() bool {
	if cs.Pid == 0 || cs.PidStart == "" {
		return true
	}
	start := procStart(cs.Pid)
	if start == "" {
		return !procFSExists()
	}
	return start == cs.PidStart
}

// procFSExists: this system has /proc to ask about processes.
func procFSExists() bool {
	_, err := os.Stat("/proc/self/stat")
	return err == nil
}

// procStart is the start time (in clock ticks since boot) of process pid, as
// /proc says; "" when there is no such process, it has exited and is only
// waiting to be reaped (a zombie is gone here as on the lock side, readLockProc),
// or there is no /proc.
func procStart(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")) // #nosec G304 -- /proc
	if err != nil {
		return ""
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return ""
	}
	if f[0] == "Z" || f[0] == "X" {
		return ""
	}
	return f[19]
}

// harnessPid is the process the hook runs under that Claude Code started: the
// nearest ancestor that isn't a shell, else the parent.
func harnessPid() int {
	parent := os.Getppid()
	pid := parent
	for range 6 {
		b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")) // #nosec G304 -- /proc
		if err != nil {
			return parent
		}
		i, j := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
		if i < 0 || j < i {
			return parent
		}
		switch comm := string(b[i+1 : j]); comm {
		case "sh", "bash", "zsh", "dash", "fish":
			f := strings.Fields(string(b[j+1:]))
			if len(f) < 2 {
				return parent
			}
			next, err := strconv.Atoi(f[1])
			if err != nil || next <= 1 {
				return parent
			}
			pid = next
		default:
			return pid
		}
	}
	return parent
}

// machineName is what Waypoint calls this computer: WAYPOINT_MACHINE, else
// the host name. Runners are matched to sessions by it.
func machineName() string {
	if m := strings.TrimSpace(os.Getenv("WAYPOINT_MACHINE")); m != "" {
		return m
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

// addSessionHeaders tells the server which agent session a request comes
// from (what the agent does is recorded on it): the session in the
// environment (SessionStart put it there), else the one that started in
// this directory. People's own commands carry none.
func (c *cli) addSessionHeaders(req *http.Request) {
	if req.Header.Get("Authorization") == "" {
		return
	}
	if t := taskBinding(); t != "" {
		req.Header.Set("X-Waypoint-Task", t)
	}
	if c.contextTokens > 0 {
		req.Header.Set("X-Waypoint-Context", strconv.FormatInt(c.contextTokens, 10))
	}
	if c.claims != "" {
		req.Header.Set("X-Waypoint-Claims", c.claims)
	}
	if prestart() {
		req.Header.Set("X-Waypoint-Prestart", "1")
		return
	}
	id := c.session
	if id == "" {
		if c.agentHeader() == "" {
			return
		}
		id = os.Getenv("WAYPOINT_SESSION")
	}
	cwd, _ := os.Getwd()
	if id == "" && cwd != "" {
		id = c.state.sessionIn(cwd)
	}
	hcli := ""
	if id == "" {
		id, hcli = harnessSessionID(os.Getenv, cwd)
	} else if h := detectHarness(os.Getenv); h != "" && h != "claude" {
		hcli = h
	}
	if id == "" {
		return
	}
	req.Header.Set("X-Waypoint-Session", id)
	if hcli != "" {
		req.Header.Set("X-Waypoint-Session-CLI", hcli)
		if cwd != "" {
			req.Header.Set("X-Waypoint-Cwd", cwd)
		}
	}
	req.Header.Set("X-Waypoint-Machine", cmpStr(os.Getenv("WAYPOINT_MACHINE"), machineName()))
}

// sessionIn is the session a command run in dir belongs to: the one that
// started in the nearest directory at or above dir that any session started
// in. That directory decides: when no session there can be used (it ended, or
// its entry is over a day old and its process can't be shown to be the one
// still running), or two live ones share it so the directory can't say which,
// the answer is "" and never a farther ancestor's session, which is another
// session's. The CLI then sends no session header, so the call is recorded
// with no session rather than another's (WP-1370).
func (s *State) sessionIn(dir string) string {
	for d := realDir(dir); ; d = filepath.Dir(d) {
		if here := s.sessionsAt(d); len(here) > 0 {
			var usable []*cwdSession
			for _, cs := range here {
				if cs.usable() {
					usable = append(usable, cs)
				}
			}
			if len(usable) == 1 {
				return usable[0].Session
			}
			return ""
		}
		if p := filepath.Dir(d); p == d {
			return ""
		}
	}
}

// realDir is dir with its symlinks resolved, so a session that started in a
// directory by one path and a command run in it by another are the same place.
// A path that can't be resolved (it is gone) stays as cleaned.
func realDir(dir string) string {
	d := filepath.Clean(dir)
	if r, err := filepath.EvalSymlinks(d); err == nil {
		return r
	}
	return d
}

// sessionsAt are the entries of sessions that started in dir, newest first.
// Both sides are compared with their symlinks resolved.
func (s *State) sessionsAt(dir string) []*cwdSession {
	var out []*cwdSession
	want := realDir(dir)
	for k, cs := range s.Cwds {
		if cs != nil && realDir(cs.dir(k)) == want {
			out = append(out, cs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// dir is the directory the entry started in; key is its key in State.Cwds.
func (cs *cwdSession) dir(key string) string {
	if cs.Cwd != "" {
		return filepath.Clean(cs.Cwd)
	}
	return filepath.Clean(key)
}

// provablyAlive: the entry names the harness process and it is still running
// with the start time recorded (so not a reused pid).
func (cs *cwdSession) provablyAlive() bool {
	return cs.Pid != 0 && cs.PidStart != "" && procStart(cs.Pid) == cs.PidStart
}

// usable says whether a command may be bound to the entry's session: its
// process is shown running, however long ago it started (a session can run for
// days and At is written only at its start); an entry that can't be shown
// alive or gone is used only within a day of its start.
func (cs *cwdSession) usable() bool {
	if cs.provablyAlive() {
		return true
	}
	return time.Since(cs.At) < 24*time.Hour && cs.alive()
}

// gitFacts is what gitInfo found about the git work tree at a directory.
type gitFacts struct {
	Repo     string // owner/name
	Branch   string // "" outside a work tree
	Detached bool   // Branch is the "(detached <sha>)" form, made with rev-parse
	Worktree bool   // a linked worktree
	Root     string // the repository's main checkout, from the common git dir; "" when it doesn't say
}

// gitInfo is the repo (owner/name), branch and worktree flag of the git
// work tree at dir; empty outside one.
func gitInfo(dir string) (repo, branch string, worktree bool) {
	g := gitFactsAt(dir)
	return g.Repo, g.Branch, g.Worktree
}

// gitFactsAt is gitInfo and the rest of what its git calls say.
func gitFactsAt(dir string) gitFacts {
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- fixed arguments; dir is the hook's cwd
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	var g gitFacts
	if git("rev-parse", "--is-inside-work-tree") != "true" {
		return g
	}
	g.Branch = git("symbolic-ref", "--short", "-q", "HEAD")
	if g.Branch == "" {
		g.Branch, g.Detached = "(detached "+git("rev-parse", "--short", "HEAD")+")", true
	}
	remote := git("remote", "get-url", "origin")
	if remote == "" {
		if first := strings.Fields(git("remote")); len(first) > 0 && codeindex.CheckRef(first[0]) == nil {
			remote = git("remote", "get-url", first[0])
		}
	}
	if r, err := repoFromRemote(remote); err == nil {
		g.Repo = r
	}
	gd, cd := git("rev-parse", "--absolute-git-dir"), git("rev-parse", "--path-format=absolute", "--git-common-dir")
	g.Worktree = gd != "" && cd != "" && filepath.Clean(gd) != filepath.Clean(cd)
	if cd != "" && filepath.Base(filepath.Clean(cd)) == ".git" {
		g.Root = filepath.Dir(filepath.Clean(cd))
	}
	return g
}

// hookClaudeEvent handles one Claude Code hook event. Silent unless it
// has something for the agent; never fails.
func hookClaudeEvent(c *cli, stdin io.Reader, stdout io.Writer, noPrompts bool) {
	var in hookEventInput
	if err := json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in); err != nil || in.SessionID == "" || in.HookEventName == "" {
		return
	}
	hookEventRun(c, in, stdout, noPrompts)
}

func hookEventRun(c *cli, in hookEventInput, stdout io.Writer, noPrompts bool) {
	if in.SessionID == "" || in.HookEventName == "" {
		return
	}
	raw := in.Prompt
	if in.HookEventName == "UserPromptSubmit" {
		in.Prompt = core.UnwrapPasted(in.Prompt)
	}
	slash, slashArg, shipText := "", "", ""
	var typedID int64
	var typedKind string
	typedAnswered := false
	if in.HookEventName == "UserPromptSubmit" {
		if p := strings.TrimSpace(in.Prompt); c.server() != "" && c.token() != "" && (noPrompts || p == "/compact" || p == "/exit") {
			typedID, typedKind, typedAnswered = c.matchTyped(in)
			if !typedAnswered {
				c.countDrop("typed_lookup", "UserPromptSubmit", 1)
			}
		}
		if refusal := c.noteAttendanceKind(in, typedKind, typedAnswered); refusal != "" {
			_, _ = stdout.Write(promptStop(refusal))
			return
		}
		if id := finishedTask(in.Prompt); id != "" {
			stop, more := c.shipStop(in, id)
			if stop != nil {
				_, _ = stdout.Write(stop)
				return
			}
			shipText = more
		}
		c.forgetStopSent(in.SessionID)
		if !strings.HasPrefix(strings.TrimSpace(in.Prompt), "<task-notification>") {
			c.dropSessionSummaries(in.SessionID)
		}
	}
	if in.HookEventName == "Stop" && c.stopAlreadySent(in.SessionID) {
		in.LastMessage = ""
	}
	if in.HookEventName == "UserPromptSubmit" {
		if cmd, _, rest, ok := parseWpCommand(in.Prompt); ok {
			if !wpServerCommand(cmd) {
				return
			}
			slash = cmd
			if cmd == "review" {
				slashArg = wpReviewKey(rest)
			}
		} else if strings.EqualFold(strings.TrimSpace(in.Prompt), "/keep") {
			slash = "keep"
		}
	}
	c.trackAgentsLocally(in)
	if c.server() == "" || c.token() == "" {
		if slash != "" {
			_, _ = stdout.Write(promptStop("Waypoint: this machine isn't logged in (`wp login`), so /wp " + slash + " did nothing."))
		}
		return
	}
	ev := sessionEvent{Event: in.HookEventName, At: time.Now().UnixMilli(), SessionID: in.SessionID, Machine: machineName(), CLI: cmpStr(in.Harness, "claude"), Cwd: in.Cwd,
		Task: taskBinding()}
	var git gitFacts
	if filepath.IsAbs(in.Cwd) {
		switch in.HookEventName {
		case "SessionStart", "UserPromptSubmit", "Stop", "PostCompact", "ConfigChange":
			git = gitFactsAt(in.Cwd)
			ev.Repo, ev.Branch, ev.Worktree = git.Repo, git.Branch, git.Worktree
		}
	}
	ev.PermissionMode, ev.Effort = in.PermissionMode, in.effortLevel()
	switch in.HookEventName {
	case "SessionStart":
		ev.Model, ev.Source = in.Model, in.Source
		ev.SpawnedBy = strings.TrimSpace(os.Getenv("WAYPOINT_FOLLOWUP"))
		if in.Source != "compact" {
			ev.Checkouts = hookCheckouts(in.Cwd, nil)
		}
		ev.Snapshot = buildSnapshot(in, git)
		rememberSession(c, in)
		c.autoUpdateInBackground()
	case "UserPromptSubmit":
		prompt := in.Prompt
		p := strings.TrimSpace(prompt)
		if noPrompts {
			ev.TypedID = typedID
		} else {
			ev.PromptSum = core.PromptSum(prompt)
		}
		ev.Key = newHookKey()
		if slash != "" {
			ev.Prompt = strings.TrimSpace("/wp " + slash + " " + slashArg)
			if slash == "keep" && strings.EqualFold(p, "/keep") {
				ev.Prompt = "/keep"
			}
		} else if key, ok := c.pastedPromptKey(in.SessionID, p); ok {
			ev.Prompt = autopilotPromptMark + " started this session for " + key
			switch {
			case noPrompts:
			case strings.HasPrefix(p, autopilotPromptMark):
				ev.Prompt = raw
			default:
				ev.Prompt += "\n\n" + prompt
			}
		} else if !noPrompts {
			ev.Prompt = raw
		} else if fixed := fixedWaypointPrompt(p); fixed != "" && !typedAnswered {
			ev.Prompt = fixed
		} else if mark := harnessPromptMark(p); mark != "" {
			ev.Prompt = mark
		}
		ev.Prompt, _, _ = redactTextCounted(ev.Prompt, []string{c.token()})
		if noPrompts && ev.Prompt == "" && p != "" {
			ev.PromptWithheld = true
		}
		ev.Title = in.SessionTitle
	case "PostCompact":
		ev.Source = in.Trigger
		ev.Snapshot = buildSnapshot(in, git)
	case "ConfigChange":
		ev.Source, ev.File = in.Source, in.FilePath
		ev.Snapshot = buildSnapshot(in, git)
	case "InstructionsLoaded":
		ev.File, ev.MemoryType, ev.LoadReason = in.FilePath, in.MemoryType, in.LoadReason
	case "PostModelSwitch":
		ev.Model, ev.FromModel, ev.RequestedModel, ev.Source = in.ToModel, in.FromModel, in.RequestedModel, in.Source
	case "Notification":
		ev.NotificationType, ev.Message = in.NotifType, in.Message
	case "PermissionDenied":
		ev.Tool, ev.ToolInput, ev.ToolUseID, ev.Reason = in.ToolName, in.ToolInput, in.ToolUseID, in.Reason
		c.watchSession(in.SessionID)
	case "PreToolUse":
		if in.ToolName != "AskUserQuestion" {
			return
		}
		ev.Tool, ev.ToolInput, ev.ToolUseID = in.ToolName, in.ToolInput, in.ToolUseID
	case "PostToolUse":
		switch {
		case in.ToolName == "AskUserQuestion":
			ev.Tool, ev.ToolUseID, ev.ToolResponse = in.ToolName, in.ToolUseID, in.ToolResponse
		case in.ToolName == "Bash" && c.watching(in.SessionID):
			ev.Tool, ev.ToolInput, ev.ToolUseID = in.ToolName, in.ToolInput, in.ToolUseID
		case in.ToolName == "Read":
			c.keepScreenshot(in)
			return
		case in.ToolName == "Agent" || in.ToolName == "Task":
			return
		default:
			return
		}
	case "Stop":
		ev.LastMessage, ev.StopHookActive = in.LastMessage, in.StopHookActive
		ev.InputTokens, ev.OutputTokens, ev.ContextTokens, ev.CacheTTL = sessionTotals(c, in)
		ev.Pending = stopPending(in, []string{c.token()}, noPrompts)
		ev.Checkouts = hookCheckouts(in.Cwd, c.state.sessionClaims(in.SessionID))
	case "SubagentStart", "SubagentStop":
		ev.AgentID, ev.AgentType = in.AgentID, in.AgentType
		if in.HookEventName == "SubagentStop" {
			ownReview(in, &ev, noPrompts)
		} else {
			ev.AgentModel = c.subagentModel(in)
		}
	case "SessionEnd":
		ev.EndReason = in.Reason
		ev.Pending = stopPending(in, []string{c.token()}, noPrompts)
		ev.InputTokens, ev.OutputTokens, ev.ContextTokens, _ = sessionTotals(c, in)
		c.pushOnSessionEnd(in.SessionID, in.Cwd, in.Reason)
	default:
		return
	}
	c.hookTimeout = hookHTTPTimeout
	if in.HookEventName == "SessionEnd" {
		c.hookTimeout = endHTTPTimeout
	}
	c.hookLimit = hookLateAfter(eventHookTimeout(in.HookEventName))
	res := c.sendSessionEvent(ev)
	kit := ""
	if in.HookEventName == "SessionStart" && ev.Repo != "" {
		kit = c.kitSessionStart(in.Cwd, ev.Repo)
	}
	switch in.HookEventName {
	case "SessionStart":
		if res != nil {
			c.maybeReportMachine()
		}
	case "Stop":
		if over, spent := c.fanoutSessionOver(in.SessionID); over {
			if res != nil {
				if out := stopOutput("", strings.TrimSpace(res.Note+"\n"+res.Text)); out != nil {
					_, _ = stdout.Write(out)
				}
			}
			endBackgroundSession(in.SessionID)
			fanoutEnded("budget", in.TranscriptPath, spent)
			return
		}
	case "SessionEnd":
		fanoutEnded("ended", in.TranscriptPath, c.sessionCost(in.SessionID))
	}
	if slash != "" && (res == nil || res.Block == "") {
		why := "Waypoint didn't answer"
		if res != nil {
			why = "Waypoint's server doesn't know it yet"
		}
		_, _ = stdout.Write(promptStop(fmt.Sprintf("Waypoint: /wp %s did nothing (%s); try again in a moment.", slash, why)))
		return
	}
	if res == nil {
		if in.HookEventName == "SessionStart" {
			_, _ = stdout.Write(sessionStartOutput(c.withTidyHint("", in), kit))
		}
		if shipText != "" {
			_, _ = stdout.Write(hookContext("UserPromptSubmit", shipText))
		}
		return
	}
	c.savePause(res.Pause)
	c.saveGuard(res.Guard)
	switch in.HookEventName {
	case "SessionStart":
		c.saveEnvHosts(ev.Repo, res.EnvHosts)
		text := c.withTidyHint(res.Text, in)
		text = joinParts(c.pauseTell(in.SessionID, hookLateAfter(hookSessionStartTimeout)), text)
		_, _ = stdout.Write(sessionStartOutput(joinParts(res.Instructions, text), kit))
	case "SubagentStart":
		if res.Instructions != "" {
			_, _ = stdout.Write(hookContext("SubagentStart", truncateUTF8(res.Instructions, maxSessionContext)))
		}
	case "UserPromptSubmit":
		c.saveEnvHosts(ev.Repo, res.EnvHosts)
		if res.Block != "" {
			_, _ = stdout.Write(promptStop(res.Block))
			return
		}
		if text := joinParts(shipText, res.Text); text != "" {
			_, _ = stdout.Write(hookContext("UserPromptSubmit", text))
		}
	case "Stop":
		if rule := c.guardStopped(in.SessionID); rule != "" {
			if out := stopOutput("", strings.TrimSpace("Quota guard: this session stays stopped ("+rule+") until you type in it.\n"+res.Note)); out != nil {
				_, _ = stdout.Write(out)
			}
			endBackgroundSession(in.SessionID)
			return
		}
		text := res.Text
		if text == "" && res.Await > 0 {
			text = c.awaitMessage(ev, res.Await)
		}
		if out := stopOutput(text, res.Note); out != nil {
			_, _ = stdout.Write(out)
		}
		if res.End && text == "" {
			endBackgroundSession(in.SessionID)
		}
	}
}

// askHint is the one line every session starts with (WP-96): questions end
// up in requests, where a person sees them, not in chat, where they miss them.
const askHint = "Prefer `wp ask \"…\" -c option -c option` to asking in chat; add `--followup never` when you won't act on the answer."

// sessionHint is what every session starts with: askHint and commonSyntax.
var sessionHint = askHint + "\n\nwp syntax (wp X -h for the rest):\n- " + strings.Join(commonSyntax, "\n- ")

// sessionStartOutput is what the SessionStart hook prints: the server's text
// (decisions that arrived, messages), sessionHint and the agent kit's
// part, as the additionalContext Claude Code adds to the session.
// Claude Code keeps 10,000 characters of it; the kit's part is cut to fit.
func sessionStartOutput(text, kit string) []byte {
	ctx := sessionHint
	if text = strings.TrimSpace(text); text != "" {
		ctx = text + "\n\n" + ctx
	}
	if kit = strings.TrimSpace(kit); kit != "" {
		if room := maxSessionContext - len(ctx) - 2; room < len(kit) {
			kit = truncateUTF8(kit, max(room-1, 0)) + "…"
		}
		if strings.TrimSuffix(kit, "…") != "" {
			ctx += "\n\n" + kit
		}
	}
	return hookContext("SessionStart", ctx)
}

// maxSessionContext is what Claude Code keeps of a hook's additionalContext.
const maxSessionContext = 10000

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// hookContext is the JSON a SessionStart or UserPromptSubmit hook prints to
// add text to what the agent sees.
func hookContext(event, text string) []byte {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": event, "additionalContext": text}})
	return append(b, '\n')
}

// promptStop is what a UserPromptSubmit hook prints to end the prompt there:
// Claude Code shows "Operation stopped by hook: " and the reason to the
// person, clears the input box and calls no model (checked against 2.1.312).
// A block would put the prompt back in the input box, where it reads as a
// draft.
func promptStop(reason string) []byte {
	b, _ := json.Marshal(map[string]any{"continue": false, "stopReason": reason})
	return append(b, '\n')
}

// stopOutput is what the Stop hook prints: a block with the reason when the
// agent has something to read (it continues with it), and the note as
// systemMessage, which Claude Code shows the person and never the model
// (checked against 2.1.299: "Stop says: …" under the turn). Nil: nothing.
func stopOutput(text, note string) []byte {
	out := map[string]string{}
	if text != "" {
		out["decision"], out["reason"] = "block", text
	}
	if note != "" {
		out["systemMessage"] = note
	}
	if len(out) == 0 {
		return nil
	}
	b, _ := json.Marshal(out)
	return append(b, '\n')
}

// awaitMessage waits (up to secs, capped) for a follow-up's message for the
// session; empty if none came.
func (c *cli) awaitMessage(ev sessionEvent, secs int) string {
	secs = min(max(secs, 1), maxStopAwait)
	c.hookTimeout = time.Duration(secs)*time.Second + hookHTTPTimeout
	b, err := c.call("POST", "/api/v1/sessions/hook/await", map[string]any{"session_id": ev.SessionID, "machine": ev.Machine, "wait": secs})
	if err != nil {
		return ""
	}
	var r hookReply
	_ = json.Unmarshal(b, &r)
	return r.Text
}

// rememberSession lets the agent's shell find its session: through
// CLAUDE_ENV_FILE (every command Claude Code runs sources it), and by
// directory, for shells that didn't.
func rememberSession(c *cli, in hookEventInput) {
	if f := os.Getenv("CLAUDE_ENV_FILE"); f != "" {
		line := "export WAYPOINT_SESSION=" + quoteArg(in.SessionID) + "\nexport WAYPOINT_MACHINE=" + quoteArg(machineName()) + "\n"
		if filepath.IsAbs(in.TranscriptPath) {
			line += "export WAYPOINT_TRANSCRIPT=" + quoteArg(in.TranscriptPath) + "\n"
		}
		if fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil { // #nosec G304 G703 -- the path Claude Code gave the hook
			_, _ = fh.WriteString(line)
			_ = fh.Close()
		}
	}
	if filepath.IsAbs(in.Cwd) {
		_ = c.updateState(func(s *State) {
			if s.Cwds == nil {
				s.Cwds = map[string]*cwdSession{}
			}
			moved := map[string]*cwdSession{}
			for k, old := range s.Cwds {
				if old != nil && old.Cwd == "" && old.Session != "" {
					old.Cwd = filepath.Clean(k)
					delete(s.Cwds, k)
					if prev := moved[old.Session]; prev == nil || old.At.After(prev.At) {
						moved[old.Session] = old
					}
				}
			}
			for sid, old := range moved {
				if _, ok := s.Cwds[sid]; !ok {
					s.Cwds[sid] = old
				}
			}
			cs := &cwdSession{Session: in.SessionID, Machine: machineName(), At: time.Now(), Cwd: filepath.Clean(in.Cwd)}
			if pid := harnessPid(); pid > 1 {
				if start := procStart(pid); start != "" {
					cs.Pid, cs.PidStart = pid, start
				}
			}
			if cs.Pid != 0 && cs.PidStart != "" {
				for k, other := range s.Cwds {
					if other != nil && k != in.SessionID && other.Pid == cs.Pid && other.PidStart == cs.PidStart {
						delete(s.Cwds, k)
					}
				}
			}
			s.Cwds[in.SessionID] = cs
			for k, cs := range s.Cwds {
				if cs == nil || (time.Since(cs.At) > 7*24*time.Hour && !cs.provablyAlive()) {
					delete(s.Cwds, k)
				}
			}
		})
	}
}

// sessionTotals counts the session's tokens from its transcript since the
// last time (input is fresh plus cache-write, as for usage reports), and
// returns the running totals, the context its last call carried (WP-34)
// and the prompt-cache tier its calls last wrote (WP-192).
func sessionTotals(c *cli, in hookEventInput) (inTok, outTok, ctxTok int64, ttl string) {
	if in.TranscriptPath == "" {
		return 0, 0, 0, ""
	}
	sess := sessionState{}
	if s := c.state.Sessions[in.SessionID]; s != nil {
		sess = *s
	}
	f, err := os.Open(in.TranscriptPath) // #nosec G304 -- the path Claude Code gave the hook
	if err != nil {
		return sess.TotIn, sess.TotOut, sess.TotCtx, sess.TotTTL
	}
	defer func() { _ = f.Close() }()
	u, err := readTranscriptUsage(f, sess.TotOffset, sess.TotSeen, time.Time{})
	if err != nil {
		return sess.TotIn, sess.TotOut, sess.TotCtx, sess.TotTTL
	}
	_ = c.updateState(func(s *State) {
		if s.Sessions == nil {
			s.Sessions = map[string]*sessionState{}
		}
		cur := s.Sessions[in.SessionID]
		if cur == nil {
			cur = &sessionState{}
			s.Sessions[in.SessionID] = cur
		}
		cur.TotOffset, cur.TotSeen = u.offset, u.seen
		cur.TotIn += u.input
		cur.TotOut += u.output
		if u.context > 0 {
			cur.TotCtx = u.context
		}
		if u.ttl != "" {
			cur.TotTTL = u.ttl
		}
		sess = *cur
	})
	return sess.TotIn, sess.TotOut, sess.TotCtx, sess.TotTTL
}

// watchWindow is how long after a refused call or a command request the
// PostToolUse hook reports Bash calls.
const watchWindow = 2 * time.Hour

// watchSession makes this session's PostToolUse hook report Bash calls for a
// while (WP-126).
func (c *cli) watchSession(id string) {
	if id == "" {
		return
	}
	_ = c.updateState(func(s *State) {
		if s.Sessions == nil {
			s.Sessions = map[string]*sessionState{}
		}
		cur := s.Sessions[id]
		if cur == nil {
			cur = &sessionState{}
			s.Sessions[id] = cur
		}
		cur.WatchUntil = time.Now().Add(watchWindow).Unix()
	})
}

// watching: a request of this session may be satisfied by a command.
func (c *cli) watching(id string) bool {
	s := c.state.Sessions[id]
	return s != nil && s.WatchUntil > time.Now().Unix()
}

// hookReply is what the server answers a batch of events with.
type hookReply struct {
	Session int64  `json:"session"`
	Text    string `json:"text"`
	Await   int    `json:"await"` // seconds to wait for a follow-up's message (WP-D22)
	// Instructions: at a SessionStart or SubagentStart, the agent
	// instructions for whoever just started (ADR146); "" from older servers.
	Instructions string `json:"instructions"`
	Note         string `json:"note"`  // a line for the person, never the model (WP-34)
	End          bool   `json:"end"`   // a task session that finished its tasks: end it (WP-97)
	Block        string `json:"block"` // UserPromptSubmit: drop the prompt, show the person this (WP-ADR9)
	// Pause: the person's session pause, or null (absent from older
	// servers); kept in a local file for the PreToolUse hooks (WP-146).
	Pause json.RawMessage `json:"pause"`
	// Guard: the live quota guard (WP-932); absent or null keeps the copy.
	Guard json.RawMessage `json:"guard"`
	// EnvHosts: at a SessionStart or prompt in a repo, the hosts of its
	// environments a person set, for the guard (WP-749, WP-1087); null
	// leaves the copy alone.
	EnvHosts *[]string `json:"env_hosts"`
}

// sendSessionEvent sends the queued events and this one in a single call.
// Unreachable (or failing) servers queue it; one that refuses it (a revoked
// token, a bad event) drops it. The reply is nil unless the server took it.
func (c *cli) sendSessionEvent(ev sessionEvent) *hookReply {
	c.expireOutbox()
	queued := c.freshOutbox()
	events := make([]sessionEvent, 0, len(queued)+1)
	for _, q := range queued {
		events = append(events, q.Event)
	}
	rep := c.claimDrops()
	events = append(events, ev)
	events[len(events)-1].Drops, events[len(events)-1].DropsID = rep.fields()
	b, err := c.call("POST", "/api/v1/sessions/hook", map[string]any{"events": events})
	if err == nil {
		c.settleDrops(rep)
	}
	if err != nil {
		if !serverRefused(err) {
			c.queueEvent(ev)
			return nil
		}
		var settled []queuedEvent
		for _, q := range queued {
			if _, err := c.call("POST", "/api/v1/sessions/hook", map[string]any{"events": []sessionEvent{q.Event}}); err != nil {
				if !serverRefused(err) {
					c.dropOutbox(settled)
					c.queueEvent(ev)
					return nil
				}
				c.countDrop("refused", q.Event.Event, 1)
			}
			settled = append(settled, q)
		}
		c.dropOutbox(settled)
		if len(queued) == 0 {
			c.countDrop("refused", ev.Event, 1)
			return nil
		}
		one := ev
		rep = c.claimDrops()
		one.Drops, one.DropsID = rep.fields()
		if b, err = c.call("POST", "/api/v1/sessions/hook", map[string]any{"events": []sessionEvent{one}}); err != nil {
			if !serverRefused(err) {
				c.queueEvent(ev)
			} else {
				c.countDrop("refused", ev.Event, 1)
			}
			return nil
		}
		c.settleDrops(rep)
		var r hookReply
		_ = json.Unmarshal(b, &r)
		return &r
	}
	c.dropOutbox(queued)
	var r hookReply
	_ = json.Unmarshal(b, &r)
	return &r
}

// eventHookTimeout is the timeout in seconds of the hook entry that sends
// this event (claudeHooks); the async ones have wp's default of 30.
func eventHookTimeout(event string) int {
	switch event {
	case "SessionStart":
		return hookSessionStartTimeout
	case "UserPromptSubmit", "Stop":
		return 20
	case "SubagentStart":
		return hookSubagentStartTimeout
	case "SessionEnd":
		return 2
	}
	return 30
}

// serverRefused is an answer that sending the same thing again can't change.
func serverRefused(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status < 500 && ae.status != http.StatusRequestTimeout && ae.status != http.StatusTooManyRequests
}

// freshOutbox is the queued events that are still worth sending.
func (c *cli) freshOutbox() []queuedEvent {
	var out []queuedEvent
	cut := time.Now().Add(-outboxMaxAge).UnixMilli()
	for _, q := range c.state.Outbox {
		if q.Event.At >= cut {
			out = append(out, q)
		}
	}
	return out
}

// outboxMaxBytes caps the queue's encoded size: events keep whole snapshots,
// and every hook process parses and rewrites the state file. 1 MiB is a
// chosen limit, not a measured one (WP-1468).
const outboxMaxBytes = 1 << 20

// preOutputWait is how long a state-lock wait before the hook's output may
// take: the time left to the hook's late point (c.hookLimit; a 30 s hook's
// when unset), at most 5 s, at least hookLockFloor.
func (c *cli) preOutputWait() time.Duration {
	limit := c.hookLimit
	if limit <= 0 {
		limit = hookLateAfter(30)
	}
	return min(5*time.Second, hookLockWait(limit))
}

// queueEvent keeps an event for the next call (the newest, within outboxMax
// events and outboxMaxBytes).
func (c *cli) queueEvent(ev sessionEvent) {
	if ev.Event == "PostToolUse" && ev.Tool != "AskUserQuestion" {
		c.countDrop("unqueued", ev.Event, 1)
		return
	}
	if ev.Event == "UserPromptSubmit" && isSessionCommand(ev.Prompt) {
		c.countDrop("unqueued", ev.Event, 1)
		return
	}
	ev.Drops, ev.DropsID = nil, ""
	if err := c.updateStateWithin(c.preOutputWait(), func(s *State) {
		s.Outbox = append(s.Outbox, queuedEvent{Event: ev})
		for len(s.Outbox) > 1 && (len(s.Outbox) > outboxMax || outboxBytes(s.Outbox) > outboxMaxBytes) {
			s.addDrop("overflow", s.Outbox[0].Event.Event, 1)
			s.Outbox = s.Outbox[1:]
		}
	}); err != nil {
		c.warnLostEvent("unqueued", ev.Event)
	}
}

// outboxBytes is the queue's encoded size.
func outboxBytes(q []queuedEvent) int {
	raw, _ := json.Marshal(q)
	return len(raw)
}

// addDrop counts hook data the outbox gave up, "reason/event" (WP-ADR145
// point 5); the next send reports it to the server.
func (s *State) addDrop(reason, event string, n int64) {
	if s.Drops == nil {
		s.Drops = map[string]int64{}
	}
	s.Drops[reason+"/"+event] += n
}

// countDrop counts a drop in the state file. It runs before the hook's output
// is written, so it waits only the hook's bounded wait for the lock
// (preOutputWait). If the lock stays held or the file can't be written, the
// count is lost (stderr is all that remains) and the hook's output still goes
// out on time.
func (c *cli) countDrop(reason, event string, n int64) {
	if err := c.updateStateWithin(c.preOutputWait(), func(s *State) { s.addDrop(reason, event, n) }); err != nil {
		fmt.Fprintf(os.Stderr, "wp hook: lost the count of a dropped %s/%s event: %v\n", reason, event, err)
	}
}

// warnLostEvent says on stderr that an event was lost because the state file
// could not be written: there is nowhere to keep the event or its count.
func (c *cli) warnLostEvent(reason, event string) {
	fmt.Fprintf(os.Stderr, "wp hook: lost a %s %s event: the state file could not be written\n", reason, event)
}

// dropReport is the drop counts one send reports, with an id the server
// applies at most once.
type dropReport struct {
	ID     string           `json:"id"`
	Counts map[string]int64 `json:"counts"`
}

// fields are the event's drops and drops_id; nothing for no report.
func (r *dropReport) fields() (map[string]int64, string) {
	if r == nil || len(r.Counts) == 0 {
		return nil, ""
	}
	return r.Counts, r.ID
}

// claimDrops is the report this send carries. Under the state lock: the one
// already in flight, unchanged (another process, or an earlier send that may
// have been applied, sent it), else a new one made of the counts so far,
// which leave Drops. Counts added later go in the next report. Nil when there
// is nothing to report or the lock stayed held (they wait for the next send).
func (c *cli) claimDrops() *dropReport {
	if c.state.DropsOut == nil && len(c.state.Drops) == 0 {
		return nil
	}
	var rep *dropReport
	_ = c.updateStateWithin(c.preOutputWait(), func(s *State) {
		if s.DropsOut == nil && len(s.Drops) > 0 {
			s.DropsOut = &dropReport{ID: newNonce(), Counts: s.Drops}
			s.Drops = nil
		}
		if s.DropsOut != nil && len(s.DropsOut.Counts) > 0 {
			rep = &dropReport{ID: s.DropsOut.ID, Counts: maps.Clone(s.DropsOut.Counts)}
		}
	})
	return rep
}

// settleDrops clears the in-flight report once a send got through, if it is
// still the one that was sent. If the lock stays held it stays in flight and
// goes again next time: the server ignores an id it has applied.
func (c *cli) settleDrops(sent *dropReport) {
	if sent == nil {
		return
	}
	if err := c.updateStateWithin(c.preOutputWait(), func(s *State) {
		if s.DropsOut != nil && s.DropsOut.ID == sent.ID {
			s.DropsOut = nil
		}
	}); err != nil {
		fmt.Fprintf(os.Stderr, "wp hook: couldn't clear a reported drop count (it is sent again, and counted once): %v\n", err)
	}
}

// expireOutbox counts and forgets the queued events older than outboxMaxAge,
// so what expired is reported by the send that finds it.
func (c *cli) expireOutbox() {
	cut := time.Now().Add(-outboxMaxAge).UnixMilli()
	if !slices.ContainsFunc(c.state.Outbox, func(q queuedEvent) bool { return q.Event.At < cut }) {
		return
	}
	_ = c.updateStateWithin(c.preOutputWait(), func(s *State) {
		var keep []queuedEvent
		for _, q := range s.Outbox {
			if q.Event.At < cut {
				s.addDrop("expired", q.Event.Event, 1)
				continue
			}
			keep = append(keep, q)
		}
		s.Outbox = keep
	})
}

// dropOutbox forgets the events that were sent, and any that are too old to
// matter (expireOutbox usually got those first). (Another hook may have queued more meanwhile: those stay.)
func (c *cli) dropOutbox(sent []queuedEvent) {
	if len(c.state.Outbox) == 0 {
		return
	}
	_ = c.updateState(func(s *State) {
		cut := time.Now().Add(-outboxMaxAge).UnixMilli()
		var keep []queuedEvent
		for _, q := range s.Outbox {
			if slices.ContainsFunc(sent, func(x queuedEvent) bool {
				return x.Event.At == q.Event.At && x.Event.Event == q.Event.Event && x.Event.SessionID == q.Event.SessionID
			}) {
				continue
			}
			if q.Event.At < cut {
				s.addDrop("expired", q.Event.Event, 1)
				continue
			}
			keep = append(keep, q)
		}
		s.Outbox = keep
	})
}

// endBackgroundSession ends a task session that finished its work: `claude
// stop` takes the id `claude --bg` printed, which is the first 8 characters
// of the session id (checked against 2.1.302). It keeps the conversation
// (claude attach reopens it), and fails harmlessly for a session that wasn't
// started in the background. Detached, so this hook returns first.
func endBackgroundSession(sessionID string) {
	defer func() { _ = recover() }()
	if len(sessionID) < 8 || os.Getenv("WP_TASK") == "" {
		return
	}
	bin, err := exec.LookPath(cliBinaries["claude"])
	if err != nil {
		return
	}
	backgroundStart(bin, "stop", sessionID[:8])
}

// prestart says this process takes a task for a session about to start
// (WP-1037): it belongs to no session yet, not to one that started in this
// directory, and the server hands its claim to the session when it starts.
func prestart() bool { return os.Getenv("WAYPOINT_PRESTART") == "1" }

// sessionID is the agent session this process belongs to, as
// addSessionHeaders finds it: empty for a person at a terminal.
func (c *cli) sessionID() string {
	if prestart() {
		return ""
	}
	id := c.session
	if id == "" {
		id = os.Getenv("WAYPOINT_SESSION")
	}
	if id == "" {
		if cwd, err := os.Getwd(); err == nil {
			id = c.state.sessionIn(cwd)
		}
	}
	return id
}

// autopilotReviewMark starts autopilot's review prompt (core's
// AutopilotReviewMark), followed by the task's key.
const autopilotReviewMark = autopilotPromptMark + " review of "

// fixedWaypointPrompt is p when it is one of the texts Waypoint types with no
// visible mark, the whole keep-warm ping or exactly /compact or /exit, else "".
// The ping is returned as core's text. They are constants, never a person's words.
func fixedWaypointPrompt(p string) string {
	switch {
	case core.IsKeepWarmPing(p):
		return core.KeepWarmPing()
	case p == "/compact" || p == "/exit":
		return p
	}
	return ""
}

// newHookKey is a fresh id for one hook invocation (sessionEvent.Key).
func newHookKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// matchTyped asks the server for the texts it typed into this session that no
// prompt has used, and compares the prompt with their digests here
// (core.PromptSum), so a hook that sends no prompt text can still tell
// Waypoint's own prompt: the id of the record it matched, and its kind, are
// all that is reported. Three outcomes: matched (id, kind, answered); the
// server answered and none matched (0, "", answered); it did not answer, by a
// failed call or a body that isn't a list (0, "", not answered). A prompt with
// no digest has nothing to look up and counts as answered with no match.
func (c *cli) matchTyped(in hookEventInput) (id int64, kind string, answered bool) {
	sums := core.PromptSums(in.Prompt)
	if len(sums) == 0 {
		return 0, "", true
	}
	c.hookTimeout = hookHTTPTimeout
	b, err := c.call("GET", "/api/v1/sessions/typed-prompts?cli="+url.QueryEscape(in.SessionID), nil)
	if err != nil {
		return 0, "", false
	}
	var pending []core.PendingTyped
	if json.Unmarshal(b, &pending) != nil {
		return 0, "", false
	}
	for _, sum := range sums {
		for _, p := range pending {
			if p.Sum == sum {
				return p.ID, p.Kind, true
			}
		}
	}
	return 0, "", true
}
