package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

var usageWhereCmd = &command{
	name: "where", args: "[--since 24h] [--until T] [--dir CLAUDE_CONFIG_DIR] [--top N] [--csv FILE] [--json]",
	summary: "where this machine's Claude Code spend went in a window, and what looks wasteful",
	about: `Reads this machine's Claude Code transcripts (main sessions, subagents and
workflow agents) and prints one report in list-price dollars: totals by
model with the cache split; by agent, project and how the session was
started; what the premium-model main threads spent each call on (reading,
searching, editing through the shell or the Edit tool, tests and builds, git,
wp, delegating, text only), counting every command of a compound line; what
woke each call (typed prompt, notification, tool result, compaction); each
session's spend before and after its first wp ship; the top sessions;
limit messages and the spend after the first; and what the report cannot
see. A call is counted once however many records stream it (the largest
usage wins). --since and --until take a duration ago (24h, 3d) or a time
(2026-11-24T15:00:00Z, 2026-11-24). --csv writes one row per call.`,
	examples: []string{"wp usage where", "wp usage where --since 3d --top 20 --csv calls.csv", "wp usage where --since 2026-11-24T15:00:00Z --until 2026-11-25T15:00:00Z --json"},
	run:      cmdUsageWhere,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageWhereCmd)
}

// The classes a model call's cost is split over.
const (
	whReadFile  = "read file"
	whSearch    = "search"
	whShellRead = "shell read"
	whShellEdit = "shell edit"
	whToolEdit  = "edit tool"
	whScratch   = "scratch write"
	whTests     = "tests/build"
	whGit       = "git"
	whWP        = "wp"
	whDelegate  = "delegate"
	whOther     = "other"
	whTextOnly  = "text only"
)

// whParts is a call's dollars by kind of token.
type whParts struct {
	Input   float64 `json:"input"`
	Output  float64 `json:"output"`
	Write5m float64 `json:"cache_write_5m"`
	Write1h float64 `json:"cache_write_1h"`
	Read    float64 `json:"cache_read"`
}

func (p whParts) total() float64 { return p.Input + p.Output + p.Write5m + p.Write1h + p.Read }

func (p *whParts) add(o whParts) {
	p.Input += o.Input
	p.Output += o.Output
	p.Write5m += o.Write5m
	p.Write1h += o.Write1h
	p.Read += o.Read
}

// whCall is one deduplicated model call.
type whCall struct {
	session, kind, project string
	file                   string
	main                   bool
	ts                     int64
	model, trigger         string
	blocks                 [][]string // the classes of each tool block
	cmds                   [][]whCmd  // the commands of each tool block
	sub                    string     // what kind of meta message woke the call
	blockIDs               map[string]bool
	ctx, size              int64
	in, out, cw5, cw1, cr  int64
	parts                  whParts
	ship                   bool
}

func (c *whCall) classes() []string {
	var out []string
	seen := map[string]bool{}
	for _, b := range c.blocks {
		for _, k := range b {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	if len(out) == 0 {
		return []string{whTextOnly}
	}
	sort.Strings(out)
	return out
}

// shares splits the call's cost equally over its tool blocks, and a block's
// share equally over its classes.
func (c *whCall) shares() map[string]float64 {
	out := map[string]float64{}
	if len(c.blocks) == 0 {
		out[whTextOnly] = 1
		return out
	}
	for _, b := range c.blocks {
		for _, k := range b {
			out[k] += 1 / float64(len(c.blocks)) / float64(len(b))
		}
	}
	return out
}

// whSession is what the report knows of one session.
type whSession struct {
	id, project, dir, prompt, launch string
	projSet                          bool
	firstShip                        int64
	ships                            int
	cost, mainCost                   float64
	calls                            int
	models                           map[string]bool
	files                            int
}

type whLimit struct {
	TS      string `json:"time"`
	Session string `json:"session"`
	Text    string `json:"text"`
	ts      int64
}

// whScan is everything read from the transcripts.
type whScan struct {
	calls      map[string]*whCall
	order      []*whCall
	sessions   map[string]*whSession
	limits     []whLimit
	limitSeen  map[string]bool
	synthetic  int
	scriptTool map[string]int
	boundaries int
	unreadable []string
	files      int
	since, end int64
	mem        traceScripts
}

type whFile struct {
	usageFile
	project string
}

func whParseWhen(s string, now time.Time, def time.Duration) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		if def == 0 {
			return time.Time{}, nil
		}
		return now.Add(-def), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	d, err := parseSince(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is a duration ago (24h, 3d) or a time (2026-11-24T15:00:00Z, 2026-11-24)", s)
	}
	return now.Add(-d), nil
}

func cmdUsageWhere(c *cli, args []string) error {
	fs := c.flags()
	since := fs.String("since", "", "from `TIME` or this long ago (default 24h)")
	until := fs.String("until", "", "to `TIME` or this long ago (default now)")
	dir := fs.String("dir", "", "Claude Code config `DIR` (default CLAUDE_CONFIG_DIR, ~/.claude)")
	top := fs.Int("top", 10, "list the `N` costliest sessions")
	csvPath := fs.String("csv", "", "also write one row per call to `FILE`")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	now := time.Now()
	from, err := whParseWhen(*since, now, 24*time.Hour)
	if err != nil {
		return c.usage("--since: %v", err)
	}
	to, err := whParseWhen(*until, now, 0)
	if err != nil {
		return c.usage("--until: %v", err)
	}
	if to.IsZero() {
		to = now.Add(time.Minute)
	}
	if !to.After(from) {
		return c.usage("--until %s is not after --since %s", to.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339))
	}
	files, err := discoverTranscripts(claudeDir(*dir), time.Since(from), nil)
	if err != nil {
		return err
	}
	prices := c.loadBurnPrices(false)
	sc := scanWhere(files, from, to, prices)
	rep := sc.report(*top)
	if *csvPath != "" {
		if err := sc.writeCSV(*csvPath); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(rep)
	return c.emit(raw, func() string { return whereText(rep) })
}

func scanWhere(files []usageFile, from, to time.Time, prices []burnPrice) *whScan {
	sc := &whScan{calls: map[string]*whCall{}, sessions: map[string]*whSession{}, limitSeen: map[string]bool{}, scriptTool: map[string]int{}, since: from.UnixMilli(), end: to.UnixMilli()}
	// the oldest file first, so that a resumed session's copy of history is
	// credited to the session that wrote it
	mod := map[string]time.Time{}
	for _, f := range files {
		if fi, err := os.Lstat(f.Path); err == nil {
			mod[f.Path] = fi.ModTime()
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return mod[files[i].Path].Before(mod[files[j].Path]) })
	sc.files = len(files)
	for _, f := range files {
		if err := sc.readFile(f, prices); err != nil {
			sc.unreadable = append(sc.unreadable, f.Path)
		}
	}
	return sc
}

func (sc *whScan) in(ts int64) bool { return ts >= sc.since && ts < sc.end }

var whProjectCut = []string{"/.claude/worktrees/", "/.worktrees/"}

// whProject names the project a session worked in: the checkout's name, from
// the working directory, else the transcript's directory.
func whProject(cwd, dirName string) string {
	if strings.Contains(dirName, "waypoint-runner-work-job") {
		return "runner jobs"
	}
	if cwd != "" {
		for _, cut := range whProjectCut {
			if i := strings.Index(cwd, cut); i > 0 {
				cwd = cwd[:i]
			}
		}
		if b := strings.TrimSuffix(path.Base(cwd), ".git"); b != "" && b != "." && b != "/" {
			return b
		}
	}
	return dirName
}

func whLaunch(dirName, prompt string) string {
	switch {
	case strings.Contains(dirName, "waypoint-runner-work-job"):
		return "runner job"
	case strings.Contains(dirName, "waypoint-runner-git"):
		return "runner checkout"
	case traceGivenRE.MatchString(prompt), strings.HasPrefix(prompt, cmpReviewPrompt):
		return "autopilot session"
	}
	return "interactive or unknown"
}

func whCost(u *rawUsage, p burnPrice) (parts whParts, cw5, cw1 int64) {
	cw5 = u.CacheCreation
	if cc := u.CC; cc != nil && cc.E5m+cc.E1h > 0 {
		cw5, cw1 = cc.E5m, cc.E1h
		if rest := u.CacheCreation - cc.E5m - cc.E1h; rest > 0 {
			cw5 += rest
		}
	}
	parts = whParts{
		Input: float64(u.Input) * p.Input / 1e6, Output: float64(u.Output) * p.Output / 1e6,
		Write5m: float64(cw5) * p.CacheWrite5m / 1e6, Write1h: float64(cw1) * p.CacheWrite1h / 1e6,
		Read: float64(u.CacheRead) * p.CacheRead / 1e6,
	}
	return
}

// whTrivial are commands that say nothing about what a call did.
var whTrivial = map[string]bool{"cd": true, "echo": true, "printf": true, "true": true, "false": true, "export": true, "set": true, "pwd": true, "sleep": true, "test": true, "[": true, "[[": true, "exit": true, "unset": true, "source": true, ".": true, "mkdir": true, "rm": true, "wait": true, "popd": true, "pushd": true, "": true}

var whTestHeads = map[string]bool{"make": true, "gmake": true, "npm": true, "npx": true, "pnpm": true, "yarn": true, "bun": true, "cargo": true, "pytest": true, "python -m pytest": true, "tsc": true, "vitest": true, "jest": true, "golangci-lint": true, "gosec": true, "govulncheck": true, "playwright": true, "wp testsum": true, "mvn": true, "gradle": true, "dotnet": true, "ruff": true, "mypy": true, "eslint": true, "prettier": true, "gofmt": true, "staticcheck": true}

// whCommandClass sorts one command of a line.
func whCommandClass(cm traceCommand, cwd string) string {
	wrote, scratch := false, false
	for _, w := range cm.wrote {
		if w.kind == "output" {
			continue
		}
		if _, work := traceWord(w.w, cm.dir, cwd); work {
			wrote = true
		} else {
			scratch = true
		}
	}
	if wrote {
		return whShellEdit
	}
	h := cm.head
	var class string
	switch {
	case whTrivial[h]:
		class = ""
	case h == "wp testsum", whTestHeads[h], strings.HasPrefix(h, "go ") && h != "go doc":
		class = whTests
	case h == "wp code" || h == "wp brief" || traceSearchH[h] || h == "git grep" || h == "git ls-files":
		class = whSearch
	case strings.HasPrefix(h, "wp "), h == "wp":
		class = whWP
	case h == "git" || strings.HasPrefix(h, "git "), h == "gh", h == "glab":
		class = whGit
	case traceReadH[h]:
		class = whShellRead
	}
	if scratch && (class == "" || class == whShellRead) {
		return whScratch
	}
	if class == "" && !whTrivial[h] {
		return whOther
	}
	return class
}

// whCmd is one command of a tool block (or a tool that is not Bash), with the
// class it was filed under.
type whCmd struct {
	Head  string `json:"head"`
	Class string `json:"class"`
	Tool  bool   `json:"tool,omitempty"` // a tool name, not a shell command
}

// whBlockClasses sorts one tool_use block: the classes of every command of
// a Bash line, or the one class of another tool. ship says the line holds a
// real wp ship.
func (sc *whScan) whBlockClasses(name string, input json.RawMessage, cwd string) (classes []string, ship bool, cmds []whCmd) {
	var in traceInput
	_ = json.Unmarshal(input, &in)
	one := func(class string) ([]string, bool, []whCmd) {
		return []string{class}, false, []whCmd{{Head: name, Class: class, Tool: true}}
	}
	switch name {
	case "Read":
		return one(whReadFile)
	case "Grep", "Glob", "LS":
		return one(whSearch)
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		if _, work := traceFile(usageOr(in.FilePath, in.NotebookPath), cwd); !work {
			return one(whScratch)
		}
		return one(whToolEdit)
	case "Agent", "Task":
		return one(whDelegate)
	case "Bash":
		var tcs []traceCommand
		home, _ := os.UserHomeDir()
		traceCommands(in.Command, cwd, 0, map[string]string{"HOME": home}, &sc.mem, &tcs)
		seen := map[string]bool{}
		for _, cm := range tcs {
			if cm.head == "wp ship" && !traceHasFlag(cm.words, "-h", "--help", "--review") {
				ship = true
			}
			if k := whCommandClass(cm, cwd); k != "" {
				cmds = append(cmds, whCmd{Head: cm.head, Class: k})
				if !seen[k] {
					seen[k] = true
					classes = append(classes, k)
				}
			}
		}
		if len(classes) == 0 {
			classes = []string{whOther}
			head := "(shell)"
			if len(tcs) > 0 {
				head = tcs[0].head
			}
			cmds = []whCmd{{Head: head, Class: whOther}}
		}
		sort.Strings(classes)
		return classes, ship, cmds
	}
	return one(whOther)
}

func (sc *whScan) readFile(f usageFile, prices []burnPrice) error {
	fh, err := openRegularTranscript(f.Path)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()
	dirName := filepath.Base(filepath.Dir(f.Path))
	if !f.Main {
		for d := filepath.Dir(f.Path); d != filepath.Dir(d) && filepath.Base(filepath.Dir(d)) != "projects"; d = filepath.Dir(d) {
			dirName = filepath.Base(filepath.Dir(d))
		}
	}
	ses := sc.sessions[f.Session]
	if ses == nil {
		ses = &whSession{id: f.Session, models: map[string]bool{}}
		sc.sessions[f.Session] = ses
	}
	ses.files++
	if ses.project == "" {
		ses.project, ses.dir = dirName, dirName
	}
	cwd, trig, sub, started := "", "", "", false
	toolNames := map[string]string{}
	br := bufio.NewReaderSize(fh, 1<<20)
	var long []byte
	for {
		line, rerr := br.ReadSlice('\n')
		if errors.Is(rerr, bufio.ErrBufferFull) {
			long = append(long, line...)
			continue
		}
		if len(long) > 0 {
			line, long = append(long, line...), nil
		}
		if len(line) > 0 && (bytes.Contains(line, markAssistant) || bytes.Contains(line, markUser) || bytes.Contains(line, markSystem)) {
			var l rawLine
			if json.Unmarshal(line, &l) == nil {
				if l.Cwd != "" {
					cwd = l.Cwd
					if f.Main && !ses.projSet {
						ses.project, ses.projSet = whProject(cwd, dirName), true
					}
				}
				ts := parseTS(l.Timestamp)
				switch l.Type {
				case "system":
					if l.Subtype == "compact_boundary" {
						trig = "compaction/resume"
						if sc.in(ts) {
							sc.boundaries++
						}
					}
				case "user":
					trig, started = sc.userTrigger(&l, f, ses, toolNames, trig, started, &sub)
				case "assistant":
					trig = sc.assistant(&l, f, ses, ts, cwd, trig, sub, toolNames, prices)
					sub = ""
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	return nil
}

func whText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var blocks []rawBlock
	_ = json.Unmarshal(raw, &blocks)
	var b strings.Builder
	for _, k := range blocks {
		if k.Type == "text" {
			b.WriteString(k.Text)
		}
	}
	return b.String()
}

func (sc *whScan) userTrigger(l *rawLine, f usageFile, ses *whSession, toolNames map[string]string, trig string, started bool, sub *string) (string, bool) {
	*sub = ""
	if l.Message == nil {
		return trig, started
	}
	var blocks []rawBlock
	if c := l.Message.Content; len(c) > 0 && c[0] == '[' {
		_ = json.Unmarshal(c, &blocks)
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			name := toolNames[b.ToolUseID]
			if name == "" {
				name = "unknown tool"
			}
			return "tool result: " + name, true
		}
	}
	text := whText(l.Message.Content)
	switch {
	case l.IsCompactSummary:
		return "compaction/resume", true
	case l.IsMeta:
		*sub = whMetaKind(text)
		return "meta/hook message", true
	case !f.Main && !started:
		return "subagent task prompt", true
	}
	switch cmpPromptKind(text) {
	case 'n':
		return "notification/peer message", true
	case 'x':
		return "keep-warm/interrupt", true
	}
	if f.Main && ses.prompt == "" {
		if t := strings.TrimSpace(core.UnwrapPasted(text)); t != "" && !strings.HasPrefix(t, "<local-command") && !strings.HasPrefix(t, "<command-name>") && !strings.HasPrefix(t, "Caveat:") {
			ses.prompt = t
		}
	}
	return "typed prompt", true
}

func (sc *whScan) assistant(l *rawLine, f usageFile, ses *whSession, ts int64, cwd, trig, sub string, toolNames map[string]string, prices []burnPrice) string {
	m := l.Message
	if m == nil {
		return trig
	}
	var blocks []rawBlock
	if len(m.Content) > 0 && m.Content[0] == '[' {
		_ = json.Unmarshal(m.Content, &blocks)
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" {
			toolNames[b.ID] = b.Name
		}
	}
	model := cleanModel(m.Model)
	if l.IsAPIError {
		if text := whText(m.Content); strings.Contains(strings.ToLower(text), "limit") && sc.in(ts) {
			key := fmt.Sprintf("%d|%s|%s", ts, f.Session, text)
			if !sc.limitSeen[key] {
				sc.limitSeen[key] = true
				sc.limits = append(sc.limits, whLimit{TS: time.UnixMilli(ts).UTC().Format(time.RFC3339), Session: f.Session, Text: truncRunes(strings.Join(strings.Fields(text), " "), 160), ts: ts})
			}
		}
		return trig
	}
	if model == "<synthetic>" {
		if sc.in(ts) {
			sc.synthetic++
			for _, b := range blocks {
				if b.Type == "tool_use" {
					cl, _, _ := sc.whBlockClasses(b.Name, b.Input, cwd)
					for _, k := range cl {
						sc.scriptTool[k]++
					}
				}
			}
		}
		return trig
	}
	if model == "" || strings.HasPrefix(model, "<") || m.Usage == nil {
		return trig
	}
	key := m.ID + "\x00" + l.RequestID
	if m.ID == "" {
		key = fmt.Sprintf("%s\x00%d\x00%p", f.Path, ts, l)
	}
	call := sc.calls[key]
	if call == nil {
		call = &whCall{session: f.Session, file: f.Path, kind: f.Kind, main: f.Main, ts: ts, trigger: trig, sub: sub, blockIDs: map[string]bool{}, project: ses.project}
		if trig == "" {
			call.trigger = "session start"
		}
		sc.calls[key] = call
		sc.order = append(sc.order, call)
	}
	u := m.Usage
	if size := u.Input + u.Output + u.CacheCreation + u.CacheRead; size >= call.size {
		p := burnPriceFor(prices, model)
		parts, cw5, cw1 := whCost(u, p)
		call.size, call.model, call.parts = size, model, parts
		call.in, call.out, call.cw5, call.cw1, call.cr = u.Input, u.Output, cw5, cw1, u.CacheRead
		call.ctx = u.Input + u.CacheRead + u.CacheCreation
	}
	for _, b := range blocks {
		if b.Type != "tool_use" || (b.ID != "" && call.blockIDs[b.ID]) {
			continue
		}
		call.blockIDs[b.ID] = true
		cl, ship, cmds := sc.whBlockClasses(b.Name, b.Input, cwd)
		call.blocks = append(call.blocks, cl)
		call.cmds = append(call.cmds, cmds)
		call.ship = call.ship || ship
	}
	return "continuation"
}

// ---- the report ----

type whModelRow struct {
	Model     string  `json:"model"`
	Calls     int     `json:"calls"`
	Cost      float64 `json:"cost"`
	MedianCtx int64   `json:"median_context"`
	whParts
}

type whAgentRow struct {
	Agent  string  `json:"agent"`
	Runs   int     `json:"runs"`
	Calls  int     `json:"calls"`
	Cost   float64 `json:"cost"`
	Median float64 `json:"median_per_run"`
	Max    float64 `json:"max_per_run"`
}

type whGroupRow struct {
	Name     string  `json:"name"`
	Sessions int     `json:"sessions"`
	Calls    int     `json:"calls"`
	Cost     float64 `json:"cost"`
}

type whClassRow struct {
	Class string  `json:"class"`
	Calls int     `json:"calls"`
	Cost  float64 `json:"cost"`
}

type whActivity struct {
	Model   string       `json:"model"`
	Calls   int          `json:"calls"`
	Cost    float64      `json:"cost"`
	Classes []whClassRow `json:"classes"`
}

type whTriggerRow struct {
	Trigger string         `json:"trigger"`
	Calls   int            `json:"calls"`
	Cost    float64        `json:"cost"`
	Detail  []whTriggerRow `json:"detail,omitempty"` // meta messages by kind
}

// whCommandRow is a command head in the costliest-commands list.
type whCommandRow struct {
	Head  string  `json:"head"`
	Class string  `json:"class"`
	Tool  bool    `json:"tool,omitempty"`
	Calls int     `json:"calls"`
	Cost  float64 `json:"cost"`
}

type whCommands struct {
	Model    string         `json:"model"` // "premium main threads"
	Commands []whCommandRow `json:"commands"`
	// OtherTools are the tools other than Bash filed under "other"
	OtherTools []whCommandRow `json:"other_tools"`
}

type whPhaseRow struct {
	Session string  `json:"session"`
	Project string  `json:"project"`
	Ships   int     `json:"ships"`
	Before  float64 `json:"before_first_ship"`
	After   float64 `json:"after_first_ship"`
	// FirstShipBeforeWindow: the session's first ship came before --since, so
	// Before holds only what the window shows of the time before it.
	FirstShipBeforeWindow bool `json:"first_ship_before_window"`
}

type whPhase struct {
	Sessions      []whPhaseRow `json:"sessions"`
	Shipped       int          `json:"sessions_with_ship"`
	Ships         int          `json:"ships"`
	Before        float64      `json:"before_first_ship"`
	After         float64      `json:"after_first_ship"`
	NoShipSession int          `json:"sessions_without_ship"`
	NoShipCost    float64      `json:"cost_without_ship"`
	BeforeWindow  int          `json:"first_ship_before_window"`
}

type whTopRow struct {
	Session string  `json:"session"`
	Project string  `json:"project"`
	Launch  string  `json:"launch"`
	Cost    float64 `json:"cost"`
	Main    float64 `json:"main_cost"`
	Calls   int     `json:"calls"`
	Ships   int     `json:"ships"`
	Prompt  string  `json:"first_typed_prompt"`
}

type whLimits struct {
	Events     []whLimit `json:"events"`
	SpendAfter float64   `json:"spend_after_first"`
}

type whNotKnown struct {
	Synthetic     int            `json:"synthetic_records"`
	ScriptTools   map[string]int `json:"tool_blocks_run_by_scripts"`
	Compactions   int            `json:"compaction_boundaries"`
	Unreadable    []string       `json:"unreadable_files,omitempty"`
	FilesRead     int            `json:"files_considered"`
	Notes         []string       `json:"notes"`
	LaunchByDir   string         `json:"launch_kind_basis"`
	UnreadableCnt int            `json:"unreadable_count"`
}

type whReport struct {
	Since    string         `json:"since"`
	Until    string         `json:"until"`
	Cost     float64        `json:"cost"`
	Calls    int            `json:"calls"`
	Sessions int            `json:"sessions"`
	Parts    whParts        `json:"cost_parts"`
	Models   []whModelRow   `json:"models"`
	Agents   []whAgentRow   `json:"agents"`
	Projects []whGroupRow   `json:"projects"`
	Launches []whGroupRow   `json:"launch_kinds"`
	Activity []whActivity   `json:"premium_main_activity"`
	Triggers []whTriggerRow `json:"triggers"`
	Commands whCommands     `json:"premium_main_costliest_commands"`
	Phase    whPhase        `json:"phase"`
	Top      []whTopRow     `json:"top_sessions"`
	Limits   whLimits       `json:"limit_events"`
	NotKnown whNotKnown     `json:"not_counted_or_not_known"`
	TopN     int            `json:"top"`
}

func (sc *whScan) report(top int) *whReport {
	r := &whReport{Since: time.UnixMilli(sc.since).UTC().Format(time.RFC3339), Until: time.UnixMilli(sc.end).UTC().Format(time.RFC3339), TopN: top}
	models := map[string]*whModelRow{}
	ctxs := map[string][]int64{}
	runs := map[string]map[string]float64{} // agent kind -> transcript -> cost
	agentCalls := map[string]int{}
	projects := map[string]*whGroupRow{}
	launches := map[string]*whGroupRow{}
	acts := map[string]*whActivity{}
	actClass := map[string]map[string]*whClassRow{}
	trigs := map[string]*whTriggerRow{}
	metas := map[string]*whTriggerRow{}
	cmdRows := map[string]*whCommandRow{}
	cmdCalls := map[string]map[*whCall]bool{}
	for _, s := range sc.sessions {
		s.cost, s.mainCost, s.calls, s.ships = 0, 0, 0, 0
	}
	sessSeen := map[string]bool{}
	for _, c := range sc.order {
		ses := sc.sessions[c.session]
		// a ship after --until is not yet a ship as far as this window goes
		if c.ship && c.ts < sc.end && (ses.firstShip == 0 || c.ts < ses.firstShip) {
			ses.firstShip = c.ts
		}
	}
	for _, c := range sc.order {
		if !sc.in(c.ts) {
			continue
		}
		ses := sc.sessions[c.session]
		cost := c.parts.total()
		r.Cost += cost
		r.Calls++
		r.Parts.add(c.parts)
		sessSeen[c.session] = true
		ses.cost += cost
		ses.calls++
		ses.models[c.model] = true
		if c.main {
			ses.mainCost += cost
		}
		if c.ship {
			ses.ships++
		}
		mr := models[c.model]
		if mr == nil {
			mr = &whModelRow{Model: c.model}
			models[c.model] = mr
		}
		mr.Calls++
		mr.Cost += cost
		mr.add(c.parts)
		ctxs[c.model] = append(ctxs[c.model], c.ctx)

		agent := "main"
		if !c.main {
			agent = c.kind
		}
		agentCalls[agent]++
		if runs[agent] == nil {
			runs[agent] = map[string]float64{}
		}
		runs[agent][c.runID()] += cost

		if c.main {
			if premiumModel(c.model) {
				a := acts[c.model]
				if a == nil {
					a = &whActivity{Model: c.model}
					acts[c.model] = a
					actClass[c.model] = map[string]*whClassRow{}
				}
				a.Calls++
				a.Cost += cost
				for k, share := range c.shares() {
					row := actClass[c.model][k]
					if row == nil {
						row = &whClassRow{Class: k}
						actClass[c.model][k] = row
					}
					row.Cost += cost * share
					row.Calls++
				}
			}
		}
		trigs[c.trigger] = bump(trigs[c.trigger], &whTriggerRow{Trigger: c.trigger}, cost)
		if c.sub != "" {
			metas[c.sub] = bump(metas[c.sub], &whTriggerRow{Trigger: c.sub}, cost)
		}
		if c.main && premiumModel(c.model) {
			for _, cmds := range c.cmds {
				for _, cm := range cmds {
					k := cm.Class + "\x00" + cm.Head
					row := cmdRows[k]
					if row == nil {
						row = &whCommandRow{Head: cm.Head, Class: cm.Class, Tool: cm.Tool}
						cmdRows[k] = row
						cmdCalls[k] = map[*whCall]bool{}
					}
					row.Cost += cost / float64(len(c.cmds)) / float64(len(cmds))
					cmdCalls[k][c] = true
				}
			}
		}
	}
	for _, mr := range models {
		mr.MedianCtx = median(ctxs[mr.Model])
		r.Models = append(r.Models, *mr)
	}
	sort.Slice(r.Models, func(i, j int) bool { return r.Models[i].Cost > r.Models[j].Cost })
	for a, byRun := range runs {
		row := whAgentRow{Agent: a, Runs: len(byRun), Calls: agentCalls[a]}
		var v []float64
		for _, x := range byRun {
			row.Cost += x
			v = append(v, x)
			row.Max = max(row.Max, x)
		}
		sort.Float64s(v)
		row.Median = v[len(v)/2]
		r.Agents = append(r.Agents, row)
	}
	sort.Slice(r.Agents, func(i, j int) bool { return r.Agents[i].Cost > r.Agents[j].Cost })
	for id := range sessSeen {
		s := sc.sessions[id]
		launch := whLaunch(s.dir, s.prompt)
		s.launch = launch
		for _, g := range []struct {
			m    map[string]*whGroupRow
			name string
		}{{projects, s.project}, {launches, launch}} {
			row := g.m[g.name]
			if row == nil {
				row = &whGroupRow{Name: g.name}
				g.m[g.name] = row
			}
			row.Cost += s.cost
			row.Calls += s.calls
			row.Sessions++
		}
	}
	r.Projects, r.Launches = groupRows(projects), groupRows(launches)
	for _, a := range acts {
		for _, row := range actClass[a.Model] {
			a.Classes = append(a.Classes, *row)
		}
		sort.Slice(a.Classes, func(i, j int) bool { return a.Classes[i].Cost > a.Classes[j].Cost })
		r.Activity = append(r.Activity, *a)
	}
	sort.Slice(r.Activity, func(i, j int) bool { return r.Activity[i].Cost > r.Activity[j].Cost })
	for _, t := range trigs {
		if t.Trigger == "meta/hook message" {
			for _, m := range metas {
				t.Detail = append(t.Detail, *m)
			}
			sort.Slice(t.Detail, func(i, j int) bool { return t.Detail[i].Cost > t.Detail[j].Cost })
		}
		r.Triggers = append(r.Triggers, *t)
	}
	r.Commands.Model = "premium main threads"
	for k, row := range cmdRows {
		row.Calls = len(cmdCalls[k])
		r.Commands.Commands = append(r.Commands.Commands, *row)
		if row.Tool && row.Class == whOther {
			r.Commands.OtherTools = append(r.Commands.OtherTools, *row)
		}
	}
	for _, l := range []*[]whCommandRow{&r.Commands.Commands, &r.Commands.OtherTools} {
		rows := *l
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Cost != rows[j].Cost {
				return rows[i].Cost > rows[j].Cost
			}
			return rows[i].Head < rows[j].Head
		})
	}
	sort.Slice(r.Triggers, func(i, j int) bool { return r.Triggers[i].Cost > r.Triggers[j].Cost })

	// phase: main-thread cost either side of the session's first real wp ship
	before, after := map[string]float64{}, map[string]float64{}
	for _, c := range sc.order {
		if !c.main || !sc.in(c.ts) {
			continue
		}
		s := sc.sessions[c.session]
		if s.firstShip != 0 && c.ts > s.firstShip {
			after[c.session] += c.parts.total()
		} else {
			before[c.session] += c.parts.total()
		}
	}
	for id := range sessSeen {
		s := sc.sessions[id]
		if s.mainCost == 0 {
			continue
		}
		if s.firstShip == 0 {
			r.Phase.NoShipSession++
			r.Phase.NoShipCost += before[id]
			continue
		}
		r.Phase.Shipped++
		r.Phase.Ships += s.ships
		r.Phase.Before += before[id]
		r.Phase.After += after[id]
		r.Phase.Sessions = append(r.Phase.Sessions, whPhaseRow{Session: shortID(id), Project: s.project, Ships: s.ships, Before: before[id], After: after[id], FirstShipBeforeWindow: s.firstShip < sc.since})
		if s.firstShip < sc.since {
			r.Phase.BeforeWindow++
		}
	}
	sort.Slice(r.Phase.Sessions, func(i, j int) bool { return r.Phase.Sessions[i].After > r.Phase.Sessions[j].After })

	var ids []string
	for id := range sessSeen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := sc.sessions[ids[i]], sc.sessions[ids[j]]
		if a.cost != b.cost {
			return a.cost > b.cost
		}
		return ids[i] < ids[j]
	})
	r.Sessions = len(ids)
	for i, id := range ids {
		if top > 0 && i >= top {
			break
		}
		s := sc.sessions[id]
		r.Top = append(r.Top, whTopRow{Session: shortID(id), Project: s.project, Launch: s.launch, Cost: s.cost, Main: s.mainCost, Calls: s.calls, Ships: s.ships, Prompt: truncRunes(strings.Join(strings.Fields(s.prompt), " "), 100)})
	}

	sort.Slice(sc.limits, func(i, j int) bool { return sc.limits[i].ts < sc.limits[j].ts })
	r.Limits.Events = sc.limits
	if len(sc.limits) > 0 {
		for _, c := range sc.order {
			if sc.in(c.ts) && c.ts >= sc.limits[0].ts {
				r.Limits.SpendAfter += c.parts.total()
			}
		}
	}
	r.NotKnown = whNotKnown{
		Synthetic: sc.synthetic, ScriptTools: sc.scriptTool, Compactions: sc.boundaries, Unreadable: sc.unreadable, UnreadableCnt: len(sc.unreadable), FilesRead: sc.files,
		LaunchByDir: "project directory name (waypoint-runner-work-job, waypoint-runner-git) and the first prompt (Autopilot assigned, review prompt); the transcript has no launch field",
		Notes: []string{
			"dollars are list price from wp's price table, not percent of a quota: no client code converts one to the other",
			"<synthetic> records carry no usage (zero-usage inner calls of REPL scripts): left out of calls and cost; their tool blocks are counted under tool_blocks_run_by_scripts",
			"compaction boundaries: the summarising call carries no usage in transcripts, so its cost is not in the totals",
			"other machines are not included: this reads only the transcripts under --dir; runner jobs that delete their transcripts are missing",
			"launch kinds are guessed from directory names and first prompts; a session that matches none is listed as interactive or unknown",
			"activity classes split a call's cost equally over its tool blocks (and a block's over the classes of its commands): a judgement, not a measurement",
		},
	}
	return r
}

func groupRows(m map[string]*whGroupRow) []whGroupRow {
	var out []whGroupRow
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cost > out[j].Cost })
	return out
}

func bump(row, fresh *whTriggerRow, cost float64) *whTriggerRow {
	if row == nil {
		row = fresh
	}
	row.Calls++
	row.Cost += cost
	return row
}

func (c *whCall) runID() string { return c.session + "\x00" + c.kind + "\x00" + c.file }

func (sc *whScan) writeCSV(p string) error {
	fh, err := os.Create(p) // #nosec G304 -- the path the user named
	if err != nil {
		return err
	}
	w := csv.NewWriter(fh)
	_ = w.Write([]string{"session", "file_kind", "ts", "model", "trigger", "tool_classes", "context_tokens", "cost_input", "cost_output", "cost_cache_write_5m", "cost_cache_write_1h", "cost_cache_read", "cost_total"})
	f := func(x float64) string { return fmt.Sprintf("%.6f", x) }
	for _, c := range sc.order {
		if !sc.in(c.ts) {
			continue
		}
		kind := "main"
		if !c.main {
			kind = c.kind
		}
		_ = w.Write([]string{c.session, kind, time.UnixMilli(c.ts).UTC().Format(time.RFC3339), c.model, c.trigger, strings.Join(c.classes(), "+"), fmt.Sprint(c.ctx),
			f(c.parts.Input), f(c.parts.Output), f(c.parts.Write5m), f(c.parts.Write1h), f(c.parts.Read), f(c.parts.total())})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		_ = fh.Close()
		return err
	}
	return fh.Close()
}

var whTagRE = regexp.MustCompile(`^<([A-Za-z][\w-]*)[^>]*>`)

// whMetaKind says what a meta message is from its leading tag or first
// words: a system-reminder is told by the words after the tag.
func whMetaKind(text string) string {
	t := strings.TrimSpace(text)
	tag := ""
	if m := whTagRE.FindStringSubmatch(t); m != nil {
		tag, t = m[1], strings.TrimSpace(t[len(m[0]):])
		if tag != "system-reminder" {
			return "<" + tag + ">"
		}
	}
	w := strings.Fields(t)
	if len(w) > 5 {
		w = w[:5]
	}
	kind := truncRunes(strings.Join(w, " "), 50)
	if tag != "" {
		return "<" + tag + "> " + kind
	}
	if kind == "" {
		return "(empty)"
	}
	return kind
}
