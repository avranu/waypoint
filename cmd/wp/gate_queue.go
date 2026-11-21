package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The slot queue (WP-1443). Claude Code refuses an Agent call once its
// session tree runs CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS agents (20 unless
// set), with "Do not retry" in the text. PreToolUse hooks run before that
// check, so the gate holds the call until a slot is free: the agent never
// sees a refusal, and the call launches when one stops.
//
// The wait reads the live table without the state lock and takes the lock
// only to claim a slot. The table can be stale (a SubagentStop lost in a
// crash), so the wait is bounded and past the bound the call goes through
// with a note and no decision: Claude Code then arbitrates.

const (
	claudeSubagentLimit = 20              // Claude Code's default
	gateQueueBound      = 4 * time.Minute // the longest a start waits for a slot
	gateQueuePoll       = 1 * time.Second // how often the wait re-reads the state
	gateQueueClaimWait  = 2 * time.Second // the most a claim waits for the state lock
)

// subagentLimit is the number of agents Claude Code runs at once: the
// CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS variable when it is a positive integer
// (the process environment, else the "env" of ~/.claude/settings.json), else 20.
func subagentLimit() int {
	if n, err := strconv.Atoi(claudeEnvSetting("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS")); err == nil && n > 0 {
		return n
	}
	return claudeSubagentLimit
}

// claudeEnvSetting reads key from the process environment, falling back to
// the "env" object in ~/.claude/settings.json.
func claudeEnvSetting(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return settingsEnv(filepath.Join(home, ".claude", "settings.json"), key)
}

// gateWaits are the clock, the sleep and the parent check of the wait, so a
// test runs it without time passing.
type gateWaits struct {
	now   func() time.Time
	sleep func(time.Duration)
	gone  func() bool // the hook's parent (Claude Code) has gone: nobody is waiting for the answer
}

// gateWait is the waits a real hook uses.
var gateWait = func() gateWaits {
	ppid := os.Getppid()
	return gateWaits{now: time.Now, sleep: time.Sleep, gone: func() bool { return os.Getppid() != ppid }}
}

// gateBound is the longest a start waits (a variable so a test shortens it).
var gateBound = gateQueueBound

// queueResult says how the wait ended.
type queueResult struct {
	waited    time.Duration
	live      int  // the agents counted at the end
	timedOut  bool // the bound passed with no slot free
	abandoned bool // Claude Code is gone
}

// slotQueue holds a call until live agents (and calls admitted but not yet
// started) are fewer than limit.
type slotQueue struct {
	limit int
	bound time.Duration
	poll  time.Duration
	w     gateWaits
	count func() (n int, ok bool) // a read without the lock; !ok: not readable now
	admit func() (n int, ok bool) // under the lock: claim a slot if one is free
}

func (q slotQueue) run() queueResult {
	start := q.w.now()
	n := 0
	for {
		if q.w.gone() {
			return queueResult{waited: q.w.now().Sub(start), live: n, abandoned: true}
		}
		if c, ok := q.count(); ok {
			n = c
			if c < q.limit {
				a, ok := q.admit()
				if ok {
					return queueResult{waited: q.w.now().Sub(start), live: a}
				}
				n = a
			}
		}
		if waited := q.w.now().Sub(start); waited >= q.bound {
			return queueResult{waited: waited, live: n, timedOut: true}
		}
		q.w.sleep(q.poll)
	}
}

// liveCount reads the agents counted against the limit for session without
// the state lock (the file is replaced whole, never edited in place). !ok when
// it can't be read: an unreadable table must not release a waiter.
func (c *cli) liveCount(session string, now time.Time) (int, bool) {
	var s State
	if c.dir == "" {
		s = c.state
	} else if err := readJSON(filepath.Join(c.dir, "state.json"), &s); err != nil {
		return 0, false
	}
	return s.slotsUsed(session, now), true
}

// slotsUsed is the live agents of session plus the Agent calls admitted that
// have not started yet.
func (s *State) slotsUsed(session string, now time.Time) int {
	n := 0
	for _, a := range s.liveIDs(session, now) {
		if !a.Waiting { // whether Claude Code holds a slot for it is not known: not counted
			n++
		}
	}
	for _, r := range s.Reserved[session] {
		if now.Sub(r.At) <= reserveTTL {
			n++
		}
	}
	return n
}

// admitSlot claims a slot for the call when fewer than limit are used; under
// the state lock. A lock it can't take admits the call: the gate fails open.
func (c *cli) admitSlot(session string, res reservation, limit int) (int, bool) {
	n, admitted := 0, true
	err := c.updateStateWithin(gateQueueClaimWait, func(s *State) {
		now := time.Now()
		s.pruneAgents(now)
		n = s.slotsUsed(session, now)
		for _, r := range s.Reserved[session] {
			if res.ID != "" && r.ID == res.ID {
				return // this call is in already (the hook ran twice)
			}
		}
		if n >= limit {
			admitted = false
			return
		}
		if s.Reserved == nil {
			s.Reserved = map[string][]reservation{}
		}
		res.At = now
		s.Reserved[session] = append(s.Reserved[session], res)
	})
	if err != nil {
		return n, true
	}
	return n, admitted
}

// waitForSlot holds an Agent call of session until a slot is free.
func (c *cli) waitForSlot(session string, res reservation, limit int) queueResult {
	w := gateWait()
	q := slotQueue{
		limit: limit, bound: gateBound, poll: gateQueuePoll, w: w,
		count: func() (int, bool) { return c.liveCount(session, w.now()) },
		admit: func() (int, bool) { return c.admitSlot(session, res, limit) },
	}
	return q.run()
}

// queueTimedOutText is what the agent is told when its start waited the whole
// bound and went through undecided.
func queueTimedOutText(r queueResult, limit int) string {
	n := max(r.live, limit)
	return fmt.Sprintf("%d agents are running, the most Claude Code runs at once; this start waited %s. If it is refused, start it when the next one finishes, which you are told about.",
		n, shortDuration(r.waited))
}

// shortDuration is d as "4m" or "45s": whole minutes when it is one.
func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}
