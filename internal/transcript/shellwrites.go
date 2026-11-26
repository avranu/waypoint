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
	for _, e := range strings.Fields("go ts tsx js jsx mjs cjs py sql md sh yml yaml json css scss html rs kt kts java toml mk tf proto vue svelte rb php c h cpp txt xml gradle ini cfg conf service tmpl graphql gql swift cs dart lua nix hcl tfvars scala ex exs erl hs ml zig m mm r pl pm clj groovy prisma rst adoc tex less sass cc hpp csv tsv") {
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
	// Unfollowed: it writes, but to paths it builds and nothing names, or to
	// more paths than are kept: what it writes is not all known.
	Unfollowed bool `json:"n,omitempty"`
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
	ps, sure, unf := traceScriptWrites(body, false)
	ps = slices.DeleteFunc(ps, func(p string) bool { return len(p) > shellMaxValue })
	if len(ps) == 0 && !unf {
		return
	}
	if len(ps) > traceMaxScriptPaths {
		ps, unf = ps[:traceMaxScriptPaths], true // the rest of what it writes is not kept
	}
	if len(*m) >= ShellMaxScripts {
		// The oldest is forgotten to make room, and a mark that one was takes
		// the first place: a script run later that is not here may be it.
		// (Its place and the mark's, or the old mark's, make two.)
		*m = append(ShellScripts{{At: shellForgot}}, (*m)[2:]...)
	}
	*m = append(*m, ShellScript{At: at, Writes: ps, Unsure: !sure, Unfollowed: unf})
}

// shellForgot is the place of the mark that a kept script was forgotten.
const shellForgot = "\x00forgot"

// forgot says a script once kept is no longer remembered.
func (m *ShellScripts) forgot() bool {
	return m != nil && len(*m) > 0 && (*m)[0].At == shellForgot
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
	// In is the script file an interpreter reads on its input (python3 <
	// edit.py), "" when it reads none there.
	In    string
	Wrote []ShellWrite
	// PipeIn: the command reads the one before it through a pipe.
	PipeIn bool
	// Sub: the command stands inside a substitution ($( ), backticks, <( ))
	// of the next command that is not one.
	Sub bool
	// Unread: what the command writes is not all known. It runs a script
	// longer than the part that is read (traceMaxScript), one whose paths
	// could not be followed or one the session no longer remembers; or the
	// line goes on past what is read of it (shellMaxCommands, the depth of
	// nesting), and this is the last command read.
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
	return shellParsed(ParseShell(cmd), cwd, depth, vars, mem, out, disk, spent)
}

// shellParsed is shellCommands for a line already parsed.
func shellParsed(parsed []*ShellCmd, cwd string, depth int, vars map[string]string, mem *ShellScripts, out *[]ShellCommand, disk bool, spent *int) string {
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
	// A cd inside ( ) ends with the group: scopes are the groups the command
	// stands in, each with the directory it began in. { } groups run in the
	// current shell and are not scopes.
	type scope struct {
		g     *ShellGroup
		began string
	}
	var scopes []scope
	for _, g := range parsed {
		if len(*out) >= shellMaxCommands {
			shellMarkUnread(out) // the commands past the cap are not read
			break
		}
		if g.Cut {
			defer shellMarkUnread(out) // the parser stopped at its depth
		}
		var chain []*ShellGroup
		for x := g.Group; x != nil; x = x.Parent {
			chain = append(chain, x)
		}
		slices.Reverse(chain)
		k := 0
		for k < len(scopes) && k < len(chain) && scopes[k].g == chain[k] {
			k++
		}
		if k < len(scopes) {
			dir = scopes[k].began
			scopes = scopes[:k]
		}
		for _, x := range chain[k:] {
			scopes = append(scopes, scope{x, dir})
		}
		// The commands inside $( ), backticks and <( ) run too, before the
		// command they are part of; their cd and variables end with them.
		if depth < 3 {
			for _, sub := range g.Subs {
				inner := map[string]string{}
				for k, v := range vars {
					inner[k] = v
				}
				from := len(*out)
				shellParsed(sub, dir, depth+1, inner, mem, out, disk, spent)
				for i := from; i < len(*out); i++ {
					(*out)[i].Sub = true
				}
			}
		} else if len(g.Subs) > 0 {
			shellMarkUnread(out) // substitutions nested deeper than are read
		}
		all := make([]string, len(g.Words))
		assigns := len(g.Words) > 0
		for i, w := range g.Words {
			all[i] = fill(w)
			assigns = assigns && AssignRE.MatchString(w)
		}
		if assigns {
			// f=$(mktemp), d=$(mktemp -d): a scratch file or directory.
			if len(all) == 1 && len(g.Subs) == 1 && strings.HasSuffix(all[0], "="+ShellSubPlaceholder) {
				if sub := g.Subs[0]; len(sub) > 0 && len(sub[len(sub)-1].Words) > 0 && path.Base(sub[len(sub)-1].Words[0]) == "mktemp" {
					all[0] = strings.TrimSuffix(all[0], ShellSubPlaceholder) + "/tmp/mktemp.x"
				}
			}
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
			// A redirect with no command of its own stands after a group or a
			// loop ({ ...; } > FILE, done > FILE): what the commands before it
			// printed goes to the file, so it is the last one's write.
			if n := len(*out); n > 0 {
				last := &(*out)[n-1]
				for _, r := range g.Redirs {
					r = fill(r)
					if !strings.HasPrefix(r, "/dev/") && shellSourceName(path.Base(r)) {
						last.Wrote = append(last.Wrote, ShellWrite{Kind: "redirect", Path: ShellAt(r, dir)})
					}
				}
			}
			continue
		}
		if Shells[path.Base(words[0])] {
			inner, ok := ShellDashC(words[1:])
			if !ok && g.Heredoc != "" {
				inner, ok = g.Heredoc, true
			}
			switch {
			case ok && depth < 3:
				shellCommands(inner, dir, depth+1, vars, mem, out, disk, spent)
				continue
			case ok:
				defer shellMarkUnread(out) // shells nested deeper than are read
			}
		}
		c := ShellCommand{Words: words, Head: HeadOf(words), Dir: dir, Script: g.Heredoc, PipeIn: g.PipeIn}
		for _, r := range g.Redirs {
			c.Redirs = append(c.Redirs, fill(r))
		}
		if in := StdinScript(words, g.Ins); in != "" {
			c.In = fill(in)
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
		if f := shellFeeder(*out); c.PipeIn && f != nil && path.Base(words[0]) == "tee" {
			// tee keeps what the command before it printed: the same write, or
			// report, as that command's output sent to the file.
			for i, w := range c.Wrote {
				if w.Kind == "tee" && !shellOutputCounts(path.Base(f.Words[0]), f.Words[1:], w.Path) {
					c.Wrote[i].Kind = "output"
				}
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
			c.Unread = c.Unread || k.Unfollowed
		} else if mem.forgot() && c.ran() != "" {
			c.Unread = true // perhaps a script that was kept and then forgotten
		}
		if last := words[len(words)-1]; len(last) <= shellMaxValue {
			vars["_"] = last
		} else {
			delete(vars, "_")
		}
		*out = append(*out, c)
	}
	if len(scopes) > 0 {
		dir = scopes[0].began // the line ended inside a group: its cd ends with it
	}
	return dir
}

// shellMarkUnread records that the line holds more than was read of it, on
// the last command read (one that stands for the rest when there is none).
func shellMarkUnread(out *[]ShellCommand) {
	if n := len(*out); n > 0 {
		(*out)[n-1].Unread = true
		return
	}
	*out = append(*out, ShellCommand{Words: []string{":"}, Head: ":", Unread: true})
}

// shellOutputCounts says a program's output kept in file r (a redirect, or
// tee after a pipe) is a write of the work as far as the name tells: what
// the agent typed (cat, echo, printf) wherever it goes; a reporter's output
// unless the file is named as a report is; any other program's when the file
// is named like a source, document or configuration file.
func shellOutputCounts(prog string, args []string, r string) bool {
	base := path.Base(r)
	switch {
	case (traceReporters[prog] || prog == "git" && !traceGitWritesText(args)) && traceReportRE.MatchString(base):
		// A generator's output into a data file (go run ./cmd/schema >
		// openapi.json) is a file of the work, not a report.
		return traceDataRE.MatchString(base) && shellGenerates(prog, args)
	case prog == "cat" || prog == "echo" || prog == "printf":
		return true
	}
	return shellSourceName(base)
}

var traceDataRE = lazyre.New(`(?i)\.(json|csv|tsv|html|xml)$`)

// shellCheckNameRE is a script or target named as a check is, whose output
// is a report whatever the file is called.
var shellCheckNameRE = lazyre.New(`(?i)test|lint|check|vet|audit|bench|cover|build|tsc|verify|typecheck|ci\b`)

// shellGenerates says a reporter program is running code of the project
// that is not named as a check: go run, go generate, cargo run, an npm,
// pnpm or yarn script, an npx binary, a make target.
func shellGenerates(prog string, args []string) bool {
	var words []string
	for _, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
			words = append(words, a)
		}
	}
	if len(words) == 0 {
		return false
	}
	switch prog {
	case "go":
		return words[0] == "run" || words[0] == "generate"
	case "cargo":
		return words[0] == "run"
	case "npm", "pnpm", "yarn":
		if words[0] != "run" && words[0] != "run-script" && words[0] != "exec" {
			return false
		}
		return len(words) > 1 && !shellCheckNameRE.MatchString(words[1])
	case "npx", "make":
		return !shellCheckNameRE.MatchString(words[0])
	}
	return false
}

var shellDotfileRE = lazyre.New(`^\.[A-Za-z][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+)*$`)
var shellDotReportRE = lazyre.New(`(?i)^\.(log|out|err|tmp|pid|lock|coverage|cache)$`)

// shellSourceName says a file name is a source, document or configuration
// file's: ShellFileShape, or a dotfile (.env, .gitignore, .env.local).
func shellSourceName(base string) bool {
	return ShellFileShape(base) || shellDotfileRE.MatchString(base) && !shellDotReportRE.MatchString(base)
}

// shellFeeder is the command whose output the next command of the line reads
// through a pipe: the last one that is not inside a substitution.
func shellFeeder(cmds []ShellCommand) *ShellCommand {
	for i := len(cmds) - 1; i >= 0; i-- {
		if !cmds[i].Sub {
			return &cmds[i]
		}
	}
	return nil
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
	// Options come first and end at the first word that is not one (a script
	// file, a module): what follows is that program's own (python3 build.py
	// -e X), as ran() reads it.
	for j := i + 1; j < len(words); j++ {
		w := words[j]
		switch {
		case w == "-c" || w == "-e":
			if j+1 < len(words) {
				return words[j+1]
			}
			return ""
		case w == "-m" || w == "-" || w == "--":
			return ""
		case interpreterValueFlags[w]:
			j++
		case strings.HasPrefix(w, "-") || w == "run":
		default:
			if traceScriptFileRE.MatchString(w) {
				return ""
			}
			// Not a script file: taken as the value of an option this
			// reading does not know (node --max-old-space-size 100 -e X), so
			// a -e after it is not passed over.
		}
	}
	return ""
}

// interpreterValueFlags are interpreter options whose next word is a value,
// not the script: python -W/-X, node -r/--require/--import (not -I, python's isolated mode).
var interpreterValueFlags = map[string]bool{"-W": true, "-X": true, "-r": true, "--require": true, "--import": true,
	"--loader": true, "--experimental-loader": true, "--conditions": true, "-C": true}

// ran is the script file a command hands its interpreter (python3 edit.py),
// "" when it runs none.
func (c *ShellCommand) ran() string {
	if c.Script != "" {
		return ""
	}
	if c.In != "" {
		return c.In
	}
	return ScriptFile(c.Words)
}

// ScriptFile is the script file a command's words hand an interpreter
// (python3 edit.py, uv run python edit.py), "" when they run none. The guard
// reads that file where the write recogniser recalls it.
func ScriptFile(words []string) string {
	i := interpreterAt(words)
	if i < 0 {
		return ""
	}
	for j := i + 1; j < len(words); j++ {
		w := words[j]
		switch {
		case w == "-c" || w == "-e" || w == "-m" || w == "-":
			return ""
		case interpreterValueFlags[w]:
			j++ // python3 -X utf8 edit.py: the option's value, not the script
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

// StdinScript is the file an interpreter reads its script from on its input
// (python3 < edit.py, python3 - < edit.py): the last of ins, when the words
// run an interpreter and hand it no script, module or file of their own. ""
// otherwise.
func StdinScript(words, ins []string) string {
	i := interpreterAt(words)
	if i < 0 || len(ins) == 0 {
		return ""
	}
	for j := i + 1; j < len(words); j++ {
		w := words[j]
		switch {
		case w == "-":
			return ins[len(ins)-1]
		case w == "-c" || w == "-e" || w == "-m":
			return ""
		case interpreterValueFlags[w]:
			j++
		case strings.HasPrefix(w, "-"):
		default:
			return "" // a file or a subcommand of its own: the input is its data
		}
	}
	return ins[len(ins)-1]
}

const traceModeRE = `['"][rbtU]*[wax+][rbtUwax+]*['"]`

// The write calls of a script given to an interpreter, and the path each
// writes: open(P, 'w'), P.write_text(, Path(P).write_text(, writeFileSync(P,.
var (
	// A write mode holds w, a, x or +, with r, b, t, U around them: 'w', 'ab',
	// 'r+', 'rb+'. The mode is a closed string, so 'x' in webbrowser.open('x')
	// is read as a mode only where it is open()'s only argument, a method's
	// (Path(...).open(MODE), p.open(MODE)).
	traceScriptWriteRE = lazyre.New(`\bopen\([^\n]*?,\s*(?:mode\s*=\s*)?` + traceModeRE + `|\.open\(\s*(?:mode\s*=\s*)?` + traceModeRE + `|\.write_(?:text|bytes)\(|\b(?:write|append)File(?:Sync)?\(|\bFile\.write\(`)
	traceScriptSiteREs = []*lazyre.Regexp{
		lazyre.New(`\bopen\(\s*([^,()\n]+?|(?:pathlib\.)?Path\(\s*[^()\n]+?\s*\))\s*,\s*(?:mode\s*=\s*)?` + traceModeRE),
		lazyre.New(`((?:pathlib\.)?Path\(\s*[^()\n]+?\s*\)|[A-Za-z_][A-Za-z0-9_]*)\.open\(\s*(?:mode\s*=\s*)?` + traceModeRE),
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
//
// unfollowed is true when the script writes, no path came of it, and nothing
// shows its writes go to scratch (a temporary directory): a path it builds
// (glob, os.walk, a join) that this reading cannot name, so the answer is
// unknown, not "writes nothing".
func traceScriptWrites(script string, whole bool) (paths []string, sure, unfollowed bool) {
	sites, strs := 64, traceMaxStrings
	if whole {
		sites, strs = -1, -1
	} else if len(script) > traceMaxScript {
		script = script[:traceMaxScript]
	}
	if !traceScriptWriteRE.MatchString(script) {
		return nil, true, false
	}
	// Each name followed searches the text again: spend a fixed amount of
	// scanning, so a long script is followed fewer times than a short one.
	budget := min(traceScriptBudget, max(3, traceScriptScan/(3*len(script)+1)))
	lineOf := func(at int) int { return strings.LastIndexByte(script[:at], '\n') + 1 }
	followed := map[int]bool{} // the lines whose write was followed to a path
	for _, re := range traceScriptSiteREs {
		for _, m := range re.FindAllStringSubmatchIndex(script, sites) {
			ps := traceScriptExpr(script, script[m[2]:m[3]], 0, &budget)
			if len(ps) > 0 {
				followed[lineOf(m[0])] = true
			}
			paths = append(paths, ps...)
		}
	}
	// A write that was not followed, on a line with no temporary directory
	// (or name set from one) to explain it, leaves the answer unknown: the
	// paths found, if any, are not all the script writes. Scratch is judged
	// where the write stands, not by a temporary directory used elsewhere.
	var scratch map[string]bool
	missed := false
	if !whole {
		ms := traceScriptWriteRE.FindAllStringIndex(script, sites)
		// More writes than are read: the ones past the last are not known.
		missed = len(ms) == sites
		for _, m := range ms {
			from := lineOf(m[0])
			if followed[from] {
				continue
			}
			if scratch == nil {
				scratch = scriptScratchNames(script)
			}
			end := len(script)
			if i := strings.IndexByte(script[m[0]:], '\n'); i >= 0 {
				end = m[0] + i
			}
			if !scriptScratchLine(script[from:end], scratch) {
				missed = true
				break
			}
		}
	}
	// A whole reading that ran out of names to follow cannot say which paths
	// the names left over stand for: it lists every path the script spells
	// and does not read as well, so none is passed over (audit 2008.1).
	spent := whole && budget <= 0
	if !whole && budget <= 0 {
		missed = true // names were left unfollowed: the paths found are not all
	}
	if len(paths) > 0 && !spent {
		return paths, true, missed
	}
	sure = len(paths) > 0
	// Each string is judged where it stands (no second search of the text),
	// and one the script reads anywhere is left out.
	var names []string
	if scratch == nil {
		scratch = scriptScratchNames(script)
	}
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
		if scriptScratchAt(script, m[0], scratch) {
			continue // a name under the temporary directory, not a path of its own
		}
		if scriptReadsAt(script, m[0], m[1]) || scriptNameOnlyRead(script, m[0], m[1]) || scriptLookedFor(script, m[1]) {
			reads[str] = true
		}
		names = append(names, str)
	}
	have := make(map[string]bool, len(paths))
	for _, p := range paths {
		have[p] = true
	}
	for _, n := range names {
		if !reads[n] && !have[n] {
			have[n] = true
			paths = append(paths, n)
		}
	}
	return paths, sure, len(paths) == 0 && missed
}

// scriptScratchRE: text that names the temporary directory.
var scriptScratchRE = lazyre.New(`\btempfile\.|\bmkdtemp|\bmkstemp|\bos\.tmpdir\(|['"]/tmp\b`)

var (
	scriptSetRE   = lazyre.New(`(?m)^[ \t]*(?:const |let |var )?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*([^=\n][^\n]*)$`)
	scriptBindRE  = lazyre.New(`(?m)^[ \t]*(?:with|for)\b([^\n]*?)\b(?:as|in)\b([^\n]*)$`)
	scriptIdentRE = lazyre.New(`[A-Za-z_][A-Za-z0-9_]*`)
)

// scriptScratchNames are the names a script sets from the temporary
// directory: d = tempfile.mkdtemp(), d = os.path.join(tempfile.gettempdir(),
// 'w'), with TemporaryDirectory() as d, for p in d.rglob(...), and names set
// from those names in turn.
func scriptScratchNames(script string) map[string]bool {
	names := map[string]bool{}
	sets := scriptSetRE.FindAllStringSubmatch(script, 256)
	binds := scriptBindRE.FindAllStringSubmatch(script, 256)
	for pass := 0; pass < 3; pass++ {
		n := len(names)
		for _, m := range sets {
			if scriptScratchLine(m[2], names) {
				names[m[1]] = true
			}
		}
		for _, m := range binds {
			// with EXPR as NAME: the value is before the name; for NAME in
			// EXPR: after it.
			val, bound := m[1], m[2]
			if strings.HasPrefix(strings.TrimSpace(m[0]), "for") {
				val, bound = m[2], m[1]
			}
			if scriptScratchLine(val, names) {
				for _, id := range scriptIdentRE.FindAllString(bound, 4) {
					names[id] = true
				}
			}
		}
		if len(names) == n {
			break
		}
	}
	return names
}

// scriptScratchLine says a piece of script names the temporary directory or
// a name set from it.
func scriptScratchLine(text string, names map[string]bool) bool {
	if scriptScratchRE.MatchString(text) {
		return true
	}
	if len(names) == 0 {
		return false
	}
	for _, id := range scriptIdentRE.FindAllString(text, 64) {
		if names[id] {
			return true
		}
	}
	return false
}

// scriptScratchAt says the string at script[at:] stands in a join whose
// first argument is the temporary directory: a call to tempfile.*, os.tmpdir,
// a '/tmp…' literal, or a name the script set from one (names).
func scriptScratchAt(script string, at int, names map[string]bool) bool {
	before := script[max(0, at-64):at]
	if scriptScratchBeforeRE.MatchString(before) {
		return true
	}
	m := scriptJoinNameBeforeRE.FindStringSubmatch(before)
	return m != nil && names[m[1]]
}

var (
	scriptOpenBeforeRE     = lazyre.New(`\bopen\(\s*$`)
	scriptReadBeforeRE     = lazyre.New(`(?:\breadFile(?:Sync)?|\bFile\.(?:read|readlines|foreach)|\b(?:exists|isfile|existsSync))\(\s*$`)
	scriptScratchBeforeRE  = lazyre.New(`\bjoin\(\s*(?:tempfile\.\w+\(\)|os\.tmpdir\(\)|['"]/tmp[^'"\n]*['"])\s*,\s*$`)
	scriptJoinNameBeforeRE = lazyre.New(`\bjoin\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*,\s*$`)
	scriptReadAfterRE      = lazyre.New(`^\s*\)\.read_(?:text|bytes)\(`)
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

var scriptHeldRE = lazyre.New(`^[ \t]*(?:const |let |var )?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*$`)

// scriptNameOnlyRead says the string at from..to is all a name is set to
// (p = '.claude/settings.json') and every other place the script spells the
// name, at least one, reads it (open(p)). A use that is anything else, or a
// second setting of the name, leaves the answer no.
func scriptNameOnlyRead(script string, from, to int) bool {
	start := strings.LastIndexByte(script[:from], '\n') + 1
	m := scriptHeldRE.FindStringSubmatch(script[start:from])
	if m == nil {
		return false
	}
	rest := script[to:]
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[:i]
	}
	if strings.Trim(rest, " \t\r;") != "" {
		return false
	}
	uses := 0
	for _, u := range scriptIdentRE.FindAllStringIndex(script, 4096) {
		if script[u[0]:u[1]] != m[1] || u[0] >= start && u[1] <= from {
			continue
		}
		if !scriptReadsAt(script, u[0], u[1]) {
			return false
		}
		uses++
	}
	return uses > 0 && uses < 4096
}

var scriptInNameRE = lazyre.New(`^\s+(?:not\s+)?in\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:[:)\]]|and\b|or\b|$)`)

// scriptLookedFor says the string ending at to is only looked for in text
// the script read ('x.json' in s, where s = open(f).read()): no path of a
// file. A name not set from a read may be the path being written (the string
// picks which file), so that stays a no.
func scriptLookedFor(script string, to int) bool {
	after := script[to:min(len(script), to+200)]
	if i := strings.IndexByte(after, '\n'); i >= 0 {
		after = after[:i]
	}
	m := scriptInNameRE.FindStringSubmatch(after)
	if m == nil {
		return false
	}
	set := regexp.MustCompile(`(?m)(?:^|[;\s])` + m[1] + `\s*=[^=\n][^\n]*\bread(?:_text|_bytes|lines)?\(`)
	return set.MatchString(script)
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
	// Every setting of the name is followed (the budget bounds the searches,
	// the script's length the matches): a value past a fixed count would be
	// one the guard let through.
	for _, m := range set.FindAllStringSubmatch(script, -1) {
		out = append(out, traceScriptExpr(script, m[1], depth+1, budget)...)
	}
	loop := regexp.MustCompile(`\bfor\s+` + expr + `\s+in\s+[\[(]([^\n]*)[\])]\s*:`)
	for _, m := range loop.FindAllStringSubmatch(script, -1) {
		for _, s := range traceScriptStrRE.FindAllStringSubmatch(m[1], -1) {
			out = append(out, s[1]+s[2])
		}
	}
	def := regexp.MustCompile(`\bdef\s+([A-Za-z_][A-Za-z0-9_]*)\(\s*` + expr + `\s*[,):=]`)
	for _, m := range def.FindAllStringSubmatch(script, -1) {
		call := regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])` + m[1] + `\(\s*([^,()\n]+?)\s*[,)]`)
		for _, c := range call.FindAllStringSubmatch(script, -1) {
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
// --write, the formatters that rewrite unless told to check (black, rustfmt,
// ruff format, ruff check --fix, eslint --fix, go fmt, cargo fmt), an archive
// unpacked (tar x, unzip), curl -o, dd of=, mv and cp, git mv and git rm, a
// patch applied, and a script given to an interpreter that writes files. What
// doesn't: what a program writes of its own accord (a code generator, go mod
// tidy, git merge or checkout), which the call doesn't show; a plain rm,
// which is as often clearing up; and touch or mkdir, which write no text. A script the session kept in a file and
// runs later is added by shellCommands, which remembers them.
//
// Output sent to a file is judged by shellOutputCounts, and so is what tee
// keeps of the command piped into it. A redirect after a group or loop is
// added by shellParsed, as are the commands inside a substitution.
func (c *ShellCommand) writes(disk bool) []ShellWrite {
	var out []ShellWrite
	prog, args := path.Base(c.Words[0]), c.Words[1:]
	for _, r := range c.Redirs {
		switch {
		case strings.HasPrefix(r, "/dev/"):
		case shellOutputCounts(prog, args, r):
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
	if shellRunners[prog] || strings.HasPrefix(prog, "python3.") {
		// npx [--prefix DIR] prettier ..., pnpm exec prettier, yarn eslint,
		// uv run ruff, python -m black: the tool is the first word named as
		// one among the first few, whatever flags and their values come
		// before it. A runner asked to install or remove names packages, not
		// a tool to run.
		for i, a := range args[:min(len(args), 6)] {
			if shellRunnerVerbs[a] {
				break
			}
			if b := path.Base(a); shellRunTools[b] {
				prog, args = b, args[i+1:]
				if len(args) > 0 && args[0] == "--" {
					args = args[1:] // npm exec prettier -- --write .
				}
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
	case "black", "rustfmt":
		// These rewrite the files they are given unless told only to check.
		if !has("--check", "--diff", "--version", "--help", "-h", "--emit=stdout") {
			each("format", plain(0))
		}
	case "ruff":
		if len(args) > 0 && (args[0] == "format" && !has("--check", "--diff") ||
			args[0] == "check" && has("--fix", "--fix-only", "--unsafe-fixes") && !has("--diff")) {
			if ws := plain(1); len(ws) > 0 {
				each("format", ws)
			} else {
				out = append(out, ShellWrite{Kind: "format", Path: "*"})
			}
		}
	case "eslint":
		if has("--fix") && !has("--fix-dry-run") {
			if ws := plain(0); len(ws) > 0 {
				each("format", ws)
			} else {
				out = append(out, ShellWrite{Kind: "format", Path: "*"})
			}
		}
	case "go", "cargo":
		// go fmt and cargo fmt rewrite the packages of the directory they run in.
		if len(args) > 0 && args[0] == "fmt" && !has("-n", "--check") {
			out = append(out, ShellWrite{Kind: "format", Path: "*"})
		}
	case "tar":
		if root, ok := tarExtracts(args); ok {
			out = append(out, ShellWrite{Kind: "extract", Path: "*", Root: root})
		}
	case "unzip":
		if !has("-l", "-t", "-v", "-p", "-c", "-Z", "-z") {
			root := ""
			for i, a := range args {
				if a == "-d" && i+1 < len(args) {
					root = args[i+1]
				} else if strings.HasPrefix(a, "-d") && len(a) > 2 {
					root = a[2:]
				}
			}
			out = append(out, ShellWrite{Kind: "extract", Path: "*", Root: root})
		}
	case "curl":
		for i := 0; i < len(args); i++ {
			a := args[i]
			f, ok := strings.CutPrefix(a, "--output=")
			if j := strings.IndexByte(a, 'o'); !ok && j > 0 && a[0] == '-' && a[1] != '-' && strings.Trim(a[1:j], curlBareFlags) == "" {
				// -o FILE, -fsSLo FILE, -oFILE: o after letters that take no value.
				if f, ok = a[j+1:], true; f == "" && i+1 < len(args) {
					i++
					f = args[i]
				}
			} else if a == "--output" && i+1 < len(args) {
				i++
				f, ok = args[i], true
			}
			if ok && f != "" {
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
		if !has("--check", "--stat", "--numstat", "--summary", "--dry-run") || prog != "patch" && has("--apply") {
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
		ps, sure, unfollowed := traceScriptWrites(script, false)
		if !sure {
			kind = "script?"
		}
		each(kind, ps)
		c.Unread = unfollowed || len(script) > traceMaxScript && !traceScriptWriteRE.MatchString(script[:traceMaxScript])
	}
	return out
}

// shellRunners run a tool of the project for the agent, and shellRunTools
// are the tools read through them: the formatters writes() knows.
var (
	shellRunners = map[string]bool{"npx": true, "bunx": true, "pnpm": true, "yarn": true, "npm": true, "bun": true, "uv": true, "uvx": true,
		"poetry": true, "pipx": true, "pdm": true, "hatch": true, "rye": true, "python": true, "python3": true}
	shellRunTools    = map[string]bool{"prettier": true, "eslint": true, "black": true, "ruff": true, "rustfmt": true, "gofmt": true, "goimports": true}
	shellRunnerVerbs = map[string]bool{"install": true, "i": true, "add": true, "remove": true, "rm": true, "uninstall": true, "update": true, "upgrade": true,
		"up": true, "why": true, "info": true, "view": true, "ls": true, "list": true, "outdated": true, "link": true, "publish": true, "pip": true}
)

// curlBareFlags are curl's one-letter options that take no value, so an o
// after them in one word is -o.
const curlBareFlags = "fsSLkiIvJNgqOR46#"

// tarExtracts says tar is asked to extract (x among its first word's letters
// or its short options, --extract, --get), and into which directory (-C DIR,
// --directory DIR; "" is the command's own).
func tarExtracts(args []string) (root string, ok bool) {
	for i, a := range args {
		switch {
		case a == "--extract" || a == "--get":
			ok = true
		case a == "-C" || a == "--directory":
			if i+1 < len(args) {
				root = args[i+1]
			}
		case strings.HasPrefix(a, "--directory="):
			root = a[len("--directory="):]
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-C") && len(a) > 2:
			root = a[2:]
		case strings.HasPrefix(a, "-") || i == 0:
			ok = ok || strings.ContainsRune(a, 'x')
			if strings.HasSuffix(a, "C") && i+1 < len(args) && strings.HasPrefix(a, "-") {
				root = args[i+1]
			}
		}
	}
	return root, ok
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
// -e. A bare -I takes its directory from the next word (next).
func perlSwitch(a string) (inPlace, code, next bool) {
	for j := 1; j < len(a); j++ {
		switch a[j] {
		case 'i':
			return true, false, false
		case 'e', 'E':
			return false, true, j == len(a)-1
		case 'I':
			// -I DIR may be two words: a bare -I takes the next one.
			return false, false, j == len(a)-1
		case 'M', 'm', 'x', 'F', 'C', 'D', 'V':
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
	ps, _, _ := traceScriptWrites(script, true)
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
