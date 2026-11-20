// Command ci-evidence prints a CI test job's results the way plain `go
// test` would, then the evidence lines Waypoint keeps (WP-704,
// internal/testevidence):
//
//	go run ./cmd/ci-evidence [-shard I/N] [-cover FILE]... [-suppressions] JSON...
//
// Each JSON is a `go test -json` output; each -cover a cover profile;
// -suppressions also lists the tree's #nosec, //nolint and //wplint:ignore comments. Run it
// from the module's root. .forgejo/scripts/gotest-shard.sh calls it when
// EVIDENCE is set.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.jmann.me/claude/waypoint/internal/testevidence"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ci-evidence:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("ci-evidence", flag.ContinueOnError)
	shard := fs.String("shard", "", "this job is shard `I/N` of the same tests")
	var covers list
	fs.Var(&covers, "cover", "a cover `profile` (repeatable)")
	sup := fs.Bool("suppressions", false, "list the tree's #nosec, //nolint and //wplint:ignore comments")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *shard != "" {
		if _, _, ok := testevidence.ShardOf(*shard); !ok {
			return fmt.Errorf("-shard %q: want I/N", *shard)
		}
	}
	module, err := modulePath("go.mod")
	if err != nil {
		return err
	}
	out := bufio.NewWriter(os.Stdout)
	p := &testevidence.Payload{Shard: *shard, Tests: []testevidence.Test{}}
	for _, name := range fs.Args() {
		tests, err := goTest(name, out, module)
		if err != nil {
			return err
		}
		p.Tests = append(p.Tests, tests...)
	}
	if len(covers) > 0 {
		c := testevidence.NewCoverage(module)
		for _, name := range covers {
			f, err := os.Open(filepath.Clean(name))
			if err != nil {
				return err
			}
			err = c.Add(f)
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		p.Cover = c.Files()
	}
	if *sup {
		if p.Suppressions, err = testevidence.Suppressions("."); err != nil {
			return err
		}
		p.ScannedSuppressions = true
	}
	if _, err := fmt.Fprintln(out, summary(p)); err != nil {
		return err
	}
	if err := testevidence.Encode(out, p); err != nil {
		return err
	}
	return out.Flush()
}

func goTest(name string, out *bufio.Writer, module string) ([]testevidence.Test, error) {
	f, err := os.Open(filepath.Clean(name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	tests, err := testevidence.GoTest(f, out, module)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return tests, nil
}

// summary is the line a person reading the log sees above the evidence.
func summary(p *testevidence.Payload) string {
	n := map[string]int{}
	for _, t := range p.Tests {
		n[t.Outcome]++
	}
	s := fmt.Sprintf("evidence for Waypoint (WP-704): %d tests (%d failed, %d skipped)", len(p.Tests), n[testevidence.Fail], n[testevidence.Skip])
	if p.Cover != nil {
		var total, covered int
		for _, f := range p.Cover {
			t, c := f.Statements()
			total += t
			covered += c
		}
		pct := 0.0
		if total > 0 {
			pct = 100 * float64(covered) / float64(total)
		}
		s += fmt.Sprintf(", %d files %.1f%% covered", len(p.Cover), pct)
		if p.Shard != "" {
			s += " by shard " + p.Shard
		}
	}
	if p.ScannedSuppressions {
		s += fmt.Sprintf(", %d suppressions", len(p.Suppressions))
	}
	return s
}

func modulePath(gomod string) (string, error) {
	b, err := os.ReadFile(filepath.Clean(gomod))
	if err != nil {
		return "", err
	}
	for l := range strings.SplitSeq(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	return "", fmt.Errorf("%s names no module", gomod)
}
