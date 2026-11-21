package main

import (
	"strconv"
	"strings"
	"time"
)

// A PreToolUse hook that Claude Code has already cancelled fails open and its
// output is discarded. A note that is "once only" must not be marked shown by
// such a run, or it is lost for good (WP-1398): hookLate says whether this
// process has lived so long that it is probably too late.
//
// A late run still tells (a cancelled run's output is discarded at no cost; one
// that was not cancelled delivers it) but marks the note shown only on its
// lateTellsMax-th late try, so a note is delivered at least once unless every
// one of three late runs is cancelled. The loss that remains is a cancel
// landing between the mark and process exit; it cannot be closed without a
// delivery acknowledgement from Claude Code.

// hookLateAfter is how far into its timeout a hook is presumed cancelled:
// three quarters, so a 2 s hook is late after 1.5 s.
func hookLateAfter(timeoutSeconds int) time.Duration {
	return time.Duration(timeoutSeconds) * time.Second * 3 / 4
}

// startedAt is the fallback age: when this package initialised. Process
// start itself (runtime, imports) is before it.
var startedAt = time.Now()

// lateTellsMax is how many late runs tell a note before one marks it shown.
const lateTellsMax = 3

// procAgeFunc reads the age from the OS; a variable so a test can make it fail.
var procAgeFunc = procAge

// osProcessAge is the OS's age for this process, or time since init if the OS
// does not say.
func osProcessAge() time.Duration {
	if d, ok := procAgeFunc(); ok {
		return d
	}
	return time.Since(startedAt)
}

// processAge is how long this process has lived. A variable so tests set it.
var processAge = osProcessAge

// hookTimeLeft is how long remains before limit (the late point), 0 if past:
// the longest a lock wait may take without carrying the run past it.
func hookTimeLeft(limit time.Duration) time.Duration {
	if d := limit - processAge(); d > 0 {
		return d
	}
	return 0
}

// hookLockFloor is the least a lock wait before the output takes, even past
// the late point: a lock held for a moment (a peer's short write) is waited
// out, where one failed try would drop the work.
//
// A var so a test can widen it: on a slow runner a fixed 50 ms races the
// goroutine that releases the lock.
var hookLockFloor = 50 * time.Millisecond

// hookLockWait is how long a lock wait before the output may take: the time
// left before limit (the late point), but at least hookLockFloor.
func hookLockWait(limit time.Duration) time.Duration {
	return max(hookTimeLeft(limit), hookLockFloor)
}

// hookLate reports whether this process has already lived longer than limit,
// measured from real process start where the OS says (Linux, macOS), so slow
// startup under load counts.
func hookLate(limit time.Duration) bool { return processAge() > limit }

// procClockTicks is USER_HZ, the unit of /proc/self/stat's times: 100 on every
// Linux platform Go supports.
const procClockTicks = 100

// ageFromProc computes the age from the text of /proc/self/stat and
// /proc/uptime. The comm field (2) is in parentheses and may hold spaces and
// parentheses, so fields are counted from the last ")".
func ageFromProc(stat, uptime string) (time.Duration, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(stat[i+1:]) // f[0] is field 3 (state)
	const startField = 22
	if len(f) < startField-2 {
		return 0, false
	}
	ticks, err := strconv.ParseUint(f[startField-3], 10, 64)
	if err != nil {
		return 0, false
	}
	u := strings.Fields(uptime)
	if len(u) == 0 {
		return 0, false
	}
	secs, err := strconv.ParseFloat(u[0], 64)
	if err != nil {
		return 0, false
	}
	age := time.Duration((secs - float64(ticks)/procClockTicks) * float64(time.Second))
	if age < 0 {
		return 0, false
	}
	return age, true
}

// hookCountWait is the longest a counter write after the hook's output may
// wait for its lock: what is left before the late point, at most 500 ms, and
// 0 (one try) once late. The output is already written, so the wait only
// matters if it carries the run past the timeout, which discards it. A
// counter lost to a held lock is acceptable: it is a statistic.
func hookCountWait(timeoutSeconds int) time.Duration {
	return min(hookTimeLeft(hookLateAfter(timeoutSeconds)), 500*time.Millisecond)
}
