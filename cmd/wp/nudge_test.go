package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Each nudge fires on the commands it is for and stays quiet on the near
// misses; a nudge is only ever a note.
func TestNudgeRules(t *testing.T) {
	const (
		wait    = "sleep-poll"
		slept   = "sleep-wait"
		api     = "forge-api"
		gh      = "gh-on-forgejo"
		sweep   = "branch-sweep"
		forgejo = "git.example.net"
	)
	for _, c := range []struct {
		cmd, host, want string
	}{
		// polling CI
		{`while true; do curl -s https://git.example.net/api/v1/repos/a/b/commits/x/status | jq .state; sleep 30; done`, forgejo, wait},
		{`until gh pr checks 12 | grep -q pass; do sleep 20; done`, "github.com", wait},
		{`for i in $(seq 1 40); do wp pr show 12 | grep -q ready && break; sleep 15; done`, forgejo, wait},
		{`while ! git ls-remote origin refs/heads/x | grep -q .; do sleep 5; done`, forgejo, wait},
		{`python3 /usr/local/lib/ci_wait.py 12 --merge`, forgejo, wait},
		{`ci_wait.py --pr 3`, "", wait},
		{`wp pr show 5 && while true; do sleep 60; wget -qO- https://example.com/x; done`, forgejo, wait},
		{`sleep 595; wp pr ls | grep -E "#(40|41)"`, forgejo, wait},
		{`sleep 1m; gh pr checks 3`, "github.com", wait},
		// a long sleep before a check (WP-191)
		{`sleep 598; tail -c 300 /tmp/w37.out`, forgejo, slept},
		{`sleep 2m && make status`, forgejo, slept},
		{`sleep 20; sleep 15; cat out.log`, forgejo, slept},
		{`sleep 0.5h`, forgejo, slept},
		{`sleep 20; go test ./...`, forgejo, ""},
		{`until [ -s out ]; do sleep 60; done`, forgejo, ""},
		{`docker compose up -d; sleep 8; curl -s localhost:1/health`, forgejo, ""},
		{`python3 -c "import time; time.sleep(60)"`, forgejo, ""},
		// not polling
		{`sleep 5 && echo done`, forgejo, ""},
		{`while read l; do echo $l; done < file`, forgejo, ""},
		{`for f in *.go; do gofmt -l $f; sleep 1; done`, forgejo, ""},
		{`wp pr wait 12`, forgejo, ""},
		{`while true; do sleep 30; wp pr wait 12; done`, forgejo, ""},
		{`make check`, forgejo, ""},
		{`echo ci_waiting`, forgejo, ""},
		// the forge API for pulls, statuses, commits
		{`curl -s -H "Authorization: token $T" https://git.example.net/api/v1/repos/Robin/waypoint/pulls/12`, forgejo, api},
		{`curl https://git.example.net/api/v1/repos/Robin/waypoint/commits/abc/statuses`, forgejo, api},
		{`curl -s https://api.github.com/repos/o/r/pulls?state=open | jq`, "github.com", api},
		{`wget -qO- https://api.github.com/repos/o/r/commits/main/status`, "github.com", api},
		{`cd x && curl -s https://git.example.net/api/v1/repos/a/b/pulls`, forgejo, api},
		{`curl -s https://git.example.net/api/v1/repos/a/b/issues/3`, forgejo, ""},
		{`curl -s https://api.github.com/repos/o/r/releases/latest`, "github.com", ""},
		{`curl -s https://example.com/pulls`, forgejo, ""},
		{`git log --grep "api/v1/repos/a/b/pulls"`, forgejo, ""},
		{`curl -s https://git.example.net/api/v1/user`, forgejo, ""},
		// gh pr off github
		{`gh pr view 12`, forgejo, gh},
		{`cd repo && gh pr create --fill`, forgejo, gh},
		{`gh pr list | head`, "git.example.org", gh},
		{`gh pr view 12`, "github.com", ""},
		{`gh pr view 12`, "", ""},
		{`git commit -m "fix gh pr merge text"`, forgejo, ""},
		{`gh issue list`, forgejo, ""},
		{`echo gh pr`, forgejo, ""},
		// branch sweeps
		{`git branch -D a b c`, forgejo, sweep},
		{`git branch -D old-1 old-2`, forgejo, sweep},
		{`git branch --merged main | xargs git branch -D`, forgejo, sweep},
		{`for b in $(git branch --merged); do git branch -D $b; done`, forgejo, sweep},
		{`git worktree remove ../a && git worktree remove ../b`, forgejo, sweep},
		{`for w in ../wt-*; do git worktree remove $w; done`, forgejo, sweep},
		{`git worktree list --porcelain | grep ^worktree | xargs -n1 git worktree remove`, forgejo, sweep},
		{`git branch -D one`, forgejo, ""},
		{`git branch -d one two`, forgejo, ""},
		{`git worktree remove ../a`, forgejo, ""},
		{`git branch --merged`, forgejo, ""},
		{`wp tidy --apply`, forgejo, ""},
	} {
		r := nudgeFor(c.cmd, func() string { return c.host })
		got := ""
		if r != nil {
			got = r.kind
		}
		if got != c.want {
			t.Errorf("%q (remote %q): nudge %q, want %q", c.cmd, c.host, got, c.want)
		}
	}
	// A giant command (a heredoc of code) is never matched.
	if r := nudgeFor("sleep 1; while true; do curl x; done\n"+strings.Repeat("a", 30000), func() string { return "" }); r != nil {
		t.Errorf("huge command nudged: %s", r.kind)
	}
	// The remote is only looked up for the rules that need it.
	looked := 0
	nudgeFor("make check", func() string { looked++; return "" })
	nudgeFor("gh pr view 1", func() string { looked++; return "" })
	if looked != 1 {
		t.Errorf("remote looked up %d times", looked)
	}
	for _, r := range nudgeRules {
		if r.text == "" || strings.Contains(r.text, "\n") || r.kind == "" {
			t.Errorf("rule %q: text %q", r.kind, r.text)
		}
	}
}

func TestRemoteHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://git.example.net/Robin/waypoint.git": "git.example.net",
		"git@github.com:owner/repo.git":              "github.com",
		"ssh://git@GitHub.com/o/r":                   "github.com",
		"https://user:tok@git.example.org/o/r":       "git.example.org",
		"":                                           "",
	} {
		if got := remoteHost(in); got != want {
			t.Errorf("remoteHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// The hook says its note as context and never refuses (it only allows a picture Read, read_pictures_test.go); counts what it showed.
func TestNudgeHook(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	c := &cli{}
	run := func(tool, command string) string {
		in, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": tool, "cwd": t.TempDir(),
			"tool_input": map[string]string{"command": command}})
		var out bytes.Buffer
		hookClaudeNudge(c, bytes.NewReader(in), &out)
		return out.String()
	}
	out := run("Bash", "git branch -D a b")
	var o struct {
		Specific struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
		Decision string `json:"permissionDecision"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.Specific.Event != "PreToolUse" || !strings.Contains(o.Specific.Context, "wp tidy") || o.Decision != "" ||
		strings.Contains(out, "deny") || strings.Contains(out, "permissionDecision") {
		t.Fatalf("output %q (%v)", out, err)
	}
	if got := run("Bash", "ls"); got != "" {
		t.Errorf("a quiet command was nudged: %q", got)
	}
	if got := run("Edit", "git branch -D a b"); got != "" {
		t.Errorf("a non-Bash tool was nudged: %q", got)
	}
	var quiet bytes.Buffer
	hookClaudeNudge(c, strings.NewReader("not json"), &quiet)
	hookClaudeNudge(c, strings.NewReader(`{"tool_name":"Bash","tool_input":"x"}`), &quiet)
	if quiet.Len() != 0 {
		t.Errorf("bad input answered: %q", quiet.String())
	}
	run("Bash", "git branch -D c d")
	if st := c.state.Nudges["branch-sweep"]; st == nil || st.Shown != 2 || time.Since(st.Last) > time.Minute {
		t.Errorf("nudge counts = %+v", c.state.Nudges)
	}
}

// It must be fast: it runs before every Bash call. The fastest of several
// batches against a loose bound, so a machine loaded by the other packages'
// tests doesn't fail it; a pattern that backtracks would still be far over.
func TestNudgeIsFast(t *testing.T) {
	if raceEnabled {
		t.Skip("timing check; the race detector slows it 10x")
	}
	cmd := strings.Repeat("echo hello world; ", 500) + "while true; do sleep 3; done"
	best := time.Hour
	for range 10 {
		start := time.Now()
		for range 20 {
			nudgeFor(cmd, func() string { return "git.example.net" })
		}
		best = min(best, time.Since(start)/20)
	}
	if best > 50*time.Millisecond {
		t.Errorf("%v per command", best)
	}
}

// Big reads: a whole-file Read, cat or wide sed of a large file gets a note;
// small files, limited reads and piped cats stay quiet.
func TestBigReadNudges(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, lines int) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("line of text\n", lines)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("big.txt", 900)
	write("small.txt", 100)
	write("huge.txt", 50000) // beyond the sample: estimated
	for _, c := range []struct {
		tool, path string
		limit      int
		cmd, want  string
	}{
		{"Read", "big.txt", 0, "", "big-read"},
		{"Read", filepath.Join(dir, "huge.txt"), 0, "", "big-read"},
		{"Read", "big.txt", 100, "", ""},
		{"Read", "small.txt", 0, "", ""},
		{"Read", "missing.txt", 0, "", ""},
		{"Read", "", 0, "", ""},
		{"Bash", "", 0, "cat big.txt", "big-cat"},
		{"Bash", "", 0, "cd x && cat -n big.txt", "big-cat"},
		{"Bash", "", 0, "cat huge.txt; echo", "big-cat"},
		{"Bash", "", 0, "cat small.txt", ""},
		{"Bash", "", 0, "cat big.txt | head -50", ""},
		{"Bash", "", 0, "cat big.txt | grep x", ""},
		{"Bash", "", 0, "cat big.txt > out", ""},
		{"Bash", "", 0, "cat /dev/null", ""},
		{"Bash", "", 0, "sed -n '100,600p' big.txt", "big-sed"},
		{"Bash", "", 0, "sed -n 1,500p big.txt", "big-sed"},
		{"Bash", "", 0, "sed -n '100,499p' big.txt", ""},
		{"Bash", "", 0, "sed -n '10,20p' big.txt", ""},
		{"Bash", "", 0, "ls", ""},
	} {
		kind, text := bigReadFor(c.tool, dir, c.path, c.limit, c.cmd)
		if kind != c.want {
			t.Errorf("%s %q %q: nudge %q, want %q", c.tool, c.path, c.cmd, kind, c.want)
		}
		if kind != "" && (strings.Contains(text, "\n") || !strings.Contains(text, "grep -n") || !strings.Contains(text, "offset/limit")) {
			t.Errorf("%q: text %q", c.cmd, text)
		}
	}
	if _, text := bigReadFor("Read", dir, "big.txt", 0, ""); !strings.Contains(text, "~900 lines") {
		t.Errorf("text: %q", text)
	}
	if n := fileLines(filepath.Join(dir, "huge.txt")); n < 40000 || n > 60000 {
		t.Errorf("estimate %d", n)
	}
	// Through the hook: Read is nudged as context only.
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	in, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "cwd": dir,
		"tool_input": map[string]any{"file_path": "big.txt"}})
	var out bytes.Buffer
	hookClaudeNudge(&cli{}, bytes.NewReader(in), &out)
	if !strings.Contains(out.String(), "additionalContext") || strings.Contains(out.String(), "permissionDecision") {
		t.Errorf("hook output %q", out.String())
	}
}

// Waiting on a background task (WP-191): sleeping on one that still runs is
// refused, reading it again is a note, a finished one is quiet; a sub-agent's
// output file (its transcript) always gets a note.
func TestTaskNudges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s", "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	running, done := filepath.Join(dir, "brun1.output"), filepath.Join(dir, "bdone2.output")
	for _, f := range []string{running, done} {
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	agent := filepath.Join(dir, "a1b2c3.output")
	if err := os.Symlink(filepath.Join(dir, "..", "agent.jsonl"), agent); err != nil {
		t.Fatal(err)
	}
	held := heldOpen
	t.Cleanup(func() { heldOpen = held })
	heldOpen = func(f string) bool { return f == running }
	for _, c := range []struct {
		tool, cmd, path string
		who             caller
		want            string
		deny            bool
	}{
		{"Bash", "sleep 60; cat " + running, "", callerMain, "task-wait", true},
		{"Bash", "sleep 2 && tail -5 " + running + " | cut -c1-200", "", callerMain, "task-wait", true},
		{"Bash", "sleep 60; cat " + running, "", callerBg, "task-wait", true},
		{"Bash", "sleep 60; cat " + running, "", callerFg, "task-wait", false},
		{"Bash", "tail -20 " + running, "", callerMain, "task-poll", false},
		{"Read", "", running, callerMain, "task-poll", false},
		{"Bash", "sleep 60; cat " + done, "", callerMain, "", false},
		{"Read", "", done, callerMain, "", false},
		{"Bash", "tail " + agent, "", callerMain, "agent-output", false},
		{"Read", "", agent, callerMain, "agent-output", false},
		{"Bash", "sleep 60; ls " + dir, "", callerMain, "", false},
		{"Bash", "cat tasks/brun1.output", "", callerMain, "", false},
	} {
		kind, text, deny := taskNudge(c.tool, c.cmd, c.path, c.who)
		if kind != c.want || deny != c.deny {
			t.Errorf("%s %q %q: %q deny=%v, want %q deny=%v", c.tool, c.cmd, c.path, kind, deny, c.want, c.deny)
		}
		if kind != "" && (strings.Contains(text, "\n") || text == "") {
			t.Errorf("%q: text %q", c.cmd, text)
		}
	}

	// Through the hook: the refusal is a deny with the reason; a background
	// call is never refused, nor a background sleep loop noted.
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WP_GUARD", "")
	tc := &cli{dir: t.TempDir()}
	hookWith := func(h func(*cli, io.Reader, io.Writer), cmd string, bg bool, agentID string) string {
		in, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": dir, "agent_id": agentID,
			"tool_input": map[string]any{"command": cmd, "run_in_background": bg}})
		var out bytes.Buffer
		h(tc, bytes.NewReader(in), &out)
		return out.String()
	}
	hook := func(cmd string, bg bool, agentID string) string { return hookWith(hookClaudeNudge, cmd, bg, agentID) }
	guard := func(cmd string, bg bool, agentID string) string { return hookWith(hookClaudeGuard, cmd, bg, agentID) }
	if out := guard("sleep 60; cat "+running, false, ""); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "brun1 is still running") {
		t.Errorf("task-wait output %q", out)
	}
	if out := hook("sleep 60; cat "+running, false, ""); out != "" {
		t.Errorf("the nudge answered a refused call: %q", out)
	}
	// A sub-agent's too (WP-1443): the notice of its background task is as
	// certain, and a sub-agent that ends its turn with one running is not asked
	// for its report.
	startAgent(tc, "", "agent-1", true)
	if out := guard("sleep 60; cat "+running, false, "agent-1"); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "brun1 is still running") {
		t.Errorf("the guard did not refuse a background sub-agent: %q", out)
	}
	if out := hook("sleep 60; cat "+running, false, "agent-1"); out != "" {
		t.Errorf("the nudge answered a refused sub-agent call: %q", out)
	}
	// A foreground sub-agent (or one not known to be a background one) is
	// only noted: its shells are reaped when it answers, so ending the turn
	// would stop the task.
	if out := guard("sleep 60; cat "+running, false, "agent-fg"); strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("the guard refused a foreground sub-agent: %q", out)
	}
	if out := hook("sleep 60; cat "+running, false, "agent-fg"); !strings.Contains(out, "600000") || strings.Contains(out, "End your turn") {
		t.Errorf("the nudge's note for a foreground sub-agent: %q", out)
	}
	if out := guard("sleep 60; cat "+running, true, ""); out != "" {
		t.Errorf("guard refused a background call: %q", out)
	}
	if out := hook("sleep 60; cat "+running, true, ""); out != "" {
		t.Errorf("background call answered %q", out)
	}
	if out := hook("while ! gh pr checks 3; do sleep 30; done", true, ""); out != "" {
		t.Errorf("background waiter answered %q", out)
	}
	if out := hook("sleep 300; cat /tmp/x.log", false, ""); !strings.Contains(out, "in the background") || strings.Contains(out, "permissionDecision") {
		t.Errorf("sleep-wait output %q", out)
	}
}

// heldOpen sees a file this process holds open, and not one it closed.
func TestHeldOpen(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	f := filepath.Join(t.TempDir(), "b1.output")
	fh, err := os.Create(f)
	if err != nil {
		t.Fatal(err)
	}
	if !heldOpen(f) {
		t.Error("an open file isn't held")
	}
	_ = fh.Close()
	if heldOpen(f) {
		t.Error("a closed file is held")
	}
}

func TestSleepSeconds(t *testing.T) {
	for cmd, want := range map[string]float64{
		"sleep 30":             30,
		"sleep 1.5m; echo":     90,
		"sleep 2h":             7200,
		"sleep 5 && sleep 10s": 15,
		"sleep $N":             0,
		"echo nosleep 40":      0,
		"x=1; sleep 4|cat":     4,
		"(sleep 100) & wait":   100,
		"sleep 12abc":          0,
	} {
		if got := sleepSeconds(cmd); got != want {
			t.Errorf("sleepSeconds(%q) = %v, want %v", cmd, got, want)
		}
	}
}
