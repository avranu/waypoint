package coremap

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUpwardPairsOnFixture(t *testing.T) {
	a, _ := fixture(t)
	pairs := a.UpwardPairs()
	if keys := sortedKeys(pairs); !slices.Equal(keys, []string{"pr>issues"}) {
		t.Fatalf("pairs %v, want [pr>issues] (references to kernel and within a group never count)", keys)
	}
	if n := len(pairs["pr>issues"]); n != 3 {
		t.Errorf("pr>issues has %d refs, want 3", n)
	}
	if n := a.AllPairs(); n < 2 {
		t.Errorf("AllPairs %d", n)
	}
}

func TestUpwardFileRoundTrip(t *testing.T) {
	a, _ := fixture(t)
	var b strings.Builder
	if err := WriteUpward(&b, a.UpwardPairs()); err != nil {
		t.Fatal(err)
	}
	got, err := ParseUpward(strings.NewReader(b.String()))
	if err != nil || len(got) != 1 || !got["pr>issues"] {
		t.Errorf("round trip: %v %v\n%s", err, got, b.String())
	}
	got, err = ParseUpward(strings.NewReader("# c\n\n b>a  # tail\na>b\n"))
	if err != nil || !got["b>a"] || !got["a>b"] || len(got) != 2 {
		t.Errorf("parse: %v %v", err, got)
	}
	for _, bad := range []string{"ab", ">b", "a>", "a > b", "a>b>c"} {
		if _, err := ParseUpward(strings.NewReader(bad)); err == nil {
			t.Errorf("ParseUpward(%q) succeeded", bad)
		}
	}
}

func TestCheckUpwardOnFixture(t *testing.T) {
	a, _ := fixture(t)
	found := a.UpwardPairs()

	if added, gone := CheckUpward(found, map[string]bool{"pr>issues": true}); added != "" || gone != "" {
		t.Errorf("matching baseline: %q %q", added, gone)
	}

	added, gone := CheckUpward(found, map[string]bool{})
	if gone != "" {
		t.Errorf("gone %q", gone)
	}
	for _, want := range []string{
		"1 new upward group pair(s)", "pr>issues: 3 reference(s)",
		"internal/core/pr_open.go:15 Service.CloseIssue", "internal/core/pr_open.go:16 notify",
		"kernel", "emit an event", "groups.txt",
		`make coremap CORE_ARGS="-edge 'pr>issues'"`,
	} {
		if !strings.Contains(added, want) {
			t.Errorf("added message lacks %q:\n%s", want, added)
		}
	}

	added, gone = CheckUpward(found, map[string]bool{"pr>issues": true, "z>a": true, "y>a": true})
	if added != "" || !strings.Contains(gone, "2 pair(s)") || !strings.Contains(gone, "y>a, z>a") || !strings.Contains(gone, "make coremap-upward") {
		t.Errorf("stale: %q %q", added, gone)
	}
}

func TestCheckUpwardCapsTheList(t *testing.T) {
	var refs []Ref
	for i := 1; i <= 13; i++ {
		refs = append(refs, Ref{File: "a.go", Line: i, Symbol: "S"})
	}
	added, _ := CheckUpward(map[string][]Ref{"a>b": refs}, nil)
	if !strings.Contains(added, "a.go:10 S") || strings.Contains(added, "a.go:11 S") || !strings.Contains(added, "... and 3 more") {
		t.Errorf("cap:\n%s", added)
	}
}

func TestWriteBaseline(t *testing.T) {
	a, _ := fixture(t)
	path := filepath.Join(t.TempDir(), "upward.txt")
	var log strings.Builder
	if err := os.WriteFile(path, []byte("old>gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeBaseline(path, a, true, &log); err == nil {
		t.Error("wrote the baseline under another order")
	}
	a.ld.TypeErrors = 1
	if err := writeBaseline(path, a, false, &log); err == nil {
		t.Error("wrote the baseline from an incomplete map")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "old>gone\n" {
		t.Errorf("a refused write changed the file: %q %v", b, err)
	}
	a.ld.TypeErrors = 0
	if err := writeBaseline(path, a, false, &log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "added pr>issues, which was not listed") {
		t.Errorf("the new pair isn't named:\n%s", log.String())
	}
	log.Reset()
	if err := writeBaseline(path, a, false, &log); err != nil || strings.Contains(log.String(), "added") {
		t.Errorf("a rewrite with nothing new: %v\n%s", err, log.String())
	}
}

// TestNoNewUpwardReferences loads the real internal/core (about as long as
// `make coremap`) and holds its upward group pairs to upward.txt (WP-1925,
// WP-ADR151 section 6): a pair not listed fails with its references; a
// listed pair that is gone is only logged. It checks pairs, not the count of references on a pair, so an existing edge can grow without
// touching the list. Up is decided by the line order of groups.txt; groups in
// one cycle are not special: whichever of the two directions the order makes
// upward is listed, the other is down. References to the kernel group and
// within a group never count.
func TestNoNewUpwardReferences(t *testing.T) {
	gf, err := os.Open("groups.txt")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := ParseGroups(gf)
	_ = gf.Close()
	if err != nil {
		t.Fatal(err)
	}
	uf, err := os.Open("upward.txt")
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := ParseUpward(uf)
	_ = uf.Close()
	if err != nil {
		t.Fatalf("upward.txt: %v", err)
	}
	var log strings.Builder
	ld, err := Load("../core", groups, &log)
	if err != nil {
		t.Fatal(err)
	}
	// A file with type errors loses references, so the check would pass on
	// what it didn't see.
	if ld.TypeErrors > 0 {
		t.Fatalf("%d type errors: the map is incomplete\n%s", ld.TypeErrors, log.String())
	}
	added, gone := CheckUpward(Analyze(ld, groups).UpwardPairs(), allowed)
	if added != "" {
		t.Error(added)
	}
	// A pair that is gone is not a failure: a change that removes the last
	// upward reference of a pair, or lands beside one that does, must not be
	// sent round CI again for it. make coremap-upward prunes the list.
	if gone != "" {
		t.Log(gone)
	}
}
