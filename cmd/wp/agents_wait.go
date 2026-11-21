package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// wp agents wait (WP-1443): one command for a coordinator subagent to block
// on while agents it started finish. Claude Code asks a subagent for its
// report the moment it ends its turn, unless a background Bash of its own is
// still running; run in the background, this keeps it from asking, and its
// exit wakes the agent once, when everything is in.
//
// It reads the live table the SubagentStart and SubagentStop hooks keep
// (gate.go); nothing is polled over the network.
//
// Edge cases, decided:
//   - An agent that is resumed later is live again, but only the runs live
//     when the command started are waited on: a run is over when the agent is
//     gone from the table or its run (the start time) is a different one.
//   - A child whose SubagentStop was lost stays in the table until the
//     3-hour TTL (gateLiveTTL); the wait ends at --timeout, listing it.
//   - No live children: exit 0 at once, "no agents of yours are running".
//   - Hooks not installed or logged out: the table is empty, which reads as
//     "none running", so the command is only as good as the hooks.

const (
	// 9 minutes, so a call made in the foreground fits in a Bash call of
	// 600000 ms, the longest there is; calling it again continues the wait.
	agentsWaitDefault = 9 * time.Minute
)

var reWaitCall = regexp.MustCompile(`(^|[\s;&|(/])wp\s+agents\s+wait\b`)

// claimWait records, from the guard hook, that a Bash call is about to run
// `wp agents wait`, and which agent made it: the command cannot tell, since a
// subagent's shell has its session's id but not its own. It is picked up by
// that command within waitClaimTTL.
func (c *cli) claimWait(in hookEventInput) {
	if in.ToolName != "Bash" || in.SessionID == "" || !strings.Contains(string(in.ToolInput), "agents") {
		return
	}
	var ti struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &ti) != nil || !reWaitCall.MatchString(ti.Command) {
		return
	}
	_ = c.updateStateWithin(hookCountWait(hookGuardTimeout), func(s *State) {
		s.pruneAgents(time.Now())
		s.WaitClaims = append(s.WaitClaims, waitClaim{Session: in.SessionID, Agent: in.AgentID, At: time.Now()})
	})
}

// takeWaitClaim removes and returns the oldest claim of session: which agent
// ("" for the main thread) made the call that runs this command. found is
// false when there is none (the guard hook isn't installed, or didn't run).
func (c *cli) takeWaitClaim(session string) (agent string, found bool) {
	_ = c.updateStateWithin(2*time.Second, func(s *State) {
		now := time.Now()
		s.pruneAgents(now)
		for i, w := range s.WaitClaims {
			if w.Session == session {
				agent, found = w.Agent, true
				s.WaitClaims = append(s.WaitClaims[:i:i], s.WaitClaims[i+1:]...)
				return
			}
		}
	})
	return agent, found
}

// agentSession is the Claude Code session this command runs in.
func agentSession() string {
	for _, k := range []string{"WAYPOINT_SESSION", "CLAUDE_CODE_SESSION_ID"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// waitTarget is one run waited on.
type waitTarget struct {
	ID    string
	Type  string
	Since time.Time
}

// waitedRun is one run that ended.
type waitedRun struct {
	waitTarget
	Ran time.Duration
}

// waitDeps are what the wait takes from outside, so a test needs no time.
type waitDeps struct {
	now   func() time.Time
	sleep func(time.Duration)
	live  func() (map[string]liveAgent, bool) // the live agents of the session; !ok: not readable now
	seen  func()                              // called each poll that finds one waiting agent still pending; may be nil
}

// waitForRuns blocks until every target's run is over or timeout passes.
func waitForRuns(targets []waitTarget, timeout, poll time.Duration, d waitDeps) (done []waitedRun, pending []waitTarget) {
	deadline := d.now().Add(timeout)
	pending = append(pending, targets...)
	for {
		if live, ok := d.live(); ok {
			var still []waitTarget
			waiting := false
			for _, t := range pending {
				if a, isLive := live[t.ID]; isLive && a.Since.Equal(t.Since) {
					waiting = waiting || a.Waiting
					still = append(still, t)
					continue
				}
				done = append(done, waitedRun{t, max(d.now().Sub(t.Since), 0)})
			}
			pending = still
			if waiting && d.seen != nil {
				d.seen()
			}
		}
		if len(pending) == 0 || !d.now().Before(deadline) {
			return done, pending
		}
		d.sleep(min(poll, deadline.Sub(d.now())))
	}
}

func cmdAgentsWait(c *cli, args []string) error {
	fs := c.flags()
	timeout := fs.Duration("timeout", agentsWaitDefault, "give up after this long (calling the command again continues the wait)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	session := agentSession()
	now := time.Now()
	st := c.loadState()
	var targets []waitTarget
	var gone []string
	if len(pos) > 0 {
		for _, want := range pos {
			t, ok := findLiveAgent(st, session, want, now)
			if !ok {
				gone = append(gone, want)
				continue
			}
			targets = append(targets, t)
		}
	} else {
		if session == "" {
			return c.usage("no Claude Code session here (CLAUDE_CODE_SESSION_ID isn't set): name the agents to wait for")
		}
		who, found := c.takeWaitClaim(session)
		if !found {
			// The guard hook records a claim for every call, the main
			// thread's too, so none means the hooks didn't see this one:
			// guessing would make a subagent wait on itself.
			c.println("wp: can't tell which agent is asking (the Waypoint hooks didn't see this call), so nothing was waited on: name the agents, `wp agents wait ID...`")
			return exitCode(2)
		}
		// A start the gate let through that is not live yet is a child too:
		// wait for each to show up (or its reservation to expire).
		st = c.settleStarts(session, who, st, *timeout)
		now = time.Now()
		for id, a := range st.childrenOf(session, who, now) {
			if id == who {
				continue // never wait for the caller itself
			}
			targets = append(targets, waitTarget{ID: id, Type: a.Type, Since: a.Since})
		}
	}
	for _, g := range gone {
		c.println(g + ": not running")
	}
	if len(targets) == 0 {
		if len(pos) == 0 {
			c.println("no agents of yours are running")
		}
		return nil
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Since.Before(targets[j].Since) })
	d := waitDeps{now: time.Now, sleep: time.Sleep, seen: func() { c.noteWaitingSeen(session) }, live: func() (map[string]liveAgent, bool) {
		var s State
		if c.dir == "" {
			s = c.state
		} else if readJSON(filepath.Join(c.dir, "state.json"), &s) != nil {
			return nil, false
		}
		if session == "" { // named ids with no session known: any
			all := map[string]liveAgent{}
			for sid := range s.Agents {
				for id, a := range s.liveIDs(sid, time.Now()) {
					all[id] = a
				}
			}
			return all, true
		}
		return s.liveIDs(session, time.Now()), true
	}}
	done, pending := waitForRuns(targets, *timeout, agentsWaitPoll, d)
	for _, r := range done {
		c.println(fmt.Sprintf("%s %s: ran %s", agentLabel(r.Type), r.ID, shortDuration(r.Ran)))
	}
	if len(pending) > 0 {
		var names []string
		for _, t := range pending {
			names = append(names, agentLabel(t.Type)+" "+t.ID)
		}
		return fmt.Errorf("timed out after %s with %d of %d still running: %s (call `wp agents wait` again to continue the wait)", *timeout, len(pending), len(targets), strings.Join(names, ", "))
	}
	return nil
}

// settleStarts waits, at most timeout, until agent's Agent calls have all
// turned into live agents or expired, and returns the state to read the
// children from.
func (c *cli) settleStarts(session, agent string, st State, timeout time.Duration) State {
	deadline := time.Now().Add(min(timeout, reserveTTL+agentsWaitPoll))
	for st.pendingStarts(session, agent, time.Now()) > 0 && time.Now().Before(deadline) {
		time.Sleep(agentsWaitPoll)
		var s State
		if c.dir == "" {
			s = c.state
		} else if readJSON(filepath.Join(c.dir, "state.json"), &s) != nil {
			break
		}
		st = s
	}
	return st
}

// agentLabel is an agent's type, "agent" when it has none.
func agentLabel(t string) string {
	if t == "" {
		return "agent"
	}
	return t
}

// findLiveAgent finds a live agent by id or a prefix of one: in session, or in
// any when session is empty.
func findLiveAgent(s State, session, want string, now time.Time) (waitTarget, bool) {
	var hit []waitTarget
	for sid := range s.Agents {
		if session != "" && sid != session {
			continue
		}
		for id, a := range s.liveIDs(sid, now) {
			if id == want {
				return waitTarget{id, a.Type, a.Since}, true
			}
			if len(want) >= 4 && strings.HasPrefix(id, want) {
				hit = append(hit, waitTarget{id, a.Type, a.Since})
			}
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	return waitTarget{}, false
}

// agentsWaitPoll is how often the wait re-reads the state (a variable so a
// test shortens it).
var agentsWaitPoll = 2 * time.Second
