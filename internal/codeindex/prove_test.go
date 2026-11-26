package codeindex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const calcOK = "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
const calcBug = "package calc\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"

// calcRepo is a Go module whose Add broke in one commit (returned).
func calcRepo(t *testing.T) (*repo, string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	r := newRepo(t)
	r.write("go.mod", "module example.com/calc\n\ngo 1.21\n")
	r.write("calc/calc.go", calcOK)
	r.write("calc/old_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestZero(t *testing.T) {\n\tif Add(0, 0) != 0 {\n\t\tt.Fatal(\"zero\")\n\t}\n}\n")
	r.commit("start", 30)
	r.write("calc/calc.go", calcBug)
	r.commit("Rework Add (#3)", 20)
	return r, r.head()
}

func prove(t *testing.T, r *repo, intro string) *FixProof {
	t.Helper()
	got, err := ProveFix(context.Background(), r.dir, ProveInput{Fix: FixInput{Merge: r.head()}, Intro: intro})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestProveFixFailsWithoutTheFixAndConfirmsTheBlame(t *testing.T) {
	r, intro := calcRepo(t)
	r.write("calc/calc.go", calcOK)
	r.write("calc/add_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 1) != 3 {\n\t\tt.Fatal(\"2+1\")\n\t}\n}\n\nfunc TestAddZero(t *testing.T) {\n\tif Add(2, 0) != 2 {\n\t\tt.Fatal(\"2+0\")\n\t}\n}\n")
	r.commit("Fix Add (#4)", 1)
	got := prove(t, r, intro)
	if got.Outcome != ProofProven || !slices.Equal(got.Failed, []string{"calc.TestAdd"}) {
		t.Fatalf("proof = %+v", got)
	}
	if !slices.Equal(got.Tests, []string{"calc.TestAdd", "calc.TestAddZero"}) {
		t.Fatalf("tests = %v", got.Tests)
	}
	if got.IntroCheck != IntroConfirmed || got.Intro != intro {
		t.Fatalf("intro check = %q on %s", got.IntroCheck, got.Intro)
	}
	if st := r.git("status", "--porcelain"); st != "" {
		t.Fatalf("the checkout was touched:\n%s", st)
	}
}

func TestProveFixRefutesABlameTheTestPassesOn(t *testing.T) {
	r, _ := calcRepo(t)
	r.write("calc/other.go", "package calc\n\nvar Other = 1\n")
	r.commit("Add Other (#5)", 10)
	other := r.head()
	r.write("calc/calc.go", calcOK)
	r.write("calc/add_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 1) != 3 {\n\t\tt.Fatal(\"2+1\")\n\t}\n}\n")
	r.commit("Fix Add (#6)", 1)
	if got := prove(t, r, other); got.Outcome != ProofProven || got.IntroCheck != IntroBefore {
		t.Fatalf("proof = %+v", got)
	}
}

func TestProveFixOutcomes(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"passes", map[string]string{"calc/calc.go": calcOK, "calc/add_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestAddZeros(t *testing.T) {\n\tif Add(0, 0) != 0 {\n\t\tt.Fatal(\"0\")\n\t}\n}\n"}, ProofPasses},
		{"no build", map[string]string{"calc/calc.go": calcOK + "\nfunc Sum(v ...int) int { return 0 }\n", "calc/sum_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n\tif Sum() != 0 {\n\t\tt.Fatal(\"sum\")\n\t}\n}\n"}, ProofNoBuild},
		{"head fails", map[string]string{"calc/calc.go": calcOK, "calc/add_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tt.Fatal(\"always\")\n}\n"}, ProofHeadFails},
		{"no test", map[string]string{"calc/calc.go": calcOK}, ProofNoTest},
		{"python", map[string]string{"calc/calc.go": calcOK, "tests/test_calc.py": "def test_add():\n    pass\n"}, ProofUnsupported},
		{"tests only", map[string]string{"calc/old_test.go": "package calc\n"}, ProofNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := calcRepo(t)
			for p, body := range c.files {
				r.write(p, body)
			}
			r.commit("Fix (#7)", 1)
			if got := prove(t, r, ""); got.Outcome != c.want {
				t.Fatalf("outcome = %q, want %q: %+v", got.Outcome, c.want, got)
			}
		})
	}
}

func TestProveFixInTheRootPackage(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	r := newRepo(t)
	r.write("go.mod", "module example.com/calc\n\ngo 1.21\n")
	r.write("calc.go", calcBug)
	r.commit("start", 30)
	r.write("calc.go", calcOK)
	r.write("add_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 1) != 3 {\n\t\tt.Fatal(\"2+1\")\n\t}\n}\n")
	r.commit("Fix Add (#2)", 1)
	if got := prove(t, r, ""); got.Outcome != ProofProven || !slices.Equal(got.Failed, []string{"TestAdd"}) {
		t.Fatalf("proof = %+v", got)
	}
}

func TestTouchedTests(t *testing.T) {
	src := "package x\n\nimport \"testing\"\n\nfunc helper() {}\n\nfunc TestA(t *testing.T) {\n\thelper()\n}\n\nfunc TestB(t *testing.T) {}\n\nfunc Testlower(t *testing.T) {}\n\nfunc TestMain(m *testing.M) {}\n"
	if got := touchedTests(src, []int{8}); !slices.Equal(got, []string{"TestA"}) {
		t.Fatalf("a line in TestA: %v", got)
	}
	if got := touchedTests(src, []int{5}); !slices.Equal(got, []string{"TestA", "TestB"}) {
		t.Fatalf("a helper: %v", got)
	}
}

func TestIsTestPath(t *testing.T) {
	for p, want := range map[string]bool{
		"internal/core/test_evidence.go":      false,
		"internal/core/test_evidence_test.go": true,
		"app/test_calc.py":                    true,
		"web/src/a.test.ts":                   true,
		"internal/core/escapes.go":            false,
	} {
		if got := isTestPath(p); got != want {
			t.Errorf("isTestPath(%q) = %v", p, got)
		}
	}
}

// A test binary that fails before running a test (here TestMain exits) built,
// so without the fix its tests fail rather than not building.
func TestProveFixCountsABinaryThatFailsAtStartAsFailing(t *testing.T) {
	r, _ := calcRepo(t)
	r.write("calc/ready.go", "package calc\n\nvar Ready = false\n")
	r.commit("Add Ready (#5)", 10)
	r.write("calc/ready.go", "package calc\n\nvar Ready = true\n")
	r.write("calc/main_test.go", "package calc\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {\n\tif !Ready {\n\t\tos.Exit(1)\n\t}\n\tos.Exit(m.Run())\n}\n\nfunc TestReady(t *testing.T) {}\n")
	r.commit("Fix Ready (#6)", 1)
	got := prove(t, r, "")
	if got.Outcome != ProofProven || !slices.Equal(got.Failed, []string{"calc.TestReady"}) || !strings.Contains(got.Note, "fail before running") {
		t.Fatalf("proof = %+v", got)
	}
}

// A package directory that is a symlink in the code before the fix is
// replaced in the temporary tree, not followed out of it.
func TestProveFixDoesNotFollowASymlinkedPackage(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep_test.go")
	if err := os.WriteFile(keep, []byte("package calc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRepo(t)
	r.write("go.mod", "module example.com/calc\n\ngo 1.21\n")
	r.write("README", "calc\n")
	if err := os.Symlink(outside, filepath.Join(r.dir, "calc")); err != nil {
		t.Fatal(err)
	}
	r.commit("start", 30)
	if err := os.Remove(filepath.Join(r.dir, "calc")); err != nil {
		t.Fatal(err)
	}
	r.write("calc/calc.go", calcOK)
	r.write("calc/add_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 1) != 3 {\n\t\tt.Fatal(\"2+1\")\n\t}\n}\n")
	r.commit("Fix Add (#2)", 1)
	if got := prove(t, r, ""); got.Outcome != ProofNoBuild {
		t.Fatalf("proof = %+v", got)
	}
	ents, err := os.ReadDir(outside)
	if err != nil || len(ents) != 1 || ents[0].Name() != "keep_test.go" {
		t.Fatalf("the symlink's target was touched: %v %v", ents, err)
	}
}

// Past maxProvePackages, packages whose code the fix changed run first and
// the note names the ones left out.
func TestProvingTestsSaysWhatItLeftOut(t *testing.T) {
	r := newRepo(t)
	r.write("go.mod", "module example.com/m\n\ngo 1.21\n")
	r.write("z/z.go", "package z\n")
	r.commit("start", 30)
	from := r.head()
	r.write("z/z.go", "package z\n\nvar Fixed = true\n")
	for _, d := range []string{"a", "b", "c", "d", "e", "f", "g", "z"} {
		r.write(d+"/x_test.go", "package "+d+"\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")
	}
	r.commit("Fix z (#2)", 1)
	fr := &fixRepo{root: r.dir, chains: map[string]map[string]bool{}, sizes: map[string]int{}}
	pkgs, outcome, note, err := fr.provingTests(from, r.head())
	if err != nil || outcome != "" {
		t.Fatalf("outcome %q, err %v", outcome, err)
	}
	var dirs []string
	for _, p := range pkgs {
		dirs = append(dirs, p.dir)
	}
	if !slices.Equal(dirs, []string{"z", "a", "b", "c", "d", "e"}) {
		t.Fatalf("dirs = %v", dirs)
	}
	if note != "only part of the fix's tests ran; left out: f (1), g (1)" {
		t.Fatalf("note = %q", note)
	}
}
