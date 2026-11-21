package main

import (
	"fmt"
	"io"
	"time"
)

// When wp testsum is run again with the same command on a tree that is
// content-identical to the tree the last run of that command tested, it
// still runs (a flaky test needs a real re-run) but says so first: the usual
// cause is an edit meant to land in between that didn't (WP-1443).
//
// The record is the one the recorder already keeps (wp-test-runs.jsonl in
// the worktree's own git directory, see testruns.go): it holds each run's
// tree and outcome, is per checkout so runs in other worktrees can't clobber
// it, is bounded, and needs no server. The tree is workingTree: HEAD's
// content plus staged, unstaged and untracked-but-not-ignored changes, by
// content. Ignored files (build output, caches) don't count as a change, as
// they don't to git or to wp ship. Only content counts: a new commit with the
// same content is the same tree and gets the line, since the tests would
// see the same files.
const (
	// unchangedWithin bounds how old the earlier run may be: the same working
	// session. After a few hours it is a person coming back, and "nothing
	// changed" is no longer a hint about a failed edit.
	unchangedWithin = 4 * time.Hour
	// unchangedNoRun is how a run that failed before any result is recorded
	// (testRunRecorder.failed).
	unchangedNoRun = "the command failed"
)

// unchangedNote is the line to print for a run of command on tree, or "":
// the newest earlier run of command, if it tested this very tree, did not
// see the tree move while it ran, and is recent enough.
func unchangedNote(runs []testRun, command, tree string, now time.Time) string {
	if tree == "" {
		return ""
	}
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if r.Command != command {
			continue
		}
		at := time.UnixMilli(r.At)
		age := now.Sub(at)
		if r.Tree != tree || r.Moved || age > unchangedWithin || age < -time.Minute {
			return "" // only the latest run of the command is the one to compare
		}
		return fmt.Sprintf("Nothing in this checkout changed since this command last ran (%s: %s). If an edit was meant to land in between, it did not.",
			agoShort(age), earlierOutcome(r))
	}
	return ""
}

// agoShort is how long ago, as "4m ago".
func agoShort(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(d.Hours()))
}

// earlierOutcome says what the recorded run came to.
func earlierOutcome(r testRun) string {
	total := r.Passed + r.Failed + r.Skipped
	switch {
	case r.Outcome == runFail && (r.Failed == 0 && len(r.Failures) == 1 && r.Failures[0] == unchangedNoRun):
		return fmt.Sprintf("the command failed with no test results, exit %d", r.Exit)
	case r.Outcome == runFail:
		return fmt.Sprintf("%d failed", max(r.Failed, len(r.Failures)))
	case r.Outcome == runEmpty:
		return "no test ran"
	case total == 0 && r.Units > 0:
		return fmt.Sprintf("passed, %d %s", r.Units, r.UnitName)
	}
	return fmt.Sprintf("passed, %d tests", total)
}

// unchanged is the line for this run, or "": any trouble finding out (no git,
// no record, unreadable file) means no line, never a failed test run.
func (r *testRunRecorder) unchanged(now time.Time) string {
	if r == nil || r.snap == nil {
		return ""
	}
	tree := r.snap()
	if tree == "" {
		return ""
	}
	p := testRunsPath()
	if p == "" {
		return ""
	}
	return unchangedNote(readTestRuns(p), runCommand(r.argv), tree, now)
}

// runCommand is the command as the record keeps it.
func runCommand(argv []string) string {
	return truncRunes(redactCommand(shellJoin(argv)), testRunCmdMax)
}

// sayUnchanged prints the note to stderr when the run starts, so one queued
// for the slot shows it early.
func sayUnchanged(errw io.Writer, note string) {
	if note != "" {
		_, _ = fmt.Fprintln(errw, "wp testsum: "+note)
	}
}
