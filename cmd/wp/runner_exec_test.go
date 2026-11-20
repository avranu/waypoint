//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newTestProc is a runner that allows the given directories.
func newTestProc(t *testing.T, allow ...string) *runnerProc {
	t.Helper()
	var dirs []string
	for _, d := range allow {
		real, err := filepath.EvalSymlinks(d)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, real)
	}
	return &runnerProc{c: &cli{out: &bytes.Buffer{}}, allowed: dirs, jobs: map[int64]*activeJob{}, freed: make(chan struct{}, 1),
		opts: runnerOpts{AllowLocalRepos: true, CacheDir: filepath.Join(t.TempDir(), "cache")}}
}

func newRun(timeout time.Duration) *jobRun {
	return &jobRun{id: 7, timeout: timeout, out: newLogSink()}
}

func (l *logSink) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.raw)
}

func TestCommandJobRunsAndStreamsOutput(t *testing.T) {
	dir := t.TempDir()
	p := newTestProc(t, dir)
	run := newRun(time.Minute)
	o := p.execCommand(context.Background(), run, commandJob{Command: "echo out; echo err >&2; pwd", Dir: dir})
	if o.State != "done" || o.ExitCode == nil || *o.ExitCode != 0 {
		t.Fatalf("outcome = %+v", o)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if got := run.out.text(); !strings.Contains(got, "out\n") || !strings.Contains(got, "err\n") || !strings.Contains(got, real) {
		t.Errorf("log = %q", got)
	}
	o = p.execCommand(context.Background(), newRun(time.Minute), commandJob{Command: "exit 3", Dir: dir})
	if o.State != "failed" || *o.ExitCode != 3 || !strings.Contains(o.Error, "code 3") {
		t.Errorf("exit 3 = %+v", o)
	}
}

func TestCommandJobTimeoutKillsTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	p := newTestProc(t, dir)
	start := time.Now()
	// A shell that starts a child and waits for it: both must die.
	o := p.execCommand(context.Background(), newRun(500*time.Millisecond), commandJob{Command: "sleep 60 & echo $! > child.pid; wait", Dir: dir})
	if o.State != "failed" || !strings.Contains(o.Error, "timed out") {
		t.Fatalf("outcome = %+v", o)
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	b, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("the child process %d outlived its job", pid)
}

func TestCommandJobCancel(t *testing.T) {
	dir := t.TempDir()
	p := newTestProc(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	o := p.execCommand(ctx, newRun(time.Minute), commandJob{Command: "sleep 60", Dir: dir})
	if o.State != "cancelled" {
		t.Errorf("outcome = %+v", o)
	}
}

func TestCommandJobOutputIsCapped(t *testing.T) {
	dir := t.TempDir()
	p := newTestProc(t, dir)
	run := newRun(time.Minute)
	o := p.execCommand(context.Background(), run, commandJob{Command: "yes 0123456789 | head -c 3000000; echo THE-END", Dir: dir})
	if o.State != "done" {
		t.Fatalf("outcome = %+v", o)
	}
	run.out.mu.Lock()
	held := len(run.out.raw)
	run.out.mu.Unlock()
	if held > logPending {
		t.Errorf("held %d bytes", held)
	}
	res := o.Result.(map[string]any)
	if res["truncated"] != true || !strings.Contains(res["tail"].(string), "THE-END") {
		t.Errorf("result = %v", res)
	}
}

// A job's environment comes from an allowlist (WP-418): the runner's SSH
// agent, desktop bus, forge tokens and Waypoint token stay behind; a
// variable the runner passes with --env goes; API keys go only when the job
// keeps them.
func TestCommandJobEnvironment(t *testing.T) {
	dir := t.TempDir()
	p := newTestProc(t, dir)
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("OPENAI_API_KEY", "sk-other")
	t.Setenv("LLM_GATEWAY_API_KEY", "sk-gw")
	t.Setenv("WAYPOINT_TOKEN", "wpt_runner")
	t.Setenv("SSH_AUTH_SOCK", "/run/agent.sock")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/bus")
	t.Setenv("FORGEJO_TOKEN", "forge-secret")
	t.Setenv("LC_TIME", "C")
	t.Setenv("KEEP_ME", "yes")
	passTestEnv(t, "KEEP_ME")
	show := `echo "a=$ANTHROPIC_API_KEY o=$OPENAI_API_KEY g=$LLM_GATEWAY_API_KEY t=$WAYPOINT_TOKEN s=$SSH_AUTH_SOCK d=$DBUS_SESSION_BUS_ADDRESS f=$FORGEJO_TOKEN l=$LC_TIME k=$KEEP_ME j=$JOB_VAR"`
	run := newRun(time.Minute)
	p.execCommand(context.Background(), run, commandJob{Command: show, Dir: dir, Env: map[string]string{"JOB_VAR": "1"}})
	if got := strings.TrimSpace(run.out.text()); got != "a= o= g= t= s= d= f= l=C k=yes j=1" {
		t.Errorf("env = %q", got)
	}
	run = newRun(time.Minute)
	p.execCommand(context.Background(), run, commandJob{Command: show, Dir: dir, KeepKeys: true})
	if got := strings.TrimSpace(run.out.text()); got != "a=sk-secret o=sk-other g=sk-gw t= s= d= f= l=C k=yes j=" {
		t.Errorf("env with keep_api_keys = %q (the runner's own token never goes)", got)
	}
}

func TestRunnerEnvFlag(t *testing.T) {
	t.Cleanup(func() { clear(runnerEnvPass) })
	if err := passJobEnv([]string{"waypoint_token"}); err == nil {
		t.Error("--env passed the runner's token")
	}
	if err := passJobEnv([]string{"A=B"}); err == nil {
		t.Error("--env took a value")
	}
	if err := passJobEnv([]string{"aws_profile", "none"}); err != nil || !runnerEnvPass["AWS_PROFILE"] {
		t.Errorf("--env aws_profile: %v %v", err, runnerEnvPass)
	}
	if w := runnerArgs(runnerOpts{Env: []string{"AWS_PROFILE"}}); strings.Join(w, " ") != "runner --env AWS_PROFILE" {
		t.Errorf("install args = %v", w)
	}
	if r := parseRunnerArgs([]string{"wp", "runner", "--env", "AWS_PROFILE", "--env=GH_HOST"}); r == nil || strings.Join(r.Env, ",") != "AWS_PROFILE,GH_HOST" {
		t.Errorf("parsed = %+v", r)
	}
}

func TestJobsStayInTheAllowedDirectories(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "proj")
	outside := filepath.Join(root, "secret")
	for _, d := range []string{allowed, outside, filepath.Join(allowed, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(allowed, "link")); err != nil {
		t.Fatal(err)
	}
	p := newTestProc(t, allowed)
	for name, dir := range map[string]string{
		"outside":            outside,
		"sibling by prefix":  allowed + "-x",
		"dot dot":            filepath.Join(allowed, "sub", "..", "..", "secret"),
		"symlink out":        filepath.Join(allowed, "link"),
		"relative":           "proj",
		"missing":            filepath.Join(allowed, "nope"),
		"parent of allowed":  root,
		"through a symlink2": filepath.Join(allowed, "link", "."),
	} {
		o := p.execCommand(context.Background(), newRun(time.Minute), commandJob{Command: "touch pwned", Dir: dir})
		if o.State != "failed" {
			t.Errorf("%s: ran (%+v)", name, o)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
		t.Error("a command ran outside the allowed directories")
	}
	if o := p.execCommand(context.Background(), newRun(time.Minute), commandJob{Command: "true", Dir: filepath.Join(allowed, "sub")}); o.State != "done" {
		t.Errorf("a sub directory should work: %+v", o)
	}
	none := newTestProc(t)
	if o := none.execCommand(context.Background(), newRun(time.Minute), commandJob{Command: "true", Dir: allowed}); o.State != "failed" || !strings.Contains(o.Error, "allows no directories") {
		t.Errorf("a runner with no --allow: %+v", o)
	}
}

func TestParseAllowed(t *testing.T) {
	d := t.TempDir()
	got, err := parseAllowed([]string{d})
	real, _ := filepath.EvalSymlinks(d)
	if err != nil || len(got) != 1 || got[0] != real {
		t.Errorf("parseAllowed = %v %v", got, err)
	}
	if _, err := parseAllowed([]string{filepath.Join(d, "missing")}); err == nil {
		t.Error("accepted a missing directory")
	}
	if _, err := parseAllowed([]string{"/"}); err == nil {
		t.Error("accepted the root")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, err := parseAllowed([]string{"~"}); err != nil || len(got) != 1 {
		t.Errorf("~ = %v %v", got, err)
	}
}

// ---- apply ----

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "Repo Owner")
	run("config", "user.email", "owner@example.com")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "b.txt"), []byte("bee\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "first")
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func blob(t *testing.T, dir, path string) string { return gitOut(t, dir, "rev-parse", "HEAD:"+path) }

func TestApplyJobWritesStagesAndCommits(t *testing.T) {
	repo := newGitRepo(t)
	p := newTestProc(t, repo)
	run := newRun(time.Minute)
	o := p.execApply(context.Background(), run, applyJob{Dir: repo, Commit: "Change a, add c, delete b", Files: []fileChange{
		{Path: "a.txt", BaseSHA: blob(t, repo, "a.txt"), Content: "uno\n"},
		{Path: "docs/c.txt", Content: "new\n"},
		{Path: "src/b.txt", BaseSHA: blob(t, repo, "src/b.txt"), Delete: true},
	}})
	if o.State != "done" {
		t.Fatalf("outcome = %+v\nlog: %s", o, run.out.text())
	}
	res := o.Result.(map[string]any)
	sha, _ := res["commit"].(string)
	if sha == "" || sha != gitOut(t, repo, "rev-parse", "HEAD") {
		t.Errorf("result = %v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "uno\n" {
		t.Errorf("a.txt = %q", b)
	}
	if _, err := os.Stat(filepath.Join(repo, "src", "b.txt")); err == nil {
		t.Error("b.txt still there")
	}
	if got := gitOut(t, repo, "log", "-1", "--format=%an <%ae>|%s"); got != "Repo Owner <owner@example.com>|Change a, add c, delete b" {
		t.Errorf("commit = %q (author comes from the repo's git config)", got)
	}
	if got := gitOut(t, repo, "status", "--porcelain"); got != "" {
		t.Errorf("work tree not clean: %q", got)
	}
	// Applying the same change again changes nothing and doesn't fail.
	o = p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: repo, Files: []fileChange{{Path: "a.txt", BaseSHA: "0000000000000000000000000000000000000000", Content: "uno\n"}}})
	if o.State != "done" {
		t.Errorf("re-apply = %+v", o)
	}
	// Nor with a commit asked for: a satisfied retry has nothing to commit.
	head := gitOut(t, repo, "rev-parse", "HEAD")
	o = p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: repo, Commit: "Change a again", Files: []fileChange{{Path: "a.txt", BaseSHA: blob(t, repo, "a.txt"), Content: "uno\n"}}})
	if o.State != "done" || gitOut(t, repo, "rev-parse", "HEAD") != head {
		t.Errorf("re-apply with a commit = %+v", o)
	}
}

func TestAgentJobRefusesRefsGitWouldTakeForOptions(t *testing.T) {
	p := newTestProc(t)
	remote := newGitRepo(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	for name, in := range map[string]agentJob{
		"base":              {Base: "--upload-pack=touch " + marker},
		"branch":            {Branch: "-x"},
		"branch with ..":    {Branch: "a..b"},
		"branch with ~":     {Branch: "a~b"},
		"branch with .lock": {Branch: "a.lock"},
	} {
		in.CLI, in.Prompt, in.Repo = "claude", "x", remote
		o := p.execAgent(context.Background(), newRun(time.Minute), "agent", in)
		if o.State != "failed" || !strings.Contains(o.Error, "valid git") {
			t.Errorf("%s: %+v", name, o)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("an option in a ref reached git")
	}
}

func TestCancelledCommandSettlesItsDelayedKill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := procCommand(ctx, "sleep", "30")
	if err := runProc(cmd); err == nil {
		t.Fatal("sleep 30 ended by itself")
	}
	left := 0
	killTimers.Range(func(k, _ any) bool {
		if k == cmd {
			left++
		}
		return true
	})
	if left != 0 {
		t.Error("the SIGKILL timer outlived the process, to land on a reused pid")
	}
}

func TestSchtasksCommandQuotesPathsWithSpaces(t *testing.T) {
	got := schtasksCommand(`C:\Program Files\wp\wp.exe`, []string{"runner", "--dir", `C:\My Projects`})
	want := `\"C:\Program Files\wp\wp.exe\" runner --dir \"C:\My Projects\"`
	if got != want {
		t.Errorf("got %s", got)
	}
}

func TestApplyJobRefusesWhenAFileChanged(t *testing.T) {
	repo := newGitRepo(t)
	p := newTestProc(t, repo)
	base := blob(t, repo, "a.txt")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("edited meanwhile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: repo, Commit: "x", Files: []fileChange{
		{Path: "src/b.txt", BaseSHA: blob(t, repo, "src/b.txt"), Content: "bee two\n"},
		{Path: "a.txt", BaseSHA: base, Content: "uno\n"},
		{Path: "src/b.txt.new", Content: "x"},
		{Path: "a.txt.orig", BaseSHA: base, Content: "missing base file"},
	}})
	if o.State != "failed" || !strings.Contains(o.Error, "a.txt (changed)") || !strings.Contains(o.Error, "nothing was written") {
		t.Fatalf("outcome = %+v", o)
	}
	conflicts := o.Result.(map[string]any)["conflicts"].([]applyConflict)
	if len(conflicts) != 2 || conflicts[0].Path != "a.txt" || conflicts[1].Reason != "was deleted" {
		t.Errorf("conflicts = %+v", conflicts)
	}
	// Nothing was written, not even the file that was fine.
	if b, _ := os.ReadFile(filepath.Join(repo, "src", "b.txt")); string(b) != "bee\n" {
		t.Errorf("b.txt was written: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "edited meanwhile\n" {
		t.Errorf("a.txt = %q", b)
	}
	if _, err := os.Stat(filepath.Join(repo, "src", "b.txt.new")); err == nil {
		t.Error("a new file was written")
	}
	// A new file where one exists is a conflict too.
	o = p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: repo, Files: []fileChange{{Path: "src/b.txt", Content: "overwrite"}}})
	if o.State != "failed" || !strings.Contains(o.Error, "exists already") {
		t.Errorf("new over existing = %+v", o)
	}
}

func TestApplyJobRefusesPathsOutsideTheDirectory(t *testing.T) {
	root := t.TempDir()
	repo := newGitRepoIn(t, filepath.Join(root, "repo"))
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(repo, "filelink")); err != nil {
		t.Fatal(err)
	}
	p := newTestProc(t, repo)
	for name, path := range map[string]string{
		"parent":        "../outside/x.txt",
		"deep parent":   "src/../../outside/x.txt",
		"absolute":      filepath.Join(outside, "x.txt"),
		"git dir":       ".git/hooks/pre-commit",
		"through link":  "escape/x.txt",
		"onto a link":   "filelink",
		"empty segment": "src//x.txt",
		"backslash":     `src\x.txt`,
	} {
		o := p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: repo, Files: []fileChange{{Path: path, Content: "pwned"}}})
		if o.State != "failed" {
			t.Errorf("%s: applied (%+v)", name, o)
		}
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Errorf("files appeared outside: %v", entries)
	}
	// And the directory itself must be allowed.
	other := newGitRepo(t)
	if o := p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: other, Files: []fileChange{{Path: "z.txt", Content: "z"}}}); o.State != "failed" {
		t.Errorf("applied in a directory the runner doesn't allow: %+v", o)
	}
}

// A request for a file outside any repo (WP-115) arrives as an apply job in
// the file's own directory: a plain directory the runner allows. The same
// rules hold: the base is checked, nothing is committed, a directory that
// isn't allowed is refused, and the job can't reach beyond its named file.
func TestApplyJobInAPlainAllowedDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Org\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, base := outsideBase(filepath.Join(dir, "CLAUDE.md"))
	p := newTestProc(t, dir)
	o := p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: dir, Files: []fileChange{{Path: "CLAUDE.md", BaseSHA: base, Content: "# Org\nbe nice\n"}}})
	if o.State != "done" {
		t.Fatalf("apply: %+v", o)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != "# Org\nbe nice\n" {
		t.Errorf("file: %q", b)
	}
	// Changed since: not written.
	o = p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: dir, Files: []fileChange{{Path: "CLAUDE.md", BaseSHA: base, Content: "other\n"}}})
	if o.State != "failed" {
		t.Errorf("stale base applied: %+v", o)
	}
	// Not an allowed directory, and not above the named file.
	elsewhere := t.TempDir()
	if o := p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: elsewhere, Files: []fileChange{{Path: "x.md", Content: "x"}}}); o.State != "failed" {
		t.Errorf("applied in a directory the runner doesn't allow: %+v", o)
	}
	if o := p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: dir, Files: []fileChange{{Path: "../x.md", Content: "x"}}}); o.State != "failed" {
		t.Errorf("escaped the directory: %+v", o)
	}
}

func newGitRepoIn(t *testing.T, dir string) string {
	t.Helper()
	made := newGitRepo(t)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(made, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestApplyJobWontSweepUpStagedWork(t *testing.T) {
	repo := newGitRepo(t)
	p := newTestProc(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "mine.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "mine.txt")
	o := p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: repo, Commit: "x", Files: []fileChange{{Path: "n.txt", Content: "n"}}})
	if o.State != "failed" || !strings.Contains(o.Error, "staged") {
		t.Errorf("outcome = %+v", o)
	}
	if _, err := os.Stat(filepath.Join(repo, "n.txt")); err == nil {
		t.Error("wrote anyway")
	}
}

func TestApplyJobPushes(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitOut(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", remote)
	repo := newGitRepo(t)
	gitOut(t, repo, "remote", "add", "origin", remote)
	gitOut(t, repo, "push", "-q", "-u", "origin", "main")
	p := newTestProc(t, repo)
	o := p.execApply(context.Background(), newRun(time.Minute), applyJob{Dir: repo, Commit: "add d", Push: true, Files: []fileChange{{Path: "d.txt", Content: "d"}}})
	if o.State != "done" || o.Result.(map[string]any)["pushed"] != true {
		t.Fatalf("outcome = %+v", o)
	}
	if got := gitOut(t, remote, "log", "-1", "--format=%s", "main"); got != "add d" {
		t.Errorf("remote head = %q", got)
	}
}

// ---- agent ----

const fakeClaude = `#!/bin/sh
# A stand-in for claude: records how it was run, edits the checkout, answers.
out="$FAKE_OUT"
echo "$@" > "$out/args"
cat > "$out/stdin"
env | grep -c '^ANTHROPIC_API_KEY=' > "$out/key" || true
env | grep '^WAYPOINT_TOKEN=' > "$out/token" || true
pwd > "$out/cwd"
case "$*" in
  *--allowedTools*) ;;
  *) echo "made by claude" > made-by-claude.txt ;;
esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"Writing the file"},{"type":"tool_use","name":"Write","input":{"file_path":"made-by-claude.txt"}}]}}'
printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"All done: wrote the file","structured_output":{"verdict":"fine"},"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000},"total_cost_usd":0.0123}'
`

// fakeCLI puts a script called name on PATH and returns the directory it
// records into.
func fakeCLI(t *testing.T, name, script string) string {
	t.Helper()
	bin := t.TempDir()
	rec := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_OUT", rec)
	passTestEnv(t, "FAKE_OUT")
	return rec
}

// passTestEnv lets jobs see a test's variable, as a runner started with
// --env NAME does (WP-418).
func passTestEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if runnerEnvPass[n] {
			continue
		}
		runnerEnvPass[n] = true
		t.Cleanup(func() { delete(runnerEnvPass, n) })
	}
}

func TestAgentJobWithClaude(t *testing.T) {
	rec := fakeCLI(t, "claude", fakeClaude)
	t.Setenv("ANTHROPIC_API_KEY", "sk-must-not-reach-claude")
	remote := newGitRepo(t)
	gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
	p := newTestProc(t)
	run := newRun(time.Minute)
	o := p.execAgent(context.Background(), run, "agent", agentJob{
		CLI: "claude", Model: "sonnet", Prompt: "Add a file.", Repo: remote, Branch: "agent/add-file", Push: true,
		JSONSchema: `{"type":"object"}`, CommitMessage: "Add the file",
	})
	if o.State != "done" {
		t.Fatalf("outcome = %+v\nlog:\n%s", o, run.out.text())
	}
	res := o.Result.(map[string]any)
	if res["answer"] != "All done: wrote the file" || res["branch"] != "agent/add-file" || res["pushed"] != true {
		t.Errorf("result = %v", res)
	}
	commits := res["commits"].([]string)
	if len(commits) != 1 || res["commit"] != commits[0] {
		t.Errorf("commits = %v", commits)
	}
	if d, _ := res["diff"].(string); !strings.Contains(d, "+made by claude") {
		t.Errorf("diff = %q", d)
	}
	if string(res["structured"].(json.RawMessage)) != `{"verdict":"fine"}` {
		t.Errorf("structured = %s", res["structured"])
	}
	tok := res["tokens"].(tokenUse)
	if tok.Input != 110 || tok.Output != 5 || tok.CacheRead != 1000 || tok.CostUSD != 0.0123 {
		t.Errorf("tokens = %+v", tok)
	}
	// The CLI was run as designed.
	args, _ := os.ReadFile(filepath.Join(rec, "args"))
	for _, want := range []string{"-p", "--output-format stream-json", "--model sonnet", "--json-schema", "--permission-mode acceptEdits", "--setting-sources user", "--strict-mcp-config", "--settings " + unattendedSettings} {
		if !strings.Contains(string(args), want) {
			t.Errorf("claude was run with %q, missing %q", args, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(rec, "stdin")); string(b) != "Add a file." {
		t.Errorf("prompt on stdin = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(rec, "key")); strings.TrimSpace(string(b)) != "0" {
		t.Error("ANTHROPIC_API_KEY reached claude")
	}
	// What it left was committed on the branch and pushed.
	if got := gitOut(t, remote, "log", "-1", "--format=%s", "agent/add-file"); got != "Add the file" {
		t.Errorf("pushed commit = %q", got)
	}
	if got := gitOut(t, remote, "show", "agent/add-file:made-by-claude.txt"); got != "made by claude" {
		t.Errorf("pushed file = %q", got)
	}
	// The log shows what it did, and the work tree is gone.
	if l := run.out.text(); !strings.Contains(l, "Writing the file") || !strings.Contains(l, "→ Write made-by-claude.txt") || strings.Contains(l, "tool_result") {
		t.Errorf("log = %q", l)
	}
	if left, _ := os.ReadDir(filepath.Join(p.cacheDir(), "work")); len(left) != 0 {
		t.Errorf("work trees left: %v", left)
	}
	if _, err := os.Stat(filepath.Join(p.cacheDir(), "local")); err != nil {
		t.Errorf("no cached clone: %v", err)
	}
}

func TestReviewJobIsReadOnly(t *testing.T) {
	rec := fakeCLI(t, "claude", fakeClaude)
	remote := newGitRepo(t)
	p := newTestProc(t)
	// Even a job that asks for a branch and a push gets neither.
	o := p.execAgent(context.Background(), newRun(time.Minute), "review", agentJob{CLI: "claude", Prompt: "Review it.", Repo: remote, Branch: "x", Push: true, JSONSchema: `{"type":"object"}`})
	if o.State != "done" {
		t.Fatalf("outcome = %+v", o)
	}
	res := o.Result.(map[string]any)
	if len(res["commits"].([]string)) != 0 || res["pushed"] != nil || res["branch"] != nil {
		t.Errorf("a review changed something: %v", res)
	}
	args, _ := os.ReadFile(filepath.Join(rec, "args"))
	if !strings.Contains(string(args), "--tools Read,Grep,Glob --permission-mode dontAsk --allowedTools Read(//") || strings.Contains(string(args), "acceptEdits") {
		t.Errorf("review args = %s", args)
	}
	if got := gitOut(t, remote, "branch", "--list", "x"); got != "" {
		t.Errorf("branch pushed: %q", got)
	}
}

func TestAgentJobFailures(t *testing.T) {
	p := newTestProc(t)
	remote := newGitRepo(t)
	// Not installed.
	gitPath, _ := exec.LookPath("git")
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+filepath.Dir(gitPath)) // git, but no claude
	if o := p.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "claude", Prompt: "x", Repo: remote}); o.State != "failed" || !strings.Contains(o.Error, "isn't installed") {
		t.Errorf("not installed: %+v", o)
	}
	// Signed out (as the last detection found).
	fakeCLI(t, "claude", fakeClaude)
	no := false
	p.clis = []runnerCLI{{Name: "claude", SignedIn: &no}}
	if o := p.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "claude", Prompt: "x", Repo: remote}); o.State != "failed" || !strings.Contains(o.Error, "claude auth login") {
		t.Errorf("signed out: %+v", o)
	}
	p.clis = nil
	// Clone URLs git would run commands for, local paths in production.
	prod := newTestProc(t)
	prod.opts.AllowLocalRepos = false
	for _, repo := range []string{"ext::sh -c touch /tmp/pwned", "--upload-pack=x", remote, "file:///etc"} {
		if o := prod.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "claude", Prompt: "x", Repo: repo}); o.State != "failed" {
			t.Errorf("cloned %q: %+v", repo, o)
		}
	}
	// A base that isn't there.
	if o := p.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "claude", Prompt: "x", Repo: remote, Base: "no-such-branch"}); o.State != "failed" || !strings.Contains(o.Error, "no-such-branch") {
		t.Errorf("missing base: %+v", o)
	}
}

func TestAgentJobStartsFromTheBaseRef(t *testing.T) {
	fakeCLI(t, "claude", fakeClaude)
	remote := newGitRepo(t)
	gitOut(t, remote, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(remote, "feature.txt"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, remote, "add", "-A")
	gitOut(t, remote, "commit", "-q", "-m", "feature work")
	gitOut(t, remote, "checkout", "-q", "main")
	p := newTestProc(t)
	run := newRun(time.Minute)
	o := p.execAgent(context.Background(), run, "review", agentJob{CLI: "claude", Prompt: "look", Repo: remote, Base: "feature"})
	if o.State != "done" {
		t.Fatalf("outcome = %+v", o)
	}
	if o.Result.(map[string]any)["base"] != gitOut(t, remote, "rev-parse", "feature") {
		t.Errorf("base = %v", o.Result.(map[string]any)["base"])
	}
	// A second job fetches rather than cloning again, and sees new commits.
	if err := os.WriteFile(filepath.Join(remote, "later.txt"), []byte("l"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, remote, "add", "-A")
	gitOut(t, remote, "commit", "-q", "-m", "later")
	run2 := newRun(time.Minute)
	o = p.execAgent(context.Background(), run2, "review", agentJob{CLI: "claude", Prompt: "look", Repo: remote})
	if o.State != "done" || o.Result.(map[string]any)["base"] != gitOut(t, remote, "rev-parse", "main") {
		t.Fatalf("second job = %+v", o)
	}
	if strings.Contains(run2.out.text(), "Cloning") {
		t.Errorf("cloned again: %s", run2.out.text())
	}
}

// OpenCode Zen's "Free usage limit reached" prints no event: opencode waits
// silently, so the runner reads its log on standard error and stops it at
// once, saying why, instead of at the job's time limit (WP-861).
func TestOpencodeStopsOnSpentModel(t *testing.T) {
	remote := newGitRepo(t)
	p := newTestProc(t)
	fakeCLI(t, "opencode", `#!/bin/sh
case "$*" in *"--help"*) echo "  --print-logs  print logs to stderr"; echo "  --format  format: default or json"; exit 0;; esac
echo "$@" > "$FAKE_OUT/args"
echo 'timestamp=2026-11-10T21:23:20.903Z level=ERROR run=5db2b547 message="stream error" providerID=opencode modelID=deepseek-v4.1-flash-free small=true agent=title mode=primary error.error="AI_RetryError: Failed after 3 attempts. Last error: Free usage limit reached."' >&2
echo 'timestamp=2026-11-10T21:23:16.360Z level=ERROR run=5db2b547 message="stream error" providerID=opencode modelID=deepseek-v4.1-flash-free small=false agent=plan mode=primary error.error="AI_APICallError: Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"' >&2
exec sleep 60
`)
	run := newRun(time.Minute)
	start := time.Now()
	o := p.execAgent(context.Background(), run, "audit", agentJob{CLI: "opencode", Model: "opencode/deepseek-v4.1-flash-free", Prompt: "Audit", Repo: remote})
	if o.State != "failed" || !strings.Contains(o.Error, "Free usage limit reached. Free models work in OpenCode") || strings.Contains(o.Error, "timed out") {
		t.Fatalf("opencode = %+v\n%s", o, run.out.text())
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("stopped after %s, not at once", d)
	}
	if a, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_OUT"), "args")); !strings.Contains(string(a), "--print-logs --log-level WARN") {
		t.Errorf("opencode args = %s", a)
	}
}

func TestOpencodeOutOfUsage(t *testing.T) {
	for line, want := range map[string]bool{
		`level=ERROR message="stream error" small=false error.error="AI_APICallError: Free usage limit reached. Subscribe."`: true,
		`level=ERROR message="stream error" small=true error.error="AI_RetryError: Free usage limit reached."`:               false, // the title call
		`level=ERROR message="stream error" small=false error.error="AI_APICallError: 429 Too Many Requests"`:                false, // opencode retries
		`level=WARN message="stream error" small=false error.error="quota exceeded"`:                                         false,
		`level=ERROR message="stream error" small=false error.error="You exceeded your current quota"`:                       true,
	} {
		if got := opencodeOutOfUsage([]byte(line)) != nil; got != want {
			t.Errorf("opencodeOutOfUsage(%s) = %v, want %v", line, got, want)
		}
	}
}

func TestOpencodeAndCodexOutput(t *testing.T) {
	remote := newGitRepo(t)
	p := newTestProc(t)

	fakeCLI(t, "opencode", `#!/bin/sh
case "$*" in *"--help"*) echo "  --format  format: default or json"; exit 0;; esac
cat > "$FAKE_OUT/stdin"
echo "$@" > "$FAKE_OUT/args"
echo '{"type":"step_start"}'
echo '{"type":"text","part":{"text":"Looking around"}}'
echo '{"type":"tool_use","part":{"tool":"read","state":{"input":{"filePath":"a.txt"}}}}'
echo '{"type":"text","part":{"text":"The answer is {\"ok\":true}"}}'
echo '{"type":"step_finish","part":{"tokens":{"input":7,"output":3,"cache":{"read":2}},"cost":0}}'
`)
	run := newRun(time.Minute)
	o := p.execAgent(context.Background(), run, "agent", agentJob{CLI: "opencode", Model: "opencode/muse-spark-1.3-contributor-free", Prompt: "Hello", Repo: remote, ReadOnly: true})
	if o.State != "done" {
		t.Fatalf("opencode = %+v\n%s", o, run.out.text())
	}
	res := o.Result.(map[string]any)
	if res["answer"] != `The answer is {"ok":true}` || res["tokens"].(tokenUse).Input != 7 {
		t.Errorf("result = %v", res)
	}
	rec := os.Getenv("FAKE_OUT")
	if a, _ := os.ReadFile(filepath.Join(rec, "args")); !strings.Contains(string(a), "--model opencode/muse-spark-1.3-contributor-free --agent plan --format json") || !strings.Contains(string(a), "run --dir ") {
		t.Errorf("opencode args = %s", a)
	}

	fakeCLI(t, "codex", `#!/bin/sh
cat > "$FAKE_OUT/stdin"
echo "$@" > "$FAKE_OUT/args"
echo '{"type":"item.completed","item":{"type":"command_execution","command":"ls"}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"{\"n\":1}"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":50,"cached_input_tokens":40,"output_tokens":9}}'
`)
	p2 := newTestProc(t)
	o = p2.execAgent(context.Background(), newRun(time.Minute), "review", agentJob{CLI: "codex", Prompt: "Review", Repo: remote, JSONSchema: `{"type":"object"}`})
	if o.State != "done" {
		t.Fatalf("codex = %+v", o)
	}
	res = o.Result.(map[string]any)
	if res["answer"] != `{"n":1}` || string(res["structured"].(json.RawMessage)) != `{"n":1}` || res["tokens"].(tokenUse).Input != 10 || res["tokens"].(tokenUse).CacheRead != 40 {
		t.Errorf("codex result = %v", res)
	}
	rec = os.Getenv("FAKE_OUT")
	if a, _ := os.ReadFile(filepath.Join(rec, "args")); !strings.Contains(string(a), "exec --json") || !strings.Contains(string(a), "--sandbox read-only") || !strings.Contains(string(a), "--output-schema") {
		t.Errorf("codex args = %s", a)
	}
}

func TestCursorOutput(t *testing.T) {
	remote := newGitRepo(t)
	// The repository's own permissions, which a read-only job must not keep.
	if err := os.MkdirAll(filepath.Join(remote, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, ".cursor", "cli.json"), []byte(`{"permissions":{"allow":["Shell(*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, remote, "add", "-A")
	gitOut(t, remote, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "cursor config")
	fakeCLI(t, "cursor-agent", `#!/bin/sh
echo "$@" > "$FAKE_OUT/args"
cp .cursor/cli.json "$FAKE_OUT/cli.json"
echo '{"type":"result","subtype":"success","is_error":false,"result":"### 1. A finding","session_id":"s1","usage":{"inputTokens":120,"outputTokens":30,"cacheReadTokens":900,"cacheWriteTokens":80}}'
`)
	p := newTestProc(t)
	o := p.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "cursor", Model: "grok-4.7-high", Prompt: "Audit", Repo: remote, ReadOnly: true})
	if o.State != "done" {
		t.Fatalf("cursor = %+v", o)
	}
	res := o.Result.(map[string]any)
	if tk := res["tokens"].(tokenUse); res["answer"] != "### 1. A finding" || tk.Input != 200 || tk.Output != 30 || tk.CacheRead != 900 {
		t.Errorf("cursor result = %v", res)
	}
	a, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_OUT"), "args"))
	// Read-only is Cursor's ask mode; the fresh checkout is trusted, or print
	// mode stops to ask.
	if !strings.Contains(string(a), "-p --output-format json --trust --model grok-4.7-high --mode ask Audit") || strings.Contains(string(a), "--force") {
		t.Errorf("cursor args = %s", a)
	}
	// Reading is all it may do: the checkout's tool rules are replaced.
	c, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_OUT"), "cli.json"))
	if !strings.Contains(string(c), `"Shell(*)"`) || !strings.Contains(string(c), `"deny":[`) {
		t.Errorf("cursor tool rules do not deny Shell: %s", c)
	}

	fakeCLI(t, "cursor-agent", `#!/bin/sh
echo "$@" > "$FAKE_OUT/args"
cp .cursor/cli.json "$FAKE_OUT/cli.json"
echo '{"type":"result","subtype":"error","is_error":true,"result":"You have hit your usage limit"}'
`)
	p2 := newTestProc(t)
	o = p2.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "cursor", Prompt: "Fix it", Repo: remote, Permission: "edit"})
	if o.State != "failed" || !strings.Contains(o.Error, "usage limit") {
		t.Errorf("cursor error = %+v", o)
	}
	if a, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_OUT"), "args")); !strings.Contains(string(a), "--trust --force Fix it") {
		t.Errorf("cursor edit args = %s", a)
	}
	if c, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_OUT"), "cli.json")); string(c) != `{"permissions":{"allow":["Shell(*)"]}}` {
		t.Errorf("edit job changed the repository Cursor config: %s", c)
	}
}

func TestLogSinkBatchesAndKeepsOffsets(t *testing.T) {
	l := newLogSink()
	_, _ = l.Write([]byte("héllo "))
	_, _ = l.Write([]byte{0xe2, 0x82}) // half of a euro sign
	off, text, n, _ := l.chunk()
	if off != 0 || text != "héllo " || n != 7 {
		t.Errorf("first chunk = %d %q %d (the half character waits)", off, text, n)
	}
	l.acked(n, text, false)
	_, _ = l.Write([]byte{0xac, 'x', 0xff, 'y'})
	l.close()
	off, text, n, last := l.chunk()
	if off != int64(len("héllo ")) || text != "€x\uFFFDy" || !last {
		t.Errorf("second chunk = %d %q %d %v", off, text, n, last)
	}
	l.acked(n, text, false)
	if l.sent != int64(len("héllo €x\uFFFDy")) {
		t.Errorf("sent = %d", l.sent)
	}
}

func TestRunnerServiceFiles(t *testing.T) {
	o := runnerOpts{Name: "my laptop", Labels: []string{"gpu"}, Allow: []string{"/home/me/my projects", "/srv/ci"}, MaxJobs: 2}
	args := runnerArgs(o)
	unit := systemdUnit("/home/me/.local/bin/wp", args, map[string]string{"PATH": "/home/me/.local/bin:/usr/bin", "WAYPOINT_CONFIG_DIR": ""}, "https://wp.example.com")
	for _, want := range []string{
		`ExecStart=/home/me/.local/bin/wp runner --name "my laptop" --label gpu --allow "/home/me/my projects" --allow /srv/ci --max-jobs 2`,
		`Environment=PATH=/home/me/.local/bin:/usr/bin`, "Restart=on-failure", "WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "WAYPOINT_CONFIG_DIR") {
		t.Error("empty config dir written")
	}
	plist := launchdPlist("/usr/local/bin/wp", []string{"runner", "--name", "a&b"}, map[string]string{"PATH": "/usr/bin"}, "/tmp/log")
	for _, want := range []string{"<string>/usr/local/bin/wp</string>", "<string>a&amp;b</string>", "<key>SuccessfulExit</key><false/>", "<key>PATH</key><string>/usr/bin</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if systemdQuote(`a"b%c$d`) != `"a\"b%%c$$d"` {
		t.Errorf("quote = %s", systemdQuote(`a"b%c$d`))
	}
}

func TestInstallWritesTheUnitAndStartsIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("no systemctl")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_RUNTIME_DIR", cfg) // set, so ensureUserBus leaves it
	var calls []string
	old, oldWait := sysRun, onlineWait
	sysRun = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "loginctl" {
			return "Linger=no", nil
		}
		return "", nil
	}
	onlineWait = 0
	defer func() { sysRun, onlineWait = old, oldWait }()
	var out bytes.Buffer
	c := &cli{out: &out}
	t.Setenv("WAYPOINT_SERVER", "http://127.0.0.1:1")
	if err := installSystemd(c, "/usr/local/bin/wp", runnerOpts{Name: "box", MaxJobs: 1, Allow: []string{"/srv"}}, map[string]string{"PATH": "/usr/bin"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg, "systemd", "user", unitName))
	if err != nil || !strings.Contains(string(b), "ExecStart=/usr/local/bin/wp runner --name box --allow /srv") {
		t.Errorf("unit = %s (%v)", b, err)
	}
	want := []string{"systemctl --user daemon-reload", "systemctl --user enable --now " + unitName, "systemctl --user restart " + unitName}
	if strings.Join(calls[:3], "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v", calls)
	}
	if !strings.Contains(out.String(), "sudo loginctl enable-linger") || !strings.Contains(out.String(), "journalctl --user -u waypoint-runner") {
		t.Errorf("output = %s", out.String())
	}
	out.Reset()
	if err := cmdRunnerUninstall(c, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "systemd", "user", unitName)); err == nil {
		t.Error("unit still there")
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("uninstall said %q", out.String())
	}
}

// alive: the process exists and isn't a zombie. In a container whose PID 1
// doesn't reap orphans, a killed child stays a zombie, which signal 0 still
// reaches.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // no /proc (macOS): signal 0 is all there is
	}
	// The state follows the command name in parentheses: "pid (comm) Z …".
	if i := strings.LastIndexByte(string(b), ')'); i >= 0 && i+2 < len(b) {
		return b[i+2] != 'Z'
	}
	return true
}

func TestCapDiffCutsAtALine(t *testing.T) {
	if got := capDiff("+a\n+b\n", 100); got != "+a\n+b\n" {
		t.Errorf("short diff changed: %q", got)
	}
	if got := capDiff("+aaa\n+bbb\n+ccc\n", 8); got != "+aaa\n… (10 more bytes of diff)\n" {
		t.Errorf("capped = %q", got)
	}
}

func TestInstallKeepsTheInstalledRunnersSettings(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	unit := systemdUnit("/usr/local/bin/wp", []string{"runner", "--name", "devhost", "--allow", "/projects", "--allow", "/srv/my dir", "--no-prep"}, nil, "http://x")
	if err := os.MkdirAll(filepath.Join(cfg, "systemd", "user"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "systemd", "user", unitName), []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := &cli{out: &out}
	o := runnerOpts{MaxJobs: 3, given: map[string]bool{"max-jobs": true}}
	keepInstalled(c, &o)
	if o.Name != "devhost" || strings.Join(o.Allow, "|") != "/projects|/srv/my dir" || o.MaxJobs != 3 || !o.NoPrep {
		t.Errorf("opts = %+v", o)
	}
	if !strings.Contains(out.String(), "--name, --allow") {
		t.Errorf("said %q", out.String())
	}

	// Saved settings come first; given flags win; --allow none clears.
	c.state.Runner = &runnerInstalled{Name: "saved", Allow: []string{"/a"}, MaxJobs: 2}
	o = runnerOpts{MaxJobs: 1, Name: "new", Allow: []string{"none"}, given: map[string]bool{"name": true, "allow": true}}
	keepInstalled(c, &o)
	if o.Name != "new" || o.Allow != nil || o.MaxJobs != 2 {
		t.Errorf("opts = %+v", o)
	}
}

func TestEnsureUserBus(t *testing.T) {
	base := t.TempDir()
	old := runtimeDirBase
	runtimeDirBase = base
	defer func() { runtimeDirBase = old }()
	t.Setenv("XDG_RUNTIME_DIR", "")
	ensureUserBus()
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		t.Fatalf("set to %q with no bus", v)
	}
	dir := filepath.Join(base, fmt.Sprint(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ensureUserBus()
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != dir {
		t.Errorf("XDG_RUNTIME_DIR = %q", v)
	}
}

// An agent that may run commands reaches Waypoint with the job's own token;
// one that can't run commands, or is confined, gets no token, and none ever
// gets the runner's (WP-418).
func TestAgentJobToken(t *testing.T) {
	rec := fakeCLI(t, "claude", fakeClaude)
	t.Setenv("WAYPOINT_TOKEN", "wpt_runner")
	remote := newGitRepo(t)
	p := newTestProc(t)
	token := func(job agentJob) string {
		t.Helper()
		run := newRun(time.Minute)
		run.token = "wpt_job"
		job.CLI, job.Prompt, job.Repo = "claude", "x", remote
		if o := p.execAgent(context.Background(), run, "agent", job); o.State != "done" {
			t.Fatalf("%+v: %+v\n%s", job, o, run.out.text())
		}
		b, _ := os.ReadFile(filepath.Join(rec, "token"))
		return strings.TrimSpace(string(b))
	}
	if got := token(agentJob{Permission: "edit_run"}); got != "WAYPOINT_TOKEN=wpt_job" {
		t.Errorf("edit_run: %q", got)
	}
	if got := token(agentJob{Permission: "edit"}); got != "" {
		t.Errorf("edit: %q", got)
	}
	if got := token(agentJob{Permission: "edit_run", Confine: true}); got != "" {
		t.Errorf("confined: %q", got)
	}
}
