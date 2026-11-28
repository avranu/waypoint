package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAntigravityHooksLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	existing := `{"user-safety":{"PreToolUse":[{"matcher":"run_command","hooks":[{"type":"command","command":"./check.sh"}]}]}}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	// First install adds "waypoint" and preserves "user-safety" + writes backup.
	res, err := installAntigravityHooks(path, hookInstallOpts{exe: "/opt/bin/wp"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.changed || res.present || res.backup == "" {
		t.Fatalf("first install = %+v", res)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if root["user-safety"] == nil || root["waypoint"] == nil {
		t.Fatalf("missing keys: %+v", root)
	}
	if !strings.Contains(string(raw), "/opt/bin/wp hook antigravity-event") || !strings.Contains(string(raw), "antigravity-guard") {
		t.Errorf("unexpected hooks.json:\n%s", raw)
	}

	// Second install is a no-op.
	res2, err := installAntigravityHooks(path, hookInstallOpts{exe: "/opt/bin/wp", keepNoPrompts: true})
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if res2.changed || !res2.present {
		t.Fatalf("second install should be unchanged: %+v", res2)
	}

	// Install with --no-prompts updates PreInvocation command.
	res3, err := installAntigravityHooks(path, hookInstallOpts{exe: "/opt/bin/wp", noPrompts: true})
	if err != nil || !res3.changed {
		t.Fatalf("no-prompts update = %+v, %v", res3, err)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), "antigravity-event --no-prompts") {
		t.Errorf("missing --no-prompts:\n%s", raw)
	}

	// Uninstall removes only "waypoint", keeping "user-safety".
	res4, err := installAntigravityHooks(path, hookInstallOpts{exe: "/opt/bin/wp", uninstall: true})
	if err != nil || !res4.changed {
		t.Fatalf("uninstall = %+v, %v", res4, err)
	}
	raw, _ = os.ReadFile(path)
	root = nil
	_ = json.Unmarshal(raw, &root)
	if root["user-safety"] == nil || root["waypoint"] != nil {
		t.Fatalf("after uninstall: %+v", root)
	}
}

func TestAntigravityHookPayloadAndReplyTranslation(t *testing.T) {
	idle := true
	in := antigravityHookInput{
		ConversationID: "conv-42",
		WorkspacePaths: []string{"/work/repo"},
		TranscriptPath: "/tmp/transcript.jsonl",
		ModelName:      "gemini-4-neon-high",
		LastUserInput:  "fix the bug",
		HookEventName:  "PreInvocation",
		FullyIdle:      &idle,
		ToolCall: &struct {
			ID   string         `json:"id"`
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}{
			ID:   "call-1",
			Name: "run_command",
			Args: map[string]any{"CommandLine": "sudo rm -rf /"},
		},
	}
	got := in.toHookEventInput()
	if got.SessionID != "conv-42" || got.Cwd != "/work/repo" || got.HookEventName != "UserPromptSubmit" || got.Harness != "antigravity" {
		t.Fatalf("toHookEventInput = %+v", got)
	}
	if got.ToolName != "Bash" || !strings.Contains(string(got.ToolInput), "sudo rm -rf /") {
		t.Fatalf("tool = %q %s", got.ToolName, got.ToolInput)
	}

	// Translate PreToolUse deny.
	deny := translateClaudeToAgyReply("PreToolUse", preToolReply("deny", "needs a person", ""))
	var denyOut map[string]any
	if err := json.Unmarshal(deny, &denyOut); err != nil || denyOut["decision"] != "deny" || denyOut["reason"] != "needs a person" {
		t.Errorf("PreToolUse deny = %s (%v)", deny, err)
	}

	// Translate SessionStart context injection.
	start := translateClaudeToAgyReply("SessionStart", hookContext("SessionStart", "hello from wp"))
	if !strings.Contains(string(start), `"injectSteps"`) || !strings.Contains(string(start), "hello from wp") {
		t.Errorf("SessionStart = %s", start)
	}

	// Translate Stop block -> continue.
	stop := translateClaudeToAgyReply("Stop", stopOutput("check queued message", ""))
	var stopOut map[string]any
	if err := json.Unmarshal(stop, &stopOut); err != nil || stopOut["decision"] != "continue" || stopOut["reason"] != "check queued message" {
		t.Errorf("Stop = %s (%v)", stop, err)
	}

	// Verify guard blocks risky command in unattended Antigravity session.
	t.Setenv("WAYPOINT_UNATTENDED", "1")
	c := &cli{dir: t.TempDir()}
	var out bytes.Buffer
	payload, _ := json.Marshal(antigravityHookInput{
		ConversationID: "conv-guard-1",
		Cwd:            t.TempDir(),
		HookEventName:  "PreToolUse",
		ToolCall: &struct {
			ID   string         `json:"id"`
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}{
			ID:   "call-9",
			Name: "run_command",
			Args: map[string]any{"CommandLine": "sudo apt-get install curl"},
		},
	})
	hookAntigravityGuard(c, bytes.NewReader(payload), &out)
	var guardRes map[string]any
	if err := json.Unmarshal(out.Bytes(), &guardRes); err != nil || guardRes["decision"] != "deny" {
		t.Errorf("guard output = %s (%v)", out.String(), err)
	}

	// Verify PreInvocation prompt deduplication.
	if !c.isNewAgyPrompt("conv-42", "first prompt") {
		t.Error("first prompt should be new")
	}
	if c.isNewAgyPrompt("conv-42", "first prompt") {
		t.Error("same prompt repeated should not be new")
	}
	if !c.isNewAgyPrompt("conv-42", "second prompt") {
		t.Error("changed prompt should be new")
	}
}
