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

const (
	gateTailBytes = 256 << 10
	gateLiveTTL   = 3 * time.Hour
)

// liveAgent is a subagent between SubagentStart and SubagentStop.
type liveAgent struct {
	Type  string    `json:"type,omitempty"`
	Since time.Time `json:"since"`
	Mode  string    `json:"mode,omitempty"` // how its Agent call started it: modeBg, modeFg, "" when not known (WP-1443)
	// Model: the model its Agent call resolved to at the gate, "" when not
	// known. Claude Code's hooks don't say a sub-agent's model (ADR146).
	Model string `json:"model,omitempty"`

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
	nested  bool
	agent   string
	context int64
	premium string
	haiku   bool
}

// gateProblems returns one short message per check that fails.
func gateProblems(f gateFacts) []string {
	var out []string
	fork := f.agent == "fork"
	if fork {
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
	if f.premium != "" && f.nested {
		out = append(out, "An agent can't start an agent on "+f.premium+" here: pass model: sonnet or haiku, or report back and let the coordinator decide.")
	} else if f.premium != "" {
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
	if mode == "deny" {
		return preToolReply("deny", text, "")
	}
	return preToolReply("", "", text)
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
	first := start > 0
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
		s.pruneAgents(now)
		if in.HookEventName == "SubagentStop" {
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
		la := liveAgent{Type: in.AgentType, Since: now}
		old, ok := s.Agents[in.SessionID][in.AgentID]
		sameRun := ok && (old.Waiting || old.Provisional)
		if sameRun {
			la.Since, la.Mode, la.Model = old.Since, old.Mode, old.Model
			if la.Type == "" {
				la.Type = old.Type
			}
		}
		p, hasParent := s.AgentParents[in.SessionID][in.AgentID]
		if hasParent {
			la.Mode = p.Mode
			if la.Model == "" {
				la.Model = p.Model
			}
			p.At = now
			s.AgentParents[in.SessionID][in.AgentID] = p
		}
		if !sameRun && !p.Taken {
			if r, ok := s.takeReservation(in.SessionID, "", p.Parent, hasParent, in.AgentType); ok {
				la.Model = r.Model
				if la.Mode == "" {
					la.Mode = r.Mode
				}
			}
			if hasParent {
				p.Taken = true
				s.AgentParents[in.SessionID][in.AgentID] = p
			}
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
			return
		}
	}
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
	if f.nested {
		if m := c.subagentModel(in); m != "" {
			parent = m
		}
	}
	dirs := agentDirs(in.Cwd)
	eff := resolveModel(ti.Model, ti.Type, parent, dirs)
	f.haiku = isModel(eff, "haiku")
	f.premium = premiumFamily(eff)
	problems := gateProblems(f)
	if fork {
		if mode == "off" {
			problems = problems[:1]
		}
		mode = "deny"
	}
	if mode == "deny" && len(problems) > 0 {
		_, _ = stdout.Write(gateOutput(mode, problems))
		return
	}
	notes := problems
	if limit := subagentLimit(); in.SessionID != "" {
		r := c.waitForSlot(in.SessionID, reservation{ID: in.ToolUseID, Mode: startMode(ti.Bg), Parent: in.AgentID, Type: ti.Type, Model: eff}, limit)
		switch {
		case r.abandoned:
			return
		case r.blocked:
			notes = append(notes, queueBlockedText(limit))
		case r.timedOut:
			notes = append(notes, queueTimedOutText(r, limit))
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
// definition file fall through to the env var or the parent. So does any of
// the three that says inherit: it names no model.
func resolveModel(call, agent, parent string, dirs []string) string {
	if agent == "fork" {
		return parent
	}
	if namesModel(call) {
		return strings.TrimSpace(call)
	}
	if m := agentDefModel(dirs, agent); namesModel(m) {
		return m
	}
	if m := subagentModelEnv(); namesModel(m) {
		return strings.TrimSpace(m)
	}
	return parent
}

// namesModel says whether m names a model: it is not empty and not inherit.
func namesModel(m string) bool {
	m = strings.TrimSpace(m)
	return m != "" && !strings.EqualFold(m, "inherit")
}

// subagentModel is the model a sub-agent that just started runs on: what the
// gate recorded for its Agent call (the call's model, its definition's, the
// env setting, its parent's), else its definition's model. When the definition
// says nothing or inherit, the env setting. "" when none of them says:
// unknown, never a guess (ADR146).
func (c *cli) subagentModel(in hookEventInput) string {
	if a, ok := c.loadState().Agents[in.SessionID][in.AgentID]; ok && a.Model != "" {
		return a.Model
	}
	return resolveModel("", in.AgentType, "", agentDirs(in.Cwd))
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
