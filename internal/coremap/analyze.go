package coremap

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// PkgSize is the file and line count of one directory's Go files.
type PkgSize struct {
	Dir        string `json:"dir"`
	Files      int    `json:"files"`
	Lines      int    `json:"lines"`
	TestFiles  int    `json:"testFiles"`
	TestLines  int    `json:"testLines"`
	TypedFiles int    `json:"typeCheckedFiles,omitempty"`
}

// Summary is the package's size and the shape of its group graph.
type Summary struct {
	Package       string    `json:"package"`
	Top           PkgSize   `json:"top"`
	Subpackages   []PkgSize `json:"subpackages"`
	ServiceFields int       `json:"serviceFields"`
	ServiceMeths  int       `json:"serviceMethods"`
	Groups        int       `json:"groups"`
	Unplaced      []string  `json:"unplaced"`
	// StemPlaced lists the files no line matches that the stem rule put in a
	// listed group (with PerFile, those it counts as kernel): Unplaced does
	// not show them.
	StemPlaced   []string `json:"stemPlaced"`
	Ready        []string `json:"ready"`
	SCCs         int      `json:"sccs"`
	LargestSCC   int      `json:"largestScc"`
	LargestSCCOf []string `json:"largestSccGroups"`
	// Cycles lists every strongly connected component of two or more groups,
	// largest first.
	Cycles    [][]string `json:"cycles"`
	CrossRefs int        `json:"crossRefs"`
	Down      int        `json:"down"`
	Up        int        `json:"up"`
	UpCalls   int        `json:"upCalls"`
	// The same three counts for references between groups other than the kernel.
	NonKernelDown    int `json:"nonKernelDown"`
	NonKernelUp      int `json:"nonKernelUp"`
	NonKernelUpCalls int `json:"nonKernelUpCalls"`
	TypeErrors       int `json:"typeErrors"`
}

// SymCount is a symbol and how many references name it.
type SymCount struct {
	Symbol string `json:"symbol"`
	Class  string `json:"class"`
	Count  int    `json:"count"`
	Calls  int    `json:"calls"`
}

// Edge is all references from one group to another.
type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Refs  int    `json:"refs"`
	Calls int    `json:"calls"`
	// Reverse counts the references the other way, To to From.
	Reverse int        `json:"reverse"`
	Top     []SymCount `json:"top"`
}

// GroupRow is one group's line in the per-group table.
type GroupRow struct {
	Name     string `json:"name"`
	Placed   bool   `json:"placed"`
	Files    int    `json:"files"`
	Lines    int    `json:"lines"`
	Inbound  int    `json:"inbound"`
	Outbound int    `json:"outbound"`
	UpOut    int    `json:"upOut"`
	// DownOut is Outbound - UpOut. UpSyms counts the distinct symbols those
	// upward references name: what has to be cut.
	DownOut int `json:"downOut"`
	UpSyms  int `json:"upSymbols"`
	// ToKernel counts the references to the kernel group. They never point up,
	// but what they name is declared in package core (Service among it), which
	// a package below it cannot import.
	ToKernel int  `json:"toKernel"`
	Ready    bool `json:"ready"`
}

// Report is the default report.
type Report struct {
	Summary     Summary    `json:"summary"`
	UpwardEdges []Edge     `json:"heaviestUpwardEdges"`
	Groups      []GroupRow `json:"groups"`
}

// Analysis answers questions about a Loaded package under a grouping.
type Analysis struct {
	ld     *Loaded
	groups *Groups
	// order lists every group bottom first: the grouping file's order, then
	// the unplaced groups by name.
	order    []string
	pos      map[string]int
	files    map[string]int // group -> files
	lines    map[string]int // group -> lines
	unplaced []string
	// stemPlaced is Summary.StemPlaced.
	stemPlaced []string
	// kernel holds the groups made of the kernel line's files: "kernel", or
	// with PerFile each of those files.
	kernel map[string]bool
}

// Analyze prepares ld for reporting. Groups are judged by the grouping file's
// order. With PerFile that file orders no group (files are not domains), so the
// order is the suggested one.
func Analyze(ld *Loaded, groups *Groups) *Analysis {
	a := &Analysis{ld: ld, groups: groups, pos: map[string]int{}, files: map[string]int{}, lines: map[string]int{}, kernel: map[string]bool{}}
	for _, f := range ld.Files {
		g := groups.Of(f)
		a.files[g]++
		a.lines[g] += ld.Lines[f]
		d := groups.Domain(f)
		if d == kernelGroup {
			a.kernel[g] = true
		}
		if !groups.Listed(f) && groups.Placed(d) && (!groups.PerFile || d == kernelGroup) {
			a.stemPlaced = append(a.stemPlaced, f)
		}
	}
	if !groups.PerFile {
		a.order = append(a.order, groups.Order...)
	}
	var rest []string
	for g := range a.files {
		if groups.PerFile || !groups.Placed(g) {
			rest = append(rest, g)
		}
	}
	sort.Strings(rest)
	a.order = append(a.order, rest...)
	for i, g := range a.order {
		a.pos[g] = i
	}
	if groups.PerFile {
		return a.SuggestOrder()
	}
	a.unplaced = rest
	return a
}

// up reports whether a reference from -> to points up the order.
func (a *Analysis) up(r Ref) bool { return a.pos[r.ToGroup] > a.pos[r.FromGroup] }

// Report builds the default report with the top heaviest upward edges.
func (a *Analysis) Report(top int) Report {
	s := Summary{Package: a.ld.ImportPath, ServiceFields: a.ld.ServiceFields,
		ServiceMeths: a.ld.ServiceMeths, TypeErrors: a.ld.TypeErrors,
		Unplaced: a.unplaced, StemPlaced: a.stemPlaced}
	for _, g := range a.order {
		if a.files[g] > 0 {
			s.Groups++
		}
	}
	rows := map[string]*GroupRow{}
	for _, g := range a.order {
		if a.files[g] > 0 {
			rows[g] = &GroupRow{Name: g, Placed: a.groups.PerFile || a.groups.Placed(g), Files: a.files[g], Lines: a.lines[g]}
		}
	}
	adj := map[string][]string{}
	seen := map[[2]string]bool{}
	upSyms := map[string]map[string]bool{}
	for _, r := range a.ld.Refs {
		s.CrossRefs++
		up := a.up(r)
		kernel := a.kernel[r.FromGroup] || a.kernel[r.ToGroup]
		if a.kernel[r.ToGroup] && !a.kernel[r.FromGroup] {
			rows[r.FromGroup].ToKernel++
		}
		switch {
		case up:
			s.Up++
			if r.Call {
				s.UpCalls++
			}
			rows[r.FromGroup].UpOut++
			if upSyms[r.FromGroup] == nil {
				upSyms[r.FromGroup] = map[string]bool{}
			}
			upSyms[r.FromGroup][declKey(r)] = true
		default:
			s.Down++
		}
		if !kernel {
			if up {
				s.NonKernelUp++
				if r.Call {
					s.NonKernelUpCalls++
				}
			} else {
				s.NonKernelDown++
			}
		}
		rows[r.FromGroup].Outbound++
		rows[r.ToGroup].Inbound++
		if k := [2]string{r.FromGroup, r.ToGroup}; !seen[k] {
			seen[k] = true
			adj[r.FromGroup] = append(adj[r.FromGroup], r.ToGroup)
		}
	}
	for _, v := range adj {
		sort.Strings(v)
	}
	var nodes []string
	for _, g := range a.order {
		if a.files[g] > 0 {
			nodes = append(nodes, g)
		}
	}
	sccs := tarjan(nodes, adj)
	s.SCCs = len(sccs)
	for _, c := range sccs {
		sort.Strings(c)
		if len(c) > 1 {
			s.Cycles = append(s.Cycles, c)
		}
	}
	sort.SliceStable(sccs, func(i, j int) bool { return len(sccs[i]) > len(sccs[j]) })
	if len(sccs) > 0 {
		s.LargestSCC, s.LargestSCCOf = len(sccs[0]), sccs[0]
	}
	sort.Slice(s.Cycles, func(i, j int) bool {
		if len(s.Cycles[i]) != len(s.Cycles[j]) {
			return len(s.Cycles[i]) > len(s.Cycles[j])
		}
		return s.Cycles[i][0] < s.Cycles[j][0]
	})

	var gr []GroupRow
	for _, g := range a.order {
		if r := rows[g]; r != nil {
			r.Ready = r.UpOut == 0 && r.ToKernel == 0
			r.DownOut = r.Outbound - r.UpOut
			r.UpSyms = len(upSyms[g])
			if r.Ready {
				s.Ready = append(s.Ready, g)
			}
			gr = append(gr, *r)
		}
	}
	s.Top, s.Subpackages = a.ld.Top, a.ld.Subpackages
	s.Top.TypedFiles = len(a.ld.Files)
	return Report{Summary: s, UpwardEdges: a.upwardEdges(top), Groups: gr}
}

// sizes counts the files and lines of dir, and of each directory below it, by
// file name (build constraints are not applied). It fails rather than leave out
// what it cannot read.
func sizes(dir string) (PkgSize, []PkgSize, error) {
	by := map[string]*PkgSize{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		n := d.Name()
		hidden := strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")
		if d.IsDir() {
			if p != dir && (hidden || n == "testdata" || n == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		// The go command ignores files named like this too.
		if hidden || !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, err := os.ReadFile(p) // #nosec G304,G122 -- a Go file under the directory being mapped
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, filepath.Dir(p))
		if err != nil {
			return err
		}
		sz := by[rel]
		if sz == nil {
			sz = &PkgSize{Dir: filepath.ToSlash(rel)}
			by[rel] = sz
		}
		if strings.HasSuffix(p, "_test.go") {
			sz.TestFiles++
			sz.TestLines += countLines(b)
		} else {
			sz.Files++
			sz.Lines += countLines(b)
		}
		return nil
	})
	if err != nil {
		return PkgSize{}, nil, err
	}
	top := PkgSize{Dir: "."}
	if t := by["."]; t != nil {
		top = *t
	}
	var subs []PkgSize
	for k, v := range by {
		if k != "." {
			subs = append(subs, *v)
		}
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].Dir < subs[j].Dir })
	return top, subs, nil
}

// symbolCounts counts references per symbol (key tells symbols apart), most first.
func symbolCounts(refs []Ref, key func(Ref) string) []SymCount {
	m := map[string]*SymCount{}
	for _, r := range refs {
		k := key(r)
		c := m[k]
		if c == nil {
			c = &SymCount{Symbol: r.Symbol, Class: r.Class}
			m[k] = c
		}
		c.Count++
		if r.Call {
			c.Calls++
		}
	}
	out := make([]SymCount, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) { // ties keep the declaration's order
		out = append(out, *m[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}

func declKey(r Ref) string { return fmt.Sprintf("%s\x00%s:%d", r.Symbol, r.DeclFile, r.DeclLine) }

// edges aggregates every edge, optionally only the upward ones.
func (a *Analysis) edges(upOnly bool) []Edge {
	by := map[[2]string][]Ref{}
	for _, r := range a.ld.Refs {
		if upOnly && !a.up(r) {
			continue
		}
		k := [2]string{r.FromGroup, r.ToGroup}
		by[k] = append(by[k], r)
	}
	rev := map[[2]string]int{}
	for _, r := range a.ld.Refs {
		rev[[2]string{r.FromGroup, r.ToGroup}]++
	}
	var out []Edge
	for k, refs := range by {
		e := Edge{From: k[0], To: k[1], Refs: len(refs), Reverse: rev[[2]string{k[1], k[0]}]}
		for _, r := range refs {
			if r.Call {
				e.Calls++
			}
		}
		e.Top = symbolCounts(refs, declKey)
		if len(e.Top) > 5 {
			e.Top = e.Top[:5]
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Refs != out[j].Refs {
			return out[i].Refs > out[j].Refs
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

func (a *Analysis) upwardEdges(top int) []Edge {
	e := a.edges(true)
	if top >= 0 && len(e) > top {
		e = e[:top]
	}
	return e
}

func (a *Analysis) hasGroup(g string) bool { return a.files[g] > 0 }

// EdgeRefs lists every reference from group from to group to.
func (a *Analysis) EdgeRefs(from, to string) ([]Ref, error) {
	for _, g := range []string{from, to} {
		if !a.hasGroup(g) {
			return nil, fmt.Errorf("no group %q (groups: %s)", g, strings.Join(a.groupNames(), " "))
		}
	}
	var out []Ref
	for _, r := range a.ld.Refs {
		if r.FromGroup == from && r.ToGroup == to {
			out = append(out, r)
		}
	}
	return out, nil
}

func (a *Analysis) groupNames() []string {
	var n []string
	for _, g := range a.order {
		if a.hasGroup(g) {
			n = append(n, g)
		}
	}
	return n
}

// GroupUse is what one group needs from the others, and what they need from it.
type GroupUse struct {
	Group string `json:"group"`
	// Needs: symbols other groups declare that Group uses.
	Needs []UseLine `json:"needs"`
	// NeededBy: symbols Group declares that other groups use.
	NeededBy []UseLine `json:"neededBy"`
}

// UseLine is one symbol, the group on its other side, and the reference count.
type UseLine struct {
	Other  string `json:"other"`
	Symbol string `json:"symbol"`
	Class  string `json:"class"`
	Count  int    `json:"count"`
	Calls  int    `json:"calls"`
	Up     bool   `json:"up"`
}

// Group reports what group g needs from other groups and what they need from it.
func (a *Analysis) Group(g string) (GroupUse, error) {
	if !a.hasGroup(g) {
		return GroupUse{}, fmt.Errorf("no group %q (groups: %s)", g, strings.Join(a.groupNames(), " "))
	}
	u := GroupUse{Group: g}
	need := map[string]*UseLine{}
	by := map[string]*UseLine{}
	for _, r := range a.ld.Refs {
		var m map[string]*UseLine
		var other string
		switch {
		case r.FromGroup == g:
			m, other = need, r.ToGroup
		case r.ToGroup == g:
			m, other = by, r.FromGroup
		default:
			continue
		}
		k := other + "\x00" + declKey(r)
		l := m[k]
		if l == nil {
			l = &UseLine{Other: other, Symbol: r.Symbol, Class: r.Class, Up: a.up(r)}
			m[k] = l
		}
		l.Count++
		if r.Call {
			l.Calls++
		}
	}
	u.Needs, u.NeededBy = sortedUse(need), sortedUse(by)
	return u, nil
}

func sortedUse(m map[string]*UseLine) []UseLine {
	rank := map[string]int{}
	for i, c := range classOrder {
		rank[c] = i
	}
	out := make([]UseLine, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) { // ties keep the declaration's order
		out = append(out, *m[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Class != b.Class {
			return rank[a.Class] < rank[b.Class]
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		return a.Other < b.Other
	})
	return out
}

// Candidate is a symbol referenced from many groups.
type Candidate struct {
	Symbol   string `json:"symbol"`
	Class    string `json:"class"`
	DeclFile string `json:"declFile"`
	DeclLine int    `json:"declLine"`
	Group    string `json:"declGroup"`
	Groups   int    `json:"groups"`
	Refs     int    `json:"refs"`
}

// KernelCandidates lists symbols referenced from at least minGroups groups
// other than the one declaring them.
func (a *Analysis) KernelCandidates(minGroups int) []Candidate {
	type acc struct {
		c      Candidate
		groups map[string]bool
	}
	m := map[string]*acc{}
	for _, r := range a.ld.Refs {
		k := declKey(r)
		x := m[k]
		if x == nil {
			x = &acc{c: Candidate{Symbol: r.Symbol, Class: r.Class, DeclFile: r.DeclFile, DeclLine: r.DeclLine, Group: r.ToGroup}, groups: map[string]bool{}}
			m[k] = x
		}
		x.groups[r.FromGroup] = true
		x.c.Refs++
	}
	var out []Candidate
	for _, x := range m {
		x.c.Groups = len(x.groups)
		if x.c.Groups >= minGroups {
			out = append(out, x.c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Groups != out[j].Groups {
			return out[i].Groups > out[j].Groups
		}
		if out[i].Refs != out[j].Refs {
			return out[i].Refs > out[j].Refs
		}
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		if out[i].DeclFile != out[j].DeclFile {
			return out[i].DeclFile < out[j].DeclFile
		}
		return out[i].DeclLine < out[j].DeclLine
	})
	return out
}

// tarjan returns the strongly connected components of adj over nodes.
func tarjan(nodes []string, adj map[string][]string) [][]string {
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var res [][]string
	n := 0
	var strong func(v string)
	strong = func(v string) {
		index[v], low[v] = n, n
		n++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if _, ok := index[w]; !ok {
				strong(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			res = append(res, comp)
		}
	}
	for _, v := range nodes {
		if _, ok := index[v]; !ok {
			strong(v)
		}
	}
	return res
}
