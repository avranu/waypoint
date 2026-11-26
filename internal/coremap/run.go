package coremap

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// Run is the command: it parses args, type-checks the package once and writes
// the report (or the queries asked for) to stdout; timings go to stderr. When
// the package has type errors it writes the same and then returns an error,
// since the map is incomplete.
func Run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("coremap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "internal/core", "the package `directory` to map")
	groupsFile := fs.String("groups", "internal/coremap/groups.txt", "the grouping `file`")
	asJSON := fs.Bool("json", false, "write JSON instead of text")
	top := fs.Int("top", 5, "how many of the heaviest upward edges the report lists")
	edge := fs.String("edge", "", "list every reference on the edge `A>B` (groups)")
	group := fs.String("group", "", "list what `G` needs from other groups and what they need from it")
	kc := fs.Bool("kernel-candidates", false, "list symbols referenced from at least -min-groups groups")
	suggest := fs.Bool("suggest-order", false, "order the groups to reduce upward references (kernel first) and report under that order")
	affinity := fs.Bool("affinity", false, "list each group with the groups it exchanges the most references with")
	perFile := fs.Bool("per-file", false, "make every file its own group, in the suggested order (file-level affinity and order)")
	minGroups := fs.Int("min-groups", 10, "the group count -kernel-candidates needs")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var from, to string
	if *edge != "" {
		var ok bool
		if from, to, ok = strings.Cut(*edge, ">"); !ok || from == "" || to == "" {
			return fmt.Errorf("-edge wants A>B, got %q (quote it: a bare > is a shell redirect)", *edge)
		}
	}
	queries := 0
	for _, q := range []bool{*affinity, *edge != "", *group != "", *kc} {
		if q {
			queries++
		}
	}
	if *asJSON && queries > 1 {
		return errors.New("-json takes one query at a time")
	}

	f, err := os.Open(*groupsFile) // #nosec G304 -- the grouping file named by this tool's flag
	if err != nil {
		return err
	}
	groups, err := ParseGroups(f)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", *groupsFile, err)
	}
	groups.PerFile = *perFile
	ld, err := Load(*dir, groups, stderr)
	if err != nil {
		return err
	}
	a := Analyze(ld, groups)
	if *suggest {
		a = a.SuggestOrder()
	}
	if len(a.kernel) == 0 {
		fmt.Fprintf(stderr, "coremap: warning: no file is in a %q group: nothing is pinned first or counted apart\n", kernelGroup)
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	emit := func(v any, text func()) error {
		if *asJSON {
			return enc.Encode(v)
		}
		text()
		return nil
	}
	// write answers the queries, or writes the report when there are none.
	write := func() error {
		if *affinity {
			af := a.Affinities()
			if err := emit(af, func() { WriteAffinity(stdout, af) }); err != nil {
				return err
			}
		}
		if *edge != "" {
			refs, err := a.EdgeRefs(from, to)
			if err != nil {
				return err
			}
			if err := emit(refs, func() { WriteEdge(stdout, from, to, refs) }); err != nil {
				return err
			}
		}
		if *group != "" {
			u, err := a.Group(*group)
			if err != nil {
				return err
			}
			if err := emit(u, func() { WriteGroup(stdout, u) }); err != nil {
				return err
			}
		}
		if *kc {
			cs := a.KernelCandidates(*minGroups)
			if err := emit(cs, func() { WriteCandidates(stdout, *minGroups, cs) }); err != nil {
				return err
			}
		}
		if queries > 0 {
			return nil
		}
		r := a.Report(*top)
		if *suggest {
			return emit(r, func() { WriteOrder(stdout, r) })
		}
		return emit(r, func() { WriteReport(stdout, r) })
	}
	if err := write(); err != nil {
		return err
	}
	if ld.TypeErrors > 0 {
		return fmt.Errorf("%d type errors: the map is incomplete", ld.TypeErrors)
	}
	return nil
}
