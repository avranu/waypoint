package core

import (
	"fmt"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	excerptMaxLines = 200
	excerptLineMax  = 400
	excerptTailLen  = 30
	blockMax        = 45
)

var (
	ansiRE    = lazyre.New(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][0-9A-Za-z]`)
	logStamp  = lazyre.New(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:?\d\d)\s?`)
	glSection = lazyre.New(`^section_(?:start|end):\d+:\S*`)
)

// cleanLog splits a job log into lines without colour codes, the forge's
// timestamps (Forgejo and GitHub put one before every line), GitLab's
// section markers, or the earlier states of a line a progress bar rewrote.
func cleanLog(log string) []string {
	log = strings.TrimSuffix(strings.ReplaceAll(log, "\r\n", "\n"), "\n")
	if log == "" {
		return nil
	}
	raw := strings.Split(log, "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimRight(l, "\r")
		if i := strings.LastIndexByte(l, '\r'); i >= 0 {
			l = l[i+1:]
		}
		l = ansiRE.ReplaceAllString(l, "")
		l = logStamp.ReplaceAllString(l, "")
		l = glSection.ReplaceAllString(l, "")
		out = append(out, strings.TrimRight(l, " \t"))
	}
	return out
}

// logRule recognises one kind of failure: the line it starts on, and how
// much around it belongs to it.
type logRule struct {
	re            *lazyre.Regexp
	before, after int
	back          func(lines []string, i int) int
	block         func(lines []string, i int) int
	// A closing line alone only says the job ended badly; the lines before it are shown instead.
	closing bool
}

func indented(l string) bool { return l != "" && (l[0] == ' ' || l[0] == '\t') }

// untilRE ends a block at the first line after the hit that matches re,
// including that line when inclusive.
func untilRE(re *lazyre.Regexp, inclusive bool) func([]string, int) int {
	return func(lines []string, i int) int {
		for j := i + 1; j < len(lines) && j <= i+blockMax; j++ {
			if re.MatchString(lines[j]) {
				if inclusive {
					return j + 1
				}
				return j
			}
		}
		return min(len(lines), i+1+blockMax)
	}
}

// whileIndented takes the indented lines after the hit (a Go test's
// messages), and with tail also the first line that isn't (the exception
// a Python traceback ends with).
func whileIndented(tail bool) func([]string, int) int {
	return func(lines []string, i int) int {
		j := i + 1
		for j < len(lines) && j <= i+blockMax && indented(lines[j]) {
			j++
		}
		if tail && j < len(lines) && j <= i+blockMax && lines[j] != "" {
			j++
		}
		return j
	}
}

// goRunBefore takes the indented lines before a Go test's "--- FAIL" when
// `go test -v` printed its messages there, under "=== RUN".
func goRunBefore(lines []string, i int) int {
	j := i
	for j > 0 && i-j < blockMax && indented(lines[j-1]) {
		j--
	}
	if j < i && j > 0 && goRunRE.MatchString(lines[j-1]) {
		return j - 1
	}
	return i
}

// headerBefore takes the lines back to the first one that isn't indented
// (ESLint names the file once, above its problems).
func headerBefore(lines []string, i int) int {
	for j := i - 1; j >= 0 && i-j <= blockMax; j-- {
		if lines[j] == "" {
			return i
		}
		if !indented(lines[j]) {
			return j
		}
	}
	return i
}

var (
	goRunRE       = lazyre.New(`^=== (?:RUN|CONT|NAME) `)
	raceEnd       = lazyre.New(`^={10,}$`)
	panicEnd      = lazyre.New(`^(?:FAIL\b|exit status \d|ok\s)`)
	playwrightHd  = lazyre.New(`^\s+\d+\) \[[^\]]+\] › `)
	playwrightEnd = lazyre.New(`^\s+\d+\) \[[^\]]+\] › |^\s+\d+ (?:failed|passed|flaky|skipped)\b`)
)

// logRules, most specific first: a line counts for the first that matches.
var logRules = []logRule{
	{re: lazyre.New(`^\s*--- FAIL: `), block: whileIndented(false), back: goRunBefore},
	{re: lazyre.New(`^FAIL\t`)},
	{re: lazyre.New(`^(?:panic: |fatal error: )`), block: untilRE(panicEnd, true)},
	{re: lazyre.New(`^WARNING: DATA RACE$`), before: 1, block: untilRE(raceEnd, true)},
	{re: lazyre.New(`^\s*Traceback \(most recent call last\):$`), block: whileIndented(true)},
	{re: lazyre.New(`^E {2,}\S`), before: 3},
	{re: lazyre.New(`^(?:FAILED|ERROR) \S+(?:::| - )`)},
	{re: lazyre.New(`^=+ .*\b\d+ (?:failed|errors?)\b.* =+$`)},
	{re: lazyre.New(`^\S+\.py:\d+: \w+`)},
	{re: playwrightHd, block: untilRE(playwrightEnd, false)},
	{re: lazyre.New(`^\s+✘ `)},
	{re: lazyre.New(`^\s+\d+ failed$`), after: 6},
	{re: lazyre.New(`\berror TS\d+:`), after: 2},
	{re: lazyre.New(`^\s+\d+:\d+\s+error\s`), back: headerBefore},
	{re: lazyre.New(`^✖ \d+ problems? `)},
	{re: lazyre.New(`^\S+\.go:\d+(?::\d+)?: `)},
	{re: lazyre.New(`^\S+:\d+(?::\d+)?: (?:fatal )?error: `), after: 2},
	{re: lazyre.New(`^thread '.*' panicked at`), after: 3},
	{re: lazyre.New(`^test result: FAILED`)},
	{re: lazyre.New(`^\s*● `), after: 12},
	{re: lazyre.New(`^(?:FAIL|ERROR): \w+ \(`), before: 1},
	{re: lazyre.New(`^FAIL[ :]\S`)},
	{re: lazyre.New(`##\[error\]Process completed with exit code`), closing: true},
	{re: lazyre.New(`^ERROR: Job failed\b`), closing: true},
	{re: lazyre.New(`^⚙️ \[runner\]: exitcode '\d+'`), closing: true},
	{re: lazyre.New(`^❌\s+Failure - `), closing: true},
	{re: lazyre.New(`🏁\s+Job failed`), closing: true},
	{re: lazyre.New(`^make(?:\[\d+\])?: \*\*\* `), before: 2},
	{re: lazyre.New(`^npm ERR! `)},
	{re: lazyre.New(`^\s*(?:Error|ERROR|error)(?:\[[^\]]*\])?: `), before: 1, after: 2},
	{re: lazyre.New(`##\[error\]`)},
}

// LogExcerpt is the failing part of a job's log.
type LogExcerpt struct {
	Text  string `json:"excerpt"`
	Lines int    `json:"lines"`         // lines in the whole log
	Shown int    `json:"shown"`         // lines of the log in the excerpt
	Cut   bool   `json:"cut,omitempty"` // more failures than fit
	// Found is false when no failure was recognised: the excerpt is then
	// the lines before the job ended.
	Found bool `json:"found"`
	// Fault: the job failed on its runner, not its code (WP-230).
	Fault *RunnerFault `json:"runner_fault,omitempty"`
}

type logSpan struct{ from, to int }

// failingExcerpt finds the failures in a job log and returns them with the
// lines around each, separated by "…", in at most limit lines (0: the
// default).
func failingExcerpt(log string, limit int) LogExcerpt {
	if limit <= 0 {
		limit = excerptMaxLines
	}
	lines := cleanLog(log)
	ex := LogExcerpt{Lines: len(lines)}
	if len(lines) == 0 {
		return ex
	}
	var spans []logSpan
	seen := map[string]bool{}
	firstClosing := -1
	for i, l := range lines {
		if l == "" {
			continue
		}
		for _, r := range logRules {
			if !r.re.MatchString(l) {
				continue
			}
			if r.closing {
				if firstClosing < 0 {
					firstClosing = i
				}
				break
			}
			if seen[l] {
				break
			}
			seen[l] = true
			from, to := i-r.before, i+1+r.after
			if r.back != nil {
				from = min(from, r.back(lines, i))
			}
			if r.block != nil {
				to = max(to, r.block(lines, i))
			}
			spans = append(spans, logSpan{max(0, from), min(len(lines), to)})
			break
		}
	}
	if ex.Fault = findRunnerFault(lines); ex.Fault != nil {
		at := ex.Fault.at
		spans = append(spans, logSpan{max(0, at-2), min(len(lines), at+3)})
		slices.SortStableFunc(spans, func(a, b logSpan) int { return a.from - b.from })
	}
	ex.Found = len(spans) > 0
	if !ex.Found {
		end := len(lines)
		if firstClosing >= 0 {
			end = firstClosing + 1
		}
		spans = []logSpan{{max(0, end-excerptTailLen), end}}
	}
	merged := spans[:1]
	for _, sp := range spans[1:] {
		if last := &merged[len(merged)-1]; sp.from <= last.to {
			last.to = max(last.to, sp.to)
			continue
		}
		merged = append(merged, sp)
	}
	var b strings.Builder
	used, texts := 0, map[string]bool{}
	for _, sp := range merged {
		block := lines[sp.from:sp.to]
		key := strings.Join(block, "\n")
		if texts[key] {
			continue
		}
		texts[key] = true
		sep := min(used, 1)
		room := limit - used - sep
		if room <= 0 || (len(block) > room && used > 0) {
			ex.Cut = true
			break
		}
		if sep == 1 {
			b.WriteString("…\n")
		}
		if len(block) > room {
			block, ex.Cut = block[:room], true
		}
		for _, l := range block {
			b.WriteString(cutLine(l))
			b.WriteByte('\n')
		}
		used += len(block) + sep
		ex.Shown += len(block)
	}
	ex.Text = b.String()
	return ex
}

// cutLine shortens a very long line (minified output, a JSON blob).
func cutLine(l string) string {
	if len(l) <= excerptLineMax {
		return l
	}
	cut := excerptLineMax
	for cut > 0 && l[cut]&0xC0 == 0x80 {
		cut--
	}
	return l[:cut] + fmt.Sprintf("… (%d more characters)", len(l)-cut)
}
