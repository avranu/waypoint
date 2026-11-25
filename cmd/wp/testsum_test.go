package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
		{"go json timeout", "go", goJSONTimeout, 1, []string{"--- x/core (timed out after 10m0s)", "time limit (-timeout 10m0s)", "still running: TestQuick (3s)", "no test ran long", "-timeout 30m"}, ""},
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
	t.Chdir(t.TempDir()) // no repo: nothing recorded in this checkout's git dir
	var out, errb bytes.Buffer
	cmd := `printf '{"Action":"pass","Package":"p","Test":"T","Elapsed":0}\n{"Action":"pass","Package":"p","Elapsed":1}\n'`
	if code := run([]string{"testsum", "--", "sh", "-c", cmd}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "PASS: 1 tests") {
		t.Errorf("pass: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"testsum", "--", "sh", "-c", "echo boom; exit 3"}, &out, &errb); code != 1 || !strings.Contains(out.String(), "exited 3") {
		t.Errorf("unrecognized failure: %d %q", code, out.String())
	}
	// Tests passed, then a type check failed: say both, exit 1.
	out.Reset()
	cmd = `printf '{"Action":"pass","Package":"p","Test":"T","Elapsed":0}\n{"Action":"pass","Package":"p","Elapsed":1}\nmypy: 2 errors\n'; exit 2`
	if code := run([]string{"testsum", "--", "sh", "-c", cmd}, &out, &errb); code != 1 || !strings.HasPrefix(out.String(), "PASS: 1 tests") ||
		!strings.Contains(out.String(), "but the command exited 2") || !strings.Contains(out.String(), "mypy: 2 errors") {
		t.Errorf("pass then failure: %d %q", code, out.String())
	}
	// The command without -- (WP-1080): flags before it are testsum's, and
	// everything from it on, flags and -- included, is the command's.
	out.Reset()
	errb.Reset()
	cmd = `printf '{"Action":"pass","Package":"p","Test":"T","Elapsed":0}\n{"Action":"pass","Package":"p","Elapsed":1}\n'; [ "$*" = "--format x -- -y" ] || exit 9`
	if code := run([]string{"testsum", "--format", "go", "sh", "-c", cmd, "sh", "--format", "x", "--", "-y"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "PASS: 1 tests") {
		t.Errorf("positional: %d %q %q", code, out.String(), errb.String())
	}
}

// A failure with more output than the tail shows keeps all of it in a file
// the message names; with no repo here that is a temp file (WP-1812).
func TestTestsumKeepsLongFailedOutput(t *testing.T) {
	slotEnv(t)
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	pass := `printf '{"Action":"pass","Package":"p","Test":"T","Elapsed":0}\n{"Action":"pass","Package":"p","Elapsed":1}\n'; `
	fail := `printf '{"Action":"fail","Package":"p","Test":"T","Elapsed":0}\n{"Action":"fail","Package":"p","Elapsed":1}\n'; `
	for name, cmd := range map[string]string{
		"unrecognized": "echo first; seq 40; exit 3",
		"passed, then": pass + "echo first; seq 40; exit 3",
		"failed, and":  fail + "echo first; seq 40; exit 3",
	} {
		var out, errb bytes.Buffer
		if code := run([]string{"testsum", "--", "sh", "-c", cmd}, &out, &errb); code != 1 {
			t.Fatalf("%s: exit %d %q", name, code, out.String())
		}
		m := regexp.MustCompile(`All (\d+) lines of its output \(kept until the next long failure here\): (.+)\n`).FindStringSubmatch(out.String())
		if m == nil {
			t.Fatalf("%s: no pointer to the output:\n%s", name, out.String())
		}
		kept, err := os.ReadFile(m[2])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := strings.Count(string(kept), "\n"); strconv.Itoa(n) != m[1] || !strings.Contains(string(kept), "first\n1\n2\n") || !strings.HasSuffix(string(kept), "\n40\n") {
			t.Errorf("%s: pointer says %s lines; file has %d:\n%s", name, m[1], n, kept)
		}
		_ = os.Remove(m[2])
	}
}

func TestKeptOutputSaysWhenItCannotSave(t *testing.T) {
	data := []byte(strings.Repeat("x\n", 30))
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := keptAt(filepath.Join(blocker, "f.log"), data, 30)
	if !strings.HasPrefix(got, "Its 30 lines of output couldn't be saved: ") {
		t.Errorf("write failure: %q", got)
	}
	// A credential above the tail isn't kept in the file.
	p := filepath.Join(t.TempDir(), "f.log")
	keptAt(p, []byte("fetch https://bot:hunter2secret@git.example/x\n"+string(data)), 31)
	if kept, err := os.ReadFile(p); err != nil || strings.Contains(string(kept), "hunter2secret") || !strings.Contains(string(kept), "https://bot:***@git.example/x\n") {
		t.Errorf("kept file: %v\n%s", err, kept)
	}
	if got := keptOutput(data, 30); got != "" {
		t.Errorf("output no longer than shown: %q", got)
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

// keptRead writes data through keptAt and reads the file back.
func keptRead(t *testing.T, data string) (pointer, file string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.log")
	n := strings.Count(strings.TrimRight(data, "\n"), "\n") + 1
	pointer = keptAt(p, []byte(data), n)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return pointer, string(b)
}

func TestKeptRedactionKeepsEveryLine(t *testing.T) {
	seq := ""
	for i := 1; i <= 40; i++ {
		seq += strconv.Itoa(i) + "\n"
	}
	tok := "ghp_" + strings.Repeat("a1B2c3", 5)
	full := strings.Repeat("MIIEabc+/9", 6) + "ABCD"
	for name, c := range map[string]struct{ in, want string }{
		"header without an end": {
			"x\n-----BEGIN RSA PRIVATE KEY-----\n" + seq + "ok\nPASS\nthe finding\n",
			"x\n-----BEGIN RSA PRIVATE KEY-----\n" + seq + "ok\nPASS\nthe finding\n",
		},
		"complete key": {
			"before\n-----BEGIN RSA PRIVATE KEY-----\n" + full + "\n" + full + "\nzz==\n-----END RSA PRIVATE KEY-----\nafter\n",
			"before\n-----BEGIN RSA PRIVATE KEY-----\n***\n***\n***\n-----END RSA PRIVATE KEY-----\nafter\n",
		},
		"key cut off, then output": {
			"-----BEGIN PRIVATE KEY-----\n" + full + "\nzz==\nok\n",
			"-----BEGIN PRIVATE KEY-----\n***\n***\nok\n",
		},
		"CRLF key": {
			"-----BEGIN PRIVATE KEY-----\r\n" + full + "\r\n" + full + "\r\nzz==\r\n-----END PRIVATE KEY-----\r\nafter\r\n",
			"-----BEGIN PRIVATE KEY-----\r\n***\r\n***\r\n***\r\n-----END PRIVATE KEY-----\r\nafter\r\n",
		},
		"encrypted key headers": {
			"-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,ABCD\n\n" + full + "\n-----END RSA PRIVATE KEY-----\nafter\n",
			"-----BEGIN RSA PRIVATE KEY-----\n***\n***\n***\n***\n-----END RSA PRIVATE KEY-----\nafter\n",
		},
		"value on the next line": {
			"api_token:\nabcdefgh12345678\nafter\n",
			"api_token:\n***\nafter\n",
		},
		"credentials": {
			"a\ntoken=abcdef123456\n" + tok + "\nfetch https://bot:hunter2secret@git.example/x\n",
			"a\ntoken=***\n***\nfetch https://bot:***@git.example/x\n",
		},
		"plain lines": {strings.Repeat("x\n", 30), strings.Repeat("x\n", 30)},
	} {
		_, got := keptRead(t, c.in)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		keptInvariant(t, name, c.in, got)
	}
}

// keptInvariant: redaction never changes the number of lines, and a line
// that ended in \r still does.
func keptInvariant(t *testing.T, name, in, got string) {
	t.Helper()
	if strings.Count(got, "\n") != strings.Count(in, "\n") {
		t.Errorf("%s: %d lines in, %d kept", name, strings.Count(in, "\n"), strings.Count(got, "\n"))
		return
	}
	a, b := strings.Split(in, "\n"), strings.Split(got, "\n")
	for i := range a {
		if strings.HasSuffix(a[i], "\r") && !strings.HasSuffix(b[i], "\r") {
			t.Errorf("%s: line %d lost its \\r: %q", name, i+1, b[i])
		}
	}
}

// redactKept works on the whole text like redact, and keeps every line and
// everything that is not credential (WP-1812).
func TestKeptRedactionWholeText(t *testing.T) {
	numbered := func(n int) string {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "line %d\n", i)
		}
		return b.String()
	}
	b64 := func(n int) string { return strings.Repeat("aB3/", n/4+1)[:n] }
	body := b64(64)
	begin, end := "-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----"
	for name, c := range map[string]struct {
		in      string
		absent  []string
		present []string
		want    string // exact output, when set
	}{
		"terminated, first body line indented": {in: begin + "\n " + body + "\n" + body + "\n" + end + "\nafter\n", absent: []string{body}, present: []string{begin, end, "after"}},
		"terminated, trailing space":           {in: begin + "\n" + body + " \n" + body + "\n" + end + "\nafter\n", absent: []string{body}, present: []string{begin, end, "after"}},
		"terminated, diff prefixes":            {in: "- " + begin + "\n- " + body + "\n- " + body + "\n- " + end + "\nafter\n", absent: []string{body}, present: []string{"- " + begin, "- " + end, "after"}},
		"terminated, log prefix":               {in: begin + "\n12:00 " + body + "\n" + end + "\n", absent: []string{body}, present: []string{begin, end}},
		"one line":                             {in: "a " + begin + "SECRETSECRET" + end + " b\n", want: "a " + begin + "***" + end + " b\n"},
		"unterminated, indented first line":    {in: begin + "\n  " + body + "\n" + body + "\n" + body + "\nall done\n", absent: []string{body}, present: []string{begin, "all done"}},
		"basic auth on the next line":          {in: "Authorization: Basic\ndXNlcjpwYXNzd29yZA==\nnext\n", absent: []string{"dXNlcjpwYXNzd29yZA=="}, present: []string{"next"}},
		"bearer on the next line":              {in: "Authorization: Bearer\nabcdef123456\nnext\n", absent: []string{"abcdef123456"}, present: []string{"next"}},
		"flag value on the next line":          {in: "--password\nhunter2hunter2\nnext\n", absent: []string{"hunter2hunter2"}, present: []string{"next"}},
		"blank line between name and value":    {in: "password:\n\nhunter22222\nafter\n", absent: []string{"hunter22222"}, present: []string{"after"}},
		"bare begin then a panic": {
			in:   begin + "\npanic: runtime error: index out of range\n\nError: boom\nok\n12\nPASS\n",
			want: begin + "\npanic: runtime error: index out of range\n\nError: boom\nok\n12\nPASS\n",
		},
		"unterminated with headers": {
			in:      begin + "\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-256-CBC,ABCDEF\n\n" + body + "\n" + b64(20) + "\nafter\n",
			absent:  []string{"Proc-Type", "DEK-Info", body, b64(20) + "\n"},
			present: []string{begin, "after"},
		},
		"39 characters stays after a bare begin": {in: begin + "\n" + b64(39) + "\nafter\n", want: begin + "\n" + b64(39) + "\nafter\n"},
		"40 characters is blanked after a begin": {in: begin + "\n" + b64(40) + "\nall done\n", want: begin + "\n***\nall done\n"},
		"CRLF key": {
			in:   begin + "\r\n" + body + "\r\n " + body + "\r\n" + b64(10) + "\r\n" + end + "\r\nafter\r\n",
			want: begin + "\r\n***\r\n***\r\n***\r\n" + end + "\r\nafter\r\n",
		},
		"CRLF unterminated key": {
			in:   begin + "\r\n" + body + "\r\n" + b64(10) + "\r\nafter\r\n",
			want: begin + "\r\n***\r\n***\r\nafter\r\n",
		},
		"unterminated, body on the begin line":        {in: "key: \"" + begin + `\n` + body + `\n` + body + "\nall done\n", absent: []string{body}, present: []string{begin, "all done"}},
		"terminated, body on the begin and end lines": {in: begin + body + "\n" + body + end + "\nafter\n", want: begin + "***\n***" + end + "\nafter\n"},
		"name alone before a key":                     {in: "api_key:\n" + begin + "\n" + body + "\n" + end + "\n", want: "api_key:\n" + begin + "\n***\n" + end + "\n"},
		"unterminated, log prefixes": {
			in:   begin + "\n12:00 " + body + "\n12:00 " + body + "\n12:00 zz9=\n12:01 all done\n",
			want: begin + "\n***\n***\n***\n12:01 all done\n",
		},
		"value glued to a begin line":      {in: "password=hunter2hunter2" + begin + "\n" + body + "\n" + end + "\n", absent: []string{"hunter2hunter2", body}, present: []string{begin, end}},
		"base64 value glued to a begin":    {in: "password=dGVzdA==" + begin + "\n" + body + "\n" + end + "\n", absent: []string{"dGVzdA==", body}, present: []string{begin, end}},
		"value with = glued to a begin":    {in: "secret=hunter2=" + begin + "\n" + body + "\n" + end + "\n", absent: []string{"hunter2", body}, present: []string{begin, end}},
		"CRLF credential on the next line": {in: "Authorization: Basic\r\nSECRETSECRET\r\nnext\r\n", absent: []string{"SECRETSECRET"}, present: []string{"next"}},
		"CRLF flag value":                  {in: "--password\r\nhunter2hunter2\r\nnext\r\n", want: "--password\r\n***\r\nnext\r\n"},
		"diff plus lines with headers": {
			in:      begin + "\n+Proc-Type: 4,ENCRYPTED\n+DEK-Info: AES-128-CBC,00112233445566778899AABBCCDDEEFF\n+\n+" + body + "\nrest\n",
			absent:  []string{"00112233445566778899AABBCCDDEEFF", body},
			present: []string{begin, "rest"},
		},
		"log prefix, short last line without =": {
			in:      begin + "\n12:00 " + body + "\n12:00 abcdEFGH1234\nrest\n",
			absent:  []string{"abcdEFGH1234", body},
			present: []string{begin, "rest"},
		},
		"log prefixed headers": {
			in:      begin + "\n12:00 Proc-Type: 4,ENCRYPTED\n12:00 DEK-Info: AES-128-CBC,00112233445566778899AABBCCDDEEFF\n\n12:00 " + body + "\nrest\n",
			absent:  []string{"00112233445566778899AABBCCDDEEFF", body},
			present: []string{begin, "rest"},
		},
		"blank line before the body": {
			in:      begin + "\n\n" + body + "\n" + body + "\nrest\n",
			absent:  []string{body},
			present: []string{begin, "rest"},
		},
		"JSON array of lines": {
			in:      begin + "\n\"" + body + "\",\n\"" + body + "\",\n\"abcd12EF\"\nrest\n",
			absent:  []string{body, "abcd12EF"},
			present: []string{begin, "rest"},
		},
		"commit SHA after a bare begin": {
			in:   begin + "\ncommit " + strings.Repeat("3f2a", 10) + "\n" + strings.Repeat("3f2a", 10) + "\nPASS\n",
			want: begin + "\ncommit " + strings.Repeat("3f2a", 10) + "\n" + strings.Repeat("3f2a", 10) + "\nPASS\n",
		},
		"a word after a bare begin": {in: begin + "\nPASS\nrest\n", want: begin + "\nPASS\nrest\n"},
		"a begin named in a message": {
			in:      "want a line: " + begin + "\nlint: finding X at a.go:3\na\nb\nc\nd\ne\n" + begin + "\n" + body + "\n" + end + "\nafter\n",
			absent:  []string{body},
			present: []string{"lint: finding X at a.go:3", "after"},
		},
		"begin with no end for 450 lines": {
			in:      begin + "\n" + numbered(450) + end + "\n",
			present: []string{"line 1\n", "line 450\n"},
		},
		"PGP block": {
			in:      "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: GnuPG v2\n\n" + body + "\n" + body + "\n=AbCd\n-----END PGP PRIVATE KEY BLOCK-----\nafter\n",
			absent:  []string{body, "=AbCd", "GnuPG"},
			present: []string{"-----BEGIN PGP PRIVATE KEY BLOCK-----", "-----END PGP PRIVATE KEY BLOCK-----", "after"},
		},
		"PGP block without an end": {
			in:      "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: GnuPG v2\n\n" + body + "\n" + body + "\n=AbCd\nafter\n",
			absent:  []string{body, "=AbCd", "GnuPG"},
			present: []string{"-----BEGIN PGP PRIVATE KEY BLOCK-----", "after"},
		},
		"digest header with CRLF": {
			in:      "Authorization: Digest username=\"u\", response=\"x\"\r\nnext\r\n",
			absent:  []string{`response="x"`},
			present: []string{"\r\nnext\r\n"},
		},
	} {
		_, got := keptRead(t, c.in)
		if c.want != "" && got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		for _, a := range c.absent {
			if strings.Contains(got, a) {
				t.Errorf("%s: %q survives:\n%s", name, a, got)
			}
		}
		for _, p := range c.present {
			if !strings.Contains(got, p) {
				t.Errorf("%s: %q was lost:\n%s", name, p, got)
			}
		}
		keptInvariant(t, name, c.in, got)
	}
}

// Outside a repo every long failure names one file, overwritten each time,
// so they don't pile up in the temp directory.
func TestKeptOutputOutsideRepoUsesOneFile(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	one := keptOutput([]byte(strings.Repeat("first\n", 30)), 20)
	two := keptOutput([]byte(strings.Repeat("second\n", 30)), 20)
	re := regexp.MustCompile(`: (.+)\n`)
	a, b := re.FindStringSubmatch(one), re.FindStringSubmatch(two)
	if a == nil || b == nil || a[1] != b[1] {
		t.Fatalf("paths differ: %q %q", one, two)
	}
	if want := filepath.Join(os.TempDir(), fmt.Sprintf("wp-failed-output-%d.log", os.Getuid())); a[1] != want {
		t.Errorf("path %q, want %q", a[1], want)
	}
	if got, _ := os.ReadFile(b[1]); string(got) != strings.Repeat("second\n", 30) {
		t.Errorf("file holds %q", got)
	}
}

// Failing tests with a clean exit have nothing else to show: no file.
func TestTestsumNoKeptOutputOnCleanExit(t *testing.T) {
	slotEnv(t)
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	cmd := `printf '{"Action":"fail","Package":"p","Test":"T","Elapsed":0}\n{"Action":"fail","Package":"p","Elapsed":1}\n'; seq 40`
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "--", "sh", "-c", cmd}, &out, &errb); code != 1 {
		t.Fatalf("exit %d %q", code, out.String())
	}
	if strings.Contains(out.String(), "lines of its output") {
		t.Errorf("a pointer for a command that exited 0:\n%s", out.String())
	}
}

// The count is the file's lines, trailing blank lines included, while the
// threshold is about the lines that hold something.
func TestKeptOutputCountsTheFilesLines(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	got := keptOutput([]byte(strings.Repeat("x\n", 31)+"\n\n"), 20)
	m := regexp.MustCompile(`All (\d+) lines of its output \(kept until the next long failure here\): (.+)\n`).FindStringSubmatch(got)
	if m == nil || m[1] != "33" {
		t.Fatalf("pointer: %q", got)
	}
	if b, _ := os.ReadFile(m[2]); strings.Count(string(b), "\n") != 33 {
		t.Errorf("file has %d lines", strings.Count(string(b), "\n"))
	}
	// 20 lines and blank ones after them: all of it was shown.
	if got := keptOutput([]byte(strings.Repeat("x\n", 20)+"\n\n"), 20); got != "" {
		t.Errorf("blank lines past the shown ones: %q", got)
	}
}

// wp testsum runs go test as go test -json; the kept file and the lines shown
// are the text of the output, not its events.
func TestKeptOutputOfGoTestJSONIsPlain(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	var b strings.Builder
	b.WriteString("# m/p [build failed]\n")
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&b, `{"Action":"output","Package":"m/p","Test":"T","Output":"line %d\n"}`+"\n", i)
		b.WriteString(`{"Action":"run","Package":"m/p","Test":"T"}` + "\n")
	}
	b.WriteString(`{"Action":"fail","Package":"m/p","Elapsed":0}` + "\n")
	got := failedTail([]byte(b.String()), 20)
	m := regexp.MustCompile(`All (\d+) lines of its output \(kept until the next long failure here\): (.+)\n`).FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("no pointer:\n%s", got)
	}
	if strings.Contains(got, `{"Action"`) || !strings.Contains(got, "line 25\n") {
		t.Errorf("tail is not plain text:\n%s", got)
	}
	kept, _ := os.ReadFile(m[2])
	if strings.Contains(string(kept), `{"Action"`) || !strings.HasPrefix(string(kept), "# m/p [build failed]\nline 1\nline 2\n") || strconv.Itoa(strings.Count(string(kept), "\n")) != m[1] {
		t.Errorf("pointer says %s lines; file has %d:\n%s", m[1], strings.Count(string(kept), "\n"), kept)
	}
	// Output that holds no events is as it was.
	if p := plainOutput([]byte("a\n{\"x\":1}\nb\n")); string(p) != "a\n{\"x\":1}\nb\n" {
		t.Errorf("plain output changed: %q", p)
	}
}

// A pointer to the whole output is for output longer than the 20 lines
// shown: 20 lines are all shown, 21 are not.
func TestTestsumPointerOnlyPastTheShownLines(t *testing.T) {
	slotEnv(t)
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	for n, want := range map[int]bool{20: false, 21: true} {
		var out, errb bytes.Buffer
		if code := run([]string{"testsum", "--", "sh", "-c", "seq " + strconv.Itoa(n) + "; exit 3"}, &out, &errb); code != 1 {
			t.Fatalf("%d lines: exit %d %q", n, code, out.String())
		}
		if got := strings.Contains(out.String(), "lines of its output"); got != want {
			t.Errorf("%d lines: pointer %v, want %v:\n%s", n, got, want, out.String())
		}
	}
}

// The kept file holds a log line that only looks like a go test event, and
// output text that looks like one (WP-1812).
func TestPlainOutputKeepsLookalikeLines(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	odd := `{"Action":"fail","Message":"nil pointer in parser"}`
	got := keptOutput([]byte(odd+"\n"+strings.Repeat("x\n", 39)), 20)
	m := regexp.MustCompile(`All (\d+) lines of its output \(kept until the next long failure here\): (.+)\n`).FindStringSubmatch(got)
	if m == nil || m[1] != "40" {
		t.Fatalf("pointer: %q", got)
	}
	if b, _ := os.ReadFile(m[2]); !strings.Contains(string(b), "nil pointer") {
		t.Errorf("the line was dropped:\n%s", b)
	}
	// Output that is itself JSON stays through failedTail's one pass.
	var b strings.Builder
	for i := 0; i < 39; i++ {
		fmt.Fprintf(&b, `{"Action":"output","Package":"p","Test":"T","Output":"o%d\n"}`+"\n", i)
	}
	b.WriteString(`{"Action":"output","Package":"p","Test":"T","Output":"{\"Action\":\"fail\",\"Package\":\"p\"}\n"}` + "\n")
	got = failedTail([]byte(b.String()), 20)
	m = regexp.MustCompile(`All (\d+) lines of its output \(kept until the next long failure here\): (.+)\n`).FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("no pointer:\n%s", got)
	}
	if k, _ := os.ReadFile(m[2]); !strings.Contains(string(k), `{"Action":"fail","Package":"p"}`+"\n") {
		t.Errorf("the output text was dropped:\n%s", k)
	}
}

// Failed tests and a failing command: nothing of the output was shown, so
// all of it is kept however short.
func TestTestsumKeepsShortOutputWithFailedTests(t *testing.T) {
	slotEnv(t)
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	cmd := `printf '{"Action":"fail","Package":"p","Test":"T","Elapsed":0}\n{"Action":"fail","Package":"p","Elapsed":1}\n'; echo LINT FAILED; exit 2`
	for _, args := range [][]string{{"testsum"}, {"testsum", "--repeat", "2"}} {
		var out, errb bytes.Buffer
		if code := run(append(args, "--", "sh", "-c", cmd), &out, &errb); code != 1 {
			t.Fatalf("%v: exit %d %q %q", args, code, out.String(), errb.String())
		}
		m := regexp.MustCompile(`All (\d+) lines of its output \(kept until the next long failure here\): (.+)\n`).FindStringSubmatch(out.String())
		if m == nil {
			t.Fatalf("%v: no pointer:\n%s", args, out.String())
		}
		if k, _ := os.ReadFile(m[2]); !strings.Contains(string(k), "LINT FAILED") {
			t.Errorf("%v: kept file:\n%s", args, k)
		}
	}
}
