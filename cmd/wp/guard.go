package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// The guard (WP-270, WP-ADR10 layer 2): a PreToolUse hook that refuses, in
// sessions no person is driving, what an agent must not do on the strength of
// a task someone else wrote: install software, run code from the web, reach
// other machines, change the host, write to other repositories, or loosen
// the guard itself. Searching and reading the web (WebSearch, WebFetch) is
// allowed (WP-ADR98; WP-1029 weighs limits on it).
// Local only, no network, no subprocess: it runs before every Bash, Write
// and Edit call. It fails open on its
// own errors. WP_GUARD=warn|deny|off in the session's launch environment
// overrides; an agent can't change its hooks' environment.

// guardMode is "deny", "warn" or "off".
func guardMode() string {
	switch m := strings.ToLower(os.Getenv("WP_GUARD")); m {
	case "deny", "warn", "off":
		return m
	}
	return "deny"
}

// guardActive: no person is driving this session. A task session
// (WP_TASK), a follow-up session (WAYPOINT_FOLLOWUP), a runner job and
// whatever it launches (WAYPOINT_UNATTENDED=1), or a person's own session
// that autopilot is driving (WP-277).
func (c *cli) guardActive(session string) bool {
	return os.Getenv("WP_TASK") != "" || os.Getenv("WAYPOINT_FOLLOWUP") != "" || os.Getenv("WAYPOINT_UNATTENDED") == "1" ||
		c.state.autopilotDriven(session)
}

// unattendedMaxAge is how long a session autopilot marked stays marked
// with no new autopilot prompt; each one marks it afresh.
const unattendedMaxAge = 7 * 24 * time.Hour

// autopilotDriven: autopilot pasted this session's last prompt and no person
// has typed one since (WP-277).
func (s *State) autopilotDriven(session string) bool {
	at, ok := s.Unattended[session]
	return session != "" && ok && time.Since(at) < unattendedMaxAge
}

// noteAttendance runs on every UserPromptSubmit, before anything that needs
// the server: a prompt autopilot pasted (it starts with autopilotPromptMark,
// its review prompt too) marks the session unattended; the next prompt a
// person types clears the mark. Waypoint's other prompts (a keep-warm ping,
// a bare /compact) and Claude Code's own (a background task reporting back)
// leave it as it is. A session autopilot clears or
// restarts gets a new id, marked by the prompt pasted into it.
//
// The quota guard (WP-932) counts from the same prompts: refusal is set
// when automation's prompt reaches a session the guard stopped.
func (c *cli) noteAttendance(in hookEventInput) (refusal string) {
	id := in.SessionID
	if id == "" {
		return ""
	}
	p := strings.TrimSpace(in.Prompt)
	switch {
	case strings.HasPrefix(p, autopilotPromptMark) || c.isPastedPrompt(id, p):
		if refusal = c.guardPrompt(id, true); refusal != "" {
			return refusal
		}
		_ = c.updateState(func(s *State) {
			if s.Unattended == nil {
				s.Unattended = map[string]time.Time{}
			}
			s.Unattended[id] = time.Now()
			for old, at := range s.Unattended {
				if time.Since(at) >= unattendedMaxAge {
					delete(s.Unattended, old)
				}
			}
		})
	case strings.HasPrefix(p, keepWarmMark) || p == "/compact" || harnessPromptMark(p) != "":
	default:
		if refusal = c.guardPrompt(id, !personCanType()); refusal != "" {
			return refusal
		}
		if _, ok := c.state.Unattended[id]; ok {
			_ = c.updateState(func(s *State) { delete(s.Unattended, id) })
		}
	}
	return ""
}

// guardMessage is what the agent is told: what was refused, and what to do
// instead.
func (h guardHit) message() string {
	way := "File `wp request run \"<the command>\" -d \"why\"` for a person to run it"
	if h.ask {
		way = "File `wp request ask \"<what you need>\" -d \"why\"` (or do it in a session a person drives)"
	}
	what := h.reason
	if h.what != "" {
		what += " (`" + h.what + "`)"
	}
	if h.way != "" {
		return fmt.Sprintf("Waypoint guard (unattended session, WP-ADR10): %s needs a person. %s.", what, h.way)
	}
	return fmt.Sprintf("Waypoint guard (unattended session, WP-ADR10): %s needs a person. %s; `wp release` the task if it can't go on without it.", what, way)
}

// guardOutput is the PreToolUse reply for a refused call in mode.
func guardOutput(mode string, h *guardHit) []byte {
	if h == nil || mode == "off" {
		return nil
	}
	if mode == "deny" {
		return preToolReply("deny", h.message(), "")
	}
	return preToolReply("", "", h.message())
}

// contained: the session runs inside a Contained sandbox (WP-473), so the
// guard leaves to the sandbox's walls what they enforce (walled). Tests set
// sandboxed to say so.
func (c *cli) contained() bool {
	if c.sandboxed != nil {
		return *c.sandboxed
	}
	return sandbox.InContained()
}

// guardCheck judges one tool call; nil lets it through.
func (c *cli) guardCheck(in hookEventInput) *guardHit {
	var ti struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if len(in.ToolInput) > 0 && json.Unmarshal(in.ToolInput, &ti) != nil {
		return nil // not a call we can judge
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	origin := sync.OnceValue(func() string { return originURLOf(cwd) })
	g := &guard{site: newGuardSite(cwd), origin: func() string { return remoteHost(origin()) },
		envHosts:  func() []string { return c.state.envHostsFor(origin()) },
		contained: c.contained()}
	g.dir = g.site.cwd
	if u, err := url.Parse(c.server()); err == nil {
		g.server = strings.ToLower(u.Hostname())
	}
	switch in.ToolName {
	case "Bash":
		if ti.Command == "" {
			return nil
		}
		return g.bashHit(ti.Command)
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		raw := ti.FilePath
		if in.ToolName == "NotebookEdit" {
			raw = ti.NotebookPath
		}
		if raw == "" {
			return nil
		}
		return g.pathHit(raw)
	}
	return nil
}

// pathHit checks a file the Write, Edit or NotebookEdit tool is asked to write.
func (g *guard) pathHit(raw string) *guardHit {
	p := g.site.resolve(g.site.cwd, raw)
	if p == "" {
		return nil
	}
	return g.keep(g.fileHit(p))
}

// fileHit judges a path the file tools write.
func (g *guard) fileHit(p string) *guardHit {
	switch {
	case loosensGuard(p):
		return hit(ruleGuard, "changing Claude Code's settings, hooks or skills", p)
	case isWaypointState(p):
		return hit(ruleGuard, stateWhy, p)
	case isHostPath(p):
		return hit(ruleHost, "writing into the host's system directories", p)
	case !g.site.confined(p):
		h := hit(ruleRepo, "writing outside this session's repository", p)
		h.ask = true
		return h
	}
	return nil
}

// hookClaudeGuard handles one PreToolUse event; never fails. First the
// nudge refusals (hookRefusal, WP-1411), in every session whatever the guard
// mode: they live here because this hook's timeout is 10 s and the nudge's
// is 2 s, and Claude Code lets the call through when a hook is cancelled.
// Then, silent unless the session is unattended (and the mode isn't off), the
// guard's own refusals.
func hookClaudeGuard(c *cli, stdin io.Reader, stdout io.Writer) {
	var in hookEventInput
	if err := json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in); err != nil {
		return
	}
	mode := guardMode()
	if kind, text := c.hookRefusalFor(in); kind != "" {
		// The guard still judges and logs a call refused here, so its finding
		// is not held back until the retry and guard.log has every hit.
		var h *guardHit
		if mode != "off" && c.guardActive(in.SessionID) {
			h = c.guardCheck(in)
		}
		problems := []string{text}
		if h != nil {
			problems = append(problems, h.message())
		}
		_, _ = stdout.Write(gateOutput("deny", problems))
		if h != nil {
			c.logGuard(in, mode, h, hookCountWait(hookGuardTimeout))
		}
		c.countNudge(kind, hookCountWait(hookGuardTimeout))
		return // refused: the command will not run, so no wait claim
	}
	// The call is let through unless the guard denies it below; only then is
	// `wp agents wait` going to run and claim its agent.
	if mode == "off" || !c.guardActive(in.SessionID) {
		c.claimWait(in)
		return
	}
	h := c.guardCheck(in)
	if h == nil {
		c.claimWait(in)
		return
	}
	_, _ = stdout.Write(guardOutput(mode, h))
	if mode != "deny" {
		c.claimWait(in) // a warning only: the call still runs
	}
	c.logGuard(in, mode, h, hookCountWait(hookGuardTimeout))
}

const (
	// guardLogMax: past this size guard.log moves to guard.log.1 (replacing
	// the one before) and a new one starts, so between 4 and 8 MB of
	// refusals are kept. Each line holds the whole call (WP-860).
	guardLogMax = 4 << 20
	// guardLogWhatMax bounds one call's text: a heredoc can carry a whole
	// file. Past it the line says how long the call was.
	guardLogWhatMax = 64 << 10
)

// logGuard appends one line to guard.log in the config directory: when,
// which session, task and directory, which rule, the part that matched, and
// the whole call that was refused (WP-860: a log that kept only the start of
// the command couldn't say why most refusals happened). Local only.
func (c *cli) logGuard(in hookEventInput, mode string, h *guardHit, wait time.Duration) {
	if c.dir == "" {
		return
	}
	what := h.what
	var ti struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		URL      string `json:"url"`
	}
	_ = json.Unmarshal(in.ToolInput, &ti)
	switch {
	case ti.Command != "":
		what = ti.Command
	case ti.FilePath != "":
		what = ti.FilePath
	case ti.URL != "":
		what = ti.URL
	}
	e := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "session_id": in.SessionID,
		"tool": in.ToolName, "mode": mode, "rule": h.rule, "reason": h.reason, "matched": h.what, "what": what}
	if len(what) > guardLogWhatMax {
		e["what"], e["what_bytes"] = strings.ToValidUTF8(what[:guardLogWhatMax], ""), len(what)
	}
	if len(h.what) > guardLogWhatMax {
		e["matched"] = strings.ToValidUTF8(h.what[:guardLogWhatMax], "")
	}
	if in.Cwd != "" {
		e["cwd"] = in.Cwd
	}
	if task := os.Getenv("WP_TASK"); task != "" {
		e["task"] = task
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	path := filepath.Join(c.dir, "guard.log")
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return
	}
	// Parallel tool calls refuse in parallel hooks: the lock keeps one hook's
	// rotation from moving another's fresh log over the full guard.log.1.
	// The refusal is already written: the lock is tried for wait at most (what
	// is left before the hook's late point, at most 500 ms, and one try once
	// late), so a hung holder cannot carry the run past the timeout, which
	// discards the output.
	// Without it the line is appended after a rotation only at twice
	// guardLogMax, a hard cap so the log cannot grow without bound; racing
	// another hook's rotation then costs at most one old guard.log.1.
	locked := false
	if lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600); err == nil { // #nosec G304 -- our own state directory
		defer func() { _ = lf.Close() }() // closing drops the flock
		for deadline := time.Now().Add(wait); ; time.Sleep(10 * time.Millisecond) {
			if ok, err := slotTryLock(lf); ok {
				locked = true
				break
			} else if err != nil {
				locked = true // no flock here (Windows): rotate unlocked, as before
				break
			} else if time.Now().After(deadline) {
				break
			}
		}
	}
	if locked {
		rotateGuardLog(path, len(line)+1, guardLogMax)
	} else {
		rotateGuardLog(path, len(line)+1, 2*guardLogMax)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- our own state directory
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// rotateGuardLog moves guard.log to guard.log.1 when adding n bytes would
// take it past limit. Lines are never cut.
func rotateGuardLog(path string, n int, limit int64) {
	st, err := os.Stat(path)
	if err != nil || st.Size()+int64(n) <= limit {
		return
	}
	_ = os.Rename(path, path+".1")
}
