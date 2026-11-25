package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoRun records one run of argv on the tree as it is now.
func repoRun(t *testing.T, argv []string, res *testResult, exit int) {
	t.Helper()
	rec := newTestRunRecorder(argv)
	rec.started()
	rec.finish(res, exit)
}

func TestUnchangedNote(t *testing.T) {
	argv := []string{"go", "test", "./x"}
	pass12 := &testResult{Passed: 12}
	failed2 := &testResult{Failed: 2, Passed: 3, Failures: []testFailure{{Name: "a"}, {Name: "b"}}}
	tests := []struct {
		name   string
		first  *testResult
		exit   int
		change func(t *testing.T)
		argv   []string
		want   string
	}{
		{"unchanged tree", pass12, 0, func(*testing.T) {}, argv, "(less than a minute ago: passed, 12 tests)"},
		{"earlier failure is reported", failed2, 1, func(*testing.T) {}, argv, ": 2 failed)"},
		{"earlier run with no results", &testResult{Failures: []testFailure{{Name: unchangedNoRun}}}, 2, func(*testing.T) {}, argv, "the command failed with no test results, exit 2"},
		{"tracked edit", pass12, 0, func(t *testing.T) { write(t, "a.txt", "two\n") }, argv, ""},
		{"staged edit", pass12, 0, func(t *testing.T) { write(t, "a.txt", "two\n"); gitDo(t, "add", "a.txt") }, argv, ""},
		{"new untracked file", pass12, 0, func(t *testing.T) { write(t, "new.txt", "x\n") }, argv, ""},
		{"deleted file", pass12, 0, func(t *testing.T) { _ = os.Remove("a.txt") }, argv, ""},
		{"edit then revert", pass12, 0, func(t *testing.T) { write(t, "a.txt", "two\n"); write(t, "a.txt", "one\n") }, argv, "passed, 12 tests"},
		{"new commit with the same content", pass12, 0, func(t *testing.T) { gitDo(t, "commit", "-q", "--allow-empty", "-m", "same") }, argv, "passed, 12 tests"},
		{"committed edit is still an edit", pass12, 0, func(t *testing.T) {
			write(t, "a.txt", "two\n")
			gitDo(t, "commit", "-q", "-am", "edit")
		}, argv, ""},
		{"changed ignored file", pass12, 0, func(t *testing.T) { write(t, "ignored", "build output\n") }, argv, "passed, 12 tests"},
		{"different argv", pass12, 0, func(*testing.T) {}, []string{"go", "test", "./y"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempRepo(t)
			repoRun(t, argv, tt.first, tt.exit)
			tt.change(t)
			got := newTestRunRecorder(tt.argv).unchanged(time.Now())
			if tt.want == "" {
				if got != "" {
					t.Errorf("line: %q", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) || !strings.HasPrefix(got, "Nothing in this checkout changed since this command last ran (") || strings.Contains(got, "\n") {
				t.Errorf("got %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestUnchangedNoteNoEarlierRunOrStale(t *testing.T) {
	tempRepo(t)
	argv := []string{"go", "test"}
	if got := newTestRunRecorder(argv).unchanged(time.Now()); got != "" {
		t.Errorf("no earlier run: %q", got)
	}
	repoRun(t, argv, &testResult{Passed: 1}, 0)
	if got := newTestRunRecorder(argv).unchanged(time.Now().Add(unchangedWithin - time.Minute)); got == "" {
		t.Error("within the bound: no line")
	}
	if got := newTestRunRecorder(argv).unchanged(time.Now().Add(unchangedWithin + time.Minute)); got != "" {
		t.Errorf("stale: %q", got)
	}
	write(t, "a.txt", "two\n")
	repoRun(t, argv, &testResult{Passed: 2}, 0)
	write(t, "a.txt", "one\n")
	if got := newTestRunRecorder(argv).unchanged(time.Now()); got != "" {
		t.Errorf("older run's tree: %q", got)
	}
}

func TestUnchangedNoteIgnoresARunWhoseTreeMoved(t *testing.T) {
	now := time.Now()
	runs := []testRun{{At: now.UnixMilli(), Tree: "t", Moved: true, Command: "c", Outcome: runPass, Passed: 1}}
	if got := unchangedNote(runs, "c", "t", now); got != "" {
		t.Errorf("moved: %q", got)
	}
	if got := unchangedNote(runs, "c", "", now); got != "" {
		t.Errorf("no tree: %q", got)
	}
}

func TestUnchangedNoteNotAGitRepo(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(t.TempDir()))
	_ = os.Unsetenv("GIT_DIR")
	if got := newTestRunRecorder([]string{"go", "test"}).unchanged(time.Now()); got != "" {
		t.Errorf("line outside git: %q", got)
	}
	var nilRec *testRunRecorder
	if got := nilRec.unchanged(time.Now()); got != "" {
		t.Errorf("nil recorder: %q", got)
	}
}

// Each worktree keeps its own record, so a run in one doesn't make a line in
// another, and neither overwrites the other's.
func TestUnchangedNoteIsPerWorktree(t *testing.T) {
	first := tempRepo(t)
	argv := []string{"go", "test"}
	other := filepath.Join(t.TempDir(), "wt")
	gitDo(t, "worktree", "add", "-q", "-b", "side", other)
	repoRun(t, argv, &testResult{Passed: 1}, 0)
	t.Chdir(other)
	if got := newTestRunRecorder(argv).unchanged(time.Now()); got != "" {
		t.Errorf("another worktree's run: %q", got)
	}
	repoRun(t, argv, &testResult{Failed: 1, Failures: []testFailure{{Name: "x"}}}, 1)
	t.Chdir(first)
	if got := newTestRunRecorder(argv).unchanged(time.Now()); !strings.Contains(got, "passed, 1 tests") {
		t.Errorf("first checkout's own record: %q", got)
	}
}

// End to end: the line is first in the summary and on stderr, the tests
// still run, and a changed tree gets none.
func TestTestsumSaysNothingChanged(t *testing.T) {
	slotEnv(t)
	tempRepo(t)
	write(t, "go.mod", "module m\n\ngo 1.22\n")
	write(t, "m_test.go", "package m\n\nimport \"testing\"\n\nfunc TestM(t *testing.T) {}\n")
	runIt := func() (string, string) {
		var out, errb bytes.Buffer
		if code := run([]string{"testsum", "--", "go", "test", "-count=1", "."}, &out, &errb); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
		}
		return out.String(), errb.String()
	}
	if out, errs := runIt(); strings.Contains(out+errs, "Nothing in this checkout") {
		t.Fatalf("first run: %s%s", out, errs)
	}
	out, errs := runIt()
	if !strings.HasPrefix(out, "Nothing in this checkout changed since this command last ran (") || !strings.Contains(out, "\nPASS") {
		t.Errorf("stdout: %q", out)
	}
	if !strings.HasPrefix(errs, "wp testsum: Nothing in this checkout changed") {
		t.Errorf("stderr: %q", errs)
	}
	write(t, "m_test.go", "package m\n\nimport \"testing\"\n\nfunc TestM(t *testing.T) {}\n\nfunc TestN(t *testing.T) {}\n")
	if out, errs := runIt(); strings.Contains(out+errs, "Nothing in this checkout") {
		t.Errorf("after an edit: %s%s", out, errs)
	}
}
