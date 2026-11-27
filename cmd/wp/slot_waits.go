package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// slotFreeTries and slotFreeRetry bound how hard a holder tries to note that
// it let its slot go: until it does, or its process ends, the runner counts
// the job as holding one.
var (
	slotFreeTries = 3
	slotFreeRetry = 100 * time.Millisecond
)

// noteSlotWait appends "wait <unix-ms> <pid> <name>" to the waits file, if
// there is one, and returns what ends the wait and what notes the release
// (WP-1915: the runner counts an agent job as heavy only while one of its
// processes holds a slot). ended(true) appends "got"; ended(false), for a
// wait that took no slot, appends "got" and "free" in one write, so no line
// lost between them can leave it reading as a hold. free appends "free" when
// the holder lets the slot go, trying again a few times, since a hold it
// can't note the end of counts until its process ends. A holder that dies
// writes no "free"; the reader sees its process gone. A line that can't be
// written is dropped, and what that costs is said: the wait itself, and the
// release, go on regardless.
func noteSlotWait(name string) (ended func(held bool), free func()) {
	path := os.Getenv(slotWaitsEnv)
	if path == "" {
		return func(bool) {}, func() {}
	}
	write := func(what ...string) error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 G703 -- see above
		if err != nil {
			return err
		}
		now, pid := time.Now().UnixMilli(), os.Getpid()
		var b strings.Builder
		for _, w := range what {
			fmt.Fprintf(&b, "%s %d %d %s\n", w, now, pid, name)
		}
		_, err = f.WriteString(b.String())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	say := func(err error, format string) {
		_, _ = fmt.Fprintf(slotWaitWarn, "wp: "+format+": %v\n", name, err)
	}
	warned := false
	warn := func(err error, format string) {
		if err != nil && !warned {
			warned = true
			say(err, format)
		}
	}
	warn(write("wait"), "this wait for a %q slot couldn't be noted for the runner, so it counts against the job's time")
	var once sync.Once
	ended = func(held bool) {
		if held {
			warn(write("got"), "couldn't note for the runner that this process took a %q slot, so it counts its time from here as waiting while the process lives, and the job as holding no slot")
			return
		}
		once.Do(func() {}) // nothing to let go
		warn(write("got", "free"), "couldn't note for the runner that this wait for a %q slot ended, so it counts its time from here as waiting while the process lives")
	}
	free = func() {
		once.Do(func() {
			err := write("free")
			for i := 1; i < slotFreeTries && err != nil && !errors.Is(err, fs.ErrNotExist); i++ {
				time.Sleep(slotFreeRetry)
				err = write("free")
			}
			// Gone: the job ended, and no runner counts it any more.
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				say(err, "couldn't note for the runner that this process let its %q slot go, so it counts the job as holding one until this process ends")
			}
		})
	}
	return ended, free
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
