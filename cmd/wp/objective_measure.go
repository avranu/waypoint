package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// What an objective's check is read with (WP-927, WP-ADR111, WP-ADR142): one
// implementation for the runner's reading of a round (objective_run.go) and
// for the worker's own working reading (wp objective check). Both run the
// command in checkouts they are given, setup first, and read a time, a
// number the command prints or just its exit status.

const (
	// checkCapture is how much of a command's output is kept to read a
	// value from: the end of it.
	checkCapture = 64 << 10
	// objectiveMaxRuns is the most timed runs a side gets.
	objectiveMaxRuns = 9
)

// capTail keeps the last limit bytes written to it, however much is.
// A command's stdout and stderr are copied by two goroutines, and both
// reach the tail that keeps everything, so writes are locked.
type capTail struct {
	limit int
	mu    sync.Mutex
	b     []byte
}

func (c *capTail) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.b = append(c.b, p...)
	if len(c.b) > 2*c.limit {
		c.b = append([]byte(nil), c.b[len(c.b)-c.limit:]...)
	}
	return len(p), nil
}

func (c *capTail) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.b
	if len(b) > c.limit {
		b = b[len(b)-c.limit:]
	}
	return string(b)
}

// teeCap writes to two capped tails.
type teeCap struct{ a, b *capTail }

func (t *teeCap) Write(p []byte) (int, error) {
	_, _ = t.a.Write(p)
	return t.b.Write(p)
}

// checkRun is one run of a command.
type checkRun struct {
	Code     int
	Stdout   string // the end of what it printed
	All      string // the end of everything it wrote
	Wall     float64
	TimedOut bool
}

// runCheck runs command in dir under env and kills the whole process group
// when timeout (0: none) passes or ctx ends. A non-zero exit is an answer
// (Code); err is only a stop (ctx) or a command that couldn't start.
func runCheck(ctx context.Context, dir string, env []string, command string, timeout time.Duration) (checkRun, error) {
	rctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := shellCommand(rctx, command)
	setProcGroup(cmd)
	killOnCancel(cmd)
	cmd.Dir, cmd.Env = dir, env
	var out, all capTail
	out.limit, all.limit = checkCapture, checkCapture
	cmd.Stdout, cmd.Stderr = &teeCap{&out, &all}, &all
	start := time.Now()
	err := cmd.Run()
	wall := time.Since(start).Seconds()
	procExited(cmd)
	// Whatever it left running would load the machine through the runs
	// that follow, and could hold the checkout open.
	killGroup(cmd)
	r := checkRun{Stdout: out.String(), All: all.String(), Wall: wall}
	if ctx.Err() != nil {
		return r, ctx.Err()
	}
	if rctx.Err() != nil {
		r.TimedOut, r.Code = true, -1
		return r, nil
	}
	var ee *exec.ExitError
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		// A straggler holding the output pipe open is not a failure.
		if cmd.ProcessState != nil {
			r.Code = cmd.ProcessState.ExitCode()
		}
	case errors.As(err, &ee):
		r.Code = max(ee.ExitCode(), 1)
	default:
		return r, fmt.Errorf("can't run it: %w", err)
	}
	return r, nil
}

// measureSpec is what to read.
type measureSpec struct {
	Check, Setup string
	// Measure: time, value, pass or "".
	Measure, Unit string
	// Timeout is the longest one command may run (0: no limit); Budget
	// all the timed runs together (0: only MaxRuns limits them).
	Timeout, Budget time.Duration
	// MaxRuns is the most timed runs of a side (0: objectiveMaxRuns).
	MaxRuns int
	Env     []string
	// Prepared, if set, is called once the setup has run in every side and
	// before the check: what the setup builds in a working tree is not a
	// change the check should be said to have raced with.
	Prepared func()
	// MinRuns is the fewest timed runs a time may be read from (0: any);
	// fewer is a stop, not a reading.
	MinRuns int
}

// measureSide is a checkout the check is read in; Dir is the folder it runs
// in (the checkout's root, or a folder of it).
type measureSide struct{ Label, Dir string }

// sideRead is what the check read in one checkout.
type sideRead struct {
	OK        bool
	Exit      int
	Output    string
	Value     *float64
	Low, High *float64
	Runs      int
	samples   []float64
	failed    bool
	warm      float64
}

// measureStop is a read that couldn't be taken at all: the message says
// what and where, for a person to act on.
type measureStop struct{ msg string }

func (e *measureStop) Error() string { return e.msg }

func stopf(format string, a ...any) error { return &measureStop{fmt.Sprintf(format, a...)} }

func (s measureSpec) runs() int {
	if s.MaxRuns > 0 {
		return min(s.MaxRuns, benchMaxRuns)
	}
	return objectiveMaxRuns
}

// measureCheck reads the check in sides: setup in each (a side whose setup
// fails reads as failed; the first side, the head, stops the read), then for
// a time one untimed warm-up run of every side and timed runs in turns, the
// side each round starts with being the one the last ended with
// (H B | B H | H B …), so drift hits both alike. A value or a pass is one
// run. Only sides[0] is required to read; a later side that fails is an
// answer (OK false) and the others carry on. p is the Mann-Whitney p of the
// first two sides' times; exact says every run in each side read the same.
func measureCheck(ctx context.Context, spec measureSpec, sides []measureSide, say func(string, ...any)) (reads []*sideRead, p *float64, exact bool, err error) {
	if say == nil {
		say = func(string, ...any) {}
	}
	reads = make([]*sideRead, len(sides))
	for i := range reads {
		reads[i] = &sideRead{}
	}
	fail := func(i int, run checkRun, what string) {
		r := reads[i]
		r.failed, r.OK, r.Exit, r.Value, r.Low, r.High = true, false, run.Code, nil, nil, nil
		r.Output = what + trimOutput(run.All, 1<<30)
	}
	if spec.Setup != "" {
		for i, s := range sides {
			say("Setting up %s", s.Label)
			run, err := runCheck(ctx, s.Dir, spec.Env, spec.Setup, spec.Timeout)
			switch {
			case err != nil:
				return nil, nil, false, stopped(ctx, "setup in "+s.Label, err)
			case run.TimedOut:
				return nil, nil, false, stopf("the setup (%s) ran longer than %s in %s and was stopped", excerpt(spec.Setup, 80), spec.Timeout.Round(time.Second), s.Label)
			case run.Code != 0:
				if i == 0 {
					return nil, nil, false, stopf("the setup (%s) failed in %s with exit %d: %s", excerpt(spec.Setup, 80), s.Label, run.Code, excerpt(strings.TrimSpace(trimOutput(run.All, 1<<30)), 400))
				}
				fail(i, run, fmt.Sprintf("its setup (%s) failed with exit %d: ", excerpt(spec.Setup, 80), run.Code))
			}
		}
	}
	if spec.Prepared != nil {
		spec.Prepared()
	}
	timed := spec.Measure == "time"
	// one run of side i; false when the side has failed and must not run again
	runSide := func(i int) (checkRun, bool, error) {
		r := reads[i]
		if r.failed {
			return checkRun{}, false, nil
		}
		run, err := runCheck(ctx, sides[i].Dir, spec.Env, spec.Check, spec.Timeout)
		switch {
		case err != nil:
			return run, false, stopped(ctx, "the check in "+sides[i].Label, err)
		case run.TimedOut:
			more := ""
			if spec.Measure == "time" {
				more = ": one run of a timed check must fit well inside that, as a time needs several (make it faster, or aim at a pass or value target)"
			}
			return run, false, stopf("the check (%s) ran longer than %s in %s and was stopped%s", excerpt(spec.Check, 80), spec.Timeout.Round(time.Second), sides[i].Label, more)
		case run.Code != 0:
			fail(i, run, "")
			r.Runs = max(r.Runs, 1)
			return run, false, nil
		}
		r.Output = run.All
		return run, true, nil
	}
	start := time.Now()
	order := make([]int, len(sides))
	for i := range order {
		order[i] = i
	}
	// The first round: a time's warm-up, a value's or pass's only run.
	var warmCost float64
	for _, i := range order {
		say("Running the check in %s", sides[i].Label)
		run, ok, err := runSide(i)
		if err != nil {
			return nil, nil, false, err
		}
		r := reads[i]
		if !ok {
			if i == 0 {
				return reads, nil, false, nil
			}
			continue
		}
		warmCost += run.Wall
		r.warm = run.Wall
		switch spec.Measure {
		case "time":
		case "value":
			v := readValue(run.Stdout, spec.Unit)
			if v == nil {
				v = readValue(run.All, spec.Unit)
			}
			r.samples, r.Runs = []float64{}, 1
			if v == nil {
				r.failed, r.OK, r.Output = true, false, "it printed no number"+unitHint(spec.Unit)+": "+lastText(run.All)
				if i == 0 {
					return reads, nil, false, nil
				}
				continue
			}
			r.samples = append(r.samples, *v)
		}
	}
	if timed {
		// Timed runs, while the budget and the run count allow.
		for round := 1; round <= spec.runs(); round++ {
			if spec.Budget > 0 && time.Since(start)+time.Duration(warmCost*1.2*float64(time.Second)) > spec.Budget {
				break
			}
			ord := slices.Clone(order)
			if round%2 == 1 {
				slices.Reverse(ord)
			}
			for _, i := range ord {
				run, ok, err := runSide(i)
				if err != nil {
					return nil, nil, false, err
				}
				if !ok {
					if i == 0 {
						return reads, nil, false, nil
					}
					continue
				}
				reads[i].samples = append(reads[i].samples, run.Wall)
			}
		}
		if got := len(reads[0].samples); got < spec.MinRuns {
			return nil, nil, false, tooSlow(spec, reads, got)
		}
		for _, r := range reads {
			if !r.failed && len(r.samples) == 0 {
				r.samples = []float64{r.warm} // no minimum asked for: the warm-up is all there is
			}
		}
	}
	for _, r := range reads {
		if r.failed {
			continue
		}
		r.OK = true
		r.Runs = max(len(r.samples), 1)
		if len(r.samples) == 0 {
			continue // a pass
		}
		sorted := slices.Sorted(slices.Values(r.samples))
		m, lo, hi := quantile(sorted, 0.5), quantile(sorted, 0.25), quantile(sorted, 0.75)
		r.Value = &m
		if timed || spec.Measure == "value" {
			r.Low, r.High = &lo, &hi
		}
	}
	exact = len(reads) > 0 && !reads[0].failed && reads[0].Runs >= 1 && len(reads[0].samples) > 0
	for _, r := range reads {
		if r.failed {
			continue
		}
		if len(r.samples) == 0 || !slices.Equal(r.samples, slices.Repeat(r.samples[:1], len(r.samples))) || timed && len(r.samples) < 3 {
			exact = false
		}
	}
	if timed && len(reads) > 1 && !reads[0].failed && !reads[1].failed {
		p = mannWhitneyP(reads[0].samples, reads[1].samples)
	}
	return reads, p, exact, nil
}

// tooSlow is the stop for a time that couldn't be run enough times: how long
// one run took, how many fit, and the ways out.
func tooSlow(spec measureSpec, reads []*sideRead, got int) error {
	var took []string
	for i, r := range reads {
		if r.warm > 0 {
			name := "on the branch"
			if i > 0 {
				name = "at the round's start"
			}
			took = append(took, fmt.Sprintf("%s %s", (time.Duration(r.warm*float64(time.Second))).Round(10*time.Millisecond), name))
		}
	}
	return stopf("the check is too slow to time: one run took %s, so %d timed run(s) fit in the %s allowed and a time needs %d (each side runs once to warm up and the runs alternate between the branch and the round's start). Make the check faster, or aim at a pass or value target, which reads the check once",
		strings.Join(took, " and "), got, spec.Budget.Round(time.Second), spec.MinRuns)
}

// stopped says a read ended by ctx: a cancel is the caller's (returned as
// it is), what ran past its time a message.
func stopped(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return stopf("%s: %v", what, err)
}

func unitHint(unit string) string {
	if unit == "" {
		return " on its last line"
	}
	return " followed by " + strconv.Quote(unit)
}

// trimOutput is the text of out, trimmed; n bounds its bytes from the end.
func trimOutput(out string, n int) string {
	out = strings.TrimSpace(stripANSI(out))
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// lastText is the last line of out that has text on it.
func lastText(out string) string {
	lines := strings.Split(strings.TrimSpace(stripANSI(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return excerpt(l, 200)
		}
	}
	return ""
}

var escapeRE = lazyre.New("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[@-Z\\\\-_]")

// stripANSI removes terminal escapes (colour, cursor moves, titles).
func stripANSI(s string) string { return escapeRE.ReplaceAllString(s, "") }

// numberRE reads "1,204" as one number: a summary line's thousands.
var numberRE = lazyre.New(`[-+]?(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.[0-9]+)?(?:[eE][-+]?[0-9]+)?|[-+]?\.[0-9]+`)

type foundNumber struct {
	v          float64
	start, end int
}

// numbersIn is the numbers on a line. A sign counts only where it starts
// the number ("-5", "(−5)", "x = -5"), not where it joins two ("10-20",
// "v1-2"), so "10-20 ms" holds 10 and 20.
func numbersIn(line string) []foundNumber {
	var out []foundNumber
	for _, m := range numberRE.FindAllStringIndex(line, -1) {
		s, e := m[0], m[1]
		if line[s] == '-' || line[s] == '+' {
			if s > 0 {
				prev := line[s-1]
				if prev >= '0' && prev <= '9' || prev >= 'a' && prev <= 'z' || prev >= 'A' && prev <= 'Z' || prev == ')' || prev == '.' || prev == '_' || prev >= 0x80 {
					s++
				}
			}
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(line[s:e], "+"), ",", ""), 64)
		if err != nil {
			continue
		}
		out = append(out, foundNumber{v, s, e})
	}
	return out
}

// readValue is the number a check prints. With a unit ("%", "warnings"),
// the last number on the last line that has one followed by the unit ("81.3%
// of 1204 statements" with "%" reads 81.3); without, the last number on the
// last line with text. No number: nil. Terminal escapes are stripped and
// the last carriage-returned state of a progress line is the one read.
func readValue(out, unit string) *float64 {
	lines := strings.Split(strings.ReplaceAll(stripANSI(out), "\r\n", "\n"), "\n")
	for i, l := range lines {
		if j := strings.LastIndex(l, "\r"); j >= 0 {
			if t := strings.TrimSpace(l[j+1:]); t != "" {
				lines[i] = l[j+1:]
			} else {
				lines[i] = strings.TrimSpace(l[:j])
				if k := strings.LastIndex(lines[i], "\r"); k >= 0 {
					lines[i] = lines[i][k+1:]
				}
			}
		}
	}
	unit = strings.TrimSpace(unit)
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		nums := numbersIn(l)
		if unit == "" {
			if len(nums) == 0 {
				return nil
			}
			v := nums[len(nums)-1].v
			return &v
		}
		// The unit is matched in the line as printed, whatever its case:
		// lowering the line first would move the numbers' byte offsets
		// wherever a letter's lower case is a different length.
		for k := len(nums) - 1; k >= 0; k-- {
			rest := strings.TrimLeft(l[nums[k].end:], " \t")
			if len(rest) < len(unit) || !strings.EqualFold(rest[:len(unit)], unit) {
				continue
			}
			// "ms" isn't "m": a unit of letters ends at a word's end.
			if after := rest[len(unit):]; after != "" && isLetter(unit[len(unit)-1]) && isLetter(after[0]) {
				continue
			}
			v := nums[k].v
			return &v
		}
	}
	return nil
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80 }
