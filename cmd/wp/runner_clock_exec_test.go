package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeClaudeNoting = `#!/bin/sh
# Records where its commands are to note their slot waits, and whether the file exists.
out="$FAKE_OUT"
printf '%s' "$WAYPOINT_SLOT_WAITS" > "$out/waits"
if [ -f "$WAYPOINT_SLOT_WAITS" ]; then echo yes > "$out/exists"; fi
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","usage":{"input_tokens":1,"output_tokens":1},"total_cost_usd":0}'
`

func TestAgentJobGetsASlotWaitsFile(t *testing.T) {
	rec := fakeCLI(t, "claude", fakeClaudeNoting)
	remote := newGitRepo(t)
	p := newTestProc(t)
	run := newRun(time.Minute)
	o := p.execAgent(context.Background(), run, "agent", agentJob{CLI: "claude", Prompt: "x", Repo: remote, WorkTimeout: 30})
	if o.State != "done" {
		t.Fatalf("outcome = %+v\nlog:\n%s", o, run.out.text())
	}
	raw, err := os.ReadFile(filepath.Join(rec, "waits"))
	if err != nil || len(raw) == 0 {
		t.Fatalf("the CLI saw no WAYPOINT_SLOT_WAITS: %q, %v", raw, err)
	}
	if got := string(raw); !strings.HasPrefix(got, filepath.Join(p.cacheDir(), "work")) || !strings.HasSuffix(got, "job7.slotwaits") {
		t.Errorf("WAYPOINT_SLOT_WAITS = %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(rec, "exists")); strings.TrimSpace(string(b)) != "yes" {
		t.Errorf("the file didn't exist while the CLI ran")
	}
	if _, err := os.Stat(string(raw)); err == nil {
		t.Error("the waits file outlived the job")
	}
}

const fakeClaudeSlow = `#!/bin/sh
exec sleep 60
`

func TestAgentJobStopsAtItsWorkLimit(t *testing.T) {
	fakeCLI(t, "claude", fakeClaudeSlow)
	remote := newGitRepo(t)
	p := newTestProc(t)
	old := clockEvery
	clockEvery = 50 * time.Millisecond
	t.Cleanup(func() { clockEvery = old })
	run := newRun(10 * time.Minute)
	start := time.Now()
	o := p.execAgent(context.Background(), run, "agent", agentJob{CLI: "claude", Prompt: "x", Repo: remote, WorkTimeout: 1})
	if o.State != "failed" || !strings.Contains(o.Error, "of work") || !strings.Contains(o.Error, "1s") {
		t.Fatalf("outcome = %+v\nlog:\n%s", o, run.out.text())
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("took %s: the work limit didn't end it", d)
	}
}
