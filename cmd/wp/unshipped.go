package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The rules judge content, not ancestry, so a squash or rebase merge is not reported as left behind.

// The kinds of unshipped work.
const (
	unshippedUncommitted = "uncommitted"
	unshippedUnpushed    = "unpushed"
	unshippedStash       = "stash"
	unshippedNoPR        = "no-pr"
)

const (
	unshippedRepoLimit  = 5 * time.Minute
	unshippedFetchLimit = 30 * time.Second
	// unshippedWalkDepth is how far below a folder wp unshipped looks for
	// repositories.
	unshippedWalkDepth = 4
	// unshippedLandingTries bounds the default branch's past states tried for
	// one branch. Running out leaves the work reported.
	unshippedLandingTries = 150
	// unshippedNoticeRoom is how much of autopilot's notice, by the characters
	// of its lines, one job may fill: what is stored is cut at 1000, and an
	// item cut off there would count as told. The rest wait for the next sweep.
	unshippedNoticeRoom = 750
	unshippedNoticeLine = 300
)

var unshippedCmd = &command{
	name: "unshipped", group: "Code and CI", args: "[DIR...] [--landed]",
	summary: "list work no remote or default branch has: uncommitted, unpushed, stashed, no pull request",
	about: `Lists what would be lost or overlooked: uncommitted changes in any
worktree, commits on no remote, stashes, and pushed branches with no pull
request. With DIRs it covers every repository up to 4 levels below them;
without, the repository you are in.

Left out, so what remains is worth reading: content the default branch has or
had (squash and rebase merges included), branches with an open pull request,
and branches whose pull request was closed at that commit. --landed lists the
content left out and where it landed. Work with a process in its folder is
listed and marked in use; how recently it changed plays no part.

It fetches each repository and changes nothing else; it is safe to run any
time and uses no model. Autopilot's sweep runs the same scan: it pushes a
task branch's commits once its checkout has sat unused for 4 hours, and tells
of work only the machine has (uncommitted, unpushed, stashed) again every 3
days.`,
	examples: []string{"wp unshipped", "wp unshipped /projects", "wp unshipped --landed", "wp unshipped --json"},
	run:      cmdUnshipped,
}

// unshippedItem is one piece of work left behind.
type unshippedItem struct {
	Kind   string `json:"kind"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
	Task   string `json:"task,omitempty"`
	Stash  string `json:"stash,omitempty"`
	Commit string `json:"commit,omitempty"` // a stash's: its position in the list shifts
	What   string `json:"what"`
	InUse  bool   `json:"in_use,omitempty"` // a process is in its folder
}

// where names the item's place: a folder, a stash or a branch.
func (it unshippedItem) where() string {
	switch {
	case it.Stash != "":
		return it.Stash
	case it.Path != "" && it.Branch != "":
		return it.Path + " (" + it.Branch + ")"
	case it.Path != "":
		return it.Path
	}
	return it.Branch
}

// onceKey identifies the item in the reports already made.
func (it unshippedItem) onceKey() string {
	return "unshipped:" + it.Kind + ":" + cmpStr(it.Commit, cmpStr(it.Branch, it.Path))
}

// unshippedReport is what one repository holds.
type unshippedReport struct {
	Root   string          `json:"root"`
	Repo   string          `json:"repo,omitempty"`
	Items  []unshippedItem `json:"items"`
	Landed []unshippedItem `json:"landed"` // left out: the default branch has or had the content
	Notes  []string        `json:"notes,omitempty"`
}

// unshippedPRs are the pull requests Waypoint knows for a repository. known
// is false when it has none: nothing can then be said of a branch having no
// pull request, and why says what went wrong if something did.
type unshippedPRs struct {
	merged, open, closed []tidyPR
	known                bool
	why                  string
}

// unshippedOpts tune a scan. cwds are the folders processes are running in.
type unshippedOpts struct {
	now  time.Time
	cwds []string
}

// unshippedScan is one repository's scan in progress.
type unshippedScan struct {
	ctx  context.Context
	dir  string
	def  string
	base string
	// Default-branch commits keyed by identity (author, time, subject), loaded on first need.
	baseCommits map[string]string
	worktreeOf  map[string]string
	// merge-tree writes go to a scratch object dir, so loose objects don't pile up in the repository
	mergeEnv []string
	noRemote bool
	prs      unshippedPRs
	o        unshippedOpts
	rep      *unshippedReport
}

// scanUnshipped finds the unshipped work of the repository containing dir.
func scanUnshipped(ctx context.Context, dir string, prs unshippedPRs, o unshippedOpts) (*unshippedReport, error) {
	if o.now.IsZero() {
		o.now = time.Now()
	}
	tidy, err := tidyScan(ctx, dir, prs.merged, tidyOpts{worktrees: true, branches: true, now: o.now, intoDefault: true})
	if err != nil {
		return nil, err
	}
	rep := &unshippedReport{Items: []unshippedItem{}, Landed: []unshippedItem{}, Notes: tidy.Notes}
	if out, err := tidyGit(ctx, dir, "worktree", "list", "--porcelain"); err == nil {
		if wts := parseWorktrees(out); len(wts) > 0 {
			rep.Root = wts[0].path
		}
	}
	s := &unshippedScan{ctx: ctx, dir: dir, def: tidy.Default, prs: prs, o: o, rep: rep, worktreeOf: map[string]string{}}
	if refs, err := tidyGit(ctx, dir, "for-each-ref", "--count=1", "refs/remotes"); err == nil && refs == "" {
		s.noRemote = true
		rep.Notes = append(rep.Notes, "no remote branches: nothing tells pushed from not, so its commits weren't judged")
	} else if !prs.known {
		rep.Notes = append(rep.Notes, "branches without a pull request aren't reported: "+cmpStr(prs.why, "Waypoint has no pull requests for this repository"))
	}
	s.base = "refs/heads/" + tidy.Default
	if _, err := s.git("rev-parse", "-q", "--verify", "refs/remotes/origin/"+tidy.Default+"^{commit}"); err == nil {
		s.base = "refs/remotes/origin/" + tidy.Default
	}
	if _, err = s.git("rev-parse", s.base+"^{tree}"); err != nil {
		return nil, err
	}
	objects, err := s.git("rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp("", "wp-unshipped-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	s.mergeEnv = []string{"GIT_OBJECT_DIRECTORY=" + scratch, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects}
	for _, r := range tidy.Rows {
		if r.Kind == "worktree" && r.Branch != "" {
			s.worktreeOf[r.Branch] = r.Path
		}
	}
	for _, r := range tidy.Rows {
		s.judgeRow(r)
	}
	s.judgeStashes()
	// Every git call failing past the deadline would read as "not landed".
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ran out of time judging %s: %w", dir, err)
	}
	return rep, nil
}

func (s *unshippedScan) git(args ...string) (string, error) {
	return tidyGit(s.ctx, s.dir, args...)
}

func (s *unshippedScan) add(it unshippedItem) {
	if m := branchKeyRE.FindStringSubmatch(it.Branch); m != nil {
		it.Task = strings.ToUpper(m[1])
	}
	s.rep.Items = append(s.rep.Items, it)
}

// judgeRow reports what a tidy row holds: its uncommitted files, then its
// commits. Verdicts aren't used, because a row can be kept or protected for
// a reason that says nothing of the work in it.
func (s *unshippedScan) judgeRow(r *tidyRow) {
	if r.prunable {
		return
	}
	if r.Kind == "worktree" {
		s.judgeUncommitted(r)
	}
	if r.MergedBy != "" || s.noRemote {
		return
	}
	switch {
	case r.unpushed > 0:
		s.judgeCommits(r, unshippedUnpushed, plural1(r.unpushed, "commit")+" on no remote")
	case s.lacksPR(r):
		what := "pushed, no pull request"
		if pr := s.mergedElsewhere(r); pr != nil {
			what = fmt.Sprintf("pushed, PR #%d merged only into %s", pr.Number, pr.Base)
		}
		s.judgeCommits(r, unshippedNoPR, what)
	}
}

// judgeUncommitted reports a worktree's changes. New files under .claude/
// are the agent's own, as they are for wp done.
func (s *unshippedScan) judgeUncommitted(r *tidyRow) {
	if r.dirty == "" {
		return
	}
	what := r.dirty
	if st, err := tidyGit(s.ctx, r.Path, "status", "--porcelain", "--untracked-files=all"); err == nil {
		files := 0
		for _, l := range strings.Split(st, "\n") {
			if l != "" && !strings.HasPrefix(l, "?? .claude/") {
				files++
			}
		}
		switch {
		case files > 0:
			what = plural1(files, "uncommitted file")
		case !strings.HasSuffix(r.dirty, "in progress"):
			return // Untracked .claude/ files are the agent's own and do not count as unshipped work.
		}
	}
	s.add(unshippedItem{Kind: unshippedUncommitted, Path: r.Path, Branch: r.Branch, What: what, InUse: s.inUse(r.Path)})
}

// judgeCommits reports a row's commits unless the default branch has or had
// their content.
func (s *unshippedScan) judgeCommits(r *tidyRow, kind, what string) {
	from := ""
	for _, pr := range s.prs.merged {
		if pr.Base == s.def && pr.HeadRef == r.Branch && pr.HeadSHA != "" && pr.HeadSHA != r.Tip && s.isAncestor(pr.HeadSHA, r.Tip) {
			from = pr.HeadSHA
			what += fmt.Sprintf(" (after merged PR #%d)", pr.Number)
			break
		}
	}
	if how, ok := s.landed(r.Tip, from); ok {
		s.rep.Landed = append(s.rep.Landed, unshippedItem{Kind: kind, Path: r.Path, Branch: r.Branch, What: how})
		return
	}
	s.add(unshippedItem{Kind: kind, Path: r.Path, Branch: r.Branch, What: what, InUse: s.inUse(r.Path)})
}

// lacksPR: the branch is unmerged and no pull request accounts for it.
func (s *unshippedScan) lacksPR(r *tidyRow) bool {
	if !s.prs.known || r.Branch == "" || r.Branch == s.def || strings.HasPrefix(r.Branch, "release/") {
		return false
	}
	carried := func(ref string) bool {
		return slices.ContainsFunc(s.prs.open, func(pr tidyPR) bool { return pr.HeadRef == ref })
	}
	if carried(r.Branch) {
		return false
	}
	// Merged into a branch that has its own open pull request: that one
	// carries it on.
	if pr := s.mergedElsewhere(r); pr != nil && carried(pr.Base) {
		return false
	}
	// Closed at this commit or past it: someone decided. Commits made since
	// aren't covered by that.
	return !slices.ContainsFunc(s.prs.closed, func(pr tidyPR) bool {
		return pr.HeadRef == r.Branch && pr.HeadSHA != "" && (pr.HeadSHA == r.Tip || s.isAncestor(r.Tip, pr.HeadSHA))
	})
}

// mergedElsewhere is the pull request that merged this tip into a branch other
// than the default one, nil if none did. That says nothing of the default
// branch having it.
func (s *unshippedScan) mergedElsewhere(r *tidyRow) *tidyPR {
	for n := range s.prs.merged {
		pr := &s.prs.merged[n]
		if pr.Base != "" && pr.Base != s.def && pr.HeadRef == r.Branch && pr.HeadSHA != "" && (pr.HeadSHA == r.Tip || s.isAncestor(r.Tip, pr.HeadSHA)) {
			return pr
		}
	}
	return nil
}

func (s *unshippedScan) isAncestor(a, b string) bool {
	_, err := s.git("merge-base", "--is-ancestor", a, b)
	return err == nil
}

// landed: the default branch has what tip adds (since from, when set), now or
// at some point in its past, and says how it knows. git failing to say counts
// as not landed, so the work stays reported.
func (s *unshippedScan) landed(tip, from string) (string, bool) {
	if s.changesNothing(s.base, from, tip) {
		return "the default branch has this content", true
	}
	if s.everyPatchThere(tip, from) {
		return "every commit's patch is on the default branch", true
	}
	if at := s.landingPoint(tip, from); at != "" {
		return s.describe(at), true
	}
	return "", false
}

// everyPatchThere: git cherry finds each commit's patch on the default branch.
// It doesn't look at merges, so one holding work of its own rules this out.
func (s *unshippedScan) everyPatchThere(tip, from string) bool {
	args := []string{"cherry", s.base, tip}
	if from != "" {
		args = append(args, from)
	}
	out, err := s.git(args...)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "+") {
			return false
		}
	}
	return s.mergesAddNothing(tip, from)
}

// mergesAddNothing: every merge on the branch is what git makes of its parents
// unaided, so it holds no conflict resolution or hand edit.
func (s *unshippedScan) mergesAddNothing(tip, from string) bool {
	args := []string{"rev-list", "--merges", tip, "^" + s.base}
	if from != "" {
		args = append(args, "^"+from)
	}
	out, err := s.git(args...)
	if err != nil {
		return false
	}
	if out == "" {
		return true
	}
	for _, sha := range strings.Split(out, "\n") {
		diff, err := tidyGitEnv(s.ctx, s.dir, s.mergeEnv, "show", "--remerge-diff", "--format=", sha)
		if err != nil || diff != "" {
			return false
		}
	}
	return true
}

// landingPoint is the commit of the default branch that held everything tip
// changes, "" when none found: a three-way merge against that commit, from
// where the branch left, changes nothing. Looking at the commit where the work
// landed means later edits to the same lines don't hide it, and new lines
// amended into a landed commit aren't there to be found.
func (s *unshippedScan) landingPoint(tip, from string) string {
	fork, err := s.git("merge-base", s.base, tip)
	if err != nil {
		return ""
	}
	if from == "" {
		from = fork
	}
	for _, at := range s.landingCandidates(tip, fork, from) {
		if s.changesNothing(at, from, tip) {
			return at
		}
	}
	return ""
}

// landingCandidates are the default branch's past states worth trying. A commit
// with the identity of one of tip's (rebased, so its hash differs) comes first:
// the identity only says where to look, the merge is the proof. Then the
// merges since the fork that touched these files, where stacks and pull
// requests come in, then the default branch's own commits, each oldest first
// since work usually lands soon after it starts.
func (s *unshippedScan) landingCandidates(tip, fork, from string) []string {
	files, err := s.git("diff", "--name-only", "-z", "--no-renames", from, tip)
	if err != nil || files == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(sha string) {
		if sha != "" && !seen[sha] {
			seen[sha] = true
			out = append(out, sha)
		}
	}
	if s.loadBaseCommits() {
		if ids, err := s.git("log", "--no-merges", commitIdentity, from+".."+tip); err == nil {
			for _, id := range strings.Split(ids, "\n") {
				add(s.baseCommits[id])
			}
		}
	}
	paths := strings.Split(strings.Trim(files, "\x00"), "\x00")
	for _, walk := range []string{"--merges", "--first-parent"} {
		args := append([]string{"--literal-pathspecs", "log", walk, "--reverse", "--format=%H", fork + ".." + s.base, "--"}, paths...)
		hist, err := s.git(args...)
		if err != nil {
			continue
		}
		for _, sha := range strings.Split(hist, "\n") {
			add(sha)
		}
	}
	return out[:min(len(out), unshippedLandingTries)]
}

// commitIdentity is what a rebase keeps of a commit, even one whose patch
// changed because a conflict was resolved.
const commitIdentity = "--format=%ae%x00%at%x00%s"

// loadBaseCommits reads the default branch's commits by identity, once.
func (s *unshippedScan) loadBaseCommits() bool {
	if s.baseCommits != nil {
		return true
	}
	out, err := s.git("log", "--no-merges", "--format=%H%x00%ae%x00%at%x00%s", s.base)
	if err != nil {
		return false
	}
	s.baseCommits = map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if hash, id, ok := strings.Cut(l, "\x00"); ok {
			s.baseCommits[id] = hash
		}
	}
	return true
}

// describe names a commit a piece of work landed in.
func (s *unshippedScan) describe(at string) string {
	line, err := s.git("log", "-1", "--format=%h %s", at)
	if err != nil {
		line = at
	}
	return "landed in " + line
}

// changesNothing: merging tip into commit at, taking from as the common
// ancestor when set, gives at's own tree. Nothing it writes stays in the
// repository (mergeEnv).
func (s *unshippedScan) changesNothing(at, from, tip string) bool {
	tree, err := s.git("rev-parse", at+"^{tree}")
	if err != nil {
		return false
	}
	args := []string{"merge-tree", "--write-tree"}
	if from != "" {
		args = append(args, "--merge-base="+from)
	}
	out, err := tidyGitEnv(s.ctx, s.dir, s.mergeEnv, append(args, at, tip)...)
	if err != nil {
		return false // a conflict, or a git too old to say
	}
	first, _, _ := strings.Cut(out, "\n")
	return first == tree
}

// inUse: a process is in the folder. Whether someone is at work there is what
// matters, not how long ago a file changed.
func (s *unshippedScan) inUse(path string) bool {
	if path == "" {
		return false
	}
	real := tidyReal(path)
	return slices.ContainsFunc(s.o.cwds, func(d string) bool { return inside(tidyReal(d), real) })
}

// stashBranch is the branch a stash's subject names ("WIP on x: ...", "On x:
// ..."); "" when it was made on a detached head.
func stashBranch(subject string) string {
	rest, ok := strings.CutPrefix(subject, "WIP on ")
	if !ok {
		if rest, ok = strings.CutPrefix(subject, "On "); !ok {
			return ""
		}
	}
	b, _, ok := strings.Cut(rest, ":")
	if !ok || b == "(no branch)" {
		return ""
	}
	return b
}

// judgeStashes reports the stashes whose changes the default branch lacks.
func (s *unshippedScan) judgeStashes() {
	out, err := s.git("stash", "list", "--format=%H%x09%ct%x09%gs")
	if err != nil {
		s.rep.Notes = append(s.rep.Notes, "stashes unreadable: "+err.Error())
		return
	}
	if out == "" {
		return
	}
	for n, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 3 {
			continue
		}
		sha, subject := f[0], f[2]
		ref := fmt.Sprintf("stash@{%d}", n)
		branch := stashBranch(subject)
		if at, ok := s.stashLanded(sha); ok {
			s.rep.Landed = append(s.rep.Landed, unshippedItem{Kind: unshippedStash, Stash: ref, Commit: sha, Branch: branch, What: at})
			continue
		}
		ts, _ := strconv.ParseInt(f[1], 10, 64)
		what := subject + " (" + unshippedAge(s.o.now.Sub(time.Unix(ts, 0))) + " old"
		if names, err := s.git("diff", "--name-only", sha+"^1", sha); err == nil && names != "" {
			what += ", " + plural1(strings.Count(names, "\n")+1, "changed file")
		}
		s.add(unshippedItem{Kind: unshippedStash, Stash: ref, Commit: sha, Branch: branch, What: what + ")", InUse: s.inUse(s.worktreeOf[branch])})
	}
}

// stashLanded: the stash's own change, against the commit it was made on,
// changes nothing on the default branch now or at some point in its past, and
// any untracked files it saved were there with the same contents.
func (s *unshippedScan) stashLanded(sha string) (string, bool) {
	at, how, ok := s.landedFrom(sha, sha+"^1")
	if !ok {
		return "", false
	}
	// What was staged is the stash's too, when the worktree doesn't hold it.
	if !s.changesNothing(sha, sha+"^1", sha+"^2") {
		if _, _, ok := s.landedFrom(sha+"^2", sha+"^1"); !ok {
			return "", false
		}
	}
	// A stash commit's parents are the base, the index, and, only when it
	// saved untracked files, a third. Read them so a failure to read isn't
	// taken for "no untracked files".
	parents, err := s.git("rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return "", false
	}
	if len(strings.Fields(parents)) < 4 {
		return how, true
	}
	listing, err := s.git("ls-tree", "-r", sha+"^3")
	if err != nil {
		return "", false
	}
	all, err := s.git("ls-tree", "-r", at)
	if err != nil {
		return "", false
	}
	had := map[string]string{}
	for _, l := range strings.Split(all, "\n") {
		if meta, path, ok := strings.Cut(l, "\t"); ok {
			had[path] = meta
		}
	}
	for _, l := range strings.Split(listing, "\n") {
		if meta, path, ok := strings.Cut(l, "\t"); ok && had[path] != meta {
			return "", false
		}
	}
	return how, true
}

// landedFrom: the default branch has what tip adds to from, now or at the
// commit returned.
func (s *unshippedScan) landedFrom(tip, from string) (at, how string, ok bool) {
	if s.changesNothing(s.base, from, tip) {
		return s.base, "the default branch has this content", true
	}
	if at = s.landingPoint(tip, from); at == "" {
		return "", "", false
	}
	return at, s.describe(at), true
}

// unshippedAge is "3 days" or "5 hours".
func unshippedAge(d time.Duration) string {
	if d >= 48*time.Hour {
		return plural1(int(d.Hours()/24), "day")
	}
	return plural1(max(int(d.Hours()), 1), "hour")
}

// findRepos lists the repositories at or below dirs, one per repository
// however many worktrees it has, including those inside another repository
// and those a symlink points to.
func findRepos(ctx context.Context, dirs []string) []string {
	var out []string
	seen := map[string]bool{}
	addRepo := func(path string) {
		if common, err := tidyGit(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil && !seen[common] {
			seen[common] = true
			out = append(out, path)
		}
	}
	for _, root := range dirs {
		root = filepath.Clean(root)
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		depth := strings.Count(root, string(filepath.Separator))
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 && !strings.HasPrefix(d.Name(), ".") && dirExists(filepath.Join(path, ".git")) {
				addRepo(path)
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			switch d.Name() {
			case ".git", "node_modules", ".cache", ".venv", "venv", "__pycache__", "vendor", "dist", "target":
				return filepath.SkipDir
			}
			if strings.Count(path, string(filepath.Separator))-depth > unshippedWalkDepth {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				addRepo(path)
			}
			return nil
		})
	}
	return out
}

// unshippedPRs asks Waypoint what it knows of the repo's pull requests.
func (c *cli) unshippedPRs(repo string) (unshippedPRs, error) {
	var p unshippedPRs
	var err error
	if p.merged, err = c.tidyPulls(repo, "merged"); err != nil {
		return p, err
	}
	if p.open, err = c.tidyPulls(repo, "open"); err != nil {
		return p, err
	}
	if p.closed, err = c.tidyPulls(repo, "closed"); err != nil {
		return p, err
	}
	p.known = len(p.merged)+len(p.open)+len(p.closed) > 0
	return p, nil
}

// unshippedRepo fetches and scans one repository.
func (c *cli) unshippedRepo(ctx context.Context, dir string, cwds []string) (*unshippedReport, error) {
	ctx, cancel := context.WithTimeout(ctx, unshippedRepoLimit)
	defer cancel()
	var notes []string
	fetch, stop := context.WithTimeout(ctx, unshippedFetchLimit)
	defer stop()
	if _, err := tidyGit(fetch, dir, "fetch", "--prune", "--quiet"); err != nil {
		notes = append(notes, "fetch failed, judged by the refs as they are: "+err.Error())
	}
	repo, _, _ := gitInfo(dir)
	prs, err := c.unshippedPRs(repo)
	if err != nil {
		prs.known, prs.why = false, "Waypoint's pull requests are unavailable: "+err.Error()
	}
	rep, err := scanUnshipped(ctx, dir, prs, unshippedOpts{cwds: cwds})
	if err != nil {
		return nil, err
	}
	rep.Repo = repo
	rep.Notes = append(notes, rep.Notes...)
	return rep, nil
}

func cmdUnshipped(c *cli, args []string) error {
	fset := c.flags()
	landed := fset.Bool("landed", false, "also list the content left out as landed, and where it landed")
	pos, err := c.parse(fset, args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	var repos []string
	if len(pos) > 0 {
		for _, d := range pos {
			if !dirExists(d) {
				return c.usage("wp unshipped: no folder %s", d)
			}
		}
		repos = findRepos(ctx, pos)
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		repos = []string{wd}
	}
	cwds, cwdErr := procCwds()
	reps := []*unshippedReport{}
	for _, dir := range repos {
		rep, err := c.unshippedRepo(ctx, dir, cwds)
		if err != nil {
			if len(pos) == 0 {
				return err
			}
			reps = append(reps, &unshippedReport{Root: dir, Items: []unshippedItem{}, Notes: []string{err.Error()}})
			continue
		}
		if cwdErr != nil {
			rep.Notes = append(rep.Notes, "can't see which folders processes are in, so nothing is marked in use")
		}
		reps = append(reps, rep)
	}
	if c.asJSON {
		return c.emitValue(reps)
	}
	c.println(unshippedText(reps, *landed))
	return nil
}

// unshippedText lists each repository's items and what was left out.
func unshippedText(reps []*unshippedReport, showLanded bool) string {
	var b strings.Builder
	items, inUse, landed, repos := 0, 0, 0, 0
	line := func(it unshippedItem) {
		task, use := "", ""
		if it.Task != "" {
			task = " [" + it.Task + "]"
		}
		if it.InUse {
			use = " (in use)"
		}
		fmt.Fprintf(&b, "  %-11s %s%s: %s%s\n", it.Kind, it.where(), task, it.What, use)
	}
	for _, r := range reps {
		landed += len(r.Landed)
		if len(r.Items) == 0 && len(r.Notes) == 0 && (!showLanded || len(r.Landed) == 0) {
			continue
		}
		if len(r.Items) > 0 {
			repos++
		}
		b.WriteString(r.Root + "\n")
		for _, it := range r.Items {
			items++
			if it.InUse {
				inUse++
			}
			line(it)
		}
		for _, n := range r.Notes {
			b.WriteString("  note: " + n + "\n")
		}
		if showLanded {
			if len(r.Landed) > 0 {
				b.WriteString("  already landed:\n")
			}
			for _, it := range r.Landed {
				line(it)
			}
		}
	}
	switch {
	case len(reps) == 0:
		b.WriteString("No repositories found.")
	case items == 0 && slices.ContainsFunc(reps, func(r *unshippedReport) bool { return len(r.Notes) > 0 }):
		b.WriteString("Nothing unshipped found, within the limits noted above.")
	case items == 0:
		b.WriteString("Nothing unshipped.")
	default:
		fmt.Fprintf(&b, "%s not shipped in %s", plural1(items, "item"), plural1(repos, "repository"))
		if inUse > 0 {
			fmt.Fprintf(&b, ", %d in use", inUse)
		}
		b.WriteString(".")
	}
	if landed > 0 && !showLanded {
		fmt.Fprintf(&b, " Left out: %d already landed (--landed lists them).", landed)
	}
	return b.String()
}

// noteUnshipped is what autopilot's sweep tells a person about: unshipped
// work in the repository that no process or session is on and that the
// worktree report hasn't told. Each item is told once, so without a complete
// view it says nothing rather than something wrong; the next sweep tells it.
func (p *runnerProc) noteUnshipped(ctx context.Context, run *jobRun, in autopilotJob, rl *relaunch) []string {
	if in.HeldUnknown {
		return nil
	}
	say := func(format string, a ...any) { fmt.Fprintf(run.out, format+"\n", a...) }
	cwds, err := procCwds()
	if err != nil {
		say("unshipped work: can't see which folders processes are in (%v)", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, unshippedRepoLimit)
	defer cancel()
	repo, _, _ := gitInfo(rl.root)
	prs, err := p.c.unshippedPRs(repo)
	if err != nil {
		say("unshipped work: Waypoint's pull requests unavailable: %v", err)
		return nil
	}
	rep, err := scanUnshipped(ctx, rl.root, prs, unshippedOpts{cwds: cwds})
	if err != nil {
		say("unshipped work: %v", err)
		return nil
	}
	items := p.pushIdle(ctx, run, rl.root, sweepUnshipped(rep.Items, in.Held), prs, time.Now())
	return p.c.unshippedOnce(rl.root, items)
}

// sweepUnshipped drops the items a sweep shouldn't tell of: work in use and
// what a quiet session still holds.
func sweepUnshipped(items []unshippedItem, held []autopilotHeld) []unshippedItem {
	return slices.DeleteFunc(slices.Clone(items), func(it unshippedItem) bool {
		if it.InUse {
			return true
		}
		return slices.ContainsFunc(held, func(h autopilotHeld) bool {
			return (it.Path != "" && h.Cwd != "" && inside(tidyReal(h.Cwd), tidyReal(it.Path))) || (it.Task != "" && strings.EqualFold(h.Key, it.Task))
		})
	})
}

// unshippedRetell is how long work only this machine has goes untold:
// uncommitted files, commits on no remote and stashes are told again after
// it, so nothing sits on one disk for days unnoticed (WP-843). A pushed
// branch with no pull request is on the forge, and is told once.
const unshippedRetell = 3 * 24 * time.Hour

// retold: the kind of work told again once unshippedRetell has passed.
func retold(kind string) bool {
	return kind == unshippedUncommitted || kind == unshippedUnpushed || kind == unshippedStash
}

// unshippedOnce keeps the items not told lately and remembers them, in the
// same record as the worktrees autopilot left, whose report (keptOnce) may
// already have told of a worktree. Work only this machine has is told again
// after unshippedRetell; the rest once. It keeps what fits one notice.
func (c *cli) unshippedOnce(root string, items []unshippedItem) []string {
	var out []string
	now := time.Now()
	_ = c.updateState(func(s *State) {
		out = nil
		if s.WorktreesKept == nil {
			s.WorktreesKept = map[string]time.Time{}
		}
		lately := func(it unshippedItem, k string) bool {
			at, told := s.WorktreesKept[k]
			return told && (!retold(it.Kind) || now.Sub(at) < unshippedRetell)
		}
		kept := func(it unshippedItem) bool {
			if it.Kind != unshippedUncommitted && it.Kind != unshippedUnpushed {
				return false
			}
			return lately(it, it.Path+"\x00"+unshippedUncommitted) || lately(it, it.Path+"\x00"+unshippedUnpushed)
		}
		room := unshippedNoticeRoom
		for _, it := range items {
			k := root + "\x00" + it.onceKey()
			if lately(it, k) || kept(it) || len(out) >= keptReportCap {
				continue
			}
			line := root + ": " + it.Kind + " " + it.where() + ": " + it.What
			if len(line) > unshippedNoticeLine {
				line = strings.ToValidUTF8(line[:unshippedNoticeLine], "")
			}
			cost := len(line) + len("- \n")
			if room -= cost; room < 0 && len(out) > 0 {
				break
			}
			s.WorktreesKept[k] = now
			out = append(out, line)
		}
	})
	return out
}
