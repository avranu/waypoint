package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// cmpEvent is one thing in a session's main conversation.
type cmpEvent struct {
	kind byte
	ts   int64

	model           string
	in, out, cw, cr int64
	cw1h            int64
	split           bool

	chars int

	pk     byte
	review bool
	names  []string

	edit, lookup, orient bool
	paths                []string
	shell                bool
	key                  string
	own                  bool
	agent                bool
	adversarial          bool
	failed               bool
	inChars, resultChars int

	trigger      string
	pre          int64
	durMs        int64
	summaryChars int
	usd          float64
	restored     []string
	referenced   []string
	kept         []int
}

func (e *cmpEvent) ctx() int64 { return e.in + e.cw + e.cr }

var (
	// shell commands that only look something up
	cmpLookupRE = lazyre.New(`^(pwd|cat|head|tail|sed -n|less|ls|find|grep|rg|wc|tree|stat|jq|go doc|git (status|log|diff|show|branch|blame|rev-parse|ls-files|worktree list)|wp (show|next|brief|code|ls|spec|define|worklog|trace|usage|pr (show|check|log|todo)|page show|design (show|ls)|ci (status|log)))\b`)
	// those that ask where the work stands
	cmpOrientRE = lazyre.New(`^(pwd|ls|git (status|log|branch|rev-parse|worktree list|diff)|wp (next|show|brief|worklog|pr (show|check|todo)))\b`)
	// those that print a file
	cmpShowRE = lazyre.New(`^(cat|head|tail|sed -n|less)\b`)
	// a file such a command names: a path anywhere in it, or a bare name
	// with an extension (cat main.go) in its first simple command
	cmpPathRE = lazyre.New(`(?:^|\s)((?:~?/|\.{0,2}/)?[\w.@-]+(?:/[\w.@-]+)+\.\w{1,6})`)
	cmpBareRE = lazyre.New(`\s([\w@-][\w.@-]*\.[A-Za-z]\w{0,5})(?:\s|$)`)
	cmpCdRE   = lazyre.New(`^(cd [^;&]+(&&|;)\s*)+`)
	// a file name in a prompt's text
	cmpNameRE = lazyre.New(`[\w@-][\w.@-]*\.[A-Za-z]\w{0,5}\b`)
)

// cmpReviewPrompt starts the prompt Waypoint types for a review round. Read by
// its visible words, the one exception to WP-1370's digest rule: this is a
// local report over transcripts, where no typed record exists to ask.
const cmpReviewPrompt = "Waypoint autopilot review of "

// cmpPromptKind tells a prompt someone typed (0) from a report Claude Code
// or Waypoint types when a background task or subagent ends ('n') and from
// text that asks for no work ('x': a keep-warm ping, an interruption).
func cmpPromptKind(text string) byte {
	t := strings.TrimSpace(core.UnwrapPasted(text))
	switch {
	case strings.HasPrefix(t, "<task-notification"), strings.HasPrefix(t, "[SYSTEM NOTIFICATION"), strings.HasPrefix(t, "Another Claude session sent a message"):
		return 'n'
	case strings.HasPrefix(t, keepWarmMark), strings.HasPrefix(t, "[Request interrupted"):
		return 'x'
	}
	return 0
}

// cmpRedirect reads a command's first simple command: the file its output
// goes to (> FILE, >> FILE; not a descriptor like 2> or >&2, not /dev/null)
// and whether it reads a here-document. Quoted text is skipped.
func cmpRedirect(s string) (target string, heredoc bool) {
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == '\\':
			i++
		case ch == '&' && i+1 < len(s) && s[i+1] == '>':
			continue
		case ch == '|' || ch == ';' || ch == '&':
			return target, heredoc
		case ch == '<':
			if strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<") {
				heredoc = true
			}
			for i+1 < len(s) && s[i+1] == '<' {
				i++
			}
		case ch == '>':
			descriptor := i > 0 && s[i-1] >= '0' && s[i-1] <= '9' && (i == 1 || s[i-2] == ' ')
			j := i + 1
			if j < len(s) && s[j] == '>' {
				j++
			}
			if j < len(s) && s[j] == '&' {
				i = j
				continue
			}
			t := strings.TrimLeft(s[j:], " ")
			if k := strings.IndexAny(t, " <>|;&"); k >= 0 {
				t = t[:k]
			}
			if t = strings.Trim(t, `"'`); !descriptor && t != "" && t != "/dev/null" && target == "" {
				target = t
			}
			i = j - 1
		}
	}
	return target, heredoc
}

// cmpOwnTranscript reports a path or command that names a Claude Code
// transcript: after a compaction the summary tells the agent where its own is.
func cmpOwnTranscript(s string) bool {
	return strings.Contains(s, ".jsonl") && strings.Contains(s, "/.claude/projects/")
}

// cmpTool classifies a tool call.
func cmpTool(name string, input json.RawMessage) cmpEvent {
	e := cmpEvent{kind: 't', inChars: len(input)}
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Pattern  string `json:"pattern"`
		Path     string `json:"path"`
		Subagent string `json:"subagent_type"`
	}
	_ = json.Unmarshal(input, &in)
	switch name {
	case "Agent", "Task":
		e.agent = true
		e.adversarial = strings.Contains(strings.ToLower(in.Subagent), "adversarial")
	case "Read":
		e.lookup, e.own = true, cmpOwnTranscript(in.FilePath)
		if in.FilePath != "" {
			e.paths = []string{filepath.Base(in.FilePath)}
		}
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		e.edit = true
		if in.FilePath != "" {
			e.paths = []string{filepath.Base(in.FilePath)}
		}
	case "Grep", "Glob":
		e.lookup, e.key = true, name+" "+in.Pattern+" "+in.Path
	case "Bash":
		raw := cmpCdRE.ReplaceAllString(strings.TrimSpace(in.Command), "")
		cmd := strings.Join(strings.Fields(raw), " ")
		e.edit = bashWrites(in.Command)
		e.lookup = !e.edit && cmpLookupRE.MatchString(cmd)
		if e.lookup {
			// cat > FILE <<EOF writes a file and cat <<EOF prints the text it
			// was given: neither puts a file in context
			line, _, _ := strings.Cut(raw, "\n")
			if target, heredoc := cmpRedirect(line); target != "" {
				e.edit, e.lookup, e.shell = true, false, true
				if !strings.Contains(target, "$") {
					e.paths = []string{filepath.Base(target)}
				}
			} else if heredoc {
				e.lookup = false
			}
		}
		e.orient = e.lookup && cmpOrientRE.MatchString(cmd)
		if e.lookup && cmpShowRE.MatchString(cmd) {
			for _, m := range cmpPathRE.FindAllStringSubmatch(cmd, -1) {
				e.paths = append(e.paths, filepath.Base(m[1]))
			}
			// Bare names count only before the first pipe or separator; spaces are doubled so adjacent names each get a space on both sides.
			first := cmd
			if i := strings.IndexAny(first, "|;&"); i >= 0 {
				first = first[:i]
			}
			for _, m := range cmpBareRE.FindAllStringSubmatch(strings.ReplaceAll(first, " ", "  "), -1) {
				if !slices.Contains(e.paths, m[1]) {
					e.paths = append(e.paths, m[1])
				}
			}
		}
		if e.lookup {
			e.own, e.shell = cmpOwnTranscript(cmd), len(e.paths) > 0
			if len(e.paths) == 0 {
				e.key = cmd
			}
		}
	}
	return e
}

type cmpLine struct {
	UUID             string `json:"uuid"`
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	Timestamp        string `json:"timestamp"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Usage   *rawUsage       `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Attachment *struct {
		Type     string `json:"type"`
		Filename string `json:"filename"`
	} `json:"attachment"`
	CompactMetadata *struct {
		Trigger    string `json:"trigger"`
		PreTokens  int64  `json:"preTokens"`
		DurationMs int64  `json:"durationMs"`
		// the last messages Claude Code leaves in place after the summary
		PreservedMessages *struct {
			UUIDs []string `json:"uuids"`
		} `json:"preservedMessages"`
	} `json:"compactMetadata"`
}

var markBoundary = []byte(`"compact_boundary"`)

// readCompactFile reads a main transcript into its events.
func readCompactFile(path string) ([]cmpEvent, error) {
	fh, err := os.Open(path) // #nosec G304 -- a transcript Claude Code wrote
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	var ev []cmpEvent
	calls := map[string]int{}
	tools := map[string]int{}
	texts := map[string]bool{}
	byLine := map[string][]int{}
	lastBoundary, pendingSummary := -1, 0
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
		if len(line) > 0 && (bytes.Contains(line, markAssistant) || bytes.Contains(line, markUser) || bytes.Contains(line, markBoundary) || bytes.Contains(line, markAttachment)) {
			var l cmpLine
			if json.Unmarshal(line, &l) == nil && !l.IsSidechain {
				switch {
				case l.Type == "assistant" && l.Message != nil:
					i, ok := calls[l.Message.ID]
					if !ok || l.Message.ID == "" {
						ev = append(ev, cmpEvent{kind: 'c', ts: parseTS(l.Timestamp), model: cleanModel(l.Message.Model)})
						i = len(ev) - 1
						calls[l.Message.ID] = i
						pendingSummary = 0
					}
					if u := l.Message.Usage; u != nil {
						c := &ev[i]
						c.in, c.out, c.cw, c.cr = max(c.in, u.Input), max(c.out, u.Output), max(c.cw, u.CacheCreation), max(c.cr, u.CacheRead)
						if u.CC != nil {
							c.cw1h, c.split = max(c.cw1h, u.CC.E1h), true
						}
					}
					var blocks []rawBlock
					_ = json.Unmarshal(l.Message.Content, &blocks)
					for _, b := range blocks {
						switch b.Type {
						case "text":
							if k := l.Message.ID + "\x00" + b.Text; l.Message.ID != "" {
								if texts[k] {
									continue
								}
								texts[k] = true
							}
							ev[i].chars += len(b.Text)
						case "tool_use":
							if _, dup := tools[b.ID]; dup && b.ID != "" {
								continue
							}
							t := cmpTool(b.Name, b.Input)
							t.ts = ev[i].ts
							ev = append(ev, t)
							tools[b.ID] = len(ev) - 1
							if l.UUID != "" {
								byLine[l.UUID] = append(byLine[l.UUID], len(ev)-1)
							}
						}
					}
				case l.Type == "user" && l.Message != nil:
					text, results := cmpUserContent(l.Message.Content)
					switch {
					case l.IsCompactSummary:
						// the summary is written beside its boundary, on either side
						if lastBoundary >= 0 && ev[lastBoundary].summaryChars == 0 && lastBoundary == lastOfKind(ev, "cb") {
							ev[lastBoundary].summaryChars = len(text)
						} else {
							pendingSummary = len(text)
						}
					case len(results) > 0:
						for _, r := range results {
							if i, ok := tools[r.ToolUseID]; ok {
								ev[i].failed, ev[i].resultChars = r.IsError, len(resultText(r.Content, 1<<24))
							}
						}
					case l.IsMeta || text == "" || strings.HasPrefix(text, "<local-command") || strings.HasPrefix(text, "<command-"):
					default:
						p := cmpEvent{kind: 'p', ts: parseTS(l.Timestamp), chars: len(text), pk: cmpPromptKind(text), review: strings.HasPrefix(strings.TrimSpace(core.UnwrapPasted(text)), cmpReviewPrompt), names: cmpNames(text)}
						ev = append(ev, p)
					}
				case l.Type == "attachment" && l.Attachment != nil:
					// Claude Code's restored-file attachments come after a compaction and before the next call
					if lastBoundary < 0 || lastBoundary != lastOfKind(ev, "cb") || l.Attachment.Filename == "" {
						break
					}
					switch name := filepath.Base(l.Attachment.Filename); l.Attachment.Type {
					case "file":
						ev[lastBoundary].restored = append(ev[lastBoundary].restored, name)
					case "compact_file_reference":
						ev[lastBoundary].referenced = append(ev[lastBoundary].referenced, name)
					}
				case l.Type == "system" && l.Subtype == "compact_boundary":
					b := cmpEvent{kind: 'b', ts: parseTS(l.Timestamp), summaryChars: pendingSummary}
					if m := l.CompactMetadata; m != nil {
						b.trigger, b.pre, b.durMs = m.Trigger, m.PreTokens, m.DurationMs
						if m.PreservedMessages != nil {
							for _, u := range m.PreservedMessages.UUIDs {
								b.kept = append(b.kept, byLine[u]...)
							}
						}
					}
					clear(byLine)
					if b.trigger != "manual" {
						b.trigger = "auto"
					}
					ev = append(ev, b)
					lastBoundary, pendingSummary = len(ev)-1, 0
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A read that failed partway would pass for a shorter session.
			return nil, err
		}
	}
	return ev, nil
}

// cmpNames is the distinct file names in a prompt's text (its first 40,000
// characters).
func cmpNames(text string) []string {
	if len(text) > 40000 {
		text = text[:40000]
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range cmpNameRE.FindAllString(text, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// lastOfKind is the index of the last event whose kind is in kinds.
func lastOfKind(ev []cmpEvent, kinds string) int {
	for i := len(ev) - 1; i >= 0; i-- {
		if strings.IndexByte(kinds, ev[i].kind) >= 0 {
			return i
		}
	}
	return -1
}

// cmpUserContent is a user line's text and its tool results.
func cmpUserContent(raw json.RawMessage) (string, []rawBlock) {
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s, nil
	}
	var blocks []rawBlock
	_ = json.Unmarshal(raw, &blocks)
	var text strings.Builder
	var results []rawBlock
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			results = append(results, b)
		case "text":
			text.WriteString(b.Text)
		}
	}
	return text.String(), results
}

// cmpWindowCalls is how many calls after a point are looked at.
const cmpWindowCalls = 30

// cmpWindow is what followed a point in a session.
type cmpWindow struct {
	Calls, Lookups, Tools, Failed int
	FileLookups, Reconsults       int   // lookups of a file; of one already in context before the point
	ReconsultChars                int64 // what those put back
	Orient                        int
	Reached                       bool // an edit came within the window
	CallsToEdit, LookupsToEdit    int
	Out                           int64 // output tokens of its calls
	Again                         int   // lookups naming no file that asked what one before the point had
	Own                           bool  // it read a Claude Code transcript
	Agents                        int   // subagents started
	USD                           cmpUSD
}

// cmpUSD is what calls cost at list prices, by part.
type cmpUSD struct{ Write, Read, Out, In float64 }

func (u cmpUSD) total() float64 { return u.Write + u.Read + u.Out + u.In }

func (u *cmpUSD) add(v cmpUSD) {
	u.Write, u.Read, u.Out, u.In = u.Write+v.Write, u.Read+v.Read, u.Out+v.Out, u.In+v.In
}

// cmpFollow reads the window after ev[at] (at -1: the session's start), up
// to end. firstSeen is each file's first event in the session.
func cmpFollow(ev []cmpEvent, at, end int, firstSeen map[string]int, usd func(*cmpEvent) cmpUSD) cmpWindow {
	var w cmpWindow
	for i := at + 1; i < end; i++ {
		e := &ev[i]
		switch e.kind {
		case 'c':
			if w.Calls == cmpWindowCalls {
				return w
			}
			w.Calls++
			w.Out += e.out
			w.USD.add(usd(e))
		case 't':
			w.Tools++
			if e.failed {
				w.Failed++
			}
			if e.edit && !w.Reached {
				w.Reached, w.CallsToEdit, w.LookupsToEdit = true, w.Calls, w.Lookups
			}
			if e.agent {
				w.Agents++
			}
			if !e.lookup {
				continue
			}
			w.Lookups++
			w.Own = w.Own || e.own
			if e.orient {
				w.Orient++
			}
			if len(e.paths) == 0 {
				if first, ok := firstSeen[cmpKeyPrefix+e.key]; ok && first <= at {
					w.Again++
				}
				continue
			}
			w.FileLookups++
			for _, p := range e.paths {
				if first, ok := firstSeen[p]; ok && first <= at {
					w.Reconsults++
					w.ReconsultChars += int64(e.resultChars)
					break
				}
			}
		}
	}
	return w
}

// cmpKeyPrefix keeps what a lookup asked apart from file names in cmpFirstSeen.
const cmpKeyPrefix = "\x00"

// cmpFirstSeen maps each file a session put in context, and each thing a
// lookup naming no file asked, to its first event.
func cmpFirstSeen(ev []cmpEvent) map[string]int {
	m := map[string]int{}
	for i := range ev {
		for _, p := range ev[i].paths {
			if _, ok := m[p]; !ok {
				m[p] = i
			}
		}
		if ev[i].key != "" {
			if _, ok := m[cmpKeyPrefix+ev[i].key]; !ok {
				m[cmpKeyPrefix+ev[i].key] = i
			}
		}
	}
	return m
}

// The sets of files a compaction could carry over, replayed against what
// the agent looked up again (WP-1097).
const (
	carryRestored = iota
	carryReferenced
	carryLast5
	carryLast10
	carryLast20
	carryEdited
	carryPrompt
	carryEditedPrompt
	carrySets
)

// cmpCarryCap is the most of one file a set carries, in characters: Claude
// Code's own limit of 5,000 tokens a restored file.
const cmpCarryCap = 18000

// cmpCarried is one set at one compaction.
type cmpCarried struct {
	Files, Covered      int
	Chars, CoveredChars int64
}

// cmpCarry is the files looked up again in the window after a compaction.
type cmpCarry struct {
	Again                                       int // files, each counted once
	AgainChars                                  int64
	Restored, Referenced, ShellOnly, ThenEdited int
	Kept                                        int // those still in the messages Claude Code left in place
	Sets                                        [carrySets]cmpCarried
}

// cmpReplay reads which files the window after the compaction at ev[at]
// looked up again, and what each set of files would have covered.
func cmpReplay(ev []cmpEvent, at, end int) cmpCarry {
	type file struct {
		last           int
		chars          int
		edited, tool   bool
		again, edited2 bool
		againChars     int
		kept           bool
	}
	files := map[string]*file{}
	for i := 0; i < at; i++ {
		e := &ev[i]
		for _, p := range e.paths {
			f := files[p]
			if f == nil {
				f = &file{}
				files[p] = f
			}
			f.last = i
			f.edited = f.edited || e.edit
			f.tool = f.tool || !e.shell
			if e.lookup {
				f.chars = max(f.chars, e.resultChars)
			}
		}
	}
	for _, i := range ev[at].kept {
		if !ev[i].lookup {
			continue
		}
		for _, p := range ev[i].paths {
			if f := files[p]; f != nil {
				f.kept = true
			}
		}
	}
	var c cmpCarry
	calls := 0
	for i := at + 1; i < end && calls <= cmpWindowCalls; i++ {
		e := &ev[i]
		if e.kind == 'c' {
			calls++
			continue
		}
		for _, p := range e.paths {
			f := files[p]
			switch {
			case f == nil:
			case e.lookup && !f.again:
				f.again, f.againChars = true, e.resultChars
			case e.edit && f.again:
				f.edited2 = true
			}
		}
	}
	var sets [carrySets]map[string]bool
	for i := range sets {
		sets[i] = map[string]bool{}
	}
	for _, p := range ev[at].restored {
		sets[carryRestored][p], sets[carryReferenced][p] = true, true
	}
	for _, p := range ev[at].referenced {
		sets[carryReferenced][p] = true
	}
	for i := at + 1; i < end && ev[i].kind != 'c'; i++ {
		for _, p := range ev[i].names {
			if files[p] != nil {
				sets[carryPrompt][p], sets[carryEditedPrompt][p] = true, true
			}
		}
	}
	names := make([]string, 0, len(files))
	for p, f := range files {
		names = append(names, p)
		if f.edited {
			sets[carryEdited][p], sets[carryEditedPrompt][p] = true, true
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if a, b := files[names[i]].last, files[names[j]].last; a != b {
			return a > b
		}
		return names[i] < names[j]
	})
	for i, p := range names {
		if i < 5 {
			sets[carryLast5][p] = true
		}
		if i < 10 {
			sets[carryLast10][p] = true
		}
		if i < 20 {
			sets[carryLast20][p] = true
		}
	}
	for p, f := range files {
		if f.again {
			c.Again++
			c.AgainChars += int64(f.againChars)
			if sets[carryRestored][p] {
				c.Restored++
			} else if sets[carryReferenced][p] {
				c.Referenced++
			}
			if !f.tool {
				c.ShellOnly++
			}
			if f.edited2 {
				c.ThenEdited++
			}
			if f.kept {
				c.Kept++
			}
		}
		if f.kept {
			continue
		}
		for s := range sets {
			if !sets[s][p] {
				continue
			}
			c.Sets[s].Files++
			c.Sets[s].Chars += int64(min(f.chars, cmpCarryCap))
			if f.again {
				c.Sets[s].Covered++
				c.Sets[s].CoveredChars += int64(f.againChars)
			}
		}
	}
	return c
}

// cmpHistory is the characters of a session between two events as a
// summariser of ours would read them: prompts, replies, tool inputs and
// tool results, the results in full and cut to capAt characters each.
func cmpHistory(ev []cmpEvent, from, to, capAt int) (full, capped int64) {
	for i := max(from, 0); i < to; i++ {
		e := &ev[i]
		switch e.kind {
		case 'b':
			full += int64(e.summaryChars)
			capped += int64(e.summaryChars)
		case 't':
			full += int64(e.inChars + e.resultChars)
			capped += int64(min(e.inChars, capAt) + min(e.resultChars, capAt))
		default:
			full += int64(e.chars)
			capped += int64(e.chars)
		}
	}
	return full, capped
}
