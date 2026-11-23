package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeWaits is a clock that only moves when the wait sleeps; onSleep runs at
// each sleep (a stop arriving, say).
type fakeWaits struct {
	t       time.Time
	sleeps  int
	onSleep func(n int)
	goneAt  int // the sleep after which the parent is gone (0: never)
}

func (f *fakeWaits) waits() gateWaits {
	return gateWaits{
		now: func() time.Time { return f.t },
		sleep: func(d time.Duration) {
			f.t = f.t.Add(d)
			f.sleeps++
			if f.onSleep != nil {
				f.onSleep(f.sleeps)
			}
		},
		gone: func() bool { return f.goneAt > 0 && f.sleeps >= f.goneAt },
	}
}

func TestSlotQueue(t *testing.T) {
	newQ := func(f *fakeWaits, used *int, limit int) slotQueue {
		return slotQueue{
			limit: limit, bound: 4 * time.Minute, poll: time.Second, w: f.waits(),
			count: func() (int, bool) { return *used, true },
			admit: func() (int, bool) {
				if *used >= limit {
					return *used, false
				}
				*used++
				return *used - 1, true
			},
		}
	}
	t.Run("under the limit: no wait", func(t *testing.T) {
		f, used := &fakeWaits{t: time.Unix(0, 0)}, 3
		r := newQ(f, &used, 5).run()
		if f.sleeps != 0 || r.timedOut || r.abandoned || r.waited != 0 || used != 4 {
			t.Errorf("%+v sleeps %d used %d", r, f.sleeps, used)
		}
	})
	t.Run("at the limit: released when a stop arrives", func(t *testing.T) {
		f, used := &fakeWaits{t: time.Unix(0, 0)}, 5
		f.onSleep = func(n int) {
			if n == 7 {
				used--
			}
		}
		r := newQ(f, &used, 5).run()
		if r.timedOut || r.abandoned || r.waited != 7*time.Second || f.sleeps != 7 || used != 5 {
			t.Errorf("%+v sleeps %d used %d", r, f.sleeps, used)
		}
	})
	t.Run("the bound passes: timed out, no slot taken", func(t *testing.T) {
		f, used := &fakeWaits{t: time.Unix(0, 0)}, 5
		r := newQ(f, &used, 5).run()
		if !r.timedOut || r.waited != 4*time.Minute || r.live != 5 || used != 5 {
			t.Errorf("%+v used %d", r, used)
		}
	})
	t.Run("the parent is gone: no orphan", func(t *testing.T) {
		f, used := &fakeWaits{t: time.Unix(0, 0), goneAt: 3}, 5
		r := newQ(f, &used, 5).run()
		if !r.abandoned || f.sleeps != 3 {
			t.Errorf("%+v sleeps %d", r, f.sleeps)
		}
	})
	t.Run("a state that can't be read does not release", func(t *testing.T) {
		f := &fakeWaits{t: time.Unix(0, 0)}
		q := newQ(f, new(int), 5)
		q.count = func() (int, bool) { return 0, false }
		if r := q.run(); !r.timedOut {
			t.Errorf("%+v", r)
		}
	})
	t.Run("another call took the slot first", func(t *testing.T) {
		f, used := &fakeWaits{t: time.Unix(0, 0)}, 4
		q := newQ(f, &used, 5)
		calls := 0
		q.admit = func() (int, bool) {
			calls++
			if calls == 1 {
				return 5, false // lost the race
			}
			return 4, true
		}
		if r := q.run(); r.timedOut || calls != 2 || f.sleeps != 1 {
			t.Errorf("%+v calls %d sleeps %d", r, calls, f.sleeps)
		}
	})
}

func TestSubagentLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for env, want := range map[string]int{"": 20, "2": 2, "0": 20, "-3": 20, "x": 20, "35": 35} {
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", env)
		if got := subagentLimit(); got != want {
			t.Errorf("env %q: %d, want %d", env, got, want)
		}
	}
}

func TestGateTimeoutExceedsTheBound(t *testing.T) {
	if hookGateTimeout < int((gateQueueBound + 30*time.Second).Seconds()) {
		t.Errorf("the gate hook's timeout %ds must exceed the queue bound %s with margin", hookGateTimeout, gateQueueBound)
	}
}

// gateFixture is a cli with a state directory, a fake clock that makes each
// sleep one second long, and the hook's stdin for an Agent call.
type gateFixture struct {
	c *cli
	f *fakeWaits
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_GATE", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "")
	g := &gateFixture{c: &cli{dir: t.TempDir()}, f: &fakeWaits{t: time.Unix(1000, 0)}}
	old, oldBound := gateWait, gateBound
	gateWait = func() gateWaits { return g.f.waits() }
	gateBound = 4 * time.Minute
	t.Cleanup(func() { gateWait, gateBound = old, oldBound })
	return g
}

func (g *gateFixture) start(session, id, typ string) {
	g.c.trackSubagent(hookEventInput{SessionID: session, AgentID: id, AgentType: typ, HookEventName: "SubagentStart"})
}

// startMode starts agent id as a child of the main thread, in the background
// or not, the way the PostToolUse of its Agent call records it.
func (g *gateFixture) startMode(session, id, typ string, bg bool) {
	ti, _ := json.Marshal(map[string]any{"run_in_background": bg})
	g.c.trackAgentParent(hookEventInput{SessionID: session, ToolInput: ti, ToolResponse: []byte(`{"agentId":"` + id + `"}`)})
	g.start(session, id, typ)
}

func (g *gateFixture) stop(session, id string) {
	g.c.trackSubagent(hookEventInput{SessionID: session, AgentID: id, HookEventName: "SubagentStop"})
}

func (g *gateFixture) run(in string) string {
	var out bytes.Buffer
	hookClaudeGate(g.c, strings.NewReader(in), &out)
	return out.String()
}

const haikuCall = `{"session_id":"s1","tool_use_id":"tu1","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"haiku"}}`

func TestGateQueue(t *testing.T) {
	t.Run("under the limit: no wait, silent", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "3")
		g.start("s1", "a", "general-purpose")
		if out := g.run(haikuCall); out != "" || g.f.sleeps != 0 {
			t.Errorf("out %q sleeps %d", out, g.f.sleeps)
		}
	})
	t.Run("at the limit: a stop releases it, silently", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "2")
		g.start("s1", "a", "general-purpose")
		g.start("s1", "b", "general-purpose")
		g.f.onSleep = func(n int) {
			if n == 5 {
				g.stop("s1", "a")
			}
		}
		out := g.run(haikuCall)
		if out != "" || g.f.sleeps != 5 {
			t.Errorf("out %q sleeps %d", out, g.f.sleeps)
		}
	})
	t.Run("the bound passes: a note, no decision", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "2")
		g.start("s1", "a", "general-purpose")
		g.start("s1", "b", "general-purpose")
		out := g.run(haikuCall)
		want := "2 agents are running, the most Claude Code runs at once; this start waited 4m. If it is refused, start it when the next one finishes, which you are told about."
		if !strings.Contains(out, want) || strings.Contains(out, "permissionDecision") {
			t.Errorf("out %q", out)
		}
		if g.f.t.Sub(time.Unix(1000, 0)) != 4*time.Minute {
			t.Errorf("waited %s", g.f.t.Sub(time.Unix(1000, 0)))
		}
	})
	t.Run("Haiku counts here, and still not toward the width rule", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "6")
		for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
			g.start("s1", id, "haiku-worker") // not in an agents dir: resolved by name below
		}
		g.c.trackSubagent(hookEventInput{SessionID: "s1", AgentID: "a", AgentType: "general-purpose", HookEventName: "SubagentStart"})
		t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "haiku")
		out := g.run(haikuCall)
		if !strings.Contains(out, "6 agents are running") || strings.Contains(out, "already live") || strings.Contains(out, "permissionDecision") {
			t.Errorf("out %q", out)
		}
	})
	t.Run("WP_GATE=off skips the queue", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "1")
		t.Setenv("WP_GATE", "off")
		g.start("s1", "a", "general-purpose")
		if out := g.run(haikuCall); out != "" || g.f.sleeps != 0 {
			t.Errorf("out %q sleeps %d", out, g.f.sleeps)
		}
	})
	t.Run("a fork is refused at once, never held", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "1")
		g.start("s1", "a", "general-purpose")
		out := g.run(`{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"fork"}}`)
		if !strings.Contains(out, "Forks are never allowed") || g.f.sleeps != 0 {
			t.Errorf("out %q sleeps %d", out, g.f.sleeps)
		}
	})
	t.Run("a refusal of the rules is not held either", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "1")
		g.start("s1", "a", "general-purpose")
		out := g.run(`{"session_id":"s1","tool_name":"Agent","agent_id":"z","tool_input":{"subagent_type":"general-purpose","model":"opus"}}`)
		if !strings.Contains(out, "coordinator") || g.f.sleeps != 0 {
			t.Errorf("out %q sleeps %d", out, g.f.sleeps)
		}
	})
	t.Run("a person's pause is not held", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "1")
		g.start("s1", "a", "general-purpose") // the table is at the limit
		g.c.savePause(json.RawMessage(testPause))
		if g.c.activePause() == nil {
			t.Fatal("the pause was not saved")
		}
		out := g.run(haikuCall)
		if !strings.Contains(out, "permissionDecision") || !strings.Contains(out, "Quota alert") || !strings.Contains(out, "No agents start until it is lifted") {
			t.Errorf("out %q: want the pause refusal", out)
		}
		if g.f.sleeps != 0 || !g.f.t.Equal(time.Unix(1000, 0)) {
			t.Errorf("the queue was entered: %d sleeps, clock moved %s", g.f.sleeps, g.f.t.Sub(time.Unix(1000, 0)))
		}
	})
	t.Run("three calls of one message, one slot free: the third waits", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "2")
		call := func(id string) string {
			return strings.Replace(haikuCall, `"tu1"`, `"`+id+`"`, 1)
		}
		for _, id := range []string{"tu1", "tu2"} {
			if out := g.run(call(id)); out != "" || g.f.sleeps != 0 {
				t.Fatalf("%s: out %q sleeps %d", id, out, g.f.sleeps)
			}
		}
		// Their SubagentStarts have not come: both slots are reserved.
		g.f.onSleep = func(n int) {
			if n == 3 {
				g.start("s1", "x", "general-purpose") // the first start answers a reservation
				g.stop("s1", "x")                     // and it finishes at once: a slot is free
			}
		}
		if out := g.run(call("tu3")); out != "" || g.f.sleeps != 3 {
			t.Errorf("third: out %q sleeps %d", out, g.f.sleeps)
		}
	})
	t.Run("the same call twice takes one slot", func(t *testing.T) {
		g := newGateFixture(t)
		t.Setenv("CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "2")
		g.run(haikuCall)
		g.run(haikuCall)
		if n := len(g.c.loadState().Reserved["s1"]); n != 1 {
			t.Errorf("%d reservations", n)
		}
	})
}

func TestTrackSubagentRuns(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	start := func(id string) {
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: id, AgentType: "general-purpose", HookEventName: "SubagentStart"})
	}
	c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "boss", ToolResponse: []byte(`{"status":"async_launched","agentId":"kid"}`)})
	start("kid")
	first := c.loadState().Agents["s"]["kid"].Since
	time.Sleep(5 * time.Millisecond)
	// stopped and resumed: a new run, the same parent
	c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid", HookEventName: "SubagentStop"})
	start("kid")
	st := c.loadState()
	if !st.Agents["s"]["kid"].Since.After(first) {
		t.Errorf("a resumed agent's run did not start again")
	}
	if p := st.AgentParents["s"]["kid"].Parent; p != "boss" {
		t.Errorf("parent %q", p)
	}
	// the parent's record may come after the start
	start("late")
	c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "boss", ToolResponse: []byte(`{"agentId":"late"}`)})
	st = c.loadState()
	if got := st.childrenOf("s", "boss", time.Now()); len(got) != 2 {
		t.Errorf("boss's children %v", got)
	}
	if got := st.childrenOf("s", "", time.Now()); len(got) != 0 {
		t.Errorf("main's children %v", got)
	}
	// a stop that came before the record leaves nothing live
	c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "fast", HookEventName: "SubagentStop"})
	c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "", ToolResponse: []byte(`{"agentId":"fast"}`)})
	st = c.loadState()
	if _, ok := st.liveIDs("s", time.Now())["fast"]; ok {
		t.Errorf("a stopped agent is live")
	}
	// a session that ends takes its agents with it
	c.forgetSessionAgents("s")
	if st := c.loadState(); len(st.Agents) != 0 || len(st.AgentParents) != 0 {
		t.Errorf("left %v %v", st.Agents, st.AgentParents)
	}
}

func TestStaleAgentsExpire(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	_ = c.updateState(func(s *State) {
		s.Agents = map[string]map[string]liveAgent{"s": {"old": {Since: time.Now().Add(-4 * time.Hour)}, "new": {Since: time.Now()}}}
	})
	n, ok := c.liveCount("s", time.Now())
	if !ok || n != 1 {
		t.Errorf("live %d %v, want the TTL to drop the lost stop", n, ok)
	}
}

func TestGateNotes(t *testing.T) {
	t.Run("a subagent's first background start is told how to wait, once", func(t *testing.T) {
		g := newGateFixture(t)
		g.startMode("s1", "boss", "general-purpose", true)
		call := `{"session_id":"s1","agent_id":"boss","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"haiku","run_in_background":true}}`
		if out := g.run(call); !strings.Contains(out, "wp agents wait") || !strings.Contains(out, "run_in_background: true") || strings.Contains(out, "permissionDecision") {
			t.Errorf("first: %q", out)
		}
		if out := g.run(call); out != "" {
			t.Errorf("second: %q", out)
		}
		// resumed: a new run, told again
		time.Sleep(5 * time.Millisecond)
		g.startMode("s1", "boss", "general-purpose", true)
		if out := g.run(call); !strings.Contains(out, "wp agents wait") {
			t.Errorf("after a resume: %q", out)
		}
	})
	t.Run("a foreground (or unknown) subagent is never told to end its turn", func(t *testing.T) {
		g := newGateFixture(t)
		g.start("s1", "unk", "general-purpose") // no record of how it was started
		g.startMode("s1", "fgboss", "general-purpose", false)
		call := `{"session_id":"s1","agent_id":"fgboss","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"haiku","run_in_background":true}}`
		if out := g.run(call); !strings.Contains(out, "wp agents wait` in the foreground") || !strings.Contains(out, "600000") || strings.Contains(out, "run_in_background: true") {
			t.Errorf("foreground: %q", out)
		}
		if out := g.run(strings.Replace(call, "fgboss", "unk", 1)); !strings.Contains(out, "600000") || strings.Contains(out, "run_in_background: true") {
			t.Errorf("unknown: %q", out)
		}
	})
	t.Run("the mode comes from a reservation made by the Agent call", func(t *testing.T) {
		g := newGateFixture(t)
		g.run(`{"session_id":"s1","tool_use_id":"tu9","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"haiku","run_in_background":true}}`)
		g.start("s1", "resv", "general-purpose")
		if got := g.c.callerOf("s1", "resv"); got != callerBg {
			t.Errorf("reserved background start: %v", got)
		}
	})
	t.Run("not the main thread, not a foreground start", func(t *testing.T) {
		g := newGateFixture(t)
		if out := g.run(`{"session_id":"s1","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"haiku","run_in_background":true}}`); out != "" {
			t.Errorf("main thread: %q", out)
		}
		if out := g.run(`{"session_id":"s1","agent_id":"boss","tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"haiku"}}`); out != "" {
			t.Errorf("foreground: %q", out)
		}
	})
}
