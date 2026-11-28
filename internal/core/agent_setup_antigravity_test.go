package core

import "testing"

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
			"SessionStart": [{"type": "command", "command": "/usr/local/bin/wp hook antigravity-event"}],
			"PreToolUse": [{"matcher": "run_command", "hooks": [{"type": "command", "command": "/usr/local/bin/wp hook antigravity-guard"}]}]
		}
	}`
	comps := settingsHooks(hooksJSON)
	if len(comps) != 2 {
		t.Fatalf("settingsHooks(antigravity) = %+v", comps)
	}
	seen := map[string]bool{}
	for _, c := range comps {
		seen[c.Name] = true
	}
	for _, want := range []string{"SessionStart · wp hook antigravity-event", "PreToolUse run_command · wp hook antigravity-guard"} {
		if !seen[want] {
			t.Errorf("missing component %q in %+v", want, comps)
		}
	}
}
