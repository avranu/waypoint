package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// putFiles writes name: text pairs under root, making folders.
func putFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	srcGo   = "package x\n"
	testsGo = "package x\nimport \"testing\"\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}\nfunc helper(t *testing.T) {}\nfunc Testing() {}\nfunc TestMain(m *testing.M) {}\nfunc FuzzF(f *testing.F) {}\nfunc ExampleE() {}\nfunc (T) TestM(t *testing.T) {}\n"
)

func selectionModule(t *testing.T) string {
	root := t.TempDir()
	putFiles(t, root, map[string]string{
		"go.mod":                          "module m\n",
		"small/a.go":                      srcGo,
		"small/a_test.go":                 testsGo,
		"heavy/foo.go":                    srcGo,
		"heavy/foo_test.go":               "package x\nimport \"testing\"\nfunc TestFoo(t *testing.T) {}\n",
		"heavy/foo_extra_test.go":         "package x\nimport \"testing\"\nfunc TestFooExtra(t *testing.T) {}\n",
		"heavy/foo_linux.go":              srcGo,
		"heavy/bar.go":                    srcGo,
		"heavy/other_test.go":             "package x\nimport \"testing\"\nfunc TestOther(t *testing.T) {}\n",
		"heavy/helpers_test.go":           srcGo,
		"heavy/foobar_test.go":            "package x\nimport \"testing\"\nfunc TestFooBar(t *testing.T) {}\n",
		"other/c.go":                      srcGo,
		"nested/go.mod":                   "module n\n",
		"nested/n.go":                     srcGo,
		"small/testdata/t.go":             srcGo,
		"vendor/v/v.go":                   srcGo,
		"docs/readme.md":                  "x\n",
		"root.go":                         srcGo,
		"heavy/foo_windows_amd64_test.go": "package x\nimport \"testing\"\nfunc TestWin(t *testing.T) {}\n",
	})
	return root
}

func TestSelectGoTests(t *testing.T) {
	root := selectionModule(t)
	heavy := []string{"./heavy/"}
	tests := []struct {
		name    string
		changed []string
		want    goSelection
	}{
		{"only non-Go changes", []string{"docs/readme.md", "web/x.ts", "Makefile"}, goSelection{}},
		{"a changed package runs whole", []string{"small/a.go"}, goSelection{Packages: []string{"./small"}, Whole: []string{"./small"}}},
		{"a changed test file of a whole package runs the package", []string{"small/a_test.go", "other/c.go"},
			goSelection{Packages: []string{"./other", "./small"}, Whole: []string{"./other", "./small"}}},
		{"heavy: a changed test file runs its tests", []string{"heavy/other_test.go"},
			goSelection{Packages: []string{"./heavy"}, Heavy: []heavyRun{{"./heavy", []string{"TestOther"}}}}},
		{"heavy: source runs the test files beside it", []string{"heavy/foo.go"},
			goSelection{Packages: []string{"./heavy"}, Heavy: []heavyRun{{"./heavy", []string{"TestFoo", "TestFooExtra", "TestWin"}}}}},
		{"heavy: a platform file runs the tests of its stem and of itself", []string{"heavy/foo_linux.go"},
			goSelection{Packages: []string{"./heavy"}, Heavy: []heavyRun{{"./heavy", []string{"TestFoo", "TestFooExtra", "TestWin"}}}}},
		{"heavy: source with no tests beside is listed", []string{"heavy/bar.go"},
			goSelection{Packages: []string{"./heavy"}, NoTests: []string{"heavy/bar.go"}, Compile: []string{"./heavy"}}},
		{"heavy: a test file with no tests in it is listed", []string{"heavy/helpers_test.go", "heavy/foo.go"},
			goSelection{Packages: []string{"./heavy"}, Heavy: []heavyRun{{"./heavy", []string{"TestFoo", "TestFooExtra", "TestWin"}}}, NoTests: []string{"heavy/helpers_test.go"}}},
		{"a deleted package is skipped", []string{"gone/g.go", "gone/g_test.go"}, goSelection{}},
		{"a deleted file in a package that is still there", []string{"small/deleted.go"}, goSelection{Packages: []string{"./small"}, Whole: []string{"./small"}}},
		{"testdata, vendor and other modules are skipped", []string{"small/testdata/t.go", "vendor/v/v.go", "nested/n.go", "../outside/o.go"}, goSelection{}},
		{"the root package", []string{"root.go"}, goSelection{Packages: []string{"."}, Whole: []string{"."}}},
		{"whole and heavy together", []string{"small/a.go", "heavy/foo_test.go"},
			goSelection{Packages: []string{"./heavy", "./small"}, Whole: []string{"./small"}, Heavy: []heavyRun{{"./heavy", []string{"TestFoo"}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectGoTests(root, tt.changed, heavy)
			got.Files, got.HeavyFiles = 0, 0 // counted in TestSelectGoTestsDataFiles
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestTopLevelTestsFollowsGoTest(t *testing.T) {
	root := t.TempDir()
	putFiles(t, root, map[string]string{"a_test.go": testsGo, "bad_test.go": "package x\nfunc ("})
	if got, want := topLevelTests(filepath.Join(root, "a_test.go")), []string{"TestA", "TestB", "FuzzF", "ExampleE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := topLevelTests(filepath.Join(root, "bad_test.go")); got != nil {
		t.Errorf("a file that doesn't parse has tests: %v", got)
	}
}

func TestGoSelectionCommands(t *testing.T) {
	sel := goSelection{
		Whole: []string{"./a", "./b"},
		Heavy: []heavyRun{{"./h1", []string{"TestX", "TestY"}}, {"./h2", []string{"TestY", "TestZ"}}},
	}
	got := sel.commands([]string{"-vet=off", "-timeout", "30m"})
	want := [][]string{
		{"go", "test", "-count=1", "-vet=off", "-timeout", "30m", "./a", "./b"},
		{"go", "test", "-count=1", "-vet=off", "-timeout", "30m", "-run", "^(TestX|TestY|TestZ)$", "./h1", "./h2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if got := (goSelection{}).commands(nil); len(got) != 0 {
		t.Errorf("nothing selected: %v", got)
	}
}

func TestGoSelectionSplitsALongRunByPackage(t *testing.T) {
	long := strings.Repeat("x", maxRunRegexLen/2+10)
	sel := goSelection{Heavy: []heavyRun{{"./h1", []string{"TestA" + long}}, {"./h2", []string{"TestB" + long}}}}
	got := sel.commands(nil)
	if len(got) != 2 || got[0][len(got[0])-1] != "./h1" || got[1][len(got[1])-1] != "./h2" {
		t.Fatalf("not split by package: %d commands", len(got))
	}
	for _, argv := range got {
		if len(argv[4]) > maxRunRegexLen {
			t.Errorf("-run argument is %d bytes", len(argv[4]))
		}
	}
	names := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		names = append(names, "Test"+strings.Repeat("y", maxRunRegexLen/10)+string(rune('A'+i%26))+strings.Repeat("z", i))
	}
	if chunks := chunkNames(names); len(chunks) < 2 {
		t.Errorf("one package over the limit is one command: %d chunks", len(chunks))
	}
}

func TestFileStems(t *testing.T) {
	for in, want := range map[string][]string{
		"foo":             {"foo"},
		"foo_linux":       {"foo_linux", "foo"},
		"foo_linux_amd64": {"foo_linux_amd64", "foo_linux", "foo"},
		"foo_bar":         {"foo_bar"},
		"linux":           {"linux"},
	} {
		if got := fileStems(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", in, got, want)
		}
	}
}

// dataModule is a module with a heavy and a light package that have testdata
// and embedded files.
func dataModule(t *testing.T) string {
	root := t.TempDir()
	putFiles(t, root, map[string]string{
		"go.mod":                           "module m\n",
		"light/l.go":                       "package x\nimport _ \"embed\"\n//go:embed tmpl/page.html\nvar page string\n",
		"light/tmpl/page.html":             "x\n",
		"light/testdata/deep/fixture.json": "{}\n",
		"heavy/foo.go":                     srcGo,
		"heavy/foo_test.go":                "package x\nimport \"testing\"\nfunc TestFoo(t *testing.T) {}\n",
		"heavy/other_test.go":              "package x\nimport \"testing\"\nfunc TestOther(t *testing.T) { _ = \"testdata/golden.txt\" }\n",
		"heavy/embed.go":                   "package x\nimport _ \"embed\"\n//go:embed schema.sql\nvar schema string\n",
		"heavy/embed_test.go":              "package x\nimport \"testing\"\nfunc TestEmbed(t *testing.T) {}\n",
		"heavy/schema.sql":                 "x\n",
		"heavy/stray.txt":                  "x\n",
		"heavy/testdata/golden.txt":        "x\n",
		"heavy/testdata/unused.txt":        "x\n",
		"heavy/sub/s.go":                   srcGo,
		"heavy/sub/testdata/x.txt":         "x\n",
		"loose/testdata/y.txt":             "x\n",
	})
	return root
}

func TestSelectGoTestsDataFiles(t *testing.T) {
	root := dataModule(t)
	heavy := []string{"heavy"}
	embeds := func(string) (embedIndex, error) {
		return embedIndex{
			owners: map[string][]string{"light/tmpl/page.html": {"light"}, "heavy/schema.sql": {"heavy"}, "heavy/sub/gone.txt": {"heavy/sub"}},
			dirs:   map[string][]string{"light/tmpl": {"light"}, "heavy": {"heavy"}, "heavy/sub": {"heavy/sub"}},
		}, nil
	}
	tests := []struct {
		name    string
		changed []string
		want    goSelection
	}{
		{"light: a testdata file runs the package whole", []string{"light/testdata/deep/fixture.json"},
			goSelection{Packages: []string{"./light"}, Whole: []string{"./light"}, Files: 1}},
		{"light: an embedded file runs the package whole", []string{"light/tmpl/page.html"},
			goSelection{Packages: []string{"./light"}, Whole: []string{"./light"}, Files: 1}},
		{"heavy: a testdata file runs the tests whose file names it", []string{"heavy/testdata/golden.txt"},
			goSelection{Packages: []string{"./heavy"}, Heavy: []heavyRun{{"./heavy", []string{"TestOther"}}}, Files: 1, HeavyFiles: 1}},
		{"heavy: a deleted testdata file selects the same", []string{"heavy/testdata/golden.txt", "heavy/testdata/removed/golden.txt"},
			goSelection{Packages: []string{"./heavy"}, Heavy: []heavyRun{{"./heavy", []string{"TestOther"}}}, NoTests: nil, Files: 2, HeavyFiles: 2}},
		{"heavy: a testdata file no test file names has no test", []string{"heavy/testdata/unused.txt"},
			goSelection{Packages: []string{"./heavy"}, NoTests: []string{"heavy/testdata/unused.txt"}, Compile: []string{"./heavy"}, Files: 1, HeavyFiles: 1}},
		{"heavy: an embedded file selects the source that embeds it, and its beside-tests", []string{"heavy/schema.sql"},
			goSelection{Packages: []string{"./heavy"}, Heavy: []heavyRun{{"./heavy", []string{"TestEmbed"}}}, Files: 1, HeavyFiles: 1}},
		{"testdata belongs to the nearest package above it (here light, not heavy)", []string{"heavy/sub/testdata/x.txt"},
			goSelection{Packages: []string{"./heavy/sub"}, Whole: []string{"./heavy/sub"}, Files: 1}},
		{"testdata with no package above selects nothing", []string{"loose/testdata/y.txt"}, goSelection{}},
		{"a go file in testdata selects nothing", []string{"light/testdata/t.go"}, goSelection{}},
		{"a deleted embedded file takes the packages embedding in its folder", []string{"heavy/sub/gone.txt"},
			goSelection{Packages: []string{"./heavy/sub"}, Whole: []string{"./heavy/sub"}, Files: 1}},
		{"other non-Go files select nothing", []string{"heavy/stray.txt", "docs/a.md", "web/src/x.tsx", "scripts/run.sh"}, goSelection{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var warn strings.Builder
			got := selectGoTestsIn(root, tt.changed, heavy, nil, embeds, &warn)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
			if warn.Len() > 0 {
				t.Errorf("warned: %s", warn.String())
			}
		})
	}
}

func TestSelectGoTestsListsEmbedsOnceAndOnlyWhenNeeded(t *testing.T) {
	root := dataModule(t)
	calls := 0
	embeds := func(string) (embedIndex, error) { calls++; return embedIndex{}, nil }
	selectGoTestsIn(root, []string{"heavy/foo.go", "light/testdata/deep/fixture.json", "light/l.go"}, nil, nil, embeds, os.Stderr)
	if calls != 0 {
		t.Errorf("go list asked %d times with no embeddable path", calls)
	}
	selectGoTestsIn(root, []string{"a.txt", "b.sql", "c/d.html"}, nil, nil, embeds, os.Stderr)
	if calls != 1 {
		t.Errorf("go list asked %d times, want once", calls)
	}
}

func TestSelectGoTestsGoListFailureIsSaidAndSelectsNothing(t *testing.T) {
	root := dataModule(t)
	var warn strings.Builder
	got := selectGoTestsIn(root, []string{"light/tmpl/page.html", "light/l.go"}, nil, nil,
		func(string) (embedIndex, error) { return embedIndex{}, os.ErrInvalid }, &warn)
	if !reflect.DeepEqual(got.Whole, []string{"./light"}) || got.Files != 1 {
		t.Errorf("only the Go file selects: %+v", got)
	}
	if !strings.Contains(warn.String(), "can't list the files packages embed") {
		t.Errorf("failure not said: %q", warn.String())
	}
}

func TestSelectGoTestsInHonoursTheMatcher(t *testing.T) {
	root := dataModule(t)
	got := selectGoTestsIn(root, []string{"light/testdata/deep/fixture.json", "heavy/foo.go"}, []string{"heavy"},
		func(dir string) bool { return dir == "light" }, nil, os.Stderr)
	if !reflect.DeepEqual(got.Packages, []string{"./light"}) {
		t.Errorf("packages %v", got.Packages)
	}
}

func TestListEmbedsReadsGoList(t *testing.T) {
	root := dataModule(t)
	idx, err := listEmbeds(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.ownersOf(root, "light/tmpl/page.html"); !reflect.DeepEqual(got, []string{"light"}) {
		t.Errorf("owners of the template: %v", got)
	}
	if got := idx.ownersOf(root, "heavy/schema.sql"); !reflect.DeepEqual(got, []string{"heavy"}) {
		t.Errorf("owners of the schema: %v", got)
	}
	if got := idx.ownersOf(root, "heavy/stray.txt"); got != nil {
		t.Errorf("a file nothing embeds has owners: %v", got)
	}
}

// The repository's own migrations are embedded by internal/db; no rule of
// their own selects it.
func TestRepositoryMigrationsSelectInternalDB(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(filepath.Join(root, "internal/db/migrations/*.sql"))
	if len(m) == 0 {
		t.Skip("no migrations here")
	}
	rel, _ := filepath.Rel(root, m[0])
	got := selectGoTests(root, []string{filepath.ToSlash(rel)}, nil)
	if !reflect.DeepEqual(got.Whole, []string{"./internal/db"}) {
		t.Errorf("%s selects %+v", rel, got)
	}
}

func TestGoSelectionCompileOnlyCommand(t *testing.T) {
	sel := goSelection{Packages: []string{"./h"}, Compile: []string{"./h"}, NoTests: []string{"h/new.go"}}
	got := sel.commands([]string{"-vet=off"})
	want := [][]string{{"go", "test", "-count=1", "-vet=off", "-run", "^$", "./h"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if !sel.compileOnly() || sel.testsRun() {
		t.Error("a compile-only selection runs no test")
	}
	if l := sel.noTestsLine(); !strings.Contains(l, "no test runs") || !strings.Contains(l, "h/new.go") {
		t.Errorf("line: %q", l)
	}
	both := goSelection{Heavy: []heavyRun{{"./a", []string{"TestA"}}}, Compile: []string{"./h"}}
	if c := both.commands(nil); len(c) != 2 || c[1][len(c[1])-1] != "./h" || both.compileOnly() {
		t.Errorf("heavy with tests plus a compile: %v", c)
	}
}

func TestUnparsableTestFileIsCompiled(t *testing.T) {
	root := t.TempDir()
	putFiles(t, root, map[string]string{"go.mod": "module m\n", "h/a.go": srcGo, "h/a_test.go": "package x\nfunc ("})
	sel := selectGoTests(root, []string{"h/a_test.go"}, []string{"h"})
	if !sel.compileOnly() || len(sel.commands(nil)) != 1 {
		t.Errorf("an unparsable test file must still be compiled: %+v", sel)
	}
}
