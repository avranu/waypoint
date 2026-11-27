package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTestsumCountNSaysHowOftenATestFailed(t *testing.T) {
	ev := func(action, test, output string) string {
		s := `{"Action":"` + action + `","Package":"m/p"`
		if test != "" {
			s += `,"Test":"` + test + `"`
		}
		if output != "" {
			s += `,"Output":` + strings.ReplaceAll(`"`+output+`\n"`, "\t", `\t`)
		}
		return s + "}\n"
	}
	var b strings.Builder
	for i, fail := range []bool{false, true, false, true, true} {
		b.WriteString(ev("run", "TestF", ""))
		if fail {
			b.WriteString(ev("output", "TestF", "    f_test.go:9: run "+string(rune('1'+i))+" unlucky"))
			b.WriteString(ev("fail", "TestF", ""))
		} else {
			b.WriteString(ev("pass", "TestF", ""))
		}
		b.WriteString(ev("run", "TestBroken", ""))
		b.WriteString(ev("output", "TestBroken", "    f_test.go:20: never works"))
		b.WriteString(ev("fail", "TestBroken", ""))
	}
	b.WriteString(ev("fail", "", ""))
	res := parseGoTest([]byte(b.String()))
	got := res.render()
	for _, want := range []string{"--- m/p.TestF (failed 3 of 5 runs: flaky)", "--- m/p.TestBroken (failed all 5 runs)", "run 2 unlucky"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "never works") != 1 || strings.Contains(got, "run 4 unlucky") {
		t.Errorf("each run's output piled up:\n%s", got)
	}
}

func TestTestsumTimeoutNamesTheTestItCutShort(t *testing.T) {
	data := `{"Action":"run","Package":"m/p","Test":"TestSlow"}
{"Action":"output","Package":"m/p","Test":"TestSlow","Output":"=== RUN   TestSlow\n"}
{"Action":"output","Package":"m/p","Test":"TestSlow","Output":"panic: test timed out after 2s\n"}
{"Action":"output","Package":"m/p","Test":"TestSlow","Output":"\trunning tests:\n"}
{"Action":"output","Package":"m/p","Test":"TestSlow","Output":"\t\tTestSlow (2s)\n"}
{"Action":"output","Package":"m/p","Test":"TestSlow","Output":"goroutine 19 [running]:\n"}
{"Action":"output","Package":"m/p","Test":"TestSlow","Output":"testing.(*M).startAlarm.func1()\n"}
{"Action":"output","Package":"m/p","Test":"TestSlow","Output":"\t/usr/go/src/testing/testing.go:2961 +0x34a\n"}
{"Action":"output","Package":"m/p","Test":"TestSlow","Output":"m/p.TestSlow(0xc000)\n"}
{"Action":"output","Package":"m/p","Test":"TestSlow","Output":"\t/src/m/p/a_test.go:5 +0x25\n"}
{"Action":"output","Package":"m/p","Output":"FAIL\tm/p\t2.020s\n"}
{"Action":"fail","Package":"m/p","Elapsed":2.02}
`
	got := parseGoTest([]byte(data)).render()
	for _, want := range []string{"--- m/p (timed out after 2s)", "at a_test.go:5", "-timeout 2s", "TestSlow (2s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "testing.go") || strings.Contains(got, "goroutine") {
		t.Errorf("the goroutine dump, not the test:\n%s", got)
	}
}

func TestBaseArgvRunsOnlyTheFailingTests(t *testing.T) {
	fails := []testFailure{{Pkg: "m/p", Test: "TestA/sub one"}, {Pkg: "m/p", Test: "TestA"}, {Pkg: "m/q", Test: "TestB.x"}}
	got := baseArgv([]string{"go", "test", "-json", "-run", "Test", "-race", "./...", "-args", "-v"}, fails)
	want := []string{"go", "test", "-json", "-race", "./...", `-run=^(TestA|TestB\.x)$`, "-args", "-v"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got := baseArgv([]string{"go", "test", "-run=X", "./..."}, fails[:1]); !slices.Equal(got, []string{"go", "test", "./...", "-run=^(TestA)$"}) {
		t.Errorf("-run=X: %q", got)
	}
	whole := []string{"go", "test", "./..."}
	if got := baseArgv(whole, []testFailure{{Pkg: "m/p"}, {Pkg: "m/p", Test: "TestA"}}); !slices.Equal(got, whole) {
		t.Errorf("package failure: %q", got)
	}
	if got := baseArgv([]string{"pytest", "-q"}, fails); !slices.Equal(got, []string{"pytest", "-q"}) {
		t.Errorf("pytest: %q", got)
	}
}

// baseModule is a git repo with a Go module whose TestOld already fails at
// the first commit, and a working tree that breaks TestMine and adds TestNew.
func baseModule(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	tempRepo(t)
	t.Setenv("GOFLAGS", "-mod=mod")
	write(t, "go.mod", "module basemod\n\ngo 1.22\n")
	if err := os.Mkdir("p", 0o750); err != nil {
		t.Fatal(err)
	}
	const head = "package p\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) { t.Fatal(\"broken before\") }\nfunc TestFine(t *testing.T) {}\n"
	write(t, "p/p_test.go", head+"func TestMine(t *testing.T) {}\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "base")
	write(t, "p/p_test.go", head+"func TestMine(t *testing.T) { t.Fatal(\"I broke it\") }\nfunc TestNew(t *testing.T) { t.Fatal(\"new\") }\n")
}

func TestTestsumBaseTellsOldFailuresFromNewOnes(t *testing.T) {
	slotEnv(t)
	baseModule(t)
	t.Chdir("p")
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "--base", "HEAD", "--", "go", "test", "."}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out.String(), errb.String())
	}
	got := out.String()
	for _, want := range []string{"FAIL: 3 failed", "At HEAD (", "the failing tests:", "fails there too   basemod/p.TestOld", "passes there      basemod/p.TestMine",
		"isn't there       basemod/p.TestNew", "1 of 3 failed there too", "wp testsum --repeat 20 go test -run '^TestMine$' basemod/p"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if b, _ := os.ReadFile("p_test.go"); !strings.Contains(string(b), "I broke it") {
		t.Errorf("the working tree changed:\n%s", b)
	}
	if wt := gitDo(t, "worktree", "list"); strings.Count(wt, "\n") != 0 || strings.Contains(wt, baseTmpPrefix) {
		t.Errorf("a checkout was left:\n%s", wt)
	}
}

func TestTestsumBaseSkipsTheBaseWhenNothingFails(t *testing.T) {
	slotEnv(t)
	baseModule(t)
	write(t, "p/p_test.go", "package p\n\nimport \"testing\"\n\nfunc TestFine(t *testing.T) {}\n")
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "--base", "HEAD", "--", "go", "test", "./p"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	if got := out.String(); !strings.HasPrefix(got, "PASS: 1 tests") || strings.Contains(got, "At HEAD") {
		t.Errorf("got:\n%s", got)
	}
}

func TestTestsumBaseUsage(t *testing.T) {
	slotEnv(t)
	tempRepo(t)
	saved := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(saved, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"testsum", "--base", "nope", "--", "go", "test", "."},
		{"testsum", "--base", "HEAD", "--repeat", "3", "--", "go", "test", "."},
		{"testsum", "--base", "HEAD", saved},
	} {
		var out bytes.Buffer
		if code := run(args, &out, &out); code != 2 {
			t.Errorf("%q: exit %d, want 2\n%s", args, code, out.String())
		}
	}
}

func TestTestsumTimeoutAfterAFailedTestStillShows(t *testing.T) {
	data := `{"Action":"run","Package":"m/p","Test":"TestBad"}
{"Action":"output","Package":"m/p","Test":"TestBad","Output":"    p_test.go:4: bad\n"}
{"Action":"fail","Package":"m/p","Test":"TestBad"}
{"Action":"run","Package":"m/p","Test":"TestHang"}
{"Action":"output","Package":"m/p","Test":"TestHang","Output":"panic: test timed out after 2s\n"}
{"Action":"output","Package":"m/p","Test":"TestHang","Output":"goroutine 7 [running]:\n"}
{"Action":"fail","Package":"m/p","Elapsed":2}
`
	res := parseGoTest([]byte(data))
	var names []string
	for _, f := range res.Failures {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"m/p.TestBad", "m/p (timed out after 2s)"}) {
		t.Fatalf("failures = %q, want the failed test and the timeout", names)
	}
	plain := `{"Action":"run","Package":"m/p","Test":"TestBad"}
{"Action":"fail","Package":"m/p","Test":"TestBad"}
{"Action":"fail","Package":"m/p","Elapsed":1}
`
	if got := parseGoTest([]byte(plain)).Failures; len(got) != 1 {
		t.Errorf("failures = %+v, want only TestBad", got)
	}
}

// A timeout parsed from go test's output belongs to its package, as any
// other failure does: the base's verdict on it, and on a test the base's
// timeout cut short, comes from that package's results there.
func TestBaseVerdictOnAParsedTimeout(t *testing.T) {
	const timeout = `{"Action":"run","Package":"m/p","Test":"TestHang"}
{"Action":"output","Package":"m/p","Test":"TestHang","Output":"panic: test timed out after 2s\n"}
{"Action":"fail","Package":"m/p","Elapsed":2}
`
	const failed = `{"Action":"run","Package":"m/p","Test":"TestB"}
{"Action":"output","Package":"m/p","Test":"TestB","Output":"    p_test.go:4: bad\n"}
{"Action":"fail","Package":"m/p","Test":"TestB"}
{"Action":"fail","Package":"m/p","Elapsed":1}
`
	const built = `{"Action":"output","Package":"m/p","Output":"# m/p\n"}
{"Action":"output","Package":"m/p","Output":"p.go:3:1: syntax error\n"}
{"Action":"fail","Package":"m/p","Elapsed":0}
`
	here, there, broken := parseGoTest([]byte(timeout)), parseGoTest([]byte(failed)), parseGoTest([]byte(built))
	if len(here.Failures) != 1 || here.Failures[0].Pkg != "m/p" {
		t.Fatalf("timeout parsed as %+v", here.Failures)
	}
	if len(broken.Failures) != 1 || broken.Failures[0].Test != "" {
		t.Fatalf("build failure parsed as %+v", broken.Failures)
	}
	for _, c := range []struct {
		name string
		f    testFailure
		base *testResult
		want string
	}{
		{"timed out here, a test failed there", here.Failures[0], there, baseOtherwise},
		{"timed out here, didn't build there", here.Failures[0], broken, baseOtherwise},
		{"timed out both", here.Failures[0], here, baseFailsToo},
		{"a test failed here, timed out there", there.Failures[0], here, baseUnknown},
	} {
		if got := baseVerdict(c.f, c.base); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBaseVerdictTellsWhatTheBaseCanSay(t *testing.T) {
	test := func(name string) testFailure { return testFailure{Name: "m/p." + name, Pkg: "m/p", Test: name} }
	pkg := testFailure{Name: "m/p (package)", Pkg: "m/p"}
	ran := func(fails []testFailure, ran ...string) *testResult {
		r := &testResult{Failures: fails, Ran: map[string]bool{}, Skips: map[string]bool{}}
		for _, n := range ran {
			r.Ran["m/p."+n] = true
		}
		return r
	}
	cases := []struct {
		name string
		f    testFailure
		base *testResult
		want string
	}{
		{"same failure", test("TestA"), ran([]testFailure{test("TestA")}, "TestA"), baseFailsToo},
		{"passed", test("TestA"), ran(nil, "TestA"), basePasses},
		{"new test", test("TestA"), ran(nil), baseMissing},
		{"skipped there", test("TestA"), ran(nil), baseMissing},
		{"base didn't build", test("TestA"), ran([]testFailure{pkg}), baseUnknown},
		{"base timed out first", test("TestA"), ran([]testFailure{pkg}, "TestB"), baseUnknown},
		{"timeout both", pkg, ran([]testFailure{pkg}), baseFailsToo},
		{"timeout here, a test failed there", pkg, ran([]testFailure{test("TestB")}, "TestB"), baseOtherwise},
		{"timeout here only", pkg, ran(nil, "TestB"), basePasses},
		{"only failures named", test("TestA"), &testResult{}, baseNotFailing},
		{"no results", test("TestA"), nil, baseUnknown},
	}
	cases[3].base.Skips["m/p.TestA"] = true
	cases[3].want = baseSkipped
	for _, c := range cases {
		if got := baseVerdict(c.f, c.base); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	text := baseText("main", "abcdef1234", []testFailure{test("TestA")}, ran([]testFailure{pkg}), 1, nil, false, nil)
	if !strings.Contains(text, "no result there") || !strings.Contains(text, "m/p (package) failed there first") || strings.Contains(text, "None failed there") {
		t.Errorf("a base that didn't build reads as a verdict:\n%s", text)
	}
}

func TestTestsumBaseInANewFolder(t *testing.T) {
	slotEnv(t)
	baseModule(t)
	writeFile(t, "q/q_test.go", "package q\n\nimport \"testing\"\n\nfunc TestQ(t *testing.T) { t.Fatal(\"new\") }\n")
	t.Chdir("q")
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "--base", "HEAD", "--", "go", "test", "."}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out.String(), errb.String())
	}
	if got := out.String(); !strings.Contains(got, "this folder isn't there: every failure is this change's") {
		t.Errorf("a new folder isn't named:\n%s%s", got, errb.String())
	}
}

func TestBuildFlagsKeepWhatATestSees(t *testing.T) {
	got := buildFlags([]string{"go", "test", "-json", "-race", "-tags", "e2e", "-run", "X", "-count=3", "-timeout=5m", "./...", "-args", "-race"})
	want := []string{"-race", "-tags", "e2e", "-timeout=5m"}
	if !slices.Equal(got, want) {
		t.Errorf("buildFlags = %q, want %q", got, want)
	}
}

// The base run's output is shown redacted, as a failed command's tail is.
func TestBaseTextWithNoResultsIsRedacted(t *testing.T) {
	data := []byte("build failed\npassword=hunter2secret\nboom\n")
	text := baseText("main", "abcdef1234", nil, nil, 1, data, false, nil)
	if strings.Contains(text, "hunter2secret") || !strings.Contains(text, "boom") {
		t.Errorf("%s", text)
	}
}

func TestBaseTextWithNoResultsShowsPlainText(t *testing.T) {
	data := []byte(`{"Action":"output","Package":"p","Output":"build failed: boom\n"}` + "\n")
	text := baseText("main", "abcdef1234", nil, nil, 1, data, false, nil)
	if !strings.Contains(text, "build failed: boom") || strings.Contains(text, `{"Action"`) {
		t.Errorf("%s", text)
	}
}
