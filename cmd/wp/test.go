package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/codeindex"
)

// The scoped check (WP-ADR161 points 2 and 3, WP-1875): wp works out what
// changed and runs the repository's own check in the tests slot; it knows
// nothing of any repository's linters.

const (
	testScriptFile = ".waypoint/test"
	// scopedMark is what the record's command says when only what changed
	// was checked, so no one reads the run as the whole suite. The server's
	// decoder refuses a field it doesn't know, so a field would break
	// clients newer than the server.
	scopedMark = " (scoped check)"

	envTestBase    = "WP_TEST_BASE"
	envTestChanged = "WP_TEST_CHANGED"
	envTestAll     = "WP_TEST_ALL"
	envTestWP      = "WP_TEST_WP"

	// testGoTimeout is the built-in Go check's -timeout, as make test had it.
	testGoTimeout = "30m"
	// testTailKeep is how much of a script's output is kept to name what failed.
	testTailKeep = 4096
)

var testCmd = &command{
	name: "test", group: "Code and CI", args: "[--all] [--base REF] [--dry-run] | go [--print] [--by-file PKG]… [-- GOTESTFLAGS…]",
	summary: "check what changed against the base, in the tests slot: lint, vet and the tests the change touches",
	about: `Runs the repository's scoped check on what differs from the base: committed
since the merge base with the remote's default branch (--base REF changes
that), staged, unstaged, and new files that aren't ignored; deleted files
count. It runs in the tests slot as wp testsum does, without secret-looking
variables, and keeps the run against the tree as "wp test (scoped check)".

The check is the repository's executable .waypoint/test, run from the
repository's root with
  WP_TEST_BASE     the base commit
  WP_TEST_CHANGED  a file listing the changed paths, one per line
  WP_TEST_ALL=1    when --all asks for the whole check (it takes long)
  WP_TEST_WP       this wp, for the script's own wp calls
Without one, a Go module gets go build ./..., go vet and the tests of the
changed packages (wp test go). Anything else has no scoped check: wp test
says how to add one and exits 2.

A pass checks the change, not the tree: CI runs the whole suite on the
pull request. --dry-run says what would run and runs nothing.
wp test go is the Go test selection alone.`,
	examples: []string{"wp test", "wp test --dry-run", "wp test go --print"},
	run:      cmdTest,
}

func init() {
	testCmd.subs = []*command{{
		name: "go", args: "[--print] [--by-file PKG]… [-- GOTESTFLAGS…]",
		summary: "run the Go tests the change touches: changed packages whole, heavy ones by file",
		about: `Runs the Go tests selected by the changed paths (WP_TEST_CHANGED if set,
else the diff against the base, as wp test). A changed package runs whole,
except the heavy ones, which run the tests of the changed test files and of
the test files beside the changed source files. Heavy packages are the
directories listed in .waypoint/heavy-tests (one per line, # comments) and
every --by-file. A file under testdata/ selects the package above it, and
so does a file a package embeds (go list says which); in a heavy package that
means the tests whose file names it. It runs at most three go test commands,
each with -count=1 and the flags after --; -vet=off isn't added, pass it when
vet already ran. Changed files in a heavy package with no test beside them
are listed, and a heavy package left with no test selected is compiled with
its tests and none runs (go test -run '^$'), so a file that doesn't build
still fails; that compile is not recorded as the tests passing.
--print shows the commands and runs nothing. This is a heuristic: a change
in foo.go can break a test in bar_test.go, and CI runs everything.`,
		examples: []string{"wp test go --print", "wp test go -- -vet=off -timeout 30m"},
		run:      cmdTestGo,
	}}
}

// testChanges is the repository root, the base commit (the merge base of
// HEAD with ref, default the remote's default branch) and the paths that
// differ from it in the working tree.
func testChanges(ref string) (root, base string, changed []string, err error) {
	root, err = localGit("rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return "", "", nil, errors.New("wp test runs inside a git repository: cd into one")
	}
	if ref == "" {
		if ref = defaultBranchRef(root); ref == "" {
			return "", "", nil, errors.New("there is no origin/main, main or master here to compare with: name the base with wp test --base REF")
		}
	} else if err := codeindex.CheckRef(ref); err != nil {
		return "", "", nil, fmt.Errorf("--base %q: %w", ref, err)
	}
	base, err = gitIn(root, "merge-base", ref, "HEAD")
	if err != nil || base == "" {
		return "", "", nil, fmt.Errorf("%s and HEAD have no common ancestor to compare with (git fetch first?): name another base with wp test --base REF", ref)
	}
	changed, err = changedFiles(root, base)
	return root, base, changed, err
}

// changedFiles lists the paths that differ between base and the working
// tree: committed, staged and unstaged (renames as a delete and an add), and
// untracked files that aren't ignored. Sorted, relative to root.
func changedFiles(root, base string) ([]string, error) {
	tracked, err := gitIn(root, "diff", "--name-only", "--no-renames", "-z", base)
	if err != nil {
		return nil, fmt.Errorf("can't list what changed since %s: %w", shortSHA(base), err)
	}
	untracked, err := gitIn(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("can't list the new files here: %w", err)
	}
	var out []string
	for _, p := range strings.Split(tracked+"\x00"+untracked, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// testScript is the repository's .waypoint/test when it is executable, else "".
func testScript(root string) string {
	p := filepath.Join(root, filepath.FromSlash(testScriptFile))
	if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 { // #nosec G703 -- a fixed name in the checkout
		return p
	}
	return ""
}

// hasTestScript is whether the repository at root has a scoped check of its own.
func hasTestScript(root string) bool { return testScript(root) != "" }

func isGoModule(root string) bool {
	st, err := os.Stat(filepath.Join(root, "go.mod"))
	return err == nil && st.Mode().IsRegular()
}

// noScopedCheck says what a repository without a scoped check does to get one.
const noScopedCheck = `wp test: this repository has no scoped check, so there is nothing to run. Add an executable .waypoint/test, run from the repository's root with WP_TEST_BASE (the base commit), WP_TEST_CHANGED (a file of the changed paths, one per line) and WP_TEST_ALL=1 (when --all asks for the whole check). For example:
  mkdir -p .waypoint && printf '#!/bin/sh\n# WP_TEST_ALL=1: the whole check; else check the paths in $WP_TEST_CHANGED\nexec make test\n' > .waypoint/test && chmod +x .waypoint/test
(Go modules need none: wp test runs go build, go vet and the changed packages' tests.)`

func cmdTest(c *cli, args []string) error {
	fs := c.flags()
	all := fs.Bool("all", false, "run the repository's whole check, not only what changed (takes long)")
	ref := fs.String("base", "", "compare with `REF` (default: where this branch left the remote's default branch)")
	dry := fs.Bool("dry-run", false, "say what would run and run nothing")
	rest, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return c.usage("unexpected %q: wp test takes flags only (the Go selection alone is wp test go)", rest[0])
	}
	root, base, changed, err := testChanges(*ref)
	if err != nil {
		return err
	}
	if len(changed) == 0 && !*all {
		c.println(fmt.Sprintf("wp test: nothing changed since %s, so nothing to check (wp test --all runs the whole check).", shortSHA(base)))
		return nil
	}
	script := testScript(root)
	what := "the repository's " + testScriptFile
	if script == "" {
		if !isGoModule(root) {
			_, _ = fmt.Fprintln(c.err, noScopedCheck)
			return exitCode(2)
		}
		what = "the built-in Go check (go build ./..., go vet and the tests of the changed packages)"
		if *all {
			what = "the built-in Go check over the whole module (go build, go vet and go test of ./...)"
		}
	}
	c.println(fmt.Sprintf("wp test: checking %s against %s with %s.", plural1(len(changed), "changed file"), shortSHA(base), what))
	t := &scopedTest{c: c, root: root, base: base, changed: changed, all: *all, script: script}
	if *dry {
		return t.dryRun()
	}
	return t.run()
}

// scopedTest is one run of wp test.
type scopedTest struct {
	c       *cli
	root    string
	base    string
	changed []string
	all     bool
	script  string
}

func (t *scopedTest) label() string {
	if t.all {
		return "wp test --all"
	}
	return "wp test" + scopedMark
}

// steps are the built-in Go check's commands; the last holds the tests.
func (t *scopedTest) goSteps() (sel goSelection, build, vet []string, tests [][]string) {
	build = []string{"go", "build", "./..."}
	if t.all {
		sel = goSelection{Packages: []string{"./..."}, Whole: []string{"./..."}}
	} else {
		sel = selectGoTests(t.root, t.changed, readHeavyTests(t.root, nil))
	}
	if len(sel.Packages) > 0 {
		vet = append([]string{"go", "vet"}, sel.Packages...)
	}
	return sel, build, vet, sel.commands([]string{"-vet=off", "-timeout", testGoTimeout})
}

func (t *scopedTest) dryRun() error {
	c := t.c
	if t.script != "" {
		all := ""
		if t.all {
			all = " " + envTestAll + "=1"
		}
		c.println(fmt.Sprintf("Would run %s from %s with %s=%s %s=FILE-OF-CHANGED-PATHS%s.", testScriptFile, t.root, envTestBase, t.base, envTestChanged, all))
		return nil
	}
	sel, build, vet, tests := t.goSteps()
	c.println("Would run:")
	c.println("  " + shellJoin(build))
	if vet != nil {
		c.println("  " + shellJoin(vet))
	}
	for _, argv := range tests {
		c.println("  " + shellJoin(argv))
	}
	t.noTestsNote(sel)
	return nil
}

func (t *scopedTest) noTestsNote(sel goSelection) {
	if l := sel.noTestsLine(); l != "" {
		t.c.println(l)
	}
}

const ciRunsAllNote = "CI runs the whole suite on the pull request."

func (t *scopedTest) run() error {
	c := t.c
	rec := newTestRunRecorder([]string{"wp", "test"})
	rec.label = t.label()
	var dropped []string
	var verdict string
	code, err := c.runInSlotDo("tests", defaultSlots(), slotRun{key: "wp test", shown: "wp test"}, slotRunCmdFor("wp test"), rec.started, func(ctx context.Context, env []string) (int, error) {
		env, dropped = testEnv(env, nil)
		if t.script != "" {
			return t.runScript(ctx, env, rec, &verdict)
		}
		return t.runGo(ctx, env, rec, &verdict)
	})
	if err != nil {
		return fmt.Errorf("wp test couldn't run: %w", err)
	}
	if code != 0 {
		c.withheld(dropped)
		c.println("wp test: failed: " + verdict)
		return exitCode(1)
	}
	if t.all {
		c.println("wp test --all: passed: " + verdict)
	} else {
		c.println("wp test: passed: " + verdict + " " + ciRunsAllNote)
	}
	return nil
}

// slotRunCmdFor is the history key of a run that takes its slot here: ""
// where this process tree already holds it.
func slotRunCmdFor(key string) string {
	if slotHeld("tests") {
		return ""
	}
	return key
}

// runScript runs the repository's .waypoint/test, its output streaming to
// stderr. verdict names what failed (the script's last line) or passed.
func (t *scopedTest) runScript(ctx context.Context, env []string, rec *testRunRecorder, verdict *string) (int, error) {
	list, err := os.CreateTemp("", "wp-test-changed-*")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(list.Name()) }()
	_, werr := list.WriteString(strings.Join(t.changed, "\n") + "\n")
	if cerr := list.Close(); werr != nil || cerr != nil {
		return 0, errors.Join(werr, cerr)
	}
	vars := []string{envTestBase + "=" + t.base, envTestChanged + "=" + list.Name()}
	if exe, err := os.Executable(); err == nil {
		vars = append(vars, envTestWP+"="+exe)
	}
	if t.all {
		vars = append(vars, envTestAll+"=1")
	}
	env = withEnvVars(env, []string{envTestBase, envTestChanged, envTestAll, envTestWP}, vars)
	tail := &tailWriter{}
	w := io.MultiWriter(t.c.err, tail)
	code, err := runSlotCmdIn(ctx, t.root, env, []string{t.script}, nil, w, w)
	if err != nil {
		return 0, err
	}
	if code == 0 {
		*verdict = testScriptFile + " exited 0."
		rec.finish(&testResult{}, 0)
		return 0, nil
	}
	*verdict = fmt.Sprintf("%s exited %d (%s)", testScriptFile, code, tail.lastLine())
	rec.failed(code)
	return code, nil
}

// runGo is the built-in Go check: build, vet and the selected tests, every
// step run even after one fails, so one run says all that is wrong.
func (t *scopedTest) runGo(ctx context.Context, env []string, rec *testRunRecorder, verdict *string) (int, error) {
	c := t.c
	sel, build, vet, tests := t.goSteps()
	t.noTestsNote(sel)
	rec.compileOnly = sel.compileOnly()
	var failed, passed []string
	step := func(name string, argv []string) error {
		code, err := runSlotCmdIn(ctx, t.root, env, argv, nil, c.err, c.err)
		if err != nil {
			return err
		}
		if code != 0 {
			failed = append(failed, name)
		} else {
			passed = append(passed, name)
		}
		return nil
	}
	if err := step("build", build); err != nil {
		return 0, err
	}
	if vet != nil {
		if err := step("vet", vet); err != nil {
			return 0, err
		}
	}
	res, raw, tcode, err := runGoTests(ctx, t.root, env, tests)
	if err != nil {
		return 0, err
	}
	if len(tests) > 0 {
		if res != nil {
			_, _ = fmt.Fprint(c.out, res.renderSaving(raw))
		} else if tcode != 0 {
			_, _ = fmt.Fprintf(c.out, "the tests exited %d and no test results were recognized; last lines:\n%s\n", tcode, lastLines(raw, 20))
		}
		name, ok := "tests", "tests"
		if sel.compileOnly() {
			name, ok = "compile", "compile (only compiled, no test ran)"
		}
		if tcode != 0 || (res != nil && res.outcome() == runFail) {
			failed = append(failed, name)
		} else {
			passed = append(passed, ok)
		}
	}
	if res == nil {
		res = &testResult{}
	}
	if len(failed) == 0 {
		*verdict = strings.Join(passed, ", ") + "."
		rec.finish(res, 0)
		return 0, nil
	}
	*verdict = strings.Join(failed, ", ")
	if len(passed) > 0 {
		*verdict += "; passed: " + strings.Join(passed, ", ")
	}
	rec.finish(res, 1)
	return 1, nil
}

// runGoTests runs the go test commands one after the other, in -json mode,
// and returns the parsed result (nil when none was recognized), all the
// output and the first non-zero exit code.
func runGoTests(ctx context.Context, root string, env []string, cmds [][]string) (res *testResult, raw []byte, code int, err error) {
	var buf bytes.Buffer
	for _, argv := range cmds {
		c, err := runSlotCmdIn(ctx, root, env, goTestJSON(argv), nil, &buf, &buf)
		if err != nil {
			return nil, nil, 0, err
		}
		if code == 0 {
			code = c
		}
	}
	res, _, _ = parseTestOutput(buf.Bytes(), "go")
	return res, buf.Bytes(), code, nil
}

// withEnvVars is env without the variables named drop, then vars (NAME=value).
func withEnvVars(env, drop, vars []string) []string {
	out := make([]string, 0, len(env)+len(vars))
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		if !slices.Contains(drop, name) {
			out = append(out, e)
		}
	}
	return append(out, vars...)
}

// tailWriter keeps the end of what is written to it.
type tailWriter struct{ b []byte }

func (w *tailWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	if len(w.b) > testTailKeep {
		w.b = w.b[len(w.b)-testTailKeep:]
	}
	return len(p), nil
}

// lastLine is the last line with anything on it.
func (w *tailWriter) lastLine() string {
	lines := strings.Split(strings.TrimRight(string(w.b), "\n \t"), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return "no output"
}

// readHeavyTests is the packages the repository lists in .waypoint/heavy-tests
// (one directory a line, # comments) and the extra ones.
func readHeavyTests(root string, extra []string) []string {
	out := slices.Clone(extra)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(heavyTestsFile))) // #nosec G304 -- a fixed name in the checkout
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(data), "\n") {
		if l, _, _ = strings.Cut(l, "#"); strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

func cmdTestGo(c *cli, args []string) error {
	fs := c.flags()
	printOnly := fs.Bool("print", false, "print the go test commands and run nothing")
	var byFile multiFlag
	fs.Var(&byFile, "by-file", "run the package `PKG` (a directory) by file as a heavy package (repeatable)")
	rest, dashed, err := c.parseBeforeCommand(fs, args)
	if err != nil {
		if strings.Contains(err.Error(), "not defined") {
			return fmt.Errorf("%w; go test's own flags go after --, as wp test go -- -vet=off", err)
		}
		return err
	}
	if len(rest) > 0 && !dashed {
		return c.usage("unexpected %q: go test's own flags go after --, as wp test go -- -vet=off", rest[0])
	}
	root, err := localGit("rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return errors.New("wp test go runs inside a git repository: cd into one")
	}
	if !isGoModule(root) {
		return fmt.Errorf("there is no go.mod in %s, so there are no Go packages to select", root)
	}
	changed, err := changedForGo()
	if err != nil {
		return err
	}
	sel := selectGoTests(root, changed, readHeavyTests(root, byFile))
	cmds := sel.commands(rest)
	if l := sel.noTestsLine(); l != "" {
		c.println(l)
	}
	if len(cmds) == 0 {
		c.println("wp test go: no changed file belongs to a Go package (or is a testdata or embedded file of one), so no go test runs.")
		return nil
	}
	if *printOnly {
		for _, argv := range cmds {
			c.println(shellJoin(argv))
		}
		return nil
	}
	rec := newTestRunRecorder([]string{"wp", "test", "go"})
	rec.label = "wp test go" + scopedMark
	var dropped []string
	var res *testResult
	var raw []byte
	code, err := c.runInSlotDo("tests", defaultSlots(), slotRun{key: "wp test go", shown: "wp test go"}, slotRunCmdFor("wp test go"), rec.started, func(ctx context.Context, env []string) (int, error) {
		env, dropped = testEnv(env, nil)
		var code int
		var err error
		res, raw, code, err = runGoTests(ctx, root, env, cmds)
		return code, err
	})
	if err != nil {
		return err
	}
	if res == nil {
		if code != 0 {
			rec.failed(code)
			c.withheld(dropped)
			return c.emitUnparsed(raw, code)
		}
		return fmt.Errorf("no test results found in the go test output")
	}
	_, _ = fmt.Fprint(c.out, res.renderSaving(raw))
	rec.compileOnly = sel.compileOnly()
	rec.finish(res, code)
	if sel.compileOnly() && code == 0 && res.outcome() != runFail {
		c.println("wp test go: only compiled, no test ran; not recorded as a pass.")
	}
	if code != 0 || res.outcome() == runFail {
		c.withheld(dropped)
		return exitCode(1)
	}
	return nil
}

// changedForGo is the changed paths wp test handed down in WP_TEST_CHANGED,
// else the diff against the default base.
func changedForGo() ([]string, error) {
	if f := os.Getenv(envTestChanged); f != "" {
		data, err := os.ReadFile(f) // #nosec G304 G703 -- the file wp test wrote for its script
		if err != nil {
			return nil, fmt.Errorf("can't read %s=%s: %w", envTestChanged, f, err)
		}
		var out []string
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out, nil
	}
	_, _, changed, err := testChanges("")
	return changed, err
}
