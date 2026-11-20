package core

import (
	"math/rand"
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
	d, add, del := unifiedDiff("a.txt", old, nw, false, false)
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
	if d, _, _ := unifiedDiff("m.txt", strings.Join(many, ""), strings.Join(changed, ""), false, false); strings.Count(d, "@@ -") != 2 {
		t.Fatalf("expected two hunks:\n%s", d)
	}
	nd, add, _ := unifiedDiff("n.txt", "", "hello\nworld", true, false)
	if add != 2 || !strings.Contains(nd, "--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,2 @@\n+hello\n+world\n\\ No newline at end of file\n") {
		t.Fatalf("new file diff:\n%s", nd)
	}
	if gitBlobSHA("hello\n") != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatal("blob sha of hello\\n should match git hash-object")
	}
}
