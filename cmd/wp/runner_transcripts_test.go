//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// fakeFilingClaude is fakeClaude that files its session the way Claude Code
// does, under $CLAUDE_CONFIG_DIR/projects/<the directory it ran in>, with a
// sub-agent's file beside it. FAKE_MODE: "" answers, "error" reports an error,
// "hang" says it began and then never ends (a timeout or a cancel ends it).
const fakeFilingClaude = `#!/bin/sh
echo "$@" > "$FAKE_OUT/args"
cat > /dev/null
proj="$CLAUDE_CONFIG_DIR/projects/$(pwd | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$proj/s1/subagents"
now=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
line() { printf '{"type":"assistant","message":{"id":"%s","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"timestamp":"%s","cwd":"%s","sessionId":"%s"}\n' "$1" "$now" "$(pwd)" "$2"; }
line m1 s1 > "$proj/s1.jsonl"
line m2 s1 > "$proj/s1/subagents/agent-a1.jsonl"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1"}'
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
		meta.Title != "Review PR 12" || meta.Started == "" || meta.Ended == "" || len(meta.Sessions) != 1 {
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

// The path that matters most: a job killed on its timeout, and one cancelled,
// both leave what the session had written so far.
func TestTimedOutAndCancelledAgentJobsKeepTheirTranscripts(t *testing.T) {
	cfg, p, remote := keepSetup(t, "hang")
	o := p.execAgent(context.Background(), keepRun(1500*time.Millisecond), "review", keepJob(remote))
	_, meta := keptJob(t, cfg, p, o, "failed")
	if !strings.Contains(meta.Error, "timed out") {
		t.Errorf("sidecar error = %q", meta.Error)
	}

	cfg2, p2, remote2 := keepSetup(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(1500 * time.Millisecond); cancel() }()
	o = p2.execAgent(ctx, keepRun(time.Minute), "review", keepJob(remote2))
	_, meta = keptJob(t, cfg2, p2, o, "cancelled")
	if meta.Error != "Stopped." {
		t.Errorf("sidecar error = %q", meta.Error)
	}
}

// Another CLI's job gets no sidecar and no result added.
func TestKeepTranscriptsLeavesOtherCLIsAlone(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	p := newTestProc(t)
	o := p.keepTranscripts(context.Background(), keepRun(time.Minute), agentJob{CLI: "codex"}, filepath.Join(t.TempDir(), "job7-1"), time.Now(), nil, outcome{State: "done"})
	if o.Result != nil {
		t.Errorf("result = %v", o.Result)
	}
	if m, _ := filepath.Glob(filepath.Join(cfg, "projects", "*")); len(m) != 0 {
		t.Errorf("made %v", m)
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
	run := keepRun(1500 * time.Millisecond)
	o := p.execAgent(context.Background(), run, "review", keepJob(remote))
	keptJob(t, cfg, p, o, "failed")
	ov, err := e.svc.UsageOverview(context.Background(), me, core.UsageQuery{Days: 3650})
	if err != nil {
		t.Fatal(err)
	}
	if ov.Totals.Messages != 2 { // the main thread's and the sub-agent's
		t.Errorf("messages on the server = %d\nlog:\n%s", ov.Totals.Messages, run.out.text())
	}
	if strings.Contains(run.out.text(), "not uploaded") {
		t.Errorf("log = %s", run.out.text())
	}
}
