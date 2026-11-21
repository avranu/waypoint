package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The fan-out gate (WP-143, X6): a PreToolUse hook on the Agent tool that
// refuses forks and checks depth, width and model before an agent is
// started. Local only, no network: it runs before every Agent call. It
// refuses in every session, interactive ones included; WP_GATE=warn|deny|off
// overrides, except for a fork, which is refused in every mode. Haiku agents are exempt from all but the fork rule (see resolveModel). Free models run as
// Bash calls (llm-task), never as Agent calls, so the gate never sees them.

const (
	gateMaxLive   = 4 // live agents of one session tree
	gateTailBytes = 256 << 10
	gateLiveTTL   = 3 * time.Hour // a SubagentStop that never came
)

// liveAgent is a subagent between SubagentStart and SubagentStop.
type liveAgent struct {
	Type  string    `json:"type,omitempty"`
	Since time.Time `json:"since"`
	Mode  string    `json:"mode,omitempty"` // how its Agent call started it: modeBg, modeFg, "" when not known (WP-1443)

	// Provisional: made by the PostToolUse of its Agent call before its
	// SubagentStart came; that start adopts it (keeping Since).
	Provisional bool `json:"provisional,omitempty"`
	// Waiting: the agent stopped with background work of its own still
	// running (SubagentStop's background_tasks). Claude Code wakes it when
	// that exits, so its run is not over: it is kept, with what it waits for.
	Waiting bool      `json:"waiting,omitempty"`
	WaitAt  time.Time `json:"wait_at,omitempty"`
	Seen    time.Time `json:"seen,omitempty"` // the last time a waiter saw what it waits for still running
	Tasks   []bgTask  `json:"tasks,omitempty"`
}

// gateMode is "deny", "warn" or "off".
func gateMode() string {
	switch m := strings.ToLower(os.Getenv("WP_GATE")); m {
	case "deny", "warn", "off":
		return m
	}
	return "deny"
}

// gateFacts is what the checks need.
type gateFacts struct {
	nested  bool        // called from inside a subagent
	agent   string      // subagent_type; "fork" is a fork, "" is general-purpose
	context int64       // the caller's current context, tokens
	premium string      // the family the effective model resolves to when premium ("Opus", "Fable", "Mythos"), else ""
	haiku   bool        // the effective model is Haiku: exempt from every rule
	live    []liveAgent // live agents of the session that are not Haiku
}

// gateProblems returns one short message per check that fails.
func gateProblems(f gateFacts) []string {
	var out []string
	fork := f.agent == "fork"
	if fork {
		// Never allowed, whatever the context size or the parent's model.
		size := " (a fork would copy this context onto this session's model)"
		if f.context >= 1000 {
			size = fmt.Sprintf(" (a fork would copy this %dk-token context onto this session's model)", f.context/1000)
		}
		msg := "Forks are never allowed here" + size + ": start a fresh sonnet-worker or haiku-worker with a focused prompt instead."
		if f.haiku {
			return []string{msg}
		}
		out = append(out, msg)
	}
	if f.haiku {
		return nil
	}
	if f.nested {
		out = append(out, "Agents can't start agents here: report back and let the coordinator decide.")
	}
	if len(f.live) >= gateMaxLive {
		var names []string
		for _, a := range f.live {
			t := a.Type
			if t == "" {
				t = "fork"
			}
			names = append(names, t)
		}
		out = append(out, fmt.Sprintf("%d agents are already live (%s): wait for one to finish, or run the rest as budgeted task sessions with `wp fanout PLAN.toml`.", len(f.live), strings.Join(names, ", ")))
	}
	if f.premium != "" {
		out = append(out, "This agent would run on "+f.premium+": pass model: sonnet unless it needs design-level judgment.")
	}
	return out
}

// gateOutput is the PreToolUse reply for the problems found in mode.
func gateOutput(mode string, problems []string) []byte {
	if len(problems) == 0 || mode == "off" {
		return nil
	}
	text := strings.Join(problems, " ")
	h := map[string]string{"hookEventName": "PreToolUse"}
	if mode == "deny" {
		h["permissionDecision"] = "deny"
		h["permissionDecisionReason"] = text
	} else {
		h["additionalContext"] = text
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": h})
	return append(b, '\n')
}

// transcriptTail reads the caller's current context (input + cache reads +
// cache writes of the last main-conversation assistant call) and its model
// from the last gateTailBytes of the transcript.
func transcriptTail(r io.ReadSeeker) (ctx int64, model string) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, ""
	}
	start := max(size-gateTailBytes, 0)
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		return 0, ""
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	first := start > 0 // the first line is probably cut
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var e struct {
			Type      string `json:"type"`
			Sidechain bool   `json:"isSidechain"`
			Message   struct {
				Model string `json:"model"`
				Usage *struct {
					In int64 `json:"input_tokens"`
					CC int64 `json:"cache_creation_input_tokens"`
					CR int64 `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Sidechain || e.Message.Usage == nil {
			continue
		}
		if n := e.Message.Usage.In + e.Message.Usage.CC + e.Message.Usage.CR; n > 0 {
			ctx = n
		}
		if m := e.Message.Model; m != "" && !strings.HasPrefix(m, "<") {
			model = m
		}
	}
	return ctx, model
}

// trackSubagent records SubagentStart and SubagentStop in the state file.
func (c *cli) trackSubagent(in hookEventInput) {
	if in.SessionID == "" || in.AgentID == "" {
		return
	}
	_ = c.updateState(func(s *State) {
		if s.Agents == nil {
			s.Agents = map[string]map[string]liveAgent{}
		}
		now := time.Now()
		s.pruneAgents(now) // forget what never stopped
		if in.HookEventName == "SubagentStop" {
			// Stopped with work of its own still running: it will be woken
			// when that exits, so its run goes on (WP-1443).
			if tasks := s.runningTasks(in.SessionID, in.AgentID, in.BackgroundTasks); len(tasks) > 0 {
				if a, ok := s.Agents[in.SessionID][in.AgentID]; ok {
					a.Waiting, a.WaitAt, a.Tasks, a.Provisional = true, now, tasks, false
					s.Agents[in.SessionID][in.AgentID] = a
					return
				}
			}
			delete(s.Agents[in.SessionID], in.AgentID)
			if len(s.Agents[in.SessionID]) == 0 {
				delete(s.Agents, in.SessionID)
			}
			return
		}
		if s.Agents[in.SessionID] == nil {
			s.Agents[in.SessionID] = map[string]liveAgent{}
		}
		// A resumed agent starts again: a new run, under the same id and
		// the same parent (the parent's record is kept young while it lives).
		// Not when it is waking from Waiting, or was made by its Agent call's
		// PostToolUse: that is the run it already is.
		la := liveAgent{Type: in.AgentType, Since: now}
		if old, ok := s.Agents[in.SessionID][in.AgentID]; ok && (old.Waiting || old.Provisional) {
			la.Since, la.Mode = old.Since, old.Mode
			if la.Type == "" {
				la.Type = old.Type
			}
		}
		p, hasParent := s.AgentParents[in.SessionID][in.AgentID]
		if hasParent {
			la.Mode = p.Mode
			p.At = now
			s.AgentParents[in.SessionID][in.AgentID] = p
		}
		// The start answers the Agent call that made it: the oldest the gate
		// let through with the same parent and type when the parent is
		// known, else the oldest of the type. It takes from it how it was
		// started when nothing else says.
		if r, ok := s.takeReservation(in.SessionID, "", p.Parent, hasParent, in.AgentType); ok && la.Mode == "" {
			la.Mode = r.Mode
		}
		s.Agents[in.SessionID][in.AgentID] = la
	})
}

// hookClaudeGate handles one PreToolUse(Agent|Task) event. Never fails; it
// speaks only when a check fails or its person paused sessions.
func hookClaudeGate(c *cli, stdin io.Reader, stdout io.Writer) {
	var in hookEventInput
	if err := json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in); err != nil || (in.ToolName != "Agent" && in.ToolName != "Task") {
		return
	}
	// A person's pause (WP-146) refuses every agent, Haiku too, whatever
	// WP_GATE says: it is their call, not a rule of thumb.
	if p := c.activePause(); p != nil {
		_, _ = stdout.Write(gateOutput("deny", []string{pauseGateText(p)}))
		return
	}
	mode := gateMode()
	var ti struct {
		Type  string `json:"subagent_type"`
		Model string `json:"model"`
		Bg    bool   `json:"run_in_background"`
	}
	if len(in.ToolInput) > 0 {
		if err := json.Unmarshal(in.ToolInput, &ti); err != nil {
			return // not a call we can judge; the gate never blocks on its own doubt
		}
	}
	// A fork is refused whatever WP_GATE says (WP-1402): the mode relaxes
	// the rules of thumb below, and "no fork" is not one of them.
	fork := ti.Type == "fork"
	if mode == "off" && !fork {
		return
	}
	f := gateFacts{nested: in.AgentID != "", agent: ti.Type}
	parent := in.Model
	if in.TranscriptPath != "" {
		if fh, err := os.Open(in.TranscriptPath); err == nil { // #nosec G304 -- the hook's own transcript, named by Claude Code
			var m string
			f.context, m = transcriptTail(fh)
			if m != "" {
				parent = m
			}
			_ = fh.Close()
		}
	}
	dirs := agentDirs(in.Cwd)
	eff := resolveModel(ti.Model, ti.Type, parent, dirs)
	f.haiku = isModel(eff, "haiku")
	f.premium = premiumFamily(eff)
	now := time.Now()
	for _, a := range c.loadState().Agents[in.SessionID] {
		if now.Sub(a.Since) > gateLiveTTL {
			continue
		}
		if a.Type != "" && a.Type != "fork" && isModel(resolveModel("", a.Type, "", dirs), "haiku") {
			continue // Haiku does not count toward the width cap
		}
		f.live = append(f.live, a)
	}
	problems := gateProblems(f)
	if fork {
		if mode == "off" {
			problems = problems[:1] // the fork message alone: off silences the rest
		}
		mode = "deny"
	}
	if mode == "deny" && len(problems) > 0 {
		_, _ = stdout.Write(gateOutput(mode, problems))
		return // refused: no slot to wait for
	}
	// What the agent is told with the call, besides a warning's own text.
	notes := problems // warn mode: they are context
	// The slot queue (WP-1443): an agent past Claude Code's limit would be
	// refused with "do not retry", so the start waits for a stop instead.
	if limit := subagentLimit(); in.SessionID != "" {
		r := c.waitForSlot(in.SessionID, reservation{ID: in.ToolUseID, Mode: startMode(ti.Bg), Parent: in.AgentID, Type: ti.Type}, limit)
		switch {
		case r.abandoned:
			return
		case r.timedOut:
			notes = append(notes, queueTimedOutText(r, limit)) // undecided: Claude Code arbitrates
		}
	}
	if in.AgentID != "" && ti.Bg && c.tellOnce(noteAgentWait, in.SessionID, in.AgentID, hookCountWait(hookGateTimeout)) {
		notes = append(notes, noteAgentWaitText(c.callerOf(in.SessionID, in.AgentID)))
	}
	if len(notes) > 0 {
		_, _ = stdout.Write(gateOutput("warn", notes))
	}
}

func isModel(m, family string) bool { return strings.Contains(strings.ToLower(m), family) }

// resolveModel is the model an Agent call will run on: the call's model, the
// agent definition's model: frontmatter, CLAUDE_CODE_SUBAGENT_MODEL, then the
// parent's model. A fork always uses the parent's. Built-in types without a
// definition file fall through to the env var or the parent.
func resolveModel(call, agent, parent string, dirs []string) string {
	if agent == "fork" {
		return parent
	}
	if call != "" {
		return call
	}
	if m := agentDefModel(dirs, agent); m != "" {
		return m
	}
	if m := subagentModelEnv(); m != "" {
		return m
	}
	return parent
}

// subagentModelEnv reads CLAUDE_CODE_SUBAGENT_MODEL from the process
// environment, falling back to the "env" object in ~/.claude/settings.json.
func subagentModelEnv() string { return claudeEnvSetting("CLAUDE_CODE_SUBAGENT_MODEL") }

// settingsEnv is key in the "env" object of the Claude Code settings file at
// path ("" when it is not there).
func settingsEnv(path, key string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- fixed path under home
	if err != nil {
		return ""
	}
	var st struct {
		Env map[string]any `json:"env"`
	}
	if json.Unmarshal(b, &st) != nil {
		return ""
	}
	switch v := st.Env[key].(type) {
	case string:
		return v
	case float64:
		return strconv.Itoa(int(v))
	}
	return ""
}

// agentDirs are the agent definition directories: the user's, then the repo
// of cwd's.
func agentDirs(cwd string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "agents"))
	}
	for d := cwd; d != "" && d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			dirs = append(dirs, filepath.Join(d, ".claude", "agents"))
			break
		}
	}
	return dirs
}

// agentDefModel finds the definition whose name: is agent and returns its
// model: frontmatter ("" if none). Only the first 4 KB of each file is read.
func agentDefModel(dirs []string, agent string) string {
	if agent == "" {
		return ""
	}
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.md"))
		for _, p := range files {
			name, model := frontmatter(p)
			if name == agent {
				return model
			}
		}
	}
	return ""
}

func frontmatter(path string) (name, model string) {
	f, err := os.Open(path) // #nosec G304 -- globbed from an agents directory
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(f, buf)
	lines := strings.Split(string(buf[:n]), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", ""
	}
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "---" {
			break
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "name":
			name = v
		case "model":
			model = v
		}
	}
	return name, model
}
