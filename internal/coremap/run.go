package coremap

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// Run is the command: it parses args, type-checks the package once and writes
// the report (or the queries asked for) to stdout; timings go to stderr.
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
	suggest := fs.Bool("suggest-order", false, "order the groups to minimise upward references (kernel first) and report under that order")
	affinity := fs.Bool("affinity", false, "list each group with the groups it exchanges the most references with")
	perFile := fs.Bool("per-file", false, "make every file its own group (file-level affinity and order)")
	writeUpward := fs.Bool("write-upward", false, "write the group pairs that refer upward to -upward and print nothing else")
	upward := fs.String("upward", "internal/coremap/upward.txt", "the baseline `file` -write-upward writes")
	minGroups := fs.Int("min-groups", 10, "the group count -kernel-candidates needs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var from, to string
	if *edge != "" {
		var ok bool
		if from, to, ok = strings.Cut(*edge, ">"); !ok || from == "" || to == "" {
			return fmt.Errorf("-edge wants A>B, got %q", *edge)
		}
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

	if *writeUpward {
		pairs := a.UpwardPairs()
		out, err := os.Create(*upward) // #nosec G304 -- the baseline file named by this tool's flag
		if err != nil {
			return err
		}
		if err := WriteUpward(out, pairs); err != nil {
			_ = out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "wrote %s: %d upward pairs of %d group pairs with references\n", *upward, len(pairs), a.AllPairs())
		return nil
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
	queried := false
	if *affinity {
		queried = true
		af := a.Affinities()
		if err := emit(af, func() { WriteAffinity(stdout, af) }); err != nil {
			return err
		}
	}
	if *edge != "" {
		queried = true
		refs, err := a.EdgeRefs(from, to)
		if err != nil {
			return err
		}
		if err := emit(refs, func() { WriteEdge(stdout, from, to, refs) }); err != nil {
			return err
		}
	}
	if *group != "" {
		queried = true
		u, err := a.Group(*group)
		if err != nil {
			return err
		}
		if err := emit(u, func() { WriteGroup(stdout, u) }); err != nil {
			return err
		}
	}
	if *kc {
		queried = true
		cs := a.KernelCandidates(*minGroups)
		if err := emit(cs, func() { WriteCandidates(stdout, *minGroups, cs) }); err != nil {
			return err
		}
	}
	if queried {
		return nil
	}
	r := a.Report(*top)
	if *suggest {
		return emit(r, func() { WriteOrder(stdout, r) })
	}
	return emit(r, func() { WriteReport(stdout, r) })
}
