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

const badWorkflow = "on:\n  pull_request:\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: a\n"

func ciHook(t *testing.T, c *cli, tool, file, cwd string) string {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": cwd, "tool_name": tool, "tool_input": map[string]string{"file_path": file}})
	var out bytes.Buffer
	hookClaudeBurn(c, bytes.NewReader(in), &out)
	return out.String()
}

func TestCIHookOnWorkflowEdit(t *testing.T) {
	repo := t.TempDir()
	wf := filepath.Join(repo, ".forgejo", "workflows", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(wf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wf, []byte(badWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(repo, "main.go")
	_ = os.WriteFile(other, []byte("package x"), 0o600)
	c := &cli{dir: t.TempDir()}

	start := time.Now()
	out := ciHook(t, c, "Edit", wf, repo)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("hook took %s, the budget is well inside 2s", d)
	}
	var got struct {
		H struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || !strings.Contains(got.H.AdditionalContext, ".forgejo/workflows/ci.yml:1 ci-no-cancel-superseded") {
		t.Fatalf("want a finding in the context, got %q (%v)", out, err)
	}
	if n := strings.Count(got.H.AdditionalContext, "\n") + 1; n > ciLintLines+1 {
		t.Errorf("%d lines, max %d plus the heading", n, ciLintLines)
	}
	if out := ciHook(t, c, "MultiEdit", wf, repo); out != "" {
		t.Errorf("repeat finding should be silent, got %q", out)
	}
	if out := ciHook(t, c, "Edit", other, repo); out != "" {
		t.Errorf("non-workflow file: %q", out)
	}
	if out := ciHook(t, &cli{dir: t.TempDir()}, "Read", wf, repo); out != "" {
		t.Errorf("Read: %q", out)
	}
	if out := ciHook(t, c, "Write", filepath.Join(repo, ".github", "workflows", "gone.yml"), repo); out != "" {
		t.Errorf("missing file: %q", out)
	}
}

func TestCILintCommand(t *testing.T) {
	wf := filepath.Join(t.TempDir(), "w.yml")
	_ = os.WriteFile(wf, []byte(badWorkflow), 0o600)
	var out bytes.Buffer
	c := &cli{out: &out}
	if err := cmdCILint(c, []string{wf}); err != nil || !strings.Contains(out.String(), "ci-no-cancel-superseded") {
		t.Fatalf("%v %q", err, out.String())
	}
}

// wp ci log's usage errors come before any call, and its text names the run,
// lists the jobs that were not asked for and shows the failing part of the
// picked ones (WP-315).
func TestCILogCommand(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "https://waypoint.example.com")
	for name, args := range map[string][]string{
		"nothing":     {"ci", "log", "--repo", "a/b"},
		"both":        {"ci", "log", "abc1234", "--run", "4", "--repo", "a/b"},
		"two commits": {"ci", "log", "abc1234", "def5678", "--repo", "a/b"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "wp ci log") {
			t.Errorf("%s: code %d, %q %q", name, code, out.String(), errOut.String())
		}
	}

	var l ciLogs
	raw := `{"repo":"acme/app","runs":[{"run_number":42,"workflow":"ci.yml","event":"push","ref":"main","sha":"aaaaaaaaaaaaaaaa","status":"failure","jobs":[
		{"name":"current","status":"success"},
		{"name":"backend","status":"failure","picked":true,"found":true,"lines":412,"shown":3,"excerpt":"--- FAIL: TestX\n"},
		{"name":"e2e","status":"failure","picked":true,"error":"the forge answered 404"}]}]}`
	if err := jsonInto([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	want := "== acme/app ci.yml #42 (failure) push main aaaaaaa\n" +
		"  current: success\n" +
		"-- backend: failure  [3 of 412 lines; --job \"backend\" --full for all]\n--- FAIL: TestX\n" +
		"-- e2e: failure\n(the forge answered 404)\n"
	if got := l.text(false); got != want {
		t.Errorf("text:\n%s\nwant:\n%s", got, want)
	}
	l.Runs[0].Jobs[1].Picked, l.Runs[0].Jobs[2].Picked = false, false
	if got := l.text(false); !strings.Contains(got, "no failed job; --job NAME reads one's log") {
		t.Errorf("nothing picked: %s", got)
	}
}
