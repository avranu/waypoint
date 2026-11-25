package transcript

// The shell-write recogniser (WP-1883): which files a Bash command line
// writes, as the line spells them. The client's trace and `wp usage where`
// and the server's session facts all read commands with it, so "a shell edit"
// is one thing everywhere. It moved here from cmd/wp/usage_trace.go.

import (
	"os"
	"path"
	"regexp"
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
	ps, sure := traceScriptWrites(body)
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

// tracePath names a file of the work: its path from the root of its checkout

// ShellCommand is one command of a Bash call.
type ShellCommand struct {
	Words  []string
	Head   string
	Dir    string
	Redirs []string
	Script string
	Wrote  []ShellWrite
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
func shellCommands(cmd, cwd string, depth int, vars map[string]string, mem *ShellScripts, out *[]ShellCommand, disk bool) string {
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
			if name == "PWD" && dir != ShellNowhere && dir != "" {
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
				shellCommands(inner, dir, depth+1, vars, mem, out, disk)
				continue
			}
			if g.heredoc != "" {
				shellCommands(g.heredoc, dir, depth+1, vars, mem, out, disk)
				continue
			}
		}
		c := ShellCommand{Words: words, Head: headOf(words), Dir: dir, Script: g.heredoc}
		for _, r := range g.redirs {
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
			for _, w := range c.Wrote {
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
		return ShellNowhere
	case strings.HasPrefix(to, "/"):
		return path.Clean(to)
	case dir == "" || dir == ShellNowhere:
		return ShellNowhere
	}
	return path.Join(dir, to)
}

// traceWord places a word of a command as a file, like traceFile. A word the
// line spells out only the start of (internal/*.go, clusters/$site/app.yaml)
// is placed by that start and not named. One it doesn't spell out the start
// of ($f, a substitution), or a relative one after a cd it doesn't spell

// interpreter finds the interpreter a command runs (python3, uv run python,
// node), as its place among the words, or -1.
func (c *ShellCommand) interpreter() int {
	for i := 0; i < len(c.Words) && i < 4; i++ {
		if b := path.Base(c.Words[i]); traceInterpreters[b] || strings.HasPrefix(b, "python3.") {
			return i
		}
	}
	return -1
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
		if !ShellFileShape(s) {
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

// ShellWrite is one file a command writes, as the command spells it.
type ShellWrite struct {
	Kind  string
	Path  string
	Whole bool
}

// writes lists the files a command writes. What counts: output sent to a
// file, tee, sed -i and perl -i, gofmt, goimports and prettier given -w or
// --write, curl -o, mv and cp, git mv and git rm, a patch
// applied, and a script given to an interpreter that writes files. What
// doesn't: what a program writes of its own accord (a formatter, go mod
// tidy, git merge or checkout), which the call doesn't show; and a plain rm,
// which is as often clearing up. A script the session kept in a file and
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
		if ws := plain(0); len(ws) > 0 && path.Base(ws[0]) == "prettier" {
			for i, a := range args {
				if a == ws[0] {
					prog, args = "prettier", args[i+1:]
					break
				}
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
		if has("-w", "--write") {
			each("format", plain(0))
		}
	case "curl":
		for i := 0; i < len(args); i++ {
			if (args[i] == "-o" || args[i] == "--output") && i+1 < len(args) {
				out = append(out, ShellWrite{Kind: "curl", Path: args[i+1], Whole: true})
				i++
			} else if f, ok := strings.CutPrefix(args[i], "--output="); ok {
				out = append(out, ShellWrite{Kind: "curl", Path: f, Whole: true})
			}
		}
	case "xargs", "find":
		for i, a := range args {
			if b := path.Base(a); b == "sed" && sedInPlace(args[i:]) || b == "perl" && i+1 < len(args) && perlInPlaceRE.MatchString(args[i+1]) {
				out = append(out, ShellWrite{Kind: "patch", Path: "*"})
				break
			}
		}
	case "patch", "git apply", "git am":
		if !has("--check", "--stat", "--numstat", "--summary", "--dry-run", "-R", "--reverse") {
			out = append(out, ShellWrite{Kind: "patch", Path: "*"})
		}
	}
	if i := c.interpreter(); i >= 0 {
		script := c.Script
		for j := i + 1; j+1 < len(c.Words) && script == ""; j++ {
			if c.Words[j] == "-c" || c.Words[j] == "-e" {
				script = c.Words[j+1]
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

var perlInPlaceRE = lazyre.New(`^-[a-zA-DF-HJ-Za-df-hj-z0-9]*i`)

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
	var out []ShellCommand
	dir := shellCommands(cmd, cwd, 0, vars, mem, &out, disk)
	return out, dir
}
