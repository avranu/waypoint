package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goJSONPass = `{"Action":"run","Package":"x/a","Test":"TestA"}
{"Action":"pass","Package":"x/a","Test":"TestA","Elapsed":0.01}
{"Action":"pass","Package":"x/a","Test":"TestB","Elapsed":0}
{"Action":"skip","Package":"x/a","Test":"TestC","Elapsed":0}
{"Action":"pass","Package":"x/a","Elapsed":1.5}
{"Action":"pass","Package":"x/b","Test":"TestD","Elapsed":0}
{"Action":"pass","Package":"x/b","Elapsed":0.5}
`

const goJSONFail = `{"Action":"output","Package":"x/a","Test":"TestBad","Output":"=== RUN   TestBad\n"}
{"Action":"output","Package":"x/a","Test":"TestBad","Output":"    bad_test.go:42: got 1, want 2\n"}
{"Action":"output","Package":"x/a","Test":"TestBad","Output":"    second line\n"}
{"Action":"output","Package":"x/a","Test":"TestBad","Output":"--- FAIL: TestBad (0.00s)\n"}
{"Action":"fail","Package":"x/a","Test":"TestBad","Elapsed":0}
{"Action":"pass","Package":"x/a","Test":"TestGood","Elapsed":0}
{"Action":"fail","Package":"x/a","Elapsed":2}
`

// go test's -timeout panic lands in the test that was running, which never
// gets a fail of its own (WP-883).
const goJSONTimeout = `{"Action":"pass","Package":"x/a","Test":"TestA","Elapsed":0.01}
{"Action":"run","Package":"x/core","Test":"TestQuick"}
{"Action":"output","Package":"x/core","Test":"TestQuick","Output":"=== RUN   TestQuick\n"}
{"Action":"output","Package":"x/core","Test":"TestQuick","Output":"panic: test timed out after 10m0s\n"}
{"Action":"output","Package":"x/core","Test":"TestQuick","Output":"\trunning tests:\n"}
{"Action":"output","Package":"x/core","Test":"TestQuick","Output":"\t\tTestQuick (3s)\n"}
{"Action":"output","Package":"x/core","Test":"TestQuick","Output":"\n"}
{"Action":"output","Package":"x/core","Test":"TestQuick","Output":"goroutine 17 [running]:\n"}
{"Action":"output","Package":"x/core","Output":"FAIL\tx/core\t600.1s\n"}
{"Action":"fail","Package":"x/core","Elapsed":600.1}
{"Action":"pass","Package":"x/a","Elapsed":1}
`

const goTextTimeout = `--- FAIL: TestEarlier (0.00s)
    e_test.go:3: wrong
panic: test timed out after 10m0s
	running tests:
		TestHang (9m58s)
		TestOther (2s)

goroutine 17 [running]:
FAIL	x/core	600.2s
ok  	x/a	1.0s
`

const goTextPass = `ok  	x/a	1.500s
ok  	x/b	0.500s
?   	x/c	[no test files]
`

const goTextFail = `--- FAIL: TestBad (0.00s)
    bad_test.go:42: got 1, want 2
    more detail
--- PASS: TestGood (0.00s)
FAIL
FAIL	x/a	0.30s
`

const pyPass = `============================= test session starts ==============================
tests/test_a.py::test_one PASSED
tests/test_b.py::test_two PASSED
============================== 2 passed in 0.12s ===============================
`

const pyFail = `============================= test session starts ==============================
tests/test_a.py .F                                                       [100%]
=================================== FAILURES ===================================
___________________________________ test_two ___________________________________

    def test_two():
>       assert 1 == 2
E       assert 1 == 2
E        +  where 1 = f()

tests/test_a.py:7: AssertionError
=========================== short test summary info ============================
FAILED tests/test_a.py::test_two - assert 1 == 2
========================= 1 failed, 1 passed in 0.30s ==========================
`

const pyQuiet = `.F                                                                       [100%]
FAILED tests/test_a.py::test_two - assert 1 == 2
1 failed, 1 passed in 0.30s
`

const utFail = `.FEs
======================================================================
ERROR: test_err (tests.test_demo.TestDemo.test_err)
----------------------------------------------------------------------
Traceback (most recent call last):
  File "/srv/repo/tests/test_demo.py", line 7, in test_err
    undefined_name()
    ^^^^^^^^^^^^^^
NameError: name 'undefined_name' is not defined

======================================================================
FAIL: test_fail (tests.test_demo.TestDemo)
A docstring's first line sits above the dashes.
----------------------------------------------------------------------
Traceback (most recent call last):
  File "tests/helpers.py", line 3, in check
    raise ValueError("x")
  File "tests/test_demo.py", line 5, in test_fail
    self.assertEqual({"a": 1}, {"a": 2})
AssertionError: {'a': 1} != {'a': 2}
- {'a': 1}
?       ^

----------------------------------------------------------------------
Ran 4 tests in 0.002s

FAILED (failures=1, errors=1, skipped=1)
E
======================================================================
ERROR: tests.test_web (unittest.loader._FailedTest.tests.test_web)
----------------------------------------------------------------------
ImportError: Failed to import test module: tests.test_web
Traceback (most recent call last):
  File "/srv/repo/app/auth.py", line 56, in <module>
    import flask
ModuleNotFoundError: No module named 'flask'


----------------------------------------------------------------------
Ran 1 test in 0.000s

FAILED (errors=1)
`

// Two discover runs in one command add up.
const utPass = `test_a (tests.test_x.T.test_a) ... ok
test_b (tests.test_x.T.test_b) ... skipped 'x'

----------------------------------------------------------------------
Ran 2 tests in 0.500s

OK (skipped=1)
...
----------------------------------------------------------------------
Ran 3 tests in 0.250s

OK
`

const junitPass = `<?xml version="1.0"?>
<testsuites><testsuite name="s1" tests="2"><testcase classname="a" name="t1" time="0.5"/><testcase classname="a" name="t2" time="0.25"/></testsuite></testsuites>`

const junitFail = `<testsuite name="s1"><testcase classname="a.B" name="t1" time="0.5" file="a/b.py" line="12"><failure message="boom">line1
line2</failure></testcase><testcase classname="a.B" name="t2" time="1"/></testsuite>`

func TestTestsumFormats(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "https://waypoint.example.com")
	tests := []struct {
		name, format, in string
		code             int
		want             []string
		exact            string
	}{
		{"go json pass", "auto", goJSONPass, 0, nil, "PASS: 4 tests in 2.0s (2 packages)\n"},
		{"go json fail", "go", goJSONFail, 1, []string{"FAIL: 1 failed", "--- x/a.TestBad", "at bad_test.go:42", "got 1, want 2", "second line"}, ""},
		{"go json timeout", "go", goJSONTimeout, 1, []string{"--- x/core (timed out after 10m0s)", "time limit (-timeout 10m0s)", "still running: TestQuick (3s)", "no test ran long", "-timeout 20m0s"}, ""},
		{"go text timeout", "go", goTextTimeout, 1, []string{"--- TestEarlier", "--- x/core (timed out after 10m0s)", "still running: TestHang (9m58s), TestOther (2s)", "TestHang ran for 9m58s of it: it may hang"}, ""},
		{"go text pass", "auto", goTextPass, 0, nil, "PASS: 2 packages in 2.0s\n"},
		{"go text fail", "go", goTextFail, 1, []string{"--- TestBad", "at bad_test.go:42", "more detail"}, ""},
		{"pytest pass", "auto", pyPass, 0, nil, "PASS: 2 tests in 0.1s (2 files)\n"},
		{"pytest fail", "pytest", pyFail, 1, []string{"--- tests/test_a.py::test_two", "at tests/test_a.py:7", "assert 1 == 2", "where 1 = f()"}, ""},
		{"pytest quiet fail", "auto", pyQuiet, 1, []string{"--- tests/test_a.py::test_two", "assert 1 == 2"}, ""},
		{"unittest fail", "auto", utFail, 1, []string{"FAIL: 3 failed, 1 passed, 1 skipped", "--- tests.test_web (didn't load)", "at /srv/repo/app/auth.py:56", "No module named 'flask'", "--- tests.test_demo.TestDemo.test_err", "at /srv/repo/tests/test_demo.py:7", "NameError: name", "--- tests.test_demo.TestDemo.test_fail", "at tests/test_demo.py:5", "AssertionError: {'a': 1} != {'a': 2}", "- {'a': 1}"}, ""},
		{"unittest pass", "auto", utPass, 0, nil, "PASS: 5 tests in 0.8s\n"},
		{"junit pass", "auto", junitPass, 0, nil, "PASS: 2 tests in 0.8s (1 suites)\n"},
		{"junit fail", "junit", junitFail, 1, []string{"--- a.B.t1", "at a/b.py:12", "boom", "line2"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "out")
			if err := os.WriteFile(p, []byte(tc.in), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			code := run([]string{"testsum", "--format", tc.format, p}, &out, &errb)
			if code != tc.code {
				t.Fatalf("exit %d, want %d: %s %s", code, tc.code, out.String(), errb.String())
			}
			if tc.exact != "" && out.String() != tc.exact {
				t.Errorf("got %q, want %q", out.String(), tc.exact)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
		})
	}
}

func TestTestsumCapsAndTruncatesMessages(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString(`{"Action":"output","Package":"p","Test":"T` + string(rune('A'+i%26)) + string(rune('a'+i/26)) + `","Output":"f_test.go:1: l1\n"}` + "\n")
		b.WriteString(`{"Action":"output","Package":"p","Test":"T` + string(rune('A'+i%26)) + string(rune('a'+i/26)) + `","Output":"l2\n"}` + "\n")
		b.WriteString(`{"Action":"fail","Package":"p","Test":"T` + string(rune('A'+i%26)) + string(rune('a'+i/26)) + `"}` + "\n")
	}
	res, _ := parseGoJSON([]byte(b.String()))
	got := res.render()
	if n := strings.Count(got, "\n"); n > testsumMaxLines {
		t.Errorf("%d lines, cap %d", n, testsumMaxLines)
	}
	if !strings.Contains(got, "more lines") {
		t.Errorf("no truncation note:\n%s", got)
	}
	lines := make([]string, 9)
	for i := range lines {
		lines[i] = "    f_test.go:1: m"
	}
	if _, msg := goMessage(lines); len(msg) != testsumMsgLines {
		t.Errorf("message has %d lines, want %d", len(msg), testsumMsgLines)
	}
}

func TestTestsumRunsCommandThroughSlot(t *testing.T) {
	slotEnv(t)
	t.Chdir(t.TempDir())
	var out, errb bytes.Buffer
	cmd := `printf '{"Action":"pass","Package":"p","Test":"T","Elapsed":0}\n{"Action":"pass","Package":"p","Elapsed":1}\n'`
	if code := run([]string{"testsum", "--", "sh", "-c", cmd}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "PASS: 1 tests") {
		t.Errorf("pass: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"testsum", "--", "sh", "-c", "echo boom; exit 3"}, &out, &errb); code != 1 || !strings.Contains(out.String(), "exited 3") {
		t.Errorf("unrecognized failure: %d %q", code, out.String())
	}
	out.Reset()
	cmd = `printf '{"Action":"pass","Package":"p","Test":"T","Elapsed":0}\n{"Action":"pass","Package":"p","Elapsed":1}\nmypy: 2 errors\n'; exit 2`
	if code := run([]string{"testsum", "--", "sh", "-c", cmd}, &out, &errb); code != 1 || !strings.HasPrefix(out.String(), "PASS: 1 tests") ||
		!strings.Contains(out.String(), "but the command exited 2") || !strings.Contains(out.String(), "mypy: 2 errors") {
		t.Errorf("pass then failure: %d %q", code, out.String())
	}
	out.Reset()
	errb.Reset()
	cmd = `printf '{"Action":"pass","Package":"p","Test":"T","Elapsed":0}\n{"Action":"pass","Package":"p","Elapsed":1}\n'; [ "$*" = "--format x -- -y" ] || exit 9`
	if code := run([]string{"testsum", "--format", "go", "sh", "-c", cmd, "sh", "--format", "x", "--", "-y"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "PASS: 1 tests") {
		t.Errorf("positional: %d %q %q", code, out.String(), errb.String())
	}
}

func TestTestsumFileOrCommand(t *testing.T) {
	dir := t.TempDir()
	saved := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(saved, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{saved: true, script: false, filepath.Join(dir, "missing"): false, dir: false} {
		if got := isOutputFile(path); got != want {
			t.Errorf("isOutputFile(%s) = %v", filepath.Base(path), got)
		}
	}
}

// Flags may follow a FILE, as before WP-1080 (audit 1000.1): only a command
// keeps the flags after it for itself.
func TestTestsumFlagsAfterFile(t *testing.T) {
	saved := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(saved, []byte(goJSONPass), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", saved, "--format", "go"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "PASS: 4 tests") {
		t.Errorf("FILE --format: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"testsum", saved, "--format", "go", "extra"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "extra") {
		t.Errorf("FILE then a second argument: %d %q %q", code, out.String(), errb.String())
	}
}

// A bare word that names a program is the program, even where a plain file
// of that name sits here; a path is the file (audit 1000.2). Output that
// was read and didn't parse says how to run it instead.
func TestTestsumBareWordIsCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"sh", "run-tests.sh"} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\ngo test ./...\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if isOutputFile("sh") {
		t.Error("sh, a program on PATH, read as saved output")
	}
	if !isOutputFile("./sh") {
		t.Error("./sh, a path to a plain file, not read as saved output")
	}
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "run-tests.sh"}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "wp testsum -- sh run-tests.sh") {
		t.Errorf("unparsed file: %d %q %q", code, out.String(), errb.String())
	}
}

// The hint names a limit above the one that was hit: at -timeout 30m it
// said "e.g. -timeout 30m" (WP-1857).
func TestGoTimeoutHintIsAboveTheLimitHit(t *testing.T) {
	f, ok := goTimeoutFailure("x/core", []string{"panic: test timed out after 30m0s", "\trunning tests:", "\t\tTestQuick (8s)"})
	if !ok {
		t.Fatal("not recognized")
	}
	got := strings.Join(f.Msg, "\n")
	if !strings.Contains(got, "e.g. -timeout 1h0m0s") || strings.Contains(got, "e.g. -timeout 30m") {
		t.Fatalf("hint = %q", got)
	}
	f, _ = goTimeoutFailure("x/core", []string{"panic: test timed out after 10m0s"})
	if got := strings.Join(f.Msg, "\n"); !strings.Contains(got, "e.g. -timeout 20m0s") {
		t.Fatalf("hint = %q", got)
	}
}
