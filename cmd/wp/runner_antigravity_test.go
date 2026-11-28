package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAntigravityDriver(t *testing.T) {
	work := filepath.Join(t.TempDir(), "job1-work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	schema := `{"type":"object","properties":{"ok":{"type":"boolean"}}}`
	d, err := antigravityDriver(agentJob{
		CLI:        "antigravity",
		Model:      "gemini-4-neon-high",
		Effort:     "medium",
		Permission: "read",
		JSONSchema: schema,
	}, "-leading-dash prompt", work)
	if err != nil {
		t.Fatalf("antigravityDriver: %v", err)
	}
	if !slices.Contains(d.args, "--output-format") || !slices.Contains(d.args, "stream-json") {
		t.Errorf("args missing stream-json: %v", d.args)
	}
	if !slices.Contains(d.args, "--model") || !slices.Contains(d.args, "gemini-4-neon-high") {
		t.Errorf("args missing model: %v", d.args)
	}
	if !slices.Contains(d.args, "--effort") || !slices.Contains(d.args, "medium") {
		t.Errorf("args missing effort: %v", d.args)
	}
	if !slices.Contains(d.args, "--mode") || !slices.Contains(d.args, "plan") || !slices.Contains(d.args, "--sandbox") {
		t.Errorf("read mode args: %v", d.args)
	}
	if !slices.Contains(d.args, "--print=-leading-dash prompt") {
		t.Errorf("prompt not attached via --print=: %v", d.args)
	}
	out := newLogSink()
	lines := []string{
		`{"event":"init","conversation_id":"conv-agy-1","init":{"model":"gemini-4-neon-high"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-agy-1","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"{\"ok\":true}"}}`,
		`{"event":"result","result":{"conversation_id":"conv-agy-1","status":"SUCCESS","response":"{\"ok\":true}\n","structured_output":{"ok":true},"usage":{"input_tokens":1000,"output_tokens":20,"thinking_tokens":30,"cache_read_tokens":500}}}`,
	}
	for _, l := range lines {
		d.line([]byte(l), out)
	}
	if d.sessionID != "conv-agy-1" {
		t.Errorf("sessionID = %q", d.sessionID)
	}
	ans, structured, tok, err := d.finish(nil)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if strings.TrimSpace(ans) != `{"ok":true}` {
		t.Errorf("answer = %q", ans)
	}
	var got struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(structured, &got); err != nil || !got.OK {
		t.Errorf("structured = %s (%v)", structured, err)
	}
	if tok.Input != 1000 || tok.Output != 50 || tok.CacheRead != 500 {
		t.Errorf("tokens = %+v", tok)
	}
}

func TestAntigravityDriverErrorAndSizeLimit(t *testing.T) {
	work := filepath.Join(t.TempDir(), "job2-work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := antigravityDriver(agentJob{CLI: "antigravity"}, strings.Repeat("a", 101<<10), work); err == nil {
		t.Error("expected error for oversized prompt")
	}
	d, err := antigravityDriver(agentJob{CLI: "antigravity", Permission: "edit_run"}, "hi", work)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(d.args, "auto") {
		t.Errorf("edit_run should use --mode auto: %v", d.args)
	}
	out := newLogSink()
	d.line([]byte(`{"event":"result","result":{"status":"ERROR","error":"quota exceeded"}}`), out)
	if _, _, _, err := d.finish(nil); err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("expected error with quota exceeded, got %v", err)
	}
}
