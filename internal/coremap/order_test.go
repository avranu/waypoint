package coremap

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestSuggestOrderChain(t *testing.T) {
	// a uses b, b uses c, c uses d, and d uses a once: the cheapest order puts
	// d first and pays only the one reference d -> a (cost 1).
	w := map[[2]string]int{{"a", "b"}: 5, {"b", "c"}: 5, {"c", "d"}: 5, {"d", "a"}: 1}
	nodes := []string{"c", "a", "d", "b"}
	got := suggestOrder(nodes, w, "")
	if want := []string{"d", "c", "b", "a"}; !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
	if c := upCost(got, w); c != 1 {
		t.Errorf("cost %d, want 1", c)
	}
	if again := suggestOrder([]string{"d", "b", "a", "c"}, w, ""); !slices.Equal(again, got) {
		t.Errorf("order depends on input order: %v vs %v", again, got)
	}
	pinned := suggestOrder(nodes, w, "a")
	if pinned[0] != "a" || len(pinned) != 4 {
		t.Errorf("pinned order %v", pinned)
	}
}

// upCost is the weight of upward references under order.
func upCost(order []string, w map[[2]string]int) int {
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	c := 0
	for k, x := range w {
		pa, ok1 := pos[k[0]]
		pb, ok2 := pos[k[1]]
		if ok1 && ok2 && pb > pa {
			c += x
		}
	}
	return c
}

func TestImproveMovesANode(t *testing.T) {
	// a uses b and c, so it belongs last.
	w := map[[2]string]int{{"a", "b"}: 2, {"a", "c"}: 2}
	order := []string{"a", "b", "c"}
	improve(order, w)
	if c := upCost(order, w); c != 0 || order[2] != "a" {
		t.Errorf("order %v, cost %d, want a last and 0", order, c)
	}
}

func TestSuggestOrderPinsSeveral(t *testing.T) {
	w := map[[2]string]int{{"p", "q"}: 3, {"p", "a"}: 9, {"q", "a"}: 9}
	got := suggestOrder([]string{"a", "p", "q"}, w, "p", "q")
	if want := []string{"q", "p", "a"}; !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

// On a tournament-like graph suggestOrder finds the known optimum.
func TestSuggestOrderBeatsSorted(t *testing.T) {
	// Layers x > y > z > w (x uses y, y uses z, ...) with lighter back references.
	names := []string{"w", "x", "y", "z"}
	w := map[[2]string]int{
		{"x", "y"}: 9, {"x", "z"}: 8, {"x", "w"}: 7,
		{"y", "z"}: 9, {"y", "w"}: 8,
		{"z", "w"}: 9,
		{"w", "x"}: 1, {"z", "y"}: 2,
	}
	got := suggestOrder(names, w, "")
	if want := []string{"w", "z", "y", "x"}; !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
	if c, s := upCost(got, w), upCost(names, w); c > s || c != 3 {
		t.Errorf("cost %d (sorted %d), want 3", c, s)
	}
}

func TestSuggestOrderFixture(t *testing.T) {
	a, _ := fixture(t)
	b := a.SuggestOrder()
	names := b.groupNames()
	if names[0] != "kernel" || names[len(names)-1] != "extra" {
		t.Errorf("order %v: want kernel first and extra last", names)
	}
	r := b.Report(5)
	if r.Summary.Up != 2 || r.Summary.Down != 13 {
		t.Errorf("under the suggested order: up %d down %d, want 2 and 13", r.Summary.Up, r.Summary.Down)
	}
	af := a.Affinities()
	byName := map[string]Affinity{}
	for _, x := range af {
		byName[x.Name] = x
	}
	// pr exchanges 5 + 2 (out) and 2 (in, issues) references with kernel and issues.
	if x := byName["kernel"]; x.Refs != 9 || x.Top[0].Group != "pr" || x.Top[0].Refs != 5 {
		t.Errorf("kernel affinity %+v", x)
	}
	if x := byName["extra"]; x.Refs != 2 || x.Top[0].Share != 50 {
		t.Errorf("extra affinity %+v", x)
	}
}

func TestNamedFileBeatsGlob(t *testing.T) {
	g, err := ParseGroups(strings.NewReader("a: x_*.go\nb: x_one.go\n"))
	if err != nil {
		t.Fatal(err)
	}
	if g.Of("x_one.go") != "b" || g.Of("x_two.go") != "a" {
		t.Errorf("x_one.go in %s, x_two.go in %s", g.Of("x_one.go"), g.Of("x_two.go"))
	}
	if _, err := ParseGroups(strings.NewReader("a: x.go\nb: x.go\n")); err == nil {
		t.Error("a file named twice was accepted")
	}
}

// Every non-test file of internal/core has to be placed by a line of
// groups.txt, so a new file cannot fall silently to the stem rule.
func TestEveryCoreFileIsPlaced(t *testing.T) {
	f, err := os.Open("groups.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g, err := ParseGroups(f)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Placed(kernelGroup) {
		t.Errorf("groups.txt has no %q line", kernelGroup)
	}
	names, err := filepath.Glob(filepath.Join("..", "core", "*.go"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no core files: %v", err)
	}
	var missing []string
	for _, n := range names {
		base := filepath.Base(n)
		if !strings.HasSuffix(base, "_test.go") && !g.Listed(base) {
			missing = append(missing, base)
		}
	}
	// Order must never decide a file's group: a file may match the globs of one
	// line only, unless a line names it.
	for _, n := range names {
		base := filepath.Base(n)
		if m := g.Matches(base); !strings.HasSuffix(base, "_test.go") && len(m) > 1 && !g.Named(base) {
			t.Errorf("%s matches the globs of several lines %v: name it in one", base, m)
		}
	}
	if len(missing) > 0 {
		t.Errorf("files matching no line of internal/coremap/groups.txt (place them; see its header): %s", strings.Join(missing, " "))
	}
}

// A file no line matches that the stem rule puts in a listed group, the kernel
// among them, is named in the summary; Unplaced cannot show it.
func TestStemPlacedFilesAreNamed(t *testing.T) {
	dir := module(t, map[string]string{
		"core.go":         "package m\n",
		"kernel_extra.go": "package m\n",
		"pr.go":           "package m\n",
		"pr_open.go":      "package m\n",
		"other.go":        "package m\n",
	})
	g, err := ParseGroups(strings.NewReader("kernel: core.go\npr: pr_*.go\n"))
	if err != nil {
		t.Fatal(err)
	}
	for perFile, want := range map[bool][]string{
		false: {"kernel_extra.go", "pr.go"},
		true:  {"kernel_extra.go"},
	} {
		g.PerFile = perFile
		ld, err := Load(dir, g, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		a := Analyze(ld, g)
		for _, r := range []Report{a.Report(0), a.SuggestOrder().Report(0)} {
			if got := r.Summary.StemPlaced; !slices.Equal(got, want) {
				t.Errorf("per file %v: stem placed %v, want %v", perFile, got, want)
			}
		}
		var out strings.Builder
		WriteReport(&out, a.Report(0))
		if line := "by their stem (" + strconv.Itoa(len(want)) + "): " + strings.Join(want, " "); !strings.Contains(out.String(), line) {
			t.Errorf("report lacks %q:\n%s", line, out.String())
		}
	}
}

// With PerFile the grouping file's domain names order nothing, even when one
// is also a file's stem, and its kernel files are still pinned and counted
// apart.
func TestPerFile(t *testing.T) {
	g, err := ParseGroups(strings.NewReader("kernel: core.go\nextra_thing: nothing_*.go\n"))
	if err != nil {
		t.Fatal(err)
	}
	g.PerFile = true
	ld, err := Load(fixtureDir, g, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	a := Analyze(ld, g)
	if got, want := a.groupNames(), []string{"core", "issues_close", "pr_open", "extra_thing"}; !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
	s := a.Report(5).Summary
	if len(s.Unplaced) != 0 || s.NonKernelDown+s.NonKernelUp >= s.CrossRefs {
		t.Errorf("unplaced %v, non-kernel %d+%d of %d", s.Unplaced, s.NonKernelDown, s.NonKernelUp, s.CrossRefs)
	}
	u, err := a.Group("issues_close")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range u.Needs {
		if l.Other == "core" && l.Up {
			t.Errorf("a use of the kernel file points up: %+v", l)
		}
	}
}
