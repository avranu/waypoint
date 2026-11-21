package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRunsAgentsWait(t *testing.T) {
	yes := []string{
		"wp agents wait",
		"  wp agents wait --timeout 5s",
		"cd /x && wp agents wait",
		"make; wp agents wait",
		"a || wp agents wait",
		"a | wp agents wait",
		"(wp agents wait)",
		"echo hi\nwp agents wait",
		"timeout 590 wp agents wait",
		"timeout -k 5 590 wp agents wait",
		"FOO=bar wp agents wait",
		"FOO=bar BAZ=1 timeout 5 wp agents wait id1",
		"env X=1 wp agents wait",
		"nohup wp agents wait &",
		"/usr/local/bin/wp agents wait",
		"bash -c 'wp agents wait'",
		`sh -c "cd /x; wp agents wait"`,
		"wp  agents   wait",
	}
	no := []string{
		"",
		`echo "wp agents wait"`,
		"echo 'wp agents wait'",
		"grep wp agents wait notes.md",
		"echo wp agents wait",
		"cat wp-agents-wait.txt",
		"wp agents ls",
		"wp agents waiting",
		"git commit -m 'wp agents wait'",
		"ls /wp/agents/wait",
	}
	for _, c := range yes {
		if !runsAgentsWait(c) {
			t.Errorf("%q should run it", c)
		}
	}
	for _, c := range no {
		if runsAgentsWait(c) {
			t.Errorf("%q should not run it", c)
		}
	}
}

func TestPickWaitClaim(t *testing.T) {
	claims := []waitClaim{
		{Session: "s", Agent: "a1", Command: "wp agents wait"},
		{Session: "other", Agent: "x", Command: "wp agents wait --timeout 5s"},
		{Session: "s", Agent: "a2", Command: "wp agents wait --timeout 5s"},
		{Session: "s", Agent: "a3", Command: "timeout 590 wp agents wait"},
		{Session: "s", Agent: "a4", Command: "timeout 590 wp agents wait"},
	}
	cases := []struct {
		name      string
		session   string
		ancestors []string
		want      int
	}{
		{"nothing readable: oldest of the session", "s", nil, 0},
		{"nothing matches: oldest of the session", "s", []string{"claude --foo", "/bin/bash -c eval 'ls'"}, 0},
		{"its own text, not the oldest", "s", []string{"/bin/bash -c eval 'wp agents wait --timeout 5s' && pwd"}, 2},
		{"the longest of those contained wins", "s", []string{"eval 'timeout 590 wp agents wait'"}, 3},
		{"identical text: the oldest of them (the remaining swap case)", "s", []string{"eval 'timeout 590 wp agents wait'", "x"}, 3},
		{"any ancestor", "s", []string{"claude", "timeout 590 wp agents wait"}, 3},
		{"another session's claim is never taken", "other", []string{"eval 'wp agents wait'"}, 1},
		{"no claim of the session", "none", []string{"wp agents wait"}, -1},
	}
	for _, tc := range cases {
		if got := pickWaitClaim(claims, tc.session, tc.ancestors); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
	// a claim without a command (state written before it was kept) is still the oldest
	if got := pickWaitClaim([]waitClaim{{Session: "s", Agent: "old"}}, "s", []string{"anything"}); got != 0 {
		t.Errorf("no command text: %d", got)
	}
}

// Two agents call close together; the later claim is the one this command's
// own text points at.
func TestTakeWaitClaimByText(t *testing.T) {
	old := ancestorCmdlines
	defer func() { ancestorCmdlines = old }()
	c := &cli{dir: t.TempDir()}
	claim := func(agent, cmd string) {
		ti, _ := json.Marshal(map[string]any{"command": cmd})
		c.claimWait(hookEventInput{SessionID: "s", AgentID: agent, ToolName: "Bash", ToolInput: ti})
		time.Sleep(time.Millisecond)
	}
	claim("first", "wp agents wait --timeout 1m")
	claim("second", "wp agents wait --timeout 2m")
	ancestorCmdlines = func() []string { return []string{"bash -c eval 'wp agents wait --timeout 2m' < /dev/null"} }
	if who, ok := c.takeWaitClaim("s"); !ok || who != "second" {
		t.Errorf("got %q %v, want second", who, ok)
	}
	ancestorCmdlines = func() []string { return nil }
	if who, ok := c.takeWaitClaim("s"); !ok || who != "first" {
		t.Errorf("got %q %v, want first", who, ok)
	}
}

// A call the guard denies leaves no claim; one it only warns about, or lets
// through, does.
func TestGuardClaimOnlyWhenAllowed(t *testing.T) {
	_, repo := guardTestSite(t)
	c := &cli{dir: t.TempDir(), cfg: Config{Server: "https://wp.example.test"}}
	run := func(cmd string) {
		in, _ := json.Marshal(map[string]any{"session_id": "sess-1", "tool_name": "Bash", "cwd": repo, "tool_input": map[string]any{"command": cmd}})
		var out bytes.Buffer
		hookClaudeGuard(c, bytes.NewReader(in), &out)
	}
	claims := func() int { return len(c.loadState().WaitClaims) }
	t.Setenv("WP_TASK", "WP-1")
	t.Setenv("WP_GUARD", "")
	const bad = "sudo apt-get install jq; wp agents wait"
	run(bad)
	if n := claims(); n != 0 {
		t.Errorf("a denied call left %d claims", n)
	}
	t.Setenv("WP_GUARD", "warn")
	run(bad)
	if n := claims(); n != 1 {
		t.Errorf("a warned call left %d claims, want 1", n)
	}
	t.Setenv("WP_GUARD", "")
	run("wp agents wait")
	if n := claims(); n != 2 {
		t.Errorf("an allowed call: %d claims, want 2", n)
	}
	run(`echo "wp agents wait"`)
	if n := claims(); n != 2 {
		t.Errorf("a command that only mentions it left a claim: %d", n)
	}
	if !strings.Contains(c.loadState().WaitClaims[1].Command, "wp agents wait") {
		t.Error("the command text is not kept")
	}
}
