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
		// Repeated step_index update must not double-count tool calls or tokens.
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-28T03:01:01Z","model":"gemini-4-neon-high","input_tokens":1000,"cache_read_tokens":500,"output_tokens":100,"thinking_tokens":50,"tool_calls":[{"name":"run_command","args":{"CommandLine":"\"git status && wp ls\""}},{"name":"read_file","args":{"Path":"\"/work/waypoint/AGENTS.md\""}},{"name":"mcp_t3-code_pull_request_list","args":{}}]}`,
		// Risky command must be recorded with outcome "unknown" (not "ran").
		`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-28T03:01:03Z","model":"gemini-4-neon-high","tool_calls":[{"name":"run_command","args":{"CommandLine":"\"sudo rm -rf /tmp/test\""}}]}`,
		`{"step_index":3,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-28T03:01:05Z","content":"ok"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(compact, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	// Also create transcript_full.jsonl to verify discovery & path canonicalization pick only one file.
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
	if f.Path != compact || f.Session != convID || f.Harness != "antigravity" || f.Cwd != "/work/waypoint" || f.Title != "Fix Antigravity sync" || !f.Main {
		t.Fatalf("file = %+v", f)
	}
	// Path lookup on transcript_full.jsonl canonicalizes to sibling transcript.jsonl with raw convID.
	if af, ok := antigravityUsageFileOfPath(full); !ok || af.Path != compact || af.Session != convID {
		t.Fatalf("antigravityUsageFileOfPath(full) = %+v, %v", af, ok)
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
	if len(parsed.messageIDs) != 1 || parsed.messageIDs[0] != convID+":step-1" {
		t.Errorf("messageIDs = %v, want [%s:step-1]", parsed.messageIDs, convID)
	}
	m := parsed.Chunk.Messages[0]
	if m.Model != "gemini-4-neon-high" || m.Input != 1000 || m.Output != 150 || m.CR != 500 || m.Repo != "Jess/waypoint" {
		t.Errorf("message row = %+v", m)
	}
	if len(parsed.Chunk.Sessions) != 1 || parsed.Chunk.Sessions[0].Session != convID || parsed.Chunk.Sessions[0].Title != "Fix Antigravity sync" || parsed.Chunk.Sessions[0].Repo != "Jess/waypoint" {
		t.Errorf("sessions = %+v", parsed.Chunk.Sessions)
	}
	// Verify tool normalization and deduplication across repeated step_index updates.
	toolCalls := map[string]int64{}
	for _, tr := range parsed.Chunk.Tools {
		toolCalls[tr.Kind+":"+tr.Tool+":"+tr.Detail] += tr.Calls
	}
	for want, wantN := range map[string]int64{
		"builtin:Bash:":        2, // step 1 + step 2 (not duplicated!)
		"bash:Bash:git status": 1,
		"bash:Bash:wp ls":      1,
		"builtin:Read:":        1,
		"mcp:mcp__t3-code__pull_request_list:t3-code": 1,
	} {
		if toolCalls[want] != wantN {
			t.Errorf("tool %q calls = %d, want %d (all: %+v)", want, toolCalls[want], wantN, parsed.Chunk.Tools)
		}
	}
	if len(parsed.Chunk.Risky) != 1 || parsed.Chunk.Risky[0].Outcome != "unknown" {
		t.Errorf("risky = %+v, want 1 row with outcome unknown", parsed.Chunk.Risky)
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

	// Verify burnTree parses Antigravity PLANNER_RESPONSE lines and deduplicates by step_index.
	bt := newBurnTree()
	bt.update([]burnFile{{path: compact, id: "main"}}, burnFallbackPrices)
	tot := bt.totals()
	if tot.Calls != 1 || tot.Cost <= 0 || bt.Agents["main"].Model != "gemini-4-neon-high" || bt.Agents["main"].CtxMax != 1500 {
		t.Errorf("burnTree = %+v, agent = %+v", tot, bt.Agents["main"])
	}

	// Verify Claude lines containing nested `"step_index":` are never dropped by readUsageFile or readTranscriptUsage.
	claudeFile := filepath.Join(t.TempDir(), "claude-sess.jsonl")
	claudeLine := `{"type":"assistant","timestamp":"2026-11-28T03:02:00Z","message":{"id":"msg_c1","model":"claude-opus-5-5","content":[{"type":"text","text":"{\"step_index\":1}"}],"usage":{"input_tokens":200,"output_tokens":40}}}` + "\n"
	if err := os.WriteFile(claudeFile, []byte(claudeLine), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := readUsageFile(usageFile{Path: claudeFile, Session: "claude-sess", Kind: "main", Main: true}, usageFileState{}, nil, false)
	if err != nil || len(cp.Chunk.Messages) != 1 || cp.Chunk.Messages[0].Input != 200 {
		t.Fatalf("claude line with nested step_index dropped: %+v, err=%v", cp, err)
	}
	cfh, _ := os.Open(claudeFile)
	defer func() { _ = cfh.Close() }()
	ctu, err := readTranscriptUsage(cfh, 0, nil, time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC))
	if err != nil || ctu.input != 200 || ctu.output != 40 {
		t.Fatalf("readTranscriptUsage dropped claude line with nested step_index: %+v, err=%v", ctu, err)
	}
}
