package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGrokCutShort(t *testing.T) {
	const prompt = ` -p --mode ask --trust --model grok-4.7-high "$(cat prompt.md)" > out.md 2>&1`
	cases := []struct {
		name   string
		cmd    string
		bg     bool
		toolMs float64
		want   string // in the refusal; "" for none
	}{
		// The runs behind WP-758.
		{"foreground at the Bash cap", "S=/x; cursor-agent" + prompt + "; echo exit=$?", false, 600000, "10 minutes"},
		{"foreground under timeout 580", "cd /r && timeout 580 agent" + prompt, false, 600000, "10 minutes"},
		{"background under timeout 600", "cd /r && timeout 600 cursor-agent" + prompt, true, 0, "10 minutes"},
		{"background under timeout 1500", "timeout 1500 cursor-agent --print --trust --model grok-4.7-high x", true, 0, "25 minutes"},

		{"foreground, no limit named", "cursor-agent" + prompt, false, 0, "2 minutes"},
		{"model with equals", "timeout 900 cursor-agent -p --model=grok-4.7-medium x", true, 0, "15 minutes"},
		{"minutes suffix", "timeout 15m cursor-agent" + prompt, true, 0, "15 minutes"},
		{"timeout flags", "timeout -k 5 900 cursor-agent" + prompt, true, 0, "15 minutes"},
		{"wrapped", "nohup env -i PATH=/bin timeout 900 /usr/local/bin/cursor-agent" + prompt, true, 0, "15 minutes"},
		{"in a substitution", "out=$(timeout 900 cursor-agent" + prompt + ")", true, 0, "15 minutes"},

		// The shell timeout doesn't apply first: Bash's default stops it at 2 minutes.
		{"foreground timeout over Bash's default", "timeout 900 cursor-agent" + prompt, false, 0, "2 minutes"},
		{"foreground timeout 301", "timeout 301 cursor-agent" + prompt, false, 0, "2 minutes"},
		{"quick check, Bash's default shorter", "timeout 300 cursor-agent" + prompt, false, 0, ""},

		// Started with &: the Bash call doesn't wait, so only a shell timeout counts.
		{"detached", "nohup cursor-agent" + prompt + " &", false, 600000, ""},
		{"detached then more", "setsid nohup cursor-agent" + prompt + " &\necho started", false, 0, ""},
		{"detached group", "(cd /r; cursor-agent" + prompt + ") &", false, 0, ""},
		{"detached group with a redirect", "(cursor-agent -p --model grok-4.7-high x) > o 2>&1 &", false, 0, ""},
		{"detached pipeline", "cd /r && cursor-agent -p --model grok-4.7-high x | tee o &", false, 0, ""},
		{"detached and-list", "cursor-agent" + prompt + " && echo done &", false, 0, ""},
		{"detached under a short timeout", "timeout 600 cursor-agent" + prompt + " &", false, 0, "10 minutes"},
		{"detached, then waited for", "cursor-agent" + prompt + " &\ncursor-agent" + prompt + " &\nwait", false, 600000, "10 minutes"},
		{"a later command is detached, not this one", "cursor-agent" + prompt + "; sleep 1 &", false, 600000, "10 minutes"},
		{"a redirect isn't a detach", "cursor-agent -p --model grok-4.7-high x &> o", false, 600000, "10 minutes"},

		{"a quick check doesn't excuse a review", "timeout 60 cursor-agent -p --model grok-4.7-high ok; timeout 600 cursor-agent" + prompt, true, 0, "10 minutes"},

		{"background, no limit", "cursor-agent" + prompt, true, 0, ""},
		{"background, tool timeout ignored", "agent" + prompt, true, 600000, ""},
		{"background, long enough", "timeout 1800 cursor-agent" + prompt, true, 0, ""},
		{"timeout 0 is no limit", "timeout 0 cursor-agent" + prompt, true, 0, ""},
		{"quick check", `timeout 120 cursor-agent -p --mode ask --trust --model grok-4.7-high "Reply OK"`, false, 0, ""},
		{"quick check by tool timeout", `cursor-agent -p --model grok-4.7-high "Reply OK"`, false, 60000, ""},
		{"not print mode", "cursor-agent --model grok-4.7-high", false, 0, ""},
		{"another model", "timeout 900 cursor-agent -p --model gpt-5 grok", true, 0, ""},
		{"listing models", "cursor-agent models | grep -i grok", false, 0, ""},
		{"mentioned in text", `echo "timeout 580 cursor-agent -p --model grok-4.7-high"`, false, 0, ""},
		{"in a heredoc", "cat > n.md <<'E'\ntimeout 580 cursor-agent -p --model grok-4.7-high x\nE", false, 0, ""},
		{"grep for it", `ps aux | grep "cursor-agent -p --model grok"`, false, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := grokCutShort(c.cmd, c.bg, c.toolMs)
			if c.want == "" && got != "" {
				t.Fatalf("refused: %q", got)
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("got %q, want it to say %q", got, c.want)
			}
			if c.want != "" && (!strings.Contains(got, "wp audit") || strings.Contains(got, "\n")) {
				t.Fatalf("refusal should be one line naming wp audit: %q", got)
			}
		})
	}
}

// Through the hook: a deny, for a sub-agent as well, and a fractional timeout
// is still read.
func TestGrokCutShortHook(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WP_GUARD", "")
	const cmd = "timeout 580 agent -p --model grok-4.7-high x"
	hook := func(agentID string, timeout any) string {
		in, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": t.TempDir(), "agent_id": agentID,
			"tool_input": map[string]any{"command": cmd, "timeout": timeout}})
		var out bytes.Buffer
		hookClaudeGuard(&cli{}, bytes.NewReader(in), &out)
		return out.String()
	}
	for _, agent := range []string{"", "agent-1"} {
		if out := hook(agent, 600000); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "wp audit") {
			t.Errorf("agent %q: output %q", agent, out)
		}
	}
	if out := hook("", 600000.5); !strings.Contains(out, "deny") {
		t.Errorf("a fractional timeout: output %q", out)
	}
}
