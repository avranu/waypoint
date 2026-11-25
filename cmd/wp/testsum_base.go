package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

// wp testsum --base REF: is a failure this change's, or was it failing
// before? Agents answered it by stashing their work or checking main's files
// out over it, in a tree others may share; this runs the failing tests in a
// temporary checkout of REF instead (WP-935).

// baseVerdicts, in the order they're listed. The first counts as already
// there; the next five as this change's (or a flake); the last as neither.
const (
	baseFailsToo   = "fails there too"
	basePasses     = "passes there"
	baseOtherwise  = "fails differently" // the package failed there, but not this way
	baseSkipped    = "skipped there"
	baseMissing    = "isn't there"
	baseNotFailing = "didn't fail there" // a format that names only failures
	baseUnknown    = "no result there"   // its package failed there first
)

var baseVerdictOrder = []string{baseFailsToo, basePasses, baseOtherwise, baseSkipped, baseMissing, baseNotFailing, baseUnknown}

func (c *cli) testsumBase(argv []string, format, ref string, keep []string) error {
	if _, err := baseCommit(ref); err != nil {
		return c.usage("%s", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rec := newTestRunRecorder(argv)              // as typed
	key := slotRunFor("wp testsum --base", argv) // the hold can include a second run at the base
	argv = goTestJSON(argv)
	env, release, err := c.takeSlotFor(ctx, "tests", defaultSlots(), key)
	if err != nil {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(c.err, "wp testsum: stopped while waiting for the slot")
			return exitCode(130)
		}
		return err
	}
	defer release()
	env, dropped := testEnv(env, keep)
	rec.started()
	var buf bytes.Buffer
	code, err := runSlotCmd(ctx, env, argv, nil, &buf, &buf)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		_, _ = fmt.Fprintln(c.err, "wp testsum: stopped; nothing was kept")
		return exitCode(130)
	}
	res, exit := c.testsumReport(buf.Bytes(), format, code, rec)
	if exit != nil {
		c.withheld(dropped)
	}
	if res == nil || len(res.Failures) == 0 {
		return exit // nothing failed, or nothing to name: nothing to compare
	}

	dir, cleanup, sha, err := baseWorktree(ref)
	if err != nil {
		_, _ = fmt.Fprintf(c.err, "wp testsum: --base %s: %v\n", ref, err)
		return exit
	}
	defer cleanup()
	if _, err := os.Stat(dir); err != nil {
		// The folder this ran in is new: so is everything in it.
		c.println(fmt.Sprintf("\nAt %s (%s), this folder isn't there: every failure is this change's.", ref, shortSHA(sha)))
		return exit
	}
	again := baseArgv(argv, res.Failures)
	var bbuf bytes.Buffer
	bcode, err := runSlotCmdIn(ctx, dir, env, again, nil, &bbuf, &bbuf)
	if err != nil {
		_, _ = fmt.Fprintf(c.err, "wp testsum: --base %s: %v\n", ref, err)
		return exit
	}
	if ctx.Err() != nil {
		_, _ = fmt.Fprintln(c.err, "wp testsum: stopped during the --base run")
		return exitCode(130)
	}
	bres, _, _ := parseTestOutput(bbuf.Bytes(), format)
	c.println(baseText(ref, sha, res.Failures, bres, bcode, bbuf.Bytes(), slices.Equal(again, argv), argv))
	return exit
}

// baseArgv is the command to run at the base: for a go test whose failures
// are all named tests, only those (by their top-level name, as -run takes
// it); otherwise the whole command, since a package that timed out or didn't
// build has no test to name.
func baseArgv(argv []string, failures []testFailure) []string {
	if len(argv) < 2 || argv[0] != "go" || argv[1] != "test" {
		return argv
	}
	var names []string
	for _, f := range failures {
		if f.Test == "" {
			return argv
		}
		top, _, _ := strings.Cut(f.Test, "/")
		if q := regexp.QuoteMeta(top); !slices.Contains(names, q) {
			names = append(names, q)
		}
	}
	out := []string{"go", "test"}
	rest := argv[2:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "-args" {
			out = append(out, rest[i:]...)
			break
		}
		name, _, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && (name == "run" || name == "test.run") {
			if !hasVal {
				i++ // its value is the next word
			}
			continue
		}
		out = append(out, a)
	}
	run := "-run=^(" + strings.Join(names, "|") + ")$"
	// Before -args, after the other flags (go test reads flags before the
	// packages too, so the place among them doesn't matter).
	if k := slices.Index(out, "-args"); k >= 0 {
		return slices.Concat(out[:k], []string{run}, out[k:])
	}
	return append(out, run)
}

// baseVerdict is what the base run says about one failure here.
func baseVerdict(f testFailure, base *testResult) string {
	if base == nil {
		return baseUnknown
	}
	pkgFailed, testFailed := false, false // in f's package, there
	for _, b := range base.Failures {
		if b.Name == f.Name {
			return baseFailsToo
		}
		if f.Pkg != "" && b.Pkg == f.Pkg {
			if b.Test == "" {
				pkgFailed = true
			} else {
				testFailed = true
			}
		}
	}
	switch {
	case base.Ran == nil:
		// Only failures are named (pytest, junit, go test's plain text):
		// not failing there is all that's known.
		return baseNotFailing
	case f.Test == "":
		// A package that timed out or didn't build here: there, a test of
		// it failed, or it failed as a whole some other way (a timeout
		// here, a build error there), or it didn't.
		if testFailed || pkgFailed {
			return baseOtherwise
		}
		return basePasses
	case base.Ran[f.Name]:
		return basePasses
	case pkgFailed:
		return baseUnknown // didn't build, or timed out before the test ran
	case base.Skips[f.Name]:
		return baseSkipped
	}
	return baseMissing // a new or renamed test
}

func baseText(ref, sha string, failures []testFailure, base *testResult, code int, data []byte, whole bool, argv []string) string {
	var b strings.Builder
	what := "the failing tests"
	if whole {
		what = "the same command"
	}
	fmt.Fprintf(&b, "\nAt %s (%s), %s:", ref, shortSHA(sha), what)
	if base == nil {
		fmt.Fprintf(&b, "\n  no test results there (exit %d), so nothing to compare; last lines:\n%s", code, lastLines([]byte(redactKept(string(plainOutput(data)))), 10))
		return b.String()
	}
	byVerdict := map[string][]testFailure{}
	for _, f := range failures {
		v := baseVerdict(f, base)
		byVerdict[v] = append(byVerdict[v], f)
	}
	for _, v := range baseVerdictOrder {
		for _, f := range byVerdict[v] {
			fmt.Fprintf(&b, "\n  %-17s %s", v, f.Name)
		}
	}
	old, unknown := len(byVerdict[baseFailsToo]), len(byVerdict[baseUnknown])
	mine := len(failures) - old - unknown
	switch {
	case mine == 0 && unknown == 0:
		fmt.Fprintf(&b, "\nAll %d failure%s were there before this change.", old, plural(old))
	case old == 0 && unknown == 0:
		b.WriteString("\nNone failed there: this change, or a flake.")
	case mine > 0:
		fmt.Fprintf(&b, "\n%d of %d failed there too; the rest are this change's, or flakes.", old, len(failures))
	}
	if unknown > 0 {
		var why []string
		for _, f := range base.Failures {
			if f.Test == "" {
				why = append(why, f.Name)
			}
		}
		fmt.Fprintf(&b, "\n%d had no result there: %s failed there first.", unknown, strings.Join(why, ", "))
	}
	if p := byVerdict[basePasses]; len(p) > 0 && p[0].Test != "" {
		top, _, _ := strings.Cut(p[0].Test, "/")
		again := slices.Concat([]string{"go", "test"}, buildFlags(argv), []string{"-run", "^" + regexp.QuoteMeta(top) + "$", p[0].Pkg})
		fmt.Fprintf(&b, "\nA flake? wp testsum --repeat 20 %s", shellJoin(again))
	}
	return b.String()
}

// buildFlags are a go test's flags that change what a test sees (-race,
// -tags...), so the flake hint runs the test as it failed: a race-only flake
// passes without -race.
func buildFlags(argv []string) []string {
	if len(argv) < 2 || argv[0] != "go" || argv[1] != "test" {
		return nil
	}
	var out []string
	rest := argv[2:]
	for i := 0; i < len(rest) && rest[i] != "-args"; i++ {
		a := rest[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch name {
		case "race", "short", "msan", "asan", "cover":
			out = append(out, a)
		case "tags", "cpu", "ldflags", "gcflags", "timeout", "coverpkg":
			out = append(out, a)
			if !hasVal && i+1 < len(rest) {
				i++
				out = append(out, rest[i])
			}
		}
	}
	return out
}
