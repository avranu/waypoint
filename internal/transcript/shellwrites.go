package transcript

// The shell-write recogniser (WP-1883): which files a Bash command line
// writes, as the line spells them. The client's trace and `wp usage where`
// and the server's session facts all read commands with it, so the parser and
// the list of what writes (sed -i, tee, redirects, formatters, scripts) are
// shared. What differs is where a written path counts as "the work": the
// client asks the disk (the session's checkouts, whether a target is a
// directory); the server cannot, and counts a path under a directory the
// session worked in, by the working directories its records show
// (ShellSession.Edit). So the two can disagree on a path in neither a
// checkout nor a seen working directory. It moved here from
// cmd/wp/usage_trace.go.

import (
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var traceExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields("go ts tsx js jsx mjs cjs py sql md sh yml yaml json css scss html rs kt kts java toml mk tf proto vue svelte rb php c h cpp txt xml gradle ini cfg conf service tmpl") {
		traceExts[e] = true
	}
}

// ShellScripts remembers the scripts a session kept in files, by where it
// put them, with what each writes: an agent that writes edit.py to a scratch
// directory and runs it has edited what edit.py writes, at the call that
// runs it. Only the paths a script writes are kept, never its text.
type ShellScripts []ShellScript

type ShellScript struct {
	At     string   `json:"a"`
	Writes []string `json:"w"`
	Unsure bool     `json:"u,omitempty"` // no written path could be followed: every file it names
}

const (
	ShellMaxScripts     = 24
	traceMaxScriptPaths = 24

	// Work the recogniser does for one command line is bounded, because the
	// server reads every Bash call of every uploaded transcript: a word or
	// variable value longer than shellMaxValue is left unexpanded (an
	// unresolved variable, as one the line never set), a line holds at most
	// shellMaxVars variables and shellMaxCommands commands, and a line over
	// shellMaxCommand bytes is not read at all. Variables filled in across the
	// whole line come to at most shellMaxExpand bytes (words past that stay
	// unexpanded), a directory longer than shellMaxValue is nowhere, and a cp
	// or mv of more than shellMaxSources files into a directory is one write
	// of any file there, so no literal word is copied once per word either.
	// The caps are sized to bound cost, not from a measurement of real calls
	// (WP-1902 asks for one).
	shellMaxValue    = 4 << 10
	shellMaxVars     = 256
	shellMaxCommands = 5000
	shellMaxCommand  = 256 << 10
	shellMaxExpand   = 1 << 20
	shellMaxSources  = 64
)

var traceScriptFileRE = lazyre.New(`\.(?:py|js|mjs|cjs|ts|rb)$`)

// Keep notes the script now at a path, replacing what was there; a script
// that writes nothing is only forgotten.
func (m *ShellScripts) Keep(at, body string) {
	if m == nil || !traceScriptFileRE.MatchString(at) {
		return
	}
	for i := range *m {
		if (*m)[i].At == at {
			*m = append((*m)[:i:i], (*m)[i+1:]...)
			break
		}
	}
	ps, sure := traceScriptWrites(body, false)
	ps = slices.DeleteFunc(ps, func(p string) bool { return len(p) > shellMaxValue })
	if len(ps) == 0 {
		return
	}
	if len(ps) > traceMaxScriptPaths {
		ps = ps[:traceMaxScriptPaths]
	}
	if len(*m) >= ShellMaxScripts {
		*m = append((*m)[:0:0], (*m)[1:]...)
	}
	*m = append(*m, ShellScript{At: at, Writes: ps, Unsure: !sure})
}

func (m *ShellScripts) at(at string) *ShellScript {
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

// ShellAt names where a word of a command points, to match a script kept
// there with the command that runs it: the path when the line says it, else
// the word as written ($S/edit.py is the same file both times).
func ShellAt(w, dir string) string {
	switch {
	case strings.HasPrefix(w, "/"):
		return path.Clean(w)
	case w == "" || w[0] == '$' || dir == "" || dir == ShellNowhere:
		return w
	}
	return path.Join(dir, w)
}

// ShellFileShape says a word names a file: a name with a known extension, or
// Makefile or Dockerfile, made of path characters only.
func ShellFileShape(w string) bool {
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

// ShellCommand is one command of a Bash call.
type ShellCommand struct {
	Words  []string
	Head   string
	Dir    string
	Redirs []string
	Script string
	Wrote  []ShellWrite
	// PipeIn: the command reads the one before it through a pipe.
	PipeIn bool
	// Unread: the command runs a script longer than the part that is read
	// (traceMaxScript), and that part writes nothing: what the rest writes is
	// not known.
	Unread bool
}

// ShellNowhere is the directory after a cd the line doesn't spell out
// (cd "$d", cd -): a relative path under it can't be placed.
const ShellNowhere = "\x00"

var traceVarRE = lazyre.New(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// shellCommands lists the commands of a line in order, going into bash -c
// and a heredoc given to a shell, on the guard's shell reader. It follows
// what the line itself says about where each command runs and what its
// variables hold: `cd web && cat src/x.ts` reads web/src/x.ts, and
// `S=/tmp/s; cat > $S/x.py` writes a scratch file. It returns the directory
// the line ends in.
func shellCommands(cmd, cwd string, depth int, vars map[string]string, mem *ShellScripts, out *[]ShellCommand, disk bool, spent *int) string {
	dir := cwd
	fill := func(w string) string {
		orig := w
		if w == "~" || strings.HasPrefix(w, "~/") {
			w = "$HOME" + w[1:]
		}
		if !strings.Contains(w, "$") {
			return w
		}
		if len(w) > shellMaxValue || *spent > shellMaxExpand {
			return orig
		}
		var b strings.Builder
		last := 0
		for _, m := range traceVarRE.FindAllStringSubmatchIndex(w, -1) {
			b.WriteString(w[last:m[0]])
			last = m[1]
			name := w[m[2]:m[3]]
			switch v, ok := vars[name]; {
			case name == "PWD" && dir != ShellNowhere && dir != "":
				b.WriteString(dir)
			case ok:
				if b.Len()+len(v) > shellMaxValue {
					return orig
				}
				b.WriteString(v)
			default:
				b.WriteString(w[m[0]:m[1]])
			}
			if b.Len() > shellMaxValue {
				return orig
			}
		}
		b.WriteString(w[last:])
		if b.Len() > shellMaxValue {
			return orig
		}
		*spent += b.Len()
		return b.String()
	}
	set := func(words []string) {
		for _, w := range words {
			if name, v, ok := strings.Cut(w, "="); ok && AssignRE.MatchString(w) && !strings.HasSuffix(name, "+") {
				if _, have := vars[name]; len(v) > shellMaxValue || !have && len(vars) >= shellMaxVars {
					delete(vars, name)
					continue
				}
				vars[name] = v
			}
		}
	}
	for _, g := range ParseShell(cmd) {
		if len(*out) >= shellMaxCommands {
			break
		}
		all := make([]string, len(g.Words))
		assigns := len(g.Words) > 0
		for i, w := range g.Words {
			all[i] = fill(w)
			assigns = assigns && AssignRE.MatchString(w)
		}
		if assigns {
			set(all)
			continue
		}
		if len(all) >= 4 && all[0] == "for" && all[2] == "in" && traceIdentRE.MatchString(all[1]) {
			// for f in LIST; do ... "$f" ...: f stands for the one word of a
			// list of one (a name or a glob, *.go), else for any file.
			list := "*"
			if w := all[3]; len(all) == 4 && !strings.HasPrefix(w, "$") && !strings.Contains(w, ShellSubPlaceholder) {
				list = w
			}
			if len(list) > shellMaxValue {
				list = "*"
			}
			if len(vars) < shellMaxVars {
				vars[all[1]] = list
			}
		}
		words := StripWords(all)
		if len(words) == 0 {
			continue
		}
		if depth < 3 && Shells[path.Base(words[0])] {
			if inner, ok := ShellDashC(words[1:]); ok {
				shellCommands(inner, dir, depth+1, vars, mem, out, disk, spent)
				continue
			}
			if g.Heredoc != "" {
				shellCommands(g.Heredoc, dir, depth+1, vars, mem, out, disk, spent)
				continue
			}
		}
		c := ShellCommand{Words: words, Head: HeadOf(words), Dir: dir, Script: g.Heredoc, PipeIn: g.PipeIn}
		for _, r := range g.Redirs {
			c.Redirs = append(c.Redirs, fill(r))
		}
		switch {
		case c.Head == "cd" || c.Head == "pushd":
			dir = traceCd(dir, words[1:])
		case c.Head == "popd":
			dir = ShellNowhere
		case c.Head == "export":
			set(words[1:])
		case words[0] == "git":
			for i := 1; i+1 < len(words) && strings.HasPrefix(words[i], "-"); i++ {
				if words[i] == "-C" {
					c.Dir = traceCd(dir, words[i+1:i+2])
				}
				if traceGitValueFlags[words[i]] {
					i++
				}
			}
		}
		c.Wrote = c.writes(disk)
		for i, w := range c.Wrote {
			if strings.HasPrefix(w.Kind, "script") {
				c.Wrote[i].Path = fill(w.Path)
			}
		}
		if prog := path.Base(words[0]); c.Script != "" && (prog == "cat" || prog == "tee") {
			// The first few files only: each one kept reads the script again.
			for _, w := range c.Wrote[:min(len(c.Wrote), traceMaxScriptPaths)] {
				mem.Keep(ShellAt(w.Path, c.Dir), c.Script)
			}
		} else if k := mem.at(ShellAt(c.ran(), c.Dir)); k != nil {
			kind := "script"
			if k.Unsure {
				kind = "script?"
			}
			for _, w := range k.Writes {
				c.Wrote = append(c.Wrote, ShellWrite{Kind: kind, Path: w})
			}
		}
		if last := words[len(words)-1]; len(last) <= shellMaxValue {
			vars["_"] = last
		} else {
			delete(vars, "_")
		}
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
	case to == "" || to == "-" || strings.ContainsAny(to, "$`*?") || len(dir)+len(to) > shellMaxValue:
		return ShellNowhere // not spelled out, or longer than any real path
	case strings.HasPrefix(to, "/"):
		return path.Clean(to)
	case dir == "" || dir == ShellNowhere:
		return ShellNowhere
	}
	return path.Join(dir, to)
}

// interpreter finds the interpreter a command runs (python3, uv run python,
// node), as its place among the words, or -1.
func (c *ShellCommand) interpreter() int { return interpreterAt(c.Words) }

func interpreterAt(words []string) int {
	for i := 0; i < len(words) && i < 4; i++ {
		if b := path.Base(words[i]); traceInterpreters[b] || strings.HasPrefix(b, "python3.") || strings.HasPrefix(b, "python2") {
			return i
		}
	}
	return -1
}

// InlineScript is the script a command hands an interpreter on the line
// itself: the heredoc on its input, else the argument of -c or -e. "" when
// the command runs no interpreter or gives it a file. The guard and the
// write recogniser both find it here, so they agree on what runs a script.
func InlineScript(words []string, heredoc string) string {
	i := interpreterAt(words)
	if i < 0 || heredoc != "" {
		if i < 0 {
			return ""
		}
		return heredoc
	}
	for j := i + 1; j+1 < len(words); j++ {
		if words[j] == "-c" || words[j] == "-e" {
			return words[j+1]
		}
	}
	return ""
}

// ran is the script file a command hands its interpreter (python3 edit.py),
// "" when it runs none.
func (c *ShellCommand) ran() string {
	i := c.interpreter()
	if i < 0 || c.Script != "" {
		return ""
	}
	for _, w := range c.Words[i+1:] {
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

// traceMaxScript bounds the text of a script that is read, and
// traceScriptBudget the names followed (each is a search of the text), so a
// script cannot cost more than a fixed amount of work.
const (
	traceMaxScript    = 64 << 10
	traceScriptBudget = 100
	traceScriptScan   = 2 << 20
	traceMaxStrings   = 200
)

// traceScriptWrites lists the paths a script writes, as the script spells
// them. A path given as a name is followed to what the script sets the name
// to (p = 'a.go'; for p in ['a.go', 'b.go']). sure is false when the script
// writes but no path could be followed: then every file the script names is
// listed, other than one it is seen to read, and the caller can't tell the
// written from the read. whole reads the script to its end and every write
// in it (the guard, which must not miss one); otherwise the first
// traceMaxScript bytes and a bounded number of writes are read.
func traceScriptWrites(script string, whole bool) (paths []string, sure bool) {
	sites, strs := 64, traceMaxStrings
	if whole {
		sites, strs = -1, -1
	} else if len(script) > traceMaxScript {
		script = script[:traceMaxScript]
	}
	if !traceScriptWriteRE.MatchString(script) {
		return nil, true
	}
	// Each name followed searches the text again: spend a fixed amount of
	// scanning, so a long script is followed fewer times than a short one.
	budget := min(traceScriptBudget, max(3, traceScriptScan/(3*len(script)+1)))
	for _, re := range traceScriptSiteREs {
		for _, m := range re.FindAllStringSubmatch(script, sites) {
			paths = append(paths, traceScriptExpr(script, m[1], 0, &budget)...)
		}
	}
	if len(paths) > 0 {
		return paths, true
	}
	// Each string is judged where it stands (no second search of the text),
	// and one the script reads anywhere is left out.
	var names []string
	reads := map[string]bool{}
	for _, m := range traceScriptStrRE.FindAllStringSubmatchIndex(script, strs) {
		at := m[2]
		if at < 0 {
			at = m[4]
		}
		str := script[at : m[1]-1]
		if !ShellFileShape(str) {
			continue
		}
		if scriptScratchBeforeRE.MatchString(script[max(0, m[0]-64):m[0]]) {
			continue // a name under the temporary directory, not a path of its own
		}
		if scriptReadsAt(script, m[0], m[1]) {
			reads[str] = true
		}
		names = append(names, str)
	}
	for _, n := range names {
		if !reads[n] {
			paths = append(paths, n)
		}
	}
	return paths, false
}

var (
	scriptOpenBeforeRE    = lazyre.New(`\bopen\(\s*$`)
	scriptReadBeforeRE    = lazyre.New(`(?:\breadFile(?:Sync)?|\bFile\.(?:read|readlines|foreach)|\b(?:exists|isfile|existsSync))\(\s*$`)
	scriptScratchBeforeRE = lazyre.New(`\bjoin\(\s*(?:tempfile\.\w+\(\)|os\.tmpdir\(\)|['"]/tmp[^'"\n]*['"])\s*,\s*$`)
	scriptReadAfterRE     = lazyre.New(`^\s*\)\.read_(?:text|bytes)\(`)
)

// scriptReadsAt says the string at script[from:to] is one the script reads:
// open(S) with no mode or a read mode, S).read_text(, readFileSync(S,
// File.read(S, exists(S).
func scriptReadsAt(script string, from, to int) bool {
	before := script[max(0, from-64):from]
	after := script[to:min(len(script), to+200)]
	if i := strings.IndexByte(after, '\n'); i >= 0 {
		after = after[:i]
	}
	switch {
	case scriptReadAfterRE.MatchString(after), scriptReadBeforeRE.MatchString(before):
		return true
	case scriptOpenBeforeRE.MatchString(before):
		end := strings.IndexByte(after, ')')
		if end < 0 {
			return false
		}
		return scriptOpenReads(after[:end])
	}
	return false
}

// scriptOpenReads reads the arguments after open's path: it only reads when
// its mode (the second argument, or mode=) is absent or has none of w, a, x
// and +.
func scriptOpenReads(rest string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true
	}
	if rest[0] != ',' {
		return false // the path is an expression that goes on (S + x, S % y)
	}
	for i, part := range strings.Split(rest[1:], ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "mode"); ok && strings.HasPrefix(strings.TrimSpace(v), "=") {
			part = strings.TrimSpace(strings.TrimSpace(v)[1:])
		} else if i > 0 || strings.Contains(part, "=") {
			continue
		}
		if len(part) < 2 || part[0] != '\'' && part[0] != '"' {
			return false // a mode the script computes
		}
		if strings.ContainsAny(part, "wax+") {
			return false
		}
	}
	return true
}

// traceScriptExpr follows a path expression of a script to the strings it
// can be: a string itself, Path(of one), a name the script sets to one, or
// the first parameter of a function the script defines, by what its calls
// pass (def sub(p, old, new): …; sub('a.go', …)).
func traceScriptExpr(script, expr string, depth int, budget *int) []string {
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
	if depth >= 3 || !traceIdentRE.MatchString(expr) || *budget <= 0 {
		return nil
	}
	*budget--
	var out []string
	set := regexp.MustCompile(`(?m)(?:^|[;\s])(?:const |let |var )?` + expr + `\s*=\s*([^=\n;][^\n;]*)`)
	for _, m := range set.FindAllStringSubmatch(script, 16) {
		out = append(out, traceScriptExpr(script, m[1], depth+1, budget)...)
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
				out = append(out, traceScriptExpr(script, c[1], depth+1, budget)...)
			}
		}
	}
	return out
}

// ShellWrite is one file a command writes, as the command spells it.
type ShellWrite struct {
	Kind  string
	Path  string
	Whole bool
	// Root, for a write whose files the command does not name (Path "*"), is
	// the directory it applies to when that is not the command's own: the
	// root a find starts from.
	Root string
}

// writes lists the files a command writes. What counts: output sent to a
// file, tee, sed -i and perl -i, gofmt, goimports and prettier given -w or
// --write, curl -o, dd of=, mv and cp, git mv and git rm, a patch
// applied, and a script given to an interpreter that writes files. What
// doesn't: what a program writes of its own accord (a code generator, go mod
// tidy, git merge or checkout), which the call doesn't show; a plain rm,
// which is as often clearing up; and touch or mkdir, which write no text. A script the session kept in a file and
// runs later is added by shellCommands, which remembers them.
//
// Output sent to a file is a write of what the agent typed (cat, echo,
// printf) wherever it goes; of another program's output only when the file
// is named like a source, document or configuration file (docs/CLI.md, not
// server.log).
func (c *ShellCommand) writes(disk bool) []ShellWrite {
	var out []ShellWrite
	prog, args := path.Base(c.Words[0]), c.Words[1:]
	for _, r := range c.Redirs {
		switch {
		case strings.HasPrefix(r, "/dev/"):
		case (traceReporters[prog] || prog == "git" && !traceGitWritesText(args)) && traceReportRE.MatchString(path.Base(r)):
			out = append(out, ShellWrite{Kind: "output", Path: r})
		case prog == "cat" || prog == "echo" || prog == "printf" || ShellFileShape(path.Base(r)):
			out = append(out, ShellWrite{Kind: "redirect", Path: r, Whole: prog == "cat" && c.Script != ""})
		default:
			out = append(out, ShellWrite{Kind: "output", Path: r})
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
			out = append(out, ShellWrite{Kind: kind, Path: w})
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
	if prog == "npx" || prog == "bunx" {
		// npx [--prefix DIR] prettier ...: the tool is the first word named
		// prettier among the first few, whatever flags and their values
		// come before it.
		for i, a := range args[:min(len(args), 4)] {
			if path.Base(a) == "prettier" {
				prog, args = "prettier", args[i+1:]
				break
			}
		}
	}
	switch prog {
	case "tee":
		for _, w := range plain(0) {
			out = append(out, ShellWrite{Kind: "tee", Path: w, Whole: c.Script != ""})
		}
	case "sed":
		if sedInPlace(c.Words) {
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
		each("perl", PerlInPlaceFiles(args))
	case "mv", "cp", "git mv":
		if ws := plain(0); len(ws) >= 2 {
			to, from := ws[len(ws)-1], ws[:len(ws)-1]
			kind := strings.TrimPrefix(prog, "git ")
			switch {
			case len(from) > shellMaxSources && traceIntoDir(to, c.Dir, len(from), disk):
				out = append(out, ShellWrite{Kind: kind, Path: strings.TrimSuffix(to, "/") + "/*", Whole: true})
			case traceIntoDir(to, c.Dir, len(from), disk):
				for _, s := range from {
					out = append(out, ShellWrite{Kind: kind, Path: strings.TrimSuffix(to, "/") + "/" + path.Base(s), Whole: true})
				}
			case prog == "cp" && traceBackupRE.MatchString(to):
				out = append(out, ShellWrite{Kind: "output", Path: to})
			default:
				out = append(out, ShellWrite{Kind: kind, Path: to, Whole: true})
			}
			if prog != "cp" {
				each("mv", from)
			}
		}
	case "git rm":
		each("rm", plain(0))
	case "gofmt", "goimports", "prettier":
		each("format", FormatterFiles(args))
	case "curl":
		for i := 0; i < len(args); i++ {
			if (args[i] == "-o" || args[i] == "--output") && i+1 < len(args) {
				out = append(out, ShellWrite{Kind: "curl", Path: args[i+1], Whole: true})
				i++
			} else if f, ok := strings.CutPrefix(args[i], "--output="); ok {
				out = append(out, ShellWrite{Kind: "curl", Path: f, Whole: true})
			}
		}
	case "dd":
		for _, a := range args {
			if f, ok := strings.CutPrefix(a, "of="); ok {
				out = append(out, ShellWrite{Kind: "dd", Path: f, Whole: true})
			}
		}
	case "xargs", "find":
		root := ""
		if prog == "find" {
			if r := plain(0); len(r) > 0 && !strings.ContainsAny(r[0], "()!") {
				root = r[0]
			}
		}
		for i, a := range args {
			b := path.Base(a)
			formats := (b == "gofmt" || b == "goimports" || b == "prettier") && slices.ContainsFunc(args[i+1:], func(x string) bool { return x == "-w" || x == "--write" })
			if formats || b == "sed" && sedInPlace(args[i:]) || b == "perl" && perlInPlace(args[i+1:]) {
				out = append(out, ShellWrite{Kind: "patch", Path: "*", Root: root})
				break
			}
		}
	case "patch", "git apply", "git am":
		// A run that only checks or lists writes nothing, unless git is also
		// told to apply.
		if !has("--check", "--stat", "--numstat", "--summary", "--dry-run", "-R", "--reverse") || prog != "patch" && has("--apply") {
			root, to := "", "" // patch -d DIR: the files are under DIR; patch -o FILE: the one file written
			for i, a := range args {
				next := ""
				if i+1 < len(args) {
					next = args[i+1]
				}
				switch {
				case strings.HasPrefix(a, "--directory="):
					root = a[len("--directory="):]
				case a == "--directory":
					root = next
				case prog != "patch":
				case strings.HasPrefix(a, "--output="):
					to = a[len("--output="):]
				case a == "--output" || a == "-o":
					to = next
				case a == "-d":
					root = next
				case strings.HasPrefix(a, "--"):
				case strings.HasPrefix(a, "-d"):
					root = a[2:]
				case strings.HasPrefix(a, "-o"):
					to = a[2:]
				}
			}
			if to != "" {
				out = append(out, ShellWrite{Kind: "patch", Path: to})
			} else {
				out = append(out, ShellWrite{Kind: "patch", Path: "*", Root: root})
			}
		}
	}
	if i := c.interpreter(); i >= 0 {
		script := InlineScript(c.Words, c.Script)
		kind := "script"
		ps, sure := traceScriptWrites(script, false)
		if !sure {
			kind = "script?"
		}
		each(kind, ps)
		c.Unread = len(script) > traceMaxScript && !traceScriptWriteRE.MatchString(script[:traceMaxScript])
	}
	return out
}

// PerlInPlaceFiles are the files `perl -i`, `-pi -e CODE` or `-i.bak` edits,
// read as perl reads its command line: switches come first and end at the
// first word that is not one (or at --); with no -e the next word is the
// program, not a file; the words after are the files, and they are edited
// only when a switch asked for in-place editing. So the options of a script
// (perl check.pl --config x) are no switches of perl's, and in -pie the e is
// -i's backup suffix and the word after is the program. The guard reads the
// same rule, so the two cannot drift apart.
func PerlInPlaceFiles(args []string) []string {
	inPlace, code := false, false
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		in, has, next := perlSwitch(a)
		inPlace, code = inPlace || in, code || has
		if next {
			i++
		}
	}
	if !code {
		i++ // the program file
	}
	if !inPlace || i >= len(args) {
		return nil
	}
	return args[i:]
}

// perlInPlace says the switches that begin args (perl's own, before its
// script or files) ask for in-place editing.
func perlInPlace(args []string) bool {
	for i := 0; i < len(args) && len(args[i]) >= 2 && args[i][0] == '-' && args[i] != "--"; i++ {
		in, _, next := perlSwitch(args[i])
		if in {
			return true
		}
		if next {
			i++
		}
	}
	return false
}

// perlSwitch reads one cluster of perl switches (-pi, -i.bak, -Mstrict,
// -0777pe). inPlace: it holds -i. code: it holds -e or -E. next: that -e
// ends the cluster, so its code is the next argument. A switch that takes
// the rest of the cluster as its value ends the reading: -i (a backup
// suffix), -M and -m (a module), -I (a directory), -x, -F, -C, -D, -V and
// -e's own code, so the i of -Mstrict or -Ilib is no -i and the e of -pie no
// -e.
func perlSwitch(a string) (inPlace, code, next bool) {
	for j := 1; j < len(a); j++ {
		switch a[j] {
		case 'i':
			return true, false, false
		case 'e', 'E':
			return false, true, j == len(a)-1
		case 'M', 'm', 'I', 'x', 'F', 'C', 'D', 'V':
			return false, false, false
		}
	}
	return false, false, false
}

// formatterValueFlags are the flags of gofmt, goimports and prettier whose
// next argument is a value, not a file.
var formatterValueFlags = func() map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(`-r -cpuprofile -local -srcdir -memprofile -memrate -trace
		--config --config-precedence --ignore-path --plugin --cache-location --cache-strategy --log-level
		--stdin-filepath --parser --print-width --tab-width --end-of-line --trailing-comma --arrow-parens
		--quote-props --prose-wrap --object-wrap --html-whitespace-sensitivity --embedded-language-formatting
		--experimental-operator-position --cursor-offset --range-start --range-end`) {
		m[f] = true
	}
	return m
}()

// FormatterFiles are the files gofmt, goimports or prettier rewrite: its file
// arguments, when -w or --write is given (without it they only print or
// list). The guard reads the same rule.
func FormatterFiles(args []string) []string {
	write := false
	var files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			files = append(files, args[i+1:]...)
			i = len(args)
		case a == "-w" || a == "--write":
			write = true
		case formatterValueFlags[a]:
			i++
		case strings.HasPrefix(a, "-") && a != "-":
		default:
			files = append(files, a)
		}
	}
	if !write {
		return nil
	}
	return files
}

// ScriptWrites lists the paths a script given to an interpreter (python,
// node, ruby) writes, as the script spells them: see traceScriptWrites. It
// reads the script whole, however long: this is the guard's reading, and a
// write past a cut would be one it let through.
func ScriptWrites(script string) []string {
	ps, _ := traceScriptWrites(script, true)
	return ps
}

// GitValueFlags are git's own flags (before its subcommand) that take the
// next word.
func GitValueFlags(flag string) bool { return traceGitValueFlags[flag] }

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

// traceIntoDir says whether the last operand of mv or cp is a directory the
// others go into. The command says so with a trailing slash or with several
// sources; else the filesystem does, where the path is still there; else a
// name with no dot is taken for a directory (mv a.go pkg is far commoner
// than renaming a file to a name with no extension), unless it is a file
// name known to have none.
func traceIntoDir(to, dir string, sources int, disk bool) bool {
	if strings.HasSuffix(to, "/") || sources > 1 {
		return true
	}
	p := to
	if !strings.HasPrefix(p, "/") && strings.HasPrefix(dir, "/") {
		p = dir + "/" + p
	}
	if disk && strings.HasPrefix(p, "/") && !strings.Contains(p, "$") {
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

// ReadShell lists the commands of a line in order (see shellCommands), with
// where each runs and the files it writes. mem remembers scripts the session
// kept in files (nil: none). disk lets it ask the filesystem whether the
// target of mv or cp is a directory; the server has no such disk to ask.
// It returns the directory the line ends in.
func ReadShell(cmd, cwd string, vars map[string]string, mem *ShellScripts, disk bool) ([]ShellCommand, string) {
	if len(cmd) > shellMaxCommand {
		return nil, cwd // too long to read: every reader gives the same answer
	}
	var out []ShellCommand
	spent := 0
	dir := shellCommands(cmd, cwd, 0, vars, mem, &out, disk, &spent)
	return out, dir
}
