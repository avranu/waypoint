// Package transcript derives per-event facts from a Claude Code transcript
// (WP-1467, ADR145): JSONL in, typed rows out. It touches no database and no
// clock. A value the transcript does not carry is nil, never 0 or a default
// label; a record it does not understand is counted by type, never skipped
// silently.
//
// The facts are one row per model call, tool call, turn, compaction, injected
// file, sub-agent spawn and settings change. Each row has a Uid that is the
// same however many times, in however many files or generations, the record
// appears, so a store can keep it once per session.
package transcript

// ParserVersion is stored on every row the parser produced; a bump re-derives
// every archived file. Change it whenever the parser would produce different
// rows from the same bytes.
const ParserVersion = 3

// Thread names who wrote a record: the main thread, or the sub-agent's id.
const MainThread = "main"

// IsEditTool is a tool that changes a file: the parser gives its calls an
// is_test flag and line counts, and the read path counts them as edits.
func IsEditTool(name string) bool {
	switch name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

// Result is what one transcript file holds.
type Result struct {
	Lines int64 // non-empty lines read
	Bytes int64 // bytes read
	// Unknown counts the records the parser did not understand, by "type",
	// "type/subtype" or "attachment/type"; "(unreadable)" and "(no type)" are
	// lines that were not a JSON object with a type, "(too long)" a line over
	// MaxLine.
	Unknown map[string]int
	// Ignored counts what it understood and took no fact from, by the same
	// names as Unknown: records ("system/x", "attachment/x", "user/meta", ...)
	// and parts of records that could not be used ("undecoded/WHAT", the
	// "api-request/..." drops, "tool_use/(no id)", "tool_result/unmatched").
	// It is not a record total and Lines is not the sum of anything here: an
	// api-request line that joined a model call, and a user line whose content
	// could not be decoded (counted only as undecoded/user_content), are in no
	// key of their own.
	Ignored map[string]int

	ModelCalls  []ModelCall
	ToolCalls   []ToolCall
	Turns       []Turn
	Compactions []Compaction
	Injected    []InjectedFile
	Spawns      []Spawn
	Settings    []Setting
}

// UnknownTotal is the number of records not understood.
func (r *Result) UnknownTotal() int {
	n := 0
	for _, c := range r.Unknown {
		n += c
	}
	return n
}

// Common to every row.
type Row struct {
	// Uid identifies the event across files and generations: a message id, a
	// tool-use id, a record uuid.
	Uid string
	// Seq is the line's number in the file (from 1), so rows of a file order
	// even when two have the same time or none.
	Seq int
	// At is the record's time in ms; nil when the record has none.
	At     *int64
	Thread string // MainThread, or a sub-agent's id
}

// ModelCall is one request to a model.
type ModelCall struct {
	Row
	MsgID          *string // the API message id; streamed blocks of one call share it
	RequestID      *string
	Model          *string
	RequestedModel *string
	Effort         *string // the session's effort when the call was made
	PerTurnEffort  *string
	QuerySource    *string // repl_main_thread, ...: from the api-request record
	StopReason     *string
	// Tokens as the API reported them. Thinking is part of Output.
	Input      *int64
	Output     *int64
	Thinking   *int64
	CacheRead  *int64
	CacheWrite *int64 // all cache writes
	CacheW5m   *int64 // nil when the usage has no split
	CacheW1h   *int64
	// Context is Input + CacheRead + CacheWrite: what the model was given.
	Context *int64
	// EndAt is when its last block was written; LatencyMS from the request
	// being sent (the api-request record) to EndAt, nil without that record.
	EndAt     *int64
	LatencyMS *int64
	// ThinkingMS: how long the model thought, as the transcript says.
	ThinkingMS *int64
	PromptID   *string // the human prompt the call belongs to
	TurnUID    *string // the turn record it falls in (main thread only)
	// GapMS is the time since the thread's previous call. ColdInferred is an
	// inference, not a measurement: a cache write after a gap longer than the
	// cache's lifetime (5 minutes, or 1 hour when the previous call wrote to the
	// 1 hour cache; Anthropic prompt caching). nil when there is no previous call
	// or no times, or when the previous call's split between the two caches is
	// unknown and the gap is between the two lifetimes.
	GapMS        *int64
	ColdInferred *bool
	Sidechain    bool
}

// ToolCall is one tool_use block with what its result said.
type ToolCall struct {
	Row
	ToolUseID string
	MsgID     *string
	PromptID  *string
	Tool      string
	// ByScript: a REPL script ran it (the record's model is <synthetic>), not
	// the model; MsgID is nil then.
	ByScript bool
	// ShellWrite (Bash only): the command wrote a file that is not scratch,
	// by the recogniser in ShellEdit; nil for other tools.
	ShellWrite *bool
	// Target is a file path, URL, command head (first words, clipped), a search
	// pattern or query (clipped), a skill name or the recipient of a message.
	// TargetKind says which; nil when the call has none.
	Target     *string
	TargetKind *string
	Detail     *string // recipient kind for SendMessage, subagent type for Agent
	ReadOffset *int64  // Read
	ReadLimit  *int64
	TodoItems  *int64 // TodoWrite: items in the list
	PlanChars  *int64 // ExitPlanMode: size of the plan
	// Background: the call asked to run in the background (nil: the tool has
	// no such option).
	Background *bool
	IsTest     *bool // an edit of a test file; nil for tools that do not edit
	// From the result (all nil while no result was found):
	ResultAt     *int64
	DurationMS   *int64 // result time - call time: includes a permission wait
	ResultBytes  *int64
	IsError      *bool
	Error        *string // first words of the error text
	ExitCode     *int64  // read from "Exit code N"; nil when the text has none
	Interrupted  *bool
	TimedOut     *bool
	LinesAdded   *int64 // edits: from the result's patch (a created file: its lines)
	LinesRemoved *int64
}

// Turn is one thing that started the model working: a prompt, a notification, a
// message from another session.
type Turn struct {
	Row
	PromptID *string
	// Origin is what the transcript says started it (human, task_notification,
	// peer, scheduled); nil when the record does not say (older Claude Code).
	Origin       *string
	PromptSource *string
	SlashCommand *string
	PromptIndex  *int64
	TurnIndex    *int64
	PromptBytes  *int64 // size of the prompt, not the prompt
	PermMode     *string
	// DurationMS and Messages are from the turn_duration record that ended it;
	// nil when the turn has none (still running, or cut short).
	DurationMS *int64
	Messages   *int64
	EndedAt    *int64
}

// Compaction is a compact_boundary record.
type Compaction struct {
	Row
	Trigger    *string
	PreTokens  *int64
	PostTokens *int64
	Dropped    *int64
	DurationMS *int64
}

// InjectedFile is something put into the model's context that the person did
// not type: an instruction or memory file, a file read for it, the skill list.
type InjectedFile struct {
	Row
	// Kind is the attachment: instructions, file, skill_listing, skill,
	// compact_file_reference, edited_file.
	Kind string
	// FileType is an instruction file's class (Project, AutoMem, User, ...).
	FileType *string
	Name     *string // path, or the skill's name
	Bytes    *int64  // size of the content; nil when the record carries none
	Lines    *int64
	Count    *int64 // skill_listing: skills listed
}

// Spawn is an Agent (or Task) call.
type Spawn struct {
	Row
	ToolUseID      string
	PromptID       *string
	AgentType      *string // nil when the call names none
	Purpose        *string // its description, clipped
	RequestedModel *string
	Background     *bool
	// From the result:
	AgentID       *string
	ResolvedModel *string
	Status        *string
	ResultAt      *int64
	DurationMS    *int64
	Tokens        *int64
}

// Setting is a change of something the session ran with, seen in the
// transcript: the timeline of a setting is its rows in order. The state at a
// moment is the latest row at or before it.
type Setting struct {
	Row
	// Name: model, effort, per_turn_effort, permission_mode, git_branch,
	// harness_version.
	Name  string
	Value string
	Prev  *string // the value before; nil for the first seen
	// NearAt is the latest time seen before the record, for a record that has no
	// time of its own (At is then nil).
	NearAt *int64
	Source string // the record it was read from
}
