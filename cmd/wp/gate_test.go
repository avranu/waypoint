package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGateProblems(t *testing.T) {
	for _, c := range []struct {
		name string
		f    gateFacts
		want []string
	}{
		{"worker, small context", gateFacts{agent: "sonnet-worker", context: 50_000}, nil},
		{"fork of a big context", gateFacts{agent: "fork", context: 150_000}, []string{"150k-token context"}},
		{"fork of a small context", gateFacts{agent: "fork", context: 20_000}, []string{"never allowed here (a fork would copy this 20k-token context onto this session's model): start a fresh sonnet-worker or haiku-worker"}},
		{"fork of an unknown context", gateFacts{agent: "fork"}, []string{"fresh sonnet-worker"}},
		{"fork from Haiku", gateFacts{agent: "fork", context: 20_000, haiku: true}, []string{"never allowed"}},
		{"fork from Haiku, nested", gateFacts{agent: "fork", haiku: true, nested: true}, []string{"never allowed"}},
		{"resolves to Fable", gateFacts{context: 20_000, premium: "Fable"}, []string{"run on Fable: pass model: sonnet"}},
		{"resolves to Mythos", gateFacts{context: 20_000, premium: "Mythos"}, []string{"run on Mythos: pass model: sonnet"}},
		{"unnamed type is general-purpose, not a fork", gateFacts{context: 150_000}, nil},
		{"resolves to Opus", gateFacts{context: 150_000, premium: "Opus"}, []string{"run on Opus: pass model: sonnet"}},
		{"nested sonnet", gateFacts{agent: "sonnet-worker", nested: true}, nil},
		{"nested premium", gateFacts{agent: "general-purpose", nested: true, premium: "Fable"}, []string{"An agent can't start an agent on Fable here: pass model: sonnet or haiku"}},
		{"nested fork", gateFacts{agent: "fork", nested: true, context: 20_000}, []string{"never allowed"}},
		{"haiku nested", gateFacts{agent: "haiku-worker", nested: true, haiku: true}, nil},
		{"all at once", gateFacts{agent: "fork", nested: true, context: 200_000, premium: "Opus"}, []string{"fresh", "can't start an agent on Opus"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := gateProblems(c.f)
			if len(got) != len(c.want) {
				t.Fatalf("got %q, want %d problems", got, len(c.want))
			}
			for i, w := range c.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

func TestGateForkUnknownContextMessage(t *testing.T) {
	got := gateProblems(gateFacts{agent: "fork"})
	if len(got) != 1 || strings.Contains(got[0], "0k") {
		t.Errorf("got %q, want one message without a size", got)
	}
}

// A Fable session: forks are refused whatever the context, and an agent that
// would run on Fable (by call, or by inheriting the parent) is refused too.
func TestHookClaudeGateFable(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GATE", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	c := &cli{dir: t.TempDir()}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tr, []byte(`{"type":"assistant","message":{"model":"claude-fable-5-2","usage":{"input_tokens":10,"cache_read_input_tokens":19000,"cache_creation_input_tokens":990}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(input string) string {
		in, _ := json.Marshal(map[string]any{"session_id": "f1", "tool_name": "Agent", "transcript_path": tr, "cwd": t.TempDir(), "tool_input": json.RawMessage(input)})
		var out bytes.Buffer
		hookClaudeGate(c, bytes.NewReader(in), &out)
		return out.String()
	}
	got := run(`{"subagent_type":"fork"}`)
	if !strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "sonnet-worker") || !strings.Contains(got, "haiku-worker") {
		t.Errorf("fork: %s", got)
	}
	if got := run(`{"subagent_type":"general-purpose","model":"fable"}`); !strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "Fable") {
		t.Errorf("model fable: %s", got)
	}
	if got := run(`{"subagent_type":"general-purpose"}`); !strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "Fable") {
		t.Errorf("inherits Fable: %s", got)
	}
	if got := run(`{"subagent_type":"general-purpose","model":"haiku"}`); got != "" {
		t.Errorf("haiku: %s", got)
	}
	if got := run(`{"subagent_type":"general-purpose","model":"sonnet"}`); got != "" {
		t.Errorf("sonnet: %s", got)
	}
}

func TestPremiumFamily(t *testing.T) {
	for m, want := range map[string]string{"claude-opus-4": "Opus", "claude-fable-5-2": "Fable", "Mythos": "Mythos", "sonnet": "", "": ""} {
		if got := premiumFamily(m); got != want || premiumModel(m) != (want != "") {
			t.Errorf("%q: %q", m, got)
		}
	}
}

func TestGateOutputAndMode(t *testing.T) {
	if gateOutput("deny", nil) != nil || gateOutput("off", []string{"x"}) != nil {
		t.Error("no problems or mode off must be silent")
	}
	var o struct {
		H map[string]string `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(gateOutput("deny", []string{"a", "b"}), &o); err != nil || o.H["permissionDecision"] != "deny" || o.H["permissionDecisionReason"] != "a b" {
		t.Errorf("deny: %v %v", o.H, err)
	}
	o.H = nil
	if err := json.Unmarshal(gateOutput("warn", []string{"a"}), &o); err != nil || o.H["additionalContext"] != "a" || o.H["permissionDecision"] != "" {
		t.Errorf("warn: %v %v", o.H, err)
	}
	for _, m := range []struct{ task, gate, want string }{
		{"", "", "deny"}, {"WP-1", "", "deny"}, {"", "warn", "warn"}, {"WP-1", "warn", "warn"}, {"", "deny", "deny"}, {"WP-1", "off", "off"}, {"WP-1", "junk", "deny"},
	} {
		t.Setenv("WP_TASK", m.task)
		t.Setenv("WP_GATE", m.gate)
		if got := gateMode(); got != m.want {
			t.Errorf("task=%q gate=%q: %s, want %s", m.task, m.gate, got, m.want)
		}
	}
}

func TestTranscriptTail(t *testing.T) {
	big := strings.Repeat(`{"type":"user","message":{"content":"`+strings.Repeat("x", 900)+`"}}`+"\n", 400)
	tr := big +
		`{"type":"assistant","message":{"model":"claude-opus-4","usage":{"input_tokens":5,"cache_read_input_tokens":60000,"cache_creation_input_tokens":1000}}}` + "\n" +
		`{"type":"assistant","isSidechain":true,"message":{"model":"x","usage":{"input_tokens":999999}}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-opus-4","usage":{"input_tokens":10,"cache_read_input_tokens":120000,"cache_creation_input_tokens":500}}}` + "\n"
	ctx, model := transcriptTail(strings.NewReader(tr))
	if ctx != 120510 || model != "claude-opus-4" {
		t.Errorf("ctx=%d model=%q", ctx, model)
	}
}

func TestHookClaudeGate(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GATE", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	c := &cli{dir: t.TempDir()}
	for _, id := range []string{"a", "b", "c", "d"} {
		c.trackSubagent(hookEventInput{SessionID: "s1", AgentID: id, AgentType: "sonnet-worker", HookEventName: "SubagentStart"})
	}
	run := func(in string) string {
		var out bytes.Buffer
		hookClaudeGate(c, strings.NewReader(in), &out)
		return out.String()
	}
	if got := run(`{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"sonnet-worker","model":"sonnet"}}`); got != "" {
		t.Errorf("fifth agent: %s", got)
	}
	if got := run(`{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"sonnet-worker","model":"sonnet"},"agent_id":"a"}`); got != "" {
		t.Errorf("fifth agent, nested: %s", got)
	}
	if got := run(`{"session_id":"s2","tool_name":"Task","tool_input":{"subagent_type":"sonnet-worker","model":"sonnet"},"agent_id":"z"}`); got != "" {
		t.Errorf("nested sonnet via Task: %s", got)
	}
	if got := run(`{"session_id":"s2","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"opus"},"agent_id":"z"}`); !strings.Contains(got, "can't start an agent on Opus") {
		t.Errorf("nested opus: %s", got)
	}
	if got := run(`{"session_id":"s2","tool_name":"Agent","tool_input":{"subagent_type":"haiku-worker","model":"haiku"},"agent_id":"z"}`); got != "" {
		t.Errorf("haiku nested: %s", got)
	}
	if got := run(`{"session_id":"s2","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose"},"model":"claude-opus-4"}`); !strings.Contains(got, "Opus") {
		t.Errorf("inherits Opus: %s", got)
	}
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "sonnet")
	if got := run(`{"session_id":"s2","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose"},"model":"claude-opus-4"}`); got != "" {
		t.Errorf("env default sonnet: %s", got)
	}
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	if got := run(`{"session_id":"s2","tool_name":"Bash","tool_input":{}}`); got != "" {
		t.Errorf("other tool: %s", got)
	}
	c.trackSubagent(hookEventInput{SessionID: "s1", AgentID: "a", HookEventName: "SubagentStop"})
	c.trackSubagent(hookEventInput{SessionID: "s1", AgentID: "e", AgentType: "fork", HookEventName: "SubagentStart"})
	if n := len(c.loadState().Agents["s1"]); n != 4 {
		t.Errorf("tracked %d, want 4", n)
	}
}

// A nested call with no model inherits its caller's model, not the main
// session's: a Sonnet worker starting general-purpose under a Fable session
// is a Sonnet agent.
func TestHookClaudeGateNestedInherits(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GATE", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	c := &cli{dir: t.TempDir()}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tr, []byte(`{"type":"assistant","message":{"model":"claude-fable-5-2","usage":{"input_tokens":10}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.trackSubagent(hookEventInput{SessionID: "n1", AgentID: "w", AgentType: "x", HookEventName: "SubagentStart"})
	_ = c.updateState(func(s *State) {
		a := s.Agents["n1"]["w"]
		a.Model = "sonnet"
		s.Agents["n1"]["w"] = a
	})
	run := func(agentID string) string {
		in, _ := json.Marshal(map[string]any{"session_id": "n1", "tool_name": "Agent", "transcript_path": tr, "cwd": t.TempDir(), "agent_id": agentID, "model": "claude-fable-5-2", "tool_input": json.RawMessage(`{"subagent_type":"general-purpose"}`)})
		var out bytes.Buffer
		hookClaudeGate(c, bytes.NewReader(in), &out)
		return out.String()
	}
	if got := run("w"); got != "" {
		t.Errorf("sonnet worker starting general-purpose: %s", got)
	}
	if got := run(""); !strings.Contains(got, "Fable") {
		t.Errorf("the Fable main session starting general-purpose: %s", got)
	}
}

// A nested no-model call takes its caller's recorded model; with none
// recorded, the session's model stands in.
func TestHookClaudeGateNestedParent(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GATE", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	c := &cli{dir: t.TempDir()}
	c.trackSubagent(hookEventInput{SessionID: "p1", AgentID: "rec", AgentType: "x", HookEventName: "SubagentStart"})
	c.trackSubagent(hookEventInput{SessionID: "p1", AgentID: "unk", AgentType: "x", HookEventName: "SubagentStart"})
	_ = c.updateState(func(s *State) {
		a := s.Agents["p1"]["rec"]
		a.Model = "opus"
		s.Agents["p1"]["rec"] = a
	})
	run := func(agentID, session string, input ...string) string {
		ti := `{"subagent_type":"general-purpose"}`
		if len(input) > 0 {
			ti = input[0]
		}
		tr := filepath.Join(t.TempDir(), "t.jsonl")
		line := `{"type":"assistant","message":{"model":"` + session + `","usage":{"input_tokens":10}}}` + "\n"
		if err := os.WriteFile(tr, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		in, _ := json.Marshal(map[string]any{"session_id": "p1", "tool_name": "Agent", "transcript_path": tr, "cwd": t.TempDir(), "agent_id": agentID, "tool_input": json.RawMessage(ti)})
		var out bytes.Buffer
		hookClaudeGate(c, bytes.NewReader(in), &out)
		return out.String()
	}
	if got := run("rec", "claude-sonnet-5-5"); !strings.Contains(got, "can't start an agent on Opus") {
		t.Errorf("caller recorded as Opus: %s", got)
	}
	if got := run("unk", "claude-fable-5-2"); !strings.Contains(got, "can't start an agent on Fable") {
		t.Errorf("unknown caller under a Fable session: %s", got)
	}
	if got := run("unk", "claude-sonnet-5-5"); got != "" {
		t.Errorf("unknown caller under a Sonnet session: %s", got)
	}
	if got := run("rec", "claude-sonnet-5-5", `{"subagent_type":"general-purpose","model":"sonnet"}`); got != "" {
		t.Errorf("caller recorded as Opus starting a Sonnet agent: %s", got)
	}
	if got := run("unk", "claude-fable-5-2", `{"subagent_type":"general-purpose","model":"opus"}`); !strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "can't start an agent on Opus") {
		t.Errorf("nested Opus call: want a deny: %s", got)
	}
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "sonnet")
	if got := run("unk", "claude-fable-5-2"); got != "" {
		t.Errorf("unknown caller under a Fable session, env sonnet: %s", got)
	}
}

// A definition that says model: inherit says nothing: the call inherits.
func TestHookClaudeGateInheritDefinition(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GATE", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	ad := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(ad, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ad, "i.md"), []byte("---\nname: inheriter\nmodel: Inherit\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &cli{dir: t.TempDir()}
	var out bytes.Buffer
	hookClaudeGate(c, strings.NewReader(`{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"inheriter"},"model":"claude-opus-5-5"}`), &out)
	if !strings.Contains(out.String(), "would run on Opus") {
		t.Errorf("inherit definition under Opus: %q", out.String())
	}
}

// A call that says model: inherit names no model: it runs on what it inherits.
func TestHookClaudeGateInheritCall(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GATE", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	c := &cli{dir: t.TempDir()}
	var out bytes.Buffer
	hookClaudeGate(c, strings.NewReader(`{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"inherit"},"model":"claude-opus-5-5"}`), &out)
	if !strings.Contains(out.String(), "would run on Opus") {
		t.Errorf("inherit call under Opus: %q", out.String())
	}
}

// subagentModel of an agent the gate did not record: its definition's model,
// and for one that says inherit, the env setting (this pins what it does).
func TestSubagentModelInheritDefinition(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ad := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(ad, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ad, "i.md"), []byte("---\nname: inheriter\nmodel: inherit\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &cli{dir: t.TempDir()}
	in := hookEventInput{SessionID: "s1", AgentID: "x", AgentType: "inheriter", Cwd: t.TempDir()}
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "sonnet")
	if got := c.subagentModel(in); got != "sonnet" {
		t.Errorf("env sonnet: got %q", got)
	}
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	if got := c.subagentModel(in); got != "" {
		t.Errorf("no env: got %q", got)
	}
}

func TestResolveModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	ad := filepath.Join(home, ".claude", "agents")
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	rd := filepath.Join(repo, ".claude", "agents")
	for dir, files := range map[string]map[string]string{
		ad: {"h.md": "---\nname: haiku-worker\nmodel: haiku\n---\nbody model: opus\n", "n.md": "---\nname: plain\n---\n", "i.md": "---\nname: inheriter\nmodel: Inherit\n---\n"},
		rd: {"r.md": "---\nname: repo-agent\nmodel: \"opus\"\n---\n"},
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		for n, c := range files {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	dirs := agentDirs(repo)
	if len(dirs) != 2 || dirs[1] != rd {
		t.Fatalf("dirs = %v", dirs)
	}
	for _, c := range []struct{ name, call, agent, parent, env, want string }{
		{"call wins", "haiku", "repo-agent", "opus", "sonnet", "haiku"},
		{"definition", "", "haiku-worker", "opus", "sonnet", "haiku"},
		{"repo definition", "", "repo-agent", "sonnet", "", "opus"},
		{"definition without model falls to env", "", "plain", "opus", "sonnet", "sonnet"},
		{"inherit definition falls to env", "", "inheriter", "opus", "sonnet", "sonnet"},
		{"inherit definition falls to parent", "", "inheriter", "opus", "", "opus"},
		{"built-in uses env", "", "general-purpose", "opus", "sonnet", "sonnet"},
		{"built-in uses parent", "", "Explore", "opus", "", "opus"},
		{"fork uses parent", "haiku", "fork", "opus", "sonnet", "opus"},
		{"inherit call falls to parent", "inherit", "general-purpose", "claude-opus-4", "", "claude-opus-4"},
		{"inherit call falls to definition", " Inherit ", "haiku-worker", "opus", "", "haiku"},
		{"inherit env falls to parent", "", "general-purpose", "claude-fable-5-2", "inherit", "claude-fable-5-2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", c.env)
			if got := resolveModel(c.call, c.agent, c.parent, dirs); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"haiku"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveModel("", "general-purpose", "opus", dirs); got != "haiku" {
		t.Errorf("settings.json env: %q", got)
	}
}

// A fork is refused whatever WP_GATE says: off and warn relax the rules of
// thumb, not this one. Other agents still follow the mode.
func TestHookClaudeGateForkIgnoresMode(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	c := &cli{dir: t.TempDir()}
	run := func(in string) string {
		var out bytes.Buffer
		hookClaudeGate(c, strings.NewReader(in), &out)
		return out.String()
	}
	fork := `{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"fork"},"model":"claude-sonnet-5-5"}`
	opus := `{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose"},"model":"claude-opus-5-5"}`
	for _, mode := range []string{"off", "warn", "deny"} {
		t.Setenv("WP_GATE", mode)
		if got := run(fork); !strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "Forks are never allowed") {
			t.Errorf("WP_GATE=%s fork: %q", mode, got)
		}
	}
	t.Setenv("WP_GATE", "off")
	if got := run(opus); got != "" {
		t.Errorf("off still silences the other rules: %q", got)
	}
	if got := run(`{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"fork"},"model":"claude-opus-5-5"}`); strings.Contains(got, "would run on") {
		t.Errorf("off fork names the model rule: %q", got)
	}
	t.Setenv("WP_GATE", "warn")
	if got := run(opus); strings.Contains(got, "permissionDecision") || !strings.Contains(got, "Opus") {
		t.Errorf("warn only advises on the other rules: %q", got)
	}
}

// A nested premium call follows the mode like a main-thread one; a nested
// fork is refused in every mode.
func TestHookClaudeGateNestedModes(t *testing.T) {
	t.Setenv("WP_TASK", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	c := &cli{dir: t.TempDir()}
	run := func(in string) string {
		var out bytes.Buffer
		hookClaudeGate(c, strings.NewReader(in), &out)
		return out.String()
	}
	opus := `{"session_id":"s1","agent_id":"z","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"opus"}}`
	fork := `{"session_id":"s1","agent_id":"z","tool_name":"Agent","tool_input":{"subagent_type":"fork"},"model":"claude-sonnet-5-5"}`
	t.Setenv("WP_GATE", "warn")
	if got := run(opus); strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "can't start an agent on Opus") {
		t.Errorf("warn, nested opus: %q", got)
	}
	t.Setenv("WP_GATE", "off")
	if got := run(opus); got != "" {
		t.Errorf("off, nested opus: %q", got)
	}
	for _, mode := range []string{"off", "warn", "deny"} {
		t.Setenv("WP_GATE", mode)
		if got := run(fork); !strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "Forks are never allowed") {
			t.Errorf("WP_GATE=%s nested fork: %q", mode, got)
		}
	}
}
