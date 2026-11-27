package main

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// unshippedWork says what in the checkouts a finish with nothing to ship
// comes from isn't in a pull request yet: the one here, and the ones the
// tasks were taken in. Finishing without it would leave it on a branch
// nobody looks at again, so it is refused unless told the work isn't the
// task's (--no-ship). "" when there is nothing (or no checkout).
func (c *cli) unshippedWork(keys []string) (string, error) {
	var tops []string
	if wd, err := os.Getwd(); err == nil {
		tops = append(tops, checkoutOf(wd))
	}
	for _, k := range keys {
		if dir, cl := c.state.claimByKey(k); cl != nil {
			tops = append(tops, cl.top(dir))
		}
	}
	var out, seen []string
	for _, top := range tops {
		if top == "" || slices.Contains(seen, top) {
			continue
		}
		if _, err := os.Stat(top); err != nil {
			continue
		}
		seen = append(seen, top)
		what, err := unshipped(top)
		if err != nil {
			return "", fmt.Errorf("can't tell what %s holds (%w); fix it, or `wp done --no-ship` to finish anyway", top, err)
		}
		if what != "" {
			out = append(out, top+" has "+what)
		}
	}
	return strings.Join(out, "; "), nil
}

// unshipped is what a checkout holds that no remote has: changed or new
// files, and commits not pushed (only once it has fetched a remote branch:
// before that nothing tells pushed from not).
func unshipped(top string) (string, error) {
	var bits []string
	st, err := gitOutput(top, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if n := uncommittedFiles(st); n > 0 {
		bits = append(bits, plural1(n, "uncommitted file"))
	}
	if k := unpushedCommits(top); k > 0 {
		bits = append(bits, plural1(k, "commit")+" not pushed")
	}
	return strings.Join(bits, " and "), nil
}

// uncommittedFiles counts the lines of git status --porcelain that are the
// task's: new files under .claude/ are the agent's (worktrees, local
// settings).
func uncommittedFiles(st string) int {
	if st == "" {
		return 0
	}
	return len(slices.DeleteFunc(strings.Split(st, "\n"), func(l string) bool { return strings.HasPrefix(l, "?? .claude/") }))
}

// unpushedCommits counts the commits in a checkout that no remote has:
// on no remote branch. 0 until it has fetched
// a remote branch: before that nothing tells pushed from not.
func unpushedCommits(top string) int {
	if refs, err := gitOutput(top, "for-each-ref", "--count=1", "refs/remotes"); err != nil || refs == "" {
		return 0
	}
	n, err := gitOutput(top, "rev-list", "--count", "HEAD", "--not", "--remotes")
	if k, _ := strconv.Atoi(n); err == nil {
		return k
	}
	return 0
}

// unshippedError is the refusal to finish with code left unshipped.
func unshippedError(what string) error {
	return fmt.Errorf("%s. To ship it, stage the task's changes and run wp ship -m \"SUMMARY\" in that checkout; --no-ship finishes without it, when it isn't this task's", what)
}
