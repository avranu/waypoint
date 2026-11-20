package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

// wp testsum --repeat: is a test flaky, and did the fix for a flake hold?
// One run can't say; N runs give a failure count per test and the range the
// true rate likely lies in (WP-935).

const testsumMaxRepeat = 1000

// repeatTally is one failing test across the runs.
type repeatTally struct {
	name  string
	fails int
	first testFailure
}

func (c *cli) testsumRepeat(argv []string, format string, n int, untilFail bool, keep []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rec := newTestRunRecorder(argv) // as typed
	argv = goTestCount1(goTestJSON(argv))
	env, release, err := c.takeSlot(ctx, "tests", defaultSlots())
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

	tallies := map[string]*repeatTally{}
	var order []string
	var runs, failedRuns, unexplained, empty int
	var firstFail, lastPass, lastEmpty *testResult
	stopped := false
	var firstFailData []byte
	firstFailCode, firstFailRun := 0, 0
	var secs []float64
	for i := 0; i < n; i++ {
		var buf bytes.Buffer
		start := time.Now()
		code, err := runSlotCmd(ctx, env, argv, nil, &buf, &buf)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			// The run the stop cut short says nothing; the finished ones
			// are still reported and kept.
			stopped = true
			break
		}
		runs++
		secs = append(secs, time.Since(start).Seconds())
		res, _, err := parseTestOutput(buf.Bytes(), format)
		if err != nil {
			res = nil
		}
		failed := code != 0 || res == nil || res.outcome() == runFail
		if res != nil {
			for _, f := range res.Failures {
				t := tallies[f.Name]
				if t == nil {
					t = &repeatTally{name: f.Name, first: f}
					tallies[f.Name] = t
					order = append(order, f.Name)
				}
				t.fails++
			}
		}
		if !failed && res != nil && res.outcome() == runEmpty {
			// No test ran (a -run that matches nothing): not a pass.
			empty++
			lastEmpty = res
			continue
		}
		if !failed {
			lastPass = res
			continue
		}
		failedRuns++
		if res == nil || len(res.Failures) == 0 {
			unexplained++
		}
		if firstFailRun == 0 {
			firstFail, firstFailData, firstFailCode, firstFailRun = res, buf.Bytes(), code, i+1
		}
		if untilFail {
			break
		}
	}

	// Keep one run for wp ship: a failure if there was one, so twenty
	// passes can't hide the run that failed.
	switch {
	case firstFailRun > 0 && firstFail != nil:
		rec.finish(firstFail, firstFailCode)
	case firstFailRun > 0:
		rec.failed(max(firstFailCode, 1))
	case lastPass != nil:
		rec.finish(lastPass, 0)
	case lastEmpty != nil:
		rec.finish(lastEmpty, 0)
	}

	if stopped {
		if runs == 0 {
			_, _ = fmt.Fprintln(c.err, "wp testsum: stopped before the first run finished")
			return exitCode(130)
		}
		_, _ = fmt.Fprintf(c.err, "wp testsum: stopped after %d of %d runs; what they showed:\n", runs, n)
	}
	if empty > 0 && empty == runs {
		c.println(fmt.Sprintf("NO TESTS RAN in any of %d runs: if you gave -run, it matched nothing, so this says nothing about a flake", runs))
	} else {
		c.println(repeatText(runs, n, failedRuns, empty, unexplained, firstFailRun, untilFail, tallies, order, lastPass, secs))
	}
	if firstFailRun > 0 && firstFail != nil {
		_, _ = fmt.Fprintf(c.out, "run %d, the first that failed:\n%s", firstFailRun, firstFail.render())
		if firstFailCode != 0 && firstFail.outcome() != runFail {
			_, _ = fmt.Fprintf(c.out, "but the command exited %d; last lines:\n%s\n", firstFailCode, lastLines(firstFailData, 20))
		}
	} else if firstFailRun > 0 {
		_, _ = fmt.Fprintf(c.out, "run %d exited %d and no test results were recognized; last lines:\n%s\n", firstFailRun, firstFailCode, lastLines(firstFailData, 20))
	}
	if firstFailRun > 0 {
		c.withheld(dropped)
	}
	switch {
	case stopped:
		return exitCode(130)
	case firstFailRun > 0, empty == runs:
		// Every run having run no test is no evidence a flake is gone.
		return exitCode(1)
	}
	return nil
}

func repeatText(runs, asked, failedRuns, empty, unexplained, firstFailRun int, untilFail bool, tallies map[string]*repeatTally, order []string, pass *testResult, secs []float64) string {
	var b strings.Builder
	slices.Sort(secs)
	each := fmtSec(quantile(secs, 0.5))
	tests := ""
	if pass != nil {
		if k := pass.Passed + pass.Failed + pass.Skipped; k > 0 {
			tests = fmt.Sprintf(", %d test%s", k, plural(k))
		}
	}
	lo, hi := wilson(failedRuns, runs)
	switch {
	case failedRuns == 0:
		ran := runs - empty
		_, hi = wilson(0, ran)
		fmt.Fprintf(&b, "PASS: %d of %d runs (%s each%s); a failure rate above %s would likely have shown", ran, ran, each, tests, pctText(hi))
		if empty > 0 {
			fmt.Fprintf(&b, "\n  %d more runs ran no tests and aren't counted", empty)
		}
	case untilFail:
		fmt.Fprintf(&b, "FAIL on run %d of up to %d: %d passed before it (%s each)", firstFailRun, asked, firstFailRun-1, each)
	case failedRuns == runs:
		fmt.Fprintf(&b, "FAIL: all %d runs failed (%s each): broken, not flaky", runs, each)
	default:
		fmt.Fprintf(&b, "FLAKY: %d of %d runs failed (%s; likely %s–%s; %s each)", failedRuns, runs, pctText(float64(failedRuns)/float64(runs)), pctText(lo), pctText(hi), each)
	}
	slices.SortStableFunc(order, func(a, c string) int { return tallies[c].fails - tallies[a].fails })
	const shown = 15
	for i, name := range order {
		if i == shown {
			fmt.Fprintf(&b, "\n  … %d more failing tests", len(order)-shown)
			break
		}
		t := tallies[name]
		fmt.Fprintf(&b, "\n  %s failed %d of %d", t.name, t.fails, runs)
		if t.first.Loc != "" {
			b.WriteString(" (at " + t.first.Loc + ")")
		}
	}
	if unexplained > 0 && len(order) > 0 {
		fmt.Fprintf(&b, "\n  %d failing runs named no failing test (the command itself failed)", unexplained)
	}
	return b.String()
}

// wilson is the 95% Wilson score interval for k successes in n trials.
func wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959964
	p, nf := float64(k)/float64(n), float64(n)
	den := 1 + z*z/nf
	mid := (p + z*z/(2*nf)) / den
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	return math.Max(0, mid-half), math.Min(1, mid+half)
}

func pctText(p float64) string {
	if p < 0.1 && p > 0 {
		return fmt.Sprintf("%.1f%%", p*100)
	}
	return fmt.Sprintf("%.0f%%", p*100)
}

// goTestCount1 adds -count=1 to a go test that doesn't set -count, so a
// repeat runs the tests rather than replaying the cached result.
func goTestCount1(argv []string) []string {
	if len(argv) < 2 || argv[0] != "go" || argv[1] != "test" {
		return argv
	}
	for _, a := range argv[2:] {
		if a == "-args" {
			break
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && name == "count" {
			return argv
		}
	}
	return append([]string{"go", "test", "-count=1"}, argv[2:]...)
}
