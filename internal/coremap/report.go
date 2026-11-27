package coremap

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func writeList(w io.Writer, label string, items []string) {
	if len(items) == 0 {
		fmt.Fprintf(w, "%s: none\n", label)
		return
	}
	fmt.Fprintf(w, "%s (%d): %s\n", label, len(items), strings.Join(items, " "))
}

// WriteReport renders the default report as text. Ordering is stable, so two
// runs over the same tree diff cleanly.
func WriteReport(w io.Writer, r Report) {
	s := r.Summary
	fmt.Fprintf(w, "coremap: %s\n\n== summary ==\n", s.Package)
	fmt.Fprintf(w, "%s: %d non-test files, %d lines (%d files type-checked); %d test files, %d lines\n",
		s.Top.Dir, s.Top.Files, s.Top.Lines, s.Top.TypedFiles, s.Top.TestFiles, s.Top.TestLines)
	for _, p := range s.Subpackages {
		fmt.Fprintf(w, "%s: %d non-test files, %d lines; %d test files, %d lines\n",
			p.Dir, p.Files, p.Lines, p.TestFiles, p.TestLines)
	}
	fmt.Fprintf(w, "type Service: %d fields, %d methods\n", s.ServiceFields, s.ServiceMeths)
	fmt.Fprintf(w, "groups: %d; strongly connected components: %d, largest %d groups\n", s.Groups, s.SCCs, s.LargestSCC)
	fmt.Fprintf(w, "cross-group references: %d; down %d (%.1f%%), up %d (%.1f%%), upward calls %d\n",
		s.CrossRefs, s.Down, pct(s.Down, s.CrossRefs), s.Up, pct(s.Up, s.CrossRefs), s.UpCalls)
	nk := s.NonKernelDown + s.NonKernelUp
	fmt.Fprintf(w, "  not touching the kernel group: %d; down %d (%.1f%%), up %d, upward calls %d\n",
		nk, s.NonKernelDown, pct(s.NonKernelDown, nk), s.NonKernelUp, s.NonKernelUpCalls)
	for _, c := range s.Cycles {
		writeList(w, "  cycle", c)
	}
	writeList(w, "unplaced groups (not in the grouping file, at the top of the order)", s.Unplaced)
	writeList(w, "files no line matches, put in a listed group by their stem", s.StemPlaced)
	writeList(w, "ready groups (no upward references and none to the kernel group)", s.Ready)
	if s.TypeErrors > 0 {
		fmt.Fprintf(w, "WARNING: %d type errors; the map may be incomplete\n", s.TypeErrors)
	}

	fmt.Fprintf(w, "\n== heaviest upward edges (from > to: refs, calls; top symbols) ==\n")
	for i, e := range r.UpwardEdges {
		var syms []string
		for _, c := range e.Top {
			syms = append(syms, fmt.Sprintf("%s(%d)", c.Symbol, c.Count))
		}
		fmt.Fprintf(w, "%2d. %s > %s: %d refs, %d calls (reverse %d); %s\n", i+1, e.From, e.To, e.Refs, e.Calls, e.Reverse, strings.Join(syms, " "))
	}

	fmt.Fprintf(w, "\n== groups (bottom of the order first) ==\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "group\tfiles\tlines\tin\tout\tup-out\tup-symbols\tto-kernel\tready\t")
	for _, g := range r.Groups {
		name := g.Name
		if !g.Placed {
			name += " (unplaced)"
		}
		ready := ""
		if g.Ready {
			ready = "ready"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t\n", name, g.Files, g.Lines, g.Inbound, g.Outbound, g.UpOut, g.UpSyms, g.ToKernel, ready)
	}
	_ = tw.Flush()
}

// WriteEdge renders the references of one edge.
func WriteEdge(w io.Writer, from, to string, refs []Ref) {
	calls := 0
	for _, r := range refs {
		if r.Call {
			calls++
		}
	}
	fmt.Fprintf(w, "edge %s > %s: %d refs, %d calls\n", from, to, len(refs), calls)
	for _, r := range refs {
		call := ""
		if r.Call {
			call = " call"
		}
		fmt.Fprintf(w, "%s:%d  %s  %s%s  (%s:%d)\n", r.File, r.Line, r.Symbol, r.Class, call, r.DeclFile, r.DeclLine)
	}
}

// WriteGroup renders what a group needs and what it provides.
func WriteGroup(w io.Writer, u GroupUse) {
	section := func(title string, lines []UseLine, side string) {
		total := 0
		for _, l := range lines {
			total += l.Count
		}
		fmt.Fprintf(w, "\n== %s: %d symbols, %d refs ==\n", title, len(lines), total)
		class := ""
		for _, l := range lines {
			if l.Class != class {
				class = l.Class
				fmt.Fprintf(w, "-- %s\n", class)
			}
			flags := ""
			if l.Calls > 0 {
				flags += fmt.Sprintf(" calls=%d", l.Calls)
			}
			if l.Up {
				flags += " UP"
			}
			fmt.Fprintf(w, "%5d  %-40s %s %s%s\n", l.Count, l.Symbol, side, l.Other, flags)
		}
	}
	fmt.Fprintf(w, "group %s", u.Group)
	section(u.Group+" needs from other groups", u.Needs, "from")
	section("other groups need from "+u.Group, u.NeededBy, "by")
}

// WriteCandidates renders the kernel candidates.
func WriteCandidates(w io.Writer, minGroups int, cs []Candidate) {
	fmt.Fprintf(w, "symbols referenced from at least %d other groups: %d\n", minGroups, len(cs))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "groups\trefs\tsymbol\tclass\tdeclared in\t")
	for _, c := range cs {
		fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s:%d (%s)\t\n", c.Groups, c.Refs, c.Symbol, c.Class, c.DeclFile, c.DeclLine, c.Group)
	}
	_ = tw.Flush()
}

// WriteOrder renders a suggested order: the names, then each group's upward
// and downward references out under it.
func WriteOrder(w io.Writer, r Report) {
	s := r.Summary
	fmt.Fprintf(w, "suggested order, bottom first (kernel kept first); references down %d (%.1f%%), up %d, upward calls %d\n",
		s.Down, pct(s.Down, s.CrossRefs), s.Up, s.UpCalls)
	var names []string
	for _, g := range r.Groups {
		names = append(names, g.Name)
	}
	fmt.Fprintf(w, "order: %s\n\n", strings.Join(names, " "))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "#\tgroup\tdown-out\tup-out\tup-symbols\t")
	for i, g := range r.Groups {
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%d\t\n", i+1, g.Name, g.DownOut, g.UpOut, g.UpSyms)
	}
	_ = tw.Flush()
}

// WriteAffinity renders each group's size and its top peers.
func WriteAffinity(w io.Writer, af []Affinity) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "group\tfiles\tlines\tcross-refs\ttop peers (refs exchanged, share of the group's total)\t")
	for _, x := range af {
		var peers []string
		for _, p := range x.Top {
			peers = append(peers, fmt.Sprintf("%s %d (%.0f%%)", p.Group, p.Refs, p.Share))
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t\n", x.Name, x.Files, x.Lines, x.Refs, strings.Join(peers, ", "))
	}
	_ = tw.Flush()
}
