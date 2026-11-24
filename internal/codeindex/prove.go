package codeindex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// A fix is proven only when its tests fail before it and pass after; a test that doesn't build without the fix proves nothing.

// Proof outcomes.
const (
	ProofProven      = "proven"
	ProofPasses      = "passes"
	ProofNoBuild     = "no_build"
	ProofHeadFails   = "head_fails"
	ProofNoTest      = "no_test"
	ProofUnsupported = "unsupported"
	ProofNone        = "none"
)

// What running the proving tests on the blamed change said.
const (
	IntroConfirmed = "confirmed"
	IntroPasses    = "passes_at_intro"
	IntroBefore    = "fails_before"
	IntroNoBuild   = "no_build"
)

const (
	maxProvePackages = 6
	maxProveTests    = 40
	proveTestTimeout = 10 * time.Minute
	proveOutputTail  = 600
	maxProofNote     = 1900
)

// ProveInput says which fix to prove.
type ProveInput struct {
	Fix   FixInput
	Intro string // the change the trace blamed most; "" for none
}

// FixProof is what proving one fix found.
type FixProof struct {
	Commit     string   `json:"commit"` // the fix
	Base       string   `json:"base"`   // the code before it
	Outcome    string   `json:"outcome"`
	Tests      []string `json:"tests"`  // dir.TestName, the tests run
	Failed     []string `json:"failed"` // of those, the ones that failed without the fix, twice
	Intro      string   `json:"intro,omitempty"`
	IntroCheck string   `json:"intro_check,omitempty"`
	Note       string   `json:"note,omitempty"`
	Seconds    int      `json:"seconds"`
}

// goPkgTests is one package's tests to run, by its directory in the repo.
type goPkgTests struct {
	dir   string
	tests []string
}

// testID names a test as dir.TestName, or TestName in the root package.
func testID(dir, test string) string {
	if dir == "." {
		return test
	}
	return dir + "." + test
}

// ProveFix proves one fix in the checkout at root, building trees in a
// temporary directory (the checkout and its index aren't touched). An error
// means nothing should be recorded: the next round tries again.
func ProveFix(ctx context.Context, root string, in ProveInput) (*FixProof, error) {
	start := time.Now()
	r := &fixRepo{root: root, chains: map[string]map[string]bool{}, sizes: map[string]int{}}
	if _, err := r.git("rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("not a git checkout: %w", err)
	}
	res := &FixProof{Outcome: ProofNone, Tests: []string{}, Failed: []string{}}
	defer func() { res.Seconds = int(time.Since(start).Seconds()) }()
	from, to, _, note, err := r.span(in.Fix)
	if err != nil {
		return nil, err
	}
	if note != "" {
		// Recording "none" would end this fix's proofs for good, so return the error instead.
		return nil, fmt.Errorf("can't place the fix: %s", note)
	}
	res.Commit, res.Base = to, from
	pkgs, outcome, note, err := r.provingTests(from, to)
	if err != nil {
		return nil, err
	}
	if outcome != "" {
		res.Outcome, res.Note = outcome, note
		return res, nil
	}
	notesText := func(notes []string) string {
		if note != "" {
			notes = append(slices.Clone(notes), note)
		}
		return lastText(strings.Join(notes, "; "), maxProofNote)
	}
	tmp, err := os.MkdirTemp("", "wp-prove-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	run := 0
	tree := func(rev string, overlay []goPkgTests) (string, error) {
		run++
		dir := filepath.Join(tmp, strconv.Itoa(run))
		if err := r.extract(rev, dir, tmp); err != nil {
			return "", err
		}
		for _, p := range pkgs {
			if err := realDirs(dir, p.dir); err != nil {
				return "", err
			}
		}
		for _, p := range overlay {
			if err := r.overlayTests(to, p.dir, dir); err != nil {
				return "", err
			}
		}
		return dir, nil
	}

	head, err := tree(to, nil)
	if err != nil {
		return nil, err
	}
	var passing []goPkgTests
	headNotes := []string{}
	for _, p := range pkgs {
		for _, t := range p.tests {
			res.Tests = append(res.Tests, testID(p.dir, t))
		}
		out, err := goTest(ctx, filepath.Join(head, filepath.FromSlash(p.dir)), p.tests)
		if err != nil {
			return nil, err
		}
		if !out.built {
			headNotes = append(headNotes, fmt.Sprintf("%s doesn't build on the fix: %s", p.dir, out.tail))
			continue
		}
		if out.crashed {
			headNotes = append(headNotes, fmt.Sprintf("%s's tests fail before running on the fix: %s", p.dir, out.tail))
			continue
		}
		var ok []string
		for _, t := range p.tests {
			switch {
			case out.passed[t]:
				ok = append(ok, t)
			case out.failed[t]:
				headNotes = append(headNotes, fmt.Sprintf("%s.%s fails on the fix", p.dir, t))
			}
		}
		if len(ok) > 0 {
			passing = append(passing, goPkgTests{dir: p.dir, tests: ok})
		}
	}
	if len(passing) == 0 {
		res.Outcome = ProofHeadFails
		if len(headNotes) == 0 {
			headNotes = append(headNotes, "none of the tests ran on the fix")
		}
		res.Note = notesText(headNotes)
		return res, nil
	}

	base, err := tree(from, passing)
	if err != nil {
		return nil, err
	}
	built := false
	var proving []goPkgTests
	var baseNotes []string
	for _, p := range passing {
		dir := filepath.Join(base, filepath.FromSlash(p.dir))
		out, err := goTest(ctx, dir, p.tests)
		if err != nil {
			return nil, err
		}
		if !out.built {
			baseNotes = append(baseNotes, fmt.Sprintf("%s doesn't build without the fix: %s", p.dir, out.tail))
			continue
		}
		built = true
		var failed []string
		for _, t := range p.tests {
			if out.failed[t] {
				failed = append(failed, t)
			}
		}
		if len(failed) == 0 {
			continue
		}
		again, err := goTest(ctx, dir, failed)
		if err != nil {
			return nil, err
		}
		var still []string
		for _, t := range failed {
			if again.failed[t] {
				still = append(still, t)
				res.Failed = append(res.Failed, testID(p.dir, t))
			}
		}
		if len(still) < len(failed) {
			baseNotes = append(baseNotes, fmt.Sprintf("%d of %s's tests failed without the fix only once", len(failed)-len(still), p.dir))
		}
		if again.crashed {
			baseNotes = append(baseNotes, fmt.Sprintf("%s's tests fail before running without the fix: %s", p.dir, again.tail))
		}
		if len(still) > 0 {
			proving = append(proving, goPkgTests{dir: p.dir, tests: still})
		}
	}
	switch {
	case len(proving) > 0:
		res.Outcome = ProofProven
	case built:
		res.Outcome = ProofPasses
	default:
		res.Outcome = ProofNoBuild
	}
	res.Note = notesText(append(headNotes, baseNotes...))
	if res.Outcome != ProofProven || in.Intro == "" || !r.exists(in.Intro) {
		return res, nil
	}
	res.Intro = in.Intro
	res.IntroCheck, err = r.checkIntro(ctx, in.Intro, proving, tree)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// checkIntro runs the proving tests on the blamed change and, if they fail
// there, on the commit before it.
func (r *fixRepo) checkIntro(ctx context.Context, intro string, proving []goPkgTests, tree func(string, []goPkgTests) (string, error)) (string, error) {
	fails := func(rev string) (bool, bool, error) {
		dir, err := tree(rev, proving)
		if err != nil {
			return false, false, err
		}
		anyBuilt, anyFailed := false, false
		for _, p := range proving {
			out, err := goTest(ctx, filepath.Join(dir, filepath.FromSlash(p.dir)), p.tests)
			if err != nil {
				return false, false, err
			}
			if !out.built {
				continue
			}
			anyBuilt = true
			var failed []string
			for _, t := range p.tests {
				if out.failed[t] {
					failed = append(failed, t)
				}
			}
			if len(failed) == 0 {
				continue
			}
			again, err := goTest(ctx, filepath.Join(dir, filepath.FromSlash(p.dir)), failed)
			if err != nil {
				return false, false, err
			}
			for _, t := range failed {
				if again.failed[t] {
					anyFailed = true
				}
			}
		}
		return anyBuilt, anyFailed, nil
	}
	built, failed, err := fails(intro)
	switch {
	case err != nil:
		return "", err
	case !built:
		return IntroNoBuild, nil
	case !failed:
		return IntroPasses, nil
	}
	parent, err := r.rev(intro + "^1")
	if err != nil {
		return IntroNoBuild, nil
	}
	built, failed, err = fails(parent)
	switch {
	case err != nil:
		return "", err
	case !built:
		return IntroNoBuild, nil
	case failed:
		return IntroBefore, nil
	}
	return IntroConfirmed, nil
}

// provingTests picks the Go tests the fix adds or changes, by package. An
// outcome instead says there is nothing to run. Past the limits, packages
// whose code the fix also changed come first, and the note says what was
// left out: a proof over part of the tests must say so.
func (r *fixRepo) provingTests(from, to string) ([]goPkgTests, string, string, error) {
	out, err := r.git("diff", "--name-status", "--no-renames", "-z", from, to)
	if err != nil {
		return nil, "", "", fmt.Errorf("git diff: %w", err)
	}
	f := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var goTests, otherTests []string
	codeDirs := map[string]bool{}
	for i := 0; i+1 < len(f); i += 2 {
		status, p := f[i], f[i+1]
		switch {
		case !isTestPath(p) && !strings.Contains(p, "testdata/"):
			if !isDocPath(p) {
				codeDirs[path.Dir(p)] = true
			}
		case strings.HasPrefix(status, "D"):
		case strings.HasSuffix(p, "_test.go"):
			goTests = append(goTests, p)
		case isTestPath(p):
			otherTests = append(otherTests, p)
		}
	}
	switch {
	case len(codeDirs) == 0:
		return nil, ProofNone, "the fix changes only tests or docs", nil
	case len(goTests) == 0 && len(otherTests) > 0:
		return nil, ProofUnsupported, "only Go tests are run so far; the fix's tests are in " + otherTests[0], nil
	case len(goTests) == 0:
		return nil, ProofNoTest, "", nil
	}
	added, err := r.addedLines(from, to, goTests)
	if err != nil {
		return nil, "", "", err
	}
	byDir := map[string][]string{}
	for _, p := range goTests {
		src, err := r.git("show", to+":"+p)
		if err != nil {
			return nil, "", "", fmt.Errorf("git show %s: %w", p, err)
		}
		d := path.Dir(p)
		for _, t := range touchedTests(src, added[p]) {
			if !slices.Contains(byDir[d], t) {
				byDir[d] = append(byDir[d], t)
			}
		}
	}
	dirs := slices.Collect(maps.Keys(byDir))
	slices.SortFunc(dirs, func(a, b string) int {
		if codeDirs[a] != codeDirs[b] {
			if codeDirs[a] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	var pkgs []goPkgTests
	var cut []string
	for _, d := range dirs {
		ts := byDir[d]
		if len(ts) == 0 {
			continue
		}
		if len(pkgs) == maxProvePackages {
			cut = append(cut, fmt.Sprintf("%s (%d)", d, len(ts)))
			continue
		}
		slices.Sort(ts)
		if len(ts) > maxProveTests {
			cut = append(cut, fmt.Sprintf("%d more in %s", len(ts)-maxProveTests, d))
			ts = ts[:maxProveTests]
		}
		pkgs = append(pkgs, goPkgTests{dir: d, tests: ts})
	}
	if len(pkgs) == 0 {
		return nil, ProofNoTest, "its test files have no Test functions", nil
	}
	note := ""
	if len(cut) > 0 {
		note = "only part of the fix's tests ran; left out: " + strings.Join(cut, ", ")
	}
	return pkgs, "", note, nil
}

var newHunkRE = lazyre.New(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// addedLines is, per file, the line numbers on the fix's side that it added
// or changed.
func (r *fixRepo) addedLines(from, to string, files []string) (map[string][]int, error) {
	out, err := r.git(append([]string{"diff", "-U0", "--no-color", "--no-ext-diff", "--no-renames", from, to, "--"}, files...)...)
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	res := map[string][]int{}
	cur := ""
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ "):
			cur = diffPath(l[4:])
		case strings.HasPrefix(l, "@@ ") && cur != "":
			m := newHunkRE.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			for i := 0; i < n; i++ {
				res[cur] = append(res[cur], start+i)
			}
			if n == 0 { // only removed lines: they were between start and start+1
				res[cur] = append(res[cur], start, start+1)
			}
		}
	}
	return res, nil
}

// isGoTestName says name is a test go test runs: Test, or Test followed by
// anything but a lower-case letter. TestMain is the package's setup, not a
// test.
func isGoTestName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Test")
	if !ok || rest == "Main" {
		return false
	}
	if rest == "" {
		return true
	}
	c, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(c)
}

// touchedTests are the test functions in src that hold a changed line. When
// none does (only a helper changed), every test in the file counts.
func touchedTests(src string, changed []int) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x_test.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var all, touched []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !isGoTestName(fn.Name.Name) {
			continue
		}
		all = append(all, fn.Name.Name)
		lo, hi := fset.Position(fn.Pos()).Line, fset.Position(fn.End()).Line
		if fn.Doc != nil {
			lo = fset.Position(fn.Doc.Pos()).Line
		}
		for _, l := range changed {
			if l >= lo && l <= hi {
				touched = append(touched, fn.Name.Name)
				break
			}
		}
	}
	if len(touched) == 0 {
		return all
	}
	return touched
}

// extract writes rev's tree into dir through a private index, leaving the
// checkout's own index and work tree alone.
func (r *fixRepo) extract(rev, dir, tmp string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	env := append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(tmp, "index"))
	for _, args := range [][]string{
		{"read-tree", rev},
		{"--work-tree=" + dir, "checkout-index", "--all", "--force"},
	} {
		cmd := exec.Command("git", args...) // #nosec G204 -- fixed git subcommands; rev is a commit id and dir our temp directory
		cmd.Dir, cmd.Env = r.root, env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// realDirs makes each directory on rel's path under root a real one. A
// symlink there (git allows one) becomes an empty directory, so the tests
// that run, and the files the overlay writes and removes, stay in the tree
// and out of the checkout or anywhere else it points.
func realDirs(root, rel string) error {
	cur := root
	for _, part := range strings.Split(rel, "/") {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case fi.IsDir():
			continue
		}
		if err := os.Remove(cur); err != nil {
			return err
		}
		if err := os.Mkdir(cur, 0o750); err != nil {
			return err
		}
	}
	return nil
}

// overlayTests makes pkg's test files and testdata in dir the fix's. The
// package's path is a real directory (realDirs).
func (r *fixRepo) overlayTests(fix, pkg, dir string) error {
	full := filepath.Join(dir, filepath.FromSlash(pkg))
	if ents, err := os.ReadDir(full); err == nil {
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
				if err := os.Remove(filepath.Join(full, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(full, "testdata")); err != nil {
		return err
	}
	prefix := pkg + "/"
	if pkg == "." {
		prefix = ""
	}
	pathspec := prefix
	if pathspec == "" {
		pathspec = "." // git refuses an empty pathspec
	}
	out, err := r.git("ls-tree", "-r", "-z", "--full-tree", fix, "--", pathspec)
	if err != nil {
		return fmt.Errorf("git ls-tree: %w", err)
	}
	for _, ent := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		meta, p, ok := strings.Cut(ent, "\t")
		if !ok || !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		test := !strings.Contains(rest, "/") && strings.HasSuffix(rest, "_test.go")
		if !test && !strings.HasPrefix(rest, "testdata/") {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" || fields[0] == "120000" {
			continue
		}
		body, err := r.git("cat-file", "blob", fields[2])
		if err != nil {
			return fmt.Errorf("git cat-file %s: %w", p, err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// goTestResult is one go test run's per-test outcome.
type goTestResult struct {
	built          bool
	passed, failed map[string]bool
	crashed        bool
	tail           string
}

// goTestEnv is the environment the tests get: the runner's own, without
// Waypoint's settings (a token has no business in a repo's tests).
func goTestEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "WAYPOINT_") {
			env = append(env, e)
		}
	}
	return env
}

// goTest runs the named top-level tests of the package in dir. A test that
// fails, panics or times out is failed, and so is every test of a binary
// that fails before running one; a package that doesn't compile (or isn't
// there) didn't build. Only a missing go or a cancelled context is an error.
func goTest(ctx context.Context, dir string, tests []string) (*goTestResult, error) {
	res := &goTestResult{passed: map[string]bool{}, failed: map[string]bool{}}
	if _, err := os.Stat(dir); err != nil {
		res.tail = "the package isn't there"
		return res, nil
	}
	quoted := make([]string, len(tests))
	for i, t := range tests {
		quoted[i] = regexp.QuoteMeta(t)
	}
	ctx, cancel := context.WithTimeout(ctx, proveTestTimeout+time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-json", "-timeout", proveTestTimeout.String(), "-run", "^("+strings.Join(quoted, "|")+")$", ".") // #nosec G204 -- go test in our temp tree; the pattern is quoted test names
	cmd.Dir, cmd.Env = dir, goTestEnv()
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil, fmt.Errorf("go test: %w", err)
	}
	ran, pkgFailed := false, false
	brokenBuild := false
	var text strings.Builder
	for _, line := range strings.Split(stdout.String(), "\n") {
		var ev struct{ Action, Test, Output string }
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Action == "build-fail" {
			brokenBuild = true
		}
		if ev.Output != "" {
			text.WriteString(ev.Output)
			if strings.Contains(ev.Output, "[build failed]") || strings.Contains(ev.Output, "[setup failed]") {
				brokenBuild = true
			}
		}
		if ev.Test == "" && ev.Action == "fail" {
			pkgFailed = true
		}
		if ev.Test == "" || strings.Contains(ev.Test, "/") {
			continue
		}
		switch ev.Action {
		case "run":
			ran = true
		case "pass":
			res.passed[ev.Test] = true
		case "fail":
			res.failed[ev.Test] = true
		}
	}
	res.built = !brokenBuild && (ran || err == nil || pkgFailed)
	res.crashed = res.built && !ran && err != nil
	if res.crashed {
		for _, t := range tests {
			res.failed[t] = true
		}
	}
	if !res.built || res.crashed {
		text.WriteString(stderr.String())
		res.tail = lastText(text.String(), proveOutputTail)
	}
	return res, nil
}

// lastText is the end of s on one line.
func lastText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = "…" + strings.ToValidUTF8(s[len(s)-n:], "")
	}
	return s
}
