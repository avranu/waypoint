package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxLine is the longest line read; a longer one is counted as "(too long)".
const MaxLine = 16 << 20

// Clips: what a fact keeps of free text.
const (
	targetRunes  = 200
	commandWords = 6
	commandRunes = 100
	patternRunes = 80
	purposeRunes = 200
	errorRunes   = 120
)

type usage struct {
	Input         *int64 `json:"input_tokens"`
	Output        *int64 `json:"output_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	Details       *struct {
		Thinking *int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	CC *struct {
		E5m *int64 `json:"ephemeral_5m_input_tokens"`
		E1h *int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

type rec struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	Timestamp      string `json:"timestamp"`
	UUID           string `json:"uuid"`
	Sidechain      bool   `json:"isSidechain"`
	AgentID        string `json:"agentId"`
	PromptID       string `json:"promptId"`
	RequestID      string `json:"requestId"`
	RequestRef     string `json:"requestRef"`
	RequestedModel string `json:"requestedModel"`
	Effort         string `json:"effort"`
	PerTurnEffort  string `json:"perTurnEffort"`
	GitBranch      string `json:"gitBranch"`
	Cwd            string `json:"cwd"`
	Version        string `json:"version"`
	IsMeta         bool   `json:"isMeta"`
	IsCompact      bool   `json:"isCompactSummary"`
	TurnOrigin     string `json:"turnOrigin"`
	Origin         *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	PromptSource   string `json:"promptSource"`
	PermissionMode string `json:"permissionMode"`
	TurnPosition   *struct {
		PromptIndex *int64 `json:"promptIndex"`
		TurnIndex   *int64 `json:"turnIndex"`
	} `json:"turnPosition"`
	DurationMs   *int64 `json:"durationMs"`
	MessageCount *int64 `json:"messageCount"`
	QuerySource  string `json:"querySource"`
	ID           string `json:"id"` // api-request
	ThinkingMs   *int64 `json:"thinkingDurationMs"`
	Compact      *struct {
		Trigger    *string `json:"trigger"`
		PreTokens  *int64  `json:"preTokens"`
		PostTokens *int64  `json:"postTokens"`
		Dropped    *int64  `json:"cumulativeDroppedTokens"`
		DurationMs *int64  `json:"durationMs"`
	} `json:"compactMetadata"`
	Message *struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		StopReason *string         `json:"stop_reason"`
		Usage      *usage          `json:"usage"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Attachment    json.RawMessage `json:"attachment"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
	Text      string          `json:"text"`
}

type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Path         string `json:"path"`
	Pattern      string `json:"pattern"`
	Command      string `json:"command"`
	URL          string `json:"url"`
	Query        string `json:"query"`
	Skill        string `json:"skill"`
	To           string `json:"to"`
	RecipientKnd string `json:"recipient_kind"`
	SubagentType string `json:"subagent_type"`
	Description  string `json:"description"`
	Model        string `json:"model"`
	Plan         string `json:"plan"`
	Offset       *int64 `json:"offset"`
	Limit        *int64 `json:"limit"`
	Background   *bool  `json:"run_in_background"`
	Todos        []any  `json:"todos"`
}

type toolResult struct {
	Interrupted     *bool  `json:"interrupted"`
	BackgroundTask  string `json:"backgroundTaskId"`
	TimedOutAfterMs *int64 `json:"timedOutAfterMs"`
	StructuredPatch []struct {
		Lines []string `json:"lines"`
	} `json:"structuredPatch"`
	Type            string          `json:"type"`
	Content         json.RawMessage `json:"content"`
	Status          string          `json:"status"`
	AgentID         string          `json:"agentId"`
	AgentType       string          `json:"agentType"`
	ResolvedModel   string          `json:"resolvedModel"`
	TotalDurationMs *int64          `json:"totalDurationMs"`
	TotalTokens     *int64          `json:"totalTokens"`
}

type attach struct {
	Type  string `json:"type"`
	Files []struct {
		Path    string  `json:"path"`
		Type    string  `json:"type"`
		Content *string `json:"content"`
	} `json:"files"`
	Filename   string          `json:"filename"`
	Content    json.RawMessage `json:"content"`
	SkillCount *int64          `json:"skillCount"`
	Skills     []struct {
		Name    string  `json:"name"`
		Content *string `json:"content"`
	} `json:"skills"`
	Reread *struct {
		Length *int64 `json:"contentLength"`
	} `json:"rereadEntry"`
}

// ignoredTypes are records the parser knows and takes no fact from.
var ignoredTypes = setOf("file-history-snapshot", "file-history-delta", "queue-operation", "mode", "bridge-session", "atis-latch",
	"last-prompt", "ai-title", "custom-title", "agent-name", "session-owner", "worktree-state", "tools-used", "cost-state",
	"api-request-blob", "api-request-shape", "pr-link", "summary", "progress", "tag", "frame-link", "fork-context-ref",
	"artifact-autoreact-ledger", "artifact-comment-monitor", "relocated")

var ignoredSystem = setOf("local_command", "stop_hook_summary", "away_summary", "bridge_status", "informational", "scheduled_task_fire")

var ignoredAttach = setOf("total_tokens_reminder", "deferred_tools_record", "environment", "prompt_snapshot", "hook_success",
	"hook_additional_context", "date", "agent_listing_delta", "mcp_instructions_delta", "deferred_tools_delta", "session_context",
	"credential_org", "auto_mode", "remote_session_change", "thinking_drop", "queued_command", "task_status", "hook_system_message",
	"hook_cancelled", "command_permissions", "hook_blocking_error", "hook_stopped_continuation", "model")

func setOf(s ...string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, x := range s {
		m[x] = true
	}
	return m
}

type reqInfo struct {
	at     int64
	source string
}

type parser struct {
	res        *Result
	file       string
	fileThread string
	seq        int
	lastTS     int64
	prompt     *string
	turn       *string
	lastTurn   int
	msgs       map[string]int
	tools      map[string]int
	spawns     map[string]int
	reqs       map[string]reqInfo
	reqRef     map[int]string
	setting    map[string]string
	settingUID map[string]int
}

// Parse reads one transcript file. file is where it sits under the session
// ("main", or "subagents/agent-x.jsonl"); a record's own agentId wins over the
// name. An error is a failed read, not an odd record.
func Parse(r io.Reader, file string) (*Result, error) {
	p := &parser{
		res:      &Result{Unknown: map[string]int{}, Ignored: map[string]int{}},
		file:     file,
		lastTurn: -1,
		msgs:     map[string]int{}, tools: map[string]int{}, spawns: map[string]int{},
		reqs: map[string]reqInfo{}, reqRef: map[int]string{}, setting: map[string]string{}, settingUID: map[string]int{},
	}
	p.fileThread = MainThread
	if file != "" && file != "main" {
		b := strings.TrimSuffix(path.Base(file), ".jsonl")
		p.fileThread = strings.TrimPrefix(b, "agent-")
	}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, tooLong, n, err := readLine(br)
		p.res.Bytes += int64(n)
		switch {
		case tooLong:
			p.res.Lines++
			p.seq++
			p.unknown("(too long)")
		default:
			if l := bytes.TrimSpace(line); len(l) > 0 {
				p.line(l)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	p.finish()
	return p.res, nil
}

func (p *parser) unknown(k string) { p.res.Unknown[k]++ }
func (p *parser) ignored(k string) { p.res.Ignored[k]++ }

// undecoded counts a field the parser could not decode, so that what it
// dropped is told and not silent (ADR145: every drop counted). The line is
// still counted once in its own kind; this is the extra "and part of it was
// lost" figure, named by what was lost.
func (p *parser) undecoded(what string) { p.res.Ignored["undecoded/"+what]++ }

func ptr[T any](v T) *T { return &v }

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func parseTS(s string) *int64 {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return ptr(t.UnixMilli())
}

func (p *parser) line(b []byte) {
	p.res.Lines++
	p.seq++
	var r rec
	if err := json.Unmarshal(b, &r); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			p.unknown("(unreadable)")
			return
		}
		p.undecoded("line_field")
	}
	ts := parseTS(r.Timestamp)
	if ts != nil {
		p.lastTS = *ts
	}
	thread := p.fileThread
	if r.AgentID != "" {
		thread = r.AgentID
	} else if r.Sidechain && thread == MainThread {
		thread = "sidechain"
	}
	main := thread == MainThread
	if main && r.Type != "permission-mode" {
		p.observe("git_branch", r.GitBranch, r.Type, &r, ts)
		p.observe("harness_version", r.Version, r.Type, &r, ts)
	}
	switch r.Type {
	case "":
		p.unknown("(no type)")
	case "assistant":
		p.assistant(&r, ts, thread, main)
	case "user":
		p.user(&r, ts, thread, main)
	case "system":
		p.system(&r, ts, thread)
	case "attachment":
		p.attachment(&r, ts, thread)
	case "permission-mode":
		p.observe("permission_mode", r.PermissionMode, "permission-mode", &r, nil)
	case "api-request":
		if r.ID != "" && ts != nil {
			p.reqs[r.ID] = reqInfo{*ts, r.QuerySource}
		} else {
			p.ignored("api-request/(no id or time)")
		}
	default:
		if ignoredTypes[r.Type] {
			p.ignored(r.Type)
		} else {
			p.unknown(r.Type)
		}
	}
}

func (p *parser) row(uid string, ts *int64, thread string) Row {
	if uid == "" {
		uid = fmt.Sprintf("%s#%d", p.file, p.seq)
	}
	return Row{Uid: uid, Seq: p.seq, At: ts, Thread: thread}
}

// observe records a change of a setting of the main thread.
func (p *parser) observe(name, value, source string, r *rec, ts *int64) {
	if value == "" || p.setting[name] == value {
		return
	}
	s := Setting{Name: name, Value: value, Source: source}
	if prev, ok := p.setting[name]; ok {
		s.Prev = ptr(prev)
	}
	p.setting[name] = value
	at := ts
	if at == nil && p.lastTS > 0 {
		s.NearAt = ptr(p.lastTS)
	}
	when := p.lastTS
	if at != nil {
		when = *at
	}
	uid := fmt.Sprintf("%s=%s@%d", name, value, when)
	p.settingUID[uid]++
	if n := p.settingUID[uid]; n > 1 {
		uid = fmt.Sprintf("%s#%d", uid, n)
	}
	s.Row = Row{Uid: uid, Seq: p.seq, At: at, Thread: MainThread}
	p.res.Settings = append(p.res.Settings, s)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (p *parser) assistant(r *rec, ts *int64, thread string, main bool) {
	m := r.Message
	if m == nil {
		p.unknown("assistant/(no message)")
		return
	}
	if strings.HasPrefix(m.Model, "<") {
		p.ignored("assistant/synthetic")
		p.toolBlocks(m.Content, m.ID, r, ts, thread, true)
		return
	}
	if main {
		p.observe("model", m.Model, "assistant", r, ts)
		p.observe("effort", r.Effort, "assistant", r, ts)
		p.observe("per_turn_effort", r.PerTurnEffort, "assistant", r, ts)
	}
	uid := m.ID
	if uid == "" {
		uid = r.UUID
	}
	idx, seen := p.msgs[uid]
	if !seen || uid == "" {
		c := ModelCall{Row: p.row(uid, ts, thread), Sidechain: r.Sidechain || !main}
		c.MsgID, c.RequestID = strp(m.ID), strp(r.RequestID)
		c.Model, c.RequestedModel = strp(m.Model), strp(r.RequestedModel)
		c.PromptID = p.prompt
		if main {
			c.TurnUID = p.turn
		}
		idx = len(p.res.ModelCalls)
		p.res.ModelCalls = append(p.res.ModelCalls, c)
		if uid != "" {
			p.msgs[uid] = idx
		}
	}
	if r.RequestRef != "" && p.reqRef[idx] == "" {
		p.reqRef[idx] = r.RequestRef
	}
	c := &p.res.ModelCalls[idx]
	c.Effort, c.PerTurnEffort = orStr(strp(r.Effort), c.Effort), orStr(strp(r.PerTurnEffort), c.PerTurnEffort)
	c.RequestID = orStr(strp(r.RequestID), c.RequestID)
	if m.StopReason != nil {
		c.StopReason = m.StopReason
	}
	if ts != nil {
		c.EndAt = ts
	}
	if r.ThinkingMs != nil {
		c.ThinkingMS = ptr(valOr(c.ThinkingMS) + *r.ThinkingMs)
	}
	if u := m.Usage; u != nil {
		c.Input, c.Output, c.CacheRead, c.CacheWrite = u.Input, u.Output, u.CacheRead, u.CacheCreation
		c.Thinking, c.CacheW5m, c.CacheW1h = nil, nil, nil
		if u.Details != nil {
			c.Thinking = u.Details.Thinking
		}
		if u.CC != nil {
			c.CacheW5m, c.CacheW1h = u.CC.E5m, u.CC.E1h
		}
		c.Context = nil
		if u.Input != nil && u.CacheCreation != nil && u.CacheRead != nil {
			c.Context = ptr(*u.Input + *u.CacheCreation + *u.CacheRead)
		}
	}
	p.toolBlocks(m.Content, m.ID, r, ts, thread, false)
}

// toolBlocks records the tool_use blocks of an assistant record. A record
// whose model is <synthetic> is not a model call (no usage, no ModelCall row);
// its blocks are tools a script ran, marked ByScript, with no message id.
func (p *parser) toolBlocks(content json.RawMessage, id string, r *rec, ts *int64, thread string, byScript bool) {
	if !bytes.Contains(content, []byte(`"tool_use"`)) {
		return
	}
	var blocks []block
	if err := json.Unmarshal(content, &blocks); err != nil {
		p.undecoded("assistant_content")
		return
	}
	msgID := id
	if byScript {
		msgID = ""
	}
	for i := range blocks {
		switch {
		case blocks[i].Type != "tool_use":
		case blocks[i].ID == "":
			p.ignored("tool_use/(no id)")
		default:
			p.toolUse(&blocks[i], r, ts, thread, msgID, byScript)
		}
	}
}

func orStr(a, b *string) *string {
	if a != nil {
		return a
	}
	return b
}

func valOr(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// testPathRE: a test file by name (foo_test.go, foo_spec.rb, test_foo.py,
// foo.test.ts, FooTest.java, FooTests.cs) or by a test directory. A heuristic:
// a path it misses is recorded as not a test, one it matches wrongly as a test.
var testPathRE = regexp.MustCompile(`(_(test|spec)\.[A-Za-z0-9]+$|(^|/)test_[^/]*\.py$|\.(test|spec)\.[A-Za-z0-9]+$|Tests?\.(java|kt|cs|scala|swift)$|(^|/)(tests?|__tests__|e2e|spec)/)`)

func isTestPath(p string) bool { return testPathRE.MatchString(p) }

// commandHead is the first words of a command line.
func commandHead(c string) string {
	w := strings.Fields(c)
	if len(w) > commandWords {
		w = w[:commandWords]
	}
	return clip(strings.Join(w, " "), commandRunes)
}

// unparsedURL is what a URL that does not parse is stored as: nothing of it
// is kept, because any part of it could be a secret.
const unparsedURL = "unparsed url"

// urlHead is a URL without its userinfo (user:password@), query or fragment,
// any of which can carry a secret; one that does not parse is not kept. Nor
// is one with a port and a later "@" and no userinfo: a password that starts
// with digits and holds "/", "?" or "#" parses as a port, so the rest of it
// would be kept as path. An empty one is "" (no target).
func urlHead(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.User == nil && u.Port() != "" && strings.Contains(raw, "@")) {
		return unparsedURL
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = nil, "", "", "", false
	return clip(u.String(), targetRunes)
}

func (p *parser) toolUse(bl *block, r *rec, ts *int64, thread, msgID string, byScript bool) {
	if _, dup := p.tools[bl.ID]; dup {
		return
	}
	var in toolInput
	if err := json.Unmarshal(bl.Input, &in); err != nil {
		p.undecoded("tool_input")
	}
	t := ToolCall{Row: p.row(bl.ID, ts, thread), ToolUseID: bl.ID, MsgID: strp(msgID), PromptID: p.prompt, Tool: bl.Name, ByScript: byScript}
	set := func(kind, target string) {
		if target != "" {
			t.Target, t.TargetKind = ptr(target), ptr(kind)
		}
	}
	if IsEditTool(bl.Name) {
		f := in.FilePath
		if f == "" {
			f = in.NotebookPath
		}
		set("file", clip(f, targetRunes))
		t.IsTest = ptr(isTestPath(f))
	}
	switch bl.Name {
	case "Read":
		set("file", clip(in.FilePath, targetRunes))
		t.ReadOffset, t.ReadLimit = in.Offset, in.Limit
	case "Bash", "PowerShell":
		set("command", commandHead(in.Command))
		t.Background = in.Background
		if bl.Name == "Bash" {
			t.ShellWrite = ptr(ShellEdit(in.Command, r.Cwd))
		}
	case "Grep", "Glob":
		set("pattern", clip(in.Pattern, patternRunes))
	case "WebFetch":
		set("url", urlHead(in.URL))
	case "WebSearch":
		set("query", clip(in.Query, patternRunes))
	case "Skill":
		set("skill", clip(in.Skill, 100))
	case "SendMessage":
		set("recipient", clip(in.To, 100))
		t.Detail = strp(clip(in.RecipientKnd, 40))
	case "TodoWrite":
		t.TodoItems = ptr(int64(len(in.Todos)))
	case "ExitPlanMode":
		t.PlanChars = ptr(int64(len(in.Plan)))
	case "Agent", "Task":
		t.Detail = strp(clip(in.SubagentType, 80))
		t.Background = in.Background
		sp := Spawn{Row: p.row(bl.ID, ts, thread), ToolUseID: bl.ID, PromptID: p.prompt, AgentType: strp(clip(in.SubagentType, 80)),
			Purpose: strp(clip(in.Description, purposeRunes)), RequestedModel: strp(in.Model), Background: in.Background}
		p.spawns[bl.ID] = len(p.res.Spawns)
		p.res.Spawns = append(p.res.Spawns, sp)
	}
	p.tools[bl.ID] = len(p.res.ToolCalls)
	p.res.ToolCalls = append(p.res.ToolCalls, t)
}

var exitCodeRE = regexp.MustCompile(`^Exit code (\d+)`)

func (p *parser) user(r *rec, ts *int64, thread string, main bool) {
	if r.PermissionMode != "" && main {
		p.observe("permission_mode", r.PermissionMode, "user", r, ts)
	}
	if r.PromptID != "" {
		p.prompt = ptr(r.PromptID)
	}
	if r.Message == nil {
		p.unknown("user/(no message)")
		return
	}
	content := bytes.TrimSpace(r.Message.Content)
	var text string
	var textBytes int64
	if len(content) > 0 && content[0] == '"' {
		if err := json.Unmarshal(content, &text); err != nil {
			p.undecoded("user_content")
			return
		}
		textBytes = int64(len(text))
	} else if len(content) > 0 && content[0] == '[' {
		var blocks []block
		if err := json.Unmarshal(content, &blocks); err != nil {
			p.undecoded("user_content")
			return
		}
		results := 0
		for i := range blocks {
			switch blocks[i].Type {
			case "tool_result":
				results++
			case "text":
				text += blocks[i].Text
				textBytes += int64(len(blocks[i].Text))
			}
		}
		if results > 0 {
			p.results(blocks, r, ts, results)
			return
		}
	} else {
		p.unknown("user/(content)")
		return
	}
	switch {
	case r.IsCompact:
		p.ignored("user/compact_summary")
		return
	case !main:
		p.ignored("user/sidechain_prompt")
		return
	case strings.HasPrefix(text, "<local-command-"):
		p.ignored("user/local_command_output")
		return
	case r.IsMeta && r.TurnOrigin == "":
		p.ignored("user/meta")
		return
	}
	t := Turn{Row: p.row(r.UUID, ts, thread), PromptID: strp(r.PromptID), PromptSource: strp(r.PromptSource), PromptBytes: ptr(textBytes), PermMode: strp(r.PermissionMode)}
	switch {
	case r.TurnOrigin != "":
		t.Origin = ptr(r.TurnOrigin)
	case r.Origin != nil && r.Origin.Kind != "":
		t.Origin = ptr(r.Origin.Kind)
	}
	if r.TurnPosition != nil {
		t.PromptIndex, t.TurnIndex = r.TurnPosition.PromptIndex, r.TurnPosition.TurnIndex
	}
	if i := strings.Index(text, "<command-name>"); i >= 0 {
		rest := text[i+len("<command-name>"):]
		if j := strings.Index(rest, "</command-name>"); j >= 0 {
			t.SlashCommand = strp(clip(rest[:j], 60))
		}
	}
	p.turn = ptr(t.Uid)
	p.lastTurn = len(p.res.Turns)
	p.res.Turns = append(p.res.Turns, t)
}

// resultSize is the size of a tool result's content and its first text; nil
// when the result has no content key (an empty string is a known 0).
func (p *parser) resultSize(raw json.RawMessage) (n *int64, text string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			p.undecoded("tool_result_content")
			return nil, ""
		}
		return ptr(int64(len(s))), s
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		p.undecoded("tool_result_content")
		return nil, ""
	}
	var total int64
	for _, b := range blocks {
		total += int64(len(b.Text))
		if text == "" {
			text = b.Text
		}
	}
	return ptr(total), text
}

func (p *parser) results(blocks []block, r *rec, ts *int64, n int) {
	var tr *toolResult
	if raw := bytes.TrimSpace(r.ToolUseResult); n == 1 && len(raw) > 0 && raw[0] == '{' {
		tr = &toolResult{}
		if err := json.Unmarshal(raw, tr); err != nil {
			p.undecoded("tool_use_result")
		}
	}
	for i := range blocks {
		bl := &blocks[i]
		if bl.Type != "tool_result" {
			continue
		}
		idx, ok := p.tools[bl.ToolUseID]
		if !ok {
			p.ignored("tool_result/unmatched")
			continue
		}
		t := &p.res.ToolCalls[idx]
		size, text := p.resultSize(bl.Content)
		t.ResultAt, t.ResultBytes, t.IsError = ts, size, ptr(bl.IsError)
		if ts != nil && t.At != nil && *ts >= *t.At {
			t.DurationMS = ptr(*ts - *t.At)
		}
		if bl.IsError {
			t.Error = strp(clip(text, errorRunes))
			if m := exitCodeRE.FindStringSubmatch(text); m != nil {
				if c, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					t.ExitCode = &c
				}
			}
		}
		if tr != nil {
			p.applyResult(t, tr, ts)
		}
		if si, ok := p.spawns[bl.ToolUseID]; ok {
			sp := &p.res.Spawns[si]
			sp.ResultAt = ts
			if tr != nil {
				sp.Status, sp.AgentID = strp(tr.Status), strp(tr.AgentID)
				sp.ResolvedModel, sp.Tokens = strp(tr.ResolvedModel), tr.TotalTokens
				sp.DurationMS = tr.TotalDurationMs
				if sp.AgentType == nil {
					sp.AgentType = strp(tr.AgentType)
				}
			}
		}
	}
}

func (p *parser) applyResult(t *ToolCall, tr *toolResult, ts *int64) {
	t.Interrupted = tr.Interrupted
	if tr.TimedOutAfterMs != nil {
		t.TimedOut = ptr(true)
	}
	if tr.BackgroundTask != "" {
		t.Background = ptr(true)
	}
	if IsEditTool(t.Tool) {
		switch {
		case tr.Type == "create":
			if s, ok := p.decodeString(tr.Content, "create_result_content"); ok {
				n := int64(strings.Count(s, "\n"))
				if s != "" && !strings.HasSuffix(s, "\n") {
					n++
				}
				t.LinesAdded, t.LinesRemoved = ptr(n), ptr(int64(0))
			}
		case tr.StructuredPatch != nil:
			var add, rem int64
			for _, h := range tr.StructuredPatch {
				for _, l := range h.Lines {
					switch {
					case strings.HasPrefix(l, "+"):
						add++
					case strings.HasPrefix(l, "-"):
						rem++
					}
				}
			}
			t.LinesAdded, t.LinesRemoved = &add, &rem
		}
	}
}

func (p *parser) system(r *rec, ts *int64, thread string) {
	switch {
	case r.Subtype == "compact_boundary":
		c := Compaction{Row: p.row(r.UUID, ts, thread)}
		if m := r.Compact; m != nil {
			c.Trigger, c.PreTokens, c.PostTokens, c.Dropped, c.DurationMS = m.Trigger, m.PreTokens, m.PostTokens, m.Dropped, m.DurationMs
		}
		p.res.Compactions = append(p.res.Compactions, c)
	case r.Subtype == "turn_duration":
		if p.lastTurn >= 0 && p.res.Turns[p.lastTurn].DurationMS == nil && thread == MainThread {
			t := &p.res.Turns[p.lastTurn]
			t.DurationMS, t.Messages, t.EndedAt = r.DurationMs, r.MessageCount, ts
			return
		}
		p.ignored("system/turn_duration (no open turn)")
	case ignoredSystem[r.Subtype]:
		p.ignored("system/" + r.Subtype)
	default:
		p.unknown("system/" + r.Subtype)
	}
}

func (p *parser) attachment(r *rec, ts *int64, thread string) {
	var a attach
	if err := json.Unmarshal(r.Attachment, &a); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			p.unknown("attachment/(unreadable)")
			return
		}
		p.undecoded("attachment_field")
	}
	add := func(i int, f InjectedFile) {
		f.Row = p.row(fmt.Sprintf("%s#%d", orUID(r.UUID, p.file, p.seq), i), ts, thread)
		p.res.Injected = append(p.res.Injected, f)
	}
	switch a.Type {
	case "instructions":
		for i, f := range a.Files {
			add(i, InjectedFile{Kind: "instructions", FileType: strp(f.Type), Name: strp(f.Path), Bytes: sizeOf(f.Content)})
		}
	case "file":
		f := InjectedFile{Kind: "file", Name: strp(a.Filename)}
		var c struct {
			File *struct {
				Content  *string `json:"content"`
				NumLines *int64  `json:"numLines"`
			} `json:"file"`
		}
		if len(bytes.TrimSpace(a.Content)) > 0 {
			if err := json.Unmarshal(a.Content, &c); err != nil {
				p.undecoded("file_attachment")
			} else if c.File != nil {
				f.Bytes, f.Lines = sizeOf(c.File.Content), c.File.NumLines
			}
		}
		add(0, f)
	case "skill_listing":
		f := InjectedFile{Kind: "skill_listing", Count: a.SkillCount}
		if s, ok := p.decodeString(a.Content, "skill_listing"); ok {
			f.Bytes = ptr(int64(len(s)))
		}
		add(0, f)
	case "invoked_skills":
		for i, s := range a.Skills {
			add(i, InjectedFile{Kind: "skill", Name: strp(s.Name), Bytes: sizeOf(s.Content)})
		}
	case "compact_file_reference":
		add(0, InjectedFile{Kind: "compact_file_reference", Name: strp(a.Filename)})
	case "edited_text_file":
		f := InjectedFile{Kind: "edited_file", Name: strp(a.Filename)}
		if a.Reread != nil {
			f.Bytes = a.Reread.Length
		}
		add(0, f)
	case "":
		p.unknown("attachment/(no type)")
	default:
		if ignoredAttach[a.Type] {
			p.ignored("attachment/" + a.Type)
		} else {
			p.unknown("attachment/" + a.Type)
		}
	}
}

// sizeOf is the length of a string that may be absent: nil when it is.
func sizeOf(s *string) *int64 {
	if s == nil {
		return nil
	}
	return ptr(int64(len(*s)))
}

// decodeString decodes a JSON string field that may be absent: absent is not
// an error and is not counted (nor is a JSON null), a value that is not a
// string is counted as undecoded under what.
func (p *parser) decodeString(raw json.RawMessage, what string) (string, bool) {
	if raw = bytes.TrimSpace(raw); len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		p.undecoded(what)
		return "", false
	}
	return s, true
}

func orUID(uuid, file string, seq int) string {
	if uuid != "" {
		return uuid
	}
	return fmt.Sprintf("%s#%d", file, seq)
}

// finish works out what needs the whole file: the gap between a thread's calls,
// the request latency and the inferred cold cache.
func (p *parser) finish() {
	type prev struct {
		at    int64
		long  bool
		split bool
		held  int64
	}
	last := map[string]prev{}
	for i := range p.res.ModelCalls {
		c := &p.res.ModelCalls[i]
		if ri, ok := p.reqs[p.reqRef[i]]; ok && p.reqRef[i] != "" {
			c.QuerySource = strp(ri.source)
			if c.EndAt != nil && *c.EndAt >= ri.at {
				c.LatencyMS = ptr(*c.EndAt - ri.at)
			}
			delete(p.reqs, p.reqRef[i])
		}
		if c.At == nil {
			continue
		}
		if pv, ok := last[c.Thread]; ok {
			c.GapMS = ptr(*c.At - pv.at)
			if c.CacheWrite != nil {
				c.ColdInferred = coldInferred(*c.CacheWrite, *c.GapMS, pv.long, pv.split)
				if pv.held > 0 && valOr(c.CacheRead) >= pv.held {
					c.ColdInferred = ptr(false)
				}
			}
		}
		at := *c.At
		if c.EndAt != nil {
			at = *c.EndAt
		}
		last[c.Thread] = prev{at: at, long: valOr(c.CacheW1h) > 0, split: c.CacheW5m != nil || c.CacheW1h != nil,
			held: valOr(c.CacheRead) + valOr(c.CacheWrite)}
	}
	for range p.reqs {
		p.ignored("api-request/unmatched")
	}
}

// Cache lifetimes (Anthropic prompt caching): 5 minutes by default, 1 hour
// when the request asks for the longer one.
const (
	cacheTTL     = 5 * 60 * 1000
	cacheTTLLong = 60 * 60 * 1000
)

// coldInferred says whether a cache write came after the previous call's cache
// had expired, judged by the gap alone (the caller overrules it when the cache
// read shows the previous call's cache was still there). nil when that cannot be told: without the previous call's
// split between the 5 minute and 1 hour caches, a gap between the two
// lifetimes may or may not have outlived it.
func coldInferred(write, gap int64, prevLong, prevSplit bool) *bool {
	if write <= 0 {
		return ptr(false)
	}
	switch {
	case prevLong:
		return ptr(gap > cacheTTLLong)
	case prevSplit:
		return ptr(gap > cacheTTL)
	case gap > cacheTTLLong:
		return ptr(true)
	case gap <= cacheTTL:
		return ptr(false)
	}
	return nil
}

// readLine reads one line; one over MaxLine is read through and reported as
// tooLong with no content. n is the bytes consumed. io.EOF comes with the last
// line (which may be empty).
func readLine(br *bufio.Reader) (line []byte, tooLong bool, n int, err error) {
	var buf []byte
	for {
		chunk, e := br.ReadSlice('\n')
		n += len(chunk)
		if errors.Is(e, bufio.ErrBufferFull) {
			if !tooLong {
				buf = append(buf, chunk...)
				if len(buf) > MaxLine {
					tooLong, buf = true, nil
				}
			}
			continue
		}
		if tooLong || len(buf)+len(chunk) > MaxLine {
			return nil, true, n, e
		}
		if buf == nil {
			return chunk, false, n, e
		}
		return append(buf, chunk...), false, n, e
	}
}
