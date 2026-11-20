package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Locked worktrees whose owner is gone (WP-1370). Claude Code locks the
// worktree it makes for an agent with the reason
//
//	claude agent NAME (pid N start T)   (a subagent's worktree)
//	claude session NAME (pid N start T) (claude --worktree NAME)
//
// where T is the process's start time as /proc/N/stat field 22 records it.
// Nothing ever unlocked them, so a task whose worktree (or whose branch's
// worktree) an agent had left behind could not be continued on its own
// branch. A lock is stale when it names a process that provably no longer
// exists: pid not running, or running with another start time (the pid was
// reused), or a zombie. Anything else (a reason in other words, no reason, a
// process still running, no view of /proc) is not shown gone, and the lock is
// left alone: age is no evidence, and a lock someone wrote by hand says what
// it is for in words only a person can judge.

var lockReasonRE = lazyre.New(`^claude (?:agent|session) \S+ \(pid ([0-9]+) start ([0-9]+)\)$`)

// lockProc is what /proc says of one process.
type lockProc struct {
	start  uint64 // /proc/N/stat field 22: start time in clock ticks after boot
	zombie bool   // exited, waiting to be reaped
}

// lockProcLookup reads process pid. found is false when there is no such
// process; err when /proc can't answer (not Linux, hidepid, ...), which says
// nothing about the process. A seam for tests.
var lockProcLookup = readLockProc

func readLockProc(pid int) (e lockProc, found bool, err error) {
	// Without a /proc of our own there is no view: a missing entry means nothing.
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return e, false, errors.New("no /proc to look in")
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") // #nosec G304 -- a pid read from a lock reason, as digits
	if errors.Is(err, fs.ErrNotExist) {
		return e, false, nil
	}
	if err != nil {
		return e, false, err
	}
	// "pid (comm) S ppid ...": comm may hold spaces and parentheses, so count
	// from the last ")". After it come fields 3 onward; start time is field 22.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return e, false, errors.New("unreadable /proc stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return e, false, errors.New("short /proc stat")
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return e, false, err
	}
	return lockProc{start: start, zombie: f[0] == "Z" || f[0] == "X"}, true, nil
}

// lockClock is this host's clock and uptime, from /proc/uptime. A seam for
// tests.
var lockClock = func() (now time.Time, uptime time.Duration, err error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}, 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 1 {
		return time.Time{}, 0, errors.New("unreadable /proc/uptime")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return time.Time{}, 0, err
	}
	return time.Now(), time.Duration(secs * float64(time.Second)), nil
}

// lockTick is /proc's start-time unit (USER_HZ, 100 on every Linux wp runs on).
const lockTick = 10 * time.Millisecond

// lockSlack is the clock skew and file-time granularity allowed when the lock
// file's time is set against a process's start.
const lockSlack = 5 * time.Second

// lockOwner judges a worktree lock's reason, written at lockedAt (the lock
// file's time; zero when unknown). gone is true only when the lock names a
// process of this host's current boot that no longer exists; why says what it
// found either way, in words for the agent.
//
// The pid and start tick mean something only on the host and boot that wrote
// them: a lock from another container's pid namespace, another host sharing
// the folder, or an earlier boot would be read against the wrong /proc. What
// this host can show: the start tick is no later than its uptime; the lock was
// written after this boot; and not before the process it names started. A lock
// failing any of these was not written by a process of this host's current
// boot, and is "can't tell", never gone. A lock from another pid namespace on
// this same kernel passes them and can't be told apart; that stays unproven.
func lockOwner(reason string, lockedAt time.Time) (gone bool, why string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return false, "its lock records no owner"
	}
	m := lockReasonRE.FindStringSubmatch(reason)
	if m == nil {
		return false, fmt.Sprintf("its lock says %q, which names no process wp can check", reason)
	}
	pid, err := strconv.Atoi(m[1])
	start, err2 := strconv.ParseUint(m[2], 10, 64)
	if err != nil || err2 != nil || pid <= 0 {
		return false, fmt.Sprintf("its lock says %q, which names no process wp can check", reason)
	}
	now, up, err := lockClock()
	if err != nil {
		return false, fmt.Sprintf("its lock names pid %d, and wp can't read this host's clock (%v)", pid, err)
	}
	startAfterBoot := time.Duration(start) * lockTick
	boot := now.Add(-up)
	switch {
	case startAfterBoot > up+lockSlack:
		return false, fmt.Sprintf("its lock names pid %d, started %s after a boot, later than this host has been up (%s): another host or container wrote it, so wp can't tell", pid, startAfterBoot.Round(time.Second), up.Round(time.Second))
	case lockedAt.IsZero():
		return false, fmt.Sprintf("its lock names pid %d, but wp can't tell when the lock was written", pid)
	case lockedAt.Before(boot.Add(-lockSlack)):
		return false, fmt.Sprintf("its lock names pid %d and was written before this host last booted, so wp can't tell whether that pid's owner is gone", pid)
	case lockedAt.Before(boot.Add(startAfterBoot - lockSlack)):
		return false, fmt.Sprintf("its lock names pid %d but was written before that process started on this host: another host or container wrote it, so wp can't tell", pid)
	}
	e, found, err := lockProcLookup(pid)
	switch {
	case err != nil:
		return false, fmt.Sprintf("its lock names pid %d, and wp can't tell whether that still runs (%v)", pid, err)
	case !found:
		return true, fmt.Sprintf("its lock names pid %d, which no longer runs", pid)
	case e.zombie:
		return true, fmt.Sprintf("its lock names pid %d, which has exited", pid)
	case e.start != start:
		return true, fmt.Sprintf("its lock names pid %d, which now belongs to another process (started at %d, not %d)", pid, e.start, start)
	}
	return false, fmt.Sprintf("its lock names pid %d, a Claude Code agent that is still running", pid)
}

// lockedAt is when the lock on the worktree at path was written: the time of
// the lock file in its administrative directory under common. Zero if it
// can't be found.
func lockedAt(common, path string) time.Time {
	ents, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		return time.Time{}
	}
	want := tidyReal(path)
	for _, e := range ents {
		dir := filepath.Join(common, "worktrees", e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "gitdir")) // #nosec G304 -- the repository's own worktree record
		if err != nil {
			continue
		}
		if tidyReal(filepath.Dir(strings.TrimSpace(string(b)))) != want {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, "locked")); err == nil {
			return fi.ModTime()
		}
	}
	return time.Time{}
}

// lockJudge is lockOwner for the worktree w of the repository whose common
// git directory is common.
func lockJudge(common string, w tidyWorktree) (gone bool, why string) {
	return lockOwner(w.lockReason, lockedAt(common, w.path))
}

// unlockStale unlocks the worktree at path if its lock's owner is gone. It
// looks at the lock again under a lock of its own on the repository (two wp
// at once take it in turn: the second finds it unlocked) and unlocks with
// git's own command, so a lock someone made in the meantime is never undone:
// an owner can only go from running to gone, so what was shown gone stays
// gone. reason is the lock it removed ("" if none was left). Never force.
func unlockStale(ctx context.Context, dir, path string) (reason string, err error) {
	common, err := tidyGit(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if f, err := os.OpenFile(filepath.Join(common, "wp-worktree.lock"), os.O_CREATE|os.O_RDWR, 0o600); err == nil { // #nosec G304 -- inside the repository's git directory
		if unlock, err := fileLock(f); err == nil {
			defer unlock()
		} else {
			_ = f.Close()
		}
	}
	out, err := tidyGit(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	p := tidyReal(path)
	for _, w := range parseWorktrees(out) {
		if w.main || (tidyReal(w.path) != p && filepath.Clean(w.path) != filepath.Clean(path)) {
			continue
		}
		if !w.locked {
			return "", nil
		}
		if gone, why := lockJudge(common, w); !gone {
			return "", errors.New(why)
		}
		if _, err := tidyGit(ctx, dir, "worktree", "unlock", w.path); err != nil {
			return "", err
		}
		return w.lockReason, nil
	}
	return "", nil // no longer registered: nothing to unlock
}

// relock puts back a lock unlockStale removed, when what was to follow it
// (removing the worktree) failed.
func relock(ctx context.Context, dir, path, reason string) {
	args := []string{"worktree", "lock"}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	_, _ = tidyGit(ctx, dir, append(args, path)...)
}

// lockedFor is what reclaimTask found for a task.
type lockedFor struct {
	taken   []string // worktrees whose stale lock was removed, each with why, for one line
	revive  string   // a branch of the task whose worktree's folder is gone, record pruned: check it out again
	blocked *tidyWorktree
	why     string // why blocked can't be taken
}

// reclaimTask frees what holds the task key's branches (see reclaimWhere).
func reclaimTask(ctx context.Context, repo *worktreeRepo, key string, top string) (*worktreeRepo, lockedFor) {
	return reclaimWhere(ctx, repo, top, func(w tidyWorktree) bool {
		return w.branch != "" && branchCarries(w.branch, []string{key})
	}, key)
}

// reclaimWhere frees the worktrees match picks, the one rule for a worktree
// that holds what a command wants (WP-1370): a worktree locked by
// an owner shown gone is unlocked (and, when its folder is gone, its record
// pruned so the branch is free), and one whose owner is alive or can't be
// shown gone, or the main checkout (which stays its person's), is answered as
// blocked. It returns the repository's worktrees as they are afterwards. It
// never removes a worktree with a folder, and never touches what it holds.
func reclaimWhere(ctx context.Context, repo *worktreeRepo, top string, match func(tidyWorktree) bool, key string) (*worktreeRepo, lockedFor) {
	var res lockedFor
	home := tidyReal(repo.home())
	changed, prune := false, false
	for i := range repo.wts {
		w := repo.wts[i]
		if w.bare || !match(w) {
			continue
		}
		if w.main {
			if top != "" && tidyReal(w.path) != tidyReal(top) && res.blocked == nil {
				res.blocked, res.why = &repo.wts[i], "the main checkout, which stays its person's"
			}
			continue
		}
		missing := !dirExists(w.path)
		if w.locked {
			gone, why := lockJudge(repo.common, w)
			if !gone {
				if res.blocked == nil {
					res.blocked, res.why = &repo.wts[i], "locked: "+why
				}
				continue
			}
			if _, err := unlockStale(ctx, repo.root, w.path); err != nil {
				if res.blocked == nil {
					res.blocked, res.why = &repo.wts[i], "locked: "+err.Error()
				}
				continue
			}
			res.taken = append(res.taken, fmt.Sprintf("%s (%s)", w.path, why))
			changed = true
		}
		if missing {
			res.revive, prune = w.branch, true
		}
	}
	if prune {
		// git worktree prune drops every unlocked missing folder's record: not
		// those of other work outside the repo's own folder, where a disk may
		// only be unmounted.
		for _, w := range repo.wts {
			if !w.main && !w.bare && !w.locked && !dirExists(w.path) && !inside(tidyReal(w.path), home) &&
				!match(w) {
				prune = false
			}
		}
		if prune {
			_, _ = tidyGit(ctx, repo.root, "worktree", "prune")
			changed = true
		} else {
			for i := range repo.wts {
				if w := repo.wts[i]; w.branch == res.revive && !dirExists(w.path) {
					res.blocked, res.why = &repo.wts[i], "its folder is gone, but pruning its record would drop another missing worktree's too"
				}
			}
			res.revive = ""
		}
	}
	if changed {
		if again, err := worktreeRepoAt(ctx, repo.root); err == nil {
			repo = again
		}
	}
	return repo, res
}

// freeBranch is a branch name for continuing from branch while it is held
// elsewhere: branch-2 and on, the first with no local or origin branch. It
// carries the task's key because branch did.
func freeBranch(ctx context.Context, root, branch string) string {
	for i := 2; i < 100; i++ {
		n := fmt.Sprintf("%s-%d", branch, i)
		_, errL := tidyGit(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+n)
		_, errR := tidyGit(ctx, root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+n)
		if errL != nil && errR != nil {
			return n
		}
	}
	return ""
}

// continuation is the task carried on from work that another's worktree
// holds and wp won't take: a worktree at slug on a new branch cut from the
// held branch. Its branch name carries the task's key (branchKeyRE), so
// Waypoint maps the review and pull request to the task: no agent invents one.
type continuation struct {
	key, from, branch, slug, heldAt, why string
}

// continueFrom plans the continuation of blocked's work for task key; the
// zero value when no free name is found.
func continueFrom(ctx context.Context, repo *worktreeRepo, blocked *tidyWorktree, why, slug string) continuation {
	key := ""
	if m := branchKeyRE.FindStringSubmatch(blocked.branch); m != nil {
		key = strings.ToUpper(m[1])
	}
	b := freeBranch(ctx, repo.root, blocked.branch)
	if b == "" {
		return continuation{}
	}
	slug = filepath.Base(freePath(repo.home(), slug, nil))
	return continuation{key: key, from: blocked.branch, branch: b, slug: slug, heldAt: blocked.path, why: why}
}

// command is what to run, with the reason, for an agent that makes the
// worktree itself.
func (c continuation) command() string {
	return fmt.Sprintf("`wp worktree %s --branch %s --from %s` continues %s's work on %s%s", c.slug, c.branch, c.from, c.from, c.branch, c.maps())
}

// maps says that the branch's name maps to the task, when it carries a key.
func (c continuation) maps() string {
	if c.key == "" {
		return ""
	}
	return ", a branch name Waypoint maps to " + c.key
}

// explain says why the task isn't on its own branch, and where it is.
func (c continuation) explain() string {
	who := c.from
	if c.key != "" {
		who += " (" + c.key + "'s earlier work)"
	}
	return fmt.Sprintf("%s is checked out in %s, which is %s, so wp left it alone. This worktree continues from %s's last commit on %s%s: use %s for the pull request, not a name of your own. Uncommitted files in %s stay there.",
		who, c.heldAt, c.why, c.from, c.branch, c.maps(), c.branch, c.heldAt)
}

// lockedByOther says whether the worktree at path is locked and its owner
// isn't shown gone.
func lockedByOther(ctx context.Context, root, path string) bool {
	repo, err := worktreeRepoAt(ctx, root)
	if err != nil {
		return false
	}
	w := repo.find(path)
	if w == nil || !w.locked {
		return false
	}
	gone, _ := lockJudge(repo.common, *w)
	return !gone
}
