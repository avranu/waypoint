package main

import (
	"regexp"
	"slices"
	"strings"
)

// Protected paths (WP-D18): an agent job may carry globs of files its
// commits must not touch (CI config, lockfiles, secrets). The runner checks
// the commits before it pushes and refuses the push when one of them does,
// so a model told to fix a failing test can't also change the pipeline that
// judges it. A glob that starts with "!" exempts the files it matches
// (committed templates like .env.example under a **/.env.* rule).

// protectRE turns a glob into a pattern over a repository path: `**/` is any
// directories, `**` anything, `*` and `?` stay within one segment.
func protectRE(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	g = strings.TrimPrefix(g, "/")
	for i := 0; i < len(g); i++ {
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case g[i] == '*':
			b.WriteString("[^/]*")
		case g[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// exemptsFrom is the "!" globs that apply to the protect glob g: all of them,
// unless g protects a whole directory (ends in "/**"), when only those written
// for that directory do. A .env.example under .github/ stays protected, but
// source code in a package called secrets can be let out (WP-750).
func exemptsFrom(g string, exempt []string) []string {
	dir, ok := strings.CutSuffix(g, "**")
	if !ok || !strings.HasSuffix(dir, "/") {
		return exempt
	}
	var out []string
	for _, e := range exempt {
		if strings.HasPrefix(e, dir) {
			out = append(out, e)
		}
	}
	return out
}

// protectedFiles lists the files that match one of the globs, unless a "!"
// exemption that applies to that glob lets them out.
func protectedFiles(globs, files []string) []string {
	var protect, exempt []string
	for _, g := range globs {
		if rest, ok := strings.CutPrefix(g, "!"); ok {
			exempt = append(exempt, rest)
		} else {
			protect = append(protect, g)
		}
	}
	matches := func(g, f string) bool {
		re, err := protectRE(g)
		return err == nil && re.MatchString(f)
	}
	isProtected := func(f string) bool {
		for _, g := range protect {
			if matches(g, f) && !slices.ContainsFunc(exemptsFrom(g, exempt), func(e string) bool { return matches(e, f) }) {
				return true
			}
		}
		return false
	}
	var bad []string
	for _, f := range files {
		if isProtected(f) {
			bad = append(bad, f)
		}
	}
	return bad
}

// resolutionIsTheMerge says whether a conflicted file's resolution is only
// the merge's own work (WP-ADR138). merged is the file as git's merge left it,
// conflict markers and all. The resolution must keep every line outside the
// conflicts as it is, in order, and settle each conflict with both sides'
// lines whole, one side's after the other's (blank lines and CRs aside): that
// drops nothing either side had and writes nothing. Anything else is the
// agent's edit: a line of neither side, a line dropped (one side's change
// left out, a line both had, the whole file) or moved. A conflict git left
// no markers for (one side deleted the file) is the merge's own only as git
// left it.
func resolutionIsTheMerge(resolved, merged string) bool {
	type segment struct {
		fixed  []string
		hunk   bool
		ab, ba []string
	}
	var segs []segment
	const (
		outside = iota
		ours
		base
		theirs
	)
	state := outside
	var cur, a, b []string
	for _, l := range strings.Split(merged, "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case conflictMarker(l, '<'):
			if state != outside {
				return false
			}
			segs = append(segs, segment{fixed: cur})
			cur, a, b, state = nil, nil, nil, ours
		case conflictMarker(l, '|'):
			if state != ours {
				return false
			}
			state = base
		case conflictMarker(l, '='):
			if state != ours && state != base {
				return false
			}
			state = theirs
		case conflictMarker(l, '>'):
			if state != theirs {
				return false
			}
			segs = append(segs, segment{hunk: true, ab: append(slices.Clip(a), b...), ba: append(slices.Clip(b), a...)})
			state = outside
		case strings.TrimSpace(l) == "":
		case state == outside:
			cur = append(cur, l)
		case state == ours:
			a = append(a, l)
		case state == theirs:
			b = append(b, l)
		}
	}
	if state != outside {
		return false
	}
	segs = append(segs, segment{fixed: cur})
	var res []string
	for _, l := range strings.Split(resolved, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			res = append(res, l)
		}
	}
	tried := map[[2]int]bool{}
	var match func(i, at int) bool
	match = func(i, at int) bool {
		if i == len(segs) {
			return at == len(res)
		}
		if tried[[2]int{i, at}] {
			return false
		}
		tried[[2]int{i, at}] = true
		opts := [][]string{segs[i].fixed}
		if segs[i].hunk {
			opts = [][]string{segs[i].ab, segs[i].ba}
		}
		for _, o := range opts {
			if at+len(o) <= len(res) && slices.Equal(res[at:at+len(o)], o) && match(i+1, at+len(o)) {
				return true
			}
		}
		return false
	}
	return match(0, 0)
}

// conflictMarker: l is a conflict marker of c, seven of it alone or before a
// space and a label.
func conflictMarker(l string, c byte) bool {
	m := strings.Repeat(string(c), 7)
	return l == m || strings.HasPrefix(l, m+" ")
}
