package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The default ship check's tests (ship_end.go). The file's name puts them
// beside it, so the scoped check runs them when it changes.

// wp ship runs wp test by default in a repository that has a .waypoint/test:
// once per tree, again after the tree changes, and not at all without one.
func TestShipRunsTheScopedCheckOncePerTree(t *testing.T) {
	root := scopedRepo(t)
	runs := 0
	old := shipScopedCheck
	t.Cleanup(func() { shipScopedCheck = old })
	shipScopedCheck = func(c *cli) (int, error) {
		runs++
		err := c.dispatch(testCmd, nil)
		var ec exitCode
		if errors.As(err, &ec) {
			return int(ec), nil
		}
		return 0, err
	}
	var out bytes.Buffer
	c := &cli{out: &out, err: &bytes.Buffer{}}

	putFiles(t, root, map[string]string{"a.txt": "a\n"})
	if err := c.runDefaultShipCheck(); err != nil || runs != 0 {
		t.Fatalf("no .waypoint/test: err %v, runs %d", err, runs)
	}

	putExec(t, root, ".waypoint/test", "#!/bin/sh\nexit 0\n")
	if err := c.runDefaultShipCheck(); err != nil || runs != 1 {
		t.Fatalf("first ship: err %v, runs %d", err, runs)
	}
	if err := c.runDefaultShipCheck(); err != nil || runs != 1 || !strings.Contains(out.String(), "already passed on this tree; not run again") {
		t.Fatalf("second ship on one tree: err %v, runs %d\n%s", err, runs, out.String())
	}
	putFiles(t, root, map[string]string{"b.txt": "b\n"})
	if err := c.runDefaultShipCheck(); err != nil || runs != 2 {
		t.Fatalf("after the tree changed: err %v, runs %d", err, runs)
	}

	putExec(t, root, ".waypoint/test", "#!/bin/sh\nexit 1\n")
	err := c.runDefaultShipCheck()
	if err == nil || runs != 3 || !strings.Contains(err.Error(), "--no-check") {
		t.Fatalf("a failing check: err %v, runs %d", err, runs)
	}
}

// A failed run on the tree doesn't count as one that already passed: wp ship
// runs the check again on the same tree, for the default scoped check and for
// --check alike, and skips it only once a run on that tree has passed
// (audit 1896.51).
func TestShipCheckRunsAgainAfterAFailureOnTheSameTree(t *testing.T) {
	root := scopedRepo(t)
	flag := filepath.Join(t.TempDir(), "ok") // outside the tree, so the tree stays the same
	putExec(t, root, ".waypoint/test", "#!/bin/sh\ntest -e "+flag+"\n")
	putFiles(t, root, map[string]string{"a.txt": "a\n"})
	runs := 0
	old := shipScopedCheck
	t.Cleanup(func() { shipScopedCheck = old })
	shipScopedCheck = func(c *cli) (int, error) {
		runs++
		err := c.dispatch(testCmd, nil)
		var ec exitCode
		if errors.As(err, &ec) {
			return int(ec), nil
		}
		return 0, err
	}
	var out bytes.Buffer
	c := &cli{out: &out, err: &bytes.Buffer{}}
	tree := workingTree()

	if err := c.runDefaultShipCheck(); err == nil || runs != 1 {
		t.Fatalf("a failing wp test: err %v, runs %d", err, runs)
	}
	if err := c.runDefaultShipCheck(); err == nil || runs != 2 || strings.Contains(out.String(), "already passed") {
		t.Fatalf("a failure on the tree skipped the next run: err %v, runs %d\n%s", err, runs, out.String())
	}
	check := "test -e " + flag
	if err := c.runShipCheck(check); err == nil {
		t.Fatal("a failing --check passed")
	}
	out.Reset()
	if err := c.runShipCheck(check); err == nil || strings.Contains(out.String(), "already passed") || !strings.Contains(out.String(), "Check failed: "+check) {
		t.Fatalf("a failed --check on the tree skipped the next run: err %v\n%s", err, out.String())
	}

	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if workingTree() != tree {
		t.Fatal("the tree changed; the test can't tell a skip from a run")
	}
	if err := c.runDefaultShipCheck(); err != nil || runs != 3 {
		t.Fatalf("a passing wp test after failures: err %v, runs %d", err, runs)
	}
	out.Reset()
	if err := c.runShipCheck(check); err != nil || !strings.Contains(out.String(), "Check passed: "+check) {
		t.Fatalf("a passing --check after failures: err %v\n%s", err, out.String())
	}
	out.Reset()
	if err := c.runDefaultShipCheck(); err != nil || runs != 3 || !strings.Contains(out.String(), "Check: wp test already passed on this tree; not run again.") {
		t.Errorf("wp test after its pass: err %v, runs %d\n%s", err, runs, out.String())
	}
	out.Reset()
	if err := c.runShipCheck(check); err != nil || !strings.Contains(out.String(), "Check: "+check+" already passed on this tree; not run again.") {
		t.Errorf("--check after its pass: err %v\n%s", err, out.String())
	}
}

// A pass over a base the caller chose is not the default scoped pass: wp ship
// runs its own check after it.
func TestShipCheckIsNotSkippedByAWpTestPassOverAChosenBase(t *testing.T) {
	root := scopedRepo(t)
	putExec(t, root, ".waypoint/test", "#!/bin/sh\nexit 0\n")
	putFiles(t, root, map[string]string{"a.txt": "a\n"})
	runs := 0
	old := shipScopedCheck
	t.Cleanup(func() { shipScopedCheck = old })
	shipScopedCheck = func(c *cli) (int, error) { runs++; return 0, nil }
	var out bytes.Buffer
	c := &cli{out: &out, err: &bytes.Buffer{}}

	if _, _, err := runWpTest(t, "--base", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := c.runDefaultShipCheck(); err != nil || runs != 1 {
		t.Fatalf("after wp test --base HEAD: err %v, runs %d\n%s", err, runs, out.String())
	}
	// The default pass still lets it skip.
	if _, _, err := runWpTest(t); err != nil {
		t.Fatal(err)
	}
	if err := c.runDefaultShipCheck(); err != nil || runs != 1 || !strings.Contains(out.String(), "already passed on this tree") {
		t.Fatalf("after wp test: err %v, runs %d\n%s", err, runs, out.String())
	}
}

// wp ship doesn't ship when wp test can't start.
func TestShipDefaultCheckThatCannotStartShipsNothing(t *testing.T) {
	root := scopedRepo(t)
	putExec(t, root, ".waypoint/test", "#!/bin/sh\nexit 0\n")
	putFiles(t, root, map[string]string{"a.txt": "a\n"})
	old := shipScopedCheck
	t.Cleanup(func() { shipScopedCheck = old })
	shipScopedCheck = func(c *cli) (int, error) { return 0, errors.New("fork/exec: no such file") }
	c := &cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err := c.runDefaultShipCheck()
	if err == nil || !strings.Contains(err.Error(), "fork/exec: no such file") || !strings.Contains(err.Error(), "(nothing shipped; --no-check ships without it)") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "failed (exit") {
		t.Errorf("a start failure read as a failed check: %v", err)
	}
}

// --no-check, given to wp ship itself, skips the scoped check; without it the
// check runs first and a check that can't start stops the ship before the pull
// request is opened.
func TestFinishNoCheckSkipsTheScopedCheck(t *testing.T) {
	root := scopedRepo(t)
	putExec(t, root, ".waypoint/test", "#!/bin/sh\nexit 0\n")
	putFiles(t, root, map[string]string{"b.txt": "b\n"})
	gitDo(t, "add", "b.txt")
	runs, opened := 0, 0
	oldCheck, oldOpen := shipScopedCheck, reopenPR
	t.Cleanup(func() { shipScopedCheck, reopenPR = oldCheck, oldOpen })
	shipScopedCheck = func(c *cli) (int, error) { runs++; return 0, errors.New("no fork today") }
	reopenPR = func(*cli, prOpenFlags) (*openedPR, []byte, error) {
		opened++
		return nil, nil, errors.New("stop at the open")
	}
	newCLI := func() *cli { return &cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}} }

	err := cmdFinish(newCLI(), []string{"--no-check", "-m", "add b"})
	if err == nil || !strings.Contains(err.Error(), "stop at the open") || runs != 0 || opened != 1 {
		t.Fatalf("--no-check: err %v, check runs %d, opens %d", err, runs, opened)
	}
	gitDo(t, "reset", "-q", "--soft", "HEAD~1") // the commit the first ship made
	err = cmdFinish(newCLI(), []string{"-m", "add b"})
	if err == nil || !strings.Contains(err.Error(), "(nothing shipped; --no-check ships without it)") || runs != 1 || opened != 1 {
		t.Fatalf("without --no-check: err %v, check runs %d, opens %d", err, runs, opened)
	}
}
