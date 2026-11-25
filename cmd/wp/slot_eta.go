package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// slotRunRec is one finished run, a line of runs.jsonl.
type slotRunRec struct {
	Name string  `json:"name"` // the slot
	Cmd  string  `json:"cmd"`  // slotCmdKey
	Repo string  `json:"repo"` // slotRepoKey
	At   int64   `json:"at"`   // unix seconds, when it ended
	Secs float64 `json:"secs"`
}

// slotHolder is a command holding a slot.
type slotHolder struct {
	Pid     int // the wp holding it; 0 when the lock file doesn't say
	Cmd     string
	Session string // the Claude Code session it runs in; "" when the lock file doesn't say
	Elapsed time.Duration
}

const (
	slotRunsFile   = "runs.jsonl"
	slotRunsLock   = "runs.jsonl.flock"
	slotRunsRecent = 10
	slotRunsKeep   = 200
	slotETAPad     = 1.2
)

// slotCmdKey is the command as the record knows it: what follows "--" in wp's
// arguments (as the lock file has them), else all of them.
func slotCmdKey(args string) string {
	if _, after, ok := strings.Cut(args, " -- "); ok {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(args)
}

// slotRunKey is the command as a run records it: the words after the "--"
// that is one of wp's own arguments, else the command run (without the -json
// that goTestJSON adds to a go test, so a ship check and wp testsum of the
// same command share a history).
func slotRunKey(osArgs, argv []string) string {
	if len(osArgs) > 1 {
		for i, a := range osArgs[1:] {
			if a == "--" {
				return strings.TrimSpace(slotField(strings.Join(osArgs[1:][i+1:], " ")))
			}
		}
	}
	if len(argv) > 2 && argv[0] == "go" && argv[1] == "test" && argv[2] == "-json" {
		argv = append([]string{"go", "test"}, argv[3:]...)
	}
	return strings.TrimSpace(slotField(strings.Join(argv, " ")))
}

// slotRepoKey names the repository the command runs in, the same for all its
// worktrees: git's common directory, else the working directory.
func slotRepoKey() string { return slotRepoKeyIn("") }

// slotRepoKeyIn is slotRepoKey for the folder dir ("": this process's).
func slotRepoKeyIn(dir string) string {
	args := []string{"rev-parse", "--path-format=absolute", "--git-common-dir"}
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	if out, err := exec.Command("git", args...).Output(); err == nil { // #nosec G204 -- fixed arguments
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	if dir != "" {
		return dir
	}
	wd, _ := os.Getwd()
	return wd
}

// slotMedianRun is the median duration of the latest runs of the command in
// the repo (repo "": anywhere); ok says there are any.
func slotMedianRun(recs []slotRunRec, name, cmd, repo string) (time.Duration, bool) {
	var secs []float64
	for _, r := range recs {
		if r.Name == name && r.Cmd == cmd && (repo == "" || r.Repo == repo) {
			secs = append(secs, r.Secs)
		}
	}
	if len(secs) == 0 {
		return 0, false
	}
	if len(secs) > slotRunsRecent {
		secs = secs[len(secs)-slotRunsRecent:]
	}
	sort.Float64s(secs)
	m := secs[len(secs)/2]
	if len(secs)%2 == 0 {
		m = (secs[len(secs)/2-1] + secs[len(secs)/2]) / 2
	}
	return time.Duration(m * float64(time.Second)), true
}

// slotWaitETA is how long until a slot is free for a new arrival: holders
// free their slots after the rest of their usual run, each of the queued
// ahead takes the earliest and holds it for run (ok: unknown).
func slotWaitETA(recs []slotRunRec, name string, holders []slotHolder, queued int, run time.Duration) (time.Duration, bool) {
	if len(holders) == 0 {
		return 0, false
	}
	var free []time.Duration
	for _, h := range holders {
		usual, ok := slotMedianRun(recs, name, h.Cmd, "")
		if !ok {
			return 0, false
		}
		free = append(free, max(0, usual-h.Elapsed))
	}
	for range queued {
		sort.Slice(free, func(i, j int) bool { return free[i] < free[j] })
		free[0] += run
	}
	sort.Slice(free, func(i, j int) bool { return free[i] < free[j] })
	return free[0], true
}

// slotETAMessage is what the command tells the agent: "" when there is no
// estimate. wait < 0: unknown; queued: it is waiting for a slot; advice: add
// what to do meanwhile, which is only true of a run that went to the
// background (a foreground run's stderr is read after it has ended).
func slotETAMessage(name string, queued bool, wait, run time.Duration, advice bool) string {
	if run <= 0 {
		return ""
	}
	total := run
	if queued && wait > 0 {
		total += wait
	}
	mins := int((time.Duration(float64(total)*slotETAPad) + 30*time.Second) / time.Minute)
	if mins < 1 {
		return ""
	}
	n := strconv.Itoa(mins) + " minutes"
	if mins == 1 {
		n = "1 minute"
	}
	what := "command"
	if name == "tests" {
		what = "test"
	}
	var head string
	switch {
	case queued && wait < 0:
		head = fmt.Sprintf("The %s is queued, and once it starts is expected to take about %s.", what, n)
	case queued:
		head = fmt.Sprintf("The %s is queued, and is expected to complete in %s.", what, n)
	default:
		head = fmt.Sprintf("The %s is running, and is expected to complete in %s.", what, n)
	}
	if !advice {
		return head
	}
	return fmt.Sprintf("%s If you have other work to do in the meantime, handle it while you wait. Otherwise, end your turn and you'll be notified when the %s concludes.", head, what)
}

// slotRuns reads the record of finished runs (none when it can't be read).
func slotRuns(dir string) []slotRunRec {
	f, err := os.Open(filepath.Join(dir, slotRunsFile)) // #nosec G304 -- our own directory
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []slotRunRec
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r slotRunRec
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Secs >= 0 {
			out = append(out, r)
		}
	}
	return out
}

// slotRecordRun appends a finished run to the record, trimming it to the
// latest lines now and then, under the record's lock. Best effort.
func slotRecordRun(dir string, r slotRunRec) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	locked := false
	if lf, err := os.OpenFile(filepath.Join(dir, slotRunsLock), os.O_CREATE|os.O_RDWR, 0o600); err == nil { // #nosec G304 -- our own directory
		defer func() { _ = lf.Close() }()
		if unlock, err := fileLock(lf); err == nil {
			defer unlock()
			locked = true
		}
	}
	p := filepath.Join(dir, slotRunsFile)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- our own directory
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	if st, err := os.Stat(p); err == nil && locked && st.Size() > 256<<10 {
		if all := slotRuns(dir); len(all) > slotRunsKeep {
			var sb strings.Builder
			for _, x := range all[len(all)-slotRunsKeep:] {
				line, _ := json.Marshal(x)
				sb.Write(line)
				sb.WriteByte('\n')
			}
			if tmp, err := os.CreateTemp(dir, slotRunsFile+".*.tmp"); err == nil {
				_, werr := tmp.WriteString(sb.String())
				cerr := tmp.Close()
				if werr != nil || cerr != nil || os.Rename(tmp.Name(), p) != nil {
					_ = os.Remove(tmp.Name())
				}
			}
		}
	}
}

// slotHolders are the commands holding slots called name.
func slotHolders(dir, name string) []slotHolder {
	files, _ := filepath.Glob(filepath.Join(dir, name+".*.lock"))
	var out []slotHolder
	for _, p := range files {
		f, err := os.OpenFile(p, os.O_RDWR, 0o600) // #nosec G304 -- from our own directory
		if err != nil {
			continue
		}
		ok, err := slotTryLock(f)
		if err == nil && ok {
			_ = f.Close()
			continue
		}
		buf := make([]byte, 4096)
		n, _ := f.Read(buf)
		_ = f.Close()
		parts := strings.SplitN(strings.TrimSpace(string(buf[:n])), "\t", 7)
		if len(parts) < 4 {
			continue
		}
		session := ""
		if len(parts) >= 5 {
			session = parts[4]
		}
		cmd := slotCmdKey(parts[3])
		if len(parts) >= 6 && parts[5] != "" {
			cmd = parts[5]
		}
		ts, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(parts[0])
		out = append(out, slotHolder{Pid: pid, Cmd: cmd, Session: session, Elapsed: time.Since(time.Unix(ts, 0))})
	}
	return out
}

func slotWaiterPath(dir, name string) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%d.wait", name, os.Getpid()))
}

// slotSession is the Claude Code session this process runs in ("" outside
// one), recorded in its slot files: another process can't read it on darwin
// (audit #1382.1).
func slotSession() string { return os.Getenv("CLAUDE_CODE_SESSION_ID") }

// slotOwn says whether a slot file's process is session's: by the session
// it recorded, else by its environment, else (a wp from before sessions were
// recorded, on darwin) whether it began just now.
func slotOwn(recorded string, pid int, session string, young bool) bool {
	if recorded != "" {
		return recorded == session
	}
	if s := slotPidSession(pid); s != "" {
		return s == session
	}
	return young
}

// slotAddWaiter registers this process as waiting from now, for any slot.
func slotAddWaiter(dir, name string) (remove func()) {
	return slotAddWaiterFor(dir, name, time.Now(), nil)
}

// slotAddWaiterFor registers this process as waiting since arrival for the
// slots in order (nil: any); the result removes it. The file is written and
// locked under another name first, so nobody finds it unlocked and takes it
// for a dead waiter's. Waiters are served by arrival (slotBlockedSlots).
func slotAddWaiterFor(dir, name string, arrival time.Time, order []int) (remove func()) {
	p := slotWaiterPath(dir, name)
	tmp := p + ".new"
	line := strconv.FormatInt(arrival.UnixNano(), 10) + "\t" + slotField(slotSession()) + "\t" + slotOrderField(order)
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- our own directory
	if err != nil {
		return func() {}
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return func() {}
	}
	if ok, err := slotTryLock(f); !ok || err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return func() {}
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return func() {}
	}
	return func() {
		_ = os.Remove(p)
		_ = f.Close()
	}
}

// slotOrderField is the slots as comma-separated numbers.
func slotOrderField(order []int) string {
	s := make([]string, len(order))
	for i, v := range order {
		s[i] = strconv.Itoa(v)
	}
	return strings.Join(s, ",")
}

// slotWaiterLine parses a waiter file: when it began (unix ns), its session
// ("": not recorded, by a wp from before it was) and the slots it may take
// (nil: any, as a wp from before they were recorded).
func slotWaiterLine(b []byte) (at int64, session string, order []int, ok bool) {
	f := strings.SplitN(strings.TrimSpace(string(b)), "\t", 3)
	at, err := strconv.ParseInt(f[0], 10, 64)
	if len(f) > 1 {
		session = f[1]
	}
	if len(f) > 2 {
		for _, v := range strings.Split(f[2], ",") {
			if n, err := strconv.Atoi(v); err == nil {
				order = append(order, n)
			}
		}
	}
	return at, session, order, err == nil
}

// slotWaiter is a live waiter of a slot name.
type slotWaiter struct {
	pid     int
	at      int64 // unix ns
	session string
	order   []int     // nil: an older wp's waiter, which recorded none
	seen    time.Time // when its file was last refreshed (its last poll)
}

// slotLiveWaiters are the waiters of name whose process still holds its file
// (the lock goes with the process, however it died); the file of one that
// doesn't is removed. This process's own is among them.
func slotLiveWaiters(dir, name string) []slotWaiter {
	var ws []slotWaiter
	files, _ := filepath.Glob(filepath.Join(dir, name+".*.wait"))
	for _, p := range files {
		pidStr := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), name+"."), ".wait")
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		f, err := os.Open(p) // #nosec G304 -- our own directory
		if err != nil {
			continue
		}
		if ok, err := slotTryLock(f); err == nil && ok {
			_ = os.Remove(p)
			_ = f.Close()
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, 4096))
		var seen time.Time
		if st, serr := f.Stat(); serr == nil {
			seen = st.ModTime()
		}
		_ = f.Close()
		if err != nil {
			continue
		}
		at, session, order, ok := slotWaiterLine(b)
		if !ok {
			continue
		}
		ws = append(ws, slotWaiter{pid, at, session, order, seen})
	}
	return ws
}

// slotWaiterStale is how long another process's waiter file may go without a
// refresh before it blocks nobody: a waiter that is alive but not polling
// (stopped with Ctrl-Z, hung) must not hold up the ones behind it. A waiter
// refreshes its file on every poll of its wait loop (slotPoll, 100ms), so this
// is 100 polls: this machine runs at several times its core count, where a
// live waiter can miss seconds, and losing its place costs it a long wait,
// while a stopped one being passed 10 seconds late costs nearly nothing.
const slotWaiterStale = 10 * time.Second

// slotTouchWaiter refreshes this process's waiter file of name: its poll is
// the sign that it is still waiting. Best effort.
func slotTouchWaiter(dir, name string) {
	now := time.Now()
	_ = os.Chtimes(slotWaiterPath(dir, name), now, now)
}

// slotBlockedSlots are the slots a waiter that came at arrival (unix ns) may
// not try yet: those an earlier live waiter may take. Ties go to the lower
// pid. A waiter held back behind its worktree's long run has no waiter file
// yet, so it blocks nobody, and one that can't use a slot doesn't block it:
// a long waiter at the head leaves the upper slots to the short ones
// (WP-1876, WP-ADR150). A waiter of an older wp that recorded no slots blocks
// none (it doesn't honour the order either, and a stall behind a long run is
// worse than one start out of turn), and neither does another process's
// waiter whose file was not refreshed within slotWaiterStale.
func slotBlockedSlots(dir, name string, arrival int64) (blocked func(slot int) bool) {
	return slotBlockedSlotsAt(dir, name, arrival, time.Now())
}

// slotBlockedSlotsAt is slotBlockedSlots as of now.
func slotBlockedSlotsAt(dir, name string, arrival int64, now time.Time) (blocked func(slot int) bool) {
	var ahead []slotWaiter
	for _, w := range slotLiveWaiters(dir, name) {
		if w.pid != os.Getpid() && w.order != nil && now.Sub(w.seen) <= slotWaiterStale &&
			(w.at < arrival || (w.at == arrival && w.pid < os.Getpid())) {
			ahead = append(ahead, w)
		}
	}
	return func(slot int) bool {
		for _, w := range ahead {
			if slices.Contains(w.order, slot) {
				return true
			}
		}
		return false
	}
}

// slotQueuedAhead counts the live waiters that began before this process.
func slotQueuedAhead(dir, name string) int { return slotQueuedAheadOf(dir, name, "") }

// slotQueuedAheadOf is slotQueuedAhead for a command that isn't this process
// but one of session's (the wait hook asks about the command it is told of):
// the waiters of that session are the command's own, not counted, and those
// that began before the latest of them are ahead; with none, all are. With
// session "": the waiters that began before this process.
func slotQueuedAheadOf(dir, name, session string) int {
	mine, minePid := time.Now().UnixNano(), os.Getpid()
	if session == "" {
		if b, err := os.ReadFile(slotWaiterPath(dir, name)); err == nil { // #nosec G304 -- our own directory
			if v, _, _, ok := slotWaiterLine(b); ok {
				mine = v
			}
		}
	}
	type waiter struct {
		pid int
		at  int64
		own bool
	}
	var ws []waiter
	for _, w := range slotLiveWaiters(dir, name) {
		if session == "" && w.pid == os.Getpid() {
			continue
		}
		own := false
		if session != "" {
			own = slotOwn(w.session, w.pid, session, time.Since(time.Unix(0, w.at)) < 15*time.Second)
		}
		ws = append(ws, waiter{w.pid, w.at, own})
	}
	if session != "" {
		first := true
		for _, w := range ws {
			if w.own && (first || w.at > mine) {
				mine, minePid, first = w.at, w.pid, false
			}
		}
	}
	ahead := 0
	for _, w := range ws {
		if !w.own && (w.at < mine || (w.at == mine && w.pid < minePid)) {
			ahead++
		}
	}
	return ahead
}

// slotQueuedETA is the message for a command that has to wait, given the
// history, the slot's holders, how many are queued ahead and the command's
// own run time: the wait is unknown (-1) when a holder's command has no
// history.
func slotQueuedETA(recs []slotRunRec, name string, holders []slotHolder, ahead int, run time.Duration, advice bool) string {
	wait, ok := slotWaitETA(recs, name, holders, ahead, run)
	if !ok {
		wait = -1
	}
	return slotETAMessage(name, true, wait, run, advice)
}

// slotQueuedNote is the ETA line for a command that has to wait: "" when
// there is no history of how long it runs. It is what a foreground run says on
// stderr, so it has no advice on what to do meanwhile.
func slotQueuedNote(dir, name, cmd string) string {
	recs := slotRuns(dir)
	run, ok := slotMedianRun(recs, name, cmd, slotRepoKey())
	if !ok {
		return ""
	}
	return slotQueuedETA(recs, name, slotHolders(dir, name), slotQueuedAhead(dir, name), run, false)
}

// slotRunningNote is the ETA line for a command that has started.
func slotRunningNote(dir, name, cmd string) string {
	run, ok := slotMedianRun(slotRuns(dir), name, cmd, slotRepoKey())
	if !ok {
		return ""
	}
	return slotETAMessage(name, false, 0, run, false)
}
