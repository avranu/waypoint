package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A whole-suite `go test` in the tests slot runs the tests beside the diff
// instead, and says so (WP-ADR161 point 3, WP-1877). Everyone reaches the slot
// (wp slot run tests, wp testsum, wp ship --check), nobody has to remember
// -run, and --all or WP_TEST_ALL=1 still runs everything. The selection is
// WP-1875's selectGoTests, the same one wp test go uses.

const (
	// narrowedMark ends the history key and the shown command of a narrowed
	// run, so its time never enters the median of the whole command's runs
	// and the reverse.
	narrowedMark = " (narrowed)"
	// narrowTestsSlot is the slot name whose runs are narrowed.
	narrowTestsSlot = "tests"
	// narrowSHALen is how much of the base commit the notice shows.
	narrowSHALen = 9
)

// goTestValueFlags and goTestBoolFlags are the flags of go test (and the build
// flags it takes) by whether they take a value, so a package argument can be
// told from a flag's value. A flag in neither is one this code doesn't know:
// the run is then left alone.
var (
	goTestValueFlags = setOf("run bench benchtime count cpu timeout parallel tags p vet covermode coverpkg coverprofile cpuprofile memprofile memprofilerate blockprofile blockprofilerate mutexprofile mutexprofilefraction outputdir trace o exec fuzz fuzztime fuzzminimizetime shuffle skip gcflags ldflags asmflags gccgoflags mod modfile overlay pkgdir toolexec buildvcs compiler installsuffix buildmode list testlogfile")
	goTestBoolFlags  = setOf("json v race short failfast cover x n a work msan asan trimpath c benchmem linkshared fullpath")
)

// goTestArgs is a direct go test command line taken apart.
type goTestArgs struct {
	Flags []string // every flag with its value, in order
	Pkgs  []string // the package arguments as typed
	// Run, Fuzz, List and Compile: a flag that picks which tests run or that
	// runs none. Such a command is left alone.
	Run, Fuzz, List, Compile bool
}

// splitGoTestArgs takes argv apart when it is `go test …` directly, with
// flags it knows and no -args; ok is false for anything else (make, sh -c,
// env VAR=… go test).
func splitGoTestArgs(argv []string) (a goTestArgs, ok bool) {
	if len(argv) < 2 || argv[0] != "go" || argv[1] != "test" {
		return a, false
	}
	for i := 2; i < len(argv); i++ {
		arg := argv[i]
		if arg == "-args" || arg == "--args" || arg == "--" {
			return a, false
		}
		if len(arg) < 2 || arg[0] != '-' {
			a.Pkgs = append(a.Pkgs, arg)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		a.Flags = append(a.Flags, arg)
		switch {
		case goTestValueFlags[name]:
			if !hasValue {
				if i+1 >= len(argv) {
					return a, false
				}
				i++
				a.Flags = append(a.Flags, argv[i])
			}
		case goTestBoolFlags[name]:
		default:
			return a, false
		}
		switch name {
		case "run":
			a.Run = true
		case "fuzz":
			a.Fuzz = true
		case "list":
			a.List = true
		case "c":
			a.Compile = true
		}
	}
	return a, true
}

// narrowEnv is what the decision to narrow reads from the world, so a test can
// give it a repository of its own.
type narrowEnv struct {
	root, cwd string
	module    string   // the Go module path, "" when unknown
	heavy     []string // .waypoint/heavy-tests
	// all: --all was given or WP_TEST_ALL=1. runner: a runner job; CI is
	// never narrowed.
	all, runner bool
	// changes is the base commit and the paths that differ from it.
	changes func() (base string, changed []string, err error)
}

// loadNarrowEnvFn is loadNarrowEnv, replaceable by a test.
var loadNarrowEnvFn = loadNarrowEnv

// loadNarrowEnv reads the checkout the command runs in; ok is false outside a
// git checkout, where nothing is narrowed.
func loadNarrowEnv(all bool) (narrowEnv, bool) {
	root, err := localGit("rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return narrowEnv{}, false
	}
	cwd, err := os.Getwd()
	if err != nil {
		return narrowEnv{}, false
	}
	return narrowEnv{
		root: root, cwd: cwd, module: goModulePath(root), heavy: readHeavyTests(root, nil),
		all: all || os.Getenv(envTestAll) == "1", runner: inRunnerJob(),
		changes: func() (string, []string, error) {
			_, base, changed, err := testChanges(os.Getenv(envTestBase))
			return base, changed, err
		},
	}, true
}

// goModulePath is the module line of root's go.mod, "" when there is none.
func goModulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod")) // #nosec G304 -- a fixed name in the checkout
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// goTestPlan is what the slot does with a go test command.
type goTestPlan struct {
	// Whole: the command runs a whole heavy package, or ./..., with no -run.
	// slotClassFor makes such a run long.
	Whole bool
	// All: it would have been narrowed but --all or WP_TEST_ALL=1 asked for
	// everything (counted by wp slot ls).
	All bool
	// Narrow: run Cmds instead, from Root. No Cmds: nothing in the asked
	// packages changed, so nothing runs.
	Narrow  bool
	Cmds    [][]string
	Root    string
	Base    string
	Sel     goSelection
	Changed int // changed Go files in the asked packages
	// HeavyChanged: how many of them are in heavy packages, which run by file.
	HeavyChanged int
	Asked        []string // the package arguments as typed
	Why          string   // why a whole heavy run was not narrowed, when that is news
}

// planGoTest decides, for argv in the tests slot, whether it is narrowed to
// the diff. It narrows when argv is `go test` directly, with no -run, -fuzz or
// -list; its packages cover a heavy package of .waypoint/heavy-tests (none:
// the working folder, which has to be one); not asked for everything, not a
// runner job, and the base and a checkout can be found. Anything else is the
// zero plan, run as asked.
func planGoTest(argv []string, in narrowEnv) goTestPlan {
	a, ok := splitGoTestArgs(argv)
	if !ok || a.Run || a.Fuzz || a.List || a.Compile || len(in.heavy) == 0 {
		return goTestPlan{}
	}
	match, ok := in.matcher(a.Pkgs)
	if !ok || !slices.ContainsFunc(in.heavy, func(h string) bool { return match(cleanPkgDir(h)) }) {
		return goTestPlan{}
	}
	p := goTestPlan{Whole: true, Root: in.root, Asked: a.Pkgs}
	if in.all || in.runner {
		p.All = in.all
		return p
	}
	base, changed, err := in.changes()
	if err != nil {
		p.Why = err.Error()
		return p
	}
	heavySet := map[string]bool{}
	for _, h := range in.heavy {
		heavySet[cleanPkgDir(h)] = true
	}
	var kept []string
	for _, f := range changed {
		f = path.Clean(filepath.ToSlash(f))
		if match(path.Dir(f)) {
			kept = append(kept, f)
			if strings.HasSuffix(f, ".go") {
				p.Changed++
				if heavySet[path.Dir(f)] {
					p.HeavyChanged++
				}
			}
		}
	}
	p.Whole, p.Narrow, p.Base = false, true, base
	p.Sel = selectGoTests(in.root, kept, in.heavy)
	p.Cmds = p.Sel.commands(a.Flags)
	return p
}

// matcher is whether a package directory (relative to the root) is one of the
// packages pkgs name; none named is the working folder. ok is false for an
// argument it can't place (all, std, a .go file).
func (in narrowEnv) matcher(pkgs []string) (func(dir string) bool, bool) {
	if len(pkgs) == 0 {
		pkgs = []string{"."}
	}
	var res []*regexp.Regexp
	for _, p := range pkgs {
		rel, ok := in.pkgRel(p)
		if !ok {
			return nil, false
		}
		res = append(res, dirPatterns(rel)...)
	}
	return func(dir string) bool {
		return slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(dir) })
	}, true
}

// pkgRel is the package argument p as a directory pattern relative to the
// root: ./x and ../x from the working folder, the module's import path, an
// absolute path, and x/y when that is a folder here.
func (in narrowEnv) pkgRel(p string) (string, bool) {
	p = filepath.ToSlash(p)
	switch {
	case strings.HasSuffix(p, ".go"):
		return "", false
	case in.module != "" && (p == in.module || strings.HasPrefix(p, in.module+"/")):
		return path.Clean("./" + strings.TrimPrefix(strings.TrimPrefix(p, in.module), "/")), true
	case p == "." || p == ".." || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../"):
		cwd, rootReal := evalDir(in.cwd), evalDir(in.root)
		rel, err := filepath.Rel(rootReal, cwd)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", false
		}
		out := path.Join(filepath.ToSlash(rel), p)
		return out, out != ".." && !strings.HasPrefix(out, "../")
	case path.IsAbs(p):
		rel, err := filepath.Rel(evalDir(in.root), p)
		out := filepath.ToSlash(rel)
		return out, err == nil && out != ".." && !strings.HasPrefix(out, "../")
	}
	dir, _, _ := strings.Cut(p, "...")
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return path.Clean(p), true
	}
	if st, err := os.Stat(filepath.Join(in.root, filepath.FromSlash(dir))); err != nil || !st.IsDir() {
		return "", false
	}
	return path.Clean(p), true
}

func evalDir(d string) string {
	if r, err := filepath.EvalSymlinks(d); err == nil {
		return r
	}
	return d
}

// dirPatterns are the expressions a directory has to match for the package
// pattern rel (with "..." for any text, as go has it; x/... takes x as well).
func dirPatterns(rel string) []*regexp.Regexp {
	parts := strings.Split(rel, "...")
	for i, s := range parts {
		parts[i] = regexp.QuoteMeta(s)
	}
	out := []*regexp.Regexp{regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")}
	if prefix, ok := strings.CutSuffix(rel, "/..."); ok {
		out = append(out, dirPatterns(prefix)...)
	}
	return out
}

// slotOpts is what the caller of runInSlotWith says about its run.
type slotOpts struct {
	all bool // --all: don't narrow
	// who is what the notice says it is (wp testsum); allCmd is the command
	// that runs the original command whole (wp testsum --all --).
	who, allCmd string
	// rec, when not nil, is marked as a scoped check if the run is narrowed,
	// so its evidence doesn't read as the whole package passing.
	rec *testRunRecorder
	// nothing, when not nil, is set when the run was narrowed to nothing: no
	// command ran, so the caller has no output to read and no evidence to keep.
	nothing *bool
}

// shownArgv is argv as the caller wrote it, without the -json goTestJSON adds.
func shownArgv(argv []string) []string {
	if len(argv) > 2 && argv[0] == "go" && argv[1] == "test" && argv[2] == "-json" {
		return append([]string{"go", "test"}, argv[3:]...)
	}
	return argv
}

// narrowNotice is what the slot says, on stderr, about a narrowed run: what
// runs and the command that runs everything. median is the recorded time of
// the whole command, 0 when there is none.
func narrowNotice(who, allCmd string, argv []string, p goTestPlan, median time.Duration) []string {
	var heavy, whole []string
	var tests []string
	for _, h := range p.Sel.Heavy {
		heavy = append(heavy, strings.TrimPrefix(h.Pkg, "./"))
		tests = append(tests, h.Tests...)
	}
	slices.Sort(tests)
	tests = slices.Compact(tests)
	for _, w := range p.Sel.Whole {
		whole = append(whole, strings.TrimPrefix(w, "./"))
	}
	asked := "the whole of " + shellJoin(p.Asked)
	if len(p.Asked) == 0 {
		asked = "the whole package"
	} else if len(p.Asked) == 1 && len(p.Sel.Packages) <= 1 && !strings.Contains(p.Asked[0], "...") {
		asked = "the whole package"
	}
	var lines []string
	base := p.Base[:min(len(p.Base), narrowSHALen)]
	switch {
	case len(p.Cmds) == 0 && len(p.Sel.NoTests) == 0:
		lines = append(lines, fmt.Sprintf("%s: nothing changed in %s since the base (%s), so no test runs.", who, shellJoin(p.askedOrHere()), base))
	case len(p.Cmds) == 0:
		lines = append(lines, fmt.Sprintf("%s: %s changed in %s since the base (%s) but no test sits beside it, so no test runs.", who, plural1(p.Changed, "file"), strings.Join(p.Sel.Packages, " "), base))
	default:
		var what []string
		if len(heavy) > 0 {
			what = append(what, fmt.Sprintf("the %s beside your %s in %s", plural1(len(tests), "test"), plural1(p.HeavyChanged, "changed file"), strings.Join(heavy, ", ")))
		}
		if len(whole) > 0 {
			what = append(what, fmt.Sprintf("every test of %s", strings.Join(whole, ", ")))
		}
		lines = append(lines, fmt.Sprintf("%s: running %s, not %s (base %s).", who, strings.Join(what, " and "), asked, base))
	}
	if len(p.Sel.NoTests) > 0 {
		lines = append(lines, "  No tests beside: "+strings.Join(p.Sel.NoTests, ", ")+".")
	}
	bracket := "CI runs the whole suite on the pull request"
	if median > 0 {
		bracket = approxDur(median) + " here; " + bracket
	}
	label := "The whole package"
	if asked != "the whole package" {
		label = "Everything asked for"
	}
	return append(lines, fmt.Sprintf("  %s: %s %s   (%s)", label, allCmd, shellJoin(shownArgv(argv)), bracket))
}

// askedOrHere is the packages asked for, or the working folder when none was named.
func (p goTestPlan) askedOrHere() []string {
	if len(p.Asked) == 0 {
		return []string{"."}
	}
	return p.Asked
}

// approxDur is d in words, in whole seconds under 90 and minutes above.
func approxDur(d time.Duration) string {
	if d < 90*time.Second {
		return fmt.Sprintf("about %d seconds", int(d.Round(time.Second)/time.Second))
	}
	return fmt.Sprintf("about %d minutes", int(d.Round(time.Minute)/time.Minute))
}

// runInSlotThen runs argv holding a slot, as runInSlotWith does for a caller
// with nothing to say about it.
func (c *cli) runInSlotThen(name string, n int, argv []string, out, errw io.Writer, started func(), envf func([]string) []string) (int, error) {
	return c.runInSlotWith(name, n, argv, out, errw, started, envf, slotOpts{})
}

// runInSlotWith runs argv holding a slot, with its output going to out and
// errw, and returns its exit code. It calls started (when not nil) once the
// slot is held, just before the command starts, and runs the command with
// envf(env) when envf isn't nil. In the tests slot a whole-suite go test is
// narrowed first (planGoTest), before the class is decided and before the run
// is recorded; the commands of the selection run one after another under the
// one hold, and the exit is the first that isn't 0.
func (c *cli) runInSlotWith(name string, n int, argv []string, out, errw io.Writer, started func(), envf func([]string) []string, opts slotOpts) (int, error) {
	own := !slotHeld(name)
	cmd := ""
	if own {
		cmd = slotRunKey(os.Args, argv)
	}
	run := slotRunOf(argv)
	var plan goTestPlan
	if name == narrowTestsSlot {
		if _, ok := splitGoTestArgs(argv); ok {
			if in, ok := loadNarrowEnvFn(opts.all); ok {
				plan = planGoTest(argv, in)
			}
		}
	}
	run.whole, run.all = plan.Whole, plan.All
	if opts.who == "" {
		opts.who, opts.allCmd = "wp slot", "wp slot run tests --all --"
	}
	do := func(ctx context.Context, env []string) (int, error) {
		if envf != nil {
			env = envf(env)
		}
		return runSlotCmd(ctx, env, argv, os.Stdin, out, errw)
	}
	if plan.Why != "" {
		_, _ = fmt.Fprintf(c.err, "%s: not narrowing this go test to your changes: %s\n", opts.who, plan.Why)
	}
	if plan.Narrow {
		var median time.Duration
		if dir, err := slotDir(); err == nil {
			median, _ = slotMedianRun(slotRuns(dir), name, slotRunKey(os.Args, argv), slotRepoKey())
		}
		for _, l := range narrowNotice(opts.who, opts.allCmd, argv, plan, median) {
			_, _ = fmt.Fprintln(c.err, l)
		}
		if len(plan.Cmds) == 0 {
			if opts.nothing != nil {
				*opts.nothing = true
			}
			return 0, nil
		}
		run.key += narrowedMark
		run.shown += narrowedMark
		run.narrowed, run.whole = true, false
		if cmd != "" {
			cmd += narrowedMark
		}
		if opts.rec != nil {
			opts.rec.label = opts.rec.command() + scopedMark
		}
		do = func(ctx context.Context, env []string) (int, error) {
			if envf != nil {
				env = envf(env)
			}
			code := 0
			for _, argv := range plan.Cmds {
				got, err := runSlotCmdIn(ctx, plan.Root, env, argv, os.Stdin, out, errw)
				if err != nil {
					return 0, err
				}
				if code == 0 {
					code = got
				}
			}
			return code, nil
		}
	}
	return c.runInSlotDo(name, n, run, cmd, started, do)
}
