package coremap

import (
	"os"
	"path/filepath"
	"slices"
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

// improve must repair what the greedy pass leaves: it never ends worse than
// the sorted order and, on a tournament-like graph, finds the known optimum.
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
	if r.Summary.Up != 2 || r.Summary.Down != 12 {
		t.Errorf("under the suggested order: up %d down %d, want 2 and 12", r.Summary.Up, r.Summary.Down)
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

// Every non-test file of internal/core has to be placed by a line of
// groups.txt, so a new file cannot fall silently to the stem rule.
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
