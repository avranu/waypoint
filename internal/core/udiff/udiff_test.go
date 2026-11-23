package udiff

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

func TestDiffLinesReconstructs(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	word := func() string { return string(rune('a'+r.Intn(5))) + "\n" }
	for n := 0; n < 300; n++ {
		var a, b []string
		for i := r.Intn(30); i > 0; i-- {
			a = append(a, word())
		}
		for i := r.Intn(30); i > 0; i-- {
			b = append(b, word())
		}
		var gotA, gotB []string
		for _, op := range diffLines(a, b) {
			if op.kind != '+' {
				gotA = append(gotA, op.line)
			}
			if op.kind != '-' {
				gotB = append(gotB, op.line)
			}
		}
		if strings.Join(gotA, "") != strings.Join(a, "") || strings.Join(gotB, "") != strings.Join(b, "") {
			t.Fatalf("round %d: ops don't rebuild the inputs\na=%q\nb=%q\ngotA=%q\ngotB=%q", n, a, b, gotA, gotB)
		}
	}
}

func TestUnifiedDiff(t *testing.T) {
	old := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	nw := "one\ntwo\nthree\nfour\nFIVE\nsix\nseven\neight\nnine\nten\neleven\n"
	d, add, del := Unified("a.txt", old, nw, false, false)
	if add != 2 || del != 1 {
		t.Fatalf("counts %d/%d\n%s", add, del, d)
	}
	for _, frag := range []string{"--- a/a.txt\n+++ b/a.txt\n", "-five\n+FIVE\n", "+eleven\n", "@@ -2,9 +2,10 @@"} {
		if !strings.Contains(d, frag) {
			t.Errorf("diff lacks %q:\n%s", frag, d)
		}
	}
	// Changes far apart are separate hunks.
	var many []string
	for i := 1; i <= 30; i++ {
		many = append(many, "line"+string(rune('a'+i%26))+"\n")
	}
	changed := append([]string(nil), many...)
	changed[1], changed[27] = "X\n", "Y\n"
	if d, _, _ := Unified("m.txt", strings.Join(many, ""), strings.Join(changed, ""), false, false); strings.Count(d, "@@ -") != 2 {
		t.Fatalf("expected two hunks:\n%s", d)
	}
	nd, add, _ := Unified("n.txt", "", "hello\nworld", true, false)
	if add != 2 || !strings.Contains(nd, "--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,2 @@\n+hello\n+world\n\\ No newline at end of file\n") {
		t.Fatalf("new file diff:\n%s", nd)
	}
	if GitBlobSHA("hello\n") != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatal("blob sha of hello\\n should match git hash-object")
	}
}

// Past the edit cap the middle is replaced whole instead of searched: the
// diff is longer than the shortest one but still turns the old text into the
// new, and the counts are of the lines it shows.
func TestUnifiedPastTheEditCap(t *testing.T) {
	var a, b []string
	a = append(a, "head\n")
	b = append(b, "head\n")
	for i := 0; i < 2000; i++ {
		a = append(a, "old"+strconv.Itoa(i)+"\n")
	}
	for i := 0; i < 1000; i++ {
		b = append(b, "new"+strconv.Itoa(i)+"\n")
	}
	for i := 0; i < 1000; i++ {
		b = append(b, "old"+strconv.Itoa(i)+"\n") // shared with a, so the shortest script is 2000 edits
	}
	a = append(a, "tail\n")
	b = append(b, "tail\n")
	var gotA, gotB []string
	for _, op := range diffLines(a, b) {
		if op.kind != '+' {
			gotA = append(gotA, op.line)
		}
		if op.kind != '-' {
			gotB = append(gotB, op.line)
		}
	}
	if strings.Join(gotA, "") != strings.Join(a, "") || strings.Join(gotB, "") != strings.Join(b, "") {
		t.Fatal("the ops don't rebuild the inputs")
	}
	d, add, del := Unified("big.txt", strings.Join(a, ""), strings.Join(b, ""), false, false)
	plus, minus := 0, 0
	for _, l := range strings.Split(d, "\n")[2:] { // after the two file lines
		switch {
		case strings.HasPrefix(l, "+"):
			plus++
		case strings.HasPrefix(l, "-"):
			minus++
		}
	}
	if add != plus || del != minus || add != 2000 || del != 2000 {
		t.Fatalf("counts +%d -%d, the diff shows +%d -%d; want the whole middle replaced (+2000 -2000)", add, del, plus, minus)
	}
	if strings.Contains(d, "-head") || strings.Contains(d, "-tail") {
		t.Fatal("the shared ends should stay context")
	}
}
