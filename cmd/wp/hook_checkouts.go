package main

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Stranded work (WP-567): a session cleared or ended mid-task leaves its
// changes in a checkout nobody is told about. Each Stop reports the git
// state of the checkouts the session works in, so when the session ends
// Waypoint can say on the task where the work is; a SessionStart reports
// only the checkout it starts in (a git status of every worktree of a repo
// with dozens takes seconds), so the new session hears of unfinished work
// left there.

// hookCheckout is core.HookCheckout: one checkout's git state.
type hookCheckout struct {
	Top       string   `json:"top"`
	Branch    string   `json:"branch,omitempty"`
	Files     []string `json:"files,omitempty"` // git status --porcelain lines, the first checkoutFiles
	FileCount int      `json:"file_count,omitempty"`
	Unpushed  int      `json:"unpushed,omitempty"` // commits on HEAD no remote branch has
	Default   bool     `json:"default,omitempty"`  // on the remote's default branch
}

const (
	checkoutFiles = 20 // uncommitted files sent per checkout
	maxCheckouts  = 6  // checkouts sent per event
)

// checkoutState is the git state of the work tree whose top is top; nil
// when git can't say.
func checkoutState(top string) *hookCheckout {
	status, err := gitOutput(top, "status", "--porcelain")
	if err != nil {
		return nil
	}
	c := &hookCheckout{Top: top}
	if b, err := gitIn(top, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && b != "" {
		c.Branch = b
	} else if h, err := gitIn(top, "rev-parse", "--short", "HEAD"); err == nil {
		c.Branch = "(detached " + h + ")"
	}
	if strings.TrimSpace(status) != "" {
		lines := strings.Split(status, "\n")
		c.FileCount = len(lines)
		c.Files = lines[:min(len(lines), checkoutFiles)]
	}
	// Commits no remote has: lost with the checkout. With no remote, or a
	// remote never fetched (no remote-tracking branches), there is nothing to
	// compare with: the whole history would count.
	if remote := fetchRemote(top); remote != "" {
		if refs, err := gitIn(top, "for-each-ref", "--count=1", "--format=%(refname)", "refs/remotes"); err == nil && refs != "" {
			if out, err := gitIn(top, "rev-list", "--count", "HEAD", "--not", "--remotes"); err == nil {
				c.Unpushed, _ = strconv.Atoi(out)
			}
		}
		if head, err := gitIn(top, "symbolic-ref", "--short", "-q", "refs/remotes/"+remote+"/HEAD"); err == nil && head != "" {
			c.Default = c.Branch == strings.TrimPrefix(head, remote+"/")
		}
	}
	return c
}

// hookCheckouts is what an event reports of the checkouts: the one cwd is
// in, always (clean says so too), then the repo's worktrees whose branch
// carries one of keys (the session's tasks).
func hookCheckouts(cwd string, keys []string) []hookCheckout {
	if !filepath.IsAbs(cwd) {
		return nil
	}
	top := gitTop(cwd)
	if top == "" {
		return nil
	}
	var out []hookCheckout
	if c := checkoutState(top); c != nil {
		out = append(out, *c)
	}
	if len(keys) == 0 {
		return out
	}
	byBranch, _ := worktrees(top)
	branches := make([]string, 0, len(byBranch))
	for b := range byBranch {
		branches = append(branches, b)
	}
	slices.Sort(branches)
	for _, b := range branches {
		path := byBranch[b]
		if len(out) >= maxCheckouts {
			break
		}
		if path == top || !branchCarries(b, keys) {
			continue
		}
		if c := checkoutState(path); c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// branchCarries: branch names one of keys.
func branchCarries(branch string, keys []string) bool {
	for _, k := range keys {
		if re := keyWordRE(k); re != nil && re.MatchString(branch) {
			return true
		}
	}
	return false
}
