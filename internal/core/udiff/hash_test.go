package udiff

import (
	"strings"
	"testing"
)

// WP-1808: the diff hash ignores what a clean merge of the base moves.
func TestChangeHash(t *testing.T) {
	file := func(hunks ...string) string {
		return "diff --git a/a.go b/a.go\nindex 1..2 100644\n--- a/a.go\n+++ b/a.go\n" + strings.Join(hunks, "")
	}
	hunk := func(start string, lines ...string) string {
		return "@@ -" + start + " +" + start + " @@ func f() {\n" + strings.Join(lines, "\n") + "\n"
	}
	bin := func(index string) string {
		return "diff --git a/b.bin b/b.bin\nindex " + index + " 100644\nBinary files a/b.bin and b/b.bin differ\n"
	}
	base := file(hunk("10,6", " one", " two", "+guard()", " three", " four", " five"))
	cases := []struct {
		name string
		diff string
		same bool
	}{
		{"identical", base, true},
		{"hunk numbers and index lines move", strings.NewReplacer("@@ -10,6 +10,6 @@", "@@ -70,6 +71,6 @@", "index 1..2", "index 9..8").Replace(base), true},
		{"far-away context changes", strings.Replace(base, " five", " 5", 1), true},
		{"the line before the change changes", strings.Replace(base, " two", " 2", 1), false},
		{"the line after the change changes", strings.Replace(base, " three", " 3", 1), false},
		{"an added line moves up", file(hunk("10,6", " one", "+guard()", " two", " three", " four", " five")), false},
		{"an added line moves to another function", file(hunk("10,6", " one", " two", " three", " four", " five"), hunk("40,2", " other", "+guard()", " tail")), false},
		{"a block moves", file(hunk("10,7", " one", " two", "+a()", "+b()", " three", " four", " five")), false},
		{"a line ends with CR", strings.Replace(base, "+guard()", "+guard()\r", 1), false},
		{"an added line differs", strings.Replace(base, "+guard()", "+other()", 1), false},
		{"a file is added", base + file(hunk("1,1", "+x"))[len("diff --git a/a.go"):], false},
		{"a mode changes", strings.Replace(base, "index 1..2 100644", "new file mode 100755", 1), false},
		{"a file is renamed", strings.Replace(base, "b/a.go", "b/c.go", 1), false},
		{"a binary file is added", base + bin("1..2"), false},
	}
	want := ChangeHash(base)
	// A binary's diff is its blob ids: another blob is another change.
	if ChangeHash(base+bin("1..2")) == ChangeHash(base+bin("1..3")) {
		t.Error("a binary file replaced by another: same hash")
	}
	// The same binary change beside a text file a merge moved: a text file's
	// index line still doesn't count.
	if ChangeHash(base+bin("1..2")) != ChangeHash(strings.Replace(base, "index 1..2", "index 9..8", 1)+bin("1..2")) {
		t.Error("the same binary change: different hashes")
	}
	for _, c := range cases {
		if got := ChangeHash(c.diff) == want; got != c.same {
			t.Errorf("%s: same = %v, want %v", c.name, got, c.same)
		}
	}

	// The function a change is in counts, for every run in the hunk; the line
	// numbers still don't.
	in := func(fn, start string, lines ...string) string {
		return "@@ -" + start + " +" + start + " @@ " + fn + "\n" + strings.Join(lines, "\n") + "\n"
	}
	guarded := []string{" if enabled {", "+\tlog()", " }"}
	twoRuns := func(fn string) string {
		return file(in(fn, "20,8", " a()", "+b()", " c()", " d()", " if enabled {", "+\tlog()", " }", " e()"))
	}
	pairs := []struct {
		name  string
		a, b  string
		equal bool
	}{
		{"the same line between the same neighbours in two functions",
			file(in("func load() {", "10,3", guarded...)), file(in("func save() {", "10,3", guarded...)), false},
		{"the second run of a hunk in another function",
			twoRuns("func load() {"), twoRuns("func save() {"), false},
		{"the same hunk lower in the file",
			file(in("func load() {", "10,3", guarded...)), file(in("func load() {", "52,3", guarded...)), true},
		{"a hunk without a heading", file(in("", "10,3", guarded...)), file(in("", "90,3", guarded...)), true},
	}
	for _, p := range pairs {
		if got := ChangeHash(p.a) == ChangeHash(p.b); got != p.equal {
			t.Errorf("%s: same = %v, want %v", p.name, got, p.equal)
		}
	}
}
