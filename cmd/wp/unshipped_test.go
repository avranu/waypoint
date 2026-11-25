package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func scanLater(t *testing.T, dir string, prs unshippedPRs, cwds ...string) *unshippedReport {
	t.Helper()
	rep, err := scanUnshipped(context.Background(), dir, prs, unshippedOpts{now: later(), cwds: cwds})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (r *unshippedReport) find(kind, name string) *unshippedItem {
	for n := range r.Items {
		it := &r.Items[n]
		if it.Kind == kind && strings.Contains(it.where(), name) {
			return it
		}
	}
	return nil
}

func wantItem(t *testing.T, r *unshippedReport, kind, name string) *unshippedItem {
	t.Helper()
	it := r.find(kind, name)
	if it == nil {
		t.Fatalf("no %s item for %s in %+v", kind, name, r.Items)
	}
	return it
}

func wantNoItem(t *testing.T, r *unshippedReport, name string) {
	t.Helper()
	for _, it := range r.Items {
		if strings.Contains(it.where(), name) {
			t.Errorf("%s reported: %+v", name, it)
		}
	}
}

func TestUnshippedReportsWorkLeftBehind(t *testing.T) {
	f := newTidyFixture(t)
	known := unshippedPRs{known: true}

	tgit(t, f.repo, "checkout", "-q", "-b", "wp-12-unpushed", "main")
	tcommit(t, f.repo, "unpushed.txt")
	tgit(t, f.repo, "checkout", "-q", "-b", "wp-13-pushed", "main")
	tcommit(t, f.repo, "pushed.txt")
	tgit(t, f.repo, "push", "-q", "origin", "wp-13-pushed")
	tgit(t, f.repo, "checkout", "-q", "main")

	tgit(t, f.repo, "branch", "dirty", "main")
	dirty := f.worktree(t, "wt-dirty", "dirty")
	writeFile(t, filepath.Join(dirty, "new.txt"), "work")

	rep := scanLater(t, f.repo, known)
	if it := wantItem(t, rep, unshippedUnpushed, "wp-12-unpushed"); it.Task != "WP-12" || !strings.Contains(it.What, "1 commit") {
		t.Errorf("unpushed item: %+v", it)
	}
	if it := wantItem(t, rep, unshippedNoPR, "wp-13-pushed"); it.Task != "WP-13" {
		t.Errorf("no-pr item: %+v", it)
	}
	wantItem(t, rep, unshippedUncommitted, "wt-dirty")
	wantNoItem(t, rep, "main")
}

func TestUnshippedNoPRNeedsWaypointToKnowTheRepo(t *testing.T) {
	f := newTidyFixture(t)
	tgit(t, f.repo, "checkout", "-q", "-b", "pushed", "main")
	tcommit(t, f.repo, "pushed.txt")
	tgit(t, f.repo, "push", "-q", "origin", "pushed")
	tgit(t, f.repo, "checkout", "-q", "main")
	rep := scanLater(t, f.repo, unshippedPRs{})
	wantNoItem(t, rep, "pushed")
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "aren't reported") {
		t.Errorf("should say what it couldn't judge: %v", rep.Notes)
	}
	why := scanLater(t, f.repo, unshippedPRs{why: "the server is down"})
	if len(why.Notes) != 1 || !strings.Contains(why.Notes[0], "the server is down") {
		t.Errorf("should say why: %v", why.Notes)
	}
}

func TestUnshippedLeavesOutPullRequestsAccountedFor(t *testing.T) {
	f := newTidyFixture(t)
	push := func(b string) string {
		tgit(t, f.repo, "checkout", "-q", "-b", b, "main")
		sha := tcommit(t, f.repo, b+".txt")
		tgit(t, f.repo, "push", "-q", "origin", b)
		tgit(t, f.repo, "checkout", "-q", "main")
		return sha
	}
	push("in-flight")
	closedTip := push("closed-here")
	closedOld := push("closed-before")
	tgit(t, f.repo, "checkout", "-q", "closed-before")
	tcommit(t, f.repo, "more.txt")
	tgit(t, f.repo, "push", "-q", "origin", "closed-before")
	tgit(t, f.repo, "checkout", "-q", "main")

	rep := scanLater(t, f.repo, unshippedPRs{known: true,
		open:   []tidyPR{{Number: 1, HeadRef: "in-flight"}},
		closed: []tidyPR{{Number: 2, HeadRef: "closed-here", HeadSHA: closedTip}, {Number: 3, HeadRef: "closed-before", HeadSHA: closedOld}},
	})
	wantNoItem(t, rep, "in-flight")
	wantNoItem(t, rep, "closed-here")
	wantItem(t, rep, unshippedNoPR, "closed-before")
}

// A squash merge leaves no ancestry, and Waypoint may not know the pull
// request: the content still tells.
func TestUnshippedLeavesOutSquashedContent(t *testing.T) {
	f := newTidyFixture(t)
	f.squash(t, "sq")
	rep := scanLater(t, f.repo, unshippedPRs{known: true})
	wantNoItem(t, rep, "sq")
	if len(rep.Landed) == 0 {
		t.Error("a squashed branch should count as landed")
	}
}

// A squash landed, then the default branch edited the same lines: the merge
// against today's tip conflicts, the one against the squash commit doesn't.
func TestUnshippedFindsWhereWorkLandedDespiteLaterEdits(t *testing.T) {
	f := newTidyFixture(t)
	tgit(t, f.repo, "checkout", "-q", "-b", "stale", "main")
	writeFile(t, filepath.Join(f.repo, "base"), "first draft")
	tgit(t, f.repo, "commit", "-q", "-am", "draft")
	writeFile(t, filepath.Join(f.repo, "base"), "final")
	tgit(t, f.repo, "commit", "-q", "-am", "finish")
	tgit(t, f.repo, "checkout", "-q", "main")
	tgit(t, f.repo, "merge", "-q", "--squash", "stale")
	tgit(t, f.repo, "commit", "-q", "-m", "squashed feature")
	writeFile(t, filepath.Join(f.repo, "base"), "edited later")
	tgit(t, f.repo, "commit", "-q", "-am", "later edit")
	tgit(t, f.repo, "push", "-q", "origin", "main")

	objects := func() string {
		out, err := tidyGit(context.Background(), f.repo, "count-objects")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := objects()
	rep := scanLater(t, f.repo, unshippedPRs{})
	wantNoItem(t, rep, "stale")
	if len(rep.Landed) != 1 || !strings.Contains(rep.Landed[0].What, "squashed feature") {
		t.Errorf("should name the commit it landed in: %+v", rep.Landed)
	}
	if after := objects(); after != before {
		t.Errorf("the scan left objects in the repository: %s, then %s", before, after)
	}
}

// A commit's author, time and subject only say where to look. What the branch
// holds that no state of the default branch ever had stays reported.
func TestUnshippedReportsRewrittenCommitsThatDiffer(t *testing.T) {
	f := newTidyFixture(t)
	const when = "2026-10-01T10:00:00Z"
	commit := func(content string) {
		writeFile(t, filepath.Join(f.repo, "base"), content)
		tgit(t, f.repo, "add", "base")
		tgit(t, f.repo, "commit", "-q", "--date="+when, "-m", "change the base")
	}
	tgit(t, f.repo, "checkout", "-q", "-b", "resolved-away", "main")
	commit("the branch's own version")
	tgit(t, f.repo, "checkout", "-q", "-b", "amended", "main")
	commit("landed\n")
	writeFile(t, filepath.Join(f.repo, "base"), "landed\namended in afterwards\n")
	tgit(t, f.repo, "add", "base")
	tgit(t, f.repo, "commit", "-q", "--amend", "--no-edit", "--date="+when)
	tgit(t, f.repo, "checkout", "-q", "main")
	commit("landed\n")
	tgit(t, f.repo, "push", "-q", "origin", "main")

	rep := scanLater(t, f.repo, unshippedPRs{})
	wantItem(t, rep, unshippedUnpushed, "resolved-away")
	wantItem(t, rep, unshippedUnpushed, "amended")
}

// A pull request merged into another branch says nothing of the default one.
func TestUnshippedPullRequestMergedElsewhere(t *testing.T) {
	f := newTidyFixture(t)
	push := func(b string) string {
		tgit(t, f.repo, "checkout", "-q", "-b", b, "main")
		sha := tcommit(t, f.repo, b+".txt")
		tgit(t, f.repo, "push", "-q", "origin", b)
		tgit(t, f.repo, "checkout", "-q", "main")
		return sha
	}
	offTip := push("off-main")
	stackedTip := push("stacked")
	intoMain := push("into-main")
	beyondTip := push("beyond")
	tgit(t, f.repo, "checkout", "-q", "beyond")
	tcommit(t, f.repo, "unrelated.txt")
	tgit(t, f.repo, "push", "-q", "origin", "beyond")
	tgit(t, f.repo, "checkout", "-q", "main")

	rep := scanLater(t, f.repo, unshippedPRs{known: true,
		merged: []tidyPR{
			{Number: 1, HeadRef: "off-main", HeadSHA: offTip, Base: "support"},
			{Number: 2, HeadRef: "stacked", HeadSHA: stackedTip, Base: "parent"},
			{Number: 3, HeadRef: "into-main", HeadSHA: intoMain, Base: "main"},
			{Number: 4, HeadRef: "beyond", HeadSHA: beyondTip, Base: "support"},
		},
		open: []tidyPR{{Number: 5, HeadRef: "parent", Base: "main"}},
	})
	if it := wantItem(t, rep, unshippedNoPR, "off-main"); !strings.Contains(it.What, "#1 merged only into support") {
		t.Errorf("should say where it was merged: %+v", it)
	}
	wantItem(t, rep, unshippedNoPR, "beyond")
	wantNoItem(t, rep, "stacked")
	wantNoItem(t, rep, "into-main")
}

// landedBranches makes feat and other, with each one's commit copied to main,
// then starts merging other into feat, left for the test to commit.
func landedBranches(t *testing.T, f *tidyFixture) {
	t.Helper()
	for _, b := range []string{"feat", "other"} {
		tgit(t, f.repo, "checkout", "-q", "-b", b, "main")
		tcommit(t, f.repo, b+".txt")
	}
	tgit(t, f.repo, "checkout", "-q", "main")
	tgit(t, f.repo, "cherry-pick", "-x", "feat")
	tgit(t, f.repo, "cherry-pick", "-x", "other")
	tgit(t, f.repo, "checkout", "-q", "feat")
	tgit(t, f.repo, "merge", "-q", "--no-ff", "--no-commit", "other")
}

// git cherry doesn't look at merges, so it can't speak for one's own work.
func TestUnshippedKeepsWorkOnlyAMergeHolds(t *testing.T) {
	f := newTidyFixture(t)
	landedBranches(t, f)
	writeFile(t, filepath.Join(f.repo, "by-hand.txt"), "only the merge has this")
	tgit(t, f.repo, "add", "by-hand.txt")
	tgit(t, f.repo, "commit", "-q", "-m", "merge other")
	tgit(t, f.repo, "push", "-q", "origin", "main")
	tgit(t, f.repo, "checkout", "-q", "main")

	wantItem(t, scanLater(t, f.repo, unshippedPRs{}), unshippedUnpushed, "feat")
}

// A merge git made unaided adds nothing, so every patch being there still
// counts, here with main having moved on so that a merge against it conflicts.
func TestUnshippedCleanMergeStillLanded(t *testing.T) {
	f := newTidyFixture(t)
	landedBranches(t, f)
	tgit(t, f.repo, "commit", "-q", "-m", "merge other")
	tgit(t, f.repo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(f.repo, "feat.txt"), "main moved on")
	tgit(t, f.repo, "commit", "-q", "-am", "main moved on")
	tgit(t, f.repo, "push", "-q", "origin", "main")

	rep := scanLater(t, f.repo, unshippedPRs{})
	wantNoItem(t, rep, "feat")
	found := false
	for _, it := range rep.Landed {
		found = found || (it.Branch == "feat" && strings.Contains(it.What, "every commit's patch"))
	}
	if !found {
		t.Errorf("should be landed by its patches: %+v", rep.Landed)
	}
}

func TestUnshippedJudgesOnlyCommitsPastAMergedPR(t *testing.T) {
	f := newTidyFixture(t)
	tip := f.squash(t, "sq-more")
	tgit(t, f.repo, "checkout", "-q", "sq-more")
	tcommit(t, f.repo, "extra.txt")
	tgit(t, f.repo, "checkout", "-q", "main")

	rep := scanLater(t, f.repo, unshippedPRs{known: true, merged: []tidyPR{{Number: 9, HeadRef: "sq-more", HeadSHA: tip, Base: "main", State: "merged"}}})
	if it := wantItem(t, rep, unshippedUnpushed, "sq-more"); !strings.Contains(it.What, "after merged PR #9") {
		t.Errorf("should say the commits came after the pull request: %+v", it)
	}
}

func TestUnshippedStashes(t *testing.T) {
	f := newTidyFixture(t)
	stash := func(name, msg string, flags ...string) {
		writeFile(t, filepath.Join(f.repo, name), "stashed "+name)
		tgit(t, f.repo, append([]string{"stash", "push", "-q", "-m", msg}, flags...)...)
	}

	writeFile(t, filepath.Join(f.repo, "base"), "changed")
	tgit(t, f.repo, "stash", "push", "-q", "-m", "keep-me")
	rep := scanLater(t, f.repo, unshippedPRs{})
	wantItem(t, rep, unshippedStash, "stash@{0}")
	tgit(t, f.repo, "stash", "drop", "-q")

	stash("only-here.txt", "untracked", "-u")
	if it := wantItem(t, scanLater(t, f.repo, unshippedPRs{}), unshippedStash, "stash@{0}"); !strings.Contains(it.What, "untracked") {
		t.Errorf("stash item: %+v", it)
	}
	tgit(t, f.repo, "stash", "drop", "-q")

	stash("landed.txt", "already there", "-u")
	writeFile(t, filepath.Join(f.repo, "landed.txt"), "stashed landed.txt")
	tgit(t, f.repo, "add", "landed.txt")
	tgit(t, f.repo, "commit", "-q", "-m", "landed")
	tgit(t, f.repo, "push", "-q", "origin", "main")
	rep = scanLater(t, f.repo, unshippedPRs{})
	wantNoItem(t, rep, "stash@")
	if len(rep.Landed) != 1 {
		t.Errorf("landed = %+v, want 1", rep.Landed)
	}
	tgit(t, f.repo, "stash", "drop", "-q")

	writeFile(t, filepath.Join(f.repo, "base"), "same change")
	tgit(t, f.repo, "stash", "push", "-q", "-m", "same")
	writeFile(t, filepath.Join(f.repo, "base"), "same change")
	tgit(t, f.repo, "commit", "-q", "-am", "same change")
	tgit(t, f.repo, "push", "-q", "origin", "main")
	wantNoItem(t, scanLater(t, f.repo, unshippedPRs{}), "stash@")
	tgit(t, f.repo, "stash", "drop", "-q")

	writeFile(t, filepath.Join(f.repo, "base"), "landed once")
	tgit(t, f.repo, "stash", "push", "-q", "-m", "landed once")
	writeFile(t, filepath.Join(f.repo, "base"), "landed once")
	tgit(t, f.repo, "commit", "-q", "-am", "landing commit")
	writeFile(t, filepath.Join(f.repo, "base"), "edited since")
	tgit(t, f.repo, "commit", "-q", "-am", "edited since")
	tgit(t, f.repo, "push", "-q", "origin", "main")
	rep = scanLater(t, f.repo, unshippedPRs{})
	wantNoItem(t, rep, "stash@")
	if len(rep.Landed) != 1 || !strings.Contains(rep.Landed[0].What, "landing commit") {
		t.Errorf("should name the commit it landed in: %+v", rep.Landed)
	}
}

// Staged content the worktree no longer has is the stash's only copy of it.
func TestUnshippedStashStagedContent(t *testing.T) {
	f := newTidyFixture(t)
	original, err := os.ReadFile(filepath.Join(f.repo, "base")) // #nosec G304 -- the test's own file
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.repo, "base"), "only ever staged\n")
	tgit(t, f.repo, "add", "base")
	writeFile(t, filepath.Join(f.repo, "base"), string(original))
	tgit(t, f.repo, "stash", "push", "-q", "-m", "staged only")
	wantItem(t, scanLater(t, f.repo, unshippedPRs{}), unshippedStash, "stash@{0}")
	tgit(t, f.repo, "stash", "drop", "-q")

	writeFile(t, filepath.Join(f.repo, "side"), "staged\n")
	tgit(t, f.repo, "add", "side")
	writeFile(t, filepath.Join(f.repo, "base"), "not staged\n")
	tgit(t, f.repo, "stash", "push", "-q", "-m", "landed whole")
	writeFile(t, filepath.Join(f.repo, "side"), "staged\n")
	writeFile(t, filepath.Join(f.repo, "base"), "not staged\n")
	tgit(t, f.repo, "add", "side", "base")
	tgit(t, f.repo, "commit", "-q", "-m", "landed whole")
	tgit(t, f.repo, "push", "-q", "origin", "main")
	wantNoItem(t, scanLater(t, f.repo, unshippedPRs{}), "stash@")
}

// The list position of a stash moves when another is made.
func TestUnshippedStashToldByCommit(t *testing.T) {
	first := unshippedItem{Kind: unshippedStash, Stash: "stash@{0}", Commit: "aaa"}
	moved := unshippedItem{Kind: unshippedStash, Stash: "stash@{1}", Commit: "aaa"}
	newer := unshippedItem{Kind: unshippedStash, Stash: "stash@{0}", Commit: "bbb"}
	if first.onceKey() != moved.onceKey() {
		t.Errorf("the same stash, moved: %s and %s", first.onceKey(), moved.onceKey())
	}
	if first.onceKey() == newer.onceKey() {
		t.Errorf("a new stash in the old one's place: %s", newer.onceKey())
	}
}

func TestUnshippedOnce(t *testing.T) {
	c := &cli{}
	kept := func(path, stays string) {
		if err := c.updateState(func(s *State) {
			if s.WorktreesKept == nil {
				s.WorktreesKept = map[string]time.Time{}
			}
			s.WorktreesKept[path+"\x00"+stays] = time.Now()
		}); err != nil {
			t.Fatal(err)
		}
	}
	kept("/r/told", unshippedUncommitted)
	got := c.unshippedOnce("/r", []unshippedItem{
		{Kind: unshippedUncommitted, Path: "/r/told", What: "the worktree report told this"},
		{Kind: unshippedNoPR, Path: "/r/told", Branch: "b", What: "pushed"},
		{Kind: unshippedUnpushed, Path: "/r/locked", Branch: "c", What: "1 commit"},
	})
	if len(got) != 2 || strings.Contains(strings.Join(got, ""), "report told") {
		t.Errorf("got %q", got)
	}
	if again := c.unshippedOnce("/r", []unshippedItem{{Kind: unshippedNoPR, Path: "/r/told", Branch: "b"}}); len(again) != 0 {
		t.Errorf("told twice: %q", again)
	}

	var many []unshippedItem
	for n := range 12 {
		many = append(many, unshippedItem{Kind: unshippedNoPR, Branch: "branch-" + strconv.Itoa(n), What: strings.Repeat("x", 100)})
	}
	first := c.unshippedOnce("/m", many)
	if len(first) == 0 || len(first) == len(many) {
		t.Fatalf("first notice held %d of %d", len(first), len(many))
	}
	total := 0
	for _, l := range first {
		total += len(l) + 3
	}
	if total > unshippedNoticeRoom {
		t.Errorf("the notice is %d long", total)
	}
	if rest := c.unshippedOnce("/m", many); len(rest) != len(many)-len(first) {
		t.Errorf("the next sweep told %d, want the other %d", len(rest), len(many)-len(first))
	}
}

// Work is listed however recently it changed. A process in the folder marks it
// in use; that is for the reader and autopilot's notice to weigh.
func TestUnshippedMarksWorkInUse(t *testing.T) {
	f := newTidyFixture(t)
	tgit(t, f.repo, "branch", "busy", "main")
	busy := f.worktree(t, "wt-busy", "busy")
	writeFile(t, filepath.Join(busy, "new.txt"), "work")

	if it := wantItem(t, scanLater(t, f.repo, unshippedPRs{}, busy), unshippedUncommitted, "wt-busy"); !it.InUse {
		t.Errorf("a process is there: %+v", it)
	}

	now, err := scanUnshipped(context.Background(), f.repo, unshippedPRs{}, unshippedOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if it := wantItem(t, now, unshippedUncommitted, "wt-busy"); it.InUse {
		t.Errorf("nothing is running there: %+v", it)
	}
}

// A scan cut short mustn't report everything it didn't finish judging.
func TestUnshippedOutOfTimeIsAnError(t *testing.T) {
	f := newTidyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rep, err := scanUnshipped(ctx, f.repo, unshippedPRs{}, unshippedOpts{}); err == nil {
		t.Errorf("no error: %+v", rep)
	}
}

func TestUnshippedIgnoresTheAgentsOwnFiles(t *testing.T) {
	f := newTidyFixture(t)
	tgit(t, f.repo, "branch", "own", "main")
	own := f.worktree(t, "wt-own", "own")
	if err := os.MkdirAll(filepath.Join(own, ".claude"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(own, ".claude", "settings.local.json"), "{}")
	wantNoItem(t, scanLater(t, f.repo, unshippedPRs{}), "wt-own")
}

func TestUnshippedNoRemotesIsNotJudged(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tgit(t, dir, "init", "-q", "-b", "main")
	tcommit(t, dir, "base")
	tcommit(t, dir, "local-only")
	rep := scanLater(t, dir, unshippedPRs{known: true})
	if len(rep.Items) != 0 || len(rep.Notes) == 0 {
		t.Errorf("a repository with no remote: items %+v, notes %v", rep.Items, rep.Notes)
	}

	writeFile(t, filepath.Join(dir, "base"), "stashed")
	tgit(t, dir, "stash", "push", "-q", "-m", "keep")
	writeFile(t, filepath.Join(dir, "new.txt"), "work")
	rep = scanLater(t, dir, unshippedPRs{known: true})
	wantItem(t, rep, unshippedStash, "stash@{0}")
	wantItem(t, rep, unshippedUncommitted, dir)
	if len(rep.Items) != 2 {
		t.Errorf("commits aren't judged here: %+v", rep.Items)
	}
}

func TestFindReposOnePerRepository(t *testing.T) {
	f := newTidyFixture(t)
	tgit(t, f.repo, "branch", "side", "main")
	f.worktree(t, "wt-side", "side")
	root := filepath.Dir(f.repo)
	deep := filepath.Join(root, "node_modules", "pkg")
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	tgit(t, deep, "init", "-q", "-b", "main")

	got := findRepos(context.Background(), []string{root})
	if len(got) != 1 {
		t.Fatalf("repos = %v, want one (no node_modules, one per set of worktrees)", got)
	}
}

// A repository inside another, or behind a link, holds work too.
func TestFindReposNestedAndLinked(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(root, "outer")
	inner := filepath.Join(outer, "inner")
	away := filepath.Join(t.TempDir(), "away")
	for _, d := range []string{outer, inner, away} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
		tgit(t, d, "init", "-q", "-b", "main")
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(away, link); err != nil {
		t.Fatal(err)
	}
	got := findRepos(context.Background(), []string{root})
	for _, want := range []string{outer, inner, link} {
		if !slices.Contains(got, want) {
			t.Errorf("no %s in %v", want, got)
		}
	}
}

func TestSweepUnshippedDropsWhatIsReportedOrHeld(t *testing.T) {
	base := t.TempDir()
	inBase := filepath.Join(base, "wp-1")
	if err := os.MkdirAll(inBase, 0o750); err != nil {
		t.Fatal(err)
	}
	items := []unshippedItem{
		{Kind: unshippedNoPR, Path: inBase, Branch: "wp-2", Task: "WP-2"},
		{Kind: unshippedUnpushed, Branch: "wp-3", Task: "WP-3"},
		{Kind: unshippedStash, Stash: "stash@{0}", Branch: "wp-4", Task: "WP-4"},
		{Kind: unshippedUnpushed, Branch: "wp-5", Task: "WP-5", InUse: true},
	}
	got := sweepUnshipped(items, []autopilotHeld{{Key: "wp-3"}})
	if len(got) != 2 || got[0].Task != "WP-2" || got[1].Task != "WP-4" {
		t.Errorf("sweepUnshipped = %+v", got)
	}
}

func TestUnshippedTextSaysWhatWasLeftOut(t *testing.T) {
	reps := []*unshippedReport{
		{Root: "/r", Items: []unshippedItem{
			{Kind: unshippedStash, Stash: "stash@{0}", Task: "WP-4", What: "On x: y (3 days old)"},
			{Kind: unshippedUnpushed, Branch: "b", What: "1 commit on no remote", InUse: true},
		}, Landed: []unshippedItem{{Kind: unshippedUnpushed, Branch: "gone", What: "landed in abc feature"}, {Kind: unshippedUnpushed, Branch: "g2", What: "x"}}},
		{Root: "/clean", Items: []unshippedItem{}},
	}
	text := unshippedText(reps, false)
	for _, want := range []string{"/r\n", "stash@{0} [WP-4]: On x: y", "1 commit on no remote (in use)", "2 items not shipped in 1 repository, 1 in use", "2 already landed"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "/clean") || strings.Contains(text, "landed in abc") {
		t.Errorf("listed what it shouldn't:\n%s", text)
	}
	if listed := unshippedText(reps, true); !strings.Contains(listed, "gone: landed in abc feature") {
		t.Errorf("--landed should list them:\n%s", listed)
	}
	if got := unshippedText(nil, false); !strings.Contains(got, "No repositories found") {
		t.Errorf("no repositories: %q", got)
	}
	clean := []*unshippedReport{{Root: "/c", Items: []unshippedItem{}}}
	if got := unshippedText(clean, false); !strings.Contains(got, "Nothing unshipped.") {
		t.Errorf("empty: %q", got)
	}
	failed := []*unshippedReport{{Root: "/f", Items: []unshippedItem{}, Notes: []string{"ran out of time"}}}
	if got := unshippedText(failed, false); strings.Contains(got, "Nothing unshipped.") || !strings.Contains(got, "note: ran out of time") {
		t.Errorf("a scan with notes can't say the tree is clean: %q", got)
	}
}
