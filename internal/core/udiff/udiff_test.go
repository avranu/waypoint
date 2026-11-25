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

// rebuild applies ops and returns the old and new texts they connect.
func rebuild(ops []diffOp) (a, b string) {
	var sa, sb strings.Builder
	for _, op := range ops {
		if op.kind != '+' {
			sa.WriteString(op.line)
		}
		if op.kind != '-' {
			sb.WriteString(op.line)
		}
	}
	return sa.String(), sb.String()
}

func numbered(prefix string, from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, prefix+strconv.Itoa(i)+"\n")
	}
	return out
}

func count(ops []diffOp) (plus, minus int) {
	for _, op := range ops {
		switch op.kind {
		case '+':
			plus++
		case '-':
			minus++
		}
	}
	return
}

func checkOps(t *testing.T, a, b []string) []diffOp {
	t.Helper()
	ops := diffLines(a, b)
	ga, gb := rebuild(ops)
	if ga != strings.Join(a, "") || gb != strings.Join(b, "") {
		t.Fatal("the ops don't rebuild the inputs")
	}
	return ops
}

// Past the edit cap the middle is split on lines unique to both sides, so
// lines the two share are still context and the counts are the real change.
func TestUnifiedPastTheEditCap(t *testing.T) {
	a := []string{"head\n"}
	b := []string{"head\n"}
	a = append(a, numbered("old", 0, 2000)...)
	b = append(b, numbered("new", 0, 1000)...)
	b = append(b, numbered("old", 0, 1000)...)
	a = append(a, "tail\n")
	b = append(b, "tail\n")
	checkOps(t, a, b)
	d, add, del := Unified("big.txt", strings.Join(a, ""), strings.Join(b, ""), false, false)
	plus, minus := 0, 0
	for _, l := range strings.Split(d, "\n")[2:] {
		switch {
		case strings.HasPrefix(l, "+"):
			plus++
		case strings.HasPrefix(l, "-"):
			minus++
		}
	}
	if add != plus || del != minus || add != 1000 || del != 1000 {
		t.Fatalf("counts +%d -%d, the diff shows +%d -%d; want +1000 -1000", add, del, plus, minus)
	}
	if strings.Contains(d, "-head") || strings.Contains(d, "-tail") {
		t.Fatal("the shared ends should stay context")
	}
}

// No line is common to both sides: nothing to anchor on, so the middle is
// replaced whole, and the diff is still valid.
func TestPastTheCapNoAnchors(t *testing.T) {
	a := append([]string{"head\n"}, numbered("old", 0, 2000)...)
	b := append([]string{"head\n"}, numbered("new", 0, 2000)...)
	ops := checkOps(t, a, b)
	if p, m := count(ops); p != 2000 || m != 2000 {
		t.Fatalf("+%d -%d, want +2000 -2000", p, m)
	}
}

// Anchors exist but the gap between two of them is itself past the cap and
// has no anchor: that gap is replaced, the rest is still matched.
func TestPastTheCapOneGapStillOver(t *testing.T) {
	a := append(numbered("x", 0, 10), "anchor\n")
	b := append(numbered("y", 0, 10), "anchor\n")
	a = append(a, numbered("old", 0, 2000)...)
	b = append(b, numbered("new", 0, 2000)...)
	a = append(a, "anchor2\n", "z\n")
	b = append(b, "anchor2\n", "w\n")
	ops := checkOps(t, a, b)
	if p, m := count(ops); p != 2011 || m != 2011 {
		t.Fatalf("+%d -%d, want +2011 -2011", p, m)
	}
	ctx := 0
	for _, op := range ops {
		if op.kind == ' ' {
			ctx++
		}
	}
	if ctx != 2 {
		t.Fatalf("%d context lines, want the two anchors", ctx)
	}
}

// Only a few distinct lines, each repeated: no line is unique, so there are
// no anchors and the diff falls back to replacing the middle.
func TestPastTheCapRepeatedLinesOnly(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	gen := func(n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, string(rune('a'+r.Intn(3)))+"\n")
		}
		return out
	}
	a, b := gen(4000), gen(4000)
	ops := checkOps(t, a, b)
	if len(uniqueAnchors(a, b)) != 0 {
		t.Fatal("expected no anchors")
	}
	if p, m := count(ops); p == 0 || m == 0 {
		t.Fatalf("+%d -%d", p, m)
	}
}

// Generated large inputs, with scattered edits, moved blocks and repeated
// lines, always give ops that turn a into b.
func TestPastTheCapReconstructs(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	for round := 0; round < 6; round++ {
		a := numbered("line", 0, 3000+r.Intn(3000))
		for i := 0; i < 200; i++ { // repeated lines
			a[r.Intn(len(a))] = "dup\n"
		}
		var b []string
		for _, l := range a {
			switch r.Intn(4) {
			case 0: // change
				b = append(b, "chg"+strconv.Itoa(r.Intn(1<<20))+"\n")
			case 1: // delete
			default:
				b = append(b, l)
			}
		}
		if round%2 == 1 { // move a block
			k := len(b) / 3
			b = append(append(append([]string(nil), b[k:2*k]...), b[:k]...), b[2*k:]...)
		}
		checkOps(t, a, b)
	}
}

// pastCapInputs: a large file with >1500 scattered edits, and one with no
// line in common.
func pastCapInputs() (scA, scB, nvA, nvB []string) {
	r := rand.New(rand.NewSource(5))
	scA = numbered("line", 0, 50000)
	for _, l := range scA {
		if r.Intn(20) == 0 {
			scB = append(scB, "changed"+strconv.Itoa(r.Intn(1<<30))+"\n")
		} else {
			scB = append(scB, l)
		}
	}
	return scA, scB, numbered("old", 0, 20000), numbered("new", 0, 20000)
}

func BenchmarkDiffLinesPastCap(b *testing.B) {
	scA, scB, nvA, nvB := pastCapInputs()
	b.Run("scattered50k", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			diffLines(scA, scB)
		}
	})
	b.Run("noAnchors20k", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			diffLines(nvA, nvB)
		}
	})
}
