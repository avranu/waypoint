//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// fakeFilingClaude is fakeClaude that files its session the way Claude Code
// does, under $CLAUDE_CONFIG_DIR/projects/<the directory it ran in>, with a
// sub-agent's file beside it and a second session (h1, like a helper's) that
// the stream's session id (s1) doesn't name. FAKE_MODE: "" answers, "error" reports an error,
// "hang" says it began, with its files written, and then never ends (a timeout or a cancel ends it).
const fakeFilingClaude = `#!/bin/sh
echo "$@" > "$FAKE_OUT/args"
cat > /dev/null
proj="$CLAUDE_CONFIG_DIR/projects/$(pwd | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$proj/s1/subagents"
now=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
line() { printf '{"type":"assistant","message":{"id":"%s","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"timestamp":"%s","cwd":"%s","sessionId":"%s"}\n' "$1" "$now" "$(pwd)" "$2"; }
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1"}'
line m3 h1 > "$proj/h1.jsonl"
line m2 s1 > "$proj/s1/subagents/agent-a1.jsonl"
line m1 s1 > "$proj/s1.jsonl"
case "$FAKE_MODE" in
  hang) sleep 60 ;;
  error) printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"it broke"}' ;;
  *) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"fine","usage":{"input_tokens":1,"output_tokens":1}}' ;;
esac
`

// keepSetup fakes claude, gives the job a config directory of its own and
// returns the directory, a runner and a repository to review.
func keepSetup(t *testing.T, mode string) (cfg string, p *runnerProc, remote string) {
	t.Helper()
	fakeCLI(t, "claude", fakeFilingClaude)
	passTestEnv(t, "FAKE_MODE")
	t.Setenv("FAKE_MODE", mode)
	cfg = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	remote = newGitRepo(t)
	p = newTestProc(t)
	return cfg, p, remote
}

// keptJob checks what a job left in cfg: its project directory with the main
// and sub-agent transcripts and the sidecar, the link in its result, and that
// the checkout is gone.
func keptJob(t *testing.T, cfg string, p *runnerProc, o outcome, state string) (proj string, meta jobMeta) {
	t.Helper()
	if o.State != state {
		t.Fatalf("outcome = %+v, want %s", o, state)
	}
	dirs, _ := filepath.Glob(filepath.Join(cfg, "projects", "*-job7-*"))
	if len(dirs) != 1 {
		t.Fatalf("project directories = %v", dirs)
	}
	proj = dirs[0]
	for _, f := range []string{"s1.jsonl", filepath.Join("s1", "subagents", "agent-a1.jsonl"), jobSidecarFile} {
		if _, err := os.Stat(filepath.Join(proj, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(proj, jobSidecarFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	res, ok := o.Result.(map[string]any)
	if !ok || res["session_id"] != "s1" || res["transcript_dir"] != proj {
		t.Errorf("the result doesn't link the transcript: %v", o.Result)
	}
	if left, _ := os.ReadDir(filepath.Join(p.cacheDir(), "work")); len(left) != 0 {
		t.Errorf("work trees left: %v", left)
	}
	if meta.Job != 7 || meta.Kind != "review" || meta.State != state || meta.SessionID != "s1" || meta.Model != "sonnet" ||
		meta.Title != "Review PR 12" || meta.Started == "" || meta.Ended == "" || !slices.Equal(meta.Sessions, []string{"h1", "s1"}) ||
		meta.CLI != "claude" || meta.Repo == "" || meta.Work == "" || meta.WPVersion != version {
		t.Errorf("sidecar = %+v", meta)
	}
	return proj, meta
}

func keepJob(repo string) agentJob {
	return agentJob{CLI: "claude", Model: "sonnet", Prompt: "Review it.", Repo: repo, JSONSchema: `{"type":"object"}`}
}

func keepRun(timeout time.Duration) *jobRun {
	r := newRun(timeout)
	r.kind, r.title = "review", "Review PR 12"
	return r
}

// A job that ends well leaves its transcripts, with the sidecar, after its
// checkout is gone; claude isn't told to skip them.
func TestAgentJobKeepsItsTranscripts(t *testing.T) {
	cfg, p, remote := keepSetup(t, "")
	o := p.execAgent(context.Background(), keepRun(time.Minute), "review", keepJob(remote))
	_, meta := keptJob(t, cfg, p, o, "done")
	if meta.Error != "" {
		t.Errorf("sidecar error = %q", meta.Error)
	}
	if args, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_OUT"), "args")); strings.Contains(string(args), "no-session-persistence") {
		t.Errorf("claude was run with %s", args)
	}
}

// A job that fails keeps them too.
func TestFailedAgentJobKeepsItsTranscripts(t *testing.T) {
	cfg, p, remote := keepSetup(t, "error")
	o := p.execAgent(context.Background(), keepRun(time.Minute), "review", keepJob(remote))
	_, meta := keptJob(t, cfg, p, o, "failed")
	if !strings.Contains(meta.Error, "it broke") {
		t.Errorf("sidecar error = %q", meta.Error)
	}
}

// waitForFile polls (briefly) for the fake claude's first transcript to exist,
// (the fake writes it last, after its init line) so a cancel comes after the session was filed however long the checkout took.
// It is called from goroutines, so it reports with t.Error and returns whether
// the file came, and the caller cancels either way.
func waitForFile(t *testing.T, cfg string) bool {
	t.Helper()
	for range 400 {
		if m, _ := filepath.Glob(filepath.Join(cfg, "projects", "*-job7-*", "s1.jsonl")); len(m) == 1 {
			time.Sleep(200 * time.Millisecond) // The fake's init line goes out just before the file appears; wait briefly so the runner reads it before a cancel.
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Error("claude never filed its session")
	return false
}

// The path that matters most: a job killed on its timeout, and one cancelled,
// both leave what the session had written so far.
func TestTimedOutAndCancelledAgentJobsKeepTheirTranscripts(t *testing.T) {
	cfg, p, remote := keepSetup(t, "hang")
	o := p.execAgent(context.Background(), keepRun(4*time.Second), "review", keepJob(remote))
	_, meta := keptJob(t, cfg, p, o, "failed")
	if !strings.Contains(meta.Error, "timed out") {
		t.Errorf("sidecar error = %q", meta.Error)
	}

	cfg2, p2, remote2 := keepSetup(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { waitForFile(t, cfg2); cancel() }()
	o = p2.execAgent(ctx, keepRun(time.Minute), "review", keepJob(remote2))
	_, meta = keptJob(t, cfg2, p2, o, "cancelled")
	if meta.Error != "The runner was stopped." {
		t.Errorf("sidecar error = %q", meta.Error)
	}

	cfg3, p3, remote3 := keepSetup(t, "hang")
	ctx, cancel = context.WithCancel(context.Background())
	run := keepRun(time.Minute)
	run.cancelled = &atomic.Bool{}
	go func() { waitForFile(t, cfg3); run.cancelled.Store(true); cancel() }()
	o = p3.execAgent(ctx, run, "review", keepJob(remote3))
	_, meta = keptJob(t, cfg3, p3, o, "cancelled")
	if meta.State != "cancelled" || meta.Error != "Cancelled by a person." {
		t.Errorf("sidecar = %+v", meta)
	}
}

// Another CLI's job gets no sidecar and no result added.
func TestKeepTranscriptsLeavesOtherCLIsAlone(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	p := newTestProc(t)
	work := filepath.Join(t.TempDir(), "job7-1")
	writeAt(t, filepath.Join(cfg, "projects", "-x-job7-1", "s1.jsonl"), tline("m1", "s1"))
	o := p.keepTranscripts(context.Background(), keepRun(time.Minute), agentJob{CLI: "codex"}, work, time.Now(), nil, outcome{State: "done"})
	if o.Result != nil {
		t.Errorf("result = %v", o.Result)
	}
	if _, err := os.Stat(filepath.Join(cfg, "projects", "-x-job7-1", jobSidecarFile)); err == nil {
		t.Error("wrote a sidecar for another CLI's job")
	}
}

// With a server, the end of the job sends the session before it returns: the
// usage the job spent is on the server without waiting for the timer, on the
// timeout path too.
func TestAgentJobUploadsItsTranscriptsAtTheEnd(t *testing.T) {
	e := newEnv(t)
	me := e.runnerUser("robin")
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	cfg, p, remote := keepSetup(t, "hang")
	var out bytes.Buffer
	p.c = &cli{dir: e.dir, out: &out, err: &out}
	run := keepRun(4 * time.Second)
	o := p.execAgent(context.Background(), run, "review", keepJob(remote))
	keptJob(t, cfg, p, o, "failed")
	ov, err := e.svc.UsageOverview(context.Background(), me, core.UsageQuery{Days: 3650})
	if err != nil {
		t.Fatal(err)
	}
	if ov.Totals.Messages != 3 {
		t.Errorf("messages on the server = %d\nlog:\n%s", ov.Totals.Messages, run.out.text())
	}
	if strings.Contains(run.out.text(), "did not finish") {
		t.Errorf("log = %s", run.out.text())
	}
}
