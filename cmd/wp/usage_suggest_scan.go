package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// sugCall is one tool call as the checks need it.
type sugCall struct {
	Tool    string   // Read, Bash, Grep, mcp__server__tool…
	Command string   // Bash
	Heads   []string // Bash: command heads
	Path    string   // Read/Edit/Write: file_path; Grep/Glob: path
	Pattern string   // Grep/Glob: pattern
	Agent   string   // Agent/Task: subagent type
	Skill   string   // Skill: its name
	Err     bool
	ErrText string // the start of an error result
	Bytes   int64  // size of the result
	Ctx     int64  // context of the request that made the call
	Nudge   string // the chain nudge (chainKind) was added to the call
	id      string
	msg     string
}

// sugSession is one transcript: a session's main thread or one subagent.
type sugSession struct {
	ID          string
	Kind        string // main, or the subagent's type
	Cwd         string
	Model       string // the model of most of its messages
	Messages    int
	StartCtx    int64 // context of the first message: system prompt, tools, instructions, memory
	PeakCtx     int64
	Over        int   // messages whose context was over sugCtxCeiling
	OverTokens  int64 // and how far over, summed
	Compactions int
	FirstAt     int64 // ms
	LastAt      int64 // ms
	Calls       []sugCall
	Tokens      sugTokens
}

type sugTokens struct{ In, Out, CW, CR int64 }

const (
	sugCtxCeiling = 200_000
	sugErrRunes   = 400
)

// sugRepo says whether a transcript's directory belongs to the repository
// the report is about: the same origin, or (for a worktree deleted since)
// a sibling directory named after the main checkout (waypoint-wp193 next
// to waypoint).
type sugRepo struct {
	key      string
	mainRoot string
	resolver *repoResolver
}

func (r *sugRepo) has(cwd string) bool {
	if cwd == "" {
		return false
	}
	if r.resolver.of(cwd) == r.key {
		return true
	}
	if _, err := os.Stat(cwd); err == nil {
		return false
	}
	base, parent := filepath.Base(r.mainRoot), filepath.Dir(r.mainRoot)
	for d := filepath.Clean(cwd); d != filepath.Dir(d); d = filepath.Dir(d) {
		if filepath.Dir(d) == parent {
			b := filepath.Base(d)
			return b == base || strings.HasPrefix(b, base+"-")
		}
	}
	return false
}

// scanSuggestSessions reads every transcript changed within since whose
// session ran in the repository.
func scanSuggestSessions(root string, since time.Duration, repo *sugRepo) ([]*sugSession, error) {
	files, err := discoverTranscripts(root, since, nil)
	if err != nil {
		return nil, err
	}
	cut := time.Now().Add(-since).UnixMilli()
	var mains, subs []usageFile
	for _, f := range files {
		if f.Main {
			mains = append(mains, f)
		} else {
			subs = append(subs, f)
		}
	}
	out := readSuggestFiles(mains, cut, func(_ usageFile, cwd string) bool { return repo.has(cwd) })
	mainIn := map[string]bool{}
	for _, s := range out {
		mainIn[s.ID] = true
	}
	out = append(out, readSuggestFiles(subs, cut, func(f usageFile, cwd string) bool {
		return mainIn[f.Session] || repo.has(cwd)
	})...)
	return out, nil
}

// readSuggestFiles reads transcripts in parallel; inRepo must be safe to
// call from several goroutines.
func readSuggestFiles(files []usageFile, cut int64, inRepo func(f usageFile, cwd string) bool) []*sugSession {
	res := make([]*sugSession, len(files))
	next := make(chan int)
	var wg sync.WaitGroup
	for range max(1, min(runtime.NumCPU(), 8)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f := files[i]
				s, err := readSuggestFile(f, cut, func(cwd string) bool { return inRepo(f, cwd) })
				if err == nil {
					res[i] = s
				}
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	var out []*sugSession
	for _, s := range res {
		if s != nil {
			out = append(out, s)
		}
	}
	return out
}

type sugRawInput struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	Path         string `json:"path"`
	Pattern      string `json:"pattern"`
	SubagentType string `json:"subagent_type"`
	Skill        string `json:"skill"`
}

// readSuggestFile reads one transcript, or returns nil when its directory
// is not in the repository or nothing in it is recent enough.
func readSuggestFile(f usageFile, cut int64, inRepo func(cwd string) bool) (*sugSession, error) {
	fh, err := os.Open(f.Path) // #nosec G304 -- a transcript Claude Code wrote
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	s := &sugSession{ID: f.Session, Kind: f.Kind}
	type msg struct {
		model string
		ctx   int64
		t     sugTokens
	}
	msgs := map[string]*msg{}
	var order []string
	open := map[string]int{}
	nudged := map[string]bool{}
	decided, recent := false, false
	br := bufio.NewReaderSize(fh, 1<<20)
	var long []byte
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long = append(long, line...)
			continue
		}
		if len(long) > 0 {
			line, long = append(long, line...), nil
		}
		if len(line) > 0 && bytes.Contains(line, markAttachment) && bytes.Contains(line, markChainNote) {
			if id := chainNoteCall(line); id != "" {
				nudged[id] = true
			}
		}
		if len(line) > 0 && (bytes.Contains(line, markAssistant) || bytes.Contains(line, markUser) || bytes.Contains(line, markSystem)) {
			var l rawLine
			if json.Unmarshal(line, &l) == nil {
				if !decided && l.Cwd != "" {
					if !inRepo(l.Cwd) {
						return nil, nil
					}
					decided, s.Cwd = true, l.Cwd
				}
				if ts := parseTS(l.Timestamp); ts > 0 {
					recent = recent || ts >= cut
					s.LastAt = max(s.LastAt, ts)
					if s.FirstAt == 0 || ts < s.FirstAt {
						s.FirstAt = ts
					}
				}
				switch l.Type {
				case "system":
					if l.Subtype == "compact_boundary" {
						s.Compactions++
					}
				case "assistant":
					if m := l.Message; m != nil {
						if m.Usage != nil && m.ID != "" && !strings.HasPrefix(m.Model, "<") {
							cur := msgs[m.ID]
							if cur == nil {
								cur = &msg{model: cleanModel(m.Model)}
								msgs[m.ID] = cur
								order = append(order, m.ID)
							}
							u := m.Usage
							cur.ctx = u.Input + u.CacheCreation + u.CacheRead
							cur.t = sugTokens{u.Input, u.Output, u.CacheCreation, u.CacheRead}
						}
						if bytes.Contains(line, markToolUse) {
							var blocks []rawBlock
							if json.Unmarshal(m.Content, &blocks) == nil {
								for _, bl := range blocks {
									if bl.Type != "tool_use" || bl.ID == "" {
										continue
									}
									if _, dup := open[bl.ID]; dup {
										continue
									}
									open[bl.ID] = len(s.Calls)
									call := suggestCall(bl.Name, bl.Input)
									call.id, call.msg = bl.ID, m.ID
									s.Calls = append(s.Calls, call)
								}
							}
						}
					}
				case "user":
					if m := l.Message; m != nil && bytes.Contains(line, markResult) && len(m.Content) > 0 && m.Content[0] == '[' {
						var blocks []rawBlock
						if json.Unmarshal(m.Content, &blocks) == nil {
							for _, bl := range blocks {
								i, ok := open[bl.ToolUseID]
								if bl.Type != "tool_result" || !ok {
									continue
								}
								c := &s.Calls[i]
								c.Bytes = int64(len(bl.Content))
								if bl.IsError {
									c.Err = true
									c.ErrText = resultText(bl.Content, sugErrRunes)
								}
							}
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	if !decided || !recent {
		return nil, nil
	}
	for i := range s.Calls {
		c := &s.Calls[i]
		if m := msgs[c.msg]; m != nil {
			c.Ctx = m.ctx
		}
		if nudged[c.id] {
			c.Nudge = chainKind
		}
	}
	models := map[string]int{}
	for i, id := range order {
		m := msgs[id]
		s.Messages++
		models[m.model]++
		if i == 0 {
			s.StartCtx = m.ctx
		}
		s.PeakCtx = max(s.PeakCtx, m.ctx)
		if m.ctx > sugCtxCeiling {
			s.Over++
			s.OverTokens += m.ctx - sugCtxCeiling
		}
		s.Tokens.In += m.t.In
		s.Tokens.Out += m.t.Out
		s.Tokens.CW += m.t.CW
		s.Tokens.CR += m.t.CR
	}
	best := 0
	for m, n := range models {
		if n > best || n == best && m < s.Model {
			s.Model, best = m, n
		}
	}
	return s, nil
}

func suggestCall(name string, input json.RawMessage) sugCall {
	var in sugRawInput
	_ = json.Unmarshal(input, &in)
	c := sugCall{Tool: name}
	switch name {
	case "Bash":
		c.Command = in.Command
		c.Heads = CommandHeads(in.Command)
	case "Grep", "Glob":
		c.Pattern, c.Path = in.Pattern, in.Path
	case "Agent", "Task":
		c.Agent = usageOr(in.SubagentType, "general-purpose")
	case "Skill":
		c.Skill = in.Skill
	default:
		c.Path = in.FilePath
	}
	return c
}

// chainNoteCall is the tool call a transcript line shows the search-chain
// nudge was added to, or "": the hook_additional_context attachment Claude
// Code records for what a PreToolUse hook added, or the hook_success one
// carrying the hook's output, when its text carries the chain note. The
// hook's command isn't matched: Claude Code 2.1.3xx records it as written in
// settings on some lines and as "command hook N" on others.
func chainNoteCall(line []byte) string {
	var a struct {
		Attachment struct {
			Type      string          `json:"type"`
			HookName  string          `json:"hookName"`
			ToolUseID string          `json:"toolUseID"`
			Content   json.RawMessage `json:"content"`
			Stdout    string          `json:"stdout"`
		} `json:"attachment"`
	}
	if json.Unmarshal(line, &a) != nil || !strings.HasPrefix(a.Attachment.HookName, "PreToolUse") {
		return ""
	}
	switch a.Attachment.Type {
	case "hook_additional_context":
		if bytes.Contains(a.Attachment.Content, markChainNote) {
			return a.Attachment.ToolUseID
		}
	case "hook_success":
		if strings.Contains(a.Attachment.Stdout, string(markChainNote)) {
			return a.Attachment.ToolUseID
		}
	}
	return ""
}
