package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		for sid, m := range s.Agents { // forget what never stopped
			for id, a := range m {
				if now.Sub(a.Since) > gateLiveTTL {
					delete(m, id)
				}
			}
			if len(m) == 0 {
				delete(s.Agents, sid)
			}
		}
		if in.HookEventName == "SubagentStop" {
			delete(s.Agents[in.SessionID], in.AgentID)
			return
		}
		if s.Agents[in.SessionID] == nil {
			s.Agents[in.SessionID] = map[string]liveAgent{}
		}
		s.Agents[in.SessionID][in.AgentID] = liveAgent{Type: in.AgentType, Since: now}
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
	if b := gateOutput(mode, problems); b != nil {
		_, _ = stdout.Write(b)
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
func subagentModelEnv() string {
	const key = "CLAUDE_CODE_SUBAGENT_MODEL"
	if v := os.Getenv(key); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")) // #nosec G304 -- fixed path under home
	if err != nil {
		return ""
	}
	var st struct {
		Env map[string]any `json:"env"`
	}
	if json.Unmarshal(b, &st) != nil {
		return ""
	}
	v, _ := st.Env[key].(string)
	return v
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
