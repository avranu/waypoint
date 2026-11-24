package core

// The rule for numbered files (WP-1815): files whose names start with a
// number that gives their order (migrations) are numbered so that what an open
// pull request adds comes after what is on the base and after what pull
// requests ahead of it add. planRenumber is the only place that decides; it
// reads no database and no forge.

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// renumberPR is an open pull request as the rule sees it.
type renumberPR struct {
	ID     int64
	Number int
	Place  int      // its place in the merge line, -1 for none
	Adds   []string // the numbered files it adds (paths)
}

// renumberMove renames one file of a pull request.
type renumberMove struct {
	PR       int64
	From, To string
	Why      renumberWhy
	By       int // the pull request whose number it clashes with (WhyPR, WhyLine)
}

type renumberWhy int

const (
	whyBase  renumberWhy = iota // the number is not above the base's
	whyPR                       // another pull request ahead adds it
	whyLine                     // a line member ahead adds a higher one
	whyAfter                    // an earlier file of the same pull request moved
)

// fileNumber is the digits a file's name starts with.
func fileNumber(p string) (n, width int, ok bool) {
	b := path.Base(p)
	for width < len(b) && b[width] >= '0' && b[width] <= '9' {
		width++
	}
	if width == 0 || width > 9 {
		return 0, 0, false
	}
	n, _ = strconv.Atoi(b[:width])
	return n, width, true
}

// withNumber is p with its number replaced by n, in the width it had.
func withNumber(p string, n int) string {
	_, w, _ := fileNumber(p)
	b := path.Base(p)
	return path.Join(path.Dir(p), fmt.Sprintf("%0*d", w, n)+b[w:])
}

// planRenumber says which files to rename. base lists the numbered files on
// the base's tip. Pull requests are taken in the merge line's order, then by
// number. A file breaks the rule when its number is not above the base's
// highest, is one an earlier pull request adds, or (in the line) is not above
// the highest a line member ahead adds. It and the pull request's later files
// move to numbers above everything on the base and in open pull requests, so a
// rename never makes a pull request outside the line break the rule.
func planRenumber(base []string, prs []renumberPR) []renumberMove {
	maxBase := 0
	for _, p := range base {
		if n, _, ok := fileNumber(p); ok {
			maxBase = max(maxBase, n)
		}
	}
	top := maxBase
	type add struct {
		path string
		n    int
	}
	adds := map[int64][]add{}
	for _, pr := range prs {
		for _, p := range pr.Adds {
			if n, _, ok := fileNumber(p); ok {
				adds[pr.ID] = append(adds[pr.ID], add{p, n})
				top = max(top, n)
			}
		}
		slices.SortFunc(adds[pr.ID], func(a, b add) int { return strings.Compare(a.path, b.path) })
	}
	order := slices.Clone(prs)
	slices.SortStableFunc(order, func(a, b renumberPR) int {
		switch {
		case a.Place >= 0 && b.Place >= 0:
			return a.Place - b.Place
		case a.Place >= 0:
			return -1
		case b.Place >= 0:
			return 1
		}
		return a.Number - b.Number
	})
	var out []renumberMove
	taken := map[int]int{} // number → the pull request (number) of the earlier one that adds it
	lineMax, lineBy := 0, 0
	for _, pr := range order {
		fs := adds[pr.ID]
		moved := false
		var final []int
		for _, f := range fs {
			n, why, by := f.n, whyAfter, 0
			if !moved {
				t, dup := taken[n]
				switch {
				case n <= maxBase:
					moved, why = true, whyBase
				case dup:
					moved, why, by = true, whyPR, t
				case pr.Place >= 0 && n <= lineMax:
					moved, why, by = true, whyLine, lineBy
				}
			}
			if moved {
				top++
				out = append(out, renumberMove{PR: pr.ID, From: f.path, To: withNumber(f.path, top), Why: why, By: by})
				n = top
			}
			final = append(final, n)
		}
		for _, n := range final {
			if _, ok := taken[n]; !ok {
				taken[n] = pr.Number
			}
			if pr.Place >= 0 && n > lineMax {
				lineMax, lineBy = n, pr.Number
			}
		}
	}
	return out
}
