package coremap

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strings"
)

// Groups maps file names to domains and gives the domains their layer order.
type Groups struct {
	// Order lists the domains of the grouping file, bottom layer first.
	Order []string
	globs map[string][]string
}

// ParseGroups reads a grouping file: `domain: glob glob ...` per line, `#`
// starts a comment, blank lines are skipped. A domain may appear only once.
func ParseGroups(r io.Reader) (*Groups, error) {
	g := &Groups{globs: map[string][]string{}}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("line %d: want `domain: glob glob ...`, got %q", n, line)
		}
		if _, dup := g.globs[name]; dup {
			return nil, fmt.Errorf("line %d: domain %q listed twice", n, name)
		}
		globs := strings.Fields(rest)
		for _, gl := range globs {
			if _, err := path.Match(gl, ""); err != nil {
				return nil, fmt.Errorf("line %d: glob %q: %w", n, gl, err)
			}
		}
		g.Order = append(g.Order, name)
		g.globs[name] = globs
	}
	return g, sc.Err()
}

// Of returns the group of a non-test file name: the first line of the
// grouping file with a matching glob, else the file's stem before its first
// underscore.
func (g *Groups) Of(file string) string {
	for _, d := range g.Order {
		for _, gl := range g.globs[d] {
			if ok, _ := path.Match(gl, file); ok {
				return d
			}
		}
	}
	stem := strings.TrimSuffix(file, ".go")
	if i := strings.IndexByte(stem, '_'); i >= 0 {
		return stem[:i]
	}
	return stem
}

// Placed reports whether the grouping file lists the domain.
func (g *Groups) Placed(domain string) bool {
	_, ok := g.globs[domain]
	return ok
}
