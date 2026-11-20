package testevidence

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Coverage gathers cover profiles (`go test -coverprofile`) into files.
// Profiles of the same tree may name the same block (two test processes,
// one package tested by several): a block ran if any profile says so.
type Coverage struct {
	module string
	files  map[string]map[block]*blockHit
}

type block struct{ sl, sc, el, ec int }

type blockHit struct {
	stmts int
	ran   bool
}

// NewCoverage gathers profiles of module's files (its path is cut from
// theirs).
func NewCoverage(module string) *Coverage {
	return &Coverage{module: module, files: map[string]map[block]*blockHit{}}
}

// Add reads one profile.
func (c *Coverage) Add(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// path/to/file.go:12.34,15.2 3 1
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return fmt.Errorf("cover profile line %d: %q", n, line)
		}
		count, err1 := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		stmts, err2 := strconv.Atoi(fields[len(fields)-2])
		loc := strings.Join(fields[:len(fields)-2], " ")
		i := strings.LastIndexByte(loc, ':')
		if err1 != nil || err2 != nil || i < 0 {
			return fmt.Errorf("cover profile line %d: %q", n, line)
		}
		var b block
		if _, err := fmt.Sscanf(loc[i+1:], "%d.%d,%d.%d", &b.sl, &b.sc, &b.el, &b.ec); err != nil {
			return fmt.Errorf("cover profile line %d: %q", n, line)
		}
		path := trimModule(loc[:i], c.module)
		blocks := c.files[path]
		if blocks == nil {
			blocks = map[block]*blockHit{}
			c.files[path] = blocks
		}
		h := blocks[b]
		if h == nil {
			h = &blockHit{stmts: stmts}
			blocks[b] = h
		}
		h.ran = h.ran || count > 0
	}
	return sc.Err()
}

// Files is the gathered coverage, by path, each file's blocks in source
// order.
func (c *Coverage) Files() []File {
	out := make([]File, 0, len(c.files))
	for path, blocks := range c.files {
		keys := make([]block, 0, len(blocks))
		for b := range blocks {
			keys = append(keys, b)
		}
		slices.SortFunc(keys, func(a, b block) int {
			for _, d := range [4]int{a.sl - b.sl, a.sc - b.sc, a.el - b.el, a.ec - b.ec} {
				if d != 0 {
					return d
				}
			}
			return 0
		})
		f := File{Path: path, Stmts: make([]int, len(keys)), Hit: make([]byte, (len(keys)+7)/8)}
		for i, b := range keys {
			h := blocks[b]
			f.Stmts[i] = h.stmts
			if h.ran {
				f.Hit[i/8] |= 1 << (i % 8)
			}
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return out
}
