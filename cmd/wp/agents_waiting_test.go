package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func stopWith(session, id, tasks string) hookEventInput {
	return hookEventInput{SessionID: session, AgentID: id, HookEventName: "SubagentStop", BackgroundTasks: json.RawMessage(tasks)}
}

func ageWait(t *testing.T, c *cli, session, id string, by time.Duration) {
	t.Helper()
	if err := c.updateState(func(s *State) {
		a := s.Agents[session][id]
		a.WaitAt = a.WaitAt.Add(-by)
		s.Agents[session][id] = a
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWaitingAgents(t *testing.T) {
	held := taskHeld
	t.Cleanup(func() { taskHeld = held })
	taskHeld = func(session, id string) bool { return false }

	// the session's whole list: the boss itself, its child, a stranger's agent,
	// a monitor, and a `wp agents wait` of its own
	const tasks = `[{"id":"boss","type":"subagent","status":"running"},{"id":"kid1","type":"subagent","status":"running"},{"id":"alien","type":"subagent","status":"running"},{"id":"m1","type":"monitor","status":"running"},{"id":"w1","type":"shell","status":"running","command":"wp agents wait"}]`
	newC := func() *cli {
		c := &cli{dir: t.TempDir()}
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "boss", AgentType: "x", HookEventName: "SubagentStart"})
		c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "boss", ToolResponse: []byte(`{"agentId":"kid1"}`)})
		c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "other", ToolResponse: []byte(`{"agentId":"alien"}`)})
		return c
	}
	live := func(c *cli) map[string]liveAgent { s := c.loadState(); return s.liveIDs("s", time.Now()) }

	t.Run("a stop with background work running keeps the agent, waiting", func(t *testing.T) {
		c := newC()
		since := c.loadState().Agents["s"]["boss"].Since
		c.trackSubagent(stopWith("s", "boss", tasks))
		a, ok := live(c)["boss"]
		if !ok || !a.Waiting || len(a.Tasks) != 1 || a.Tasks[0].ID != "kid1" {
			t.Fatalf("not kept waiting on exactly its child (not itself, a stranger, a monitor, its own wait): %+v", a)
		}
		// the wake is the same run
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "boss", AgentType: "x", HookEventName: "SubagentStart"})
		a = live(c)["boss"]
		if a.Waiting || !a.Since.Equal(since) {
			t.Errorf("woken: %+v, since was %v", a, since)
		}
		// a stop with nothing running ends it
		c.trackSubagent(stopWith("s", "boss", `[]`))
		if _, ok := live(c)["boss"]; ok {
			t.Error("still there after the last stop")
		}
	})
	t.Run("only a monitor, or a field we can't read: over", func(t *testing.T) {
		for _, bt := range []string{`[{"id":"m1","type":"monitor","status":"running"}]`, `[{"id":"k","type":"subagent","status":"completed"}]`, ``, `null`} {
			c := newC()
			c.trackSubagent(stopWith("s", "boss", bt))
			if _, ok := live(c)["boss"]; ok {
				t.Errorf("%q: kept", bt)
			}
		}
	})
	t.Run("the gate does not count a waiting agent", func(t *testing.T) {
		c := newC()
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "other", AgentType: "x", HookEventName: "SubagentStart"})
		s := c.loadState()
		if n := s.slotsUsed("s", time.Now()); n != 2 {
			t.Fatalf("before: %d", n)
		}
		c.trackSubagent(stopWith("s", "boss", tasks))
		s = c.loadState()
		if n := s.slotsUsed("s", time.Now()); n != 1 {
			t.Errorf("waiting counted: %d", n)
		}
	})
	t.Run("it is over once nothing it waits for runs, after the grace", func(t *testing.T) {
		c := newC()
		c.trackSubagent(stopWith("s", "boss", tasks))
		ageWait(t, c, "s", "boss", 10*time.Second)
		if _, ok := live(c)["boss"]; !ok {
			t.Error("dropped within the grace")
		}
		ageWait(t, c, "s", "boss", time.Minute)
		if _, ok := live(c)["boss"]; ok {
			t.Error("kept with its child gone and the grace passed")
		}
	})
	t.Run("the grace counts from the last time its work was seen running", func(t *testing.T) {
		c := newC()
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid1", AgentType: "x", HookEventName: "SubagentStart"})
		c.trackSubagent(stopWith("s", "boss", tasks))
		ageWait(t, c, "s", "boss", time.Minute)
		c.noteWaitingSeen("s") // a waiter polls while the child runs
		c.trackSubagent(stopWith("s", "kid1", `[]`))
		if _, ok := live(c)["boss"]; !ok {
			t.Error("dropped at once when its work ended: the wake had no time")
		}
		if err := c.updateState(func(s *State) {
			a := s.Agents["s"]["boss"]
			a.Seen = a.Seen.Add(-time.Minute)
			s.Agents["s"]["boss"] = a
		}); err != nil {
			t.Fatal(err)
		}
		if _, ok := live(c)["boss"]; ok {
			t.Error("kept past the grace")
		}
	})
	t.Run("it is kept while its child agent runs, or its command's output is held", func(t *testing.T) {
		c := newC()
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid1", AgentType: "x", HookEventName: "SubagentStart"})
		c.trackSubagent(stopWith("s", "boss", tasks))
		ageWait(t, c, "s", "boss", time.Minute)
		if _, ok := live(c)["boss"]; !ok {
			t.Error("dropped while its child agent runs")
		}
		c.trackSubagent(stopWith("s", "kid1", `[]`))
		if _, ok := live(c)["boss"]; ok {
			t.Error("kept after its child stopped")
		}

		c = newC()
		c.trackSubagent(stopWith("s", "boss", `[{"id":"bsh1","type":"local_bash","status":"running"}]`))
		ageWait(t, c, "s", "boss", time.Minute)
		taskHeld = func(session, id string) bool { return session == "s" && id == "bsh1" }
		if _, ok := live(c)["boss"]; !ok {
			t.Error("dropped while its command runs")
		}
		taskHeld = func(string, string) bool { return false }
		if _, ok := live(c)["boss"]; ok {
			t.Error("kept after its command ended")
		}
	})
	t.Run("wp agents wait waits through the interim stop", func(t *testing.T) {
		old := agentsWaitPoll
		agentsWaitPoll = 10 * time.Millisecond
		t.Cleanup(func() { agentsWaitPoll = old })
		t.Setenv("WAYPOINT_SESSION", "s")
		var o, e bytes.Buffer
		c := &cli{dir: t.TempDir(), out: &o, err: &e}
		startAgent(c, "", "boss", true)
		startAgent(c, "boss", "kid1", true)
		c.trackSubagent(stopWith("s", "boss", tasks))
		ti, _ := json.Marshal(map[string]any{"command": "wp agents wait"})
		c.claimWait(hookEventInput{SessionID: "s", AgentID: "", ToolName: "Bash", ToolInput: ti})
		err := cmdAgentsWait(c, []string{"--timeout", "300ms"})
		if err == nil || !strings.Contains(err.Error(), "boss") {
			t.Fatalf("returned at the interim stop: %v %q", err, o.String())
		}
		c.claimWait(hookEventInput{SessionID: "s", AgentID: "", ToolName: "Bash", ToolInput: ti})
		go func() {
			time.Sleep(60 * time.Millisecond)
			c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "boss", AgentType: "x", HookEventName: "SubagentStart"})
			c.trackSubagent(stopWith("s", "boss", `[]`))
		}()
		if err := cmdAgentsWait(c, []string{"--timeout", "5s"}); err != nil || !strings.Contains(o.String(), "boss: ran") {
			t.Errorf("after the last stop: %v %q", err, o.String())
		}
	})
}

func TestProvisionalAgentsAndReservations(t *testing.T) {
	post := func(c *cli, who, id, tuid, typ string, status string) {
		ti, _ := json.Marshal(map[string]any{"subagent_type": typ})
		resp := `{"agentId":"` + id + `","status":"` + status + `"}`
		c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: who, ToolUseID: tuid, ToolInput: ti, ToolResponse: []byte(resp)})
	}
	t.Run("an async launch makes the agent live before its start, and the start adopts it", func(t *testing.T) {
		c := &cli{dir: t.TempDir()}
		post(c, "", "k1", "tu1", "worker", "async_launched")
		s := c.loadState()
		a, ok := s.liveIDs("s", time.Now())["k1"]
		if !ok || !a.Provisional || a.Type != "worker" || a.Mode != modeBg {
			t.Fatalf("not made: %+v", a)
		}
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "k1", AgentType: "worker", HookEventName: "SubagentStart"})
		b := c.loadState().Agents["s"]["k1"]
		if b.Provisional || !b.Since.Equal(a.Since) || b.Mode != modeBg {
			t.Errorf("adopted: %+v from %+v", b, a)
		}
	})
	t.Run("a call that returned when its agent was over makes none", func(t *testing.T) {
		c := &cli{dir: t.TempDir()}
		post(c, "", "k2", "tu2", "worker", "completed")
		if _, ok := c.loadState().Agents["s"]["k2"]; ok {
			t.Error("made an entry for a foreground call's agent")
		}
	})
	t.Run("a provisional entry whose start never came is dropped", func(t *testing.T) {
		c := &cli{dir: t.TempDir()}
		post(c, "", "k3", "tu3", "worker", "async_launched")
		s := c.loadState()
		if _, ok := s.liveIDs("s", time.Now().Add(provisionalTTL+time.Second))["k3"]; ok {
			t.Error("kept past the TTL")
		}
	})
	t.Run("the call's reservation is dropped by its tool_use_id", func(t *testing.T) {
		c := &cli{dir: t.TempDir()}
		c.admitSlot("s", reservation{ID: "tu4", Mode: modeBg, Type: "worker"}, 20)
		c.admitSlot("s", reservation{ID: "tu5", Mode: modeBg, Type: "worker"}, 20)
		post(c, "", "k4", "tu5", "worker", "async_launched")
		s := c.loadState()
		if l := s.Reserved["s"]; len(l) != 1 || l[0].ID != "tu4" {
			t.Errorf("reserved: %+v", l)
		}
	})
	t.Run("a start takes the oldest reservation of its parent and type", func(t *testing.T) {
		c := &cli{dir: t.TempDir()}
		// the start's parent is known from its Agent call's record
		c.admitSlot("s", reservation{ID: "r1", Mode: modeFg, Parent: "p1", Type: "worker"}, 20)
		c.admitSlot("s", reservation{ID: "r2", Mode: modeBg, Parent: "p2", Type: "worker"}, 20)
		c.admitSlot("s", reservation{ID: "r3", Mode: modeFg, Parent: "p2", Type: "other"}, 20)
		c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "p2", ToolResponse: []byte(`{"agentId":"x1"}`)}) // x1: p2's, mode fg by its call
		// the parent record has a mode of its own and wins; check the reservation taken
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "x1", AgentType: "worker", HookEventName: "SubagentStart"})
		s := c.loadState()
		var ids []string
		for _, r := range s.Reserved["s"] {
			ids = append(ids, r.ID)
		}
		if strings.Join(ids, ",") != "r1,r3" {
			t.Errorf("left %v, want r1,r3 (p2's worker taken)", ids)
		}
		// no parent known: the oldest of the type
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "x2", AgentType: "worker", HookEventName: "SubagentStart"})
		s = c.loadState()
		if l := s.Reserved["s"]; len(l) != 1 || l[0].ID != "r3" {
			t.Errorf("left %+v", l)
		}
		if got := s.Agents["s"]["x2"].Mode; got != modeFg {
			t.Errorf("mode from the reservation: %q", got)
		}
	})
	t.Run("reserved starts count as children for the no-op test and the wait", func(t *testing.T) {
		old := bgTaskRunning
		bgTaskRunning = func(string) bool { return false }
		t.Cleanup(func() { bgTaskRunning = old })
		c := &cli{dir: t.TempDir()}
		c.admitSlot("s", reservation{ID: "r1", Mode: modeBg, Parent: "boss", Type: "worker"}, 20)
		if !c.hasLiveChildren("s", "boss") || c.hasLiveChildren("s", "") {
			t.Error("hasLiveChildren")
		}
		old2 := agentsWaitPoll
		agentsWaitPoll = 10 * time.Millisecond
		t.Cleanup(func() { agentsWaitPoll = old2 })
		go func() {
			time.Sleep(50 * time.Millisecond)
			c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid", AgentType: "worker", HookEventName: "SubagentStart"})
		}()
		st := c.settleStarts("s", "boss", c.loadState(), 5*time.Second)
		if st.pendingStarts("s", "boss", time.Now()) != 0 {
			t.Error("still pending")
		}
		if _, ok := st.Agents["s"]["kid"]; !ok {
			t.Error("the start's agent is not in the state it returns")
		}
	})
}
