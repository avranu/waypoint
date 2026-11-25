package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A subagent transcript as Claude Code writes it: the prompt, a system
// reminder, tool calls, text, then the report handed back.
const reviewTranscript = `{"type":"user","message":{"role":"user","content":"Attack the diff in WP-9: the nil map in Save."}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder> Your final report is delivered through SubagentHandback </system-reminder>"}]}}
{"type":"assistant","message":{"model":"claude-sonnet-5-5","content":[{"type":"text","text":"Reading the diff."},{"type":"tool_use","name":"Read","input":{"file_path":"x.go"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"package x"}]}}
{"type":"system","message":"a line of another shape"}
{"type":"assistant","message":{"model":"claude-sonnet-5-5","content":[{"type":"tool_use","name":"SubagentHandback","input":{"message":"1. x.go:12 Save writes to a nil map: panics on first call."}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}
`

func writeSubagent(t *testing.T, meta, transcript string) (hookEventInput, string) {
	t.Helper()
	dir := t.TempDir()
	session := filepath.Join(dir, "sess.jsonl")
	sub := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "agent-a1.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	if meta != "" {
		if err := os.WriteFile(filepath.Join(sub, "agent-a1.meta.json"), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return hookEventInput{HookEventName: "SubagentStop", TranscriptPath: session, AgentID: "a1", LastMessage: "last words"}, filepath.Join(sub, "agent-a1.jsonl")
}

func TestOwnReviewFromTranscript(t *testing.T) {
	in, _ := writeSubagent(t, `{"agentType":"adversarial-review","description":"Adversarial review of WP-9"}`, reviewTranscript)
	var ev sessionEvent
	ownReview(in, &ev, false)
	if ev.AgentReport != "1. x.go:12 Save writes to a nil map: panics on first call." {
		t.Fatalf("report = %q", ev.AgentReport)
	}
	if ev.AgentPrompt != "Attack the diff in WP-9: the nil map in Save." || ev.AgentModel != "claude-sonnet-5-5" ||
		ev.AgentType != "adversarial-review" || ev.AgentDescription != "Adversarial review of WP-9" {
		t.Fatalf("event = %+v", ev)
	}

	ev = sessionEvent{}
	ownReview(in, &ev, true)
	if ev.AgentReport == "" || ev.AgentPrompt != "" || ev.AgentDescription != "" || ev.AgentType != "adversarial-review" {
		t.Fatalf("no prompts: %+v", ev)
	}
}

func TestOwnReviewWhich(t *testing.T) {
	plain := `{"type":"user","message":{"content":"look"}}
{"type":"assistant","message":{"model":"claude-haiku-5-5","content":[{"type":"text","text":"No issues found."}]}}
`
	in, _ := writeSubagent(t, `{"agentType":"general-purpose","description":"Adversarial review of the gate"}`, plain)
	in.LastMessage = ""
	var ev sessionEvent
	ownReview(in, &ev, false)
	if ev.AgentReport != "No issues found." || ev.AgentType != "general-purpose" {
		t.Fatalf("described review: %+v", ev)
	}

	midway := `{"type":"user","message":{"content":"look"}}
{"type":"assistant","message":{"model":"claude-haiku-5-5","content":[{"type":"text","text":"Reading the diff."},{"type":"tool_use","name":"Read","input":{}}]}}
`
	in, _ = writeSubagent(t, `{"agentType":"adversarial-review","description":"Review WP-9"}`, midway)
	in.LastMessage = "1. x.go:3 broken."
	ev = sessionEvent{}
	ownReview(in, &ev, false)
	if ev.AgentReport != "1. x.go:3 broken." {
		t.Fatalf("last message: %+v", ev)
	}

	in, _ = writeSubagent(t, `{"agentType":"general-purpose","description":"Adversarial test cases for the parser"}`, plain)
	ev = sessionEvent{}
	ownReview(in, &ev, false)
	if ev.AgentReport != "" {
		t.Fatalf("test writer sent %+v", ev)
	}

	in, _ = writeSubagent(t, `{"agentType":"Explore","description":"Find the gate"}`, plain)
	ev = sessionEvent{}
	ownReview(in, &ev, false)
	if ev.AgentReport != "" || ev.AgentPrompt != "" {
		t.Fatalf("explore sent %+v", ev)
	}

	in, path := writeSubagent(t, "", reviewTranscript+`{"type":"assistant","message":{"content":[{"type":"te`)
	in.TranscriptPath, in.AgentID, in.AgentType, in.AgentTranscript = "", "", "adversarial-review", path
	ev = sessionEvent{}
	ownReview(in, &ev, false)
	if !strings.Contains(ev.AgentReport, "nil map") {
		t.Fatalf("given path: %+v", ev)
	}

	in = hookEventInput{HookEventName: "SubagentStop", AgentType: "adversarial-review", AgentID: "zz", TranscriptPath: "/nonexistent/s.jsonl", LastMessage: "Found 2 issues."}
	ev = sessionEvent{}
	ownReview(in, &ev, false)
	if ev.AgentReport != "Found 2 issues." {
		t.Fatalf("last message: %+v", ev)
	}
}
