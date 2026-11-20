package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// landForge is the shipping fake with branches: what is in what, when
// commits were made, and what was deleted, marked merged or closed.
type landForge struct {
	*shipForge
	lm        sync.Mutex
	branches  []ForgeBranch
	contained map[string]bool      // "base...head": head is already in base
	times     map[string]time.Time // sha → commit time
	refuse    bool                 // the forge refuses manual merges
	noRepo    bool                 // the repo is gone: its default branch is a 404
	defCalls  int
	deleted   []string
	marked    []string // "number:sha"
	closed    []int
}

func newLandForge(se *shipEnv) *landForge {
	lf := &landForge{shipForge: se.sg, contained: map[string]bool{}, times: map[string]time.Time{}}
	se.s.Forges = lf
	return lf
}

func (g *landForge) DefaultBranch(ctx context.Context, f *Forge, repo string) (string, error) {
	g.lm.Lock()
	defer g.lm.Unlock()
	g.defCalls++
	if g.noRepo {
		return "", &forgeAPIError{method: "GET", path: "/repos/" + repo, status: http.StatusNotFound}
	}
	return "main", nil
}

func (g *landForge) Branches(ctx context.Context, f *Forge, repo string) ([]ForgeBranch, error) {
	g.lm.Lock()
	defer g.lm.Unlock()
	return slices.Clone(g.branches), nil
}

func (g *landForge) Ahead(ctx context.Context, f *Forge, repo, base, head string) (int, error) {
	g.lm.Lock()
	defer g.lm.Unlock()
	if g.contained[base+"..."+head] {
		return 0, nil
	}
	return 1, nil
}

func (g *landForge) CommitTime(ctx context.Context, f *Forge, repo, sha string) (time.Time, error) {
	g.lm.Lock()
	defer g.lm.Unlock()
	return g.times[sha], nil
}

func (g *landForge) DeleteBranch(ctx context.Context, f *Forge, repo, branch string) error {
	g.lm.Lock()
	defer g.lm.Unlock()
	g.deleted = append(g.deleted, branch)
	return nil
}

func (g *landForge) MarkMerged(ctx context.Context, f *Forge, repo string, number int, sha string) error {
	g.lm.Lock()
	defer g.lm.Unlock()
	if f.Kind != "forgejo" {
		return ErrUnsupported
	}
	if g.refuse {
		return &forgeAPIError{method: "POST", path: "merge", status: http.StatusMethodNotAllowed, body: "manual merge is not allowed"}
	}
	g.marked = append(g.marked, fmt.Sprintf("%d:%s", number, sha))
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prs[mk(f, repo, number)].State = "merged"
	return nil
}

func (g *landForge) ClosePR(ctx context.Context, f *Forge, repo string, number int) error {
	g.lm.Lock()
	defer g.lm.Unlock()
	g.closed = append(g.closed, number)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prs[mk(f, repo, number)].State = "closed"
	return nil
}

func (g *landForge) lists() (deleted, marked []string, closed []int) {
	g.lm.Lock()
	defer g.lm.Unlock()
	return slices.Clone(g.deleted), slices.Clone(g.marked), slices.Clone(g.closed)
}

func (g *landForge) setBranches(bs ...ForgeBranch) {
	g.lm.Lock()
	defer g.lm.Unlock()
	g.branches = bs
}

func (g *landForge) landed(base, head string) {
	g.lm.Lock()
	defer g.lm.Unlock()
	g.contained[base+"..."+head] = true
}

// pushMain is a push to main, as the forge's webhook says it.
func (se *shipEnv) pushBranch(branch string) {
	se.t.Helper()
	err := se.hook(se.f, "push", map[string]any{"ref": "refs/heads/" + branch, "before": strings.Repeat("1", 40), "after": strings.Repeat("9", 40), "pusher": map[string]string{"name": "sam"},
		"repository": map[string]any{"full_name": "me/app"}, "commits": []map[string]any{{"id": "9999", "message": "m", "url": "u", "added": []string{"a.go"}, "modified": []string{}, "removed": []string{}}}})
	if err != nil {
		se.t.Fatal(err)
	}
	se.s.Wait()
}

func (se *shipEnv) arm(n int) {
	se.t.Helper()
	if _, err := se.s.db.W.Exec(`UPDATE pull_requests SET armed_at = ?, armed_style = 'merge' WHERE number = ?`, se.s.nowMS(), n); err != nil {
		se.t.Fatal(err)
	}
}

func (se *shipEnv) comments(n int) []string {
	se.sg.mu.Lock()
	defer se.sg.mu.Unlock()
	var out []string
	for _, c := range se.sg.comments[mk(se.f, "me/app", n)] {
		out = append(out, c.Body)
	}
	return out
}

const carrierSHA = "9070ca0abcdef"

// landedEnv has the stack of 2026-10-23: #4 carried #5's and #6's work onto
// main, and both are still open (#6 as a draft), #5 armed.
func landedEnv(t *testing.T, kind string) (*shipEnv, *landForge) {
	se := newShipEnv(t, kind)
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	lf := newLandForge(se)
	lf.setBranches(ForgeBranch{Name: "main", SHA: "aaaaaaaa000"}, ForgeBranch{Name: "fix-login", SHA: "1111111aaaa"}, ForgeBranch{Name: "docs", SHA: "3333333cccc"},
		ForgeBranch{Name: "stack", SHA: "2222222bbbb"})
	old := se.clockNow().Add(-72 * time.Hour)
	for _, sha := range []string{"1111111aaaa", "2222222bbbb", "3333333cccc"} {
		lf.times[sha] = old
	}
	// #4: stack, merged into main; its head contains the others'.
	carrier := ForgePR{Number: 4, Title: "Stack", Author: "sam", BaseRef: "main", HeadRef: "stack", HeadSHA: "2222222bbbb", State: "merged", MergeSHA: carrierSHA}
	se.pullHook("closed", carrier)
	se.s.Wait()
	one := testPR
	one.BaseRef = "main"
	se.pullHook("opened", one)
	two := ForgePR{Number: 6, Title: "Docs", Author: "sam", BaseRef: "main", HeadRef: "docs", HeadSHA: "3333333cccc", State: "draft"}
	se.pullHook("opened", two)
	se.s.Wait()
	lf.landed("2222222bbbb", "1111111aaaa")
	lf.landed("2222222bbbb", "3333333cccc")
	lf.landed("main", "1111111aaaa")
	lf.landed("main", "3333333cccc")
	lf.landed("main", "2222222bbbb")
	se.arm(5)
	return se, lf
}

func TestLandedPullRequestsAreFinished(t *testing.T) {
	se, lf := landedEnv(t, "forgejo")
	se.pushBranch("main")
	deleted, marked, closed := lf.lists()
	// Both are marked merged with the commit that carried them, drafts too.
	if !slices.Equal(marked, []string{"5:" + carrierSHA, "6:" + carrierSHA}) || len(closed) != 0 {
		t.Fatalf("marked %v closed %v", marked, closed)
	}
	for _, n := range []int{5, 6} {
		if pr := se.pr(n); pr.State != "merged" || pr.ArmedAt != nil {
			t.Errorf("#%d = %s armed %v", n, pr.State, pr.ArmedAt)
		}
	}
	want := "Landed on `main` with #4 (9070ca0); nothing left to merge."
	if c := se.comments(5); len(c) != 1 || c[0] != want {
		t.Errorf("comments on #5 = %q", c)
	}
	// Their branches are gone, and the stack's that carried them, and main's is not.
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"docs", "fix-login", "stack"}) {
		t.Errorf("deleted %v", deleted)
	}
	// Doing it again says nothing more.
	se.pushBranch("main")
	se.s.CheckLandedPRs(se.ctx)
	if c := se.comments(5); len(c) != 1 {
		t.Errorf("commented twice: %q", c)
	}
	if _, m, _ := lf.lists(); len(m) != 2 {
		t.Errorf("marked again: %v", m)
	}
}

func TestLandedPullRequestIsClosedWhenManualMergeIsRefused(t *testing.T) {
	se, lf := landedEnv(t, "forgejo")
	lf.refuse = true
	se.s.CheckLandedPRs(se.ctx) // the sync's safety net, no push needed
	se.s.Wait()
	_, marked, closed := lf.lists()
	slices.Sort(closed)
	if len(marked) != 0 || !slices.Equal(closed, []int{5, 6}) {
		t.Fatalf("marked %v closed %v", marked, closed)
	}
	if pr := se.pr(5); pr.State != "closed" || pr.ArmedAt != nil {
		t.Errorf("#5 = %s", pr.State)
	}
	if c := se.comments(5); len(c) != 1 {
		t.Errorf("comments = %q", c)
	}
}

func TestPullRequestWithWorkLeftIsLeftAlone(t *testing.T) {
	se, lf := landedEnv(t, "forgejo")
	more := ForgePR{Number: 7, Title: "More", Author: "sam", BaseRef: "main", HeadRef: "more", HeadSHA: "4444444dddd", State: "open"}
	se.pullHook("opened", more)
	se.pushBranch("main")
	for _, n := range []int{7} {
		if pr := se.pr(n); pr.State != "open" {
			t.Errorf("#%d = %s", n, pr.State)
		}
	}
	if c := se.comments(7); len(c) != 0 {
		t.Errorf("commented on a pull request with work left: %q", c)
	}
	_ = lf
}

func TestGitHubGetsTimeToMarkItMerged(t *testing.T) {
	se, lf := landedEnv(t, "github")
	se.s.CheckLandedPRs(se.ctx)
	se.s.Wait()
	if _, _, closed := lf.lists(); len(closed) != 0 || len(se.comments(5)) != 0 {
		t.Fatalf("acted at once: closed %v", closed)
	}
	// It marks #5 merged itself; #6 is still open some minutes later.
	lf.mu.Lock()
	lf.prs[mk(se.f, "me/app", 5)].State = "merged"
	lf.mu.Unlock()
	se.setClock(se.clockNow().Add(6 * time.Minute))
	se.s.CheckLandedPRs(se.ctx)
	se.s.Wait()
	_, _, closed := lf.lists()
	if !slices.Equal(closed, []int{6}) {
		t.Fatalf("closed %v", closed)
	}
	if c := se.comments(5); len(c) != 0 {
		t.Errorf("commented on one the forge merged: %q", c)
	}
	if c := se.comments(6); len(c) != 1 {
		t.Errorf("comments on #6 = %q", c)
	}
}

func TestMergedBranchesAreSwept(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	lf := newLandForge(se)
	now := se.clockNow()
	tips := map[string]time.Duration{"old-merged": 72 * time.Hour, "just-cut": 10 * time.Minute, "pushed-now": 72 * time.Hour, "stack-base": 72 * time.Hour,
		"stack-top": 72 * time.Hour, "protected-b": 72 * time.Hour, "release/1.0": 72 * time.Hour, "stale": 40 * 24 * time.Hour, "wip": 24 * time.Hour}
	bs := []ForgeBranch{{Name: "main", SHA: "aaaaaaaa000"}}
	for name, age := range tips {
		sha := fmt.Sprintf("%040x", len(bs)*7919)
		bs = append(bs, ForgeBranch{Name: name, SHA: sha, Protected: name == "protected-b"})
		lf.times[sha] = now.Add(-age)
		if name != "stale" && name != "wip" {
			lf.landed("main", sha)
		}
	}
	lf.setBranches(bs...)
	// A push seen half an hour ago, and an open pull request stacked on stack-base.
	se.setClock(now.Add(-30 * time.Minute))
	se.pushBranch("pushed-now")
	se.setClock(now)
	top := ForgePR{Number: 8, Title: "Top", Author: "sam", BaseRef: "main", HeadRef: "stack-top", HeadSHA: "5555555eeee", State: "open"}
	se.pullHook("opened", top)
	se.s.Wait()
	if _, err := se.s.db.W.Exec(`UPDATE pull_requests SET base_ref = 'stack-base' WHERE number = 8`); err != nil {
		t.Fatal(err)
	}

	plan, err := se.s.RemoteTidy(se.ctx, se.admin, "", "me/app")
	if err != nil || len(plan) != 1 {
		t.Fatalf("plan %v %v", plan, err)
	}
	got := map[string]string{}
	for _, r := range plan[0].Rows {
		got[r.Branch] = r.Verdict
	}
	want := map[string]string{"main": BranchProtected, "old-merged": BranchRemovable, "just-cut": BranchKeep, "pushed-now": BranchKeep, "stack-base": BranchKeep,
		"stack-top": BranchKeep, "protected-b": BranchProtected, "release/1.0": BranchProtected, "stale": BranchStale, "wip": BranchKeep}
	for b, v := range want {
		if got[b] != v {
			t.Errorf("%s = %q, want %q", b, got[b], v)
		}
	}
	// With the setting off nothing is deleted.
	if err := se.s.SetRepoBranchCleanup(se.ctx, se.admin, se.p.Key, se.repo.ID, false); err != nil {
		t.Fatal(err)
	}
	se.s.SweepBranches(se.ctx)
	if d, _, _ := lf.lists(); len(d) != 0 {
		t.Fatalf("deleted with the setting off: %v", d)
	}
	if plan, _ = se.s.RemoteTidy(se.ctx, se.admin, "", "me/app"); plan[0].Enabled {
		t.Error("listing says deleting is on")
	}
	// On (the default), only the merged and idle one goes.
	if err := se.s.SetRepoBranchCleanup(se.ctx, se.admin, se.p.Key, se.repo.ID, true); err != nil {
		t.Fatal(err)
	}
	se.s.SweepBranches(se.ctx)
	if d, _, _ := lf.lists(); !slices.Equal(d, []string{"old-merged"}) {
		t.Fatalf("deleted %v", d)
	}
	// Swept today: the next hour's sweep leaves it be.
	se.s.SweepBranches(se.ctx)
	if d, _, _ := lf.lists(); len(d) != 1 {
		t.Fatalf("swept twice: %v", d)
	}
}

func TestBranchCallsPerForge(t *testing.T) {
	ctx := context.Background()
	// Forgejo: compare counts base...head, deleting goes by name, manual merge says which commit.
	a, f, c := newAPIFake(t, "forgejo")
	a.reply["GET /repos/me/app/compare/main...abc"] = map[string]any{"total_commits": 0}
	a.reply["GET /repos/me/app/branches?limit=100&page=1"] = []map[string]any{{"name": "fix/x", "protected": true, "commit": map[string]any{"id": "abc"}}}
	a.reply["DELETE /repos/me/app/branches/fix/x"] = map[string]any{}
	a.reply["POST /repos/me/app/pulls/5/merge"] = map[string]any{}
	a.reply["GET /repos/me/app/git/commits/abc"] = map[string]any{"created": "2026-10-20T10:00:00Z"}
	if n, err := c.Ahead(ctx, f, "me/app", "main", "abc"); err != nil || n != 0 {
		t.Errorf("ahead %d %v", n, err)
	}
	if bs, err := c.Branches(ctx, f, "me/app"); err != nil || len(bs) != 1 || bs[0].SHA != "abc" || !bs[0].Protected {
		t.Errorf("branches %+v %v", bs, err)
	}
	if at, err := c.CommitTime(ctx, f, "me/app", "abc"); err != nil || at.Day() != 20 {
		t.Errorf("commit time %v %v", at, err)
	}
	if err := c.DeleteBranch(ctx, f, "me/app", "fix/x"); err != nil {
		t.Error(err)
	}
	if err := c.MarkMerged(ctx, f, "me/app", 5, "abc"); err != nil {
		t.Fatal(err)
	}
	if b := a.call("POST", "/repos/me/app/pulls/5/merge").body; b["Do"] != "manually-merged" || b["merge_commit_id"] != "abc" {
		t.Errorf("manual merge = %+v", b)
	}
	// GitHub and GitLab mark such pull requests merged themselves.
	a, f, c = newAPIFake(t, "github")
	if err := c.MarkMerged(ctx, f, "me/app", 5, "abc"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("github manual merge: %v", err)
	}
	a.reply["GET /repos/me/app/compare/main...abc?per_page=1"] = map[string]any{"ahead_by": 3}
	a.reply["DELETE /repos/me/app/git/refs/heads/fix/x"] = 422 // already gone
	if n, err := c.Ahead(ctx, f, "me/app", "main", "abc"); err != nil || n != 3 {
		t.Errorf("github ahead %d %v", n, err)
	}
	if err := c.DeleteBranch(ctx, f, "me/app", "fix/x"); err != nil {
		t.Errorf("github delete: %v", err)
	}
	a, f, c = newAPIFake(t, "gitlab")
	a.reply["GET /projects/me%2Fapp/repository/compare?from=main&to=abc"] = map[string]any{"commits": []map[string]any{}}
	if n, err := c.Ahead(ctx, f, "me/app", "main", "abc"); err != nil || n != 0 {
		t.Errorf("gitlab ahead %d %v", n, err)
	}
}

func TestPullRequestTheForgeDoesNotHaveIsClosedOnce(t *testing.T) {
	se, lf := landedEnv(t, "forgejo")
	lf.mu.Lock()
	delete(lf.prs, mk(se.f, "me/app", 5)) // deleted on the forge, or its repo is gone
	delete(lf.prs, mk(se.f, "me/app", 6))
	lf.mu.Unlock()
	se.s.CheckLandedPRs(se.ctx)
	se.s.Wait()
	for _, n := range []int{5, 6} {
		if pr := se.pr(n); pr.State != "closed" || pr.ArmedAt != nil {
			t.Errorf("#%d = %s armed %v", n, pr.State, pr.ArmedAt)
		}
	}
	if _, marked, closed := lf.lists(); len(marked) != 0 || len(closed) != 0 {
		t.Errorf("acted on the forge: marked %v closed %v", marked, closed)
	}
	// Closed pull requests aren't asked about again.
	var n int
	if err := se.s.db.R.QueryRow(`SELECT count(*) FROM pull_requests WHERE state IN ('open','draft') AND number IN (5,6)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d still open (%v)", n, err)
	}
}

func TestPullRequestsOfAMovedRepoFollowIt(t *testing.T) {
	se, lf := landedEnv(t, "forgejo")
	lf.mu.Lock()
	delete(lf.prs, mk(se.f, "me/app", 5))
	lf.repos[se.f.ID] = []ForgeRepo{{ID: 77, Path: "me/app2"}}
	lf.mu.Unlock()
	if _, err := se.s.db.W.Exec(`UPDATE project_repos SET forge_repo_id = 77`); err != nil {
		t.Fatal(err)
	}
	se.s.CheckLandedPRs(se.ctx)
	se.s.Wait()
	var repo, state string
	if err := se.s.db.R.QueryRow(`SELECT repo, state FROM pull_requests WHERE number = 5`).Scan(&repo, &state); err != nil {
		t.Fatal(err)
	}
	if repo != "me/app2" || state == "closed" {
		t.Errorf("#5 is %s in %s: it should have moved, not been closed", state, repo)
	}
}

func TestBranchSweepOfAGoneRepoIsNotRetried(t *testing.T) {
	se, lf := landedEnv(t, "forgejo")
	lf.noRepo = true
	se.s.SweepBranches(se.ctx)
	lf.lm.Lock()
	calls := lf.defCalls
	lf.lm.Unlock()
	if calls != 1 {
		t.Fatalf("asked %d times", calls)
	}
	se.s.SweepBranches(se.ctx)
	se.s.SweepBranches(se.ctx)
	lf.lm.Lock()
	again := lf.defCalls
	lf.lm.Unlock()
	if again != calls {
		t.Errorf("retried a gone repo: %d calls", again)
	}
	if _, ok := se.s.goneRepos.Load(fmt.Sprintf("%d|me/app", se.f.ID)); !ok {
		t.Error("not noted as gone")
	}
}

// The front of the merge queue whose head a push to main already holds is
// finished as landed, not brought up to date: an update would leave it one
// empty merge ahead of main, open, and running the whole CI (WP-464).
func TestFrontWhoseHeadLandedIsFinishedNotUpdated(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	lf := newLandForge(se)
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true}
	pr := testPR
	pr.BaseRef = "main"
	se.pullHook("opened", pr)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	id := se.pr(5).ID
	se.s.maybeAutoMerge(se.ctx, id)
	se.s.Wait()
	if n := se.sg.updated(); n != 1 {
		t.Fatalf("updates before = %d", n)
	}
	// Its update is pending when a push moves main to tip-2, which holds
	// #5's head (the push of 2026-11-02): still "behind" by the forge's
	// "base has commits the head lacks".
	se.sg.sm.Lock()
	se.sg.baseTip = "tip-2"
	se.sg.sm.Unlock()
	lf.setBranches(ForgeBranch{Name: "main", SHA: "tip-2"}, ForgeBranch{Name: "fix-login", SHA: head})
	lf.landed("tip-2", head)
	lf.landed("main", head)
	se.s.maybeAutoMerge(se.ctx, id)
	se.s.Wait()
	if n := se.sg.updated(); n != 1 {
		t.Fatalf("updated the branch again: %d updates", n)
	}
	if _, marked, _ := lf.lists(); !slices.Equal(marked, []string{"5:tip-2"}) {
		t.Fatalf("marked %v", marked)
	}
	if p := se.pr(5); p.State != "merged" || p.ArmedAt != nil {
		t.Errorf("#5 = %s armed %v", p.State, p.ArmedAt)
	}
	if c := se.comments(5); len(c) != 1 || c[0] != "Landed on `main` in tip-2; nothing left to merge." {
		t.Errorf("comments = %q", c)
	}
	if m := se.sg.merged(); len(m) != 0 {
		t.Errorf("merged an empty head: %+v", m)
	}
}

// A pull request whose head is main plus merges of main that brought nothing
// (it was brought up to date just as its work landed) is finished as landed;
// one whose merge changed something is left alone (WP-464).
func TestHeadAheadOnlyByEmptyMergesHasLanded(t *testing.T) {
	const (
		work   = "1111111aaaa" // the branch's own commit, now in main
		m1     = "cd1b5300000" // main when the branch was brought up to date
		empty1 = "e0000000001" // Merge branch 'main' into fix-login: nothing new
		m2     = "f00d0000000" // main since
		empty2 = "e0000000002" // brought up to date once more
		evil   = "e0000000003" // a merge of main that changed something
	)
	for _, tc := range []struct {
		name   string
		head   string
		landed bool
	}{
		{"one empty merge", empty1, true},
		{"two empty merges", empty2, true},
		{"a merge that changed something", evil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := newShipEnv(t, "forgejo")
			se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
			lf := newLandForge(se)
			lf.setBranches(ForgeBranch{Name: "main", SHA: m2}, ForgeBranch{Name: "fix-login", SHA: tc.head})
			se.sg.parents = map[string][]string{empty1: {work, m1}, empty2: {empty1, m2}, evil: {work, m1}, work: {"0ld0000"}}
			se.sg.trees = map[string]string{empty1: "t1", m1: "t1", empty2: "t2", m2: "t2", evil: "t9"}
			for _, c := range []string{work, m1, m2} {
				lf.landed("main", c)
			}
			lf.landed(m1, work)
			lf.landed(m2, work)
			lf.landed(m2, m1)
			pr := testPR
			pr.BaseRef, pr.HeadSHA = "main", tc.head
			se.pullHook("opened", pr)
			se.s.Wait()
			se.s.CheckLandedPRs(se.ctx)
			se.s.Wait()
			_, marked, closed := lf.lists()
			if !tc.landed {
				if len(marked) != 0 || len(closed) != 0 || len(se.comments(5)) != 0 || se.pr(5).State != "open" {
					t.Fatalf("finished one with work left: marked %v closed %v", marked, closed)
				}
				return
			}
			if !slices.Equal(marked, []string{"5:" + m2}) {
				t.Fatalf("marked %v closed %v", marked, closed)
			}
			if c := se.comments(5); len(c) != 1 || c[0] != "Landed on `main` in "+short(m2)+"; nothing left to merge." {
				t.Errorf("comments = %q", c)
			}
			if p := se.pr(5); p.State != "merged" {
				t.Errorf("#5 = %s", p.State)
			}
		})
	}
}
