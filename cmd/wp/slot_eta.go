package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// An ETA for a command that waits for a slot and then runs (WP-1571): the
// wait comes from how long the holders have run against how long their
// commands usually take and from who is queued ahead; the run from the
// median of this command's recent runs here. Both come from a record of
// finished runs kept next to the slot state (runs.jsonl).

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
	Cmd     string
	Elapsed time.Duration
}

const (
	slotRunsFile   = "runs.jsonl"
	slotRunsRecent = 10  // runs the median is taken over
	slotRunsKeep   = 200 // lines kept when the file is trimmed
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

// slotRepoKey names the repository the command runs in, the same for all its
// worktrees: git's common directory, else the working directory.
func slotRepoKey() string {
	if out, err := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output(); err == nil { // #nosec G204 -- fixed arguments
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
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
// estimate. wait < 0: unknown; queued: it is waiting for a slot.
func slotETAMessage(name string, queued bool, wait, run time.Duration) string {
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
// latest lines now and then. Best effort.
func slotRecordRun(dir string, r slotRunRec) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	p := filepath.Join(dir, slotRunsFile)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- our own directory
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	if st, err := os.Stat(p); err == nil && st.Size() > 256<<10 {
		if all := slotRuns(dir); len(all) > slotRunsKeep {
			var sb strings.Builder
			for _, x := range all[len(all)-slotRunsKeep:] {
				line, _ := json.Marshal(x)
				sb.Write(line)
				sb.WriteByte('\n')
			}
			_ = os.WriteFile(p, []byte(sb.String()), 0o600)
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
		parts := strings.SplitN(strings.TrimSpace(string(buf[:n])), "\t", 4)
		if len(parts) < 4 {
			continue
		}
		ts, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, slotHolder{Cmd: slotCmdKey(parts[3]), Elapsed: time.Since(time.Unix(ts, 0))})
	}
	return out
}

// A waiter leaves NAME.PID.wait holding the time it began to wait, so those
// who come later can count who is ahead of them.

func slotWaiterPath(dir, name string) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%d.wait", name, os.Getpid()))
}

// slotAddWaiter registers this process as waiting; the result removes it.
func slotAddWaiter(dir, name string) (remove func()) {
	p := slotWaiterPath(dir, name)
	_ = os.WriteFile(p, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600)
	return func() { _ = os.Remove(p) }
}

// slotQueuedAhead counts the live waiters that began before this process.
func slotQueuedAhead(dir, name string) int {
	mine := time.Now().UnixNano()
	if b, err := os.ReadFile(slotWaiterPath(dir, name)); err == nil { // #nosec G304 -- our own directory
		if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			mine = v
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, name+".*.wait"))
	ahead := 0
	for _, p := range files {
		pidStr := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), name+"."), ".wait")
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid == os.Getpid() {
			continue
		}
		if !slotPidAlive(pid) {
			_ = os.Remove(p)
			continue
		}
		b, err := os.ReadFile(p) // #nosec G304 -- our own directory
		if err != nil {
			continue
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && (v < mine || (v == mine && pid < os.Getpid())) {
			ahead++
		}
	}
	return ahead
}

// slotQueuedNote is the ETA line for a command that has to wait: "" when
// there is no history of how long it runs.
func slotQueuedNote(dir, name, cmd string) string {
	recs := slotRuns(dir)
	run, ok := slotMedianRun(recs, name, cmd, slotRepoKey())
	if !ok {
		return ""
	}
	wait, ok := slotWaitETA(recs, name, slotHolders(dir, name), slotQueuedAhead(dir, name), run)
	if !ok {
		wait = -1
	}
	return slotETAMessage(name, true, wait, run)
}

// slotRunningNote is the ETA line for a command that has started.
func slotRunningNote(dir, name, cmd string) string {
	run, ok := slotMedianRun(slotRuns(dir), name, cmd, slotRepoKey())
	if !ok {
		return ""
	}
	return slotETAMessage(name, false, 0, run)
}
