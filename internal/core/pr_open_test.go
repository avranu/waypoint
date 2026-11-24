package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOpenPullRequest(t *testing.T) {
	e := newEnv(t)
	g := newShipForge()
	e.s.Forges = g
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot")
	in := OpenPRInput{Project: p.Key, Head: "feature", Title: "Do it", Body: "Because."}

	if _, err := e.s.OpenPullRequest(e.ctx, e.admin, in); err == nil || !strings.Contains(err.Error(), "no repository attached") {
		t.Errorf("no repo attached: %v", err)
	}
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.OpenPullRequest(e.ctx, e.admin, in); err == nil || !strings.Contains(err.Error(), "no API token") {
		t.Errorf("no token: %v", err)
	}
	if _, err := e.s.UpdateForge(e.ctx, e.admin, f.ID, ForgeInput{Token: str("t")}); err != nil {
		t.Fatal(err)
	}

	outsider := e.user("sam", false)
	if _, err := e.s.OpenPullRequest(e.ctx, outsider, in); err == nil {
		t.Error("an outsider opened a pull request")
	}
	if _, err := e.s.OpenPullRequest(e.ctx, e.admin, OpenPRInput{Project: p.Key, Head: "main", Title: "x"}); err == nil || !strings.Contains(err.Error(), "base branch") {
		t.Errorf("from the base branch: %v", err)
	}
	if _, err := e.s.OpenPullRequest(e.ctx, e.admin, OpenPRInput{Project: p.Key, Head: "feature"}); !IsInvalid(err) {
		t.Errorf("no title: %v", err)
	}

	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	iss, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: p.Key, Title: "Do it"})
	if err != nil {
		t.Fatal(err)
	}
	in.Issue = iss.Key
	res, err := e.s.OpenPullRequest(e.ctx, agent, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Existing || res.Warning != "" || res.Pull.Title != "Do it" {
		t.Errorf("first open: %+v warning %q", res.Pull.PRSummary, res.Warning)
	}
	if len(g.created) != 1 || g.created[0].Base != "main" || g.created[0].Body != "Because." {
		t.Errorf("created: %+v", g.created)
	}
	again, err := e.s.OpenPullRequest(e.ctx, agent, in)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Existing || again.Pull.ID != res.Pull.ID || len(g.created) != 1 {
		t.Errorf("second open: existing=%v created=%d", again.Existing, len(g.created))
	}
	in.Title = ""
	if retry, err := e.s.OpenPullRequest(e.ctx, agent, in); err != nil || !retry.Existing {
		t.Errorf("a retry without a title: %v", err)
	}
	d, err := e.s.GetIssue(e.ctx, e.admin, iss.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Links) != 1 || d.Links[0].Kind != "pull" {
		t.Errorf("links: %+v", d.Links)
	}
	if pr, err := e.s.prByID(e.ctx, e.s.db.R, res.Pull.ID); err != nil || pr.Agent != "anthropic/claude-opus-5-5" {
		t.Errorf("the pull request's agent: %+v %v", pr, err)
	}
	if _, err := e.s.MergePR(e.ctx, agent, res.Pull.ID, MergeInput{WhenReady: true}); err != nil {
		t.Errorf("arming its own: %v", err)
	}
}

// EditPRBase makes the shipping fake retarget pull requests, as the forge
// does.
// Branches lists g.brs: the shipping fake is a forge that lists branches (none
// by default, so none is protected).
func (g *shipForge) Branches(context.Context, *Forge, string) ([]ForgeBranch, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	return g.brs, g.brErr
}

func (g *shipForge) EditPRBase(ctx context.Context, f *Forge, repo string, number int, base string) error {
	g.sm.Lock()
	if g.baseErr != nil {
		defer g.sm.Unlock()
		return g.baseErr
	}
	g.baseEdits = append(g.baseEdits, fmt.Sprintf("%d:%s", number, base))
	g.sm.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if pr := g.prs[mk(f, repo, number)]; pr != nil {
		pr.BaseRef = base
	}
	return nil
}

// A pull request stacked on another: when that one merges, the forge doesn't
// delete the branch under it. It moves onto the parent's base, armed as
// before, and the branch goes after. If it can't move, it mustn't land in
// the leftover branch: disarmed, and the branch stays (WP-634).
func TestMergeMovesStackedPullRequestsOff(t *testing.T) {
	for _, fail := range []bool{false, true} {
		se := newShipEnv(t, "forgejo")
		se.policy(MergePolicyInput{AutoMerge: ptr(false), ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(0)})
		se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
		parent, child := testPR, testPR
		parent.BaseRef = "main"
		child.Number, child.HeadRef, child.HeadSHA, child.BaseRef = 6, "on-login", "2222222bbbb", parent.HeadRef
		se.pullHook("opened", parent)
		se.pullHook("opened", child)
		se.drain()
		if _, err := se.s.recordPR(se.ctx, se.f, "me/app", &child, false); err != nil {
			t.Fatal(err)
		}
		se.sg.setChecks(parent.HeadSHA, CommitCheck{Name: "ci / web", State: CheckSuccess})
		se.poll()
		if res, err := se.s.MergePR(se.ctx, se.admin, se.pr(6).ID, MergeInput{WhenReady: true}); err != nil || !res.Armed {
			t.Fatalf("arm the child: %+v %v", res, err)
		}
		if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET queued_at = 1 WHERE id = ?`, se.pr(6).ID); err != nil {
			t.Fatal(err)
		}
		if fail {
			se.sg.baseErr = errors.New("forge down")
		}
		if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{}); err != nil {
			t.Fatal(err)
		}
		m := se.sg.merged()
		if len(m) != 1 || m[0].DeleteBranch {
			t.Fatalf("fail=%v: merge options %+v: the forge would delete the child's base", fail, m)
		}
		c := se.pr(6)
		if fail {
			if c.BaseRef != parent.HeadRef || c.ArmedAt != nil || len(se.sg.deleted) != 0 {
				t.Errorf("not moved: base %q armed %v deleted %v", c.BaseRef, c.ArmedAt, se.sg.deleted)
			}
			continue
		}
		if c.BaseRef != "main" || c.ArmedAt == nil || c.QueuedAt != nil {
			t.Errorf("moved: base %q armed %v queued %v", c.BaseRef, c.ArmedAt, c.QueuedAt)
		}
		if !slices.Equal(se.sg.baseEdits, []string{"6:main"}) || !slices.Equal(se.sg.deleted, []string{parent.HeadRef}) {
			t.Errorf("edits %v deleted %v", se.sg.baseEdits, se.sg.deleted)
		}
	}
}

// setForgeState changes a pull request on the fake forge only, as a close
// whose webhook hasn't come yet.
func (g *shipForge) setForgeState(f *Forge, repo string, number int, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prs[mk(f, repo, number)].State = state
}

// WP-634: a stacked branch merges into the pull request it is stacked on,
// --base moves a pull request already open, and one closed on the forge
// (its webhook not in yet) isn't reused.
func TestOpenPullRequestStackAndBase(t *testing.T) {
	e := newEnv(t)
	g := newShipForge()
	e.s.Forges = g
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	open := func(in OpenPRInput) *OpenPRResult {
		t.Helper()
		in.Project, in.Ready = p.Key, true
		if in.Title == "" {
			in.Title = in.Head
		}
		res, err := e.s.OpenPullRequest(e.ctx, agent, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	parent := open(OpenPRInput{Head: "gate-go"})
	if parent.Pull.BaseRef != "main" || parent.Notice != "" {
		t.Fatalf("parent: base %q notice %q", parent.Pull.BaseRef, parent.Notice)
	}
	child := open(OpenPRInput{Head: "upgrade", StackedOn: []string{"scratch", "gate-go"}})
	if child.Pull.BaseRef != "gate-go" || !strings.Contains(child.Notice, "#"+fmt.Sprint(parent.Pull.Number)) || !strings.Contains(child.Notice, "--base main") {
		t.Errorf("stacked: base %q notice %q", child.Pull.BaseRef, child.Notice)
	}
	var detail string
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT detail FROM audit_log WHERE action = 'pr.open' AND target_id = ?`, child.Pull.ID).Scan(&detail); err != nil ||
		detail != fmt.Sprintf("opened “upgrade” into gate-go (stacked on #%d)", parent.Pull.Number) {
		t.Errorf("audit detail %q %v", detail, err)
	}
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT detail FROM audit_log WHERE action = 'pr.open' AND target_id = ?`, parent.Pull.ID).Scan(&detail); err != nil || detail != "opened “gate-go” into main" {
		t.Errorf("audit detail %q %v", detail, err)
	}
	if all := open(OpenPRInput{Head: "together", Base: "main", StackedOn: []string{"gate-go"}}); all.Pull.BaseRef != "main" || all.Notice != "" {
		t.Errorf("--base main: base %q notice %q", all.Pull.BaseRef, all.Notice)
	}
	g.setForgeState(f, "acme/app", parent.Pull.Number, "merged")
	if late := open(OpenPRInput{Head: "late", StackedOn: []string{"gate-go"}}); late.Pull.BaseRef != "main" || late.Notice != "" {
		t.Errorf("parent merged: base %q notice %q", late.Pull.BaseRef, late.Notice)
	}
	if pr, _ := e.s.prByID(e.ctx, e.s.db.R, parent.Pull.ID); pr.State != "merged" {
		t.Errorf("the merged parent is recorded %q", pr.State)
	}
	e.s.bg.Wait()
	if pr, _ := e.s.prByID(e.ctx, e.s.db.R, child.Pull.ID); pr.BaseRef != "main" {
		t.Errorf("the child of the merged parent is into %q", pr.BaseRef)
	}

	g.sm.Lock()
	g.baseEdits = nil
	g.sm.Unlock()
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE pull_requests SET queued_at = 1 WHERE id = ?`, child.Pull.ID); err != nil {
		t.Fatal(err)
	}
	moved := open(OpenPRInput{Head: "upgrade", Base: "release"})
	if pr, err := e.s.prByID(e.ctx, e.s.db.R, child.Pull.ID); err != nil {
		t.Fatal(err)
	} else if pr.QueuedAt != nil {
		t.Errorf("moved to another base, it keeps its place in the old queue: %v", pr.QueuedAt)
	}
	if !moved.Existing || moved.Pull.ID != child.Pull.ID || moved.Pull.BaseRef != "release" || !strings.Contains(moved.Notice, "is now open into release") {
		t.Errorf("moved: existing %v base %q notice %q", moved.Existing, moved.Pull.BaseRef, moved.Notice)
	}
	if want := []string{fmt.Sprintf("%d:release", child.Pull.Number)}; !slices.Equal(g.baseEdits, want) {
		t.Errorf("base edits %v, want %v", g.baseEdits, want)
	}
	if again := open(OpenPRInput{Head: "upgrade", Base: "release"}); again.Notice != "" || len(g.baseEdits) != 1 {
		t.Errorf("same base again: notice %q edits %v", again.Notice, g.baseEdits)
	}

	g.setForgeState(f, "acme/app", child.Pull.Number, "closed")
	reopened := open(OpenPRInput{Head: "upgrade"})
	if reopened.Existing || reopened.Pull.ID == child.Pull.ID {
		t.Errorf("reused the closed pull request: %+v", reopened.Pull.PRSummary)
	}
	if _, err := e.s.MergePR(e.ctx, agent, child.Pull.ID, MergeInput{WhenReady: true}); err == nil {
		t.Error("armed the closed pull request")
	}

	if err := e.s.EditPullRequest(e.ctx, agent, reopened.Pull.ID, EditPRInput{Base: str(" ")}); !IsInvalid(err) {
		t.Errorf("empty base: %v", err)
	}
	if err := e.s.EditPullRequest(e.ctx, agent, reopened.Pull.ID, EditPRInput{Base: str("upgrade")}); !IsInvalid(err) {
		t.Errorf("its own branch as base: %v", err)
	}
	before := len(g.baseEdits)
	if err := e.s.EditPullRequest(e.ctx, agent, reopened.Pull.ID, EditPRInput{Base: str("main")}); err != nil || len(g.baseEdits) != before {
		t.Errorf("same base: %v, edits %v", err, g.baseEdits)
	}

	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE pull_requests SET queued_at = 1 WHERE id = ?`, reopened.Pull.ID); err != nil {
		t.Fatal(err)
	}
	if err := g.EditPRBase(e.ctx, f, "acme/app", reopened.Pull.Number, "release"); err != nil {
		t.Fatal(err)
	}
	open(OpenPRInput{Head: "upgrade"})
	if pr, err := e.s.prByID(e.ctx, e.s.db.R, reopened.Pull.ID); err != nil {
		t.Fatal(err)
	} else if pr.BaseRef != "release" || pr.QueuedAt != nil {
		t.Errorf("moved on the forge: base %q queued %v", pr.BaseRef, pr.QueuedAt)
	}

	fork := ForgePR{Number: 900, Title: "Fork", Author: "wp-bot", URL: "https://git.example.com/acme/app/pulls/900", State: "open", BaseRef: "main", HeadRef: "shared", HeadSHA: "abcdef0", HeadRepo: "sam/app"}
	g.setPR(f, "acme/app", fork, sampleDiff)
	if _, err := e.s.recordPR(e.ctx, f, "acme/app", &fork, false); err != nil {
		t.Fatal(err)
	}
	if onFork := open(OpenPRInput{Head: "on-fork", StackedOn: []string{"shared"}}); onFork.Pull.BaseRef != "main" || onFork.Notice != "" {
		t.Errorf("stacked on a fork's branch name: base %q notice %q", onFork.Pull.BaseRef, onFork.Notice)
	}

	mine, theirs := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Mine"}), e.issue(e.admin, IssueInput{Project: p.Key, Title: "Theirs"})
	open(OpenPRInput{Head: "theirs", Issue: theirs.Key})
	before = len(g.baseEdits)
	if res := open(OpenPRInput{Head: "theirs", Base: "release", Issue: mine.Key}); len(g.baseEdits) != before || res.Pull.BaseRef != "main" ||
		!strings.Contains(res.Notice, "stays open into main") || !strings.Contains(res.Notice, "--base release") {
		t.Errorf("another task's: edits %v base %q notice %q", g.baseEdits, res.Pull.BaseRef, res.Notice)
	}
}

// branchyForge is the shipping fake that also lists branches.
type branchyForge struct {
	*shipForge
	branches []ForgeBranch
}

func (b *branchyForge) Branches(context.Context, *Forge, string) ([]ForgeBranch, error) {
	return b.branches, nil
}

// WP-760: a parent merged outside Waypoint's merge path moves the pull
// requests open into its branch onto its base, as the merge path does. Not
// when the branch is one the repo keeps (default or protected), and not a
// pull request only some other pull request's base happens to name.
func TestStackedChildrenMoveWhenParentMergesOutside(t *testing.T) {
	for _, tc := range []struct {
		name     string
		head     string
		branches []ForgeBranch
		moved    bool
	}{
		{"deleted branch", "gate-go", nil, true},
		{"branch kept, not protected", "gate-go", []ForgeBranch{{Name: "gate-go"}}, true},
		{"protected branch", "develop", []ForgeBranch{{Name: "develop", Protected: true}}, false},
		{"default branch", "main", []ForgeBranch{{Name: "main"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			g := newShipForge()
			e.s.Forges = &branchyForge{shipForge: g, branches: tc.branches}
			p := e.project(e.admin, "App", "")
			f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
			if err != nil {
				t.Fatal(err)
			}
			e.forgePeople(f, "wp-bot")
			if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
				t.Fatal(err)
			}
			agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
			open := func(head, base string) *OpenPRResult {
				t.Helper()
				res, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Ready: true, Head: head, Base: base, Title: head})
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			parentBase := "main"
			if tc.head == "main" {
				parentBase = "release"
			}
			parent := open(tc.head, parentBase)
			child := open("child-work", tc.head)
			unrelated := open("elsewhere", "other")
			g.setForgeState(f, "acme/app", parent.Pull.Number, "merged")
			fp, err := g.GetPR(e.ctx, f, "acme/app", parent.Pull.Number)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.s.recordPR(e.ctx, f, "acme/app", fp, true); err != nil {
				t.Fatal(err)
			}
			e.s.bg.Wait()
			want := tc.head
			if tc.moved {
				want = parentBase
			}
			if pr, _ := e.s.prByID(e.ctx, e.s.db.R, child.Pull.ID); pr.BaseRef != want {
				t.Errorf("child is into %q, want %q", pr.BaseRef, want)
			}
			if pr, _ := e.s.prByID(e.ctx, e.s.db.R, unrelated.Pull.ID); pr.BaseRef != "other" {
				t.Errorf("an unrelated pull request moved to %q", pr.BaseRef)
			}
			if tc.moved != (len(g.baseEdits) == 1) {
				t.Errorf("base edits %v", g.baseEdits)
			}
			if _, err := e.s.recordPR(e.ctx, f, "acme/app", fp, true); err != nil {
				t.Fatal(err)
			}
			e.s.bg.Wait()
			if tc.moved != (len(g.baseEdits) == 1) {
				t.Errorf("after a repeat: base edits %v", g.baseEdits)
			}
		})
	}
}

// --base on an open pull request the forge can't move fails, and the
// record keeps the base the forge still has (WP-1670).
func TestOpenWithBaseFailsWhenForgeCannotMove(t *testing.T) {
	e := newEnv(t)
	g := newShipForge()
	e.s.Forges = g
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	in := OpenPRInput{Project: p.Key, Ready: true, Head: "feat", Title: "feat"}
	res, err := e.s.OpenPullRequest(e.ctx, agent, in)
	if err != nil {
		t.Fatal(err)
	}
	g.baseErr = errors.New("the forge refused")
	in.Base = "release"
	if _, err := e.s.OpenPullRequest(e.ctx, agent, in); err == nil || !strings.Contains(err.Error(), "moving it failed") {
		t.Fatalf("open with --base the forge refuses: %v", err)
	}
	if err := e.s.EditPullRequest(e.ctx, agent, res.Pull.ID, EditPRInput{Base: str("release")}); err == nil {
		t.Error("wp pr edit --base succeeded against a forge that refused")
	}
	if pr, _ := e.s.prByID(e.ctx, e.s.db.R, res.Pull.ID); pr.BaseRef != "main" {
		t.Errorf("recorded base %q after a failed move", pr.BaseRef)
	}
}

// stackedPair opens a parent into main and a child into the parent's
// branch, both with green CI, and arms the child (WP-1670).
func stackedPair(t *testing.T) (se *shipEnv, parent, child ForgePR) {
	t.Helper()
	return stackedPairWith(t, "open", true)
}

// stackedPairWith is stackedPair with the parent in state (open or draft),
// and the child armed or not.
func stackedPairWith(t *testing.T, state string, arm bool) (se *shipEnv, parent, child ForgePR) {
	t.Helper()
	se = newShipEnv(t, "forgejo")
	se.policy(MergePolicyInput{AutoMerge: ptr(false), ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(0)})
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	parent, child = testPR, testPR
	parent.BaseRef, parent.State = "main", state
	child.Number, child.HeadRef, child.HeadSHA, child.BaseRef = 6, "on-login", "2222222bbbb", parent.HeadRef
	se.pullHook("opened", parent)
	se.pullHook("opened", child)
	se.drain()
	if _, err := se.s.recordPR(se.ctx, se.f, "me/app", &child, false); err != nil {
		t.Fatal(err)
	}
	se.sg.setChecks(parent.HeadSHA, CommitCheck{Name: "ci / web", State: CheckSuccess})
	se.sg.setChecks(child.HeadSHA, CommitCheck{Name: "ci / web", State: CheckSuccess})
	se.poll()
	if arm {
		if res, err := se.s.MergePR(se.ctx, se.admin, se.pr(6).ID, MergeInput{WhenReady: true}); err != nil || !res.Armed {
			t.Fatalf("arm the child: %+v %v", res, err)
		}
	}
	return se, parent, child
}

// WP-ADR153, WP-1670: an armed child is never merged into its parent's
// branch while the parent is open: that would move the parent's head after
// its review. The parent's merge moves it onto the parent's base, still
// armed, same head and reviews, and it is held no longer.
func TestStackedChildWaitsForParent(t *testing.T) {
	se, parent, _ := stackedPair(t)
	before := se.pr(6)
	for range 2 {
		se.poll()
	}
	for _, m := range se.sg.merged() {
		t.Fatalf("merged into the parent's branch: %+v", m)
	}
	rd, err := se.s.readinessFor(se.ctx, se.pr(6), true)
	if err != nil {
		t.Fatal(err)
	}
	if r := mergeWaitIn(rd.Reasons); !strings.Contains(r, "merges after #5, which it is stacked on (Waypoint moves it onto main when #5 merges)") {
		t.Fatalf("reasons %q", rd.Reasons)
	}
	if rd.Ready {
		t.Error("ready while its parent is open")
	}
	if _, err := se.s.SetPRMergeAfter(se.ctx, se.admin, se.pr(6).ID, "", 5, false); err == nil || !strings.Contains(err.Error(), "wp pr edit 6 --base main") {
		t.Errorf("off: %v", err)
	}

	// Once moved it is held no longer, so the auto-merge the move nudges
	// may land it before Wait returns: only ever onto main, after the move.
	var childMergedOnto []string
	se.sg.sm.Lock()
	se.sg.onMergeN = func(n int) { // onMergeN runs inside MergePR with sm held, so it must not lock sm.
		if n == 6 {
			childMergedOnto = slices.Clone(se.sg.baseEdits)
		}
	}
	se.sg.sm.Unlock()
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{}); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	c := se.pr(6)
	if c.BaseRef != "main" || c.HeadSHA != before.HeadSHA || (c.ArmedAt == nil && c.State != "merged") {
		t.Fatalf("moved: base %q armed %v state %q head %q", c.BaseRef, c.ArmedAt, c.State, c.HeadSHA)
	}
	if c.State == "merged" && !slices.Equal(childMergedOnto, []string{"6:main"}) {
		t.Fatalf("merged before its move: base edits then %v", childMergedOnto)
	}
	if c.State != "merged" {
		if rd, err = se.s.readinessFor(se.ctx, c, true); err != nil || mergeWaitIn(rd.Reasons) != "" {
			t.Errorf("still waits: %+v %v", rd, err)
		}
	}
	if !slices.Equal(se.sg.baseEdits, []string{"6:main"}) {
		t.Errorf("base edits %v", se.sg.baseEdits)
	}
	if n := se.auditCount("pr.edit", "moved the base from "+parent.HeadRef+" to main of"); n != 1 {
		t.Errorf("%d audit entries for the move", n)
	}
}

// WP-1670: a parent closed without merging leaves its child nothing to
// merge into: a person's call, with the way out in the reason.
func TestStackedChildOfClosedParentNeedsPerson(t *testing.T) {
	se, parent, _ := stackedPair(t)
	parent.State = "closed"
	se.pullHook("closed", parent)
	se.s.Wait()
	se.poll()
	for _, m := range se.sg.merged() {
		t.Fatalf("merged into a closed parent's branch: %+v", m)
	}
	c := se.pr(6)
	if c.BaseRef != "fix-login" {
		t.Fatalf("base %q: nothing moves it automatically", c.BaseRef)
	}
	rd, err := se.s.readinessFor(se.ctx, c, true)
	if err != nil {
		t.Fatal(err)
	}
	why := personal(c, rd.Reasons)
	if rd.Ready || !strings.Contains(why, "stacked on a pull request closed without merging: its base fix-login is #5's branch; move it with `wp pr edit 6 --base main`") {
		t.Fatalf("ready %v reasons %q", rd.Ready, rd.Reasons)
	}
}

// WP-1670: a pull request closed on the forge isn't armed.
func TestArmClosedPullRequestFails(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setForgeState(se.f, "me/app", 5, "closed")
	if res, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{WhenReady: true}); err == nil || !strings.Contains(err.Error(), "this pull request is closed") {
		t.Fatalf("armed a closed pull request: %+v %v", res, err)
	}
	if pr := se.pr(5); pr.ArmedAt != nil || pr.State != "closed" {
		t.Errorf("armed %v, recorded %q", pr.ArmedAt, pr.State)
	}
}

// The base a ship asked for in its review round is the one a later open of
// that branch uses without --base, even when --stacked-on names an open
// parent (the asked base wins; WP-1670). A round for another branch of the
// task doesn't apply, and no task has no asked base.
func TestAskedBaseRound(t *testing.T) {
	e := newEnv(t)
	g := newShipForge()
	e.s.Forges = g
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Work"})
	round := func(branch, asked string) {
		t.Helper()
		bp, _ := json.Marshal(beforePR{Project: p.Key, Repo: "acme/app", Branch: branch, Base: "main", Asked: asked})
		now := time.Now().UnixMilli()
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO work_log(issue_id, kind, state, before_pr, created_at, updated_at) VALUES (?, 'review', 'done', ?, ?, ?)`, e.issueID(task.Key), string(bp), now, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.s.askedBase(e.ctx, e.admin, "", "upgrade"); got != "" {
		t.Errorf("no task: %q", got)
	}
	round("upgrade", "release")
	if got := e.s.askedBase(e.ctx, e.admin, task.Key, "elsewhere"); got != "" {
		t.Errorf("another branch's round applied: %q", got)
	}
	if got := e.s.askedBase(e.ctx, e.admin, task.Key, "upgrade"); got != "release" {
		t.Errorf("asked base %q", got)
	}
	open := func(head string, stacked []string) *OpenPRResult {
		t.Helper()
		res, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Ready: true, Head: head, Title: head, For: task.Key, StackedOn: stacked})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if _, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Ready: true, Head: "gate-go", Title: "gate-go", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	if res := open("upgrade", []string{"gate-go"}); res.Pull.BaseRef != "release" {
		t.Errorf("stacked on an open parent, asked for release: base %q", res.Pull.BaseRef)
	}
	if res := open("other-branch", []string{"gate-go"}); res.Pull.BaseRef != "gate-go" {
		t.Errorf("another branch: base %q", res.Pull.BaseRef)
	}
}
