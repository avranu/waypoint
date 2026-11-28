package core

import (
	"strings"
	"testing"
)

func TestAgentSetupAntigravityFiles(t *testing.T) {
	cases := map[string]string{
		"GEMINI.md":                           fileInstructions,
		".agents/hooks.json":                  fileSettings,
		".agents/rules/style.md":              fileInstructions,
		".agents/skills/review/SKILL.md":      fileSkill,
		".agents/agents/helper.md":            fileAgent,
		".gemini/GEMINI.md":                   fileInstructions,
		".gemini/config/hooks.json":           fileSettings,
		".gemini/config/mcp_config.json":      fileMCP,
		".gemini/config/rules/global.md":      fileInstructions,
		".gemini/config/skills/tidy/SKILL.md": fileSkill,
		".gemini/config/agents/reviewer.md":   fileAgent,
	}
	for p, want := range cases {
		if got := machineFileKind(p); got != want {
			t.Errorf("machineFileKind(%q) = %q, want %q", p, got, want)
		}
	}
	hooksJSON := `{
		"waypoint": {
			"SessionStart": [{"type": "command", "command": "API_TOKEN=secret123 /usr/local/bin/wp hook antigravity-event"}],
			"PreToolUse": [{"matcher": "run_command", "hooks": [{"type": "command", "command": "/usr/local/bin/wp hook antigravity-guard"}]}]
		},
		"auth-gate": {
			"PreToolUse": [{"type": "command", "command": "/usr/local/bin/check-auth", "headers": {"Authorization": "Bearer secret-tok"}}]
		},
		"disabled-group": {
			"enabled": false,
			"Stop": [{"type": "command", "command": "/usr/local/bin/should-not-appear"}]
		}
	}`
	red, ok := redactAntigravityHooks(hooksJSON)
	if !ok {
		t.Fatal("redactAntigravityHooks failed")
	}
	for _, secret := range []string{"secret123", "secret-tok"} {
		if strings.Contains(red, secret) {
			t.Errorf("redactAntigravityHooks leaked %q:\n%s", secret, red)
		}
	}
	if !strings.Contains(red, "wp hook antigravity-event") || !strings.Contains(red, "check-auth") {
		t.Errorf("redactAntigravityHooks wiped SessionStart or auth-gate:\n%s", red)
	}
	comps := settingsHooks(red)
	if len(comps) != 3 {
		t.Fatalf("settingsHooks(antigravity) = %+v", comps)
	}
	seen := map[string]bool{}
	for _, c := range comps {
		seen[c.Name] = true
	}
	for _, want := range []string{
		"SessionStart · wp hook antigravity-event",
		"PreToolUse run_command · wp hook antigravity-guard",
		"PreToolUse · check-auth",
	} {
		if !seen[want] {
			t.Errorf("missing component %q in %+v", want, comps)
		}
	}
	if seen["Stop · should-not-appear"] {
		t.Errorf("disabled hook group should be skipped: %+v", comps)
	}
}
