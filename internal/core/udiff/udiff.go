// Package udiff renders unified diffs and computes git blob ids, for the
// change requests that propose edits to files in a repo.
package udiff

import (
	"crypto/sha1" // #nosec G505 -- git names blobs by SHA-1; this is not a security use
	"encoding/hex"
	"fmt"
	"strings"
)

// GitBlobSHA is the id git gives a file's content, so a change request can
// say which version of a file it was made against.
func GitBlobSHA(content string) string {
	h := sha1.New() // #nosec G401 -- see the import
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

// splitLines splits text into lines keeping their newlines; a last line
// without one is returned as it is.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type diffOp struct {
	kind byte
	line string
}

// maxEdits bounds the Myers search on one piece; past it the piece is
// split on anchors instead.
const maxEdits = 1500

// diffLines is the shortest edit script from a to b (Myers, O(ND)), after
// trimming what both share at the ends. Where a piece needs more than
// maxEdits differences it is split on lines that occur exactly once on each
// side (see anchored); a piece with no such line is replaced wholesale.
func diffLines(a, b []string) []diffOp {
	return diffRange(a, b, make([]diffOp, 0, len(a)+len(b)))
}

// diffRange appends the edit script from a to b to out.
func diffRange(a, b []string, out []diffOp) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	for _, l := range a[:pre] {
		out = append(out, diffOp{' ', l})
	}
	out = diffMiddle(a[pre:len(a)-suf], b[pre:len(b)-suf], out)
	for _, l := range a[len(a)-suf:] {
		out = append(out, diffOp{' ', l})
	}
	return out
}

// diffMiddle diffs pieces with no shared ends: capped Myers, then anchors.
func diffMiddle(a, b []string, out []diffOp) []diffOp {
	if len(a) == 0 || len(b) == 0 {
		return appendReplace(a, b, out)
	}
	if ops, ok := myers(a, b, maxEdits); ok {
		return append(out, ops...)
	}
	anchors := uniqueAnchors(a, b)
	if len(anchors) == 0 {
		return appendReplace(a, b, out)
	}
	pa, pb := 0, 0
	for _, an := range anchors {
		out = diffRange(a[pa:an.a], b[pb:an.b], out)
		out = append(out, diffOp{' ', a[an.a]})
		pa, pb = an.a+1, an.b+1
	}
	return diffRange(a[pa:], b[pb:], out)
}

func appendReplace(a, b []string, out []diffOp) []diffOp {
	for _, l := range a {
		out = append(out, diffOp{'-', l})
	}
	for _, l := range b {
		out = append(out, diffOp{'+', l})
	}
	return out
}

type anchor struct{ a, b int }

// uniqueAnchors returns the longest run, increasing in both a and b, of
// lines that occur exactly once in a and exactly once in b (patience
// diff's anchors). O((n+m) log(n+m)).
func uniqueAnchors(a, b []string) []anchor {
	type occ struct{ ca, cb, ia, ib int }
	idx := make(map[string]int, len(a))
	occs := make([]occ, 0, len(a))
	for i, l := range a {
		if k, ok := idx[l]; ok {
			occs[k].ca++
		} else {
			idx[l] = len(occs)
			occs = append(occs, occ{ca: 1, ia: i})
		}
	}
	for i, l := range b {
		if k, ok := idx[l]; ok {
			occs[k].cb++
			occs[k].ib = i
		}
	}
	// Candidates in a's order, each with its position in b.
	var cand []anchor
	for i, l := range a {
		if o := occs[idx[l]]; o.ca == 1 && o.cb == 1 {
			cand = append(cand, anchor{i, o.ib})
		}
	}
	// Longest increasing subsequence on b by patience sorting.
	var tails []int // tails[k] = index in cand ending a run of length k+1
	prev := make([]int, len(cand))
	for i, c := range cand {
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if cand[tails[mid]].b < c.b {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		prev[i] = -1
		if lo > 0 {
			prev[i] = tails[lo-1]
		}
		if lo == len(tails) {
			tails = append(tails, i)
		} else {
			tails[lo] = i
		}
	}
	if len(tails) == 0 {
		return nil
	}
	res := make([]anchor, len(tails))
	for k, i := len(tails)-1, tails[len(tails)-1]; k >= 0; k, i = k-1, prev[i] {
		res[k] = cand[i]
	}
	return res
}

// myers is the shortest edit script, or ok false when it needs more than
// maxEdits edits.
func myers(a, b []string, maxEdits int) (ops []diffOp, ok bool) {
	n, m := len(a), len(b)
	maxD := min(n+m, maxEdits)
	off := maxD + 1
	v := make([]int, 2*off+1)
	var snaps [][]int
	found := -1
rounds:
	for d := 0; d <= maxD; d++ {
		snap := make([]int, 2*d+3)
		for i := range snap {
			snap[i] = v[off+i-(d+1)]
		}
		snaps = append(snaps, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = d
				break rounds
			}
		}
	}
	if found < 0 {
		return nil, false
	}
	var rev []diffOp
	x, y := n, m
	for d := found; d >= 0; d-- {
		get := func(k int) int { return snaps[d][k+d+1] }
		k := x - y
		var pk int
		if k == -d || (k != d && get(k-1) < get(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := get(pk)
		py := px - pk
		if d == 0 {
			px, py = 0, 0
		}
		for x > px && y > py {
			x--
			y--
			rev = append(rev, diffOp{' ', a[x]})
		}
		if d > 0 {
			if x == px {
				y--
				rev = append(rev, diffOp{'+', b[y]})
			} else {
				x--
				rev = append(rev, diffOp{'-', a[x]})
			}
		}
		x, y = px, py
	}
	out := make([]diffOp, len(rev))
	for i, op := range rev {
		out[len(rev)-1-i] = op
	}
	return out, true
}

// Unified renders the change from old to new for one file with three
// lines of context, in the format git and patch use. Paths are shown as a/
// and b/; an empty old side is a new file, an empty new side with deleted
// set is a deleted one. The numbers are lines added and removed.
func Unified(path, old, new string, isNew, deleted bool) (diff string, added, removed int) {
	a, b := splitLines(old), splitLines(new)
	ops := diffLines(a, b)
	var sb strings.Builder
	from, to := "a/"+path, "b/"+path
	if isNew {
		from = "/dev/null"
	}
	if deleted {
		to = "/dev/null"
	}
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", from, to)
	const ctx = 3
	type span struct{ start, end int }
	var spans []span
	for i := 0; i < len(ops); i++ {
		if ops[i].kind == ' ' {
			continue
		}
		start := max(0, i-ctx)
		j := i
		for j < len(ops) {
			if ops[j].kind != ' ' {
				j++
				continue
			}
			k := j
			for k < len(ops) && ops[k].kind == ' ' {
				k++
			}
			if k < len(ops) && k-j <= 2*ctx {
				j = k
				continue
			}
			break
		}
		end := min(len(ops), j+ctx)
		if len(spans) > 0 && start <= spans[len(spans)-1].end {
			spans[len(spans)-1].end = end
		} else {
			spans = append(spans, span{start, end})
		}
		i = j
	}
	aLine, bLine := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for i, op := range ops {
		aLine[i+1], bLine[i+1] = aLine[i], bLine[i]
		if op.kind != '+' {
			aLine[i+1]++
		}
		if op.kind != '-' {
			bLine[i+1]++
		}
	}
	for _, sp := range spans {
		na, nb := aLine[sp.end]-aLine[sp.start], bLine[sp.end]-bLine[sp.start]
		sa, sb2 := aLine[sp.start]+1, bLine[sp.start]+1
		if na == 0 {
			sa = aLine[sp.start]
		}
		if nb == 0 {
			sb2 = bLine[sp.start]
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", sa, na, sb2, nb)
		for _, op := range ops[sp.start:sp.end] {
			sb.WriteByte(op.kind)
			sb.WriteString(strings.TrimSuffix(op.line, "\n"))
			sb.WriteByte('\n')
			if !strings.HasSuffix(op.line, "\n") {
				sb.WriteString("\\ No newline at end of file\n")
			}
			switch op.kind {
			case '+':
				added++
			case '-':
				removed++
			}
		}
	}
	return sb.String(), added, removed
}
