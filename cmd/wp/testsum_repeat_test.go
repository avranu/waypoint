package main

import (
	"bytes"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// flakyModule is a Go module whose TestFlaky fails on the runs listed in
// FAIL_ON (counted in a file), so a flake is reproducible.
func flakyModule(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module flaky\n\ngo 1.22\n",
		"f_test.go": `package flaky

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestFlaky(t *testing.T) {
	p := os.Getenv("RUNS_FILE")
	b, _ := os.ReadFile(p)
	n := len(b) + 1
	_ = os.WriteFile(p, append(b, 'x'), 0o600)
	for _, f := range strings.Split(os.Getenv("FAIL_ON"), ",") {
		if f == strconv.Itoa(n) {
			t.Fatalf("run %d unlucky", n)
		}
	}
}

func TestSolid(t *testing.T) {}
`,
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("RUNS_FILE", filepath.Join(t.TempDir(), "runs"))
	t.Setenv("GOFLAGS", "-mod=mod")
	return dir
}

func TestTestsumRepeatCountsFlakes(t *testing.T) {
	slotEnv(t)
	t.Chdir(flakyModule(t))
	t.Setenv("FAIL_ON", "2,4")
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "--repeat", "6", "--", "go", "test", "."}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out.String(), errb.String())
	}
	got := out.String()
	for _, want := range []string{"FLAKY: 2 of 6 runs failed", "flaky.TestFlaky failed 2 of 6", "run 2, the first that failed", "run 2 unlucky"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "TestSolid") {
		t.Errorf("a passing test is listed:\n%s", got)
	}
}

func TestTestsumRepeatUntilFailStops(t *testing.T) {
	slotEnv(t)
	t.Chdir(flakyModule(t))
	t.Setenv("FAIL_ON", "3,5")
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "--repeat", "10", "--until-fail", "--", "go", "test", "."}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "FAIL on run 3 of up to 10: 2 passed before it") {
		t.Errorf("got:\n%s", out.String())
	}
	if b, _ := os.ReadFile(os.Getenv("RUNS_FILE")); len(b) != 3 {
		t.Errorf("ran %d times, want 3", len(b))
	}
}

func TestTestsumRepeatAllPass(t *testing.T) {
	slotEnv(t)
	t.Chdir(flakyModule(t))
	t.Setenv("FAIL_ON", "")
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "--repeat", "3", "--", "go", "test", "."}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "PASS: 3 of 3 runs") || !strings.Contains(out.String(), "2 tests") {
		t.Errorf("got:\n%s", out.String())
	}
}

func TestTestsumRepeatUsage(t *testing.T) {
	slotEnv(t)
	saved := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(saved, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"testsum", "--repeat", "3"},
		{"testsum", "--repeat", "0", "--", "true"},
		{"testsum", "--until-fail", "--", "true"},
		{"testsum", "--repeat", "3", saved},
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, errb.String())
		}
	}
}

func TestWilson(t *testing.T) {
	for _, c := range []struct {
		k, n   int
		lo, hi float64
	}{
		{0, 30, 0, 0.1135},
		{3, 12, 0.0889, 0.5323},
		{10, 10, 0.7225, 1},
	} {
		lo, hi := wilson(c.k, c.n)
		if math.Abs(lo-c.lo) > 0.001 || math.Abs(hi-c.hi) > 0.001 {
			t.Errorf("wilson(%d, %d) = %.4f–%.4f, want %.4f–%.4f", c.k, c.n, lo, hi, c.lo, c.hi)
		}
	}
}

func TestGoTestCount1(t *testing.T) {
	for in, want := range map[string]string{
		"go test ./...":              "go test -count=1 ./...",
		"go test -count=3 ./...":     "go test -count=3 ./...",
		"go test ./x -args -count=2": "go test -count=1 ./x -args -count=2",
		"pytest -q":                  "pytest -q",
	} {
		if got := strings.Join(goTestCount1(strings.Fields(in)), " "); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestTestsumRepeatNoTestsRanIsNoPass(t *testing.T) {
	slotEnv(t)
	t.Chdir(flakyModule(t))
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "--repeat", "2", "--", "go", "test", "-run", "TestFlkay", "."}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "NO TESTS RAN in any of 2 runs") || strings.Contains(out.String(), "would likely have shown") {
		t.Errorf("got:\n%s", out.String())
	}
}
