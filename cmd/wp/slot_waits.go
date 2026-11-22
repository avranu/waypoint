package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Slot waits (WP-1618, WP-ADR149): a command that waits for a machine-wide
// slot inside an agent job says so in the file the runner named in
// WAYPOINT_SLOT_WAITS, so the runner can leave that time out of the job's
// work clock and let the waiting job give up its heavy place.

// slotWaitsEnv names the file an agent job's commands note their slot waits in.
const slotWaitsEnv = "WAYPOINT_SLOT_WAITS"

// noteSlotWait appends "wait <unix-ms> <pid> <name>" to the waits file, if
// there is one, and returns what appends the matching "got" line. A line
// that can't be written is dropped: the wait itself goes on regardless.
func noteSlotWait(name string) (done func()) {
	path := os.Getenv(slotWaitsEnv)
	if path == "" {
		return func() {}
	}
	note := func(what string) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- named by the runner that started this job
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(f, "%s %d %d %s\n", what, time.Now().UnixMilli(), os.Getpid(), name)
		_ = f.Close()
	}
	note("wait")
	return func() { note("got") }
}

// slotWaitReader reads a waits file. It keeps when it first saw a waiting
// process gone, which is when that wait is taken to have ended.
type slotWaitReader struct {
	path  string
	alive func(pid int) bool
	dead  map[int]time.Time
}

func newSlotWaitReader(path string) *slotWaitReader {
	return &slotWaitReader{path: path, alive: slotPidAlive, dead: map[int]time.Time{}}
}

type waitSpan struct {
	from, to time.Time
	live     bool // open, and its process is alive
}

// read returns how long slot waits took in all (several commands may wait at
// once: time they overlap counts once) and, when a live wait is open, when
// the unbroken stretch of waiting it belongs to began; zero otherwise.
func (r *slotWaitReader) read(now time.Time) (total time.Duration, waitingSince time.Time) {
	b, err := os.ReadFile(r.path)
	if err != nil {
		return 0, time.Time{}
	}
	type key struct {
		pid  int
		name string
	}
	open := map[key]time.Time{}
	var spans []waitSpan
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 || (f[0] != "wait" && f[0] != "got") {
			continue
		}
		ms, err1 := strconv.ParseInt(f[1], 10, 64)
		pid, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || pid <= 0 {
			continue
		}
		at, k := time.UnixMilli(ms), key{pid, f[3]}
		if f[0] == "wait" {
			open[k] = at
		} else if from, ok := open[k]; ok {
			delete(open, k)
			if at.After(from) {
				spans = append(spans, waitSpan{from: from, to: at})
			}
		}
	}
	for k, from := range open {
		end, live := now, true
		if !r.alive(k.pid) {
			live = false
			if _, ok := r.dead[k.pid]; !ok {
				r.dead[k.pid] = now
			}
			end = r.dead[k.pid]
		}
		if end.After(from) {
			spans = append(spans, waitSpan{from: from, to: end, live: live})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from.Before(spans[j].from) })
	var cur *waitSpan
	flush := func() {
		if cur == nil {
			return
		}
		total += cur.to.Sub(cur.from)
		if cur.live {
			waitingSince = cur.from
		}
	}
	for i := range spans {
		s := spans[i]
		if cur != nil && !s.from.After(cur.to) {
			if s.to.After(cur.to) {
				cur.to = s.to
			}
			cur.live = cur.live || s.live
			continue
		}
		flush()
		cur = &s
	}
	flush()
	return total, waitingSince
}
