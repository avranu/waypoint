package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// transcript format is Claude Code's internal and changes between versions: skip and count unrecognised lines; keep a byte offset to resume

type usageMsgRow struct {
	Hour    int64  `json:"h"`
	Session string `json:"s"`
	Repo    string `json:"repo,omitempty"`
	Model   string `json:"model"`
	Kind    string `json:"kind,omitempty"`
	N       int64  `json:"n"`
	Input   int64  `json:"in"`
	Output  int64  `json:"out"`
	CW5     int64  `json:"cw5"`
	CW1     int64  `json:"cw1"`
	CR      int64  `json:"cr"`
}

type usageToolRow struct {
	Hour     int64  `json:"h"`
	Session  string `json:"s"`
	Repo     string `json:"repo,omitempty"`
	Model    string `json:"model,omitempty"`
	Kind     string `json:"kind"`
	Tool     string `json:"tool,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Calls    int64  `json:"calls"`
	Errors   int64  `json:"errors,omitempty"`
	Refusals int64  `json:"refusals,omitempty"`
	Prompts  int64  `json:"prompts,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
}

type usageSessionRow struct {
	Session     string `json:"s"`
	Cwd         string `json:"cwd,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Version     string `json:"version,omitempty"`
	Title       string `json:"title,omitempty"`
	First       int64  `json:"first,omitempty"`
	Last        int64  `json:"last,omitempty"`
	Compactions int64  `json:"compactions,omitempty"`
}

type usageRefusalRow struct {
	At      int64  `json:"t"`
	Session string `json:"s"`
	Repo    string `json:"repo,omitempty"`
	Model   string `json:"model,omitempty"`
	Tool    string `json:"tool"`
	Detail  string `json:"detail,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Source  string `json:"source,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// usageRiskyRow is one risky shell command (WP-647) and what came of it.
type usageRiskyRow struct {
	At      int64  `json:"t"`
	Session string `json:"s"`
	Repo    string `json:"repo,omitempty"`
	Model   string `json:"model,omitempty"`
	Kind    string `json:"kind"`
	Level   string `json:"level"`
	Label   string `json:"label"`
	Command string `json:"command"`           // the whole line, redacted (up to riskyCommandRunes)
	Segment string `json:"segment,omitempty"` // the risky part, when the line as sent doesn't hold it
	Outcome string `json:"outcome"`           // ran, error, refused, unknown (no result was read)
	Source  string `json:"source,omitempty"`  // who refused it
}

// usageLedgerRow is the per-call measures of one hour (WP-35): what each API
// call's context was, which calls were cold cache starts, and how subagents
// began. Same key as usageMsgRow; kept apart so history can be re-read for
// them without counting its messages twice.
type usageLedgerRow struct {
	Hour     int64  `json:"h"`
	Session  string `json:"s"`
	Repo     string `json:"repo,omitempty"`
	Model    string `json:"model"`
	Kind     string `json:"kind,omitempty"`
	Calls    int64  `json:"n"`     // calls measured
	Ctx      int64  `json:"ctx"`   // their context, summed (input + cache write + cache read)
	Over200  int64  `json:"o200"`  // calls with over 200k of context
	Over300  int64  `json:"o300"`  // … over 300k
	CR300    int64  `json:"cr300"` // cache reads made by the calls over 300k
	Cold     int64  `json:"cold"`  // cache rebuilt after a gap longer than the cache's lifetime
	ColdW5   int64  `json:"cold_w5,omitempty"`
	ColdW1   int64  `json:"cold_w1,omitempty"`
	Forks    int64  `json:"forks,omitempty"` // subagents that began from the parent's context
	ForkCtx  int64  `json:"fork_ctx,omitempty"`
	Fresh    int64  `json:"fresh,omitempty"` // subagents that began empty
	FreshCtx int64  `json:"fresh_ctx,omitempty"`
}

// usageChunk is what one range of one file added; its id names the range.
type usageChunk struct {
	ID       string            `json:"id"`
	Ledger   []usageLedgerRow  `json:"ledger,omitempty"`
	Messages []usageMsgRow     `json:"messages,omitempty"`
	Tools    []usageToolRow    `json:"tools,omitempty"`
	Sessions []usageSessionRow `json:"sessions,omitempty"`
	Refusals []usageRefusalRow `json:"refusals,omitempty"`
	Risky    []usageRiskyRow   `json:"risky,omitempty"`
	CLI      []usageCLIRow     `json:"cli,omitempty"`
	Traces   []usageTraceRow   `json:"traces,omitempty"`
}

func (c *usageChunk) rows() int {
	return len(c.Messages) + len(c.Tools) + len(c.Sessions) + len(c.Refusals) + len(c.Ledger) + len(c.Risky) + len(c.CLI) + len(c.Traces)
}

// usageFileState is where a transcript has been read to, and what the next
// read needs to know about what came before.
type usageFileState struct {
	Ino  uint64 `json:"ino,omitempty"`
	Size int64  `json:"size"`
	Off  int64  `json:"off"`
	FP   string `json:"fp,omitempty"` // hash of the file's first bytes: a rewritten file starts over
	// LV: the ledger version the file was read with. A file read before the
	// ledger (0) is read again from the start for its ledger rows only.
	LV int `json:"lv,omitempty"`
	// RV: the risky-command version (WP-647); an older file is read again
	// for its risky rows the same way.
	RV int `json:"rv,omitempty"`
	// CV: the wp usage counters' version (WP-618), read again the same way.
	// CLICounted: the file has been counted once; CLIPending: wp commands
	// still watching what the next few Bash calls run.
	CV int `json:"cv,omitempty"`
	// TV: the navigation traces' version (WP-250), read again the same way.
	// Trace: the takes counted and the segment still open.
	TV         int               `json:"tv,omitempty"`
	Trace      *traceState       `json:"tr,omitempty"`
	CLICounted bool              `json:"cc,omitempty"`
	CLIPending []usageCLIPending `json:"cp,omitempty"`
	// CLILastCI: the last Bash call ran wp pr or wp ci. CLISleepHour and
	// CLISleepModel: the last Bash call slept beside neither, so what comes
	// next decides whether that sleep was waiting on CI.
	CLILastCI     bool   `json:"cl,omitempty"`
	CLISleepHour  int64  `json:"csh,omitempty"`
	CLISleepModel string `json:"csm,omitempty"`
	// Spawned: the subagent's first call is counted; Fork: it began from the
	// parent's context (a fork-context-ref line); PrevTS and PrevLong: the
	// previous call's time and whether it wrote the 1 hour cache.
	Spawned  bool  `json:"sp,omitempty"`
	Fork     bool  `json:"fk,omitempty"`
	PrevTS   int64 `json:"pt,omitempty"`
	PrevLong bool  `json:"pl,omitempty"`
	// Last: the final messages read, with the usage already counted, so a
	// message written again with more tokens adds only the difference.
	Last []usageSeenMsg `json:"last,omitempty"`
	// Open: tool calls whose result hasn't been read yet.
	Open    map[string]*usageOpenCall `json:"open,omitempty"`
	Cwd     string                    `json:"cwd,omitempty"`
	Repo    string                    `json:"repo,omitempty"`
	Branch  string                    `json:"branch,omitempty"`
	Version string                    `json:"version,omitempty"`
	Title   string                    `json:"title,omitempty"`
}

type usageSeenMsg struct {
	ID  string `json:"id"`
	In  int64  `json:"i,omitempty"`
	Out int64  `json:"o,omitempty"`
	CW5 int64  `json:"w,omitempty"`
	CW1 int64  `json:"x,omitempty"`
	CR  int64  `json:"r,omitempty"`
}

type usageToolKey struct {
	Kind   string `json:"k"`
	Tool   string `json:"t,omitempty"`
	Detail string `json:"d,omitempty"`
}

type usageOpenCall struct {
	Tool    string         `json:"tool"`
	Detail  string         `json:"detail,omitempty"`
	Hour    int64          `json:"h"`
	Model   string         `json:"m,omitempty"`
	Keys    []usageToolKey `json:"keys"`
	Excerpt string         `json:"x,omitempty"`
	Risky   *riskyHit      `json:"r,omitempty"`
	// WP: the wp commands the call ran, other than help; its result is read
	// for their usage errors (WP-618).
	WP    []string `json:"wp,omitempty"`
	shell *cliShellCall
	// T: what the navigation trace keeps of the call until its result is read.
	T *traceCall `json:"tc,omitempty"`
}

const (
	usageLedgerVersion = 1
	usageRiskyVersion  = 1
	usageKeepLast      = 8
	usageKeepOpen      = 64
	usageOpenMaxAge    = 36 * time.Hour
	excerptRunes       = 300
	// riskyCommandRunes: a risky command is sent whole up to this (WP-860);
	// the server keeps as much.
	riskyCommandRunes = 16 << 10
	// riskyLineRunes: wp usage risky's text shows a command on one line of
	// this much; --json has it whole.
	riskyLineRunes = 300
)

// usageFile is a transcript to read.
type usageFile struct {
	Path    string
	Session string
	Kind    string // main, or the subagent's type
	Main    bool
}

// usageStats counts what the parser saw.
type usageStats struct {
	Lines    int64            // complete lines read
	Parsed   int64            // understood
	Ignored  int64            // known and not needed (attachments, snapshots…)
	Skipped  int64            // of a kind not recognised, or unreadable
	Unknown  map[string]int64 // skipped line types
	Messages int64
	Calls    int64
	Refusals int64
	Bytes    int64
}

func (s *usageStats) add(o usageStats) {
	s.Lines += o.Lines
	s.Parsed += o.Parsed
	s.Ignored += o.Ignored
	s.Skipped += o.Skipped
	s.Messages += o.Messages
	s.Calls += o.Calls
	s.Refusals += o.Refusals
	s.Bytes += o.Bytes
	for k, v := range o.Unknown {
		if s.Unknown == nil {
			s.Unknown = map[string]int64{}
		}
		s.Unknown[k] += v
	}
}

// usageParsed is the result of reading one file.
type usageParsed struct {
	File     usageFile
	From, To int64
	State    usageFileState
	Chunk    usageChunk
	Stats    usageStats
	Changed  bool // the file had something new
	// LedgerOnly, RiskyOnly: an older file read again, up to where it had
	// been read, for the rows that didn't exist when it was first read.
	LedgerOnly bool
	RiskyOnly  bool
	CLIOnly    bool
	TraceOnly  bool

	messageIDs []string
}

type rawUsage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	CC            *struct {
		E5m int64 `json:"ephemeral_5m_input_tokens"`
		E1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

type rawLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Version   string `json:"version"`
	AiTitle   string `json:"aiTitle"`
	Sidechain bool   `json:"isSidechain"`
	Message   *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Usage   *rawUsage       `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	PermissionDecision *rawDecision `json:"permissionDecision"`
}

type rawDecision struct {
	Decision   string `json:"decision"`
	Source     string `json:"source"`
	ReasonType string `json:"reasonType"`
}

type rawBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
	Text      string          `json:"text"`
}

type rawInput struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	Skill        string `json:"skill"`
	SubagentType string `json:"subagent_type"`
	Description  string `json:"description"`
}

var (
	markAssistant = []byte(`"type":"assistant"`)
	markUser      = []byte(`"type":"user"`)
	markSystem    = []byte(`"type":"system"`)
	markTitle     = []byte(`"type":"ai-title"`)
	markToolUse   = []byte(`"type":"tool_use"`)
	markResult    = []byte(`"type":"tool_result"`)
	markCommand   = []byte(`<command-name>`)
	// the search-chain nudge's note (WP-183) in what a hook added to a call
	markAttachment = []byte(`"type":"attachment"`)
	markChainNote  = []byte("grep and sed waste tokens: wp code find WORDS")
	markForkRef    = []byte(`{"type":"fork-context-ref"`)
	// line types known to carry nothing we use
	ignorable = [][]byte{[]byte(`"type":"attachment"`), []byte(`"type":"file-history-snapshot"`), []byte(`"type":"file-history-delta"`),
		[]byte(`"type":"queue-operation"`), []byte(`"type":"mode"`), []byte(`"type":"permission-mode"`), []byte(`"type":"bridge-session"`),
		[]byte(`"type":"atis-latch"`), []byte(`"type":"last-prompt"`), []byte(`"type":"pr-link"`), []byte(`"type":"custom-title"`),
		[]byte(`"type":"summary"`), []byte(`"type":"progress"`), []byte(`"type":"agent-name"`), []byte(`"type":"tag"`),
		[]byte(`"type":"frame-link"`), []byte(`"type":"fork-context-ref"`), []byte(`"type":"cost-state"`), []byte(`"type":"artifact-autoreact-ledger"`),
		[]byte(`"type":"artifact-comment-monitor"`)}
	knownTypes = map[string]bool{"attachment": true, "file-history-snapshot": true, "file-history-delta": true, "queue-operation": true, "mode": true,
		"permission-mode": true, "bridge-session": true, "atis-latch": true, "last-prompt": true, "pr-link": true, "custom-title": true,
		"summary": true, "progress": true, "agent-name": true, "tag": true, "frame-link": true, "fork-context-ref": true, "cost-state": true,
		"artifact-autoreact-ledger": true, "artifact-comment-monitor": true}
	commandRE = lazyre.New(`<command-name>\s*(/?[^<\s]+)\s*</command-name>`)
	reasonRE  = lazyre.New(`Reason: \[([^\]]{1,80})\]`)
	hookRE    = lazyre.New(`hook error: \[([^\]]{1,300})\]`)
)

// readUsageFile reads the new part of a transcript.
func readUsageFile(f usageFile, st usageFileState, repoOf func(cwd string) string, wantIDs bool) (*usageParsed, error) {
	fh, err := os.Open(f.Path) // #nosec G304 -- a transcript Claude Code wrote
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	fi, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	fp := fingerprint(fh, size)
	if st.FP != "" && (st.FP != fp || size < st.Off) {
		st = usageFileState{}
	}
	ledgerOnly := st.Off > 0 && st.LV < usageLedgerVersion
	riskyOnly := st.Off > 0 && st.RV < usageRiskyVersion
	cliOnly := st.Off > 0 && st.CV < usageCLIVersion
	traceOnly := f.Main && st.Off > 0 && st.TV < usageTraceVersion // only main transcripts carry the navigation trace; subagent files do not
	if ledgerOnly || riskyOnly || cliOnly || traceOnly {
		// a version bump re-reads the bytes already read, for the rows the old version did not count
		size = st.Off
		st = usageFileState{}
	}
	st.LV, st.RV, st.CV, st.TV = usageLedgerVersion, usageRiskyVersion, usageCLIVersion, usageTraceVersion
	st.FP, st.Ino, st.Size = fp, inodeOf(fi), fi.Size()
	out := &usageParsed{File: f, From: st.Off, State: st, LedgerOnly: ledgerOnly, RiskyOnly: riskyOnly, CLIOnly: cliOnly, TraceOnly: traceOnly}
	if st.Off >= size {
		out.To = st.Off
		return out, nil
	}
	if _, err := fh.Seek(st.Off, io.SeekStart); err != nil {
		return nil, err
	}
	p := newUsageParser(f, &out.State)
	br := bufio.NewReaderSize(io.LimitReader(fh, size-st.Off), 1<<20)
	var long []byte
	consumed := int64(0)
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long = append(long, line...)
			continue
		}
		if len(long) > 0 {
			line, long = append(long, line...), nil
		}
		if err != nil {
			break // EOF: a half-written last line is read next time
		}
		consumed += int64(len(line))
		p.line(line)
	}
	out.To = st.Off + consumed
	out.State.Off = out.To
	out.Changed = consumed > 0
	p.finish(out, repoOf)
	if wantIDs {
		out.messageIDs = p.order
	}
	out.Stats = p.stats
	out.Stats.Bytes = consumed
	return out, nil
}

func fingerprint(fh *os.File, size int64) string {
	n := min(size, 256)
	if n == 0 {
		return ""
	}
	b := make([]byte, n)
	if _, err := fh.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

type usageMsg struct {
	id    string
	model string
	ts    int64
	u     usageSeenMsg
}

type usageParser struct {
	f     usageFile
	st    *usageFileState
	stats usageStats

	msgs         map[string]*usageMsg
	order        []string
	calls        map[string]*usageOpenCall
	done         map[string]bool
	tools        map[toolAccKey]*usageToolRow
	refs         []usageRefusalRow
	risky        []usageRiskyRow
	cli          map[usageCLIKey]int64
	traceRows    map[int]usageTraceRow
	traceTouched bool
	comp         int64
	first        int64
	last         int64
}

type toolAccKey struct {
	hour  int64
	model string
	k     usageToolKey
}

func newUsageParser(f usageFile, st *usageFileState) *usageParser {
	p := &usageParser{f: f, st: st, msgs: map[string]*usageMsg{}, calls: map[string]*usageOpenCall{}, done: map[string]bool{},
		tools: map[toolAccKey]*usageToolRow{}, cli: map[usageCLIKey]int64{}}
	return p
}

func (p *usageParser) skip(kind string) {
	p.stats.Skipped++
	if p.stats.Unknown == nil {
		p.stats.Unknown = map[string]int64{}
	}
	if len(p.stats.Unknown) < 50 || p.stats.Unknown[kind] > 0 {
		p.stats.Unknown[kind]++
	}
}

// line handles one complete line.
func (p *usageParser) line(b []byte) {
	b = bytes.TrimRight(b, "\r\n")
	if len(b) == 0 {
		return
	}
	p.stats.Lines++
	switch {
	case bytes.Contains(b, markAssistant), bytes.Contains(b, markUser), bytes.Contains(b, markSystem), bytes.Contains(b, markTitle):
	default:
		if bytes.HasPrefix(b, markForkRef) {
			p.st.Fork = true
		}
		for _, m := range ignorable {
			if bytes.Contains(b, m) {
				p.stats.Ignored++
				return
			}
		}
		t := topLevelType(b)
		switch {
		case t == "":
			p.skip("(no type)")
		case knownTypes[t]:
			p.stats.Ignored++
		default:
			p.skip(t)
		}
		return
	}
	var l rawLine
	if err := json.Unmarshal(b, &l); err != nil {
		p.skip("(unreadable)")
		return
	}
	switch l.Type {
	case "assistant":
		p.meta(&l)
		p.assistant(b, &l)
	case "user":
		p.meta(&l)
		p.user(b, &l)
	case "system":
		p.meta(&l)
		if l.Subtype == "compact_boundary" {
			p.comp++
		}
		p.stats.Parsed++
	case "ai-title":
		if l.AiTitle != "" {
			p.st.Title = l.AiTitle
		}
		p.stats.Parsed++
	default:
		if knownTypes[l.Type] {
			p.stats.Ignored++
		} else {
			p.skip(usageOr(l.Type, "(no type)"))
		}
	}
}

func usageOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (p *usageParser) meta(l *rawLine) {
	if l.Cwd != "" && p.st.Cwd == "" {
		p.st.Cwd = l.Cwd
	}
	if l.GitBranch != "" {
		p.st.Branch = l.GitBranch
	}
	if l.Version != "" {
		p.st.Version = l.Version
	}
	if ts := parseTS(l.Timestamp); ts > 0 {
		if p.first == 0 || ts < p.first {
			p.first = ts
		}
		p.last = max(p.last, ts)
	}
}

func parseTS(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func hourOf(ms int64) int64 { return ms - ms%3_600_000 }

func cleanModel(m string) string {
	if i := strings.IndexByte(m, '['); i > 0 {
		m = m[:i]
	}
	return strings.TrimSpace(m)
}

func (p *usageParser) assistant(b []byte, l *rawLine) {
	p.stats.Parsed++
	m := l.Message
	if m == nil {
		return
	}
	ts := parseTS(l.Timestamp)
	if ts == 0 {
		ts = p.last
	}
	model := cleanModel(m.Model)
	if m.Usage != nil && m.ID != "" && model != "" && !strings.HasPrefix(model, "<") {
		u := usageSeenMsg{ID: m.ID, In: m.Usage.Input, Out: m.Usage.Output, CR: m.Usage.CacheRead}
		if cc := m.Usage.CC; cc != nil && cc.E5m+cc.E1h > 0 {
			u.CW5, u.CW1 = cc.E5m, cc.E1h
			if rest := m.Usage.CacheCreation - cc.E5m - cc.E1h; rest > 0 {
				u.CW5 += rest
			}
		} else {
			u.CW5 = m.Usage.CacheCreation
		}
		cur := p.msgs[m.ID]
		if cur == nil {
			cur = &usageMsg{id: m.ID, model: model, ts: ts}
			p.msgs[m.ID] = cur
			p.order = append(p.order, m.ID)
		}
		cur.u = u // the last line of a message has its final usage
	}
	if !p.st.CLICounted && ts > 0 && model != "" && !strings.HasPrefix(model, "<") {
		p.st.CLICounted = true
		p.addCLI(hourOf(ts), model, usageCLIFile, "", "", 1)
	}
	if !bytes.Contains(b, markToolUse) {
		return
	}
	var blocks []rawBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for i := range blocks {
		bl := &blocks[i]
		if bl.Type != "tool_use" || bl.ID == "" || bl.Name == "" || p.calls[bl.ID] != nil || (p.st.Open != nil && p.st.Open[bl.ID] != nil) {
			continue
		}
		call := classifyCall(bl.Name, bl.Input)
		call.Hour, call.Model = hourOf(ts), model
		if p.f.Main && !l.Sidechain {
			var ctx int64
			if m.Usage != nil {
				ctx = m.Usage.Input + m.Usage.CacheCreation + m.Usage.CacheRead
			}
			if p.st.Trace == nil {
				p.st.Trace = &traceState{}
			}
			call.T = classifyTraceCall(bl.Name, bl.Input, usageOr(l.Cwd, p.st.Cwd), ts, ctx, model, &p.st.Trace.Scripts)
		}
		p.calls[bl.ID] = call
		p.stats.Calls++
		for _, k := range call.Keys {
			p.tool(call.Hour, call.Model, k).Calls++
		}
		p.noteCLI(call)
	}
}

func (p *usageParser) tool(hour int64, model string, k usageToolKey) *usageToolRow {
	key := toolAccKey{hour, model, k}
	r := p.tools[key]
	if r == nil {
		r = &usageToolRow{Hour: hour, Model: model, Kind: k.Kind, Tool: k.Tool, Detail: k.Detail}
		p.tools[key] = r
	}
	return r
}

// classifyCall names a tool call's rows and keeps what a refusal would show.
func classifyCall(name string, input json.RawMessage) *usageOpenCall {
	var in rawInput
	_ = json.Unmarshal(input, &in)
	call := &usageOpenCall{Tool: name}
	switch {
	case strings.HasPrefix(name, "mcp__"):
		parts := strings.SplitN(name, "__", 3)
		server := parts[1]
		call.Keys = []usageToolKey{{Kind: "mcp", Tool: name, Detail: server}}
		if len(parts) == 3 {
			call.Detail = parts[2]
		}
		call.Excerpt = compactJSON(input)
	case name == "Skill":
		call.Keys = []usageToolKey{{Kind: "builtin", Tool: name}}
		if in.Skill != "" {
			call.Keys = append(call.Keys, usageToolKey{Kind: "skill", Detail: in.Skill})
		}
		call.Detail, call.Excerpt = in.Skill, in.Skill
	case name == "Agent" || name == "Task":
		typ := usageOr(in.SubagentType, "general-purpose")
		call.Keys = []usageToolKey{{Kind: "builtin", Tool: name}, {Kind: "subagent", Detail: typ}}
		call.Detail, call.Excerpt = typ, typ+": "+in.Description
	case name == "Bash":
		call.Keys = []usageToolKey{{Kind: "builtin", Tool: name}}
		heads := CommandHeads(in.Command)
		for _, h := range heads {
			call.Keys = append(call.Keys, usageToolKey{Kind: "bash", Tool: name, Detail: h})
		}
		call.Detail, call.Excerpt = mainHead(heads), in.Command
		call.shell = readCLIShell(in.Command)
		call.Risky = riskyCommand(in.Command)
		if call.Risky != nil {
			// redact before cutting, so a cut never leaves the start of a secret
			full := truncRunes(in.Command, riskyCommandRunes)
			if strings.Contains(full, call.Risky.Segment) {
				call.Risky.Segment = ""
			} else {
				call.Risky.Segment = truncRunes(redact(call.Risky.Segment), riskyCommandRunes)
			}
			call.Risky.Command = truncRunes(redact(in.Command), riskyCommandRunes)
		}
	default:
		call.Keys = []usageToolKey{{Kind: "builtin", Tool: name}}
		switch {
		case in.FilePath != "":
			call.Excerpt = in.FilePath
		case len(input) > 0:
			call.Excerpt = compactJSON(input)
		}
	}
	// Redacted past the cut, so the cut leaves no token's start, without
	// running the patterns over a whole file's content.
	call.Excerpt = truncRunes(redact(truncRunes(call.Excerpt, 4*excerptRunes)), excerptRunes)
	return call
}

// mainHead is the head a refusal is filed under: the first that isn't
// plumbing around the real command.
func mainHead(heads []string) string {
	for _, h := range heads {
		switch h {
		case "cd", "echo", "true", "export", "set", "source", "test", "(variable)":
			continue
		}
		return h
	}
	if len(heads) > 0 {
		return heads[0]
	}
	return ""
}

func compactJSON(b json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return ""
	}
	return buf.String()
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (p *usageParser) user(b []byte, l *rawLine) {
	p.stats.Parsed++
	if l.Message == nil || len(l.Message.Content) == 0 {
		return
	}
	ts := parseTS(l.Timestamp)
	if ts == 0 {
		ts = p.last
	}
	content := l.Message.Content
	if content[0] == '"' {
		var text string
		if bytes.Contains(b, markCommand) && json.Unmarshal(content, &text) == nil {
			p.command(text, ts)
		}
		return
	}
	hasResult, hasCommand := bytes.Contains(b, markResult), bytes.Contains(b, markCommand)
	if !hasResult && !hasCommand {
		return
	}
	var blocks []rawBlock
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for i := range blocks {
		bl := &blocks[i]
		switch bl.Type {
		case "text":
			if hasCommand {
				p.command(bl.Text, ts)
			}
		case "tool_result":
			p.result(bl, l.PermissionDecision, ts)
		}
	}
}

// command counts a slash command typed by the person.
func (p *usageParser) command(text string, ts int64) {
	head := text
	if len(head) > 600 {
		head = head[:600]
	}
	if m := commandRE.FindStringSubmatch(head); m != nil {
		name := m[1]
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		p.tool(hourOf(ts), "", usageToolKey{Kind: "command", Detail: truncRunes(name, 100)}).Calls++
	}
}

func (p *usageParser) result(bl *rawBlock, pd *rawDecision, ts int64) {
	id := bl.ToolUseID
	call := p.calls[id]
	if call == nil && p.st.Open != nil {
		call = p.st.Open[id]
	}
	if call == nil || p.done[id] {
		return
	}
	p.done[id] = true
	text := ""
	if bl.IsError || pd != nil && pd.Decision == "reject" {
		text = resultText(bl.Content, 1600)
	}
	refused, prompted := false, false
	var reason, source string
	switch {
	case pd != nil && pd.Decision == "reject":
		refused = true
		reason, source = refusalReason(text, pd.Source, pd.ReasonType)
	case bl.IsError && looksRefused(text):
		refused = true
		reason, source = refusalReason(text, "", "")
	case pd != nil && pd.Decision == "accept" && strings.HasPrefix(pd.Source, "user"):
		prompted = true
	}
	size := int64(len(bl.Content))
	for _, k := range call.Keys {
		r := p.tool(call.Hour, call.Model, k)
		r.Bytes += size
		switch {
		case refused:
			r.Refusals++
		case bl.IsError:
			r.Errors++
		}
		if prompted {
			r.Prompts++
		}
	}
	if refused {
		p.stats.Refusals++
		p.refs = append(p.refs, usageRefusalRow{At: ts, Session: p.f.Session, Model: call.Model, Tool: call.Tool, Detail: call.Detail,
			Reason: reason, Source: source, Excerpt: call.Excerpt})
	}
	if !refused {
		p.noteCLIResult(call, bl.Content)
	}
	if call.T != nil {
		p.traceResult(call.T, bl, refused)
	}
	if call.Risky != nil {
		outcome := "ran"
		switch {
		case refused:
			outcome = "refused"
		case bl.IsError:
			outcome = "error" // it ran, and failed: what it did may be partly done
		default:
			source = ""
		}
		p.addRisky(call, ts, outcome, source)
	}
}

func (p *usageParser) addRisky(call *usageOpenCall, at int64, outcome, source string) {
	h := call.Risky
	p.risky = append(p.risky, usageRiskyRow{At: at, Session: p.f.Session, Model: call.Model, Kind: h.Kind, Level: h.Level, Label: h.Label,
		Command: orStr(h.Command, call.Excerpt), Segment: h.Segment, Outcome: outcome, Source: source})
}

func looksRefused(text string) bool {
	return strings.HasPrefix(text, "Permission for this") || strings.Contains(text, "denied by the Claude Code auto mode classifier") ||
		strings.Contains(text, "hook error:") && strings.Contains(text, "Blocked") ||
		strings.HasPrefix(text, "The user doesn't want to proceed") || strings.HasPrefix(text, "Permission to use ")
}

// resultText is the start of a tool result's text.
func resultText(raw json.RawMessage, n int) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if raw[0] == '"' {
		if json.Unmarshal(raw, &s) == nil {
			return truncRunes(s, n)
		}
		return ""
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			b.WriteString(x.Text)
			if b.Len() >= n {
				break
			}
		}
		return truncRunes(b.String(), n)
	}
	return ""
}

// refusalReason sorts a refusal: the classifier's category, the hook that
// blocked it, a built-in safety check, a person saying no, or a rule.
func refusalReason(text, decisionSource, reasonType string) (reason, source string) {
	switch {
	case strings.Contains(text, "auto mode classifier"):
		if m := reasonRE.FindStringSubmatch(text); m != nil {
			return m[1], "classifier"
		}
		return "Classifier (no reason given)", "classifier"
	case reasonType == "hook" || decisionSource == "hook" || strings.Contains(text, "hook error:"):
		name := "hook"
		if m := hookRE.FindStringSubmatch(text); m != nil {
			f := strings.Fields(m[1])
			if len(f) > 0 {
				name = path.Base(f[len(f)-1])
			}
		}
		return "hook: " + name, "hook"
	case reasonType == "safetyCheck" || strings.Contains(text, "built-in Claude Code safety check"):
		return "Built-in safety check", "safety_check"
	case strings.HasPrefix(decisionSource, "user") || strings.HasPrefix(text, "The user doesn't want to proceed"):
		return "Declined by you", "user"
	case reasonType == "classifier":
		return "Classifier (no reason given)", "classifier"
	}
	return "Denied by a permission rule", "rule"
}

// finish settles the run: message usage as differences against what was
// counted before, open calls to carry over, and the chunk's rows.
func (p *usageParser) finish(out *usageParsed, repoOf func(string) string) {
	st := &out.State
	counted := map[string]usageSeenMsg{}
	for _, m := range st.Last {
		counted[m.ID] = m
	}
	repo := st.Repo
	if repo == "" && st.Cwd != "" && repoOf != nil {
		repo = repoOf(st.Cwd)
		st.Repo = repo
	}
	type mk struct {
		hour  int64
		model string
	}
	acc := map[mk]*usageMsgRow{}
	led := map[ledgerKey]*usageLedgerRow{}
	kind := p.f.Kind
	for _, id := range p.order {
		m := p.msgs[id]
		prev, seen := counted[id]
		// A message's totals only grow. A rewritten one with smaller totals adds
		// nothing, and the larger totals already counted stay the baseline.
		d := usageSeenMsg{In: max(m.u.In-prev.In, 0), Out: max(m.u.Out-prev.Out, 0), CW5: max(m.u.CW5-prev.CW5, 0), CW1: max(m.u.CW1-prev.CW1, 0), CR: max(m.u.CR-prev.CR, 0)}
		if seen {
			m.u.In, m.u.Out, m.u.CW5, m.u.CW1, m.u.CR = max(m.u.In, prev.In), max(m.u.Out, prev.Out), max(m.u.CW5, prev.CW5), max(m.u.CW1, prev.CW1), max(m.u.CR, prev.CR)
		}
		if seen && d == (usageSeenMsg{}) {
			continue
		}
		key := mk{hourOf(m.ts), m.model}
		r := acc[key]
		if r == nil {
			r = &usageMsgRow{Hour: key.hour, Session: p.f.Session, Repo: repo, Model: m.model, Kind: kind}
			acc[key] = r
		}
		if !seen {
			r.N++
			p.stats.Messages++
			p.ledger(led, key.hour, key.model, repo, kind, m)
		}
		r.Input += d.In
		r.Output += d.Out
		r.CW5 += d.CW5
		r.CW1 += d.CW1
		r.CR += d.CR
	}
	for _, r := range led {
		out.Chunk.Ledger = append(out.Chunk.Ledger, *r)
	}
	sort.Slice(out.Chunk.Ledger, func(i, j int) bool {
		a, b := out.Chunk.Ledger[i], out.Chunk.Ledger[j]
		return a.Hour < b.Hour || a.Hour == b.Hour && a.Model < b.Model
	})
	for _, r := range acc {
		out.Chunk.Messages = append(out.Chunk.Messages, *r)
	}
	sort.Slice(out.Chunk.Messages, func(i, j int) bool {
		a, b := out.Chunk.Messages[i], out.Chunk.Messages[j]
		return a.Hour < b.Hour || a.Hour == b.Hour && a.Model < b.Model
	})
	// The last messages carry over to the next run so one seen again there is not counted twice.
	var merged []usageSeenMsg
	for _, m := range st.Last {
		if _, inRun := p.msgs[m.ID]; !inRun {
			merged = append(merged, m)
		}
	}
	for _, id := range p.order {
		merged = append(merged, p.msgs[id].u)
	}
	st.Last = merged[max(0, len(merged)-usageKeepLast):]
	open := map[string]*usageOpenCall{}
	for id, c := range st.Open {
		if !p.done[id] {
			open[id] = c
		}
	}
	for id, c := range p.calls {
		if !p.done[id] {
			open[id] = c
		}
	}
	newest := p.last
	if newest == 0 {
		newest = time.Now().UnixMilli()
	}
	cut := newest - usageOpenMaxAge.Milliseconds()
	for id, c := range open {
		if c.Hour < cut {
			p.dropOpen(c)
			delete(open, id)
		}
	}
	if len(open) > usageKeepOpen {
		ids := make([]string, 0, len(open))
		for id := range open {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return open[ids[i]].Hour > open[ids[j]].Hour })
		for _, id := range ids[usageKeepOpen:] {
			p.dropOpen(open[id])
			delete(open, id)
		}
	}
	if len(open) == 0 {
		open = nil
	}
	st.Open = open
	for _, r := range p.tools {
		r.Session, r.Repo = p.f.Session, repo
		out.Chunk.Tools = append(out.Chunk.Tools, *r)
	}
	sort.Slice(out.Chunk.Tools, func(i, j int) bool {
		a, b := out.Chunk.Tools[i], out.Chunk.Tools[j]
		if a.Hour != b.Hour {
			return a.Hour < b.Hour
		}
		return a.Kind+a.Tool+a.Detail+a.Model < b.Kind+b.Tool+b.Detail+b.Model
	})
	for _, r := range p.refs {
		r.Repo = repo
		out.Chunk.Refusals = append(out.Chunk.Refusals, r)
	}
	for _, r := range p.risky {
		r.Repo = repo
		out.Chunk.Risky = append(out.Chunk.Risky, r)
	}
	out.Chunk.CLI = p.cliRows(repo)
	out.Chunk.Traces = p.traceFinish()
	if out.LedgerOnly || out.RiskyOnly || out.CLIOnly || out.TraceOnly { // Messages, tools, refusals and sessions were counted on the file's first read; a ledger-only or trace-only pass must not count them again.
		out.Chunk.Messages, out.Chunk.Tools, out.Chunk.Refusals, out.Chunk.Sessions = nil, nil, nil, nil
		if !out.CLIOnly {
			out.Chunk.CLI = nil
		}
		if !out.LedgerOnly {
			out.Chunk.Ledger = nil
		}
		if !out.RiskyOnly {
			out.Chunk.Risky = nil
		}
		if !out.TraceOnly {
			out.Chunk.Traces = nil
		}
		return
	}
	if p.f.Main && (p.first > 0 || p.comp > 0 || st.Title != "") && out.Changed {
		out.Chunk.Sessions = append(out.Chunk.Sessions, usageSessionRow{Session: p.f.Session, Cwd: st.Cwd, Repo: repo, Branch: st.Branch,
			Version: st.Version, Title: st.Title, First: p.first, Last: p.last, Compactions: p.comp})
	}
}

// dropOpen lets go of a call whose result never came; a risky one is
// still logged, with no outcome.
func (p *usageParser) dropOpen(c *usageOpenCall) {
	if c.Risky != nil {
		p.addRisky(c, c.Hour, "unknown", "")
	}
}

// topLevelType finds the "type" of a JSON object line without decoding
// it, for lines whose type isn't one the parser reads.
func topLevelType(b []byte) string {
	depth := 0
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '"':
			j := i + 1
			for j < len(b) && b[j] != '"' {
				if b[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(b) {
				return ""
			}
			if depth == 1 && string(b[i+1:j]) == "type" && j+1 < len(b) && b[j+1] == ':' {
				k := j + 2
				if k < len(b) && b[k] == '"' {
					e := bytes.IndexByte(b[k+1:], '"')
					if e >= 0 {
						return string(b[k+1 : k+1+e])
					}
				}
				return ""
			}
			i = j
		}
	}
	return ""
}

// Cache lifetimes: the default 5 minute one, or the 1 hour one. A call after
// a longer gap than the cache lived re-writes the context.
const (
	cacheTTL5m = 5 * 60 * 1000
	cacheTTL1h = 60 * 60 * 1000
)

type ledgerKey struct {
	hour  int64
	model string
}

// ledger adds one newly seen call to the hour's ledger row (WP-35).
func (p *usageParser) ledger(led map[ledgerKey]*usageLedgerRow, hour int64, model, repo, kind string, m *usageMsg) {
	k := ledgerKey{hour, model}
	r := led[k]
	if r == nil {
		r = &usageLedgerRow{Hour: hour, Session: p.f.Session, Repo: repo, Model: model, Kind: kind}
		led[k] = r
	}
	ctx := m.u.In + m.u.CW5 + m.u.CW1 + m.u.CR
	r.Calls++
	r.Ctx += ctx
	if ctx > 200_000 {
		r.Over200++
	}
	if ctx > 300_000 {
		r.Over300++
		r.CR300 += m.u.CR
	}
	st := p.st
	if !p.f.Main && !st.Spawned {
		st.Spawned = true
		if st.Fork {
			r.Forks++
			r.ForkCtx += ctx
		} else {
			r.Fresh++
			r.FreshCtx += ctx
		}
	}
	if st.PrevTS > 0 && m.ts > 0 {
		ttl := int64(cacheTTL5m)
		if st.PrevLong {
			ttl = cacheTTL1h
		}
		if w := m.u.CW5 + m.u.CW1; w > 0 && m.ts-st.PrevTS > ttl {
			r.Cold++
			r.ColdW5 += m.u.CW5
			r.ColdW1 += m.u.CW1
		}
	}
	if m.ts > 0 {
		st.PrevTS, st.PrevLong = m.ts, m.u.CW1 > 0
	}
}
