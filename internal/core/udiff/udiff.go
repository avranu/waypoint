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

// diffLines is the shortest edit script from a to b (Myers, O(ND)), after
// trimming what both share at the ends. Past maxEdits differences it gives
// up on finding the shortest and replaces the middle wholesale.
func diffLines(a, b []string) []diffOp {
	const maxEdits = 1500
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var out []diffOp
	for _, l := range a[:pre] {
		out = append(out, diffOp{' ', l})
	}
	out = append(out, myers(a[pre:len(a)-suf], b[pre:len(b)-suf], maxEdits)...)
	for _, l := range a[len(a)-suf:] {
		out = append(out, diffOp{' ', l})
	}
	return out
}

func myers(a, b []string, maxEdits int) []diffOp {
	n, m := len(a), len(b)
	replace := func() []diffOp {
		out := make([]diffOp, 0, n+m)
		for _, l := range a {
			out = append(out, diffOp{'-', l})
		}
		for _, l := range b {
			out = append(out, diffOp{'+', l})
		}
		return out
	}
	if n == 0 || m == 0 {
		return replace()
	}
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
		return replace()
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
	return out
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
