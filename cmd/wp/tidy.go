package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// wp tidy (ADR2 §5, WP-136): which local branches and worktrees are
// finished, and removing exactly those. The verdicts follow git-tidy's
// (removable, keep, protected, stale) and its safety rules; what Waypoint adds
// is that it knows squash and rebase merges: a branch whose pull request
// merged, on the head the branch still has, is as merged as one git can see.

// Verdicts.
const (
	tidyRemovable = "removable"
	tidyKeep      = "keep"      // needs a person: dirty, unpushed, unmerged, locked
	tidyProtected = "protected" // the default branch, release/*, where you are
	tidyStale     = "stale"     // unmerged and idle for a month: report only
)

const tidyStaleAfter = 30 * 24 * time.Hour

// tidyQuiet is how long a merged branch or worktree must have been left alone
// before it is removable: a worktree an agent just made from the default
// branch has nothing of its own yet and is "merged" until it commits.
const tidyQuiet = 2 * time.Hour

// touchedAgo is how long since the newest of refs' reflogs (read in dir)
// moved; -1 when there is no reflog to say.
func touchedAgo(ctx context.Context, dir string, now time.Time, refs ...string) time.Duration {
	newest := time.Duration(-1)
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		out, err := tidyGit(ctx, dir, "reflog", "show", "-1", "--format=%ct", ref)
		if err != nil {
			continue
		}
		if ts, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
			if d := now.Sub(time.Unix(ts, 0)); newest < 0 || d < newest {
				newest = d
			}
		}
	}
	return newest
}

var tidyCmd = &command{
	name: "tidy", group: "Code and CI", args: "[--apply] [--explain BRANCH] [--worktrees-only|--branches-only] [--remote]",
	summary: "list merged local branches and worktrees; --apply removes them",
	about: `Gives each local branch and worktree a verdict and reason: removable
(merged, clean, nothing unpushed, not where you are, not locked), keep (needs
a person), protected (default branch, release/*, where you are), stale
(unmerged, no commit for 30 days; reported, never removed). Merged includes
squash and rebase merges Waypoint knows of. Anything touched in the last two
hours is kept. An unmerged worktree is removable too when it is clean, every
commit is on a remote, no process is in it, and every session Waypoint
recorded in it on this machine has ended; its branch stays.

Only reads until --apply: it fetches --prune, removes removable worktrees
(never --force) and deletes their branches. Remote branches are never touched.
--remote lists the forge's branches Waypoint's daily sweep deletes or keeps.
--explain BRANCH lists every rule for it.`,
	examples: []string{"wp tidy --apply", "wp tidy --explain wp-12-calmer-ui", "wp tidy --remote"},
	run:      cmdTidy,
}

// tidyRow is one worktree or branch with its verdict.
type tidyRow struct {
	Kind      string   `json:"kind"` // worktree or branch
	Branch    string   `json:"branch,omitempty"`
	Path      string   `json:"path,omitempty"`
	Tip       string   `json:"tip,omitempty"`
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason"`
	MergedBy  string   `json:"merged_by,omitempty"` // "origin/main" or "PR #12"
	Checks    []string `json:"checks,omitempty"`    // every rule, for --explain
	Action    string   `json:"action,omitempty"`    // after --apply
	idle      time.Duration
	prunable  bool   // a worktree whose directory is gone
	staleLock string // the lock reason of a worktree whose lock's owner is gone (WP-1370): unlocked first when removed
	stays     string // why a kept worktree is worth telling a person: uncommitted, unpushed, in use, held
	dirty     string // what is uncommitted, whatever the verdict
	unpushed  int    // commits on no remote; only counted when not merged
}

func (r *tidyRow) name() string {
	switch {
	case r.Kind == "worktree" && r.Branch != "":
		return r.Path + " (" + r.Branch + ")"
	case r.Kind == "worktree":
		return r.Path
	}
	return r.Branch
}

// tidyPR is a merged pull request, as far as tidy needs it.
type tidyPR struct {
	Number  int    `json:"number"`
	HeadRef string `json:"head_ref"`
	HeadSHA string `json:"head_sha"`
	Base    string `json:"base_ref"`
	State   string `json:"state"`
}

// tidyReport is what wp tidy found.
type tidyReport struct {
	Repo    string         `json:"repo,omitempty"`
	Default string         `json:"default_branch"`
	Rows    []*tidyRow     `json:"rows"`
	Counts  map[string]int `json:"counts"`
	Notes   []string       `json:"notes,omitempty"`
}

// tidyOpts select what a scan covers.
type tidyOpts struct {
	worktrees, branches bool // which rows (both: all)
	now                 time.Time
	quick               bool // only what decides "removable" (the SessionStart hint): no unpushed counts
	// Autopilot's sweep (WP-ADR57): ended is the worktree of the session it
	// has just finished with, which needn't be merged or quiet to go; and
	// worktreeBranches deletes a removed worktree's branch when it is merged.
	ended            string
	worktreeBranches bool
	// noPrune leaves git worktree prune out: a caller kept a worktree whose
	// folder is missing (wp next's sweep keeps those outside
	// .claude/worktrees/), and prune would drop its record with the rest.
	noPrune bool
	// sessionEnded, when set, says whether the sessions that worked in a
	// clean, fully pushed, unmerged worktree are all over (wp tidy asks
	// Waypoint's session record and the machine's processes, WP-843); such a
	// worktree goes as autopilot's ended one does. Never with quick, which
	// counts no unpushed commits.
	sessionEnded func(path, branch string) bool
	// intoDefault counts only pull requests merged into the default branch,
	// for a caller asking whether that branch has the work (WP-ADR74).
	intoDefault bool
}

// tidyGit runs git in dir; stdout trimmed.
func tidyGit(ctx context.Context, dir string, args ...string) (string, error) {
	return tidyGitEnv(ctx, dir, nil, args...)
}

// tidyGitEnv is tidyGit with extra environment.
func tidyGitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) // #nosec G204 G702 -- fixed git subcommands; dir is the user's repo
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0"), env...)
	cmd.WaitDelay = 2 * time.Second // ssh or credential helpers may hold the pipes open past a timeout's kill
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// tidyWorktree is one entry of git worktree list.
type tidyWorktree struct {
	path, head, branch string
	locked, prunable   bool
	lockReason         string // what git worktree lock --reason recorded, "" if nothing
	bare, main         bool
}

func parseWorktrees(porcelain string) []tidyWorktree {
	var out []tidyWorktree
	var cur *tidyWorktree
	for _, line := range strings.Split(porcelain, "\n") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			out = append(out, tidyWorktree{path: val})
			cur = &out[len(out)-1]
		case "HEAD":
			cur.head = val
		case "branch":
			cur.branch = strings.TrimPrefix(val, "refs/heads/")
		case "locked":
			cur.locked = true
			cur.lockReason = strings.TrimSpace(val)
		case "prunable":
			cur.prunable = true
		case "bare":
			cur.bare = true
		}
	}
	if len(out) > 0 {
		out[0].main = true
	}
	return out
}

// tidyScan judges the branches and worktrees of the repo containing dir.
// merged are the pull requests Waypoint knows merged (may be nil).
func tidyScan(ctx context.Context, dir string, merged []tidyPR, o tidyOpts) (*tidyReport, error) {
	g := func(args ...string) (string, error) { return tidyGit(ctx, dir, args...) }
	if o.now.IsZero() {
		o.now = time.Now()
	}
	top, err := g("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not in a git repository")
	}
	top = tidyReal(top)
	rep := &tidyReport{Counts: map[string]int{}}

	// The default branch and what counts as merged into it.
	def := ""
	if ref, err := g("symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		def = strings.TrimPrefix(ref, "origin/")
	}
	has := func(ref string) bool { _, err := g("rev-parse", "-q", "--verify", ref+"^{commit}"); return err == nil }
	if def == "" {
		for _, c := range []string{"main", "master", "trunk"} {
			if has("refs/heads/" + c) {
				def = c
				break
			}
		}
	}
	if def == "" {
		return nil, fmt.Errorf("can't tell the default branch (no origin/HEAD, no main or master)")
	}
	rep.Default = def
	base := "refs/heads/" + def
	if has("refs/remotes/origin/" + def) {
		base = "refs/remotes/origin/" + def
	} else {
		rep.Notes = append(rep.Notes, "no origin/"+def+": merged is judged against the local "+def)
	}

	// Where we are.
	curBranch, _ := g("symbolic-ref", "-q", "--short", "HEAD")

	wtOut, err := g("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	wts := parseWorktrees(wtOut)
	common, _ := g("rev-parse", "--path-format=absolute", "--git-common-dir")
	byBranch := map[string]*tidyWorktree{}
	for i := range wts {
		if wts[i].branch != "" && !wts[i].prunable {
			byBranch[wts[i].branch] = &wts[i]
		}
	}

	// Local branches.
	type branchInfo struct {
		tip  string
		idle time.Duration
	}
	branches := map[string]branchInfo{}
	var names []string
	if out, err := g("for-each-ref", "--format=%(refname:short)%09%(objectname)%09%(committerdate:unix)", "refs/heads"); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			f := strings.Split(line, "\t")
			if len(f) < 3 {
				continue
			}
			ts, _ := strconv.ParseInt(f[2], 10, 64)
			branches[f[0]] = branchInfo{tip: f[1], idle: o.now.Sub(time.Unix(ts, 0))}
			names = append(names, f[0])
		}
	}
	sort.Strings(names)

	isAncestor := func(a, b string) bool {
		if strings.HasPrefix(a, "-") || strings.HasPrefix(b, "-") { // a forge's head sha is a sha, not an option
			return false
		}
		_, err := g("merge-base", "--is-ancestor", a, b)
		return err == nil
	}
	// One call tells which branches are in base; a detached HEAD needs its own.
	inBase := map[string]bool{}
	if out, err := g("for-each-ref", "--merged="+base, "--format=%(refname:short)", "refs/heads"); err == nil && out != "" {
		for _, n := range strings.Split(out, "\n") {
			inBase[n] = true
		}
	}
	// judge says how a tip is merged: "" when it isn't.
	judge := func(branch, tip string) (by string, checks []string) {
		inBaseNow := false
		if branch != "" {
			inBaseNow = inBase[branch] && branches[branch].tip == tip
		} else {
			inBaseNow = isAncestor(tip, base)
		}
		if inBaseNow {
			checks = append(checks, "merged: the tip is in "+strings.TrimPrefix(base, "refs/remotes/"))
			return strings.TrimPrefix(strings.TrimPrefix(base, "refs/remotes/"), "refs/heads/"), checks
		}
		checks = append(checks, "merged: the tip is not in "+strings.TrimPrefix(base, "refs/remotes/"))
		if branch == "" {
			return "", checks
		}
		for _, pr := range merged {
			if pr.HeadRef != branch || pr.HeadSHA == "" || (o.intoDefault && pr.Base != def) {
				continue
			}
			switch {
			case pr.HeadSHA == tip:
				checks = append(checks, fmt.Sprintf("merged: pull request #%d merged this very commit", pr.Number))
				return fmt.Sprintf("PR #%d", pr.Number), checks
			case isAncestor(tip, pr.HeadSHA):
				checks = append(checks, fmt.Sprintf("merged: the tip is behind pull request #%d's merged head", pr.Number))
				return fmt.Sprintf("PR #%d", pr.Number), checks
			default:
				checks = append(checks, fmt.Sprintf("merged: pull request #%d merged, but this branch has commits it didn't have", pr.Number))
			}
		}
		return "", checks
	}
	unpushed := func(tip string) int {
		if o.quick {
			return 0
		}
		out, err := g("rev-list", "--count", tip, "--not", "--remotes")
		n, _ := strconv.Atoi(out)
		if err != nil {
			return 1
		}
		return n
	}
	// protectedName: the default branch and the release branches.
	protectedName := func(b string) string {
		switch {
		case b == def:
			return "the default branch"
		case strings.HasPrefix(b, "release/"):
			return "a release branch"
		}
		return ""
	}

	// decide turns the facts into a verdict.
	type facts struct {
		branch, tip      string
		by               string
		checks           []string
		dirty            string // what is uncommitted, "" when clean
		locked           bool
		inUse            string // "the worktree you are in", "the main checkout", ...
		idle             time.Duration
		unpushedN        int
		checkedOutIn     string // a branch row whose branch is in a worktree
		detachedUnmerged bool
		touched          time.Duration // since a ref moved or HEAD did; -1: unknown
		ended            bool          // the worktree of a session autopilot has finished with
	}
	decide := func(f facts) (verdict, reason string) {
		switch {
		case f.inUse != "":
			return tidyProtected, f.inUse
		case f.branch != "" && protectedName(f.branch) != "":
			return tidyProtected, protectedName(f.branch)
		case f.locked:
			return tidyKeep, "worktree is locked"
		case f.dirty != "":
			return tidyKeep, f.dirty
		case f.checkedOutIn != "":
			return tidyKeep, "checked out in " + f.checkedOutIn
		case f.by != "" && !f.ended && f.touched >= 0 && f.touched < tidyQuiet:
			// A branch just cut from the default branch is "merged" too.
			return tidyKeep, fmt.Sprintf("active: moved %d min ago", int(f.touched.Minutes()))
		case f.by != "":
			return tidyRemovable, "merged (" + f.by + "), clean, nothing unpushed"
		case f.unpushedN > 0:
			return tidyKeep, fmt.Sprintf("%d commit%s not pushed anywhere", f.unpushedN, plural(f.unpushedN))
		case f.ended && f.branch != "":
			return tidyRemovable, "its session is finished, clean, all pushed; the branch stays"
		case f.idle > tidyStaleAfter:
			return tidyStale, fmt.Sprintf("unmerged, no commit for %d days", int(f.idle.Hours()/24))
		case f.detachedUnmerged:
			return tidyKeep, "detached HEAD not in " + def
		}
		return tidyKeep, "unmerged"
	}

	if o.worktrees {
		for _, w := range wts {
			if w.bare {
				continue
			}
			row := &tidyRow{Kind: "worktree", Path: w.path, Branch: w.branch, Tip: w.head}
			// A lock whose owner is provably gone doesn't keep a worktree (WP-1370).
			staleWhy := ""
			if w.locked {
				if gone, why := lockJudge(common, w); gone {
					staleWhy = why
					row.staleLock = w.lockReason
				}
			}
			locked := w.locked && staleWhy == ""
			f := facts{branch: w.branch, tip: w.head, locked: locked, ended: o.ended != "" && tidyReal(w.path) == o.ended}
			if bi, ok := branches[w.branch]; ok {
				f.idle = bi.idle
			}
			if w.main {
				f.inUse = "the main checkout"
			}
			if tidyReal(w.path) == top {
				f.inUse = "the worktree you are in"
			}
			if w.prunable || (w.locked && !dirExists(w.path)) { // git doesn't call a locked one prunable
				row.prunable = true
				row.Verdict, row.Reason = tidyRemovable, "the directory is gone (git worktree prune)"
				row.Checks = []string{"directory: missing, nothing to lose"}
				// Its branch is judged as a branch too (it has no worktree row of its own
				// there), but the row names it, so --explain BRANCH finds the worktree.
				if locked {
					row.Verdict, row.Reason = tidyKeep, "worktree is locked"
				} else if staleWhy != "" {
					row.Checks = append(row.Checks, "lock: owner gone ("+staleWhy+")")
				}
				rep.Rows = append(rep.Rows, row)
				continue
			}
			f.by, f.checks = judge(w.branch, w.head)
			f.touched = -1
			if f.by != "" {
				f.touched = touchedAgo(ctx, w.path, o.now, "HEAD", branchRef(w.branch))
			}
			f.dirty = tidyDirty(ctx, w.path)
			if f.dirty == "" {
				f.checks = append(f.checks, "clean: nothing modified, staged, untracked or in progress")
			} else {
				f.checks = append(f.checks, "clean: no, "+f.dirty)
			}
			if f.by == "" {
				f.unpushedN = unpushed(w.head)
				f.detachedUnmerged = w.branch == ""
				if !f.ended && !o.quick && o.sessionEnded != nil && f.unpushedN == 0 && f.dirty == "" && w.branch != "" &&
					!f.locked && f.inUse == "" && protectedName(w.branch) == "" && o.sessionEnded(w.path, w.branch) {
					f.ended = true
					f.checks = append(f.checks, "sessions: every one that worked here has ended, and no process is in it")
				}
			}
			f.checks = append(f.checks, fmt.Sprintf("unpushed: %d commits on no remote", f.unpushedN),
				fmt.Sprintf("locked: %v", f.locked), "in use: "+cmpStr(f.inUse, "no"))
			if staleWhy != "" {
				f.checks = append(f.checks, "lock: owner gone ("+staleWhy+")")
			}
			row.Verdict, row.Reason = decide(f)
			row.MergedBy, row.Checks, row.idle = f.by, f.checks, f.idle
			row.dirty, row.unpushed = f.dirty, f.unpushedN
			if row.Verdict == tidyKeep && !f.locked {
				switch {
				case f.dirty != "":
					row.stays = "uncommitted"
				case f.unpushedN > 0:
					row.stays = "unpushed"
				}
			}
			rep.Rows = append(rep.Rows, row)
		}
	}
	if o.branches {
		for _, b := range names {
			bi := branches[b]
			w := byBranch[b]
			if w != nil && o.worktrees {
				continue // its worktree row covers it
			}
			row := &tidyRow{Kind: "branch", Branch: b, Tip: bi.tip}
			f := facts{branch: b, tip: bi.tip, idle: bi.idle}
			f.by, f.checks = judge(b, bi.tip)
			f.touched = -1
			if f.by != "" {
				f.touched = touchedAgo(ctx, dir, o.now, "refs/heads/"+b)
			}
			if f.by == "" {
				f.unpushedN = unpushed(bi.tip)
			}
			f.checks = append(f.checks, fmt.Sprintf("unpushed: %d commits on no remote", f.unpushedN))
			if b == curBranch {
				f.inUse = "the branch you are on"
			}
			if w != nil {
				f.checkedOutIn = w.path
				f.checks = append(f.checks, "checked out in "+w.path)
				if w.main {
					f.inUse = "checked out in the main checkout"
				}
			}
			row.Verdict, row.Reason = decide(f)
			row.MergedBy, row.Checks, row.idle = f.by, f.checks, f.idle
			row.unpushed = f.unpushedN
			rep.Rows = append(rep.Rows, row)
		}
	}
	sort.SliceStable(rep.Rows, func(i, j int) bool {
		a, b := rep.Rows[i], rep.Rows[j]
		if ra, rb := tidyRank(a.Verdict), tidyRank(b.Verdict); ra != rb {
			return ra < rb
		}
		if a.Kind != b.Kind {
			return a.Kind == "worktree"
		}
		return a.name() < b.name()
	})
	for _, r := range rep.Rows {
		rep.Counts[r.Verdict]++
	}
	return rep, nil
}

func tidyRank(v string) int {
	switch v {
	case tidyRemovable:
		return 0
	case tidyKeep:
		return 1
	case tidyStale:
		return 2
	}
	return 3
}

func tidyReal(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// tidyDirty says what is uncommitted in the worktree at path; empty when
// nothing is. Ignored files don't count (git status leaves them out).
func tidyDirty(ctx context.Context, path string) string {
	if _, err := os.Stat(path); err != nil {
		return "the directory can't be read"
	}
	out, err := tidyGit(ctx, path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "git status failed"
	}
	if out != "" {
		var untracked, changed int
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "??") {
				untracked++
			} else {
				changed++
			}
		}
		var bits []string
		if changed > 0 {
			bits = append(bits, fmt.Sprintf("%d modified file%s", changed, plural(changed)))
		}
		if untracked > 0 {
			bits = append(bits, fmt.Sprintf("%d untracked file%s", untracked, plural(untracked)))
		}
		return strings.Join(bits, ", ")
	}
	for _, p := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG"} {
		gp, err := tidyGit(ctx, path, "rev-parse", "--git-path", p)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(gp) {
			gp = filepath.Join(path, gp)
		}
		if _, err := os.Stat(gp); err == nil {
			return "a " + strings.ToLower(strings.SplitN(p, "_", 2)[0]) + " is in progress"
		}
	}
	return ""
}

// tidyMergedPRs asks Waypoint for the repo's merged pull requests (the last
// year's, up to the server's cap).
func (c *cli) tidyMergedPRs(repo string) ([]tidyPR, error) {
	return c.tidyPulls(repo, "merged")
}

// tidyPulls asks Waypoint for the repo's pull requests in a state (the last
// year's, up to the server's cap).
func (c *cli) tidyPulls(repo, state string) ([]tidyPR, error) {
	if repo == "" {
		return nil, fmt.Errorf("the repo has no remote Waypoint can match")
	}
	if c.server() == "" || c.token() == "" {
		return nil, fmt.Errorf("not logged in to Waypoint")
	}
	q := url.Values{"state": {state}, "repo": {repo}, "days": {"366"}}
	raw, err := c.call("GET", "/api/v1/pulls?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Pulls []tidyPR `json:"pulls"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return list.Pulls, nil
}

// tidyApply removes the removable rows. It never touches anything else, and
// re-reads a branch's tip right before deleting it.
func tidyApply(ctx context.Context, dir string, rep *tidyReport, o tidyOpts) {
	g := func(args ...string) (string, error) { return tidyGit(ctx, dir, args...) }
	deleteBranch := func(r *tidyRow) string {
		if tip, err := g("rev-parse", "--verify", "-q", "refs/heads/"+r.Branch); err != nil || tip != r.Tip {
			return "kept: the branch moved since the scan"
		}
		if _, err := g("branch", "-D", "--", r.Branch); err != nil {
			return "failed: " + err.Error()
		}
		return "deleted"
	}
	for _, r := range rep.Rows {
		if r.Verdict != tidyRemovable {
			continue
		}
		switch {
		case r.Kind == "worktree" && r.prunable:
			if o.noPrune {
				continue
			}
			if r.staleLock != "" { // git worktree prune skips a locked one
				if _, err := unlockJudged(ctx, dir, r.Path, r.staleLock); err != nil {
					r.Action = "kept: " + err.Error()
					continue
				}
			}
			r.Action = "pruned"
		case r.Kind == "worktree":
			// The reclaim lock is held from unlocking through a relock.
			release := func() {}
			if r.staleLock != "" {
				rel, common, err := holdReclaim(ctx, dir)
				if err == nil {
					_, err = unlockHeld(ctx, dir, common, r.Path, &r.staleLock)
				}
				if err != nil {
					if rel != nil {
						rel()
					}
					r.Action = "kept: " + err.Error()
					continue
				}
				release = rel
			}
			if _, err := g("worktree", "remove", r.Path); err != nil {
				if r.staleLock != "" {
					relock(ctx, dir, r.Path, r.staleLock)
				}
				release()
				r.Action = "failed: " + err.Error()
				continue
			}
			release()
			r.Action = "removed"
			if r.Branch != "" && r.MergedBy != "" && (o.branches || o.worktreeBranches) {
				r.Action += ", branch " + deleteBranch(r)
			}
		default:
			r.Action = deleteBranch(r)
		}
	}
	if !o.noPrune {
		_, _ = g("worktree", "prune")
	}
}

func cmdTidy(c *cli, args []string) error {
	fs := c.flags()
	apply := fs.Bool("apply", false, "remove what is removable")
	explain := fs.String("explain", "", "list every rule for this `BRANCH` or path")
	wtOnly := fs.Bool("worktrees-only", false, "only worktrees")
	brOnly := fs.Bool("branches-only", false, "only branches")
	remote := fs.Bool("remote", false, "list the forge branches the daily sweep deletes or keeps")
	verbose := fs.Bool("verbose", false, "list every row (default: the first 12)")
	short(fs, "v", "verbose")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("wp tidy takes no arguments (use --explain BRANCH)")
	}
	if *remote {
		if *apply || *explain != "" || *wtOnly || *brOnly {
			return c.usage("--remote only lists: Waypoint's sweep deletes the branches (no --apply, --explain, --worktrees-only or --branches-only)")
		}
		return c.tidyRemote()
	}
	if *wtOnly && *brOnly {
		return c.usage("--worktrees-only and --branches-only exclude each other")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	ctx := context.Background()
	o := tidyOpts{worktrees: !*brOnly, branches: !*wtOnly, sessionEnded: c.tidySessionEnded()}
	if *apply {
		if _, err := tidyGit(ctx, dir, "fetch", "--prune", "--quiet"); err != nil {
			c.println("note: fetch --prune failed (" + err.Error() + "); judging by the refs as they are")
		}
	}
	repo, _, _ := gitInfo(dir)
	merged, perr := c.tidyMergedPRs(repo)
	rep, err := tidyScan(ctx, dir, merged, o)
	if err != nil {
		return err
	}
	rep.Repo = repo
	if perr != nil {
		rep.Notes = append(rep.Notes, "Waypoint's merged pull requests unavailable ("+perr.Error()+"): squash and rebase merges aren't recognised")
	}
	if *apply {
		tidyApply(ctx, dir, rep, o)
	}
	if n := worktreesBeside(ctx, dir, rep); n > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d kept worktree(s) sit beside the main checkout: `wp worktree move` puts them under .claude/worktrees/ with their work (WP-563)", n))
	}
	if *explain != "" {
		var hit []*tidyRow
		for _, r := range rep.Rows {
			if r.Branch == *explain || r.Path == *explain || filepath.Base(r.Path) == *explain && r.Path != "" {
				hit = append(hit, r)
			}
		}
		if len(hit) == 0 {
			return fmt.Errorf("no branch or worktree %q here", *explain)
		}
		rep.Rows = hit
		if c.asJSON {
			return c.emitValue(rep)
		}
		for _, r := range hit {
			c.println(fmt.Sprintf("%s %s: %s, %s", r.Kind, r.name(), strings.ToUpper(r.Verdict), r.Reason))
			for _, ch := range r.Checks {
				c.println("  " + ch)
			}
		}
		return nil
	}
	if c.asJSON {
		return c.emitValue(rep)
	}
	c.println(tidyText(rep, *apply, *verbose || *apply))
	return nil
}

const tidyRowCap = 12

// tidyText is the table, the counts and the command that applies it.
func tidyText(rep *tidyReport, applied, verbose bool) string {
	var b strings.Builder
	w := 0
	for _, r := range rep.Rows {
		w = max(w, len(r.name()))
	}
	w = min(w, 60)
	rows := rep.Rows
	if !verbose && len(rows) > tidyRowCap {
		rows = rows[:tidyRowCap]
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "%-9s %-8s %-*s  %s", strings.ToUpper(r.Verdict), r.Kind, w, r.name(), r.Reason)
		if r.Action != "" {
			fmt.Fprintf(&b, "  -> %s", r.Action)
		}
		b.WriteString("\n")
	}
	if len(rep.Rows) == 0 {
		b.WriteString("Nothing here.\n")
	} else if len(rows) < len(rep.Rows) {
		fmt.Fprintf(&b, "+%d more (-v)\n", len(rep.Rows)-len(rows))
	}
	var parts []string
	for _, v := range []string{tidyRemovable, tidyKeep, tidyStale, tidyProtected} {
		if n := rep.Counts[v]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, v))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, ", ") + " (default branch: " + rep.Default + ")\n")
	}
	for _, n := range rep.Notes {
		b.WriteString("note: " + n + "\n")
	}
	if !applied && rep.Counts[tidyRemovable] > 0 {
		b.WriteString("Remove the removable ones: wp tidy --apply\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// tidyRemoteRepo is a repo's branches on the forge, as Waypoint's sweep
// judges them (WP-140).
type tidyRemoteRepo struct {
	Repo    string `json:"repo"`
	Default string `json:"default_branch"`
	Enabled bool   `json:"enabled"`
	Rows    []struct {
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	} `json:"rows"`
	Counts map[string]int `json:"counts"`
	Notes  []string       `json:"notes"`
}

// tidyRemote lists what the forge sweep would delete or reports as stale.
func (c *cli) tidyRemote() error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, _, _ := gitInfo(dir)
	if repo == "" {
		return fmt.Errorf("the repo has no remote Waypoint can match")
	}
	if c.server() == "" || c.token() == "" {
		return fmt.Errorf("not logged in to Waypoint")
	}
	raw, err := c.call("GET", "/api/v1/branches/tidy?"+url.Values{"repo": {repo}}.Encode(), nil)
	if err != nil {
		return err
	}
	var repos []tidyRemoteRepo
	if err := json.Unmarshal(raw, &repos); err != nil {
		return err
	}
	if len(repos) == 0 {
		return fmt.Errorf("no repo %s is attached in Waypoint", repo)
	}
	rep := &tidyReport{Repo: repo, Counts: map[string]int{}}
	for _, r := range repos {
		rep.Default = r.Default
		for _, b := range r.Rows {
			rep.Rows = append(rep.Rows, &tidyRow{Kind: "branch", Branch: b.Branch, Tip: b.SHA, Verdict: b.Verdict, Reason: b.Reason})
			rep.Counts[b.Verdict]++
		}
		rep.Notes = append(rep.Notes, r.Notes...)
	}
	if c.asJSON {
		return c.emitValue(rep)
	}
	text := tidyText(rep, true, true)
	c.println(strings.Replace(text, "Remove the removable ones: wp tidy --apply\n", "", 1))
	if rep.Counts[tidyRemovable] > 0 {
		c.println("Waypoint deletes the removable ones in its daily sweep; nothing here is deleted by this command.")
	}
	return nil
}

// branchRef is a branch's full ref, "" for no branch: a name that starts
// with a dash is never given to git bare.
func branchRef(branch string) string {
	if branch == "" {
		return ""
	}
	return "refs/heads/" + branch
}
