package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var testsumCmd = &command{
	name: "testsum", group: "Code and CI", args: "[--all] [--repeat N | --base REF] [--keep-env NAME] CMD… | [--format F] [FILE|-]",
	summary: "run tests and summarize them: one line on pass, failures with file:line",
	about: `Runs CMD in the "tests" host slot, records the result for wp ship, and prints a
short summary (one line on pass; file:line + first lines on failure, plus full
log path if truncated). Flags go before CMD; secret-looking env vars are stripped
(--keep-env NAME keeps one). Pass a FILE or - to summarize saved output.

In repos with .waypoint/heavy-tests, a whole-package go test without -run scopes
to tests beside changed files unless --all or WP_TEST_ALL=1 is set.`,
	examples: []string{"wp testsum -- go test -run '^TestSlot' ./cmd/wp", "wp testsum pytest -q", "wp testsum --repeat 30 go test -run TestFlaky ./internal/core", "wp testsum --base origin/main go test ./cmd/wp", "go test -json -run '^TestSlot' ./cmd/wp | wp testsum"},
	run:      cmdTestsum,
}

const testsumMaxLines = 60
const testsumMsgLines = 5

type testFailure struct {
	Name string
	Loc  string // file:line
	Msg  []string
	// Runs and Fails: how often the test ran and failed, where it ran more
	// than once (go test -count=N); 0 where it ran once or isn't known.
	Runs, Fails int
	// Pkg and Test: a go test's package and name ("" Test for the package
	// itself), so it can be run again elsewhere.
	Pkg, Test string
}

// runsText says how often a test that ran more than once failed.
func (f testFailure) runsText() string {
	switch {
	case f.Runs < 2:
		return ""
	case f.Fails >= f.Runs:
		return fmt.Sprintf(" (failed all %d runs)", f.Runs)
	}
	return fmt.Sprintf(" (failed %d of %d runs: flaky)", f.Fails, f.Runs)
}

type testResult struct {
	Passed, Failed, Skipped int
	Seconds                 float64
	Units                   int      // packages or files
	UnitName                string   // "packages", "files"
	Names                   []string // the units' names, sorted, where the format gives them
	// NoneRan: the format counts tests and none ran (a -run that matches
	// nothing), which go test still calls ok.
	NoneRan  bool
	Failures []testFailure
	// Ran: the tests that ran (go's pkg.Test names), so a failure can be
	// told from a test that isn't there; nil where the format doesn't say.
	// Skips: the ones that only skipped.
	Ran, Skips map[string]bool
}

// outcome is what the run shows: pass, fail, or empty when no test ran.
func (r *testResult) outcome() string {
	switch {
	case r.Failed > 0 || len(r.Failures) > 0:
		return runFail
	case r.NoneRan:
		return runEmpty
	}
	return runPass
}

func cmdTestsum(c *cli, args []string) error {
	fs := c.flags()
	format := fs.String("format", "auto", "input `FORMAT`: auto, go, pytest, unittest or junit")
	repeat := fs.Int("repeat", 0, "run CMD `N` times: which tests fail how often (flaky or broken)")
	untilFail := fs.Bool("until-fail", false, "with --repeat, stop at the first failing run")
	base := fs.String("base", "", "if tests fail, rerun them at `REF` in a temporary checkout (your tree is left alone): failing there too?")
	all := fs.Bool("all", false, "run a whole-suite go test whole: without it the tests slot runs the tests beside your changes (WP-1877)")
	var keep keepEnvFlag
	fs.Var(&keep, "keep-env", "pass the secret-looking variable `NAME` to CMD (repeatable)")
	rest, dashed, err := c.parseBeforeCommand(fs, args)
	if err != nil {
		return err
	}
	if dashed && len(rest) == 0 {
		return c.usage("-- ends the flags; give the command after it")
	}
	var file string
	if !dashed && len(rest) > 0 && (rest[0] == "-" || isOutputFile(rest[0])) {
		more, err := c.parse(fs, rest[1:])
		if err != nil {
			return err
		}
		if len(more) > 0 {
			return c.usage("%s is a file to read, so only flags may follow it, not %s (to run a command: wp testsum -- CMD)", rest[0], more[0])
		}
		file, rest = rest[0], nil
	}
	switch *format {
	case "auto", "go", "pytest", "unittest", "junit":
	default:
		return c.usage("--format is auto, go, pytest, unittest or junit")
	}
	repeatSet := *untilFail
	fs.Visit(func(f *flag.Flag) { repeatSet = repeatSet || f.Name == "repeat" })
	if repeatSet {
		switch {
		case len(rest) == 0:
			return c.usage("--repeat runs a command: wp testsum --repeat N CMD…")
		case *repeat < 1 || *repeat > testsumMaxRepeat:
			return c.usage("--repeat is 1 to %d", testsumMaxRepeat)
		}
		if *base != "" {
			return c.usage("--base and --repeat don't go together: repeat first, then compare the failures with --base")
		}
		return c.testsumRepeat(rest, *format, *repeat, *untilFail, keep)
	}
	if *base != "" {
		if len(rest) == 0 {
			return c.usage("--base runs a command: wp testsum --base REF CMD…")
		}
		return c.testsumBase(rest, *format, *base, keep)
	}
	if len(rest) > 0 {
		var buf bytes.Buffer
		rec := newTestRunRecorder(rest)
		note := rec.unchanged(time.Now())
		sayUnchanged(c.err, note)
		argv := goTestJSON(rest)
		var dropped []string
		var nothing bool
		code, err := c.runInSlotWith("tests", defaultSlots(), argv, &buf, &buf, rec.started, func(env []string) []string {
			env, dropped = testEnv(env, keep)
			return env
		}, slotOpts{all: *all, who: "wp testsum", allCmd: "wp testsum --all --", rec: rec, nothing: &nothing})
		if err != nil {
			return err
		}
		if nothing {
			return nil
		}
		if note != "" {
			c.println(note)
		}
		_, err = c.testsumReport(buf.Bytes(), *format, code, rec)
		if err != nil {
			c.withheld(dropped)
		}
		return err
	}
	if len(keep) > 0 {
		return c.usage("--keep-env is for running CMD: output read from a file ran with whatever it had")
	}
	var data []byte
	if file != "" && file != "-" {
		data, err = os.ReadFile(file) // #nosec G304 G703 -- the user named the file on their own command line
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return err
	}
	res, err := c.testsumReport(data, *format, 0, nil)
	if res == nil && err != nil && file != "" && file != "-" {
		return fmt.Errorf("%w (read %s as saved test output; to run it: wp testsum -- sh %s)", err, file, shellJoin([]string{file}))
	}
	return err
}

// parseBeforeCommand reads the flags before the first argument that isn't
// one and returns the rest untouched, as nice and timeout do: the flags
// after it are the command's. dashed is whether a -- ended the flags.
func (c *cli) parseBeforeCommand(fs *flag.FlagSet, args []string) (rest []string, dashed bool, err error) {
	c.fs = fs
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if c.cur != nil {
				c.commandHelp(c.cur, fs)
			}
			return nil, false, errHelpShown
		}
		return nil, false, c.usage("%s", err.Error())
	}
	rest = fs.Args()
	used := len(args) - len(rest)
	return rest, used > 0 && args[used-1] == "--" && !consumedAsValue(fs, args, used-1), nil
}

// consumedAsValue: args[i] is the value of the flag just before it (as in
// --keep-env --), not a terminator.
func consumedAsValue(fs *flag.FlagSet, args []string, i int) bool {
	if i == 0 {
		return false
	}
	prev := args[i-1]
	if !strings.HasPrefix(prev, "-") || prev == "--" || strings.Contains(prev, "=") {
		return false
	}
	f := fs.Lookup(strings.TrimLeft(prev, "-"))
	if f == nil {
		return false
	}
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return false
	}
	return true
}

// isOutputFile: path is an existing file that isn't a program, so an
// argument naming it is saved test output to read, not a command to run. A
// bare word that names a program on PATH is that program, whatever files
// sit here (audit 1000.2).
func isOutputFile(path string) bool {
	if !strings.ContainsRune(path, '/') {
		if _, err := exec.LookPath(path); err == nil {
			return false
		}
	}
	st, err := os.Stat(path) // #nosec G703 -- only whether an argument the user typed is a saved-output file
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 == 0
}

// withheld says, after a failing run, which secret-looking variables it ran
// without (WP-1065).
func (c *cli) withheld(dropped []string) {
	if note := withheldNote(dropped); note != "" {
		c.println(note)
	}
}

// testsumReport prints the summary of one run's output, keeps the run for
// wp ship (rec, when there is one) and returns the parsed result (nil if
// none) and the exit to give.
func (c *cli) testsumReport(data []byte, format string, cmdFailed int, rec *testRunRecorder) (*testResult, error) {
	res, f, err := parseTestOutput(data, format)
	if err != nil {
		return nil, err
	}
	if res == nil {
		if cmdFailed != 0 {
			rec.failed(cmdFailed)
			return nil, c.emitUnparsed(data, cmdFailed)
		}
		if f == "" {
			return nil, fmt.Errorf("can't tell the test format; pass --format")
		}
		return nil, fmt.Errorf("no test results found in the %s output", f)
	}
	_, _ = fmt.Fprint(c.out, res.renderSaving(data))
	rec.finish(res, cmdFailed)
	if res.Failed > 0 || len(res.Failures) > 0 {
		if cmdFailed != 0 {
			// None of the output is shown, so all of it is kept.
			_, _ = fmt.Fprint(c.out, keptOutput(data, 0))
		}
		return res, exitCode(1)
	}
	if cmdFailed != 0 {
		_, _ = fmt.Fprintf(c.out, "but the command exited %d; last lines:\n%s", cmdFailed, failedTail(data, 20))
		return res, exitCode(1)
	}
	return res, nil
}

// lastLines is the last n lines of data.
func lastLines(data []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// failedOutputFile holds the whole output of the last failed command whose
// output was longer than the lines shown, in the checkout's git directory;
// each later long failure overwrites it. Outside a repo it is
// wp-failed-output-UID.log in the temp directory, overwritten the same way.
const failedOutputFile = "wp-failed-output.log"

// failedTail is the last n lines of a failed command's output, then a line
// saying where all of it is when it is longer. One n for both, so the lines
// shown and the threshold for keeping can't drift apart.
func failedTail(data []byte, n int) string {
	data = plainOutput(data)
	// Redacted whole, so a key whose BEGIN is above the tail is blanked
	// too; the kept file is redacted when it is written.
	return lastLines([]byte(redactKept(string(data))), n) + "\n" + keptPlain(data, n)
}

// goStreamEvent holds exactly the fields test2json and go build -json
// write, so a line with any other field is not taken for one of their events.
type goStreamEvent struct {
	Time, Action, Package, ImportPath, Test, Output, FailedBuild, Key, Value string
	Elapsed                                                                  float64
}

// plainOutput is the text of a go test -json stream: the Output of each
// event that has one, and any other line as it was (a build error printed
// outside the events, or a JSON line with fields no event has). Output that
// holds no event is returned unchanged.
func plainOutput(data []byte) []byte {
	var out []byte
	found := false
	for _, l := range bytes.SplitAfter(data, []byte("\n")) {
		var ev goStreamEvent
		dec := json.NewDecoder(bytes.NewReader(l))
		dec.DisallowUnknownFields()
		if bytes.HasPrefix(l, []byte("{")) && dec.Decode(&ev) == nil && !dec.More() && ev.Action != "" {
			found = true
			out = append(out, ev.Output...)
			continue
		}
		out = append(out, l...)
	}
	if !found {
		return data
	}
	return out
}

// keptOutput saves a failed command's whole output when it is longer than
// the shown lines, and returns a line saying where ("" when all of it was
// shown). A failed make check shows its last lines; the step's own output,
// with the finding that failed it, is above them, and the file is what
// gives it without another run (WP-1812).
func keptOutput(data []byte, shown int) string {
	return keptPlain(plainOutput(data), shown)
}

// keptPlain is keptOutput for data that is already plain text.
func keptPlain(data []byte, shown int) string {
	if len(bytes.Trim(data, "\n")) == 0 {
		return ""
	}
	if strings.Count(strings.TrimRight(string(data), "\n"), "\n")+1 <= shown {
		return ""
	}
	// The file keeps trailing blank lines, so the count is of the file's.
	n := bytes.Count(data, []byte("\n"))
	if !bytes.HasSuffix(data, []byte("\n")) {
		n++
	}
	// In a checkout the file goes when the checkout does: a runner removes
	// its job's, so the log of an ended job must not promise it (audit
	// 1816.2).
	until := "the next long failure here, or until this checkout is removed"
	p, err := localGit("rev-parse", "--git-path", failedOutputFile)
	if err != nil || p == "" {
		// Not in a repo: one temp file per user, which a later failure
		// overwrites, so they don't pile up.
		p = filepath.Join(os.TempDir(), fmt.Sprintf("wp-failed-output-%d.log", os.Getuid()))
		until = "the next long failure here"
	} else if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return keptAt(p, until, data, n)
}

var (
	pemBeginRE  = lazyre.New(`-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----`)
	pemEndRE    = lazyre.New(`-----END [A-Z ]*PRIVATE KEY-----`)
	pgpEndRE    = lazyre.New(`-----END [A-Z ]*PRIVATE KEY BLOCK-----`)
	pemBodyRE   = lazyre.New(`^[A-Za-z0-9+/=]+$`)
	pemHeaderRE = lazyre.New(`(?:^|\s)[-+>]?(?:Proc-Type|DEK-Info|Comment|Version|Charset|Hash|MessageID): `)
)

// pemFullLine is the least length taken for a whole line of a key's body
// (they are 64 or more); a shorter base64 line is a key's last line only
// after one of those, so "ok" or "12" after a bare header stays.
const pemFullLine = 40

// pemMaxLines is how far below a BEGIN line its END line is looked for (a
// 16384-bit RSA key is under 250 lines).
const pemMaxLines = 400

// pemKeyLike says a body line is mixed case with a digit, as base64 is; a
// commit SHA or a word is not.
func pemKeyLike(s string) bool {
	return strings.ContainsAny(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz") && strings.ContainsAny(s, "0123456789")
}

// pemBodyField is what is matched as a key's body on a line that may have a
// log prefix, quotes, or fields after the body (audit 1819.2): the last
// field long enough to be a full line of a key, else the last field.
func pemBodyField(line string) string {
	last, full := line, ""
	for _, f := range strings.Fields(line) {
		last = strings.TrimSuffix(strings.Trim(f, `"',;`), `\n`)
		if len(last) >= pemFullLine && pemBodyRE.MatchString(last) {
			full = last
		}
	}
	if full != "" {
		return full
	}
	return last
}

// pemUnmarked is a line without the blanks and the diff or quote marker
// before its text.
func pemUnmarked(line string) string {
	t := strings.Trim(strings.TrimSuffix(line, "\r"), " \t")
	if t != "" && strings.IndexByte("-+>", t[0]) >= 0 {
		t = strings.TrimLeft(t[1:], " \t")
	}
	return t
}

// pemShortLine says a line that follows a key's full line holds its short
// last one: the last field is base64 and not all letters, or any field is
// key-like and a whole number of base64 groups, 8 characters or more, for one
// with fields after it (audit 1860.1).
func pemShortLine(t string) bool {
	last := pemBodyField(t)
	if pemBodyRE.MatchString(last) && strings.Trim(last, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") != "" {
		return true
	}
	for _, f := range strings.Fields(t) {
		f = strings.Trim(f, `"',;`)
		if len(f) >= 8 && len(f)%4 == 0 && pemBodyRE.MatchString(f) && pemKeyLike(f) {
			return true
		}
	}
	return false
}

// redactKept is redact for a kept output file, returning as many lines as it
// got, each \r kept, so "All N lines" stays true. Every line between a
// BEGIN and an END PRIVATE KEY line becomes ***, when the END is within 400
// lines and before another BEGIN. Otherwise (a BEGIN with no END) only the
// lines that look like the key do, so the rest of the output stays: its
// headers, base64 lines of 40 or more characters (the first one mixed case
// with a digit, so a commit SHA is not one), then one shorter line that is
// not all letters. A diff or quote marker, a log prefix, quotes and a comma
// around a line are allowed, and fields after a full line's base64. Where
// some other line ends that run, the key may go on below it, so as far as an
// END is looked for, each mixed-case full line with a digit, the full lines
// right after it, and a short one after those, becomes *** too. Then redact's other patterns run over the
// whole text, a match that spanned lines giving them back as *** lines.
func redactKept(s string) string {
	lines := strings.Split(s, "\n")
	star := func(j int) {
		if strings.HasSuffix(lines[j], "\r") {
			lines[j] = "***\r"
		} else {
			lines[j] = "***"
		}
	}
	noEnd := map[*lazyre.Regexp]bool{} // no END line of this kind below this point
	for i := 0; i < len(lines); i++ {
		l, _ := strings.CutSuffix(lines[i], "\r")
		loc := pemBeginRE.FindStringIndex(l)
		if loc == nil {
			continue
		}
		// A key ends at an END line of its own kind, as in pemKeyRE.
		endRE := pemEndRE
		if strings.HasSuffix(l[loc[0]:loc[1]], " BLOCK-----") {
			endRE = pgpEndRE
		}
		if endRE.MatchString(l[loc[1]:]) {
			lines[i] = pemKeyRE.ReplaceAllString(l, pemKeyRepl) + lines[i][len(l):]
			continue
		}
		end := -1
		if !noEnd[endRE] {
			j := i + 1
			for ; j < len(lines) && j <= i+pemMaxLines; j++ {
				if pemBeginRE.MatchString(lines[j]) {
					break
				}
				if endRE.MatchString(lines[j]) {
					end = j
					break
				}
			}
			noEnd[endRE] = end < 0 && j >= len(lines)
		}
		// Anything after BEGIN on its own line is the key (one printed
		// with escaped newlines, say).
		if strings.TrimSpace(l[loc[1]:]) != "" {
			lines[i] = l[:loc[1]] + "***" + lines[i][len(l):]
		}
		if end >= 0 {
			for j := i + 1; j < end; j++ {
				star(j)
			}
			// So is what comes before END on its line, unless it is only
			// a diff or quote prefix.
			if at := endRE.FindStringIndex(lines[end]); strings.Trim(lines[end][:at[0]], " \t-+>") != "" {
				lines[end] = "***" + lines[end][at[0]:]
			}
			i = end
			continue
		}
		inHeaders, sawHeader, sawFull := true, false, false
		j := i + 1
	body:
		for ; j < len(lines); j++ {
			t := pemUnmarked(lines[j])
			last := pemBodyField(t)
			switch {
			case inHeaders && pemHeaderRE.MatchString(t):
				sawHeader = true
			case t == "" && !sawFull:
				// Nothing to blank; the body may follow.
				if sawHeader {
					inHeaders = false
				}
				continue
			case pemBodyRE.MatchString(last) && len(last) >= pemFullLine && (sawFull || pemKeyLike(last)):
				inHeaders, sawFull = false, true
			case sawFull && pemShortLine(t):
				star(j)
				j++
				break body
			default:
				break body
			}
			star(j)
		}
		// A header not listed, or another process's line, doesn't end the
		// key (audit 1860.1). The first full line after it must look like a
		// key, so a commit SHA stays; the full lines right after that one need
		// not, as in the body above (audit 2091.1).
		afterFull := false
		for k := j; k < len(lines) && k <= i+pemMaxLines && !pemBeginRE.MatchString(lines[k]); k++ {
			t := pemUnmarked(lines[k])
			last := pemBodyField(t)
			full := len(last) >= pemFullLine && pemBodyRE.MatchString(last) && (afterFull || pemKeyLike(last))
			if full || afterFull && pemShortLine(t) {
				star(k)
			}
			afterFull = full
		}
		i = j - 1
	}
	s = strings.Join(lines, "\n")
	for _, st := range redactSteps {
		s = replaceStep(s, st, func(m, r string) string {
			if v, ok := strings.CutSuffix(m, "-----BEGIN"); ok && v != "" {
				if strings.TrimSuffix(r, "***")+"-----BEGIN" == m {
					// "api_key:" and a key's BEGIN line after it: the
					// value is the key, blanked above, and the line stays
					// readable.
					return m
				}
				// A value glued to a BEGIN line: blank it, keep the marker.
				return keepLines(m, r) + "-----BEGIN"
			}
			return keepLines(m, r)
		})
	}
	return s
}

// keepLines pads r, a replacement of m, with *** lines for the lines of m it
// lost, keeping the \r that ended each line of m.
func keepLines(m, r string) string {
	d := strings.Count(m, "\n") - strings.Count(r, "\n")
	if d <= 0 {
		return r
	}
	cr := func(l string) string {
		if strings.HasSuffix(l, "\r") {
			return "\r"
		}
		return ""
	}
	ml := strings.Split(m, "\n")
	pieces := strings.Split(r, "\n")
	var b strings.Builder
	for j := range len(ml) - 1 {
		if j < len(pieces)-1 {
			b.WriteString(strings.TrimSuffix(pieces[j], "\r"))
		} else if j == len(pieces)-1 {
			b.WriteString(pieces[j])
		} else {
			b.WriteString("***")
		}
		b.WriteString(cr(ml[j]) + "\n")
	}
	// d > 0 means ml has at least as many lines as pieces.
	b.WriteString("***")
	return b.String()
}

// keptAt writes data, n lines, to p and says where and until when it is
// kept, or why it couldn't be.
// Credentials are blanked first, since lines above the tail were never
// shown and the file outlives the run; the write is a rename, so two
// failures at once leave one run's output, not a mix (audit 1779).
func keptAt(p, until string, data []byte, n int) string {
	if err := writeFileAtomic(p, []byte(redactKept(string(data))), 0o600); err != nil {
		return fmt.Sprintf("Its %d lines of output couldn't be saved: %v\n", n, err)
	}
	return fmt.Sprintf("All %d lines of its output (kept until %s): %s\n", n, until, p)
}

// emitUnparsed reports a command that failed without producing results.
func (c *cli) emitUnparsed(data []byte, code int) error {
	_, _ = fmt.Fprintf(c.out, "FAIL: the command exited %d and no test results were recognized; last lines:\n%s", code, failedTail(data, 20))
	return exitCode(1)
}

var (
	reGoText   = lazyre.New(`(?m)^(=== RUN|--- (PASS|FAIL|SKIP)|ok  \t|FAIL\t|\?   \t)`)
	reUnittest = lazyre.New(`(?m)^(Ran \d+ tests? in [\d.]+s$|(FAIL|ERROR): \S+ \([\w.]+\)$)`)
	rePyTotal  = lazyre.New(`(?m)(\d+ (passed|failed|error|skipped)[^\n]* in [\d.]+s|test session starts|^(PASSED|FAILED|ERROR) \S+::)`)
)

func detectTestFormat(data []byte) string {
	t := bytes.TrimSpace(data)
	switch {
	case bytes.HasPrefix(t, []byte("<")):
		return "junit"
	case bytes.Contains(t, []byte(`"Action"`)) && bytes.HasPrefix(t, []byte("{")):
		return "go"
	case rePyTotal.Match(t):
		return "pytest"
	case reUnittest.Match(t):
		return "unittest"
	case reGoText.Match(t):
		return "go"
	}
	return ""
}

// parseTestOutput reads data as format (auto: detected). res is nil when
// the format can't be told (f "") or nothing in data is a test result.
func parseTestOutput(data []byte, format string) (res *testResult, f string, err error) {
	f = format
	if f == "auto" {
		f = detectTestFormat(data)
	}
	switch f {
	case "go":
		res = parseGoTest(data)
	case "pytest":
		res = parsePytest(data)
	case "unittest":
		res = parseUnittest(data)
	case "junit":
		if res, err = parseJUnit(data); err != nil {
			return nil, f, err
		}
	default:
		return nil, "", nil
	}
	if res.Passed+res.Failed+res.Skipped == 0 && len(res.Failures) == 0 && res.Units == 0 {
		return nil, f, nil
	}
	return res, f, nil
}

func (r *testResult) render() string { return r.renderCut(nil) }

// renderCut is render; when it cuts lines, the cut line is followed by what
// full returns (a line saying where the whole output is), if that is not "".
func (r *testResult) renderCut(full func() string) string {
	var out []string
	total := r.Passed + r.Failed + r.Skipped
	units := ""
	if r.Units > 0 {
		units = fmt.Sprintf(" (%d %s)", r.Units, r.UnitName)
	}
	secs := strconv.FormatFloat(r.Seconds, 'f', 1, 64) + "s"
	if r.Failed == 0 && len(r.Failures) == 0 {
		switch {
		case r.NoneRan:
			return fmt.Sprintf("NO TESTS RAN in %s%s: if you gave -run, it matched nothing\n", secs, units)
		case total == 0 && r.Units > 0:
			return fmt.Sprintf("PASS: %d %s in %s\n", r.Units, r.UnitName, secs)
		}
		return fmt.Sprintf("PASS: %d tests in %s%s\n", total, secs, units)
	}
	out = append(out, fmt.Sprintf("FAIL: %d failed, %d passed, %d skipped in %s%s", max(r.Failed, len(r.Failures)), r.Passed, r.Skipped, secs, units))
	for _, f := range r.Failures {
		out = append(out, "--- "+f.Name+f.runsText())
		if f.Loc != "" {
			out = append(out, "  at "+f.Loc)
		}
		for _, m := range f.Msg {
			out = append(out, "  "+m)
		}
	}
	if len(out) > testsumMaxLines {
		note := ""
		if full != nil {
			note = full()
		}
		keep := testsumMaxLines - 1
		if note != "" {
			keep--
		}
		out = append(out[:keep], fmt.Sprintf("... %d more lines", len(out)-keep))
		if note != "" {
			out = append(out, note)
		}
	}
	return strings.Join(out, "\n") + "\n"
}

var reGoLoc = lazyre.New(`(?:^|\s)((?:[\w./-]+/)?[\w.-]+\.go):(\d+)`)

type goEvent struct {
	Action, Package, Test, Output string
	ImportPath, FailedBuild       string
	Elapsed                       float64
}

// goTestJSON adds -json to a plain go test, so its tests are counted and a
// -run that matches nothing shows (WP-592). -c and -list run no tests.
func goTestJSON(argv []string) []string {
	if len(argv) < 2 || argv[0] != "go" || argv[1] != "test" {
		return argv
	}
	for _, a := range argv[2:] {
		if a == "-args" {
			break
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && (name == "json" || name == "c" || name == "list") {
			return argv
		}
	}
	return append([]string{"go", "test", "-json"}, argv[2:]...)
}

// goMessage keeps the lines of a test's output that say what went wrong.
func goMessage(lines []string) (loc string, msg []string) {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "=== ") || strings.HasPrefix(t, "--- ") || t == "FAIL" || t == "PASS" || strings.HasPrefix(t, "FAIL\t") || strings.HasPrefix(t, "ok  \t") {
			continue
		}
		if loc == "" {
			if m := reGoLoc.FindStringSubmatch(l); m != nil {
				loc = m[1] + ":" + m[2]
			}
		}
		if len(msg) < testsumMsgLines {
			msg = append(msg, t)
		}
	}
	return loc, msg
}

func parseGoTest(data []byte) *testResult {
	res := &testResult{UnitName: "packages"}
	if res2, ok := parseGoJSON(data); ok {
		return res2
	}
	return parseGoText(data, res)
}

func parseGoJSON(data []byte) (*testResult, bool) {
	res := &testResult{UnitName: "packages", Ran: map[string]bool{}, Skips: map[string]bool{}}
	out := map[string][]string{}
	firstFail := map[string][]string{}
	runs, fails := map[string]int{}, map[string]int{}
	running := map[string]bool{}
	build := map[string][]string{}
	pkgs := map[string]bool{}
	timeouts := map[string][]string{}
	var failed []string
	failedSeen := map[string]bool{}
	seen := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e goEvent
		if json.Unmarshal(line, &e) != nil || e.Action == "" {
			continue
		}
		seen++
		key := e.Package + "\x00" + e.Test
		switch e.Action {
		case "build-output":
			build[e.ImportPath] = append(build[e.ImportPath], strings.TrimRight(e.Output, "\n"))
		case "run":
			out[key] = nil
			running[key] = true
		case "output":
			l := strings.TrimRight(e.Output, "\n")
			out[key] = append(out[key], l)
			if timeouts[e.Package] != nil || reGoTimeout.MatchString(l) {
				timeouts[e.Package] = append(timeouts[e.Package], l)
			}
		case "pass", "fail", "skip":
			delete(running, key)
			if e.Test == "" {
				if e.Action != "skip" {
					pkgs[e.Package] = true
				}
				res.Seconds += e.Elapsed
				cut := cutShort(e.Package, running, out)
				if e.Action == "fail" && (timeouts[e.Package] != nil || !pkgHasFailedTest(failed, e.Package) || timedOut(cut)) {
					failed = append(failed, key)
					if lines := build[e.FailedBuild]; e.FailedBuild != "" && len(lines) > 0 {
						out[key] = lines
					} else if len(cut) > 0 {
						out[key] = cut
					}
				}
				continue
			}
			if e.Action == "skip" {
				res.Skips[e.Package+"."+e.Test] = true
			} else {
				runs[key]++
				res.Ran[e.Package+"."+e.Test] = true
			}
			switch e.Action {
			case "pass":
				res.Passed++
			case "skip":
				res.Skipped++
			case "fail":
				res.Failed++
				fails[key]++
				if !failedSeen[key] {
					failedSeen[key] = true
					failed = append(failed, key)
					firstFail[key] = out[key]
				}
			}
		}
	}
	if seen == 0 {
		return nil, false
	}
	res.Units = len(pkgs)
	res.Names = sortedKeys(pkgs)
	res.NoneRan = res.Passed+res.Failed+res.Skipped == 0 && len(failed) == 0 && res.Units > 0
	for _, key := range failed {
		pkg, test, _ := strings.Cut(key, "\x00")
		lines := out[key]
		if l, ok := firstFail[key]; ok {
			lines = l
		}
		if f, ok := goTimeoutFailure(pkg, timeouts[pkg]); ok && test == "" {
			f.Loc = cutShortLoc(pkg, running, out, "")
			res.Failures = append(res.Failures, f)
			continue
		}
		name := pkg + "." + test
		if test == "" {
			name = pkg + " (package)"
		}
		loc, msg := goMessage(lines)
		f := testFailure{Name: name, Loc: loc, Msg: msg, Pkg: pkg, Test: test}
		if test == "" {
			f.Loc = cutShortLoc(pkg, running, out, loc)
		} else if runs[key] > 1 {
			f.Runs, f.Fails = runs[key], fails[key]
		}
		res.Failures = append(res.Failures, f)
	}
	return res, true
}

// cutShort is what the tests of pkg that started and never ended printed: a
// package that fails with no failing test of its own was stopped mid-test,
// by -timeout's panic (which names the running tests) or a crash. Only the
// lines before the goroutine dump are kept; the dump says little.
func cutShort(pkg string, running map[string]bool, out map[string][]string) []string {
	var keys []string
	for k := range running {
		if p, t, _ := strings.Cut(k, "\x00"); p == pkg && t != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var lines []string
	for _, k := range keys {
		for _, l := range out[k] {
			if strings.HasPrefix(l, "goroutine ") {
				break
			}
			lines = append(lines, l)
		}
	}
	return lines
}

// cutShortLoc places a timeout or crash at the cut-short test's own frame in
// the goroutine dump (the panic's first file:line is the testing package's
// alarm), else keeps loc.
func cutShortLoc(pkg string, running map[string]bool, out map[string][]string, loc string) string {
	for _, k := range sortedKeys(running) {
		p, t, _ := strings.Cut(k, "\x00")
		if p != pkg || t == "" {
			continue
		}
		fn := "." + strings.SplitN(t, "/", 2)[0] + "("
		lines := out[k]
		for i, l := range lines {
			if strings.Contains(l, fn) && i+1 < len(lines) {
				if m := reGoLoc.FindStringSubmatch(lines[i+1]); m != nil {
					return filepath.Base(m[1]) + ":" + m[2]
				}
			}
		}
	}
	return loc
}

// timedOut: the lines a package's cut-short tests printed hold -timeout's
// panic.
func timedOut(cut []string) bool {
	return slices.ContainsFunc(cut, func(l string) bool {
		return strings.HasPrefix(strings.TrimSpace(l), "panic: test timed out")
	})
}

var (
	reGoTimeout = lazyre.New(`^panic: test timed out after (\S+)$`)
	reGoRunning = lazyre.New(`^\s+(\S+) \((\S+)\)$`)
)

// goTimeoutFailure names go test's time limit as what stopped pkg (WP-883),
// from its output starting at the timeout panic: a package that ran past
// -timeout fails with no test named, which reads as a failing test to find.
// A test that ran for most of the limit may hang; otherwise the package as
// a whole was slow, which a busy machine does to a long suite.
func goTimeoutFailure(pkg string, lines []string) (testFailure, bool) {
	if len(lines) == 0 {
		return testFailure{}, false
	}
	m := reGoTimeout.FindStringSubmatch(strings.TrimSpace(lines[0]))
	if m == nil {
		return testFailure{}, false
	}
	limit, _ := time.ParseDuration(m[1])
	var running []string
	var longest time.Duration
	longName := ""
	for i, l := range lines[1:] {
		if i == 0 && strings.TrimSpace(l) == "running tests:" {
			continue
		}
		r := reGoRunning.FindStringSubmatch(l)
		if r == nil || strings.TrimSpace(l) == "" {
			break
		}
		running = append(running, r[1]+" ("+r[2]+")")
		if d, err := time.ParseDuration(r[2]); err == nil && d > longest {
			longest, longName = d, r[1]
		}
	}
	msg := []string{"go test stopped the package at its time limit (-timeout " + m[1] + ")"}
	if len(running) > 0 {
		shown := running[:min(len(running), 5)]
		s := "still running: " + strings.Join(shown, ", ")
		if n := len(running) - len(shown); n > 0 {
			s += fmt.Sprintf(" and %d more", n)
		}
		msg = append(msg, s)
	}
	if limit > 0 && longest >= limit/2 {
		msg = append(msg, longName+" ran for "+longest.String()+" of it: it may hang")
	} else {
		hint := "no test ran long, so the package as a whole was slow (a busy machine?): run it again with a longer -timeout"
		if limit > 0 {
			hint += ", e.g. -timeout " + (2 * limit).String()
		}
		msg = append(msg, hint)
	}
	return testFailure{Name: pkg + " (timed out after " + m[1] + ")", Msg: msg, Pkg: pkg}, true
}

func pkgHasFailedTest(failed []string, pkg string) bool {
	for _, k := range failed {
		if p, t, _ := strings.Cut(k, "\x00"); p == pkg && t != "" {
			return true
		}
	}
	return false
}

var (
	reGoTestLine = lazyre.New(`^(\s*)--- (PASS|FAIL|SKIP): (\S+) \(([\d.]+)s\)`)
	reGoPkgOK    = lazyre.New(`^ok\s+(\S+)\s+(?:([\d.]+)s|\(cached\))`)
	reGoPkgFail  = lazyre.New(`^FAIL\s+(\S+)\s+(?:([\d.]+)s|\[(.*)\])`)
)

func parseGoText(data []byte, res *testResult) *testResult {
	lines := strings.Split(string(data), "\n")
	pkgs := map[string]bool{}
	var cur *testFailure
	var curLines []string
	var curIndent int
	matchedNone := 0
	var timeout []string
	flush := func() {
		if cur != nil {
			cur.Loc, cur.Msg = goMessage(curLines)
			res.Failures = append(res.Failures, *cur)
			cur = nil
		}
	}
	for _, l := range lines {
		if timeout != nil || reGoTimeout.MatchString(l) {
			timeout = append(timeout, l)
		}
		if m := reGoTestLine.FindStringSubmatch(l); m != nil {
			flush()
			switch m[2] {
			case "PASS":
				res.Passed++
			case "SKIP":
				res.Skipped++
			case "FAIL":
				res.Failed++
				cur, curLines, curIndent = &testFailure{Name: m[3]}, nil, len(m[1])
			}
			continue
		}
		if m := reGoPkgOK.FindStringSubmatch(l); m != nil {
			flush()
			pkgs[m[1]] = true
			s, _ := strconv.ParseFloat(m[2], 64)
			res.Seconds += s
			if strings.Contains(l, "[no tests to run]") {
				matchedNone++
			}
			continue
		}
		if m := reGoPkgFail.FindStringSubmatch(l); m != nil {
			flush()
			pkgs[m[1]] = true
			s, _ := strconv.ParseFloat(m[2], 64)
			res.Seconds += s
			if f, ok := goTimeoutFailure(m[1], timeout); ok {
				res.Failures = append(res.Failures, f)
			} else if res.Failed == 0 && len(res.Failures) == 0 {
				res.Failures = append(res.Failures, testFailure{Name: m[1] + " (package)", Msg: []string{strings.TrimSpace(l)}, Pkg: m[1]})
			}
			timeout = nil
			continue
		}
		if cur != nil && (strings.HasPrefix(l, strings.Repeat(" ", curIndent+1)) || strings.HasPrefix(l, "\t")) {
			curLines = append(curLines, l)
		} else if cur != nil {
			flush()
		}
	}
	flush()
	res.Units = len(pkgs)
	res.Names = sortedKeys(pkgs)
	res.NoneRan = res.Units > 0 && matchedNone == res.Units && res.Passed+res.Failed+res.Skipped == 0 && len(res.Failures) == 0
	return res
}

var (
	rePySummary = lazyre.New(`(\d+) (passed|failed|errors?|skipped|xfailed|xpassed|deselected|warnings?)`)
	rePyTime    = lazyre.New(`in ([\d.]+)s`)
	rePyFailed  = lazyre.New(`^(FAILED|ERROR) (\S+?)(?: - (.*))?$`)
	rePySection = lazyre.New(`^_{3,} (.+?) _{3,}$`)
	rePyLoc     = lazyre.New(`^([\w./-]+\.py):(\d+):`)
	rePyFile    = lazyre.New(`^([\w./-]+\.py) [.FsxXEr]+`)
	rePyNodeID  = lazyre.New(`^(?:(?:PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS) )?([\w./-]+\.py)::\S+(?: (?:PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS))?`)
)

func parsePytest(data []byte) *testResult {
	res := &testResult{UnitName: "files"}
	files := map[string]bool{}
	lines := strings.Split(string(data), "\n")
	type section struct {
		loc     string
		e, body []string
	}
	sections := map[string]*section{}
	var cur *section
	inReport := false
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if m := rePySection.FindStringSubmatch(l); m != nil && !strings.HasPrefix(l, "=") {
			cur = &section{}
			sections[m[1]] = cur
			inReport = true
			continue
		}
		if strings.HasPrefix(l, "=") {
			cur = nil
			inReport = strings.Contains(l, "FAILURES") || strings.Contains(l, "ERRORS")
			if strings.Contains(l, " in ") && rePySummary.MatchString(l) {
				parsePyTotals(l, res)
			}
			continue
		}
		if m := rePyFile.FindStringSubmatch(l); m != nil {
			files[m[1]] = true
		}
		if m := rePyNodeID.FindStringSubmatch(l); m != nil {
			files[m[1]] = true
		}
		if cur != nil && inReport {
			if strings.HasPrefix(l, "E ") {
				cur.e = append(cur.e, strings.TrimSpace(strings.TrimPrefix(l, "E")))
			} else if m := rePyLoc.FindStringSubmatch(l); m != nil {
				cur.loc = m[1] + ":" + m[2]
			}
			cur.body = append(cur.body, l)
		}
		if res.Passed+res.Failed == 0 && !strings.HasPrefix(l, "FAILED") && strings.Contains(l, " in ") && rePyTime.MatchString(l) && rePySummary.MatchString(l) && !strings.Contains(l, "::") {
			parsePyTotals(l, res)
		}
	}
	res.Units = len(files)
	seen := map[string]bool{}
	for _, l := range lines {
		m := rePyFailed.FindStringSubmatch(strings.TrimRight(l, "\r"))
		if m == nil || seen[m[2]] {
			continue
		}
		seen[m[2]] = true
		f := testFailure{Name: m[2]}
		short := m[2][strings.LastIndex(m[2], "::")+2:]
		s := sections[short]
		if s == nil {
			for k, v := range sections {
				if strings.HasSuffix(m[2], k) || strings.HasSuffix(m[2], "::"+k) {
					s = v
				}
			}
		}
		if s != nil {
			f.Loc = s.loc
			f.Msg = s.e
		}
		if len(f.Msg) == 0 && m[3] != "" {
			f.Msg = []string{m[3]}
		}
		if f.Loc == "" {
			f.Loc = m[2][:strings.Index(m[2]+"::", "::")]
		}
		if len(f.Msg) > testsumMsgLines {
			f.Msg = f.Msg[:testsumMsgLines]
		}
		res.Failures = append(res.Failures, f)
	}
	if res.Failed < len(res.Failures) {
		res.Failed = len(res.Failures)
	}
	return res
}

func parsePyTotals(l string, res *testResult) {
	for _, m := range rePySummary.FindAllStringSubmatch(l, -1) {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "passed":
			res.Passed = n
		case "failed", "error", "errors":
			res.Failed += n
		case "skipped":
			res.Skipped = n
		}
	}
	if m := rePyTime.FindStringSubmatch(l); m != nil {
		res.Seconds, _ = strconv.ParseFloat(m[1], 64)
	}
}

var (
	reUtRan    = lazyre.New(`^Ran (\d+) tests? in ([\d.]+)s$`)
	reUtStatus = lazyre.New(`^(OK|FAILED)(?: \((.*)\))?$`)
	reUtHead   = lazyre.New(`^(FAIL|ERROR|UNEXPECTED SUCCESS): (\S+) \(([\w.]+)\)`)
	reUtFrame  = lazyre.New(`^\s*File "([^"]+)", line (\d+)`)
)

// parseUnittest reads python -m unittest output. Several runs one after the
// other (two discover calls in one command) add up.
func parseUnittest(data []byte) *testResult {
	res := &testResult{}
	lines := strings.Split(string(data), "\n")
	wd, _ := os.Getwd()
	ran := 0
	for i := 0; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], "\r")
		if m := reUtRan.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			secs, _ := strconv.ParseFloat(m[2], 64)
			ran += n
			res.Seconds += secs
			continue
		}
		if m := reUtStatus.FindStringSubmatch(l); m != nil {
			for _, part := range strings.Split(m[2], ",") {
				k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
				if !ok {
					continue
				}
				n, _ := strconv.Atoi(v)
				switch k {
				case "failures", "errors", "unexpected successes":
					res.Failed += n
				case "skipped":
					res.Skipped += n
				}
			}
			continue
		}
		m := reUtHead.FindStringSubmatch(l)
		if m == nil || i == 0 || !strings.HasPrefix(strings.TrimSpace(lines[i-1]), "=====") {
			continue
		}
		f := testFailure{Name: m[3]}
		if !strings.HasSuffix(m[3], "."+m[2]) {
			f.Name = m[3] + "." + m[2]
		}
		if strings.HasPrefix(m[3], "unittest.loader._FailedTest") {
			f.Name = m[2] + " (didn't load)"
		}
		start := i + 1
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			if strings.HasPrefix(lines[j], "-----") {
				start = j + 1
				break
			}
		}
		var body []string
		for j := start; j < len(lines); j++ {
			b := strings.TrimRight(lines[j], "\r")
			if strings.HasPrefix(b, "=====") || strings.HasPrefix(b, "-----") {
				break
			}
			body = append(body, b)
		}
		last := -1
		for j, b := range body {
			if fm := reUtFrame.FindStringSubmatch(b); fm != nil {
				last = j
				p := fm[1]
				if wd != "" {
					if rel, ok := strings.CutPrefix(p, wd+string(os.PathSeparator)); ok {
						p = rel
					}
				}
				f.Loc = p + ":" + fm[2]
			}
		}
		rest := body
		if last >= 0 {
			rest = body[last+1:]
			if len(rest) > 0 && strings.HasPrefix(rest[0], "    ") {
				rest = rest[1:]
			}
		}
		for _, b := range rest {
			b = strings.TrimSpace(b)
			if b == "" || strings.HasPrefix(b, "^") || strings.HasPrefix(b, "~") || b == "?" || strings.HasPrefix(b, "? ") {
				continue
			}
			if len(f.Msg) == testsumMsgLines {
				break
			}
			f.Msg = append(f.Msg, b)
		}
		res.Failures = append(res.Failures, f)
	}
	if res.Failed < len(res.Failures) {
		res.Failed = len(res.Failures)
	}
	res.Passed = max(0, ran-res.Failed-res.Skipped)
	return res
}

type junitSuite struct {
	Name   string       `xml:"name,attr"`
	Cases  []junitCase  `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"`
}

type junitCase struct {
	Name      string      `xml:"name,attr"`
	Class     string      `xml:"classname,attr"`
	File      string      `xml:"file,attr"`
	Line      string      `xml:"line,attr"`
	Time      float64     `xml:"time,attr"`
	Failure   *junitProbl `xml:"failure"`
	Error     *junitProbl `xml:"error"`
	Skipped   *junitProbl `xml:"skipped"`
	SuiteName string      `xml:"-"`
}

type junitProbl struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

var reAnyLoc = lazyre.New(`([\w./-]+\.(?:py|go|java|js|ts|rb|rs|php)):(\d+)`)

func parseJUnit(data []byte) (*testResult, error) {
	var root struct {
		XMLName xml.Name
		junitSuite
	}
	if err := xml.Unmarshal(data, &root); err != nil { // #nosec G709 -- test output the user ran or named, parsed into plain structs
		return nil, fmt.Errorf("junit xml: %w", err)
	}
	res := &testResult{UnitName: "suites"}
	suites := map[string]bool{}
	var walk func(s junitSuite)
	walk = func(s junitSuite) {
		for _, tc := range s.Cases {
			suites[s.Name] = true
			res.Seconds += tc.Time
			prob := tc.Failure
			if prob == nil {
				prob = tc.Error
			}
			switch {
			case prob != nil:
				res.Failed++
				name := tc.Name
				if tc.Class != "" {
					name = tc.Class + "." + tc.Name
				}
				loc := ""
				if tc.File != "" {
					loc = tc.File
					if tc.Line != "" {
						loc += ":" + tc.Line
					}
				}
				var msg []string
				for _, l := range strings.Split(strings.TrimSpace(prob.Message+"\n"+prob.Text), "\n") {
					if l = strings.TrimSpace(l); l != "" && (len(msg) == 0 || msg[len(msg)-1] != l) {
						msg = append(msg, l)
					}
				}
				if loc == "" {
					if m := reAnyLoc.FindStringSubmatch(prob.Text + " " + prob.Message); m != nil {
						loc = m[1] + ":" + m[2]
					}
				}
				if len(msg) > testsumMsgLines {
					msg = msg[:testsumMsgLines]
				}
				res.Failures = append(res.Failures, testFailure{Name: name, Loc: loc, Msg: msg})
			case tc.Skipped != nil:
				res.Skipped++
			default:
				res.Passed++
			}
		}
		for _, sub := range s.Suites {
			walk(sub)
		}
	}
	walk(root.junitSuite)
	res.Units = len(suites)
	return res, nil
}
