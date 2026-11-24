package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"
)

// fetchTimeout bounds the fetch wp next starts: a hung remote shows as a
// note, not a stuck command.
const fetchTimeout = 20 * time.Second

// startFetch fetches the checkout's remote in the background while the
// server answers; the returned func waits for it. nil outside a git work tree.
func startFetch(dir string) func() error {
	top := gitTop(dir)
	if top == "" {
		return nil
	}
	remote := fetchRemote(top)
	if remote == "" {
		return func() error { return nil }
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "-C", top, "fetch", "--quiet", remote) // #nosec G204 -- fixed subcommand; the remote name comes from git itself
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		cmd.WaitDelay = 2 * time.Second // ssh or credential helpers may hold stderr open past the kill
		var errb bytes.Buffer
		cmd.Stderr = &errb
		err := cmd.Run()
		if err == nil {
			head := exec.CommandContext(ctx, "git", "-C", top, "remote", "set-head", "--auto", remote) // #nosec G204 -- fixed subcommand; the remote name comes from git itself
			head.Env = cmd.Env
			head.WaitDelay = cmd.WaitDelay
			_ = head.Run()
		}
		switch {
		case ctx.Err() != nil:
			err = fmt.Errorf("no answer from %s in %s", remote, fetchTimeout)
		case err != nil:
			if msg := lastLine(errb.String()); msg != "" {
				err = fmt.Errorf("%s", msg)
			}
		}
		done <- err
	}()
	return func() error { return <-done }
}

// fetchHere starts the fetch for the checkout this command runs in, unless
// turned off (--no-git, WP_NO_GIT=1) or the answer is JSON for a tool.
func (c *cli) fetchHere(on bool) func() error {
	if !on || c.asJSON || os.Getenv("WP_NO_GIT") != "" {
		return nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return nil
	}
	return startFetch(dir)
}

// fetchRemote is origin, else the first remote, else "".
func fetchRemote(top string) string {
	out, err := gitIn(top, "remote")
	if err != nil {
		return ""
	}
	names := strings.Split(out, "\n")
	// A remote named like an option (a config's [remote "--upload-pack=CMD"])
	// is no remote git is told about: fetch would read it as one.
	usable := names[:0]
	for _, n := range names {
		if n != "" && codeindex.CheckRef(n) == nil {
			usable = append(usable, n)
		}
	}
	for _, n := range usable {
		if n == "origin" {
			return n
		}
	}
	if len(usable) > 0 {
		return usable[0]
	}
	return ""
}

// printCheckout waits for the fetch and prints the checkout report for the
// task taken. It never fails the command.
func (c *cli) printCheckout(key string, wait func() error) {
	if wait == nil {
		return
	}
	if key == "" {
		return
	}
	fetchErr := wait()
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	at := c.ownWorktree(dir, key)
	if text := checkoutReport(at, key, fetchErr); text != "" {
		c.println()
		c.print(text)
	}
}

const (
	statusLines = 8
	baseCommits = 3
)

// checkoutReport is the git state of the work tree at dir for someone about
// to work on task key: the branch against the default branch, uncommitted
// files, the default branch's latest commits, branches and worktrees already
// named for the task, and how many worktrees there are. "" outside a work tree.
func checkoutReport(dir, key string, fetchErr error) string {
	top := gitTop(dir)
	if top == "" {
		return ""
	}
	git := func(args ...string) string {
		out, err := gitIn(top, args...)
		if err != nil {
			return ""
		}
		return out
	}
	var b strings.Builder
	b.WriteString("Checkout")
	if fetchErr != nil {
		fmt.Fprintf(&b, " (fetch failed: %s; remote branches as of the last fetch)", fetchErr)
	} else if fetchRemote(top) != "" {
		b.WriteString(" (just fetched)")
	}
	b.WriteString(":\n")

	remote := fetchRemote(top)
	base := checkoutBase(top, remote)
	branch := git("symbolic-ref", "--short", "-q", "HEAD")
	where := "on " + branch
	if branch == "" {
		where = "detached at " + git("rev-parse", "--short", "HEAD")
	}
	if _, _, wt := gitInfo(top); wt {
		where += " (a linked worktree)"
	}
	line := fmt.Sprintf("  %s %s", top, where)
	if base != "" && base != branch {
		line += ", " + aheadBehind(top, base, "HEAD")
	}
	if up := git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); up != "" && up != base {
		if n, _ := strconv.Atoi(git("rev-list", "--count", up+"..HEAD")); n > 0 {
			line += fmt.Sprintf("; %d commit%s not pushed to %s", n, plural(n), up)
		}
	}
	b.WriteString(line + "\n")
	if base == "" && remote != "" {
		b.WriteString("  Default branch not found on " + remote + "; comparison with it skipped\n")
	}

	status, statusErr := gitOutput(top, "status", "--porcelain")
	if statusErr != nil {
		b.WriteString("  Uncommitted files unknown: git status failed: " + statusErr.Error() + "\n")
	} else if strings.TrimSpace(status) != "" {
		files := strings.Split(status, "\n")
		fmt.Fprintf(&b, "  Uncommitted (%d file%s):\n", len(files), plural(len(files)))
		for i, f := range files {
			if i == statusLines {
				fmt.Fprintf(&b, "    +%d more (git status)\n", len(files)-statusLines)
				break
			}
			b.WriteString("    " + f + "\n")
		}
	}

	if base != "" {
		if log := git("log", "-"+strconv.Itoa(baseCommits), "--format=%h%x09%cr%x09%s", base); log != "" {
			fmt.Fprintf(&b, "  Latest on %s:\n", base)
			for _, l := range strings.Split(log, "\n") {
				if f := strings.SplitN(l, "\t", 3); len(f) == 3 {
					subj := f[2]
					if t := truncRunes(subj, 80); t != subj {
						subj = strings.TrimSpace(t) + "…"
					}
					fmt.Fprintf(&b, "    %s %s (%s)\n", f[0], subj, f[1])
				}
			}
		}
	}

	wts, nwt := worktrees(top)
	for _, ref := range branchesFor(top, key) {
		if ref == branch {
			continue
		}
		line := "  Already named for " + key + ": " + ref
		if p := wts[ref]; p != "" {
			line += " (worktree " + p + ")"
		}
		if base != "" {
			line += ", " + aheadBehind(top, base, ref)
		}
		b.WriteString(line + "\n")
	}
	if nwt > 1 {
		fmt.Fprintf(&b, "  %d worktrees of this repo (wp tidy lists the merged ones)\n", nwt)
	}
	return b.String()
}

// checkoutBase is the default branch of the remote that was fetched
// (origin/main), so the report compares against what was just fetched. With
// no remote it is the local default branch.
func checkoutBase(top, remote string) string {
	if remote == "" {
		return defaultBranchRef(top)
	}
	if out, err := gitIn(top, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil && out != "" {
		return out
	}
	for _, r := range []string{remote + "/main", remote + "/master"} {
		if _, err := gitIn(top, "rev-parse", "--verify", "--quiet", r); err == nil {
			return r
		}
	}
	return ""
}

// aheadBehind says where ref stands against base: "even with origin/main",
// "3 behind origin/main", "2 ahead of and 3 behind origin/main".
func aheadBehind(top, base, ref string) string {
	out, err := gitIn(top, "rev-list", "--left-right", "--count", base+"..."+ref)
	if err != nil {
		return "not comparable with " + base
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return "not comparable with " + base
	}
	behind, ahead := f[0], f[1]
	switch {
	case ahead == "0" && behind == "0":
		return "even with " + base
	case ahead == "0":
		return behind + " behind " + base
	case behind == "0":
		return ahead + " ahead of " + base
	}
	return ahead + " ahead of and " + behind + " behind " + base
}

// branchesFor are the local and remote branches whose name carries the task
// key as a word (wp-301-next for WP-301, not wp-3010): work already begun on
// it. A remote branch with a local twin is left out; at most five.
func branchesFor(top, key string) []string {
	re := keyWordRE(key)
	if re == nil {
		return nil
	}
	refs := func(prefix string) []string {
		out, err := gitIn(top, "for-each-ref", "--format=%(refname:short)", prefix)
		if err != nil || out == "" {
			return nil
		}
		return strings.Split(out, "\n")
	}
	local := map[string]bool{}
	var found []string
	for _, r := range refs("refs/heads") {
		if re.MatchString(r) {
			local[r] = true
			found = append(found, r)
		}
	}
	for _, r := range refs("refs/remotes") {
		_, name, _ := strings.Cut(r, "/")
		if re.MatchString(name) && !local[name] {
			found = append(found, r)
		}
	}
	if len(found) > 5 {
		found = found[:5]
	}
	return found
}

// keyWordRE matches a branch name that carries key (WP-301) as a word:
// wp-301, wp301, feat/WP-301_x; not wp-3010 or xwp-301.
func keyWordRE(key string) *regexp.Regexp {
	prefix, num, ok := strings.Cut(strings.ToLower(key), "-")
	if !ok || prefix == "" || num == "" {
		return nil
	}
	return regexp.MustCompile(`(?i)(^|[^a-z0-9])` + regexp.QuoteMeta(prefix) + `-?` + regexp.QuoteMeta(num) + `($|[^0-9])`)
}

// worktrees are the repo's worktrees: how many, and the path of each one
// that has a branch checked out, by branch.
func worktrees(top string) (byBranch map[string]string, count int) {
	out, err := gitIn(top, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, 0
	}
	byBranch = map[string]string{}
	path := ""
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "worktree "):
			path = filepath.Clean(strings.TrimPrefix(l, "worktree "))
			count++
		case strings.HasPrefix(l, "branch "):
			byBranch[strings.TrimPrefix(strings.TrimPrefix(l, "branch "), "refs/heads/")] = path
		}
	}
	return byBranch, count
}
