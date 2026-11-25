package main

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	stallFloor    = 5 * time.Minute
	stallMinAge   = 30 * time.Minute
	stallQuiet    = 15 * time.Minute
	stallTimeout  = time.Hour
	stallEvery    = time.Minute
	clockTicks    = 100
	stallWorkCPU  = 20
	stallBurstCPU = 10
)

// procEntry is one process as /proc shows it.
type procEntry struct {
	pid, ppid, pgrp int
	comm            string
	args            []string
	start           uint64
	own             uint64
	cpu             uint64
	io              uint64
	ioKnown         bool
	mine            bool
	outs            []string
}

// shell: a shell, or a script one runs (`./wait.sh` shows its script's
// name, with the shell first on its command line).
func (p *procEntry) shell() bool {
	return shellNames[p.comm] || len(p.args) > 0 && shellNames[filepath.Base(p.args[0])]
}

// procKey tells a process apart from a later one with the same pid.
type procKey struct {
	pid   int
	start uint64
}

func (p *procEntry) key() procKey { return procKey{p.pid, p.start} }

// procTable is one look at /proc.
type procTable struct {
	at    time.Time
	boot  time.Time
	procs map[int]*procEntry
	kids  map[int][]int
}

// taskOutputRE is a Claude Code background task's output file: the
// session's id, then the task's.
var taskOutputRE = lazyre.New(`/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/tasks/([A-Za-z0-9_-]{1,64})\.output$`)

// readProcTable reads every process under root (/proc). Fds are only
// readable for this user's processes, which are the only ones that can be
// its jobs; the rest are read for their command lines.
func readProcTable(root string, now time.Time) (*procTable, error) {
	boot, err := readBootTime(filepath.Join(root, "stat"))
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	t := &procTable{at: now, boot: boot, procs: map[int]*procEntry{}, kids: map[int][]int{}}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p := readProc(filepath.Join(root, e.Name()), pid)
		if p == nil {
			continue
		}
		t.procs[pid] = p
		t.kids[p.ppid] = append(t.kids[p.ppid], pid)
	}
	return t, nil
}

func readBootTime(path string) (time.Time, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- /proc/stat
	if err != nil {
		return time.Time{}, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(s, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("no btime in %s", path)
}

// readProc reads one process; nil when it is gone or unreadable.
func readProc(dir string, pid int) *procEntry {
	stat, err := os.ReadFile(filepath.Join(dir, "stat")) // #nosec G304 -- /proc
	if err != nil {
		return nil
	}
	p := parseProcStat(string(stat))
	if p == nil || p.pid != pid {
		return nil
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil { // #nosec G304 -- /proc
		p.args = strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(p.args) == 1 && p.args[0] == "" {
			p.args = nil
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "io")); err == nil { // #nosec G304 -- /proc
		p.io, p.ioKnown = parseProcIO(string(b)), true
	}
	fdDir := filepath.Join(dir, "fd")
	if fds, err := os.ReadDir(fdDir); err == nil {
		p.mine = true
		for _, fd := range fds {
			if l, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && taskOutputRE.MatchString(l) && !slices.Contains(p.outs, l) {
				p.outs = append(p.outs, l)
			}
		}
	}
	return p
}

// parseProcStat reads /proc/PID/stat; the command name is in parentheses
// and may itself hold spaces and parentheses, so the fields count from the
// last ')'.
func parseProcStat(s string) *procEntry {
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(s[:open]))
	if err != nil {
		return nil
	}
	f := strings.Fields(s[end+1:])
	if len(f) < 20 {
		return nil
	}
	n := func(i int) uint64 {
		v, _ := strconv.ParseInt(f[i-3], 10, 64)
		return uint64(max(v, 0))
	}
	id := func(i int) int {
		v, _ := strconv.Atoi(f[i-3])
		return v
	}
	p := &procEntry{pid: pid, comm: s[open+1 : end], ppid: id(4), pgrp: id(5), start: n(22)}
	p.own = n(14) + n(15)
	p.cpu = p.own + n(16) + n(17)
	return p
}

func parseProcIO(s string) uint64 {
	var total uint64
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && (k == "rchar" || k == "wchar") {
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			total += n
		}
	}
	return total
}

// startedAt is when a process started.
func (t *procTable) startedAt(p *procEntry) time.Time {
	tick := time.Second / clockTicks
	ticks := min(p.start, uint64(math.MaxInt64/tick)) // #nosec G115 -- a positive int64
	return t.boot.Add(time.Duration(ticks) * tick)    // #nosec G115 -- bounded just above
}

// subtree is the process and all its descendants, the process first.
func (t *procTable) subtree(pid int) []*procEntry {
	var out []*procEntry
	seen := map[int]bool{}
	var walk func(int)
	walk = func(id int) {
		p := t.procs[id]
		if p == nil || seen[id] || len(out) > 500 {
			return
		}
		seen[id] = true
		out = append(out, p)
		for _, k := range t.kids[id] {
			walk(k)
		}
	}
	walk(pid)
	return out
}

// bgJob is one background job: the topmost process holding its output.
type bgJob struct {
	session, task, out string
	root               *procEntry
}

// findJobs finds the Claude Code background jobs in the table.
func findJobs(t *procTable) []*bgJob {
	byOut := map[string]*procEntry{}
	for _, p := range t.procs {
		for _, o := range p.outs {
			if parent := t.procs[p.ppid]; parent != nil && slices.Contains(parent.outs, o) {
				continue
			}
			if cur := byOut[o]; cur == nil || p.start < cur.start || (p.start == cur.start && p.pid < cur.pid) {
				byOut[o] = p
			}
		}
	}
	var out []*bgJob
	for o, p := range byOut {
		m := taskOutputRE.FindStringSubmatch(o)
		out = append(out, &bgJob{session: m[1], task: m[2], out: o, root: p})
	}
	slices.SortFunc(out, func(a, b *bgJob) int { return strings.Compare(a.out, b.out) })
	return out
}

// jobCommand is what the agent asked to run: Claude Code runs it as
// `bash -c '<setup> && eval '\”COMMAND'\” && pwd -P >| …'`. ok is false
// when the root isn't that shell: a process a finished command left behind
// (`npm run dev &`) still holds the output file, but it isn't a job Claude
// Code runs or can stop.
func jobCommand(root *procEntry) (cmd string, ok bool) {
	if len(root.args) < 3 || root.args[1] != "-c" || !shellNames[filepath.Base(root.args[0])] {
		return strings.Join(root.args, " "), false
	}
	for _, c := range parseShell(root.args[2]) {
		if len(c.Words) >= 2 && c.Words[0] == "eval" {
			return strings.Join(c.Words[1:], " "), true
		}
	}
	return root.args[2], false
}

// pollerNames wait or look and go: a lasting one isn't work.
var pollerNames = map[string]bool{"sleep": true, "pgrep": true, "pidof": true, "flock": true, "timeout": true, "inotifywait": true}

// wpWaiter: the process is a Waypoint command that waits on something it
// names (a pull request, a slot, a request), described for the agent.
func wpWaiter(p *procEntry) string {
	if len(p.args) < 2 || filepath.Base(p.args[0]) != "wp" {
		return ""
	}
	var words []string
	for _, a := range p.args[1:] {
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
		}
		if len(words) == 2 {
			break
		}
	}
	if len(words) == 0 {
		return ""
	}
	two := words[0]
	if len(words) > 1 {
		two += " " + words[1]
	}
	switch {
	case words[0] == "slot", words[0] == "testsum", two == "pr wait", two == "request wait", two == "ci wait":
		return excerpt(strings.Join(append([]string{"wp"}, p.args[1:]...), " "), 80)
	}
	return ""
}

// timeoutBound is the timeout the whole command runs under, if it is one
// `timeout DURATION …` (after any cd): zero when nothing bounds the whole
// job, as with `timeout 60 lint; git push`, whose push nothing bounds.
func timeoutBound(cmd string) time.Duration {
	var main []*guardCmd
	for _, c := range parseShell(cmd) {
		if len(c.Words) > 0 && c.Words[0] == "cd" && !c.PipeIn {
			continue
		}
		main = append(main, c)
	}
	if len(main) != 1 || len(main[0].Words) == 0 || filepath.Base(main[0].Words[0]) != "timeout" {
		return 0
	}
	w := main[0].Words[1:]
	for len(w) > 0 && strings.HasPrefix(w[0], "-") {
		a := w[0]
		w = w[1:]
		if (a == "-s" || a == "-k" || a == "--signal" || a == "--kill-after") && len(w) > 0 {
			w = w[1:]
		}
	}
	if len(w) < 2 {
		return 0
	}
	return parseTimeoutDuration(w[0])
}

// parseTimeoutDuration reads timeout(1)'s DURATION: a number with an
// optional s, m, h or d.
func parseTimeoutDuration(s string) time.Duration {
	unit := time.Second
	switch {
	case strings.HasSuffix(s, "s"):
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		s, unit = s[:len(s)-1], time.Minute
	case strings.HasSuffix(s, "h"):
		s, unit = s[:len(s)-1], time.Hour
	case strings.HasSuffix(s, "d"):
		s, unit = s[:len(s)-1], 24*time.Hour
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return time.Duration(f * float64(unit))
}

// pgrepUse is a pgrep or pkill in a command and the pattern it looks for.
type pgrepUse struct {
	tool, pattern string
	exact         bool
	full          bool
}

// pgrepValueFlags take a value: the rest of their word (`-unobody`), or the
// next word.
var pgrepValueFlags = map[string]bool{"-d": true, "-g": true, "-G": true, "-P": true, "-s": true, "-t": true, "-u": true, "-U": true, "-F": true,
	"-O": true, "-r": true,
	"--delimiter": true, "--pgroup": true, "--group": true, "--parent": true, "--session": true, "--terminal": true, "--euid": true, "--uid": true,
	"--pidfile": true, "--ns": true, "--nslist": true, "--signal": true, "--older": true, "--runstates": true, "--cgroup": true, "--env": true}

// pkillSignalRE is pkill's `-SIGNAL` (-9, -TERM, -SIGUSR1), not options.
var pkillSignalRE = lazyre.New(`^-(\d+|(SIG)?[A-Z]{2,}[0-9]*)$`)

// pgrepUses finds the pgrep and pkill calls in a command.
func pgrepUses(cmd string) []pgrepUse {
	var out []pgrepUse
	var walk func([]*guardCmd)
	walk = func(cs []*guardCmd) {
		for _, c := range cs {
			for _, s := range c.Subs {
				walk(s)
			}
			if len(c.Words) < 2 {
				continue
			}
			tool := filepath.Base(c.Words[0])
			if tool != "pgrep" && tool != "pkill" {
				continue
			}
			u := pgrepUse{tool: tool}
			w := c.Words[1:]
			for len(w) > 0 {
				a := w[0]
				w = w[1:]
				switch {
				case a == "--":
					if len(w) > 0 {
						u.pattern = w[0]
					}
					w = nil
				case a == "--full":
					u.full = true
				case a == "--exact":
					u.exact = true
				case pgrepValueFlags[a]:
					if len(w) > 0 {
						w = w[1:]
					}
				case strings.HasPrefix(a, "--"):
				case tool == "pkill" && pkillSignalRE.MatchString(a):
				case strings.HasPrefix(a, "-") && len(a) > 1:
					for i, r := range a[1:] {
						if r == 'f' {
							u.full = true
						}
						if r == 'x' {
							u.exact = true
						}
						if pgrepValueFlags["-"+string(r)] {
							if i+2 == len(a) && len(w) > 0 {
								w = w[1:]
							}
							break
						}
					}
				default:
					u.pattern = a
					w = nil
				}
			}
			if u.pattern != "" {
				out = append(out, u)
			}
		}
	}
	walk(parseShell(cmd))
	return out
}

// matches says whether the pattern finds text, as pgrep's extended regular
// expression (read as Go's; a pattern Go can't read is taken literally).
func (u pgrepUse) matches(text string) bool {
	pat := u.pattern
	if u.exact {
		pat = "^(?:" + pat + ")$"
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		if u.exact {
			return text == u.pattern
		}
		return strings.Contains(text, u.pattern)
	}
	return re.MatchString(text)
}

// selfProof is the pattern written so that it can't match its own text:
// the first letter or digit of each alternative in brackets
// (`[g]olangci-lint run`, `[v]ite|[w]ebpack`). Empty when that doesn't do it.
func (u pgrepUse) selfProof() string {
	var alts []string
	for _, alt := range topAlternatives(u.pattern) {
		if (pgrepUse{pattern: alt}).matches(alt) {
			alt = bracketFirst(alt)
		}
		alts = append(alts, alt)
	}
	p := strings.Join(alts, "|")
	if p == u.pattern || (pgrepUse{pattern: p}).matches(p) {
		return ""
	}
	return p
}

// topAlternatives splits a regular expression at its top-level `|`s.
func topAlternatives(re string) []string {
	var out []string
	depth, class, from := 0, false, 0
	for i := 0; i < len(re); i++ {
		switch c := re[i]; {
		case c == '\\':
			i++
		case class:
			class = c != ']'
		case c == '[':
			class = true
			if i+1 < len(re) && re[i+1] == ']' {
				i++
			}
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == '|' && depth == 0:
			out = append(out, re[from:i])
			from = i + 1
		}
	}
	return append(out, re[from:])
}

// bracketFirst puts an expression's first plain letter or digit, outside
// any class or escape, in brackets.
func bracketFirst(re string) string {
	class := false
	for i := 0; i < len(re); i++ {
		switch c := re[i]; {
		case c == '\\':
			i++
		case class:
			class = c != ']'
		case c == '[':
			class = true
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9':
			return re[:i] + "[" + string(c) + "]" + re[i+1:]
		}
	}
	return re
}

// pgrepSelfNudge refuses a command whose `pgrep -f`/`pkill -f` pattern is
// in its own text: Claude Code runs the command under a `bash -c` whose
// command line holds that text, so pgrep always finds that shell and pkill
// kills it (WP-605: a `while pgrep -f "golangci-lint run"` loop never
// ended). Empty when there is nothing to say.
func pgrepSelfNudge(cmd string) string {
	for _, u := range pgrepUses(cmd) {
		if !u.full || strings.ContainsRune(u.pattern, '\'') || !u.matches("/bin/bash -c "+cmd) {
			continue
		}
		what := "always finds that shell, so a loop waiting on it never ends"
		if u.tool == "pkill" {
			what = "kills that shell, which ends this command"
		}
		text := fmt.Sprintf("`%s -f '%s'` matches this command's own `bash -c` shell, whose command line holds the pattern: it %s.", u.tool, u.pattern, what)
		if p := u.selfProof(); p != "" {
			text += fmt.Sprintf(" Write the pattern so it can't match itself, e.g. `%s -f '%s'`.", u.tool, p)
		} else {
			text += " Write the pattern so it can't match itself: put one letter of each alternative in brackets, as in `[g]olangci-lint run`."
		}
		return text
	}
	return ""
}

// jobSample is what a look saw of one job.
type jobSample struct {
	at     time.Time
	cpu    uint64
	procs  map[procKey]uint64
	io     map[procKey]uint64
	outLen int64
	outMod time.Time
}

// stallWatch is what the runner remembers of one job between looks.
type stallWatch struct {
	session  string
	task     string
	first    time.Time
	moved    time.Time
	prev     *jobSample
	comeback map[string]int
	watched  int
	reported bool
	sent     bool
	due      time.Time
	said     string
}

// stalledJob is a job the runner reports: what it asks Waypoint to tell
// the session (core.StalledJob).
type stalledJob struct {
	Session   string    `json:"session"`
	Task      string    `json:"task"`
	PID       int       `json:"pid"`
	Group     bool      `json:"group"` // its pid leads its own process group
	Command   string    `json:"command"`
	StartedAt time.Time `json:"started_at"`
	Quiet     int       `json:"quiet_minutes"`
	Evidence  []string  `json:"evidence"`
	Running   []string  `json:"running"`
	// Timeout: the timeout its whole command runs under, in minutes (policy
	// 2 only: older Waypoints refuse fields they don't know).
	Timeout int `json:"timeout_minutes,omitempty"`
}

// settledJob is a reported job that moved again or ended (policy 2).
type settledJob struct {
	Session string `json:"session"`
	Task    string `json:"task"`
	How     string `json:"how"` // progress, ended
}

// stallWatcher looks at a machine's background jobs.
type stallWatcher struct {
	jobs    map[string]*stallWatch
	policy  int
	probed  time.Time
	settled []settledJob
}

func newStallWatcher() *stallWatcher { return &stallWatcher{jobs: map[string]*stallWatch{}} }

// settle: a reported job moved again or ended.
func (w *stallWatcher) settle(sw *stallWatch, how string) {
	if sw.sent && len(w.settled) < 200 {
		w.settled = append(w.settled, settledJob{Session: sw.session, Task: sw.task, How: how})
	}
	sw.sent, sw.reported, sw.due, sw.said = false, false, time.Time{}, ""
}

// look takes one look at the table and returns the jobs newly stalled.
// stat reads an output file's size and modification time. A panic undoes
// this look's marks, so the jobs it found are reported at a later look.
func (w *stallWatcher) look(t *procTable, stat func(string) (os.FileInfo, error)) []stalledJob {
	var out []stalledJob
	var marked []*stallWatch
	defer func() {
		if r := recover(); r != nil {
			for _, sw := range marked {
				sw.reported, sw.due = false, time.Time{}
			}
			panic(r)
		}
	}()
	seen := map[string]bool{}
	for _, j := range findJobs(t) {
		id := fmt.Sprintf("%s|%d|%d", j.out, j.root.pid, j.root.start)
		seen[id] = true
		started := t.startedAt(j.root)
		cmd, ok := jobCommand(j.root)
		if !ok {
			continue
		}
		bound := timeoutBound(cmd)
		if w.policy < 2 && bound > 0 && bound < stallTimeout {
			continue
		}
		fi, err := stat(j.out)
		if err != nil {
			continue
		}
		tree := t.subtree(j.root.pid)
		cur := &jobSample{at: t.at, procs: map[procKey]uint64{}, io: map[procKey]uint64{}, outLen: fi.Size(), outMod: fi.ModTime()}
		waiter := ""
		for _, p := range tree {
			cur.cpu += p.cpu
			if w := wpWaiter(p); w != "" && len(t.kids[p.pid]) == 0 {
				waiter = w
			}
			if p != j.root && !p.shell() && !pollerNames[p.comm] {
				cur.procs[p.key()] = p.own
				if p.ioKnown {
					cur.io[p.key()] = p.io
				}
			}
		}
		sw := w.jobs[id]
		if sw == nil {
			sw = &stallWatch{session: j.session, task: j.task, first: t.at, comeback: map[string]int{}}
			w.jobs[id] = sw
		} else {
			sw.watched++
			if progressed(sw.prev, cur) || waiter != "" {
				sw.moved = t.at
				if w.policy >= 2 && (sw.reported || sw.sent) {
					w.settle(sw, "progress")
				}
			}
			for _, p := range tree {
				if p != j.root && t.startedAt(p).After(sw.prev.at) && !p.shell() {
					sw.comeback[p.comm]++
				}
			}
		}
		sw.prev = cur
		quietFrom := sw.first
		if sw.moved.After(quietFrom) {
			quietFrom = sw.moved
		}
		minAge, quiet, again := stallMinAge, stallQuiet, !sw.reported
		if w.policy >= 2 {
			minAge, quiet, again = stallFloor, stallFloor, !t.at.Before(sw.due)
		}
		if !again || waiter != "" || t.at.Sub(started) < minAge || t.at.Sub(quietFrom) < quiet || t.at.Sub(cur.outMod) < quiet {
			continue
		}
		r := stalledReport(t, j, sw, cmd, started, tree)
		sw.reported = true
		if w.policy >= 2 {
			if bound > 0 {
				r.Timeout = max(int(bound.Minutes()), 1)
			}
			sw.sent, sw.due = true, t.at.Add(stallAgainMax)
		}
		marked = append(marked, sw)
		out = append(out, r)
	}
	for id, sw := range w.jobs {
		if !seen[id] {
			w.settle(sw, "ended")
			delete(w.jobs, id)
		}
	}
	return out
}

// stallAgainMax: with policy 2, a quiet job is reported again at least
// this often unless Waypoint says otherwise.
const stallAgainMax = 15 * time.Minute

// find is the job a report named.
func (w *stallWatcher) find(session, task string) *stallWatch {
	for _, sw := range w.jobs {
		if sw.task == task && sw.session == session {
			return sw
		}
	}
	return nil
}

// progressed: between two looks the job wrote output, a lasting process
// worked (CPU or reads and writes), or its short-lived processes used
// more CPU than looking and sleeping does.
func progressed(prev, cur *jobSample) bool {
	if prev == nil {
		return false
	}
	if cur.outLen != prev.outLen || !cur.outMod.Equal(prev.outMod) {
		return true
	}
	for k, cpu := range cur.procs {
		if was, ok := prev.procs[k]; ok && cpu >= was+stallWorkCPU {
			return true
		}
	}
	for k, n := range cur.io {
		if was, ok := prev.io[k]; ok && n != was {
			return true
		}
	}
	secs := cur.at.Sub(prev.at).Seconds()
	return cur.cpu > prev.cpu && float64(cur.cpu-prev.cpu)/clockTicks >= stallBurstCPU*secs/60
}

// stalledReport puts the evidence into words.
func stalledReport(t *procTable, j *bgJob, sw *stallWatch, cmd string, started time.Time, tree []*procEntry) stalledJob {
	now := t.at
	quietFrom := sw.first
	if sw.moved.After(quietFrom) {
		quietFrom = sw.moved
	}
	r := stalledJob{Session: j.session, Task: j.task, PID: j.root.pid, Group: j.root.pgrp == j.root.pid, Command: excerpt(cmd, 300),
		StartedAt: started.UTC(), Quiet: int(now.Sub(quietFrom).Minutes())}
	out := fmt.Sprintf("no output for %s", minutes(now.Sub(sw.prev.outMod)))
	if sw.prev.outLen == 0 {
		out = "no output at all"
	}
	r.Evidence = append(r.Evidence, fmt.Sprintf("Running %s; %s.", minutes(now.Sub(started)), out))
	var work, idle []string
	for _, p := range tree[1:] {
		if p.shell() || pollerNames[p.comm] {
			continue
		}
		work = append(work, p.comm)
		if !t.startedAt(p).After(quietFrom) {
			idle = append(idle, fmt.Sprintf("%s (pid %d)", p.comm, p.pid))
		}
	}
	watched := minutes(now.Sub(quietFrom))
	switch {
	case len(work) == 0:
		r.Evidence = append(r.Evidence, fmt.Sprintf("In the %s watched, nothing under it but shells and %s; its processes used too little CPU for work.", watched, pollerList(tree)))
	case len(idle) > 0:
		r.Evidence = append(r.Evidence, fmt.Sprintf("In the %s watched, no CPU work and no reads or writes by %s.", watched, strings.Join(idle[:min(len(idle), 4)], ", ")))
	default:
		r.Evidence = append(r.Evidence, fmt.Sprintf("In the %s watched, its processes used too little CPU for work and none lasted with reads or writes.", watched))
	}
	var back []string
	for name, n := range sw.comeback {
		if n >= 2 && pollerNames[name] {
			back = append(back, name)
		}
	}
	slices.Sort(back)
	if len(back) > 0 {
		r.Evidence = append(r.Evidence, fmt.Sprintf("A polling loop: %s keeps coming back with a new pid.", strings.Join(back, ", ")))
	}
	inTree := map[int]bool{}
	for _, p := range tree {
		inTree[p.pid] = true
	}
	for _, u := range pgrepUses(cmd) {
		if u.full && u.matches(strings.Join(j.root.args, " ")) {
			r.Evidence = append(r.Evidence, fmt.Sprintf("`%s -f '%s'` matches the job's own `bash -c` command line, so it always finds a process.", u.tool, u.pattern))
		}
		var found []string
		for _, p := range t.procs {
			if !p.mine || inTree[p.pid] || p.comm == "pgrep" || p.comm == "pkill" {
				continue
			}
			text := p.comm
			if u.full {
				text = strings.Join(p.args, " ")
			}
			if text != "" && u.matches(text) {
				found = append(found, fmt.Sprintf("pid %d %s", p.pid, excerpt(strings.Join(p.args, " "), 60)))
			}
		}
		slices.Sort(found)
		if len(found) == 0 {
			r.Evidence = append(r.Evidence, fmt.Sprintf("Nothing outside the job matches %s's pattern '%s'.", u.tool, u.pattern))
		} else {
			r.Evidence = append(r.Evidence, fmt.Sprintf("Outside the job, '%s' matches %s.", u.pattern, strings.Join(found[:min(len(found), 2)], "; ")))
		}
	}
	for _, p := range tree[:min(len(tree), 6)] {
		line := excerpt(strings.Join(p.args, " "), 80)
		if p == j.root {
			line = p.comm + " (the job's shell)"
		}
		r.Running = append(r.Running, fmt.Sprintf("pid %d: %s", p.pid, line))
	}
	return r
}

// pollerList names the pollers running under a job now ("sleep").
func pollerList(tree []*procEntry) string {
	var names []string
	for _, p := range tree {
		if pollerNames[p.comm] && !slices.Contains(names, p.comm) {
			names = append(names, p.comm)
		}
	}
	if len(names) == 0 {
		return "short-lived commands"
	}
	return strings.Join(names, ", ")
}

// minutes says a duration in whole minutes ("27 min").
func minutes(d time.Duration) string {
	return fmt.Sprintf("%d min", int(d.Minutes()))
}
