//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The runner uploads Claude Code's usage while it runs (WP-D17, WP-D20): at
// start and then on a timer, when the machine has a Claude directory.
func TestRunnerSyncsUsageOnATimer(t *testing.T) {
	e := newEnv(t)
	me := e.runnerUser("robin")
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	ctx := context.Background()
	messages := func() int64 {
		ov, err := e.svc.UsageOverview(ctx, me, core.UsageQuery{Days: 3650})
		if err != nil {
			t.Fatal(err)
		}
		return ov.Totals.Messages
	}

	rctx, stop := context.WithCancel(ctx)
	var out syncBuffer
	done := make(chan error, 1)
	c := &cli{dir: e.dir, out: &out, err: &out}
	go func() {
		done <- runRunner(rctx, c, runnerOpts{Name: "box", MaxJobs: 1, PollWait: -1, UsageEvery: 150 * time.Millisecond, UsageDir: dir})
	}()
	// The first sync happens on its own.
	waitFor(t, "the first usage sync", 20*time.Second, func() bool { return messages() == 3 })

	// A message written later is picked up by a later sync, with no hook in sight.
	transcript := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("\n"+`{"type":"assistant","message":{"id":"m9","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"More."}],"usage":{"input_tokens":4,"output_tokens":6,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"timestamp":%q,"cwd":"/work/app","sessionId":"sess1"}`+"\n", time.Now().UTC().Format(time.RFC3339Nano))
	_, werr := f.WriteString(line)
	_ = f.Close()
	if werr != nil {
		t.Fatal(werr)
	}
	waitFor(t, "a later usage sync", 20*time.Second, func() bool { return messages() == 4 })
	stop()
	<-done
	if !strings.Contains(out.String(), "is connected") {
		t.Errorf("runner said %q", out.String())
	}
}

func TestRunnerUsageSyncCanBeTurnedOffAndNeedsAClaudeDir(t *testing.T) {
	e := newEnv(t)
	me := e.runnerUser("robin")
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	ctx := context.Background()
	messages := func() int64 {
		ov, err := e.svc.UsageOverview(ctx, me, core.UsageQuery{Days: 3650})
		if err != nil {
			t.Fatal(err)
		}
		return ov.Totals.Messages
	}
	run := func(o runnerOpts) {
		rctx, stop := context.WithCancel(ctx)
		var out syncBuffer
		done := make(chan error, 1)
		c := &cli{dir: e.dir, out: &out, err: &out}
		o.Name, o.MaxJobs, o.PollWait, o.UsageEvery = "box", 1, -1, 100*time.Millisecond
		go func() { done <- runRunner(rctx, c, o) }()
		waitFor(t, "the runner to register", 10*time.Second, func() bool {
			rs, _ := e.svc.ListRunners(ctx, me, false)
			return len(rs) == 1
		})
		time.Sleep(800 * time.Millisecond) // several rounds
		stop()
		<-done
	}
	// --no-usage-sync: nothing is uploaded.
	run(runnerOpts{UsageDir: fixtureDir(t), NoUsageSync: true})
	if n := messages(); n != 0 {
		t.Errorf("uploaded %d messages with usage sync off", n)
	}
	// A machine without a Claude directory: nothing to do, nothing said.
	run(runnerOpts{UsageDir: filepath.Join(t.TempDir(), "no-such-claude-dir")})
	if n := messages(); n != 0 {
		t.Errorf("uploaded %d messages without a Claude directory", n)
	}
	// The service line carries the flag.
	if args := runnerArgs(runnerOpts{Name: "n", MaxJobs: 1, NoUsageSync: true}); args[len(args)-1] != "--no-usage-sync" {
		t.Errorf("args: %v", args)
	}
	if args := runnerArgs(runnerOpts{Name: "n", MaxJobs: 1}); strings.Contains(strings.Join(args, " "), "usage") {
		t.Errorf("args: %v", args)
	}
}
