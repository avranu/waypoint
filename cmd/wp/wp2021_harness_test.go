package main

import "testing"

func TestDetectAgentAndHarness(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		agent   string
		harness string
	}{
		{"claude", map[string]string{"CLAUDECODE": "1"}, "anthropic/claude-code", "claude"},
		{"antigravity", map[string]string{"ANTIGRAVITY_AGENT": "1"}, "google/antigravity", "antigravity"},
		{"gemini-cli", map[string]string{"GEMINI_CLI": "1"}, "google/gemini-cli", "gemini-cli"},
		{"opencode", map[string]string{"OPENCODE": "1"}, "opencode", "opencode"},
		{"codex", map[string]string{"CODEX_SANDBOX": "seatbelt"}, "openai/codex", "codex"},
		{"none", map[string]string{}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if got := detectAgent(getenv); got != tc.agent {
				t.Errorf("detectAgent = %q, want %q", got, tc.agent)
			}
			if got := detectHarness(getenv); got != tc.harness {
				t.Errorf("detectHarness = %q, want %q", got, tc.harness)
			}
		})
	}
}

func TestHarnessSessionID(t *testing.T) {
	t.Run("t3 thread id", func(t *testing.T) {
		getenv := func(k string) string {
			return map[string]string{
				"ANTIGRAVITY_AGENT": "1",
				"T3CODE_THREAD_ID":  "thr_abc-123",
			}[k]
		}
		id, cli := harnessSessionID(getenv, "/tmp/repo")
		if id != "antigravity-thr_abc-123" || cli != "antigravity" {
			t.Fatalf("got (%q, %q)", id, cli)
		}
	})
	t.Run("cwd fallback is deterministic", func(t *testing.T) {
		getenv := func(k string) string { return map[string]string{"CODEX_SANDBOX": "1"}[k] }
		id1, cli1 := harnessSessionID(getenv, "/work/project-a")
		id2, cli2 := harnessSessionID(getenv, "/work/project-a")
		id3, _ := harnessSessionID(getenv, "/work/project-b")
		if id1 == "" || id1 != id2 || cli1 != "codex" || cli2 != "codex" {
			t.Fatalf("got (%q,%q) and (%q,%q)", id1, cli1, id2, cli2)
		}
		if id1 == id3 {
			t.Fatalf("different cwds produced same id %q", id1)
		}
	})
	t.Run("claude stays on hooks", func(t *testing.T) {
		getenv := func(k string) string {
			return map[string]string{"CLAUDECODE": "1", "T3CODE_THREAD_ID": "thr_1"}[k]
		}
		if id, cli := harnessSessionID(getenv, "/tmp/repo"); id != "" || cli != "" {
			t.Fatalf("claude should not use harness fallback: got (%q, %q)", id, cli)
		}
	})
}
