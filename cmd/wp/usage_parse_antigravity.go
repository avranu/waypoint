package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var markAgyStepIndex = []byte(`"step_index":`)

type agyTranscriptLine struct {
	StepIndex       int    `json:"step_index"`
	Source          string `json:"source"`
	Type            string `json:"type"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	Model           string `json:"model"`
	InputTokens     int64  `json:"input_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ThinkingTokens  int64  `json:"thinking_tokens"`
	ToolCalls       []struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"tool_calls"`
	Error string `json:"error"`
}

// agyConvMeta holds metadata from Antigravity's conversation_summaries.db for
// one conversation so usage analytics can resolve repo, title, and subagents.
type agyConvMeta struct {
	Cwd       string
	Title     string
	ParentID  string
	AgentName string
}

// loadAntigravityConvMeta reads conversation_summaries.db beside brainDir
// strictly read-only. Missing or unreadable DBs return an empty map.
func loadAntigravityConvMeta(brainDir string) map[string]agyConvMeta {
	dbPath := filepath.Join(filepath.Dir(brainDir), "conversation_summaries.db")
	fi, err := os.Lstat(dbPath)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return nil
	}
	dsn := "file:" + filepath.ToSlash(dbPath) + "?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT conversation_id, coalesce(title,''), coalesce(workspace_uris,''), coalesce(parent_conversation_id,''), coalesce(agent_name,'') FROM conversation_summaries`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	out := map[string]agyConvMeta{}
	for rows.Next() {
		var id, title, uris, parent, agent string
		if rows.Scan(&id, &title, &uris, &parent, &agent) != nil || id == "" {
			continue
		}
		out[id] = agyConvMeta{
			Cwd:       firstWorkspacePath(uris),
			Title:     strings.TrimSpace(title),
			ParentID:  strings.TrimSpace(parent),
			AgentName: strings.TrimSpace(agent),
		}
	}
	return out
}

// firstWorkspacePath extracts the first local filesystem path from a JSON
// array of workspace URIs (e.g. `["file:///projects/waypoint"]`).
func firstWorkspacePath(raw string) string {
	var uris []string
	if json.Unmarshal([]byte(raw), &uris) != nil {
		return ""
	}
	for _, u := range uris {
		if strings.HasPrefix(u, "file://") {
			if parsed, err := url.Parse(u); err == nil && parsed.Path != "" {
				return parsed.Path
			}
			return strings.TrimPrefix(u, "file://")
		}
		if filepath.IsAbs(u) {
			return u
		}
	}
	return ""
}

// discoverAntigravityUsageFiles returns one transcript file per Antigravity
// conversation for usage counting (preferring transcript.jsonl over
// transcript_full.jsonl so tokens are never double-counted).
func discoverAntigravityUsageFiles(home string, since time.Duration) []usageFile {
	if home == "" {
		return nil
	}
	cut := time.Time{}
	if since > 0 {
		cut = time.Now().Add(-since)
	}
	var out []usageFile
	seenConv := map[string]bool{}
	for _, brainDir := range antigravityBrainRoots(home) {
		entries, err := os.ReadDir(brainDir)
		if err != nil {
			continue
		}
		meta := loadAntigravityConvMeta(brainDir)
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			convID := ent.Name()
			if seenConv[convID] {
				continue
			}
			logsDir := filepath.Join(brainDir, convID, ".system_generated", "logs")
			chosen := ""
			for _, name := range []string{"transcript.jsonl", "transcript_full.jsonl"} {
				p := filepath.Join(logsDir, name)
				if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
					if cut.IsZero() || fi.ModTime().After(cut) {
						chosen = p
					}
					break
				}
			}
			if chosen == "" {
				continue
			}
			seenConv[convID] = true
			cm := meta[convID]
			sessID := sanitizeTranscriptSession("agy-" + convID)
			kind := "main"
			isMain := true
			if cm.ParentID != "" {
				sessID = sanitizeTranscriptSession("agy-" + cm.ParentID)
				kind = cmpStr(cm.AgentName, "subagent")
				isMain = false
			}
			out = append(out, usageFile{
				Path:    chosen,
				Session: sessID,
				Kind:    kind,
				Harness: "antigravity",
				Cwd:     cm.Cwd,
				Title:   cm.Title,
				Main:    isMain,
			})
		}
	}
	return out
}

// antigravityUsageFileOfPath builds a usageFile for a single Antigravity
// transcript path (e.g. passed via --hook or Files), enriching it with
// conversation_summaries.db metadata when present.
func antigravityUsageFileOfPath(p string) (usageFile, bool) {
	hf, ok := harnessFileOfPath(p)
	if !ok || (hf.Kind != "antigravity" && hf.Kind != "antigravity_compact") {
		return usageFile{}, false
	}
	slash := filepath.ToSlash(p)
	idx := strings.Index(slash, "/brain/")
	if idx < 0 {
		return usageFile{}, false
	}
	brainDir := filepath.FromSlash(slash[:idx+len("/brain")])
	convID, _, _ := strings.Cut(slash[idx+len("/brain/"):], "/")
	cm := loadAntigravityConvMeta(brainDir)[convID]
	sessID := hf.Session
	kind := "main"
	isMain := true
	if cm.ParentID != "" {
		sessID = sanitizeTranscriptSession("agy-" + cm.ParentID)
		kind = cmpStr(cm.AgentName, "subagent")
		isMain = false
	}
	return usageFile{
		Path:    p,
		Session: sessID,
		Kind:    kind,
		Harness: "antigravity",
		Cwd:     cm.Cwd,
		Title:   cm.Title,
		Main:    isMain,
	}, true
}

// unquoteAgyArg handles Antigravity transcript.jsonl's JSON-quoted string arg
// values (e.g. `"\"git status\""` -> `"git status"`).
func unquoteAgyArg(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var u string
		if json.Unmarshal([]byte(s), &u) == nil {
			return u
		}
	}
	return s
}

// normalizeAntigravityTool maps an Antigravity native tool name and args onto
// Waypoint's canonical tool name and JSON input (matching Claude Code's tool
// taxonomy for Bash, Read, Edit, Write, Agent, and MCP analytics).
func normalizeAntigravityTool(name string, args map[string]any) (string, json.RawMessage) {
	argStr := func(keys ...string) string {
		for _, k := range keys {
			if s := unquoteAgyArg(args[k]); s != "" {
				return s
			}
		}
		return ""
	}
	marshal := func(m map[string]any) json.RawMessage {
		b, _ := json.Marshal(m)
		return b
	}
	switch name {
	case "run_command":
		return "Bash", marshal(map[string]any{"command": argStr("CommandLine", "command")})
	case "read_file", "view_file":
		return "Read", marshal(map[string]any{"file_path": argStr("Path", "AbsolutePath", "file_path")})
	case "edit_file", "replace_file_content", "multi_replace_file_content":
		return "Edit", marshal(map[string]any{"file_path": argStr("Path", "TargetFile", "file_path")})
	case "write_file", "write_to_file":
		return "Write", marshal(map[string]any{"file_path": argStr("Path", "TargetFile", "file_path")})
	case "notebook_edit":
		return "NotebookEdit", marshal(map[string]any{"notebook_path": argStr("Path", "TargetFile", "notebook_path")})
	case "grep_search":
		return "Grep", marshal(map[string]any{"pattern": argStr("Query", "pattern")})
	case "find_by_name", "list_dir":
		return "Glob", marshal(map[string]any{"pattern": argStr("Pattern", "SearchDirectory", "DirectoryPath")})
	case "invoke_subagent":
		return "Agent", marshal(map[string]any{
			"subagent_type": argStr("TypeName", "subagent_type"),
			"description":   argStr("Role", "description"),
		})
	case "ask_question":
		return "AskUserQuestion", marshal(args)
	}
	if rest, ok := strings.CutPrefix(name, "mcp_"); ok {
		if server, tool, ok := strings.Cut(rest, "_"); ok && server != "" && tool != "" {
			return "mcp__" + server + "__" + tool, marshal(args)
		}
	}
	return name, marshal(args)
}

// parseAntigravityLine parses one JSONL line of an Antigravity transcript.
func (p *usageParser) parseAntigravityLine(b []byte) {
	p.stats.Lines++
	var l agyTranscriptLine
	if json.Unmarshal(b, &l) != nil {
		p.skip("(unreadable)")
		return
	}
	ts := parseTS(l.CreatedAt)
	if ts > 0 {
		if p.first == 0 || ts < p.first {
			p.first = ts
		}
		p.last = max(p.last, ts)
	}
	if ts == 0 {
		ts = p.last
	}
	switch l.Type {
	case "PLANNER_RESPONSE":
		p.stats.Parsed++
		model := cleanModel(l.Model)
		if model != "" && (l.InputTokens+l.OutputTokens+l.ThinkingTokens+l.CacheReadTokens) > 0 {
			id := fmt.Sprintf("agy-step-%d", l.StepIndex)
			u := usageSeenMsg{
				ID:  id,
				In:  l.InputTokens,
				Out: l.OutputTokens + l.ThinkingTokens,
				CR:  l.CacheReadTokens,
			}
			cur := p.msgs[id]
			if cur == nil {
				cur = &usageMsg{id: id, model: model, ts: ts}
				p.msgs[id] = cur
				p.order = append(p.order, id)
			}
			cur.u = u
		}
		for i, tc := range l.ToolCalls {
			if tc.Name == "" {
				continue
			}
			canonName, rawIn := normalizeAntigravityTool(tc.Name, tc.Args)
			call := classifyCall(canonName, rawIn)
			call.Hour, call.Model = hourOf(ts), model
			p.stats.Calls++
			for _, k := range call.Keys {
				p.tool(call.Hour, call.Model, k).Calls++
			}
			p.noteCLI(call)
			if call.Risky != nil {
				p.addRisky(call, ts, "ran", "")
			}
			_ = i
		}
	case "USER_INPUT", "GENERIC", "SYSTEM_MESSAGE", "EPHEMERAL_MESSAGE", "CHECKPOINT", "CONVERSATION_HISTORY", "COMMAND_STATUS", "ERROR_MESSAGE":
		p.stats.Parsed++
	default:
		if l.Type == "" {
			p.skip("(no type)")
		} else {
			p.stats.Ignored++
		}
	}
}

// isAntigravityTranscriptLine reports whether a raw JSONL line looks like an
// Antigravity transcript step.
func isAntigravityTranscriptLine(line []byte) bool {
	return bytes.Contains(line, markAgyStepIndex)
}
