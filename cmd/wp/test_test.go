package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scopedRepo is a repo on branch work, one commit past main, with a slot dir
// of its own and no slot held.
func scopedRepo(t *testing.T) string {
	t.Helper()
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	root := tempRepo(t)
	gitDo(t, "checkout", "-q", "-b", "work")
	return root
}

func putExec(t *testing.T, root, name, text string) {
	t.Helper()
	putFiles(t, root, map[string]string{name: text})
	if err := os.Chmod(filepath.Join(root, name), 0o755); err != nil { // #nosec G302 -- a script the test runs
		t.Fatal(err)
	}
}

func runWpTest(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	c := &cli{out: &out, err: &errb}
	err = c.dispatch(testCmd, args)
	return out.String(), errb.String(), err
}

func TestChangedFilesListsEveryKindOfChange(t *testing.T) {
	root := scopedRepo(t)
	putFiles(t, root, map[string]string{"gone.txt": "x\n", "moved.txt": "m\n", "keep.txt": "k\n"})
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "more")
	base := gitDo(t, "rev-parse", "HEAD")
	putFiles(t, root, map[string]string{"committed.txt": "c\n"})
	gitDo(t, "add", "committed.txt")
	gitDo(t, "commit", "-q", "-m", "committed")
	putFiles(t, root, map[string]string{"staged.txt": "s\n", "keep.txt": "k2\n", "untracked.txt": "u\n", "ignored": "i\n", "dir/with space.txt": "w\n"})
	gitDo(t, "add", "staged.txt")
	gitDo(t, "rm", "-q", "gone.txt")
	gitDo(t, "mv", "moved.txt", "renamed.txt")
	got, err := changedFiles(root, base)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"committed.txt", "dir/with space.txt", "gone.txt", "keep.txt", "moved.txt", "renamed.txt", "staged.txt", "untracked.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestWpTestNothingChangedRunsNothing(t *testing.T) {
	root := scopedRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("MARKER", marker)
	putExec(t, root, ".waypoint/test", "#!/bin/sh\ntouch \"$MARKER\"\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "script")
	gitDo(t, "update-ref", "refs/heads/main", "HEAD")
	out, _, err := runWpTest(t)
	if err != nil || !strings.Contains(out, "nothing changed since "+shortSHA(gitDo(t, "rev-parse", "HEAD"))) || strings.Count(out, "\n") != 1 {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the script ran with nothing changed")
	}
	// --all runs the whole check when nothing changed.
	if _, _, err := runWpTest(t, "--all"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("--all didn't run the script")
	}
}

func TestWpTestRunsTheRepositoryScript(t *testing.T) {
	root := scopedRepo(t)
	dump := t.TempDir()
	t.Setenv("DUMP", dump)
	t.Setenv("FAKE_API_TOKEN", "secret")
	putExec(t, root, ".waypoint/test", `#!/bin/sh
pwd > "$DUMP/pwd"
echo "$WP_TEST_BASE" > "$DUMP/base"
echo "${WP_TEST_ALL:-unset}" > "$DUMP/all"
cp "$WP_TEST_CHANGED" "$DUMP/changed"
echo "${FAKE_API_TOKEN:-dropped}" > "$DUMP/token"
echo "lint ok"
`)
	putFiles(t, root, map[string]string{"a.go": "package a\n", "docs/x.md": "x\n"})
	gitDo(t, "add", "-A")
	t.Chdir(filepath.Join(root, "docs"))
	out, errs, err := runWpTest(t)
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, errs)
	}
	read := func(n string) string {
		b, _ := os.ReadFile(filepath.Join(dump, n))
		return strings.TrimSpace(string(b))
	}
	if read("pwd") != root && read("pwd") != evalSym(t, root) {
		t.Errorf("the script ran in %s, not the repository root %s", read("pwd"), root)
	}
	if read("base") != gitDo(t, "rev-parse", "main") || read("all") != "unset" || read("token") != "dropped" {
		t.Errorf("base %q all %q token %q", read("base"), read("all"), read("token"))
	}
	if got := read("changed"); got != ".waypoint/test\na.go\ndocs/x.md" {
		t.Errorf("changed paths: %q", got)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "wp test: checking 3 changed files against "+shortSHA(read("base"))) || !strings.Contains(lines[0], ".waypoint/test") {
		t.Errorf("first line: %q", lines[0])
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "wp test: passed") || !strings.Contains(last, "CI runs the whole suite on the pull request.") {
		t.Errorf("last line: %q", last)
	}
	if !strings.Contains(errs, "lint ok") {
		t.Errorf("the script's output isn't shown: %q", errs)
	}
	runs := readTestRuns(testRunsPath())
	if len(runs) != 1 || runs[0].Command != "wp test (scoped check)" || runs[0].Outcome != runPass || runs[0].Tree != workingTree() {
		t.Errorf("recorded %+v", runs)
	}

	if _, _, err := runWpTest(t, "--all"); err != nil {
		t.Fatal(err)
	}
	if read("all") != "1" {
		t.Errorf("--all: WP_TEST_ALL is %q", read("all"))
	}
	runs = readTestRuns(testRunsPath())
	if len(runs) != 2 || runs[1].Command != "wp test --all" {
		t.Errorf("recorded %+v", runs)
	}
}

func evalSym(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestWpTestScriptFailureNamesWhatFailed(t *testing.T) {
	root := scopedRepo(t)
	putExec(t, root, ".waypoint/test", "#!/bin/sh\necho 'lint broke' >&2\necho 'make check: failed: lint; passed: vet'\nexit 3\n")
	out, errs, err := runWpTest(t)
	var ec exitCode
	if !errors.As(err, &ec) || ec != 1 {
		t.Fatalf("err %v", err)
	}
	last := strings.TrimSpace(out[strings.LastIndex(strings.TrimSpace(out), "\n")+1:])
	if !strings.HasPrefix(last, "wp test: failed:") || !strings.Contains(last, "exited 3") || !strings.Contains(last, "failed: lint; passed: vet") {
		t.Errorf("last line: %q", last)
	}
	if !strings.Contains(errs, "lint broke") {
		t.Errorf("stderr: %q", errs)
	}
	if runs := readTestRuns(testRunsPath()); len(runs) != 1 || runs[0].Outcome != runFail || runs[0].Exit != 3 {
		t.Errorf("recorded %+v", runs)
	}
}

func TestWpTestIgnoresAScriptThatIsNotExecutable(t *testing.T) {
	root := scopedRepo(t)
	putFiles(t, root, map[string]string{".waypoint/test": "#!/bin/sh\nexit 1\n", "x.txt": "x\n"})
	if testScript(root) != "" {
		t.Error("a file without the execute bit is the check")
	}
	_, errs, err := runWpTest(t)
	var ec exitCode
	if !errors.As(err, &ec) || ec != 2 {
		t.Fatalf("err %v", err)
	}
	for _, want := range []string{"no scoped check", ".waypoint/test", "WP_TEST_BASE", "WP_TEST_CHANGED", "WP_TEST_ALL=1", "chmod +x .waypoint/test"} {
		if !strings.Contains(errs, want) {
			t.Errorf("message lacks %q:\n%s", want, errs)
		}
	}
}

func TestWpTestDryRunRunsNothing(t *testing.T) {
	root := scopedRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("MARKER", marker)
	putExec(t, root, ".waypoint/test", "#!/bin/sh\ntouch \"$MARKER\"\n")
	out, _, err := runWpTest(t, "--dry-run")
	if err != nil || !strings.Contains(out, "1 changed file ") || !strings.Contains(out, "Would run .waypoint/test") || !strings.Contains(out, "WP_TEST_BASE="+gitDo(t, "rev-parse", "main")) {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("--dry-run ran the script")
	}
	if runs := readTestRuns(testRunsPath()); len(runs) != 0 {
		t.Errorf("--dry-run recorded %+v", runs)
	}
}

func TestWpTestBuiltInGoCheck(t *testing.T) {
	root := scopedRepo(t)
	putFiles(t, root, map[string]string{
		"go.mod":                "module example.com/m\n\ngo 1.21\n",
		"p/p.go":                "package p\n\nfunc One() int { return 1 }\n",
		"p/p_test.go":           "package p\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {\n\tif One() != 1 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n",
		"q/q.go":                "package q\n",
		"q/q_test.go":           "package q\n\nimport \"testing\"\n\nfunc TestQ(t *testing.T) { t.Fatal(\"broken\") }\n",
		".waypoint/heavy-tests": "# heavy\nq\n",
	})
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "module")
	gitDo(t, "update-ref", "refs/heads/main", "HEAD")
	putFiles(t, root, map[string]string{"p/p.go": "package p\n\nfunc One() int { return 1 }\n\nfunc Two() int { return 2 }\n"})
	out, _, err := runWpTest(t)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "built-in Go check") || !strings.Contains(out, "PASS: 1 tests") || !strings.Contains(out, "wp test: passed: build, vet, tests.") {
		t.Errorf("output:\n%s", out)
	}
	// q is heavy, and only its test file changed: that test runs, and fails.
	putFiles(t, root, map[string]string{"q/q_test.go": "package q\n\nimport \"testing\"\n\nfunc TestQ(t *testing.T) { t.Fatal(\"still broken\") }\n"})
	out, _, err = runWpTest(t)
	var ec exitCode
	if !errors.As(err, &ec) || ec != 1 || !strings.Contains(out, "FAIL: 1 failed") || !strings.Contains(out, "wp test: failed: tests; passed: build, vet") {
		t.Errorf("%v\n%s", err, out)
	}
	if runs := readTestRuns(testRunsPath()); len(runs) != 2 || runs[0].Command != "wp test (scoped check)" || runs[1].Outcome != runFail {
		t.Errorf("recorded %+v", runs)
	}
}

func TestWpTestGoSelectsFromTheChangedList(t *testing.T) {
	root := scopedRepo(t)
	putFiles(t, root, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.21\n", "h/h.go": "package h\n", "h/h_test.go": "package h\n\nimport \"testing\"\n\nfunc TestH(t *testing.T) {}\n",
		"h/other.go": "package h\n", "w/w.go": "package w\n", ".waypoint/heavy-tests": "h\n",
	})
	list := filepath.Join(t.TempDir(), "changed")
	if err := os.WriteFile(list, []byte("h/h.go\nh/other.go\nw/w.go\nREADME.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envTestChanged, list)
	var out, errb bytes.Buffer
	c := &cli{out: &out, err: &errb}
	if err := c.dispatch(testCmd, []string{"go", "--print", "--", "-vet=off"}); err != nil {
		t.Fatal(err)
	}
	want := "No test sits beside: h/other.go (heavy package: only tests in changed test files and beside changed files run).\n" +
		"go test -count=1 -vet=off ./w\n" +
		"go test -count=1 -vet=off -run '^(TestH)$' ./h\n"
	if out.String() != want {
		t.Errorf("got:\n%swant:\n%s", out.String(), want)
	}
	out.Reset()
	if err := c.dispatch(testCmd, []string{"go", "--by-file", "w", "--print"}); err != nil || !strings.Contains(out.String(), "w/w.go") {
		t.Errorf("--by-file: %v\n%s", err, out.String())
	}
	if err := os.WriteFile(list, []byte("README.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := c.dispatch(testCmd, []string{"go", "--print"}); err != nil || !strings.Contains(out.String(), "no changed file belongs to a Go package") {
		t.Errorf("nothing selected: %v\n%s", err, out.String())
	}
	if err := c.dispatch(testCmd, []string{"go", "-vet=off"}); err == nil || !strings.Contains(err.Error(), "after --") {
		t.Errorf("flags without --: %v", err)
	}
}

// A heavy package whose changed file has no test beside it is compiled: a
// file that doesn't build fails, and a compile that passes is not kept as the
// tests passing.
func TestWpTestCompilesAHeavyPackageWithNoTestBeside(t *testing.T) {
	root := scopedRepo(t)
	putFiles(t, root, map[string]string{
		"go.mod":                "module example.com/m\n\ngo 1.21\n",
		"q/q.go":                "package q\n",
		"q/q_test.go":           "package q\n\nimport \"testing\"\n\nfunc TestQ(t *testing.T) {}\n",
		".waypoint/heavy-tests": "q\n",
	})
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "module")
	gitDo(t, "update-ref", "refs/heads/main", "HEAD")
	putFiles(t, root, map[string]string{"q/new.go": "package q\n\nfunc New() int { return 1 }\n"})
	out, _, err := runWpTest(t)
	if err != nil || !strings.Contains(out, "compile (only compiled, no test ran)") || !strings.Contains(out, "No test sits beside: q/new.go") {
		t.Fatalf("%v\n%s", err, out)
	}
	if runs := readTestRuns(testRunsPath()); len(runs) != 0 {
		t.Errorf("a compile-only pass was recorded: %+v", runs)
	}
	putFiles(t, root, map[string]string{"q/new.go": "package q\n\nfunc New() int { return undefinedName }\n"})
	out, _, err = runWpTest(t)
	var ec exitCode
	if !errors.As(err, &ec) || ec != 1 || !strings.Contains(out, "wp test: failed:") {
		t.Fatalf("a file that doesn't build passed: %v\n%s", err, out)
	}
	if runs := readTestRuns(testRunsPath()); len(runs) == 0 || runs[len(runs)-1].Outcome != runFail {
		t.Errorf("the failure was not recorded: %+v", runs)
	}
}

// wp test and wp test go keep the full output of a failure they cut.
func TestWpTestKeepsTheFullOutputOfALongFailure(t *testing.T) {
	root := scopedRepo(t)
	t.Setenv("WAYPOINT_TESTOUT_DIR", t.TempDir())
	var b strings.Builder
	b.WriteString("package q\n\nimport \"testing\"\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "func TestF%02d(t *testing.T) { t.Fatal(\"boom %02d\") }\n", i, i)
	}
	putFiles(t, root, map[string]string{"go.mod": "module example.com/m\n\ngo 1.21\n", "q/q.go": "package q\n", "q/q_test.go": b.String()})
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "module")
	gitDo(t, "update-ref", "refs/heads/main", "HEAD")
	putFiles(t, root, map[string]string{"q/q.go": "package q\n\nvar X = 1\n"})
	out, _, err := runWpTest(t)
	if err == nil || !strings.Contains(out, "Full output: ") || !strings.Contains(out, " lines)") {
		t.Errorf("wp test: %v\n%s", err, out)
	}
	out, _, err = runWpTest(t, "go")
	if err == nil || !strings.Contains(out, "Full output: ") {
		t.Errorf("wp test go: %v\n%s", err, out)
	}
}

// wp ship runs wp test by default in a repository that has a .waypoint/test:
// once per tree, again after the tree changes, and not at all without one.
func TestShipRunsTheScopedCheckOncePerTree(t *testing.T) {
	root := scopedRepo(t)
	runs := 0
	old := shipScopedCheck
	t.Cleanup(func() { shipScopedCheck = old })
	shipScopedCheck = func(c *cli) (int, error) {
		runs++
		err := c.dispatch(testCmd, nil)
		var ec exitCode
		if errors.As(err, &ec) {
			return int(ec), nil
		}
		return 0, err
	}
	var out bytes.Buffer
	c := &cli{out: &out, err: &bytes.Buffer{}}

	putFiles(t, root, map[string]string{"a.txt": "a\n"})
	if err := c.runDefaultShipCheck(); err != nil || runs != 0 {
		t.Fatalf("no .waypoint/test: err %v, runs %d", err, runs)
	}

	putExec(t, root, ".waypoint/test", "#!/bin/sh\nexit 0\n")
	if err := c.runDefaultShipCheck(); err != nil || runs != 1 {
		t.Fatalf("first ship: err %v, runs %d", err, runs)
	}
	if err := c.runDefaultShipCheck(); err != nil || runs != 1 || !strings.Contains(out.String(), "already passed on this tree; not run again") {
		t.Fatalf("second ship on one tree: err %v, runs %d\n%s", err, runs, out.String())
	}
	putFiles(t, root, map[string]string{"b.txt": "b\n"})
	if err := c.runDefaultShipCheck(); err != nil || runs != 2 {
		t.Fatalf("after the tree changed: err %v, runs %d", err, runs)
	}

	putExec(t, root, ".waypoint/test", "#!/bin/sh\nexit 1\n")
	err := c.runDefaultShipCheck()
	if err == nil || runs != 3 || !strings.Contains(err.Error(), "--no-check") {
		t.Fatalf("a failing check: err %v, runs %d", err, runs)
	}
}
