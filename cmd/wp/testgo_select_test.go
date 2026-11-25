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
			goSelection{Packages: []string{"./heavy"}, NoTests: []string{"heavy/bar.go"}}},
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
