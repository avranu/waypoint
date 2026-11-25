package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestPureNoop(t *testing.T) {
	yes := []string{"echo waiting", "true", ":", "echo a; sleep 5", "printf x && sleep 2", "echo 'polling...'", "sleep 3; echo ok"}
	no := []string{`echo "$x"`, "echo done > f", "echo x | tee f", `printf '%s' "$(cmd)"`, "true && make", "sleep 30", "echo a; make", "echo a & ", "ls", ""}
	for _, c := range yes {
		if !pureNoop(c) {
			t.Errorf("%q should be a no-op", c)
		}
	}
	for _, c := range no {
		if pureNoop(c) {
			t.Errorf("%q should not be a no-op", c)
		}
	}
}

func bashIn(agent, cmd string, bg bool) hookEventInput {
	ti, _ := json.Marshal(map[string]any{"command": cmd, "run_in_background": bg})
	return hookEventInput{SessionID: "s", AgentID: agent, ToolName: "Bash", ToolInput: ti, Cwd: "/"}
}

// startAgent records that who (empty: the main thread) started agent id, in
// the background or not, and that it is running.
func startAgent(c *cli, who, id string, bg bool) {
	ti, _ := json.Marshal(map[string]any{"run_in_background": bg})
	c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: who, ToolInput: ti, ToolResponse: []byte(`{"agentId":"` + id + `"}`)})
	c.trackSubagent(hookEventInput{SessionID: "s", AgentID: id, AgentType: "x", HookEventName: "SubagentStart"})
}

func TestCallerOf(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	startAgent(c, "", "bgA", true)
	startAgent(c, "", "fgA", false)
	for id, want := range map[string]caller{"": callerMain, "bgA": callerBg, "fgA": callerFg, "nobody": callerFg} {
		if got := c.callerOf("s", id); got != want {
			t.Errorf("callerOf(%q) = %v, want %v", id, got, want)
		}
	}
	if got := (*cli)(nil).callerOf("s", "bgA"); got != callerFg {
		t.Errorf("nil cli: %v", got)
	}
}

func TestNoopRefusal(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	old := bgTaskRunning
	defer func() { bgTaskRunning = old }()
	bgTaskRunning = func(string) bool { return false }

	if k, text := c.hookRefusalFor(bashIn("", "echo waiting", true)); k != "" {
		t.Errorf("a background call is not refused: %s %s", k, text)
	}
	startAgent(c, "boss", "a1", true)
	startAgent(c, "boss", "f1", false)
	for _, cmd := range []string{"echo waiting", "echo done", "true"} {
		for _, agent := range []string{"", "a1", "f1"} {
			if k, text := c.hookRefusalFor(bashIn(agent, cmd, false)); k != "" || text != "" {
				t.Errorf("%q by %q with nothing to wait for: refused %s %q", cmd, agent, k, text)
			}
		}
	}
	var nilC *cli
	if k, text := nilC.hookRefusalFor(bashIn("a1", "echo done", false)); k != "" {
		t.Errorf("nil cli, nothing running: %s %q", k, text)
	}
	bgTaskRunning = func(string) bool { return true }
	if k, text := nilC.hookRefusalFor(bashIn("a1", "echo done", false)); k != "noop-poll" || !strings.Contains(text, "background command") {
		t.Errorf("nil cli, task running: %s %q", k, text)
	}
	if k, text := nilC.hookRefusalFor(bashIn("", "echo done", false)); k != "noop-poll" || !strings.Contains(text, "you are told when it exits") {
		t.Errorf("nil cli, main, task running: %s %q", k, text)
	}
	if k, text := c.hookRefusalFor(bashIn("", "echo done", false)); k != "noop-poll" || !strings.Contains(text, "does nothing") || strings.Contains(text, "Monitor") {
		t.Errorf("task running: %s %q", k, text)
	}
	bgTaskRunning = func(string) bool { return false }
	startAgent(c, "f2", "kid9", true)
	if k, text := c.hookRefusalFor(bashIn("f2", "echo done", false)); k != "noop-poll" || !strings.Contains(text, "agents are still running") {
		t.Errorf("live children: %s %q", k, text)
	}
	bgTaskRunning = func(string) bool { return true }
	_, sub := c.hookRefusalFor(bashIn("a1", "echo waiting", false))
	_, fg := c.hookRefusalFor(bashIn("f1", "echo waiting", false))
	_, main := c.hookRefusalFor(bashIn("", "echo waiting", false))
	if !strings.Contains(sub, "end your turn and you are woken") || !strings.Contains(main, "you are told when it exits") || strings.Contains(sub+main+fg, "Monitor") {
		t.Errorf("task: %q / %q", sub, main)
	}
	if !strings.Contains(fg, "600000") || strings.Contains(fg, "end your turn and you") || !strings.Contains(fg, "stops it") {
		t.Errorf("task, foreground: %q", fg)
	}
	bgTaskRunning = func(string) bool { return false }
	c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "a1", ToolResponse: []byte(`{"agentId":"kid"}`)})
	c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid", AgentType: "x", HookEventName: "SubagentStart"})
	_, sub = c.hookRefusalFor(bashIn("a1", "echo waiting", false))
	_, main = c.hookRefusalFor(bashIn("", "echo waiting", false))
	if !strings.Contains(sub, "run_in_background: true and end your turn") || strings.Contains(sub, "Monitor") {
		t.Errorf("children, sub: %q", sub)
	}
	startAgent(c, "f1", "kid3", false)
	_, fg = c.hookRefusalFor(bashIn("f1", "echo waiting", false))
	if !strings.Contains(fg, "wp agents wait` in the foreground") || !strings.Contains(fg, "600000") || strings.Contains(fg, "run_in_background: true") || strings.Contains(fg, "Monitor") {
		t.Errorf("children, foreground: %q", fg)
	}
	if strings.Contains(main, "wp agents wait") || strings.Contains(main, "agents you started") {
		t.Errorf("children, main (not its agents): %q", main)
	}
	c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "", ToolResponse: []byte(`{"agentId":"kid"}`)})
	_, main = c.hookRefusalFor(bashIn("", "echo waiting", false))
	if strings.Contains(main, "wp agents wait") || !strings.Contains(main, "agents you started") {
		t.Errorf("children, main: %q", main)
	}
}

// reEndWarning is the one sentence that may mention ending the turn in a
// foreground agent's text: the warning that doing so ends its run.
var reEndWarning = regexp.MustCompile(`Ending your turn instead ends your run[^.]*\.`)

func TestSleepTexts(t *testing.T) {
	cases := map[string]string{
		"sleep 60; wp pr show 5":      "wp pr wait N",
		"sleep 60; wp request show 1": "wp request wait R-N",
		"sleep 9; wp agents ls":       "wp agents wait",
		"sleep 9; wp testsum -- x":    "queue for the slot",
	}
	for cmd, want := range cases {
		for _, who := range []caller{callerBg, callerFg, callerMain} {
			got := sleepWaitText(cmd, who)
			if !strings.Contains(got, want) {
				t.Errorf("%q who=%v: %q lacks %q", cmd, who, got, want)
			}
			if who != callerMain && strings.Contains(got, "Monitor") {
				t.Errorf("%q: a subagent is told of Monitor: %q", cmd, got)
			}
			if who == callerFg {
				if rest := strings.ToLower(reEndWarning.ReplaceAllString(got, "")); strings.Contains(rest, "end your turn") || strings.Contains(rest, "ending your turn") {
					t.Errorf("%q: a foreground agent is told to end its turn: %q", cmd, got)
				}
			}
		}
	}
	if s := sleepWaitText("sleep 9; wp agents ls", callerFg); !strings.Contains(s, "foreground") || !strings.Contains(s, "600000") {
		t.Errorf("fg agents: %q", s)
	}
	if s := sleepWaitText("sleep 5; foo", callerBg); s != sleepWaitBgText || strings.Contains(s, "Monitor") {
		t.Errorf("bg general: %q", s)
	}
	if s := sleepWaitText("sleep 5; foo", callerFg); s != sleepWaitFgText || strings.Contains(s, "Monitor") || !strings.Contains(s, "600000") {
		t.Errorf("fg general: %q", s)
	}
	if s := sleepWaitText("sleep 5; foo", callerMain); s != sleepWaitMainText {
		t.Errorf("main general: %q", s)
	}
}

func TestMonitorNote(t *testing.T) {
	c := &cli{dir: t.TempDir()}
	startAgent(c, "", "a", true)
	in := `{"session_id":"s","agent_id":"a","tool_name":"Monitor","tool_input":{"command":"sleep 5; echo hi"}}`
	var out bytes.Buffer
	hookClaudeNudge(c, strings.NewReader(in), &out)
	if !strings.Contains(out.String(), "does not keep a subagent's run open") || strings.Contains(out.String(), "Sleeping") || !strings.Contains(out.String(), "run_in_background: true") {
		t.Errorf("first: %q", out.String())
	}
	startAgent(c, "", "f", false)
	var fo bytes.Buffer
	hookClaudeNudge(c, strings.NewReader(strings.Replace(in, `"agent_id":"a"`, `"agent_id":"f"`, 1)), &fo)
	if !strings.Contains(fo.String(), "600000") || strings.Contains(fo.String(), "run_in_background: true") {
		t.Errorf("foreground: %q", fo.String())
	}
	out.Reset()
	hookClaudeNudge(c, strings.NewReader(in), &out)
	if out.Len() != 0 {
		t.Errorf("second: %q", out.String())
	}
	out.Reset()
	hookClaudeNudge(c, strings.NewReader(strings.Replace(in, `"agent_id":"a",`, "", 1)), &out)
	if out.Len() != 0 {
		t.Errorf("main thread: %q", out.String())
	}
}

func TestWaitForRuns(t *testing.T) {
	t0 := time.Unix(100, 0)
	now := t0
	live := map[string]liveAgent{"a": {Since: t0}, "b": {Since: t0}}
	sleeps := 0
	d := waitDeps{
		now: func() time.Time { return now },
		sleep: func(x time.Duration) {
			now = now.Add(x)
			sleeps++
			switch sleeps {
			case 2:
				delete(live, "a")
			case 3:
				live["b"] = liveAgent{Since: now}
			}
		},
		live: func() (map[string]liveAgent, bool) { return live, true },
	}
	targets := []waitTarget{{ID: "a", Since: t0}, {ID: "b", Since: t0}}
	done, pending := waitForRuns(targets, time.Minute, time.Second, d)
	if len(done) != 2 || len(pending) != 0 || sleeps != 3 {
		t.Errorf("done %v pending %v sleeps %d", done, pending, sleeps)
	}
	live = map[string]liveAgent{"a": {Since: t0}}
	now = t0
	done, pending = waitForRuns([]waitTarget{{ID: "a", Since: t0}}, 5*time.Second, time.Second, d)
	if len(done) != 0 || len(pending) != 1 || now.Sub(t0) != 5*time.Second {
		t.Errorf("timeout: done %v pending %v at %s", done, pending, now.Sub(t0))
	}
	now = t0
	d.live = func() (map[string]liveAgent, bool) { return nil, false }
	if _, pending = waitForRuns([]waitTarget{{ID: "a", Since: t0}}, 3*time.Second, time.Second, d); len(pending) != 1 {
		t.Errorf("unreadable: %v", pending)
	}
}

func TestAgentsWaitCommand(t *testing.T) {
	old := agentsWaitPoll
	agentsWaitPoll = 10 * time.Millisecond
	defer func() { agentsWaitPoll = old }()
	t.Setenv("WAYPOINT_SESSION", "s")

	newC := func() (*cli, *bytes.Buffer, *bytes.Buffer) {
		var o, e bytes.Buffer
		c := &cli{dir: t.TempDir(), out: &o, err: &e}
		return c, &o, &e
	}
	c, out, _ := newC()
	cti, _ := json.Marshal(map[string]any{"command": "wp agents wait"})
	c.claimWait(hookEventInput{SessionID: "s", AgentID: "", ToolName: "Bash", ToolInput: cti})
	if err := cmdAgentsWait(c, nil); err != nil || !strings.Contains(out.String(), "no agents of yours are running") {
		t.Errorf("none: %v %q", err, out.String())
	}

	c, out, errb := newC()
	c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "a1", ToolResponse: []byte(`{"agentId":"kid1"}`)})
	c.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "", ToolResponse: []byte(`{"agentId":"kid2"}`)})
	c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid1", AgentType: "worker", HookEventName: "SubagentStart"})
	c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid2", AgentType: "worker", HookEventName: "SubagentStart"})
	ti, _ := json.Marshal(map[string]any{"command": "wp agents wait --timeout 5s"})
	c.claimWait(hookEventInput{SessionID: "s", AgentID: "a1", ToolName: "Bash", ToolInput: ti})
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid1", HookEventName: "SubagentStop"})
	}()
	if err := cmdAgentsWait(c, []string{"--timeout", "5s"}); err != nil || !strings.Contains(out.String(), "worker kid1: ran") || strings.Contains(out.String(), "kid2") {
		t.Errorf("claimed: %v %q %q", err, out.String(), errb.String())
	}

	c2, out2, err2 := newC()
	c2.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid2", AgentType: "worker", HookEventName: "SubagentStart"})
	err := cmdAgentsWait(c2, []string{"--timeout", "100ms"})
	if ec, ok := err.(exitCode); !ok || int(ec) != 2 || !strings.Contains(out2.String()+err2.String(), "can't tell which agent is asking") || !strings.Contains(out2.String()+err2.String(), "wp agents wait ID") {
		t.Errorf("no claim: %v %q %q", err, out2.String(), err2.String())
	}

	c4, _, _ := newC()
	c4.trackAgentParent(hookEventInput{SessionID: "s", AgentID: "", ToolResponse: []byte(`{"agentId":"kid2"}`)})
	c4.trackSubagent(hookEventInput{SessionID: "s", AgentID: "kid2", AgentType: "worker", HookEventName: "SubagentStart"})
	c4.claimWait(hookEventInput{SessionID: "s", AgentID: "", ToolName: "Bash", ToolInput: ti})
	err = cmdAgentsWait(c4, []string{"--timeout", "100ms"})
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "kid2") || !strings.Contains(err.Error(), "again") {
		t.Errorf("timeout: %v", err)
	}
	if agentsWaitDefault != 9*time.Minute {
		t.Errorf("default timeout %v", agentsWaitDefault)
	}

	c3, out3, _ := newC()
	c3.trackSubagent(hookEventInput{SessionID: "s", AgentID: "abcdef123", AgentType: "worker", HookEventName: "SubagentStart"})
	go func() {
		time.Sleep(50 * time.Millisecond)
		c3.trackSubagent(hookEventInput{SessionID: "s", AgentID: "abcdef123", HookEventName: "SubagentStop"})
	}()
	if err := cmdAgentsWait(c3, []string{"abcdef", "zzz"}); err != nil || !strings.Contains(out3.String(), "zzz: not running") || !strings.Contains(out3.String(), "abcdef123: ran") {
		t.Errorf("named: %v %q", err, out3.String())
	}
}
