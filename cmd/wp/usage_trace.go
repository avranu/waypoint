package main

import (
	"encoding/json"
	"hash/fnv"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"git.jmann.me/claude/waypoint/internal/lazyre"
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

// traceScripts remembers the scripts a session kept in files, by where it
// put them, with what each writes: an agent that writes edit.py to a scratch
// directory and runs it has edited what edit.py writes, at the call that
// runs it. Only the paths a script writes are kept, never its text.
type traceScripts []traceScript

type traceScript struct {
	At     string   `json:"a"`
	Writes []string `json:"w"`
	Unsure bool     `json:"u,omitempty"` // no written path could be followed: every file it names
}

const (
	traceMaxScripts     = 24
	traceMaxScriptPaths = 24
)

var traceScriptFileRE = lazyre.New(`\.(?:py|js|mjs|cjs|ts|rb)$`)

// keep notes the script now at a path, replacing what was there; a script
// that writes nothing is only forgotten.
func (m *traceScripts) keep(at, body string) {
	if m == nil || !traceScriptFileRE.MatchString(at) {
		return
	}
	for i := range *m {
		if (*m)[i].At == at {
			*m = append((*m)[:i:i], (*m)[i+1:]...)
			break
		}
	}
	ps, sure := traceScriptWrites(body)
	if len(ps) == 0 {
		return
	}
	if len(ps) > traceMaxScriptPaths {
		ps = ps[:traceMaxScriptPaths]
	}
	if len(*m) >= traceMaxScripts {
		*m = append((*m)[:0:0], (*m)[1:]...)
	}
	*m = append(*m, traceScript{At: at, Writes: ps, Unsure: !sure})
}

func (m *traceScripts) at(at string) *traceScript {
	if m == nil {
		return nil
	}
	for i := range *m {
		if (*m)[i].At == at {
			return &(*m)[i]
		}
	}
	return nil
}

// traceAt names where a word of a command points, to match a script kept
// there with the command that runs it: the path when the line says it, else
// the word as written ($S/edit.py is the same file both times).
func traceAt(w, dir string) string {
	switch {
	case strings.HasPrefix(w, "/"):
		return path.Clean(w)
	case w == "" || w[0] == '$' || dir == "" || dir == traceNowhere:
		return w
	}
	return path.Join(dir, w)
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
	traceExts    = map[string]bool{}
	traceSearchH = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true, "find": true, "fd": true, "ls": true, "tree": true}
	traceReadH   = map[string]bool{"sed": true, "awk": true, "cat": true, "less": true, "head": true, "tail": true, "wc": true, "nl": true, "bat": true}
	traceIndexH  = map[string]bool{"wp code": true, "wp brief": true}
	traceGrepH   = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true, "git grep": true}
	traceLocateH = map[string]bool{"find": true, "fd": true, "ls": true, "tree": true, "git ls-files": true, "wp code": true, "wp brief": true}
)

func init() {
	for _, e := range strings.Fields("go ts tsx js jsx mjs cjs py sql md sh yml yaml json css scss html rs kt kts java toml mk tf proto vue svelte rb php c h cpp txt xml gradle ini cfg conf service tmpl") {
		traceExts[e] = true
	}
}

// traceFileShape says a word names a file: a name with a known extension, or
// Makefile or Dockerfile, made of path characters only.
func traceFileShape(w string) bool {
	if w == "" || len(w) > 400 {
		return false
	}
	for i := 0; i < len(w); i++ {
		c := w[i]
		pathChar := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.@+/-", c) >= 0
		if !pathChar {
			return false
		}
	}
	base := path.Base(w)
	if base == "Makefile" || base == "Dockerfile" {
		return true
	}
	i := strings.LastIndexByte(base, '.')
	return i > 0 && traceExts[base[i+1:]] && !strings.HasSuffix(w, "/")
}

// tracePath names a file of the work: its path from the root of its checkout
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

// traceCommand is one command of a Bash call.
type traceCommand struct {
	words  []string
	head   string
	dir    string
	redirs []string
	script string
	wrote  []traceWrite
}

// traceNowhere is the directory after a cd the line doesn't spell out
// (cd "$d", cd -): a relative path under it can't be placed.
const traceNowhere = "\x00"

var traceVarRE = lazyre.New(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// traceCommands lists the commands of a line in order, going into bash -c
// and a heredoc given to a shell, on the guard's shell reader. It follows
// what the line itself says about where each command runs and what its
// variables hold: `cd web && cat src/x.ts` reads web/src/x.ts, and
// `S=/tmp/s; cat > $S/x.py` writes a scratch file. It returns the directory
// the line ends in.
func traceCommands(cmd, cwd string, depth int, vars map[string]string, mem *traceScripts, out *[]traceCommand) string {
	dir := cwd
	fill := func(w string) string {
		if w == "~" || strings.HasPrefix(w, "~/") {
			w = "$HOME" + w[1:]
		}
		if !strings.Contains(w, "$") {
			return w
		}
		return traceVarRE.ReplaceAllStringFunc(w, func(m string) string {
			name := strings.Trim(m, "${}")
			if name == "PWD" && dir != traceNowhere && dir != "" {
				return dir
			}
			if v, ok := vars[name]; ok {
				return v
			}
			return m
		})
	}
	set := func(words []string) {
		for _, w := range words {
			if name, v, ok := strings.Cut(w, "="); ok && assignRE.MatchString(w) && !strings.HasSuffix(name, "+") {
				vars[name] = v
			}
		}
	}
	for _, g := range parseShell(cmd) {
		all := make([]string, len(g.words))
		assigns := len(g.words) > 0
		for i, w := range g.words {
			all[i] = fill(w)
			assigns = assigns && assignRE.MatchString(w)
		}
		if assigns {
			set(all)
			continue
		}
		words := stripWords(all)
		if len(words) == 0 {
			continue
		}
		if depth < 3 && shells[path.Base(words[0])] {
			if inner, ok := shellDashC(words[1:]); ok {
				traceCommands(inner, dir, depth+1, vars, mem, out)
				continue
			}
			if g.heredoc != "" {
				traceCommands(g.heredoc, dir, depth+1, vars, mem, out)
				continue
			}
		}
		c := traceCommand{words: words, head: headOf(words), dir: dir, script: g.heredoc}
		for _, r := range g.redirs {
			c.redirs = append(c.redirs, fill(r))
		}
		switch {
		case c.head == "cd" || c.head == "pushd":
			dir = traceCd(dir, words[1:])
		case c.head == "popd":
			dir = traceNowhere
		case c.head == "export":
			set(words[1:])
		case words[0] == "git":
			for i := 1; i+1 < len(words) && strings.HasPrefix(words[i], "-"); i++ {
				if words[i] == "-C" {
					c.dir = traceCd(dir, words[i+1:i+2])
				}
				if traceGitValueFlags[words[i]] {
					i++
				}
			}
		}
		c.wrote = c.writes()
		for i, w := range c.wrote {
			if strings.HasPrefix(w.kind, "script") {
				c.wrote[i].w = fill(w.w)
			}
		}
		if prog := path.Base(words[0]); c.script != "" && (prog == "cat" || prog == "tee") {
			for _, w := range c.wrote {
				mem.keep(traceAt(w.w, c.dir), c.script)
			}
		} else if k := mem.at(traceAt(c.ran(), c.dir)); k != nil {
			kind := "script"
			if k.Unsure {
				kind = "script?"
			}
			for _, w := range k.Writes {
				c.wrote = append(c.wrote, traceWrite{kind: kind, w: w})
			}
		}
		vars["_"] = words[len(words)-1]
		*out = append(*out, c)
	}
	return dir
}

// traceCd is the directory after `cd ARGS` in dir.
func traceCd(dir string, args []string) string {
	to := ""
	for _, a := range args {
		if a == "-" || !strings.HasPrefix(a, "-") {
			to = a
			break
		}
	}
	switch {
	case to == "" || to == "-" || strings.ContainsAny(to, "$`*?"):
		return traceNowhere
	case strings.HasPrefix(to, "/"):
		return path.Clean(to)
	case dir == "" || dir == traceNowhere:
		return traceNowhere
	}
	return path.Join(dir, to)
}

// traceWord places a word of a command as a file, like traceFile. A word the
// line spells out only the start of (internal/*.go, clusters/$site/app.yaml)
// is placed by that start and not named. One it doesn't spell out the start
// of ($f, a substitution), or a relative one after a cd it doesn't spell
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

// interpreter finds the interpreter a command runs (python3, uv run python,
// node), as its place among the words, or -1.
func (c *traceCommand) interpreter() int {
	for i := 0; i < len(c.words) && i < 4; i++ {
		if b := path.Base(c.words[i]); traceInterpreters[b] || strings.HasPrefix(b, "python3.") {
			return i
		}
	}
	return -1
}

// ran is the script file a command hands its interpreter (python3 edit.py),
// "" when it runs none.
func (c *traceCommand) ran() string {
	i := c.interpreter()
	if i < 0 || c.script != "" {
		return ""
	}
	for _, w := range c.words[i+1:] {
		switch {
		case w == "-c" || w == "-e" || w == "-m" || w == "-":
			return ""
		case strings.HasPrefix(w, "-") || w == "run":
		default:
			if traceScriptFileRE.MatchString(w) {
				return w
			}
			return ""
		}
	}
	return ""
}

// The write calls of a script given to an interpreter, and the path each
// writes: open(P, 'w'), P.write_text(, Path(P).write_text(, writeFileSync(P,.
var (
	traceScriptWriteRE = lazyre.New(`\bopen\([^\n]*?,\s*(?:mode\s*=\s*)?['"][wax]|\.write_(?:text|bytes)\(|\b(?:write|append)File(?:Sync)?\(|\bFile\.write\(`)
	traceScriptSiteREs = []*lazyre.Regexp{
		lazyre.New(`\bopen\(\s*([^,()\n]+?|(?:pathlib\.)?Path\(\s*[^()\n]+?\s*\))\s*,\s*(?:mode\s*=\s*)?['"][wax]`),
		lazyre.New(`((?:pathlib\.)?Path\(\s*[^()\n]+?\s*\)|[A-Za-z_][A-Za-z0-9_]*)\.write_(?:text|bytes)\(`),
		lazyre.New(`\b(?:write|append)File(?:Sync)?\(\s*([^,()\n]+?)\s*,`),
		lazyre.New(`\bFile\.write\(\s*([^,()\n]+?)\s*,`),
	}
	traceScriptPathRE = lazyre.New(`^(?:pathlib\.)?Path\(\s*(.+?)\s*\)$`)
	traceScriptStrRE  = lazyre.New(`'([^'\n]{1,400})'|"([^"\n]{1,400})"`)
	traceIdentRE      = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*$`)
	traceInterpreters = map[string]bool{"python": true, "python3": true, "node": true, "ruby": true, "bun": true, "deno": true}
)

const traceMaxScript = 256 << 10

// traceScriptWrites lists the paths a script writes, as the script spells
// them. A path given as a name is followed to what the script sets the name
// to (p = 'a.go'; for p in ['a.go', 'b.go']). sure is false when the script
// writes but no path could be followed: then every file the script names is
// listed, and the caller can't tell the written from the read.
func traceScriptWrites(script string) (paths []string, sure bool) {
	if len(script) > traceMaxScript {
		script = script[:traceMaxScript]
	}
	if !traceScriptWriteRE.MatchString(script) {
		return nil, true
	}
	for _, re := range traceScriptSiteREs {
		for _, m := range re.FindAllStringSubmatch(script, 64) {
			paths = append(paths, traceScriptExpr(script, m[1], 0)...)
		}
	}
	if len(paths) > 0 {
		return paths, true
	}
	for _, m := range traceScriptStrRE.FindAllStringSubmatch(script, 2000) {
		s := m[1] + m[2]
		if !traceFileShape(s) {
			continue
		}
		q := `['"]` + regexp.QuoteMeta(s) + `['"]\s*\)`
		if read := regexp.MustCompile(`\bopen\(\s*` + q + `|` + q + `\.read_(?:text|bytes)\(`); read.MatchString(script) {
			continue
		}
		paths = append(paths, s)
	}
	return paths, false
}

// traceScriptExpr follows a path expression of a script to the strings it
// can be: a string itself, Path(of one), a name the script sets to one, or
// the first parameter of a function the script defines, by what its calls
// pass (def sub(p, old, new): …; sub('a.go', …)).
func traceScriptExpr(script, expr string, depth int) []string {
	expr = strings.TrimSpace(expr)
	if m := traceScriptPathRE.FindStringSubmatch(expr); m != nil {
		expr = m[1]
	}
	if n := len(expr); n >= 2 && (expr[0] == '\'' || expr[0] == '"') && expr[n-1] == expr[0] {
		if s := expr[1 : n-1]; !strings.ContainsAny(s, `'"`) {
			return []string{s}
		}
		return nil
	}
	if depth >= 3 || !traceIdentRE.MatchString(expr) {
		return nil
	}
	var out []string
	set := regexp.MustCompile(`(?m)(?:^|[;\s])(?:const |let |var )?` + expr + `\s*=\s*([^=\n;][^\n;]*)`)
	for _, m := range set.FindAllStringSubmatch(script, 16) {
		out = append(out, traceScriptExpr(script, m[1], depth+1)...)
	}
	loop := regexp.MustCompile(`\bfor\s+` + expr + `\s+in\s+[\[(]([^\n]*)[\])]\s*:`)
	for _, m := range loop.FindAllStringSubmatch(script, 16) {
		for _, s := range traceScriptStrRE.FindAllStringSubmatch(m[1], 64) {
			out = append(out, s[1]+s[2])
		}
	}
	def := regexp.MustCompile(`\bdef\s+([A-Za-z_][A-Za-z0-9_]*)\(\s*` + expr + `\s*[,):=]`)
	for _, m := range def.FindAllStringSubmatch(script, 4) {
		call := regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])` + m[1] + `\(\s*([^,()\n]+?)\s*[,)]`)
		for _, c := range call.FindAllStringSubmatch(script, 64) {
			if c[1] != expr {
				out = append(out, traceScriptExpr(script, c[1], depth+1)...)
			}
		}
	}
	return out
}

// traceWrite is one file a command writes, as the command spells it.
type traceWrite struct {
	kind  string
	w     string
	whole bool
}

// writes lists the files a command writes. What counts: output sent to a
// file, tee, sed -i and perl -i, mv and cp, git mv and git rm, a patch
// applied, and a script given to an interpreter that writes files. What
// doesn't: what a program writes of its own accord (a formatter, go mod
// tidy, git merge or checkout), which the call doesn't show; and a plain rm,
// which is as often clearing up. A script the session kept in a file and
// runs later is added by traceCommands, which remembers them.
//
// Output sent to a file is a write of what the agent typed (cat, echo,
// printf) wherever it goes; of another program's output only when the file
// is named like a source, document or configuration file (docs/CLI.md, not
// server.log).
func (c *traceCommand) writes() []traceWrite {
	var out []traceWrite
	prog, args := path.Base(c.words[0]), c.words[1:]
	for _, r := range c.redirs {
		switch {
		case strings.HasPrefix(r, "/dev/"):
		case (traceReporters[prog] || prog == "git" && !traceGitWritesText(args)) && traceReportRE.MatchString(path.Base(r)):
			out = append(out, traceWrite{kind: "output", w: r})
		case prog == "cat" || prog == "echo" || prog == "printf" || traceFileShape(path.Base(r)):
			out = append(out, traceWrite{kind: "redirect", w: r, whole: prog == "cat" && c.script != ""})
		default:
			out = append(out, traceWrite{kind: "output", w: r})
		}
	}
	if prog == "git" && len(args) > 0 {
		args = traceGitArgs(args)
		if len(args) > 0 {
			prog, args = "git "+args[0], args[1:]
		}
	}
	plain := func(skip int) (ws []string) {
		for _, a := range args {
			if strings.HasPrefix(a, "-") && a != "-" {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			ws = append(ws, a)
		}
		return ws
	}
	each := func(kind string, ws []string) {
		for _, w := range ws {
			out = append(out, traceWrite{kind: kind, w: w})
		}
	}
	has := func(flags ...string) bool {
		for _, a := range args {
			for _, f := range flags {
				if a == f {
					return true
				}
			}
		}
		return false
	}
	switch prog {
	case "tee":
		for _, w := range plain(0) {
			out = append(out, traceWrite{kind: "tee", w: w, whole: c.script != ""})
		}
	case "sed":
		if sedInPlace(c.words) {
			skip := 1
			var files []string
			for i := 0; i < len(args); i++ {
				a := args[i]
				switch {
				case a == "-e" || a == "-f" || a == "--expression" || a == "--file":
					skip = 0
					i++
				case strings.HasPrefix(a, "-"):
				default:
					files = append(files, a)
				}
			}
			if skip < len(files) {
				each("sed", files[skip:])
			}
		}
	case "perl":
		inPlace := false
		var files []string
		for i := 0; i < len(args); i++ {
			a := args[i]
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
				f := a[1:]
				if j := strings.IndexByte(f, 'i'); j >= 0 && !strings.ContainsAny(f[:j], "eEI") {
					inPlace = true
				}
				if strings.HasSuffix(f, "e") || strings.HasSuffix(f, "E") {
					i++
				}
				continue
			}
			files = append(files, a)
		}
		if inPlace {
			each("perl", files)
		}
	case "mv", "cp", "git mv":
		if ws := plain(0); len(ws) >= 2 {
			to, from := ws[len(ws)-1], ws[:len(ws)-1]
			kind := strings.TrimPrefix(prog, "git ")
			switch {
			case traceIntoDir(to, c.dir, len(from)):
				for _, s := range from {
					out = append(out, traceWrite{kind: kind, w: strings.TrimSuffix(to, "/") + "/" + path.Base(s), whole: true})
				}
			case prog == "cp" && traceBackupRE.MatchString(to):
				out = append(out, traceWrite{kind: "output", w: to})
			default:
				out = append(out, traceWrite{kind: kind, w: to, whole: true})
			}
			if prog != "cp" {
				each("mv", from)
			}
		}
	case "git rm":
		each("rm", plain(0))
	case "xargs", "find":
		for i, a := range args {
			if b := path.Base(a); b == "sed" && sedInPlace(args[i:]) || b == "perl" && i+1 < len(args) && perlInPlaceRE.MatchString(args[i+1]) {
				out = append(out, traceWrite{kind: "patch", w: "*"})
				break
			}
		}
	case "patch", "git apply", "git am":
		if !has("--check", "--stat", "--numstat", "--summary", "--dry-run", "-R", "--reverse") {
			out = append(out, traceWrite{kind: "patch", w: "*"})
		}
	}
	if i := c.interpreter(); i >= 0 {
		script := c.script
		for j := i + 1; j+1 < len(c.words) && script == ""; j++ {
			if c.words[j] == "-c" || c.words[j] == "-e" {
				script = c.words[j+1]
			}
		}
		kind := "script"
		ps, sure := traceScriptWrites(script)
		if !sure {
			kind = "script?"
		}
		each(kind, ps)
	}
	return out
}

// traceWordPaths: the files a command line names, in order.
func traceWordPaths(cmds []traceCommand, cwd string) (out []tracePath) {
	for _, c := range cmds {
		for _, w := range c.words {
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
			if f, _ := traceWord(w, c.dir, cwd); f.P != "" && traceFileShape(f.P) && !hasPath(out, f) {
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
		} else if name == "Write" {
			mem.keep(traceAt(in.FilePath, cwd), in.Content)
		}
	case "Agent", "Task":
		tc.Class, tc.Scan = trDelegated, true
	case "Bash":
		tc.Scan = true
		var cmds []traceCommand
		home, _ := os.UserHomeDir()
		traceCommands(in.Command, cwd, 0, map[string]string{"HOME": home}, mem, &cmds)
		for _, c := range cmds {
			switch c.head {
			case "wp next", "wp take":
				if !traceHasFlag(c.words, "-h", "--help", "--peek") {
					tc.Start = true
				}
			case "wp ship", "wp done", "wp release":
				if !traceHasFlag(c.words, "-h", "--help") {
					tc.End, tc.Keys = strings.TrimPrefix(c.head, "wp "), traceKeys(c.words)
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
		for _, w := range cmds[i].wrote {
			wrote[i] = wrote[i] || w.kind == "redirect" || w.kind == "output" || w.kind == "sed"
			if w.kind == "output" {
				continue
			}
			f, work := traceWord(w.w, cmds[i].dir, cwd)
			edit = edit || work
			whole = whole && (!work || w.whole)
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
		h := c.head
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
		grep = grep || traceGrepH[c.head]
		locate = locate || traceLocateH[c.head]
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

// sedInPlaceRE: -i alone, with a suffix, or after sed's other one-letter
// flags (-ri, -Ei, -ni).
var sedInPlaceRE = lazyre.New(`^-[nrEsuz]*i`)

// traceGitArgs is git's arguments from its subcommand on: after git's own
// flags (-C dir, -c k=v, --no-pager).
func traceGitArgs(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if traceGitValueFlags[args[0]] && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	return args
}

// traceGitWritesText: git show and git cat-file print a file's text, which a
// redirect puts back into the work; the rest of git prints reports.
func traceGitWritesText(args []string) bool {
	args = traceGitArgs(args)
	return len(args) > 0 && (args[0] == "show" || args[0] == "cat-file")
}

func sedInPlace(words []string) bool {
	for _, w := range words[1:] {
		if w == "--in-place" || strings.HasPrefix(w, "--in-place=") || !strings.HasPrefix(w, "--") && sedInPlaceRE.MatchString(w) {
			return true
		}
	}
	return false
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

var perlInPlaceRE = lazyre.New(`^-[a-zA-DF-HJ-Za-df-hj-z0-9]*i`)

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

// traceIntoDir says whether the last operand of mv or cp is a directory the
// others go into. The command says so with a trailing slash or with several
// sources; else the filesystem does, where the path is still there; else a
// name with no dot is taken for a directory (mv a.go pkg is far commoner
// than renaming a file to a name with no extension), unless it is a file
// name known to have none.
func traceIntoDir(to, dir string, sources int) bool {
	if strings.HasSuffix(to, "/") || sources > 1 {
		return true
	}
	p := to
	if !strings.HasPrefix(p, "/") && strings.HasPrefix(dir, "/") {
		p = dir + "/" + p
	}
	if strings.HasPrefix(p, "/") && !strings.Contains(p, "$") {
		if fi, err := os.Stat(p); err == nil {
			return fi.IsDir()
		}
	}
	base := path.Base(to)
	return !strings.ContainsAny(base, ".$") && !traceBareNames[base]
}

// traceBareNames are file names with no extension.
var traceBareNames = map[string]bool{"Makefile": true, "Dockerfile": true, "LICENSE": true, "README": true, "CODEOWNERS": true, "Jenkinsfile": true, "Procfile": true, "Gemfile": true, "Caddyfile": true}

// traceBackupRE is a copy kept beside a file before changing it: no edit of
// the work.
var traceBackupRE = lazyre.New(`(\.(bak|orig|old|backup|save|tmp)|~)$`)

// traceReporters are programs whose output sent to a file is mostly a
// report kept for reading (a test run, a search, a fetched page), not text
// the agent wrote into the work; it is one when the file is named as a
// report is (traceReportRE: out.txt, test.log, a name with no ending). A
// generator's output into a file of the work (go run ./cmd/gen > docs/CLI.md)
// stays an edit.
var traceReportRE = lazyre.New(`(?i)^[^.]+$|\.(txt|log|out|err|json|jsonl|csv|tsv|diff|patch|tmp|html|xml)$`)

var traceReporters = map[string]bool{"go": true, "wp": true, "gh": true, "npm": true, "npx": true, "pnpm": true, "yarn": true, "make": true,
	"pytest": true, "cargo": true, "curl": true, "wget": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "find": true, "fd": true,
	"ls": true, "tree": true, "diff": true, "docker": true, "kubectl": true, "tsc": true, "golangci-lint": true, "time": true, "wc": true, "du": true,
	"sqlite3": true, "psql": true, "journalctl": true, "ps": true, "env": true, "llm-task": true}

// traceGitValueFlags are git's own flags that take the next word.
var traceGitValueFlags = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--exec-path": true}

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
