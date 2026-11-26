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

// slotWaitsEnv names the file an agent job's commands note their slot waits in.
const slotWaitsEnv = "WAYPOINT_SLOT_WAITS"

// slotWaitWarn is where a wait that can't be noted is said.
var slotWaitWarn io.Writer = os.Stderr

// noteSlotWait appends "wait <unix-ms> <pid> <name>" to the waits file, if
// there is one, and returns what appends the matching "got" line, and what
// appends "free" when the holder lets the slot go (WP-1915: the runner counts
// an agent job as heavy only while one of its processes holds a slot). A
// holder that dies writes no "free" line; the reader sees its process gone.
// A line that can't be written is dropped, and said once: the wait itself
// goes on regardless, and counts as the job's work.
func noteSlotWait(name string) (got, free func()) {
	path := os.Getenv(slotWaitsEnv)
	if path == "" {
		return func() {}, func() {}
	}
	warned := false
	note := func(what string) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 G703 -- see above
		if err == nil {
			_, err = fmt.Fprintf(f, "%s %d %d %s\n", what, time.Now().UnixMilli(), os.Getpid(), name)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil && !warned {
			warned = true
			_, _ = fmt.Fprintf(slotWaitWarn, "wp: this wait for a %q slot couldn't be noted for the runner, so it counts against the job's time: %v\n", name, err)
		}
	}
	note("wait")
	var once sync.Once
	return func() { note("got") }, func() { once.Do(func() { note("free") }) }
}

// slotWaitReader reads a waits file: "wait", "got" and "free" lines. It keeps when it first saw a waiting
// process gone, which is when that wait is taken to have ended: kept per
// wait, not per pid, since a later process may get the same pid. For the
// same reason it keeps when each waiter's process started, as first seen: a
// live pid whose process started at another time is not the waiter. The job's
// clock reads it from its watcher and from whoever ends the job, so read
// holds mu. A wait counts only between start (the job's; zero: any time)
// and now, whatever its lines say: the file is the job's own commands' to
// write, and a wrong clock or a stray line must not add up to more waiting
// than the job has run.
type slotWaitReader struct {
	path    string
	alive   func(pid int) bool
	started func(pid int) string
	mu      sync.Mutex
	start   time.Time
	dead    map[deadWait]time.Time
	seen    map[deadWait]string
}

func newSlotWaitReader(path string) *slotWaitReader {
	return &slotWaitReader{path: path, alive: slotPidAlive, started: procStarted, dead: map[deadWait]time.Time{}, seen: map[deadWait]string{}}
}

// procStarted is the kernel's start time of process pid (clock ticks since
// boot, from /proc/PID/stat), or "" where there is no /proc or no such
// process. With the pid it names one process.
func procStarted(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

// deadWait names an open wait; the reader keeps what it saw of its process.
type deadWait struct {
	pid  int
	name string
	from int64
}

type waitSpan struct {
	from, to time.Time
	live     bool
}

// read returns how long slot waits took in all (several commands may wait at
// once: time they overlap counts once) and, when a live wait is open, when
// the unbroken stretch of waiting it belongs to began; zero otherwise.
func (r *slotWaitReader) read(now time.Time) (total time.Duration, waitingSince time.Time) {
	total, waitingSince, _ = r.scan(now)
	return total, waitingSince
}

// scan is read and also says whether a live process of the job holds a slot
// now: it has a "got" line with no "free" after it, and is still the process
// that wrote it (a holder that died or was killed writes no "free").
func (r *slotWaitReader) scan(now time.Time) (total time.Duration, waitingSince time.Time, holding bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := os.ReadFile(r.path)
	if err != nil {
		return 0, time.Time{}, false
	}
	type key struct {
		pid  int
		name string
	}
	open := map[key]time.Time{}
	held := map[key]time.Time{}
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
		if len(f) != 4 || (f[0] != "wait" && f[0] != "got" && f[0] != "free") {
			continue
		}
		ms, err1 := strconv.ParseInt(f[1], 10, 64)
		pid, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || pid <= 0 {
			continue
		}
		at, k := time.UnixMilli(ms), key{pid, f[3]}
		switch f[0] {
		case "wait":
			open[k] = at
			delete(held, k)
		case "got":
			held[k] = at
			if from, ok := open[k]; ok {
				delete(open, k)
				add(from, at, false)
			}
		case "free":
			delete(held, k)
		}
	}
	for k, from := range held {
		if !r.gone(k.pid, k.name, from) {
			holding = true
		}
	}
	for k, from := range open {
		end, live := now, true
		d := deadWait{k.pid, k.name, from.UnixMilli()}
		if r.gone(k.pid, k.name, from) {
			live = false
			if _, ok := r.dead[d]; !ok {
				r.dead[d] = now
			}
			end = r.dead[d]
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
	return total, waitingSince, holding
}

// gone says whether the process that wrote the line at from is gone: its pid
// is dead, or is another process's now (a live pid whose start time differs
// from the one first seen). Called with mu held.
func (r *slotWaitReader) gone(pid int, name string, from time.Time) bool {
	if !r.alive(pid) {
		return true
	}
	d := deadWait{pid, name, from.UnixMilli()}
	at := r.started(pid)
	if was, ok := r.seen[d]; !ok {
		r.seen[d] = at
	} else if was != "" && at != was {
		return true
	}
	return false
}
