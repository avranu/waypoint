package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// wp usage subagents (WP-1443): reading one subagent transcript into calls
// (a tool_use paired with its tool_result) and the entries that resumed it,
// then sorting each call into at most one wait/retry pattern.

// saCallCap is the longest gap charged to a call: an agent idle for longer
// was parked, not working.
const saCallCap = 30 * time.Minute

// Why a call is counted as loss. Each call has at most one, the first that
// applies in this order.
const (
	patRefusedStart = "refused-start"
	patBlockedSleep = "refused-wait-sleep"
	patBlockedLoop  = "refused-wait-loop"
	patNoop         = "noop-poll"
	patRepeatTest   = "repeat-test"
	patPostHandback = "post-handback"
)

var saLossOrder = []string{patRefusedStart, patBlockedSleep, patBlockedLoop, patNoop, patRepeatTest, patPostHandback}

// How a span after a hand-back began.
const (
	resumeRetasked   = "re-tasked"
	resumeBackground = "woken by its own background task"
	resumeNudged     = "nudged"
	resumeOther      = "other"
)

// saCall is one tool_use of a subagent.
type saCall struct {
	ID, Name   string
	Line       int
	TS         float64 // unix seconds
	Cmd        string  // Bash command; for other tools a compact form of the input
	Background bool
	HasResult  bool
	IsError    bool
	ResLine    int
	ResTS      float64
	Head       string // start of the result text
	Mins       float64
	InCall     float64 // minutes from tool_use to tool_result, 0 when unpaired

	Pattern string // loss pattern, "" when none
	Sub     string // sub-kind of the pattern
	Wait    string // "", "foreground" or "background": a wait form (information)
	Extra   bool   // a no-op poll the narrow `echo waiting` rule would miss
}

// saEvent is a user entry that is text rather than tool results.
type saEvent struct {
	Line int
	TS   float64
	Text string
}

// saAgent is what one transcript yielded.
type saAgent struct {
	Session, ID, Type, Desc, Path string
	Gz                            bool
	Calls                         []*saCall
	Nudges                        int
	DupNudges                     int // nudged after a hand-back had already been delivered
	FirstTS, LastTS               float64

	RefusedRetried60s   int
	RefusedBeforeChild  int
	HandbackCalls       int
	PostHandbackByKind  map[string]*saCount
	PostHandbackRetasks int
}

type saCount struct {
	Calls  int
	Mins   float64
	InCall float64
}

type saRawLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type saRawBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Text      string          `json:"text"`
}

// openTranscript opens a plain or gzipped transcript. The reader is
// bounded: a transcript larger than 4 GB unpacked is not one Claude Code wrote.
func openTranscript(path string) (io.Reader, func(), error) {
	f, err := os.Open(path) // #nosec G304 -- a transcript Claude Code wrote
	if err != nil {
		return nil, nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return io.LimitReader(f, 4<<30), func() { _ = f.Close() }, nil
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return io.LimitReader(zr, 4<<30), func() { _ = zr.Close(); _ = f.Close() }, nil
}

func saTime(s string) float64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// readSubagent reads one transcript and classifies its calls.
func saReadAgent(path string, a *saAgent) error {
	r, closeFn, err := openTranscript(path)
	if err != nil {
		return err
	}
	defer closeFn()
	byID := map[string]*saCall{}
	var events []saEvent
	br := bufio.NewReaderSize(r, 1<<20)
	for ln := 1; ; ln++ {
		b, err := br.ReadBytes('\n')
		if len(b) > 0 {
			saLine(a, b, ln, byID, &events)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	saFinish(a, events)
	return nil
}

func saLine(a *saAgent, b []byte, ln int, byID map[string]*saCall, events *[]saEvent) {
	if t := topLevelType(b); t != "assistant" && t != "user" {
		return
	}
	var l saRawLine
	if json.Unmarshal(b, &l) != nil || l.Timestamp == "" {
		return
	}
	ts := saTime(l.Timestamp)
	if ts == 0 {
		return
	}
	if a.FirstTS == 0 || ts < a.FirstTS {
		a.FirstTS = ts
	}
	a.LastTS = max(a.LastTS, ts)
	raw := l.Message.Content
	if len(raw) == 0 {
		return
	}
	if raw[0] == '"' {
		var s string
		if l.Type == "user" && json.Unmarshal(raw, &s) == nil && s != "" {
			*events = append(*events, saEvent{ln, ts, s})
		}
		return
	}
	var blocks []saRawBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	var text strings.Builder
	hasResult := false
	for i := range blocks {
		bl := &blocks[i]
		switch {
		case l.Type == "assistant" && bl.Type == "tool_use":
			if byID[bl.ID] != nil {
				continue
			}
			c := &saCall{ID: bl.ID, Name: bl.Name, Line: ln, TS: ts}
			var in struct {
				Command    string `json:"command"`
				Background bool   `json:"run_in_background"`
			}
			_ = json.Unmarshal(bl.Input, &in)
			c.Background = in.Background
			if bl.Name == "Bash" || bl.Name == "bash" {
				c.Cmd = in.Command
			} else {
				c.Cmd = compactJSON(bl.Input)
			}
			byID[bl.ID] = c
			a.Calls = append(a.Calls, c)
		case l.Type == "user" && bl.Type == "tool_result":
			hasResult = true
			// only the block for its own id counts, and only the first: later
			// entries can echo a result
			if c := byID[bl.ToolUseID]; c != nil && !c.HasResult {
				c.HasResult, c.IsError, c.ResLine, c.ResTS = true, bl.IsError, ln, ts
				c.Head = resultText(bl.Content, 600)
			}
		case l.Type == "user" && bl.Type == "text":
			text.WriteString(bl.Text)
			text.WriteByte('\n')
		}
	}
	if l.Type == "user" && !hasResult && text.Len() > 0 {
		*events = append(*events, saEvent{ln, ts, text.String()})
	}
}

var (
	saTestRe  = regexp.MustCompile(`\bgo (-C \S+ )?test\b|\bwp testsum\b|\bmake (check|test)\b|\bnpm (run )?test\b|\bpytest\b`)
	saWaitRe  = regexp.MustCompile(`\bsleep\b|\buntil\b|\bwhile\b|\bpgrep\b|\bwp (pr|request) wait\b`)
	saWhileRd = regexp.MustCompile(`\bwhile\s+(IFS=|read\b)`)
	saTreeRe  = regexp.MustCompile(`\bgit (checkout|apply|commit|stash|reset|restore)\b|\bsed -i|\bpatch\b|\bmv |\bcp |\brm |\btee `)
	saRedirRe = regexp.MustCompile(`(^|[^0-9&>=<-])>>?\s*(&?[^\s&>|;)]+)`)
	saOldPoll = regexp.MustCompile(`^\s*(sleep\s+\d+\s*;\s*)?(echo\s+waiting\w*|true|:)\s*$`)
	saSpaceRe = regexp.MustCompile(`\s+`)
)

func saIsWait(c *saCall) bool {
	if c.Name == "Monitor" {
		return true
	}
	if c.Name != "Bash" && c.Name != "bash" {
		return false
	}
	cmd := saWhileRd.ReplaceAllString(c.Cmd, "")
	return saWaitRe.MatchString(cmd)
}

// saWritesFile reports a shell redirection into a file; /dev and /tmp targets
// and descriptor duplication (2>&1) don't change the tree.
func saWritesFile(cmd string) bool {
	for _, m := range saRedirRe.FindAllStringSubmatch(cmd, -1) {
		t := strings.Trim(m[2], `"'`)
		if strings.HasPrefix(t, "&") || strings.HasPrefix(t, "/dev/") || strings.HasPrefix(t, "/tmp/") || strings.HasPrefix(t, "/var/tmp/") {
			continue
		}
		return true
	}
	return false
}

func saTreeChanging(c *saCall) bool {
	if !c.HasResult || c.IsError {
		return false
	}
	switch c.Name {
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		return true
	case "Bash", "bash":
		return saTreeRe.MatchString(c.Cmd) || saWritesFile(c.Cmd)
	}
	return false
}

// saNeverRan: the call was refused or failed before it started.
func saNeverRan(c *saCall) bool {
	if !c.HasResult {
		return true
	}
	h := c.Head
	return c.IsError && (strings.HasPrefix(h, "<tool_use_error>") || strings.HasPrefix(h, "PreToolUse") ||
		strings.Contains(h, "Blocked:") || strings.Contains(h, "no longer exists, so this command was not run") ||
		strings.HasPrefix(h, "No such tool") || strings.Contains(h, "hook error"))
}

// saNoop reports a Bash command that does nothing but print a literal or
// succeed: echo/printf of literal text, true, ":", with sleep N segments.
func saNoop(cmd string) bool {
	if strings.ContainsAny(cmd, "$`<>|") || strings.Contains(cmd, "&&&") {
		return false
	}
	segs, ok := saSegments(cmd)
	if !ok {
		return false
	}
	did := false
	for _, s := range segs {
		f := strings.Fields(s)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "sleep":
			if len(f) != 2 || strings.Trim(f[1], "0123456789.smh") != "" {
				return false
			}
		case "true", ":":
			if len(f) != 1 && f[0] == "true" {
				return false
			}
			did = true
		case "echo", "printf":
			did = true
		default:
			return false
		}
	}
	return did
}

// saSegments splits on ; && and newlines outside quotes. A lone & (background
// or fd duplication) makes it not a simple command.
func saSegments(cmd string) ([]string, bool) {
	var segs []string
	var cur strings.Builder
	var q rune
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case q != 0:
			if r == q {
				q = 0
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			q = r
			cur.WriteRune(r)
		case r == ';' || r == '\n':
			segs = append(segs, cur.String())
			cur.Reset()
		case r == '&':
			if i+1 < len(rs) && rs[i+1] == '&' {
				segs = append(segs, cur.String())
				cur.Reset()
				i++
			} else {
				return nil, false
			}
		case r == '\\' || r == '(' || r == ')' || r == '{' || r == '}':
			return nil, false
		default:
			cur.WriteRune(r)
		}
	}
	if q != 0 {
		return nil, false
	}
	return append(segs, cur.String()), true
}

func saClassifyResume(text string) string {
	t := strings.TrimLeft(text, " \n")
	switch {
	case strings.HasPrefix(t, "The coordinator sent a message"):
		return resumeRetasked
	case strings.HasPrefix(t, "[handback-send-enforce]"):
		return resumeNudged
	case strings.HasPrefix(t, "[SYSTEM NOTIFICATION") || strings.Contains(t[:min(len(t), 1200)], "<task-notification>"):
		return resumeBackground
	}
	return resumeOther
}

var saChildDone = regexp.MustCompile(`<summary>Agent "[^"]*" finished`)

func saFinish(a *saAgent, events []saEvent) {
	calls := a.Calls
	for i, c := range calls {
		next := c.ResTS
		if i+1 < len(calls) {
			next = calls[i+1].TS
		}
		if next < c.TS {
			next = c.TS
		}
		c.Mins = min(next-c.TS, saCallCap.Seconds()) / 60
		if c.HasResult && c.ResTS >= c.TS {
			c.InCall = (c.ResTS - c.TS) / 60
		}
		if saIsWait(c) {
			c.Wait = "foreground"
			if c.Background {
				c.Wait = "background"
			}
		}
	}
	// refused starts
	for i, c := range calls {
		if (c.Name == "Agent" || c.Name == "Task") && strings.HasPrefix(c.Head, "Concurrent subagent limit reached") {
			c.Pattern, c.Sub = patRefusedStart, "refused"
			for _, d := range calls[i+1:] {
				if d.TS-c.TS > 60 {
					break
				}
				if d.Name == "Agent" || d.Name == "Task" {
					a.RefusedRetried60s++
					break
				}
			}
			child := false
			for _, e := range events {
				if e.TS < c.TS && saChildDone.MatchString(e.Text) {
					child = true
					break
				}
			}
			if !child {
				a.RefusedBeforeChild++
			}
		}
	}
	// hand-backs and nudges
	firstHB := 0
	var hbLines []int
	for _, c := range calls {
		if c.Name == "SubagentHandback" {
			a.HandbackCalls++
			if c.HasResult && !c.IsError && strings.Contains(c.Head, "Report delivered") {
				hbLines = append(hbLines, c.ResLine)
				if firstHB == 0 {
					firstHB = c.ResLine
				}
			}
		}
	}
	for _, e := range events {
		if !strings.HasPrefix(strings.TrimLeft(e.Text, " \n"), "[handback-send-enforce]") {
			continue
		}
		a.Nudges++
		if firstHB != 0 && firstHB < e.Line {
			for _, c := range calls {
				if c.Name == "SubagentHandback" && c.Line > e.Line {
					a.DupNudges++
					break
				}
			}
		}
	}
	a.PostHandbackByKind = map[string]*saCount{}
	kindOf := map[*saCall]string{}
	if firstHB != 0 {
		for _, c := range calls {
			if c.Line <= firstHB {
				continue
			}
			h := 0
			for _, l := range hbLines {
				if l < c.Line {
					h = l
				}
			}
			k := resumeOther
			first := true
			for _, e := range events {
				if e.Line <= h || e.Line >= c.Line {
					continue
				}
				ek := saClassifyResume(e.Text)
				if ek == resumeRetasked {
					k = ek
					break
				}
				if first {
					k, first = ek, false
				}
			}
			kindOf[c] = k
			s := a.PostHandbackByKind[k]
			if s == nil {
				s = &saCount{}
				a.PostHandbackByKind[k] = s
			}
			s.Calls++
			s.Mins += c.Mins
			s.InCall += c.InCall
		}
	}
	// refused waits and no-op polls
	for _, c := range calls {
		if c.Name != "Bash" && c.Name != "bash" {
			continue
		}
		switch {
		case c.Pattern != "":
		case strings.Contains(c.Head, "Blocked: sleep") || strings.Contains(c.Head, "Blocked: standalone sleep"):
			c.Pattern, c.Sub = patBlockedSleep, "Claude Code blocked a sleep"
		case strings.Contains(c.Head, "background wait loop with no bound"):
			c.Pattern, c.Sub = patBlockedLoop, "hook refused an unbounded wait loop"
		case !c.HasResult || c.IsError && saNeverRan(c):
		case saNoop(c.Cmd):
			c.Pattern, c.Sub = patNoop, "no-op"
			c.Extra = !saOldPoll.MatchString(c.Cmd)
		}
	}
	// repeated tests on an unchanged tree
	last := map[string]int{}
	runs := map[string]int{}
	for _, c := range calls {
		isBash := c.Name == "Bash" || c.Name == "bash"
		if saTreeChanging(c) {
			clear(last)
		}
		if !isBash || !saTestRe.MatchString(c.Cmd) || saIsWait(c) || saNeverRan(c) || c.Pattern == patNoop {
			continue
		}
		k := saSpaceRe.ReplaceAllString(strings.TrimSpace(c.Cmd), " ")
		runs[k]++
		if last[k] > 0 && c.Pattern == "" {
			c.Pattern, c.Sub = patRepeatTest, "run "+strconv.Itoa(runs[k])+" of the same command, tree unchanged"
		}
		last[k] = runs[k]
	}
	// work after a hand-back that nobody asked for
	for c, k := range kindOf {
		if k != resumeRetasked && c.Pattern == "" {
			c.Pattern, c.Sub = patPostHandback, k
		}
	}
	a.PostHandbackRetasks = 0
	if s := a.PostHandbackByKind[resumeRetasked]; s != nil {
		a.PostHandbackRetasks = s.Calls
	}
}
