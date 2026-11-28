package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAntigravityTranscriptParseAndDiscovery(t *testing.T) {
	home := t.TempDir()
	brain := filepath.Join(home, ".gemini", "antigravity-cli", "brain")
	convID := "conv-agy-test-1"
	logsDir := filepath.Join(brain, convID, ".system_generated", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed conversation_summaries.db with workspace URI and title.
	dbPath := filepath.Join(filepath.Dir(brain), "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY,
		title TEXT,
		workspace_uris TEXT,
		parent_conversation_id TEXT,
		agent_name TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?,?,?,?,?)`,
		convID, "Fix Antigravity sync", `["file:///work/waypoint"]`, "", ""); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	compact := filepath.Join(logsDir, "transcript.jsonl")
	full := filepath.Join(logsDir, "transcript_full.jsonl")
	lines := strings.Join([]string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-28T03:00:00Z","content":"hi"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-28T03:01:00Z","model":"gemini-4-neon-high","input_tokens":1000,"cache_read_tokens":500,"output_tokens":100,"thinking_tokens":50,"tool_calls":[{"name":"run_command","args":{"CommandLine":"\"git status && wp ls\""}},{"name":"read_file","args":{"Path":"\"/work/waypoint/AGENTS.md\""}},{"name":"mcp_t3-code_pull_request_list","args":{}}]}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-28T03:01:05Z","content":"ok"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(compact, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	// Also create transcript_full.jsonl to verify discovery picks only one file per conversation.
	if err := os.WriteFile(full, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTIGRAVITY_APPDATA_DIR", "")
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", "")

	files := discoverAntigravityUsageFiles(home, 0)
	if len(files) != 1 {
		t.Fatalf("discovered %d files, want 1: %+v", len(files), files)
	}
	f := files[0]
	if f.Path != compact || f.Session != "agy-"+convID || f.Harness != "antigravity" || f.Cwd != "/work/waypoint" || f.Title != "Fix Antigravity sync" || !f.Main {
		t.Fatalf("file = %+v", f)
	}

	parsed, err := readUsageFile(f, usageFileState{}, func(cwd string) string {
		if cwd == "/work/waypoint" {
			return "Jess/waypoint"
		}
		return ""
	}, true)
	if err != nil {
		t.Fatalf("readUsageFile: %v", err)
	}
	if parsed.Stats.Skipped != 0 {
		t.Errorf("skipped = %d (unknown: %+v)", parsed.Stats.Skipped, parsed.Stats.Unknown)
	}
	if len(parsed.Chunk.Messages) != 1 {
		t.Fatalf("messages = %+v", parsed.Chunk.Messages)
	}
	m := parsed.Chunk.Messages[0]
	if m.Model != "gemini-4-neon-high" || m.Input != 1000 || m.Output != 150 || m.CR != 500 || m.Repo != "Jess/waypoint" {
		t.Errorf("message row = %+v", m)
	}
	if len(parsed.Chunk.Sessions) != 1 || parsed.Chunk.Sessions[0].Title != "Fix Antigravity sync" || parsed.Chunk.Sessions[0].Repo != "Jess/waypoint" {
		t.Errorf("sessions = %+v", parsed.Chunk.Sessions)
	}
	// Verify tool normalization: Bash (with git + wp heads), Read, and MCP.
	sawTool := map[string]bool{}
	for _, tr := range parsed.Chunk.Tools {
		sawTool[tr.Kind+":"+tr.Tool+":"+tr.Detail] = true
	}
	for _, want := range []string{"builtin:Bash:", "bash:Bash:git status", "bash:Bash:wp ls", "builtin:Read:", "mcp:mcp__t3-code__pull_request_list:t3-code"} {
		if !sawTool[want] {
			t.Errorf("missing tool row %q; got %+v", want, parsed.Chunk.Tools)
		}
	}

	// Verify readTranscriptUsage also reads Antigravity tokens for task usage reporting.
	fh, err := os.Open(compact)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	tu, err := readTranscriptUsage(fh, 0, nil, time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("readTranscriptUsage: %v", err)
	}
	if tu.input != 1000 || tu.output != 150 || tu.model != "gemini-4-neon-high" || tu.context != 1500 {
		t.Errorf("transcript usage = %+v", tu)
	}
}
