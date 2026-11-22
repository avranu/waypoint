package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Slot waits (WP-1618, WP-ADR149): a command that waits for a machine-wide
// slot inside an agent job says so in the file the runner named in
// WAYPOINT_SLOT_WAITS, so the runner can leave that time out of the job's
// work clock and let the waiting job give up its heavy place.

// slotWaitsEnv names the file an agent job's commands note their slot waits in.
const slotWaitsEnv = "WAYPOINT_SLOT_WAITS"

// slotWaitWarn is where a wait that can't be noted is said.
var slotWaitWarn io.Writer = os.Stderr

// noteSlotWait appends "wait <unix-ms> <pid> <name>" to the waits file, if
// there is one, and returns what appends the matching "got" line. A line
// that can't be written is dropped, and said once: the wait itself goes on
// regardless, and counts as the job's work.
func noteSlotWait(name string) (done func()) {
	path := os.Getenv(slotWaitsEnv)
	if path == "" {
		return func() {}
	}
	warned := false
	note := func(what string) {
		// The path is the environment's, so whatever runs this command
		// chose it: it is opened with that user's own rights, to append,
		// and never created.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 G703 -- see above
		if err != nil {
			if !warned {
				warned = true
				_, _ = fmt.Fprintf(slotWaitWarn, "wp: this wait for a %q slot couldn't be noted for the runner, so it counts against the job's time: %v\n", name, err)
			}
			return
		}
		_, _ = fmt.Fprintf(f, "%s %d %d %s\n", what, time.Now().UnixMilli(), os.Getpid(), name)
		_ = f.Close()
	}
	note("wait")
	return func() { note("got") }
}

// slotWaitReader reads a waits file. It keeps when it first saw a waiting
// process gone, which is when that wait is taken to have ended. The job's
// clock reads it from its watcher and from whoever ends the job, so read
// holds mu. A wait counts only between start (the job's; zero: any time)
// and now, whatever its lines say: the file is the job's own commands' to
// write, and a wrong clock or a stray line must not add up to more waiting
// than the job has run.
type slotWaitReader struct {
	path  string
	alive func(pid int) bool
	mu    sync.Mutex
	start time.Time
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
	add := func(from, to time.Time, live bool) {
		if from.Before(r.start) {
			from = r.start
		}
		if to.After(now) {
			to = now
		}
		if to.After(from) {
			spans = append(spans, waitSpan{from: from, to: to, live: live})
		}
	}
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
			add(from, at, false)
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
		add(from, end, live)
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
