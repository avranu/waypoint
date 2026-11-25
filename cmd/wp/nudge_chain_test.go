package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChainStep(t *testing.T) {
	for _, c := range []struct{ tool, cmd, path, want string }{
		{"Bash", `grep -rn MergeReady internal/`, "", stepFind},
		{"Bash", `cd x && rg -n "func Merge" --type go`, "", stepFind},
		{"Bash", `git grep -n claimFor`, "", stepFind},
		{"Bash", `grep -rn Merge . | head -20`, "", stepFind},
		{"Bash", `sed -n '120,180p' internal/core/merge.go`, "", stepLook},
		{"Bash", `sed -n 40p cmd/wp/main.go`, "", stepLook},
		{"Bash", `grep -n "func " internal/core/merge.go`, "", stepLook},
		{"Bash", `head -50 cmd/wp/nudge.go`, "", stepLook},
		{"Bash", `cat docs/CLI.md`, "", stepLook},
		{"Read", "", "/x/y.go", stepLook},
		{"Bash", `wp code find MergeReady`, "", stepWP},
		{"Bash", `cd x; wp brief "how do merges work"`, "", stepWP},
		{"Bash", `grep -rn X . && wp code show merge.go`, "", stepWP},
		{"Bash", `go test ./...`, "", ""},
		{"Bash", `wp show WP-1 | grep foo`, "", ""},
		{"Bash", `sed -i 's/a/b/' x.go`, "", ""},
		{"Bash", `cat /dev/null`, "", ""},
		{"Bash", `git log | head -5`, "", ""},
		{"Read", "", "", ""},
		{"Edit", "", "/x/y.go", ""},
	} {
		if got := chainStep(c.tool, c.cmd, c.path); got != c.want {
			t.Errorf("%s %q %q: %q, want %q", c.tool, c.cmd, c.path, got, c.want)
		}
	}
}

// A transcript is told once, after its second search and fifth step; one
// that ran wp code first, a subagent of a told session, and calls outside a
// checkout are counted apart.
func TestChainNudge(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "internal", "core")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	c := &cli{dir: t.TempDir()}
	run := func(session, agent, cwd string, steps ...string) []bool {
		var told []bool
		for _, s := range steps {
			told = append(told, c.chainNudge(&hookEventInput{SessionID: session, AgentID: agent}, cwd, s, true))
		}
		return told
	}
	got := run("s1", "", sub, stepFind, stepLook, stepLook, stepLook, stepFind, stepFind, stepLook)
	if want := []bool{false, false, false, false, true, false, false}; !equalBools(got, want) {
		t.Errorf("chain: %v, want %v", got, want)
	}
	if got := run("s1", "agent1", repo, stepFind, stepFind, stepLook, stepLook, stepLook); !got[4] {
		t.Errorf("a subagent's own chain: %v", got)
	}
	if got := run("s2", "", repo, stepWP, stepFind, stepFind, stepFind, stepLook, stepLook, stepLook); anyBool(got) {
		t.Errorf("after wp code: %v", got)
	}
	if got := run("s3", "", t.TempDir(), stepFind, stepFind, stepFind, stepLook, stepLook, stepLook); anyBool(got) {
		t.Errorf("outside a checkout: %v", got)
	}
	if got := run("../x", "", repo, stepFind, stepFind, stepFind, stepFind, stepFind); anyBool(got) {
		t.Errorf("unsafe session id: %v", got)
	}
	old := filepath.Join(c.dir, "nudge", "s1.json")
	past := time.Now().Add(-chainKeep - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	run("s4", "", repo, stepFind)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old state kept: %v", err)
	}
}

// Through the hook: the note comes as context on the fifth call.
func TestChainNudgeHook(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &cli{dir: t.TempDir()}
	var last string
	for _, cmd := range []string{`grep -rn Foo .`, `sed -n 1,20p a.go`, `grep -rn Bar .`, `sed -n 5,9p b.go`, `sed -n 9,30p b.go`} {
		in, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": repo,
			"tool_input": map[string]string{"command": cmd}})
		var out bytes.Buffer
		hookClaudeNudge(c, bytes.NewReader(in), &out)
		last = out.String()
	}
	if !strings.Contains(last, "wp code find") || strings.Contains(last, "permissionDecision") {
		t.Errorf("hook output %q", last)
	}
	if c.state.Nudges[chainKind] == nil {
		t.Errorf("not counted: %+v", c.state.Nudges)
	}
}

func TestOutlineHint(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if h := outlineHint(filepath.Join(repo, "x", "merge.go"), repo); !strings.Contains(h, "wp code show x/merge.go") {
		t.Errorf("go file: %q", h)
	}
	for _, p := range []string{filepath.Join(repo, "out.log"), filepath.Join(repo, "notes.txt"), filepath.Join(t.TempDir(), "a.go"), ""} {
		if h := outlineHint(p, repo); h != "" {
			t.Errorf("%s: %q", p, h)
		}
	}
}

func TestOutlineHintOtherCheckout(t *testing.T) {
	repo, other := t.TempDir(), t.TempDir()
	for _, d := range []string{repo, other} {
		if err := os.Mkdir(filepath.Join(d, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if h := outlineHint(filepath.Join(other, "x.go"), repo); h != "" {
		t.Errorf("file in another checkout: %q", h)
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func anyBool(a []bool) bool {
	for _, b := range a {
		if b {
			return true
		}
	}
	return false
}
