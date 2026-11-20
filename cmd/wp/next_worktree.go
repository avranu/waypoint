package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A task in a worktree of its own (WP-200). Agents taking a task in a
// person's session worked wherever the session was: the main checkout, left
// detached with another task's unpushed commits, or a worktree on another
// task's branch, which the next session had to tiptoe around. So when an
// agent takes a task with wp next in the main checkout, or in a checkout
// that holds other work, wp makes the task its worktree under
// .claude/worktrees/ (wp worktree, WP-563) and says to cd there; and since
// the pile has to stay bounded, it then removes the repo's finished ones
// there, as autopilot does before each start (WP-ADR57). Autopilot's own
// sessions start in a fresh worktree named for the task, so they stay put.

// sessionFolder is the folder the agent's session runs in: Claude Code
// reads and writes there without asking, and keeps a cd only inside it. On
// Linux it is the folder the Claude Code process ($CLAUDE_PID) started in;
// "" when unknown.
var sessionFolder = func() string {
	pid := os.Getenv("CLAUDE_PID")
	if n, err := strconv.Atoi(pid); err != nil || n <= 0 {
		return ""
	}
	p, err := os.Readlink(filepath.Join("/proc", pid, "cwd"))
	if err != nil {
		return ""
	}
	return p
}

// noWorktreeEnv set to anything keeps a task taken in the checkout it is
// taken in, as --no-worktree does.
const noWorktreeEnv = "WP_NO_WORKTREE"

// checkoutHolds says why the checkout whose top is top isn't the place for
// task key's work: it is the main checkout, or it holds other work
// (uncommitted files, commits not on base). "" when it is: its branch names
// the task, or it holds nothing. A worktree on a branch that names another
// task but holds no work stays usable (autopilot names a session's worktree
// for the epic whose sub-task it takes).
func checkoutHolds(top, key, base string, main bool) string {
	branch, _ := gitIn(top, "symbolic-ref", "--short", "-q", "HEAD")
	if branch != "" && branchCarries(branch, []string{key}) {
		return ""
	}
	if main {
		return "this is the main checkout, which stays its person's (WP-ADR57)"
	}
	held := heldWork(top, base)
	if len(held) == 0 {
		return ""
	}
	whose := "other work"
	if m := branchKeyRE.FindStringSubmatch(branch); m != nil {
		whose = strings.ToUpper(m[1]) + "'s work"
	} else if branch == "" {
		whose = "other work on a detached HEAD"
	}
	return "this checkout holds " + whose + " (" + strings.Join(held, ", ") + ")"
}

// heldWork lists what the checkout at top holds that isn't on base:
// uncommitted files, commits not on base (not on any remote, with no base).
func heldWork(top, base string) []string {
	var held []string
	if st, err := gitOutput(top, "status", "--porcelain"); err == nil && strings.TrimSpace(st) != "" {
		n := len(strings.Split(strings.TrimRight(st, "\n"), "\n"))
		held = append(held, fmt.Sprintf("%d uncommitted file%s", n, plural(n)))
	}
	// With no default branch to compare to, commits on no remote branch count.
	revArgs, onWhat := []string{"rev-list", "--count", "HEAD", "--not", "--remotes"}, "a remote"
	if base != "" {
		revArgs, onWhat = []string{"rev-list", "--count", base + "..HEAD"}, base
	}
	if out, err := gitIn(top, revArgs...); err == nil {
		if n, _ := strconv.Atoi(out); n > 0 {
			held = append(held, fmt.Sprintf("%d commit%s not on %s", n, plural(n), onWhat))
		}
	}
	return held
}

// ownWorktree is where an agent works on task key taken in dir: a worktree
// of its own when checkoutHolds says dir's checkout isn't the place and the
// worktree's folder is inside the session's (else the agent would be asked
// for every read there). It prints what it does as it goes, so a command cut
// off during the setup has already said where to work, and answers with the
// folder whose git state to report.
func (c *cli) ownWorktree(dir, key string) (at string) {
	if c.noOwnWorktree || os.Getenv(noWorktreeEnv) != "" || c.agentHeader() == "" {
		return dir
	}
	top := gitTop(dir)
	if top == "" {
		return dir
	}
	ctx := context.Background()
	repo, err := worktreeRepoAt(ctx, top)
	if err != nil {
		return dir
	}
	why := checkoutHolds(top, key, checkoutBase(top, fetchRemote(top)), tidyReal(top) == tidyReal(repo.root))
	if why == "" {
		return dir
	}
	slug := worktreeSlug(key)
	target := filepath.Join(repo.home(), slug)
	// A worktree of the task left locked by an agent that is gone is taken
	// over; one whose owner is alive (or can't be shown gone) is not, and the
	// task goes on a branch cut from its work (WP-1370).
	repo, held := reclaimTask(ctx, repo, key, top)
	existing := ""
	for _, w := range repo.wts {
		if !w.main && !w.prunable && !w.locked && w.branch != "" && branchCarries(w.branch, []string{key}) {
			existing, target = w.path, w.path
			break
		}
	}
	var cont continuation
	if existing == "" && held.revive == "" && held.blocked != nil {
		cont = continueFrom(ctx, repo, held.blocked, held.why, slug)
	}
	folder := sessionFolder()
	if folder == "" {
		folder = top
	}
	c.println()
	if !inside(tidyReal(target), tidyReal(folder)) {
		cmd := "wp worktree " + key
		if existing != "" {
			cmd = "cd " + existing
		} else if cont.branch != "" {
			cmd = cont.command()
		}
		c.println(fmt.Sprintf("Worktree: %s; %s's own would be outside this session's folder (%s), where every read asks first, so wp left it: `%s` if you want it.", why, key, folder, cmd))
		return dir
	}
	var made madeWorktree
	if existing == "" {
		// wp next fetched a moment ago (or the remote didn't answer): no second fetch.
		switch {
		case held.revive != "": // its folder was gone: the task's own branch again
			made, err = makeWorktree(ctx, repo, slug, held.revive, "", false, c.note)
		case cont.branch != "":
			made, err = makeWorktree(ctx, repo, cont.slug, cont.branch, cont.from, false, c.note)
		default:
			made, err = makeWorktree(ctx, repo, slug, "", "", false, c.note)
		}
		if err != nil {
			// Another agent starting the same task a moment ago may have made it.
			if again, e2 := worktreeRepoAt(ctx, top); e2 == nil {
				for _, w := range again.wts {
					if !w.main && !w.prunable && !w.locked && w.branch != "" && branchCarries(w.branch, []string{key}) {
						existing, target, err = w.path, w.path, nil
						break
					}
				}
			}
		}
		if err != nil {
			c.println(fmt.Sprintf("Worktree: %s, but making %s's own failed (%v); working here.", why, key, err))
			if cont.branch != "" {
				c.println("  " + cont.command())
			}
			return dir
		}
		if existing == "" {
			target = made.path
		}
	}
	if existing == "" && made.made == "" && !made.branchOut {
		// A worktree already at the path: ours only if it carries the task or holds nothing.
		if w := repo.find(made.path); w != nil && (w.locked || checkoutHolds(made.path, key, checkoutBase(made.path, fetchRemote(made.path)), false) != "") {
			c.println(fmt.Sprintf("Worktree: %s, but %s is another's (locked, or holds other work); working here.", why, made.path))
			c.println(fmt.Sprintf("  Make the task's own on a branch named for it: wp worktree %s-2 --branch %s-2 (any branch name holding %s maps to the task)", slug, strings.ToLower(key), key))
			return dir
		}
	}
	keys := c.takenKeys
	if len(keys) == 0 {
		keys = []string{key}
	}
	claimErr := c.moveClaim(dir, target, keys)
	c.println("Worktree of its own: " + why + ".")
	for _, t := range append(slices.Clone(held.taken), made.took...) {
		c.println("  Took over " + t + ": the agent that locked it is gone, so the lock was stale.")
	}
	if claimErr != nil {
		c.println(fmt.Sprintf("  Couldn't record the task for that folder (%v): wp ship there won't find it; pass the key.", claimErr))
	}
	switch {
	case existing != "":
		c.println("  " + key + " already has one: " + existing)
		if n := heldWork(existing, checkoutBase(existing, fetchRemote(existing))); len(n) > 0 {
			c.println("  It holds " + strings.Join(n, ", ") + ": the task's own work, left in place.")
		}
	case made.made != "":
		c.println("  Made " + made.path + " on " + made.made)
		if cont.branch != "" {
			c.println("  " + cont.explain())
		}
	default:
		c.println("  " + made.path + " is already a worktree (" + made.what + ")")
	}
	c.println("  Work there: cd " + target + " (the task is taken for that folder; WP_NO_WORKTREE=1 or wp next --no-worktree keeps the next one where it is taken)")
	if made.made != "" {
		if fileExists(filepath.Join(target, autopilotSetupFile)) {
			c.note("wp: running %s in %s (it can take minutes; if this command is cut off, run it there yourself: cd %s && ./%s)", autopilotSetupFile, target, target, autopilotSetupFile)
		}
		for _, n := range setupWorktree(ctx, repo.root, target) {
			c.println("  " + strings.ReplaceAll(n, "\n", "\n    "))
		}
	}
	if runtime.GOOS != "linux" {
		return target // no /proc: the sweep can't see who is in a worktree, so it never runs
	}
	if gone, err := c.sweepFinishedWorktrees(ctx, repo, target); err != nil {
		c.println("  Finished worktrees left as they are: " + err.Error())
	} else if len(gone) > 0 {
		c.println(fmt.Sprintf("  Removed %d finished worktree%s (merged or their sessions over; clean; all pushed; a merged one's branch too): %s", len(gone), plural(len(gone)), strings.Join(gone, ", ")))
	}
	return target
}

// moveClaim records the tasks keys, taken in from, for the worktree to: wp
// ship there finds them, and the token hook counts the session's work on
// them. The claim in from keeps the tasks it held besides (and goes when it
// holds none); one already in to keeps its tasks too.
func (c *cli) moveClaim(from, to string, keys []string) error {
	from, to = filepath.Clean(from), filepath.Clean(to)
	return c.updateState(func(s *State) {
		old := s.Claims[from]
		if old == nil || from == to {
			return
		}
		var moving, staying []string
		for _, k := range old.keys() {
			if slices.ContainsFunc(keys, func(x string) bool { return sameKey(x, k) }) {
				moving = append(moving, k)
			} else {
				staying = append(staying, k)
			}
		}
		if len(moving) == 0 {
			return
		}
		moved := *old
		moved.Key, moved.Top = moving[0], checkoutOf(to)
		also := moving[1:]
		if prev := s.Claims[to]; prev != nil {
			also = addKeys(also, prev.keys()...)
		}
		moved.Also = slices.DeleteFunc(also, func(k string) bool { return sameKey(k, moved.Key) })
		s.Claims[to] = &moved
		if len(staying) == 0 {
			delete(s.Claims, from)
			return
		}
		kept := *old
		kept.Key, kept.Also = staying[0], staying[1:]
		s.Claims[from] = &kept
	})
}

// sweepFinishedWorktrees removes the repo's worktrees under
// .claude/worktrees/ that wp tidy --apply would (merged, or every session
// that worked there over; clean; nothing unpushed), except spare and any a
// process or a live Waypoint session is in. Without a view of both it
// removes nothing. It answers with the folders' names.
func (c *cli) sweepFinishedWorktrees(ctx context.Context, repo *worktreeRepo, spare string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cwds, err := procCwds()
	if err != nil {
		return nil, fmt.Errorf("can't see which folders processes are in (%v)", err)
	}
	live, err := c.liveSessionCwds()
	if err != nil {
		return nil, fmt.Errorf("live sessions unavailable (%v)", err)
	}
	slug, _, _ := gitInfo(repo.root)
	merged, _ := c.tidyMergedPRs(slug) // without them only plain merges count: fewer go
	o := tidyOpts{worktrees: true, worktreeBranches: true, sessionEnded: c.tidySessionEnded(), now: worktreeNow()}
	rep, err := tidyScan(ctx, repo.root, merged, o)
	if err != nil {
		return nil, err
	}
	home, spare := tidyReal(repo.home()), tidyReal(spare)
	busy := append(slices.Clone(cwds), live...)
	// Who is where may have changed during the scan: look again just before
	// removing, and spare what has been entered.
	if again, err := procCwds(); err == nil {
		busy = append(busy, again...)
	}
	if again, err := c.liveSessionCwds(); err == nil {
		busy = append(busy, again...)
	}
	for _, r := range rep.Rows {
		if r.Kind != "worktree" {
			if r.Verdict == tidyRemovable {
				r.Verdict = tidyKeep // only worktrees (and their merged branches) go here
			}
			continue
		}
		p := tidyReal(r.Path)
		if r.prunable {
			p = filepath.Clean(r.Path) // gone: nothing to resolve
		}
		keep := p == home || !inside(p, home) || inside(spare, p) ||
			slices.ContainsFunc(busy, func(d string) bool { return inside(tidyReal(d), p) })
		if keep && r.Verdict == tidyRemovable {
			r.Verdict = tidyKeep
		}
		if r.prunable && r.Verdict != tidyRemovable {
			o.noPrune = true // git worktree prune would drop its record too
		}
	}
	tidyApply(ctx, repo.root, rep, o)
	var gone []string
	for _, r := range rep.Rows {
		if strings.HasPrefix(r.Action, "removed") || r.Action == "pruned" {
			gone = append(gone, filepath.Base(r.Path))
		}
	}
	return gone, nil
}

// repoHere says whether repo (owner/name) is the one this command runs in.
func (c *cli) repoHere(repo string) bool {
	dir, err := os.Getwd()
	if err != nil {
		return false
	}
	here, _, _ := gitInfo(dir)
	return here != "" && strings.EqualFold(strings.TrimSuffix(repo, ".git"), here)
}
