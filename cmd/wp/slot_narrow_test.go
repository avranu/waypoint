package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSplitGoTestArgs(t *testing.T) {
	cases := []struct {
		name  string
		argv  []string
		ok    bool
		pkgs  []string
		flags []string
		run   bool
	}{
		{"plain", []string{"go", "test", "./..."}, true, []string{"./..."}, nil, false},
		{"no package", []string{"go", "test"}, true, nil, nil, false},
		{"flags kept in order", []string{"go", "test", "-race", "-count=3", "-timeout", "30m", "-tags", "x", "./a", "./b"}, true, []string{"./a", "./b"}, []string{"-race", "-count=3", "-timeout", "30m", "-tags", "x"}, false},
		{"json and v", []string{"go", "test", "-json", "-v", "./a"}, true, []string{"./a"}, []string{"-json", "-v"}, false},
		{"double dash flag", []string{"go", "test", "--race", "./a"}, true, []string{"./a"}, []string{"--race"}, false},
		{"-run", []string{"go", "test", "-run", "TestX", "./a"}, true, []string{"./a"}, []string{"-run", "TestX"}, true},
		{"-run=", []string{"go", "test", "-run=TestX", "./a"}, true, []string{"./a"}, []string{"-run=TestX"}, true},
		{"--run", []string{"go", "test", "--run", "TestX", "./a"}, true, []string{"./a"}, []string{"--run", "TestX"}, true},
		{"-run after the packages", []string{"go", "test", "./a", "./b", "-run", "TestX"}, true, []string{"./a", "./b"}, []string{"-run", "TestX"}, true},
		{"bench keeps all tests", []string{"go", "test", "-bench", ".", "./a"}, true, []string{"./a"}, []string{"-bench", "."}, false},
		{"-args stops", []string{"go", "test", "./a", "-args", "-x"}, false, nil, nil, false},
		{"unknown flag", []string{"go", "test", "-nosuchflag", "./a"}, false, nil, nil, false},
		{"value missing", []string{"go", "test", "./a", "-timeout"}, false, nil, nil, false},
		{"make", []string{"make", "test"}, false, nil, nil, false},
		{"sh -c", []string{"sh", "-c", "go test ./..."}, false, nil, nil, false},
		{"env first", []string{"env", "A=1", "go", "test", "./..."}, false, nil, nil, false},
		{"go build", []string{"go", "build", "./..."}, false, nil, nil, false},
	}
	for _, c := range cases {
		a, ok := splitGoTestArgs(c.argv)
		if ok != c.ok {
			t.Errorf("%s: ok %v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if !slices.Equal(a.Pkgs, c.pkgs) || !slices.Equal(a.Flags, c.flags) || a.Run != c.run {
			t.Errorf("%s: pkgs %v flags %v run %v; want %v %v %v", c.name, a.Pkgs, a.Flags, a.Run, c.pkgs, c.flags, c.run)
		}
	}
	for flag, field := range map[string]func(goTestArgs) bool{
		"-fuzz": func(a goTestArgs) bool { return a.Fuzz }, "-list": func(a goTestArgs) bool { return a.List }, "-c": func(a goTestArgs) bool { return a.Compile },
	} {
		argv := []string{"go", "test", flag, "x", "./a"}
		if flag == "-c" {
			argv = []string{"go", "test", "-c", "./a"}
		}
		if a, ok := splitGoTestArgs(argv); !ok || !field(a) {
			t.Errorf("%s not recognised: %+v %v", flag, a, ok)
		}
	}
}

// narrowRepo is a module with a heavy package (internal/core: a.go and b.go
// with a_test.go and b_test.go beside them) and a light one (internal/small),
// and the changed paths the test gives it.
func narrowRepo(t *testing.T, changed ...string) narrowEnv {
	t.Helper()
	return narrowRepoIn(t, t.TempDir(), changed...)
}

// narrowRepoIn is narrowRepo in the folder root.
func narrowRepoIn(t *testing.T, root string, changed ...string) narrowEnv {
	t.Helper()
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.22\n")
	write("internal/core/a.go", "package core\n")
	write("internal/core/b.go", "package core\n")
	write("internal/core/a_test.go", "package core\nimport \"testing\"\nfunc TestA1(t *testing.T) {}\nfunc TestA2(t *testing.T) {}\n")
	write("internal/core/b_test.go", "package core\nimport \"testing\"\nfunc TestB1(t *testing.T) {}\n")
	write("internal/small/s.go", "package small\n")
	write("internal/small/s_test.go", "package small\nimport \"testing\"\nfunc TestS(t *testing.T) {}\n")
	return narrowEnv{
		root: root, cwd: root, module: "example.com/m", heavy: []string{"internal/core"},
		changes: func() (string, []string, error) { return "8240ca8a9ffff", changed, nil },
	}
}

func cmdLines(p goTestPlan) []string {
	var out []string
	for _, c := range p.Cmds {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func TestPlanGoTestNarrows(t *testing.T) {
	changed := []string{"internal/core/a.go", "internal/small/s.go", "README.md"}
	cases := []struct {
		name string
		argv []string
		want []string // the commands
	}{
		{"asked the heavy package only", []string{"go", "test", "./internal/core"}, []string{"go test -count=1 -run ^(TestA1|TestA2)$ ./internal/core"}},
		{"bare path", []string{"go", "test", "internal/core"}, []string{"go test -count=1 -run ^(TestA1|TestA2)$ ./internal/core"}},
		{"import path", []string{"go", "test", "example.com/m/internal/core"}, []string{"go test -count=1 -run ^(TestA1|TestA2)$ ./internal/core"}},
		{"everything", []string{"go", "test", "./..."}, []string{"go test -count=1 ./internal/small", "go test -count=1 -run ^(TestA1|TestA2)$ ./internal/core"}},
		{"a ... pattern holding it", []string{"go", "test", "./internal/..."}, []string{"go test -count=1 ./internal/small", "go test -count=1 -run ^(TestA1|TestA2)$ ./internal/core"}},
		{"module pattern", []string{"go", "test", "example.com/m/..."}, []string{"go test -count=1 ./internal/small", "go test -count=1 -run ^(TestA1|TestA2)$ ./internal/core"}},
		{"x/... takes x", []string{"go", "test", "./internal/core/..."}, []string{"go test -count=1 -run ^(TestA1|TestA2)$ ./internal/core"}},
		{"light package asked with the heavy one", []string{"go", "test", "./internal/small", "./internal/core"}, []string{"go test -count=1 ./internal/small", "go test -count=1 -run ^(TestA1|TestA2)$ ./internal/core"}},
		{"flags kept", []string{"go", "test", "-race", "-timeout", "30m", "-count=2", "-json", "./internal/core"}, []string{"go test -count=1 -race -timeout 30m -count=2 -json -run ^(TestA1|TestA2)$ ./internal/core"}},
		{"-run after the packages is not narrowed", nil, nil},
		{"bench without -run", []string{"go", "test", "-bench", ".", "./internal/core"}, []string{"go test -count=1 -bench . -run ^(TestA1|TestA2)$ ./internal/core"}},
	}
	for _, c := range cases {
		if c.argv == nil {
			continue
		}
		p := planGoTest(c.argv, narrowRepo(t, changed...))
		if !p.Narrow || p.Whole {
			t.Errorf("%s: narrow %v whole %v", c.name, p.Narrow, p.Whole)
			continue
		}
		if got := cmdLines(p); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestPlanGoTestRestrictsToAskedPackages(t *testing.T) {
	// The light package changed too: asking for the heavy one alone leaves it out.
	p := planGoTest([]string{"go", "test", "./internal/core"}, narrowRepo(t, "internal/core/b.go", "internal/small/s.go"))
	if got := cmdLines(p); !slices.Equal(got, []string{"go test -count=1 -run ^(TestB1)$ ./internal/core"}) {
		t.Errorf("commands %q", got)
	}
	if p.Changed != 1 || p.Base != "8240ca8a9ffff" {
		t.Errorf("changed %d base %q", p.Changed, p.Base)
	}
}

func TestPlanGoTestWorkingFolder(t *testing.T) {
	in := narrowRepo(t, "internal/core/a.go", "internal/small/s.go")
	in.cwd = filepath.Join(in.root, "internal", "core")
	p := planGoTest([]string{"go", "test"}, in)
	if !p.Narrow || !slices.Equal(cmdLines(p), []string{"go test -count=1 -run ^(TestA1|TestA2)$ ./internal/core"}) {
		t.Errorf("no package in a heavy folder: %+v", p)
	}
	p = planGoTest([]string{"go", "test", "./..."}, in)
	if !p.Narrow || len(p.Cmds) != 1 {
		t.Errorf("./... from the heavy folder covers only it: %q", cmdLines(p))
	}
	p = planGoTest([]string{"go", "test", "../small"}, in)
	if p.Narrow || p.Whole {
		t.Errorf("a light package alone is left alone: %+v", p)
	}
	in.cwd = filepath.Join(in.root, "internal", "small")
	if p = planGoTest([]string{"go", "test"}, in); p.Narrow || p.Whole {
		t.Errorf("no package in a light folder is left alone: %+v", p)
	}
}

func TestPlanGoTestLeavesAlone(t *testing.T) {
	changed := []string{"internal/core/a.go"}
	zero := func(name string, argv []string, edit func(*narrowEnv)) {
		t.Helper()
		in := narrowRepo(t, changed...)
		if edit != nil {
			edit(&in)
		}
		if p := planGoTest(argv, in); p.Narrow || p.Whole || p.All {
			t.Errorf("%s: %+v", name, p)
		}
	}
	zero("-run", []string{"go", "test", "-run", "TestA1", "./internal/core"}, nil)
	zero("-run=", []string{"go", "test", "-run=TestA1", "./internal/core"}, nil)
	zero("-run after packages", []string{"go", "test", "./internal/core", "-run", "TestA1"}, nil)
	zero("-fuzz", []string{"go", "test", "-fuzz", "F", "./internal/core"}, nil)
	zero("-list", []string{"go", "test", "-list", ".", "./internal/core"}, nil)
	zero("make", []string{"make", "check-all"}, nil)
	zero("sh -c", []string{"sh", "-c", "go test ./..."}, nil)
	zero("light package", []string{"go", "test", "./internal/small"}, nil)
	zero("all, std", []string{"go", "test", "std"}, nil)
	zero("no heavy list", []string{"go", "test", "./..."}, func(in *narrowEnv) { in.heavy = nil })
	zero("a .go file", []string{"go", "test", "internal/core/a_test.go"}, nil)
}

func TestPlanGoTestWholeCases(t *testing.T) {
	changed := []string{"internal/core/a.go"}
	argv := []string{"go", "test", "./internal/core"}
	for _, c := range []struct {
		name    string
		edit    func(*narrowEnv)
		all     bool
		why     bool
		message string
	}{
		{"--all or WP_TEST_ALL", func(in *narrowEnv) { in.all = true }, true, false, ""},
		{"a runner job", func(in *narrowEnv) { in.runner = true }, false, false, ""},
		{"no base", func(in *narrowEnv) {
			in.changes = func() (string, []string, error) { return "", nil, errors.New("no origin/main") }
		}, false, true, "no origin/main"},
	} {
		in := narrowRepo(t, changed...)
		c.edit(&in)
		p := planGoTest(argv, in)
		if p.Narrow || !p.Whole || p.All != c.all || (p.Why != "") != c.why || !strings.Contains(p.Why, c.message) {
			t.Errorf("%s: %+v", c.name, p)
		}
	}
}

func TestPlanGoTestNothingSelected(t *testing.T) {
	p := planGoTest([]string{"go", "test", "./internal/core"}, narrowRepo(t, "internal/small/s.go", "docs/x.md"))
	if !p.Narrow || len(p.Cmds) != 0 || p.Whole {
		t.Fatalf("%+v", p)
	}
	lines := narrowNotice("wp testsum", "wp testsum --all --", []string{"go", "test", "-json", "./internal/core"}, p, 0)
	want := []string{
		"wp testsum: no file changed in ./internal/core since the base (8240ca8a9), so no test runs.",
		"  The whole package: wp testsum --all -- go test ./internal/core   (CI runs the whole suite on the pull request)",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("notice:\n%s", strings.Join(lines, "\n"))
	}
	// Files changed, none selecting a test: said apart from nothing changed.
	p = planGoTest([]string{"go", "test", "./internal/core"}, narrowRepo(t, "internal/core/notes.md", "internal/core/z.txt", "internal/small/s.go"))
	lines = narrowNotice("wp testsum", "wp testsum --all --", []string{"go", "test", "./internal/core"}, p, 0)
	if len(p.Cmds) != 0 || !strings.Contains(lines[0], "2 files changed in ./internal/core since the base") ||
		!strings.Contains(lines[0], "none of them selects a test (internal/core/notes.md, internal/core/z.txt)") {
		t.Errorf("changed, selecting nothing:\n%s", strings.Join(lines, "\n"))
	}
	if got := listFiles([]string{"a", "b", "c", "d", "e", "f", "g"}, 5); got != "a, b, c, d, e and 2 more" {
		t.Errorf("listFiles: %q", got)
	}
}

// A changed source file in a heavy package with no test beside it is compiled,
// not skipped, and the notice says so.
func TestPlanGoTestNoTestBesideIsCompiled(t *testing.T) {
	in := narrowRepo(t, "internal/core/c.go")
	if err := os.WriteFile(filepath.Join(in.root, "internal/core/c.go"), []byte("package core\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := planGoTest([]string{"go", "test", "-race", "./internal/core"}, in)
	if got := cmdLines(p); !slices.Equal(got, []string{"go test -count=1 -race -run ^$ ./internal/core"}) {
		t.Fatalf("commands %q", got)
	}
	if !p.Sel.compileOnly() {
		t.Error("not compile-only")
	}
	lines := strings.Join(narrowNotice("wp testsum", "wp testsum --all --", []string{"go", "test", "./internal/core"}, p, 0), "\n")
	if !strings.Contains(lines, "no test sits beside the change, so the package is compiled and no test runs") || !strings.Contains(lines, "No test sits beside: internal/core/c.go") {
		t.Errorf("notice:\n%s", lines)
	}
}

// A testdata file and an embedded file select their package through the slot's
// plan too, even when the asked package names only the package folder.
func TestPlanGoTestSelectsTestdataAndEmbeddedFiles(t *testing.T) {
	in := narrowRepo(t, "internal/small/testdata/in.json", "internal/core/testdata/g.txt")
	for p, s := range map[string]string{
		"internal/small/s.go":     "package small\nimport _ \"embed\"\n//go:embed tpl.html\nvar tpl string\n",
		"internal/small/tpl.html": "x\n",
		"internal/core/b_test.go": "package core\nimport \"testing\"\nfunc TestB1(t *testing.T) { _ = \"testdata/g.txt\" }\n",
	} {
		if err := os.WriteFile(filepath.Join(in.root, p), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := planGoTest([]string{"go", "test", "./..."}, in)
	want := []string{"go test -count=1 ./internal/small", "go test -count=1 -run ^(TestB1)$ ./internal/core"}
	if got := cmdLines(p); !slices.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	in = narrowRepo(t, "internal/small/tpl.html")
	if err := os.WriteFile(filepath.Join(in.root, "internal/small/s.go"), []byte("package small\nimport _ \"embed\"\n//go:embed tpl.html\nvar tpl string\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in.root, "internal/small/tpl.html"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p = planGoTest([]string{"go", "test", "./internal/core", "./internal/small"}, in)
	if got := cmdLines(p); !slices.Equal(got, []string{"go test -count=1 ./internal/small"}) {
		t.Errorf("embedded file in a light package: %q", got)
	}
}

// A flag whose value is a file path is not narrowed; the notice says why.
func TestPlanGoTestFileFlagIsNotNarrowed(t *testing.T) {
	for _, f := range []string{"coverprofile", "o", "cpuprofile", "memprofile", "blockprofile", "mutexprofile", "trace", "outputdir", "modfile", "overlay", "exec"} {
		for _, form := range [][]string{{"-" + f, "x.out"}, {"-" + f + "=x.out"}, {"--" + f, "x.out"}} {
			argv := append(append([]string{"go", "test"}, form...), "./internal/core")
			p := planGoTest(argv, narrowRepo(t, "internal/core/a.go"))
			if p.Narrow || !p.Whole || len(p.Cmds) != 0 || !strings.Contains(p.Why, "-"+f+" names a file") {
				t.Errorf("%v: narrow %v whole %v why %q", form, p.Narrow, p.Whole, p.Why)
			}
		}
	}
	// A light package alone is run as asked without a notice; so is --all.
	if p := planGoTest([]string{"go", "test", "-coverprofile=c.out", "./internal/small"}, narrowRepo(t, "internal/small/s.go")); p.Why != "" || p.Whole {
		t.Errorf("light package: %+v", p)
	}
	in := narrowRepo(t, "internal/core/a.go")
	in.all = true
	if p := planGoTest([]string{"go", "test", "-coverprofile=c.out", "./internal/core"}, in); p.Why != "" || !p.All {
		t.Errorf("--all: %+v", p)
	}
}

func TestNarrowNotice(t *testing.T) {
	in := narrowRepo(t, "internal/core/a.go", "internal/core/b.go", "internal/core/c.go", "internal/small/s.go")
	if err := os.WriteFile(filepath.Join(in.root, "internal/core/c.go"), []byte("package core\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := planGoTest([]string{"go", "test", "./internal/core"}, in)
	want := []string{
		"wp testsum: running the 3 tests beside your 3 changed files in internal/core, not the whole package (base 8240ca8a9).",
		"  No test sits beside: internal/core/c.go (heavy package: only tests in changed test files and beside changed files run).",
		"  The whole package: wp testsum --all -- go test ./internal/core   (about 21 minutes here; CI runs the whole suite on the pull request)",
	}
	got := narrowNotice("wp testsum", "wp testsum --all --", []string{"go", "test", "-json", "./internal/core"}, p, 21*time.Minute+10*time.Second)
	if !slices.Equal(got, want) {
		t.Errorf("notice:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	p = planGoTest([]string{"go", "test", "./..."}, in)
	got = narrowNotice("wp slot", "wp slot run tests --all --", []string{"go", "test", "./..."}, p, 40*time.Second)
	if !strings.Contains(got[0], "the 3 tests beside your 3 changed files in internal/core and every test of internal/small, not the whole of ./...") ||
		!strings.Contains(got[len(got)-1], "Everything asked for: wp slot run tests --all -- go test ./...   (about 40 seconds here;") {
		t.Errorf("many packages:\n%s", strings.Join(got, "\n"))
	}
}

func TestSlotClassFor(t *testing.T) {
	long := []waitRec{{Name: "tests", Repo: "r", Cmd: "go test ./x", Secs: 600}}
	cases := []struct {
		name string
		recs []waitRec
		run  slotRun
		want string
	}{
		{"whole heavy run, first time", nil, slotRun{key: "go test ./internal/core", whole: true}, slotClassLong},
		{"narrowed run, no history", nil, slotRun{key: "go test ./internal/core" + narrowedMark, narrowed: true}, slotClassShort},
		{"-run run, no history", nil, slotRun{key: "go test -run X ./internal/core"}, slotClassShort},
		{"make check-all, no history", nil, slotRun{key: "make check-all"}, slotClassShort},
		{"make check-all, long history", []waitRec{{Name: "tests", Repo: "r", Cmd: "make check-all", Secs: 900}}, slotRun{key: "make check-all"}, slotClassLong},
		{"-run run follows its history", long, slotRun{key: "go test ./x"}, slotClassLong},
	}
	for _, c := range cases {
		if got, _, _ := slotClassFor(c.recs, "tests", "r", c.run.key, c.run); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// And through planGoTest: --all and no heavy file give the flags that decide it.
	in := narrowRepo(t, "internal/core/a.go")
	in.all = true
	if p := planGoTest([]string{"go", "test", "./internal/core"}, in); !p.Whole || p.Narrow {
		t.Errorf("--all: %+v", p)
	}
	in.heavy = nil
	if p := planGoTest([]string{"go", "test", "./internal/core"}, in); p.Whole {
		t.Errorf("no heavy file: %+v", p)
	}
}

// fakeGo puts a go on PATH that logs its arguments and exits with the code
// listed for its turn (one per line in codes).
func fakeGo(t *testing.T, codes ...string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "log")
	script := "#!/bin/sh\necho \"$PWD|$*\" >> '" + log + "'\nn=$(wc -l < '" + log + "')\nsed -n \"${n}p\" '" + filepath.Join(bin, "codes") + "' | { read c; exit ${c:-0}; }\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codes"), []byte(strings.Join(codes, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// narrowedSlot sets up a git checkout with the narrowRepo files, a go on PATH
// that exits with codes in turn, and the paths changed.
func narrowedSlot(t *testing.T, changed []string, codes ...string) (log string) {
	t.Helper()
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	t.Setenv(envTestAll, "")
	repo := sqRepo(t)
	probesAre(t, false, false, false)
	log = fakeGo(t, codes...)
	in := narrowRepoIn(t, repo, changed...)
	old := loadNarrowEnvFn
	loadNarrowEnvFn = func(all bool) (narrowEnv, bool) {
		in.all = all
		return in, true
	}
	t.Cleanup(func() { loadNarrowEnvFn = old })
	return log
}

func TestSlotRunsNarrowedCommandsUnderOneHold(t *testing.T) {
	log := narrowedSlot(t, []string{"internal/core/a.go", "internal/small/s.go"}, "0", "7")
	c, out, errb := sqCLI()
	rec := newTestRunRecorder([]string{"go", "test", "./..."})
	code, err := c.runInSlotWith("tests", 2, []string{"go", "test", "-json", "./..."}, out, out, nil, nil,
		slotOpts{who: "wp testsum", allCmd: "wp testsum --all --", rec: rec})
	if err != nil || code != 7 {
		t.Fatalf("code %d err %v, want the second command's 7", code, err)
	}
	logged, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "|test -count=1 -json ./internal/small") || !strings.HasSuffix(lines[1], "-json -run ^(TestA1|TestA2)$ ./internal/core") {
		t.Errorf("go ran:\n%s", logged)
	}
	if !strings.HasPrefix(errb.String(), "wp testsum: running the 2 tests beside your 1 changed file in internal/core and every test of internal/small, not the whole of ./... (base 8240ca8a9).") {
		t.Errorf("stderr:\n%s", errb)
	}
	if rec.command() != "go test ./..."+scopedMark {
		t.Errorf("evidence says %q", rec.command())
	}
	dir, _ := slotDir()
	recs := readWaitRecs(dir)
	if len(recs) != 1 || !recs[0].Narrowed || recs[0].All || recs[0].Cmd != "go test -json ./..."+narrowedMark {
		t.Errorf("records %+v", recs)
	}
}

func TestSlotNarrowedRunKeepsItsOwnTimes(t *testing.T) {
	narrowedSlot(t, []string{"internal/core/a.go"}, "0")
	c, out, _ := sqCLI()
	code, err := c.runInSlotWith("tests", 2, []string{"go", "test", "./internal/core"}, out, out, nil, nil, slotOpts{})
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	dir, _ := slotDir()
	runs := slotRuns(dir)
	if len(runs) != 1 || runs[0].Cmd != "go test ./internal/core"+narrowedMark {
		t.Errorf("runs.jsonl %+v: the narrowed run must not be filed under the whole command", runs)
	}
	if recs := readWaitRecs(dir); len(recs) != 1 || recs[0].Cmd != "go test -json ./internal/core"+narrowedMark || recs[0].Class != slotClassShort {
		t.Errorf("waits %+v", recs)
	}
}

func TestSlotAllRunsEverythingAndIsLong(t *testing.T) {
	log := narrowedSlot(t, []string{"internal/core/a.go"}, "0")
	c, out, errb := sqCLI()
	code, err := c.runInSlotWith("tests", 2, []string{"go", "test", "./internal/core"}, out, out, nil, nil, slotOpts{all: true})
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	logged, _ := os.ReadFile(log)
	if strings.TrimSpace(string(logged))[strings.Index(string(logged), "|"):] != "|test ./internal/core" {
		t.Errorf("go ran %q, want the command as given", logged)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr %q", errb)
	}
	dir, _ := slotDir()
	recs := readWaitRecs(dir)
	if len(recs) != 1 || recs[0].Narrowed || !recs[0].All || recs[0].Class != slotClassLong || recs[0].Cmd != "go test -json ./internal/core" {
		t.Errorf("records %+v: want an --all record, long on its first run", recs)
	}
}

func TestSlotWPTestAllReachesTheSlot(t *testing.T) {
	log := narrowedSlot(t, []string{"internal/core/a.go"}, "0")
	t.Setenv(envTestAll, "1")
	c, out, _ := sqCLI()
	// The real loader reads the variable; the test loader takes the flag, so
	// check the real one's rule on its own.
	in, ok := loadNarrowEnv(false)
	if !ok || !in.all {
		t.Fatalf("WP_TEST_ALL=1 isn't read as --all: %+v %v", in.all, ok)
	}
	if _, dropped := testEnv([]string{envTestAll + "=1"}, nil); len(dropped) != 0 {
		t.Errorf("testEnv drops %v, so a nested run would not see it", dropped)
	}
	if code, err := c.runInSlotWith("tests", 2, []string{"go", "test", "./internal/core"}, out, out, nil, nil, slotOpts{all: true}); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if logged, _ := os.ReadFile(log); strings.Contains(string(logged), "-run") {
		t.Errorf("narrowed: %s", logged)
	}
}

func TestSlotNarrowedToNothingRunsNothing(t *testing.T) {
	log := narrowedSlot(t, []string{"internal/small/s.go"})
	c, out, errb := sqCLI()
	var nothing bool
	code, err := c.runInSlotWith("tests", 2, []string{"go", "test", "./internal/core"}, out, out, nil, nil, slotOpts{who: "wp testsum", allCmd: "wp testsum --all --", nothing: &nothing})
	if err != nil || code != 0 || !nothing {
		t.Fatalf("code %d err %v nothing %v", code, err, nothing)
	}
	if _, serr := os.Stat(log); serr == nil {
		t.Error("go ran")
	}
	if !strings.Contains(errb.String(), "wp testsum: no file changed in ./internal/core since the base") || !strings.Contains(errb.String(), "wp testsum --all -- go test ./internal/core") {
		t.Errorf("stderr:\n%s", errb)
	}
	dir, _ := slotDir()
	if recs := readWaitRecs(dir); len(recs) != 0 {
		t.Errorf("a run that took no slot left records %+v", recs)
	}
}

func TestNonGoTestIsNotNarrowed(t *testing.T) {
	log := narrowedSlot(t, nil)
	c, out, errb := sqCLI()
	code, err := c.runInSlotWith("tests", 2, []string{"sh", "-c", "exit 3"}, out, out, nil, nil, slotOpts{})
	if err != nil || code != 3 || errb.Len() != 0 {
		t.Errorf("code %d err %v stderr %q", code, err, errb)
	}
	if _, serr := os.Stat(log); serr == nil {
		t.Error("go ran")
	}
	// Another slot name is never narrowed.
	code, err = c.runInSlotWith("other", 2, []string{"go", "test", "./..."}, out, out, nil, nil, slotOpts{})
	if err != nil || code != 0 || errb.Len() != 0 {
		t.Errorf("other slot: code %d err %v stderr %q", code, err, errb)
	}
}

func TestSlotLsCountsNarrowedAndAll(t *testing.T) {
	slotEnv(t)
	c, out, _ := sqCLI()
	dir, _ := slotDir()
	now := time.Now().Unix()
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/r/.git", Cmd: "a" + narrowedMark, Class: slotClassShort, At: now, Narrowed: true})
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/r/.git", Cmd: "a" + narrowedMark, Class: slotClassShort, At: now, Narrowed: true})
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/r/.git", Cmd: "a", Class: slotClassLong, At: now, All: true})
	if err := cmdSlotLs(c, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "go test runs narrowed to the diff: 2; run whole with --all or WP_TEST_ALL=1: 1") {
		t.Errorf("ls:\n%s", out)
	}
}

// A cancel between the narrowed commands: the next one does not start, and
// the run ends with the code of a signalled one.
func TestSlotNarrowedCancelBetweenCommandsSkipsTheRest(t *testing.T) {
	log := narrowedSlot(t, []string{"internal/core/a.go", "internal/small/s.go"}, "0", "0")
	// The first go stops the run as Ctrl-C does (the slot's context catches
	// SIGTERM) and gives it a moment to arrive.
	script := "#!/bin/sh\necho \"$PWD|$*\" >> '" + log + "'\nkill -TERM $PPID\nsleep 0.5\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(log), "go"), []byte(script), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	c, out, _ := sqCLI()
	code, err := c.runInSlotWith("tests", 2, []string{"go", "test", "./..."}, out, out, nil, nil, slotOpts{who: "wp testsum", allCmd: "wp testsum --all --"})
	if err != nil || code != 128+15 {
		t.Fatalf("code %d err %v, want 143", code, err)
	}
	if logged, _ := os.ReadFile(log); strings.Count(string(logged), "\n") != 1 {
		t.Errorf("go ran:\n%s, want only the first command", logged)
	}
}
