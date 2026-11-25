package main

import (
	"encoding/json"
	"hash/fnv"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"git.jmann.me/claude/waypoint/internal/lazyre"
	trscript "git.jmann.me/claude/waypoint/internal/transcript"
)

// usageTraceVersion: a file read before traces (0) is read again, from the
// start, for its trace rows alone.
const usageTraceVersion = 1

// usageTraceRow is what is uploaded (the server's core.UsageTrace).
type usageTraceRow struct {
	Key          string           `json:"key"`
	Session      string           `json:"s"`
	Seq          int              `json:"seq"`
	Started      int64            `json:"started"`
	Last         int64            `json:"last"`
	Ended        string           `json:"ended,omitempty"`
	Model        string           `json:"model,omitempty"`
	Calls        int64            `json:"calls"`
	Locate       int64            `json:"locate"`
	Infile       int64            `json:"infile"`
	Reads        int64            `json:"reads"`
	Edits        int64            `json:"edits"`
	Delegated    int64            `json:"delegated"`
	Other        int64            `json:"other"`
	PreCalls     int64            `json:"pre_calls"`
	PreLocate    int64            `json:"pre_locate"`
	PreInfile    int64            `json:"pre_infile"`
	PreReads     int64            `json:"pre_reads"`
	PreDelegated int64            `json:"pre_delegated"`
	FirstEditAt  *int64           `json:"first_edit_at,omitempty"`
	CtxStart     int64            `json:"ctx_start"`
	CtxFirstEdit *int64           `json:"ctx_first_edit,omitempty"`
	Compacted    bool             `json:"compacted,omitempty"`
	Files        []usageTraceFile `json:"files,omitempty"`
}

type usageTraceFile struct {
	Path    string `json:"p"`
	Repo    string `json:"o,omitempty"` // owner/name of the file's repository, when its checkout's origin tells
	Nav     int    `json:"n"`
	Read    bool   `json:"r,omitempty"`
	Edited  bool   `json:"e,omitempty"`
	Seen    bool   `json:"s,omitempty"`
	Created bool   `json:"c,omitempty"`
}

const (
	traceMaxFiles   = 80
	traceMaxSurf    = 1000
	traceResultCap  = 30000
	traceMaxCmdPath = 12
	traceDropTokens = 5000
)

// call classes
const (
	trLocate    = "locate"
	trInfile    = "infile"
	trRead      = "read"
	trEdit      = "edit"
	trDelegated = "delegated"
	trOther     = "other"
)

// traceCall is what a tool call needs kept until its result is read.
type traceCall struct {
	Class string      `json:"c"`
	Paths []tracePath `json:"p,omitempty"`  // the files of the work it names
	Write bool        `json:"w,omitempty"`  // a Write: a new file when first touched
	Start bool        `json:"s,omitempty"`  // runs wp next or wp take
	End   string      `json:"e,omitempty"`  // runs wp ship, done or release
	Keys  []string    `json:"ek,omitempty"` // the tasks that command names (none: the one held)
	Scan  bool        `json:"x,omitempty"`  // its result may name files
	At    int64       `json:"t"`
	Ctx   int64       `json:"ctx,omitempty"`
	Model string      `json:"m,omitempty"`
}

type traceCounts struct {
	Calls, Locate, Infile, Reads, Edits, Delegated, Other int64
}

// traceSeg is the segment being read.
type traceSeg struct {
	Key        string           `json:"k"`
	Seq        int              `json:"n"`
	Start      int64            `json:"t"`
	Last       int64            `json:"l"`
	StartModel string           `json:"sm,omitempty"`
	CtxStart   int64            `json:"cs"`
	PrevCtx    int64            `json:"pc,omitempty"`
	Models     map[string]int   `json:"m,omitempty"`
	N          traceCounts      `json:"c"`
	Pre        traceCounts      `json:"p"`
	HasEdit    bool             `json:"he,omitempty"`
	FirstEdit  int64            `json:"fe,omitempty"`
	CtxEdit    int64            `json:"ce,omitempty"`
	Compacted  bool             `json:"cp,omitempty"`
	Files      []usageTraceFile `json:"f,omitempty"`
	Surf       []uint32         `json:"s,omitempty"` // hashes of file names an earlier result had named
}

// traceState is a transcript's trace memory: takes counted so far and the open segment.
type traceState struct {
	Seq     int          `json:"n"`
	Cur     *traceSeg    `json:"c,omitempty"`
	Scripts traceScripts `json:"sc,omitempty"`
}

func (t *traceState) clone() *traceState {
	if t == nil {
		return nil
	}
	c := *t
	c.Scripts = append(traceScripts(nil), t.Scripts...)
	if t.Cur != nil {
		s := *t.Cur
		s.Files = append([]usageTraceFile(nil), t.Cur.Files...)
		s.Surf = append([]uint32(nil), t.Cur.Surf...)
		if t.Cur.Models != nil {
			s.Models = make(map[string]int, len(t.Cur.Models))
			for k, v := range t.Cur.Models {
				s.Models[k] = v
			}
		}
		c.Cur = &s
	}
	return &c
}

var (
	traceKeyRE   = lazyre.New(`^([A-Z][A-Z0-9]{1,9}-\d+) "`)
	traceGivenRE = lazyre.New(`^(?:Autopilot assigned|.{1,120} started this session for) ([A-Z][A-Z0-9]{1,9}-\d+)(?: to you)?: "`)
	traceSearchH = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true, "find": true, "fd": true, "ls": true, "tree": true}
	traceReadH   = map[string]bool{"sed": true, "awk": true, "cat": true, "less": true, "head": true, "tail": true, "wc": true, "nl": true, "bat": true}
	traceIndexH  = map[string]bool{"wp code": true, "wp brief": true}
	traceGrepH   = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true, "git grep": true}
	traceLocateH = map[string]bool{"find": true, "fd": true, "ls": true, "tree": true, "git ls-files": true, "wp code": true, "wp brief": true}
)

// and, when the checkout's origin tells, its repository (owner/name).
type tracePath struct {
	P string `json:"p"`
	O string `json:"o,omitempty"`
}

// traceCheckout is the checkout a directory is in: the nearest directory at
// or above it that holds a .git, the repository its origin names, and the
// main checkout it is a worktree of (itself when it is one).
type traceCheckout struct{ root, repo, main string }

// traceGit remembers checkouts by directory: a sync reads hundreds of
// transcripts from a few directories.
var traceGit = struct {
	sync.Mutex
	dirs map[string]traceCheckout
}{dirs: map[string]traceCheckout{}}

// traceScratch are the directories agents keep scratch files in: nothing
// under them is a file of the work, whatever checkout it is in.
var traceScratch = []string{"/tmp/", "/var/tmp/"}

// traceWorktreeRE reads the path of a worktree kept inside its checkout, by
// Claude Code (.claude/worktrees, WP-ADR57) or by hand (.worktrees): the
// main checkout, the directory worktrees are kept in, the worktree's name,
// and the rest.
var traceWorktreeRE = lazyre.New(`^(.*?)/(\.claude/worktrees|\.worktrees)/([^/]+)(/.*)?$`)

// traceCheckoutOf finds a directory's checkout (the zero value when it is in
// none); a directory that is gone is in the checkout still above it. The
// caller holds traceGit's lock.
func traceCheckoutOf(dir string) traceCheckout {
	dir = path.Clean(dir)
	if v, ok := traceGit.dirs[dir]; ok {
		return v
	}
	var v traceCheckout
	if cfg := gitConfigFor(dir); cfg != "" {
		v.root, v.main = dir, dir
		if r, err := repoFromRemote(originURL(cfg)); err == nil {
			v.repo = r
		}
		if git := path.Dir(cfg); path.Base(git) == ".git" {
			v.main = path.Dir(git)
		}
	} else if up := path.Dir(dir); up != dir {
		v = traceCheckoutOf(up)
	}
	if len(traceGit.dirs) > 8192 {
		traceGit.dirs = map[string]traceCheckout{}
	}
	traceGit.dirs[dir] = v
	return v
}

// traceMainOf places a session whose files the disk can no longer place: the
// root its working directory has or had, and the main checkout that root is
// a worktree of (the root itself when nothing says it is one). A worktree
// that is there says so itself. Of one that is gone, a Claude Code worktree
// names its main checkout in its path, and a worktree made beside its
// checkout is named after it (app-fix beside app). The caller holds
// traceGit's lock.
func traceMainOf(cwd string) (root string, main traceCheckout) {
	if cwd == "" {
		return "", main
	}
	cwd = path.Clean(cwd)
	if c := traceCheckoutOf(cwd); c.root != "" {
		if m := traceCheckoutOf(c.main); m.root == c.main {
			return c.root, m
		}
		return c.root, traceCheckout{root: c.main, repo: c.repo, main: c.main}
	}
	if m := traceWorktreeRE.FindStringSubmatch(cwd); m != nil {
		return m[1] + "/" + m[2] + "/" + m[3], traceCheckout{root: m[1], main: m[1]}
	}
	if _, err := os.Stat(cwd); err != nil {
		for i := strings.LastIndexByte(cwd, '-'); i > strings.LastIndexByte(cwd, '/')+1; i = strings.LastIndexByte(cwd[:i], '-') {
			if c := traceCheckoutOf(cwd[:i]); c.root == cwd[:i] {
				return cwd, c
			}
		}
	}
	return cwd, traceCheckout{root: cwd, main: cwd}
}

func traceUnder(p, dir string) bool {
	return dir != "" && len(p) > len(dir)+1 && p[len(dir)] == '/' && strings.HasPrefix(p, dir)
}

// traceFile says whether a path is a file of the work, and names it when it
// can be placed.
//
// A file of the work is a file in a checkout, of whichever repository: a
// task's work is not always in the repository its session was started in, and
// the same repository is worked in from its main checkout, from worktrees
// beside it and from Claude Code's worktrees inside it, all of which name a
// file by the same path. Scratch directories and Claude Code's own directory
// hold none.
//
// Worktrees are removed when their work merges, and a transcript is often
// read after that. A file whose directory is gone and in no checkout is taken
// for a file of the work, so that the first edit of a take stays an edit; it
// is named only when the session's own checkout places it (see traceMainOf).
func traceFile(p, cwd string) (f tracePath, work bool) {
	if p == "" || strings.HasPrefix(p, "~") {
		return f, false
	}
	if !strings.HasPrefix(p, "/") {
		if cwd == "" {
			if p = path.Clean(p); p == "." || p == ".." || strings.HasPrefix(p, "../") {
				return f, false
			}
			return tracePath{P: p}, true
		}
		p = cwd + "/" + p
	}
	p = path.Clean(p)
	for _, d := range traceScratch {
		if strings.HasPrefix(p, d) {
			return f, false
		}
	}
	if traceUnder(p, claudeDir("")) {
		return f, false
	}
	traceGit.Lock()
	defer traceGit.Unlock()
	rel, repo := "", ""
	// A checkout's own root (gofmt -w . run in it) is the work, though no
	// file of it is named.
	if self := traceCheckoutOf(p); self.root == p {
		return f, true
	} else if m := traceWorktreeRE.FindStringSubmatch(p); m != nil && m[4] == "" {
		return f, true
	}
	if own := traceCheckoutOf(path.Dir(p)); traceUnder(p, own.root) {
		rel, repo = p[len(own.root)+1:], own.repo
	} else {
		if _, err := os.Stat(path.Dir(p)); err == nil {
			return f, false
		}
		root, main := traceMainOf(cwd)
		repo = main.repo
		switch {
		case traceUnder(p, root):
			rel = p[len(root)+1:]
		case traceUnder(p, main.root):
			rel = p[len(main.root)+1:]
		case path.Dir(main.root) == "/" && strings.HasPrefix(p, "/"), traceUnder(p, path.Dir(main.root)):
			parent := strings.TrimSuffix(path.Dir(main.root), "/")
			if name, in, ok := strings.Cut(p[len(parent)+1:], "/"); ok && strings.HasPrefix(name, path.Base(main.root)+"-") {
				rel = in
			}
		}
		if rel == "" {
			return f, true
		}
	}
	if m := traceWorktreeRE.FindStringSubmatch("/" + rel); m != nil && m[1] == "" {
		if rel = strings.TrimPrefix(m[4], "/"); rel == "" {
			return f, false
		}
	}
	return tracePath{P: rel, O: repo}, true
}

type traceInput struct {
	Command      string `json:"command"`
	Content      string `json:"content"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Path         string `json:"path"`
}

// out, is no file the parser can speak of.
func traceWord(w, dir, cwd string) (f tracePath, work bool) {
	abs := strings.HasPrefix(w, "/")
	if w == "" || w[0] == '$' || w[0] == '`' || (dir == traceNowhere && !abs) {
		return f, false
	}
	if !abs && dir != "" {
		w = dir + "/" + w
	}
	if i := strings.IndexAny(w, "$`*?[{"); i >= 0 {
		_, work = traceFile(w[:i]+"x", cwd)
		return f, work
	}
	return traceFile(w, cwd)
}

// traceWordPaths: the files a command line names, in order.
func traceWordPaths(cmds []traceCommand, cwd string) (out []tracePath) {
	for _, c := range cmds {
		for _, w := range c.Words {
			if strings.HasPrefix(w, "-") {
				if _, v, ok := strings.Cut(w, "="); ok {
					w = v
				} else {
					continue
				}
			}
			if i := strings.IndexByte(w, ':'); i > 0 {
				w = w[:i]
			}
			if !traceFileShape(w) {
				continue
			}
			if f, _ := traceWord(w, c.Dir, cwd); f.P != "" && traceFileShape(f.P) && !hasPath(out, f) {
				if out = append(out, f); len(out) == traceMaxCmdPath {
					return out
				}
			}
		}
	}
	return out
}

func hasPath(s []tracePath, v tracePath) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// classifyTraceCall sorts one tool call (nav.py's cls and transcripts.py's classify_bash).
func classifyTraceCall(name string, input json.RawMessage, cwd string, at, ctx int64, model string, mem *traceScripts) *traceCall {
	var in traceInput
	_ = json.Unmarshal(input, &in)
	tc := &traceCall{At: at, Ctx: ctx, Model: model, Class: trOther}
	switch name {
	case "Read":
		tc.Class = trRead
		if f, _ := traceFile(in.FilePath, cwd); f.P != "" {
			tc.Paths = []tracePath{f}
		}
	case "Grep", "Glob":
		tc.Class, tc.Scan = trLocate, true
		if f, _ := traceFile(in.Path, cwd); f.P != "" && traceFileShape(f.P) {
			tc.Class, tc.Paths = trInfile, []tracePath{f}
		}
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		if f, work := traceFile(usageOr(in.FilePath, in.NotebookPath), cwd); work {
			tc.Class, tc.Write = trEdit, name == "Write"
			if f.P != "" {
				tc.Paths = []tracePath{f}
			}
		}
		if name == "Write" {
			mem.Keep(trscript.ShellAt(in.FilePath, cwd), in.Content)
		}
	case "Agent", "Task":
		tc.Class, tc.Scan = trDelegated, true
	case "Bash":
		tc.Scan = true
		var cmds []traceCommand
		home, _ := os.UserHomeDir()
		traceCommands(in.Command, cwd, 0, map[string]string{"HOME": home}, mem, &cmds)
		for _, c := range cmds {
			switch c.Head {
			case "wp next", "wp take":
				if !traceHasFlag(c.Words, "-h", "--help", "--peek") {
					tc.Start = true
				}
			case "wp ship", "wp done", "wp release":
				if !traceHasFlag(c.Words, "-h", "--help") {
					tc.End, tc.Keys = strings.TrimPrefix(c.Head, "wp "), traceKeys(c.Words)
				}
			}
		}
		tc.Class, tc.Paths, tc.Write = classifyTraceBash(cmds, cwd)
	}
	return tc
}

// classifyTraceBash sorts a Bash call. It is an edit when any of its
// commands writes a file of the work (traceCommand.writes); the files are
// the ones written. Otherwise it is navigation when it begins with a search
// or a read; a command that sends its output to a file shows the agent
// nothing and counts as neither. whole says every file it wrote was written
// whole, so one not touched before is a file the agent made.
func classifyTraceBash(cmds []traceCommand, cwd string) (class string, paths []tracePath, whole bool) {
	var edits []tracePath
	edit := false
	whole = true
	wrote := make([]bool, len(cmds))
	for i := range cmds {
		for _, w := range cmds[i].Wrote {
			wrote[i] = wrote[i] || w.Kind == "redirect" || w.Kind == "output" || w.Kind == "sed"
			if w.Kind == "output" {
				continue
			}
			f, work := traceWord(w.Path, cmds[i].Dir, cwd)
			edit = edit || work
			whole = whole && (!work || w.Whole)
			if work && f.P != "" && !hasPath(edits, f) && len(edits) < traceMaxCmdPath {
				edits = append(edits, f)
			}
		}
	}
	if edit {
		return trEdit, edits, whole
	}
	search, read := 0, 0
	first := ""
	for i, c := range cmds {
		h := c.Head
		if first == "" && h != "cd" {
			first = h
		}
		switch {
		case wrote[i]:
		case traceSearchH[h] || h == "git grep" || h == "git ls-files" || traceIndexH[h]:
			search++
		case traceReadH[h]:
			read++
		}
	}
	nav := search+read > 0 && (traceSearchH[first] || traceReadH[first] || traceIndexH[first] || first == "git grep" || first == "git ls-files")
	if !nav {
		return trOther, nil, false
	}
	ps := traceWordPaths(cmds, cwd)
	var grep, locate bool
	for i, c := range cmds {
		if i >= 6 {
			break
		}
		grep = grep || traceGrepH[c.Head]
		locate = locate || traceLocateH[c.Head]
	}
	switch {
	case locate:
		return trLocate, ps, false
	case grep && len(ps) > 0:
		return trInfile, ps, false
	case grep:
		return trLocate, ps, false
	}
	return trRead, ps, false
}

func traceHasFlag(words []string, flags ...string) bool {
	for _, w := range words {
		for _, f := range flags {
			if w == f {
				return true
			}
		}
	}
	return false
}

var traceKeyWordRE = lazyre.New(`^[A-Z][A-Z0-9]{1,9}-\d+$`)

// traceKeys lists the tasks a wp command names as arguments (a flag's value
// is not one).
func traceKeys(words []string) (keys []string) {
	for i, w := range words {
		if i > 0 && traceKeyWordRE.MatchString(w) && !strings.HasPrefix(words[i-1], "-") {
			keys = append(keys, w)
		}
	}
	return keys
}

// traceTakeKey reads the task a wp next or wp take answer gives: its first
// line, after the notes in brackets the answer may open with ("(Passed over
// WP-5.)", "(You already hold this task.)"), in each form the server writes
// it (agentTaskText).
func traceTakeKey(text string) string {
	for n := 0; n < 8; n++ {
		line, rest, _ := strings.Cut(text, "\n")
		text = rest
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "(") && strings.HasSuffix(line, ")") {
			continue
		}
		if m := traceKeyRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
		if m := traceGivenRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
		return ""
	}
	return ""
}

// namedFiles hashes the base names of the files a tool result names.
func namedFiles(text string) []uint32 {
	var out []uint32
	seen := map[uint32]bool{}
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		w := text[start:end]
		start = -1
		if traceFileShape(w) {
			h := hashName(path.Base(w))
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '@' || c == '+' || c == '/' || c == '-' {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(text))
	return out
}

func hashName(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func (s *traceSeg) navBefore() int { return int(s.N.Locate + s.N.Infile + s.N.Reads) }

func (s *traceSeg) surfaced(h uint32) bool {
	for _, x := range s.Surf {
		if x == h {
			return true
		}
	}
	return false
}

func (s *traceSeg) touch(tc *traceCall) {
	for _, p := range tc.Paths {
		var f *usageTraceFile
		for i := range s.Files {
			if s.Files[i].Path == p.P && s.Files[i].Repo == p.O {
				f = &s.Files[i]
				break
			}
		}
		if f == nil {
			if len(s.Files) >= traceMaxFiles {
				continue
			}
			s.Files = append(s.Files, usageTraceFile{Path: p.P, Repo: p.O, Nav: s.navBefore(), Seen: s.surfaced(hashName(path.Base(p.P))), Created: tc.Write && tc.Class == trEdit})
			f = &s.Files[len(s.Files)-1]
		}
		if tc.Class == trEdit {
			f.Edited = true
		} else {
			f.Read = true
		}
	}
}

func (s *traceSeg) add(tc *traceCall) {
	if tc.Class == trRead || tc.Class == trInfile || tc.Class == trEdit {
		s.touch(tc)
	}
	if tc.Class == trEdit && !s.HasEdit {
		s.Pre, s.HasEdit, s.FirstEdit, s.CtxEdit = s.N, true, tc.At, tc.Ctx
	}
	if !s.HasEdit && tc.Ctx > 0 {
		if s.PrevCtx > 0 && tc.Ctx+traceDropTokens < s.PrevCtx {
			s.Compacted = true
		}
		s.PrevCtx = tc.Ctx
	}
	s.N.Calls++
	switch tc.Class {
	case trLocate:
		s.N.Locate++
	case trInfile:
		s.N.Infile++
	case trRead:
		s.N.Reads++
	case trEdit:
		s.N.Edits++
	case trDelegated:
		s.N.Delegated++
	default:
		s.N.Other++
	}
	if tc.Model != "" {
		if s.Models == nil {
			s.Models = map[string]int{}
		}
		s.Models[tc.Model]++
	}
	s.Last = max(s.Last, tc.At)
}

func (s *traceSeg) row(session, ended string) usageTraceRow {
	pre := s.N
	if s.HasEdit {
		pre = s.Pre
	}
	model, best := s.StartModel, 0
	names := make([]string, 0, len(s.Models))
	for m := range s.Models {
		names = append(names, m)
	}
	sort.Strings(names)
	for _, m := range names {
		if s.Models[m] > best {
			model, best = m, s.Models[m]
		}
	}
	r := usageTraceRow{Key: s.Key, Session: session, Seq: s.Seq, Started: s.Start, Last: max(s.Last, s.Start), Ended: ended, Model: model,
		Calls: s.N.Calls, Locate: s.N.Locate, Infile: s.N.Infile, Reads: s.N.Reads, Edits: s.N.Edits, Delegated: s.N.Delegated, Other: s.N.Other,
		PreCalls: pre.Calls, PreLocate: pre.Locate, PreInfile: pre.Infile, PreReads: pre.Reads, PreDelegated: pre.Delegated,
		CtxStart: s.CtxStart, Compacted: s.Compacted, Files: append([]usageTraceFile(nil), s.Files...)}
	if s.HasEdit {
		fe, ce := s.FirstEdit, s.CtxEdit
		r.FirstEditAt, r.CtxFirstEdit = &fe, &ce
		if ce > 0 && s.CtxStart > 0 && ce < s.CtxStart {
			r.Compacted = true
		}
	}
	return r
}

// traceResult takes a call whose result has been read into the trace.
func (p *usageParser) traceResult(tc *traceCall, bl *rawBlock, refused bool) {
	if !p.f.Main {
		return
	}
	ts := p.st.Trace
	surface := func(cur *traceSeg) {
		for _, h := range namedFiles(resultText(bl.Content, traceResultCap)) {
			if len(cur.Surf) >= traceMaxSurf {
				break
			}
			if !cur.surfaced(h) {
				cur.Surf = append(cur.Surf, h)
			}
		}
	}
	if tc.Start && !bl.IsError && !refused {
		if key := traceTakeKey(resultText(bl.Content, 800)); key != "" {
			if ts == nil {
				ts = &traceState{}
				p.st.Trace = ts
			}
			p.traceTouched = true
			if ts.Cur != nil && ts.Cur.Key == key {
				surface(ts.Cur)
				return
			}
			if ts.Cur != nil {
				p.traceClose("next")
			}
			ts.Seq++
			ts.Cur = &traceSeg{Key: key, Seq: ts.Seq, Start: tc.At, Last: tc.At, StartModel: tc.Model, CtxStart: tc.Ctx, PrevCtx: tc.Ctx}
			surface(ts.Cur)
			return
		}
	}
	if ts == nil || ts.Cur == nil || refused {
		return
	}
	p.traceTouched = true
	cur := ts.Cur
	if tc.End != "" && !bl.IsError && (len(tc.Keys) == 0 || slices.Contains(tc.Keys, cur.Key)) {
		p.traceClose(tc.End)
		return
	}
	if bl.IsError && tc.Class == trEdit && !tc.Scan {
		failed := *tc
		failed.Class, failed.Paths, failed.Write = trOther, nil, false
		tc = &failed
	}
	cur.add(tc)
	if tc.Scan {
		surface(cur)
	}
}

// traceClose ends the open segment, keeping its final row for this run's chunk.
func (p *usageParser) traceClose(ended string) {
	ts := p.st.Trace
	if ts == nil || ts.Cur == nil {
		return
	}
	if p.traceRows == nil {
		p.traceRows = map[int]usageTraceRow{}
	}
	p.traceRows[ts.Cur.Seq] = ts.Cur.row(p.f.Session, ended)
	ts.Cur = nil
}

// traceFinish lists the rows this run changed: segments closed in it, and the
// open one if it was touched.
func (p *usageParser) traceFinish() []usageTraceRow {
	if !p.f.Main {
		return nil
	}
	rows := p.traceRows
	if ts := p.st.Trace; ts != nil && ts.Cur != nil && p.traceTouched {
		if rows == nil {
			rows = map[int]usageTraceRow{}
		}
		rows[ts.Cur.Seq] = ts.Cur.row(p.f.Session, "")
	}
	out := make([]usageTraceRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}
