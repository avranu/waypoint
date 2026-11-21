package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// What the hooks record about the agents a session runs, beyond the live
// table of gate.go (WP-1443): who started each one, the Agent calls the gate
// has let through that SubagentStart has not answered yet, the claim a call
// to `wp agents wait` leaves, and which once-per-run notes were told.

const (
	reserveTTL   = 30 * time.Second // an admitted Agent call whose SubagentStart never came (refused by Claude Code, or by another hook)
	waitClaimTTL = 2 * time.Minute  // a `wp agents wait` call whose command never ran
)

// agentParent says who started a background agent: the agent id of the
// caller, empty for the main thread (or a caller we weren't told).
type agentParent struct {
	Parent string    `json:"parent,omitempty"`
	Mode   string    `json:"mode,omitempty"` // modeBg or modeFg, from its Agent call
	At     time.Time `json:"at"`
	// Taken: the reservation of the Agent call that made this agent is
	// accounted for, so a later SubagentStart of it takes none.
	Taken bool `json:"taken,omitempty"`
}

// reservation is an Agent call the gate let through: it counts against the
// Claude Code limit until its SubagentStart arrives, so three calls of one
// message are not all admitted into the last free slot.
type reservation struct {
	ID     string    `json:"id,omitempty"` // the tool_use_id
	Mode   string    `json:"mode,omitempty"`
	Parent string    `json:"parent,omitempty"` // the agent that made the call ("" the main thread)
	Type   string    `json:"type,omitempty"`   // its subagent_type
	At     time.Time `json:"at"`
}

// provisionalTTL is how long an agent made by its Agent call's PostToolUse
// waits for its SubagentStart before it is taken for one that never came (or
// one that was over before the call returned).
const provisionalTTL = 2 * time.Minute

// waitGrace is how long an agent that stopped with work running is still
// taken as running after that work is gone: the wake needs time to arrive.
const waitGrace = 30 * time.Second

// bgTask is work an agent started that was still running when it stopped.
type bgTask struct {
	ID   string `json:"id"`
	Type string `json:"type,omitempty"` // "subagent", a shell's kind; never "monitor"
}

// runningTasks reads the running tasks of SubagentStop's background_tasks
// that keep agent open. The list is the whole session's, the stopping agent
// itself included, so it is cut down to what can be this agent's:
//   - not a Monitor, which does not keep an agent open;
//   - not the agent itself;
//   - an agent only when this one started it (its parent record says so);
//   - not a `wp agents wait`, which waits for agents and would be waited for
//     in turn.
//
// A shell cannot be told from another agent's; it counts while its output
// file is held open, which ends it either way.
func (s *State) runningTasks(session, agent string, raw json.RawMessage) []bgTask {
	var l []struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Status  string `json:"status"`
		Command string `json:"command"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &l) != nil {
		return nil
	}
	var out []bgTask
	for _, t := range l {
		switch {
		case t.ID == "" || t.ID == agent || t.Status != "running" || t.Type == "monitor":
		case t.Type == "subagent":
			if p, ok := s.AgentParents[session][t.ID]; ok && p.Parent == agent {
				out = append(out, bgTask{ID: t.ID, Type: t.Type})
			}
		case runsAgentsWait(t.Command):
		default:
			out = append(out, bgTask{ID: t.ID, Type: t.Type})
		}
	}
	return out
}

// taskHeld says whether the background command of session with this id still
// runs: its task output file is held open by a process. A variable so tests
// set it.
var taskHeld = func(session, id string) bool {
	files, _ := filepath.Glob(filepath.Join(os.TempDir(), "claude-*", "*", session, "tasks", id+".output"))
	for _, f := range files {
		if heldOpen(f) {
			return true
		}
	}
	return false
}

// agentRunning says whether the agent's run is not over: it is not waiting,
// or it is within waitGrace of stopping, or something it waits for still
// runs (an agent of the session that is itself running, or a command whose
// output file is held open).
func (s *State) agentRunning(session string, a liveAgent, now time.Time, depth int) bool {
	if !a.Waiting || s.tasksRunning(session, a, now, depth) {
		return true
	}
	// Nothing it waits for runs: the wake is on its way. The grace counts
	// from the stop, or from the last time a waiter saw the work running.
	last := a.WaitAt
	if a.Seen.After(last) {
		last = a.Seen
	}
	return now.Sub(last) <= waitGrace
}

// tasksRunning says whether something the waiting agent waits for runs.
func (s *State) tasksRunning(session string, a liveAgent, now time.Time, depth int) bool {
	if depth > 3 {
		return false
	}
	for _, t := range a.Tasks {
		if t.Type == "subagent" {
			if b, ok := s.Agents[session][t.ID]; ok && s.agentRunning(session, b, now, depth+1) {
				return true
			}
			continue
		}
		if taskHeld(session, t.ID) {
			return true
		}
	}
	return false
}

// noteWaitingSeen marks the waiting agents of session whose work still runs as
// seen running now, so the grace before one is given up counts from the end of
// that work and not from the stop. A waiter calls it as it polls.
func (c *cli) noteWaitingSeen(session string) {
	_ = c.updateStateWithin(time.Second, func(s *State) {
		now := time.Now()
		for id, a := range s.Agents[session] {
			if a.Waiting && s.tasksRunning(session, a, now, 0) {
				a.Seen = now
				s.Agents[session][id] = a
			}
		}
	})
}

// takeReservation removes and returns the oldest unexpired reservation of
// session that answers an agent: the one with this tool_use_id when given,
// else the oldest of the parent (when known) and type.
func (s *State) takeReservation(session, toolUseID, parent string, parentKnown bool, typ string) (reservation, bool) {
	l := s.Reserved[session]
	pick := -1
	for i, r := range l {
		switch {
		case toolUseID != "":
			if r.ID == toolUseID {
				pick = i
			}
		case (typ == "" || r.Type == "" || r.Type == typ) && (!parentKnown || r.Parent == parent):
			pick = i
		}
		if pick >= 0 {
			break
		}
	}
	if pick < 0 {
		return reservation{}, false
	}
	r := l[pick]
	rest := append(l[:pick:pick], l[pick+1:]...)
	if len(rest) == 0 {
		delete(s.Reserved, session)
	} else {
		s.Reserved[session] = rest
	}
	return r, true
}

// pendingStarts counts the Agent calls of parent (empty: the main thread) in
// session that the gate let through and whose agent has not shown up yet.
func (s *State) pendingStarts(session, parent string, now time.Time) int {
	n := 0
	for _, r := range s.Reserved[session] {
		if r.Parent == parent && now.Sub(r.At) <= reserveTTL {
			n++
		}
	}
	return n
}

// waitClaim is what the guard hook records when a Bash call runs `wp agents
// wait`: which agent (empty: the main thread) of which session made it.
type waitClaim struct {
	Session string    `json:"session"`
	Agent   string    `json:"agent,omitempty"`
	Command string    `json:"command,omitempty"` // the Bash command, to tell claims of one session apart
	At      time.Time `json:"at"`
}

// pruneAgents forgets what has outlived what could still be true.
func (s *State) pruneAgents(now time.Time) {
	for sid, m := range s.Agents { // forget what never stopped
		for id, a := range m {
			if now.Sub(a.Since) > gateLiveTTL || (a.Provisional && now.Sub(a.Since) > provisionalTTL) {
				delete(m, id)
			}
		}
		for id, a := range m { // what it waited for is gone: it will not be woken
			if !s.agentRunning(sid, a, now, 0) {
				delete(m, id)
			}
		}
		if len(m) == 0 {
			delete(s.Agents, sid)
		}
	}
	for sid, m := range s.AgentParents {
		for id, p := range m {
			if now.Sub(p.At) > gateLiveTTL {
				delete(m, id)
			}
		}
		if len(m) == 0 {
			delete(s.AgentParents, sid)
		}
	}
	for sid, l := range s.Reserved {
		keep := l[:0]
		for _, r := range l {
			if now.Sub(r.At) <= reserveTTL {
				keep = append(keep, r)
			}
		}
		if len(keep) == 0 {
			delete(s.Reserved, sid)
		} else {
			s.Reserved[sid] = keep
		}
	}
	keep := s.WaitClaims[:0]
	for _, w := range s.WaitClaims {
		if now.Sub(w.At) <= waitClaimTTL {
			keep = append(keep, w)
		}
	}
	s.WaitClaims = keep
	for k, t := range s.Told {
		if now.Sub(t) > gateLiveTTL {
			delete(s.Told, k)
		}
	}
}

// liveIDs are the agents of session that SubagentStart listed and no
// SubagentStop has removed, within the TTL of a stop that never came.
func (s *State) liveIDs(session string, now time.Time) map[string]liveAgent {
	out := map[string]liveAgent{}
	for id, a := range s.Agents[session] {
		if now.Sub(a.Since) <= gateLiveTTL && (!a.Provisional || now.Sub(a.Since) <= provisionalTTL) && s.agentRunning(session, a, now, 0) {
			out[id] = a
		}
	}
	return out
}

// trackAgentParent records, on PostToolUse(Agent), which agent started the
// background agent the call launched. SubagentStart may come before or after
// it; this table is separate from the live one so neither order lists an agent
// that has already stopped.
func (c *cli) trackAgentParent(in hookEventInput) {
	if in.SessionID == "" || len(in.ToolResponse) == 0 {
		return
	}
	var r struct {
		AgentID string `json:"agentId"`
		Status  string `json:"status"`
	}
	if json.Unmarshal(in.ToolResponse, &r) != nil || r.AgentID == "" {
		return
	}
	var ti struct {
		Bg   bool   `json:"run_in_background"`
		Type string `json:"subagent_type"`
	}
	_ = json.Unmarshal(in.ToolInput, &ti)
	async := ti.Bg || r.Status == "async_launched"
	mode := startMode(async)
	_ = c.updateState(func(s *State) {
		if s.AgentParents == nil {
			s.AgentParents = map[string]map[string]agentParent{}
		}
		if s.AgentParents[in.SessionID] == nil {
			s.AgentParents[in.SessionID] = map[string]agentParent{}
		}
		// The call is over for its reservation, which is dropped by its id;
		// the agent's start then takes none (Taken).
		ap := agentParent{Parent: in.AgentID, Mode: mode, At: time.Now(), Taken: s.AgentParents[in.SessionID][r.AgentID].Taken}
		if in.ToolUseID != "" {
			if _, ok := s.takeReservation(in.SessionID, in.ToolUseID, "", false, ""); ok {
				ap.Taken = true
			}
		}
		s.AgentParents[in.SessionID][r.AgentID] = ap
		if a, ok := s.Agents[in.SessionID][r.AgentID]; ok {
			a.Mode = mode // the call is a better witness than the order of starts
			s.Agents[in.SessionID][r.AgentID] = a
		} else if r.Status == "async_launched" {
			// Launched, and its SubagentStart not here yet: it exists now
			// (a foreground call returns only when its agent is over, so
			// only an async launch makes one).
			if s.Agents == nil {
				s.Agents = map[string]map[string]liveAgent{}
			}
			if s.Agents[in.SessionID] == nil {
				s.Agents[in.SessionID] = map[string]liveAgent{}
			}
			s.Agents[in.SessionID][r.AgentID] = liveAgent{Type: ti.Type, Since: time.Now(), Mode: mode, Provisional: true}
		}
		s.pruneAgents(time.Now())
	})
}

// forgetSessionAgents drops a session's agents, from SessionEnd and from a
// SessionStart that is a new process (a resumed session has none running).
func (c *cli) forgetSessionAgents(sid string) {
	if sid == "" {
		return
	}
	_ = c.updateStateWithin(time.Second, func(s *State) { // SessionEnd has 2 s
		delete(s.Agents, sid)
		delete(s.AgentParents, sid)
		delete(s.Reserved, sid)
	})
}

// trackAgentsLocally keeps the agent tables for one hook event, whether or not
// the machine is logged in.
func (c *cli) trackAgentsLocally(in hookEventInput) {
	switch in.HookEventName {
	case "SessionStart":
		if in.Source != "compact" { // a new process: nothing of the session's still runs
			c.forgetSessionAgents(in.SessionID)
		}
	case "SessionEnd":
		c.forgetSessionAgents(in.SessionID)
	case "SubagentStart", "SubagentStop":
		c.trackSubagent(in)
	case "PostToolUse":
		if in.ToolName == "Agent" || in.ToolName == "Task" {
			c.trackAgentParent(in)
		}
	}
}

// tellOnce says whether the note of this kind has not been told to this agent
// in its current run, and marks it told. A run is from its SubagentStart (a
// resumed agent starts a new one). It waits at most wait for the lock and
// tells nothing when it cannot take it: a note missed is better than a hook
// that runs late.
func (c *cli) tellOnce(kind, session, agent string, wait time.Duration) bool {
	if session == "" || agent == "" {
		return false
	}
	key := kind + "|" + session + "|" + agent
	tell := false
	_ = c.updateStateWithin(wait, func(s *State) {
		now := time.Now()
		since := s.Agents[session][agent].Since
		if t, ok := s.Told[key]; ok && !t.Before(since) {
			return
		}
		if s.Told == nil {
			s.Told = map[string]time.Time{}
		}
		s.Told[key] = now
		s.pruneAgents(now)
		tell = true
	})
	return tell
}

// childrenOf are the live agents session's agent (empty: the main thread)
// started, by id. An agent whose starter was never recorded counts as the main
// thread's.
func (s *State) childrenOf(session, agent string, now time.Time) map[string]liveAgent {
	out := map[string]liveAgent{}
	for id, a := range s.liveIDs(session, now) {
		if s.AgentParents[session][id].Parent == agent {
			out[id] = a
		}
	}
	return out
}

// How an agent was started, which decides what it may be told about waiting:
// Claude Code keeps a background agent open while a background command of its
// own runs, and wakes it when that exits; it reaps a foreground agent's shells
// when the agent gives its final answer (WP-1443).
const (
	modeBg = "bg"
	modeFg = "fg"
)

func startMode(background bool) string {
	if background {
		return modeBg
	}
	return modeFg
}

// caller is who made a tool call, as far as what it may be told about waiting.
type caller int

const (
	callerMain caller = iota // the main thread
	callerFg                 // a subagent started in the foreground, or one whose start isn't known
	callerBg                 // a subagent started in the background
)

// callerOf is the kind of the agent (empty: the main thread) of session. Not
// knowing how a subagent started reads as foreground: its advice never says to
// end the turn.
func (c *cli) callerOf(session, agent string) caller {
	if agent == "" {
		return callerMain
	}
	if c == nil {
		return callerFg
	}
	s := c.loadState()
	if s.Agents[session][agent].Mode == modeBg {
		return callerBg
	}
	return callerFg
}
